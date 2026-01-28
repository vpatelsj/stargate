package controller

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	api "github.com/vpatelsj/stargate/api/v1alpha1"
)

// OperationReconciler reconciles an Operation object
type OperationReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Bootstrap configuration (defaults, can be overridden by ProvisioningProfile)
	KindContainerName       string
	ControlPlaneTailscaleIP string
	ControlPlaneHostname    string
	ControlPlaneMode        string // "kind", "tailscale", or "aks"
	ControlPlaneSSHUser     string // SSH user for tailscale mode
	SSHPrivateKeyPath       string // Default SSH key path
	SSHPort                 int
	AdminUsername           string // Default admin username

	// AKS configuration (for aks mode)
	AKSAPIServer          string // AKS API server URL (auto-detected from kubeconfig if empty)
	AKSClusterName        string // Cluster name for node labels
	AKSResourceGroup      string // Cluster resource group for node labels
	AKSClusterDNS         string // Cluster DNS IP (default 10.0.0.10)
	AKSSubscriptionID     string // Azure subscription ID for provider-id
	AKSVMResourceGroup    string // Resource group containing the worker VMs
	AKSAPIServerPrivateIP string // Private IP to use instead of public FQDN (via Tailscale mesh)

	// Routing configuration for hybrid connectivity
	DCRouterTailscaleIP  string // Tailscale IP of the DC router (for route updates)
	AKSRouterTailscaleIP string // Tailscale IP of the AKS router (for route updates)
	AzureRouteTableName  string // Azure route table name for pod CIDR routes
	AzureVNetName        string // Azure VNet name containing the subnets
	AzureSubnetName      string // Azure subnet name where AKS nodes reside

	// Runtime fields (populated automatically)
	Clientset    *kubernetes.Clientset // For creating SA tokens
	CACertBase64 string                // Fetched from rest config
	RestConfig   interface{}           // Store rest.Config for CA cert extraction
}

// bootstrapConfig holds resolved configuration for a bootstrap operation
type bootstrapConfig struct {
	kubernetesVersion string
	adminUsername     string
	sshPrivateKey     string // The actual key content or path
	sshPrivateKeyPath string // Temp file path if from secret
	sshPort           int
}

// +kubebuilder:rbac:groups=stargate.io,resources=operations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=stargate.io,resources=operations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=stargate.io,resources=operations/finalizers,verbs=update
// +kubebuilder:rbac:groups=stargate.io,resources=servers,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=stargate.io,resources=servers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=stargate.io,resources=provisioningprofiles,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// Reconcile handles Operation reconciliation
func (r *OperationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the Operation
	var operation api.Operation
	if err := r.Get(ctx, req.NamespacedName, &operation); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Skip if already completed
	if operation.Status.Phase == api.OperationPhaseSucceeded || operation.Status.Phase == api.OperationPhaseFailed {
		return ctrl.Result{}, nil
	}

	// Fetch referenced Server
	var server api.Server
	serverKey := client.ObjectKey{
		Namespace: operation.Namespace,
		Name:      operation.Spec.ServerRef.Name,
	}
	if err := r.Get(ctx, serverKey, &server); err != nil {
		logger.Error(err, "Failed to get Server", "server", operation.Spec.ServerRef.Name)
		return r.updateOperationStatus(ctx, &operation, api.OperationPhaseFailed, fmt.Sprintf("Server not found: %v", err))
	}

	// Provider gating: only handle azure servers
	if server.Spec.Provider != "" && server.Spec.Provider != "azure" {
		logger.Info("Skipping server with non-azure provider", "server", server.Name, "provider", server.Spec.Provider)
		return ctrl.Result{}, nil
	}

	// Fetch referenced ProvisioningProfile
	var profile api.ProvisioningProfile
	profileKey := client.ObjectKey{
		Namespace: operation.Namespace,
		Name:      operation.Spec.ProvisioningProfileRef.Name,
	}
	if err := r.Get(ctx, profileKey, &profile); err != nil {
		logger.Error(err, "Failed to get ProvisioningProfile", "provisioningProfile", operation.Spec.ProvisioningProfileRef.Name)
		return r.updateOperationStatus(ctx, &operation, api.OperationPhaseFailed, fmt.Sprintf("ProvisioningProfile not found: %v", err))
	}

	// Handle based on current phase
	switch operation.Status.Phase {
	case "", api.OperationPhasePending:
		return r.handlePending(ctx, &operation, &server, &profile)
	case api.OperationPhaseRunning:
		return r.handleRunning(ctx, &operation, &server, &profile)
	default:
		return ctrl.Result{}, nil
	}
}

// handlePending initiates the repave operation
func (r *OperationReconciler) handlePending(ctx context.Context, operation *api.Operation, server *api.Server, profile *api.ProvisioningProfile) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Initiating repave via SSH bootstrap", "server", server.Name, "ipv4", server.Spec.IPv4, "k8sVersion", profile.Spec.KubernetesVersion)

	// Resolve bootstrap configuration from profile and secrets
	cfg, cleanup, err := r.resolveBootstrapConfig(ctx, operation.Namespace, profile)
	if err != nil {
		logger.Error(err, "Failed to resolve bootstrap config")
		return r.updateOperationStatus(ctx, operation, api.OperationPhaseFailed, fmt.Sprintf("Failed to resolve config: %v", err))
	}
	defer cleanup()

	// Update server status to provisioning
	server.Status.State = "provisioning"
	server.Status.Message = fmt.Sprintf("Repave initiated by operation %s", operation.Name)
	server.Status.LastUpdated = metav1.Now()
	if err := r.Status().Update(ctx, server); err != nil {
		logger.Error(err, "Failed to update Server status")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, err
	}

	// Update operation status to running
	operation.Status.Phase = api.OperationPhaseRunning
	now := metav1.Now()
	operation.Status.StartTime = &now
	operation.Status.Message = "Bootstrap in progress"

	if err := r.Status().Update(ctx, operation); err != nil {
		logger.Error(err, "Failed to update Operation status")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, err
	}

	// Perform SSH bootstrap
	if err := r.bootstrapServer(ctx, server, profile, cfg); err != nil {
		logger.Error(err, "Bootstrap failed", "server", server.Name)

		// Update server status to error
		server.Status.State = "error"
		server.Status.Message = fmt.Sprintf("Bootstrap failed: %v", err)
		server.Status.LastUpdated = metav1.Now()
		if updateErr := r.Status().Update(ctx, server); updateErr != nil {
			logger.Error(updateErr, "Failed to update Server status to error")
		}

		return r.updateOperationStatus(ctx, operation, api.OperationPhaseFailed, fmt.Sprintf("Bootstrap failed: %v", err))
	}

	// Configure routing for the new node (DC router, AKS router, Azure route tables)
	if err := r.configureNodeRouting(ctx, server, cfg); err != nil {
		logger.Error(err, "Failed to configure routing (node will function but may have connectivity issues)", "server", server.Name)
		// Don't fail the operation, just log the warning - routing can be fixed manually
	}

	// Bootstrap succeeded - update server status
	logger.Info("Bootstrap succeeded", "server", server.Name)
	server.Status.State = "ready"
	server.Status.CurrentOS = fmt.Sprintf("k8s-%s", profile.Spec.KubernetesVersion)
	server.Status.AppliedProvisioningProfile = profile.Name
	server.Status.Message = fmt.Sprintf("Repaved successfully by operation %s", operation.Name)
	server.Status.LastUpdated = metav1.Now()
	if err := r.Status().Update(ctx, server); err != nil {
		logger.Error(err, "Failed to update Server status to ready")
	}

	return r.updateOperationStatus(ctx, operation, api.OperationPhaseSucceeded, "Repave completed successfully")
}

