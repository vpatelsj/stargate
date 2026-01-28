#!/usr/bin/env bash
#
# deploy-aks-stargate.sh - Full AKS + Stargate deployment for CI/CD
#
# This script automates the complete deployment:
#   1. Creates AKS cluster with Cilium CNI
#   2. Installs Stargate CRDs
#   3. Provisions Tailscale router VM in AKS VNet
#   4. Creates DC infrastructure (router + workers)
#   5. Creates Server CRs for worker nodes
#   6. Bootstraps workers to join the AKS cluster
#
# Environment variables required:
#   TAILSCALE_AUTH_KEY       - Tailscale auth key (reusable, ephemeral recommended)
#   TAILSCALE_CLIENT_ID      - Tailscale OAuth client ID (for route approval)
#   TAILSCALE_CLIENT_SECRET  - Tailscale OAuth client secret
#   AZURE_SUBSCRIPTION_ID    - (optional) Azure subscription ID
#
# Usage:
#   ./scripts/deploy-aks-stargate.sh [options]
#
# Options:
#   --name NAME              Deployment name (default: auto-generated)
#   --location LOCATION      Azure location (default: canadacentral)
#   --workers N              Number of worker VMs (default: 1)
#   --skip-aks              Skip AKS creation (use existing cluster)
#   --skip-router           Skip router VM creation
#   --skip-workers          Skip worker VM creation
#   --cleanup               Destroy all resources and exit
#   --dry-run               Print what would be done without executing
#
set -euo pipefail

# ============================================================================
# Configuration
# ============================================================================

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# Defaults
DEPLOY_NAME="${DEPLOY_NAME:-}"
LOCATION="${LOCATION:-canadacentral}"
WORKER_COUNT="${WORKER_COUNT:-1}"
K8S_VERSION="${K8S_VERSION:-1.33}"
VM_SIZE="${VM_SIZE:-Standard_D2s_v5}"
AKS_NODE_COUNT="${AKS_NODE_COUNT:-2}"
AKS_NODE_SIZE="${AKS_NODE_SIZE:-Standard_D2s_v5}"
ADMIN_USER="${ADMIN_USER:-ubuntu}"
SSH_KEY_PATH="${SSH_KEY_PATH:-${HOME}/.ssh/id_rsa.pub}"
NAMESPACE="${NAMESPACE:-stargate}"

# DC VNet configuration
DC_VNET_CIDR="${DC_VNET_CIDR:-10.50.0.0/16}"
DC_SUBNET_CIDR="${DC_SUBNET_CIDR:-10.50.1.0/24}"

# Feature flags
SKIP_AKS="${SKIP_AKS:-false}"
SKIP_ROUTER="${SKIP_ROUTER:-false}"
SKIP_WORKERS="${SKIP_WORKERS:-false}"
DRY_RUN="${DRY_RUN:-false}"
CLEANUP="${CLEANUP:-false}"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

# ============================================================================
# Helper Functions
# ============================================================================

log_info()  { echo -e "${GREEN}[INFO]${NC}  $*"; }
log_warn()  { echo -e "${YELLOW}[WARN]${NC}  $*"; }
log_error() { echo -e "${RED}[ERROR]${NC} $*"; }
log_step()  { echo -e "\n${BLUE}=== $* ===${NC}"; }

die() {
    log_error "$@"
    exit 1
}

run_cmd() {
    if [[ "${DRY_RUN}" == "true" ]]; then
        echo -e "${YELLOW}[DRY-RUN]${NC} $*"
    else
        "$@"
    fi
}

# Apply YAML from stdin (handles dry-run correctly)
apply_yaml() {
    if [[ "${DRY_RUN}" == "true" ]]; then
        echo -e "${YELLOW}[DRY-RUN]${NC} kubectl apply -f - (yaml content omitted)"
        cat > /dev/null  # Consume stdin
    else
        kubectl apply -f -
    fi
}

