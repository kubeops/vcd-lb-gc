# VCD LB Port Removal Bug Report

## Summary

Removing a port from a Kubernetes `LoadBalancer` Service does **not** delete the corresponding VCD objects (pool, virtual service, DNAT rule). Those objects become permanently orphaned until manually deleted. The CPI only reconciles deletions triggered by a full `kubectl delete svc` — incremental port removal is not handled.

---

## Environment

| Item | Value |
|------|-------|
| VCD endpoint | `https://vcd.example.com` |
| Tenant | `dbaas` |
| Edge Gateway | `dbaas internet Edge1` (`urn:vcloud:gateway:cb64f385-38ee-4a1f-b954-866e087a5094`) |
| Cluster | `kube22-dbaas01` (`bc77c367-851b-4bd6-b879-8832657025d8`) |
| K8s context | `kube22-dbaas01-admin@kube22-dbaas01` |

---

## Test Steps & Observations

### Step 1 — Deploy app-a + 1-port LB (port 80)

Applied `app-a` (nginx:alpine) and a `LoadBalancer` service `lb-test` with a single port 80.

**K8s result:**
```
NAME      TYPE           CLUSTER-IP       EXTERNAL-IP    PORT(S)        AGE
lb-test   LoadBalancer   100.128.119.16   185.9.43.232   80:32421/TCP   22s
```

**VCD objects created (all REALIZED / UP):**

| Type | Name | VIP | Port |
|------|------|-----|------|
| Pool | `ingress-pool-lb-test-...-http` | — | — |
| Virtual Service | `ingress-vs-lb-test-...-http` | `192.168.8.10` | 80 |
| DNAT Rule | `dnat-ingress-vs-lb-test-...-http` | `185.9.43.232` → `192.168.8.10` | 80 |

**Result: PASS** — 1 pool, 1 VS, 1 NAT rule as expected.

---

### Step 2 — Add app-b + second port 8080

Updated `lb-test` to include a second port `http-alt:8080`. Applied `app-b` (http-echo:8080).

**K8s result:**
```
NAME      TYPE           CLUSTER-IP       EXTERNAL-IP    PORT(S)                       AGE
lb-test   LoadBalancer   100.128.119.16   185.9.43.232   80:32421/TCP,8080:31878/TCP   ~5m
```

**VCD objects after reconciliation:**

| Type | Name | VIP | Port | Health |
|------|------|-----|------|--------|
| Pool | `ingress-pool-lb-test-...-http` | — | — | UP (3/3) |
| Pool | `ingress-pool-lb-test-...-http-alt` | — | — | DOWN (0/3)* |
| Virtual Service | `ingress-vs-lb-test-...-http` | `192.168.8.10` | 80 | UP |
| Virtual Service | `ingress-vs-lb-test-...-http-alt` | `192.168.8.11` | 8080 | DOWN* |
| DNAT Rule | `dnat-ingress-vs-lb-test-...-http` | `185.9.43.232` → `192.168.8.10` | 80 | — |
| DNAT Rule | `dnat-ingress-vs-lb-test-...-http-alt` | `185.9.43.232` → `192.168.8.11` | 8080 | — |

\* DOWN expected — service selector is `app: app-a` which has no container on port 8080.

**Result: PASS** — 2 pools, 2 VSes, 2 NAT rules created correctly.

---

### Step 3 — Remove port 8080 (bug under test)

Reverted `lb-test` back to v1 (port 80 only) via `kubectl apply`.

**K8s result:**
```
NAME      TYPE           CLUSTER-IP       EXTERNAL-IP    PORT(S)        AGE
lb-test   LoadBalancer   100.128.119.16   185.9.43.232   80:32421/TCP   ~35m
```

**VCD state polled every 5 seconds for 2 minutes (24 polls):**

```
00:32:23 pools=2 vs=2 nat=2
00:32:31 pools=2 vs=2 nat=2
...
00:35:31 pools=2 vs=2 nat=2
TIMEOUT — objects still present after 2min
```

**VCD objects after 2 minutes — `http-alt` still present:**

| Type | Name | VIP | Port | Status |
|------|------|-----|------|--------|
| Pool | `ingress-pool-lb-test-...-http-alt` | — | — | DOWN — **ORPHANED** |
| Virtual Service | `ingress-vs-lb-test-...-http-alt` | `192.168.8.11` | 8080 | DOWN — **ORPHANED** |
| DNAT Rule | `dnat-ingress-vs-lb-test-...-http-alt` | `185.9.43.232` → `192.168.8.11` | 8080 | — — **ORPHANED** |

**Result: FAIL** — Port removal from a live LB service does not trigger VCD object cleanup.

---

### Step 4 — Full service deletion (`kubectl delete svc lb-test`)

**VCD state after `kubectl delete svc lb-test`:**

| Type | Name | Status |
|------|------|--------|
| Pool `...-http` | deleted | GONE |
| Virtual Service `...-http` | deleted | GONE |
| DNAT Rule `...-http` | deleted | GONE |
| Pool `...-http-alt` | **still present** | **ORPHANED** |
| Virtual Service `...-http-alt` | **still present** | **ORPHANED** |
| DNAT Rule `...-http-alt` | **still present** | **ORPHANED** |

**Result: FAIL** — Even a full service delete did not remove the objects that were orphaned by the earlier port removal. The CPI had already lost track of them.

---

## Root Cause (Assessment)

The CPI reconcile loop for `Service` updates computes desired VCD objects from the **current** service spec. When a port is removed, the orphaned objects are no longer in the desired state — but the CPI appears to only **create/update** objects found in the new spec, without **diffing against existing VCD objects** to delete the ones that were removed.

On full service delete, the CPI likely iterates over the service's current port list to issue deletes — so `http-alt` is already absent from the spec and never gets a delete call issued.

---

## Impact

- Orphaned VCD objects consume LB VIP addresses from the pool (e.g., `192.168.8.11` remains allocated).
- Orphaned DNAT rules keep firewall entries active for ports that no longer serve traffic.
- Accumulates silently — no error or event is raised in K8s.
- Cannot be recovered automatically; requires manual VCD cleanup.

---

## Orphaned Objects to Clean Up (from this test)

| Object Type | Name |
|-------------|------|
| Pool | `ingress-pool-lb-test-capvcdCluster:bc77c367-851b-4bd6-b879-8832657025d8-http-alt` |
| Virtual Service | `ingress-vs-lb-test-capvcdCluster:bc77c367-851b-4bd6-b879-8832657025d8-http-alt` |
| DNAT Rule | `dnat-ingress-vs-lb-test-capvcdCluster:bc77c367-851b-4bd6-b879-8832657025d8-http-alt` |
