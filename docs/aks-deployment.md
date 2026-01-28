# Stargate on AKS - Deployment Guide

Deploy Stargate on an existing AKS cluster to provision and manage worker nodes in a remote datacenter.

## Quick Start (Automated)

For CI/CD or one-command deployment, use the automated script:

```bash
# Set required environment variables
export TAILSCALE_AUTH_KEY="tskey-auth-..."
export TAILSCALE_CLIENT_ID="..."
export TAILSCALE_CLIENT_SECRET="tskey-client-..."

# Deploy everything
./scripts/deploy-aks-stargate.sh --name my-cluster --workers 2

# Cleanup when done
./scripts/deploy-aks-stargate.sh --name my-cluster --cleanup
```

The script handles: AKS creation → CRDs → Router VMs → Worker VMs → Bootstrap tokens → Operation CRs.

See [CI/CD Integration](#cicd-integration) for pipeline examples.

---

## Manual Deployment

The following sections walk through each step individually.

## Prerequisites

- Azure CLI (authenticated)
- kubectl
- Tailscale account with:
  - Auth key (`tskey-auth-...`)
  - OAuth client ID and secret (for route approval)
- SSH key pair (`~/.ssh/id_rsa.pub`)
- Go 1.21+ (for building binaries)

## Environment Setup

```bash
# Required
export TAILSCALE_AUTH_KEY="tskey-auth-..."
export TAILSCALE_CLIENT_ID="..."
export TAILSCALE_CLIENT_SECRET="tskey-client-..."

# Azure (optional if already logged in)
export AZURE_SUBSCRIPTION_ID="..."
```

## 1. Build Binaries

```bash
make clean-all && make build
```

This produces:
- `bin/azure-controller` – watches Server/Operation CRs, provisions Azure VMs
- `bin/prep-dc-inventory` – discovers AKS network config, generates Server CRs

## 2. Create AKS Cluster

```bash
# Pick a unique deployment identifier
export DEPLOY_NUM=$(date +%y%m%d%H%M)
export AKS_RG="aks-stargate-${DEPLOY_NUM}"
export AKS_NAME="aks-stargate-${DEPLOY_NUM}"
export LOCATION="canadacentral"

# Create resource group
az group create --name $AKS_RG --location $LOCATION

# Create AKS with Cilium CNI
az aks create \
  --resource-group $AKS_RG \
  --name $AKS_NAME \
  --kubernetes-version 1.33 \
  --network-plugin azure \
  --network-policy cilium \
  --network-dataplane cilium \
  --node-count 2 \
  --node-vm-size Standard_D2s_v5 \
  --generate-ssh-keys

# Get credentials
az aks get-credentials --resource-group $AKS_RG --name $AKS_NAME --overwrite-existing
```

## 3. Install CRDs and Create Namespace

```bash
# Install Stargate CRDs
kubectl apply -f config/crd/bases/

# Create namespace for the DC
kubectl create namespace azure-dc

# Create secrets
kubectl create secret generic azure-ssh-credentials \
  --namespace azure-dc \
  --from-file=ssh-publickey=$HOME/.ssh/id_rsa.pub

kubectl create secret generic tailscale-auth \
  --namespace azure-dc \
  --from-literal=auth-key=$TAILSCALE_AUTH_KEY
```

## 4. Bootstrap RBAC

The controller needs permissions to watch and update CRs:

```bash
kubectl create clusterrolebinding stargate-admin \
  --clusterrole=cluster-admin \
  --user=$(az ad signed-in-user show --query id -o tsv)
```

## 5. Create DC Resource Group

```bash
export DC_RG="stargate-dc-${DEPLOY_NUM}"
export DC_LOCATION="eastus"
export DC_VNET_CIDR="10.70.0.0/16"
export DC_SUBNET_CIDR="10.70.0.0/24"

az group create --name $DC_RG --location $DC_LOCATION
```

## 6. Provision AKS Router

The AKS router sits in the AKS VNet and bridges traffic to/from Tailscale:

```bash
bin/azure-controller provision-aks-router \
  --aks-resource-group $AKS_RG \
  --aks-cluster-name $AKS_NAME \
  --router-name "stargate-aks-router-${DEPLOY_NUM}"
```

This:
- Discovers the AKS VNet, Pod CIDR, and Service CIDR
- Creates a router VM in the AKS VNet
- Configures Tailscale to advertise the AKS network ranges
- Creates Azure UDRs so DC traffic routes through the router

## 7. Provision DC Router

The DC router sits in the simulated datacenter VNet:

```bash
bin/azure-controller provision-dc-router \
  --resource-group $DC_RG \
  --router-name "stargate-dc-router-${DEPLOY_NUM}" \
  --vnet-cidr $DC_VNET_CIDR \
  --subnet-cidr $DC_SUBNET_CIDR \
  --location $DC_LOCATION
```

## 8. Generate Server CRs for Workers

```bash
bin/prep-dc-inventory \
  --aks-resource-group $AKS_RG \
  --aks-cluster-name $AKS_NAME \
  --dc-resource-group $DC_RG \
  --vnet-cidr $DC_VNET_CIDR \
  --subnet-cidr $DC_SUBNET_CIDR \
  --worker-count 2 \
  --output servers.yaml

kubectl apply -f servers.yaml
```

## 9. Run the Controller

```bash
bin/azure-controller \
  --kubeconfig ~/.kube/config \
  --namespace azure-dc \
  --tailscale-client-id $TAILSCALE_CLIENT_ID \
  --tailscale-client-secret $TAILSCALE_CLIENT_SECRET
```

The controller watches for Server and Operation CRs in the `azure-dc` namespace.

## 10. Bootstrap Workers via Operation CR

Create an Operation to provision the workers:

```yaml
apiVersion: stargate.io/v1alpha1
kind: Operation
metadata:
  name: provision-workers
  namespace: azure-dc
spec:
  type: Provision
  target:
    serverSelector:
      matchLabels:
        role: worker
```

```bash
kubectl apply -f - <<EOF
apiVersion: stargate.io/v1alpha1
kind: Operation
metadata:
  name: provision-workers
  namespace: azure-dc
spec:
  type: Provision
  target:
    serverSelector:
      matchLabels:
        role: worker
EOF
```

Monitor progress:

```bash
kubectl get operations -n azure-dc -w
kubectl get servers -n azure-dc
```

## 11. Verify Connectivity

Once workers are provisioned, verify they're reachable from the AKS cluster:

```bash
# Get worker IPs from Server status
kubectl get servers -n azure-dc -o jsonpath='{.items[*].status.ipAddress}'

# Test connectivity from an AKS pod
kubectl run test --rm -it --image=busybox -- sh
# inside pod: ping <worker-ip>
```

## Cleanup

```bash
# Delete Azure resources
az group delete --name $AKS_RG --yes --no-wait
az group delete --name $DC_RG --yes --no-wait

# Remove Tailscale devices (via admin console or API)
```

## Architecture Overview

```
┌─────────────────────────────────────────────────────────────────┐
│                        Tailscale Network                        │
│                                                                 │
│  ┌──────────────┐                          ┌──────────────┐    │
│  │  AKS Router  │◄────── WireGuard ───────►│  DC Router   │    │
│  │  (Azure VM)  │                          │  (Azure VM)  │    │
│  └──────┬───────┘                          └──────┬───────┘    │
│         │                                         │             │
└─────────┼─────────────────────────────────────────┼─────────────┘
          │                                         │
          ▼                                         ▼
┌─────────────────────┐                 ┌─────────────────────┐
│     AKS VNet        │                 │     DC VNet         │
│  ┌───────────────┐  │                 │  ┌───────────────┐  │
│  │  AKS Nodes    │  │                 │  │  Worker VMs   │  │
│  │  (Cilium)     │  │                 │  │  (Cilium)     │  │
│  └───────────────┘  │                 │  └───────────────┘  │
│  Pod CIDR: 10.x.x.x │                 │  10.70.0.0/24       │
└─────────────────────┘                 └─────────────────────┘
```

**Traffic Flow:**
1. AKS pod wants to reach DC worker (10.70.0.10)
2. Azure UDR routes 10.70.0.0/24 → AKS Router VM
3. AKS Router forwards via Tailscale tunnel to DC Router

---

## CI/CD Integration

The `deploy-aks-stargate.sh` script is designed for CI/CD pipelines.

### GitHub Actions Example

```yaml
name: Deploy Stargate

on:
  workflow_dispatch:
    inputs:
      name:
        description: 'Deployment name'
        required: true
      workers:
        description: 'Number of workers'
        default: '1'

jobs:
  deploy:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      
      - name: Azure Login
        uses: azure/login@v2
        with:
          creds: ${{ secrets.AZURE_CREDENTIALS }}
      
      - name: Setup Go
        uses: actions/setup-go@v5
        with:
          go-version: '1.21'
      
      - name: Build
        run: make build
      
      - name: Deploy
        env:
          TAILSCALE_AUTH_KEY: ${{ secrets.TAILSCALE_AUTH_KEY }}
          TAILSCALE_CLIENT_ID: ${{ secrets.TAILSCALE_CLIENT_ID }}
          TAILSCALE_CLIENT_SECRET: ${{ secrets.TAILSCALE_CLIENT_SECRET }}
        run: |
          ./scripts/deploy-aks-stargate.sh \
            --name ${{ github.event.inputs.name }} \
            --workers ${{ github.event.inputs.workers }}
      
      - name: Start Controller
        run: |
          nohup ./bin/azure-controller > /tmp/controller.log 2>&1 &
          sleep 30
          kubectl get nodes
```

### Azure DevOps Example

```yaml
trigger: none

parameters:
  - name: deploymentName
    type: string
  - name: workerCount
    type: number
    default: 1

variables:
  - group: stargate-secrets  # Contains TAILSCALE_* vars

stages:
  - stage: Deploy
    jobs:
      - job: DeployStargate
        pool:
          vmImage: ubuntu-latest
        steps:
          - task: AzureCLI@2
            inputs:
              azureSubscription: 'Azure-Connection'
              scriptType: bash
              scriptLocation: inlineScript
              inlineScript: |
                make build
                ./scripts/deploy-aks-stargate.sh \
                  --name ${{ parameters.deploymentName }} \
                  --workers ${{ parameters.workerCount }}
            env:
              TAILSCALE_AUTH_KEY: $(TAILSCALE_AUTH_KEY)
              TAILSCALE_CLIENT_ID: $(TAILSCALE_CLIENT_ID)
              TAILSCALE_CLIENT_SECRET: $(TAILSCALE_CLIENT_SECRET)
```

### Script Options

| Option | Description | Default |
|--------|-------------|---------|
| `--name NAME` | Deployment name | Auto-generated |
| `--location LOC` | Azure region | canadacentral |
| `--workers N` | Number of worker VMs | 1 |
| `--skip-aks` | Use existing AKS cluster | false |
| `--skip-router` | Skip router provisioning | false |
| `--skip-workers` | Skip worker provisioning | false |
| `--cleanup` | Delete all resources | false |
| `--dry-run` | Show commands without running | false |

### Environment Variables

| Variable | Required | Description |
|----------|----------|-------------|
| `TAILSCALE_AUTH_KEY` | Yes | Tailscale auth key |
| `TAILSCALE_CLIENT_ID` | Yes | OAuth client ID |
| `TAILSCALE_CLIENT_SECRET` | Yes | OAuth client secret |
| `AZURE_SUBSCRIPTION_ID` | No | Uses current default |
| `K8S_VERSION` | No | Kubernetes version (default: 1.33) |
| `VM_SIZE` | No | Worker VM size (default: Standard_D2s_v5) |
4. DC Router delivers to worker on local subnet
