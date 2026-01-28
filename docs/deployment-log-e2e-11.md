# Stargate AKS E2E Deployment Log

**Cluster Name:** stargate-aks-e2e-11  
**Resource Group:** stargate-aks-e2e-11  
**DC Resource Group:** stargate-aks-e2e-11-dc  
**Location:** canadacentral  
**Date Started:** 2026-01-28

---

## Prerequisites Checklist

- [ ] Azure CLI logged in
- [ ] kubectl configured
- [ ] TAILSCALE_AUTH_KEY set (new tailnet)
- [ ] TAILSCALE_CLIENT_ID set (new tailnet)
- [ ] TAILSCALE_CLIENT_SECRET set (new tailnet)
- [ ] TAILSCALE_API_KEY set (for cleanup - generate at https://login.tailscale.com/admin/settings/keys)
- [ ] Local machine joined to new tailnet
- [ ] Binaries built (`make build`)

---

## Step 0: Build Binaries

```bash
# Kill any existing controller processes
pkill -f azure-controller || true

make build
```

**Status:** ✅ Complete  
**Output:**
```
go build -o bin/azure-controller ./main.go
go build -o bin/qemu-controller ./cmd/qemu-controller/main.go
go build -o bin/mockapi ./mockapi/main.go
go build -o bin/simulator ./cmd/simulator/main.go
go build -o bin/prep-dc-inventory ./cmd/infra-prep/main.go
go build -o bin/azure ./cmd/azure/main.go
go build -o bin/mx-azure ./cmd/mx-azure/main.go
```

---

## Step 1: Create Resource Group

```bash
az group create --name stargate-aks-e2e-11 --location canadacentral
```

**Status:** ✅ Complete  
**Output:**
```json
{
  "id": "/subscriptions/44654aed-2753-4b88-9142-af7132933b6b/resourceGroups/stargate-aks-e2e-11",
  "location": "canadacentral",
  "name": "stargate-aks-e2e-11",
  "properties": { "provisioningState": "Succeeded" }
}
```

---

## Step 2: Create AKS Cluster

> **Note:** Config derived from working cluster `aks-vapa-dev-1`. Must include `--network-plugin-mode overlay` when using `--pod-cidr` with Azure CNI.

```bash
az aks create \
  --resource-group stargate-aks-e2e-11 \
  --name stargate-aks-e2e-11 \
  --kubernetes-version 1.33.5 \
  --node-count 2 \
  --node-vm-size Standard_D2ads_v5 \
  --network-plugin azure \
  --network-plugin-mode overlay \
  --network-policy cilium \
  --network-dataplane cilium \
  --pod-cidr 10.244.0.0/16 \
  --service-cidr 10.0.0.0/16 \
  --generate-ssh-keys
```

**Status:** ✅ Complete  
**Output:**
```
provisioningState: Succeeded
kubernetesVersion: 1.33.5
fqdn: stargate-a-stargate-aks-e2e-44654a-6b15sns4.hcp.canadacentral.azmk8s.io
nodeResourceGroup: MC_stargate-aks-e2e-11_stargate-aks-e2e-11_canadacentral
podCidr: 10.244.0.0/16
serviceCidr: 10.0.0.0/16
```

---

## Step 3: Get AKS Credentials

```bash
az aks get-credentials --resource-group stargate-aks-e2e-11 --name stargate-aks-e2e-11 --overwrite-existing
```

**Status:** ✅ Complete  
**Output:**
```
Merged "stargate-aks-e2e-11" as current context in /home/vapa/.kube/config
```

---

## Step 4: Verify Cluster Access

```bash
kubectl get nodes
kubectl cluster-info
```

**Status:** ✅ Complete  
**Output:**
```
NAME                                STATUS   ROLES    AGE    VERSION
aks-nodepool1-37124291-vmss000000   Ready    <none>   6m     v1.33.5
aks-nodepool1-37124291-vmss000001   Ready    <none>   6m1s   v1.33.5

Kubernetes control plane is running at https://stargate-a-stargate-aks-e2e-44654a-6b15sns4.hcp.canadacentral.azmk8s.io:443
```

---

## Step 5: Install Stargate CRDs

```bash
kubectl apply -f config/crd/bases/
```

**Status:** ✅ Complete  
**Output:**
```
customresourcedefinition.apiextensions.k8s.io/operations.stargate.io created
customresourcedefinition.apiextensions.k8s.io/provisioningprofiles.stargate.io created
customresourcedefinition.apiextensions.k8s.io/servers.stargate.io created
```

---

## Step 6: Create Stargate Namespace

```bash
kubectl create namespace stargate
```

**Status:** ✅ Complete  
**Output:**
```
namespace/stargate created
```

---

## Step 7: Provision AKS Router

> **Note:** Use `-aks-subnet-cidr 10.237.0.0/24` to avoid overlap with existing AKS subnets (10.224.0.0/16, 10.238.0.0/24, 10.239.0.0/16). Routes are automatically approved via Tailscale API.

```bash
./bin/prep-dc-inventory \
  -role aks-router \
  -resource-group stargate-aks-e2e-11 \
  -aks-cluster-name stargate-aks-e2e-11 \
  -aks-router-name stargate-aks-e2e-11-router \
  -aks-subnet-cidr 10.237.0.0/24 \
  -location canadacentral
```

**Status:** ✅ Complete  
**Output:**
```
[aks] detected: VNet=aks-vnet-11486953 RG=MC_stargate-aks-e2e-11_stargate-aks-e2e-11_canadacentral
CIDRs=[10.224.0.0/12 10.0.0.0/16]
[tailscale-api] routes enabled for device 3416566641323576: [10.224.0.0/12 10.0.0.0/16]
AKS router ready and reachable.
  stargate-aks-e2e-11-router: TailscaleIP=100.127.209.84 PublicIP=20.63.78.178 PrivateIP=10.237.0.4
```

---

## Step 8: Create DC Resource Group

> **Important:** DC infrastructure must be in a **separate** resource group to simulate an actual datacenter.

```bash
az group create --name stargate-aks-e2e-11-dc --location canadacentral
```

**Status:** ✅ Complete  
**Output:**
```json
{
  "id": "/subscriptions/44654aed-2753-4b88-9142-af7132933b6b/resourceGroups/stargate-aks-e2e-11-dc",
  "location": "canadacentral",
  "name": "stargate-aks-e2e-11-dc",
  "properties": { "provisioningState": "Succeeded" }
}
```

---

## Step 9: Provision DC Infrastructure (Router + Workers)

```bash
./bin/prep-dc-inventory \
  -role dc \
  -resource-group stargate-aks-e2e-11-dc \
  -aks-cluster-name stargate-aks-e2e-11 \
  -router-name stargate-aks-e2e-11-dc-router \
  -vm stargate-aks-e2e-11-worker-1 \
  -vm stargate-aks-e2e-11-worker-2 \
  -location canadacentral
```

**Status:** ✅ Complete  
**Output:**
```
[aks] detected CIDRs for worker routing: [10.224.0.0/12 10.0.0.0/16]
[connectivity] router stargate-aks-e2e-11-dc-router tailscale IP: 100.74.211.119
[tailscale-api] routes enabled for device 3024075417041169: [10.50.1.0/24]
[aks-rt] route table created: stargate-workers-rt
[aks-rt] adding route to-workers -> 10.50.0.0/16 via 10.237.0.4...
[aks-rt] adding DC worker pod routes (10.244.55.0/24 - 10.244.60.0/24) via 10.237.0.4...
[aks-rt] route table associated with AKS subnet
[cilium] patched CiliumNode aks-nodepool1-37124291-vmss000000 with podCIDR 10.244.0.0/24
[cilium] patched CiliumNode aks-nodepool1-37124291-vmss000001 with podCIDR 10.244.1.0/24
[server-cr] created Server azure-dc/stargate-aks-e2e-11-worker-1 (MAC: 60:45:bd:5f:83:02)
[server-cr] created Server azure-dc/stargate-aks-e2e-11-worker-2 (MAC: 60:45:bd:5f:e5:be)
Infrastructure ready and reachable.
```

> **Bug Fixed:** Initial run failed with `RouteConflict` - `derivePodCIDR()` was returning the node's /24 subnet instead of 10.244.x.0/24. Fixed to match bootstrap script formula: `(third_octet*10 + fourth_octet) % 200 + 50`.

---

## Step 10: ~~Approve DC Router Routes in Tailscale~~

> **Note:** Routes are now automatically approved in Step 9 via Tailscale API. This step is no longer needed.

**Status:** ⏳ Skipped (automated in Step 9)  

---

## Step 11: Create Bootstrap Token

> **TODO:** Delete this step if SA token approach works. The controller uses ServiceAccount tokens for AKS mode, not bootstrap tokens.

```bash
# Generate token components
TOKEN_ID=$(cat /dev/urandom | tr -dc 'a-z0-9' | head -c 6)
TOKEN_SECRET=$(cat /dev/urandom | tr -dc 'a-z0-9' | head -c 16)
BOOTSTRAP_TOKEN="${TOKEN_ID}.${TOKEN_SECRET}"

echo "Bootstrap Token: $BOOTSTRAP_TOKEN"

# Create bootstrap token secret
kubectl apply -f - <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: bootstrap-token-${TOKEN_ID}
  namespace: kube-system
type: bootstrap.kubernetes.io/token
stringData:
  token-id: "${TOKEN_ID}"
  token-secret: "${TOKEN_SECRET}"
  usage-bootstrap-authentication: "true"
  usage-bootstrap-signing: "true"
  auth-extra-groups: "system:bootstrappers:worker"
EOF
```

**Status:** ✅ Complete  
**Bootstrap Token:**
```
1nso67.uwm4qeae0ncmw8jj
```

---

## Step 12: Create Secrets and ProvisioningProfile

> **Note:** The controller needs SSH credentials and a ProvisioningProfile to repave workers.

```bash
# Create SSH credentials secret (uses default ~/.ssh/id_rsa)
kubectl apply -f - <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: azure-ssh-credentials
  namespace: azure-dc
type: Opaque
stringData:
  username: ubuntu
  privateKey: |
$(cat ~/.ssh/id_rsa | sed 's/^/    /')
EOF

# Create Tailscale auth secret
kubectl apply -f - <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: tailscale-auth
  namespace: azure-dc
type: Opaque
stringData:
  authKey: "${TAILSCALE_AUTH_KEY}"
EOF

# Create ProvisioningProfile
kubectl apply -f - <<EOF
apiVersion: stargate.io/v1alpha1
kind: ProvisioningProfile
metadata:
  name: azure-k8s-worker
  namespace: azure-dc
spec:
  kubernetesVersion: "1.33"
  containerRuntime: containerd
  sshCredentialsSecretRef: azure-ssh-credentials
  tailscaleAuthKeySecretRef: tailscale-auth
  adminUsername: ubuntu
EOF
```

**Status:** ✅ Complete  
**Output:**
```
secret/azure-ssh-credentials created
secret/tailscale-auth created
provisioningprofile.stargate.io/azure-k8s-worker created
```

---

## Step 12a: Create kubelet-bootstrap ServiceAccount

> **Note:** The controller uses ServiceAccount tokens (not bootstrap tokens) for AKS mode. The SA needs permission to register nodes.

```bash
# Create ServiceAccount
kubectl create serviceaccount kubelet-bootstrap -n kube-system

# Bind to node-bootstrapper role (allows creating CSRs and nodes)
kubectl create clusterrolebinding kubelet-bootstrap \
  --clusterrole=system:node-bootstrapper \
  --serviceaccount=kube-system:kubelet-bootstrap

# Also need permission to get/update nodes for kubelet to function
kubectl create clusterrolebinding kubelet-bootstrap-node \
  --clusterrole=system:node \
  --serviceaccount=kube-system:kubelet-bootstrap
```

**Status:** ✅ Complete  
**Output:**
```
serviceaccount/kubelet-bootstrap created
clusterrolebinding.rbac.authorization.k8s.io/kubelet-bootstrap created
clusterrolebinding.rbac.authorization.k8s.io/kubelet-bootstrap-node created
```

---

## Step 13: Create Operation CRs for Workers

```bash
# For worker-1
kubectl apply -f - <<EOF
apiVersion: stargate.io/v1alpha1
kind: Operation
metadata:
  name: stargate-aks-e2e-11-worker-1-repave
  namespace: azure-dc
spec:
  serverRef:
    name: stargate-aks-e2e-11-worker-1
  provisioningProfileRef:
    name: azure-k8s-worker
  operation: repave
EOF

# For worker-2
kubectl apply -f - <<EOF
apiVersion: stargate.io/v1alpha1
kind: Operation
metadata:
  name: stargate-aks-e2e-11-worker-2-repave
  namespace: azure-dc
spec:
  serverRef:
    name: stargate-aks-e2e-11-worker-2
  provisioningProfileRef:
    name: azure-k8s-worker
  operation: repave
EOF
```

**Status:** ✅ Complete  
**Output:**
```
operation.stargate.io/stargate-aks-e2e-11-worker-1-repave created
operation.stargate.io/stargate-aks-e2e-11-worker-2-repave created

NAME                                  SERVER                         OPERATION   PHASE   AGE
stargate-aks-e2e-11-worker-1-repave   stargate-aks-e2e-11-worker-1   repave              3s
stargate-aks-e2e-11-worker-2-repave   stargate-aks-e2e-11-worker-2   repave              3s
```

---

## Step 14: Run Azure Controller

> **Note:** The controller needs multiple flags for AKS mode to SSH to workers via DC router and perform TLS bootstrap.
> 
> **Important:** The Server objects must have `routerIP` set to the Tailscale IP (100.65.32.1), not the hostname. If SSH fails with "Could not resolve hostname", patch the servers:
> ```bash
> kubectl patch server stargate-aks-e2e-11-worker-1 -n azure-dc --type='merge' -p '{"spec":{"routerIP":"100.65.32.1"}}'
> kubectl patch server stargate-aks-e2e-11-worker-2 -n azure-dc --type='merge' -p '{"spec":{"routerIP":"100.65.32.1"}}'
> ```

```bash
nohup ./bin/azure-controller \
  -control-plane-mode aks \
  -enable-route-sync \
  -aks-api-server "https://stargate-a-stargate-aks-e2e-1144654a-j2lo86eb.hcp.canadacentral.azmk8s.io:443" \
  -aks-cluster-name stargate-aks-e2e-11 \
  -aks-resource-group stargate-aks-e2e-11 \
  -aks-node-resource-group MC_stargate-aks-e2e-11_stargate-aks-e2e-11_canadacentral \
  -aks-subscription-id 44654aed-2753-4b88-9142-af7132933b6b \
  -aks-vm-resource-group stargate-aks-e2e-11-dc \
  -dc-router-tailscale-ip 100.65.32.1 \
  -aks-router-tailscale-ip 100.125.241.110 \
  -aks-router-private-ip 10.237.0.4 \
  -azure-route-table-name stargate-workers-rt \
  -router-route-table-name stargate-router-rt \
  -router-subnet-name stargate-aks-router-subnet \
  -azure-vnet-name aks-vnet-11486953 \
  -dc-subnet-cidr 10.50.0.0/16 \
  -tailscale-client-id "$TAILSCALE_CLIENT_ID" \
  -tailscale-client-secret "$TAILSCALE_CLIENT_SECRET" \
  > /tmp/azure-controller.log 2>&1 &

# Check controller is running
sleep 2 && pgrep -f azure-controller && echo "Controller started in background"
# View logs with: tail -f /tmp/azure-controller.log
```

**Controller Flags Reference:**

| Flag | Description | Example Value |
|------|-------------|---------------|
| `-control-plane-mode` | Control plane mode: `aks` or `self-hosted` | `aks` |
| `-enable-route-sync` | Enable automatic Azure route table and Tailscale route sync | (boolean flag) |
| `-aks-api-server` | AKS API server URL | `https://...hcp.canadacentral.azmk8s.io:443` |
| `-aks-cluster-name` | AKS cluster name | `stargate-aks-e2e-11` |
| `-aks-resource-group` | AKS cluster resource group | `stargate-aks-e2e-11` |
| `-aks-node-resource-group` | AKS managed infrastructure resource group (MC_*) | `MC_stargate-aks-e2e-11_...` |
| `-aks-subscription-id` | Azure subscription ID | `44654aed-...` |
| `-aks-vm-resource-group` | DC worker VMs resource group | `stargate-aks-e2e-11-dc` |
| `-dc-router-tailscale-ip` | DC router Tailscale IP | `100.65.32.1` |
| `-aks-router-tailscale-ip` | AKS router Tailscale IP | `100.125.241.110` |
| `-aks-router-private-ip` | AKS router private IP (next hop for routes) | `10.237.0.4` |
| `-azure-route-table-name` | Route table for AKS→DC traffic | `stargate-workers-rt` |
| `-router-route-table-name` | Route table for DC→AKS return traffic | `stargate-router-rt` |
| `-router-subnet-name` | AKS router subnet name | `stargate-aks-router-subnet` |
| `-azure-vnet-name` | AKS VNet name | `aks-vnet-11486953` |
| `-dc-subnet-cidr` | DC network CIDR | `10.50.0.0/16` |
| `-tailscale-client-id` | Tailscale OAuth client ID | `$TAILSCALE_CLIENT_ID` |
| `-tailscale-client-secret` | Tailscale OAuth client secret | `$TAILSCALE_CLIENT_SECRET` |

**Status:** ✅ Complete  
**Output:**
```
2026-01-28T08:53:04-05:00       INFO    setup   starting manager
2026-01-28T08:53:04-05:00       INFO    Starting Controller     {"controller": "operation"}
2026-01-28T08:54:41-05:00       INFO    Bootstrap succeeded     {"server": "stargate-aks-e2e-11-worker-2"}
2026-01-28T08:56:12-05:00       INFO    Bootstrap succeeded     {"server": "stargate-aks-e2e-11-worker-1"}
```

---

## Step 15: Verify Workers Joined

```bash
kubectl get nodes
kubectl get operations -n azure-dc
```

**Status:** ✅ Complete  
**Output:**
```
NAME                                STATUS   ROLES    AGE     VERSION
aks-nodepool1-37124291-vmss000000   Ready    <none>   24m     v1.33.5
aks-nodepool1-37124291-vmss000001   Ready    <none>   24m     v1.33.5
stargate-aks-e2e-11-worker-1        Ready    <none>   2m47s   v1.33.7
stargate-aks-e2e-11-worker-2        Ready    <none>   4m23s   v1.33.7

NAME                                  SERVER                         OPERATION   PHASE       AGE
stargate-aks-e2e-11-worker-1-repave   stargate-aks-e2e-11-worker-1   repave      Succeeded   7m36s
stargate-aks-e2e-11-worker-2-repave   stargate-aks-e2e-11-worker-2   repave      Succeeded   7m36s
```

✅ **Deployment Status:** Complete

---

## Step 16: Deploy Goldpinger for Connectivity Testing

```bash
# Deploy goldpinger DaemonSet
kubectl create namespace goldpinger
kubectl apply -f - <<'EOF'
apiVersion: v1
kind: ServiceAccount
metadata:
  name: goldpinger
  namespace: goldpinger
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: goldpinger
rules:
- apiGroups: [""]
  resources: ["pods"]
  verbs: ["list"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: goldpinger
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: goldpinger
subjects:
- kind: ServiceAccount
  name: goldpinger
  namespace: goldpinger
---
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: goldpinger
  namespace: goldpinger
spec:
  selector:
    matchLabels:
      app: goldpinger
  template:
    metadata:
      labels:
        app: goldpinger
    spec:
      serviceAccountName: goldpinger
      tolerations:
      - operator: Exists
      containers:
      - name: goldpinger
        image: bloomberg/goldpinger:v3.7.0
        env:
        - name: HOST
          value: "0.0.0.0"
        - name: PORT
          value: "8080"
        - name: HOSTNAME
          valueFrom:
            fieldRef:
              fieldPath: spec.nodeName
        - name: POD_IP
          valueFrom:
            fieldRef:
              fieldPath: status.podIP
        ports:
        - containerPort: 8080
        livenessProbe:
          httpGet:
            path: /healthz
            port: 8080
          initialDelaySeconds: 5
          periodSeconds: 10
        readinessProbe:
          httpGet:
            path: /healthz
            port: 8080
          initialDelaySeconds: 5
          periodSeconds: 10
---
apiVersion: v1
kind: Service
metadata:
  name: goldpinger
  namespace: goldpinger
spec:
  selector:
    app: goldpinger
  ports:
  - port: 8080
EOF

# Port forward to access the Goldpinger UI
# Note: If the service routes to a DC worker pod with networking issues, it may fail.
# In that case, specify an AKS node pod directly:
#   kubectl port-forward -n goldpinger pod/$(kubectl get pods -n goldpinger -o name | grep -m1 goldpinger) 8080:8080 &
kubectl port-forward -n goldpinger svc/goldpinger 8080:8080 &
```

**Status:** ✅ Complete  
**Output:**
```
NAME               READY   STATUS    RESTARTS   AGE   IP             NODE
goldpinger-5qqqc   1/1     Running   0          27s   10.244.66.2    stargate-aks-e2e-11-worker-2
goldpinger-h5rvg   1/1     Running   0          27s   10.244.65.2    stargate-aks-e2e-11-worker-1
goldpinger-wrzgr   1/1     Running   0          27s   10.244.0.120   aks-nodepool1-37124291-vmss000001
goldpinger-zmzpt   1/1     Running   0          27s   10.244.1.244   aks-nodepool1-37124291-vmss000000
```

**Connectivity Results:** ✅ All OK
- AKS → DC: goldpinger-wrzgr (10.244.0.120) → goldpinger-5qqqc (10.244.66.2): ✅
- AKS → DC: goldpinger-zmzpt (10.244.1.244) → goldpinger-h5rvg (10.244.65.2): ✅  
- DC → AKS: goldpinger-5qqqc (10.244.66.2) → goldpinger-wrzgr (10.244.0.120): ✅
- DC → DC: goldpinger-h5rvg (10.244.65.2) → goldpinger-5qqqc (10.244.66.2): ✅



## Cleanup (when done)

```bash
# Kill any running port-forwards and controllers
pkill -f "kubectl port-forward" || true
pkill -f azure-controller || true

# Delete Azure resources
az aks delete --name stargate-aks-e2e-11 --resource-group stargate-aks-e2e-11 --yes --no-wait
az group delete --name stargate-aks-e2e-11-dc --yes --no-wait
az group delete --name stargate-aks-e2e-11 --yes --no-wait

# Remove Tailscale devices with "stargate" in the name
# Requires TAILSCALE_API_KEY to be set (generate at https://login.tailscale.com/admin/settings/keys)
DEVICES=$(curl -s -H "Authorization: Bearer ${TAILSCALE_API_KEY}" \
  "https://api.tailscale.com/api/v2/tailnet/-/devices" 2>/dev/null | \
  jq -r '.devices[]? | select(.hostname | contains("stargate")) | .id' 2>/dev/null || echo "")
for device_id in $DEVICES; do
  echo "Deleting Tailscale device: $device_id"
  curl -s -X DELETE -H "Authorization: Bearer ${TAILSCALE_API_KEY}" \
    "https://api.tailscale.com/api/v2/device/$device_id"
done
```
