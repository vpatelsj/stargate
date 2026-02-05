// Package azure implements the Provider interface for Azure/AKS TLS bootstrapping.
package azure

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	pb "github.com/vpatelsj/stargate/gen/baremetal/v1"
	"github.com/vpatelsj/stargate/internal/stargate/provider"
)

// Config holds Azure provider configuration.
type Config struct {
	// SSH settings
	SSHPrivateKeyPath string
	SSHPort           int
	SSHUser           string

	// AKS settings
	AKSAPIServer          string
	AKSClusterName        string
	AKSResourceGroup      string
	AKSClusterDNS         string
	AKSSubscriptionID     string
	AKSVMResourceGroup    string
	AKSAPIServerPrivateIP string
	CACertBase64          string

	// Routing
	DCRouterTailscaleIP  string
	AKSRouterTailscaleIP string
	DCRouterPrivateIP    string // DC router's private IP on DC network (e.g., 10.50.1.4)
	AKSNodeSubnet        string // AKS node subnet CIDR (e.g., 10.224.0.0/16)
	AKSPodSubnet         string // AKS pod subnet CIDR (e.g., 10.244.0.0/20)

	// Kubernetes client for SA token creation
	Clientset *kubernetes.Clientset

	// Log callback
	logCallback provider.LogCallback
}

// Provider implements the provider.Provider interface for Azure.
type Provider struct {
	cfg Config
}

// New creates a new Azure provider.
func New(cfg Config) *Provider {
	return &Provider{cfg: cfg}
}

// SetLogCallback sets the log callback function.
func (p *Provider) SetLogCallback(cb provider.LogCallback) {
	p.cfg.logCallback = cb
}

func (p *Provider) log(runID, stream string, format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	if p.cfg.logCallback != nil {
		p.cfg.logCallback(runID, stream, []byte(msg+"\n"))
	}
}

// SetNetboot sets the netboot profile (not needed for Azure VMs).
func (p *Provider) SetNetboot(ctx context.Context, runID string, machine *pb.Machine, profile string) error {
	p.log(runID, "stdout", "[netboot] Skipping netboot for Azure VM (not applicable)")
	return nil
}

// Reboot reboots a machine via SSH.
func (p *Provider) Reboot(ctx context.Context, runID string, machine *pb.Machine, force bool) error {
	p.log(runID, "stdout", "[reboot] Rebooting machine %s", machine.MachineId)

	host, err := p.getSSHHost(machine)
	if err != nil {
		return err
	}

	cmd := "sudo reboot"
	if force {
		cmd = "sudo reboot -f"
	}

	// SSH reboot - it will disconnect, so we ignore the error
	_ = p.runSSHCommand(ctx, runID, host, cmd)
	p.log(runID, "stdout", "[reboot] Reboot command sent, waiting for machine to come back...")

	// Wait for machine to come back online
	time.Sleep(30 * time.Second)

	// Try to reconnect
	for i := 0; i < 30; i++ {
		if err := p.runSSHCommand(ctx, runID, host, "echo 'Machine is back online'"); err == nil {
			p.log(runID, "stdout", "[reboot] Machine is back online")
			return nil
		}
		time.Sleep(10 * time.Second)
	}

	return fmt.Errorf("machine did not come back online after reboot")
}

// Repave reprovisions a machine (for Azure, this triggers the bootstrap script).
func (p *Provider) Repave(ctx context.Context, runID string, machine *pb.Machine, imageRef, cloudInitRef string) error {
	p.log(runID, "stdout", "[repave] Starting repave for machine %s", machine.MachineId)

	// For Azure, repave means running the bootstrap script to join AKS
	host, err := p.getSSHHost(machine)
	if err != nil {
		return err
	}

	// Get SA token for kubelet authentication
	saToken, err := p.getOrCreateSAToken(ctx)
	if err != nil {
		return fmt.Errorf("failed to get SA token: %w", err)
	}
	p.log(runID, "stdout", "[repave] Created ServiceAccount token for kubelet")

	// Get node IP from SSH endpoint
	nodeIP := strings.Split(host, ":")[0]

	// Build the bootstrap script
	script := p.buildAKSBootstrapScript(nodeIP, machine.MachineId, saToken)

	// Run the bootstrap script
	p.log(runID, "stdout", "[repave] Running AKS bootstrap script on %s...", host)
	if err := p.runBootstrapScript(ctx, runID, host, script); err != nil {
		return fmt.Errorf("bootstrap failed: %w", err)
	}

	p.log(runID, "stdout", "[repave] Repave completed successfully")
	return nil
}

