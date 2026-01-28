#!/bin/bash
set -euo pipefail

#
# Stargate AKS E2E Deployment Script
# Automates the full deployment process from deployment-log-e2e-11.md
#
# Usage: ./scripts/deploy-aks-e2e.sh <cluster-name> [location]
# Example: ./scripts/deploy-aks-e2e.sh stargate-aks-e2e-12 canadacentral
#

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

log_step() {
    echo -e "\n${BLUE}==>${NC} ${GREEN}$1${NC}"
}

log_info() {
    echo -e "${YELLOW}    $1${NC}"
}

log_error() {
    echo -e "${RED}ERROR: $1${NC}" >&2
}

# Parse arguments
CLUSTER_NAME="${1:-}"
LOCATION="${2:-canadacentral}"

if [[ -z "$CLUSTER_NAME" ]]; then
    echo "Usage: $0 <cluster-name> [location]"
    echo "Example: $0 stargate-aks-e2e-12 canadacentral"
    exit 1
fi

# Derived names
RESOURCE_GROUP="$CLUSTER_NAME"
DC_RESOURCE_GROUP="${CLUSTER_NAME}-dc"
AKS_ROUTER_NAME="${CLUSTER_NAME}-router"
DC_ROUTER_NAME="${CLUSTER_NAME}-dc-router"
WORKER_1="${CLUSTER_NAME}-worker-1"
WORKER_2="${CLUSTER_NAME}-worker-2"

# Validate prerequisites
log_step "Checking prerequisites..."

if [[ -z "${TAILSCALE_AUTH_KEY:-}" ]]; then
    log_error "TAILSCALE_AUTH_KEY is not set"
    exit 1
fi

if [[ -z "${TAILSCALE_CLIENT_ID:-}" ]]; then
    log_error "TAILSCALE_CLIENT_ID is not set"
    exit 1
fi

if [[ -z "${TAILSCALE_CLIENT_SECRET:-}" ]]; then
    log_error "TAILSCALE_CLIENT_SECRET is not set"
    exit 1
fi

if ! az account show &>/dev/null; then
    log_error "Azure CLI not logged in. Run: az login"
    exit 1
fi

if ! command -v kubectl &>/dev/null; then
    log_error "kubectl not found"
    exit 1
fi

if [[ ! -f ~/.ssh/id_rsa ]]; then
    log_error "SSH key not found at ~/.ssh/id_rsa"
    exit 1
fi

log_info "All prerequisites met"

# Get script directory
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"

cd "$PROJECT_DIR"

# Step 0: Build binaries
log_step "Step 0: Building binaries..."
pkill -f azure-controller || true
make build

# Step 1: Create resource group
log_step "Step 1: Creating resource group $RESOURCE_GROUP..."
az group create --name "$RESOURCE_GROUP" --location "$LOCATION" --output table

# Step 2: Create AKS cluster
log_step "Step 2: Creating AKS cluster $CLUSTER_NAME (this may take several minutes)..."
az aks create \
  --resource-group "$RESOURCE_GROUP" \
  --name "$CLUSTER_NAME" \
  --kubernetes-version 1.33.5 \
  --node-count 2 \
  --node-vm-size Standard_D2ads_v5 \
  --network-plugin azure \
  --network-plugin-mode overlay \
  --network-policy cilium \
  --network-dataplane cilium \
  --pod-cidr 10.244.0.0/16 \
  --service-cidr 10.0.0.0/16 \
  --generate-ssh-keys \
  --output table

# Step 3: Get AKS credentials
log_step "Step 3: Getting AKS credentials..."
az aks get-credentials --resource-group "$RESOURCE_GROUP" --name "$CLUSTER_NAME" --overwrite-existing

# Step 4: Verify cluster access
log_step "Step 4: Verifying cluster access..."
kubectl get nodes
kubectl cluster-info

# Get AKS details for later
AKS_FQDN=$(az aks show --resource-group "$RESOURCE_GROUP" --name "$CLUSTER_NAME" --query "fqdn" -o tsv)
NODE_RESOURCE_GROUP=$(az aks show --resource-group "$RESOURCE_GROUP" --name "$CLUSTER_NAME" --query "nodeResourceGroup" -o tsv)
SUBSCRIPTION_ID=$(az account show --query "id" -o tsv)

log_info "AKS FQDN: $AKS_FQDN"
log_info "Node Resource Group: $NODE_RESOURCE_GROUP"

# Step 5: Install Stargate CRDs
log_step "Step 5: Installing Stargate CRDs..."
kubectl apply -f config/crd/bases/

# Step 6: Create stargate namespace
log_step "Step 6: Creating stargate namespace..."
kubectl create namespace stargate || true

