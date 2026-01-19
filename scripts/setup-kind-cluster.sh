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

    if [[ -z "${TAILSCALE_AUTH_KEY}" ]]; then
        log_error "TAILSCALE_AUTH_KEY is not set"
        exit 1
    fi
    
    log_info "All prerequisites met"
}

# Get Tailscale IP
install_tailscale_in_control_plane() {
    log_info "Installing Tailscale inside control-plane container..."
    docker exec ${CLUSTER_NAME}-control-plane sh -c "apt-get update && apt-get install -y curl iptables iproute2 ca-certificates" >/dev/null
    docker exec ${CLUSTER_NAME}-control-plane sh -c "curl -fsSL https://tailscale.com/install.sh | sh" >/dev/null
}

start_tailscaled_in_control_plane() {
    log_info "Starting tailscaled inside control-plane..."
    docker exec ${CLUSTER_NAME}-control-plane sh -c "nohup tailscaled --state=/var/lib/tailscale/tailscaled.state --socket=/var/run/tailscale/tailscaled.sock >/var/log/tailscaled.log 2>&1 &"
    sleep 2
    if ! docker exec ${CLUSTER_NAME}-control-plane pidof tailscaled >/dev/null 2>&1; then
        log_error "tailscaled did not start in control-plane"
        exit 1
    fi
}

start_tailscale_in_control_plane() {
    log_info "Bringing up Tailscale inside control-plane..."
    docker exec ${CLUSTER_NAME}-control-plane sh -c "tailscale --socket /var/run/tailscale/tailscaled.sock up --authkey '${TAILSCALE_AUTH_KEY}' --ssh" >/dev/null
}