// resolveBootstrapConfig resolves configuration from profile and secrets
func (r *OperationReconciler) resolveBootstrapConfig(ctx context.Context, namespace string, profile *api.ProvisioningProfile) (*bootstrapConfig, func(), error) {
	cfg := &bootstrapConfig{
		kubernetesVersion: profile.Spec.KubernetesVersion,
		adminUsername:     r.AdminUsername,
		sshPrivateKeyPath: r.SSHPrivateKeyPath,
		sshPort:           r.SSHPort,
	}

	cleanup := func() {} // No-op by default

	// Override admin username from profile if set
	if profile.Spec.AdminUsername != "" {
		cfg.adminUsername = profile.Spec.AdminUsername
	}

	// Default kubernetes version
	if cfg.kubernetesVersion == "" {
		cfg.kubernetesVersion = "1.34"
	}

	// Default admin username
	if cfg.adminUsername == "" {
		cfg.adminUsername = "ubuntu"
	}

	// Default SSH port
	if cfg.sshPort == 0 {
		cfg.sshPort = 22
	}

	// Default SSH key path
	if cfg.sshPrivateKeyPath == "" {
		cfg.sshPrivateKeyPath = filepath.Join(os.Getenv("HOME"), ".ssh", "id_rsa")
	}

	// Fetch SSH credentials from secret if specified
	if profile.Spec.SSHCredentialsSecretRef != "" {
		var secret corev1.Secret
		secretKey := client.ObjectKey{
			Namespace: namespace,
			Name:      profile.Spec.SSHCredentialsSecretRef,
		}
		if err := r.Get(ctx, secretKey, &secret); err != nil {
			return nil, cleanup, fmt.Errorf("failed to get SSH credentials secret %s: %w", profile.Spec.SSHCredentialsSecretRef, err)
		}

		// Get private key
		if privateKey, ok := secret.Data["privateKey"]; ok {
			// Write to temp file
			tmpFile, err := os.CreateTemp("", "ssh-key-*")
			if err != nil {
				return nil, cleanup, fmt.Errorf("failed to create temp file for SSH key: %w", err)
			}
			if err := os.WriteFile(tmpFile.Name(), privateKey, 0600); err != nil {
				os.Remove(tmpFile.Name())
				return nil, cleanup, fmt.Errorf("failed to write SSH key to temp file: %w", err)
			}
			cfg.sshPrivateKeyPath = tmpFile.Name()
			cleanup = func() {
				os.Remove(tmpFile.Name())
			}
		}

		// Get username if present
		if username, ok := secret.Data["username"]; ok {
			cfg.adminUsername = string(username)
		}
	}

	return cfg, cleanup, nil
}

// handleRunning checks on an already-running operation (shouldn't normally happen with sync bootstrap)
func (r *OperationReconciler) handleRunning(ctx context.Context, operation *api.Operation, server *api.Server, profile *api.ProvisioningProfile) (ctrl.Result, error) {
	// With synchronous SSH bootstrap, we shouldn't get here often
	// If we do, it means the controller restarted mid-operation
	// Just mark as failed and let user retry
	return r.updateOperationStatus(ctx, operation, api.OperationPhaseFailed, "Operation was interrupted (controller restart?). Please create a new operation to retry.")
}

// bootstrapServer runs the bootstrap script on the server via SSH
func (r *OperationReconciler) bootstrapServer(ctx context.Context, server *api.Server, profile *api.ProvisioningProfile, cfg *bootstrapConfig) error {
	// Determine SSH target - use IPv4 from spec
	target := server.Spec.IPv4
	if target == "" {
		return fmt.Errorf("server %s has no IPv4 address", server.Name)
	}

	// Get router IP for SSH proxy (empty for router itself)
	routerIP := server.Spec.RouterIP

	// Delete existing K8s Node object if it exists (for repave)
	if err := r.deleteNodeIfExists(ctx, server.Name); err != nil {
		log.FromContext(ctx).Error(err, "Failed to delete existing node (continuing anyway)", "node", server.Name)
	}

	var script string

	// AKS mode uses ServiceAccount token instead of kubeadm
	if r.ControlPlaneMode == "aks" {
		// Generate fresh SA token for this bootstrap
		saToken, err := r.getOrCreateSAToken(ctx)
		if err != nil {
			return fmt.Errorf("get SA token for AKS bootstrap: %w", err)
		}
		log.FromContext(ctx).Info("Building AKS bootstrap script", "nodeIP", target, "vmName", server.Name)
		script = r.buildAKSBootstrapScript(cfg.kubernetesVersion, target, server.Name, saToken)
		// Debug: write script to file for inspection
		os.WriteFile("/tmp/aks-bootstrap-debug.sh", []byte(script), 0755)
	} else {
		// Get control plane Tailscale IP if not set
		controlPlaneIP := r.ControlPlaneTailscaleIP
		if controlPlaneIP == "" {
			var err error
			controlPlaneIP, err = r.detectControlPlaneTailscaleIP()
			if err != nil {
				return fmt.Errorf("detect control plane IP: %w", err)
			}
		}

		// Generate kubeadm join command
		joinCmd, err := r.generateKubeadmJoinCommand(controlPlaneIP)
		if err != nil {
			return fmt.Errorf("generate join command: %w", err)
		}

		// Build the bootstrap script with node's actual IP
		script = r.buildBootstrapScript(controlPlaneIP, joinCmd, cfg.kubernetesVersion, target)
	}

	// Run the script via SSH (via router proxy if routerIP is set)
	return r.runRemoteBootstrap(ctx, target, routerIP, script, cfg)
}

