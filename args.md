# Args & Secret Values

All values below are for the **kube22-dbaas01** cluster (`vcloud-ccm-configmap`). Commands run against `kube-system` namespace.

---

## Secret (`deploy/secret.yaml`)

| Key | Value | How to find |
|-----|-------|-------------|
| `VCD_ENDPOINT` | `https://vcd.example.com` | `kubectl get cm vcloud-ccm-configmap -n kube-system -o jsonpath='{.data.vcloud-ccm-config\.yaml}' \| grep host` |
| `VCD_ORG` | `dbaas` | same configmap → `org:` field |
| `VCD_USER` | `<vcd-user>` | `kubectl get secret vcloud-basic-auth -n kube-system -o jsonpath='{.data.username}' \| base64 -d` → strip the `org/` prefix (`dbaas/<vcd-user>` → `<vcd-user>`) |
| `VCD_PASSWORD` | *(your password)* | known to the operator |

---

## Deployment flags (`deploy/deployment.yaml`)

| Flag | Value | How to find |
|------|-------|-------------|
| `--cluster-id` | `capvcdCluster:bc77c367-851b-4bd6-b879-8832657025d8` | `kubectl get secret vcloud-clusterid-secret -n kube-system -o jsonpath='{.data.clusterid}' \| base64 -d` → outputs `urn:vcloud:entity:vmware:capvcdCluster:<uuid>`. Take only the **`capvcdCluster:<uuid>`** suffix — that is what the CPI embeds in VCD object names. |
| `--edge-gateway-id` | `urn:vcloud:gateway:cb64f385-38ee-4a1f-b954-866e087a5094` | VCD UI → Networking → Edge Gateways → select the gateway → copy the URN from the browser URL or details pane. Not stored in cluster config. |
| `--interval` | `60s` | operator choice |
| `--dry-run` | `true` (start here) | flip to `false` only after verifying logs |
| `--skip-dnat` | `false` | check `vcloud-ccm-configmap` → `enableVirtualServiceSharedIP`; if `true`, set this flag |

---

## Notes

### cluster-id format
The secret stores the full VCD entity URN:
```
urn:vcloud:entity:vmware:capvcdCluster:bc77c367-851b-4bd6-b879-8832657025d8
```
The CPI embeds only `capvcdCluster:<uuid>` in VCD object names, so the GC `--cluster-id` must use that shorter form. Before flipping `--dry-run=false`, open the VCD UI → Networking → Virtual Services and confirm that an existing VS name contains `capvcdCluster:bc77c367-851b-4bd6-b879-8832657025d8` (and not the full URN).

### edge-gateway-id
This value is not stored anywhere in the Kubernetes cluster. Source it from the VCD portal or ask the cloud tenant admin. The value already in `deploy/deployment.yaml` (`urn:vcloud:gateway:cb64f385-38ee-4a1f-b954-866e087a5094`) was set manually during initial setup.
