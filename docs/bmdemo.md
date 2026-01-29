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

Or run the interactive demo script:
```bash
./scripts/run-bmdemo.sh          # Normal speed
./scripts/run-bmdemo.sh --slow   # Slower timings for live demos
```

Or run the scripted demo for design docs:
```bash
go run ./cmd/bmdemo-cli demo
```

## Build

```bash
make bmdemo          # Build binaries to bin/
make proto           # Regenerate protobuf (requires protoc + plugins)
```

## Commands

| Command | Description |
|---------|-------------|
| `import <N>` | Register N machines with predictable IDs (machine-1, machine-2, ...) |
| `list` | List machines with phase, effective state, conditions |
| `repave <id>` | Execute plan/repave-join and watch progress |
| `rma <id>` | Execute plan/rma and watch progress |
| `reboot <id>` | Execute plan/reboot and watch progress |
| `runs` | List all runs |
| `plans` | List available plans |
| `watch [machine-id]` | Watch run events (streaming) |
| `logs <run-id>` | Stream run logs |
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
| `Provisioned` | Machine has been successfully provisioned |
| `Healthy` | Machine is operating normally |

### 3. Effective State
Computed view applying precedence rules (from `lifecycle.EffectiveState()`):

```
1. Active run (PENDING/RUNNING)     → PROVISIONING
2. Explicit RMA                     → RMA
3. Explicit RETIRED                 → RETIRED
4. Explicit MAINTENANCE             → MAINTENANCE
5. InCustomerCluster condition=true → IN_SERVICE
6. FACTORY_READY                    → FACTORY_READY
7. Otherwise                        → READY
```

**Example:**
```
MACHINE      PHASE          EFFECTIVE      CONDITIONS
machine-1    READY          IN_SERVICE     InCustomerCluster=✓
machine-2    MAINTENANCE    MAINTENANCE    InCustomerCluster=✓, NeedsIntervention=✓
machine-3    FACTORY_READY  PROVISIONING   (active run)
```

## Plans

Built-in plans in `internal/bmdemo/plans`:

| Plan | Steps |
|------|-------|
| `plan/repave-join` | set-netboot → reboot-to-netboot → repave-image → join-cluster → verify-in-cluster |
| `plan/rma` | drain-check → graceful-shutdown → mark-rma |
| `plan/reboot` | reboot |
| `plan/upgrade` | cordon-node → drain-node → upgrade-kubelet → restart-kubelet → uncordon-node → verify-upgrade |
| `plan/net-reconfig` | apply-network-config → verify-connectivity |

## Step Types

Steps are defined in proto with specific action kinds:

| Step Kind | Description |
|-----------|-------------|
| `Step_Ssh` | Execute SSH command/script on machine |
| `Step_Reboot` | Reboot machine (graceful or forced) |
| `Step_Netboot` | Set netboot/PXE profile |
| `Step_Repave` | Reprovision with new OS image |
| `Step_Join` | Join machine to Kubernetes cluster |
| `Step_Verify` | Verify machine is healthy in cluster |
| `Step_Net` | Apply network reconfiguration |
| `Step_Rma` | Mark machine for RMA (hardware return) |

## Run Semantics

A **Run** has both:
- `type` - operation category (REPAVE, RMA, REBOOT, UPGRADE, NET_RECONFIG)
- `plan_id` - specific plan executed (e.g., `plan/repave-join`)

**Plan selection precedence:**
1. If `plan_id` is provided, use that plan
2. Otherwise, map `type` to default plan (e.g., REPAVE → plan/repave-join)

**Idempotency:** 
- StartRun requires `request_id` for idempotency
- Idempotency is scoped to (machine_id, request_id) tuple
- Same request_id can be reused across different machines
- Replaying the same request returns the existing run without creating a duplicate

**Retry behavior:**
- If a run is still PENDING when retried, execution is re-attempted
- Terminal states (SUCCEEDED, FAILED, CANCELED) are immutable

**Single active run:** Only one run can be active per machine at a time.

## Architecture

