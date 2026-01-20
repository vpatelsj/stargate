package providers

const (
	RoleRouter = "router"
	RoleWorker = "worker"
)

// NodeSpec describes a node to provision.
type NodeSpec struct {
	Name string
	Role string
}

// NodeInfo captures addresses for connectivity checks.
type NodeInfo struct {
	Name        string
	Role        string
	PublicIP    string
	PrivateIP   string
	TailnetFQDN string
	TailscaleIP string // Tailscale IPv4 address (router only in subnet mode)
}
