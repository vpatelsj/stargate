#!/bin/bash
set -euo pipefail

#
# Boulder Lab AKS Integration Script
# Deploys physical Boulder lab servers as AKS worker nodes
#
# Prerequisites:
#   - AKS cluster already exists
#   - stargate-boulderlab-gateway VM running with subnet routes approved
#   - Physical servers accessible via 172.18.10.x (through Tailscale mesh)
#   - SSH key configured for physical servers
#
# Usage: ./scripts/deploy-boulder-lab.sh <cluster-name> [--workers worker1,worker2,...]
# Example: ./scripts/deploy-boulder-lab.sh stargate-aks-e2e-17 --workers boulder-node-1,boulder-node-2
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

log_warn() {
    echo -e "${YELLOW}WARNING: $1${NC}"
}

log_error() {
    echo -e "${RED}ERROR: $1${NC}" >&2
}

# Default configuration
CLUSTER_NAME="${1:-}"
NAMESPACE="boulder-dc"
BOULDER_GATEWAY_TS_NAME="stargate-boulderlab-gateway"

# Boulder lab network configuration
BOULDER_VLAN10_CIDR="172.18.10.0/24"
BOULDER_VLAN20_CIDR="172.18.20.0/24"

# Default workers (physical server hostnames and their VLAN 10 IPs)
declare -A BOULDER_WORKERS
# Example: BOULDER_WORKERS["boulder-node-1"]="172.18.10.10"
# These should be set via --workers flag or environment

# Parse arguments
shift || true  # Skip cluster name
while [[ $# -gt 0 ]]; do
    case $1 in
        --workers)
            IFS=',' read -ra WORKER_LIST <<< "$2"
            shift 2
            ;;
        --namespace)
            NAMESPACE="$2"
            shift 2
            ;;
        *)
            log_error "Unknown argument: $1"
            exit 1
            ;;
    esac
done

if [[ -z "$CLUSTER_NAME" ]]; then
    echo "Usage: $0 <cluster-name> [--workers worker1:ip1,worker2:ip2,...]"
    echo "Example: $0 stargate-aks-e2e-17 --workers boulder-node-1:172.18.10.10,boulder-node-2:172.18.10.11"
    exit 1
fi

# Parse workers into associative array
for worker in "${WORKER_LIST[@]:-}"; do
    if [[ "$worker" == *":"* ]]; then
        name="${worker%%:*}"
        ip="${worker##*:}"
        BOULDER_WORKERS["$name"]="$ip"
    else
        log_error "Worker must be in format 'name:ip', got: $worker"
        exit 1
    fi
done

