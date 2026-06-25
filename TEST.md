# Testing vcd-lb-gc

This guide covers two layers of testing:

1. **[Local checks](#1-local-checks)** — build and vet without a cluster.
2. **[End-to-end test](#2-end-to-end-test)** — reproduce the CPI orphan bug on a
   real VCD-backed cluster, then prove `vcd-lb-gc` cleans the orphans up.

The e2e flow mirrors the original bug reproduction in
[`poc/report.md`](poc/report.md) and [`poc/test-lb-plan.md`](poc/test-lb-plan.md):
create a multi-port `LoadBalancer`, remove a port to strand VCD objects, then
let the controller garbage-collect them.

---

## 1. Local checks

No cluster or VCD access required.

```bash
go build ./...
go vet ./...
go test ./...        # add unit tests under pkg/gc as they land
```

The `portKey` logic in [`pkg/gc/reconciler.go`](pkg/gc/reconciler.go) is the
highest-value thing to unit-test, since the whole orphan match hinges on the
controller computing the same `<svc>-<cluster-id>-<port-name>` suffix the CPI
emits.

---

## 2. End-to-end test

### Prerequisites

- A Kubernetes cluster provisioned through VMware Cloud Director with
  `cloud-provider-for-cloud-director` (CPI) installed and creating LB objects.
- VCD tenant credentials with **LB + NAT read/write** rights on the edge gateway.
- The cluster's `--cluster-id` and `--edge-gateway-id`. See [`args.md`](args.md)
  for exactly how to extract these from the cluster.
- A namespace to test in (examples below use `vcd`):

  ```bash
  kubectl create namespace vcd
  ```

Set these once for the snippets below:

```bash
export EDGE_GW=urn:vcloud:gateway:cb64f385-38ee-4a1f-b954-866e087a5094
export CLUSTER_ID=capvcdCluster:bc77c367-851b-4bd6-b879-8832657025d8
```

### How to inspect VCD state

You need a way to list virtual services / pools / NAT rules to confirm what got
created and cleaned up. Any of these work:

- **VCD UI** — Tenant Portal → Networking → Edge Gateways → *your gateway* →
  Load Balancer → Virtual Services / Pools, and the NAT tab.
- **OpenAPI via curl** — templates in [`curl/`](curl/) list each object type.
  Replace `<VCD_JWT_TOKEN>` / `<VCD_SESSION_TOKEN>` with a live session token
  (grab it from your browser dev tools after logging into the VCD portal) and
  the endpoint/gateway URN with yours:

  ```bash
  bash curl/curl_virtual_services.txt | jq '.values[].name'
  bash curl/curl_pools.txt            | jq '.values[].name'
  bash curl/curl_nat_rules.txt        | jq '.values[].name'
  ```

A small poll loop makes the "did it get cleaned up?" steps easy to watch:

```bash
watch -n5 "bash curl/curl_virtual_services.txt | jq -r '.values[].name' | grep $CLUSTER_ID"
```

---

### Step 1 — Create the orphan (reproduce the bug)

Deploy two apps and a 2-port LoadBalancer Service, then remove the second port.
The manifests are in [`examples/`](examples/) (originally from
[`poc/test-lb-plan.md`](poc/test-lb-plan.md)):

```bash
# app-a (nginx:80) + app-b (http-echo:8080) in namespace vcd
kubectl apply -f examples/deploy-a.yaml -f examples/deploy-b.yaml

# lb-test with TWO ports: http:80 and http-alt:8080
kubectl apply -f examples/lb-svc-2port.yaml
kubectl get svc lb-test -n vcd -w   # wait for EXTERNAL-IP
```

Confirm VCD now has **two** of each object — names ending `-http` and `-http-alt`:

```
ingress-vs-lb-test-<cluster-id>-http
ingress-vs-lb-test-<cluster-id>-http-alt      <-- about to be orphaned
ingress-pool-lb-test-<cluster-id>-http
ingress-pool-lb-test-<cluster-id>-http-alt
dnat-ingress-vs-lb-test-<cluster-id>-http
dnat-ingress-vs-lb-test-<cluster-id>-http-alt
```

Now revert to the **1-port** spec (port 80 only) and re-apply:

```bash
kubectl apply -f examples/lb-svc-1port.yaml
kubectl get svc lb-test -n vcd   # shows only 80/TCP
```

**Expected bug:** the `-http-alt` virtual service, pool, and DNAT rule remain in
VCD even though the port is gone from the Service. Confirm they're still present
(this is the orphan `vcd-lb-gc` will clean):

```bash
bash curl/curl_virtual_services.txt | jq -r '.values[].name' | grep http-alt
```

---

### Step 2 — Run the controller in dry-run

Run `vcd-lb-gc` out-of-cluster against your kubeconfig first, so nothing is
deleted until you've eyeballed the plan. `--dry-run` logs what *would* be
deleted; `--disable-leader-election` skips the Lease since you're running a
single local process.

```bash
go run ./cmd/vcd-lb-gc \
  --vcd-endpoint=https://vcd.example.com \
  --vcd-org=dbaas \
  --vcd-user=<vcd-user> \
  --vcd-password=<vcd-password> \
  --cluster-id=$CLUSTER_ID \
  --edge-gateway-id=$EDGE_GW \
  --interval=15s \
  --dry-run=true \
  --disable-leader-election \
  --v=2
```

**Expected log lines** — exactly the three orphaned `-http-alt` objects, and
nothing matching a live port:

```
DRY-RUN would delete virtual service  name="ingress-vs-lb-test-<cluster-id>-http-alt" ...
DRY-RUN would delete pool             name="ingress-pool-lb-test-<cluster-id>-http-alt" ...
DRY-RUN would delete NAT rule         name="dnat-ingress-vs-lb-test-<cluster-id>-http-alt" ...
```

> ⚠️ If you see a live object (e.g. anything ending `-http`) in the dry-run
> output, **stop** — your `--cluster-id` or the `portKey` convention doesn't
> match what your CPI emits. Re-check [`args.md`](args.md) and the `portKey`
> note in [`pkg/gc/reconciler.go`](pkg/gc/reconciler.go) before going further.

### Step 3 — Let it delete

Once the dry-run plan is exactly the orphans, drop `--dry-run`:

```bash
go run ./cmd/vcd-lb-gc \
  --vcd-endpoint=https://vcd.example.com --vcd-org=dbaas \
  --vcd-user=<vcd-user> --vcd-password=<vcd-password> \
  --cluster-id=$CLUSTER_ID --edge-gateway-id=$EDGE_GW \
  --interval=15s --disable-leader-election --v=2
```

**Expected logs** (note: deletes VS → pool → NAT, in that order):

```
deleting orphan virtual service  name="ingress-vs-lb-test-<cluster-id>-http-alt" ...
deleting orphan pool             name="ingress-pool-lb-test-<cluster-id>-http-alt" ...
deleting orphan NAT rule         name="dnat-ingress-vs-lb-test-<cluster-id>-http-alt" ...
```

**Verify in VCD** the `-http-alt` objects are gone and the live `-http` objects
survived:

```bash
bash curl/curl_virtual_services.txt | jq -r '.values[].name' | grep $CLUSTER_ID
# -> only ...-http remains; ...-http-alt is gone
```

This is the pass condition: **orphans removed, live LB untouched.**

---

### Step 4 — Idempotency / safety check

With the orphans gone, leave the controller running for another interval or two.
It must converge to **zero deletes** — every remaining object now matches a live
Service port, so nothing should be logged as an orphan. A controller that keeps
deleting on a steady-state cluster is misconfigured (wrong `--cluster-id` or
`portKey` mismatch).

---

### Step 5 — In-cluster deployment test

After the out-of-cluster run passes, validate the real deployment path:

```bash
kubectl apply -f deploy/rbac.yaml

# Fill deploy/secret.example.yaml -> secret.yaml (see args.md), then:
kubectl apply -f deploy/secret.yaml

# deploy/deployment.yaml ships with --dry-run=true and 2 replicas + leader election.
# Set --cluster-id / --edge-gateway-id to your values first.
kubectl apply -f deploy/deployment.yaml

kubectl -n vcd logs deploy/vcd-lb-gc -f
```

Checks:

- **Leader election** — with 2 replicas, exactly one pod logs `vcd-lb-gc starting`
  and reconciles; the other waits. Delete the leader pod and confirm the standby
  takes over within ~30s (the Lease duration).
- **Dry-run first** — the deployed manifest defaults to `--dry-run=true`. Confirm
  the logged plan matches expectations, then remove `--dry-run=true` and reapply
  to enable real deletions.

---

## Teardown

```bash
kubectl delete -f deploy/deployment.yaml -f deploy/secret.yaml -f deploy/rbac.yaml
kubectl delete svc lb-test -n vcd
kubectl delete -f examples/deploy-a.yaml -f examples/deploy-b.yaml
```

Deleting `lb-test` cleans up its remaining (`-http`) VCD objects. If any
`-http-alt` orphans predate the controller and survive — e.g. created before
RBAC/rights were granted — fall back to the manual cleanup recipe in
[`poc/findings.md` §5](poc/findings.md#5-manual-cleanup-recipe-for-existing-orphans).
