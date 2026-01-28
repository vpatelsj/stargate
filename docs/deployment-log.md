# Stargate AKS E2E Deployment Log

**Cluster Name:** stargate-aks-e2e-2  
**Resource Group:** stargate-aks-e2e-2  
**DC Resource Group:** stargate-aks-e2e-2-dc  
**Location:** canadacentral  
**Date Started:** 2026-01-27

---

## Prerequisites Checklist

- [ ] Azure CLI logged in
- [ ] kubectl configured
- [ ] TAILSCALE_AUTH_KEY set (new tailnet)
- [ ] TAILSCALE_CLIENT_ID set (new tailnet)
- [ ] TAILSCALE_CLIENT_SECRET set (new tailnet)
- [ ] Local machine joined to new tailnet
- [ ] Binaries built (`make build`)

---

## Step 0a: Join Local Machine to Tailnet

> **Important:** Your local machine must be on the same tailnet as the routers/workers for `tailscale ping` to work.

```bash
sudo tailscale logout
sudo tailscale up --authkey $TAILSCALE_AUTH_KEY
tailscale status
```

**Status:** ⏳ Pending  
**Output:**
```
# paste output here
```

---

## Step 0b: Build Binaries

```bash
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
az group create --name stargate-aks-e2e-2 --location canadacentral
```

**Status:** ✅ Complete  
**Output:**
```json
{
  "id": "/subscriptions/44654aed-2753-4b88-9142-af7132933b6b/resourceGroups/stargate-aks-e2e-2",
  "location": "canadacentral",
  "name": "stargate-aks-e2e-2",
  "properties": { "provisioningState": "Succeeded" }
}
```

---

## Step 2: Create AKS Cluster

> **Note:** Config derived from working cluster `aks-vapa-dev-1`. Must include `--network-plugin-mode overlay` when using `--pod-cidr` with Azure CNI.

```bash
az aks create \
  --resource-group stargate-aks-e2e-2 \
  --name stargate-aks-e2e-2 \
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
fqdn: stargate-a-stargate-aks-e2e-44654a-2vsms81u.hcp.canadacentral.azmk8s.io
```

---

## Step 3: Get AKS Credentials

```bash
az aks get-credentials --resource-group stargate-aks-e2e-2 --name stargate-aks-e2e-2 --overwrite-existing
```

**Status:** ✅ Complete  
**Output:**
```
Merged "stargate-aks-e2e-2" as current context in /home/vapa/.kube/config
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
NAME                                STATUS   ROLES    AGE     VERSION
aks-nodepool1-29915583-vmss000000   Ready    <none>   3m28s   v1.33.5
aks-nodepool1-29915583-vmss000001   Ready    <none>   3m26s   v1.33.5

Kubernetes control plane is running at https://stargate-a-stargate-aks-e2e-44654a-2vsms81u.hcp.canadacentral.azmk8s.io:443
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
  -resource-group stargate-aks-e2e-2 \
  -aks-cluster-name stargate-aks-e2e-2 \
  -aks-router-name stargate-aks-e2e-2-router \
  -aks-subnet-cidr 10.237.0.0/24 \
  -location canadacentral
```

**Status:** ✅ Complete  
**Output:**
```
[aks] detected: VNet=aks-vnet-11486953 RG=MC_stargate-aks-e2e-2_stargate-aks-e2e-2_canadacentral
CIDRs=[10.224.0.0/12 10.244.0.0/16 10.0.0.0/16]
Routes approved: 10.224.0.0/12, 10.244.0.0/16, 10.0.0.0/16
AKS router ready and reachable.
```

---

## Step 8: Create DC Resource Group

> **Important:** DC infrastructure must be in a **separate** resource group to simulate an actual datacenter.

```bash
az group create --name stargate-aks-e2e-2-dc --location canadacentral
```

**Status:** ✅ Complete  
**Output:**
```json
{
  "id": "/subscriptions/44654aed-2753-4b88-9142-af7132933b6b/resourceGroups/stargate-aks-e2e-2-dc",
  "location": "canadacentral",
  "name": "stargate-aks-e2e-2-dc",
  "properties": { "provisioningState": "Succeeded" }
}
```

---

## Step 9: Provision DC Infrastructure (Router + Workers)

