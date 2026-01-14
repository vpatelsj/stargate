# Stargate

A proof-of-concept for managing bare-metal server lifecycle across multiple datacenters from a central Kubernetes management cluster.

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│  Management Cluster                                         │
│                                                             │
│  ┌─────────────────────────────────────────────────────┐    │
│  │  CRDs                                               │    │
│  │  - Hardware (inventory)                             │    │
│  │  - Template (provisioning config)                   │    │
│  │  - Job (operations)                                 │    │
│  └─────────────────────────────────────────────────────┘    │
│                                                             │
│  ┌─────────────────────────────────────────────────────┐    │
│  │  Job Controller                                     │    │
│  │  - Watches Job CRs                                  │    │
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

### 7. Trigger a repave job

```bash
kubectl apply -f config/samples/job-repave.yaml
```

### 8. Watch the job progress

```bash
kubectl get jobs.stargate.io -n dc-west -w
```

You should see:

```
NAME                HARDWARE     OPERATION   PHASE       AGE
repave-server-001   server-001   repave      Pending     0s
repave-server-001   server-001   repave      Running     1s
repave-server-001   server-001   repave      Succeeded   32s
```

### 9. Check hardware status

```bash
kubectl get hardwares -n dc-west
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
│   ├── hardware_types.go
│   ├── template_types.go
│   ├── job_types.go
│   └── groupversion_info.go
├── controller/
│   └── job_controller.go   # Job reconciliation logic
├── dcclient/
│   ├── client.go           # DC API interface
│   └── http_client.go      # HTTP implementation
├── mockapi/
│   └── main.go             # Mock DC API server
├── config/
│   ├── crd/bases/          # CRD YAML manifests
│   └── samples/            # Sample resources
├── main.go                 # Controller entrypoint
├── Makefile
└── README.md
```

## CRDs

### Hardware

Represents a bare-metal server in a datacenter.

```yaml
apiVersion: stargate.io/v1alpha1
kind: Hardware
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

### Template

Defines provisioning configuration.

```yaml
apiVersion: stargate.io/v1alpha1
kind: Template
metadata:
  name: os-2-0-0
  namespace: dc-west
spec:
  osVersion: "2.0.0"
  osImage: "https://images.example.com/ubuntu-22-aks-2.0.0.img"
```

### Job

Triggers an operation on hardware.

```yaml
apiVersion: stargate.io/v1alpha1
kind: Job
metadata:
  name: repave-server-001
  namespace: dc-west
spec:
  hardwareRef:
    name: server-001
  templateRef:
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

## Next Steps

- [ ] Add namespace-scoped controller configuration
- [ ] Add cloud-init support for cluster join
- [ ] Add reboot operation
- [ ] Add job TTL / garbage collection
- [ ] Add metrics and observability
- [ ] Add webhook validation
