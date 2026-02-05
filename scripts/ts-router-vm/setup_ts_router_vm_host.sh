#!/bin/bash
#
# setup_ts_router_vm_host.sh
#
# Creates a Tailscale subnet router VM on the host WITHOUT disturbing
# the existing Headscale + tailscale client running on the host.
#
# Usage:
#   sudo ./setup_ts_router_vm_host.sh
#
# Environment variables (or edit defaults below):
#   TRUNK_IFACE   - Physical NIC connected to trunk port (default: eno1)
#   BRIDGE_NAME   - Name for the VLAN-aware bridge (default: br-trunk)
#   VM_NAME       - Name for the router VM (default: ts-router)
#   VLAN_LIST     - Space-separated VLAN IDs (default: "10 20 99")
#   VM_RAM_MB     - VM RAM in MB (default: 2048)
#   VM_VCPUS      - VM vCPUs (default: 2)
#   VM_DISK_GB    - VM disk size in GB (default: 20)
#

set -euo pipefail

# =============================================================================
# Configuration (override via environment variables)
# =============================================================================
TRUNK_IFACE="${TRUNK_IFACE:-eno1}"
BRIDGE_NAME="${BRIDGE_NAME:-br-trunk}"
VM_NAME="${VM_NAME:-stargate-boulderlab-gateway}"
VLAN_LIST="${VLAN_LIST:-10 20 99}"
VM_RAM_MB="${VM_RAM_MB:-2048}"
VM_VCPUS="${VM_VCPUS:-2}"
VM_DISK_GB="${VM_DISK_GB:-20}"

# Paths
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
VM_DIR="/var/lib/libvirt/images/${VM_NAME}"
CLOUD_IMAGE_URL="https://cloud-images.ubuntu.com/jammy/current/jammy-server-cloudimg-amd64.img"
CLOUD_IMAGE_NAME="ubuntu-22.04-cloudimg.qcow2"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

log_info()  { echo -e "${GREEN}[INFO]${NC} $*"; }
log_warn()  { echo -e "${YELLOW}[WARN]${NC} $*"; }
log_error() { echo -e "${RED}[ERROR]${NC} $*"; }

# =============================================================================
# Pre-flight checks
# =============================================================================
preflight_checks() {
    log_info "Running pre-flight checks..."

    # Must be root
    if [[ $EUID -ne 0 ]]; then
        log_error "This script must be run as root (sudo)"
        exit 1
    fi

    # Check OS
    if [[ -f /etc/os-release ]]; then
        . /etc/os-release
        if [[ "$ID" != "ubuntu" && "$ID" != "debian" ]]; then
            log_warn "This script is designed for Ubuntu/Debian. Your OS: $ID"
            log_warn "Proceeding, but some commands may fail."
        fi
    fi

    # Check trunk interface exists
    if ! ip link show "$TRUNK_IFACE" &>/dev/null; then
        log_error "Trunk interface '$TRUNK_IFACE' does not exist!"
        log_info "Available interfaces:"
        ip -br link show
        exit 1
    fi

    # Warn if interface has IP (we won't move it)
    if ip addr show "$TRUNK_IFACE" | grep -q "inet "; then
        log_warn "Interface '$TRUNK_IFACE' has IP addresses configured."
        log_warn "We will NOT move these to the bridge to avoid breaking connectivity."
        log_warn "The bridge will be used ONLY for VM traffic."
    fi

    log_info "Pre-flight checks passed."
}

# =============================================================================
# Install dependencies
# =============================================================================
install_dependencies() {
    log_info "Installing KVM/libvirt packages..."

    apt-get update -qq

    # Core virtualization packages
    PACKAGES=(
        qemu-kvm
        libvirt-daemon-system
        libvirt-clients
        bridge-utils
        virtinst
        cloud-image-utils
        genisoimage
        wget
        jq
    )

    for pkg in "${PACKAGES[@]}"; do
        if dpkg -l "$pkg" &>/dev/null; then
            log_info "  $pkg: already installed"
        else
            log_info "  $pkg: installing..."
            apt-get install -y -qq "$pkg"
        fi
    done

    # Enable and start libvirtd
    systemctl enable --now libvirtd
    log_info "libvirtd is running."
}

