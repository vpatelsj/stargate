#!/usr/bin/env bash
set -e

if [ -z "$1" ]; then
  echo "Usage: $0 <bootstrap-token>"
  echo "  bootstrap-token: The kubelet bootstrap token for TLS bootstrapping"
  exit 1
fi

BOOTSTRAP_TOKEN="$1"

function logValue {
  local NAME=$1
  local VALUE=$2

  printf "%25s: %s\n" "$NAME" "$VALUE"
}

function logProgress {
  echo "$1"
}

function logProgress2 {
  echo "  - $1"
}

NODE_NAME=$(hostname)
logValue "NODE_NAME" $NODE_NAME
logProgress "Linking resolv.conf"
ln -sf /run/systemd/resolve/resolv.conf /etc/resolv.conf

logProgress "Creating Directories"
mkdir -p /var/lib/cni
mkdir -p /opt/cni/bin
mkdir -p /etc/cni/net.d
mkdir -p /etc/kubernetes/volumeplugins
mkdir -p /etc/kubernetes/certs
mkdir -p /etc/containerd
mkdir -p /usr/lib/systemd/system/kubelet.service.d
mkdir -p /var/lib/kubelet

logProgress "Creating /usr/lib/systemd/system/containerd.service"

tee /usr/lib/systemd/system/containerd.service >/dev/null <<EOF
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
# Having non-zero Limit*s causes performance problems due to accounting overhead
# in the kernel. We recommend using cgroups to do container-local accounting.
LimitNPROC=infinity
LimitCORE=infinity
LimitNOFILE=infinity
# Comment TasksMax if your systemd version does not supports it.
# Only systemd 226 and above support this version.
TasksMax=infinity
OOMScoreAdjust=-999
[Install]
WantedBy=multi-user.target
EOF

logProgress "Creating /etc/containerd/config.toml"

tee /etc/containerd/config.toml >/dev/null <<EOF
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
		[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.untrusted]
			runtime_type = "io.containerd.runc.v2"
		[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.untrusted.options]
			BinaryName = "/usr/bin/runc"
	[plugins."io.containerd.grpc.v1.cri".cni]
		bin_dir = "/opt/cni/bin"
		conf_dir = "/etc/cni/net.d"
		conf_template = "/etc/containerd/kubenet_template.conf"
	[plugins."io.containerd.grpc.v1.cri".registry]
		config_path = "/etc/containerd/certs.d"
	[plugins."io.containerd.grpc.v1.cri".registry.headers]
		X-Meta-Source-Client = ["azure/aks"]
[metrics]
	address = "0.0.0.0:10257"
EOF

logProgress "Creating /etc/containerd/kubenet_template.conf"

# for kubenet
tee /etc/containerd/kubenet_template.conf >/dev/null <<'EOF'
{
    "cniVersion": "0.3.1",
    "name": "kubenet",
    "plugins": [{
    "type": "bridge",
    "bridge": "cbr0",
    "mtu": 1500,
    "addIf": "eth0",
    "isGateway": true,
    "ipMasq": false,
    "promiscMode": true,
    "hairpinMode": false,
    "ipam": {
        "type": "host-local",
        "ranges": [{{range $i, $range := .PodCIDRRanges}}{{if $i}}, {{end}}[{"subnet": "{{$range}}"}]{{end}}],
        "routes": [{{range $i, $route := .Routes}}{{if $i}}, {{end}}{"dst": "{{$route}}"}{{end}}]
    }
    },
    {
    "type": "portmap",
    "capabilities": {"portMappings": true},
    "externalSetMarkChain": "KUBE-MARK-MASQ"
    }]
}
EOF

logProgress "Creating /etc/sysctl.d/999-sysctl-aks.conf"

tee /etc/sysctl.d/999-sysctl-aks.conf >/dev/null <<EOF
# container networking
net.ipv4.ip_forward = 1
net.ipv4.conf.all.forwarding = 1
net.ipv6.conf.all.forwarding = 1
net.bridge.bridge-nf-call-iptables = 1

