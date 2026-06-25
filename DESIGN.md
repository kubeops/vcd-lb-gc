# VCD LB Port Removal — Findings & Remediation

## TL;DR

1. **This is a known, officially acknowledged bug** in the VMware Cloud Director Cloud Provider (`cloud-provider-for-cloud-director`, aka CPI / CCM). It affects every released version from CPI 1.3 through 1.6.1 (latest, July 2024) and is documented verbatim in the upstream README and in Broadcom CPI 1.6/1.6.1 release notes. The exact port-removal manifestation that matches your report is tracked in [GitHub issue #336](https://github.com/vmware/cloud-provider-for-cloud-director/issues/336), which is **OPEN with no fix** and last activity Feb 2024.
2. **No fix has shipped, and none is coming from upstream** — the repo `vmware/cloud-provider-for-cloud-director` was archived (read‑only) and moved to `vmware-archive/` on 2026-01-20. CSE 4.2.4 / CPI 1.6.2 release notes list no related fix.
3. **What to do now:** stop mutating the multi-port LB Service in place. The cleanest path for your KubeDB-port-per-DB workflow is **one LoadBalancer Service per DB port** (optionally combined with `enableVirtualServiceSharedIP=true` so all of them share one external IP). The vendor-blessed quick fix for an existing in‑place LB is **delete-and-recreate the Service**. Existing orphans in VCD must be cleaned manually (UI, Terraform, or VCD OpenAPI).

---

## 1. Known bug status

### 1.1 Upstream Known Issue (verbatim)

From `vmware/cloud-provider-for-cloud-director` README "Known Issues" section, and repeated verbatim in Broadcom release notes for CPI 1.3, 1.4, 1.4.1, 1.5, and 1.6/1.6.1:

> Updating service from `LoadBalancer` to `ClusterIP` does not clean up all LoadBalancer service CCM resources. If a DNAT is used, this may get cleaned up, but the virtual service and pools may still remain.
> **Workaround:** Delete the LoadBalancer service and recreate the service.

This is the type-change wording, but the underlying gap is the same as your port-removal symptom: the reconcile loop does not diff against existing VCD state when a desired item is removed.

### 1.2 GitHub issue #336 — exact match to your scenario

**[vmware/cloud-provider-for-cloud-director #336](https://github.com/vmware/cloud-provider-for-cloud-director/issues/336)** — filed 2024-01-24, labeled `bug`, **state: OPEN**, last activity 2024-02-26.

Key comments (verified verbatim via gh API):

- **Contributor viveksyngh** (proposed reproduction + scope extension):
  > Looks like we have same issue when we remove a port from the list of ports from virtual service.

- **Community user luisdavim** (confirms your exact symptom):
  > when we try to remove a port from a LB service the reconciliation of that service gets stuck until we manually delete the corresponding VirtualService and Pool from VCD.

- **Contributor viveksyngh** (proposed fix shape — confirming no API exists today):
  > we should have some way to select all virtual ports services for a service and then figure out the diff. We can implement this in controller for load balancer if VCD has API to list all virtual services for a kubernetes service.

- **Maintainer arunmk** (no internal fix existed):
  > @viveksyngh can we get a bugzilla also going on for this so that we can prioritize

### 1.3 Adjacent history

- **CPI 1.1.1** fixed `nodePort` / `port` *value* updates ([#49](https://github.com/vmware/cloud-provider-for-cloud-director/issues/49)). That fix did **not** cover port-name updates or port additions/removals from a multi-port set — which is why #336 had to be filed later.
- **CSE 4.2.4 / CPI 1.6.2 / CAPVCD 1.3.3** release notes list 7 resolved issues; **none** mention LoadBalancer, port, orphan, virtual service, DNAT, pool, AVI, or NSX-ALB. The only CPI fix in that bundle is for Datacenter Group Network cluster create/resize failures.
- **Upstream archived 2026-01-20** → `vmware-archive/cloud-provider-for-cloud-director`. No further fixes will land upstream.

---

## 2. Annotations & controller flags

**Bottom line: there is no annotation or controller flag that will make the CPI clean up orphans.** Broadcom's CPI 1.6 L4/L7 docs only document SSL-related Service annotations; there is no full-resync hook, no GC flag, and no shared-IP or load-balancer-class annotation.

| Annotation / flag | Exists? | What it does | Helps with orphans? |
|---|---|---|---|
| `service.beta.kubernetes.io/vcloud-avi-ssl-no-termination` | ✅ | Selects ports to pass SSL through (no termination) | No |
| `service.beta.kubernetes.io/vcloud-avi-ssl-termination` (`vcloud-avi-ssl-ports` + `vcloud-avi-ssl-cert-alias`) | ✅ | Selects ports and cert alias for SSL termination at VCD | No |
| `service.beta.kubernetes.io/vcloud-load-balancer-class` | ❌ | Not documented in CPI 1.6 docs | — |
| `oneArm` (config) | ✅ (controller config, not annotation) | When non-nil, virtual services share an internal IP and DNAT maps shared internal → external | Architectural; doesn't fix the diff bug |
| `enableVirtualServiceSharedIP` (configmap, not annotation) | ✅ (CPI ≥ 1.2.0 on VCD ≥ 10.4.0) | Multiple virtual services on **one external IP**, different ports; removes per-port DNAT | Sidesteps the bug when combined with one-Service-per-port (see §3.A) |
| Sync period / full-resync controller flag | ❌ | Not documented anywhere | — |
| Reconcile-diff against existing VCD objects | ❌ | Contributor proposal in #336, not implemented | — |

`enableVirtualServiceSharedIP` is documented verbatim in the README and the Broadcom CPI 1.6 PDF:

> The `enableVirtualServiceSharedIP` feature allows utilizing a feature in VMware Cloud Director 10.4.0 and newer versions, in which you can create multiple virtual services with the same external IP address and different ports. This removes the need to create a DNAT rule…

⚠️ Caveat: whether the shared-IP code path orphans on port removal has not been explicitly tested in the public record. It changes the resource shape (no per-port DNAT) but does not necessarily fix the reconcile diff.

---

## 3. Workarounds — ranked for your KubeDB-per-DB workflow

### A. One LoadBalancer Service per DB port  ✅ recommended

Sidesteps the bug entirely because the CPI **does** correctly clean up VCD objects when a Service is fully deleted (it only fails on incremental port mutation).

- KubeDB pattern: each DB instance gets its own `Service` of type `LoadBalancer` with a single port.
- Combine with `enableVirtualServiceSharedIP: true` (VCD ≥ 10.4.0, CPI ≥ 1.2.0) in your CPI configmap so all DB Services share **one** external IP across different ports — same VIP footprint as today, but lifecycle is per-DB and clean.
- Cost: one extra Service object per DB (negligible). VCD virtual service / pool count is unchanged.

### B. Delete-and-recreate the LB Service on port change  ✅ vendor-documented

This is the workaround verbatim from the upstream README and Broadcom release notes.

- When a DB is removed (port goes away), tear down the entire LB Service and recreate it with the remaining ports — do not `kubectl apply` a smaller spec.
- Brief downtime per change (a few seconds — the recreate is fast).
- Best when migrating to (A) is too disruptive but you want to stop the bleed today.

### C. Terraform-based orphan cleanup  ✅ for backlog

See §5 (Manual cleanup recipe). Use this to clean up what you already have, regardless of which forward strategy you pick.

### D. Pre-removal hook that splits a port into a throwaway Service  ❌ not recommended

Undocumented. The CPI does not migrate existing VCD objects between Kubernetes Services — the existing pool / VS / DNAT are tied to the original Service's UID. Creating a separate Service for an about-to-be-removed port would create *new* VCD objects, not transfer ownership. Deleting that throwaway Service would only clean up the new objects, not the original orphans. **Skip this.**

---

## 4. Long-term alternatives

(Confidence: medium — these are general K8s patterns; their fit depends on your NSX-T topology and tenant network rights.)

- **In-cluster ingress controller** (nginx-ingress, traefik) behind a single `LoadBalancer` Service on 80/443 + an `nginx-ingress` `tcp-services`/`udp-services` ConfigMap for DB TCP traffic. The single LB Service never changes its port set, so the CPI bug never triggers. Routing is by host/port at the ingress controller. Good fit if you're willing to standardize DB external addressing on a fixed pair of ports + hostnames.
- **NodePort + externally-managed NSX-ALB virtual services** declared by Terraform. You keep KubeDB Services as NodePort; Terraform creates / destroys the ALB virtual service per DB. Lifecycle is explicit, no CPI involvement. Most operational work, most control.
- **MetalLB / kube-vip** on workers announcing a VCD-allocated IP range. Bypasses CPI entirely. Viability depends on whether your tenant has the NSX-T routing rights to advertise these IPs out the edge gateway — often not in self-service VCD orgs. Worth a quick test, but don't bet on it.

---

## 5. Manual cleanup recipe (for existing orphans)

You have three viable paths. **Option 5.1 (Terraform)** is the most automatable.

### 5.1 Terraform — `vmware/vcd` provider

The provider supports import + destroy of every relevant object. Object import path is `<org>.<vdc-or-vdc-group>.<edge-gateway>.<object-name>` (4 dot-separated components).

**Important order:** virtual service first (it holds a `pool_id` reference), then the pool, then any DNAT rule.

```hcl
# main.tf
terraform {
  required_providers {
    vcd = { source = "vmware/vcd" }
  }
}

provider "vcd" {
  url      = "https://vcd.example.com/api"
  org      = "dbaas"
  user     = "..."
  password = "..."
}

resource "vcd_nsxt_alb_virtual_service" "orphan_vs_http_alt" {
  name            = "ingress-vs-lb-test-capvcdCluster:bc77c367-851b-4bd6-b879-8832657025d8-http-alt"
  edge_gateway_id = "urn:vcloud:gateway:cb64f385-38ee-4a1f-b954-866e087a5094"
  # required fields — fill from import; you only need this resource in state to destroy it
}

resource "vcd_nsxt_alb_pool" "orphan_pool_http_alt" {
  name            = "ingress-pool-lb-test-capvcdCluster:bc77c367-851b-4bd6-b879-8832657025d8-http-alt"
  edge_gateway_id = "urn:vcloud:gateway:cb64f385-38ee-4a1f-b954-866e087a5094"
}

resource "vcd_nsxt_nat_rule" "orphan_dnat_http_alt" {
  name            = "dnat-ingress-vs-lb-test-capvcdCluster:bc77c367-851b-4bd6-b879-8832657025d8-http-alt"
  edge_gateway_id = "urn:vcloud:gateway:cb64f385-38ee-4a1f-b954-866e087a5094"
  rule_type       = "DNAT"
}
```

```bash
# Import (replace <org>, <vdc> with your values)
terraform import vcd_nsxt_alb_virtual_service.orphan_vs_http_alt \
  'dbaas.<vdc>.dbaas internet Edge1.ingress-vs-lb-test-capvcdCluster:bc77c367-851b-4bd6-b879-8832657025d8-http-alt'

terraform import vcd_nsxt_alb_pool.orphan_pool_http_alt \
  'dbaas.<vdc>.dbaas internet Edge1.ingress-pool-lb-test-capvcdCluster:bc77c367-851b-4bd6-b879-8832657025d8-http-alt'

# DNAT rule import takes a numeric or named identifier — see provider docs
terraform import vcd_nsxt_nat_rule.orphan_dnat_http_alt \
  'dbaas.<vdc>.dbaas internet Edge1.dnat-ingress-vs-lb-test-capvcdCluster:bc77c367-851b-4bd6-b879-8832657025d8-http-alt'

# Destroy in the right order
terraform destroy -target=vcd_nsxt_alb_virtual_service.orphan_vs_http_alt
terraform destroy -target=vcd_nsxt_alb_pool.orphan_pool_http_alt
terraform destroy -target=vcd_nsxt_nat_rule.orphan_dnat_http_alt
```

### 5.2 VCD UI

Tenant Portal → Networking → Edge Gateways → `dbaas internet Edge1`:

1. **Load Balancer → Virtual Services** → delete the `-http-alt` virtual service.
2. **Load Balancer → Pools** → delete the `-http-alt` pool.
3. **NAT** → delete the DNAT rule for the orphan.

### 5.3 VCD OpenAPI direct calls

Useful if you want to script bulk cleanup. Endpoints (VCD 10.4+ shape; exact paths vary):

```
GET    /cloudapi/1.0.0/loadBalancer/virtualServices?filter=(name==ingress-vs-lb-test-capvcdCluster*)
DELETE /cloudapi/1.0.0/loadBalancer/virtualServices/{urn}
GET    /cloudapi/1.0.0/loadBalancer/pools?filter=(name==ingress-pool-lb-test-capvcdCluster*)
DELETE /cloudapi/1.0.0/loadBalancer/pools/{urn}
GET    /cloudapi/1.0.0/edgeGateways/{edge-urn}/nat/rules
DELETE /cloudapi/1.0.0/edgeGateways/{edge-urn}/nat/rules/{rule-id}
```

CPI-generated objects all carry the cluster ID prefix (`...capvcdCluster:bc77c367-...`) so you can filter cleanly. Delete VS before its pool.

---

## 6. Sources

1. **GitHub — `vmware/cloud-provider-for-cloud-director` README (Known Issues + `enableVirtualServiceSharedIP` docs)** — https://github.com/vmware/cloud-provider-for-cloud-director/blob/main/README.md
2. **GitHub issue #336 — port removal does not clean up VCD objects (OPEN)** — https://github.com/vmware/cloud-provider-for-cloud-director/issues/336
3. **GitHub issue #49 — port/nodePort value updates (FIXED in CPI 1.1.1)** — https://github.com/vmware/cloud-provider-for-cloud-director/issues/49
4. **GitHub — archived repo location (2026-01-20)** — https://github.com/vmware-archive/cloud-provider-for-cloud-director
5. **Broadcom TechDocs — Kubernetes Cloud Provider for VCD 1.6 Release Notes** — https://techdocs.broadcom.com/us/en/vmware-cis/cloud-director/k8cp-for-vcd/1-6/release-notes/kubernetes-cloud-provider-for-vmware-cloud-director-16-release-notes.html
6. **Broadcom TechDocs — Kubernetes Cloud Provider for VCD 1.6 PDF (full user guide)** — https://techdocs.broadcom.com/content/dam/broadcom/techdocs/us/en/pdf/vmware/cloud-director/k8cp-for-vcd/kubernetes-cloud-provider-for-vmware-cloud-director-1-6.pdf
7. **Broadcom TechDocs — L4 and L7 Load Balancer Configuration (CPI 1.6 — Service annotations)** — https://techdocs.broadcom.com/us/en/vmware-cis/cloud-director/k8cp-for-vcd/1-6/kubernetes-cloud-provider-for-vmware-cloud-director-user-guide-1-6/l4-and-l7-load-balancer-configuration.html
8. **VMware Docs mirror — CPI 1.6 release notes** — https://docs.vmware.com/en/Kubernetes-Cloud-Provider-for-VMware-Cloud-Director/1.6/rn/kubernetes-cloud-provider-for-vmware-cloud-director-16-release-notes/index.html
9. **VMware Docs mirror — CPI 1.5 release notes** — https://docs.vmware.com/en/Kubernetes-Cloud-Provider-for-VMware-Cloud-Director/1.5/rn/kubernetes-cloud-provider-for-vmware-cloud-director-15-release-notes/index.html
10. **Broadcom TechDocs — CSE 4.2.4 release notes (no LB fix listed)** — https://techdocs.broadcom.com/us/en/vmware-cis/cloud-director/container-service-extension/4-2/release-notes/vmware-cloud-director-container-service-extension-424-release-notes.html
11. **Terraform Registry — `vcd_nsxt_alb_virtual_service` resource (import + destroy)** — https://registry.terraform.io/providers/vmware/vcd/latest/docs/resources/nsxt_alb_virtual_service
12. **Terraform Registry — `vcd_nsxt_alb_pool` resource (import + destroy)** — https://registry.terraform.io/providers/vmware/vcd/latest/docs/resources/nsxt_alb_pool
13. **Terraform Registry — NSX-T ALB guide** — https://registry.terraform.io/providers/vmware/vcd/latest/docs/guides/nsxt_alb
14. **VMware Developer — Edge Gateway Load Balancer Virtual Service OpenAPI** — https://developer.vmware.com/apis/vmware-cloud-director/latest/edge-gateway-load-balancer-virtual-service/
15. **VMware Developer — Edge Gateway Load Balancer Pools OpenAPI** — https://developer.vmware.com/apis/vmware-cloud-director/latest/edge-gateway-load-balancer-pools/
16. **vmware-archive — earlier related issue #242** — https://github.com/vmware-archive/cloud-provider-for-cloud-director/issues/242
17. **vmware/cluster-api-provider-cloud-director issue #211 (related LB lifecycle context)** — https://github.com/vmware/cluster-api-provider-cloud-director/issues/211
18. **vstellar.com — How to force-delete a stale virtual service in NSX-ALB (blog, supplementary)** — https://vstellar.com/2024/02/how-to-force-delete-a-stale-virtual-service-in-nsx-alb/

---

## Caveats

- The vendor Known Issue text strictly describes the `LoadBalancer` → `ClusterIP` type-change path. The exact port-removal manifestation is established via comments on issue #336, not the Known Issue verbatim — but both stem from the same reconciliation gap.
- `enableVirtualServiceSharedIP=true` consolidates VIPs but its behavior on port-removal has not been explicitly tested in the public record. Combine with one-Service-per-port (§3.A) to be safe.
- Issue #336 last activity is Feb 2024; the upstream repo is archived. No upstream fix is coming.
- For tenants without Service Engine Group write access in their VCD org, Terraform import/destroy may be denied — verify with a single test resource before bulk cleanup.
