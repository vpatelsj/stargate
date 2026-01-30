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

> "This is a gRPC-based baremetal provisioning system. It manages physical machines from registration through reprovisioning, cluster joining, and RMA. The system is idempotent, observable, and extensible."
>
> "Today we're using a fake provider that simulates hardware operations. In production, this would call real BMC APIs and SSH to machines. The provider streams logs in real-time so you can see exactly what's happening."

### Step 1: Start Server

> "The server exposes three services: **MachineService** for inventory, **PlanService** for workflow definitions, and **RunService** for execution with full observability."

### Step 2: View Plans

> "Plans are declarative step sequences. In the demo we have plan/repave-join, plan/rma, plan/reboot, plan/upgrade, and plan/net-reconfig, each with explicit step order, timeouts, and retry policies."

### Step 3: Import Machines

> "Importing machines from inventory. Each has a unique ID, provider=fake, a MAC address, and an SSH endpoint. They start in FACTORY_READY."

### Step 4: Machine Phases

| Phase | Meaning |
|-------|---------|
| FACTORY_READY | Imported, never provisioned |
| PROVISIONING | A run is actively modifying it |
| READY | Managed by us but not in service |
| IN_SERVICE | Serving workloads |
| MAINTENANCE | Disruptive ops allowed |
| RMA | Marked for hardware replacement |
| RETIRED | Removed from inventory |

### Effective State (what the UI shows)

The `list` command shows both `PHASE` and `EFFECTIVE` columns. Effective State is computed on-the-fly with these precedence rules:

```
1. Active run (PENDING/RUNNING)     → PROVISIONING
2. Explicit RMA/RETIRED/MAINTENANCE → that phase
3. InCustomerCluster condition=true → IN_SERVICE
4. FACTORY_READY                    → FACTORY_READY
5. Otherwise                        → READY
```

**Why?** This avoids "state explosion" - the explicit phase stays minimal (7 values) while the UI shows a truthful combined view. Overlays like `NeedsIntervention` trigger banners without adding phases.

**Example:**
```
MACHINE      PHASE          EFFECTIVE      CONDITIONS
machine-1    READY          IN_SERVICE     InCustomerCluster=✓
machine-2    MAINTENANCE    MAINTENANCE    NeedsIntervention=✓
machine-3    FACTORY_READY  PROVISIONING   (has active run)
```

### Step 5: Repave

> "Repave executes the full provisioning workflow. Watch the logs and events stream in real-time - you're seeing two gRPC streams interleaved."

**The 5-step plan/repave-join flow:**
1. **set-netboot** - Configure PXE/iPXE for network boot
2. **reboot-to-netboot** - BMC reboot, machine boots from network
3. **repave-image** - Download image, write to disk, configure bootloader
4. **join-cluster** - Mint join token, run kubeadm join
5. **verify-in-cluster** - Confirm node is Ready in K8s

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
| `│ [repave] Writing image...` | Provider logs (via `StreamRunLogs`) |
| `[15:26:21] Step X succeeded` | Executor events (via `WatchRuns`) |

**Production swap:** Replace `fake.Provider` with a real implementation that calls BMC APIs, runs Ansible, or SSHs to machines. The interface stays the same.

---

**Reading the output:**
| Symbol | Meaning |
|--------|---------|
| `→ step-name...` | Step started |
| `✓ step-name` | Step succeeded |
| `│ [tag] message` | Real-time log from provider (`StreamRunLogs`) |
| `[HH:MM:SS] Step X started/succeeded` | Event from `WatchRuns` |

**Why interleaved?** The CLI subscribes to two gRPC streams simultaneously:
- `WatchRuns` → step phase changes
- `StreamRunLogs` → provider log output

These arrive asynchronously, so logs may appear before/after the checkmark.

**Result:** Machine transitions `FACTORY_READY → PROVISIONING → IN_SERVICE`, conditions `Provisioned` + `InCustomerCluster` become true.

### Step 6: Idempotency

> "Idempotency is critical for distributed systems. If a client times out and retries, we don't want to create a duplicate run."

**How it works:**
- `request_id` is scoped to `(machine_id, request_id)` tuple
- Same request_id for same machine → returns existing run immediately
- No duplicate execution, no side effects

**What to look for in the demo:**
```
First request:  request_id=demo-idempotent-123 → run-ABC created, executes 5 steps
Second request: request_id=demo-idempotent-123 → run-ABC returned immediately (SUCCEEDED)
                                                  ↑ No new execution!
```

**Expected output for second call:**
```
┌─────────────────────────────────────────────────────────────
│ Run: run-ABC              ← SAME run ID as first call
│ Machine: machine-2
│ Request ID: demo-idempotent-123
└─────────────────────────────────────────────────────────────

┌─────────────────────────────────────────────────────────────
│ ✓ SUCCEEDED               ← Already completed, no steps run
│ Duration: 0.001s
└─────────────────────────────────────────────────────────────
```

**Why this matters:** Network timeouts, load balancer retries, and client crashes are common. Idempotency means safe retries without corrupting state.

### Step 7: Reboot

> "Simple operations use simple plans. Reboot is a single step - same execution engine, same observability."

### Step 8: Cancellation

> "Canceling sets the run to CANCELED and the machine to MAINTENANCE with an OperationCanceled condition (not NeedsIntervention). Cancellations are treated as expected operator actions."

### Step 9: RMA

> "RMA flow runs drain-check (SSH), graceful-shutdown (reboot), then mark-rma. On success the machine phase becomes RMA."

### Step 10: Run History

> "Every operation recorded with an audit trail: run id, machine id, plan, phase, timestamps, and step outcomes."

### Closing

> "Summary: Type-safe gRPC API with streaming, declarative plans, idempotent operations, complete observability, extensible Provider interface. The fake provider simulates hardware - in production this calls BMC APIs, Ansible, or Kubernetes APIs."

---

## FAQ

**Q: How does it integrate with CMDB?**  
A: MachineService accepts registrations from any source via import jobs.

**Q: What happens if a step fails after all retries?**  
A: Run → FAILED, machine → MAINTENANCE, NeedsIntervention condition set.

**Q: Can we run operations in parallel?**  
A: Yes, across machines. Only one active run per machine is allowed.

**Q: How do we add new step types?**  
A: Add a proto message, regenerate, implement executor handling + provider method.

**Q: Can clients update machine status directly?**  
A: No. UpdateMachine only accepts Spec and Labels; status is owned by the backend/executor.

**Q: In-memory store a problem?**  
A: For production, back with PostgreSQL or etcd. The store interface is abstracted.

---

## Tips

- Use `--slow` for live demos
- Terminal: 120+ columns, large font
- Have grpcurl ready for raw API questions
- Clean Ctrl+C shows graceful shutdown

## Quick Reference

```bash
go run ./cmd/bmdemo-cli list
go run ./cmd/bmdemo-cli plans
go run ./cmd/bmdemo-cli runs
go run ./cmd/bmdemo-cli import 5
go run ./cmd/bmdemo-cli repave <id>
go run ./cmd/bmdemo-cli reboot <id>
go run ./cmd/bmdemo-cli rma <id>
go run ./cmd/bmdemo-cli cancel <run-id>
go run ./cmd/bmdemo-cli watch
go run ./cmd/bmdemo-cli logs <run-id>
```
