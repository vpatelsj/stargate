// Package main implements the bmdemo CLI for baremetal provisioning demo.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/vpatelsj/stargate/gen/baremetal/v1"
	"github.com/vpatelsj/stargate/internal/bmdemo/lifecycle"
)

var (
	serverAddr = flag.String("server", "localhost:50051", "The server address")
)

func main() {
	flag.Parse()

	if len(flag.Args()) < 1 {
		printUsage()
		os.Exit(1)
	}

	conn, err := grpc.Dial(*serverAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("failed to connect: %v", err)
	}
	defer conn.Close()

	cmd := flag.Args()[0]
	args := flag.Args()[1:]

	switch cmd {
	case "import":
		importMachines(conn, args)
	case "list":
		listMachines(conn)
	case "reboot":
		rebootMachine(conn, args)
	case "reimage":
		reimageMachine(conn, args)
	case "enter-maintenance":
		enterMaintenance(conn, args)
	case "exit-maintenance":
		exitMaintenance(conn, args)
	case "cancel":
		cancelOperation(conn, args)
	case "ops":
		listOperations(conn)
	case "watch":
		watchOperations(conn, args)
	case "logs":
		streamLogs(conn, args)
	case "demo":
		runDemo(conn)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`bmdemo-cli - Baremetal Demo CLI

Usage: bmdemo-cli [flags] <command> [args]

Commands:
  import <N>                  Import N fake machines (factory-ready, provider=fake)
  list                        List machines with phase and conditions
  reboot <machine-id>         Reboot a machine (requires READY or MAINTENANCE)
  reimage <machine-id>        Reimage a machine (requires MAINTENANCE)
  enter-maintenance <machine-id>  Enter maintenance mode
  exit-maintenance <machine-id>   Exit maintenance mode
  cancel <operation-id>       Cancel an operation in progress
  ops                         List all operations
  watch [machine-id]          Watch operation events (streaming)
  logs <operation-id>         Stream operation logs
  demo                        Run scripted demo for design docs

Flags:`)
	flag.PrintDefaults()
}

// ============================================================================
// import - Register N fake machines
// ============================================================================

func importMachines(conn *grpc.ClientConn, args []string) {
	if len(args) < 1 {
		log.Fatal("usage: import <N>")
	}

	var count int
	if _, err := fmt.Sscanf(args[0], "%d", &count); err != nil || count <= 0 {
		log.Fatalf("invalid count: %s (must be positive integer)", args[0])
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client := pb.NewMachineServiceClient(conn)

	fmt.Printf("Importing %d fake machines...\n", count)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "MACHINE_ID\tMAC\tPHASE")

	for i := 0; i < count; i++ {
		// Generate fake MAC address: 02:00:00:00:XX:YY
		mac := fmt.Sprintf("02:00:00:00:%02x:%02x", i/256, i%256)
		machineID := fmt.Sprintf("machine-%d", i+1)

		machine, err := client.RegisterMachine(ctx, &pb.RegisterMachineRequest{
			Machine: &pb.Machine{
				MachineId: machineID,
				Spec: &pb.MachineSpec{
					Provider:     "fake",
					MacAddresses: []string{mac},
					SshEndpoint:  fmt.Sprintf("10.0.%d.%d:22", i/256, i%256+10),
				},
				Status: &pb.MachineStatus{
					Phase: pb.MachineStatus_FACTORY_READY,
				},
				Labels: map[string]string{
					"role":     "worker",
					"imported": "true",
				},
			},
		})
		if err != nil {
			log.Printf("failed to import machine %d: %v", i, err)
			continue
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", machine.MachineId, mac, machine.Status.Phase.String())
	}
	w.Flush()
	fmt.Printf("\nImported %d machines.\n", count)
}

// ============================================================================
// list - List machines with phase and conditions
// ============================================================================

func listMachines(conn *grpc.ClientConn) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	machineClient := pb.NewMachineServiceClient(conn)

	resp, err := machineClient.ListMachines(ctx, &pb.ListMachinesRequest{})
	if err != nil {
		log.Fatalf("ListMachines failed: %v", err)
	}

	if len(resp.Machines) == 0 {
		fmt.Println("No machines registered. Use 'import <N>' to create fake machines.")
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "MACHINE_ID\tPHASE\tREACHABLE\tPROVISIONED\tNEEDS_HELP\tACTIVE_OP")

	for _, m := range resp.Machines {
		phase := "UNKNOWN"
		reachable := "-"
		provisioned := "-"
		needsHelp := "-"
		activeOp := "-"

		if m.Status != nil {
			phase = m.Status.Phase.String()

			// Conditions
			if c := lifecycle.GetCondition(m.Status, lifecycle.ConditionReachable); c != nil {
				if c.Status {
					reachable = "✓"
				} else {
					reachable = "✗"
				}
			}
			if c := lifecycle.GetCondition(m.Status, lifecycle.ConditionProvisioned); c != nil {
				if c.Status {
					provisioned = "✓"
				} else {
					provisioned = "✗"
				}
			}
			if c := lifecycle.GetCondition(m.Status, lifecycle.ConditionNeedsIntervention); c != nil {
				if c.Status {
					needsHelp = "⚠"
				} else {
					needsHelp = "-"
				}
			}

			if m.Status.ActiveOperationId != "" {
				activeOp = m.Status.ActiveOperationId[:8] // Short ID
			}
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			m.MachineId, phase, reachable, provisioned, needsHelp, activeOp)
	}
	w.Flush()
}