# =============================================================================
# Create VLAN-aware bridge (for VM only, host IPs untouched)
# =============================================================================
create_vlan_bridge() {
    log_info "Creating VLAN-aware bridge '$BRIDGE_NAME'..."

    # Check if bridge already exists
    if ip link show "$BRIDGE_NAME" &>/dev/null; then
        log_info "Bridge '$BRIDGE_NAME' already exists."
    else
        # Create the bridge with VLAN filtering enabled
        ip link add name "$BRIDGE_NAME" type bridge
        ip link set "$BRIDGE_NAME" type bridge vlan_filtering 1
        ip link set "$BRIDGE_NAME" up
        log_info "Created bridge '$BRIDGE_NAME' with VLAN filtering."
    fi

    # Attach trunk interface to bridge (if not already)
    CURRENT_MASTER=$(ip -j link show "$TRUNK_IFACE" | jq -r '.[0].master // empty')
    if [[ "$CURRENT_MASTER" == "$BRIDGE_NAME" ]]; then
        log_info "Interface '$TRUNK_IFACE' already attached to '$BRIDGE_NAME'."
    elif [[ -n "$CURRENT_MASTER" ]]; then
        log_error "Interface '$TRUNK_IFACE' is already attached to bridge '$CURRENT_MASTER'!"
        log_error "Please detach it first or use a different interface."
        exit 1
    else
        ip link set "$TRUNK_IFACE" master "$BRIDGE_NAME"
        log_info "Attached '$TRUNK_IFACE' to '$BRIDGE_NAME'."
    fi

    # Configure VLANs on the trunk interface
    log_info "Configuring VLAN trunking for VLANs: $VLAN_LIST"

    # First, remove default VLAN 1 from trunk port (optional, for cleanliness)
    bridge vlan del vid 1 dev "$TRUNK_IFACE" 2>/dev/null || true
    bridge vlan del vid 1 dev "$BRIDGE_NAME" self 2>/dev/null || true

    # Add each VLAN as tagged on the trunk interface
    for vlan in $VLAN_LIST; do
        bridge vlan add vid "$vlan" dev "$TRUNK_IFACE"
        bridge vlan add vid "$vlan" dev "$BRIDGE_NAME" self
        log_info "  Added VLAN $vlan to trunk"
    done

    # Ensure trunk interface is up
    ip link set "$TRUNK_IFACE" up

    log_info "Bridge configuration complete."
}

# =============================================================================
# Make bridge config persistent (systemd-networkd or netplan)
# =============================================================================
persist_bridge_config() {
    log_info "Making bridge configuration persistent..."

    # Check if netplan is used (Ubuntu 18.04+)
    if command -v netplan &>/dev/null && [[ -d /etc/netplan ]]; then
        NETPLAN_FILE="/etc/netplan/60-ts-router-bridge.yaml"

        if [[ -f "$NETPLAN_FILE" ]]; then
            log_info "Netplan config already exists at $NETPLAN_FILE"
        else
            cat > "$NETPLAN_FILE" << EOF
# Tailscale Router VM Bridge Configuration
# This bridge is for VM traffic only; host IPs are not moved here.
network:
  version: 2
  bridges:
    ${BRIDGE_NAME}:
      interfaces: []
      parameters:
        stp: false
      # No IP on this bridge - it's just for VM trunk traffic
      # The trunk interface ($TRUNK_IFACE) keeps its existing config
EOF
            log_info "Created $NETPLAN_FILE"
            log_warn "NOTE: We're not fully managing $TRUNK_IFACE via netplan to avoid breaking existing config."
            log_warn "The bridge will be recreated on boot via the systemd service below."
        fi
    fi

    # Create a systemd oneshot service to recreate bridge on boot
    SYSTEMD_SERVICE="/etc/systemd/system/ts-router-bridge.service"
    cat > "$SYSTEMD_SERVICE" << EOF
[Unit]
Description=Tailscale Router VM Bridge Setup
After=network-pre.target
Before=network.target
Wants=network.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/bin/bash -c 'ip link show ${BRIDGE_NAME} || ip link add name ${BRIDGE_NAME} type bridge; ip link set ${BRIDGE_NAME} type bridge vlan_filtering 1; ip link set ${BRIDGE_NAME} up; ip link set ${TRUNK_IFACE} master ${BRIDGE_NAME} 2>/dev/null || true; ip link set ${TRUNK_IFACE} up; for v in ${VLAN_LIST}; do bridge vlan add vid \$v dev ${TRUNK_IFACE}; bridge vlan add vid \$v dev ${BRIDGE_NAME} self; done; sysctl -w net.ipv4.ip_forward=1; sysctl -w net.ipv4.conf.all.proxy_arp=1; for v in ${VLAN_LIST}; do sysctl -w net.ipv4.conf.${TRUNK_IFACE}v\$v.proxy_arp=1 2>/dev/null || true; done'

[Install]
WantedBy=multi-user.target
EOF

    systemctl daemon-reload
    systemctl enable ts-router-bridge.service
    log_info "Created and enabled ts-router-bridge.service for persistence."
}

