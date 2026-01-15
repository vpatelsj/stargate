package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// OperationPhase represents the current phase of an Operation
type OperationPhase string

const (
	OperationPhasePending   OperationPhase = "Pending"
	OperationPhaseRunning   OperationPhase = "Running"
	OperationPhaseSucceeded OperationPhase = "Succeeded"
	OperationPhaseFailed    OperationPhase = "Failed"
)

// OperationType represents the type of operation to perform
type OperationType string

const (
	OperationTypeRepave OperationType = "repave"
	OperationTypeReboot OperationType = "reboot"
)

// OperationSpec defines the desired state of Operation
type OperationSpec struct {
	// ServerRef references the Server to operate on
	ServerRef LocalObjectReference `json:"serverRef"`

	// ProvisioningProfileRef references the ProvisioningProfile to use for provisioning
	ProvisioningProfileRef LocalObjectReference `json:"provisioningProfileRef"`

	// Operation to perform (e.g., "repave", "reboot")
	Operation OperationType `json:"operation"`
}

// LocalObjectReference contains enough information to locate the referenced resource
type LocalObjectReference struct {
	// Name of the referenced resource
	Name string `json:"name"`
}

// OperationStatus defines the observed state of Operation
type OperationStatus struct {
	// Phase of the operation: Pending, Running, Succeeded, Failed
	Phase OperationPhase `json:"phase,omitempty"`

	// StartTime is when the operation started
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// CompletionTime is when the operation completed
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// Message provides additional status information
	Message string `json:"message,omitempty"`

	// DCJobID is the ID returned by the datacenter API
	DCJobID string `json:"dcJobID,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Server",type="string",JSONPath=".spec.serverRef.name"
// +kubebuilder:printcolumn:name="Operation",type="string",JSONPath=".spec.operation"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// Operation represents an operation to be performed on Server
type Operation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   OperationSpec   `json:"spec,omitempty"`
	Status OperationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// OperationList contains a list of Operation
type OperationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Operation `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Operation{}, &OperationList{})
}
