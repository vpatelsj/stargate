// Package main implements the bmdemo gRPC server for baremetal provisioning demo.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"

	pb "github.com/vpatelsj/stargate/gen/baremetal/v1"
	"github.com/vpatelsj/stargate/internal/bmdemo/executor"
	"github.com/vpatelsj/stargate/internal/bmdemo/plans"
	"github.com/vpatelsj/stargate/internal/bmdemo/provider/fake"
	"github.com/vpatelsj/stargate/internal/bmdemo/store"
)

var (
	port     = flag.Int("port", 50051, "The server port")
	slowMode = flag.Bool("slow", false, "Use slow timing for demos")
	logLevel = flag.String("log-level", "info", "Log level (debug, info, warn, error)")
)

func main() {
	flag.Parse()

	// Setup structured logging
	var level slog.Level
	switch *logLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	// Initialize components
	s := store.New()
	pl := plans.NewRegistry()

	// Configure provider timing
	var providerCfg *fake.Config
	if *slowMode {
		providerCfg = fake.SlowConfig()
		slog.Info("using slow demo timing")
	} else {
		providerCfg = fake.DefaultConfig()
	}

	// Create runner first (provider needs its EmitLog method)
	runner := executor.NewRunner(s, nil, pl)

	// Create provider with log callback that forwards to runner
	provider := fake.New(providerCfg, runner.EmitLog)

	// Set the provider on the runner
	runner.SetProvider(provider)

	// Create gRPC server
	grpcServer := grpc.NewServer()

	// Register services
	machineService := &machineServer{store: s, logger: logger.With("service", "machine")}
	planService := &planServer{plans: pl, logger: logger.With("service", "plan")}
	runService := &runServer{
		store:  s,
		runner: runner,
		plans:  pl,
		logger: logger.With("service", "run"),
	}

	pb.RegisterMachineServiceServer(grpcServer, machineService)
	pb.RegisterPlanServiceServer(grpcServer, planService)
	pb.RegisterRunServiceServer(grpcServer, runService)

	// Enable reflection for grpcurl
	reflection.Register(grpcServer)

	// Start listener
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		slog.Error("failed to listen", "error", err)
		os.Exit(1)
	}

	// Graceful shutdown
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		slog.Info("shutting down server")
		runner.Shutdown()
		grpcServer.GracefulStop()
	}()

	slog.Info("bmdemo-server starting", "port", *port)
	if err := grpcServer.Serve(lis); err != nil {
		slog.Error("failed to serve", "error", err)
		os.Exit(1)
	}
}

// ============================================================================
// MachineService
// ============================================================================

type machineServer struct {
	pb.UnimplementedMachineServiceServer
	store  *store.Store
	logger *slog.Logger
}

func (s *machineServer) RegisterMachine(ctx context.Context, req *pb.RegisterMachineRequest) (*pb.Machine, error) {
	if req.Machine == nil {
		return nil, status.Error(codes.InvalidArgument, "machine is required")
	}

	m, err := s.store.UpsertMachine(req.Machine)
	if err != nil {
		s.logger.Error("failed to register machine", "error", err)
		return nil, status.Errorf(codes.Internal, "failed to register: %v", err)
	}

	s.logger.Info("registered machine",
		"machine_id", m.MachineId,
		"endpoint", m.Spec.GetSshEndpoint())
	return m, nil
}

func (s *machineServer) GetMachine(ctx context.Context, req *pb.GetMachineRequest) (*pb.Machine, error) {
	m, ok := s.store.GetMachine(req.MachineId)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "machine %q not found", req.MachineId)
	}
	return m, nil
}

func (s *machineServer) ListMachines(ctx context.Context, req *pb.ListMachinesRequest) (*pb.ListMachinesResponse, error) {
	machines := s.store.ListMachines()
	return &pb.ListMachinesResponse{Machines: machines}, nil
}

func (s *machineServer) UpdateMachine(ctx context.Context, req *pb.UpdateMachineRequest) (*pb.Machine, error) {
	if req.Machine == nil {
		return nil, status.Error(codes.InvalidArgument, "machine is required")
	}

	m, err := s.store.UpdateMachine(req.Machine)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "%v", err)
	}

	s.logger.Info("updated machine", "machine_id", m.MachineId)
	return m, nil
}

