# Test: LB Port Removal Cleanup in VMware Cloud Director

Goal: verify that removing a port from a LoadBalancer Service deletes corresponding VCD objects (pools, virtual services).

---

## Step 1 — Initial deploy: 1 deployment, 1-port LB

```yaml
# deploy-a.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: app-a
  namespace: vcd
spec:
  replicas: 1
  selector:
    matchLabels:
      app: app-a
  template:
    metadata:
      labels:
        app: app-a
    spec:
      containers:
      - name: app-a
        image: nginx:alpine
        ports:
        - containerPort: 80
```

```yaml
# lb-svc.yaml  (v1 — 1 port)
apiVersion: v1
kind: Service
metadata:
  name: lb-test
  namespace: vcd
spec:
  type: LoadBalancer
  selector:
    app: app-a
  ports:
  - name: http
    port: 80
    targetPort: 80
    protocol: TCP
```

```bash
kubectl apply -f deploy-a.yaml
kubectl apply -f lb-svc.yaml
kubectl get svc lb-test -w   # wait for EXTERNAL-IP
```

**VCD check**: 1 Virtual Service + 1 Pool created for `lb-test`.

---

## Step 2 — Add second deployment + second port

```yaml
# deploy-b.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: app-b
  namespace: vcd
spec:
  replicas: 1
  selector:
    matchLabels:
      app: app-b
  template:
    metadata:
      labels:
        app: app-b
    spec:
      containers:
      - name: app-b
        image: hashicorp/http-echo:latest
        args: ["-listen=:8080", "-text=app-b"]
        ports:
        - containerPort: 8080
```

```yaml
# lb-svc.yaml  (v2 — 2 ports)
apiVersion: v1
kind: Service
metadata:
  name: lb-test
  namespace: vcd
spec:
  type: LoadBalancer
  selector:
    app: app-a
  ports:
  - name: http
    port: 80
    targetPort: 80
    protocol: TCP
  - name: http-alt
    port: 8080
    targetPort: 8080
    protocol: TCP
```

```bash
kubectl apply -f deploy-b.yaml
kubectl apply -f lb-svc.yaml
kubectl get svc lb-test
```

**VCD check**: 2 Virtual Services + 2 Pools now exist.

---

## Step 3 — Remove one port (the bug under test)

Revert `lb-svc.yaml` back to v1 (only port 80), then:

```bash
kubectl apply -f lb-svc.yaml   # v1 with only port 80
kubectl get svc lb-test
```

**VCD check**:
- **Expected**: Virtual Service + Pool for port 8080 are deleted
- **Actual (bug)**: They remain as orphaned objects

---

## Step 4 — Full cleanup

```bash
kubectl delete svc lb-test
kubectl delete -f deploy-a.yaml -f deploy-b.yaml
```

**VCD check**: All LB objects for `lb-test` removed.
