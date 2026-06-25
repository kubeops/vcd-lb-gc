// Package gc reconciles VCD load-balancer objects against Kubernetes
// LoadBalancer Services and deletes orphans left behind by the CPI bug
// where incremental port removal does not clean up the VS/pool/DNAT.
//
// Object naming convention used by cloud-provider-for-cloud-director:
//
//	pool : ingress-pool-<svc-name>-<cluster-id>-<port-name>
//	vs   : ingress-vs-<svc-name>-<cluster-id>-<port-name>
//	dnat : dnat-ingress-vs-<svc-name>-<cluster-id>-<port-name>
//
// where <cluster-id> is the literal string the CPI was configured with,
// typically "capvcdCluster:<uuid>".
package gc

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"kubeops.dev/vcd-lb-gc/pkg/vcd"
)

type Options struct {
	ClusterID     string // e.g. "capvcdCluster:bc77c367-851b-4bd6-b879-8832657025d8"
	EdgeGatewayID string // urn:vcloud:gateway:...
	Interval      time.Duration
	DryRun        bool
	SkipDNAT      bool // set when CPI is configured with enableVirtualServiceSharedIP
}

type Reconciler struct {
	K8s  kubernetes.Interface
	VCD  *vcd.Client
	Opts Options
}

// Run blocks until ctx is cancelled, reconciling every Opts.Interval.
func (r *Reconciler) Run(ctx context.Context) error {
	if r.Opts.Interval <= 0 {
		r.Opts.Interval = 60 * time.Second
	}
	t := time.NewTicker(r.Opts.Interval)
	defer t.Stop()
	// Run once immediately, then on each tick.
	r.reconcileOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			r.reconcileOnce(ctx)
		}
	}
}

func (r *Reconciler) reconcileOnce(ctx context.Context) {
	start := time.Now()
	if err := r.reconcile(ctx); err != nil {
		klog.ErrorS(err, "reconcile failed", "elapsed", time.Since(start))
		return
	}
	klog.V(2).InfoS("reconcile done", "elapsed", time.Since(start))
}

func (r *Reconciler) reconcile(ctx context.Context) error {
	expected, err := r.expectedNames(ctx)
	if err != nil {
		return fmt.Errorf("collect expected names: %w", err)
	}
	klog.V(3).InfoS("expected names", "count", len(expected))

	// Common prefixes derived from cluster ID. We do server-side name filter
	// on these to avoid pulling every VCD object in the tenancy.
	clusterTag := r.Opts.ClusterID
	vsPrefix := "ingress-vs-"
	poolPrefix := "ingress-pool-"
	dnatPrefix := "dnat-ingress-vs-"

	// We can't filter by "contains" cheaply, so pull the prefix bucket and
	// filter client-side for cluster ID match.
	vsList, err := r.VCD.ListVirtualServices(ctx, r.Opts.EdgeGatewayID, vsPrefix)
	if err != nil {
		return fmt.Errorf("list virtual services: %w", err)
	}
	poolList, err := r.VCD.ListPools(ctx, r.Opts.EdgeGatewayID, poolPrefix)
	if err != nil {
		return fmt.Errorf("list pools: %w", err)
	}
	var dnatList []vcd.NATRule
	if !r.Opts.SkipDNAT {
		dnatList, err = r.VCD.ListNATRules(ctx, r.Opts.EdgeGatewayID, dnatPrefix)
		if err != nil {
			return fmt.Errorf("list nat rules: %w", err)
		}
	}

	// Orphan = belongs to this cluster AND not in expected set.
	// Delete VS first (it references the pool), then pools, then DNAT rules.
	for _, vs := range vsList {
		if !strings.Contains(vs.Name, clusterTag) {
			continue
		}
		if _, ok := expected[vs.Name]; ok {
			continue
		}
		r.deleteVS(ctx, vs)
	}
	for _, p := range poolList {
		if !strings.Contains(p.Name, clusterTag) {
			continue
		}
		if _, ok := expected[p.Name]; ok {
			continue
		}
		r.deletePool(ctx, p)
	}
	for _, n := range dnatList {
		if !strings.Contains(n.Name, clusterTag) {
			continue
		}
		if _, ok := expected[n.Name]; ok {
			continue
		}
		r.deleteNAT(ctx, n)
	}
	return nil
}

// expectedNames lists every LoadBalancer Service in the cluster and emits all
// the VCD object names the CPI would have created for it.
func (r *Reconciler) expectedNames(ctx context.Context) (map[string]struct{}, error) {
	svcList, err := r.K8s.CoreV1().Services("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make(map[string]struct{}, len(svcList.Items)*4)
	for _, svc := range svcList.Items {
		if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
			continue
		}
		for _, p := range svc.Spec.Ports {
			portName := portKey(p)
			suffix := fmt.Sprintf("%s-%s-%s", svc.Name, r.Opts.ClusterID, portName)
			out["ingress-vs-"+suffix] = struct{}{}
			out["ingress-pool-"+suffix] = struct{}{}
			out["dnat-ingress-vs-"+suffix] = struct{}{}
		}
	}
	return out, nil
}

// portKey mirrors the CPI's port-name selection: it uses ServicePort.Name when set,
// otherwise falls back to the protocol-port combo. Keep this in sync with how
// your CPI deployment generates names — verify against an existing live VS name
// before disabling --dry-run.
func portKey(p corev1.ServicePort) string {
	if p.Name != "" {
		return p.Name
	}
	return fmt.Sprintf("%s-%d", strings.ToLower(string(p.Protocol)), p.Port)
}

func (r *Reconciler) deleteVS(ctx context.Context, vs vcd.VirtualService) {
	if r.Opts.DryRun {
		klog.InfoS("DRY-RUN would delete virtual service", "name", vs.Name, "id", vs.ID, "vip", vs.VirtualIPAddress)
		return
	}
	klog.InfoS("deleting orphan virtual service", "name", vs.Name, "id", vs.ID)
	if err := r.VCD.DeleteVirtualService(ctx, vs.ID); err != nil {
		klog.ErrorS(err, "delete virtual service", "name", vs.Name, "id", vs.ID)
	}
}

func (r *Reconciler) deletePool(ctx context.Context, p vcd.Pool) {
	if r.Opts.DryRun {
		klog.InfoS("DRY-RUN would delete pool", "name", p.Name, "id", p.ID)
		return
	}
	klog.InfoS("deleting orphan pool", "name", p.Name, "id", p.ID)
	if err := r.VCD.DeletePool(ctx, p.ID); err != nil {
		klog.ErrorS(err, "delete pool", "name", p.Name, "id", p.ID)
	}
}

func (r *Reconciler) deleteNAT(ctx context.Context, n vcd.NATRule) {
	if r.Opts.DryRun {
		klog.InfoS("DRY-RUN would delete NAT rule", "name", n.Name, "id", n.ID)
		return
	}
	klog.InfoS("deleting orphan NAT rule", "name", n.Name, "id", n.ID)
	if err := r.VCD.DeleteNATRule(ctx, r.Opts.EdgeGatewayID, n.ID); err != nil {
		klog.ErrorS(err, "delete NAT rule", "name", n.Name, "id", n.ID)
	}
}