# refer to https://github.com/kubernetes/kubernetes/blob/75d45bdfc9eeda15fb550e00da662c12d7d37985/pkg/kubelet/cm/container_manager_linux.go#L359-L397
vm.overcommit_memory = 1
kernel.panic = 10
kernel.panic_on_oops = 1
# to ensure node stability, we set this to the PID_MAX_LIMIT on 64-bit systems: refer to https://kubernetes.io/docs/concepts/policy/pid-limiting/
kernel.pid_max = 4194304
# https://github.com/Azure/AKS/issues/772
fs.inotify.max_user_watches = 1048576
# Ubuntu 22.04 has inotify_max_user_instances set to 128, where as Ubuntu 18.04 had 1024. 
fs.inotify.max_user_instances = 1024

# This is a partial workaround to this upstream Kubernetes issue:
# https://github.com/kubernetes/kubernetes/issues/41916#issuecomment-312428731
net.ipv4.tcp_retries2=8
net.core.message_burst=80
net.core.message_cost=40
net.core.somaxconn=16384
net.ipv4.tcp_max_syn_backlog=16384
net.ipv4.neigh.default.gc_thresh1=4096
net.ipv4.neigh.default.gc_thresh2=8192
net.ipv4.neigh.default.gc_thresh3=16384
EOF

logProgress "Creating /etc/default/kubelet"

# adust flags as desired
tee /etc/default/kubelet >/dev/null <<EOF
KUBELET_NODE_LABELS="\
kubernetes.azure.com/cluster=MC_starlab_phynet-stretch_westus2,\
kubernetes.azure.com/agentpool=starlab,\
kubernetes.azure.com/mode=user,\
kubernetes.azure.com/role=agent,\
node.kubernetes.io/exclude-from-external-load-balancers=true,\
kubernetes.azure.com/managed=false,\
kubernetes.azure.com/stretch=true\
"
KUBELET_FLAGS="\
  --address=0.0.0.0 \
  --anonymous-auth=false \
  --authentication-token-webhook=true \
  --authorization-mode=Webhook \
  --cgroup-driver=systemd \
  --cgroups-per-qos=true \
  --client-ca-file=/etc/kubernetes/certs/ca.crt \
  --cluster-dns=10.0.0.10 \
  --cluster-domain=cluster.local \
  --enforce-node-allocatable=pods \
  --event-qps=0  \
  --eviction-hard=memory.available<100Mi,nodefs.available<10%,nodefs.inodesFree<5%  \
  --kube-reserved=cpu=100m,memory=1000Mi  \
  --image-gc-high-threshold=85  \
  --image-gc-low-threshold=80  \
  --max-pods=110  \
  --node-status-update-frequency=10s  \
  --pod-infra-container-image=mcr.microsoft.com/oss/kubernetes/pause:3.6  \
  --pod-max-pids=-1  \
  --protect-kernel-defaults=true  \
  --read-only-port=0  \
  --resolv-conf=/run/systemd/resolve/resolv.conf  \
  --streaming-connection-idle-timeout=4h  \
  --tls-cipher-suites=TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,TLS_RSA_WITH_AES_256_GCM_SHA384,TLS_RSA_WITH_AES_128_GCM_SHA256 \
  "
EOF

logProgress "Creating /usr/lib/systemd/system/kubelet.service.d/10-containerd.conf"

# can simplify this + 2 following files by merging together
tee /usr/lib/systemd/system/kubelet.service.d/10-containerd.conf >/dev/null <<'EOF'
[Service]
Environment=KUBELET_CONTAINERD_FLAGS="--runtime-request-timeout=15m --container-runtime-endpoint=unix:///run/containerd/containerd.sock"
EOF

logProgress "Creating /usr/lib/systemd/system/kubelet.service"

tee /usr/lib/systemd/system/kubelet.service >/dev/null <<'EOF'
[Unit]
Description=Kubelet
ConditionPathExists=/usr/bin/kubelet
[Service]
Restart=always
EnvironmentFile=/etc/default/kubelet
SuccessExitStatus=143
# Ace does not recall why this is done
ExecStartPre=/bin/bash -c "if [ $(mount | grep \"/var/lib/kubelet\" | wc -l) -le 0 ] ; then /bin/mount --bind /var/lib/kubelet /var/lib/kubelet ; fi"
ExecStartPre=/bin/mount --make-shared /var/lib/kubelet
ExecStartPre=-/sbin/ebtables -t nat --list
ExecStartPre=-/sbin/iptables -t nat --numeric --list
ExecStart=/usr/bin/kubelet \
        --enable-server \
        --node-labels="${KUBELET_NODE_LABELS}" \
        --v=2 \
        --volume-plugin-dir=/etc/kubernetes/volumeplugins \
        $KUBELET_TLS_BOOTSTRAP_FLAGS \
        $KUBELET_CONFIG_FILE_FLAGS \
        $KUBELET_CONTAINERD_FLAGS \
        $KUBELET_FLAGS 