# =============================================================================
# Enable IP forwarding and proxy ARP for VM to BMC connectivity
# =============================================================================
configure_routing_for_bmc() {
    log_info "Configuring IP forwarding and proxy ARP for BMC access..."

    # Enable IP forwarding
    sysctl -w net.ipv4.ip_forward=1
    
    # Enable proxy ARP on all interfaces
    sysctl -w net.ipv4.conf.all.proxy_arp=1
    
    # Enable proxy ARP on VLAN interfaces (for BMC access from VM)
    for vlan in $VLAN_LIST; do
        VLAN_IFACE="${TRUNK_IFACE}v${vlan}"
        if ip link show "$VLAN_IFACE" &>/dev/null; then
            sysctl -w "net.ipv4.conf.${VLAN_IFACE}.proxy_arp=1" 2>/dev/null || true
            log_info "  Enabled proxy ARP on $VLAN_IFACE"
        fi
    done
    
    # Add iptables NAT masquerade for VM to BMC traffic
    # This allows the VM (on bridge) to reach devices on the host's VLAN interfaces
    log_info "Configuring iptables NAT for VM to BMC access..."
    
    # Install iptables-persistent if available
    apt-get install -y -qq iptables-persistent 2>/dev/null || true
    
    # Add masquerade rules for each VLAN subnet
    # Assumes 172.18.X.0/24 where X is the VLAN ID
    for vlan in $VLAN_LIST; do
        VLAN_SUBNET="172.18.${vlan}.0/24"
        VLAN_IFACE="${TRUNK_IFACE}v${vlan}"
        
        # Check if rule already exists
        if ! iptables -t nat -C POSTROUTING -s "$VLAN_SUBNET" -o "$VLAN_IFACE" -j MASQUERADE 2>/dev/null; then
            iptables -t nat -A POSTROUTING -s "$VLAN_SUBNET" -o "$VLAN_IFACE" -j MASQUERADE 2>/dev/null || true
            log_info "  Added NAT masquerade for $VLAN_SUBNET via $VLAN_IFACE"
        fi
    done
    
    # Save iptables rules
    if command -v netfilter-persistent &>/dev/null; then
        netfilter-persistent save 2>/dev/null || true
    fi
    
    # Make sysctl settings persistent
    SYSCTL_FILE="/etc/sysctl.d/99-ts-router-vm.conf"
    cat > "$SYSCTL_FILE" << EOF
# Tailscale Router VM - IP forwarding and proxy ARP for BMC access
net.ipv4.ip_forward = 1
net.ipv4.conf.all.proxy_arp = 1
EOF
    
    log_info "Routing configuration complete. Settings persisted to $SYSCTL_FILE"
}

