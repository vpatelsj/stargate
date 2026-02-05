# Tailscale Subnet Router VM for Boulder Lab Gateway

This directory contains scripts to deploy a VM on the Boulder Lab gateway host (`apollo@100.64.0.3`). The VM advertises routes for the lab VLANs, enabling remote access to physical servers via Tailscale.

## Overview

The gateway VM:
- Runs Ubuntu 22.04 with cloud-init
- Connects to Tailscale and advertises subnet routes
- Bridges VLANs 10, 20, and optionally 99 (BMC) to the Tailscale mesh
- Enables SSH and IP forwarding

## Network Configuration

| VLAN | Subnet | VM IP | Purpose |
|------|--------|-------|---------|
| 10 | 172.18.10.0/24 | 172.18.10.253 | Server management |
| 20 | 172.18.20.0/24 | 172.18.20.253 | Server data |
| 99 | 172.18.99.0/24 | 172.18.99.253 | BMC/IPMI (optional) |

## Prerequisites

1. **SSH access to Boulder gateway:**
   ```bash
   ssh -i ~/.ssh/boulder_key apollo@100.64.0.3
   # sudo password is stored in your password manager
   ```

2. **Tailscale auth key** - Get a reusable, pre-authorized key from:
   https://login.tailscale.com/admin/settings/keys

3. **Files in this directory:**
   - `setup_ts_router_vm_host.sh` - Main setup script
   - `user-data` - Cloud-init user configuration
   - `meta-data` - Cloud-init instance metadata
   - `network-config` - Cloud-init network configuration (VLANs)
   - `libvirt-qemu-hook.sh` - Libvirt hook for VLAN persistence (installed automatically)

## Quick Start

### 1. Copy scripts to Boulder gateway

```bash
# From your local machine
scp -i ~/.ssh/boulder_key -r scripts/ts-router-vm apollo@100.64.0.3:~/
```

Or using base64 to preserve special characters:
```bash
for f in user-data meta-data network-config setup_ts_router_vm_host.sh; do
  cat scripts/ts-router-vm/$f | base64 | \
    ssh -i ~/.ssh/boulder_key apollo@100.64.0.3 "base64 -d > ~/ts-router-vm/$f"
done
ssh -i ~/.ssh/boulder_key apollo@100.64.0.3 "chmod +x ~/ts-router-vm/setup_ts_router_vm_host.sh"
```

### 2. Set Tailscale auth key

Get a reusable, pre-authorized auth key from https://login.tailscale.com/admin/settings/keys

The auth key is passed via environment variable (not stored in files).

### 3. Run the setup script

```bash
ssh -i ~/.ssh/boulder_key apollo@100.64.0.3
cd ~/ts-router-vm

# Set the auth key and run (key is NOT saved to disk)
sudo TAILSCALE_AUTHKEY='tskey-auth-YOUR-KEY-HERE' ./setup_ts_router_vm_host.sh
```

The script will:
- Install required packages (libvirt, qemu, bridge-utils)
- Create a VLAN-aware bridge on `eno1`
- Download Ubuntu 22.04 cloud image
- Create cloud-init ISO with network config
- Install a libvirt hook for automatic VLAN configuration
- Create and start the VM
- Configure VLAN trunking on the VM NIC
- Verify VLAN configuration

### 4. Wait for cloud-init (~2-3 minutes)

Monitor progress:
```bash
# From gateway host
ping 172.18.10.253  # Wait until pingable
```

**Note:** SSH to the VM via VLAN IP is blocked by firewall. Access is only via Tailscale:
```bash
# Once Tailscale is connected (check your Tailscale admin console)
ssh ubuntu@<tailscale-ip>  # or use Tailscale SSH
```

### 5. Approve subnet routes

1. Go to https://login.tailscale.com/admin/machines
2. Find `stargate-boulderlab-gateway`
3. Click "Edit route settings"
4. Enable the advertised routes:
   - `172.18.10.0/24`
   - `172.18.20.0/24`

## VM Details

