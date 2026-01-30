# Baremetal Demo - Presenter Guide

**Duration:** 10-15 minutes

## Running the Demo

```bash
./scripts/run-bmdemo.sh          # Normal speed
./scripts/run-bmdemo.sh --slow   # For live presentations
```

---


## Script with Talking Points

### Opening (30s)

> "This is a gRPC-based baremetal provisioning system. It manages physical machines from registration through reprovisioning and cluster joining. The system is idempotent, observable, and extensible."
>
> "Today we're using a fake provider that simulates hardware operations. In production, this would call real BMC APIs and SSH to machines. The provider streams logs in real-time so you can see exactly what's happening."

### Step 1: Start Server

> "The server exposes two services: **MachineService** for inventory and lifecycle operations (reboot, reimage, etc.), and **OperationService** for read-only observability into execution."

### Step 2: Understanding the Dual State Model

> "The key innovation here is the separation of **Phase** (intent) from **EffectiveState** (observed reality). This prevents state explosion while maintaining truthful visibility."

### Step 3: Import Machines

> "Importing machines from inventory. Each has a unique ID, provider=fake, a MAC address, and an SSH endpoint. They start in FACTORY_READY with EFFECTIVE=NEW."

### Step 4: Machine Phases (Imperative Intent)

| Phase | Meaning |
|-------|---------|
| FACTORY_READY | Imported, never provisioned |
| READY | Active management, workloads possible |
| MAINTENANCE | Disruptive operations allowed |

**Note:** RMA and RETIRED are expressed as *conditions*, not phases. This keeps the phase space minimal.

### Effective State (Observed Reality)

The `list` command shows both `PHASE` and `EFFECTIVE` columns. EffectiveState is computed on-the-fly with these precedence rules:

```
1. Condition Retired=true or RMA=true → BLOCKED
2. Condition NeedsIntervention=true   → ATTENTION
3. Active operation (PENDING/RUNNING) → BUSY
4. Phase=MAINTENANCE                  → MAINTENANCE_IDLE
5. Phase=FACTORY_READY                → NEW
6. Otherwise                          → IDLE
```

**Why?** This avoids "state explosion" - the explicit phase stays minimal (3 values) while the UI shows a truthful combined view. Conditions like `NeedsIntervention` or `RMA` trigger appropriate UI treatment without adding phases.

**Example:**
```
MACHINE      PHASE          EFFECTIVE      CONDITIONS
machine-1    READY          IDLE           InCustomerCluster=✓, Provisioned=✓
machine-2    MAINTENANCE    MAINT          (no active op)
machine-3    MAINTENANCE    BUSY           (has active reimage op)
machine-4    READY          ⚠ATTN          NeedsIntervention=✓
machine-5    READY          ⛔BLOCK        RMA=✓
```

### Step 5: Enter Maintenance & Reimage

> "To reimage a machine, we first enter maintenance mode. This is a safety gate - the API rejects reimage requests on machines in READY phase. Watch the logs and events stream in real-time - you're seeing two gRPC streams interleaved."

**The internal 5-step reimage flow** (not visible in SDK, just logs):
1. **set-netboot** - Configure PXE/iPXE for network boot
2. **reboot-to-netboot** - BMC reboot, machine boots from network
3. **repave-image** - Download image, write to disk, configure bootloader
4. **join-cluster** - Mint join token, run kubeadm join
5. **verify-in-cluster** - Confirm node is Ready in K8s

**Note:** Clients only see `current_stage` field - the full step list is internal.

### Step 6: Exit Maintenance

> "After reimage completes, the machine stays in MAINTENANCE. The operator explicitly calls exit-maintenance to transition to READY. This gives operators control over when machines re-enter production."

## About the Fake Provider

The demo uses a **simulated provider** (`internal/bmdemo/provider/fake`) that implements the full `Provider` interface without touching real hardware.

**What it does:**
- Simulates BMC, SSH, PXE, and Kubernetes operations with realistic timing
- Streams logs via callback → appears as `│ [tag] message` in CLI output
- Supports failure injection for testing error paths
- Respects context cancellation

**Two log streams in demo output:**
| Output | Source |
|--------|--------|
| `│ [repave] Writing image...` | Provider logs (via `StreamLogs`) |
| `[15:26:21] Step X succeeded` | Executor events (via `WatchOperations`) |

**Production swap:** Replace `fake.Provider` with a real implementation that calls BMC APIs, runs Ansible, or SSHs to machines. The interface stays the same.

---

