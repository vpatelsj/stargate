package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Helper to create a temp file with content
func createTempFile(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "ssh-key-*")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("failed to write temp file: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("failed to close temp file: %v", err)
	}
	return f.Name()
}

func TestValidate_MissingSubscriptionID(t *testing.T) {
	cfg := &Config{
		SubscriptionID: "",
		Location:       "eastus",
	}

	errs := cfg.ValidateMinimal()

	if !errs.HasErrors() {
		t.Fatal("expected validation errors for missing subscription ID")
	}

	found := false
	for _, e := range errs {
		if e.Field == "subscription-id" {
			found = true
			if !strings.Contains(e.Message, "required") {
				t.Errorf("expected 'required' in message, got: %s", e.Message)
			}
			if !strings.Contains(e.Hint, "AZURE_SUBSCRIPTION_ID") {
				t.Errorf("expected hint to mention env var, got: %s", e.Hint)
			}
		}
	}
	if !found {
		t.Error("expected error for subscription-id field")
	}
}

func TestValidate_InvalidSubscriptionID(t *testing.T) {
	cfg := &Config{
		SubscriptionID:    "not-a-guid",
		Location:          "eastus",
		SSHPublicKeyPath:  createTempFile(t, "ssh-rsa AAAA... user@host"),
		TailscaleAuthKey:  "tskey-auth-xxx",
		KubernetesVersion: "1.29",
	}

	errs := cfg.Validate()

	found := false
	for _, e := range errs {
		if e.Field == "subscription-id" && strings.Contains(e.Message, "invalid format") {
			found = true
			if !strings.Contains(e.Hint, "GUID") {
				t.Errorf("hint should mention GUID format, got: %s", e.Hint)
			}
		}
	}
	if !found {
		t.Error("expected validation error for invalid subscription ID format")
	}
}

func TestValidate_ValidSubscriptionID(t *testing.T) {
	cfg := &Config{
		SubscriptionID:    "44654aed-2753-4b88-9142-af7132933b6b",
		Location:          "eastus",
		SSHPublicKeyPath:  createTempFile(t, "ssh-rsa AAAA... user@host"),
		TailscaleAuthKey:  "tskey-auth-xxx",
		KubernetesVersion: "1.29",
	}

	errs := cfg.Validate()

	for _, e := range errs {
		if e.Field == "subscription-id" {
			t.Errorf("unexpected error for valid subscription ID: %v", e)
		}
	}
}

func TestValidate_MissingLocation(t *testing.T) {
	cfg := &Config{
		SubscriptionID: "44654aed-2753-4b88-9142-af7132933b6b",
		Location:       "",
	}

	errs := cfg.ValidateMinimal()

	found := false
	for _, e := range errs {
		if e.Field == "location" {
			found = true
			if !strings.Contains(e.Message, "required") {
				t.Errorf("expected 'required' in message, got: %s", e.Message)
			}
		}
	}
	if !found {
		t.Error("expected error for missing location")
	}
}

func TestValidate_MissingSSHKeyPath(t *testing.T) {
	cfg := &Config{
		SubscriptionID:   "44654aed-2753-4b88-9142-af7132933b6b",
		Location:         "eastus",
		SSHPublicKeyPath: "",
	}

	errs := cfg.ValidateMinimal()

	found := false
	for _, e := range errs {
		if e.Field == "ssh-public-key-path" {
			found = true
			if !strings.Contains(e.Message, "required") {
				t.Errorf("expected 'required' in message, got: %s", e.Message)
			}
		}
	}
	if !found {
		t.Error("expected error for missing ssh-public-key-path")
	}
}

func TestValidate_SSHKeyFileNotFound(t *testing.T) {
	cfg := &Config{
		SubscriptionID:    "44654aed-2753-4b88-9142-af7132933b6b",
		Location:          "eastus",
		SSHPublicKeyPath:  "/nonexistent/path/to/key.pub",
		TailscaleAuthKey:  "tskey-auth-xxx",
		KubernetesVersion: "1.29",
	}

	errs := cfg.Validate()

	found := false
	for _, e := range errs {
		if e.Field == "ssh-public-key-path" && strings.Contains(e.Message, "not found") {
			found = true
		}
	}
	if !found {
		t.Error("expected 'file not found' error for nonexistent SSH key path")
	}
}