| Property | Value |
|----------|-------|
| VM Name | `stargate-boulderlab-gateway` |
| RAM | 2GB |
| vCPUs | 2 |
| Disk | 20GB |
| OS | Ubuntu 22.04 |
| Default user | `ubuntu` |
| Default password | `changeme` |

## Troubleshooting

### Check VM status
```bash
sudo virsh list --all
sudo virsh domstate stargate-boulderlab-gateway
```

### View VM console
```bash
sudo virsh console stargate-boulderlab-gateway
# Press Enter to get login prompt
# Escape with: Ctrl+]
```

### Check cloud-init logs
```bash
# If VM is running (SSH from gateway)
ssh ubuntu@172.18.10.253 "cat /var/log/cloud-init-output.log"

# If VM is stuck (mount disk directly)
sudo virsh shutdown stargate-boulderlab-gateway
sudo guestmount -a /var/lib/libvirt/images/stargate-boulderlab-gateway/stargate-boulderlab-gateway.qcow2 \
  -i --ro /mnt/vm
cat /mnt/vm/var/log/cloud-init-output.log
sudo umount /mnt/vm
```

### Check Tailscale status
```bash
ssh ubuntu@172.18.10.253 "tailscale status"
ssh ubuntu@172.18.10.253 "cat /var/log/tailscale-setup.log"
```

### Recreate VM from scratch
```bash
sudo virsh destroy stargate-boulderlab-gateway
sudo virsh undefine stargate-boulderlab-gateway
sudo rm -rf /var/lib/libvirt/images/stargate-boulderlab-gateway
sudo ./setup_ts_router_vm_host.sh
```

### Network not working in VM
The NoCloud datasource requires a separate `network-config` file. Make sure it exists and is included in the cloud-init ISO. The file uses cloud-init network config version 1 format.

### VLAN config lost after VM restart
The setup script installs a libvirt hook at `/etc/libvirt/hooks/qemu` that automatically configures VLANs when the VM starts. Check the hook log:
```bash
sudo cat /var/log/libvirt-qemu-hook.log
```

If the hook is missing or broken, reinstall it:
```bash
cd ~/ts-router-vm
sudo cp libvirt-qemu-hook.sh /etc/libvirt/hooks/qemu
sudo chmod +x /etc/libvirt/hooks/qemu
```

**Important:** The hook must NOT use `virsh` commands - this causes a deadlock with libvirtd. It uses `bridge link show` instead.

## Files Reference

### user-data
Cloud-init user configuration containing:
- Package installation (curl, net-tools, iptables, etc.)
- Tailscale setup script at `/opt/setup-tailscale.sh`
- VLAN setup script at `/opt/setup-vlans.sh`
- Firewall rules at `/opt/setup-firewall.sh`
- Sysctl settings for IP forwarding

### network-config
Cloud-init network configuration:
- Physical interface: `enp1s0` (virtio NIC)
- VLAN subinterfaces: `enp1s0.10`, `enp1s0.20`, `enp1s0.99`
- Static IPs and DNS configuration

### meta-data
Minimal instance metadata with hostname.

### setup_ts_router_vm_host.sh
Host setup script that:
- Creates VLAN-aware bridge `br-trunk`
- Downloads Ubuntu cloud image
- Creates cloud-init ISO
- Installs libvirt qemu hook for VLAN persistence
- Deploys VM with virt-install
- Configures VLAN trunking on VM NIC
- Verifies VLAN configuration

### libvirt-qemu-hook.sh
Libvirt hook that automatically configures VLAN trunking on the VM's virtual NIC when the VM starts. Installed to `/etc/libvirt/hooks/qemu`. Uses `bridge link show` to find the vnet interface (not `virsh` which would cause deadlock).

## Security Notes

- Default password `changeme` should be changed after first login
- Tailscale auth key is embedded in user-data - use reusable keys with appropriate tags
- BMC network (VLAN 99) is disabled by default - set `ENABLE_BMC=true` in user-data to enable
- The VM has SSH enabled on Tailscale (`--ssh` flag)
