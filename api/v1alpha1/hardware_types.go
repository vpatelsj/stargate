package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// HardwareSpec defines the desired state of Hardware
type HardwareSpec struct {
	// MAC address of the server's primary NIC
	MAC string `json:"mac"`

	// IPv4 address of the server
	IPv4 string `json:"ipv4,omitempty"`

	// BMC configuration for out-of-band management
	BMC *BMCConfig `json:"bmc,omitempty"`

	// Inventory metadata
	Inventory HardwareInventory `json:"inventory,omitempty"`
}

// BMCConfig holds BMC connection details
type BMCConfig struct {
	// Address of the BMC (IP or hostname)
	Address string `json:"address"`

	// CredentialSecretRef references a Secret containing BMC credentials
	CredentialSecretRef string `json:"credentialSecretRef,omitempty"`
}

// HardwareInventory holds inventory metadata for the hardware
type HardwareInventory struct {
	// SKU of the hardware (e.g., "GPU-8xH100")
	SKU string `json:"sku,omitempty"`

	// Location within the datacenter (e.g., "rack-5-slot-12")
	Location string `json:"location,omitempty"`

	// SerialNumber of the hardware
	SerialNumber string `json:"serialNumber,omitempty"`
}

// HardwareStatus defines the observed state of Hardware
type HardwareStatus struct {
	// State of the hardware: ready, provisioning, error
	State string `json:"state,omitempty"`

	// CurrentOS version running on the hardware
	CurrentOS string `json:"currentOS,omitempty"`

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

// Hardware represents a bare-metal server in a datacenter
type Hardware struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HardwareSpec   `json:"spec,omitempty"`
	Status HardwareStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// HardwareList contains a list of Hardware
type HardwareList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Hardware `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Hardware{}, &HardwareList{})
}
