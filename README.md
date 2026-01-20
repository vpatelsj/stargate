# Stargate - Hybrid Kubernetes Cluster Manager

Stargate enables creating hybrid Kubernetes clusters with a local Kind control plane and remote Azure VMs as worker nodes, connected via Tailscale.

## Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                        Tailscale Network                        │
├─────────────────────────────────────────────────────────────────┤
│                                                                 │
│   ┌──────────────────┐       ┌──────────────────────────────┐  │
│   │  Local Machine   │       │         Azure VMs            │  │
│   │                  │       │                              │  │
│   │  ┌────────────┐  │       │  ┌────────┐  ┌────────┐     │  │
│   │  │   Kind     │  │       │  │  VM 1  │  │  VM 2  │ ... │  │
│   │  │  Control   │◄─┼───────┼──┤ Worker │  │ Worker │     │  │
│   │  │   Plane    │  │       │  │  Node  │  │  Node  │     │  │
│   │  └────────────┘  │       │  └────────┘  └────────┘     │  │
│   │                  │       │                              │  │
│   │  ┌────────────┐  │       └──────────────────────────────┘  │
│   │  │ Controller │  │                                         │
│   │  └────────────┘  │                                         │
│   └──────────────────┘                                         │
└─────────────────────────────────────────────────────────────────┘
```

## Prerequisites

- Docker
- Kind
- kubectl
- Tailscale (installed and authenticated)
- Azure CLI (authenticated)
- Go 1.21+

## Quick Start

### 1. Set Environment Variables

```bash
export TAILSCALE_AUTH_KEY="tskey-auth-..."           # Tailscale auth key for VMs
export TAILSCALE_CLIENT_ID="..."                      # Tailscale OAuth client ID (for cleanup)
export TAILSCALE_CLIENT_SECRET="tskey-client-..."     # Tailscale OAuth client secret (for cleanup)
export AZURE_SUBSCRIPTION_ID="..."                    # Azure subscription ID
```

### 2. Build All Binaries

```bash
make clean-all && make build
```

### 3. Create Kind Cluster with Tailscale

```bash
./scripts/create-mx-cluster.sh
```

This script:
- Creates a Kind cluster named `stargate-demo`
- Installs Tailscale inside the control-plane container
- Configures the API server to be accessible via Tailscale IP
- Installs Flannel CNI
- Installs Stargate CRDs
- Creates `azure-dc` namespace with required secrets (`azure-ssh-credentials`, `tailscale-auth`)
- Creates default `ProvisioningProfile` (`azure-k8s-worker`) in `azure-dc` namespace
- Starts the controller

### 4. Provision Azure VMs

```bash
# Generate unique deployment number (YYMMDDHHmm format)
export DEPLOY_NUM=$(date +%y%m%d%H%M)

bin/prep-dc-inventory \
  --provider azure \
  --subscription-id "$AZURE_SUBSCRIPTION_ID" \
  --resource-group stargate-vapa-$DEPLOY_NUM \
  --location canadacentral \
  --zone 1 \
  --vnet-name stargate-vnet \
  --vnet-cidr 10.50.0.0/16 \
  --subnet-name stargate-subnet \
  --subnet-cidr 10.50.1.0/24 \
  --vm stargate-azure-vm$DEPLOY_NUM-1 \
  --vm stargate-azure-vm$DEPLOY_NUM-2 \
  --vm stargate-azure-vm$DEPLOY_NUM-3 \
  --vm-size Standard_D2s_v5 \
  --admin-username adminuser \
  --ssh-public-key "$HOME/.ssh/id_rsa.pub" \
  --tailscale-auth-key "$TAILSCALE_AUTH_KEY" \
  --namespace azure-dc
```

This command:
- Creates Azure resource group, VNet, subnet, and NSG
- Provisions VMs with Tailscale and Kubernetes prerequisites
- Verifies connectivity via Tailscale
- Creates `Server` CRs in the `azure-dc` namespace

### 5. Bootstrap VMs as Kubernetes Workers

Create an `Operation` for each VM to trigger the bootstrap (use same `DEPLOY_NUM`):

```bash
for i in 1 2 3; do
kubectl apply -f - <<EOF
apiVersion: stargate.io/v1alpha1
kind: Operation
metadata:
  name: bootstrap-vm$DEPLOY_NUM-$i
  namespace: azure-dc
