package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ProvisioningProfileSpec defines the desired state of ProvisioningProfile
type ProvisioningProfileSpec struct {
	// OSVersion to install (e.g., "2.0.0")
	OSVersion string `json:"osVersion"`

	// OSImage URL (optional, for future use)
	OSImage string `json:"osImage,omitempty"`

	// CloudInit configuration (optional, for future use)
	CloudInit string `json:"cloudInit,omitempty"`

	// CloudInitSecretRef references a Secret containing cloud-init data (optional)
	CloudInitSecretRef string `json:"cloudInitSecretRef,omitempty"`
}

// ProvisioningProfileStatus defines the observed state of ProvisioningProfile
type ProvisioningProfileStatus struct {
	// Ready indicates if the template is valid and ready for use
	Ready bool `json:"ready,omitempty"`

	// Message provides additional status information
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="OSVersion",type="string",JSONPath=".spec.osVersion"
// +kubebuilder:printcolumn:name="Ready",type="boolean",JSONPath=".status.ready"

// ProvisioningProfile defines a provisioning configuration that can be applied to Server
type ProvisioningProfile struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ProvisioningProfileSpec   `json:"spec,omitempty"`
	Status ProvisioningProfileStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ProvisioningProfileList contains a list of ProvisioningProfile
type ProvisioningProfileList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ProvisioningProfile `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ProvisioningProfile{}, &ProvisioningProfileList{})
}
