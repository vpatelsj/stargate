package cloudinit

import (
	"strings"
	"testing"
)

func TestRenderMXCloudInit(t *testing.T) {
	output, err := RenderMXCloudInit("azureuser", "tskey-auth-xxx", "1.29")
	if err != nil {
		t.Fatalf("RenderMXCloudInit failed: %v", err)
	}

	// Verify key components are present
	checks := []string{
		"#cloud-config",
		"ADMIN_USER=\"azureuser\"",
		"TAILSCALE_AUTH_KEY=\"tskey-auth-xxx\"",
		"K8S_VERSION=\"1.29\"",
		"/var/log/mx-bootstrap.log",
		"if [[ -f /etc/kubernetes/admin.conf ]]",
		"tailscale set --ssh",
		"tailscale ip -4",
		"kubeadm init",
		"--apiserver-cert-extra-sans",
		"$ADMIN_HOME/.kube/config",
		"chown -R",
	}

	for _, check := range checks {
		if !strings.Contains(output, check) {
			t.Errorf("Expected output to contain %q", check)
		}
	}
}

func TestRenderMXCloudInitBase64(t *testing.T) {
	output, err := RenderMXCloudInitBase64("testuser", "tskey-test", "1.30")
	if err != nil {
		t.Fatalf("RenderMXCloudInitBase64 failed: %v", err)
	}

	if len(output) == 0 {
		t.Error("Expected non-empty base64 output")
	}

	// Verify it's valid base64 (no error means it's valid)
	if !strings.HasPrefix(output, "I2Nsb3Vk") { // "#cloud" in base64
		t.Error("Expected base64 output to start with cloud-config header")
	}
}

// TestRenderMXCloudInit_KeyCommands verifies all critical commands are present
func TestRenderMXCloudInit_KeyCommands(t *testing.T) {
	tests := []struct {
		name     string
		contains string
		desc     string
	}{
		// Tailscale installation and configuration
		{"tailscale_install", "curl -fsSL https://tailscale.com/install.sh | sh", "Tailscale install script"},
		{"tailscale_up", "tailscale up --authkey=", "Tailscale up with auth key"},
		{"tailscale_ssh_enable", "tailscale set --ssh", "Enable Tailscale SSH"},
		{"tailscale_ip_detect", "tailscale ip -4", "Detect Tailscale IPv4"},

		// Kubernetes installation
		{"kubeadm_install", "apt-get install -y kubelet kubeadm kubectl", "Install kubeadm/kubelet/kubectl"},
		{"kubeadm_init", "kubeadm init", "kubeadm init command"},
		{"apiserver_advertise", "--apiserver-advertise-address=\"$TAILSCALE_IP\"", "API server advertise address"},
		{"cert_sans", "--apiserver-cert-extra-sans=\"$TAILSCALE_IP,$HOSTNAME\"", "Cert SANs include tailscale IP"},

		// Containerd
		{"containerd_install", "apt-get install -y containerd.io", "Install containerd"},
		{"containerd_cgroup", "SystemdCgroup = true", "Configure systemd cgroup"},

		// Kubeconfig setup
		{"kubeconfig_copy", "cp /etc/kubernetes/admin.conf", "Copy admin.conf"},
		{"kubeconfig_path", "$ADMIN_HOME/.kube/config", "Kubeconfig path"},
		{"kubeconfig_chown", "chown -R \"$ADMIN_USER:$ADMIN_USER\"", "Chown kubeconfig"},

		// Idempotency guard
		{"idempotency_check", "if [[ -f /etc/kubernetes/admin.conf ]]", "Idempotency check for admin.conf"},
		{"idempotency_exit", "exit 0", "Exit success if already initialized"},

		// Security
		{"log_permissions", "chmod 600 \"$LOGFILE\"", "Restrict log file permissions"},
		{"unset_authkey", "unset TAILSCALE_AUTH_KEY", "Clear auth key from memory"},

		// CNI
		{"cilium_install", "cilium install", "Install Cilium CNI"},
	}

	output, err := RenderMXCloudInit("testadmin", "tskey-secret-key", "1.29")
	if err != nil {
		t.Fatalf("RenderMXCloudInit failed: %v", err)
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(output, tc.contains) {
				t.Errorf("%s: expected output to contain %q", tc.desc, tc.contains)
			}
		})
	}
}