spec:
  serverRef:
    name: stargate-azure-vm$DEPLOY_NUM-$i
  provisioningProfileRef:
    name: azure-k8s-worker
  operation: repave
EOF
done
```

### 6. Verify Cluster

```bash
kubectl get nodes -o wide
kubectl get operations -n azure-dc
kubectl get servers -n azure-dc
```

## Cleanup

### Full Cleanup

Cleans up everything: Kind cluster, Tailscale devices, Azure resource groups, and local processes.

```bash
TAILSCALE_CLIENT_ID="..." \
TAILSCALE_CLIENT_SECRET="tskey-client-..." \
make clean-all
```

### Partial Cleanup

```bash
# Delete only Kind cluster
make clean-kind

# Remove only Tailscale devices
TAILSCALE_CLIENT_ID="..." TAILSCALE_CLIENT_SECRET="..." make clean-tailscale

# Delete only Azure resource groups (stargate-vapa-*)
make clean-azure

# Stop local processes only
make clean-local
```

## Custom Resource Definitions (CRDs)

### Server

Represents a physical or virtual machine that can be provisioned.

```yaml
apiVersion: stargate.io/v1alpha1
kind: Server
metadata:
  name: my-server
spec:
  ipv4: 100.x.x.x          # Tailscale IP
  provisioningProfile: azure-k8s-worker
status:
  state: ready             # pending, provisioning, ready, error
  os: k8s-1.34
```

### ProvisioningProfile

Defines how servers should be provisioned.

```yaml
apiVersion: stargate.io/v1alpha1
kind: ProvisioningProfile
metadata:
  name: azure-k8s-worker
spec:
  kubernetesVersion: "1.34"
  sshCredentialsSecretRef: azure-ssh-credentials
  tailscaleAuthKeySecretRef: tailscale-auth
```

### Operation

Triggers a provisioning operation on a server.

```yaml
apiVersion: stargate.io/v1alpha1
kind: Operation
metadata:
  name: bootstrap-my-server
spec:
  serverRef:
    name: my-server
  provisioningProfileRef:
    name: azure-k8s-worker
  operation: repave        # repave is the only supported operation
status:
  phase: Succeeded         # Pending, Running, Succeeded, Failed
  message: "Bootstrap completed successfully"
```

## Makefile Targets

| Target | Description |
|--------|-------------|
| `make build` | Build all binaries (azure-controller, mockapi, simulator, prep-dc-inventory, azure) |
| `make clean` | Remove built binaries |
| `make clean-all` | Full cleanup: Kind cluster, Azure RGs, Tailscale, local |
| `make clean-kind` | Delete local Kind cluster |
| `make clean-azure` | Delete all stargate-vapa-* Azure resource groups |
| `make clean-tailscale` | Remove stargate-* devices from Tailscale |
| `make clean-local` | Stop processes and clean up local resources |
| `make install-crds` | Install CRDs to cluster |
| `make help` | Show all available targets |

## Troubleshooting

### Controller Logs

```bash
# Follow logs in real-time
tail -f /tmp/stargate-controller.log

# View last 50 lines
tail -50 /tmp/stargate-controller.log

# Search for errors
grep -i error /tmp/stargate-controller.log
```

### Check Operation Status

```bash
kubectl get operation <name> -o yaml
```

### SSH to Azure VM

```bash
ssh adminuser@<tailscale-ip>
```

### Check Tailscale Status in Kind

```bash
docker exec stargate-demo-control-plane tailscale --socket /var/run/tailscale/tailscaled.sock status
```

### Regenerate Join Token

```bash
docker exec stargate-demo-control-plane kubeadm token create --print-join-command
```

## Project Structure

```
stargate/
├── api/v1alpha1/           # CRD type definitions
├── bin/                    # Built binaries
├── cmd/
│   ├── azure/              # Azure VM provisioner (legacy)
│   ├── infra-prep/         # Infrastructure preparation tool
│   └── simulator/          # QEMU VM simulator
├── config/
│   ├── crd/bases/          # CRD YAML manifests
│   └── samples/            # Sample CR YAML files
├── controller/             # Kubernetes controller logic
├── dcclient/               # Datacenter API client
├── mockapi/                # Mock datacenter API
├── pkg/
│   ├── infra/providers/    # Infrastructure provider implementations
│   └── qemu/               # QEMU VM management
├── scripts/
│   └── create-mx-cluster.sh
├── main.go                 # Controller entrypoint
└── Makefile
```