# Step 7: Provision AKS Router
log_step "Step 7: Provisioning AKS router..."
./bin/prep-dc-inventory \
  -role aks-router \
  -resource-group "$RESOURCE_GROUP" \
  -aks-cluster-name "$CLUSTER_NAME" \
  -aks-router-name "$AKS_ROUTER_NAME" \
  -aks-subnet-cidr 10.237.0.0/24 \
  -location "$LOCATION"

# Capture AKS router IPs
AKS_ROUTER_TS_IP=$(tailscale status --json | jq -r ".Peer[] | select(.HostName == \"$AKS_ROUTER_NAME\") | .TailscaleIPs[0]" 2>/dev/null || echo "")
if [[ -z "$AKS_ROUTER_TS_IP" ]]; then
    log_info "Waiting for AKS router to appear in Tailscale..."
    sleep 10
    AKS_ROUTER_TS_IP=$(tailscale status --json | jq -r ".Peer[] | select(.HostName == \"$AKS_ROUTER_NAME\") | .TailscaleIPs[0]" 2>/dev/null || echo "100.0.0.1")
fi
AKS_ROUTER_PRIVATE_IP="10.237.0.4"
log_info "AKS Router Tailscale IP: $AKS_ROUTER_TS_IP"

# Step 8: Create DC resource group
log_step "Step 8: Creating DC resource group $DC_RESOURCE_GROUP..."
az group create --name "$DC_RESOURCE_GROUP" --location "$LOCATION" --output table

# Step 9: Provision DC infrastructure
log_step "Step 9: Provisioning DC infrastructure (router + workers)..."
./bin/prep-dc-inventory \
  -role dc \
  -resource-group "$DC_RESOURCE_GROUP" \
  -aks-cluster-name "$CLUSTER_NAME" \
  -router-name "$DC_ROUTER_NAME" \
  -vm "$WORKER_1" \
  -vm "$WORKER_2" \
  -location "$LOCATION"

# Capture DC router IP
DC_ROUTER_TS_IP=$(tailscale status --json | jq -r ".Peer[] | select(.HostName == \"$DC_ROUTER_NAME\") | .TailscaleIPs[0]" 2>/dev/null || echo "")
if [[ -z "$DC_ROUTER_TS_IP" ]]; then
    log_info "Waiting for DC router to appear in Tailscale..."
    sleep 10
    DC_ROUTER_TS_IP=$(tailscale status --json | jq -r ".Peer[] | select(.HostName == \"$DC_ROUTER_NAME\") | .TailscaleIPs[0]" 2>/dev/null || echo "100.0.0.2")
fi
log_info "DC Router Tailscale IP: $DC_ROUTER_TS_IP"

# Get VNet name
VNET_NAME=$(az network vnet list --resource-group "$NODE_RESOURCE_GROUP" --query "[0].name" -o tsv)
log_info "VNet Name: $VNET_NAME"

# Step 10: Skipped (automated in Step 9)
log_step "Step 10: Skipped (routes auto-approved in Step 9)"

# Step 11: Create bootstrap token
log_step "Step 11: Creating bootstrap token..."
TOKEN_ID=$(head -c 100 /dev/urandom | tr -dc 'a-z0-9' | head -c 6)
TOKEN_SECRET=$(head -c 100 /dev/urandom | tr -dc 'a-z0-9' | head -c 16)
BOOTSTRAP_TOKEN="${TOKEN_ID}.${TOKEN_SECRET}"

log_info "Bootstrap Token: $BOOTSTRAP_TOKEN"

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

# Step 12: Create secrets and ProvisioningProfile
log_step "Step 12: Creating secrets and ProvisioningProfile..."

# SSH credentials secret
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

# Tailscale auth secret
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

# ProvisioningProfile
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

# Step 12a: Create kubelet-bootstrap ServiceAccount
log_step "Step 12a: Creating kubelet-bootstrap ServiceAccount..."
kubectl create serviceaccount kubelet-bootstrap -n kube-system || true
kubectl create clusterrolebinding kubelet-bootstrap \
  --clusterrole=system:node-bootstrapper \
  --serviceaccount=kube-system:kubelet-bootstrap || true
kubectl create clusterrolebinding kubelet-bootstrap-node \
  --clusterrole=system:node \
  --serviceaccount=kube-system:kubelet-bootstrap || true

# Step 13: Create Operation CRs for workers
log_step "Step 13: Creating Operation CRs for workers..."

kubectl apply -f - <<EOF
apiVersion: stargate.io/v1alpha1
kind: Operation
metadata:
  name: ${WORKER_1}-repave
  namespace: azure-dc
spec:
  serverRef:
    name: ${WORKER_1}
  provisioningProfileRef:
    name: azure-k8s-worker
  operation: repave
EOF

kubectl apply -f - <<EOF
apiVersion: stargate.io/v1alpha1
kind: Operation
metadata:
  name: ${WORKER_2}-repave
  namespace: azure-dc
spec:
  serverRef:
    name: ${WORKER_2}
  provisioningProfileRef:
    name: azure-k8s-worker
  operation: repave