// ============================================================================
// reboot - Reboot a machine
// ============================================================================

func rebootMachine(conn *grpc.ClientConn, args []string) {
	if len(args) < 1 {
		log.Fatal("usage: reboot <machine-id> [--request-id=<id>]")
	}
	machineID := args[0]
	requestID := getRequestID(args[1:])

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	setupSignalHandler(cancel)

	client := pb.NewMachineServiceClient(conn)
	opClient := pb.NewOperationServiceClient(conn)

	op, err := client.RebootMachine(ctx, &pb.RebootMachineRequest{
		MachineId: machineID,
		RequestId: requestID,
	})
	if err != nil {
		log.Fatalf("RebootMachine failed: %v", err)
	}

	printOperationHeader(op, "REBOOT")
	watchAndStreamOperation(ctx, opClient, op)
}

// ============================================================================
// reimage - Reimage a machine (requires MAINTENANCE)
// ============================================================================

func reimageMachine(conn *grpc.ClientConn, args []string) {
	if len(args) < 1 {
		log.Fatal("usage: reimage <machine-id> [--request-id=<id>] [--image=<ref>]")
	}
	machineID := args[0]
	requestID := getRequestID(args[1:])
	imageRef := getFlag(args[1:], "--image")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	setupSignalHandler(cancel)

	client := pb.NewMachineServiceClient(conn)
	opClient := pb.NewOperationServiceClient(conn)

	op, err := client.ReimageMachine(ctx, &pb.ReimageMachineRequest{
		MachineId: machineID,
		RequestId: requestID,
		ImageRef:  imageRef,
	})
	if err != nil {
		log.Fatalf("ReimageMachine failed: %v", err)
	}

	printOperationHeader(op, "REIMAGE")
	watchAndStreamOperation(ctx, opClient, op)
}

// ============================================================================
// enter-maintenance - Enter maintenance mode
// ============================================================================

func enterMaintenance(conn *grpc.ClientConn, args []string) {
	if len(args) < 1 {
		log.Fatal("usage: enter-maintenance <machine-id> [--request-id=<id>]")
	}
	machineID := args[0]
	requestID := getRequestID(args[1:])

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	setupSignalHandler(cancel)

	client := pb.NewMachineServiceClient(conn)
	opClient := pb.NewOperationServiceClient(conn)

	op, err := client.EnterMaintenance(ctx, &pb.EnterMaintenanceRequest{
		MachineId: machineID,
		RequestId: requestID,
	})
	if err != nil {
		log.Fatalf("EnterMaintenance failed: %v", err)
	}

	printOperationHeader(op, "ENTER_MAINTENANCE")
	watchAndStreamOperation(ctx, opClient, op)
}

// ============================================================================
// exit-maintenance - Exit maintenance mode
// ============================================================================