```bash
./bin/prep-dc-inventory \
  -role dc \
  -resource-group stargate-aks-e2e-2-dc \
  -aks-cluster-name stargate-aks-e2e-2 \
  -router-name stargate-aks-e2e-2-dc-router \
  -vm stargate-aks-e2e-2-worker-1 \
  -vm stargate-aks-e2e-2-worker-2 \
  -location canadacentral
```

**Status:** ✅ Complete  
**Output:**
```
[aks] detected CIDRs for worker routing: [10.224.0.0/12 10.244.0.0/16 10.0.0.0/16]
[connectivity] router stargate-aks-e2e-2-dc-router tailscale IP: 100.105.92.5
[tailscale-api] routes enabled for device 3124758845656288: [10.50.1.0/24]
[aks-rt] route table created, associated with AKS subnet
[server-cr] created Server azure-dc/stargate-aks-e2e-2-worker-1 (MAC: 70:a8:a5:0b:23:d9)
[server-cr] created Server azure-dc/stargate-aks-e2e-2-worker-2 (MAC: 7c:ed:8d:65:97:fa)
Infrastructure ready and reachable.
```

---

## Step 10: ~~Approve DC Router Routes in Tailscale~~

> **Note:** Routes are now automatically approved in Step 9 via Tailscale API. This step is no longer needed.

**Status:** ✅ Skipped (automated)

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
4zwwrn.nw0gnd1gusa6nxn9
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
namespace/azure-dc configured
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
  name: stargate-aks-e2e-2-worker-1-repave
  namespace: azure-dc
spec:
  serverRef:
    name: stargate-aks-e2e-2-worker-1
  provisioningProfileRef:
    name: azure-k8s-worker
  operation: repave
EOF

# For worker-2
kubectl apply -f - <<EOF
apiVersion: stargate.io/v1alpha1
kind: Operation
metadata:
  name: stargate-aks-e2e-2-worker-2-repave
  namespace: azure-dc
spec:
  serverRef:
    name: stargate-aks-e2e-2-worker-2
  provisioningProfileRef:
    name: azure-k8s-worker
  operation: repave
EOF
```

**Status:** ✅ Complete  
**Output:**
```
operation.stargate.io/stargate-aks-e2e-2-worker-1-repave created
operation.stargate.io/stargate-aks-e2e-2-worker-2-repave created

NAME                                 SERVER                        OPERATION   PHASE   AGE
stargate-aks-e2e-2-worker-1-repave   stargate-aks-e2e-2-worker-1   repave              12s
stargate-aks-e2e-2-worker-2-repave   stargate-aks-e2e-2-worker-2   repave              5s
```

---

## Step 14: Run Azure Controller

> **Note:** The controller needs multiple flags for AKS mode to SSH to workers via DC router and perform TLS bootstrap.
> 
> **Important:** The Server objects must have `routerIP` set to the Tailscale IP (100.105.92.5), not the hostname. If SSH fails with "Could not resolve hostname", patch the servers:
> ```bash
> kubectl patch server stargate-aks-e2e-2-worker-1 -n azure-dc --type='merge' -p '{"spec":{"routerIP":"100.105.92.5"}}'
> kubectl patch server stargate-aks-e2e-2-worker-2 -n azure-dc --type='merge' -p '{"spec":{"routerIP":"100.105.92.5"}}'
> ```

```bash
./bin/azure-controller \
  -control-plane-mode aks \
  -aks-api-server "https://stargate-a-stargate-aks-e2e-44654a-j2lo86eb.hcp.canadacentral.azmk8s.io:443" \
  -aks-cluster-name stargate-aks-e2e-2 \
  -aks-resource-group stargate-aks-e2e-2 \
  -aks-subscription-id 44654aed-2753-4b88-9142-af7132933b6b \
  -aks-vm-resource-group stargate-aks-e2e-2-dc \
  -dc-router-tailscale-ip 100.105.92.5 \
  -aks-router-tailscale-ip 100.119.186.117
