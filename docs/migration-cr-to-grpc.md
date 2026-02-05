# CRD Retirement: Complete Migration to gRPC API

This document outlines the **complete retirement** of Kubernetes Custom Resources (`Server`, `ProvisioningProfile`, `Operation`) in favor of the gRPC-based baremetal API.

## Executive Summary

**Decision: CRDs are being fully retired.** The Kubernetes CRD-based architecture will be completely replaced by the gRPC API (`baremetal.v1`).

### Why Retire CRDs?

| CRD Limitation | gRPC Solution |
|---|---|
| Requires Kubernetes cluster to manage bare-metal | Standalone gRPC server runs anywhere |
| No streaming - must poll for updates | `WatchOperations` and `StreamOperationLogs` |
| Complex RBAC via K8s service accounts | Simple API tokens or mTLS |
| Namespace isolation adds complexity | Flat machine IDs with label-based filtering |
| controller-runtime overhead | Direct provider calls |
| CRD versioning/migration pain | Proto backward compatibility |

### What the gRPC API Provides

- Real-time operation streaming
- Idempotent operations with request IDs
- Richer machine lifecycle states (phase + effective_state)
- Server-side plan selection
- No Kubernetes dependency for machine management

## Current vs Target Architecture

```
┌─────────────────────────────────────────────────────────────────────────┐
│                         CURRENT ARCHITECTURE                            │
├─────────────────────────────────────────────────────────────────────────┤
│                                                                         │
│   kubectl apply                                                         │
│       │                                                                 │
│       ▼                                                                 │
│   ┌─────────────────┐     ┌─────────────────┐     ┌──────────────────┐  │
│   │ Server CR       │     │ ProvProfile CR  │     │ Operation CR     │  │
│   │ (api/v1alpha1)  │     │ (api/v1alpha1)  │     │ (api/v1alpha1)   │  │
│   └────────┬────────┘     └────────┬────────┘     └────────┬─────────┘  │
│            │                       │                       │            │
│            └───────────────────────┼───────────────────────┘            │
│                                    │                                    │
│                                    ▼                                    │
│                     ┌──────────────────────────┐                        │
│                     │  operation_controller.go │                        │
│                     │  (controller-runtime)    │                        │
│                     └──────────────────────────┘                        │
│                                    │                                    │
│                                    ▼                                    │
│                           SSH Bootstrap                                 │
│                                                                         │
└─────────────────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────────────────┐
│                         TARGET ARCHITECTURE                             │
├─────────────────────────────────────────────────────────────────────────┤
│                                                                         │
│   CLI / UI / SDK                                                        │
│       │                                                                 │
│       ▼                                                                 │
│   ┌─────────────────────────────────────────────────────────────────┐   │
│   │                      gRPC API (baremetal.v1)                    │   │
│   │  ┌─────────────────────┐     ┌───────────────────────────────┐  │   │
│   │  │   MachineService    │     │     OperationService          │  │   │
│   │  │ • RegisterMachine   │     │ • GetOperation                │  │   │
│   │  │ • GetMachine        │     │ • ListOperations              │  │   │
│   │  │ • ListMachines      │     │ • WatchOperations (stream)    │  │   │
│   │  │ • UpdateMachine     │     │ • StreamOperationLogs         │  │   │
│   │  │ • RebootMachine     │     └───────────────────────────────┘  │   │
│   │  │ • ReimageMachine    │                                        │   │
│   │  │ • EnterMaintenance  │                                        │   │
│   │  │ • ExitMaintenance   │                                        │   │
│   │  │ • CancelOperation   │                                        │   │
│   │  └─────────────────────┘                                        │   │
│   └─────────────────────────────────────────────────────────────────┘   │
│                                    │                                    │
│                                    ▼                                    │
│   ┌─────────────────────────────────────────────────────────────────┐   │
│   │                    internal/bmdemo                              │   │
│   │  ┌─────────────┐  ┌──────────────┐  ┌────────────────────────┐  │   │
│   │  │    store    │  │   executor   │  │       provider         │  │   │
│   │  │ (in-memory) │  │ (workflow)   │  │ (fake / real impls)    │  │   │
│   │  └─────────────┘  └──────────────┘  └────────────────────────┘  │   │
│   └─────────────────────────────────────────────────────────────────┘   │
│                                    │                                    │
│                                    ▼                                    │
│                     Provider Implementation                             │
│                     (SSH, BMC, PXE, etc.)                               │
│                                                                         │
└─────────────────────────────────────────────────────────────────────────┘
```