check_required_env() {
    local missing=()
    [[ -z "${TAILSCALE_AUTH_KEY:-}" ]] && missing+=("TAILSCALE_AUTH_KEY")
    [[ -z "${TAILSCALE_CLIENT_ID:-}" ]] && missing+=("TAILSCALE_CLIENT_ID")
    [[ -z "${TAILSCALE_CLIENT_SECRET:-}" ]] && missing+=("TAILSCALE_CLIENT_SECRET")
    
    if [[ ${#missing[@]} -gt 0 ]]; then
        die "Missing required environment variables: ${missing[*]}"
    fi
}

check_prerequisites() {
    log_step "Checking prerequisites"
    
    local missing=()
    command -v az &>/dev/null || missing+=("az (Azure CLI)")
    command -v kubectl &>/dev/null || missing+=("kubectl")
    command -v ssh &>/dev/null || missing+=("ssh")
    
    if [[ ${#missing[@]} -gt 0 ]]; then
        die "Missing required tools: ${missing[*]}"
    fi
    
    # Check SSH key
    if [[ ! -f "${SSH_KEY_PATH}" ]]; then
        die "SSH public key not found: ${SSH_KEY_PATH}"
    fi
    
    # Check Azure login
    if ! az account show &>/dev/null; then
        die "Not logged into Azure. Run 'az login' first."
    fi
    
    # Check binaries are built
    if [[ ! -x "${PROJECT_ROOT}/bin/prep-dc-inventory" ]]; then
        log_warn "Binaries not built. Building now..."
        run_cmd make -C "${PROJECT_ROOT}" build
    fi
    
    log_info "All prerequisites satisfied"
}

generate_deploy_name() {
    if [[ -z "${DEPLOY_NAME}" ]]; then
        # Format: YYMMDDHHMM
        DEPLOY_NAME="stargate-$(date +%y%m%d%H%M)"
    fi
    echo "${DEPLOY_NAME}"
}

# ============================================================================
# Parse Arguments
# ============================================================================

parse_args() {
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --name)
                DEPLOY_NAME="$2"
                shift 2
                ;;
            --location)
                LOCATION="$2"
                shift 2
                ;;
            --workers)
                WORKER_COUNT="$2"
                shift 2
                ;;
            --skip-aks)
                SKIP_AKS=true
                shift
                ;;
            --skip-router)
                SKIP_ROUTER=true
                shift
                ;;
            --skip-workers)
                SKIP_WORKERS=true
                shift
                ;;
            --cleanup)
                CLEANUP=true
                shift
                ;;
            --dry-run)
                DRY_RUN=true
                shift
                ;;
            -h|--help)
                show_help
                exit 0
                ;;
            *)
                die "Unknown option: $1"
                ;;
        esac
    done
}

show_help() {
    cat <<EOF
Usage: $(basename "$0") [options]

Deploy AKS cluster with Stargate worker nodes.

Options:
  --name NAME           Deployment name (default: auto-generated)
  --location LOCATION   Azure location (default: canadacentral)
  --workers N           Number of worker VMs (default: 1)
  --skip-aks            Skip AKS creation (use existing cluster)
  --skip-router         Skip router VM creation
  --skip-workers        Skip worker VM creation
  --cleanup             Destroy all resources and exit
  --dry-run             Print what would be done without executing
  -h, --help            Show this help message

Required Environment Variables:
  TAILSCALE_AUTH_KEY        Tailscale auth key
  TAILSCALE_CLIENT_ID       Tailscale OAuth client ID
  TAILSCALE_CLIENT_SECRET   Tailscale OAuth client secret

Optional Environment Variables:
  AZURE_SUBSCRIPTION_ID     Azure subscription (uses current default if not set)
  DEPLOY_NAME               Deployment name
  LOCATION                  Azure location
  K8S_VERSION               Kubernetes version (default: 1.33)
  VM_SIZE                   VM size for workers (default: Standard_D2s_v5)

Example:
  export TAILSCALE_AUTH_KEY=tskey-auth-...
  export TAILSCALE_CLIENT_ID=...
  export TAILSCALE_CLIENT_SECRET=...
  ./scripts/deploy-aks-stargate.sh --name mytest --workers 2
EOF
}

# ============================================================================
# Cleanup Function
# ============================================================================

cleanup_resources() {
    log_step "Cleaning up resources"
    
    local rg_name="${DEPLOY_NAME}"
    
    # Delete AKS resource group (also deletes MC_* group)
    if az group show --name "${rg_name}" &>/dev/null; then
        log_info "Deleting resource group: ${rg_name}"
        run_cmd az group delete --name "${rg_name}" --yes --no-wait
    fi
    
    # Delete DC resource group
    local dc_rg="${rg_name}-dc"
    if az group show --name "${dc_rg}" &>/dev/null; then
        log_info "Deleting resource group: ${dc_rg}"
        run_cmd az group delete --name "${dc_rg}" --yes --no-wait
    fi
    
    log_info "Cleanup initiated. Resources will be deleted in the background."
}

# ============================================================================
# Step 1: Create AKS Cluster
# ============================================================================

