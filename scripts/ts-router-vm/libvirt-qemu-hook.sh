#!/bin/bash
#
# Libvirt QEMU hook to configure VLAN trunking on VM NICs
# This ensures VLAN config is applied after VM starts and NIC attaches to bridge
#
# IMPORTANT: Do NOT use virsh commands here - it causes deadlock with libvirtd!
# Use bridge/ip commands instead to find interfaces.
#

GUEST_NAME="$1"
ACTION="$2"

# Only handle our gateway VM
if [[ "$GUEST_NAME" != "stargate-boulderlab-gateway" ]]; then
    exit 0
fi

# Configuration
BRIDGE_NAME="br-trunk"
VLAN_LIST="10 20 99"
LOG_FILE="/var/log/libvirt-qemu-hook.log"

log() {
    echo "$(date '+%Y-%m-%d %H:%M:%S') [$GUEST_NAME] $*" >> "$LOG_FILE"
}

configure_vlans() {
    log "Configuring VLANs for $GUEST_NAME (action: $ACTION)"
    
    # Find the newest vnet interface on the bridge (don't use virsh - causes deadlock!)
    # Look for vnet interfaces that were just created
    for i in {1..30}; do
        # Find vnet interfaces on the bridge
        VNET=$(bridge link show | grep "$BRIDGE_NAME" | grep -oE 'vnet[0-9]+' | tail -1)
        if [[ -n "$VNET" ]] && ip link show "$VNET" &>/dev/null; then
            log "Found interface: $VNET"
            break
        fi
        sleep 0.2
    done

    if [[ -z "$VNET" ]]; then
        log "ERROR: Could not find vnet interface on $BRIDGE_NAME"
        exit 0
    fi

    # Check if already configured
    CURRENT=$(bridge vlan show dev "$VNET" 2>/dev/null | grep -E "^\s+10$" || true)
    if [[ -n "$CURRENT" ]]; then
        log "VLANs already configured on $VNET"
        exit 0
    fi

    # Remove default VLAN 1
    bridge vlan del vid 1 dev "$VNET" 2>/dev/null || true
    
    # Add trunk VLANs
    for vlan in $VLAN_LIST; do
        bridge vlan add vid "$vlan" dev "$VNET" 2>/dev/null
        log "Added VLAN $vlan to $VNET"
    done

    # Verify
    RESULT=$(bridge vlan show dev "$VNET" 2>/dev/null | tr '\n' ' ')
    log "Final VLAN config: $RESULT"
}

case "$ACTION" in
    started|reconnect)
        configure_vlans
        ;;
esac

exit 0