## Type Mappings

### Server CR → Machine (gRPC)

| Server CR Field | Machine gRPC Field | Notes |
|---|---|---|
| `metadata.name` | `machine_id` | Auto-generated if empty |
| `metadata.namespace` | N/A | gRPC is not namespaced |
| `metadata.labels` | `labels` | Direct mapping |
| `spec.mac` | `spec.mac_addresses[0]` | gRPC supports multiple MACs |
| `spec.provider` | `spec.provider` | e.g., "boulder", "azure", "qemu" |
| `spec.ipv4` | `spec.ssh_endpoint` | May include port, e.g., `192.168.1.10:22` |
| `spec.routerIP` | `spec.ssh_endpoint` via jumphost | Or use Tailscale mesh |
| `spec.bmc.address` | `spec.bmc_address` | Optional |
| `spec.bmc.credentialSecretRef` | External secret management | Not stored in proto |
| `spec.inventory.sku` | `labels["sku"]` | Use labels for metadata |
| `spec.inventory.location` | `labels["location"]` | Use labels for metadata |
| `spec.inventory.serialNumber` | `spec.serial` | Direct field in proto |
| `status.state` | `status.effective_state` | Computed by server (read-only) |
| `status.currentOS` | `labels["current_os"]` | Or track via operations |
| `status.appliedProvisioningProfile` | N/A | Profiles don't exist |
| `status.lastUpdated` | `status.last_seen` | Timestamp |
| `status.message` | `status.conditions[*].message` | Use conditions |

### ProvisioningProfile CR → Operation Params + Server Config

The ProvisioningProfile concept is **eliminated**. Its fields are absorbed into:

| ProvisioningProfile Field | New Location | Notes |
|---|---|---|
| `spec.kubernetesVersion` | `Operation.params["k8s_version"]` | Passed per-operation |
| `spec.containerRuntime` | `Operation.params["container_runtime"]` | Or server-side default |
| `spec.tailscaleAuthKeySecretRef` | Provider config / vault | Not in proto |
| `spec.sshCredentialsSecretRef` | Provider config / vault | Not in proto |
| `spec.adminUsername` | Provider config | Server-side |
| `spec.customBootstrapScript` | `plans.Registry` | Server-side plan |

**Rationale**: Provisioning profiles were a Kubernetes pattern for templating. The gRPC API uses:
1. **Server-side plan selection** (`internal/bmdemo/plans/`) - the server chooses the right plan
2. **Operation params** - client passes any overrides (e.g., `image_ref`)
3. **Provider config** - secrets/credentials managed externally

### Operation CR → Operation (gRPC)

| Operation CR Field | Operation gRPC Field | Notes |
|---|---|---|
| `metadata.name` | `operation_id` | Auto-generated |
| `metadata.namespace` | N/A | Not namespaced |
| N/A | `request_id` | **New**: Client-provided for idempotency |
| `spec.serverRef.name` | `machine_id` | Direct reference |
| `spec.provisioningProfileRef.name` | N/A | Absorbed into params |
| `spec.operation` (repave/reboot) | `type` | Enum: REBOOT, REIMAGE, ENTER_MAINTENANCE, EXIT_MAINTENANCE |
| N/A | `params` | **New**: Key-value params (e.g., `image_ref`) |
| `status.phase` | `phase` | PENDING, RUNNING, SUCCEEDED, FAILED, CANCELED |
| `status.startTime` | `started_at` | Timestamp |
| `status.completionTime` | `finished_at` | Timestamp |
| `status.message` | `current_stage` or `error.message` | Progress vs error |
| `status.dcJobID` | Internal only | Not exposed in proto |
| N/A | `error` | **New**: Structured error with code, retryable flag |

