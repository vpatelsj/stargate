#!/bin/bash
set -euo pipefail

#
# Stargate AKS E2E Deployment Script
# Deploys AKS cluster with DC workers using gRPC-based Stargate API
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

# Stargate server config
STARGATE_PORT=50051
STARGATE_ADDR="localhost:${STARGATE_PORT}"

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
pkill -f stargate-server || true
make build

# Step 1: Create resource group
log_step "Step 1: Creating resource group $RESOURCE_GROUP..."
az group create --name "$RESOURCE_GROUP" --location "$LOCATION" --output table

# Step 2: Create AKS cluster (skip if exists)
log_step "Step 2: Creating AKS cluster $CLUSTER_NAME..."
if az aks show --resource-group "$RESOURCE_GROUP" --name "$CLUSTER_NAME" &>/dev/null; then
    log_info "AKS cluster $CLUSTER_NAME already exists, skipping creation"
else
    log_info "Creating new AKS cluster (this may take several minutes)..."
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
fi

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

# Step 5: Provision AKS Router
log_step "Step 5: Provisioning AKS router..."
./bin/prep-dc-inventory \
  -role aks-router \
  -resource-group "$RESOURCE_GROUP" \
  -aks-cluster-name "$CLUSTER_NAME" \
  -aks-router-name "$AKS_ROUTER_NAME" \
  -aks-subnet-cidr 10.237.0.0/24 \
  -location "$LOCATION" \
  -skip-server-cr

# Capture AKS router IPs
AKS_ROUTER_TS_IP=$(tailscale status --json | jq -r ".Peer[] | select(.HostName == \"$AKS_ROUTER_NAME\") | .TailscaleIPs[0]" 2>/dev/null || echo "")
if [[ -z "$AKS_ROUTER_TS_IP" ]]; then
    log_info "Waiting for AKS router to appear in Tailscale..."
    sleep 10
    AKS_ROUTER_TS_IP=$(tailscale status --json | jq -r ".Peer[] | select(.HostName == \"$AKS_ROUTER_NAME\") | .TailscaleIPs[0]" 2>/dev/null || echo "100.0.0.1")
fi
AKS_ROUTER_PRIVATE_IP="10.237.0.4"
log_info "AKS Router Tailscale IP: $AKS_ROUTER_TS_IP"

# Step 6: Create DC resource group
log_step "Step 6: Creating DC resource group $DC_RESOURCE_GROUP..."
az group create --name "$DC_RESOURCE_GROUP" --location "$LOCATION" --output table

# Step 7: Provision DC infrastructure
log_step "Step 7: Provisioning DC infrastructure (router + workers)..."
./bin/prep-dc-inventory \
  -role dc \
  -resource-group "$DC_RESOURCE_GROUP" \
  -aks-cluster-name "$CLUSTER_NAME" \
  -router-name "$DC_ROUTER_NAME" \
  -vm "$WORKER_1" \
  -vm "$WORKER_2" \
  -location "$LOCATION" \
  -skip-server-cr

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

# Step 8: Get worker IPs from Azure
log_step "Step 8: Getting worker VM IPs..."
WORKER_1_IP=$(az vm list-ip-addresses --resource-group "$DC_RESOURCE_GROUP" --name "$WORKER_1" --query "[0].virtualMachine.network.privateIpAddresses[0]" -o tsv 2>/dev/null || echo "10.50.0.10")
WORKER_2_IP=$(az vm list-ip-addresses --resource-group "$DC_RESOURCE_GROUP" --name "$WORKER_2" --query "[0].virtualMachine.network.privateIpAddresses[0]" -o tsv 2>/dev/null || echo "10.50.0.11")
log_info "Worker 1 IP: $WORKER_1_IP"
log_info "Worker 2 IP: $WORKER_2_IP"

# Step 9: Create bootstrap token
log_step "Step 9: Creating bootstrap token..."
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

# Step 10: Create kubelet-bootstrap ServiceAccount
log_step "Step 10: Creating kubelet-bootstrap ServiceAccount..."
kubectl create serviceaccount kubelet-bootstrap -n kube-system 2>/dev/null || true
kubectl create clusterrolebinding kubelet-bootstrap \
  --clusterrole=system:node-bootstrapper \
  --serviceaccount=kube-system:kubelet-bootstrap 2>/dev/null || true
kubectl create clusterrolebinding kubelet-bootstrap-node \
  --clusterrole=system:node \
  --serviceaccount=kube-system:kubelet-bootstrap 2>/dev/null || true

# Step 11: Start Stargate server
log_step "Step 11: Starting Stargate server..."