EOF

kubectl get operations -n azure-dc

# Step 14: Run Azure Controller
log_step "Step 14: Starting Azure controller..."

nohup ./bin/azure-controller \
  -control-plane-mode aks \
  -enable-route-sync \
  -aks-api-server "https://${AKS_FQDN}:443" \
  -aks-cluster-name "$CLUSTER_NAME" \
  -aks-resource-group "$RESOURCE_GROUP" \
  -aks-node-resource-group "$NODE_RESOURCE_GROUP" \
  -aks-subscription-id "$SUBSCRIPTION_ID" \
  -aks-vm-resource-group "$DC_RESOURCE_GROUP" \
  -dc-router-tailscale-ip "$DC_ROUTER_TS_IP" \
  -aks-router-tailscale-ip "$AKS_ROUTER_TS_IP" \
  -aks-router-private-ip "$AKS_ROUTER_PRIVATE_IP" \
  -azure-route-table-name stargate-workers-rt \
  -router-route-table-name stargate-router-rt \
  -router-subnet-name stargate-aks-router-subnet \
  -azure-vnet-name "$VNET_NAME" \
  -dc-subnet-cidr 10.50.0.0/16 \
  -tailscale-client-id "$TAILSCALE_CLIENT_ID" \
  -tailscale-client-secret "$TAILSCALE_CLIENT_SECRET" \
  > /tmp/azure-controller.log 2>&1 &

sleep 2
if pgrep -f azure-controller > /dev/null; then
    log_info "Controller started in background (PID: $(pgrep -f azure-controller))"
    log_info "View logs: tail -f /tmp/azure-controller.log"
else
    log_error "Controller failed to start. Check /tmp/azure-controller.log"
    exit 1
fi

# Step 15: Wait for workers to join
log_step "Step 15: Waiting for workers to join cluster..."

MAX_WAIT=600  # 10 minutes
WAIT_INTERVAL=15
ELAPSED=0

while [[ $ELAPSED -lt $MAX_WAIT ]]; do
    PHASES=$(kubectl get operations -n azure-dc -o jsonpath='{.items[*].status.phase}' 2>/dev/null || echo "")
    SUCCEEDED=0
    if [[ -n "$PHASES" ]]; then
        SUCCEEDED=$(echo "$PHASES" | tr ' ' '\n' | grep -c "Succeeded" 2>/dev/null || echo "0")
    fi
    
    if [[ "$SUCCEEDED" -ge 2 ]]; then
        log_info "Both operations succeeded!"
        break
    fi
    
    log_info "Waiting for operations to complete... ($ELAPSED/$MAX_WAIT seconds)"
    kubectl get operations -n azure-dc 2>/dev/null || true
    sleep $WAIT_INTERVAL
    ELAPSED=$((ELAPSED + WAIT_INTERVAL))
done

if [[ $ELAPSED -ge $MAX_WAIT ]]; then
    log_error "Timeout waiting for operations to complete"
    kubectl get operations -n azure-dc
    exit 1
fi

echo ""
kubectl get nodes
echo ""
kubectl get operations -n azure-dc

# Step 16: Deploy Goldpinger
log_step "Step 16: Deploying Goldpinger for connectivity testing..."

kubectl create namespace goldpinger || true
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

# Wait for goldpinger pods
log_info "Waiting for Goldpinger pods to be ready..."
sleep 15
kubectl get pods -n goldpinger -o wide

# Start port-forward in background
kubectl port-forward -n goldpinger svc/goldpinger 8080:8080 &
sleep 3

# Test connectivity
log_step "Testing connectivity..."
if curl -s http://localhost:8080/check_all | jq -r '.responses | to_entries[] | "\(.key) -> OK: \(.value.OK)"'; then
    log_info "All connectivity checks passed!"
else
    log_info "Connectivity check failed or still initializing. Try: curl -s http://localhost:8080/check_all | jq ."
fi

# Summary
echo ""
echo -e "${GREEN}========================================${NC}"
echo -e "${GREEN}  Deployment Complete!${NC}"
echo -e "${GREEN}========================================${NC}"
echo ""
echo "Cluster Name:      $CLUSTER_NAME"
echo "Resource Group:    $RESOURCE_GROUP"
echo "DC Resource Group: $DC_RESOURCE_GROUP"
echo "Location:          $LOCATION"
echo ""
echo "AKS API Server:    https://${AKS_FQDN}:443"
echo "AKS Router TS IP:  $AKS_ROUTER_TS_IP"
echo "DC Router TS IP:   $DC_ROUTER_TS_IP"
echo ""
echo "Controller logs:   tail -f /tmp/azure-controller.log"
echo "Goldpinger UI:     http://localhost:8080"
echo ""
echo "To cleanup, run:"
echo "  ./scripts/cleanup-aks-e2e.sh $CLUSTER_NAME"
