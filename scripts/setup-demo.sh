#!/bin/bash
set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

DEMO_DIR="/tmp/stargate-demo"
KIND_CLUSTER_NAME="stargate-demo"

# Preserve user's PATH when running with sudo (for tools like kind in ~/.local/bin)
if [[ -n "$SUDO_USER" ]]; then
    USER_HOME=$(getent passwd "$SUDO_USER" | cut -d: -f6)
    export PATH="$PATH:$USER_HOME/.local/bin:$USER_HOME/bin:/usr/local/bin"
fi

echo -e "${GREEN}=== Stargate Simulator Demo Setup ===${NC}"

# Check if running as root
if [[ $EUID -ne 0 ]]; then
    echo -e "${RED}This script must be run as root (for networking setup)${NC}"
    exit 1
fi

# Function to check if a command exists
check_cmd() {
    if ! command -v "$1" &> /dev/null; then
        echo -e "${RED}✗ $1 is not installed${NC}"
        return 1
    fi
    echo -e "${GREEN}✓ $1 found${NC}"
    return 0
}

# Check prerequisites
echo -e "\n${YELLOW}Checking prerequisites...${NC}"
MISSING=0

check_cmd qemu-system-x86_64 || MISSING=1
check_cmd qemu-img || MISSING=1
check_cmd kind || MISSING=1
check_cmd kubectl || MISSING=1
check_cmd docker || MISSING=1

# Check for ISO generation tool
if command -v genisoimage &> /dev/null; then
    echo -e "${GREEN}✓ genisoimage found${NC}"
elif command -v mkisofs &> /dev/null; then
    echo -e "${GREEN}✓ mkisofs found${NC}"
elif command -v xorrisofs &> /dev/null; then
    echo -e "${GREEN}✓ xorrisofs found${NC}"
else
    echo -e "${RED}✗ No ISO generation tool found (need genisoimage, mkisofs, or xorrisofs)${NC}"
    MISSING=1
fi

# Check KVM
if [[ -e /dev/kvm ]]; then
    echo -e "${GREEN}✓ KVM available${NC}"
else
    echo -e "${RED}✗ KVM not available (/dev/kvm not found)${NC}"
    MISSING=1
fi

if [[ $MISSING -eq 1 ]]; then
    echo -e "\n${RED}Please install missing prerequisites and try again.${NC}"
    echo "On Ubuntu/Debian:"
    echo "  apt install qemu-system-x86 qemu-utils genisoimage"
    echo "  # For kind: https://kind.sigs.k8s.io/docs/user/quick-start/#installation"
    exit 1
fi

# Detect host IP
echo -e "\n${YELLOW}Detecting host IP...${NC}"
HOST_IP=$(ip route get 1 | awk '{print $7;exit}')
if [[ -z "$HOST_IP" ]]; then
    HOST_IP=$(hostname -I | awk '{print $1}')
fi
echo -e "${GREEN}Host IP: ${HOST_IP}${NC}"

# API server port (use 6444 to avoid conflicts with existing clusters on 6443)
API_PORT=6444

# Bridge IP - this is what VMs will use to connect to the kind cluster
# The kind cluster binds to 0.0.0.0, so VMs can reach it via the bridge gateway
BRIDGE_IP="192.168.100.1"

# Create or reuse kind cluster
echo -e "\n${YELLOW}Setting up kind cluster...${NC}"
if kind get clusters 2>/dev/null | grep -q "^${KIND_CLUSTER_NAME}$"; then
    echo "Kind cluster '${KIND_CLUSTER_NAME}' already exists"
else
    echo "Creating kind cluster '${KIND_CLUSTER_NAME}'..."
    
    # Create kind config - use 0.0.0.0 to avoid WSL2 port binding issues
    # The VMs will connect via the bridge network to the host IP
    # Add certSANs so the API server cert is valid for the bridge IP
    cat > /tmp/kind-config.yaml <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
