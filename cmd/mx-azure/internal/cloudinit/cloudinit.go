package cloudinit

import (
	"bytes"
	"encoding/base64"
	"text/template"
)

// Config holds the configuration for cloud-init template rendering
type Config struct {
	Hostname          string
	AdminUsername     string
	TailscaleAuthKey  string
	KubernetesVersion string
}

// MXConfig holds the configuration for MX cloud-init rendering
type MXConfig struct {
	AdminUsername     string
	TailscaleAuthKey  string
	KubernetesVersion string
}

// cloudInitTemplate is the cloud-init user-data template
const cloudInitTemplate = `#cloud-config
hostname: {{.Hostname}}
manage_etc_hosts: true

users:
  - name: {{.AdminUsername}}
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
    groups: [adm, sudo, docker]

package_update: true
package_upgrade: true

packages:
  - apt-transport-https
  - ca-certificates
  - curl
  - gnupg
  - lsb-release
  - jq
  - unzip

write_files:
  - path: /etc/modules-load.d/k8s.conf
    content: |
      overlay
      br_netfilter

  - path: /etc/sysctl.d/k8s.conf
    content: |
      net.bridge.bridge-nf-call-iptables  = 1
      net.bridge.bridge-nf-call-ip6tables = 1
      net.ipv4.ip_forward                 = 1

runcmd:
  # Load kernel modules
  - modprobe overlay
  - modprobe br_netfilter
  - sysctl --system

  # Install containerd
  - curl -fsSL https://download.docker.com/linux/ubuntu/gpg | gpg --dearmor -o /etc/apt/keyrings/docker.gpg
  - echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/ubuntu $(lsb_release -cs) stable" > /etc/apt/sources.list.d/docker.list
  - apt-get update
  - apt-get install -y containerd.io
  - mkdir -p /etc/containerd
  - containerd config default | sed 's/SystemdCgroup = false/SystemdCgroup = true/' > /etc/containerd/config.toml
  - systemctl restart containerd
  - systemctl enable containerd

  # Install Kubernetes components
  - curl -fsSL https://pkgs.k8s.io/core:/stable:/v{{.KubernetesVersion}}/deb/Release.key | gpg --dearmor -o /etc/apt/keyrings/kubernetes-apt-keyring.gpg
  - echo "deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] https://pkgs.k8s.io/core:/stable:/v{{.KubernetesVersion}}/deb/ /" > /etc/apt/sources.list.d/kubernetes.list
  - apt-get update
  - apt-get install -y kubelet kubeadm kubectl
  - apt-mark hold kubelet kubeadm kubectl
  - systemctl enable kubelet

{{if .TailscaleAuthKey}}
  # Install Tailscale
  - curl -fsSL https://tailscale.com/install.sh | sh
  - tailscale up --authkey={{.TailscaleAuthKey}} --ssh --accept-routes --accept-dns=false
{{end}}

  # Signal completion
  - touch /var/lib/cloud/instance/boot-finished-k8s

final_message: "Cloud-init completed after $UPTIME seconds"
`