```

**Status:** ✅ Complete  
**Output:**
```
2026-01-27T17:57:26-05:00       INFO    setup   starting manager
2026-01-27T17:57:26-05:00       INFO    Starting Controller     {"controller": "operation"}
2026-01-27T17:57:26-05:00       INFO    Starting workers        {"controller": "operation", "worker count": 1}
2026-01-27T17:57:26-05:00       INFO    Initiating repave via SSH bootstrap     {"server": "stargate-aks-e2e-2-worker-2", "ipv4": "10.50.1.6"}
2026-01-27T17:57:26-05:00       INFO    Building AKS bootstrap script   {"nodeIP": "10.50.1.6", "vmName": "stargate-aks-e2e-2-worker-2"}
2026-01-27T17:57:50-05:00       INFO    Bootstrap script output written to /tmp/bootstrap-output.log    {"bytes": 18794}
2026-01-27T17:57:50-05:00       INFO    Configuring routing for node    {"server": "stargate-aks-e2e-2-worker-2", "nodeIP": "10.50.1.6", "podCIDR": "10.244.66.0/24"}
2026-01-27T17:57:50-05:00       INFO    Bootstrap succeeded     {"server": "stargate-aks-e2e-2-worker-2"}
2026-01-27T17:57:51-05:00       INFO    Initiating repave via SSH bootstrap     {"server": "stargate-aks-e2e-2-worker-1", "ipv4": "10.50.1.5"}
2026-01-27T17:57:51-05:00       INFO    Building AKS bootstrap script   {"nodeIP": "10.50.1.5", "vmName": "stargate-aks-e2e-2-worker-1"}
...
2026-01-27T17:58:15-05:00       INFO    Bootstrap succeeded     {"server": "stargate-aks-e2e-2-worker-1"}
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
aks-nodepool1-10551117-vmss000000   Ready    <none>   4h39m   v1.33.5
aks-nodepool1-10551117-vmss000001   Ready    <none>   4h39m   v1.33.5
stargate-aks-e2e-2-worker-1         Ready    <none>   18s     v1.33.7
stargate-aks-e2e-2-worker-2         Ready    <none>   42s     v1.33.7

NAME                                 SERVER                        OPERATION   PHASE       AGE
stargate-aks-e2e-2-worker-1-repave   stargate-aks-e2e-2-worker-1   repave      Succeeded   69s
stargate-aks-e2e-2-worker-2-repave   stargate-aks-e2e-2-worker-2   repave      Succeeded   69s
```

🎉 **Deployment Complete!** DC workers successfully joined the AKS cluster.

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
```

**Status:** ✅ Complete  
**Output:**
```
NAME               READY   STATUS    RESTARTS   AGE     IP             NODE
goldpinger-68zzs   1/1     Running   0          4m22s   10.244.0.139   aks-nodepool1-10551117-vmss000001
goldpinger-86t5l   1/1     Running   0          14s     10.244.66.2    stargate-aks-e2e-2-worker-2
goldpinger-bpgrf   1/1     Running   0          4m23s   10.244.1.224   aks-nodepool1-10551117-vmss000000
goldpinger-fslw6   1/1     Running   0          15s     10.244.65.3    stargate-aks-e2e-2-worker-1
```

---

## Notes & Issues

### Issue 1: CNI Configuration for DC Workers

**Problem:** DC workers were stuck in `ContainerCreating` with error: `failed to find network info for sandbox`

**Root Cause:** AKS uses Cilium with `ipam: delegated-plugin` which relies on Azure CNS. Azure CNS doesn't create `NodeNetworkConfig` CRs for non-AKS nodes.

**Solution:** Write a Cilium CNI config with `host-local` IPAM directly on DC workers:
```json
{
  "cniVersion": "0.3.1",
  "name": "cilium",
  "plugins": [{
    "type": "cilium-cni",
    "ipam": {
      "type": "host-local",
      "ranges": [[{"subnet": "10.244.65.0/24"}]]
    }
  }]
}
```

**Fix Applied:** Updated `controller/operation_controller.go` to write this CNI config automatically during bootstrap after the podCIDR is assigned.

### Issue 2: Server routerIP Must Be Tailscale IP

**Problem:** SSH failed with `Could not resolve hostname stargate-aks-e2e-2-dc-router`

**Root Cause:** `findRouterTarget()` in `cmd/infra-prep/main.go` preferred `TailnetFQDN` (hostname) over `TailscaleIP`. The hostname doesn't resolve from machines not on the tailnet.

**Fix Applied:** Updated `findRouterTarget()` to prefer `TailscaleIP` over `TailnetFQDN`:
```go
// Before: firstNonEmpty(n.TailnetFQDN, n.TailscaleIP, n.PublicIP)
// After:  firstNonEmpty(n.TailscaleIP, n.TailnetFQDN, n.PublicIP)
```

