// Package main implements the bmdemo gRPC server for baremetal provisioning demo.
package main

import (
	"context"
	"errors"
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
	"google.golang.org/protobuf/proto"

	pb "github.com/vpatelsj/stargate/gen/baremetal/v1"
	"github.com/vpatelsj/stargate/internal/bmdemo/executor"
	"github.com/vpatelsj/stargate/internal/bmdemo/lifecycle"
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

	// Create a context for operation execution that's canceled on server shutdown.
	// This is separate from RPC request contexts which are short-lived.
	opCtx, opCancel := context.WithCancel(context.Background())

	// Register services
	machineService := &machineServer{
		store:  s,
		runner: runner,
		plans:  pl,
		logger: logger.With("service", "machine"),
		opCtx:  opCtx,
	}
	operationService := &operationServer{
		store:  s,
		runner: runner,
		logger: logger.With("service", "operation"),
	}

	pb.RegisterMachineServiceServer(grpcServer, machineService)
	pb.RegisterOperationServiceServer(grpcServer, operationService)

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
		opCancel() // Cancel the operation execution context
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
	runner *executor.Runner
	plans  *plans.Registry
	logger *slog.Logger
	opCtx  context.Context // Long-lived context for operation execution
}

// populateEffectiveState computes and sets the effective_state field on a machine.
// This looks up the active operation (if any) to determine the correct state.
func (s *machineServer) populateEffectiveState(m *pb.Machine) {
	if m == nil || m.Status == nil {
		return
	}

	var activeOp *pb.Operation
	if m.Status.ActiveOperationId != "" {
		activeOp, _ = s.store.GetOperation(m.Status.ActiveOperationId)
	}

	lifecycle.PopulateEffectiveState(m, activeOp)
}

// sanitizeOperation removes internal workflow engine fields from an Operation
// before returning it to external callers. This hides plan_id and steps.
func sanitizeOperation(op *pb.Operation) *pb.Operation {
	if op == nil {
		return nil
	}
	// Clone to avoid mutating stored data
	clone := proto.Clone(op).(*pb.Operation)
	clone.PlanId = ""
	clone.Steps = nil
	return clone
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

	// Populate effective_state before returning
	s.populateEffectiveState(m)

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

	// Populate effective_state before returning
	s.populateEffectiveState(m)

	return m, nil
}

func (s *machineServer) ListMachines(ctx context.Context, req *pb.ListMachinesRequest) (*pb.ListMachinesResponse, error) {
	machines := s.store.ListMachines()

	// Populate effective_state for each machine before returning
	for _, m := range machines {
		s.populateEffectiveState(m)
	}

	return &pb.ListMachinesResponse{Machines: machines}, nil
}

// UpdateMachine updates a machine's Spec and Labels.
// NOTE: Status changes from clients are ignored - status is owned by the backend/executor.
// This prevents clients from accidentally corrupting machine lifecycle state.
// The effective_state and phase fields in status are backend-owned and will be ignored.
func (s *machineServer) UpdateMachine(ctx context.Context, req *pb.UpdateMachineRequest) (*pb.Machine, error) {
	if req.Machine == nil {
		return nil, status.Error(codes.InvalidArgument, "machine is required")
	}

	// Get existing machine to preserve status
	existing, ok := s.store.GetMachine(req.Machine.MachineId)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "machine %q not found", req.Machine.MachineId)
	}

	// Only update Spec and Labels - ignore Status from client entirely
	// Status fields (phase, effective_state, conditions) are backend-owned
	if req.Machine.Spec != nil {
		existing.Spec = req.Machine.Spec
	}
	if req.Machine.Labels != nil {
		existing.Labels = req.Machine.Labels
	}

	m, err := s.store.UpdateMachine(existing)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}

	// Populate effective_state before returning
	s.populateEffectiveState(m)

	s.logger.Info("updated machine", "machine_id", m.MachineId)
	return m, nil
}

// RebootMachine starts a reboot operation on a machine.
// Can be called on machines in READY or MAINTENANCE phase.
func (s *machineServer) RebootMachine(ctx context.Context, req *pb.RebootMachineRequest) (*pb.Operation, error) {
	if req.MachineId == "" {
		return nil, status.Error(codes.InvalidArgument, "machine_id is required")
	}
	if req.RequestId == "" {
		return nil, status.Error(codes.InvalidArgument, "request_id is required for idempotency")
	}

	machine, ok := s.store.GetMachine(req.MachineId)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "machine %q not found", req.MachineId)
	}

	// Reboot allowed in READY or MAINTENANCE
	phase := machine.Status.GetPhase()
	if phase != pb.MachineStatus_READY && phase != pb.MachineStatus_MAINTENANCE {
		return nil, status.Errorf(codes.FailedPrecondition,
			"machine %q is in phase %s; reboot requires READY or MAINTENANCE", req.MachineId, phase)
	}

	return s.startOperation(req.MachineId, req.RequestId, pb.Operation_REBOOT, plans.PlanReboot, nil)
}