networking:
  apiServerAddress: "0.0.0.0"
  apiServerPort: ${API_PORT}
nodes:
  - role: control-plane
    kubeadmConfigPatches:
    - |
      kind: ClusterConfiguration
      apiServer:
        certSANs:
        - "${BRIDGE_IP}"
        - "localhost"
        - "127.0.0.1"
EOF
    
    kind create cluster --name "${KIND_CLUSTER_NAME}" --config /tmp/kind-config.yaml
fi

# Always ensure kubeconfig is set up correctly (whether cluster was just created or already existed)
echo "Setting up kubeconfig..."
kind get kubeconfig --name "${KIND_CLUSTER_NAME}" | sed 's/0\.0\.0\.0/127.0.0.1/g' > /tmp/stargate-kubeconfig

# Copy to root's kubeconfig
mkdir -p ~/.kube
cp /tmp/stargate-kubeconfig ~/.kube/config

# Also copy to the original user's kubeconfig if running via sudo
if [[ -n "$SUDO_USER" ]]; then
    SUDO_USER_HOME=$(getent passwd "$SUDO_USER" | cut -d: -f6)
    mkdir -p "${SUDO_USER_HOME}/.kube"
    cp /tmp/stargate-kubeconfig "${SUDO_USER_HOME}/.kube/config"
    chown "$SUDO_USER:$SUDO_USER" "${SUDO_USER_HOME}/.kube/config"
    echo "Kubeconfig copied to ${SUDO_USER_HOME}/.kube/config"
fi

# Wait for cluster to be ready
echo "Waiting for cluster to be ready..."
kubectl wait --for=condition=Ready nodes --all --timeout=120s

# Update cluster-info and kubeadm-config to use bridge IP (so VMs can resolve the endpoint)
echo -e "\n${YELLOW}Updating cluster configuration for VM access...${NC}"
kubectl get cm cluster-info -n kube-public -o json 2>/dev/null | \
    jq ".data.kubeconfig |= gsub(\"https://${KIND_CLUSTER_NAME}-control-plane:6443\"; \"https://${BRIDGE_IP}:${API_PORT}\")" | \
    kubectl apply -f - 2>/dev/null || true

kubectl get cm kubeadm-config -n kube-system -o json 2>/dev/null | \
    jq ".data.ClusterConfiguration |= gsub(\"${KIND_CLUSTER_NAME}-control-plane:6443\"; \"${BRIDGE_IP}:${API_PORT}\")" | \
    kubectl apply -f - 2>/dev/null || true

# Remove cgroupRoot from kubelet-config (kind uses cgroup v1 setting that breaks cgroup v2 VMs)
echo "Patching kubelet-config to remove cgroupRoot..."
kubectl get cm kubelet-config -n kube-system -o json 2>/dev/null | \
    jq 'del(.metadata.managedFields, .metadata.annotations)' | \
    jq '.data.kubelet |= sub("cgroupRoot: /kubelet\n"; "")' | \
    kubectl apply -f - 2>/dev/null || true

# Fix kube-proxy configmap to use bridge IP instead of internal hostname
# (kube-proxy on VMs can't resolve stargate-demo-control-plane)
echo "Patching kube-proxy configmap..."
kubectl get cm kube-proxy -n kube-system -o json 2>/dev/null | \
    jq 'del(.metadata.managedFields, .metadata.annotations, .metadata.resourceVersion, .metadata.uid, .metadata.creationTimestamp)' | \
    jq ".data[\"kubeconfig.conf\"] |= gsub(\"${KIND_CLUSTER_NAME}-control-plane:6443\"; \"${BRIDGE_IP}:${API_PORT}\")" | \
    kubectl apply -f - 2>/dev/null || true

# Add route from kind container to VM bridge network
# This is needed for kindnet to add routes to worker node pod CIDRs
echo "Adding route from kind container to VM network..."
docker exec ${KIND_CLUSTER_NAME}-control-plane ip route add ${BRIDGE_SUBNET} via $(docker network inspect kind -f '{{range .IPAM.Config}}{{.Gateway}}{{end}}') 2>/dev/null || true