**Workaround (if using old binary):** Patch Server CRs to use DC router Tailscale IP:
```bash
kubectl patch server <name> -n azure-dc --type='merge' -p '{"spec":{"routerIP":"100.105.92.5"}}'
```

### Issue 3: Pod-to-Pod Routing Between AKS and DC Workers (TODO)

**Problem:** Goldpinger connectivity test shows AKS pods cannot reach DC worker pods:
- AKS → AKS: ✅ Works (10.244.0.x, 10.244.1.x)
- AKS → DC workers: ❌ Timeout (10.244.65.x, 10.244.66.x)

**Root Cause:** Multiple routing components need to be configured for pod traffic:

1. **Azure Route Table** - Only has route for DC worker VNet (`10.50.0.0/16`), missing pod CIDRs
2. **AKS Router** - No routes to forward pod CIDRs via Tailscale interface
3. **DC Router** - Not advertising pod CIDRs via Tailscale, and no routes to forward to workers
4. **Tailscale** - Pod CIDR routes not approved

The traffic path should be:
```
AKS pod → AKS node → Azure route table → AKS router (10.237.0.4) → Tailscale → DC router → DC worker
```

**Findings:**
- Cilium on AKS uses native routing - it sends traffic to node InternalIP (10.50.1.5) which works
- However, Cilium's ipcache shows `tunnelendpoint=10.50.1.5` for DC worker pods
- Node-to-node connectivity works (AKS node can ping DC worker node IP 10.50.1.5)
- Pod-to-pod fails because:
  - Azure route table doesn't route pod CIDRs to AKS router
  - AKS router doesn't know to forward pod CIDRs via Tailscale
  - DC router only advertises `10.50.1.0/24`, not pod CIDRs

**TODO:** Update the controller/prep-dc-inventory to:
1. Add Azure route table entries for each DC worker's podCIDR (in MC_... resource group, not DC resource group)
2. Add routes on AKS router for pod CIDRs via tailscale0 interface
3. Add routes on DC router for pod CIDRs to worker IPs
4. Advertise pod CIDRs via Tailscale on DC router and approve via API

**Manual Workaround:**
```bash
# 1. Add Azure route table entries for DC worker pod CIDRs
az network route-table route create \
  --resource-group MC_stargate-aks-e2e-2_stargate-aks-e2e-2_canadacentral \
  --route-table-name stargate-workers-rt \
  --name pod-cidr-worker1 \
  --address-prefix 10.244.65.0/24 \
  --next-hop-type VirtualAppliance \
  --next-hop-ip-address 10.237.0.4

az network route-table route create \
  --resource-group MC_stargate-aks-e2e-2_stargate-aks-e2e-2_canadacentral \
  --route-table-name stargate-workers-rt \
  --name pod-cidr-worker2 \
  --address-prefix 10.244.66.0/24 \
  --next-hop-type VirtualAppliance \
  --next-hop-ip-address 10.237.0.4

# 2. Add routes on AKS router (100.119.186.117 / 10.237.0.4) to forward pod CIDRs via Tailscale
ssh ubuntu@100.119.186.117 "
sudo ip route add 10.244.65.0/24 dev tailscale0
sudo ip route add 10.244.66.0/24 dev tailscale0
"

# 3. Add routes on DC router (100.105.92.5) to forward pod CIDRs to workers
ssh ubuntu@100.105.92.5 "
sudo ip route add 10.244.65.0/24 via 10.50.1.5
sudo ip route add 10.244.66.0/24 via 10.50.1.6
"

# 4. Advertise pod CIDRs via Tailscale on DC router
ssh ubuntu@100.105.92.5 "sudo tailscale set --advertise-routes=10.50.1.0/24,10.244.65.0/24,10.244.66.0/24"

# 5. Approve routes via Tailscale API (requires OAuth token)
TOKEN=$(curl -s -X POST "https://api.tailscale.com/api/v2/oauth/token" \
  -u "${TAILSCALE_CLIENT_ID}:${TAILSCALE_CLIENT_SECRET}" \
  -d "grant_type=client_credentials" | jq -r '.access_token')

# Get DC router device ID
DEVICE_ID=$(curl -s -X GET "https://api.tailscale.com/api/v2/tailnet/-/devices" \
  -H "Authorization: Bearer $TOKEN" | jq -r '.devices[] | select(.hostname == "stargate-aks-e2e-2-dc-router") | .id')

# Enable routes
curl -s -X POST "https://api.tailscale.com/api/v2/device/$DEVICE_ID/routes" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"routes": ["10.50.1.0/24", "10.244.65.0/24", "10.244.66.0/24"]}'
```