// detectControlPlaneTailscaleIP gets the Tailscale IP from the Kind control plane container
func (r *OperationReconciler) detectControlPlaneTailscaleIP() (string, error) {
	if r.ControlPlaneMode == "tailscale" {
		// In tailscale mode, we require the IP to be provided
		if r.ControlPlaneTailscaleIP != "" {
			return r.ControlPlaneTailscaleIP, nil
		}
		// Try to get IP via tailscale ssh
		target := r.ControlPlaneHostname
		sshUser := r.ControlPlaneSSHUser
		if sshUser == "" {
			sshUser = "azureuser"
		}
		cmd := exec.Command("tailscale", "ssh", fmt.Sprintf("%s@%s", sshUser, target), "--", "tailscale", "ip", "-4")
		out, err := cmd.Output()
		if err != nil {
			return "", fmt.Errorf("tailscale ssh to get IP: %w", err)
		}
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		if len(lines) == 0 || lines[0] == "" {
			return "", fmt.Errorf("no Tailscale IP found")
		}
		return strings.TrimSpace(lines[0]), nil
	}

	// Kind mode: use docker exec
	containerName := r.KindContainerName
	if containerName == "" {
		containerName = "stargate-demo-control-plane"
	}

	cmd := exec.Command("docker", "exec", containerName, "tailscale", "--socket", "/var/run/tailscale/tailscaled.sock", "ip", "-4")
	out, err := cmd.Output()
	if err != nil {
		// Try without socket flag
		cmd = exec.Command("docker", "exec", containerName, "tailscale", "ip", "-4")
		out, err = cmd.Output()
		if err != nil {
			return "", fmt.Errorf("docker exec tailscale ip: %w", err)
		}
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 0 || lines[0] == "" {
		return "", fmt.Errorf("no Tailscale IP found")
	}

	return strings.TrimSpace(lines[0]), nil
}

// generateKubeadmJoinCommand creates a new join token and returns the join command
func (r *OperationReconciler) generateKubeadmJoinCommand(controlPlaneTailscaleIP string) (string, error) {
	var out []byte
	var err error

	if r.ControlPlaneMode == "tailscale" {
		// Use tailscale SSH to reach the control plane
		target := controlPlaneTailscaleIP
		if target == "" {
			target = r.ControlPlaneHostname
		}
		sshUser := r.ControlPlaneSSHUser
		if sshUser == "" {
			sshUser = "azureuser"
		}
		cmd := exec.Command("tailscale", "ssh", fmt.Sprintf("%s@%s", sshUser, target), "--", "sudo", "kubeadm", "token", "create", "--print-join-command")
		out, err = cmd.Output()
		if err != nil {
			return "", fmt.Errorf("kubeadm token create via tailscale ssh: %w", err)
		}
	} else {
		// Use docker exec for Kind control plane
		containerName := r.KindContainerName
		if containerName == "" {
			containerName = "stargate-demo-control-plane"
		}
		cmd := exec.Command("docker", "exec", containerName, "kubeadm", "token", "create", "--print-join-command")
		out, err = cmd.Output()
		if err != nil {
			return "", fmt.Errorf("kubeadm token create: %w", err)
		}
	}

	joinCmd := strings.TrimSpace(string(out))
	if joinCmd == "" {
		return "", fmt.Errorf("empty join command from kubeadm")
	}

	// Replace the API server address with the Tailscale IP
	if controlPlaneTailscaleIP != "" {
		re := regexp.MustCompile(`kubeadm join\s+[^\s]+`)
		joinCmd = re.ReplaceAllString(joinCmd, fmt.Sprintf("kubeadm join %s:6443", controlPlaneTailscaleIP))
	}

	if !strings.Contains(joinCmd, "--token") || !strings.Contains(joinCmd, "--discovery-token-ca-cert-hash") {
		return "", fmt.Errorf("join command missing token or ca hash: %s", joinCmd)
	}

	return joinCmd, nil
}

// buildBootstrapScript creates the bash script that installs k8s and joins the cluster
// Workers behind a router don't have Tailscale - they use their local IP for node registration
func (r *OperationReconciler) buildBootstrapScript(controlPlaneTailscaleIP, joinCmd, kubernetesVersion, nodeIP string) string {
	controlPlaneHostname := r.ControlPlaneHostname
	if controlPlaneHostname == "" {
		// Try to get hostname from Kind container
		containerName := r.KindContainerName
		if containerName == "" {
			containerName = "stargate-demo-control-plane"
		}
		cmd := exec.Command("docker", "exec", containerName, "hostname")
		if out, err := cmd.Output(); err == nil {
			controlPlaneHostname = strings.TrimSpace(string(out))
		} else {
			controlPlaneHostname = containerName // Fall back to container name
		}
	}

	// Extract major.minor version for the k8s repo
	k8sRepoVersion := kubernetesVersion
	if k8sRepoVersion == "" {
		k8sRepoVersion = "1.34"
	}
	// Handle versions like "1.34.0" -> "1.34"
	parts := strings.Split(k8sRepoVersion, ".")
	if len(parts) >= 2 {
		k8sRepoVersion = parts[0] + "." + parts[1]
	}

	return fmt.Sprintf(`#!/bin/bash
set -ex

KUBERNETES_VERSION="%s"
NODE_IP="%s"

# Add control plane hostname to /etc/hosts for kubeadm to resolve
echo '%s %s' >> /etc/hosts

echo "Using node IP: $NODE_IP"

JOIN_CMD='%s'

API_SERVER=$(echo "$JOIN_CMD" | grep -oP 'kubeadm join \K[^\s]+')
TOKEN=$(echo "$JOIN_CMD" | grep -oP -- '--token \K[^\s]+')
CA_CERT_HASH=$(echo "$JOIN_CMD" | grep -oP -- '--discovery-token-ca-cert-hash \K[^\s]+')

cat > /tmp/kubeadm-join-config.yaml <<EOF
apiVersion: kubeadm.k8s.io/v1beta3
kind: JoinConfiguration
discovery:
  bootstrapToken:
    apiServerEndpoint: $API_SERVER
    token: $TOKEN
    caCertHashes:
      - $CA_CERT_HASH
nodeRegistration:
  kubeletExtraArgs:
    cgroup-root: /
    node-ip: "$NODE_IP"
EOF

echo "Configuring kernel params..."
modprobe overlay
modprobe br_netfilter
sysctl -w net.bridge.bridge-nf-call-iptables=1
sysctl -w net.bridge.bridge-nf-call-ip6tables=1
sysctl -w net.ipv4.ip_forward=1
swapoff -a
sed -i '/swap/d' /etc/fstab

if ! command -v containerd >/dev/null; then
  mkdir -p /etc/apt/keyrings
  curl -fsSL https://download.docker.com/linux/ubuntu/gpg | gpg --dearmor -o /etc/apt/keyrings/docker.gpg
  echo "deb [arch=amd64 signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/ubuntu $(lsb_release -cs) stable" > /etc/apt/sources.list.d/docker.list
  apt-get update
  apt-get install -y containerd.io
  mkdir -p /etc/containerd
  containerd config default > /etc/containerd/config.toml
  sed -i 's/SystemdCgroup = false/SystemdCgroup = true/' /etc/containerd/config.toml
  systemctl restart containerd
  systemctl enable containerd
fi

if ! command -v kubeadm >/dev/null; then
  curl -fsSL https://pkgs.k8s.io/core:/stable:/v${KUBERNETES_VERSION}/deb/Release.key | gpg --dearmor -o /etc/apt/keyrings/kubernetes-apt-keyring.gpg
  echo "deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] https://pkgs.k8s.io/core:/stable:/v${KUBERNETES_VERSION}/deb/ /" > /etc/apt/sources.list.d/kubernetes.list
  apt-get update
  apt-get install -y kubelet kubeadm kubectl
  apt-mark hold kubelet kubeadm kubectl
fi

timeout 120 kubeadm reset -f || true
rm -rf /etc/cni/net.d/* || true

echo "Running kubeadm join..."
timeout 180 kubeadm join --config /tmp/kubeadm-join-config.yaml

CONTROL_PLANE_TAILSCALE_IP='%s'
if [[ -n "$CONTROL_PLANE_TAILSCALE_IP" ]]; then
  iptables -t nat -I OUTPUT -d 10.96.0.1 -p tcp --dport 443 -j DNAT --to-destination "$CONTROL_PLANE_TAILSCALE_IP:6443"
  iptables -t nat -I PREROUTING -d 10.96.0.1 -p tcp --dport 443 -j DNAT --to-destination "$CONTROL_PLANE_TAILSCALE_IP:6443"
  iptables -t nat -A POSTROUTING -d "$CONTROL_PLANE_TAILSCALE_IP" -p tcp --dport 6443 -j MASQUERADE
  mkdir -p /etc/iptables
  iptables-save > /etc/iptables/rules.v4
fi

# Advertise pod CIDR via Tailscale for routing (only if Tailscale is present)
if command -v tailscale >/dev/null; then
  for i in {1..60}; do
    POD_CIDR=$(kubectl --kubeconfig /etc/kubernetes/kubelet.conf get node $(hostname) -o jsonpath='{.spec.podCIDR}' 2>/dev/null || true)
    if [[ -n "$POD_CIDR" && "$POD_CIDR" != "<no value>" ]]; then
      tailscale set --advertise-routes="$POD_CIDR" --accept-routes || true
      break
    fi
    sleep 5
  done
fi

echo "Bootstrap complete!"
`,
		k8sRepoVersion,
		nodeIP,
		controlPlaneTailscaleIP,
		controlPlaneHostname,
		joinCmd,
		controlPlaneTailscaleIP,
	)
}

// buildAKSBootstrapScript creates a bash script for AKS node join
// This uses a ServiceAccount token (not bootstrap tokens) because AKS doesn't support TLS bootstrapping
// It also sets provider-id so the Azure cloud-controller-manager recognizes the node
func (r *OperationReconciler) buildAKSBootstrapScript(kubernetesVersion, nodeIP, vmName, saToken string) string {
	// Default values
	clusterDNS := r.AKSClusterDNS
	if clusterDNS == "" {
		clusterDNS = "10.0.0.10"
	}

	clusterName := r.AKSClusterName
	if clusterName == "" {
		clusterName = "aks-cluster"
	}

	resourceGroup := r.AKSResourceGroup
	if resourceGroup == "" {
		resourceGroup = "aks-rg"
	}

	subscriptionID := r.AKSSubscriptionID
	vmResourceGroup := r.AKSVMResourceGroup
	if vmResourceGroup == "" {
		vmResourceGroup = resourceGroup
	}

	// Use private IP for API server if configured (for Tailscale mesh connectivity)
	// The AKS router proxies port 6443 to the AKS API server
	apiServer := r.AKSAPIServer
	// Ensure the API server URL has https:// prefix
	if !strings.HasPrefix(apiServer, "https://") && !strings.HasPrefix(apiServer, "http://") {
		apiServer = "https://" + apiServer
	}
	if r.AKSAPIServerPrivateIP != "" {
		apiServer = fmt.Sprintf("https://%s:6443", r.AKSAPIServerPrivateIP)
	}

	// Construct Azure provider-id so cloud-controller-manager won't delete the node
	providerID := fmt.Sprintf("azure:///subscriptions/%s/resourceGroups/%s/providers/Microsoft.Compute/virtualMachines/%s",
		subscriptionID, vmResourceGroup, vmName)

	return fmt.Sprintf(`#!/bin/bash
set -ex

NODE_NAME=$(hostname)
NODE_IP="%s"
SA_TOKEN="%s"
API_SERVER="%s"
CA_CERT_BASE64="%s"
CLUSTER_DNS="%s"
CLUSTER_NAME="%s"
RESOURCE_GROUP="%s"
PROVIDER_ID="%s"

echo "=== AKS Node Join for $NODE_NAME ==="
echo "DEBUG: NODE_IP is '$NODE_IP'"
echo "Provider ID: $PROVIDER_ID"

# Stop existing services to ensure clean reconfiguration
echo "Stopping existing kubelet and containerd if running..."
systemctl stop kubelet 2>/dev/null || true
systemctl stop containerd 2>/dev/null || true

# Clean up stale CNI interfaces from previous kubenet configuration
# These cause routing conflicts if left behind after repave
echo "Cleaning up stale CNI interfaces..."
ip link delete cni0 2>/dev/null || true
ip link delete cbr0 2>/dev/null || true
ip link delete flannel.1 2>/dev/null || true
ip link delete docker0 2>/dev/null || true
# Remove any stale routes that reference deleted interfaces
ip route flush cache 2>/dev/null || true

# Clean up any stale CNI state
rm -rf /var/lib/cni/networks/* 2>/dev/null || true
rm -rf /var/lib/cni/cache/* 2>/dev/null || true
rm -f /etc/cni/net.d/*.conf /etc/cni/net.d/*.conflist 2>/dev/null || true

# Link resolv.conf
ln -sf /run/systemd/resolve/resolv.conf /etc/resolv.conf || true

# Create required directories
mkdir -p /var/lib/cni
mkdir -p /opt/cni/bin
mkdir -p /etc/cni/net.d
mkdir -p /etc/kubernetes/volumeplugins
mkdir -p /etc/kubernetes/certs
mkdir -p /etc/containerd
mkdir -p /usr/lib/systemd/system/kubelet.service.d
mkdir -p /var/lib/kubelet

# Install containerd
echo "Installing containerd..."
cat > /usr/lib/systemd/system/containerd.service <<'CONTAINERD_SVC'
[Unit]
Description=containerd container runtime
Documentation=https://containerd.io
After=network.target local-fs.target
[Service]
ExecStartPre=-/sbin/modprobe overlay
ExecStart=/usr/bin/containerd
Type=notify
Delegate=yes
KillMode=process
Restart=always
RestartSec=5
LimitNPROC=infinity
LimitCORE=infinity
LimitNOFILE=infinity
TasksMax=infinity
OOMScoreAdjust=-999
[Install]
WantedBy=multi-user.target
CONTAINERD_SVC

cat > /etc/containerd/config.toml <<'CONTAINERD_CFG'
version = 2
oom_score = 0
[plugins."io.containerd.grpc.v1.cri"]
    sandbox_image = "mcr.microsoft.com/oss/kubernetes/pause:3.6"
    [plugins."io.containerd.grpc.v1.cri".containerd]
        default_runtime_name = "runc"
        [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc]
            runtime_type = "io.containerd.runc.v2"
        [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc.options]
            BinaryName = "/usr/bin/runc"
            SystemdCgroup = true
    [plugins."io.containerd.grpc.v1.cri".cni]
        bin_dir = "/opt/cni/bin"
        conf_dir = "/etc/cni/net.d"
        # Note: conf_template is not used - Cilium manages its own CNI config
    [plugins."io.containerd.grpc.v1.cri".registry]
        config_path = "/etc/containerd/certs.d"
    [plugins."io.containerd.grpc.v1.cri".registry.headers]
        X-Meta-Source-Client = ["azure/aks"]
[metrics]
    address = "0.0.0.0:10257"
CONTAINERD_CFG

# Sysctl settings for Kubernetes
cat > /etc/sysctl.d/999-sysctl-aks.conf <<'SYSCTL_CFG'
net.ipv4.ip_forward = 1
net.ipv4.conf.all.forwarding = 1
net.ipv6.conf.all.forwarding = 1
net.bridge.bridge-nf-call-iptables = 1
vm.overcommit_memory = 1
kernel.panic = 10
kernel.panic_on_oops = 1
kernel.pid_max = 4194304
fs.inotify.max_user_watches = 1048576
fs.inotify.max_user_instances = 1024
net.ipv4.tcp_retries2 = 8
net.core.message_burst = 80
net.core.message_cost = 40
net.core.somaxconn = 16384
net.ipv4.tcp_max_syn_backlog = 16384
net.ipv4.neigh.default.gc_thresh1 = 4096
net.ipv4.neigh.default.gc_thresh2 = 8192
net.ipv4.neigh.default.gc_thresh3 = 16384
SYSCTL_CFG

# Write CA certificate
echo "Writing CA certificate..."
KUBE_CA_PATH="/etc/kubernetes/certs/ca.crt"
touch "${KUBE_CA_PATH}"
chmod 0600 "${KUBE_CA_PATH}"
chown root:root "${KUBE_CA_PATH}"
echo "${CA_CERT_BASE64}" | base64 -d > "${KUBE_CA_PATH}"

# Create kubelet config.yaml
# IMPORTANT: rotateCertificates and serverTLSBootstrap must be false for SA token auth
cat > /var/lib/kubelet/config.yaml <<KUBELET_CONFIG
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
authentication:
  anonymous:
    enabled: false
  webhook:
    cacheTTL: 0s
    enabled: true
  x509:
    clientCAFile: /etc/kubernetes/certs/ca.crt
authorization:
  mode: Webhook
  webhook:
    cacheAuthorizedTTL: 0s
    cacheUnauthorizedTTL: 0s
cgroupDriver: systemd
clusterDNS:
- ${CLUSTER_DNS}
clusterDomain: cluster.local
cpuManagerReconcilePeriod: 0s
evictionPressureTransitionPeriod: 0s
fileCheckFrequency: 0s
healthzBindAddress: 127.0.0.1
healthzPort: 10248
httpCheckFrequency: 0s
imageMinimumGCAge: 0s
nodeStatusReportFrequency: 0s
nodeStatusUpdateFrequency: 0s
rotateCertificates: false
serverTLSBootstrap: false
runtimeRequestTimeout: 0s
shutdownGracePeriod: 0s
shutdownGracePeriodCriticalPods: 0s
streamingConnectionIdleTimeout: 0s
syncFrequency: 0s
volumeStatsAggPeriod: 0s
KUBELET_CONFIG

# Create kubeconfig with ServiceAccount token (direct auth, not bootstrap)
cat > /var/lib/kubelet/kubeconfig <<KUBECONFIG
apiVersion: v1
kind: Config
clusters:
- name: aks
  cluster:
    certificate-authority: /etc/kubernetes/certs/ca.crt
    server: "${API_SERVER}"
users:
- name: kubelet
  user:
    token: "${SA_TOKEN}"
contexts:
- context:
    cluster: aks
    user: kubelet
  name: aks
current-context: aks
KUBECONFIG

chmod 0600 /var/lib/kubelet/kubeconfig

# NOTE: kubelet.service is created AFTER package installation to prevent apt from overwriting it

# CNI Configuration
# Cilium will install its own CNI config when the cilium-agent pod starts
# We just need to ensure the CNI directory exists and is clean
mkdir -p /etc/cni/net.d
# Remove any existing CNI configs to let Cilium take over
rm -f /etc/cni/net.d/*.conf /etc/cni/net.d/*.conflist 2>/dev/null || true
# Create a placeholder to prevent containerd from failing before Cilium starts
echo '{"cniVersion":"0.3.1","name":"waiting-for-cilium","type":"loopback"}' > /etc/cni/net.d/99-loopback.conf

# Create empty azure.json for cloud-provider
AZURE_JSON_PATH="/etc/kubernetes/azure.json"
touch "${AZURE_JSON_PATH}"
chmod 0600 "${AZURE_JSON_PATH}"
chown root:root "${AZURE_JSON_PATH}"

# Install containerd if not present
if ! command -v containerd >/dev/null; then
  echo "Installing containerd..."
  export DEBIAN_FRONTEND=noninteractive
  apt-get update
  apt-get install -y apt-transport-https ca-certificates curl gnupg
  mkdir -p /etc/apt/keyrings
  rm -f /etc/apt/keyrings/docker.gpg
  curl -fsSL https://download.docker.com/linux/ubuntu/gpg | gpg --batch --yes --dearmor -o /etc/apt/keyrings/docker.gpg
  echo "deb [arch=amd64 signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/ubuntu $(lsb_release -cs) stable" > /etc/apt/sources.list.d/docker.list
  apt-get update
  apt-get install -y -o Dpkg::Options::="--force-confold" containerd.io
fi

# Install kubelet if not present
if ! command -v kubelet >/dev/null; then
  echo "Installing kubelet..."
  rm -f /etc/apt/keyrings/kubernetes-apt-keyring.gpg
  curl -fsSL https://pkgs.k8s.io/core:/stable:/v1.33/deb/Release.key | gpg --batch --yes --dearmor -o /etc/apt/keyrings/kubernetes-apt-keyring.gpg
  echo "deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] https://pkgs.k8s.io/core:/stable:/v1.33/deb/ /" > /etc/apt/sources.list.d/kubernetes.list
  apt-get update
  apt-get install -y kubelet kubectl
  apt-mark hold kubelet kubectl
fi

# Install basic CNI plugins (loopback is needed, Cilium brings its own cilium-cni)
mkdir -p /opt/cni/bin
if [ ! -f /opt/cni/bin/loopback ]; then
  echo "Installing loopback CNI plugin..."
  CNI_VERSION="v1.3.0"
  curl -L "https://github.com/containernetworking/plugins/releases/download/${CNI_VERSION}/cni-plugins-linux-amd64-${CNI_VERSION}.tgz" | tar -C /opt/cni/bin -xz
fi
# Cilium will install cilium-cni binary when the DaemonSet starts

# Load kernel modules
modprobe overlay || true
modprobe br_netfilter || true

# Apply sysctl settings
sysctl --system

# NOW create kubelet.service AFTER all package installation to avoid it being overwritten
# Write to /lib/systemd/system/ which is the canonical location on Ubuntu
echo "Writing custom AKS kubelet.service..."
cat > /lib/systemd/system/kubelet.service <<'KUBELET_SVC'
[Unit]
Description=Kubelet
ConditionPathExists=/usr/bin/kubelet
After=containerd.service
[Service]
Restart=always
SuccessExitStatus=143
EnvironmentFile=-/etc/default/kubelet
ExecStartPre=/bin/bash -c "if [ $(mount | grep \"/var/lib/kubelet\" | wc -l) -le 0 ] ; then /bin/mount --bind /var/lib/kubelet /var/lib/kubelet ; fi"
ExecStartPre=/bin/mount --make-shared /var/lib/kubelet
ExecStartPre=-/sbin/ebtables -t nat --list
ExecStartPre=-/sbin/iptables -t nat --numeric --list
ExecStart=/usr/bin/kubelet \
        --enable-server \
        --v=2 \
        --kubeconfig=/var/lib/kubelet/kubeconfig \
        --config=/var/lib/kubelet/config.yaml \
        --container-runtime-endpoint=unix:///run/containerd/containerd.sock \
        --volume-plugin-dir=/etc/kubernetes/volumeplugins \
        $KUBELET_EXTRA_ARGS
[Install]
WantedBy=multi-user.target
KUBELET_SVC

# Kubelet environment with provider-id and node labels
# NOTE: kubernetes.azure.com/ebpf-dataplane=cilium is required for Cilium DaemonSet to schedule on this node
cat > /etc/default/kubelet <<KUBELET_ENV
KUBELET_EXTRA_ARGS=--provider-id=${PROVIDER_ID} --node-ip=${NODE_IP} --node-labels=kubernetes.azure.com/cluster=MC_${RESOURCE_GROUP}_${CLUSTER_NAME},kubernetes.azure.com/agentpool=stargate,kubernetes.azure.com/mode=user,kubernetes.azure.com/role=agent,kubernetes.azure.com/managed=false,kubernetes.azure.com/stargate=true,kubernetes.azure.com/ebpf-dataplane=cilium
KUBELET_ENV

# Verify kubelet.service was written correctly
if [ $(wc -c < /lib/systemd/system/kubelet.service) -lt 500 ]; then
  echo "ERROR: kubelet.service seems too small, something went wrong"
  cat /lib/systemd/system/kubelet.service
  exit 1
fi

# Enable and restart services (force restart even if already running)
echo "Starting containerd and kubelet..."
systemctl daemon-reload
systemctl enable containerd
systemctl enable kubelet
systemctl restart containerd
sleep 3
systemctl restart kubelet

# Wait for node to register and patch it with a unique PodCIDR
# Since AKS doesn't auto-allocate PodCIDRs to external nodes, we must set it ourselves
# Use the last two octets of the node IP to generate a unique /24 subnet in 10.244.0.0/16
echo "Waiting for node to register..."
echo "DEBUG at wait: NODE_IP='$NODE_IP'"
NODE_REGISTERED=false
for i in {1..60}; do
  if kubectl --kubeconfig=/var/lib/kubelet/kubeconfig get node "$NODE_NAME" &>/dev/null; then
    echo "Node registered, allocating PodCIDR..."
    NODE_REGISTERED=true
    # Generate unique PodCIDR based on node IP (e.g., 10.70.1.5 -> 10.244.70.0/24)
    # Use third octet of node IP to avoid collision with AKS nodes (which use 10.244.0-3.x)
    THIRD_OCTET=$(echo "$NODE_IP" | cut -d. -f3)
    FOURTH_OCTET=$(echo "$NODE_IP" | cut -d. -f4)
    echo "DEBUG: THIRD=$THIRD_OCTET FOURTH=$FOURTH_OCTET"
    # Combine to create unique subnet: 10.244.<third*10 + fourth mod 256>.0/24
    # NOTE: use double percent to escape for Go fmt.Sprintf
    UNIQUE_OCTET=$(( (THIRD_OCTET * 10 + FOURTH_OCTET) %% 200 + 50 ))
    POD_CIDR="10.244.${UNIQUE_OCTET}.0/24"
    echo "DEBUG: UNIQUE_OCTET=$UNIQUE_OCTET POD_CIDR=$POD_CIDR"
    
    echo "Patching node $NODE_NAME with PodCIDR: $POD_CIDR"
    kubectl --kubeconfig=/var/lib/kubelet/kubeconfig patch node "$NODE_NAME" --type='json' \
      -p="[{\"op\":\"add\",\"path\":\"/spec/podCIDR\",\"value\":\"${POD_CIDR}\"},{\"op\":\"add\",\"path\":\"/spec/podCIDRs\",\"value\":[\"${POD_CIDR}\"]}]" || true

		echo "Patching CiliumNode $NODE_NAME with PodCIDR: $POD_CIDR"
		for j in {1..30}; do
			if kubectl --kubeconfig=/var/lib/kubelet/kubeconfig patch ciliumnode "$NODE_NAME" --type merge -p "{\"spec\":{\"ipam\":{\"podCIDRs\":[\"${POD_CIDR}\"]}}}"; then
				echo "CiliumNode patched with podCIDR"
				break
			fi
			echo "Waiting for CiliumNode resource... attempt $j/30"
			sleep 2
		done
    
    # Write Cilium CNI config with host-local IPAM for non-AKS nodes
    # This is required because AKS uses delegated-plugin IPAM which relies on Azure CNS
    # For DC workers, we use host-local IPAM with the node's podCIDR
    echo "Writing Cilium CNI config with host-local IPAM for podCIDR: $POD_CIDR"
    cat > /etc/cni/net.d/05-cilium.conflist <<CILIUM_CNI
{
  "cniVersion": "0.3.1",
  "name": "cilium",
  "plugins": [
    {
      "type": "cilium-cni",
      "enable-debug": false,
      "log-file": "/var/run/cilium/cilium-cni.log",
      "ipam": {
        "type": "host-local",
        "ranges": [
          [{"subnet": "${POD_CIDR}"}]
        ],
        "routes": [
          {"dst": "0.0.0.0/0"}
        ]
      }
    }
  ]
}
CILIUM_CNI
    # Remove the placeholder now that we have the real config
    rm -f /etc/cni/net.d/99-loopback.conf
    
    # Restart containerd to pick up the new PodCIDR in the CNI template
    echo "Restarting containerd to apply new PodCIDR..."
    systemctl restart containerd
    sleep 5
    systemctl restart kubelet
    break
  fi
  echo "Waiting for node registration... attempt $i/60"
  sleep 2
done

if [ "$NODE_REGISTERED" != "true" ]; then
  echo "ERROR: Node failed to register after 120 seconds"
  echo "Checking kubelet logs..."
  journalctl -u kubelet --no-pager -n 30 || true
  exit 1
fi

# Final cleanup: remove any stale routes from previous PodCIDR assignments
# This prevents routing conflicts when nodes get reassigned different PodCIDRs
echo "Cleaning up stale pod routes..."
for iface in cni0 cbr0 flannel.1; do
  ip route show | grep "dev $iface" | while read route; do
    ip route del $route 2>/dev/null || true
  done
done
# Remove routes to other pods' CIDRs that might conflict with new assignment
for cidr in 10.244.0.0/24 10.244.1.0/24 10.244.2.0/24 10.244.3.0/24; do
  # Only delete if it points to a local interface (not via router)
  if ip route show $cidr | grep -v "via" | grep -q "dev"; then
    ip route del $cidr 2>/dev/null || true
  fi
done

# Final verification - ensure kubelet is installed and running
echo "=== Verifying bootstrap success ==="
if ! command -v kubelet >/dev/null; then
  echo "ERROR: kubelet binary not found after installation"
  exit 1
fi

if ! systemctl is-active --quiet kubelet; then
  echo "ERROR: kubelet service is not running"
  systemctl status kubelet --no-pager || true
  exit 1
fi

echo "=== AKS Node Join complete for $NODE_NAME ==="
echo "Provider ID: $PROVIDER_ID"
echo "kubelet is installed and running successfully"
`,
		nodeIP,
		saToken,
		apiServer,
		r.CACertBase64,
		clusterDNS,
		clusterName,
		resourceGroup,
		providerID,
	)
}

// runRemoteBootstrap executes the bootstrap script on the remote server via SSH
func (r *OperationReconciler) runRemoteBootstrap(ctx context.Context, host, routerIP, script string, cfg *bootstrapConfig) error {
	sshArgs := []string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=30",
		"-i", cfg.sshPrivateKeyPath,
	}

	// If we have a router IP, SSH via the router as a proxy
	if routerIP != "" {
		proxyCmd := fmt.Sprintf("ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -i %s -p %d -W %%h:%%p %s@%s",
			cfg.sshPrivateKeyPath, cfg.sshPort, cfg.adminUsername, routerIP)
		sshArgs = append(sshArgs, "-o", fmt.Sprintf("ProxyCommand=%s", proxyCmd))
	}

	sshArgs = append(sshArgs,
		"-p", strconv.Itoa(cfg.sshPort),
		fmt.Sprintf("%s@%s", cfg.adminUsername, host),
		"sudo", "bash", "-s",
	)

	cmd := exec.CommandContext(ctx, "ssh", sshArgs...)
	cmd.Stdin = strings.NewReader(script)

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ssh bootstrap failed: %w\nOutput: %s", err, buf.String())
	}

	// Log script output for debugging
	os.WriteFile("/tmp/bootstrap-output.log", buf.Bytes(), 0644)
	log.FromContext(ctx).Info("Bootstrap script output written to /tmp/bootstrap-output.log", "bytes", len(buf.String()))

	return nil
}