# Generate kubeadm join command
echo -e "\n${YELLOW}Generating kubeadm join command...${NC}"

# Get the join token
TOKEN=$(docker exec ${KIND_CLUSTER_NAME}-control-plane kubeadm token create 2>/dev/null)

# Get the CA cert hash
CA_HASH=$(docker exec ${KIND_CLUSTER_NAME}-control-plane sh -c \
    "openssl x509 -pubkey -in /etc/kubernetes/pki/ca.crt | openssl rsa -pubin -outform der 2>/dev/null | openssl dgst -sha256 -hex | sed 's/^.* //'")

# Use BRIDGE_IP so VMs on the bridge network can reach kind cluster
JOIN_CMD="kubeadm join ${BRIDGE_IP}:${API_PORT} --token ${TOKEN} --discovery-token-ca-cert-hash sha256:${CA_HASH}"
echo -e "${GREEN}Join command: ${JOIN_CMD}${NC}"

# Create demo directory
echo -e "\n${YELLOW}Creating demo manifests...${NC}"
mkdir -p "${DEMO_DIR}"

# Create Hardware CRs
cat > "${DEMO_DIR}/hardware.yaml" <<EOF
apiVersion: stargate.io/v1alpha1
kind: Hardware
metadata:
  name: sim-worker-001
  namespace: dc-simulator
spec:
  mac: "52:54:00:00:00:01"
  inventory:
    sku: "VM-2CPU-4GB"
    location: "simulator"
---
apiVersion: stargate.io/v1alpha1
kind: Hardware
metadata:
  name: sim-worker-002
  namespace: dc-simulator
spec:
  mac: "52:54:00:00:00:02"
  inventory:
    sku: "VM-2CPU-4GB"
    location: "simulator"
EOF

# Create Template CR with cloud-init
cat > "${DEMO_DIR}/template-k8s-worker.yaml" <<EOF
apiVersion: stargate.io/v1alpha1
kind: Template
metadata:
  name: k8s-worker
  namespace: dc-simulator
spec:
  osVersion: "ubuntu-22.04"
  osImage: "https://cloud-images.ubuntu.com/jammy/current/jammy-server-cloudimg-amd64.img"
  cloudInit: |
    #cloud-config
    hostname: sim-worker-001
    
    users:
      - name: ubuntu
        sudo: ALL=(ALL) NOPASSWD:ALL
        shell: /bin/bash
        lock_passwd: false
        plain_text_passwd: ubuntu
    
    ssh_pwauth: true
    
    # Add /etc/hosts entry for kind control-plane so kube-proxy/kindnet can resolve it
    manage_etc_hosts: false
    bootcmd:
      - echo "${BRIDGE_IP} stargate-demo-control-plane" >> /etc/hosts
    
    package_update: true
    package_upgrade: false
    
    packages:
      - apt-transport-https
      - ca-certificates
      - curl
      - gnupg
      - lsb-release
    
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
      
      # CNI config for kindnet - newer kindnet versions don't auto-create this
      # because the VM uses enp0s2 instead of eth0
      - path: /etc/cni/net.d/10-kindnet.conflist
        permissions: '0644'
        content: |
          {
            "cniVersion": "0.3.1",
            "name": "kindnet",
            "plugins": [
              {
                "type": "ptp",
                "mtu": 1500,
                "ipMasq": false,
                "ipam": {
                  "type": "host-local",
                  "dataDir": "/run/cni-ipam-state",
                  "routes": [{ "dst": "0.0.0.0/0" }],
                  "ranges": [[{ "subnet": "10.244.0.0/16" }]]
                }
              },
              {
                "type": "portmap",
                "capabilities": { "portMappings": true }
              }
            ]
          }
      
      - path: /tmp/install-k8s.sh
        permissions: '0755'
        content: |
          #!/bin/bash
          set -ex
          
          # Load kernel modules
          modprobe overlay
          modprobe br_netfilter
          sysctl --system
          
          # Disable swap
          swapoff -a
          sed -i '/swap/d' /etc/fstab
          
          # Install containerd
          curl -fsSL https://download.docker.com/linux/ubuntu/gpg | gpg --dearmor -o /etc/apt/keyrings/docker.gpg
          echo "deb [arch=amd64 signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/ubuntu \$(lsb_release -cs) stable" > /etc/apt/sources.list.d/docker.list
          apt-get update
          apt-get install -y containerd.io
          
          # Configure containerd
          mkdir -p /etc/containerd
          containerd config default > /etc/containerd/config.toml
          sed -i 's/SystemdCgroup = false/SystemdCgroup = true/' /etc/containerd/config.toml
          systemctl restart containerd
          systemctl enable containerd
          
          # Install Kubernetes components
          curl -fsSL https://pkgs.k8s.io/core:/stable:/v1.29/deb/Release.key | gpg --dearmor -o /etc/apt/keyrings/kubernetes-apt-keyring.gpg
          echo 'deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] https://pkgs.k8s.io/core:/stable:/v1.29/deb/ /' > /etc/apt/sources.list.d/kubernetes.list
          apt-get update
          apt-get install -y kubelet kubeadm kubectl
          apt-mark hold kubelet kubeadm kubectl
          
          # Join the cluster
          ${JOIN_CMD}
    
    runcmd:
      - /tmp/install-k8s.sh
    
    final_message: "Kubernetes worker node setup complete after \$UPTIME seconds"