func TestValidate_InvalidSSHKeyFormat(t *testing.T) {
	// Create file with invalid SSH key content
	badKeyPath := createTempFile(t, "this is not an ssh key")

	cfg := &Config{
		SubscriptionID:    "44654aed-2753-4b88-9142-af7132933b6b",
		Location:          "eastus",
		SSHPublicKeyPath:  badKeyPath,
		TailscaleAuthKey:  "tskey-auth-xxx",
		KubernetesVersion: "1.29",
	}

	errs := cfg.Validate()

	found := false
	for _, e := range errs {
		if e.Field == "ssh-public-key-path" && strings.Contains(e.Message, "invalid SSH public key format") {
			found = true
			if !strings.Contains(e.Hint, "ssh-rsa") {
				t.Errorf("hint should mention valid key types, got: %s", e.Hint)
			}
		}
	}
	if !found {
		t.Error("expected 'invalid format' error for bad SSH key content")
	}
}

func TestValidate_ValidSSHKeyFormats(t *testing.T) {
	testCases := []struct {
		name    string
		keyData string
	}{
		{"rsa", "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABgQC... user@host"},
		{"ed25519", "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA... user@host"},
		{"ecdsa-256", "ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTIt... user@host"},
		{"ecdsa-384", "ecdsa-sha2-nistp384 AAAAE2VjZHNhLXNoYTIt... user@host"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			keyPath := createTempFile(t, tc.keyData)

			cfg := &Config{
				SubscriptionID:    "44654aed-2753-4b88-9142-af7132933b6b",
				Location:          "eastus",
				SSHPublicKeyPath:  keyPath,
				TailscaleAuthKey:  "tskey-auth-xxx",
				KubernetesVersion: "1.29",
			}

			errs := cfg.Validate()

			for _, e := range errs {
				if e.Field == "ssh-public-key-path" {
					t.Errorf("unexpected error for valid %s key: %v", tc.name, e)
				}
			}

			// Verify key was stored
			if cfg.SSHPublicKey == "" {
				t.Error("SSHPublicKey should be populated after validation")
			}
		})
	}
}

func TestValidate_MissingTailscaleAuthKey(t *testing.T) {
	keyPath := createTempFile(t, "ssh-rsa AAAA... user@host")

	cfg := &Config{
		SubscriptionID:    "44654aed-2753-4b88-9142-af7132933b6b",
		Location:          "eastus",
		SSHPublicKeyPath:  keyPath,
		TailscaleAuthKey:  "",
		KubernetesVersion: "1.29",
	}

	errs := cfg.Validate()

	found := false
	for _, e := range errs {
		if e.Field == "tailscale-auth-key" {
			found = true
			if !strings.Contains(e.Message, "required") {
				t.Errorf("expected 'required' in message, got: %s", e.Message)
			}
			if !strings.Contains(e.Hint, "TAILSCALE_AUTH_KEY") {
				t.Errorf("hint should mention env var, got: %s", e.Hint)
			}
		}
	}
	if !found {
		t.Error("expected error for missing tailscale-auth-key")
	}
}

func TestValidate_InvalidTailscaleAuthKey(t *testing.T) {
	keyPath := createTempFile(t, "ssh-rsa AAAA... user@host")

	cfg := &Config{
		SubscriptionID:    "44654aed-2753-4b88-9142-af7132933b6b",
		Location:          "eastus",
		SSHPublicKeyPath:  keyPath,
		TailscaleAuthKey:  "invalid-key-format",
		KubernetesVersion: "1.29",
	}

	errs := cfg.Validate()

	found := false
	for _, e := range errs {
		if e.Field == "tailscale-auth-key" && strings.Contains(e.Message, "invalid format") {
			found = true
			if !strings.Contains(e.Hint, "tskey-auth-") {
				t.Errorf("hint should mention valid prefix, got: %s", e.Hint)
			}
		}
	}
	if !found {
		t.Error("expected error for invalid tailscale auth key format")
	}
}

func TestValidate_ValidTailscaleAuthKeyFormats(t *testing.T) {
	testCases := []struct {
		name string
		key  string
	}{
		{"auth_key", "tskey-auth-xxx123ABC"},
		{"client_key", "tskey-client-xxx123ABC"},
	}

	keyPath := createTempFile(t, "ssh-rsa AAAA... user@host")

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{
				SubscriptionID:    "44654aed-2753-4b88-9142-af7132933b6b",
				Location:          "eastus",
				SSHPublicKeyPath:  keyPath,
				TailscaleAuthKey:  tc.key,
				KubernetesVersion: "1.29",
			}

			errs := cfg.Validate()

			for _, e := range errs {
				if e.Field == "tailscale-auth-key" {
					t.Errorf("unexpected error for valid %s: %v", tc.name, e)
				}
			}
		})
	}
}