nohup ./bin/stargate-server \
  -port $STARGATE_PORT \
  -provider azure \
  -aks-api-server "$AKS_FQDN" \
  -aks-cluster-name "$CLUSTER_NAME" \
  -aks-resource-group "$RESOURCE_GROUP" \
  -aks-subscription-id "$SUBSCRIPTION_ID" \
  -aks-vm-resource-group "$DC_RESOURCE_GROUP" \
  -dc-router-tailscale-ip "$DC_ROUTER_TS_IP" \
  -dc-router-private-ip "10.50.1.4" \
  -aks-node-subnet "10.224.0.0/16" \
  -aks-pod-subnet "10.244.0.0/20" \
  -ssh-user ubuntu \
  > /tmp/stargate-server.log 2>&1 &

sleep 2
if pgrep -f stargate-server > /dev/null; then
    log_info "Stargate server started (PID: $(pgrep -f stargate-server))"
    log_info "View logs: tail -f /tmp/stargate-server.log"
else
    log_error "Stargate server failed to start. Check /tmp/stargate-server.log"
    exit 1
fi

# Step 11b: Start Azure Controller for route synchronization
log_step "Step 11b: Starting Azure Controller for route sync..."

# Kill any existing controllers that might conflict
pkill -f "azure-controller" 2>/dev/null || true
pkill -f "boulder-controller" 2>/dev/null || true

# Kill any process using our ports
fuser -k 8091/tcp 2>/dev/null || true
fuser -k 8092/tcp 2>/dev/null || true

nohup ./bin/azure-controller \
  -metrics-bind-address ":8091" \
  -health-probe-bind-address ":8092" \
  -aks-subscription-id "$SUBSCRIPTION_ID" \
  -aks-cluster-name "$CLUSTER_NAME" \
  -aks-resource-group "$RESOURCE_GROUP" \
  -aks-node-resource-group "$NODE_RESOURCE_GROUP" \
  -aks-vm-resource-group "$DC_RESOURCE_GROUP" \
  -azure-route-table-name "stargate-workers-rt" \
  -router-route-table-name "stargate-router-rt" \
  -azure-subnet-name "aks-subnet" \
  -router-subnet-name "stargate-aks-router-subnet" \
  -aks-router-private-ip "$AKS_ROUTER_PRIVATE_IP" \
  -aks-router-tailscale-ip "$AKS_ROUTER_TS_IP" \
  -dc-router-tailscale-ip "$DC_ROUTER_TS_IP" \
  -dc-subnet-cidr "10.50.0.0/16" \
  -dc-pod-cidr "10.244.64.0/20" \
  > /tmp/azure-controller.log 2>&1 &

sleep 2
if pgrep -f azure-controller > /dev/null; then
    log_info "Azure controller started (PID: $(pgrep -f azure-controller))"
    log_info "View logs: tail -f /tmp/azure-controller.log"
else
    log_error "Azure controller failed to start. Check /tmp/azure-controller.log"
    # Don't exit - controller is optional, routes can be added manually
fi

# Step 12: Register machines via gRPC
log_step "Step 12: Registering machines with Stargate..."

# Use grpcurl for direct gRPC calls
if command -v grpcurl &>/dev/null; then
    log_info "Registering machines via grpcurl..."
    
    # Register Worker 1
    grpcurl -plaintext -d '{
      "machine": {
        "machine_id": "'"$WORKER_1"'",
        "spec": {
          "provider": "azure",
          "ssh_endpoint": "'"${WORKER_1_IP}:22"'",
          "mac_addresses": ["'"$(printf '02:00:00:00:%02x:01' $((RANDOM % 256)))"'"]
        },
        "labels": {
          "cluster": "'"$CLUSTER_NAME"'",
          "role": "worker",
          "aks_fqdn": "'"$AKS_FQDN"'"
        }
      }
    }' "$STARGATE_ADDR" baremetal.v1.MachineService/RegisterMachine

    # Register Worker 2
    grpcurl -plaintext -d '{
      "machine": {
        "machine_id": "'"$WORKER_2"'",
        "spec": {
          "provider": "azure",
          "ssh_endpoint": "'"${WORKER_2_IP}:22"'",
          "mac_addresses": ["'"$(printf '02:00:00:00:%02x:02' $((RANDOM % 256)))"'"]
        },
        "labels": {
          "cluster": "'"$CLUSTER_NAME"'",
          "role": "worker",
          "aks_fqdn": "'"$AKS_FQDN"'"
        }
      }
    }' "$STARGATE_ADDR" baremetal.v1.MachineService/RegisterMachine
