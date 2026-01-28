#!/bin/bash
set -euo pipefail

#
# Stargate AKS E2E Cleanup Script
# Cleans up all resources created by deploy-aks-e2e.sh
#
# Usage: ./scripts/cleanup-aks-e2e.sh <cluster-name>
# Example: ./scripts/cleanup-aks-e2e.sh stargate-aks-e2e-12
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

if [[ -z "$CLUSTER_NAME" ]]; then
    echo "Usage: $0 <cluster-name>"
    echo "Example: $0 stargate-aks-e2e-12"
    exit 1
fi

# Derived names
RESOURCE_GROUP="$CLUSTER_NAME"
DC_RESOURCE_GROUP="${CLUSTER_NAME}-dc"

echo -e "${RED}========================================${NC}"
echo -e "${RED}  WARNING: This will delete:${NC}"
echo -e "${RED}========================================${NC}"
echo ""
echo "  - AKS cluster: $CLUSTER_NAME"
echo "  - Resource group: $RESOURCE_GROUP"
echo "  - DC resource group: $DC_RESOURCE_GROUP"
echo "  - All Tailscale devices containing 'stargate'"
echo ""
read -p "Are you sure you want to continue? (y/N) " -n 1 -r
echo ""

if [[ ! $REPLY =~ ^[Yy]$ ]]; then
    echo "Aborted."
    exit 0
fi

# Kill local processes
log_step "Killing local processes..."
pkill -f "kubectl port-forward" || true
pkill -f azure-controller || true
log_info "Local processes stopped"

# Delete Azure resources
log_step "Deleting AKS cluster..."
az aks delete --name "$CLUSTER_NAME" --resource-group "$RESOURCE_GROUP" --yes --no-wait || true

log_step "Deleting DC resource group..."
az group delete --name "$DC_RESOURCE_GROUP" --yes --no-wait || true

log_step "Deleting main resource group..."
az group delete --name "$RESOURCE_GROUP" --yes --no-wait || true

log_info "Azure resource deletion initiated (running in background)"

# Remove Tailscale devices
log_step "Removing Tailscale devices containing 'stargate'..."

if [[ -n "${TAILSCALE_API_KEY:-}" ]]; then
    DEVICES=$(curl -s -H "Authorization: Bearer ${TAILSCALE_API_KEY}" \
        "https://api.tailscale.com/api/v2/tailnet/-/devices" 2>/dev/null | \
        jq -r '.devices[]? | select(.hostname | contains("stargate")) | "\(.id) \(.hostname)"' 2>/dev/null || echo "")
    
    if [[ -n "$DEVICES" ]]; then
        echo "$DEVICES" | while read -r device_id hostname; do
            if [[ -n "$device_id" ]]; then
                log_info "Deleting device: $hostname ($device_id)"
                curl -s -X DELETE -H "Authorization: Bearer ${TAILSCALE_API_KEY}" \
                    "https://api.tailscale.com/api/v2/device/$device_id" || true
            fi
        done
        log_info "Tailscale devices removed"
    else
        log_info "No Tailscale devices found containing 'stargate'"
    fi
else
    log_info "TAILSCALE_API_KEY not set, skipping Tailscale cleanup"
    log_info "Generate API key at: https://login.tailscale.com/admin/settings/keys"
    log_info "Or manually remove devices at: https://login.tailscale.com/admin/machines"
fi

# Summary
echo ""
echo -e "${GREEN}========================================${NC}"
echo -e "${GREEN}  Cleanup Initiated!${NC}"
echo -e "${GREEN}========================================${NC}"
echo ""
echo "Azure resources are being deleted in the background."
echo "This may take several minutes to complete."
echo ""
echo "Check status with:"
echo "  az group show --name $RESOURCE_GROUP 2>/dev/null || echo 'Deleted'"
echo "  az group show --name $DC_RESOURCE_GROUP 2>/dev/null || echo 'Deleted'"