# =============================================================================
# Create libvirt network using the bridge
# =============================================================================
create_libvirt_network() {
    log_info "Creating libvirt network for bridge '$BRIDGE_NAME'..."

    NETWORK_NAME="ts-router-net"

    # Check if network already exists
    if virsh net-info "$NETWORK_NAME" &>/dev/null; then
        log_info "Libvirt network '$NETWORK_NAME' already exists."
        return
    fi

    # Create network XML
    NET_XML=$(mktemp)
    cat > "$NET_XML" << EOF
<network>
  <name>${NETWORK_NAME}</name>
  <forward mode="bridge"/>
  <bridge name="${BRIDGE_NAME}"/>
  <virtualport type='openvswitch'/>
</network>
EOF

    # Try with openvswitch first, fall back to standard bridge
    if ! virsh net-define "$NET_XML" 2>/dev/null; then
        # Fallback: standard Linux bridge (no openvswitch)
        cat > "$NET_XML" << EOF
<network>
  <name>${NETWORK_NAME}</name>
  <forward mode="bridge"/>
  <bridge name="${BRIDGE_NAME}"/>
</network>
EOF
        virsh net-define "$NET_XML"
    fi

    virsh net-start "$NETWORK_NAME"
    virsh net-autostart "$NETWORK_NAME"
    rm -f "$NET_XML"

    log_info "Libvirt network '$NETWORK_NAME' created and started."
}

# =============================================================================
# Download cloud image
# =============================================================================
download_cloud_image() {
    log_info "Preparing cloud image..."

    mkdir -p "$VM_DIR"

    if [[ -f "$VM_DIR/$CLOUD_IMAGE_NAME" ]]; then
        log_info "Cloud image already exists at $VM_DIR/$CLOUD_IMAGE_NAME"
    else
        log_info "Downloading Ubuntu 22.04 cloud image..."
        wget -q --show-progress -O "$VM_DIR/$CLOUD_IMAGE_NAME" "$CLOUD_IMAGE_URL"
        log_info "Download complete."
    fi
}

# =============================================================================
# Create cloud-init ISO
# =============================================================================
create_cloud_init_iso() {
    log_info "Creating cloud-init ISO..."

    CLOUD_INIT_DIR="$VM_DIR/cloud-init"
    mkdir -p "$CLOUD_INIT_DIR"

    # Copy user-data and meta-data from script directory if they exist
    if [[ -f "$SCRIPT_DIR/user-data" ]]; then
        cp "$SCRIPT_DIR/user-data" "$CLOUD_INIT_DIR/user-data"
        log_info "Copied user-data from $SCRIPT_DIR"
    else
        log_error "user-data file not found at $SCRIPT_DIR/user-data"
        log_error "Please create it first (see documentation)"
        exit 1
    fi

    if [[ -f "$SCRIPT_DIR/meta-data" ]]; then
        cp "$SCRIPT_DIR/meta-data" "$CLOUD_INIT_DIR/meta-data"
        log_info "Copied meta-data from $SCRIPT_DIR"
    else
        log_error "meta-data file not found at $SCRIPT_DIR/meta-data"
        log_error "Please create it first (see documentation)"
        exit 1
    fi

    # Create the ISO
    genisoimage -output "$VM_DIR/cloud-init.iso" \
        -volid cidata -joliet -rock \
        "$CLOUD_INIT_DIR/user-data" "$CLOUD_INIT_DIR/meta-data"

    log_info "Cloud-init ISO created at $VM_DIR/cloud-init.iso"
}

# =============================================================================
# Create VM disk
# =============================================================================
create_vm_disk() {
    log_info "Creating VM disk..."

    VM_DISK="$VM_DIR/${VM_NAME}.qcow2"

    if [[ -f "$VM_DISK" ]]; then
        log_warn "VM disk already exists at $VM_DISK"
        read -p "Delete and recreate? (y/N): " confirm
        if [[ "$confirm" =~ ^[Yy]$ ]]; then
            rm -f "$VM_DISK"
        else
            log_info "Keeping existing disk."
            return
        fi
    fi

    # Create a copy of the cloud image and resize it
    cp "$VM_DIR/$CLOUD_IMAGE_NAME" "$VM_DISK"
    qemu-img resize "$VM_DISK" "${VM_DISK_GB}G"

    log_info "Created VM disk: $VM_DISK (${VM_DISK_GB}GB)"
}