// updateOperationStatus updates the operation status and returns appropriate result
func (r *OperationReconciler) updateOperationStatus(ctx context.Context, operation *api.Operation, phase api.OperationPhase, message string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	operation.Status.Phase = phase
	operation.Status.Message = message

	if phase == api.OperationPhaseSucceeded || phase == api.OperationPhaseFailed {
		now := metav1.Now()
		operation.Status.CompletionTime = &now
	}

	if err := r.Status().Update(ctx, operation); err != nil {
		logger.Error(err, "Failed to update Operation status")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, err
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager
func (r *OperationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&api.Operation{}).
		Complete(r)
}

// deleteNodeIfExists removes a Kubernetes Node object if it exists (for repave operations)
func (r *OperationReconciler) deleteNodeIfExists(ctx context.Context, nodeName string) error {
	logger := log.FromContext(ctx)

	node := &corev1.Node{}
	if err := r.Get(ctx, client.ObjectKey{Name: nodeName}, node); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return err
		}
		// Node doesn't exist, nothing to delete
		return nil
	}

	logger.Info("Deleting existing node for repave", "node", nodeName)
	if err := r.Delete(ctx, node); err != nil {
		return fmt.Errorf("delete node %s: %w", nodeName, err)
	}

	// Wait briefly for node deletion to propagate
	time.Sleep(2 * time.Second)
	return nil
}

