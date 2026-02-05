#!/bin/bash
#
# test_ts_router_vm_local.sh
#
# Simplified local test of the Tailscale subnet router VM.
# This version uses NAT networking instead of VLAN trunk.
#
# Usage:
#   sudo ./test_ts_router_vm_local.sh
#

set -euo pipefail

VM_NAME="${VM_NAME:-ts-router-test}"
VM_RAM_MB="${VM_RAM_MB:-2048}"
VM_VCPUS="${VM_VCPUS:-2}"
VM_DISK_GB="${VM_DISK_GB:-10}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
VM_DIR="/var/lib/libvirt/images/${VM_NAME}"
CLOUD_IMAGE_URL="https://cloud-images.ubuntu.com/jammy/current/jammy-server-cloudimg-amd64.img"
CLOUD_IMAGE_NAME="ubuntu-22.04-cloudimg.qcow2"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

log_info()  { echo -e "${GREEN}[INFO]${NC} $*"; }
log_warn()  { echo -e "${YELLOW}[WARN]${NC} $*"; }
log_error() { echo -e "${RED}[ERROR]${NC} $*"; }

# Check root
if [[ $EUID -ne 0 ]]; then
    log_error "Run as root: sudo $0"
    exit 1
fi

# Install dependencies
log_info "Installing dependencies..."
apt-get update -qq
apt-get install -y -qq qemu-kvm libvirt-daemon-system libvirt-clients virtinst cloud-image-utils genisoimage wget

systemctl enable --now libvirtd

# Prepare VM directory
mkdir -p "$VM_DIR"

# Download cloud image if needed
if [[ ! -f "$VM_DIR/$CLOUD_IMAGE_NAME" ]]; then
    log_info "Downloading Ubuntu 22.04 cloud image..."
    wget -q --show-progress -O "$VM_DIR/$CLOUD_IMAGE_NAME" "$CLOUD_IMAGE_URL"
fi

# Create simplified cloud-init for local testing
log_info "Creating cloud-init files..."

CLOUD_INIT_DIR="$VM_DIR/cloud-init"
mkdir -p "$CLOUD_INIT_DIR"

cat > "$CLOUD_INIT_DIR/meta-data" << 'EOF'
instance-id: ts-router-test-001
local-hostname: ts-router-test
EOF

# Get auth key from environment or prompt
TAILSCALE_AUTH_KEY="${TAILSCALE_AUTH_KEY:-}"
if [[ -z "$TAILSCALE_AUTH_KEY" ]]; then
    log_warn "TAILSCALE_AUTH_KEY not set. You'll need to configure it manually in the VM."
    TAILSCALE_AUTH_KEY="__SET_ME__"
fi

cat > "$CLOUD_INIT_DIR/user-data" << EOF
#cloud-config
hostname: ts-router-test
manage_etc_hosts: true

users:
  - name: ubuntu
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
    lock_passwd: false
    plain_text_passwd: changeme

ssh_pwauth: true
chpasswd:
  expire: false

package_update: true
packages:
  - curl
  - jq
  - net-tools

write_files:
  - path: /etc/sysctl.d/99-ip-forward.conf
    content: |
      net.ipv4.ip_forward = 1
    permissions: '0644'

  - path: /opt/setup-tailscale.sh
    permissions: '0755'
    content: |
      #!/bin/bash
      set -euo pipefail
      echo "=== Tailscale Setup ==="
      
      TAILSCALE_AUTHKEY="${TAILSCALE_AUTH_KEY}"
      
      if [[ "\$TAILSCALE_AUTHKEY" == "__SET_ME__" || -z "\$TAILSCALE_AUTHKEY" ]]; then
          echo "ERROR: Set TAILSCALE_AUTHKEY in /opt/setup-tailscale.sh or run:"
          echo "  tailscale up --authkey=YOUR_KEY --ssh"
          exit 1
      fi
      
      # Install Tailscale
      if ! command -v tailscale &>/dev/null; then
          curl -fsSL https://tailscale.com/install.sh | sh
      fi
      
      systemctl enable --now tailscaled
      sleep 2
      
      # For local test, just join tailnet without advertising routes
      # (we don't have the VLAN subnets locally)
      tailscale up \\
          --authkey="\$TAILSCALE_AUTHKEY" \\
          --ssh \\
          --hostname="ts-router-test"
      
      echo "Tailscale connected!"
      tailscale status

runcmd:
  - sysctl --system
  - /opt/setup-tailscale.sh || echo "Run /opt/setup-tailscale.sh manually after setting auth key"

power_state:
  mode: reboot
  condition: false
EOF

# Create cloud-init ISO
genisoimage -output "$VM_DIR/cloud-init.iso" \
    -volid cidata -joliet -rock \
    "$CLOUD_INIT_DIR/user-data" "$CLOUD_INIT_DIR/meta-data"

log_info "Cloud-init ISO created."

# Create VM disk
VM_DISK="$VM_DIR/${VM_NAME}.qcow2"
if [[ -f "$VM_DISK" ]]; then
    log_warn "VM disk exists. Deleting..."
    rm -f "$VM_DISK"
fi
cp "$VM_DIR/$CLOUD_IMAGE_NAME" "$VM_DISK"
qemu-img resize "$VM_DISK" "${VM_DISK_GB}G"

log_info "VM disk created: $VM_DISK"

# Remove existing VM if present
if virsh dominfo "$VM_NAME" &>/dev/null; then
    log_info "Removing existing VM..."
    virsh destroy "$VM_NAME" 2>/dev/null || true
    virsh undefine "$VM_NAME"
fi

# Create VM using default NAT network
log_info "Creating VM..."
virt-install \
    --name "$VM_NAME" \
    --ram "$VM_RAM_MB" \
    --vcpus "$VM_VCPUS" \
    --os-variant ubuntu22.04 \
    --disk path="$VM_DISK",format=qcow2,bus=virtio \
    --disk path="$VM_DIR/cloud-init.iso",device=cdrom \
    --network network=default,model=virtio \
    --graphics none \
    --console pty,target_type=serial \
    --noautoconsole \
    --import

log_info "VM '$VM_NAME' created and starting..."

# Wait for VM
sleep 5

if virsh domstate "$VM_NAME" | grep -q running; then
    log_info "VM is running!"
else
    log_error "VM failed to start"
    exit 1
fi

# Get VM IP (from default NAT network)
log_info "Waiting for VM to get IP..."
for i in {1..30}; do
    VM_IP=$(virsh domifaddr "$VM_NAME" 2>/dev/null | grep -oE '192\.168\.[0-9]+\.[0-9]+' | head -1 || true)
    if [[ -n "$VM_IP" ]]; then
        break
    fi
    sleep 2
done

echo ""
echo "============================================================================="
echo "                         LOCAL TEST VM READY"
echo "============================================================================="
echo ""
echo "VM Name: $VM_NAME"
echo "VM IP:   ${VM_IP:-'(waiting for DHCP, check: virsh domifaddr $VM_NAME)'}"
echo ""
echo "Connect to VM:"
echo "  virsh console $VM_NAME"
echo "  # Login: ubuntu / changeme"
echo ""
if [[ -n "$VM_IP" ]]; then
echo "Or SSH (after a minute for boot):"
echo "  ssh ubuntu@$VM_IP"
echo ""
fi
echo "Check Tailscale status:"
echo "  virsh console $VM_NAME"
echo "  tailscale status"
echo ""
echo "To destroy test VM:"
echo "  sudo virsh destroy $VM_NAME && sudo virsh undefine $VM_NAME"
echo ""
