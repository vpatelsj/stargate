#!/bin/bash
# Baremetal Provisioning Demo Script
# Run this script to demonstrate the bmdemo system end-to-end
#
# Usage: ./scripts/run-bmdemo.sh [--slow]
#
# Options:
#   --slow    Use slower timings for better visibility during demos

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
cd "$PROJECT_DIR"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
NC='\033[0m' # No Color
BOLD='\033[1m'

# Configuration
SERVER_PORT=50051
SLOW_MODE=""
if [[ "$1" == "--slow" ]]; then
    SLOW_MODE="--slow"
    echo -e "${YELLOW}Running in slow mode for demo visibility${NC}"
fi

# Helper functions
print_header() {
    echo ""
    echo -e "${BLUE}═══════════════════════════════════════════════════════════════${NC}"
    echo -e "${BOLD}${CYAN}  $1${NC}"
    echo -e "${BLUE}═══════════════════════════════════════════════════════════════${NC}"
    echo ""
}

print_step() {
    echo -e "${GREEN}▶${NC} ${BOLD}$1${NC}"
}

print_info() {
    echo -e "${YELLOW}  ℹ${NC} $1"
}

print_command() {
    echo -e "${CYAN}  \$${NC} $1"
}

wait_for_input() {
    echo ""
    echo -e "${YELLOW}Press Enter to continue...${NC}"
    read -r
}

run_cli() {
    echo -e "${CYAN}  \$${NC} go run ./cmd/bmdemo-cli $*"
    go run ./cmd/bmdemo-cli "$@"
}

cleanup() {
    echo ""
    echo -e "${YELLOW}Cleaning up...${NC}"
    if [[ -n "$SERVER_PID" ]]; then
        kill "$SERVER_PID" 2>/dev/null || true
    fi
}
trap cleanup EXIT

# ============================================================================
# DEMO START
# ============================================================================

print_header "Baremetal Provisioning Demo"

echo "This demo shows our gRPC-based baremetal provisioning system."
echo ""
echo "Key components:"
echo "  • MachineService   - Register machines, trigger lifecycle operations"
echo "  • OperationService - Track operations with streaming logs/events"
echo ""
echo "Key concepts:"
echo "  • Phase          - Imperative intent (FACTORY_READY, READY, MAINTENANCE)"
echo "  • EffectiveState - Observed state (NEW, IDLE, BUSY, MAINT, ATTENTION, BLOCKED)"
echo ""

wait_for_input

# ============================================================================
# Step 1: Build and Start Server
# ============================================================================

print_header "Step 1: Build and Start the Server"

print_step "Building the demo binaries..."
print_command "make bmdemo"
make bmdemo

echo ""
print_step "Starting the gRPC server..."
print_command "go run ./cmd/bmdemo-server $SLOW_MODE &"

go run ./cmd/bmdemo-server $SLOW_MODE &
SERVER_PID=$!

# Wait for server to be ready (check if port is listening)
print_info "Waiting for server to start..."
for i in {1..30}; do
    if nc -z localhost $SERVER_PORT 2>/dev/null; then
        break
    fi
    sleep 0.5
done

# Verify server is responding
if ! nc -z localhost $SERVER_PORT 2>/dev/null; then
    echo -e "${RED}ERROR: Server failed to start on port $SERVER_PORT${NC}"
    exit 1
fi

print_info "Server running on port $SERVER_PORT (PID: $SERVER_PID)"

wait_for_input

# ============================================================================
# Step 2: Explain Dual State Model
# ============================================================================

print_header "Step 2: Understanding Phase vs Effective State"

echo "The CLI shows two state columns:"
echo ""
echo "  PHASE     - Operator intent (gates which operations are allowed)"
echo "              Values: FACTORY, READY, MAINT"
echo ""
echo "  EFFECTIVE - Observed state computed by the server"
echo "              Values: NEW, IDLE, BUSY, MAINT, ⚠ATTN, ⛔BLOCK"
echo ""
echo "Precedence rules for EFFECTIVE:"
echo "  1. Retired/RMA condition  → BLOCKED"
echo "  2. NeedsIntervention      → ATTENTION"
echo "  3. Active operation       → BUSY"
echo "  4. Phase=MAINTENANCE      → MAINT"
echo "  5. Phase=FACTORY_READY    → NEW"
echo "  6. Otherwise              → IDLE"
echo ""

wait_for_input

# ============================================================================
# Step 3: Import Machines
# ============================================================================

print_header "Step 3: Import Machines into Inventory"

print_step "Importing 3 machines from simulated datacenter..."
run_cli import 3

echo ""
print_step "Verifying machines were imported..."
run_cli list

echo ""
print_info "Machines are registered with SSH endpoints."
print_info "Notice: PHASE=FACTORY (intent), EFFECTIVE=NEW (observed)."

wait_for_input

# ============================================================================
# Step 4: Enter Maintenance
# ============================================================================

print_header "Step 4: Enter Maintenance Mode"

MACHINE_1="machine-1"
MACHINE_2="machine-2"
MACHINE_3="machine-3"

print_step "Entering maintenance mode on $MACHINE_1..."
print_info "This is required before reimage (safety gate)."
echo ""

run_cli enter-maintenance "$MACHINE_1"