// MintJoinMaterial generates join material (SA token for AKS).
func (p *Provider) MintJoinMaterial(ctx context.Context, runID string, targetCluster *pb.TargetClusterRef) (*provider.JoinMaterial, error) {
	p.log(runID, "stdout", "[join-material] Generating join material for AKS cluster")

	token, err := p.getOrCreateSAToken(ctx)
	if err != nil {
		return nil, err
	}

	return &provider.JoinMaterial{
		Endpoint:  p.cfg.AKSAPIServer,
		Token:     token,
		ExpiresAt: time.Now().Add(24 * time.Hour),
		ClusterID: p.cfg.AKSClusterName,
	}, nil
}

// JoinNode joins a node to the AKS cluster.
func (p *Provider) JoinNode(ctx context.Context, runID string, machine *pb.Machine, material *provider.JoinMaterial) error {
	p.log(runID, "stdout", "[join] Joining node %s to cluster %s", machine.MachineId, material.ClusterID)

	host, err := p.getSSHHost(machine)
	if err != nil {
		return err
	}

	nodeIP := strings.Split(host, ":")[0]
	script := p.buildAKSBootstrapScript(nodeIP, machine.MachineId, material.Token)

	if err := p.runBootstrapScript(ctx, runID, host, script); err != nil {
		return fmt.Errorf("join failed: %w", err)
	}

	p.log(runID, "stdout", "[join] Node successfully joined cluster")
	return nil
}

// VerifyInCluster verifies a machine is in the cluster.
func (p *Provider) VerifyInCluster(ctx context.Context, runID string, machine *pb.Machine, targetCluster *pb.TargetClusterRef) error {
	p.log(runID, "stdout", "[verify] Checking node %s in cluster", machine.MachineId)

	if p.cfg.Clientset == nil {
		p.log(runID, "stdout", "[verify] No clientset - skipping verification")
		return nil
	}

	// Check if node exists
	_, err := p.cfg.Clientset.CoreV1().Nodes().Get(ctx, machine.MachineId, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("node %s not found in cluster: %w", machine.MachineId, err)
	}

	p.log(runID, "stdout", "[verify] Node %s verified in cluster", machine.MachineId)
	return nil
}

// RMA initiates RMA process (not implemented for Azure).
func (p *Provider) RMA(ctx context.Context, runID string, machine *pb.Machine, reason string) error {
	p.log(runID, "stdout", "[rma] RMA not implemented for Azure provider")
	return fmt.Errorf("RMA not supported for Azure provider")
}

// ExecuteSSHCommand runs a command on a machine via SSH.
func (p *Provider) ExecuteSSHCommand(ctx context.Context, runID string, machine *pb.Machine, scriptRef string, args map[string]string) error {
	host, err := p.getSSHHost(machine)
	if err != nil {
		return err
	}

	return p.runSSHCommand(ctx, runID, host, scriptRef)
}

// getSSHHost extracts the SSH host from machine spec.
func (p *Provider) getSSHHost(machine *pb.Machine) (string, error) {
	if machine.Spec == nil || machine.Spec.SshEndpoint == "" {
		return "", fmt.Errorf("machine %s has no SSH endpoint", machine.MachineId)
	}
	// Return just the host part (without port)
	parts := strings.Split(machine.Spec.SshEndpoint, ":")
	return parts[0], nil
}

// getOrCreateSAToken creates a token for kubelet-bootstrap ServiceAccount.
func (p *Provider) getOrCreateSAToken(ctx context.Context) (string, error) {
	if p.cfg.Clientset == nil {
		return "", fmt.Errorf("kubernetes clientset not initialized")
	}

	expirationSeconds := int64(86400) // 24 hours
	tokenRequest := &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{
			ExpirationSeconds: &expirationSeconds,
		},
	}

	result, err := p.cfg.Clientset.CoreV1().ServiceAccounts("kube-system").CreateToken(
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

// runSSHCommand runs a single command via SSH.
func (p *Provider) runSSHCommand(ctx context.Context, runID, host, command string) error {
	sshArgs := []string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=30",
	}

	if p.cfg.SSHPrivateKeyPath != "" {
		sshArgs = append(sshArgs, "-i", p.cfg.SSHPrivateKeyPath)
	}

	// If DC router is configured, use it as a proxy
	if p.cfg.DCRouterTailscaleIP != "" {
		proxyCmd := fmt.Sprintf("ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -i %s -p %d -W %%h:%%p %s@%s",
			p.cfg.SSHPrivateKeyPath, p.cfg.SSHPort, p.cfg.SSHUser, p.cfg.DCRouterTailscaleIP)
		sshArgs = append(sshArgs, "-o", fmt.Sprintf("ProxyCommand=%s", proxyCmd))
	}

	sshArgs = append(sshArgs,
		"-p", strconv.Itoa(p.cfg.SSHPort),
		fmt.Sprintf("%s@%s", p.cfg.SSHUser, host),
		command,
	)

	cmd := exec.CommandContext(ctx, "ssh", sshArgs...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ssh command failed: %w\nOutput: %s", err, buf.String())
	}

	return nil
}