func exitMaintenance(conn *grpc.ClientConn, args []string) {
	if len(args) < 1 {
		log.Fatal("usage: exit-maintenance <machine-id> [--request-id=<id>]")
	}
	machineID := args[0]
	requestID := getRequestID(args[1:])

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	setupSignalHandler(cancel)

	client := pb.NewMachineServiceClient(conn)
	opClient := pb.NewOperationServiceClient(conn)

	op, err := client.ExitMaintenance(ctx, &pb.ExitMaintenanceRequest{
		MachineId: machineID,
		RequestId: requestID,
	})
	if err != nil {
		log.Fatalf("ExitMaintenance failed: %v", err)
	}

	printOperationHeader(op, "EXIT_MAINTENANCE")
	watchAndStreamOperation(ctx, opClient, op)
}

// ============================================================================
// Helper functions for operation watching
// ============================================================================

func getRequestID(args []string) string {
	for _, arg := range args {
		if strings.HasPrefix(arg, "--request-id=") {
			return strings.TrimPrefix(arg, "--request-id=")
		}
	}
	return uuid.New().String()
}

func getFlag(args []string, prefix string) string {
	for _, arg := range args {
		if strings.HasPrefix(arg, prefix+"=") {
			return strings.TrimPrefix(arg, prefix+"=")
		}
	}
	return ""
}

func setupSignalHandler(cancel context.CancelFunc) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println("\n⚠ Interrupted. Operation continues in background.")
		cancel()
	}()
}

func printOperationHeader(op *pb.Operation, opType string) {
	fmt.Printf("┌─────────────────────────────────────────────────────────────\n")
	fmt.Printf("│ Operation: %s\n", op.OperationId)
	fmt.Printf("│ Machine: %s\n", op.MachineId)
	fmt.Printf("│ Type: %s\n", opType)
	fmt.Printf("│ Request ID: %s\n", op.RequestId)
	fmt.Printf("└─────────────────────────────────────────────────────────────\n")
	fmt.Println()
}

func watchAndStreamOperation(ctx context.Context, client pb.OperationServiceClient, op *pb.Operation) {
	var wg sync.WaitGroup

	// Event watcher
	wg.Add(1)
	go func() {
		defer wg.Done()
		watchOperationEvents(ctx, client, op.OperationId, op.MachineId)
	}()

	// Log streamer
	wg.Add(1)
	go func() {
		defer wg.Done()
		streamOperationLogs(ctx, client, op.OperationId)
	}()

	// Poll for completion
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	lastStep := ""

	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case <-ticker.C:
			o, err := client.GetOperation(ctx, &pb.GetOperationRequest{OperationId: op.OperationId})
			if err != nil {
				continue
			}

			// Print step transitions
			if o.CurrentStage != lastStep {
				if lastStep != "" {
					fmt.Printf("  ✓ %s\n", lastStep)
				}
				if o.CurrentStage != "" {
					fmt.Printf("→ %s...\n", o.CurrentStage)
				}
				lastStep = o.CurrentStage
			}

			// Check for terminal state
			if lifecycle.IsTerminalOperationPhase(o.Phase) {
				fmt.Println()
				printOperationResult(o)
				wg.Wait()
				return
			}
		}
	}
}

func watchOperationEvents(ctx context.Context, client pb.OperationServiceClient, opID, machineID string) {
	stream, err := client.WatchOperations(ctx, &pb.WatchOperationsRequest{
		Filter: fmt.Sprintf("machine_id=%s", machineID),
	})
	if err != nil {
		return
	}

	for {
		event, err := stream.Recv()
		if err != nil {
			return // Context cancelled or stream ended
		}
		if event.Snapshot != nil && event.Snapshot.OperationId == opID {
			if event.Message != "" {
				ts := event.Ts.AsTime().Format("15:04:05")
				fmt.Printf("  [%s] %s\n", ts, event.Message)
			}
		}
	}
}