echo ""
print_step "Checking machine state..."
run_cli list

echo ""
print_info "$MACHINE_1 is now in MAINTENANCE phase (EFFECTIVE=MAINT)."

wait_for_input

# ============================================================================
# Step 5: Reimage a Machine
# ============================================================================

print_header "Step 5: Reimage Machine (Full Provisioning Flow)"

print_step "Starting reimage operation on $MACHINE_1..."
print_info "This will: set netboot → reboot → repave image → join cluster → verify"
echo ""

run_cli reimage "$MACHINE_1"

echo ""
print_step "Checking operation status..."
run_cli ops

echo ""
print_step "Checking machine state after reimage..."
run_cli list

echo ""
print_info "Machine stays in MAINTENANCE after reimage - must explicitly exit."

wait_for_input

# ============================================================================
# Step 6: Exit Maintenance
# ============================================================================

print_header "Step 6: Exit Maintenance Mode"

print_step "Exiting maintenance mode on $MACHINE_1..."
run_cli exit-maintenance "$MACHINE_1"

echo ""
print_step "Final state of $MACHINE_1..."
run_cli list

echo ""
print_info "$MACHINE_1 is now READY/IDLE and provisioned!"

wait_for_input

# ============================================================================
# Step 7: Demonstrate Idempotency
# ============================================================================

print_header "Step 7: Idempotency Demonstration"

print_step "Entering maintenance on $MACHINE_2..."
run_cli enter-maintenance "$MACHINE_2"

echo ""
print_step "Trying to reimage $MACHINE_2 with same request_id twice..."
echo ""

REQUEST_ID="demo-idempotent-$(date +%s)"

print_info "First request with request_id: $REQUEST_ID"
run_cli reimage "$MACHINE_2" --request-id="$REQUEST_ID"

echo ""
print_info "Second request with SAME request_id (should return same operation)..."
run_cli reimage "$MACHINE_2" --request-id="$REQUEST_ID"

echo ""
print_info "Idempotency prevents duplicate operations for retried requests."

wait_for_input

# ============================================================================
# Step 8: Reboot Operation
# ============================================================================

print_header "Step 8: Simple Reboot Operation"

print_step "Exiting maintenance on $MACHINE_2 first..."
run_cli exit-maintenance "$MACHINE_2"

echo ""
print_step "Executing reboot on $MACHINE_2..."
print_info "Reboot works on READY machines (no maintenance required)."
run_cli reboot "$MACHINE_2"

echo ""
print_step "Checking operation history..."
run_cli ops

wait_for_input

# ============================================================================
# Step 9: Cancel Operation
# ============================================================================

print_header "Step 9: Cancellation Handling"

print_step "Entering maintenance on $MACHINE_3..."
run_cli enter-maintenance "$MACHINE_3"

echo ""
print_step "Starting a new reimage on $MACHINE_3..."
# Start in background so we can cancel it
go run ./cmd/bmdemo-cli reimage "$MACHINE_3" --request-id="cancel-demo" &
CLI_PID=$!
sleep 1

echo ""
print_step "Canceling the operation mid-flight..."
# Get the latest operation for machine-3
CANCEL_OP_ID=$(go run ./cmd/bmdemo-cli ops 2>/dev/null | grep "$MACHINE_3" | grep -E "PENDING|RUNNING" | awk '{print $1}' | head -1)
if [[ -n "$CANCEL_OP_ID" ]]; then
    run_cli cancel "$CANCEL_OP_ID"
else
    print_info "Operation completed before we could cancel - that's OK!"
fi

# Wait for background CLI to finish
wait $CLI_PID 2>/dev/null || true

echo ""
print_step "Checking machine state after cancellation..."
run_cli list

echo ""
print_info "Canceled operations set OperationCanceled condition."
print_info "Machine stays in MAINTENANCE for investigation."

wait_for_input

# ============================================================================
# Step 10: View All Operations
# ============================================================================

print_header "Step 10: Operation History and Audit Trail"

print_step "Viewing all operations across the system..."
run_cli ops

echo ""
print_info "Each operation has: ID, machine, type, phase, stage, and error."
print_info "This provides a complete audit trail for compliance."

wait_for_input

# ============================================================================
# Summary
# ============================================================================

print_header "Demo Complete!"

echo "What we demonstrated:"
echo ""
echo "  ✓ Dual state model: Phase (intent) vs EffectiveState (observed)"
echo "  ✓ Lifecycle operations: reboot, reimage, enter/exit-maintenance"
echo "  ✓ Safety gates: Reimage requires MAINTENANCE phase"
echo "  ✓ Idempotent operations for safe retries"
echo "  ✓ Real-time logging and event streaming"
echo "  ✓ Complete audit trail for all operations"
echo ""
echo "API design:"
echo ""
echo "  • MachineService  - CRUD + lifecycle operations (reboot, reimage, etc.)"
echo "  • OperationService - Read-only: Get, List, Watch, StreamLogs"
echo "  • No PlanService exposed - plans are internal implementation details"
echo ""
echo -e "${GREEN}Thank you for watching the demo!${NC}"
echo ""

# Keep server running for Q&A
echo -e "${YELLOW}Server still running. Press Ctrl+C to stop.${NC}"
wait $SERVER_PID
