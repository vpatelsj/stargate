#!/bin/bash
# Setup Kind cluster for Stargate with Tailscale connectivity
# This script creates a Kind cluster configured to work with Azure VMs via Tailscale

set -e

CLUSTER_NAME="${CLUSTER_NAME:-stargate-demo}"
TAILSCALE_IP=""

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

log_info() { echo -e "${GREEN}[INFO]${NC} $1"; }
log_warn() { echo -e "${YELLOW}[WARN]${NC} $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $1"; }

# Check prerequisites
check_prerequisites() {
    log_info "Checking prerequisites..."
    
    if ! command -v kind &> /dev/null; then
        log_error "kind is not installed. Install from https://kind.sigs.k8s.io/"
        exit 1
    fi
    
    if ! command -v docker &> /dev/null; then
        log_error "docker is not installed"
        exit 1
    fi
    
    if ! command -v kubectl &> /dev/null; then
        log_error "kubectl is not installed"
        exit 1
    fi
    
    if ! command -v tailscale &> /dev/null; then
        log_error "tailscale is not installed"
        exit 1
    fi
    
    log_info "All prerequisites met"
}

# Get Tailscale IP
get_tailscale_ip() {
    log_info "Getting Tailscale IP..."
    TAILSCALE_IP=$(tailscale ip -4 2>/dev/null || true)
    
    if [[ -z "$TAILSCALE_IP" ]]; then
        log_error "Could not get Tailscale IP. Is Tailscale running?"
        exit 1
    fi
    
    log_info "Tailscale IP: $TAILSCALE_IP"
}

# Delete existing cluster if it exists
delete_existing_cluster() {
    if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
        log_warn "Cluster '$CLUSTER_NAME' already exists. Deleting..."
        kind delete cluster --name "$CLUSTER_NAME"
    fi
}

# Create Kind cluster with proper networking
create_cluster() {
    log_info "Creating Kind cluster '$CLUSTER_NAME'..."
    
    # Create Kind config that binds API server to all interfaces
    cat > /tmp/kind-config.yaml <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
name: ${CLUSTER_NAME}
networking:
  # Bind to all interfaces so Tailscale can reach the API server
  apiServerAddress: "0.0.0.0"
  apiServerPort: 6443
  # Use default pod/service subnets
  podSubnet: "10.244.0.0/16"
  serviceSubnet: "10.96.0.0/16"
nodes:
- role: control-plane
EOF

    kind create cluster --name "$CLUSTER_NAME" --config /tmp/kind-config.yaml
    rm /tmp/kind-config.yaml
    
    log_info "Cluster created successfully"
}

# Install CNI (Flannel) since Kind with 0.0.0.0 doesn't install kindnet
install_cni() {
    log_info "Installing Flannel CNI..."
    
    # Wait for API server to be ready
    kubectl wait --for=condition=Ready node/${CLUSTER_NAME}-control-plane --timeout=60s || true
    
    # Install Flannel
    kubectl apply -f https://github.com/flannel-io/flannel/releases/latest/download/kube-flannel.yml
    
    # Install CNI plugins in the Kind container (needed for Flannel)
    log_info "Installing CNI plugins in Kind container..."
    docker exec ${CLUSTER_NAME}-control-plane sh -c \
        'curl -sL https://github.com/containernetworking/plugins/releases/download/v1.4.0/cni-plugins-linux-amd64-v1.4.0.tgz | tar -xz -C /opt/cni/bin'
    
    log_info "Waiting for Flannel to be ready..."
    sleep 10
    kubectl wait --for=condition=Ready pod -l app=flannel -n kube-flannel --timeout=120s || true
}

# Configure API server to advertise Tailscale IP
configure_apiserver_advertise() {
    log_info "Configuring API server to advertise Tailscale IP..."
    
    # Update the API server manifest to use Tailscale IP
    docker exec ${CLUSTER_NAME}-control-plane sed -i \
        "s/--advertise-address=[0-9.]*$/--advertise-address=${TAILSCALE_IP}/" \
        /etc/kubernetes/manifests/kube-apiserver.yaml
    
    log_info "Waiting for API server to restart..."
    sleep 30
    
    # Wait for API server to be ready again
    for i in {1..30}; do
        if kubectl get nodes &>/dev/null; then
            log_info "API server is ready"
            break
        fi
        sleep 2
    done
}

# Verify the configuration
verify_cluster() {
    log_info "Verifying cluster configuration..."
    
    # Check nodes
    kubectl get nodes -o wide
    
    # Check API server endpoint
    API_ENDPOINT=$(kubectl get endpointslice kubernetes -n default -o jsonpath='{.endpoints[0].addresses[0]}' 2>/dev/null || echo "unknown")
    log_info "Kubernetes API endpoint: $API_ENDPOINT"
    
    # Check port binding
    PORT_BINDING=$(docker port ${CLUSTER_NAME}-control-plane 6443 2>/dev/null || echo "unknown")
    log_info "Docker port binding: $PORT_BINDING"
    
    # Verify we can reach API via Tailscale IP
    if curl -sk --connect-timeout 5 "https://${TAILSCALE_IP}:6443/healthz" | grep -q "ok"; then
        log_info "API server is accessible via Tailscale IP"
    else
        log_warn "API server may not be accessible via Tailscale IP yet"
    fi
    
    echo ""
    log_info "Cluster setup complete!"
    echo ""
    echo "To provision Azure VMs that join this cluster, run:"
    echo ""
    echo "  bin/azure \\"
    echo "    --resource-group stargate-vapa-<N> \\"
    echo "    --subscription-id \"\$AZURE_SUBSCRIPTION_ID\" \\"
    echo "    --vm-name stargate-azure-vm<N> \\"
    echo "    --tailscale-auth-key \"\$TAILSCALE_AUTH_KEY\""
    echo ""
}

# Main
main() {
    echo "========================================"
    echo "  Stargate Kind Cluster Setup"
    echo "========================================"
    echo ""
    
    check_prerequisites
    get_tailscale_ip
    delete_existing_cluster
    create_cluster
    install_cni
    configure_apiserver_advertise
    verify_cluster
}

main "$@"
