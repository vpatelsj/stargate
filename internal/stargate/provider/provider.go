// Package provider defines the interface for baremetal provisioning operations.
package provider

import (
	"context"
	"time"

	pb "github.com/vpatelsj/stargate/gen/baremetal/v1"
)

// LogCallback is called to stream logs from provider operations.
// stream is "stdout" or "stderr", data is the log content.
type LogCallback func(runID, stream string, data []byte)

// JoinMaterial contains the information needed to join a node to a cluster.
type JoinMaterial struct {
	Endpoint    string
	Token       string
	CAHash      string
	ExpiresAt   time.Time
	ClusterID   string
	Certificate []byte // optional client cert
}

// Provider defines the interface for baremetal provisioning operations.
// Implementations include fake.Provider for demos and testing.
type Provider interface {
	// SetNetboot sets the netboot profile on a machine.
	SetNetboot(ctx context.Context, runID string, machine *pb.Machine, profile string) error

	// Reboot reboots a machine. If force is true, uses hard reboot.
	Reboot(ctx context.Context, runID string, machine *pb.Machine, force bool) error

	// Repave reprovisions a machine with a new image.
	Repave(ctx context.Context, runID string, machine *pb.Machine, imageRef, cloudInitRef string) error

	// MintJoinMaterial generates join material for adding a node to a cluster.
	MintJoinMaterial(ctx context.Context, runID string, targetCluster *pb.TargetClusterRef) (*JoinMaterial, error)

	// JoinNode joins a node to a Kubernetes cluster using the provided join material.
	JoinNode(ctx context.Context, runID string, machine *pb.Machine, material *JoinMaterial) error

	// VerifyInCluster verifies that a machine is properly joined to a cluster.
	VerifyInCluster(ctx context.Context, runID string, machine *pb.Machine, targetCluster *pb.TargetClusterRef) error

	// RMA initiates the RMA (Return Merchandise Authorization) process for a machine.
	RMA(ctx context.Context, runID string, machine *pb.Machine, reason string) error

	// ExecuteSSHCommand runs a script on a machine via SSH.
	ExecuteSSHCommand(ctx context.Context, runID string, machine *pb.Machine, scriptRef string, args map[string]string) error
}