create_aks_cluster() {
    log_step "Creating AKS Cluster"
    
    local rg_name="${DEPLOY_NAME}"
    local aks_name="${DEPLOY_NAME}"
    
    # Create resource group
    log_info "Creating resource group: ${rg_name}"
    run_cmd az group create \
        --name "${rg_name}" \
        --location "${LOCATION}" \
        --output none
    
    # Create AKS cluster with Cilium
    log_info "Creating AKS cluster: ${aks_name} (this takes 5-10 minutes)"
    run_cmd az aks create \
        --resource-group "${rg_name}" \
        --name "${aks_name}" \
        --kubernetes-version "${K8S_VERSION}" \
        --network-plugin azure \
        --network-policy cilium \
        --network-dataplane cilium \
        --node-count "${AKS_NODE_COUNT}" \
        --node-vm-size "${AKS_NODE_SIZE}" \
        --generate-ssh-keys \
        --output none
    
    # Get credentials
    log_info "Fetching kubeconfig"
    run_cmd az aks get-credentials \
        --resource-group "${rg_name}" \
        --name "${aks_name}" \
        --overwrite-existing
    
    # Wait for nodes to be ready
    log_info "Waiting for nodes to be ready..."
    run_cmd kubectl wait --for=condition=Ready nodes --all --timeout=300s
    
    log_info "AKS cluster ready"
}

# ============================================================================
# Step 2: Install CRDs
# ============================================================================

install_crds() {
    log_step "Installing Stargate CRDs"
    
    run_cmd kubectl apply -f "${PROJECT_ROOT}/config/crd/bases/"
    
    # Create namespace
    if [[ "${DRY_RUN}" == "true" ]]; then
        echo -e "${YELLOW}[DRY-RUN]${NC} kubectl create namespace ${NAMESPACE}"
    else
        kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
    fi
    
    log_info "CRDs installed"
}

# ============================================================================
# Step 3: Provision AKS Router
# ============================================================================

provision_aks_router() {
    log_step "Provisioning AKS Router"
    
    local rg_name="${DEPLOY_NAME}"
    local aks_name="${DEPLOY_NAME}"
    local router_name="${DEPLOY_NAME}-aks-router"
    
    # Get subscription ID
    local subscription_id
    subscription_id=$(az account show --query id -o tsv)
    
    log_info "Creating Tailscale router in AKS VNet"
    run_cmd "${PROJECT_ROOT}/bin/prep-dc-inventory" \
        -role aks-router \
        -aks-router-name "${router_name}" \
        -aks-cluster-name "${aks_name}" \
        -aks-cluster-rg "${rg_name}" \
        -subscription-id "${subscription_id}" \
        -location "${LOCATION}" \
        -vm-size "${VM_SIZE}" \
        -admin-username "${ADMIN_USER}" \
        -ssh-public-key "${SSH_KEY_PATH}" \
        -tailscale-auth-key "${TAILSCALE_AUTH_KEY}" \
        -tailscale-client-id "${TAILSCALE_CLIENT_ID}" \
        -tailscale-client-secret "${TAILSCALE_CLIENT_SECRET}"
    
    log_info "AKS router provisioned"
}

# ============================================================================
# Step 4: Provision DC Infrastructure
# ============================================================================

provision_dc_infrastructure() {
    log_step "Provisioning DC Infrastructure"
    
    local rg_name="${DEPLOY_NAME}"
    local aks_name="${DEPLOY_NAME}"
    local dc_rg="${DEPLOY_NAME}-dc"
    local router_name="${DEPLOY_NAME}-dc-router"
    
    # Get subscription ID
    local subscription_id
    subscription_id=$(az account show --query id -o tsv)
    
    # Build worker names
    local worker_args=()
    for i in $(seq 1 "${WORKER_COUNT}"); do
        worker_args+=("-vm" "${DEPLOY_NAME}-worker-${i}")
    done
    
    log_info "Creating DC router and ${WORKER_COUNT} worker(s)"
    run_cmd "${PROJECT_ROOT}/bin/prep-dc-inventory" \
        -role dc \
        -router-name "${router_name}" \
        "${worker_args[@]}" \
        -subscription-id "${subscription_id}" \
        -location "${LOCATION}" \
        -resource-group "${dc_rg}" \
        -vnet-name "${DEPLOY_NAME}-dc-vnet" \
        -vnet-cidr "${DC_VNET_CIDR}" \
        -subnet-name "${DEPLOY_NAME}-dc-subnet" \
        -subnet-cidr "${DC_SUBNET_CIDR}" \
        -vm-size "${VM_SIZE}" \
        -admin-username "${ADMIN_USER}" \
        -ssh-public-key "${SSH_KEY_PATH}" \
        -tailscale-auth-key "${TAILSCALE_AUTH_KEY}" \
        -tailscale-client-id "${TAILSCALE_CLIENT_ID}" \
        -tailscale-client-secret "${TAILSCALE_CLIENT_SECRET}" \
        -aks-cluster-name "${aks_name}" \
        -aks-cluster-rg "${rg_name}" \
        -namespace "${NAMESPACE}"
    
    log_info "DC infrastructure provisioned"
}

