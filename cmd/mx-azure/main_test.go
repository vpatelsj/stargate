package main

import (
	"flag"
	"os"
	"testing"
)

func TestAddCommonFlags(t *testing.T) {
	// Save and restore env
	origSub := os.Getenv("AZURE_SUBSCRIPTION_ID")
	defer os.Setenv("AZURE_SUBSCRIPTION_ID", origSub)
	os.Setenv("AZURE_SUBSCRIPTION_ID", "test-sub-from-env")

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	var cf CommonFlags
	addCommonFlags(fs, &cf)

	t.Run("defaults from env", func(t *testing.T) {
		if err := fs.Parse([]string{}); err != nil {
			t.Fatalf("parse failed: %v", err)
		}
		if cf.SubscriptionID != "test-sub-from-env" {
			t.Errorf("expected subscription from env, got %s", cf.SubscriptionID)
		}
		if cf.Location != "canadacentral" {
			t.Errorf("expected default location 'canadacentral', got %s", cf.Location)
		}
		if cf.ResourceGroup != "mx-azure-rg" {
			t.Errorf("expected default resource group 'mx-azure-rg', got %s", cf.ResourceGroup)
		}
		if cf.LogJSON != false {
			t.Error("expected LogJSON default to be false")
		}
	})
}

func TestAddCommonFlags_Override(t *testing.T) {
	os.Setenv("AZURE_SUBSCRIPTION_ID", "env-sub")
	defer os.Unsetenv("AZURE_SUBSCRIPTION_ID")

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	var cf CommonFlags
	addCommonFlags(fs, &cf)

	args := []string{
		"--subscription-id=cli-sub",
		"--location=eastus",
		"--resource-group=my-rg",
		"--log-json",
	}

	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse failed: %v", err)
	}

	if cf.SubscriptionID != "cli-sub" {
		t.Errorf("CLI flag should override env, got %s", cf.SubscriptionID)
	}
	if cf.Location != "eastus" {
		t.Errorf("expected location 'eastus', got %s", cf.Location)
	}
	if cf.ResourceGroup != "my-rg" {
		t.Errorf("expected resource group 'my-rg', got %s", cf.ResourceGroup)
	}
	if cf.LogJSON != true {
		t.Error("expected LogJSON to be true")
	}
}

func TestAddVMFlags(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	var vf VMFlags
	addVMFlags(fs, &vf)

	t.Run("defaults", func(t *testing.T) {
		if err := fs.Parse([]string{}); err != nil {
			t.Fatalf("parse failed: %v", err)
		}

		defaults := map[string]string{
			"Zone":              "1",
			"VNetName":          "mx-vnet",
			"VNetAddressSpace":  "10.0.0.0/16",
			"SubnetName":        "mx-subnet",
			"SubnetPrefix":      "10.0.1.0/24",
			"NSGName":           "mx-nsg",
			"PublicIPName":      "mx-pip",
			"NICName":           "mx-nic",
			"VMName":            "mx-vm",
			"AdminUsername":     "azureuser",
			"VMSize":            "Standard_D2s_v5",
			"ImagePublisher":    "Canonical",
			"ImageOffer":        "0001-com-ubuntu-server-jammy",
			"ImageSKU":          "22_04-lts-gen2",
			"KubernetesVersion": "1.29",
		}

		if vf.Zone != defaults["Zone"] {
			t.Errorf("Zone: expected %s, got %s", defaults["Zone"], vf.Zone)
		}
		if vf.VNetName != defaults["VNetName"] {
			t.Errorf("VNetName: expected %s, got %s", defaults["VNetName"], vf.VNetName)
		}
		if vf.VMName != defaults["VMName"] {
			t.Errorf("VMName: expected %s, got %s", defaults["VMName"], vf.VMName)
		}
		if vf.AdminUsername != defaults["AdminUsername"] {
			t.Errorf("AdminUsername: expected %s, got %s", defaults["AdminUsername"], vf.AdminUsername)
		}
		if vf.VMSize != defaults["VMSize"] {
			t.Errorf("VMSize: expected %s, got %s", defaults["VMSize"], vf.VMSize)
		}
	})
}