// getOrCreateSAToken creates a new token for the kubelet-bootstrap ServiceAccount
func (r *OperationReconciler) getOrCreateSAToken(ctx context.Context) (string, error) {
	if r.Clientset == nil {
		return "", fmt.Errorf("kubernetes clientset not initialized")
	}

	// Create a token request for the kubelet-bootstrap SA
	// Token is valid for 24 hours (AKS limits token duration)
	expirationSeconds := int64(86400)
	tokenRequest := &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{
			ExpirationSeconds: &expirationSeconds,
		},
	}

	result, err := r.Clientset.CoreV1().ServiceAccounts("kube-system").CreateToken(
		ctx,
		"kubelet-bootstrap",
		tokenRequest,
		metav1.CreateOptions{},
	)
	if err != nil {
		return "", fmt.Errorf("failed to create SA token: %w", err)
	}

	return result.Status.Token, nil
}

// getCACertBase64 returns the base64-encoded CA certificate from the rest config
func (r *OperationReconciler) getCACertBase64() string {
	return r.CACertBase64
}

// InitializeAKSCredentials fetches and caches AKS credentials from the kubeconfig
func (r *OperationReconciler) InitializeAKSCredentials(cfg interface{}) error {
	// Type assert to *rest.Config - we use interface{} to avoid import cycles
	// The actual rest.Config is passed from main.go
	type restConfigWithCA interface {
		GetCAData() []byte
		GetHost() string
	}

	if rc, ok := cfg.(restConfigWithCA); ok {
		caData := rc.GetCAData()
		if len(caData) > 0 {
			r.CACertBase64 = base64.StdEncoding.EncodeToString(caData)
		}
		if r.AKSAPIServer == "" {
			r.AKSAPIServer = rc.GetHost()
		}
	}

	return nil
}