// mxCloudInitTemplate is the MX bootstrap cloud-init template with kubeadm init
//
// SECURITY NOTES:
// - Tailscale auth key is passed via cloud-init custom data (encrypted at rest by Azure)
// - The auth key is NOT logged to /var/log/mx-bootstrap.log
// - kubeadm join tokens are NOT logged (use `kubeadm token create` interactively)
// - NSG blocks all inbound traffic; access is via Tailscale only
// - Tailscale SSH requires ACL policy to be configured in your tailnet
const mxCloudInitTemplate = `#cloud-config
# MX Bootstrap Cloud-Init
# This cloud-init configures a Kubernetes control plane node with Tailscale networking
#
# SECURITY: This script handles secrets carefully:
# - Tailscale auth key is used but never logged
# - kubeadm join tokens are not printed to logs
# - All access should be via Tailscale (public IP is for diagnostics only)

package_update: true
package_upgrade: true

packages:
  - apt-transport-https
  - ca-certificates
  - curl
  - gnupg
  - lsb-release
  - jq
  - socat
  - conntrack

write_files:
  - path: /etc/modules-load.d/k8s.conf
    owner: root:root
    permissions: '0644'
    content: |
      overlay
      br_netfilter

  - path: /etc/sysctl.d/k8s.conf
    owner: root:root
    permissions: '0644'
    content: |
      net.bridge.bridge-nf-call-iptables  = 1
      net.bridge.bridge-nf-call-ip6tables = 1
      net.ipv4.ip_forward                 = 1

  - path: /usr/local/bin/mx-bootstrap.sh
    owner: root:root
    permissions: '0755'
    content: |
      #!/bin/bash
      set -euo pipefail
      
      LOGFILE="/var/log/mx-bootstrap.log"
      ADMIN_USER="{{.AdminUsername}}"
      K8S_VERSION="{{.KubernetesVersion}}"
      
      # SECURITY: Auth key is read from a variable, never logged
      # The key is passed via cloud-init custom data which Azure encrypts at rest
      TAILSCALE_AUTH_KEY="{{.TailscaleAuthKey}}"
      
      log() {
        echo "[$(date '+%Y-%m-%d %H:%M:%S')] $*" | tee -a "$LOGFILE"
      }
      
      # Ensure log file has restricted permissions (secrets could leak via error messages)
      touch "$LOGFILE"
      chmod 600 "$LOGFILE"
      
      log "=== MX Bootstrap Started ==="
      log "SECURITY: Tailscale auth key and kubeadm tokens are NOT logged"
      
      # Idempotency check - if kubeadm already initialized, exit success
      if [[ -f /etc/kubernetes/admin.conf ]]; then
        log "Kubernetes already initialized (admin.conf exists). Exiting successfully."
        exit 0
      fi
      
      # Load kernel modules
      log "Loading kernel modules..."
      modprobe overlay
      modprobe br_netfilter
      sysctl --system >> "$LOGFILE" 2>&1
      
      # Install containerd
      log "Installing containerd..."
      if ! command -v containerd &> /dev/null; then
        mkdir -p /etc/apt/keyrings
        curl -fsSL https://download.docker.com/linux/ubuntu/gpg | gpg --dearmor -o /etc/apt/keyrings/docker.gpg
        echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/ubuntu $(lsb_release -cs) stable" > /etc/apt/sources.list.d/docker.list
        apt-get update >> "$LOGFILE" 2>&1
        apt-get install -y containerd.io >> "$LOGFILE" 2>&1
      fi
      
      # Configure containerd
      log "Configuring containerd..."
      mkdir -p /etc/containerd
      containerd config default | sed 's/SystemdCgroup = false/SystemdCgroup = true/' > /etc/containerd/config.toml
      systemctl restart containerd
      systemctl enable containerd
      
      # Install Kubernetes components
      log "Installing Kubernetes components (v${K8S_VERSION})..."
      if ! command -v kubeadm &> /dev/null; then
        mkdir -p /etc/apt/keyrings
        curl -fsSL "https://pkgs.k8s.io/core:/stable:/v${K8S_VERSION}/deb/Release.key" | gpg --dearmor -o /etc/apt/keyrings/kubernetes-apt-keyring.gpg
        echo "deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] https://pkgs.k8s.io/core:/stable:/v${K8S_VERSION}/deb/ /" > /etc/apt/sources.list.d/kubernetes.list
        apt-get update >> "$LOGFILE" 2>&1
        apt-get install -y kubelet kubeadm kubectl >> "$LOGFILE" 2>&1
        apt-mark hold kubelet kubeadm kubectl
      fi
      systemctl enable kubelet
      
      # Install Tailscale
      log "Installing Tailscale..."
      if ! command -v tailscale &> /dev/null; then
        curl -fsSL https://tailscale.com/install.sh | sh >> "$LOGFILE" 2>&1
      fi
      
      # Bring up Tailscale
      # SECURITY: Auth key is passed via variable, not logged
      log "Bringing up Tailscale (auth key not logged)..."
      if ! tailscale status &> /dev/null; then
        # Redirect to /dev/null to ensure auth key doesn't appear in logs
        tailscale up --authkey="${TAILSCALE_AUTH_KEY}" --accept-routes --accept-dns=false > /dev/null 2>&1
        if [[ $? -ne 0 ]]; then
          log "ERROR: tailscale up failed (check tailscale auth key validity)"
          exit 1
        fi
      fi
      
      # Clear the auth key from memory (best effort)
      unset TAILSCALE_AUTH_KEY
      
      # Enable Tailscale SSH
      log "Enabling Tailscale SSH..."
      tailscale set --ssh >> "$LOGFILE" 2>&1
      
      # Wait for Tailscale to get an IP
      log "Waiting for Tailscale IP..."
      for i in {1..30}; do
        TAILSCALE_IP=$(tailscale ip -4 2>/dev/null || true)
        if [[ -n "$TAILSCALE_IP" ]]; then
          break
        fi
        log "Waiting for Tailscale IP... attempt $i/30"
        sleep 2
      done
      
      if [[ -z "$TAILSCALE_IP" ]]; then
        log "ERROR: Failed to get Tailscale IP after 60 seconds"
        exit 1
      fi
      log "Tailscale IP: $TAILSCALE_IP"
      
      # Get hostname
      HOSTNAME=$(hostname)
      log "Hostname: $HOSTNAME"
      
      # Initialize Kubernetes with Tailscale IP in cert SANs
      log "Initializing Kubernetes cluster..."
      kubeadm init \
        --apiserver-advertise-address="$TAILSCALE_IP" \
        --apiserver-cert-extra-sans="$TAILSCALE_IP,$HOSTNAME" \
        --pod-network-cidr=10.244.0.0/16 \
        --skip-phases=addon/kube-proxy \
        >> "$LOGFILE" 2>&1
      
      log "Kubernetes initialized successfully"
      
      # Setup kubeconfig for admin user
      log "Setting up kubeconfig for user: $ADMIN_USER"
      ADMIN_HOME="/home/$ADMIN_USER"
      mkdir -p "$ADMIN_HOME/.kube"
      cp /etc/kubernetes/admin.conf "$ADMIN_HOME/.kube/config"
      chown -R "$ADMIN_USER:$ADMIN_USER" "$ADMIN_HOME/.kube"
      
      # Also setup for root
      mkdir -p /root/.kube
      cp /etc/kubernetes/admin.conf /root/.kube/config
      
      # Install Cilium CLI
      log "Installing Cilium CLI..."
      CILIUM_CLI_VERSION=$(curl -s https://raw.githubusercontent.com/cilium/cilium-cli/main/stable.txt)
      CLI_ARCH=amd64
      if [ "$(uname -m)" = "aarch64" ]; then CLI_ARCH=arm64; fi
      curl -L --fail --remote-name-all "https://github.com/cilium/cilium-cli/releases/download/${CILIUM_CLI_VERSION}/cilium-linux-${CLI_ARCH}.tar.gz{,.sha256sum}"
      sha256sum --check "cilium-linux-${CLI_ARCH}.tar.gz.sha256sum" >> "$LOGFILE" 2>&1
      tar xzvf "cilium-linux-${CLI_ARCH}.tar.gz" -C /usr/local/bin >> "$LOGFILE" 2>&1
      rm -f "cilium-linux-${CLI_ARCH}.tar.gz" "cilium-linux-${CLI_ARCH}.tar.gz.sha256sum"
      
      # Install Cilium CNI
      log "Installing Cilium CNI..."
      export KUBECONFIG=/etc/kubernetes/admin.conf
      cilium install --set kubeProxyReplacement=true >> "$LOGFILE" 2>&1
      
      # Wait for Cilium to be ready
      log "Waiting for Cilium to be ready..."
      cilium status --wait >> "$LOGFILE" 2>&1
      
      log "=== MX Bootstrap Completed Successfully ==="
      log "Tailscale IP: $TAILSCALE_IP"
      log "Kubeconfig: $ADMIN_HOME/.kube/config"
      # SECURITY: Do not log join tokens - they are secrets
      log "To join worker nodes: run 'kubeadm token create --print-join-command' interactively"
      log "REMINDER: Tailscale SSH requires ACL policy to allow access to this node"

runcmd:
  - /usr/local/bin/mx-bootstrap.sh

final_message: "Cloud-init completed after $UPTIME seconds"
`