// ============================================================================
// PlanService
// ============================================================================

type planServer struct {
	pb.UnimplementedPlanServiceServer
	plans  *plans.Registry
	logger *slog.Logger
}

func (s *planServer) GetPlan(ctx context.Context, req *pb.GetPlanRequest) (*pb.Plan, error) {
	plan, ok := s.plans.GetPlan(req.PlanId)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "plan %q not found", req.PlanId)
	}
	return plan, nil
}

func (s *planServer) ListPlans(ctx context.Context, req *pb.ListPlansRequest) (*pb.ListPlansResponse, error) {
	planList := s.plans.ListPlans()
	return &pb.ListPlansResponse{Plans: planList}, nil
}

// ============================================================================
// RunService
// ============================================================================

type runServer struct {
	pb.UnimplementedRunServiceServer
	store  *store.Store
	runner *executor.Runner
	plans  *plans.Registry
	logger *slog.Logger
}

func (s *runServer) StartRun(ctx context.Context, req *pb.StartRunRequest) (*pb.Run, error) {
	// Validate request_id for idempotency (must be first)
	if req.RequestId == "" {
		return nil, status.Error(codes.InvalidArgument, "request_id is required for idempotency")
	}

	// Determine run type and plan_id
	runType := req.Type
	planID := req.PlanId

	// Validate we have something to execute
	if runType == "" && planID == "" {
		return nil, status.Error(codes.InvalidArgument, "type or plan_id is required")
	}

	// If plan_id not provided, resolve default plan from type
	if planID == "" && runType != "" {
		planID = s.defaultPlanForType(runType)
	}

	// Validate plan exists if plan_id provided
	if planID != "" {
		if _, ok := s.plans.GetPlan(planID); !ok {
			return nil, status.Errorf(codes.NotFound, "plan %q not found", planID)
		}
	}

	// Create run (idempotent) - this handles:
	// - Returning existing run for same (machine_id, request_id)
	// - Rejecting if machine has a different active run
	// - Creating new run if no conflicts
	run, created, err := s.store.CreateRunIfNotExists(req.RequestId, req.MachineId, runType, planID)
	if err != nil {
		errMsg := err.Error()
		// Map specific errors to gRPC codes
		if contains(errMsg, "not found") {
			return nil, status.Errorf(codes.NotFound, "%v", err)
		}
		if contains(errMsg, "already has active run") {
			return nil, status.Errorf(codes.FailedPrecondition, "%v", err)
		}
		s.logger.Error("failed to create run",
			"machine_id", req.MachineId,
			"request_id", req.RequestId,
			"error", err)
		return nil, status.Errorf(codes.Internal, "%v", err)
	}

	if created {
		s.logger.Info("created run",
			"run_id", run.RunId,
			"machine_id", req.MachineId,
			"type", runType,
			"plan_id", planID,
			"request_id", req.RequestId)

		// Start execution asynchronously
		if err := s.runner.StartRun(ctx, run.RunId); err != nil {
			s.logger.Error("failed to start run execution",
				"run_id", run.RunId,
				"error", err)
			// Run is created but failed to start - it will stay PENDING
		}
	} else {
		s.logger.Debug("idempotent run request",
			"run_id", run.RunId,
			"request_id", req.RequestId)

		// If run is still PENDING, try to start it (retry scenario)
		if run.Phase == pb.Run_PENDING {
			s.logger.Info("retrying pending run",
				"run_id", run.RunId,
				"request_id", req.RequestId)
			if err := s.runner.StartRun(ctx, run.RunId); err != nil {
				s.logger.Error("failed to retry run execution",
					"run_id", run.RunId,
					"error", err)
			}
		}
	}

	return run, nil
}

func (s *runServer) GetRun(ctx context.Context, req *pb.GetRunRequest) (*pb.Run, error) {
	run, ok := s.store.GetRun(req.RunId)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "run %q not found", req.RunId)
	}
	return run, nil
}

