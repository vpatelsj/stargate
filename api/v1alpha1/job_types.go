package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// JobPhase represents the current phase of a Job
type JobPhase string

const (
	JobPhasePending   JobPhase = "Pending"
	JobPhaseRunning   JobPhase = "Running"
	JobPhaseSucceeded JobPhase = "Succeeded"
	JobPhaseFailed    JobPhase = "Failed"
)

// JobOperation represents the type of operation to perform
type JobOperation string

const (
	JobOperationRepave JobOperation = "repave"
	JobOperationReboot JobOperation = "reboot"
)

// JobSpec defines the desired state of Job
type JobSpec struct {
	// HardwareRef references the Hardware to operate on
	HardwareRef LocalObjectReference `json:"hardwareRef"`

	// TemplateRef references the Template to use for provisioning
	TemplateRef LocalObjectReference `json:"templateRef"`

	// Operation to perform (e.g., "repave", "reboot")
	Operation JobOperation `json:"operation"`
}

// LocalObjectReference contains enough information to locate the referenced resource
type LocalObjectReference struct {
	// Name of the referenced resource
	Name string `json:"name"`
}

// JobStatus defines the observed state of Job
type JobStatus struct {
	// Phase of the job: Pending, Running, Succeeded, Failed
	Phase JobPhase `json:"phase,omitempty"`

	// StartTime is when the job started
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// CompletionTime is when the job completed
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// Message provides additional status information
	Message string `json:"message,omitempty"`

	// DCJobID is the ID returned by the datacenter API
	DCJobID string `json:"dcJobID,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Hardware",type="string",JSONPath=".spec.hardwareRef.name"
// +kubebuilder:printcolumn:name="Operation",type="string",JSONPath=".spec.operation"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// Job represents an operation to be performed on Hardware
type Job struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   JobSpec   `json:"spec,omitempty"`
	Status JobStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// JobList contains a list of Job
type JobList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Job `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Job{}, &JobList{})
}
