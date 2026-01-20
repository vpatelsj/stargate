package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ServerSpec defines the desired state of Server
type ServerSpec struct {
	// MAC address of the server's primary NIC
	MAC string `json:"mac"`

	// Provider indicates which controller should manage this server (e.g., "azure" or "qemu")
	Provider string `json:"provider,omitempty"`

	// IPv4 address of the server
	IPv4 string `json:"ipv4,omitempty"`

	// RouterIP is the Tailscale IP or FQDN of the subnet router for SSH access (for workers behind a router)
	RouterIP string `json:"routerIP,omitempty"`

	// BMC configuration for out-of-band management
	BMC *BMCConfig `json:"bmc,omitempty"`

	// Inventory metadata
	Inventory ServerInventory `json:"inventory,omitempty"`
}

// BMCConfig holds BMC connection details
type BMCConfig struct {
	// Address of the BMC (IP or hostname)
	Address string `json:"address"`

	// CredentialSecretRef references a Secret containing BMC credentials
	CredentialSecretRef string `json:"credentialSecretRef,omitempty"`
}

// ServerInventory holds inventory metadata for the server
type ServerInventory struct {
	// SKU of the hardware (e.g., "GPU-8xH100")
	SKU string `json:"sku,omitempty"`

	// Location within the datacenter (e.g., "rack-5-slot-12")
	Location string `json:"location,omitempty"`

	// SerialNumber of the hardware
	SerialNumber string `json:"serialNumber,omitempty"`
}

// ServerStatus defines the observed state of Server
type ServerStatus struct {
	// State of the hardware: ready, provisioning, error
	State string `json:"state,omitempty"`

	// CurrentOS version running on the hardware
	CurrentOS string `json:"currentOS,omitempty"`

	// AppliedProvisioningProfile is the name of the last successfully applied ProvisioningProfile
	AppliedProvisioningProfile string `json:"appliedProvisioningProfile,omitempty"`

	// LastUpdated timestamp
	LastUpdated metav1.Time `json:"lastUpdated,omitempty"`

	// Message provides additional status information
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="State",type="string",JSONPath=".status.state"
// +kubebuilder:printcolumn:name="OS",type="string",JSONPath=".status.currentOS"
// +kubebuilder:printcolumn:name="IPv4",type="string",JSONPath=".spec.ipv4"

// Server represents a bare-metal server in a datacenter
type Server struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ServerSpec   `json:"spec,omitempty"`
	Status ServerStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ServerList contains a list of Server
type ServerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Server `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Server{}, &ServerList{})
}
