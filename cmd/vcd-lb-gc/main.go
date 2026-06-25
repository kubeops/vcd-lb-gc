/*
Copyright AppsCode Inc. and Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// vcd-lb-gc is a Kubernetes controller that garbage-collects orphaned
// VMware Cloud Director load-balancer objects (virtual services, pools,
// DNAT rules) left behind by cloud-provider-for-cloud-director when a port
// is removed from a multi-port LoadBalancer Service.
//
// It is a workaround for the known upstream bug tracked at
// https://github.com/vmware/cloud-provider-for-cloud-director/issues/336.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"kubeops.dev/vcd-lb-gc/pkg/gc"
	"kubeops.dev/vcd-lb-gc/pkg/vcd"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/klog/v2"
)

func main() {
	klog.InitFlags(nil)

	var (
		vcdEndpoint = flag.String("vcd-endpoint", env("VCD_ENDPOINT", ""), "VCD URL, e.g. https://vcd.example.com")
		vcdOrg      = flag.String("vcd-org", env("VCD_ORG", ""), "VCD tenant org name")
		vcdUser     = flag.String("vcd-user", env("VCD_USER", ""), "VCD tenant user")
		vcdPass     = flag.String("vcd-password", env("VCD_PASSWORD", ""), "VCD tenant password")
		vcdInsec    = flag.Bool("vcd-insecure", false, "Skip TLS verification when talking to VCD")

		clusterID = flag.String("cluster-id", env("CLUSTER_ID", ""), `CPI cluster ID, e.g. "capvcdCluster:<uuid>"`)
		edgeGwID  = flag.String("edge-gateway-id", env("EDGE_GATEWAY_ID", ""), `URN of the edge gateway hosting the LB, e.g. urn:vcloud:gateway:<uuid>`)
		interval  = flag.Duration("interval", 60*time.Second, "Reconcile interval")
		dryRun    = flag.Bool("dry-run", false, "Log orphans without deleting")
		skipDNAT  = flag.Bool("skip-dnat", false, "Skip DNAT cleanup (set when enableVirtualServiceSharedIP is on)")

		kubeconfig = flag.String("kubeconfig", env("KUBECONFIG", defaultKubeconfig()), "Path to kubeconfig (in-cluster if empty and run inside a pod)")
		leaderNS   = flag.String("leader-namespace", env("POD_NAMESPACE", "kube-system"), "Namespace for the leader-election Lease")
		leaderName = flag.String("leader-name", "vcd-lb-gc", "Name of the leader-election Lease")
		identity   = flag.String("identity", env("POD_NAME", os.Args[0]), "Identity for leader election")
		disableLE  = flag.Bool("disable-leader-election", false, "Run without leader election (single-replica mode)")
	)
	flag.Parse()

	if *vcdEndpoint == "" || *vcdOrg == "" || *vcdUser == "" || *vcdPass == "" {
		fatal("--vcd-endpoint/--vcd-org/--vcd-user/--vcd-password are required (or VCD_* env)")
	}
	if *clusterID == "" {
		fatal(`--cluster-id is required, e.g. "capvcdCluster:<uuid>"`)
	}
	if *edgeGwID == "" {
		fatal("--edge-gateway-id is required (needed to list virtual services and pools, not just DNAT rules)")
	}

	restCfg, err := loadKubeConfig(*kubeconfig)
	if err != nil {
		fatal("kubeconfig: %v", err)
	}
	kc, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		fatal("kubernetes client: %v", err)
	}

	vc := vcd.New(vcd.Config{
		Endpoint: *vcdEndpoint, Org: *vcdOrg, User: *vcdUser, Password: *vcdPass, Insecure: *vcdInsec,
	})

	r := &gc.Reconciler{
		K8s: kc,
		VCD: vc,
		Opts: gc.Options{
			ClusterID:     *clusterID,
			EdgeGatewayID: *edgeGwID,
			Interval:      *interval,
			DryRun:        *dryRun,
			SkipDNAT:      *skipDNAT,
		},
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	run := func(ctx context.Context) {
		klog.InfoS("vcd-lb-gc starting",
			"cluster-id", *clusterID, "interval", *interval, "dry-run", *dryRun, "skip-dnat", *skipDNAT)
		if err := r.Run(ctx); err != nil && ctx.Err() == nil {
			klog.ErrorS(err, "reconciler exited")
		}
	}

	if *disableLE {
		run(ctx)
		return
	}

	lock, err := resourcelock.NewFromKubeconfig(
		resourcelock.LeasesResourceLock,
		*leaderNS, *leaderName,
		resourcelock.ResourceLockConfig{Identity: *identity},
		restCfg,
		15*time.Second,
	)
	if err != nil {
		fatal("leader-election lock: %v", err)
	}
	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock:            lock,
		LeaseDuration:   30 * time.Second,
		RenewDeadline:   20 * time.Second,
		RetryPeriod:     5 * time.Second,
		ReleaseOnCancel: true,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: run,
			OnStoppedLeading: func() { klog.InfoS("lost leadership; exiting") },
		},
	})
}

func loadKubeConfig(path string) (*rest.Config, error) {
	if path == "" {
		return rest.InClusterConfig()
	}
	return clientcmd.BuildConfigFromFlags("", path)
}

func defaultKubeconfig() string {
	// Return empty string when running in-cluster so rest.InClusterConfig() is used.
	if _, err := os.Stat("/var/run/secrets/kubernetes.io/serviceaccount/token"); err == nil {
		return ""
	}
	if h, _ := os.UserHomeDir(); h != "" {
		return filepath.Join(h, ".kube", "config")
	}
	return ""
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func fatal(format string, a ...any) {
	fmt.Fprintln(os.Stderr, "fatal: "+fmt.Sprintf(format, a...))
	os.Exit(2)
}
