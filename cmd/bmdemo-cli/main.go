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
	case "repave":
		runAndWatch(conn, args, "REPAVE", "plan/repave-join")
	case "rma":
		runAndWatch(conn, args, "RMA", "plan/rma")
	case "reboot":
		runAndWatch(conn, args, "REBOOT", "plan/reboot")
	case "runs":
		listRuns(conn)
	case "plans":
		listPlans(conn)
	case "watch":
		watchRuns(conn, args)
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
  import <N>              Import N fake machines (factory-ready, provider=fake)
  list                    List machines with phase, effective state, conditions
  repave <machine-id>     Start repave run (plan/repave-join) and watch
  rma <machine-id>        Start RMA run (plan/rma) and watch
  reboot <machine-id>     Start reboot run (plan/reboot) and watch
  runs                    List all runs
  plans                   List available plans
  watch [machine-id]      Watch run events (streaming)
  logs <run-id>           Stream run logs
  demo                    Run scripted demo for design docs

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

		machine, err := client.RegisterMachine(ctx, &pb.RegisterMachineRequest{
			Machine: &pb.Machine{
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
// list - List machines with effective state
// ============================================================================

func listMachines(conn *grpc.ClientConn) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	machineClient := pb.NewMachineServiceClient(conn)
	runClient := pb.NewRunServiceClient(conn)

	resp, err := machineClient.ListMachines(ctx, &pb.ListMachinesRequest{})
	if err != nil {
		log.Fatalf("ListMachines failed: %v", err)
	}

	if len(resp.Machines) == 0 {
		fmt.Println("No machines registered. Use 'import <N>' to create fake machines.")
		return
	}

	// Get all runs to compute effective state
	runsResp, _ := runClient.ListRuns(ctx, &pb.ListRunsRequest{})
	activeRuns := make(map[string]*pb.Run)
	for _, r := range runsResp.GetRuns() {
		if lifecycle.IsActiveRunPhase(r.Phase) {
			activeRuns[r.MachineId] = r
		}
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "MACHINE_ID\tPHASE\tEFFECTIVE\tREACHABLE\tIN_CLUSTER\tNEEDS_HELP\tACTIVE_RUN")

	for _, m := range resp.Machines {
		phase := "UNKNOWN"
		effective := "UNKNOWN"
		reachable := "-"
		inCluster := "-"
		needsHelp := "-"
		activeRun := "-"

		if m.Status != nil {
			phase = strings.TrimPrefix(m.Status.Phase.String(), "")

			// Compute effective state
			ar := activeRuns[m.MachineId]
			effectivePhase := lifecycle.EffectiveState(m.Status, ar)
			effective = effectivePhase.String()

			// Conditions
			if c := lifecycle.GetCondition(m.Status, lifecycle.ConditionReachable); c != nil {
				if c.Status {
					reachable = "✓"
				} else {
					reachable = "✗"
				}
			}
			if c := lifecycle.GetCondition(m.Status, lifecycle.ConditionInCustomerCluster); c != nil {
				if c.Status {
					inCluster = "✓"
				} else {
					inCluster = "✗"
				}
			}
			if c := lifecycle.GetCondition(m.Status, lifecycle.ConditionNeedsIntervention); c != nil {
				if c.Status {
					needsHelp = "⚠"
				} else {
					needsHelp = "-"
				}
			}

			if m.Status.ActiveRunId != "" {
				activeRun = m.Status.ActiveRunId[:8] // Short ID
			}
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			m.MachineId, phase, effective, reachable, inCluster, needsHelp, activeRun)
	}
	w.Flush()
}

// ============================================================================
// repave/rma/reboot - Start run and watch until completion
// ============================================================================

func runAndWatch(conn *grpc.ClientConn, args []string, runType, planID string) {
	if len(args) < 1 {
		log.Fatalf("usage: %s <machine-id>", strings.ToLower(runType))
	}
	machineID := args[0]
	requestID := uuid.New().String()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	runClient := pb.NewRunServiceClient(conn)

	// Start the run
	run, err := runClient.StartRun(ctx, &pb.StartRunRequest{
		MachineId: machineID,
		RequestId: requestID,
		Type:      runType,
		PlanId:    planID,
	})
	if err != nil {
		log.Fatalf("StartRun failed: %v", err)
	}

	fmt.Printf("┌─────────────────────────────────────────────────────────────\n")
	fmt.Printf("│ Run: %s\n", run.RunId)
	fmt.Printf("│ Machine: %s\n", machineID)
	fmt.Printf("│ Type: %s | Plan: %s\n", runType, planID)
	fmt.Printf("│ Request ID: %s\n", requestID)
	fmt.Printf("└─────────────────────────────────────────────────────────────\n")
	fmt.Println()

	// Watch for events and logs concurrently
	var wg sync.WaitGroup
	done := make(chan struct{})

	// Event watcher
	wg.Add(1)
	go func() {
		defer wg.Done()
		watchRunEvents(ctx, runClient, run.RunId, machineID, done)
	}()

	// Log streamer
	wg.Add(1)
	go func() {
		defer wg.Done()
		streamRunLogs(ctx, runClient, run.RunId, done)
	}()

	// Poll for completion
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	lastPhase := run.Phase
	lastStep := ""

	for {
		select {
		case <-ctx.Done():
			fmt.Println("\n⚠ Interrupted. Run continues in background.")
			close(done)
			wg.Wait()
			return
		case <-ticker.C:
			r, err := runClient.GetRun(ctx, &pb.GetRunRequest{RunId: run.RunId})
			if err != nil {
				continue
			}

			// Print step transitions
			if r.CurrentStep != lastStep {
				if lastStep != "" {
					fmt.Printf("  ✓ %s\n", lastStep)
				}
				if r.CurrentStep != "" {
					fmt.Printf("→ %s...\n", r.CurrentStep)
				}
				lastStep = r.CurrentStep
			}

			// Print phase transitions
			if r.Phase != lastPhase {
				lastPhase = r.Phase
			}

			// Check for terminal state
			if lifecycle.IsTerminalRunPhase(r.Phase) {
				close(done)
				wg.Wait()

				fmt.Println()
				printRunResult(r)
				return
			}
		}
	}
}

func watchRunEvents(ctx context.Context, client pb.RunServiceClient, runID, machineID string, done <-chan struct{}) {
	stream, err := client.WatchRuns(ctx, &pb.WatchRunsRequest{
		Filter: fmt.Sprintf("machine_id=%s", machineID),
	})
	if err != nil {
		return
	}

	for {
		select {
		case <-done:
			return
		default:
			event, err := stream.Recv()
			if err != nil {
				return
			}
			if event.Snapshot != nil && event.Snapshot.RunId == runID {
				if event.Message != "" {
					ts := event.Ts.AsTime().Format("15:04:05")
					fmt.Printf("  [%s] %s\n", ts, event.Message)
				}
			}
		}
	}
}

func streamRunLogs(ctx context.Context, client pb.RunServiceClient, runID string, done <-chan struct{}) {
	stream, err := client.StreamRunLogs(ctx, &pb.StreamRunLogsRequest{RunId: runID})
	if err != nil {
		return
	}

	for {
		select {
		case <-done:
			return
		default:
			chunk, err := stream.Recv()
			if err != nil {
				return
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
}

func printRunResult(r *pb.Run) {
	fmt.Printf("┌─────────────────────────────────────────────────────────────\n")

	var icon string
	switch r.Phase {
	case pb.Run_SUCCEEDED:
		icon = "✓"
		fmt.Printf("│ %s SUCCEEDED\n", icon)
	case pb.Run_FAILED:
		icon = "✗"
		fmt.Printf("│ %s FAILED\n", icon)
	case pb.Run_CANCELED:
		icon = "○"
		fmt.Printf("│ %s CANCELED\n", icon)
	default:
		fmt.Printf("│ Phase: %s\n", r.Phase)
	}

	// Duration
	if r.StartedAt != nil && r.FinishedAt != nil {
		duration := r.FinishedAt.AsTime().Sub(r.StartedAt.AsTime())
		fmt.Printf("│ Duration: %s\n", duration.Round(time.Millisecond))
	}

	// Steps summary
	succeeded := 0
	failed := 0
	for _, s := range r.Steps {
		switch s.State {
		case pb.StepStatus_SUCCEEDED:
			succeeded++
		case pb.StepStatus_FAILED:
			failed++
		}
	}
	fmt.Printf("│ Steps: %d/%d completed\n", succeeded, len(r.Steps))

	// Error details
	if r.Error != nil {
		fmt.Printf("│\n")
		fmt.Printf("│ Error: %s\n", r.Error.Message)
		if r.Error.Code != "" {
			fmt.Printf("│ Code: %s\n", r.Error.Code)
		}
		if r.Error.Retryable {
			fmt.Printf("│ (retryable)\n")
		}
	}

	fmt.Printf("└─────────────────────────────────────────────────────────────\n")
}

// ============================================================================
// runs - List all runs
// ============================================================================

func listRuns(conn *grpc.ClientConn) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client := pb.NewRunServiceClient(conn)

	resp, err := client.ListRuns(ctx, &pb.ListRunsRequest{})
	if err != nil {
		log.Fatalf("ListRuns failed: %v", err)
	}

	if len(resp.Runs) == 0 {
		fmt.Println("No runs.")
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "RUN_ID\tMACHINE\tTYPE\tPHASE\tSTEP\tERROR")

	for _, r := range resp.Runs {
		step := r.CurrentStep
		if step == "" {
			step = "-"
		}

		errMsg := "-"
		if r.Error != nil && r.Error.Message != "" {
			errMsg = truncate(r.Error.Message, 30)
		}

		// Short run ID for readability
		runID := r.RunId
		if len(runID) > 8 {
			runID = runID[:8]
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			runID, r.MachineId, r.Type, r.Phase, step, errMsg)
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
// plans - List available plans
// ============================================================================

func listPlans(conn *grpc.ClientConn) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client := pb.NewPlanServiceClient(conn)

	resp, err := client.ListPlans(ctx, &pb.ListPlansRequest{})
	if err != nil {
		log.Fatalf("ListPlans failed: %v", err)
	}

	if len(resp.Plans) == 0 {
		fmt.Println("No plans available.")
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "PLAN_ID\tNAME\tSTEPS")

	for _, p := range resp.Plans {
		stepNames := make([]string, 0, len(p.Steps))
		for _, s := range p.Steps {
			stepNames = append(stepNames, s.Name)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", p.PlanId, p.DisplayName, strings.Join(stepNames, " → "))
	}
	w.Flush()
}

// ============================================================================
// watch - Watch run events
// ============================================================================

func watchRuns(conn *grpc.ClientConn, args []string) {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	client := pb.NewRunServiceClient(conn)

	filter := ""
	if len(args) > 0 {
		filter = fmt.Sprintf("machine_id=%s", args[0])
	}

	stream, err := client.WatchRuns(ctx, &pb.WatchRunsRequest{Filter: filter})
	if err != nil {
		log.Fatalf("WatchRuns failed: %v", err)
	}

	fmt.Println("Watching run events (Ctrl+C to stop)...")
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
			log.Fatalf("WatchRuns stream error: %v", err)
		}

		ts := event.Ts.AsTime().Format("15:04:05")
		snap := event.Snapshot

		runID := snap.RunId
		if len(runID) > 8 {
			runID = runID[:8]
		}

		step := snap.CurrentStep
		if step == "" {
			step = "-"
		}

		fmt.Printf("[%s] %s | %s | %s | step=%s | %s\n",
			ts, runID, snap.MachineId, snap.Phase, step, event.Message)
	}
}

// ============================================================================
// logs - Stream run logs
// ============================================================================

func streamLogs(conn *grpc.ClientConn, args []string) {
	if len(args) < 1 {
		log.Fatal("usage: logs <run-id>")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	client := pb.NewRunServiceClient(conn)

	stream, err := client.StreamRunLogs(ctx, &pb.StreamRunLogsRequest{RunId: args[0]})
	if err != nil {
		log.Fatalf("StreamRunLogs failed: %v", err)
	}

	fmt.Printf("Streaming logs for run %s (Ctrl+C to stop)...\n\n", args[0])

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
			log.Fatalf("StreamRunLogs stream error: %v", err)
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
	runClient := pb.NewRunServiceClient(conn)

	fmt.Println("# Baremetal Lifecycle Demo")
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
	demoListMachines(ctx, machineClient, runClient)

	// Step 2: Repave machine-1
	fmt.Println("## 2. Repave machine-1 (plan/repave-join)")
	fmt.Println("```")

	run, err := runClient.StartRun(ctx, &pb.StartRunRequest{
		MachineId: "machine-1",
		RequestId: uuid.New().String(),
		Type:      "REPAVE",
		PlanId:    "plan/repave-join",
	})
	if err != nil {
		log.Fatalf("StartRun failed: %v", err)
	}
	fmt.Printf("run_id=%s  machine=%s  type=%s  phase=%s\n", run.RunId[:8], run.MachineId, run.Type, run.Phase)

	// Wait for completion
	for {
		time.Sleep(200 * time.Millisecond)
		r, _ := runClient.GetRun(ctx, &pb.GetRunRequest{RunId: run.RunId})
		if r.CurrentStep != "" {
			fmt.Printf("  step: %s\n", r.CurrentStep)
		}
		if lifecycle.IsTerminalRunPhase(r.Phase) {
			fmt.Printf("result: %s\n", r.Phase)
			break
		}
	}
	fmt.Println("```")
	fmt.Println()

	// Show state after repave
	demoListMachines(ctx, machineClient, runClient)

	// Step 3: Mark machine-1 degraded
	fmt.Println("## 3. Set Degraded Condition on machine-1")
	fmt.Println("```")

	m1, _ := machineClient.GetMachine(ctx, &pb.GetMachineRequest{MachineId: "machine-1"})
	lifecycle.SetCondition(m1, "Degraded", true, "HighTemp", "GPU temp exceeds threshold")
	m1, _ = machineClient.UpdateMachine(ctx, &pb.UpdateMachineRequest{Machine: m1})

	fmt.Printf("machine-1 conditions:\n")
	for _, c := range m1.Status.Conditions {
		status := "false"
		if c.Status {
			status = "true"
		}
		fmt.Printf("  %s=%s (%s)\n", c.Type, status, c.Reason)
	}
	fmt.Println("```")
	fmt.Println()

	// Show state with degraded condition
	demoListMachines(ctx, machineClient, runClient)

	// Step 4: Move to MAINTENANCE and start upgrade
	fmt.Println("## 4. Move machine-1 to MAINTENANCE, Start Upgrade")
	fmt.Println("```")

	m1, _ = machineClient.GetMachine(ctx, &pb.GetMachineRequest{MachineId: "machine-1"})
	lifecycle.SetMachinePhase(m1, pb.MachineStatus_MAINTENANCE)
	m1, _ = machineClient.UpdateMachine(ctx, &pb.UpdateMachineRequest{Machine: m1})
	fmt.Printf("machine-1 phase: %s\n", m1.Status.Phase)

	// Start upgrade run (will use plan/upgrade if exists, otherwise stub)
	upgradeRun, err := runClient.StartRun(ctx, &pb.StartRunRequest{
		MachineId: "machine-1",
		RequestId: uuid.New().String(),
		Type:      "UPGRADE",
		PlanId:    "plan/upgrade",
	})
	if err != nil {
		fmt.Printf("upgrade run: (plan not found - expected for stub)\n")
	} else {
		fmt.Printf("upgrade run_id=%s  phase=%s\n", upgradeRun.RunId[:8], upgradeRun.Phase)
		// Wait for completion
		for {
			time.Sleep(200 * time.Millisecond)
			r, _ := runClient.GetRun(ctx, &pb.GetRunRequest{RunId: upgradeRun.RunId})
			if lifecycle.IsTerminalRunPhase(r.Phase) {
				fmt.Printf("upgrade result: %s\n", r.Phase)
				break
			}
		}
	}
	fmt.Println("```")
	fmt.Println()

	// Show state after maintenance/upgrade
	demoListMachines(ctx, machineClient, runClient)

	// Step 5: RMA machine-1
	fmt.Println("## 5. RMA machine-1")
	fmt.Println("```")

	// First clear MAINTENANCE phase to allow RMA
	m1, _ = machineClient.GetMachine(ctx, &pb.GetMachineRequest{MachineId: "machine-1"})
	lifecycle.SetMachinePhase(m1, pb.MachineStatus_READY)
	m1, _ = machineClient.UpdateMachine(ctx, &pb.UpdateMachineRequest{Machine: m1})

	rmaRun, err := runClient.StartRun(ctx, &pb.StartRunRequest{
		MachineId: "machine-1",
		RequestId: uuid.New().String(),
		Type:      "RMA",
		PlanId:    "plan/rma",
	})
	if err != nil {
		log.Fatalf("RMA run failed: %v", err)
	}
	fmt.Printf("rma run_id=%s  phase=%s\n", rmaRun.RunId[:8], rmaRun.Phase)

	// Wait for completion
	for {
		time.Sleep(200 * time.Millisecond)
		r, _ := runClient.GetRun(ctx, &pb.GetRunRequest{RunId: rmaRun.RunId})
		if r.CurrentStep != "" {
			fmt.Printf("  step: %s\n", r.CurrentStep)
		}
		if lifecycle.IsTerminalRunPhase(r.Phase) {
			fmt.Printf("result: %s\n", r.Phase)
			break
		}
	}
	fmt.Println("```")
	fmt.Println()

	// Final state
	fmt.Println("## Final State")
	demoListMachines(ctx, machineClient, runClient)

	fmt.Println("---")
	fmt.Println("*Demo complete. Machine-1 transitioned: FACTORY_READY → IN_SERVICE → MAINTENANCE → RMA*")
}

func demoListMachines(ctx context.Context, machineClient pb.MachineServiceClient, runClient pb.RunServiceClient) {
	fmt.Println("### Machine State")
	fmt.Println("```")

	resp, _ := machineClient.ListMachines(ctx, &pb.ListMachinesRequest{})
	runsResp, _ := runClient.ListRuns(ctx, &pb.ListRunsRequest{})

	activeRuns := make(map[string]*pb.Run)
	for _, r := range runsResp.GetRuns() {
		if lifecycle.IsActiveRunPhase(r.Phase) {
			activeRuns[r.MachineId] = r
		}
	}

	fmt.Printf("%-12s  %-14s  %-14s  %s\n", "MACHINE", "PHASE", "EFFECTIVE", "CONDITIONS")
	for _, m := range resp.Machines {
		if m.Status == nil {
			continue
		}
		ar := activeRuns[m.MachineId]
		effective := lifecycle.EffectiveState(m.Status, ar)

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

		fmt.Printf("%-12s  %-14s  %-14s  %s\n",
			m.MachineId, m.Status.Phase, effective, condStr)
	}
	fmt.Println("```")
	fmt.Println()
}
