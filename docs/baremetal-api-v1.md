# Bare Metal Provisioning API v1 (gRPC + Hybrid Lifecycle)

## Why
We need a customer-facing inventory/ops service for bare-metal machines across multiple providers (datacenter we fully control + 3P providers).
The API must allow callers to trigger async operations (repave, join, reboot, upgrade, RMA, net reconfig) and observe progress.

We intentionally separate:
- "What a machine is" from "a run of work" from "the plan/recipe" (common pattern used by multiple BM systems).

## Goals
- Minimal gRPC API usable by other workstreams
- Async, idempotent operations with observable state machine (steps + logs)
- Provider-agnostic adapters (capability-based)
- Hybrid machine lifecycle: small explicit phase + derived Conditions + derived EffectiveState for UI

## Non-goals (v1)
- Full IPAM/network modeling like MAAS/Ironic
- Workflow DSL (v1 Plans are a small step list)
- Multi-machine atomic orchestration

---

## Data Model

### 1) Machine (customer-visible)
Stable identity + inventory facts + minimal explicit lifecycle `phase` + derived Conditions.

Machine is equivalent to "Hardware" in template/workflow systems (conceptual mapping).

Fields (conceptual):
- id, labels/tags, provider, location/rack
- access hints: ssh endpoint, bmc endpoint, network identifiers (MACs, serial)
- phase (explicit, minimal)
- conditions[] (derived truth/health)
- references: customer_id, target_cluster_ref (optional)
- active_run_id (optional)

### 2) Plan (reusable recipe)
A Plan is a small list of Steps that execute sequentially.
Equivalent to "Template" conceptually; steps are typed, not a free-form blob.

### 3) Run (execution record)
A Run applies a Plan (or operation type) to a Machine.
Equivalent to "Workflow" conceptually. Tracks the state machine and step progress.

Run is also our external "Operation handle" for long-running requests.

---

## Hybrid lifecycle (Machine)

### Explicit Phase (keep diagram simple; v1 = 7 phases)
- FACTORY_READY   (arrived w/ base OS + baseline networking)
- READY           (imported/manageable by us)
- PROVISIONING    (a Run is actively modifying it)
- IN_SERVICE      (serving workloads in customer cluster)
- MAINTENANCE     (customer drained / disruptive ops permitted)
- RMA             (removed from service for return/replace)
- RETIRED         (removed from inventory; no ops)

We do NOT add "ERROR" as a phase. Errors are Conditions.

### Derived Conditions (truth signals)
Minimal v1 conditions:
Connectivity:
- Reachable, BMCReachable
Truth:
- Provisioned, InCustomerCluster, Drained
Health/attention:
- Degraded, NeedsIntervention

### EffectiveState (what UI shows)
Precedence:
1) If active_run.phase in {PENDING,RUNNING} => PROVISIONING
2) Else if phase in {RMA, RETIRED, MAINTENANCE} => phase
3) Else if InCustomerCluster == true => IN_SERVICE
4) Else if phase == FACTORY_READY => FACTORY_READY
5) Else => READY
Overlays:
- NeedsIntervention=true => banner
- Degraded=true => banner

This avoids "state explosion" while keeping UI truthful.

---

## Run state machine

Run.phase:
- PENDING -> RUNNING -> (SUCCEEDED | FAILED | CANCELED)

Plus step-level statuses:
- WAITING, RUNNING, SUCCEEDED, FAILED
- retry_count, timestamps, message
Logs/events are attached to Run (not Machine).

---

## gRPC API (minimal)

Services:
- MachineService: Import/Register, Get, List, Update
- PlanService: Get/List (Create optional; can be config-driven first)
- RunService: Start, Get, List, Cancel, Watch, StreamLogs

Pattern: StartRun returns a Run handle; clients poll/watch Run for completion.

---

## Provider adapter model

Providers differ by capabilities. We standardize around Step execution, not "provider = entire world".

Provider interface is capability-based:
- Inspect/Discover
- PowerControl
- Netboot/PXE
- Imaging/Repave
- RemoteExec (SSH via router/jump)
- ClusterJoin (mint join material + run join)

Run executor:
- picks provider by machine.provider
- executes plan.steps sequentially
- writes step status + logs
- updates machine.phase at Run boundaries
- recomputes conditions asynchronously

---

## Operational guarantees

- Idempotency: StartRun requires request_id; server returns existing Run if same key is replayed.
- Concurrency: At most one active Run per Machine (enforced by DB/lock).
- Recovery: If executor crashes, it resumes from persisted Run step state.
- Auditing: all Run transitions + logs are preserved.

---

## v1 deliverables
- Proto definitions + Go server skeleton
- Run executor with 3-5 core step kinds:
  - SSHCommand, Reboot, SetNetboot, RepaveImage, KubeadmJoin, VerifyInCluster
- Minimal condition reconciler (Reachable, InCustomerCluster)
