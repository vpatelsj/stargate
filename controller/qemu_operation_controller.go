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

// QemuOperationReconciler reconciles Operation resources for QEMU VMs
type QemuOperationReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Control plane configuration
	KindContainerName       string // Name of the Kind control plane container
	ControlPlaneTailscaleIP string // Tailscale IP of the control plane node
	ControlPlaneHostname    string // Hostname of the control plane node

	// SSH configuration
	SSHPrivateKeyPath string
	SSHPort           int
	AdminUsername     string
}

// qemuBootstrapConfig holds resolved bootstrap configuration for QEMU VMs
type qemuBootstrapConfig struct {
	kubernetesVersion string
	adminUsername     string
	sshPrivateKeyPath string
	sshPort           int
}

// Reconcile handles Operation resources for QEMU VMs
func (r *QemuOperationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the Operation
	var operation api.Operation
	if err := r.Get(ctx, req.NamespacedName, &operation); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Skip if operation is already completed
	if operation.Status.Phase == api.OperationPhaseSucceeded || operation.Status.Phase == api.OperationPhaseFailed {
		return ctrl.Result{}, nil
	}

	// Get the referenced Server
	var server api.Server
	serverKey := client.ObjectKey{
		Namespace: operation.Namespace,
		Name:      operation.Spec.ServerRef.Name,
	}
	if err := r.Get(ctx, serverKey, &server); err != nil {
		logger.Error(err, "Failed to get Server", "serverRef", operation.Spec.ServerRef.Name)
		return r.updateOperationStatus(ctx, &operation, api.OperationPhaseFailed, fmt.Sprintf("Server not found: %s", operation.Spec.ServerRef.Name))
	}

	// Provider gating: only handle qemu servers
	if server.Spec.Provider != "" && server.Spec.Provider != "qemu" {
		logger.Info("Skipping server with non-qemu provider", "server", server.Name, "provider", server.Spec.Provider)
		return ctrl.Result{}, nil
	}

	// Get the referenced ProvisioningProfile
	var profile api.ProvisioningProfile
	profileKey := client.ObjectKey{
		Namespace: operation.Namespace,
		Name:      operation.Spec.ProvisioningProfileRef.Name,
	}
	if err := r.Get(ctx, profileKey, &profile); err != nil {
		logger.Error(err, "Failed to get ProvisioningProfile", "profileRef", operation.Spec.ProvisioningProfileRef.Name)
		return r.updateOperationStatus(ctx, &operation, api.OperationPhaseFailed, fmt.Sprintf("ProvisioningProfile not found: %s", operation.Spec.ProvisioningProfileRef.Name))
	}

	// Handle based on current phase
	switch operation.Status.Phase {
	case api.OperationPhasePending, "":
		return r.handlePending(ctx, &operation, &server, &profile)
	case api.OperationPhaseRunning:
		return r.handleRunning(ctx, &operation, &server, &profile)
	default:
		return ctrl.Result{}, nil
	}
}