EOF

# Create Job CRs
cat > "${DEMO_DIR}/job.yaml" <<EOF
apiVersion: stargate.io/v1alpha1
kind: Job
metadata:
  name: repave-sim-worker-001
  namespace: dc-simulator
spec:
  hardwareRef:
    name: sim-worker-001
  templateRef:
    name: k8s-worker
  operation: repave
EOF

cat > "${DEMO_DIR}/job-002.yaml" <<EOF
apiVersion: stargate.io/v1alpha1
kind: Job
metadata:
  name: repave-sim-worker-002
  namespace: dc-simulator
spec:
  hardwareRef:
    name: sim-worker-002
  templateRef:
    name: k8s-worker
  operation: repave
EOF

# Create namespace
cat > "${DEMO_DIR}/namespace.yaml" <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: dc-simulator
EOF

echo -e "${GREEN}Demo manifests created in ${DEMO_DIR}/${NC}"

# Summary
echo -e "\n${GREEN}=== Setup Complete ===${NC}"
echo ""
echo "Demo files created:"
echo "  ${DEMO_DIR}/namespace.yaml"
echo "  ${DEMO_DIR}/hardware.yaml"
echo "  ${DEMO_DIR}/template-k8s-worker.yaml"
echo "  ${DEMO_DIR}/job.yaml"
echo ""
echo "Next steps:"
echo ""
echo "1. Build the simulator:"
echo "   make build"
echo ""
echo "2. Install CRDs:"
echo "   kubectl apply -f config/crd/bases/"
echo ""
echo "3. Create namespace and resources:"
echo "   kubectl apply -f ${DEMO_DIR}/namespace.yaml"
echo "   kubectl apply -f ${DEMO_DIR}/hardware.yaml"
echo "   kubectl apply -f ${DEMO_DIR}/template-k8s-worker.yaml"
echo ""
echo "4. Start the simulator controller (as root):"
echo "   sudo ./bin/simulator"
echo ""
echo "5. Trigger a repave job:"
echo "   kubectl apply -f ${DEMO_DIR}/job.yaml"
echo ""
echo "6. Watch progress:"
echo "   kubectl get jobs.stargate.io -n dc-simulator -w"
echo "   sudo tail -f /var/lib/stargate/vms/sim-worker-001/serial.log"
echo "   kubectl get nodes -w"