# ============================================================================
# Step 5: Create Bootstrap Token
# ============================================================================

create_bootstrap_token() {
    log_step "Creating Bootstrap Token"
    
    # Generate token parts (format: [a-z0-9]{6}.[a-z0-9]{16})
    local token_id="starga"
    local token_secret
    token_secret=$(head -c 16 /dev/urandom | base64 | tr -dc 'a-z0-9' | head -c 16)
    
    # Check if bootstrap token secret already exists
    if [[ "${DRY_RUN}" != "true" ]] && kubectl get secret bootstrap-token-${token_id} -n kube-system &>/dev/null; then
        log_info "Bootstrap token already exists"
        # Still save the token for later use
        token_secret=$(kubectl get secret bootstrap-token-${token_id} -n kube-system -o jsonpath='{.data.token-secret}' | base64 -d)
        echo "${token_id}.${token_secret}" > "${PROJECT_ROOT}/.bootstrap-token"
        return
    fi
    
    # Create bootstrap token secret
    cat <<EOF | apply_yaml
apiVersion: v1
kind: Secret
metadata:
  name: bootstrap-token-${token_id}
  namespace: kube-system
type: bootstrap.kubernetes.io/token
stringData:
  token-id: "${token_id}"
  token-secret: "${token_secret}"
  usage-bootstrap-authentication: "true"
  usage-bootstrap-signing: "true"
  auth-extra-groups: "system:bootstrappers:stargate"
EOF
    
    # Grant permissions to bootstrappers
    cat <<EOF | apply_yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: stargate-bootstrap
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: system:node-bootstrapper
subjects:
- apiGroup: rbac.authorization.k8s.io
  kind: Group
  name: system:bootstrappers:stargate
EOF
    
    # Auto-approve CSRs for bootstrappers
    cat <<EOF | apply_yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: stargate-node-autoapprove-bootstrap
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: system:certificates.k8s.io:certificatesigningrequests:nodeclient
subjects:
- apiGroup: rbac.authorization.k8s.io
  kind: Group
  name: system:bootstrappers:stargate
EOF
    
    cat <<EOF | apply_yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: stargate-node-autoapprove-renewal
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: system:certificates.k8s.io:certificatesigningrequests:selfnodeclient
subjects:
- apiGroup: rbac.authorization.k8s.io
  kind: Group
  name: system:nodes
EOF
    
    log_info "Bootstrap token created: ${token_id}.${token_secret}"
    echo "${token_id}.${token_secret}" > "${PROJECT_ROOT}/.bootstrap-token"
}

# ============================================================================
# Step 6: Bootstrap Workers
# ============================================================================

bootstrap_workers() {
    log_step "Bootstrapping Workers"
    
    # Read bootstrap token
    local bootstrap_token
    if [[ -f "${PROJECT_ROOT}/.bootstrap-token" ]]; then
        bootstrap_token=$(cat "${PROJECT_ROOT}/.bootstrap-token")
    elif [[ "${DRY_RUN}" == "true" ]]; then
        bootstrap_token="starga.dryruntoken12345"
    else
        die "Bootstrap token not found. Run create_bootstrap_token first."
    fi
    
    # Get Server CRs (skip in dry-run)
    local servers
    if [[ "${DRY_RUN}" == "true" ]]; then
        # In dry-run, simulate worker names
        servers=""
        for i in $(seq 1 "${WORKER_COUNT}"); do
            servers="${servers} ${DEPLOY_NAME}-worker-${i}"
        done
    else
        servers=$(kubectl get servers -n "${NAMESPACE}" -o jsonpath='{.items[*].metadata.name}')
    fi
    
    if [[ -z "${servers}" ]]; then
        log_warn "No Server CRs found in namespace ${NAMESPACE}"
        return
    fi
    
    log_info "Triggering bootstrap for workers..."
    
    for server in ${servers}; do
        log_info "Creating Operation CR for ${server}"
        
        cat <<EOF | apply_yaml
apiVersion: stargate.io/v1alpha1
kind: Operation
metadata:
  name: bootstrap-${server}
  namespace: ${NAMESPACE}
spec:
  serverRef:
    name: ${server}
  type: bootstrap
  bootstrap:
    token: "${bootstrap_token}"
EOF
    done
    
    log_info "Bootstrap operations created"
}