**Status:** Manual workaround applied but connectivity still not working.

### Issue 4: CiliumNode Resources Missing podCIDRs

**Problem:** Even after configuring routes, pod-to-pod connectivity still fails.

**Diagnosis:**
```bash
# Check Cilium node list - shows Endpoint CIDR is blank for DC workers
CILIUM_POD=$(kubectl get pods -n kube-system -l k8s-app=cilium --field-selector spec.nodeName=aks-nodepool1-10551117-vmss000000 -o jsonpath='{.items[0].metadata.name}')
kubectl exec -n kube-system "$CILIUM_POD" -c cilium-agent -- cilium node list

# Output BEFORE fix:
Name                                IPv4 Address   Endpoint CIDR   IPv6 Address   Endpoint CIDR   Source
aks-nodepool1-10551117-vmss000000   10.224.0.4     10.4.0.0/16                                    local
aks-nodepool1-10551117-vmss000001   10.224.0.5                                                    custom-resource
stargate-aks-e2e-2-worker-1         10.50.1.5                                                     custom-resource  # <-- BLANK!
stargate-aks-e2e-2-worker-2         10.50.1.6                                                     custom-resource  # <-- BLANK!
```

**Root Cause:** 
- AKS Cilium uses `ipam: delegated-plugin` mode (delegates IPAM to Azure CNI)
- With this mode, Cilium doesn't read `node.spec.podCIDR` from Kubernetes
- The CiliumNode resources for DC workers have `spec.ipam.podCIDRs: null`
- Without knowing the Endpoint CIDR, Cilium can't route traffic to those pods

**Verification:**
```bash
# Check CiliumNode resource - shows empty IPAM
kubectl get ciliumnode stargate-aks-e2e-2-worker-1 -o yaml
# Shows: spec.ipam.pools: {}

# Check Kubernetes node has podCIDR (it does)
kubectl get node stargate-aks-e2e-2-worker-1 -o jsonpath='{.spec.podCIDR}'
# Output: 10.244.65.0/24
```

**Fix Applied:** Patch CiliumNode resources to include podCIDRs:
```bash
kubectl patch ciliumnode stargate-aks-e2e-2-worker-1 --type=merge -p '{"spec":{"ipam":{"podCIDRs":["10.244.65.0/24"]}}}'
kubectl patch ciliumnode stargate-aks-e2e-2-worker-2 --type=merge -p '{"spec":{"ipam":{"podCIDRs":["10.244.66.0/24"]}}}'
```

**Verification after fix:**
```bash
# Now Cilium sees the Endpoint CIDRs
kubectl exec -n kube-system "$CILIUM_POD" -c cilium-agent -- cilium node list

# Output AFTER fix:
Name                                IPv4 Address   Endpoint CIDR    IPv6 Address   Endpoint CIDR   Source
aks-nodepool1-10551117-vmss000000   10.224.0.4     10.4.0.0/16                                     local
aks-nodepool1-10551117-vmss000001   10.224.0.5                                                     custom-resource
stargate-aks-e2e-2-worker-1         10.50.1.5      10.244.65.0/24                                  custom-resource  # <-- NOW HAS CIDR!
stargate-aks-e2e-2-worker-2         10.50.1.6      10.244.66.0/24                                  custom-resource  # <-- NOW HAS CIDR!
```

**TODO:** Update controller to automatically patch CiliumNode resources with podCIDRs when DC workers join the cluster. The controller should:
1. Watch for new Node resources with label `kubernetes.azure.com/stargate: "true"`
2. Read the `node.spec.podCIDR` value
3. Patch the corresponding CiliumNode with `spec.ipam.podCIDRs`

**Status:** CiliumNode patched, but connectivity still failing. Investigating further layers.

### Issue 5: Router Subnet Missing Route Table for Return Traffic