// runBootstrapScript runs the bootstrap script via SSH.
func (p *Provider) runBootstrapScript(ctx context.Context, runID, host, script string) error {
	sshArgs := []string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=30",
	}

	if p.cfg.SSHPrivateKeyPath != "" {
		sshArgs = append(sshArgs, "-i", p.cfg.SSHPrivateKeyPath)
	}

	// If DC router is configured, use it as a proxy
	if p.cfg.DCRouterTailscaleIP != "" {
		proxyCmd := fmt.Sprintf("ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -i %s -p %d -W %%h:%%p %s@%s",
			p.cfg.SSHPrivateKeyPath, p.cfg.SSHPort, p.cfg.SSHUser, p.cfg.DCRouterTailscaleIP)
		sshArgs = append(sshArgs, "-o", fmt.Sprintf("ProxyCommand=%s", proxyCmd))
	}

	sshArgs = append(sshArgs,
		"-p", strconv.Itoa(p.cfg.SSHPort),
		fmt.Sprintf("%s@%s", p.cfg.SSHUser, host),
		"sudo", "bash", "-s",
	)

	cmd := exec.CommandContext(ctx, "ssh", sshArgs...)
	cmd.Stdin = strings.NewReader(script)

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	p.log(runID, "stdout", "[bootstrap] Running bootstrap script...")

	if err := cmd.Run(); err != nil {
		os.WriteFile("/tmp/bootstrap-error.log", buf.Bytes(), 0644)
		return fmt.Errorf("ssh bootstrap failed: %w\nOutput: %s", err, buf.String())
	}

	os.WriteFile("/tmp/bootstrap-output.log", buf.Bytes(), 0644)
	p.log(runID, "stdout", "[bootstrap] Script completed successfully")

	return nil
}

