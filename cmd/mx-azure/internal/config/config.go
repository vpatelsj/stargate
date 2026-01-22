package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Config holds all CLI configuration for mx-azure
type Config struct {
	SubscriptionID    string
	Location          string
	Zone              string
	ResourceGroup     string
	VNetName          string
	VNetAddressSpace  string
	SubnetName        string
	SubnetPrefix      string
	NSGName           string
	PublicIPName      string
	NICName           string
	VMName            string
	AdminUsername     string
	SSHPublicKeyPath  string
	VMSize            string
	ImagePublisher    string
	ImageOffer        string
	ImageSKU          string
	TailscaleAuthKey  string
	KubernetesVersion string

	// Internal: populated during validation
	SSHPublicKey string
}

// ValidationError represents a single validation error with actionable message
type ValidationError struct {
	Field   string
	Message string
	Hint    string
}

func (e ValidationError) Error() string {
	if e.Hint != "" {
		return fmt.Sprintf("%s: %s (%s)", e.Field, e.Message, e.Hint)
	}
	return fmt.Sprintf("%s: %s", e.Field, e.Message)
}

// ValidationErrors is a collection of validation errors
type ValidationErrors []ValidationError

func (errs ValidationErrors) Error() string {
	if len(errs) == 0 {
		return ""
	}
	var msgs []string
	for _, e := range errs {
		msgs = append(msgs, e.Error())
	}
	return strings.Join(msgs, "; ")
}

// HasErrors returns true if there are any validation errors
func (errs ValidationErrors) HasErrors() bool {
	return len(errs) > 0
}

// Validate checks the configuration and returns any validation errors.
// It performs all validations and returns all errors at once (not fail-fast).
func (c *Config) Validate() ValidationErrors {
	var errs ValidationErrors

	// Required: Subscription ID
	if c.SubscriptionID == "" {
		errs = append(errs, ValidationError{
			Field:   "subscription-id",
			Message: "required but not provided",
			Hint:    "set via --subscription-id flag or AZURE_SUBSCRIPTION_ID env var",
		})
	} else if !isValidSubscriptionID(c.SubscriptionID) {
		errs = append(errs, ValidationError{
			Field:   "subscription-id",
			Message: "invalid format",
			Hint:    "must be a valid GUID like 'xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx'",
		})
	}

	// Required: Location
	if c.Location == "" {
		errs = append(errs, ValidationError{
			Field:   "location",
			Message: "required but not provided",
			Hint:    "set via --location flag, e.g., 'canadacentral', 'eastus'",
		})
	}

	// Optional but validated: Zone
	if c.Zone != "" && !isValidZone(c.Zone) {
		errs = append(errs, ValidationError{
			Field:   "zone",
			Message: fmt.Sprintf("invalid value '%s'", c.Zone),
			Hint:    "must be '1', '2', or '3' (or empty for non-zonal)",
		})
	}

	// Required: SSH public key path
	if c.SSHPublicKeyPath == "" {
		errs = append(errs, ValidationError{
			Field:   "ssh-public-key-path",
			Message: "required but not provided",
			Hint:    "set via --ssh-public-key-path, e.g., '~/.ssh/id_rsa.pub'",
		})
	} else {
		// Validate file exists and is readable
		keyData, err := os.ReadFile(c.SSHPublicKeyPath)
		if err != nil {
			if os.IsNotExist(err) {
				errs = append(errs, ValidationError{
					Field:   "ssh-public-key-path",
					Message: fmt.Sprintf("file not found: %s", c.SSHPublicKeyPath),
					Hint:    "check path exists and is readable",
				})
			} else if os.IsPermission(err) {
				errs = append(errs, ValidationError{
					Field:   "ssh-public-key-path",
					Message: fmt.Sprintf("permission denied: %s", c.SSHPublicKeyPath),
					Hint:    "check file permissions (should be readable by current user)",
				})
			} else {
				errs = append(errs, ValidationError{
					Field:   "ssh-public-key-path",
					Message: fmt.Sprintf("failed to read: %v", err),
					Hint:    "ensure file exists and is accessible",
				})
			}
		} else {
			// Validate SSH key format
			keyStr := strings.TrimSpace(string(keyData))
			if !isValidSSHPublicKey(keyStr) {
				errs = append(errs, ValidationError{
					Field:   "ssh-public-key-path",
					Message: "invalid SSH public key format",
					Hint:    "file should start with 'ssh-rsa', 'ssh-ed25519', 'ecdsa-sha2-nistp256', etc.",
				})
			} else {
				// Store validated key
				c.SSHPublicKey = keyStr
			}
		}
	}

	// Required: Tailscale auth key (for auto-enrollment)
	if c.TailscaleAuthKey == "" {
		errs = append(errs, ValidationError{
			Field:   "tailscale-auth-key",
			Message: "required but not provided",
			Hint:    "set via --tailscale-auth-key or TAILSCALE_AUTH_KEY env var; get key from https://login.tailscale.com/admin/settings/keys",
		})
	} else if !isValidTailscaleAuthKey(c.TailscaleAuthKey) {
		errs = append(errs, ValidationError{
			Field:   "tailscale-auth-key",
			Message: "invalid format",
			Hint:    "should start with 'tskey-auth-' prefix",
		})
	}

	// Validate Kubernetes version format
	if c.KubernetesVersion == "" {
		errs = append(errs, ValidationError{
			Field:   "kubernetes-version",
			Message: "required but not provided",
			Hint:    "set via --kubernetes-version, e.g., '1.29', '1.30'",
		})
	} else if !isValidKubernetesVersion(c.KubernetesVersion) {
		errs = append(errs, ValidationError{
			Field:   "kubernetes-version",
			Message: fmt.Sprintf("invalid format '%s'", c.KubernetesVersion),
			Hint:    "use format 'X.Y' like '1.29' or '1.30' (not full semver)",
		})
	}

	// Validate resource names (Azure naming constraints)
	if c.VMName != "" && !isValidAzureResourceName(c.VMName) {
		errs = append(errs, ValidationError{
			Field:   "vm-name",
			Message: fmt.Sprintf("invalid name '%s'", c.VMName),
			Hint:    "must be 1-64 chars, alphanumeric and hyphens only, cannot start/end with hyphen",
		})
	}

	if c.ResourceGroup != "" && !isValidAzureResourceName(c.ResourceGroup) {
		errs = append(errs, ValidationError{
			Field:   "resource-group",
			Message: fmt.Sprintf("invalid name '%s'", c.ResourceGroup),
			Hint:    "must be 1-90 chars, alphanumeric, underscores, hyphens, and periods only",
		})
	}

	// Validate admin username
	if c.AdminUsername != "" && !isValidLinuxUsername(c.AdminUsername) {
		errs = append(errs, ValidationError{
			Field:   "admin-username",
			Message: fmt.Sprintf("invalid username '%s'", c.AdminUsername),
			Hint:    "must be 1-32 chars, start with letter, alphanumeric and underscores only; cannot be 'root'",
		})
	}

	return errs
}