# =============================================================================
# Create and start VM
# =============================================================================
create_vm() {
    log_info "Creating VM '$VM_NAME'..."

    # Check if VM already exists
    if virsh dominfo "$VM_NAME" &>/dev/null; then
        log_warn "VM '$VM_NAME' already exists."
        read -p "Delete and recreate? (y/N): " confirm
        if [[ "$confirm" =~ ^[Yy]$ ]]; then
            virsh destroy "$VM_NAME" 2>/dev/null || true
            virsh undefine "$VM_NAME" --remove-all-storage 2>/dev/null || true
            # Recreate disk since we deleted it
            create_vm_disk
        else
            log_info "Keeping existing VM."
            return
        fi
    fi

    VM_DISK="$VM_DIR/${VM_NAME}.qcow2"

    # Create the VM with trunk NIC attached to bridge
    virt-install \
        --name "$VM_NAME" \
        --ram "$VM_RAM_MB" \
        --vcpus "$VM_VCPUS" \
        --os-variant ubuntu22.04 \
        --disk path="$VM_DISK",format=qcow2,bus=virtio \
        --disk path="$VM_DIR/cloud-init.iso",device=cdrom \
        --network bridge="$BRIDGE_NAME",model=virtio \
        --graphics none \
        --console pty,target_type=serial \
        --noautoconsole \
        --import

    log_info "VM '$VM_NAME' created and starting..."

    # Wait for VM to be running
    sleep 5
    if virsh domstate "$VM_NAME" | grep -q running; then
        log_info "VM '$VM_NAME' is running."
    else
        log_error "VM failed to start. Check: virsh console $VM_NAME"
        exit 1
    fi
}

# =============================================================================
# Configure VM NIC for VLAN trunking
# =============================================================================
configure_vm_trunk() {
    log_info "Configuring VM NIC for VLAN trunking..."

    # Get the VM's vnet interface
    VNET_IFACE=$(virsh domiflist "$VM_NAME" | grep "$BRIDGE_NAME" | awk '{print $1}')

    if [[ -z "$VNET_IFACE" ]]; then
        log_error "Could not find VM's vnet interface on bridge $BRIDGE_NAME"
        exit 1
    fi

    log_info "VM interface: $VNET_IFACE"

    # Remove default VLAN and add trunk VLANs
    bridge vlan del vid 1 dev "$VNET_IFACE" 2>/dev/null || true

    for vlan in $VLAN_LIST; do
        bridge vlan add vid "$vlan" dev "$VNET_IFACE"
        log_info "  Added VLAN $vlan to $VNET_IFACE"
    done

    log_info "VM NIC configured for VLAN trunk."
}

# =============================================================================
# Configure host networking for VM communication
# =============================================================================
configure_host_networking() {
    log_info "Configuring host networking for VM communication..."

    # Create VLAN interfaces on the bridge for host<->VM communication
    for vlan in $VLAN_LIST; do
        VLAN_IFACE="br-trunk.${vlan}"
        
        if ip link show "$VLAN_IFACE" &>/dev/null; then
            log_info "  $VLAN_IFACE already exists"
        else
            ip link add link "$BRIDGE_NAME" name "$VLAN_IFACE" type vlan id "$vlan"
            log_info "  Created $VLAN_IFACE"
        fi
        
        # Assign an IP for host to communicate with VM (use .252)
        case $vlan in
            10) HOST_IP="172.18.10.252/24" ;;
            20) HOST_IP="172.18.20.252/24" ;;
            99) HOST_IP="172.18.99.252/24" ;;
            *)  HOST_IP="" ;;
        esac
        
        if [[ -n "$HOST_IP" ]]; then
            ip addr add "$HOST_IP" dev "$VLAN_IFACE" 2>/dev/null || log_info "  $VLAN_IFACE IP already set"
        fi
        
        ip link set "$VLAN_IFACE" up
    done

    # Add host routes to reach VM on each VLAN
    ip route add 172.18.10.253/32 dev br-trunk.10 2>/dev/null || true
    ip route add 172.18.20.253/32 dev br-trunk.20 2>/dev/null || true
    ip route add 172.18.99.253/32 dev br-trunk.99 2>/dev/null || true

    # Get the WAN interface (interface with default route)
    WAN_IFACE=$(ip route | grep default | awk '{print $5}' | head -1)
    log_info "WAN interface: $WAN_IFACE"

    # Add NAT/masquerade for VM to reach internet
    if [[ -n "$WAN_IFACE" ]]; then
        for subnet in "172.18.10.0/24" "172.18.20.0/24" "172.18.99.0/24"; do
            if ! iptables -t nat -C POSTROUTING -s "$subnet" -o "$WAN_IFACE" -j MASQUERADE 2>/dev/null; then
                iptables -t nat -A POSTROUTING -s "$subnet" -o "$WAN_IFACE" -j MASQUERADE
                log_info "  Added NAT for $subnet via $WAN_IFACE"
            fi
        done
    fi

    log_info "Host networking configured."
}

