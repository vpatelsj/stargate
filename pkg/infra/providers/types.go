package providers

// NodeSpec describes a node to provision.
type NodeSpec struct {
	Name string
}

// NodeInfo captures addresses for connectivity checks.
type NodeInfo struct {
	Name        string
	PublicIP    string
	PrivateIP   string
	TailnetFQDN string
	TailscaleIP string // Tailscale IPv4 address for mesh connectivity
}