// buildAKSBootstrapScript generates the kubelet bootstrap script for AKS.
func (p *Provider) buildAKSBootstrapScript(nodeIP, vmName, saToken string) string {
	clusterDNS := p.cfg.AKSClusterDNS
	if clusterDNS == "" {
		clusterDNS = "10.0.0.10"
	}

	clusterName := p.cfg.AKSClusterName
	if clusterName == "" {
		clusterName = "aks-cluster"
	}

	resourceGroup := p.cfg.AKSResourceGroup
	if resourceGroup == "" {
		resourceGroup = "aks-rg"
	}

	subscriptionID := p.cfg.AKSSubscriptionID
	vmResourceGroup := p.cfg.AKSVMResourceGroup
	if vmResourceGroup == "" {
		vmResourceGroup = resourceGroup
	}

	apiServer := p.cfg.AKSAPIServer
	if !strings.HasPrefix(apiServer, "https://") && !strings.HasPrefix(apiServer, "http://") {
		apiServer = "https://" + apiServer
	}
	if p.cfg.AKSAPIServerPrivateIP != "" {
		apiServer = fmt.Sprintf("https://%s:6443", p.cfg.AKSAPIServerPrivateIP)
	}

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
DC_ROUTER_PRIVATE_IP="%s"
AKS_NODE_SUBNET="%s"
AKS_POD_SUBNET="%s"

echo "=== AKS Node Join for $NODE_NAME ==="
echo "DEBUG: NODE_IP is '$NODE_IP'"
echo "Provider ID: $PROVIDER_ID"

# Stop existing services
systemctl stop kubelet 2>/dev/null || true
systemctl stop containerd 2>/dev/null || true

# Clean up stale CNI interfaces
ip link delete cni0 2>/dev/null || true
ip link delete cbr0 2>/dev/null || true
ip link delete flannel.1 2>/dev/null || true
ip route flush cache 2>/dev/null || true
rm -rf /var/lib/cni/networks/* /var/lib/cni/cache/* 2>/dev/null || true
rm -f /etc/cni/net.d/*.conf /etc/cni/net.d/*.conflist 2>/dev/null || true

# Add route to AKS nodes via DC router (for cross-datacenter pod connectivity)
if [ -n "$DC_ROUTER_PRIVATE_IP" ] && [ -n "$AKS_NODE_SUBNET" ]; then
  echo "Adding route to AKS subnet $AKS_NODE_SUBNET via DC router $DC_ROUTER_PRIVATE_IP"
  ip route del "$AKS_NODE_SUBNET" 2>/dev/null || true
  ip route add "$AKS_NODE_SUBNET" via "$DC_ROUTER_PRIVATE_IP" || true
fi

# Add route to AKS pods via DC router
if [ -n "$DC_ROUTER_PRIVATE_IP" ] && [ -n "$AKS_POD_SUBNET" ]; then
  echo "Adding route to AKS pod subnet $AKS_POD_SUBNET via DC router $DC_ROUTER_PRIVATE_IP"
  ip route del "$AKS_POD_SUBNET" 2>/dev/null || true
  ip route add "$AKS_POD_SUBNET" via "$DC_ROUTER_PRIVATE_IP" || true
fi

ln -sf /run/systemd/resolve/resolv.conf /etc/resolv.conf || true

mkdir -p /var/lib/cni /opt/cni/bin /etc/cni/net.d /etc/kubernetes/volumeplugins
mkdir -p /etc/kubernetes/certs /etc/containerd /var/lib/kubelet

# Containerd service
cat > /usr/lib/systemd/system/containerd.service <<'CONTAINERD_SVC'
[Unit]
Description=containerd container runtime
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
            SystemdCgroup = true
    [plugins."io.containerd.grpc.v1.cri".cni]
        bin_dir = "/opt/cni/bin"
        conf_dir = "/etc/cni/net.d"
[metrics]
    address = "0.0.0.0:10257"
CONTAINERD_CFG

# Sysctl
cat > /etc/sysctl.d/999-sysctl-aks.conf <<'SYSCTL_CFG'
net.ipv4.ip_forward = 1
net.ipv4.conf.all.forwarding = 1
net.ipv6.conf.all.forwarding = 1
net.bridge.bridge-nf-call-iptables = 1
vm.overcommit_memory = 1
kernel.panic = 10
kernel.panic_on_oops = 1
SYSCTL_CFG

# CA certificate
echo "${CA_CERT_BASE64}" | base64 -d > /etc/kubernetes/certs/ca.crt
chmod 0600 /etc/kubernetes/certs/ca.crt

# Kubelet config
cat > /var/lib/kubelet/config.yaml <<KUBELET_CONFIG
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
authentication:
  anonymous:
    enabled: false
  webhook:
    enabled: true
  x509:
    clientCAFile: /etc/kubernetes/certs/ca.crt
authorization:
  mode: Webhook
cgroupDriver: systemd
clusterDNS:
- ${CLUSTER_DNS}
clusterDomain: cluster.local
rotateCertificates: false
serverTLSBootstrap: false
KUBELET_CONFIG

# Kubeconfig with SA token
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

mkdir -p /etc/cni/net.d
echo '{"cniVersion":"0.3.1","name":"waiting-for-cilium","type":"loopback"}' > /etc/cni/net.d/99-loopback.conf

# Install containerd
if ! command -v containerd >/dev/null; then
  export DEBIAN_FRONTEND=noninteractive
  apt-get update
  apt-get install -y apt-transport-https ca-certificates curl gnupg
  mkdir -p /etc/apt/keyrings
  rm -f /etc/apt/keyrings/docker.gpg
  curl -fsSL https://download.docker.com/linux/ubuntu/gpg | gpg --batch --yes --dearmor -o /etc/apt/keyrings/docker.gpg
  echo "deb [arch=amd64 signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/ubuntu $(lsb_release -cs) stable" > /etc/apt/sources.list.d/docker.list
  apt-get update
  apt-get install -y containerd.io
fi

# Install kubelet
if ! command -v kubelet >/dev/null; then
  rm -f /etc/apt/keyrings/kubernetes-apt-keyring.gpg
  curl -fsSL https://pkgs.k8s.io/core:/stable:/v1.33/deb/Release.key | gpg --batch --yes --dearmor -o /etc/apt/keyrings/kubernetes-apt-keyring.gpg
  echo "deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] https://pkgs.k8s.io/core:/stable:/v1.33/deb/ /" > /etc/apt/sources.list.d/kubernetes.list
  apt-get update
  apt-get install -y kubelet kubectl
  apt-mark hold kubelet kubectl
fi

# CNI plugins
mkdir -p /opt/cni/bin
if [ ! -f /opt/cni/bin/loopback ]; then
  CNI_VERSION="v1.3.0"
  curl -L "https://github.com/containernetworking/plugins/releases/download/${CNI_VERSION}/cni-plugins-linux-amd64-${CNI_VERSION}.tgz" | tar -C /opt/cni/bin -xz
fi

modprobe overlay || true
modprobe br_netfilter || true
sysctl --system

# Kubelet service
cat > /lib/systemd/system/kubelet.service <<'KUBELET_SVC'
[Unit]
Description=Kubelet
After=containerd.service
[Service]
EnvironmentFile=/etc/default/kubelet
Restart=always
ExecStartPre=/bin/bash -c "if [ $(mount | grep \"/var/lib/kubelet\" | wc -l) -le 0 ] ; then /bin/mount --bind /var/lib/kubelet /var/lib/kubelet ; fi"
ExecStartPre=/bin/mount --make-shared /var/lib/kubelet
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

cat > /etc/default/kubelet <<KUBELET_ENV
KUBELET_EXTRA_ARGS=--provider-id=${PROVIDER_ID} --node-ip=${NODE_IP} --node-labels=kubernetes.azure.com/cluster=MC_${RESOURCE_GROUP}_${CLUSTER_NAME},kubernetes.azure.com/agentpool=stargate,kubernetes.azure.com/mode=user,kubernetes.azure.com/role=agent,kubernetes.azure.com/managed=false,kubernetes.azure.com/stargate=true,kubernetes.azure.com/ebpf-dataplane=cilium
KUBELET_ENV

systemctl daemon-reload
systemctl enable containerd kubelet
systemctl restart containerd
sleep 3
systemctl restart kubelet

# Wait for node registration and set PodCIDR
echo "Waiting for node to register..."
NODE_REGISTERED=false
for i in {1..60}; do
  if kubectl --kubeconfig=/var/lib/kubelet/kubeconfig get node "$NODE_NAME" &>/dev/null; then
    NODE_REGISTERED=true
    THIRD_OCTET=$(echo "$NODE_IP" | cut -d. -f3)
    FOURTH_OCTET=$(echo "$NODE_IP" | cut -d. -f4)
    UNIQUE_OCTET=$(( (THIRD_OCTET * 10 + FOURTH_OCTET) %% 200 + 50 ))
    POD_CIDR="10.244.${UNIQUE_OCTET}.0/24"
    
    echo "Patching node $NODE_NAME with PodCIDR: $POD_CIDR"
    kubectl --kubeconfig=/var/lib/kubelet/kubeconfig patch node "$NODE_NAME" --type='json' \
      -p="[{\"op\":\"add\",\"path\":\"/spec/podCIDR\",\"value\":\"${POD_CIDR}\"},{\"op\":\"add\",\"path\":\"/spec/podCIDRs\",\"value\":[\"${POD_CIDR}\"]}]" || true
    
    for j in {1..30}; do
      if kubectl --kubeconfig=/var/lib/kubelet/kubeconfig patch ciliumnode "$NODE_NAME" --type merge -p "{\"spec\":{\"ipam\":{\"podCIDRs\":[\"${POD_CIDR}\"]}}}"; then
        break
      fi
      sleep 2
    done
    
    cat > /etc/cni/net.d/05-cilium.conflist <<CILIUM_CNI
{
  "cniVersion": "0.3.1",
  "name": "cilium",
  "plugins": [
    {
      "type": "cilium-cni",
      "ipam": {
        "type": "host-local",
        "ranges": [[{"subnet": "${POD_CIDR}"}]],
        "routes": [{"dst": "0.0.0.0/0"}]
      }
    }
  ]
}
CILIUM_CNI
    rm -f /etc/cni/net.d/99-loopback.conf
    systemctl restart containerd
    sleep 5
    systemctl restart kubelet
    break
  fi
  echo "Waiting... attempt $i/60"
  sleep 2
done

if [ "$NODE_REGISTERED" != "true" ]; then
  echo "ERROR: Node failed to register"
  journalctl -u kubelet --no-pager -n 30 || true
  exit 1
fi

echo "=== AKS Node Join complete for $NODE_NAME ==="
`,
		nodeIP,
		saToken,
		apiServer,
		p.cfg.CACertBase64,
		clusterDNS,
		clusterName,
		resourceGroup,
		providerID,
		p.cfg.DCRouterPrivateIP,
		p.cfg.AKSNodeSubnet,
		p.cfg.AKSPodSubnet,
	)
}
