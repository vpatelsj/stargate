# Baremetal gRPC Demo

A gRPC service for baremetal machine lifecycle management with hybrid state tracking.

## Quick Start

```bash
# Terminal 1: Server
go run ./cmd/bmdemo-server

# Terminal 2: CLI
go run ./cmd/bmdemo-cli import 3
go run ./cmd/bmdemo-cli repave machine-1
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
| `list` | Show machines with phase, effective state, conditions |
| `repave <id>` | Repave and join cluster |
| `rma <id>` | Return merchandise authorization flow |
| `reboot <id>` | Simple reboot |
| `runs` | List all runs |
| `plans` | List available plans |
| `watch [machine-id]` | Stream run events |
| `logs <run-id>` | Stream run logs |
| `cancel <run-id>` | Cancel active run |

## Hybrid Lifecycle Model

Machines have a **hybrid state** from three sources:

### Explicit Phase
Operator-set state: `FACTORY_READY → READY → IN_SERVICE → MAINTENANCE → RMA → RETIRED`

### Conditions
Boolean signals: `Reachable`, `InCustomerCluster`, `NeedsIntervention`, `Provisioned`, `Healthy`

### Effective State
Computed view with precedence rules:
1. Active run → `PROVISIONING`
2. Explicit `RMA/RETIRED/MAINTENANCE` → that phase
3. `InCustomerCluster=true` → `IN_SERVICE`
4. `FACTORY_READY` → `FACTORY_READY`
5. Otherwise → `READY`

## Plans

| Plan | Steps |
|------|-------|
| `plan/repave-join` | set-netboot → reboot → repave-image → join-cluster → verify |
| `plan/rma` | drain-check → graceful-shutdown → mark-rma |
| `plan/reboot` | reboot |
| `plan/upgrade` | cordon → drain → upgrade → restart → uncordon → verify |
| `plan/net-reconfig` | apply-network-config → verify-connectivity |

## Run Semantics

- **Idempotency:** `request_id` required; scoped to `(machine_id, request_id)`
- **Single active run:** One run per machine at a time
- **Retry:** PENDING runs re-execute on replay; terminal states are immutable

## Architecture

```
cmd/bmdemo-server         gRPC server
cmd/bmdemo-cli            CLI with streaming output

internal/bmdemo/
├── store/                Thread-safe in-memory store (idempotent ops)
├── lifecycle/            Phase/condition helpers, EffectiveState()
├── plans/                Plan registry
├── provider/fake/        Simulated provider with configurable timing
└── executor/             Async execution with retries and streaming
```

### Provider Interface

```go
type Provider interface {
    SetNetboot(ctx, runID, machine, profile) error
    Reboot(ctx, runID, machine, force) error
    Repave(ctx, runID, machine, imageRef, cloudInitRef) error
    MintJoinMaterial(ctx, runID, targetCluster) (*JoinMaterial, error)
    JoinNode(ctx, runID, machine, material) error
    VerifyInCluster(ctx, runID, machine, targetCluster) error
    RMA(ctx, runID, machine, reason) error
    ExecuteSSHCommand(ctx, runID, machine, scriptRef, args) error
}
```

## gRPC API

```protobuf
service MachineService {
  rpc RegisterMachine(RegisterMachineRequest) returns (Machine);
  rpc GetMachine(GetMachineRequest) returns (Machine);
  rpc ListMachines(ListMachinesRequest) returns (ListMachinesResponse);
  rpc UpdateMachine(UpdateMachineRequest) returns (Machine);
}

service PlanService {
  rpc GetPlan(GetPlanRequest) returns (Plan);
  rpc ListPlans(ListPlansRequest) returns (ListPlansResponse);
}

service RunService {
  rpc StartRun(StartRunRequest) returns (Run);
  rpc GetRun(GetRunRequest) returns (Run);
  rpc ListRuns(ListRunsRequest) returns (ListRunsResponse);
  rpc CancelRun(CancelRunRequest) returns (Run);
  rpc WatchRuns(WatchRunsRequest) returns (stream RunEvent);
  rpc StreamRunLogs(StreamRunLogsRequest) returns (stream LogChunk);
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

$ go run ./cmd/bmdemo-cli repave machine-1
→ set-netboot...      ✓
→ reboot-to-netboot... ✓
→ repave-image...     ✓
→ join-cluster...     ✓
→ verify-in-cluster... ✓
✓ SUCCEEDED (3.5s)

$ go run ./cmd/bmdemo-cli list
machine-1   IN_SERVICE   InCustomerCluster=✓
machine-2   FACTORY_READY
machine-3   FACTORY_READY

$ go run ./cmd/bmdemo-cli rma machine-1
→ drain-check...       ✓
→ graceful-shutdown... ✓
→ mark-rma...         ✓
✓ SUCCEEDED (1.2s)

$ go run ./cmd/bmdemo-cli list
machine-1   RMA
```

See [bmdemo-presenter-guide.md](bmdemo-presenter-guide.md) for demo talking points.