**Problem:** Even after applying Issues 3 and 4 fixes, pod-to-pod connectivity between AKS nodes and DC workers still fails. Traffic from AKS pods to DC worker pods times out.

**Diagnosis Method:** Compared configuration with a known working cluster (`aks-vapa-dev-1`).

```bash
# Check router subnet route table on WORKING cluster
az network vnet subnet show \
  --resource-group MC_aks-vapa-dev-1_aks-vapa-dev-1_canadacentral \
  --vnet-name aks-vnet-42162606 \
  --name stargate-aks-router-subnet -o json | jq '.routeTable'
# Output:
{
  "id": ".../routeTables/stargate-router-rt",
  "resourceGroup": "MC_aks-vapa-dev-1_aks-vapa-dev-1_canadacentral"
}

# Check router subnet route table on CURRENT cluster (broken)
az network vnet subnet show \
  --resource-group MC_stargate-aks-e2e-2_stargate-aks-e2e-2_canadacentral \
  --vnet-name aks-vnet-11486953 \
  --name stargate-aks-router-subnet -o json | jq '.routeTable'
# Output:
null  # <-- NO ROUTE TABLE!
```

**Root Cause:**
The working cluster has TWO route tables:

1. **`stargate-workers-rt`** (on `aks-subnet`) - Routes traffic FROM AKS nodes TO DC workers:
   - `10.50.0.0/16 → 10.237.0.4` (DC network via AKS router)
   - `10.244.65.0/24 → 10.237.0.4` (DC worker-1 pods via AKS router)
   - `10.244.66.0/24 → 10.237.0.4` (DC worker-2 pods via AKS router)

2. **`stargate-router-rt`** (on `stargate-aks-router-subnet`) - Routes return traffic FROM DC workers BACK to AKS pods:
   - `10.244.0.0/24 → 10.224.0.5` (AKS node 1 pods)
   - `10.244.1.0/24 → 10.224.0.4` (AKS node 0 pods)

The current cluster was **missing `stargate-router-rt`** on the router subnet. Without this route table:
- Traffic from AKS pods → DC worker pods works (routes exist on aks-subnet)
- Return traffic from DC worker pods → AKS pods **fails** because the AKS router doesn't know how to reach AKS pod CIDRs (10.244.0.0/24, 10.244.1.0/24)

**Traffic Flow Analysis:**
```
AKS Pod (10.244.1.x) → AKS Node (10.224.0.4) → AKS Router (10.237.0.4) → Tailscale → DC Router → DC Worker → DC Pod (10.244.65.x)
                                                                                                              ↓
                                   ❌ FAILS HERE - AKS Router doesn't know route to 10.244.1.0/24 ←←←←←←←← Reply
```

**Fix Applied:**
```bash
# 1. Create route table for router subnet
az network route-table create \
  --resource-group MC_stargate-aks-e2e-2_stargate-aks-e2e-2_canadacentral \
  --name stargate-router-rt \
  --location canadacentral

# 2. Add routes for AKS node pod CIDRs
# Node vmss000000 (10.224.0.4) has pods in 10.244.1.0/24
az network route-table route create \
  --resource-group MC_stargate-aks-e2e-2_stargate-aks-e2e-2_canadacentral \
  --route-table-name stargate-router-rt \
  --name pod-cidr-node-0 \
  --address-prefix 10.244.1.0/24 \
  --next-hop-type VirtualAppliance \
  --next-hop-ip-address 10.224.0.4

# Node vmss000001 (10.224.0.5) has pods in 10.244.0.0/24
az network route-table route create \
  --resource-group MC_stargate-aks-e2e-2_stargate-aks-e2e-2_canadacentral \
  --route-table-name stargate-router-rt \
  --name pod-cidr-node-1 \
  --address-prefix 10.244.0.0/24 \
  --next-hop-type VirtualAppliance \
  --next-hop-ip-address 10.224.0.5

# 3. Associate route table with router subnet
az network vnet subnet update \
  --resource-group MC_stargate-aks-e2e-2_stargate-aks-e2e-2_canadacentral \
  --vnet-name aks-vnet-11486953 \
  --name stargate-aks-router-subnet \
  --route-table stargate-router-rt
```