func streamOperationLogs(ctx context.Context, client pb.OperationServiceClient, opID string) {
	stream, err := client.StreamOperationLogs(ctx, &pb.StreamOperationLogsRequest{OperationId: opID})
	if err != nil {
		return
	}

	for {
		chunk, err := stream.Recv()
		if err != nil {
			return // Context cancelled or stream ended
		}
		data := strings.TrimSpace(string(chunk.Data))
		if data != "" {
			prefix := "│"
			if chunk.Stream == "stderr" {
				prefix = "│!"
			}
			for _, line := range strings.Split(data, "\n") {
				fmt.Printf("  %s %s\n", prefix, line)
			}
		}
	}
}

func printOperationResult(op *pb.Operation) {
	fmt.Printf("┌─────────────────────────────────────────────────────────────\n")

	var icon string
	switch op.Phase {
	case pb.Operation_SUCCEEDED:
		icon = "✓"
		fmt.Printf("│ %s SUCCEEDED\n", icon)
	case pb.Operation_FAILED:
		icon = "✗"
		fmt.Printf("│ %s FAILED\n", icon)
	case pb.Operation_CANCELED:
		icon = "○"
		fmt.Printf("│ %s CANCELED\n", icon)
	default:
		fmt.Printf("│ Phase: %s\n", op.Phase)
	}

	// Duration
	if op.StartedAt != nil && op.FinishedAt != nil {
		duration := op.FinishedAt.AsTime().Sub(op.StartedAt.AsTime())
		fmt.Printf("│ Duration: %s\n", duration.Round(time.Millisecond))
	}

	// Steps summary
	succeeded := 0
	failed := 0
	for _, s := range op.Steps {
		switch s.State {
		case pb.StepStatus_SUCCEEDED:
			succeeded++
		case pb.StepStatus_FAILED:
			failed++
		}
	}
	if len(op.Steps) > 0 {
		fmt.Printf("│ Steps: %d/%d completed\n", succeeded, len(op.Steps))
	}

	// Error details
	if op.Error != nil {
		fmt.Printf("│\n")
		fmt.Printf("│ Error: %s\n", op.Error.Message)
		if op.Error.Code != "" {
			fmt.Printf("│ Code: %s\n", op.Error.Code)
		}
		if op.Error.Retryable {
			fmt.Printf("│ (retryable)\n")
		}
	}

	fmt.Printf("└─────────────────────────────────────────────────────────────\n")
}

// ============================================================================
// ops - List all operations
// ============================================================================

func listOperations(conn *grpc.ClientConn) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client := pb.NewOperationServiceClient(conn)

	resp, err := client.ListOperations(ctx, &pb.ListOperationsRequest{})
	if err != nil {
		log.Fatalf("ListOperations failed: %v", err)
	}

	if len(resp.Operations) == 0 {
		fmt.Println("No operations.")
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "OPERATION_ID\tMACHINE\tTYPE\tPHASE\tSTAGE\tERROR")

	for _, op := range resp.Operations {
		stage := op.CurrentStage
		if stage == "" {
			stage = "-"
		}

		errMsg := "-"
		if op.Error != nil && op.Error.Message != "" {
			errMsg = truncate(op.Error.Message, 30)
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			op.OperationId, op.MachineId, op.Type, op.Phase, stage, errMsg)
	}
	w.Flush()
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}

// ============================================================================
// cancel - Cancel an operation in progress
// ============================================================================

func cancelOperation(conn *grpc.ClientConn, args []string) {
	if len(args) < 1 {
		fmt.Println("Usage: bmdemo-cli cancel <operation-id>")
		os.Exit(1)
	}

	opID := args[0]

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client := pb.NewMachineServiceClient(conn)

	op, err := client.CancelOperation(ctx, &pb.CancelOperationRequest{OperationId: opID})
	if err != nil {
		log.Fatalf("CancelOperation failed: %v", err)
	}

	fmt.Printf("✓ Canceled operation %s (phase: %s)\n", op.OperationId, op.Phase)
}

// ============================================================================
// watch - Watch operation events
// ============================================================================

