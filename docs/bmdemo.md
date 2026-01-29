# Baremetal gRPC Demo

A standalone gRPC service demonstrating baremetal machine lifecycle management with hybrid state tracking.

## Quick Start

```bash
# Terminal 1: Start server
go run ./cmd/bmdemo-server

# Terminal 2: Run commands
go run ./cmd/bmdemo-cli import 3
go run ./cmd/bmdemo-cli list
go run ./cmd/bmdemo-cli repave machine-1
go run ./cmd/bmdemo-cli rma machine-1
```

Or run the scripted demo:
```bash
go run ./cmd/bmdemo-cli demo
```

## Build

```bash
make proto           # Generate protobuf (requires buf)
make bmdemo          # Build binaries to bin/
```

## Commands

| Command | Description |
|---------|-------------|
| `import <N>` | Register N fake machines (FACTORY_READY, provider=fake) |
| `list` | List machines with phase, effective state, conditions |
| `repave <id>` | Execute plan/repave-join and watch progress |
| `rma <id>` | Execute plan/rma and watch progress |
| `reboot <id>` | Execute plan/reboot and watch progress |
| `runs` | List all runs |
| `plans` | List available plans |
| `demo` | Run scripted demo (for design docs) |

## Hybrid Lifecycle Model

Machines have a **hybrid state** combining three concepts:

### 1. Explicit Phase
Operator-set state stored in `status.phase`:
```
FACTORY_READY → READY → IN_SERVICE → MAINTENANCE → RMA → RETIRED
```

### 2. Conditions
Boolean signals tracking runtime state:
| Condition | Meaning |
|-----------|---------|
| `Reachable` | Machine responds to health probes |
| `InCustomerCluster` | Successfully joined target cluster |
| `NeedsIntervention` | Requires manual action |
| `Degraded` | Operating with reduced capacity |

### 3. Effective State
Computed view applying precedence rules:

```
1. Active run (PENDING/RUNNING)  → PROVISIONING
2. Explicit RMA/RETIRED/MAINTENANCE → that phase
3. InCustomerCluster=true        → IN_SERVICE
4. Otherwise                     → explicit phase
```

**Example:**
```
MACHINE      PHASE          EFFECTIVE      CONDITIONS
machine-1    READY          IN_SERVICE     InCustomerCluster=✓
machine-2    MAINTENANCE    MAINTENANCE    InCustomerCluster=✓, Degraded=✓
machine-3    FACTORY_READY  PROVISIONING   (active run)
```

## Plans

Built-in plans in `internal/bmdemo/plans`:

| Plan | Steps |
|------|-------|
| `plan/repave-join` | set-netboot → repave → mint-join → join → verify |
| `plan/rma` | drain → wipe → notify |
| `plan/reboot` | reboot |
| `plan/upgrade` | drain → set-netboot → repave → mint-join → join → verify |
| `plan/net-reconfig` | apply-net-config → verify-connectivity |

## Architecture

```
cmd/bmdemo-server          gRPC server wiring all components
cmd/bmdemo-cli             CLI with grpcurl-like UX

internal/bmdemo/
├── store/                 Thread-safe in-memory store (idempotent ops)
├── lifecycle/             Phase/condition helpers, EffectiveState()
├── plans/                 Plan registry with built-in plans
├── provider/fake/         Simulated provider with configurable timing
└── executor/              Async run execution with retries, streaming
```

## Proto

Services defined in `proto/baremetal/v1/baremetal.proto`:

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

## Example Session

```bash
$ go run ./cmd/bmdemo-cli import 3
Importing 3 fake machines...
MACHINE_ID    MAC                 PHASE
machine-1     02:00:00:00:00:01   FACTORY_READY
machine-2     02:00:00:00:00:02   FACTORY_READY
machine-3     02:00:00:00:00:03   FACTORY_READY

$ go run ./cmd/bmdemo-cli list
MACHINE_ID    PHASE          EFFECTIVE      REACHABLE  IN_CLUSTER  ACTIVE_RUN
machine-1     FACTORY_READY  FACTORY_READY  -          -           -
machine-2     FACTORY_READY  FACTORY_READY  -          -           -
machine-3     FACTORY_READY  FACTORY_READY  -          -           -

$ go run ./cmd/bmdemo-cli repave machine-1
┌─────────────────────────────────────────────────────────────
│ Run: abc12345-...
│ Machine: machine-1
│ Type: REPAVE | Plan: plan/repave-join
└─────────────────────────────────────────────────────────────

→ set-netboot...
  ✓ set-netboot
→ repave...
  ✓ repave
→ mint-join...
  ✓ mint-join
→ join...
  ✓ join
→ verify...
  ✓ verify

┌─────────────────────────────────────────────────────────────
│ ✓ SUCCEEDED
│ Duration: 2.5s
│ Steps: 5/5 completed
└─────────────────────────────────────────────────────────────

$ go run ./cmd/bmdemo-cli list
MACHINE_ID    PHASE   EFFECTIVE   REACHABLE  IN_CLUSTER  ACTIVE_RUN
machine-1     READY   IN_SERVICE  ✓          ✓           -
machine-2     FACTORY_READY  ...
machine-3     FACTORY_READY  ...
```

## Server Flags

```
--port        gRPC port (default: 50051)
--slow        Use slow timing for demos
--log-level   debug|info|warn|error (default: info)
```

## CLI Flags

```
--server      Server address (default: localhost:50051)
```