func TestValidate_InvalidKubernetesVersion(t *testing.T) {
	keyPath := createTempFile(t, "ssh-rsa AAAA... user@host")

	testCases := []struct {
		name    string
		version string
	}{
		{"empty", ""},
		{"full_semver", "1.29.1"},
		{"too_old", "1.23"},
		{"invalid_format", "v1.29"},
		{"random", "kubernetes"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{
				SubscriptionID:    "44654aed-2753-4b88-9142-af7132933b6b",
				Location:          "eastus",
				SSHPublicKeyPath:  keyPath,
				TailscaleAuthKey:  "tskey-auth-xxx",
				KubernetesVersion: tc.version,
			}

			errs := cfg.Validate()

			found := false
			for _, e := range errs {
				if e.Field == "kubernetes-version" {
					found = true
				}
			}
			if !found {
				t.Errorf("expected error for invalid kubernetes version '%s'", tc.version)
			}
		})
	}
}

func TestValidate_ValidKubernetesVersions(t *testing.T) {
	keyPath := createTempFile(t, "ssh-rsa AAAA... user@host")

	validVersions := []string{"1.24", "1.25", "1.26", "1.27", "1.28", "1.29", "1.30", "1.31"}

	for _, version := range validVersions {
		t.Run(version, func(t *testing.T) {
			cfg := &Config{
				SubscriptionID:    "44654aed-2753-4b88-9142-af7132933b6b",
				Location:          "eastus",
				SSHPublicKeyPath:  keyPath,
				TailscaleAuthKey:  "tskey-auth-xxx",
				KubernetesVersion: version,
			}

			errs := cfg.Validate()

			for _, e := range errs {
				if e.Field == "kubernetes-version" {
					t.Errorf("unexpected error for valid version %s: %v", version, e)
				}
			}
		})
	}
}

func TestValidate_InvalidZone(t *testing.T) {
	keyPath := createTempFile(t, "ssh-rsa AAAA... user@host")

	invalidZones := []string{"0", "4", "a", "zone1"}

	for _, zone := range invalidZones {
		t.Run(zone, func(t *testing.T) {
			cfg := &Config{
				SubscriptionID:    "44654aed-2753-4b88-9142-af7132933b6b",
				Location:          "eastus",
				Zone:              zone,
				SSHPublicKeyPath:  keyPath,
				TailscaleAuthKey:  "tskey-auth-xxx",
				KubernetesVersion: "1.29",
			}

			errs := cfg.Validate()

			found := false
			for _, e := range errs {
				if e.Field == "zone" {
					found = true
					if !strings.Contains(e.Hint, "1") || !strings.Contains(e.Hint, "2") || !strings.Contains(e.Hint, "3") {
						t.Errorf("hint should mention valid zones, got: %s", e.Hint)
					}
				}
			}
			if !found {
				t.Errorf("expected error for invalid zone '%s'", zone)
			}
		})
	}
}

func TestValidate_ValidZones(t *testing.T) {
	keyPath := createTempFile(t, "ssh-rsa AAAA... user@host")

	validZones := []string{"1", "2", "3", ""} // empty is valid (non-zonal)

	for _, zone := range validZones {
		name := zone
		if name == "" {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			cfg := &Config{
				SubscriptionID:    "44654aed-2753-4b88-9142-af7132933b6b",
				Location:          "eastus",
				Zone:              zone,
				SSHPublicKeyPath:  keyPath,
				TailscaleAuthKey:  "tskey-auth-xxx",
				KubernetesVersion: "1.29",
			}

			errs := cfg.Validate()

			for _, e := range errs {
				if e.Field == "zone" {
					t.Errorf("unexpected error for valid zone '%s': %v", zone, e)
				}
			}
		})
	}
}

func TestValidate_InvalidAdminUsername(t *testing.T) {
	keyPath := createTempFile(t, "ssh-rsa AAAA... user@host")

	invalidUsernames := []struct {
		name     string
		username string
	}{
		{"root", "root"},
		{"starts_with_number", "1user"},
		{"uppercase", "User"},
		{"special_chars", "user@name"},
	}

	for _, tc := range invalidUsernames {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{
				SubscriptionID:    "44654aed-2753-4b88-9142-af7132933b6b",
				Location:          "eastus",
				SSHPublicKeyPath:  keyPath,
				TailscaleAuthKey:  "tskey-auth-xxx",
				KubernetesVersion: "1.29",
				AdminUsername:     tc.username,
			}

			errs := cfg.Validate()

			found := false
			for _, e := range errs {
				if e.Field == "admin-username" {
					found = true
				}
			}
			if !found {
				t.Errorf("expected error for invalid admin username '%s'", tc.username)
			}
		})
	}
}