**Verification:**
```bash
az network route-table route list \
  --resource-group MC_stargate-aks-e2e-2_stargate-aks-e2e-2_canadacentral \
  --route-table-name stargate-router-rt -o table

# Output:
AddressPrefix    Name             NextHopIpAddress    NextHopType       ProvisioningState
---------------  ---------------  ------------------  ----------------  -------------------
10.244.1.0/24    pod-cidr-node-0  10.224.0.4          VirtualAppliance  Succeeded
10.244.0.0/24    pod-cidr-node-1  10.224.0.5          VirtualAppliance  Succeeded
```

**Complete Route Table Configuration:**
```
┌─────────────────────────────────────────────────────────────────────────────┐
│                         REQUIRED ROUTE TABLES                               │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                             │
│  1. stargate-workers-rt (on aks-subnet)                                     │
│     Purpose: Route outbound traffic FROM AKS nodes TO DC workers            │
│     ┌─────────────────────┬───────────────────────────────────────────────┐ │
│     │ Prefix              │ Next Hop (AKS Router)                         │ │
│     ├─────────────────────┼───────────────────────────────────────────────┤ │
│     │ 10.50.0.0/16        │ 10.237.0.4 (DC node network)                  │ │
│     │ 10.244.65.0/24      │ 10.237.0.4 (DC worker-1 pods)                 │ │
│     │ 10.244.66.0/24      │ 10.237.0.4 (DC worker-2 pods)                 │ │
│     └─────────────────────┴───────────────────────────────────────────────┘ │
│                                                                             │
│  2. stargate-router-rt (on stargate-aks-router-subnet)                      │
│     Purpose: Route return traffic FROM DC workers BACK to AKS pods          │
│     ┌─────────────────────┬───────────────────────────────────────────────┐ │
│     │ Prefix              │ Next Hop (AKS Node)                           │ │
│     ├─────────────────────┼───────────────────────────────────────────────┤ │
│     │ 10.244.0.0/24       │ 10.224.0.5 (vmss000001)                       │ │
│     │ 10.244.1.0/24       │ 10.224.0.4 (vmss000000)                       │ │
│     └─────────────────────┴───────────────────────────────────────────────┘ │
│                                                                             │
└─────────────────────────────────────────────────────────────────────────────┘
```

**TODO:** Update `prep-dc-inventory` (when role=aks-router) to:
1. Create `stargate-router-rt` route table in the MC_ resource group
2. Associate it with the router subnet
3. Add routes for each AKS node's pod CIDR pointing to the node's IP
4. Watch for AKS node scale events to add/remove routes dynamically

### Finding: DC worker NICs lacked IP forwarding (vs working cluster)

- Working cluster `aks-vapa-dev-1` worker NICs (`stargate-aks-vm2601222214-1-nic`, `-2-nic`) have `enableIPForwarding: true`.
- Broken cluster NICs (`stargate-aks-e2e-2-worker-1-nic`, `-2-nic`) had `enableIPForwarding: false`; enabling fixed acceptance of non-local pod traffic.
- Commands applied:
  - `az network nic update --resource-group STARGATE-AKS-E2E-2-DC --name stargate-aks-e2e-2-worker-1-nic --ip-forwarding true`
  - `az network nic update --resource-group STARGATE-AKS-E2E-2-DC --name stargate-aks-e2e-2-worker-2-nic --ip-forwarding true`

### Finding: DC subnet missing per-pod-CIDR UDRs to workers

- Working DC route table includes /24 pod CIDR routes to each worker (10.244.65.0/24 → worker-1, 10.244.66.0/24 → worker-2) in addition to broader routes.
- Broken cluster only had broad 10.244.0.0/16 to router; added explicit /24 routes restored AKS→DC pod ping.
- Commands applied:
  - `az network route-table route create --resource-group STARGATE-AKS-E2E-2-DC --route-table-name stargate-aks-e2e-2-dc-route-table --name pod-cidr-65 --address-prefix 10.244.65.0/24 --next-hop-type VirtualAppliance --next-hop-ip-address 10.50.1.5`
  - `az network route-table route create --resource-group STARGATE-AKS-E2E-2-DC --route-table-name stargate-aks-e2e-2-dc-route-table --name pod-cidr-66 --address-prefix 10.244.66.0/24 --next-hop-type VirtualAppliance --next-hop-ip-address 10.50.1.6`
  - Verification: AKS pod on `aks-nodepool1-10551117-vmss000001` → `10.244.65.3` now succeeds (5/5 ICMP replies).

