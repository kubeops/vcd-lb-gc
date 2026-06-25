# vcd-lb-gc

A Kubernetes controller that garbage-collects orphaned VMware Cloud Director
load-balancer objects left behind by the `cloud-provider-for-cloud-director`
bug where incremental port removal from a `Service` of type `LoadBalancer`
does **not** clean up the corresponding VCD virtual service, pool, and DNAT
rule.

See [`DESIGN.md`](DESIGN.md) for the upstream bug references
([#336](https://github.com/vmware/cloud-provider-for-cloud-director/issues/336)
and the Known Issue in CPI 1.3–1.6.1 release notes).

## What it does

Every `--interval` (default 60s) the controller:

1. Lists every `Service` of type `LoadBalancer` cluster-wide and computes the
   set of VCD object names the CPI **would** have created for them. The CPI
   convention is:

   ```
   ingress-vs-<svc-name>-<cluster-id>-<port-name>
   ingress-pool-<svc-name>-<cluster-id>-<port-name>
   dnat-ingress-vs-<svc-name>-<cluster-id>-<port-name>
   ```

2. Lists every VCD virtual service / pool / NAT rule whose name contains the
   configured `--cluster-id` (e.g. `capvcdCluster:<uuid>`).
3. Deletes anything in (2) that is not in (1). Order: VS → pool → NAT rule.

Two replicas run with leader election so only one reconciles at a time.

## Build

```bash
go mod tidy
go build ./...
docker build -t ghcr.io/arnobkumarsaha/vcd-lb-gc:latest .
docker push ghcr.io/arnobkumarsaha/vcd-lb-gc:latest
```

## Configure

See [`args.md`](args.md) for all flags, secret values, and how to extract them from the cluster.

## Deploy

```bash
kubectl apply -f deploy/rbac.yaml

# Fill in deploy/secret.example.yaml → secret.yaml, then:
kubectl apply -f deploy/secret.yaml

# Edit deploy/deployment.yaml: set --cluster-id and --edge-gateway-id to your values.
kubectl apply -f deploy/deployment.yaml

# Watch the dry-run output before flipping it off.
kubectl -n vcd logs deploy/vcd-lb-gc -f
```

Once the dry-run logs match what you expect to delete, remove `--dry-run=true`
from `deploy/deployment.yaml` and reapply.

## Test

See [`TEST.md`](TEST.md) for how to reproduce the orphan bug and verify the
controller cleans it up — local build checks plus a full end-to-end run.

## Safety

- **Filter is scoped by `--cluster-id`.** Only objects whose name contains the
  configured cluster ID are eligible for deletion. Set this carefully.
- **Order:** virtual services (which reference pools) are deleted first, then
  pools, then NAT rules.
- **Dry run by default in the sample manifest.** Always validate first.
- **Port-name convention** (`pkg/gc/reconciler.go: portKey`) must match what
  your CPI deployment generates. If your CPI emits a different port suffix
  (e.g. it hashes the port name), update `portKey` accordingly — verify by
  inspecting one live, non-orphan VS name and confirming the suffix matches.

## Caveats

- This is a workaround, not a fix. The upstream CPI repo was archived
  2026-01-20; see [`DESIGN.md`](DESIGN.md).
- The controller assumes one CPI-managed cluster per deployment instance.
  For multiple clusters, run one Deployment per cluster with distinct
  `--cluster-id` and `--leader-name`.
- If your tenant lacks LB + NAT write rights via the OpenAPI, deletions
  will return 403 — fall back to the manual cleanup recipe in
  [`DESIGN.md`](DESIGN.md).