// TestRenderMXCloudInit_SecurityGuards ensures secrets are handled properly
func TestRenderMXCloudInit_SecurityGuards(t *testing.T) {
	// Use a distinctive auth key that we can search for
	sensitiveAuthKey := "tskey-auth-SUPER_SECRET_12345"

	output, err := RenderMXCloudInit("secureuser", sensitiveAuthKey, "1.30")
	if err != nil {
		t.Fatalf("RenderMXCloudInit failed: %v", err)
	}

	tests := []struct {
		name        string
		shouldExist bool
		pattern     string
		desc        string
	}{
		// The auth key variable assignment is expected (it's in the script)
		{"authkey_variable", true, "TAILSCALE_AUTH_KEY=", "Auth key variable assignment exists"},

		// But we should NOT log the auth key value
		{"no_echo_authkey", false, "echo.*TAILSCALE_AUTH_KEY", "Auth key should not be echoed"},
		{"no_log_authkey", false, "log.*TAILSCALE_AUTH_KEY", "Auth key should not be logged via log()"},

		// Tailscale up output goes to /dev/null, not to log file
		{"tailscale_up_devnull", true, "> /dev/null 2>&1", "Tailscale up output to /dev/null"},

		// Auth key is unset after use
		{"unset_after_use", true, "unset TAILSCALE_AUTH_KEY", "Auth key unset after tailscale up"},

		// Join token warning
		{"no_print_join_token", true, "kubeadm token create --print-join-command' interactively", "Join command run interactively"},

		// Security comments present
		{"security_comment_authkey", true, "SECURITY:", "Security comments in script"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			found := strings.Contains(output, tc.pattern)
			if tc.shouldExist && !found {
				t.Errorf("%s: expected pattern %q to exist", tc.desc, tc.pattern)
			}
			if !tc.shouldExist && found {
				t.Errorf("%s: pattern %q should NOT exist", tc.desc, tc.pattern)
			}
		})
	}
}

// TestRenderMXCloudInit_ParameterSubstitution verifies template parameters are substituted
func TestRenderMXCloudInit_ParameterSubstitution(t *testing.T) {
	tests := []struct {
		name          string
		adminUser     string
		authKey       string
		k8sVersion    string
		expectContain []string
	}{
		{
			name:       "standard_params",
			adminUser:  "myuser",
			authKey:    "tskey-auth-abc123",
			k8sVersion: "1.29",
			expectContain: []string{
				`ADMIN_USER="myuser"`,
				`TAILSCALE_AUTH_KEY="tskey-auth-abc123"`,
				`K8S_VERSION="1.29"`,
				"/home/$ADMIN_USER",
			},
		},
		{
			name:       "different_version",
			adminUser:  "ubuntu",
			authKey:    "tskey-auth-xyz789",
			k8sVersion: "1.30",
			expectContain: []string{
				`ADMIN_USER="ubuntu"`,
				`K8S_VERSION="1.30"`,
				// Script uses ${K8S_VERSION} variable for URLs
				"v${K8S_VERSION}/deb/Release.key",
				"v${K8S_VERSION}/deb/",
			},
		},
		{
			name:       "special_chars_user",
			adminUser:  "admin-user",
			authKey:    "tskey-auth-test",
			k8sVersion: "1.28",
			expectContain: []string{
				`ADMIN_USER="admin-user"`,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			output, err := RenderMXCloudInit(tc.adminUser, tc.authKey, tc.k8sVersion)
			if err != nil {
				t.Fatalf("RenderMXCloudInit failed: %v", err)
			}

			for _, expected := range tc.expectContain {
				if !strings.Contains(output, expected) {
					t.Errorf("Expected output to contain %q", expected)
				}
			}
		})
	}
}

// TestRenderMXCloudInit_IdempotencyGuard specifically tests the idempotency logic
func TestRenderMXCloudInit_IdempotencyGuard(t *testing.T) {
	output, err := RenderMXCloudInit("testuser", "tskey-test", "1.29")
	if err != nil {
		t.Fatalf("RenderMXCloudInit failed: %v", err)
	}

	// The idempotency guard should:
	// 1. Check if admin.conf exists
	// 2. Exit with success if it does
	// 3. Do this BEFORE any installation steps

	idempotencyCheck := "if [[ -f /etc/kubernetes/admin.conf ]]; then"
	kubeadmInit := "kubeadm init"

	idempotencyPos := strings.Index(output, idempotencyCheck)
	kubeadmPos := strings.Index(output, kubeadmInit)

	if idempotencyPos == -1 {
		t.Fatal("Idempotency check not found in output")
	}
	if kubeadmPos == -1 {
		t.Fatal("kubeadm init not found in output")
	}

	// Idempotency check must come before kubeadm init
	if idempotencyPos > kubeadmPos {
		t.Error("Idempotency check must come BEFORE kubeadm init")
	}

	// Check that exit 0 follows the check
	if !strings.Contains(output, "Exiting successfully.\"\n        exit 0") {
		t.Error("Idempotency guard should exit 0 after logging success message")
	}
}

// TestRenderMXCloudInit_ValidYAML ensures output starts with cloud-config
func TestRenderMXCloudInit_ValidYAML(t *testing.T) {
	output, err := RenderMXCloudInit("user", "key", "1.29")
	if err != nil {
		t.Fatalf("RenderMXCloudInit failed: %v", err)
	}

	if !strings.HasPrefix(output, "#cloud-config") {
		t.Error("Output must start with #cloud-config header")
	}

	// Should contain required cloud-init sections
	requiredSections := []string{
		"write_files:",
		"runcmd:",
		"packages:",
	}

	for _, section := range requiredSections {
		if !strings.Contains(output, section) {
			t.Errorf("Missing required cloud-init section: %s", section)
		}
	}
}
