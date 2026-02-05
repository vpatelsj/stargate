# Tailscale Subnet Router VM - Validation Checklist

This document provides step-by-step validation for the Tailscale subnet router VM setup.

---

## Prerequisites

Before validating, ensure:

- [ ] VM is running: `virsh domstate ts-router`
- [ ] Auth key was set in `/etc/default/tailscale-setup` on the VM
- [ ] Tailscale setup script was run: `/opt/setup-tailscale.sh`

---

## 1. VM Health Checks

### 1.1 Access the VM Console

```bash
# From the host (apollo-lab-bou-gw)
virsh console ts-router

# Login: ubuntu / changeme (or use SSH key if configured)
```

### 1.2 Verify VLAN Interfaces

```bash
# Check interfaces are up with correct IPs
ip addr show eth0.10  # Should have 172.18.10.253/24
ip addr show eth0.20  # Should have 172.18.20.253/24
ip addr show eth0.99  # Should have 172.18.99.253/24 (if enabled)

# Check all interfaces at once
ip -br addr
```

**Expected output:**
```
lo               UNKNOWN        127.0.0.1/8 ::1/128
eth0             UP             
eth0.10@eth0     UP             172.18.10.253/24
eth0.20@eth0     UP             172.18.20.253/24
eth0.99@eth0     UP             172.18.99.253/24
tailscale0       UNKNOWN        100.x.x.x/32
```

### 1.3 Verify IP Forwarding

```bash
sysctl net.ipv4.ip_forward
# Should return: net.ipv4.ip_forward = 1
```

### 1.4 Check Tailscale Status

```bash
tailscale status

# Check if connected to your tailnet
tailscale status --json | jq '.BackendState'
# Should return: "Running"
```

---

## 2. Network Connectivity from VM

### 2.1 Ping Lab Nodes on VLAN 10

```bash
# Ping gateway or nodes on VLAN 10
ping -c 3 172.18.10.1
ping -c 3 172.18.10.2
# ... other nodes on this VLAN
```

### 2.2 Ping Lab Nodes on VLAN 20

```bash
ping -c 3 172.18.20.1
ping -c 3 172.18.20.2
```

### 2.3 Ping BMC Network (if enabled)

```bash
# Only if ENABLE_BMC=true was set
ping -c 3 172.18.99.1
```

### 2.4 Verify Tailscale Connectivity

```bash
# Ping another Tailscale node (e.g., your Azure VM)
tailscale ping <azure-vm-tailscale-name>

# Or by IP
ping -c 3 100.x.x.x  # Your Azure VM's Tailscale IP
```

---

## 3. Approve Subnet Routes (Tailscale Admin Console)

### 3.1 Open Admin Console

Go to: **https://login.tailscale.com/admin/machines**

### 3.2 Find the Router Node

Look for: `ts-router-lab` (or whatever hostname was set)

### 3.3 Approve Routes

1. Click on the node
2. Find "Subnet routes" section
3. You should see:
   - `172.18.10.0/24` - **Approve this**
   - `172.18.20.0/24` - **Approve this**
   - `172.18.99.0/24` - **Approve only if BMC access is needed**
4. Click "Save"

### 3.4 Verify Routes are Approved

```bash
# On the VM
tailscale status --json | jq '.Self.AllowedIPs'

# Should show the approved routes
```

---

## 4. End-to-End Validation from Azure VM

### 4.1 Connect to Azure VM

```bash
# SSH to your Azure VM (already on Tailscale)
ssh user@azure-vm-tailscale-name
```

### 4.2 Verify Routes are Received

```bash
# On Azure VM
tailscale status

# Check if routes from ts-router-lab are visible
ip route | grep 172.18
```

**Expected output:**
```
172.18.10.0/24 dev tailscale0 
172.18.20.0/24 dev tailscale0
```

### 4.3 Ping Lab Nodes from Azure

```bash
# From Azure VM
ping -c 3 172.18.10.1
ping -c 3 172.18.10.2
ping -c 3 172.18.20.1
```

### 4.4 SSH to Lab Nodes from Azure

```bash
# From Azure VM - SSH through the Tailscale tunnel to lab nodes
ssh user@172.18.10.2
ssh user@172.18.20.5
```

### 4.5 Traceroute to Verify Path

```bash
# From Azure VM
traceroute 172.18.10.1

# Should show:
# 1. ts-router-lab (100.x.x.x)
# 2. 172.18.10.1 (final destination)
```

---

## 5. Troubleshooting

### 5.1 Routes Not Advertised

**Symptom:** `tailscale status` doesn't show routes

**Check:**
```bash
# On VM
journalctl -u tailscaled -f

# Verify tailscale up command
tailscale up --advertise-routes=172.18.10.0/24,172.18.20.0/24 --snat-subnet-routes=true
```

**Possible causes:**
- Auth key doesn't have route permissions
- Need to re-run tailscale up with correct flags

### 5.2 Routes Not Approved

**Symptom:** Routes show as "pending" in admin console, or remote nodes can't reach subnets