// configureNodeRouting sets up routing for a newly bootstrapped node
// This configures:
// 1. DC router: adds route for node's pod CIDR via the node IP
// 2. Azure route table: adds route for pod CIDR via AKS router
func (r *OperationReconciler) configureNodeRouting(ctx context.Context, server *api.Server, cfg *bootstrapConfig) error {
	logger := log.FromContext(ctx)

	// Calculate the pod CIDR for this node (same logic as in bootstrap script)
	nodeIP := server.Spec.IPv4
	parts := strings.Split(nodeIP, ".")
	if len(parts) != 4 {
		return fmt.Errorf("invalid node IP format: %s", nodeIP)
	}
	thirdOctet, _ := strconv.Atoi(parts[2])
	fourthOctet, _ := strconv.Atoi(parts[3])
	uniqueOctet := (thirdOctet*10+fourthOctet)%200 + 50
	podCIDR := fmt.Sprintf("10.244.%d.0/24", uniqueOctet)

	logger.Info("Configuring routing for node", "server", server.Name, "nodeIP", nodeIP, "podCIDR", podCIDR)

	// Configure DC router route
	if r.DCRouterTailscaleIP != "" {
		if err := r.configureDCRouterRoute(ctx, nodeIP, podCIDR, cfg); err != nil {
			logger.Error(err, "Failed to configure DC router route")
			// Continue with other configurations
		}
	}

	// Configure Azure route table
	if r.AzureRouteTableName != "" && r.AKSVMResourceGroup != "" {
		if err := r.configureAzureRouteTable(ctx, server.Name, podCIDR); err != nil {
			logger.Error(err, "Failed to configure Azure route table")
			// Continue - routing can be fixed manually
		}
	}

	return nil
}

