package providers

const (
	RoleRouter    = "router"
	RoleWorker    = "worker"
	RoleAKSRouter = "aks-router"
)

// NodeSpec describes a node to provision.
type NodeSpec struct {
	Name string
	Role string
}

// AKSRouterConfig holds configuration for provisioning a router in the AKS VNet.
type AKSRouterConfig struct {
	Name          string   // VM name for the AKS router
	ResourceGroup string   // Resource group containing the AKS VNet (usually MC_*)
	VNetName      string   // Existing AKS VNet name
	SubnetName    string   // Subnet name in AKS VNet
	SubnetCIDR    string   // CIDR for the subnet (for advertising to Tailscale)
	VNetCIDR      string   // Full VNet CIDR to advertise to Tailscale
	ClusterName   string   // AKS cluster name (for auto-detecting CIDRs)
	ClusterRG     string   // AKS cluster resource group
	RouteCIDRs    []string // All CIDRs to advertise (VNet, Pod, Service)
	APIServerFQDN string   // AKS API server FQDN for the proxy
}

// NodeInfo captures addresses for connectivity checks.
type NodeInfo struct {
	Name        string
	Role        string
	PublicIP    string
	PrivateIP   string
	TailnetFQDN string
	TailscaleIP string // Tailscale IPv4 address (router only in subnet mode)
	RouterIP    string // Private IP of the router for workers behind it
	PodCIDR     string // Expected pod CIDR for this node (derived from private IP)
}