# ============================================================================
# Step 7: Wait for Nodes
# ============================================================================

wait_for_nodes() {
    log_step "Waiting for Worker Nodes to Join"
    
    local expected_workers="${WORKER_COUNT}"
    local timeout=600
    local interval=10
    local elapsed=0
    
    log_info "Waiting for ${expected_workers} Stargate worker(s) to join..."
    
    while [[ ${elapsed} -lt ${timeout} ]]; do
        local ready_count
        ready_count=$(kubectl get nodes -l kubernetes.azure.com/managed=false --no-headers 2>/dev/null | grep -c "Ready" || true)
        
        if [[ ${ready_count} -ge ${expected_workers} ]]; then
            log_info "All ${expected_workers} worker(s) joined and ready!"
            kubectl get nodes -l kubernetes.azure.com/managed=false
            return 0
        fi
        
        log_info "Waiting... (${ready_count}/${expected_workers} ready, ${elapsed}s elapsed)"
        sleep "${interval}"
        elapsed=$((elapsed + interval))
    done
    
    log_error "Timeout waiting for workers to join"
    kubectl get nodes
    return 1
}

# ============================================================================
# Step 8: Run Controller
# ============================================================================

start_controller() {
    log_step "Starting Stargate Controller"
    
    log_info "Controller will process Operation CRs and bootstrap workers"
    log_info "Run this command to start the controller:"
    echo ""
    echo "  ${PROJECT_ROOT}/bin/azure-controller"
    echo ""
    log_info "Or to run in background:"
    echo ""
    echo "  nohup ${PROJECT_ROOT}/bin/azure-controller > /tmp/stargate-controller.log 2>&1 &"
    echo ""
}

# ============================================================================
# Summary
# ============================================================================

print_summary() {
    log_step "Deployment Summary"
    
    local rg_name="${DEPLOY_NAME}"
    local aks_name="${DEPLOY_NAME}"
    local dc_rg="${DEPLOY_NAME}-dc"
    
    echo ""
    echo "Deployment Name:    ${DEPLOY_NAME}"
    echo "Location:           ${LOCATION}"
    echo "AKS Resource Group: ${rg_name}"
    echo "AKS Cluster:        ${aks_name}"
    echo "DC Resource Group:  ${dc_rg}"
    echo "Workers:            ${WORKER_COUNT}"
    echo "Namespace:          ${NAMESPACE}"
    echo ""
    echo "Kubeconfig:"
    echo "  az aks get-credentials --resource-group ${rg_name} --name ${aks_name}"
    echo ""
    echo "View Stargate resources:"
    echo "  kubectl get servers,operations -n ${NAMESPACE}"
    echo ""
    echo "View worker nodes:"
    echo "  kubectl get nodes -l kubernetes.azure.com/managed=false"
    echo ""
    echo "Cleanup:"
    echo "  $0 --name ${DEPLOY_NAME} --cleanup"
    echo ""
}

# ============================================================================
# Main
# ============================================================================

main() {
    parse_args "$@"
    
    DEPLOY_NAME=$(generate_deploy_name)
    
    if [[ "${CLEANUP}" == "true" ]]; then
        if [[ -z "${DEPLOY_NAME}" ]]; then
            die "--name required for cleanup"
        fi
        cleanup_resources
        exit 0
    fi
    
    check_required_env
    check_prerequisites
    
    log_step "Starting Deployment: ${DEPLOY_NAME}"
    echo ""
    echo "Configuration:"
    echo "  Name:     ${DEPLOY_NAME}"
    echo "  Location: ${LOCATION}"
    echo "  Workers:  ${WORKER_COUNT}"
    echo "  K8s Ver:  ${K8S_VERSION}"
    echo ""
    
    if [[ "${SKIP_AKS}" != "true" ]]; then
        create_aks_cluster
    fi
    
    install_crds
    
    if [[ "${SKIP_ROUTER}" != "true" ]]; then
        provision_aks_router
    fi
    
    if [[ "${SKIP_WORKERS}" != "true" ]]; then
        provision_dc_infrastructure
    fi
    
    create_bootstrap_token
    
    # Note: Controller runs Operations which do the actual bootstrap
    # For now, just create the Operation CRs
    bootstrap_workers
    
    start_controller
    
    print_summary
    
    log_info "Deployment complete! Run the controller to bootstrap workers."
}

main "$@"
