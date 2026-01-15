# Stargate

A proof-of-concept for managing bare-metal server lifecycle across multiple datacenters from a central Kubernetes management cluster.

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│  Management Cluster                                         │
│                                                             │
│  ┌─────────────────────────────────────────────────────┐    │
│  │  CRDs                                               │    │
│  │  - Server (inventory)                               │    │
│  │  - ProvisioningProfile (provisioning config)        │    │
│  │  - Operation (operations)                           │    │
│  └─────────────────────────────────────────────────────┘    │
│                                                             │
│  ┌─────────────────────────────────────────────────────┐    │
│  │  Operation Controller                               │    │
│  │  - Watches Operation CRs                            │    │
│  │  - Calls DC API to execute operations               │    │
│  │  - Updates status                                   │    │
│  └─────────────────────────────────────────────────────┘    │
└──────────────────────────┬──────────────────────────────────┘
                           │
              ┌────────────┴────────────┐
              ▼                         ▼
       ┌─────────────┐           ┌─────────────┐
       │ DC West API │           │ DC East API │
       │ (port 8080) │           │ (port 8081) │
       └─────────────┘           └─────────────┘
```

## Prerequisites

- Go 1.22+
- kubectl
- A Kubernetes cluster (kind, minikube, or real cluster)

## Quick Start

### 1. Install dependencies

```bash
make deps
```

### 2. Build binaries

```bash
make build
```

### 3. Install CRDs

```bash
make install-crds
```

### 4. Start Mock DC APIs

In separate terminals:

```bash
# Terminal 1 - DC West
make run-mockapi-west
```

### 5. Start the Controller

```bash
# Terminal 2 - DC Controller
make run-controller
```

### 6. Create sample resources

```bash
make create-samples
```

### 7. Trigger a repave operation

```bash
kubectl apply -f config/samples/operation-repave.yaml
```

### 8. Watch the operation progress

```bash
kubectl get operations.stargate.io -n dc-west -w
```

You should see:

```
NAME                SERVER       OPERATION   PHASE       AGE
repave-server-001   server-001   repave      Pending     0s
repave-server-001   server-001   repave      Running     1s
repave-server-001   server-001   repave      Succeeded   32s
```

### 9. Check server status

```bash
kubectl get servers -n dc-west
```

You should see:

```
NAME         STATE   OS      IPV4
server-001   ready   2.0.0   10.0.1.5
server-002                   10.0.1.6
```

## Project Structure

```
├── api/v1alpha1/           # CRD type definitions
│   ├── server_types.go
│   ├── provisioningprofile_types.go
│   ├── operation_types.go
│   └── groupversion_info.go
├── cmd/
│   └── simulator/          # QEMU simulator controller
│       └── main.go
├── controller/
│   └── operation_controller.go   # Operation reconciliation logic
├── dcclient/
│   ├── client.go           # DC API interface
│   └── http_client.go      # HTTP implementation
├── mockapi/
│   └── main.go             # Mock DC API server
├── pkg/
│   └── qemu/               # QEMU VM management
│       ├── vm.go           # VM create, start, stop
│       ├── cloudinit.go    # Cloud-init ISO generation
│       ├── network.go      # Bridge/tap networking
│       └── image.go        # Image download/cache
├── scripts/
│   └── setup-demo.sh       # Demo environment setup
├── config/
│   ├── crd/bases/          # CRD YAML manifests
│   └── samples/            # Sample resources
├── main.go                 # Controller entrypoint
├── Makefile
└── README.md
```

## CRDs

### Server

Represents a bare-metal server in a datacenter.

```yaml
apiVersion: stargate.io/v1alpha1
kind: Server
metadata:
  name: server-001
  namespace: dc-west
spec:
  mac: "aa:bb:cc:dd:ee:01"
  ipv4: "10.0.1.5"
  inventory:
    sku: "GPU-8xH100"
    location: "rack-5-slot-12"
status:
  state: ready
  currentOS: "2.0.0"
```

### ProvisioningProfile

Defines provisioning configuration.

```yaml
apiVersion: stargate.io/v1alpha1
kind: ProvisioningProfile
metadata:
  name: os-2-0-0
  namespace: dc-west
