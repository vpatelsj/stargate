package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TemplateSpec defines the desired state of Template
type TemplateSpec struct {
	// OSVersion to install (e.g., "2.0.0")
	OSVersion string `json:"osVersion"`

	// OSImage URL (optional, for future use)
	OSImage string `json:"osImage,omitempty"`

	// CloudInit configuration (optional, for future use)
	CloudInit string `json:"cloudInit,omitempty"`

	// CloudInitSecretRef references a Secret containing cloud-init data (optional)
	CloudInitSecretRef string `json:"cloudInitSecretRef,omitempty"`
}

// TemplateStatus defines the observed state of Template
type TemplateStatus struct {
	// Ready indicates if the template is valid and ready for use
	Ready bool `json:"ready,omitempty"`

	// Message provides additional status information
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="OSVersion",type="string",JSONPath=".spec.osVersion"
// +kubebuilder:printcolumn:name="Ready",type="boolean",JSONPath=".status.ready"

// Template defines a provisioning configuration that can be applied to Hardware
type Template struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TemplateSpec   `json:"spec,omitempty"`
	Status TemplateStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// TemplateList contains a list of Template
type TemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Template `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Template{}, &TemplateList{})
}