// ValidateMinimal performs minimal validation (only truly required fields).
// Use this when tailscale auth key is optional (e.g., for testing).
func (c *Config) ValidateMinimal() ValidationErrors {
	var errs ValidationErrors

	if c.SubscriptionID == "" {
		errs = append(errs, ValidationError{
			Field:   "subscription-id",
			Message: "required but not provided",
			Hint:    "set via --subscription-id flag or AZURE_SUBSCRIPTION_ID env var",
		})
	}

	if c.Location == "" {
		errs = append(errs, ValidationError{
			Field:   "location",
			Message: "required but not provided",
			Hint:    "set via --location flag",
		})
	}

	if c.SSHPublicKeyPath == "" {
		errs = append(errs, ValidationError{
			Field:   "ssh-public-key-path",
			Message: "required but not provided",
			Hint:    "set via --ssh-public-key-path flag",
		})
	}

	return errs
}

// --- Validation helpers ---

var (
	// UUID/GUID pattern for subscription ID
	guidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

	// Kubernetes version pattern (X.Y format)
	k8sVersionPattern = regexp.MustCompile(`^1\.(2[4-9]|[3-9][0-9])$`)

	// Azure resource name pattern (simplified)
	azureResourceNamePattern = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-_.]*[a-zA-Z0-9])?$`)

	// Linux username pattern
	linuxUsernamePattern = regexp.MustCompile(`^[a-z_][a-z0-9_-]*$`)

	// SSH public key prefixes
	sshKeyPrefixes = []string{"ssh-rsa ", "ssh-ed25519 ", "ecdsa-sha2-nistp256 ", "ecdsa-sha2-nistp384 ", "ecdsa-sha2-nistp521 ", "sk-ssh-ed25519@openssh.com ", "sk-ecdsa-sha2-nistp256@openssh.com "}
)

func isValidSubscriptionID(id string) bool {
	return guidPattern.MatchString(id)
}

func isValidZone(zone string) bool {
	return zone == "1" || zone == "2" || zone == "3"
}

func isValidSSHPublicKey(key string) bool {
	for _, prefix := range sshKeyPrefixes {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

func isValidTailscaleAuthKey(key string) bool {
	return strings.HasPrefix(key, "tskey-auth-") || strings.HasPrefix(key, "tskey-client-")
}

func isValidKubernetesVersion(version string) bool {
	return k8sVersionPattern.MatchString(version)
}

func isValidAzureResourceName(name string) bool {
	if len(name) == 0 || len(name) > 90 {
		return false
	}
	return azureResourceNamePattern.MatchString(name)
}

func isValidLinuxUsername(username string) bool {
	if len(username) == 0 || len(username) > 32 {
		return false
	}
	if username == "root" {
		return false
	}
	return linuxUsernamePattern.MatchString(username)
}