spec:
  osVersion: "2.0.0"
  osImage: "https://images.example.com/ubuntu-22-aks-2.0.0.img"
```

### Operation

Triggers an operation on a server.

```yaml
apiVersion: stargate.io/v1alpha1
kind: Operation
metadata:
  name: repave-server-001
  namespace: dc-west
spec:
  serverRef:
    name: server-001
  provisioningProfileRef:
    name: os-2-0-0
  operation: repave
status:
  phase: Succeeded
  dcJobID: "job-1234567890"
```

## Multi-DC Support

To run multiple DCs:

1. Start multiple mock APIs on different ports
2. Run separate controller instances per DC, or configure a single controller to handle multiple DCs (future enhancement)

For now, each controller instance watches all namespaces and connects to one DC API. In production, you would:

- Use namespace selectors to scope each controller to specific namespaces
- Configure each controller with its corresponding DC API URL

## Simulator Mode

The simulator controller creates real QEMU VMs instead of simulating operations via the mock API. This allows you to test the full provisioning flow including cloud-init and Kubernetes node join.

### Prerequisites

- QEMU with KVM support
- ISO generation tool (genisoimage, mkisofs, or xorrisofs)
- kind cluster
- Root access (for networking)

### Simulator Quick Start

```bash
# 1. Setup (creates kind cluster, generates manifests with join command)
sudo ./scripts/setup-demo.sh

# 2. Build
make build

# 3. Install CRDs
kubectl apply -f config/crd/bases/

# 4. Apply demo resources
kubectl apply -f /tmp/stargate-demo/namespace.yaml
kubectl apply -f /tmp/stargate-demo/server.yaml
kubectl apply -f /tmp/stargate-demo/provisioningprofile-k8s-worker.yaml

# 5. Start simulator controller (requires root for networking)
sudo ./bin/simulator

# 6. Trigger repave (in another terminal)
kubectl apply -f /tmp/stargate-demo/operation.yaml

# 7. Watch operation progress
kubectl get operations.stargate.io -n dc-simulator -w

# 8. Watch node join cluster
kubectl get nodes -w
```

### Simulator Architecture

```
┌─────────────────────────────────────────────────────────────┐
│  Kind Cluster (runs on host)                                │
│  ┌─────────────────────────────────────────────────────┐    │
│  │  - Stargate CRDs (Server, ProvisioningProfile, Operation) │    │
│  │  - API server exposed on host IP                    │    │
│  └─────────────────────────────────────────────────────┘    │
└─────────────────────────────────────────────────────────────┘
       │
       │ watches Operation CRs
       ▼
┌─────────────────────────────────────────────────────────────┐
│  Simulator Controller (runs on host, outside cluster)      │
│  - Watches Operation CRs                                   │
│  - On repave: creates QEMU VM with cloud-init from ProvisioningProfile │
│  - Updates Server status with VM IP                        │
│  - Updates Operation status (Pending → Running → Succeeded)│
└─────────────────────────────────────────────────────────────┘
       │
       │ creates
       ▼
┌─────────────────────────────────────────────────────────────┐
│  QEMU VM (bridge network: 192.168.100.0/24)                 │
│  - Boots Ubuntu cloud image                                 │
│  - Runs cloud-init from ProvisioningProfile                 │
│  - Installs containerd, kubelet, kubeadm                    │
│  - Executes kubeadm join to join kind cluster               │
└─────────────────────────────────────────────────────────────┘
```

### Technical Details

- **VM Specs**: 2 CPU, 4GB RAM, 20GB disk
- **Base Image**: Ubuntu 22.04 cloud image (auto-downloaded)
- **Networking**: Bridge `stargate-br0` with NAT (192.168.100.0/24)
- **Storage**: `/var/lib/stargate/` (images and VM disks)

## Next Steps

- [ ] Add namespace-scoped controller configuration
- [x] Add cloud-init support for cluster join
- [ ] Add reboot operation
- [ ] Add operation TTL / garbage collection
- [ ] Add metrics and observability
- [ ] Add webhook validation
- [ ] Add VM health monitoring and auto-recovery
