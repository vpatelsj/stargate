package dcclient

import (
	"context"
)

// RepaveRequest contains the parameters for a repave operation
type RepaveRequest struct {
	// ServerID identifies the server (typically MAC or IP)
	ServerID string `json:"serverId"`

	// MAC address of the server
	MAC string `json:"mac"`

	// IPv4 address of the server
	IPv4 string `json:"ipv4"`

	// OSVersion to install
	OSVersion string `json:"osVersion"`
}

// RepaveResponse contains the result of initiating a repave
type RepaveResponse struct {
	// JobID assigned by the DC API
	JobID string `json:"jobId"`

	// Status of the request
	Status string `json:"status"`
}

// JobStatus represents the status of an ongoing job
type JobStatus struct {
	// JobID of the operation
	JobID string `json:"jobId"`

	// Phase: pending, running, succeeded, failed
	Phase string `json:"phase"`

	// Message provides additional details
	Message string `json:"message"`
}

// Client defines the interface for datacenter API operations
type Client interface {
	// Repave initiates a repave operation on a server
	Repave(ctx context.Context, req RepaveRequest) (*RepaveResponse, error)

	// GetJobStatus retrieves the status of an ongoing job
	GetJobStatus(ctx context.Context, jobID string) (*JobStatus, error)
}