## Migration Steps

### Phase 1: Create gRPC Client Library

**Goal**: Create a Go client that controllers can use instead of direct CR access.

1. **Create client package** (`pkg/baremetal/client.go`):
   ```go
   package baremetal

   import (
       pb "github.com/vpatelsj/stargate/gen/baremetal/v1"
       "google.golang.org/grpc"
   )

   type Client struct {
       machines   pb.MachineServiceClient
       operations pb.OperationServiceClient
   }

   func NewClient(conn *grpc.ClientConn) *Client {
       return &Client{
           machines:   pb.NewMachineServiceClient(conn),
           operations: pb.NewOperationServiceClient(conn),
       }
   }
   ```

2. **Implement helper methods** for common patterns:
   ```go
   func (c *Client) WaitForOperation(ctx context.Context, opID string) (*pb.Operation, error)
   func (c *Client) GetMachineByMAC(ctx context.Context, mac string) (*pb.Machine, error)
   ```

### Phase 2: Add Persistent Storage

**Goal**: Replace in-memory store with persistent backend.

The current `internal/bmdemo/store/store.go` uses in-memory maps. Options:
1. **SQLite/PostgreSQL** - Standard relational DB
2. **etcd** - Kubernetes-native, good for small datasets
3. **Redis** - Fast, good for caching + persistence

Recommended: Start with **SQLite** for simplicity, migrate to PostgreSQL for production.

```go
// internal/bmdemo/store/store.go
type Store interface {
    UpsertMachine(*pb.Machine) (*pb.Machine, error)
    GetMachine(machineID string) (*pb.Machine, bool)
    ListMachines() []*pb.Machine
    // ... etc
}

type MemoryStore struct { /* current impl */ }
type SQLiteStore struct { db *sql.DB }
```

### Phase 3: Delete Controllers, Move Logic to gRPC Server

**Goal**: Delete all CR-based controllers. The gRPC server becomes the single source of truth.

The `bmdemo-server` already implements the target pattern. We will:
1. Delete `controller/operation_controller.go`
2. Delete `controller/qemu_operation_controller.go`
3. Delete `cmd/azure-controller/`, `cmd/boulder-controller/`, `cmd/qemu-controller/`
4. Move provider-specific logic into `internal/stargate/provider/` implementations

```go
// cmd/stargate-server/main.go (production version of bmdemo-server)
func main() {
    store := store.NewSQLiteStore("stargate.db")
    
    // Provider implementations for different environments
    providers := map[string]provider.Provider{
        "boulder": boulder.NewProvider(boulderCfg),
        "azure":   azure.NewProvider(azureCfg),
        "qemu":    qemu.NewProvider(qemuCfg),
    }
    
    runner := executor.NewRunner(store, providers, plans.NewRegistry())
    
    grpcServer := grpc.NewServer()
    pb.RegisterMachineServiceServer(grpcServer, &machineServer{store: store, runner: runner})
    pb.RegisterOperationServiceServer(grpcServer, &operationServer{store: store, runner: runner})
    
    grpcServer.Serve(listener)
}
```

**Note**: `controller/route_sync_controller.go` watches Kubernetes Nodes (not our CRs), so it stays if AKS route table sync is still needed. It can be moved to a separate binary or integrated into the gRPC server.

### Phase 4: Update CLI/Tooling

**Goal**: Replace `kubectl` commands with gRPC CLI.