**Status:** Route table created and associated, but still not working due to Issue 6.

### Issue 6: Cilium Native Routing Mode vs Azure UDRs

**Problem:** Even after adding all Azure route tables correctly, pod-to-pod connectivity between AKS nodes and DC workers still fails. ICMP packets never reach the AKS router.

**Root Cause Analysis:**

The issue is a fundamental conflict between **Cilium's native routing mode** and **Azure User Defined Routes (UDRs)**:

1. **Azure UDRs work at the Azure SDN/hypervisor level** - They intercept traffic AFTER it leaves the VM's NIC
2. **Cilium's native routing mode** - Makes routing decisions INSIDE the VM based on the kernel routing table BEFORE traffic reaches the NIC

When Cilium needs to route traffic to a DC worker pod (e.g., `10.244.65.3`):
1. Cilium looks up the pod in its ipcache: `10.244.65.3 → tunnelendpoint=10.50.1.5`
2. Cilium then tries to route to the node IP `10.50.1.5`
3. Cilium looks at the kernel routing table: **NO ROUTE TO 10.50.0.0/16**
4. Traffic is dropped before it even reaches the NIC

The Azure UDR (`10.50.0.0/16 → 10.237.0.4`) never sees this traffic because it's dropped inside the VM.

**Diagnosis:**
```bash
# Check kernel routing table on AKS node
kubectl debug node/aks-nodepool1-10551117-vmss000000 -it --image=busybox --profile=general -- /bin/sh -c "ip route show | grep 10.50"
# Output: No route to DC network

# Check Cilium's ipcache - it knows DC worker pods
kubectl exec -n kube-system cilium-xxx -c cilium-agent -- cilium bpf ipcache list | grep 10.244.65
# Output: 10.244.65.3/32 tunnelendpoint=10.50.1.5 (but can't reach 10.50.1.5!)
```

**Solution Options:**

1. **Add kernel routes on AKS nodes (NOT POSSIBLE on AKS)**
   - AKS nodes are managed and we can't modify system routes
   - Routes would be lost on node restart anyway

2. **Use Cilium tunnel mode (VXLAN/Geneve)**
   - Encapsulates traffic in UDP, doesn't rely on kernel routing for external nodes
   - Requires modifying Cilium config - may not be possible on managed AKS
   - Would change networking for entire cluster

3. **Use Cilium with `auto-direct-node-routes: true`**
   - Cilium adds kernel routes for remote nodes
   - Only works for nodes Cilium knows about (in-cluster)
   - DC workers are in-cluster, so this SHOULD work

4. **Override Cilium's routing for DC workers**
   - Create a separate routing mechanism for DC worker traffic
   - Use a DaemonSet with hostNetwork to add kernel routes

**Attempted Fix - Enable auto-direct-node-routes:**
```bash
# Update Cilium config
kubectl patch configmap -n kube-system cilium-config --type=merge -p '{"data":{"auto-direct-node-routes":"true"}}'

# Restart Cilium DaemonSet
kubectl rollout restart ds/cilium -n kube-system
```

**Result:** Still not working - `auto-direct-node-routes` only adds routes for nodes on the SAME network (reachable via L2). DC workers on `10.50.0.0/16` require L3 routing via the AKS router, which Cilium doesn't auto-configure.

**Alternative Approach - Add kernel routes via DaemonSet:**

We need to add kernel routes on every AKS node to route DC traffic via the AKS router IP. Since we can't persist routes on AKS nodes, we'll use a DaemonSet.

```yaml
# TODO: Create a stargate-route-manager DaemonSet that:
# 1. Runs on AKS nodes (nodeSelector for agentpool)
# 2. Adds route: ip route add 10.50.0.0/16 via 10.237.0.4
# 3. Adds route for each DC worker pod CIDR via 10.237.0.4
# 4. Keeps routes in sync with DC worker nodes in the cluster
```

**Status:** Issue identified. Need to implement kernel route injection on AKS nodes.

---

## Cleanup (when done)

```bash
az aks delete --name stargate-aks-e2e-2 --resource-group stargate-aks-e2e-2 --yes --no-wait
az group delete --name stargate-aks-e2e-2-dc --yes --no-wait
```