**Reading the output:**
| Symbol | Meaning |
|--------|---------|
| `→ step-name...` | Step started |
| `✓ step-name` | Step succeeded |
| `│ [tag] message` | Real-time log from provider (`StreamLogs`) |
| `[HH:MM:SS] Step X started/succeeded` | Event from `WatchOperations` |

**Why interleaved?** The CLI subscribes to two gRPC streams simultaneously:
- `WatchOperations` → step phase changes
- `StreamLogs` → provider log output

These arrive asynchronously, so logs may appear before/after the checkmark.

**Result:** Machine EFFECTIVE state transitions `NEW → BUSY → IDLE`. Conditions `Provisioned` + `InCustomerCluster` become true.

### Step 7: Idempotency

> "Idempotency is critical for distributed systems. If a client times out and retries, we don't want to create a duplicate operation."

**How it works:**
- `request_id` is scoped to `(machine_id, request_id)` tuple
- Same request_id for same machine → returns existing operation immediately
- No duplicate execution, no side effects

**What to look for in the demo:**
```
First request:  request_id=demo-idempotent-123 → op-ABC created, executes 5 steps
Second request: request_id=demo-idempotent-123 → op-ABC returned immediately (SUCCEEDED)
                                                  ↑ No new execution!
```

**Expected output for second call:**
```
┌─────────────────────────────────────────────────────────────
│ Operation: op-ABC         ← SAME operation ID as first call
│ Machine: machine-2
│ Request ID: demo-idempotent-123
└─────────────────────────────────────────────────────────────

┌─────────────────────────────────────────────────────────────
│ ✓ SUCCEEDED               ← Already completed, no steps run
│ Duration: 0.001s
└─────────────────────────────────────────────────────────────
```

**Why this matters:** Network timeouts, load balancer retries, and client crashes are common. Idempotency means safe retries without corrupting state.

### Step 8: Reboot

> "Simple operations use simple plans. Reboot is a single step - same execution engine, same observability. Reboot works on READY machines without entering maintenance."

### Step 9: Cancellation

> "Canceling sets the operation to CANCELED and sets an OperationCanceled condition (not NeedsIntervention). Cancellations are treated as expected operator actions. Machine stays in its current phase."

### Step 10: Operation History

> "Every operation recorded with an audit trail: operation id, machine id, type, phase, timestamps, and step outcomes."

### Closing

> "Summary: Type-safe gRPC API with streaming, dual state model (Phase + EffectiveState), idempotent operations, complete observability, extensible Provider interface. Plans and steps are internal - clients only see operation type, phase, and current_stage. This keeps the SDK stable while letting us evolve the workflow engine."

---

## FAQ

**Q: How does it integrate with CMDB?**  
A: MachineService accepts registrations from any source via import jobs.

**Q: What happens if a step fails after all retries?**  
A: Operation → FAILED, NeedsIntervention condition set. Machine stays in its current phase. EFFECTIVE becomes ATTENTION.

**Q: Can we run operations in parallel?**  
A: Yes, across machines. Only one active operation per machine is allowed.

**Q: How do we add new step types?**  
A: Plans/steps are internal Go types (not in proto). Add a new `StepKind` in `workflow/types.go`, implement executor handling + provider method. SDK consumers don't see step details.

**Q: Can clients update machine status directly?**  
A: No. UpdateMachine only accepts Spec and Labels; status is owned by the backend/executor.

**Q: In-memory store a problem?**  
A: For production, back with PostgreSQL or etcd. The store interface is abstracted.

**Q: What about RMA/RETIRED? They were phases before.**  
A: Now they're conditions (Retired=true, RMA=true). Effective state becomes BLOCKED. This simplifies the phase model while preserving semantics.

---

## Tips

- Use `--slow` for live demos
- Terminal: 120+ columns, large font
- Have grpcurl ready for raw API questions
- Clean Ctrl+C shows graceful shutdown

## Quick Reference

```bash
go run ./cmd/bmdemo-cli list
go run ./cmd/bmdemo-cli ops
go run ./cmd/bmdemo-cli import 5
go run ./cmd/bmdemo-cli enter-maintenance <id>
go run ./cmd/bmdemo-cli reimage <id>
go run ./cmd/bmdemo-cli exit-maintenance <id>
go run ./cmd/bmdemo-cli reboot <id>
go run ./cmd/bmdemo-cli cancel <op-id>
go run ./cmd/bmdemo-cli watch
go run ./cmd/bmdemo-cli logs <op-id>
```