func watchOperations(conn *grpc.ClientConn, args []string) {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	client := pb.NewOperationServiceClient(conn)

	filter := ""
	if len(args) > 0 {
		filter = fmt.Sprintf("machine_id=%s", args[0])
	}

	stream, err := client.WatchOperations(ctx, &pb.WatchOperationsRequest{Filter: filter})
	if err != nil {
		log.Fatalf("WatchOperations failed: %v", err)
	}

	fmt.Println("Watching operation events (Ctrl+C to stop)...")
	fmt.Println()

	for {
		event, err := stream.Recv()
		if err == io.EOF {
			return
		}
		if err != nil {
			if ctx.Err() != nil {
				fmt.Println("\nStopped watching.")
				return
			}
			log.Fatalf("WatchOperations stream error: %v", err)
		}

		ts := event.Ts.AsTime().Format("15:04:05")
		snap := event.Snapshot

		opID := snap.OperationId
		if len(opID) > 8 {
			opID = opID[:8]
		}

		stage := snap.CurrentStage
		if stage == "" {
			stage = "-"
		}

		fmt.Printf("[%s] %s | %s | %s | stage=%s | %s\n",
			ts, opID, snap.MachineId, snap.Phase, stage, event.Message)
	}
}

// ============================================================================
// logs - Stream operation logs
// ============================================================================

func streamLogs(conn *grpc.ClientConn, args []string) {
	if len(args) < 1 {
		log.Fatal("usage: logs <operation-id>")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	client := pb.NewOperationServiceClient(conn)

	stream, err := client.StreamOperationLogs(ctx, &pb.StreamOperationLogsRequest{OperationId: args[0]})
	if err != nil {
		log.Fatalf("StreamOperationLogs failed: %v", err)
	}

	fmt.Printf("Streaming logs for operation %s (Ctrl+C to stop)...\n\n", args[0])

	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			return
		}
		if err != nil {
			if ctx.Err() != nil {
				fmt.Println("\nStopped streaming.")
				return
			}
			log.Fatalf("StreamOperationLogs stream error: %v", err)
		}

		ts := chunk.Ts.AsTime().Format("15:04:05")
		prefix := "stdout"
		if chunk.Stream == "stderr" {
			prefix = "stderr"
		}
		data := strings.TrimSuffix(string(chunk.Data), "\n")
		fmt.Printf("[%s] %s: %s\n", ts, prefix, data)
	}
}

// ============================================================================
// demo - Scripted demo for design docs
// ============================================================================