// handlePending initiates the repave operation for QEMU VM
func (r *QemuOperationReconciler) handlePending(ctx context.Context, operation *api.Operation, server *api.Server, profile *api.ProvisioningProfile) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Initiating QEMU VM bootstrap via SSH", "server", server.Name, "ipv4", server.Spec.IPv4, "k8sVersion", profile.Spec.KubernetesVersion)

	// Resolve bootstrap configuration
	cfg, cleanup, err := r.resolveBootstrapConfig(ctx, operation.Namespace, profile)
	if err != nil {
		logger.Error(err, "Failed to resolve bootstrap config")
		return r.updateOperationStatus(ctx, operation, api.OperationPhaseFailed, fmt.Sprintf("Failed to resolve config: %v", err))
	}
	defer cleanup()

	// Re-fetch server to avoid conflicts
	var freshServer api.Server
	if err := r.Get(ctx, client.ObjectKeyFromObject(server), &freshServer); err != nil {
		return ctrl.Result{RequeueAfter: 2 * time.Second}, err
	}

	// Update server status to provisioning
	freshServer.Status.State = "provisioning"
	freshServer.Status.Message = fmt.Sprintf("Bootstrap initiated by operation %s", operation.Name)
	freshServer.Status.LastUpdated = metav1.Now()
	if err := r.Status().Update(ctx, &freshServer); err != nil {
		logger.Error(err, "Failed to update Server status")
		return ctrl.Result{RequeueAfter: 2 * time.Second}, err
	}

	// Re-fetch operation to avoid conflicts
	var freshOperation api.Operation
	if err := r.Get(ctx, client.ObjectKeyFromObject(operation), &freshOperation); err != nil {
		return ctrl.Result{RequeueAfter: 2 * time.Second}, err
	}

	// Update operation status to running
	freshOperation.Status.Phase = api.OperationPhaseRunning
	now := metav1.Now()
	freshOperation.Status.StartTime = &now
	freshOperation.Status.Message = "Bootstrap in progress"

	if err := r.Status().Update(ctx, &freshOperation); err != nil {
		logger.Error(err, "Failed to update Operation status")
		return ctrl.Result{RequeueAfter: 2 * time.Second}, err
	}

	// Perform SSH bootstrap
	if err := r.bootstrapServer(ctx, server, profile, cfg); err != nil {
		logger.Error(err, "Bootstrap failed", "server", server.Name)

		// Re-fetch and update server status to error
		if getErr := r.Get(ctx, client.ObjectKeyFromObject(server), &freshServer); getErr == nil {
			freshServer.Status.State = "error"
			freshServer.Status.Message = fmt.Sprintf("Bootstrap failed: %v", err)
			freshServer.Status.LastUpdated = metav1.Now()
			if updateErr := r.Status().Update(ctx, &freshServer); updateErr != nil {
				logger.Error(updateErr, "Failed to update Server status to error")
			}
		}

		// Re-fetch operation for final status update
		if getErr := r.Get(ctx, client.ObjectKeyFromObject(operation), &freshOperation); getErr == nil {
			return r.updateOperationStatus(ctx, &freshOperation, api.OperationPhaseFailed, fmt.Sprintf("Bootstrap failed: %v", err))
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, err
	}

	// Bootstrap succeeded - re-fetch objects for final update
	if err := r.Get(ctx, client.ObjectKeyFromObject(server), &freshServer); err != nil {
		return ctrl.Result{RequeueAfter: 2 * time.Second}, err
	}
	logger.Info("Bootstrap succeeded", "server", server.Name)
	freshServer.Status.State = "ready"
	freshServer.Status.CurrentOS = fmt.Sprintf("k8s-%s", profile.Spec.KubernetesVersion)
	freshServer.Status.AppliedProvisioningProfile = profile.Name
	freshServer.Status.Message = fmt.Sprintf("Joined cluster successfully via operation %s", operation.Name)
	freshServer.Status.LastUpdated = metav1.Now()
	if err := r.Status().Update(ctx, &freshServer); err != nil {
		logger.Error(err, "Failed to update Server status to ready")
	}

	// Re-fetch operation for final status update
	if err := r.Get(ctx, client.ObjectKeyFromObject(operation), &freshOperation); err != nil {
		return ctrl.Result{RequeueAfter: 2 * time.Second}, err
	}
	return r.updateOperationStatus(ctx, &freshOperation, api.OperationPhaseSucceeded, "Bootstrap completed successfully - node joined cluster")
}

// resolveBootstrapConfig resolves configuration from profile and secrets
func (r *QemuOperationReconciler) resolveBootstrapConfig(ctx context.Context, namespace string, profile *api.ProvisioningProfile) (*qemuBootstrapConfig, func(), error) {
	cfg := &qemuBootstrapConfig{
		kubernetesVersion: profile.Spec.KubernetesVersion,
		adminUsername:     r.AdminUsername,
		sshPrivateKeyPath: r.SSHPrivateKeyPath,
		sshPort:           r.SSHPort,
	}

	cleanup := func() {}

	// Override admin username from profile if set
	if profile.Spec.AdminUsername != "" {
		cfg.adminUsername = profile.Spec.AdminUsername
	}

	// Defaults
	if cfg.kubernetesVersion == "" {
		cfg.kubernetesVersion = "1.34"
	}
	if cfg.adminUsername == "" {
		cfg.adminUsername = "ubuntu"
	}
	if cfg.sshPort == 0 {
		cfg.sshPort = 22
	}
	if cfg.sshPrivateKeyPath == "" {
		// Try to use the user's home directory even when running as root
		home := os.Getenv("HOME")
		if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" {
			home = filepath.Join("/home", sudoUser)
		}
		cfg.sshPrivateKeyPath = filepath.Join(home, ".ssh", "id_rsa")
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

		if privateKey, ok := secret.Data["privateKey"]; ok {
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

		if username, ok := secret.Data["username"]; ok {
			cfg.adminUsername = string(username)
		}
	}

	return cfg, cleanup, nil
}

// handleRunning handles already-running operations
func (r *QemuOperationReconciler) handleRunning(ctx context.Context, operation *api.Operation, server *api.Server, profile *api.ProvisioningProfile) (ctrl.Result, error) {
	return r.updateOperationStatus(ctx, operation, api.OperationPhaseFailed, "Operation was interrupted (controller restart?). Please create a new operation to retry.")
}