func (s *runServer) ListRuns(ctx context.Context, req *pb.ListRunsRequest) (*pb.ListRunsResponse, error) {
	runs := s.store.ListRuns()

	// Apply filter if provided (simple machine_id filter)
	if req.Filter != "" {
		// Simple filter: "machine_id=xxx"
		var filtered []*pb.Run
		for _, r := range runs {
			// Basic filter matching
			if req.Filter == fmt.Sprintf("machine_id=%s", r.MachineId) {
				filtered = append(filtered, r)
			}
		}
		runs = filtered
	}

	return &pb.ListRunsResponse{Runs: runs}, nil
}

func (s *runServer) CancelRun(ctx context.Context, req *pb.CancelRunRequest) (*pb.Run, error) {
	// Cancel via runner (handles store update, machine state, and active execution)
	if err := s.runner.CancelRun(req.RunId); err != nil {
		errMsg := err.Error()
		if contains(errMsg, "not found") {
			return nil, status.Errorf(codes.NotFound, "run %q not found", req.RunId)
		}
		if contains(errMsg, "already finished") {
			return nil, status.Errorf(codes.FailedPrecondition, "%v", err)
		}
		return nil, status.Errorf(codes.Internal, "%v", err)
	}

	run, ok := s.store.GetRun(req.RunId)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "run %q not found", req.RunId)
	}

	s.logger.Info("cancelled run", "run_id", req.RunId, "phase", run.Phase)
	return run, nil
}

// defaultPlanForType returns the default plan_id for a given run type.
func (s *runServer) defaultPlanForType(runType string) string {
	switch runType {
	case "REPAVE", "repave":
		return plans.PlanRepaveJoin
	case "RMA", "rma":
		return plans.PlanRMA
	case "REBOOT", "reboot":
		return plans.PlanReboot
	case "UPGRADE", "upgrade":
		return plans.PlanUpgrade
	case "NET_RECONFIG", "net-reconfig":
		return plans.PlanNetReconfig
	default:
		return plans.PlanReboot // fallback
	}
}

func (s *runServer) WatchRuns(req *pb.WatchRunsRequest, stream pb.RunService_WatchRunsServer) error {
	s.logger.Debug("watch runs started", "filter", req.Filter)

	// Create event channel
	eventCh := make(chan *pb.RunEvent, 100)

	// Subscribe to events
	unsubscribe := s.runner.SubscribeEvents(func(event *pb.RunEvent) {
		// Apply filter if provided
		if req.Filter != "" {
			// Simple filter: "machine_id=xxx"
			if event.Snapshot != nil {
				expectedFilter := fmt.Sprintf("machine_id=%s", event.Snapshot.MachineId)
				if req.Filter != expectedFilter {
					return
				}
			}
		}

		select {
		case eventCh <- event:
		default:
			// Channel full, drop event
			s.logger.Warn("event channel full, dropping event")
		}
	})
	defer unsubscribe()

	// Stream events
	for {
		select {
		case event := <-eventCh:
			if err := stream.Send(event); err != nil {
				s.logger.Debug("watch runs stream error", "error", err)
				return err
			}
		case <-stream.Context().Done():
			s.logger.Debug("watch runs client disconnected")
			return nil
		}
	}
}

func (s *runServer) StreamRunLogs(req *pb.StreamRunLogsRequest, stream pb.RunService_StreamRunLogsServer) error {
	s.logger.Debug("stream logs started", "run_id", req.RunId)

	// Verify run exists
	if _, ok := s.store.GetRun(req.RunId); !ok {
		return status.Errorf(codes.NotFound, "run %q not found", req.RunId)
	}

	// Create log channel
	logCh := make(chan *pb.LogChunk, 100)

	// Subscribe to logs for this run
	unsubscribe := s.runner.SubscribeLogs(req.RunId, func(chunk *pb.LogChunk) {
		select {
		case logCh <- chunk:
		default:
			// Channel full, drop log
		}
	})
	defer unsubscribe()

	// Stream logs
	for {
		select {
		case chunk := <-logCh:
			if err := stream.Send(chunk); err != nil {
				s.logger.Debug("log stream error", "run_id", req.RunId, "error", err)
				return err
			}
		case <-stream.Context().Done():
			s.logger.Debug("log stream client disconnected", "run_id", req.RunId)
			return nil
		}
	}
}

// contains checks if s contains substr (helper for error matching).
func contains(s, substr string) bool {
	return strings.Contains(s, substr)
}