func runDemo(conn *grpc.ClientConn) {
	ctx := context.Background()

	machineClient := pb.NewMachineServiceClient(conn)
	opClient := pb.NewOperationServiceClient(conn)

	fmt.Println("# Baremetal Lifecycle Demo (New API)")
	fmt.Println()
	fmt.Println("## 1. Import Factory-Ready Machines")
	fmt.Println("```")

	// Import 3 machines
	var machines []*pb.Machine
	for i := 0; i < 3; i++ {
		mac := fmt.Sprintf("02:00:00:00:00:%02x", i+1)
		m, err := machineClient.RegisterMachine(ctx, &pb.RegisterMachineRequest{
			Machine: &pb.Machine{
				MachineId: fmt.Sprintf("machine-%d", i+1),
				Spec: &pb.MachineSpec{
					Provider:     "fake",
					MacAddresses: []string{mac},
					SshEndpoint:  fmt.Sprintf("10.0.0.%d:22", i+10),
				},
				Status: &pb.MachineStatus{
					Phase: pb.MachineStatus_FACTORY_READY,
				},
				Labels: map[string]string{"role": "worker"},
			},
		})
		if err != nil {
			log.Fatalf("register failed: %v", err)
		}
		machines = append(machines, m)
		fmt.Printf("%-12s  %-20s  %s\n", m.MachineId, mac, m.Status.Phase)
	}
	fmt.Println("```")
	fmt.Println()

	// Print initial state
	demoListMachines(ctx, machineClient)

	// Step 2: Enter maintenance for machine-1
	fmt.Println("## 2. Enter Maintenance for machine-1")
	fmt.Println("```")

	maintenanceOp, err := machineClient.EnterMaintenance(ctx, &pb.EnterMaintenanceRequest{
		MachineId: "machine-1",
		RequestId: uuid.New().String(),
	})
	if err != nil {
		log.Fatalf("EnterMaintenance failed: %v", err)
	}
	fmt.Printf("operation_id=%s  machine=%s  type=%s  phase=%s\n",
		maintenanceOp.OperationId[:8], maintenanceOp.MachineId, maintenanceOp.Type, maintenanceOp.Phase)

	// Wait for completion
	waitForOperation(ctx, opClient, maintenanceOp.OperationId)
	fmt.Println("```")
	fmt.Println()

	demoListMachines(ctx, machineClient)

	// Step 3: Reimage machine-1 (now in MAINTENANCE)
	fmt.Println("## 3. Reimage machine-1 (requires MAINTENANCE)")
	fmt.Println("```")

	reimageOp, err := machineClient.ReimageMachine(ctx, &pb.ReimageMachineRequest{
		MachineId: "machine-1",
		RequestId: uuid.New().String(),
	})
	if err != nil {
		log.Fatalf("ReimageMachine failed: %v", err)
	}
	fmt.Printf("operation_id=%s  machine=%s  type=%s  phase=%s\n",
		reimageOp.OperationId[:8], reimageOp.MachineId, reimageOp.Type, reimageOp.Phase)

	// Wait for completion
	waitForOperation(ctx, opClient, reimageOp.OperationId)
	fmt.Println("```")
	fmt.Println()

	// Show state after reimage
	demoListMachines(ctx, machineClient)

	// Step 4: Exit maintenance for machine-1
	fmt.Println("## 4. Exit Maintenance for machine-1")
	fmt.Println("```")

	exitOp, err := machineClient.ExitMaintenance(ctx, &pb.ExitMaintenanceRequest{
		MachineId: "machine-1",
		RequestId: uuid.New().String(),
	})
	if err != nil {
		log.Fatalf("ExitMaintenance failed: %v", err)
	}
	fmt.Printf("operation_id=%s  machine=%s  type=%s  phase=%s\n",
		exitOp.OperationId[:8], exitOp.MachineId, exitOp.Type, exitOp.Phase)

	waitForOperation(ctx, opClient, exitOp.OperationId)
	fmt.Println("```")
	fmt.Println()

	demoListMachines(ctx, machineClient)

	// Step 5: Try to Reimage machine-2 without MAINTENANCE (should fail)
	fmt.Println("## 5. Try to Reimage machine-2 without MAINTENANCE (should fail)")
	fmt.Println("```")

	_, err = machineClient.ReimageMachine(ctx, &pb.ReimageMachineRequest{
		MachineId: "machine-2",
		RequestId: uuid.New().String(),
	})
	if err != nil {
		fmt.Printf("Expected error: %v\n", err)
	}
	fmt.Println("```")
	fmt.Println()

	// Final state
	fmt.Println("## Final State")
	demoListMachines(ctx, machineClient)

	fmt.Println("---")
	fmt.Println("*Demo complete. Shows: MAINTENANCE required for reimage, proper phase transitions.*")
}

func waitForOperation(ctx context.Context, client pb.OperationServiceClient, opID string) {
	for {
		time.Sleep(200 * time.Millisecond)
		op, err := client.GetOperation(ctx, &pb.GetOperationRequest{OperationId: opID})
		if err != nil {
			continue
		}
		if op.CurrentStage != "" {
			fmt.Printf("  step: %s\n", op.CurrentStage)
		}
		if lifecycle.IsTerminalOperationPhase(op.Phase) {
			fmt.Printf("result: %s\n", op.Phase)
			break
		}
	}
}

func demoListMachines(ctx context.Context, machineClient pb.MachineServiceClient) {
	fmt.Println("### Machine State")
	fmt.Println("```")

	resp, _ := machineClient.ListMachines(ctx, &pb.ListMachinesRequest{})

	fmt.Printf("%-12s  %-14s  %s\n", "MACHINE", "PHASE", "CONDITIONS")
	for _, m := range resp.Machines {
		if m.Status == nil {
			continue
		}

		var conds []string
		for _, c := range m.Status.Conditions {
			if c.Status {
				conds = append(conds, c.Type+"=✓")
			}
		}
		condStr := "-"
		if len(conds) > 0 {
			condStr = strings.Join(conds, ", ")
		}

		fmt.Printf("%-12s  %-14s  %s\n",
			m.MachineId, m.Status.Phase, condStr)
	}
	fmt.Println("```")
	fmt.Println()
}