// configureDCRouterRoute adds a route on the DC router for the node's pod CIDR
func (r *OperationReconciler) configureDCRouterRoute(ctx context.Context, nodeIP, podCIDR string, cfg *bootstrapConfig) error {
	logger := log.FromContext(ctx)

	// Build SSH command to add route on DC router
	// Route format: ip route add <podCIDR> via <nodeIP>
	routeCmd := fmt.Sprintf(`
		# Add route for pod CIDR via node IP (if not exists)
		if ! ip route show %s | grep -q "via %s"; then
			ip route add %s via %s dev eth0 || ip route replace %s via %s dev eth0
			echo "Added route: %s via %s"
		else
			echo "Route already exists: %s via %s"
		fi
	`, podCIDR, nodeIP, podCIDR, nodeIP, podCIDR, nodeIP, podCIDR, nodeIP, podCIDR, nodeIP)

	sshArgs := []string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=10",
		"-i", cfg.sshPrivateKeyPath,
		"-p", strconv.Itoa(cfg.sshPort),
		fmt.Sprintf("%s@%s", cfg.adminUsername, r.DCRouterTailscaleIP),
		"sudo", "bash", "-c", routeCmd,
	}

	cmd := exec.CommandContext(ctx, "ssh", sshArgs...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to configure DC router route: %w (output: %s)", err, buf.String())
	}

	logger.Info("Configured DC router route", "podCIDR", podCIDR, "via", nodeIP, "output", buf.String())
	return nil
}

