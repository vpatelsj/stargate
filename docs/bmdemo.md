# Baremetal gRPC Demo

A gRPC service for baremetal machine lifecycle management with streamlined operations.

## Quick Start

```bash
# Terminal 1: Server
go run ./cmd/bmdemo-server

# Terminal 2: CLI
go run ./cmd/bmdemo-cli import 3
go run ./cmd/bmdemo-cli enter-maintenance machine-1
go run ./cmd/bmdemo-cli reimage machine-1
go run ./cmd/bmdemo-cli exit-maintenance machine-1
go run ./cmd/bmdemo-cli list
```

Interactive demo: `./scripts/run-bmdemo.sh` (add `--slow` for live presentations)

## Build

```bash
make bmdemo    # Build binaries
make proto     # Regenerate protobuf
```

## CLI Commands

| Command | Description |
|---------|-------------|
| `import <N>` | Register N machines (machine-1, machine-2, ...) |
| `list` | Show machines with phase and conditions |
| `reboot <id>` | Reboot a machine (READY or MAINTENANCE required) |
| `reimage <id>` | Reimage a machine (MAINTENANCE required) |
| `enter-maintenance <id>` | Enter maintenance mode |
| `exit-maintenance <id>` | Exit maintenance mode (return to READY) |
| `ops` | List all operations |
| `watch [machine-id]` | Stream operation events |
| `logs <operation-id>` | Stream operation logs |
| `cancel <operation-id>` | Cancel active operation |
| `demo` | Run scripted demo for design docs |

## Lifecycle Model

Machines have a **simple three-phase lifecycle** plus boolean conditions:

### Machine Phases
| Phase | Description |
|-------|-------------|
| `FACTORY_READY` | New machine, never provisioned |
| `READY` | Provisioned and in service |
| `MAINTENANCE` | Out of service for maintenance/reimage |

### Conditions
Boolean signals tracked on the machine status:
- `Reachable` - Machine is reachable via SSH
- `Provisioned` - Machine has been successfully imaged
- `NeedsIntervention` - Requires operator attention
- `OperationCanceled` - Last operation was canceled

### Key Design Decisions

1. **External API = MachineService + OperationService only**
   - No PlanService exposed to customers
   - Plans are internal implementation details

2. **Machine.phase limited to 3 values**
   - FACTORY_READY, READY, MAINTENANCE only
   - No PROVISIONING, IN_SERVICE, RMA, RETIRED exposed to customers

3. **Streaming via WatchOperations + StreamOperationLogs**
   - Real-time status updates
   - Log streaming for debugging

4. **Reimage requires MAINTENANCE**
   - Safety gate to prevent accidental reimages
   - Must explicitly enter maintenance first

## Operation Types

| Type | Description | Required Phase |
|------|-------------|----------------|
| `REBOOT` | Simple reboot | READY or MAINTENANCE |
| `REIMAGE` | Full reimage/repave | MAINTENANCE |
| `ENTER_MAINTENANCE` | Enter maintenance mode | Any |
| `EXIT_MAINTENANCE` | Return to READY | MAINTENANCE |

## Operation Semantics

- **Idempotency:** `request_id` required; scoped to `(machine_id, request_id)`
- **Single active operation:** One operation per machine at a time
- **Replay:** Same `request_id` returns existing operation (no duplicate work)
- **Terminal states:** SUCCEEDED, FAILED, CANCELED are immutable

## Architecture

```
cmd/bmdemo-server         gRPC server (MachineService + OperationService)
cmd/bmdemo-cli            CLI with streaming output

internal/bmdemo/
├── store/                Thread-safe in-memory store (idempotent ops)
├── lifecycle/            Phase/condition helpers
├── plans/                Internal plan registry (not exposed to customers)
├── provider/fake/        Simulated provider with configurable timing
└── executor/             Async execution with retries and streaming
```

### Provider Interface

```go
type Provider interface {
    SetNetboot(ctx, operationID, machine, profile) error
    Reboot(ctx, operationID, machine, force) error
    Repave(ctx, operationID, machine, imageRef, cloudInitRef) error
    MintJoinMaterial(ctx, operationID, targetCluster) (*JoinMaterial, error)
    JoinNode(ctx, operationID, machine, material) error
    VerifyInCluster(ctx, operationID, machine, targetCluster) error
    RMA(ctx, operationID, machine, reason) error
    ExecuteSSHCommand(ctx, operationID, machine, scriptRef, args) error
}
```

## gRPC API

### MachineService (Customer-facing)
```protobuf
service MachineService {
  // Machine CRUD
  rpc RegisterMachine(RegisterMachineRequest) returns (Machine);
  rpc GetMachine(GetMachineRequest) returns (Machine);
  rpc ListMachines(ListMachinesRequest) returns (ListMachinesResponse);
  rpc UpdateMachine(UpdateMachineRequest) returns (Machine);
  
  // Lifecycle Operations
  rpc RebootMachine(RebootMachineRequest) returns (Operation);
  rpc ReimageMachine(ReimageMachineRequest) returns (Operation);
  rpc EnterMaintenance(EnterMaintenanceRequest) returns (Operation);
  rpc ExitMaintenance(ExitMaintenanceRequest) returns (Operation);
  rpc CancelOperation(CancelOperationRequest) returns (Operation);
}
```

### OperationService (Customer-facing)
```protobuf
service OperationService {
  rpc GetOperation(GetOperationRequest) returns (Operation);
  rpc ListOperations(ListOperationsRequest) returns (ListOperationsResponse);
  rpc WatchOperations(WatchOperationsRequest) returns (stream OperationEvent);
  rpc StreamOperationLogs(StreamOperationLogsRequest) returns (stream LogChunk);
}
```

## Server Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--port` | 50051 | gRPC port |
| `--slow` | false | Slower timings for demos |
| `--log-level` | info | debug/info/warn/error |

## Example Session

```
$ go run ./cmd/bmdemo-cli import 3
machine-1   FACTORY_READY
machine-2   FACTORY_READY
machine-3   FACTORY_READY

$ go run ./cmd/bmdemo-cli enter-maintenance machine-1
Operation: abc12345...
→ entering maintenance...
✓ SUCCEEDED

$ go run ./cmd/bmdemo-cli reimage machine-1
Operation: def67890...
→ set-netboot...      ✓
→ reboot-to-netboot... ✓
→ repave-image...     ✓
→ join-cluster...     ✓
→ verify-in-cluster... ✓
✓ SUCCEEDED

$ go run ./cmd/bmdemo-cli exit-maintenance machine-1
Operation: ghi11111...
✓ SUCCEEDED

$ go run ./cmd/bmdemo-cli list
MACHINE_ID   PHASE   REACHABLE  PROVISIONED  NEEDS_HELP  ACTIVE_OP
machine-1    READY   ✓          ✓            -           -
machine-2    FACTORY_READY  -  -            -           -
machine-3    FACTORY_READY  -  -            -           -
```

See [bmdemo-presenter-guide.md](bmdemo-presenter-guide.md) for demo talking points.