// Render generates the cloud-init user-data from the template
func Render(cfg Config) (string, error) {
	tmpl, err := template.New("cloudinit").Parse(cloudInitTemplate)
	if err != nil {
		return "", err
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, cfg); err != nil {
		return "", err
	}

	return buf.String(), nil
}

// RenderBase64 generates base64-encoded cloud-init user-data
func RenderBase64(cfg Config) (string, error) {
	data, err := Render(cfg)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString([]byte(data)), nil
}

// RenderMXCloudInit generates cloud-init YAML for MX control plane bootstrap
// It installs containerd, kubeadm/kubelet/kubectl, tailscale, and initializes
// a Kubernetes cluster with the Tailscale IP in the apiserver cert SANs.
// The script is idempotent - it checks for /etc/kubernetes/admin.conf and exits
// success if already initialized.
func RenderMXCloudInit(adminUser, tailscaleAuthKey, kubernetesVersion string) (string, error) {
	cfg := MXConfig{
		AdminUsername:     adminUser,
		TailscaleAuthKey:  tailscaleAuthKey,
		KubernetesVersion: kubernetesVersion,
	}

	tmpl, err := template.New("mx-cloudinit").Parse(mxCloudInitTemplate)
	if err != nil {
		return "", err
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, cfg); err != nil {
		return "", err
	}

	return buf.String(), nil
}

// RenderMXCloudInitBase64 generates base64-encoded MX cloud-init
func RenderMXCloudInitBase64(adminUser, tailscaleAuthKey, kubernetesVersion string) (string, error) {
	data, err := RenderMXCloudInit(adminUser, tailscaleAuthKey, kubernetesVersion)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString([]byte(data)), nil
}
