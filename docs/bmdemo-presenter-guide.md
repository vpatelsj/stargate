# Baremetal Provisioning Demo - Presenter Guide

## Overview

This guide provides talking points for presenting the baremetal provisioning demo to your team. The demo showcases a gRPC-based system for managing the lifecycle of physical machines in a datacenter.

**Duration:** 10-15 minutes  
**Prerequisites:** Go 1.23+, terminal with color support

---

## Running the Demo

```bash
# Interactive mode (pauses between steps)
./scripts/run-bmdemo.sh

# Slow mode for better visibility
./scripts/run-bmdemo.sh --slow
```

---

## Talking Points by Section

### Opening (30 seconds)

> "Today I'm going to show you our baremetal provisioning system. This is a gRPC-based API that manages the complete lifecycle of physical machines - from initial registration through reprovisioning, cluster joining, and eventually RMA when hardware fails."
>
> "The system is designed to be idempotent, observable, and extensible. Let me walk you through it."

---

### Step 1: Build and Start Server

**What to say:**

> "First, let's start the server. The system is built in Go with protobuf definitions for type safety. The server exposes three main services:
>
> - **MachineService** for registering and tracking physical machines
> - **PlanService** for defining multi-step provisioning workflows
> - **RunService** for executing those workflows with full observability"

**Key points:**
- gRPC provides type-safe APIs and efficient serialization
- Server is stateless (state lives in the store, which could be backed by a database)
- The `--slow` flag shows more realistic timings

---

### Step 2: View Available Plans

**What to say:**

> "Before we provision anything, let's look at the built-in plans. A plan is a sequence of steps - things like setting netboot, rebooting, imaging, joining a Kubernetes cluster, and verifying the node is healthy."

**Key points:**
- Plans are declarative - define WHAT to do, not HOW
- Each step has configurable timeouts and retry policies
- Plans can be extended or customized per environment
- Current plans: `repave-join`, `rma`, `reboot`, `upgrade`, `net-reconfig`

---

### Step 3: Import Machines

**What to say:**

> "Now let's import some machines. In production, this would come from a CMDB or inventory system. Here we're simulating a datacenter with three machines, each with BMC endpoints and SSH access."

**Key points:**
- Each machine has a unique ID, SSH endpoint, and target cluster assignment
- The machine spec includes BMC credentials for out-of-band management
- Target cluster reference tells us where the node should join

---

### Step 4: View Machine Inventory

**What to say:**

> "Here's our inventory. Each machine has a phase that tracks its lifecycle state. They all start as AVAILABLE, meaning they're registered but not yet provisioned."

**Machine phases:**
| Phase | Meaning |
|-------|---------|
| FACTORY_READY | Just imported, never provisioned |
| PROVISIONING | Currently being set up |
| READY | OS installed, not yet in cluster |
| IN_SERVICE | Actively serving workloads |
| MAINTENANCE | Temporarily out of service (or canceled) |
| RMA | Awaiting hardware replacement |

---

### Step 5: Repave a Machine

**What to say:**

> "Let's repave machine-1. This executes the full provisioning workflow: set the netboot profile, reboot to PXE, install a fresh OS image, join the Kubernetes cluster, and verify the node is healthy."
>
> "Watch the logs stream in real-time - each step shows what's happening on the machine."

**Key points to highlight:**
- Each step has a name and execution status
- Failed steps are retried according to the plan's retry policy
- The machine phase transitions: AVAILABLE → PROVISIONING → READY → IN_SERVICE
- Conditions track specific state (Provisioned, InCustomerCluster)

---

### Step 6: Idempotency Demonstration

**What to say:**

> "One critical feature is idempotency. If a client retries a request - maybe due to a network timeout - we need to return the same result, not create a duplicate run."
>
> "Watch: I'm going to submit the same request twice with the same request_id. The second call returns the existing run instead of creating a new one."

**Key points:**
- Request ID is scoped to (machine_id, request_id) tuple
- Safe to retry any operation without side effects
- Essential for reliable distributed systems

---

### Step 7: Reboot Operation

**What to say:**