# =============================================================================
# Print status and next steps
# =============================================================================
print_status() {
    echo ""
    echo "============================================================================="
    echo "                         SETUP COMPLETE"
    echo "============================================================================="
    echo ""
    log_info "Bridge status:"
    ip -br link show "$BRIDGE_NAME"
    echo ""

    log_info "VLAN configuration on bridge:"
    bridge vlan show dev "$BRIDGE_NAME"
    bridge vlan show dev "$TRUNK_IFACE"
    echo ""

    log_info "VM status:"
    virsh domstate "$VM_NAME"
    echo ""

    log_info "Libvirt network:"
    virsh net-list --all | grep ts-router
    echo ""

    # Get VM's vnet interface
    VNET_IFACE=$(virsh domiflist "$VM_NAME" | grep "$BRIDGE_NAME" | awk '{print $1}' || echo "unknown")
    if [[ -n "$VNET_IFACE" && "$VNET_IFACE" != "unknown" ]]; then
        log_info "VM NIC VLAN config:"
        bridge vlan show dev "$VNET_IFACE"
    fi

    echo ""
    echo "============================================================================="
    echo "                         NEXT STEPS"
    echo "============================================================================="
    echo ""
    echo "1. Connect to the VM console:"
    echo "   virsh console $VM_NAME"
    echo ""
    echo "2. Or SSH once Tailscale is up (from another Tailscale node):"
    echo "   ssh ubuntu@<tailscale-ip>"
    echo ""
    echo "3. Check Tailscale status on the VM:"
    echo "   tailscale status"
    echo ""
    echo "4. Approve subnet routes in Tailscale Admin Console:"
    echo "   https://login.tailscale.com/admin/machines"
    echo ""
    echo "5. Validate routing (see validate_ts_router_vm.md)"
    echo ""
}

# =============================================================================
# Main
# =============================================================================
main() {
    echo ""
    echo "============================================================================="
    echo "  Tailscale Subnet Router VM Setup"
    echo "============================================================================="
    echo ""
    echo "Configuration:"
    echo "  TRUNK_IFACE: $TRUNK_IFACE"
    echo "  BRIDGE_NAME: $BRIDGE_NAME"
    echo "  VM_NAME:     $VM_NAME"
    echo "  VLAN_LIST:   $VLAN_LIST"
    echo "  VM_RAM_MB:   $VM_RAM_MB"
    echo "  VM_VCPUS:    $VM_VCPUS"
    echo "  VM_DISK_GB:  $VM_DISK_GB"
    echo ""
    read -p "Proceed with setup? (y/N): " confirm
    if [[ ! "$confirm" =~ ^[Yy]$ ]]; then
        echo "Aborted."
        exit 0
    fi

    preflight_checks
    install_dependencies
    create_vlan_bridge
    persist_bridge_config
    configure_routing_for_bmc
    create_libvirt_network
    download_cloud_image
    create_cloud_init_iso
    create_vm_disk
    create_vm
    configure_vm_trunk
    configure_host_networking
    print_status

    log_info "Setup complete!"
}

main "$@"
