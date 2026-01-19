package controller

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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
	SSHPrivateKeyPath       string // Default SSH key path
	SSHPort                 int
	AdminUsername           string // Default admin username
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

	// Build the bootstrap script
	script := r.buildBootstrapScript(controlPlaneIP, joinCmd, cfg.kubernetesVersion)

	// Run the script via SSH
	return r.runRemoteBootstrap(ctx, target, script, cfg)
}

// detectControlPlaneTailscaleIP gets the Tailscale IP from the Kind control plane container
func (r *OperationReconciler) detectControlPlaneTailscaleIP() (string, error) {
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
	containerName := r.KindContainerName
	if containerName == "" {
		containerName = "stargate-demo-control-plane"
	}

	cmd := exec.Command("docker", "exec", containerName, "kubeadm", "token", "create", "--print-join-command")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("kubeadm token create: %w", err)
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
func (r *OperationReconciler) buildBootstrapScript(controlPlaneTailscaleIP, joinCmd, kubernetesVersion string) string {
	controlPlaneHostname := r.ControlPlaneHostname
	if controlPlaneHostname == "" {
		controlPlaneHostname = "stargate-demo-control-plane"
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

# Add control plane hostname to /etc/hosts for kubeadm to resolve
echo '%s %s' >> /etc/hosts

echo "Waiting for Tailscale IP..."
for i in {1..30}; do
  TAILSCALE_IP=$(tailscale ip -4 2>/dev/null | head -n1 || true)
  if [[ -n "$TAILSCALE_IP" ]]; then
    echo "Tailscale IP: $TAILSCALE_IP"
    break
  fi
  sleep 2
done

if [[ -z "$TAILSCALE_IP" ]]; then
  echo "ERROR: Could not get Tailscale IP"
  exit 1
fi

JOIN_CMD='%s'

API_SERVER=$(echo "$JOIN_CMD" | grep -oP 'kubeadm join \K[^\s]+')
TOKEN=$(echo "$JOIN_CMD" | grep -oP -- '--token \K[^\s]+')
CA_CERT_HASH=$(echo "$JOIN_CMD" | grep -oP -- '--discovery-token-ca-cert-hash \K[^\s]+')

cat > /tmp/kubeadm-join-config.yaml <<EOF
apiVersion: kubeadm.k8s.io/v1beta4
kind: JoinConfiguration
discovery:
  bootstrapToken:
    apiServerEndpoint: $API_SERVER
    token: $TOKEN
    caCertHashes:
      - $CA_CERT_HASH
nodeRegistration:
  kubeletExtraArgs:
    - name: cgroup-root
      value: /
    - name: node-ip
      value: "$TAILSCALE_IP"
---
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
cgroupRoot: /
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

for i in {1..60}; do
  POD_CIDR=$(kubectl --kubeconfig /etc/kubernetes/kubelet.conf get node $(hostname) -o jsonpath='{.spec.podCIDR}' 2>/dev/null || true)
  if [[ -n "$POD_CIDR" && "$POD_CIDR" != "<no value>" ]]; then
    tailscale set --advertise-routes="$POD_CIDR" --accept-routes || true
    break
  fi
  sleep 5
done

echo "Bootstrap complete!"
`,
		k8sRepoVersion,
		controlPlaneTailscaleIP,
		controlPlaneHostname,
		joinCmd,
		controlPlaneTailscaleIP,
	)
}

// runRemoteBootstrap executes the bootstrap script on the remote server via SSH
func (r *OperationReconciler) runRemoteBootstrap(ctx context.Context, host, script string, cfg *bootstrapConfig) error {
	sshArgs := []string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=30",
		"-i", cfg.sshPrivateKeyPath,
		"-p", strconv.Itoa(cfg.sshPort),
		fmt.Sprintf("%s@%s", cfg.adminUsername, host),
		"sudo", "bash", "-s",
	}

	cmd := exec.CommandContext(ctx, "ssh", sshArgs...)
	cmd.Stdin = strings.NewReader(script)

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ssh bootstrap failed: %w\nOutput: %s", err, buf.String())
	}

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
