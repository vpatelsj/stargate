// Package baremetal provides a gRPC client for the baremetal API.
package baremetal

import (
	"context"
	"fmt"
	"io"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/vpatelsj/stargate/gen/baremetal/v1"
)

// Client wraps the gRPC clients for MachineService and OperationService.
type Client struct {
	conn       *grpc.ClientConn
	Machines   pb.MachineServiceClient
	Operations pb.OperationServiceClient
}

// NewClient creates a new baremetal API client connected to the given address.
func NewClient(ctx context.Context, addr string, opts ...grpc.DialOption) (*Client, error) {
	if len(opts) == 0 {
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	conn, err := grpc.NewClient(addr, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to %s: %w", addr, err)
	}

	return &Client{
		conn:       conn,
		Machines:   pb.NewMachineServiceClient(conn),
		Operations: pb.NewOperationServiceClient(conn),
	}, nil
}

// Close closes the underlying connection.
func (c *Client) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

// RegisterMachine registers a new machine with the given spec.
func (c *Client) RegisterMachine(ctx context.Context, machine *pb.Machine) (*pb.Machine, error) {
	return c.Machines.RegisterMachine(ctx, &pb.RegisterMachineRequest{Machine: machine})
}

// GetMachine retrieves a machine by ID.
func (c *Client) GetMachine(ctx context.Context, machineID string) (*pb.Machine, error) {
	return c.Machines.GetMachine(ctx, &pb.GetMachineRequest{MachineId: machineID})
}

// ListMachines lists all machines.
func (c *Client) ListMachines(ctx context.Context) ([]*pb.Machine, error) {
	resp, err := c.Machines.ListMachines(ctx, &pb.ListMachinesRequest{})
	if err != nil {
		return nil, err
	}
	return resp.Machines, nil
}

// RebootMachine starts a reboot operation on a machine.
func (c *Client) RebootMachine(ctx context.Context, machineID, requestID string, force bool) (*pb.Operation, error) {
	return c.Machines.RebootMachine(ctx, &pb.RebootMachineRequest{
		MachineId: machineID,
		RequestId: requestID,
		Force:     force,
	})
}

// ReimageMachine starts a reimage operation on a machine.
func (c *Client) ReimageMachine(ctx context.Context, machineID, requestID, imageRef string) (*pb.Operation, error) {
	return c.Machines.ReimageMachine(ctx, &pb.ReimageMachineRequest{
		MachineId: machineID,
		RequestId: requestID,
		ImageRef:  imageRef,
	})
}

// EnterMaintenance puts a machine into maintenance mode.
func (c *Client) EnterMaintenance(ctx context.Context, machineID, requestID string) (*pb.Operation, error) {
	return c.Machines.EnterMaintenance(ctx, &pb.EnterMaintenanceRequest{
		MachineId: machineID,
		RequestId: requestID,
	})
}

// ExitMaintenance takes a machine out of maintenance mode.
func (c *Client) ExitMaintenance(ctx context.Context, machineID, requestID string) (*pb.Operation, error) {
	return c.Machines.ExitMaintenance(ctx, &pb.ExitMaintenanceRequest{
		MachineId: machineID,
		RequestId: requestID,
	})
}

// CancelOperation cancels an in-progress operation.
func (c *Client) CancelOperation(ctx context.Context, operationID string) (*pb.Operation, error) {
	return c.Machines.CancelOperation(ctx, &pb.CancelOperationRequest{
		OperationId: operationID,
	})
}

// GetOperation retrieves an operation by ID.
func (c *Client) GetOperation(ctx context.Context, operationID string) (*pb.Operation, error) {
	return c.Operations.GetOperation(ctx, &pb.GetOperationRequest{OperationId: operationID})
}

// ListOperations lists all operations.
func (c *Client) ListOperations(ctx context.Context) ([]*pb.Operation, error) {
	resp, err := c.Operations.ListOperations(ctx, &pb.ListOperationsRequest{})
	if err != nil {
		return nil, err
	}
	return resp.Operations, nil
}

// WaitForOperation polls until an operation reaches a terminal state.
func (c *Client) WaitForOperation(ctx context.Context, operationID string, pollInterval time.Duration) (*pb.Operation, error) {
	if pollInterval == 0 {
		pollInterval = 2 * time.Second
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			op, err := c.GetOperation(ctx, operationID)
			if err != nil {
				return nil, err
			}

			switch op.Phase {
			case pb.Operation_SUCCEEDED, pb.Operation_FAILED, pb.Operation_CANCELED:
				return op, nil
			}
		}
	}
}

// WatchOperations streams operation events matching the filter.
func (c *Client) WatchOperations(ctx context.Context, filter string) (<-chan *pb.OperationEvent, <-chan error) {
	events := make(chan *pb.OperationEvent)
	errs := make(chan error, 1)

	go func() {
		defer close(events)
		defer close(errs)

		stream, err := c.Operations.WatchOperations(ctx, &pb.WatchOperationsRequest{Filter: filter})
		if err != nil {
			errs <- err
			return
		}

		for {
			event, err := stream.Recv()
			if err == io.EOF {
				return
			}
			if err != nil {
				errs <- err
				return
			}

			select {
			case events <- event:
			case <-ctx.Done():
				return
			}
		}
	}()

	return events, errs
}

// StreamOperationLogs streams logs for a specific operation.
func (c *Client) StreamOperationLogs(ctx context.Context, operationID string) (<-chan *pb.LogChunk, <-chan error) {
	logs := make(chan *pb.LogChunk)
	errs := make(chan error, 1)

	go func() {
		defer close(logs)
		defer close(errs)

		stream, err := c.Operations.StreamOperationLogs(ctx, &pb.StreamOperationLogsRequest{OperationId: operationID})
		if err != nil {
			errs <- err
			return
		}

		for {
			chunk, err := stream.Recv()
			if err == io.EOF {
				return
			}
			if err != nil {
				errs <- err
				return
			}

			select {
			case logs <- chunk:
			case <-ctx.Done():
				return
			}
		}
	}()

	return logs, errs
}

// GetMachineByMAC finds a machine by its MAC address.
func (c *Client) GetMachineByMAC(ctx context.Context, mac string) (*pb.Machine, error) {
	machines, err := c.ListMachines(ctx)
	if err != nil {
		return nil, err
	}

	for _, m := range machines {
		for _, addr := range m.Spec.GetMacAddresses() {
			if addr == mac {
				return m, nil
			}
		}
	}

	return nil, fmt.Errorf("machine with MAC %s not found", mac)
}

// GetMachinesByProvider filters machines by provider.
func (c *Client) GetMachinesByProvider(ctx context.Context, provider string) ([]*pb.Machine, error) {
	machines, err := c.ListMachines(ctx)
	if err != nil {
		return nil, err
	}

	var result []*pb.Machine
	for _, m := range machines {
		if m.Spec.GetProvider() == provider {
			result = append(result, m)
		}
	}

	return result, nil
}
