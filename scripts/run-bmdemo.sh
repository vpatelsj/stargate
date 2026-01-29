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
echo "  • MachineService - Register and manage physical machines"
echo "  • PlanService    - Define multi-step provisioning workflows"
echo "  • RunService     - Execute plans with retries and streaming logs"
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
# Step 2: Show Available Plans
# ============================================================================

print_header "Step 2: View Available Provisioning Plans"

print_step "Listing built-in plans..."
run_cli plans

echo ""
print_info "Plans define the steps to execute during provisioning."
print_info "Each step has timeouts, retry policies, and specific actions."

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
print_info "Machines are registered with SSH endpoints and target cluster info."

wait_for_input

# ============================================================================
# Step 4: List Machines
# ============================================================================

print_header "Step 4: View Machine Inventory"

print_step "Listing all registered machines..."
run_cli list

# Machine IDs are now predictable: machine-1, machine-2, machine-3
MACHINE_1="machine-1"
MACHINE_2="machine-2"
MACHINE_3="machine-3"

echo ""
print_info "Machines start in FACTORY_READY phase, ready for provisioning."

wait_for_input

# ============================================================================
# Step 5: Repave a Machine
# ============================================================================

print_header "Step 5: Repave Machine (Full Provisioning Flow)"

print_step "Starting repave operation on $MACHINE_1..."
print_info "This will: set netboot → reboot → repave image → join cluster → verify"
echo ""

run_cli repave "$MACHINE_1"

echo ""
print_step "Checking run status..."
run_cli runs

echo ""
print_step "Checking machine state after repave..."
run_cli list

wait_for_input

# ============================================================================
# Step 6: Demonstrate Idempotency
# ============================================================================

print_header "Step 6: Idempotency Demonstration"

print_step "Trying to repave $MACHINE_2 with same request_id twice..."
echo ""

REQUEST_ID="demo-idempotent-$(date +%s)"

print_info "First request with request_id: $REQUEST_ID"
run_cli repave "$MACHINE_2" --request-id="$REQUEST_ID"

echo ""
print_info "Second request with SAME request_id (should return same run)..."
run_cli repave "$MACHINE_2" --request-id="$REQUEST_ID"

echo ""
print_info "Idempotency prevents duplicate runs for retried requests."

wait_for_input

# ============================================================================
# Step 7: Reboot Operation
# ============================================================================

print_header "Step 7: Simple Reboot Operation"

print_step "Executing reboot on $MACHINE_3..."
run_cli reboot "$MACHINE_3"

echo ""
print_step "Checking run history..."
run_cli runs

wait_for_input

# ============================================================================
# Step 8: Cancel Operation
# ============================================================================

print_header "Step 8: Cancellation Handling"

print_step "Starting a new repave on $MACHINE_2..."
# Start in background so we can cancel it
go run ./cmd/bmdemo-cli repave "$MACHINE_2" --request-id="cancel-demo" &
CLI_PID=$!
sleep 1

echo ""
print_step "Canceling the run mid-flight..."
# Get the latest run for machine-2
CANCEL_RUN_ID=$(go run ./cmd/bmdemo-cli runs 2>/dev/null | grep "$MACHINE_2" | grep RUNNING | awk '{print $1}' | head -1)
if [[ -n "$CANCEL_RUN_ID" ]]; then
    run_cli cancel "$CANCEL_RUN_ID"
else
    print_info "Run completed before we could cancel - that's OK!"
fi

# Wait for background CLI to finish
wait $CLI_PID 2>/dev/null || true

echo ""
print_step "Checking machine state after cancellation..."
run_cli list

echo ""
print_info "Canceled runs set machine to MAINTENANCE with NeedsIntervention condition."
print_info "This signals that manual investigation may be needed."

wait_for_input

# ============================================================================
# Step 9: RMA Flow
# ============================================================================

print_header "Step 9: RMA (Return Merchandise Authorization)"

print_step "Initiating RMA for $MACHINE_1 (hardware failure scenario)..."
run_cli rma "$MACHINE_1"

echo ""
print_step "Final machine states..."
run_cli list

echo ""
print_info "$MACHINE_1 is now in RMA phase, awaiting hardware replacement."

wait_for_input

# ============================================================================
# Step 10: View All Runs
# ============================================================================

print_header "Step 10: Run History and Audit Trail"

print_step "Viewing all runs across the system..."
run_cli runs

echo ""
print_info "Each run has: ID, machine, type, phase, timing, and step status."
print_info "This provides a complete audit trail for compliance."

wait_for_input

# ============================================================================
# Summary
# ============================================================================

print_header "Demo Complete!"

echo "What we demonstrated:"
echo ""
echo "  ✓ gRPC-based API with streaming support"
echo "  ✓ Machine lifecycle management (AVAILABLE → PROVISIONING → IN_SERVICE → RMA)"
echo "  ✓ Multi-step provisioning plans with retries"
echo "  ✓ Idempotent operations for safe retries"
echo "  ✓ Real-time logging and event streaming"
echo "  ✓ Complete audit trail for all operations"
echo ""
echo "Architecture highlights:"
echo ""
echo "  • Provider interface allows swapping fake/real implementations"
echo "  • Thread-safe in-memory store (can be backed by database)"
echo "  • Async execution with cancellation support"
echo "  • Condition-based status tracking"
echo ""
echo -e "${GREEN}Thank you for watching the demo!${NC}"
echo ""

# Keep server running for Q&A
echo -e "${YELLOW}Server still running. Press Ctrl+C to stop.${NC}"
wait $SERVER_PID