func TestValidate_ValidAdminUsernames(t *testing.T) {
	keyPath := createTempFile(t, "ssh-rsa AAAA... user@host")

	validUsernames := []string{"azureuser", "ubuntu", "admin_user", "user123", "a"}

	for _, username := range validUsernames {
		t.Run(username, func(t *testing.T) {
			cfg := &Config{
				SubscriptionID:    "44654aed-2753-4b88-9142-af7132933b6b",
				Location:          "eastus",
				SSHPublicKeyPath:  keyPath,
				TailscaleAuthKey:  "tskey-auth-xxx",
				KubernetesVersion: "1.29",
				AdminUsername:     username,
			}

			errs := cfg.Validate()

			for _, e := range errs {
				if e.Field == "admin-username" {
					t.Errorf("unexpected error for valid username '%s': %v", username, e)
				}
			}
		})
	}
}

func TestValidate_AllErrorsReturned(t *testing.T) {
	// Config with multiple issues - should return all errors, not fail fast
	cfg := &Config{
		SubscriptionID:    "", // missing
		Location:          "", // missing
		SSHPublicKeyPath:  "", // missing
		TailscaleAuthKey:  "", // missing
		KubernetesVersion: "", // missing
	}

	errs := cfg.Validate()

	if len(errs) < 5 {
		t.Errorf("expected at least 5 validation errors, got %d: %v", len(errs), errs)
	}

	// Verify we get errors for multiple fields
	fields := make(map[string]bool)
	for _, e := range errs {
		fields[e.Field] = true
	}

	requiredFields := []string{"subscription-id", "location", "ssh-public-key-path", "tailscale-auth-key", "kubernetes-version"}
	for _, f := range requiredFields {
		if !fields[f] {
			t.Errorf("expected error for field '%s'", f)
		}
	}
}

func TestValidationError_Error(t *testing.T) {
	t.Run("with_hint", func(t *testing.T) {
		e := ValidationError{
			Field:   "test-field",
			Message: "is invalid",
			Hint:    "try something else",
		}
		s := e.Error()
		if !strings.Contains(s, "test-field") {
			t.Errorf("error string should contain field name: %s", s)
		}
		if !strings.Contains(s, "is invalid") {
			t.Errorf("error string should contain message: %s", s)
		}
		if !strings.Contains(s, "try something else") {
			t.Errorf("error string should contain hint: %s", s)
		}
	})

	t.Run("without_hint", func(t *testing.T) {
		e := ValidationError{
			Field:   "test-field",
			Message: "is invalid",
		}
		s := e.Error()
		if !strings.Contains(s, "test-field") || !strings.Contains(s, "is invalid") {
			t.Errorf("error string format incorrect: %s", s)
		}
	})
}

func TestValidationErrors_Error(t *testing.T) {
	errs := ValidationErrors{
		{Field: "a", Message: "error a"},
		{Field: "b", Message: "error b"},
	}

	s := errs.Error()
	if !strings.Contains(s, "a: error a") {
		t.Errorf("combined error should contain first error: %s", s)
	}
	if !strings.Contains(s, "b: error b") {
		t.Errorf("combined error should contain second error: %s", s)
	}
}

func TestValidationErrors_HasErrors(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		var errs ValidationErrors
		if errs.HasErrors() {
			t.Error("empty errors should return false")
		}
	})

	t.Run("with_errors", func(t *testing.T) {
		errs := ValidationErrors{{Field: "a", Message: "error"}}
		if !errs.HasErrors() {
			t.Error("non-empty errors should return true")
		}
	})
}

func TestValidate_SSHKeyPermissions(t *testing.T) {
	// Create a file with restrictive permissions
	tmpDir := t.TempDir()
	keyPath := filepath.Join(tmpDir, "unreadable.pub")

	// Create file
	if err := os.WriteFile(keyPath, []byte("ssh-rsa AAAA..."), 0600); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	// Make it unreadable (this only works as non-root)
	if os.Getuid() != 0 {
		if err := os.Chmod(keyPath, 0000); err != nil {
			t.Fatalf("failed to chmod test file: %v", err)
		}
		defer os.Chmod(keyPath, 0600) // Cleanup

		cfg := &Config{
			SubscriptionID:    "44654aed-2753-4b88-9142-af7132933b6b",
			Location:          "eastus",
			SSHPublicKeyPath:  keyPath,
			TailscaleAuthKey:  "tskey-auth-xxx",
			KubernetesVersion: "1.29",
		}

		errs := cfg.Validate()

		found := false
		for _, e := range errs {
			if e.Field == "ssh-public-key-path" && strings.Contains(e.Message, "permission denied") {
				found = true
			}
		}
		if !found {
			t.Error("expected permission denied error for unreadable SSH key file")
		}
	}
}