[Install]
WantedBy=multi-user.target
EOF

logProgress "Creating /usr/lib/systemd/system/kubelet.service.d/10-tlsbootstrap.conf"

tee /usr/lib/systemd/system/kubelet.service.d/10-tlsbootstrap.conf >/dev/null <<'EOF'
[Service]
Environment=KUBELET_TLS_BOOTSTRAP_FLAGS="--kubeconfig /var/lib/kubelet/kubeconfig --bootstrap-kubeconfig /var/lib/kubelet/bootstrap-kubeconfig"
EOF

logProgress "Creating /var/lib/kubelet/bootstrap-kubeconfig"

tee /var/lib/kubelet/bootstrap-kubeconfig >/dev/null <<EOF
apiVersion: v1
kind: Config
clusters:
- name: localcluster
  cluster:
    certificate-authority: /etc/kubernetes/certs/ca.crt
    server: "https://phynet-str-starlab-864302-qejwbnik.hcp.westus2.azmk8s.io"
users:
- name: kubelet-bootstrap
  user:
    token: "${BOOTSTRAP_TOKEN}"
contexts:
- context:
    cluster: localcluster
    user: kubelet-bootstrap
  name: bootstrap-context
current-context: bootstrap-context
EOF

logProgress "Creating /etc/kubernetes/certs/ca.crt"

