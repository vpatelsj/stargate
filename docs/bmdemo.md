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
| `list` | Show machines with phase, effective state, and conditions |
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

Machines use a **dual-state model**: imperative `phase` (intent gate) and observed `effective_state`.

### Phase (Imperative Intent)

The `phase` field represents the operator's **intent** for the machine. It gates which operations are allowed.

| Phase | Description | Allowed Operations |
|-------|-------------|-------------------|
| `FACTORY_READY` | New machine, never provisioned | enter-maintenance |
| `READY` | Provisioned and in service | reboot, enter-maintenance |
| `MAINTENANCE` | Out of service for maintenance | reboot, reimage, exit-maintenance |

Phase changes **only** via explicit API calls:
- `EnterMaintenance` → sets phase to MAINTENANCE
- `ExitMaintenance` → sets phase to READY
- `RegisterMachine` → initial phase (FACTORY_READY or READY)

### Effective State (Observed)

The `effective_state` field is a **read-only derived value** computed server-side. It combines phase, conditions, and active operation status to answer: "What is this machine doing right now?"

| EffectiveState | Meaning |
|----------------|---------|
| `NEW` | phase == FACTORY_READY |
| `IDLE` | phase == READY, no active operation |
| `BUSY` | Active operation PENDING/RUNNING |
| `MAINTENANCE` | phase == MAINTENANCE, no active operation |
| `ATTENTION` | NeedsIntervention condition true |
| `BLOCKED` | RMA or Retired condition true |

#### Precedence Rules (server computes in this order)

1. **BLOCKED** if `Retired=true` OR `RMA=true`
2. **ATTENTION** if `NeedsIntervention=true`
3. **BUSY** if active operation in PENDING/RUNNING
4. **MAINTENANCE** if phase == MAINTENANCE (and no active op)
5. **NEW** if phase == FACTORY_READY
6. **IDLE** otherwise

### Conditions

Boolean signals tracked on the machine status:
- `Reachable` - Machine is reachable via SSH
- `Provisioned` - Machine has been successfully imaged
- `NeedsIntervention` - Requires operator attention
- `OperationCanceled` - Last operation was canceled
- `RMA` - Machine flagged for RMA
- `Retired` - Machine retired from service

### Key Design Decisions

1. **External API = MachineService + OperationService only**
   - Plans/steps are internal implementation details (not in public proto)
   - Operations expose only: `type`, `phase`, `current_stage`, `params`, `error`, timestamps
   - Internal workflow engine can evolve without breaking SDK consumers

2. **Machine.phase limited to 3 values**
   - FACTORY_READY, READY, MAINTENANCE only
   - No PROVISIONING, IN_SERVICE, RMA, RETIRED as phases (these are conditions or effective states)

3. **Phase is imperative intent, not modified by failures**
   - Failures/cancellations do NOT auto-change phase
   - Only EnterMaintenance/ExitMaintenance modify phase
   - Failures set `NeedsIntervention=true`, cancellations set `OperationCanceled=true`

4. **Streaming via WatchOperations + StreamOperationLogs**
   - Real-time status updates
   - Log streaming for debugging

5. **Reimage requires MAINTENANCE**
   - Safety gate to prevent accidental reimages
   - Must explicitly enter maintenance first

6. **Status fields are backend-owned**
   - UpdateMachine ignores any client-supplied phase/effective_state
   - Only Spec and Labels can be updated by clients

7. **Operation.type is an enum**
   - `REBOOT`, `REIMAGE`, `ENTER_MAINTENANCE`, `EXIT_MAINTENANCE`
   - Invalid types are rejected at the API layer

8. **image_ref is passed through as operation param**
   - `ReimageMachine(image_ref="ubuntu-2204")` stores in `Operation.params["image_ref"]`
   - Fake provider logs the image being used

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

$ go run ./cmd/bmdemo-cli list
MACHINE_ID   PHASE    EFFECTIVE  REACHABLE  PROVISIONED  ACTIVE_OP
machine-1    FACTORY  NEW        -          -            -
machine-2    FACTORY  NEW        -          -            -
machine-3    FACTORY  NEW        -          -            -

$ go run ./cmd/bmdemo-cli enter-maintenance machine-1
Operation: abc12345...
→ entering maintenance...
✓ SUCCEEDED

$ go run ./cmd/bmdemo-cli list
MACHINE_ID   PHASE    EFFECTIVE  REACHABLE  PROVISIONED  ACTIVE_OP
machine-1    MAINT    MAINT      -          -            -
machine-2    FACTORY  NEW        -          -            -
machine-3    FACTORY  NEW        -          -            -

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
MACHINE_ID   PHASE    EFFECTIVE  REACHABLE  PROVISIONED  ACTIVE_OP
machine-1    READY    IDLE       ✓          ✓            -
machine-2    FACTORY  NEW        -          -            -
machine-3    FACTORY  NEW        -          -            -
```

See [bmdemo-presenter-guide.md](bmdemo-presenter-guide.md) for demo talking points.