// ReimageMachine starts a reimage operation on a machine.
// Requires the machine to be in MAINTENANCE phase (safety gate).
func (s *machineServer) ReimageMachine(ctx context.Context, req *pb.ReimageMachineRequest) (*pb.Operation, error) {
	if req.MachineId == "" {
		return nil, status.Error(codes.InvalidArgument, "machine_id is required")
	}
	if req.RequestId == "" {
		return nil, status.Error(codes.InvalidArgument, "request_id is required for idempotency")
	}

	machine, ok := s.store.GetMachine(req.MachineId)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "machine %q not found", req.MachineId)
	}

	// Reimage REQUIRES MAINTENANCE phase (safety gate per design decision D)
	if machine.Status.GetPhase() != pb.MachineStatus_MAINTENANCE {
		return nil, status.Errorf(codes.FailedPrecondition,
			"machine %q is in phase %s; reimage requires MAINTENANCE", req.MachineId, machine.Status.GetPhase())
	}

	// Store image_ref in operation params
	params := make(map[string]string)
	imageRef := req.ImageRef
	if imageRef == "" {
		imageRef = "ubuntu-2204-lab" // Default lab image
	}
	params["image_ref"] = imageRef

	// Use default reimage plan (server-side plan selection per decision E)
	return s.startOperation(req.MachineId, req.RequestId, pb.Operation_REIMAGE, plans.PlanRepaveJoin, params)
}

// EnterMaintenance transitions a machine to MAINTENANCE phase.
func (s *machineServer) EnterMaintenance(ctx context.Context, req *pb.EnterMaintenanceRequest) (*pb.Operation, error) {
	if req.MachineId == "" {
		return nil, status.Error(codes.InvalidArgument, "machine_id is required")
	}
	if req.RequestId == "" {
		return nil, status.Error(codes.InvalidArgument, "request_id is required for idempotency")
	}

	_, ok := s.store.GetMachine(req.MachineId)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "machine %q not found", req.MachineId)
	}

	return s.startOperation(req.MachineId, req.RequestId, pb.Operation_ENTER_MAINTENANCE, "", nil)
}

// ExitMaintenance transitions a machine out of MAINTENANCE phase to READY.
func (s *machineServer) ExitMaintenance(ctx context.Context, req *pb.ExitMaintenanceRequest) (*pb.Operation, error) {
	if req.MachineId == "" {
		return nil, status.Error(codes.InvalidArgument, "machine_id is required")
	}
	if req.RequestId == "" {
		return nil, status.Error(codes.InvalidArgument, "request_id is required for idempotency")
	}

	machine, ok := s.store.GetMachine(req.MachineId)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "machine %q not found", req.MachineId)
	}

	// ExitMaintenance only makes sense if in MAINTENANCE
	if machine.Status.GetPhase() != pb.MachineStatus_MAINTENANCE {
		return nil, status.Errorf(codes.FailedPrecondition,
			"machine %q is in phase %s; exit-maintenance requires MAINTENANCE", req.MachineId, machine.Status.GetPhase())
	}

	return s.startOperation(req.MachineId, req.RequestId, pb.Operation_EXIT_MAINTENANCE, "", nil)
}

// CancelOperation cancels an in-progress operation.
func (s *machineServer) CancelOperation(ctx context.Context, req *pb.CancelOperationRequest) (*pb.Operation, error) {
	if req.OperationId == "" {
		return nil, status.Error(codes.InvalidArgument, "operation_id is required")
	}

	if err := s.runner.CancelOperation(req.OperationId); err != nil {
		errMsg := err.Error()
		if strings.Contains(errMsg, "not found") {
			return nil, status.Errorf(codes.NotFound, "operation %q not found", req.OperationId)
		}
		if strings.Contains(errMsg, "already finished") {
			return nil, status.Errorf(codes.FailedPrecondition, "%v", err)
		}
		return nil, status.Errorf(codes.Internal, "%v", err)
	}

	op, ok := s.store.GetOperation(req.OperationId)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "operation %q not found", req.OperationId)
	}

	s.logger.Info("cancelled operation", "operation_id", req.OperationId, "phase", op.Phase)
	return sanitizeOperation(op), nil
}

