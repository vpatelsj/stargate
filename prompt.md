# Stargate Simulator Controller

## Goal

Add a simulator datacenter controller that creates real QEMU VMs when an `Operation` CR with `operation: repave` is created. The VM should boot an OS, run cloud-init (defined in the ProvisioningProfile CR), install kubelet, and join a kind cluster as a worker node.

## Context

Stargate is a system for managing bare-metal server lifecycle across multiple datacenters from a central Kubernetes management cluster. We have three CRDs:

- **Server** — represents a bare-metal server (or VM slot in simulator)
- **ProvisioningProfile** — defines provisioning config including cloud-init
- **Operation** — triggers an operation (repave) on a Server using a ProvisioningProfile

Currently there's a mock controller that simulates operations via a fake HTTP API. We want a real simulator that actually creates QEMU VMs.

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│  Kind Cluster (runs on host)                                │
│  ┌─────────────────────────────────────────────────────┐    │
│  │  - Stargate CRDs (Server, ProvisioningProfile, Operation)          │    │
│  │  - API server exposed on host IP                    │    │
│  └─────────────────────────────────────────────────────┘    │
└─────────────────────────────────────────────────────────────┘
       │
       │ watches Operation CRs
       ▼
┌─────────────────────────────────────────────────────────────┐
│  Simulator Controller (runs on host, outside cluster)       │
│  - Watches Operation CRs                                          │
│  - On repave: creates QEMU VM with cloud-init from ProvisioningProfile │
│  - Updates Server status with VM IP                       │
│  - Updates Operation status (Pending → Running → Succeeded)       │
└─────────────────────────────────────────────────────────────┘
       │
       │ creates
       ▼
┌─────────────────────────────────────────────────────────────┐
│  QEMU VM (bridge network: 192.168.100.0/24)                 │
│  - Boots Ubuntu cloud image                                 │
│  - Runs cloud-init from ProvisioningProfile                            │
│  - Installs containerd, kubelet, kubeadm                    │
│  - Executes kubeadm join to join kind cluster               │
└─────────────────────────────────────────────────────────────┘
```

## Requirements

### 1. QEMU VM Management (`pkg/qemu/vm.go`)

- Create VM from Ubuntu cloud image (download and cache)
- Attach cloud-init ISO as second drive
- Use bridge/tap networking so VM gets IP on `192.168.100.0/24`
- Support start, stop, status check
- Store VM disk in work directory (e.g., `/var/lib/stargate/vms/<name>/`)

### 2. Cloud-Init ISO Generation (`pkg/qemu/cloudinit.go`)

- Generate NoCloud datasource ISO from ProvisioningProfile's `spec.cloudInit`
- Include meta-data with hostname and instance-id
- Use `genisoimage`, `mkisofs`, or `xorrisofs`

### 3. Bridge/Tap Networking (`pkg/qemu/network.go`)

- Create bridge `stargate-br0` with IP `192.168.100.1/24`
- Enable NAT/masquerade so VMs can reach external network
- Create tap device per VM, attach to bridge
- Allocate IPs for VMs (192.168.100.11, .12, etc.)

### 4. Simulator Controller (`cmd/simulator/main.go`)

- Kubernetes controller using controller-runtime
- Watches `Operation` CRs in all namespaces
- On new Operation with `operation: repave`:
  1. Fetch referenced Server and ProvisioningProfile
  2. Download base image if not cached
  3. Generate cloud-init ISO from ProvisioningProfile's cloudInit
  4. Create tap device and attach to bridge
  5. Create and start QEMU VM
  6. Update Operation status: Pending → Running
  7. Poll/wait for VM to be running
  8. Update Operation status: Running → Succeeded
  9. Update Server status with IP and state=ready
- Runs outside the cluster (on host) with kubeconfig

### 5. Setup Script (`scripts/setup-demo.sh`)

- Check prerequisites (qemu, genisoimage, kind, kubectl)
- Detect host IP
- Create kind cluster with API server exposed on host IP
- Generate kubeadm join command
- Create ready-to-use manifests:
  - Server CR
  - ProvisioningProfile CR with cloud-init that installs k8s and runs join command
  - Operation CR to trigger repave

## Cloud-Init ProvisioningProfile

The ProvisioningProfile CR should contain cloud-init that:

1. Updates packages
2. Installs containerd
3. Installs kubelet, kubeadm, kubectl
4. Configures kernel modules (overlay, br_netfilter)
5. Disables swap
6. Runs `kubeadm join <host-ip>:6443 --token <token> --discovery-token-ca-cert-hash <hash>`

## Demo Flow

```bash
# 1. Setup (creates kind cluster, generates manifests)
sudo ./scripts/setup-demo.sh

# 2. Build
make build

# 3. Install CRDs
kubectl apply -f config/crd/bases/

# 4. Apply demo resources
kubectl apply -f /tmp/stargate-demo/server.yaml
kubectl apply -f /tmp/stargate-demo/provisioningprofile-k8s-worker.yaml

# 5. Start simulator controller (requires root for networking)
sudo ./bin/simulator

# 6. Trigger repave
kubectl apply -f /tmp/stargate-demo/operation.yaml

# 7. Watch operation progress
kubectl get operations.stargate.io -n dc-simulator -w

# 8. Watch node join cluster
kubectl get nodes -w
```

## Technical Notes

- Ubuntu cloud image URL: `https://cloud-images.ubuntu.com/jammy/current/jammy-server-cloudimg-amd64.img`
- VM specs: 2 CPU, 4GB RAM, 20GB disk
- QEMU command needs KVM acceleration (`-machine type=q35,accel=kvm`)
- Controller runs on host (not in pod) to manage QEMU directly
- Root required for bridge/tap networking and KVM access
- Use controller-runtime for the Kubernetes controller

## Files to Create/Modify

```
stargate/
├── cmd/
│   └── simulator/
│       └── main.go           # NEW: Simulator controller
├── pkg/
│   └── qemu/
│       ├── vm.go             # NEW: QEMU VM management
│       ├── cloudinit.go      # NEW: Cloud-init ISO generation
│       ├── network.go        # NEW: Bridge/tap networking
│       └── image.go          # NEW: Image download/cache
├── scripts/
│   └── setup-demo.sh         # NEW: Demo setup script
├── config/
│   └── samples/
│       └── provisioningprofile-k8s-worker.yaml  # NEW: K8s worker profile
├── Makefile                  # MODIFY: Add simulator target
└── README.md                 # MODIFY: Add simulator docs
```

## Success Criteria

1. Running `kubectl apply -f operation.yaml` creates a QEMU VM
2. VM boots Ubuntu and runs cloud-init
3. After ~3-5 minutes, `kubectl get nodes` shows a new worker node
4. Operation status shows Succeeded
5. Server status shows state=ready with VM's IP