// bootstrapServer runs the bootstrap script on the QEMU VM via SSH
func (r *QemuOperationReconciler) bootstrapServer(ctx context.Context, server *api.Server, profile *api.ProvisioningProfile, cfg *qemuBootstrapConfig) error {
	// Use the VM's bridge network IP (IPv4 from Server spec)
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

	// Get control plane Tailscale IP (via Kind control-plane container, same as azure flow)
	controlPlaneIP := r.ControlPlaneTailscaleIP
	if controlPlaneIP == "" {
		var err error
		controlPlaneIP, err = r.detectControlPlaneTailscaleIP()
		if err != nil {
			return fmt.Errorf("detect control plane IP: %w", err)
		}
	}

	// Generate kubeadm join command (inside the Kind control-plane container)
	joinCmd, err := r.generateKubeadmJoinCommand(controlPlaneIP)
	if err != nil {
		return fmt.Errorf("generate join command: %w", err)
	}

	// Build the bootstrap script with the node's actual IP
	script := r.buildBootstrapScript(controlPlaneIP, joinCmd, cfg.kubernetesVersion, target)

	// Run the script via SSH (via router proxy if routerIP is set)
	return r.runRemoteBootstrap(ctx, target, routerIP, script, cfg)
}

// detectControlPlaneTailscaleIP gets the Tailscale IP from the Kind control-plane container (aligns with azure controller)
func (r *QemuOperationReconciler) detectControlPlaneTailscaleIP() (string, error) {
	containerName := r.KindContainerName
	if containerName == "" {
		containerName = "stargate-demo-control-plane"
	}

	cmd := exec.Command("docker", "exec", containerName, "tailscale", "--socket", "/var/run/tailscale/tailscaled.sock", "ip", "-4")
	out, err := cmd.Output()
	if err != nil {
		// Try without socket flag as a fallback
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

// generateKubeadmJoinCommand creates a new join token from inside the Kind control-plane container and returns the join command
func (r *QemuOperationReconciler) generateKubeadmJoinCommand(controlPlaneTailscaleIP string) (string, error) {
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

// buildBootstrapScript creates the bash script for QEMU VM bootstrap
// Workers behind a router don't have Tailscale - they use their local IP for node registration
func (r *QemuOperationReconciler) buildBootstrapScript(controlPlaneTailscaleIP, joinCmd, kubernetesVersion, nodeIP string) string {
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
	parts := strings.Split(k8sRepoVersion, ".")
	if len(parts) >= 2 {
		k8sRepoVersion = parts[0] + "." + parts[1]
	}

	return fmt.Sprintf(`#!/bin/bash
set -ex

KUBERNETES_VERSION="%s"
NODE_IP="%s"

# Add control plane hostname to /etc/hosts
echo '%s %s' >> /etc/hosts

echo "Using node IP: $NODE_IP"

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
      value: "$NODE_IP"
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

# Install containerd if not present
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

# Install kubeadm if not present
if ! command -v kubeadm >/dev/null; then
  curl -fsSL https://pkgs.k8s.io/core:/stable:/v${KUBERNETES_VERSION}/deb/Release.key | gpg --dearmor -o /etc/apt/keyrings/kubernetes-apt-keyring.gpg
  echo "deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] https://pkgs.k8s.io/core:/stable:/v${KUBERNETES_VERSION}/deb/ /" > /etc/apt/sources.list.d/kubernetes.list
  apt-get update
  apt-get install -y kubelet kubeadm kubectl
  apt-mark hold kubelet kubeadm kubectl
fi

# Reset any previous join attempts
timeout 120 kubeadm reset -f || true
rm -rf /etc/cni/net.d/* || true

echo "Running kubeadm join..."
timeout 300 kubeadm join --config /tmp/kubeadm-join-config.yaml

# Set up iptables rules for API server access via Tailscale
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

echo "Bootstrap complete! Node joined cluster."
`,
		k8sRepoVersion,
		nodeIP,
		controlPlaneTailscaleIP,
		controlPlaneHostname,
		joinCmd,
		controlPlaneTailscaleIP,
	)
}

// runRemoteBootstrap executes the bootstrap script on the QEMU VM via SSH
func (r *QemuOperationReconciler) runRemoteBootstrap(ctx context.Context, host, routerIP, script string, cfg *qemuBootstrapConfig) error {
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

	return nil
}

// updateOperationStatus updates the operation status
func (r *QemuOperationReconciler) updateOperationStatus(ctx context.Context, operation *api.Operation, phase api.OperationPhase, message string) (ctrl.Result, error) {
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
func (r *QemuOperationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&api.Operation{}).
		Complete(r)
}

// deleteNodeIfExists removes a Kubernetes Node object if it exists (for repave operations)
func (r *QemuOperationReconciler) deleteNodeIfExists(ctx context.Context, nodeName string) error {
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