else
    log_info "grpcurl not found - using sgctl register..."
    # Register workers with their actual IDs and endpoints
    ./bin/sgctl -server "$STARGATE_ADDR" register "$WORKER_1" -ssh "${WORKER_1_IP}:22" -provider azure -labels "cluster=$CLUSTER_NAME,aks_fqdn=$AKS_FQDN"
    ./bin/sgctl -server "$STARGATE_ADDR" register "$WORKER_2" -ssh "${WORKER_2_IP}:22" -provider azure -labels "cluster=$CLUSTER_NAME,aks_fqdn=$AKS_FQDN"
fi

# Step 13: List registered machines
log_step "Step 13: Listing registered machines..."
./bin/sgctl -server "$STARGATE_ADDR" list

# Step 14: Enter maintenance mode (required for reimage)
log_step "Step 14: Entering maintenance mode for workers..."

./bin/sgctl -server "$STARGATE_ADDR" enter-maintenance "$WORKER_1" || true
./bin/sgctl -server "$STARGATE_ADDR" enter-maintenance "$WORKER_2" || true

# Wait for maintenance operations
sleep 5
./bin/sgctl -server "$STARGATE_ADDR" list

# Step 15: Trigger reimage operations
log_step "Step 15: Triggering reimage operations..."

./bin/sgctl -server "$STARGATE_ADDR" reimage "$WORKER_1" || true
./bin/sgctl -server "$STARGATE_ADDR" reimage "$WORKER_2" || true

# Step 16: Wait for operations to complete
log_step "Step 16: Waiting for reimage operations to complete..."

MAX_WAIT=600  # 10 minutes
WAIT_INTERVAL=15
ELAPSED=0

while [[ $ELAPSED -lt $MAX_WAIT ]]; do
    log_info "Checking operation status... ($ELAPSED/$MAX_WAIT seconds)"
    
    # List operations
    ./bin/sgctl -server "$STARGATE_ADDR" ops
    
    # Check if operations completed (with fake provider, they should complete quickly)
    SUCCEEDED_COUNT=$(./bin/sgctl -server "$STARGATE_ADDR" ops 2>/dev/null | grep -c "SUCCEEDED" || true)
    SUCCEEDED_COUNT=${SUCCEEDED_COUNT:-0}
    if [[ "$SUCCEEDED_COUNT" -ge 2 ]]; then
        log_info "Both operations succeeded!"
        break
    fi
    
    sleep $WAIT_INTERVAL
    ELAPSED=$((ELAPSED + WAIT_INTERVAL))
done

if [[ $ELAPSED -ge $MAX_WAIT ]]; then
    log_error "Timeout waiting for operations to complete"
    ./bin/sgctl -server "$STARGATE_ADDR" ops
    log_info "Operations may still be running. Check: ./bin/sgctl -server $STARGATE_ADDR ops"
fi

# Step 17: Verify nodes joined (if using real provider)
log_step "Step 17: Checking node status..."
kubectl get nodes || log_info "Nodes may not have joined yet (using fake provider for demo)"

# Step 18: Deploy Goldpinger
log_step "Step 18: Deploying Goldpinger for connectivity testing..."

kubectl create namespace goldpinger 2>/dev/null || true
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

# Summary
echo ""
echo -e "${GREEN}========================================${NC}"
echo -e "${GREEN}  Deployment Complete!${NC}"
echo -e "${GREEN}========================================${NC}"
echo ""
echo "Cluster Name:       $CLUSTER_NAME"
echo "Resource Group:     $RESOURCE_GROUP"
echo "DC Resource Group:  $DC_RESOURCE_GROUP"
echo "Location:           $LOCATION"
echo ""
echo "AKS API Server:     https://${AKS_FQDN}:443"
echo "AKS Router TS IP:   $AKS_ROUTER_TS_IP"
echo "DC Router TS IP:    $DC_ROUTER_TS_IP"
echo ""
echo "Stargate Server:    $STARGATE_ADDR"
echo "Server logs:        tail -f /tmp/stargate-server.log"
echo ""
echo "Commands:"
echo "  List machines:    ./bin/sgctl -server $STARGATE_ADDR list"
echo "  List operations:  ./bin/sgctl -server $STARGATE_ADDR ops"
echo "  Watch operations: ./bin/sgctl -server $STARGATE_ADDR watch"
echo ""
echo "To port-forward Goldpinger:"
echo "  kubectl port-forward -n goldpinger svc/goldpinger 8080:8080"
echo ""
echo "To cleanup, run:"
echo "  ./scripts/cleanup-aks-e2e.sh $CLUSTER_NAME"