```
cmd/bmdemo-server          gRPC server wiring all components
cmd/bmdemo-cli             CLI with real-time streaming output

internal/bmdemo/
├── store/                 Thread-safe in-memory store
│                          - Idempotent operations
│                          - Machine-scoped request deduplication
├── lifecycle/             Phase/condition helpers, EffectiveState()
├── plans/                 Plan registry with built-in plans
├── provider/              Provider interface for extensibility
│   └── fake/              Simulated provider with configurable timing
└── executor/              Async run execution
                           - Exponential backoff retries
                           - Real-time event/log streaming
                           - Context-based cancellation

scripts/
└── run-bmdemo.sh          Interactive demo script with presenter pauses
```

### Provider Interface

The executor uses a `provider.Provider` interface, making it easy to swap implementations:

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
MACHINE_ID  MAC                PHASE
machine-1   02:00:00:00:00:00  FACTORY_READY
machine-2   02:00:00:00:00:01  FACTORY_READY
machine-3   02:00:00:00:00:02  FACTORY_READY

Imported 3 machines.

$ go run ./cmd/bmdemo-cli list
MACHINE_ID  PHASE          EFFECTIVE      REACHABLE  IN_CLUSTER  NEEDS_HELP  ACTIVE_RUN
machine-1   FACTORY_READY  FACTORY_READY  -          -           -           -
machine-2   FACTORY_READY  FACTORY_READY  -          -           -           -
machine-3   FACTORY_READY  FACTORY_READY  -          -           -           -

$ go run ./cmd/bmdemo-cli repave machine-1
┌─────────────────────────────────────────────────────────────
│ Run: run-1234567890...
│ Machine: machine-1
│ Type: REPAVE | Plan: plan/repave-join
│ Request ID: abc12345-...
└─────────────────────────────────────────────────────────────

  │ [netboot] Setting netboot profile: pxe-ubuntu-22.04
  [12:00:01] Step set-netboot succeeded
→ reboot-to-netboot...
  │ [reboot] Initiating graceful reboot
  ✓ reboot-to-netboot
→ repave-image...
  │ [repave] Downloading image...
  │ [repave] Writing image to disk...
  ✓ repave-image
→ join-cluster...
  │ [join] Joining node to cluster: default-cluster
  ✓ join-cluster
→ verify-in-cluster...
  │ [verify] Node status: Ready
  [12:00:05] Run succeeded

┌─────────────────────────────────────────────────────────────
│ ✓ SUCCEEDED
│ Duration: 3.5s
│ Steps: 5/5 completed
└─────────────────────────────────────────────────────────────

$ go run ./cmd/bmdemo-cli list
MACHINE_ID  PHASE       EFFECTIVE   REACHABLE  IN_CLUSTER  NEEDS_HELP  ACTIVE_RUN
machine-1   IN_SERVICE  IN_SERVICE  -          ✓           -           -
machine-2   FACTORY_READY  ...
machine-3   FACTORY_READY  ...

$ go run ./cmd/bmdemo-cli rma machine-1
┌─────────────────────────────────────────────────────────────
│ Run: run-9876543210...
│ Machine: machine-1
│ Type: RMA | Plan: plan/rma
└─────────────────────────────────────────────────────────────

→ drain-check...
  ✓ drain-check
→ graceful-shutdown...
  ✓ graceful-shutdown
→ mark-rma...
  │ [rma] Machine marked for RMA
  │ [rma] Ticket created: RMA-12345
  ✓ mark-rma

┌─────────────────────────────────────────────────────────────
│ ✓ SUCCEEDED
│ Duration: 1.2s
│ Steps: 3/3 completed
└─────────────────────────────────────────────────────────────

$ go run ./cmd/bmdemo-cli list
MACHINE_ID  PHASE  EFFECTIVE  REACHABLE  IN_CLUSTER  NEEDS_HELP  ACTIVE_RUN
machine-1   RMA    RMA        -          -           -           -
...
```

## Server Flags

```
--port        gRPC port (default: 50051)
--slow        Use slow timing for demos (more realistic durations)
--log-level   debug|info|warn|error (default: info)
```

## CLI Flags

```
--server      Server address (default: localhost:50051)
```

## Demo Script

For live demos, use the interactive script:

```bash
./scripts/run-bmdemo.sh          # Normal speed
./scripts/run-bmdemo.sh --slow   # Slower for visibility
```

The script:
- Builds binaries and starts the server
- Walks through each feature with pauses for explanation
- Demonstrates import, repave, idempotency, reboot, and RMA
- Shows machine lifecycle transitions
- Cleans up on exit

See [docs/bmdemo-presenter-guide.md](bmdemo-presenter-guide.md) for talking points.