> "Not every operation needs the full repave workflow. Sometimes you just need a simple reboot. The reboot plan is a single step that triggers a graceful restart."

**Key points:**
- Different operations use different plans
- Minimal plans for minimal operations
- Same execution engine, same observability

---

### Step 8: Cancellation Handling

**What to say:**

> "Sometimes we need to cancel an operation in progress - maybe the wrong machine was selected, or priorities changed. Let's see how cancellation works."
>
> "When we cancel a run, it's marked CANCELED immediately, and the machine moves to MAINTENANCE with a NeedsIntervention condition. This signals that someone should investigate before starting another operation."

**Key points:**
- Cancel is idempotent - safe to call multiple times
- Machine moves to MAINTENANCE, not left in PROVISIONING
- NeedsIntervention condition is set for alerting
- Active run ID is cleared so new runs can start after intervention

---

### Step 9: RMA Flow

**What to say:**

> "Finally, let's look at the RMA flow. When hardware fails, we need to safely drain the node and mark it for replacement."
>
> "This plan drains workloads, shuts down gracefully, and marks the machine as awaiting RMA. The machine moves to the RMA phase where it awaits physical replacement."

**Key points:**
- RMA is a terminal state until hardware is physically replaced
- The audit trail shows exactly when and why the machine was RMA'd
- Integrates with ticketing systems in production

---

### Step 10: Run History

**What to say:**

> "Every operation is recorded with full audit trail. We can see all runs - who initiated them, when they started and finished, which plan was executed, and what the outcome was."

**Key points:**
- Complete audit trail for compliance
- Each run links to machine and plan
- Timestamps for SLA tracking

---

### Closing (1 minute)

**What to say:**

> "To summarize what we've built:
>
> 1. **Type-safe gRPC API** with streaming support for real-time logs
> 2. **Declarative plans** that define multi-step workflows
> 3. **Idempotent operations** for safe retries
> 4. **Complete observability** with conditions, events, and logs
> 5. **Extensible architecture** - the Provider interface lets us swap implementations
>
> The fake provider we used today simulates real hardware operations. In production, this would call BMC APIs, run Ansible playbooks, or interact with Kubernetes APIs."

---

## Common Questions

**Q: How does this integrate with existing CMDB/inventory?**
> The MachineService accepts machine registrations from any source. We'd add an import job that syncs from your CMDB.

**Q: What happens if a step fails after all retries?**
> The run moves to FAILED state, the machine goes to MAINTENANCE, and the NeedsIntervention condition is set. This triggers alerts for manual investigation.

**Q: Can we run operations on multiple machines in parallel?**
> Yes, each run is independent. You could submit 100 repave operations and they'd all execute concurrently.

**Q: How do we add new step types?**
> Add a new message to the proto, regenerate, implement the handler in the executor, and add the provider method. About 30 minutes of work.

**Q: Is the in-memory store a problem?**
> For production, we'd back the store with PostgreSQL or etcd. The store interface is already abstracted for this.

---

## Demo Tips

1. **Use slow mode** (`--slow`) for live demos - gives people time to read
2. **Resize your terminal** to at least 120 columns for better formatting
3. **Increase font size** so remote viewers can read
4. **Have grpcurl ready** to show raw API calls if someone asks
5. **Kill the server cleanly** with Ctrl+C to show graceful shutdown

---

## Quick Reference Commands

```bash
# Start server
go run ./cmd/bmdemo-server

# CLI commands
go run ./cmd/bmdemo-cli list              # List machines
go run ./cmd/bmdemo-cli plans             # List plans
go run ./cmd/bmdemo-cli runs              # List runs
go run ./cmd/bmdemo-cli import 5          # Import 5 machines
go run ./cmd/bmdemo-cli repave <id>       # Repave machine
go run ./cmd/bmdemo-cli reboot <id>       # Reboot machine
go run ./cmd/bmdemo-cli rma <id>          # RMA machine
go run ./cmd/bmdemo-cli cancel <run-id>   # Cancel a run in progress
go run ./cmd/bmdemo-cli watch             # Watch events (streaming)
go run ./cmd/bmdemo-cli logs <run-id>     # Stream run logs
```