**Fix:**
1. Go to https://login.tailscale.com/admin/machines
2. Click on ts-router-lab
3. Approve the subnet routes

### 5.3 Overlapping CIDRs

**Symptom:** Some Tailscale nodes can't reach lab subnets

**Check:**
```bash
# On remote nodes
tailscale status --json | jq '.Peer[] | select(.AllowedIPs != null) | {name: .HostName, routes: .AllowedIPs}'
```

**Possible causes:**
- Another node is advertising the same subnet
- Node has local network with overlapping CIDR

**Fix:**
- Only one node should advertise each subnet
- Use `--accept-routes=false` on the router if it shouldn't accept routes from others

### 5.4 Forwarding Not Working

**Symptom:** Ping from VM to lab works, but not from remote Tailscale nodes

**Check IP forwarding:**
```bash
# On VM
sysctl net.ipv4.ip_forward
# Must be 1
```

**Check iptables:**
```bash
# On VM
iptables -L FORWARD -v -n

# Should show ACCEPT rules for tailscale0 <-> eth0.X
```

**Fix forwarding:**
```bash
# On VM
sysctl -w net.ipv4.ip_forward=1
/opt/setup-firewall.sh
```

### 5.5 Firewall Blocking Traffic

**Symptom:** Some traffic blocked, logs show drops

**Check logs:**
```bash
# On VM
dmesg | grep iptables
journalctl -k | grep iptables
```

**Temporarily disable firewall for testing:**
```bash
iptables -P FORWARD ACCEPT
iptables -F FORWARD
```

**Then re-enable:**
```bash
/opt/setup-firewall.sh
```

### 5.6 VLAN Not Working

**Symptom:** VM can't reach nodes on VLAN

**Check VLAN tagging on host:**
```bash
# On host (apollo-lab-bou-gw)
bridge vlan show

# Check VM's vnet interface has VLANs
VNET=$(virsh domiflist ts-router | grep br-trunk | awk '{print $1}')
bridge vlan show dev $VNET
```

**Check inside VM:**
```bash
# On VM
cat /proc/net/vlan/config
ip -d link show eth0.10
```

### 5.7 SNAT Issues

**Symptom:** Lab nodes can't reply to Tailscale traffic

By default, `--snat-subnet-routes=true` means the router does SNAT, so lab nodes see traffic from `172.18.X.253` (the router's IP on that VLAN).

If you want lab nodes to see the original Tailscale IP:
```bash
tailscale up --advertise-routes=172.18.10.0/24,172.18.20.0/24 --snat-subnet-routes=false
```

But then you need to add routes on lab nodes pointing to the router.

### 5.8 MTU Issues

**Symptom:** Large packets fail, small pings work

**Check:**
```bash
# Test with larger packet
ping -c 3 -s 1400 172.18.10.1
```

**Fix:**
```bash
# On VM, reduce MTU on VLAN interfaces
ip link set eth0.10 mtu 1280
ip link set eth0.20 mtu 1280
```

---

## 6. Quick Reference Commands

### On the VM (ts-router)

```bash
# Tailscale status
tailscale status
tailscale status --json | jq .

# Re-run setup
source /etc/default/tailscale-setup
/opt/setup-tailscale.sh

# Check routes
ip route
tailscale status --json | jq '.Self.AllowedIPs'

# Check firewall
iptables -L -v -n

# Network debug
tcpdump -i tailscale0 -n
tcpdump -i eth0.10 -n
```

### On the Host (apollo-lab-bou-gw)

```bash
# VM management
virsh list --all
virsh console ts-router
virsh start ts-router
virsh shutdown ts-router

# Bridge status
bridge vlan show
ip link show br-trunk

# Check VM NIC
virsh domiflist ts-router
```

### On Remote Tailscale Nodes (Azure VMs)

```bash
# Check routes
ip route | grep 172.18
tailscale status

# Test connectivity
ping 172.18.10.253  # Router
ping 172.18.10.1    # Lab node

# Traceroute
traceroute 172.18.10.1
```

---

## 7. Security Checklist

- [ ] SSH only accessible via Tailscale (not from VLANs or public)
- [ ] BMC network (VLAN 99) only advertised if explicitly enabled
- [ ] Auth key is reusable but with limited scope/tags
- [ ] Firewall rules are saved persistently
- [ ] Host's Headscale/Tailscale is unchanged and working

---

## 8. Maintenance

### Update Tailscale

```bash
# On VM
curl -fsSL https://tailscale.com/install.sh | sh
systemctl restart tailscaled
```

### Change Advertised Routes

```bash
# On VM
tailscale up --advertise-routes=172.18.10.0/24,172.18.20.0/24,172.18.99.0/24 --snat-subnet-routes=true --ssh

# Then approve new routes in admin console
```

### Rotate Auth Key

1. Generate new key in Tailscale admin
2. Update `/etc/default/tailscale-setup`
3. Run `tailscale logout && source /etc/default/tailscale-setup && /opt/setup-tailscale.sh`