// configureAzureRouteTable adds a route in the Azure route table for the node's pod CIDR
func (r *OperationReconciler) configureAzureRouteTable(ctx context.Context, nodeName, podCIDR string) error {
	logger := log.FromContext(ctx)

	// Use az CLI to add route
	// Route goes via the AKS router which will forward to DC router via Tailscale
	routeName := fmt.Sprintf("pod-cidr-%s", strings.ReplaceAll(nodeName, "-", ""))

	// Check if route already exists
	checkCmd := exec.CommandContext(ctx, "az", "network", "route-table", "route", "show",
		"--resource-group", r.AKSVMResourceGroup,
		"--route-table-name", r.AzureRouteTableName,
		"--name", routeName,
		"--output", "json",
	)
	if err := checkCmd.Run(); err == nil {
		logger.Info("Azure route already exists", "routeName", routeName)
		return nil
	}

	// Add the route - next hop is the AKS router private IP
	aksRouterIP := r.AKSAPIServerPrivateIP // Reuse the AKS router IP
	if aksRouterIP == "" {
		logger.Info("Skipping Azure route table update - AKS router IP not configured")
		return nil
	}

	addCmd := exec.CommandContext(ctx, "az", "network", "route-table", "route", "create",
		"--resource-group", r.AKSVMResourceGroup,
		"--route-table-name", r.AzureRouteTableName,
		"--name", routeName,
		"--address-prefix", podCIDR,
		"--next-hop-type", "VirtualAppliance",
		"--next-hop-ip-address", aksRouterIP,
		"--output", "json",
	)

	var buf bytes.Buffer
	addCmd.Stdout = &buf
	addCmd.Stderr = &buf

	if err := addCmd.Run(); err != nil {
		return fmt.Errorf("failed to create Azure route: %w (output: %s)", err, buf.String())
	}

	logger.Info("Created Azure route", "routeName", routeName, "podCIDR", podCIDR, "nextHop", aksRouterIP)
	return nil
}