KUBE_CA_PATH="/etc/kubernetes/certs/ca.crt"
touch "${KUBE_CA_PATH}"
chmod 0600 "${KUBE_CA_PATH}"
chown root:root "${KUBE_CA_PATH}"
echo 'LS0tLS1CRUdJTiBDRVJUSUZJQ0FURS0tLS0tCk1JSUU2RENDQXRDZ0F3SUJBZ0lRTjkzdVBpNXZXaFRaZldWUklUQ3ZUakFOQmdrcWhraUc5dzBCQVFzRkFEQU4KTVFzd0NRWURWUVFERXdKallUQWdGdzB5TmpBeE1UUXlNelV6TlRKYUdBOHlNRFUyTURFeE5UQXdNRE0xTWxvdwpEVEVMTUFrR0ExVUVBeE1DWTJFd2dnSWlNQTBHQ1NxR1NJYjNEUUVCQVFVQUE0SUNEd0F3Z2dJS0FvSUNBUURxCnJ1NmlqaFhEVkptR2dHdGVrTEVPWksxbGNkT0lwSWYvZ3RodHQ4MTU0T1VCK1ZrYjZtTldqV24rSTRCbFhTSTkKREhSMkpJNE5qMSt1eXpzbSsydU9kRDR5SnlUZnJxSmZpQjNsM21GUTRhMWtFSGorQXVOSUNIdkJtWnp0QU5RSQpMZWwrOXhnK2RFSk83TU5tTUxIdk1qZUU3bUR4NmxOZjZQUlp2d0tRUnd6bW10QW9UM0M1cUpYYnlGZnRGSjEvCjkwb3BOT080NlAzaXpPeHJlRnI5ZnNacUM5c0tNaEZLc01CVlg3UVNBRUQ2MzlwSUZwc0FNWEN4OUF0RXlNUTYKUjZZRkRubURNRm0zOWp3KzN0K1p0cnFNbU5wbUhzeGhSUkhRTmI4UEtHQ20weUlzZmd1cWJhc2pFNEdKMkJZTAo1NWNsSDBoZ3BwSU1JUXN2WklTMzcxazlMVStJdHY0c0hIb0NRdy9lckxIbWdYQW51ZTk2WktqT09IT3BRenViCk1HblczdWtWTUl3TTl2cHpEbjYwZ1cxVDFWQ1VZRHM5NHh5VmFpQ2dzKysxeGg5Wmc0akxUanY0cTNaeDgyVWkKVlNzbkdueFoxa1UzV0paNU03TUs0T1RaNlJ0cDJmOERkY0NSeXVwcEFWRHdwTHZCWW5SSnNsTHZIT29tb3QzKwpyWGMxYmJtTmp1VDJUQTdyR3BLTjZzRUJNYzVsQWl0NjFXME9wci92ajBBVTg1S1l6aVh3OUtLdjU4NDBRTGxJCkVPS2lUZkZkdDZxVTVlWmRwaVBsdTVhdkNBem5UQkdBaVNRcFFiMjUzdHBmYVRSWlFoaGRwMkhUdWtvRCtubHYKN2htWU1pMElIQm84VWs4MWQwdmwxZ21xdGlxbXFOeGdxY1ZPZEVLTmdRSURBUUFCbzBJd1FEQU9CZ05WSFE4QgpBZjhFQkFNQ0FxUXdEd1lEVlIwVEFRSC9CQVV3QXdFQi96QWRCZ05WSFE0RUZnUVV2bWVqUHhFV3pxOWIxSVVuClhPaW8rZDh6aWxZd0RRWUpLb1pJaHZjTkFRRUxCUUFEZ2dJQkFJNXN0RVF5ZkRtbm5XdE04NUJBRTRMQ1dmODIKZFFCdmVIK0prU0hkYjZ4UTNkUFV4dkxmNjJ4T2I5cUoyaVdFTUQwNUJETUxnc0QvcGZvSS9GTjlocE5YODdEegpCeGtNelB3QmpEb0FVYldjcVpJSlplWitCSWtHek8zR1BTSTlsQnlhWVlwY1haL3h0dlUwczEyYkNoT0xzVXc2CjBQZExpUnhDRWF5aXVZZ3M1bTFNdFB3alB2Y2Y5Nnc1UDNxNUI0YmN0RUdZTVJNMHcyU1BqblhDaEQ1RzUxdVAKdlJIK1RiaE8vYkpaVlV3TDZDekVaSUhuQ3pFUEdLRXFTUDhKMkR5c21naEcrUTZNb2wxN213VGlqRXE1WWZ6TgpPcmx4cTU3emcyb21DZS85RkRwVUdwTW16OTh2M3RGK2FsdTFSb0R5eFhFMEg5NjIydVJ6RElXaTE3OG5KYmRlCmF1bnl5aUJ6ZUhXSTBFVms3ZEFqRHd3MUJxRzQrNHRFT1hTdjJHZ0RLbnplSWtnZ1M2RVh6eDZLY2JoR2xiOGwKTEhPektRUkhLdVVWdy9qYmR3S0hNbDZ3TUl1MWp3c0pqWXZ0K2IvWm5QWnBaMEIxVnBpMk5nc0luY1JZU2Q4SwpUL2VSQnFWZ2hOMG5FaEY4YWZFRTVvSmEraGluQTJoOC9yYmdEbjlkeXJjNVZNT1p2ZVUwQnlQTXJENE95REFECnVCYzBlVlRVenF6ZHI2YnNBeXE3TFNqS3U1cXlnSXlyNXhubE5aU3V1TG5yaXRWdXR1dzd1WmZXaVF5U25uSWUKdE1Da0EvZ1hoRm1jV3hPbGlRR0M2RkRVOGdrbzR2OFJ1V3NibzYzZTBEWTYrZzlaKzJaQjJOOXdrOTJTN2tkKwozc0UwY0lzTHJvWkkvWTFTCi0tLS0tRU5EIENFUlRJRklDQVRFLS0tLS0K' | base64 -d >/etc/kubernetes/certs/ca.crt

logProgress "Creating /etc/kubernetes/azure.json"

AZURE_JSON_PATH="/etc/kubernetes/azure.json"
touch "${AZURE_JSON_PATH}"
chmod 0600 "${AZURE_JSON_PATH}"
chown root:root "${AZURE_JSON_PATH}"

logProgress "Enabling containerd and kubelet serfvices"

sysctl --system
systemctl enable --now containerd
systemctl enable --now kubelet

# sanity check? might be uninitialized at this point
# timeout 30s grep -q 'NodeReady' <(journalctl -u kubelet -f --no-tail)

echo
echo
echo The node has now been configured and both containerd and kubelet have been started. Sometimes the node needs rebooting after configuration, and
echo sometimes it needs draining of pods. To do a drain, run these two commands to drain all pods then make the node ready for the pods again.
echo \* kubectl drain $NODE_NAME --force --ignore-daemonsets --delete-emptydir-data
echo \* kubectl uncordon $NODE_NAME