1. **Create CLI** (`cmd/stargatecc/main.go`) using Cobra:
   ```go
   // List machines
   stargatecc machines list
   
   // Reimage a machine
   stargatecc machines reimage m-12345 --image ubuntu-2204
   
   // Watch operation
   stargatecc operations watch op-67890
   ```

2. **Remove kubectl dependency** from scripts:
   ```bash
   # Before
   kubectl apply -f operation-repave.yaml
   
   # After
   stargatecc machines reimage m-12345 --image ubuntu-2204
   ```

### Phase 5: Remove CRD Artifacts

**Goal**: Clean up Kubernetes-specific code.

1. Delete these files:
   - `api/v1alpha1/server_types.go`
   - `api/v1alpha1/provisioningprofile_types.go`
   - `api/v1alpha1/operation_types.go`
   - `api/v1alpha1/groupversion_info.go`
   - `api/v1alpha1/zz_generated.deepcopy.go`
   - `config/crd/bases/*`
   - `config/samples/*.yaml` (CR samples)

2. Update imports in remaining files to use `gen/baremetal/v1` instead of `api/v1alpha1`.

3. Remove controller-runtime dependency from go.mod.

## File-by-File Changes

| File | Action | Details |
|---|---|---|
| `api/v1alpha1/*` | **DELETE** | Remove all CR types |
| `config/crd/*` | **DELETE** | Remove CRD manifests |
| `config/samples/*.yaml` | **DELETE** | Remove CR samples (replaced by CLI) |
| `controller/operation_controller.go` | **DELETE** | Logic moves to gRPC server providers |
| `controller/qemu_operation_controller.go` | **DELETE** | Logic moves to gRPC server providers |
| `controller/route_sync_controller.go` | KEEP/MOVE | Move to separate binary if still needed |
| `cmd/azure-controller/` | **DELETE** | Entire directory |
| `cmd/boulder-controller/` | **DELETE** | Entire directory |
| `cmd/qemu-controller/` | **DELETE** | Entire directory |
| `cmd/bmdemo-server/` | RENAME | Becomes `cmd/stargate-server/` |
| `internal/bmdemo/` | RENAME | Becomes `internal/stargate/` |
| `pkg/baremetal/client.go` | CREATE | gRPC client wrapper |
| `cmd/sgctl/` | CREATE | CLI tool (replacing kubectl) |

## Proto Enhancements Needed

Before migration, consider adding to `proto/baremetal/v1/baremetal.proto`:

```protobuf
// 1. Add TargetCluster details for join operations
message ReimageMachineRequest {
  string machine_id = 1;
  string request_id = 2;
  string image_ref = 3;
  TargetClusterRef target_cluster = 4;  // NEW: Where to join after reimage
  string k8s_version = 5;               // NEW: Kubernetes version
}

// 2. Add filtering to list operations
message ListMachinesRequest {
  int32 page_size = 1;
  string page_token = 2;
  string filter = 3;
  string provider = 4;      // NEW: Filter by provider
}

// 3. Add batch operations (optional)
message BatchReimageMachinesRequest {
  repeated string machine_ids = 1;
  string request_id = 2;
  string image_ref = 3;
}
message BatchReimageMachinesResponse {
  repeated Operation operations = 1;
}
```

## Testing Strategy

1. **Unit tests**: Mock gRPC server responses
2. **Integration tests**: Run `stargate-server` with fake provider
3. **E2E tests**: Use real provider against test hardware

## Timeline

| Phase | Duration | Deliverable |
|---|---|---|
| Phase 1: Client Library | 2 days | `pkg/baremetal/client.go` |
| Phase 2: Persistent Storage | 3 days | SQLite store implementation |
| Phase 3: Delete Controllers | 2 days | Remove all CRD-based code |
| Phase 4: CLI Tooling | 2 days | `cmd/sgctl/` |
| Phase 5: Rename & Cleanup | 1 day | `bmdemo` → `stargate` |