// startOperation is the shared helper that creates and starts an operation.
// Enforces idempotency scoped by (machine_id, request_id) and single active operation per machine.
func (s *machineServer) startOperation(machineID, requestID string, opType pb.Operation_OperationType, planID string, params map[string]string) (*pb.Operation, error) {
	// Create operation (idempotent) - this handles:
	// - Returning existing operation for same (machine_id, request_id)
	// - Rejecting if machine has a different active operation
	// - Creating new operation if no conflicts
	op, created, err := s.store.CreateOperationIfNotExists(requestID, machineID, opType, planID, params)
	if err != nil {
		// Map sentinel errors to gRPC codes
		if errors.Is(err, store.ErrMachineNotFound) {
			return nil, status.Errorf(codes.NotFound, "%v", err)
		}
		if errors.Is(err, store.ErrMachineHasActiveOperation) {
			return nil, status.Errorf(codes.FailedPrecondition, "%v", err)
		}
		s.logger.Error("failed to create operation",
			"machine_id", machineID,
			"request_id", requestID,
			"error", err)
		return nil, status.Errorf(codes.Internal, "%v", err)
	}

	if created {
		s.logger.Info("created operation",
			"operation_id", op.OperationId,
			"machine_id", machineID,
			"type", opType,
			"plan_id", planID,
			"request_id", requestID)

		// Start execution asynchronously using the long-lived opCtx
		// (not the RPC request ctx which is canceled when the RPC returns)
		if err := s.runner.StartOperation(s.opCtx, op.OperationId); err != nil {
			s.logger.Error("failed to start operation execution",
				"operation_id", op.OperationId,
				"error", err)
			// Operation is created but failed to start - it will stay PENDING
		}
	} else {
		s.logger.Debug("idempotent operation request",
			"operation_id", op.OperationId,
			"request_id", requestID)

		// If operation is still PENDING, try to start it (retry scenario)
		if op.Phase == pb.Operation_PENDING {
			s.logger.Info("retrying pending operation",
				"operation_id", op.OperationId,
				"request_id", requestID)
			if err := s.runner.StartOperation(s.opCtx, op.OperationId); err != nil {
				s.logger.Error("failed to retry operation execution",
					"operation_id", op.OperationId,
					"error", err)
			}
		}
	}

	// Sanitize before returning to caller (hide plan_id and steps)
	return sanitizeOperation(op), nil
}

// ============================================================================
// OperationService
// ============================================================================

type operationServer struct {
	pb.UnimplementedOperationServiceServer
	store  *store.Store
	runner *executor.Runner
	logger *slog.Logger
}

func (s *operationServer) GetOperation(ctx context.Context, req *pb.GetOperationRequest) (*pb.Operation, error) {
	op, ok := s.store.GetOperation(req.OperationId)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "operation %q not found", req.OperationId)
	}
	return sanitizeOperation(op), nil
}

func (s *operationServer) ListOperations(ctx context.Context, req *pb.ListOperationsRequest) (*pb.ListOperationsResponse, error) {
	ops := s.store.ListOperations()

	// Apply filter if provided (simple machine_id filter)
	if req.Filter != "" {
		var filtered []*pb.Operation
		for _, op := range ops {
			// Basic filter matching
			if req.Filter == fmt.Sprintf("machine_id=%s", op.MachineId) {
				filtered = append(filtered, op)
			}
		}
		ops = filtered
	}

	// Sanitize all operations before returning
	sanitized := make([]*pb.Operation, len(ops))
	for i, op := range ops {
		sanitized[i] = sanitizeOperation(op)
	}

	return &pb.ListOperationsResponse{Operations: sanitized}, nil
}

func (s *operationServer) WatchOperations(req *pb.WatchOperationsRequest, stream pb.OperationService_WatchOperationsServer) error {
	s.logger.Debug("watch operations started", "filter", req.Filter)

	// Create event channel
	eventCh := make(chan *pb.OperationEvent, 100)

	// Subscribe to events
	unsubscribe := s.runner.SubscribeEvents(func(event *pb.OperationEvent) {
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

		// Sanitize event snapshot before sending
		if event.Snapshot != nil {
			event.Snapshot = sanitizeOperation(event.Snapshot)
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
				s.logger.Debug("watch operations stream error", "error", err)
				return err
			}
		case <-stream.Context().Done():
			s.logger.Debug("watch operations client disconnected")
			return nil
		}
	}
}

func (s *operationServer) StreamOperationLogs(req *pb.StreamOperationLogsRequest, stream pb.OperationService_StreamOperationLogsServer) error {
	s.logger.Debug("stream logs started", "operation_id", req.OperationId)

	// Verify operation exists
	if _, ok := s.store.GetOperation(req.OperationId); !ok {
		return status.Errorf(codes.NotFound, "operation %q not found", req.OperationId)
	}

	// Create log channel
	logCh := make(chan *pb.LogChunk, 100)

	// Subscribe to logs for this operation
	unsubscribe := s.runner.SubscribeLogs(req.OperationId, func(chunk *pb.LogChunk) {
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
				s.logger.Debug("log stream error", "operation_id", req.OperationId, "error", err)
				return err
			}
		case <-stream.Context().Done():
			s.logger.Debug("log stream client disconnected", "operation_id", req.OperationId)
			return nil
		}
	}
}
