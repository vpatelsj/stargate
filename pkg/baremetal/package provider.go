package provider

import (
	"context"
	"io"
	"time"
)

type Machine struct {
	ID          string
	Provider    string
	Macs        []string
	SSHEndpoint string
	BMCAddress  string
}

type TargetClusterRef struct {
	ClusterID           string
	KubeconfigSecretRef string
}

type JoinMaterial struct {
	Endpoint  string
	Token     string
	CAHash    string
	ExpiresAt time.Time
}

type RemoteExec interface {
	Run(ctx context.Context, m Machine, script string, args map[string]string) (stdout io.ReadCloser, stderr io.ReadCloser, err error)
}

type Power interface {
	Reboot(ctx context.Context, m Machine, force bool) error
	SetNetboot(ctx context.Context, m Machine, profile string) error
}

type Imaging interface {
	Repave(ctx context.Context, m Machine, imageRef string, cloudInitRef string) error
}

type ClusterJoin interface {
	MintJoinMaterial(ctx context.Context, target TargetClusterRef) (JoinMaterial, error)
	JoinNode(ctx context.Context, m Machine, jm JoinMaterial) error
	VerifyInCluster(ctx context.Context, m Machine, target TargetClusterRef) error
}

type Inspector interface {
	// Optional: discovery/refresh facts
	Inspect(ctx context.Context, m Machine) (map[string]string, error)
}

type Provider interface {
	RemoteExec() RemoteExec
	Power() Power
	Imaging() Imaging
	ClusterJoin() ClusterJoin
	Inspector() Inspector
}