**Total: ~10 days**

## Execution Order

```bash
# 1. Create client library and persistent storage (can be parallel)
# 2. Delete CRD files and controllers (big bang)
git rm -r api/v1alpha1/
git rm -r config/crd/
git rm -r config/samples/
git rm controller/operation_controller.go
git rm controller/qemu_operation_controller.go
git rm -r cmd/azure-controller/
git rm -r cmd/boulder-controller/
git rm -r cmd/qemu-controller/

# 3. Rename bmdemo to stargate
git mv cmd/bmdemo-server cmd/stargate-server
git mv cmd/bmdemo-cli cmd/sgctl  
git mv internal/bmdemo internal/stargate

# 4. Update all imports
find . -name '*.go' -exec sed -i 's|internal/bmdemo|internal/stargate|g' {} \;
find . -name '*.go' -exec sed -i 's|api/v1alpha1|gen/baremetal/v1|g' {} \;

# 5. Remove controller-runtime from go.mod
go mod tidy
```

## Files to Delete

The following files will be **permanently removed** as part of this retirement:

```
api/
  v1alpha1/
    groupversion_info.go      # DELETE
    operation_types.go         # DELETE
    provisioningprofile_types.go # DELETE
    server_types.go            # DELETE
    zz_generated.deepcopy.go   # DELETE

config/
  crd/
    bases/                     # DELETE entire directory
  samples/
    operation-repave.yaml      # DELETE
    provisioningprofile-*.yaml # DELETE
    server-*.yaml              # DELETE

controller/
  operation_controller.go      # DELETE (logic moves to gRPC server)
  qemu_operation_controller.go # DELETE (logic moves to gRPC server)

cmd/
  azure-controller/           # DELETE entire directory
  boulder-controller/          # DELETE entire directory  
  qemu-controller/             # DELETE entire directory
```

## Files to Keep/Modify

```
controller/
  route_sync_controller.go    # KEEP - watches K8s Nodes, not our CRs

cmd/
  bmdemo-server/              # RENAME to cmd/stargate-server, enhance for production

internal/bmdemo/              # RENAME to internal/stargate, add real providers
```

## No Rollback Path

This is a one-way migration. CRDs will be deleted from the repository.

If you need to preserve existing CR data:
1. Export machines: `kubectl get servers -o json > servers-backup.json`
2. Convert to gRPC registration calls before deletion
3. There is no path back to CRDs after deletion

## Design Decisions

1. **No multi-tenancy initially**: Single flat namespace for machines. Add tenant labels if needed later.
2. **API tokens for auth**: Simple bearer tokens, upgrade to mTLS for production.
3. **SQLite for MVP**: Start simple, migrate to PostgreSQL when needed.
4. **Single server**: No HA initially. Add leader election later if required.

## Completed Changes

The following changes have been implemented:

### Deleted Files
- `api/v1alpha1/` - All CRD type definitions
- `config/crd/` - CRD manifests
- `config/samples/` - Sample CR YAML files
- `controller/operation_controller.go` - CR-based operation controller
- `controller/qemu_operation_controller.go` - CR-based QEMU controller
- `cmd/azure-controller/` - Azure controller binary
- `cmd/boulder-controller/` - Boulder controller binary
- `cmd/qemu-controller/` - QEMU controller binary
- `cmd/simulator/` - CRD-based simulator

### Renamed
- `cmd/bmdemo-server/` → `cmd/stargate-server/`
- `cmd/bmdemo-cli/` → `cmd/sgctl/`
- `internal/bmdemo/` → `internal/stargate/`

### Created
- `pkg/baremetal/client.go` - gRPC client library

### Kept
- `controller/route_sync_controller.go` - Watches K8s Nodes (not our CRs)
- `cmd/azure/` - Standalone Azure provisioning tool
- `cmd/infra-prep/` - Infrastructure preparation tool