if [[ ${#BOULDER_WORKERS[@]} -eq 0 ]]; then
    log_error "No workers specified. Use --workers boulder-node-1:172.18.10.10,boulder-node-2:172.18.10.11"
    exit 1
fi

# Validate prerequisites
log_step "Checking prerequisites..."

if ! az account show &>/dev/null; then
    log_error "Azure CLI not logged in. Run: az login"
    exit 1
fi

if ! command -v kubectl &>/dev/null; then
    log_error "kubectl not found"
    exit 1
fi

if ! command -v tailscale &>/dev/null; then
    log_error "tailscale not found"
    exit 1
fi

# Check if we can reach the boulder gateway via Tailscale
BOULDER_GATEWAY_TS_IP=$(tailscale status --json | jq -r ".Peer[] | select(.HostName == \"$BOULDER_GATEWAY_TS_NAME\") | .TailscaleIPs[0]" 2>/dev/null || echo "")
if [[ -z "$BOULDER_GATEWAY_TS_IP" ]]; then
    log_error "Cannot find $BOULDER_GATEWAY_TS_NAME in Tailscale network"
    log_info "Make sure stargate-boulderlab-gateway VM is running and connected to Tailscale"
    exit 1
fi
log_info "Boulder gateway Tailscale IP: $BOULDER_GATEWAY_TS_IP"

# Check subnet routes are approved (optional - don't fail if not approved)
ROUTES_APPROVED=$(tailscale status --json | jq -r ".Peer[] | select(.HostName == \"$BOULDER_GATEWAY_TS_NAME\") | .AllowedIPs[]" 2>/dev/null | { grep "172.18" 2>/dev/null || true; } | wc -l)
ROUTES_APPROVED=${ROUTES_APPROVED:-0}
if [[ $ROUTES_APPROVED -lt 1 ]]; then
    log_warn "Subnet routes not approved for $BOULDER_GATEWAY_TS_NAME"
    log_info "Consider approving routes at: https://login.tailscale.com/admin/machines"
else
    log_info "Subnet routes approved: $ROUTES_APPROVED"
fi

log_info "All prerequisites met"

# Get script directory
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"

cd "$PROJECT_DIR"

# Build binaries
log_step "Building boulder-controller..."
make boulder-controller

# Get AKS cluster details
log_step "Getting AKS cluster details..."
RESOURCE_GROUP=$(az aks list --query "[?name=='$CLUSTER_NAME'].resourceGroup" -o tsv)
if [[ -z "$RESOURCE_GROUP" ]]; then
    log_error "AKS cluster '$CLUSTER_NAME' not found"
    exit 1
fi

AKS_FQDN=$(az aks show --resource-group "$RESOURCE_GROUP" --name "$CLUSTER_NAME" --query "fqdn" -o tsv)
NODE_RESOURCE_GROUP=$(az aks show --resource-group "$RESOURCE_GROUP" --name "$CLUSTER_NAME" --query "nodeResourceGroup" -o tsv)
SUBSCRIPTION_ID=$(az account show --query "id" -o tsv)

log_info "Resource Group: $RESOURCE_GROUP"
log_info "AKS FQDN: $AKS_FQDN"
log_info "Subscription ID: $SUBSCRIPTION_ID"

# Get AKS credentials
log_step "Getting AKS credentials..."
az aks get-credentials --resource-group "$RESOURCE_GROUP" --name "$CLUSTER_NAME" --overwrite-existing

# Create namespace
log_step "Creating namespace $NAMESPACE..."
kubectl create namespace "$NAMESPACE" 2>/dev/null || true

# Create SSH credentials secret
log_step "Creating SSH credentials secret..."
kubectl apply -f - <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: boulder-ssh-credentials
  namespace: $NAMESPACE
type: Opaque
stringData:
  username: ubuntu
  privateKey: |
$(cat ~/.ssh/id_rsa | sed 's/^/    /')
EOF

# Create ProvisioningProfile
log_step "Creating ProvisioningProfile..."
kubectl apply -f - <<EOF
apiVersion: stargate.io/v1alpha1
kind: ProvisioningProfile
metadata:
  name: boulder-k8s-worker
  namespace: $NAMESPACE
spec:
  kubernetesVersion: "1.33"
  containerRuntime: containerd
  sshCredentialsSecretRef: boulder-ssh-credentials
  adminUsername: ubuntu
EOF

# Create Server CRs for each physical server
log_step "Creating Server CRs for Boulder workers..."
for name in "${!BOULDER_WORKERS[@]}"; do
    ip="${BOULDER_WORKERS[$name]}"
    log_info "Creating Server CR: $name ($ip)"
    kubectl apply -f - <<EOF
apiVersion: stargate.io/v1alpha1
kind: Server
metadata:
  name: $name
  namespace: $NAMESPACE
spec:
  mac: "00:00:00:00:00:00"
  provider: boulder
  ipv4: "$ip"
EOF
done

# Create bootstrap token
log_step "Creating bootstrap token..."
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

# Ensure kubelet-bootstrap ServiceAccount exists
log_step "Creating kubelet-bootstrap ServiceAccount..."
kubectl create serviceaccount kubelet-bootstrap -n kube-system 2>/dev/null || true
kubectl create clusterrolebinding kubelet-bootstrap \
  --clusterrole=system:node-bootstrapper \
  --serviceaccount=kube-system:kubelet-bootstrap 2>/dev/null || true
kubectl create clusterrolebinding kubelet-bootstrap-node \
  --clusterrole=system:node \
  --serviceaccount=kube-system:kubelet-bootstrap 2>/dev/null || true

# Create Operation CRs for each worker
log_step "Creating Operation CRs for workers..."
for name in "${!BOULDER_WORKERS[@]}"; do
    log_info "Creating Operation CR: ${name}-repave"
    kubectl apply -f - <<EOF
apiVersion: stargate.io/v1alpha1
kind: Operation
metadata:
  name: ${name}-repave
  namespace: $NAMESPACE
spec:
  serverRef:
    name: $name
  provisioningProfileRef:
    name: boulder-k8s-worker
  operation: repave
EOF
done

kubectl get operations -n "$NAMESPACE"

# Get AKS router and DC router info
AKS_ROUTER_NAME="${CLUSTER_NAME}-router"
AKS_ROUTER_TS_IP=$(tailscale status --json | jq -r ".Peer[] | select(.HostName == \"$AKS_ROUTER_NAME\") | .TailscaleIPs[0]" 2>/dev/null || echo "")
DC_ROUTER_NAME="${CLUSTER_NAME}-dc-router"
DC_ROUTER_TS_IP=$(tailscale status --json | jq -r ".Peer[] | select(.HostName == \"$DC_ROUTER_NAME\") | .TailscaleIPs[0]" 2>/dev/null || echo "")
AKS_NODE_RG="MC_${CLUSTER_NAME}_${CLUSTER_NAME}_canadacentral"

log_info "AKS Router Tailscale IP: ${AKS_ROUTER_TS_IP:-not found}"
log_info "DC Router Tailscale IP: ${DC_ROUTER_TS_IP:-not found}"
log_info "AKS Node Resource Group: $AKS_NODE_RG"

# Start boulder-controller
log_step "Starting boulder-controller..."
pkill -f boulder-controller 2>/dev/null || true

nohup ./bin/boulder-controller \
  -control-plane-mode aks \
  -aks-api-server "https://${AKS_FQDN}:443" \
  -aks-cluster-name "$CLUSTER_NAME" \
  -aks-resource-group "$RESOURCE_GROUP" \
  -aks-subscription-id "$SUBSCRIPTION_ID" \
  -boulder-gateway-ip "$BOULDER_GATEWAY_TS_IP" \
  -aks-router-tailscale-ip "$AKS_ROUTER_TS_IP" \
  -dc-router-tailscale-ip "$DC_ROUTER_TS_IP" \
  -azure-route-table-name "stargate-workers-rt" \
  -azure-route-table-rg "$AKS_NODE_RG" \
  -aks-router-private-ip "10.237.0.4" \
  -namespace "$NAMESPACE" \
  > /tmp/boulder-controller.log 2>&1 &

sleep 2
if pgrep -f boulder-controller > /dev/null; then
    log_info "Controller started in background (PID: $(pgrep -f boulder-controller))"
    log_info "View logs: tail -f /tmp/boulder-controller.log"
else
    log_error "Controller failed to start. Check /tmp/boulder-controller.log"
    exit 1
fi

# Wait for workers to join
log_step "Waiting for workers to join cluster..."

MAX_WAIT=600  # 10 minutes
WAIT_INTERVAL=15
ELAPSED=0
TOTAL_WORKERS=${#BOULDER_WORKERS[@]}

while [[ $ELAPSED -lt $MAX_WAIT ]]; do
    SUCCEEDED=$(kubectl get operations -n "$NAMESPACE" -o jsonpath='{.items[*].status.phase}' 2>/dev/null | tr ' ' '\n' | grep "Succeeded" | wc -l)
    SUCCEEDED=${SUCCEEDED:-0}
    
    log_info "Operations succeeded: $SUCCEEDED / $TOTAL_WORKERS (elapsed: ${ELAPSED}s)"
    
    if [[ $SUCCEEDED -ge $TOTAL_WORKERS ]]; then
        log_info "All operations succeeded!"
        break
    fi
    
    sleep $WAIT_INTERVAL
    ELAPSED=$((ELAPSED + WAIT_INTERVAL))
done

if [[ $SUCCEEDED -lt $TOTAL_WORKERS ]]; then
    log_error "Timeout waiting for operations to complete"
    log_info "Check operation status: kubectl get operations -n $NAMESPACE"
    log_info "Check controller logs: tail -f /tmp/boulder-controller.log"
    exit 1
fi

# Show final status
log_step "Deployment complete!"
echo ""
echo "Cluster nodes:"
kubectl get nodes -o wide
echo ""
echo "Operations:"
kubectl get operations -n "$NAMESPACE"
echo ""
echo "Boulder workers joined the AKS cluster successfully!"