get_control_plane_tailscale_ip() {
    log_info "Getting control-plane Tailscale IP..."
    TAILSCALE_IP=$(docker exec ${CLUSTER_NAME}-control-plane tailscale --socket /var/run/tailscale/tailscaled.sock ip -4 2>/dev/null | head -n1 || true)
    if [[ -z "$TAILSCALE_IP" ]]; then
        log_error "Could not get control-plane Tailscale IP. Is Tailscale up inside the container?"
        exit 1
    fi
    log_info "Control-plane Tailscale IP: $TAILSCALE_IP"
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
    # Disable default CNI (kindnet) since we'll use Flannel
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
  # Disable kindnet - we'll use Flannel instead
  disableDefaultCNI: true
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

# Configure API server to advertise Tailscale IP and regenerate certs
configure_apiserver_for_tailscale() {
    log_info "Configuring API server for Tailscale connectivity..."
    
    # Step 1: Update the API server manifest to use Tailscale IP
    log_info "Updating --advertise-address to ${TAILSCALE_IP}..."
    docker exec ${CLUSTER_NAME}-control-plane sed -i \
        "s/--advertise-address=[0-9.]*$/--advertise-address=${TAILSCALE_IP}/" \
        /etc/kubernetes/manifests/kube-apiserver.yaml
    
    # Step 2: Regenerate API server certificates with Tailscale IP as SAN
    log_info "Regenerating API server certificate with Tailscale IP as SAN..."

    # Include the container's docker IP in the SAN so controller-manager can talk to the API server
    INTERNAL_IP=$(docker inspect -f '{{range.NetworkSettings.Networks}}{{.IPAddress}}{{end}}' ${CLUSTER_NAME}-control-plane 2>/dev/null || true)
    if [[ -n "$INTERNAL_IP" ]]; then
        log_info "Control-plane container IP: $INTERNAL_IP"
    else
        log_warn "Could not determine control-plane container IP; proceeding without it"
    fi

    SAN_LIST="${TAILSCALE_IP},127.0.0.1,localhost,${CLUSTER_NAME}-control-plane,0.0.0.0,10.96.0.1"
    if [[ -n "$INTERNAL_IP" ]]; then
        SAN_LIST+=",$INTERNAL_IP"
    fi
    
    # Backup existing certs
    docker exec ${CLUSTER_NAME}-control-plane sh -c \
        'mv /etc/kubernetes/pki/apiserver.crt /etc/kubernetes/pki/apiserver.crt.bak && \
         mv /etc/kubernetes/pki/apiserver.key /etc/kubernetes/pki/apiserver.key.bak'
    
    # Regenerate API server cert with Tailscale IP as additional SAN
    docker exec ${CLUSTER_NAME}-control-plane kubeadm init phase certs apiserver \
        --apiserver-advertise-address="${TAILSCALE_IP}" \
        --apiserver-cert-extra-sans="$SAN_LIST"
    
    log_info "Waiting for API server to restart with new certificate..."
    sleep 10
    
    # Force API server restart by modifying the manifest slightly
    docker exec ${CLUSTER_NAME}-control-plane sh -c \
        'echo "# Updated: $(date)" >> /etc/kubernetes/manifests/kube-apiserver.yaml'
    
    sleep 15
    
    # Wait for API server to be ready again
    log_info "Waiting for API server to come back online..."
    for i in {1..60}; do
        if kubectl get nodes &>/dev/null; then
            log_info "API server is ready"
            break
        fi
        if [[ $i -eq 60 ]]; then
            log_error "API server did not come back online"
            exit 1
        fi
        sleep 2
    done
    
    # Verify the new certificate includes Tailscale IP
    log_info "Verifying certificate SANs..."
    docker exec ${CLUSTER_NAME}-control-plane openssl x509 -in /etc/kubernetes/pki/apiserver.crt -noout -text 2>/dev/null | grep -A1 "Subject Alternative Name" || true
    
    # Step 3: Update kubeadm-config and cluster-info to use Tailscale IP
    log_info "Updating kubeadm-config with Tailscale IP as controlPlaneEndpoint..."
    
    # Update kubeadm-config ConfigMap to add controlPlaneEndpoint
    kubectl get cm kubeadm-config -n kube-system -o json | \
        jq --arg ip "${TAILSCALE_IP}" '.data.ClusterConfiguration |= sub("clusterName: kubernetes"; "clusterName: kubernetes\n    controlPlaneEndpoint: \($ip):6443")' | \
        kubectl apply -f -
    
    # Delete old cluster-info so bootstrap-token will recreate it with new endpoint
    kubectl delete cm cluster-info -n kube-public 2>/dev/null || true
    
    # Regenerate cluster-info with the updated kubeadm-config
    log_info "Regenerating cluster-info with Tailscale endpoint..."
    docker exec ${CLUSTER_NAME}-control-plane kubeadm init phase bootstrap-token
    
    # Verify the cluster-info has the correct server
    CLUSTER_INFO_SERVER=$(kubectl get cm cluster-info -n kube-public -o jsonpath='{.data.kubeconfig}' | grep "server:" | awk '{print $2}')
    log_info "Cluster-info server: $CLUSTER_INFO_SERVER"
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
}

# Install Stargate CRDs
install_crds() {
    log_info "Installing Stargate CRDs..."
    
    SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
    CRD_DIR="${SCRIPT_DIR}/../config/crd/bases"
    
    if [[ ! -d "$CRD_DIR" ]]; then
        log_error "CRD directory not found: $CRD_DIR"
        exit 1
    fi
    
    for crd in "$CRD_DIR"/*.yaml; do
        if [[ -f "$crd" ]]; then
            log_info "Applying CRD: $(basename "$crd")"
            kubectl apply -f "$crd"
        fi
    done
    
    log_info "CRDs installed successfully"
    kubectl get crds | grep stargate || true
}

# Build and start the controller
start_controller() {
    log_info "Building and starting Stargate controller..."
    
    SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
    PROJECT_DIR="${SCRIPT_DIR}/.."
    
    # Build the controller
    log_info "Building controller binary..."
    (cd "$PROJECT_DIR" && go build -o bin/controller ./main.go)
    
    if [[ ! -x "${PROJECT_DIR}/bin/controller" ]]; then
        log_error "Controller binary not found after build"
        exit 1
    fi
    
    # Start controller in background (uses KUBECONFIG env or ~/.kube/config automatically)
    log_info "Starting controller in background..."
    nohup "${PROJECT_DIR}/bin/controller" > /tmp/stargate-controller.log 2>&1 &
    CONTROLLER_PID=$!
    
    sleep 2
    if kill -0 "$CONTROLLER_PID" 2>/dev/null; then
        log_info "Controller started with PID $CONTROLLER_PID"
        log_info "Logs: /tmp/stargate-controller.log"
    else
        log_error "Controller failed to start. Check /tmp/stargate-controller.log"
        exit 1
    fi
}

# Print final summary
print_summary() {
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
    echo "Controller PID: $CONTROLLER_PID"
    echo "Controller logs: /tmp/stargate-controller.log"
    echo ""
}

# Main
main() {
    echo "========================================"
    echo "  Stargate Kind Cluster Setup"
    echo "========================================"
    echo ""
    
    check_prerequisites
    delete_existing_cluster
    create_cluster
    install_cni
    install_tailscale_in_control_plane
    start_tailscaled_in_control_plane
    start_tailscale_in_control_plane
    get_control_plane_tailscale_ip
    configure_apiserver_for_tailscale
    verify_cluster
    install_crds
    start_controller
    print_summary
}

main "$@"
