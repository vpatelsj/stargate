package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ProvisioningProfileSpec defines the desired state of ProvisioningProfile
type ProvisioningProfileSpec struct {
	// KubernetesVersion to install (e.g., "1.34")
	KubernetesVersion string `json:"kubernetesVersion"`

	// ContainerRuntime to use (default: containerd)
	ContainerRuntime string `json:"containerRuntime,omitempty"`

	// TailscaleAuthKeySecretRef references a Secret containing the Tailscale auth key
	// Secret should have key "authKey"
	TailscaleAuthKeySecretRef string `json:"tailscaleAuthKeySecretRef,omitempty"`

	// SSHCredentialsSecretRef references a Secret containing SSH credentials for bootstrap
	// Secret should have keys "privateKey" and optionally "username" (default: ubuntu)
	SSHCredentialsSecretRef string `json:"sshCredentialsSecretRef,omitempty"`

	// AdminUsername for SSH access (default: ubuntu)
	AdminUsername string `json:"adminUsername,omitempty"`

	// CustomBootstrapScript allows overriding the default bootstrap script
	// If empty, uses the built-in k8s worker bootstrap script
	CustomBootstrapScript string `json:"customBootstrapScript,omitempty"`
}

// ProvisioningProfileStatus defines the observed state of ProvisioningProfile
type ProvisioningProfileStatus struct {
	// Ready indicates if the profile is valid and ready for use
	Ready bool `json:"ready,omitempty"`

	// Message provides additional status information
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="K8sVersion",type="string",JSONPath=".spec.kubernetesVersion"
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