func TestAddVMFlags_Override(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	var vf VMFlags
	addVMFlags(fs, &vf)

	args := []string{
		"--zone=2",
		"--vm-name=my-custom-vm",
		"--vm-size=Standard_B2s",
		"--admin-username=ubuntu",
		"--ssh-public-key-path=/path/to/key.pub",
		"--kubernetes-version=1.30",
	}

	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse failed: %v", err)
	}

	if vf.Zone != "2" {
		t.Errorf("Zone: expected 2, got %s", vf.Zone)
	}
	if vf.VMName != "my-custom-vm" {
		t.Errorf("VMName: expected my-custom-vm, got %s", vf.VMName)
	}
	if vf.VMSize != "Standard_B2s" {
		t.Errorf("VMSize: expected Standard_B2s, got %s", vf.VMSize)
	}
	if vf.AdminUsername != "ubuntu" {
		t.Errorf("AdminUsername: expected ubuntu, got %s", vf.AdminUsername)
	}
	if vf.SSHPublicKeyPath != "/path/to/key.pub" {
		t.Errorf("SSHPublicKeyPath: expected /path/to/key.pub, got %s", vf.SSHPublicKeyPath)
	}
	if vf.KubernetesVersion != "1.30" {
		t.Errorf("KubernetesVersion: expected 1.30, got %s", vf.KubernetesVersion)
	}
}

func TestTailscaleAuthKeyFromEnv(t *testing.T) {
	origKey := os.Getenv("TAILSCALE_AUTH_KEY")
	defer os.Setenv("TAILSCALE_AUTH_KEY", origKey)

	os.Setenv("TAILSCALE_AUTH_KEY", "tskey-auth-test123")

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	var vf VMFlags
	addVMFlags(fs, &vf)

	if err := fs.Parse([]string{}); err != nil {
		t.Fatalf("parse failed: %v", err)
	}

	if vf.TailscaleAuthKey != "tskey-auth-test123" {
		t.Errorf("expected tailscale auth key from env, got %s", vf.TailscaleAuthKey)
	}
}

func TestDestroyFlagYes(t *testing.T) {
	fs := flag.NewFlagSet("destroy", flag.ContinueOnError)
	var cf CommonFlags
	addCommonFlags(fs, &cf)
	yes := fs.Bool("yes", false, "Skip confirmation")

	t.Run("default_no_yes", func(t *testing.T) {
		if err := fs.Parse([]string{}); err != nil {
			t.Fatalf("parse failed: %v", err)
		}
		if *yes != false {
			t.Error("expected --yes default to be false")
		}
	})
}

func TestDestroyFlagYes_Set(t *testing.T) {
	fs := flag.NewFlagSet("destroy", flag.ContinueOnError)
	var cf CommonFlags
	addCommonFlags(fs, &cf)
	yes := fs.Bool("yes", false, "Skip confirmation")

	if err := fs.Parse([]string{"--yes", "--resource-group=test-rg"}); err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if *yes != true {
		t.Error("expected --yes to be true when flag is set")
	}
	if cf.ResourceGroup != "test-rg" {
		t.Errorf("expected resource-group 'test-rg', got %s", cf.ResourceGroup)
	}
}

func TestTruncateID(t *testing.T) {
	testCases := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "full_azure_resource_id",
			input:    "/subscriptions/12345/resourceGroups/my-rg/providers/Microsoft.Compute/virtualMachines/my-vm",
			expected: ".../virtualMachines/my-vm",
		},
		{
			name:     "short_id",
			input:    "abc",
			expected: "abc",
		},
		{
			name:     "two_parts",
			input:    "a/b",
			expected: "a/b",
		},
		{
			name:     "empty",
			input:    "",
			expected: "",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := truncateID(tc.input)
			if result != tc.expected {
				t.Errorf("expected %s, got %s", tc.expected, result)
			}
		})
	}
}

func TestSetupLogger(t *testing.T) {
	t.Run("text_format", func(t *testing.T) {
		logger := setupLogger(false)
		if logger == nil {
			t.Error("expected non-nil logger")
		}
	})

	t.Run("json_format", func(t *testing.T) {
		logger := setupLogger(true)
		if logger == nil {
			t.Error("expected non-nil logger")
		}
	})
}
