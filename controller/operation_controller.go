package controller

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	api "github.com/vpatelsj/stargate/api/v1alpha1"
	"github.com/vpatelsj/stargate/dcclient"
)

// OperationReconciler reconciles an Operation object
type OperationReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	DCClient dcclient.Client
}

// +kubebuilder:rbac:groups=stargate.io,resources=operations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=stargate.io,resources=operations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=stargate.io,resources=operations/finalizers,verbs=update
// +kubebuilder:rbac:groups=stargate.io,resources=servers,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=stargate.io,resources=servers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=stargate.io,resources=provisioningprofiles,verbs=get;list;watch

// Reconcile handles Operation reconciliation
func (r *OperationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the Operation
	var operation api.Operation
	if err := r.Get(ctx, req.NamespacedName, &operation); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Skip if already completed
	if operation.Status.Phase == api.OperationPhaseSucceeded || operation.Status.Phase == api.OperationPhaseFailed {
		return ctrl.Result{}, nil
	}

	// Fetch referenced Server
	var server api.Server
	serverKey := client.ObjectKey{
		Namespace: operation.Namespace,
		Name:      operation.Spec.ServerRef.Name,
	}
	if err := r.Get(ctx, serverKey, &server); err != nil {
		logger.Error(err, "Failed to get Server", "server", operation.Spec.ServerRef.Name)
		return r.updateOperationStatus(ctx, &operation, api.OperationPhaseFailed, fmt.Sprintf("Server not found: %v", err))
	}

	// Fetch referenced ProvisioningProfile
	var profile api.ProvisioningProfile
	profileKey := client.ObjectKey{
		Namespace: operation.Namespace,
		Name:      operation.Spec.ProvisioningProfileRef.Name,
	}
	if err := r.Get(ctx, profileKey, &profile); err != nil {
		logger.Error(err, "Failed to get ProvisioningProfile", "provisioningProfile", operation.Spec.ProvisioningProfileRef.Name)
		return r.updateOperationStatus(ctx, &operation, api.OperationPhaseFailed, fmt.Sprintf("ProvisioningProfile not found: %v", err))
	}

	// Handle based on current phase
	switch operation.Status.Phase {
	case "", api.OperationPhasePending:
		return r.handlePending(ctx, &operation, &server, &profile)
	case api.OperationPhaseRunning:
		return r.handleRunning(ctx, &operation, &server, &profile)
	default:
		return ctrl.Result{}, nil
	}
}

// handlePending initiates the repave operation
func (r *OperationReconciler) handlePending(ctx context.Context, operation *api.Operation, server *api.Server, profile *api.ProvisioningProfile) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Initiating repave", "server", server.Name, "osVersion", profile.Spec.OSVersion)

	// Update server status to provisioning
	server.Status.State = "provisioning"
	server.Status.LastUpdated = metav1.Now()
	if err := r.Status().Update(ctx, server); err != nil {
		logger.Error(err, "Failed to update Server status")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, err
	}

	// Call DC API to initiate repave
	resp, err := r.DCClient.Repave(ctx, dcclient.RepaveRequest{
		ServerID:  server.Name,
		MAC:       server.Spec.MAC,
		IPv4:      server.Spec.IPv4,
		OSVersion: profile.Spec.OSVersion,
	})
	if err != nil {
		logger.Error(err, "Failed to initiate repave")
		return r.updateOperationStatus(ctx, operation, api.OperationPhaseFailed, fmt.Sprintf("Failed to initiate repave: %v", err))
	}

	// Update operation status to running
	operation.Status.Phase = api.OperationPhaseRunning
	operation.Status.DCJobID = resp.JobID
	now := metav1.Now()
	operation.Status.StartTime = &now
	operation.Status.Message = "Repave initiated"

	if err := r.Status().Update(ctx, operation); err != nil {
		logger.Error(err, "Failed to update Operation status")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, err
	}

	// Requeue to poll for status
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// handleRunning polls the DC API for job status
func (r *OperationReconciler) handleRunning(ctx context.Context, operation *api.Operation, server *api.Server, profile *api.ProvisioningProfile) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if operation.Status.DCJobID == "" {
		return r.updateOperationStatus(ctx, operation, api.OperationPhaseFailed, "No DC job ID found")
	}

	// Poll DC API for status
	status, err := r.DCClient.GetJobStatus(ctx, operation.Status.DCJobID)
	if err != nil {
		logger.Error(err, "Failed to get job status from DC API")
		// Don't fail immediately, requeue and retry
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	logger.Info("DC job status", "phase", status.Phase, "message", status.Message)

	switch status.Phase {
	case "succeeded":
		// Update server status
		server.Status.State = "ready"
		server.Status.CurrentOS = profile.Spec.OSVersion
		server.Status.LastUpdated = metav1.Now()
		if err := r.Status().Update(ctx, server); err != nil {
			logger.Error(err, "Failed to update Server status")
			return ctrl.Result{RequeueAfter: 5 * time.Second}, err
		}

		return r.updateOperationStatus(ctx, operation, api.OperationPhaseSucceeded, "Repave completed successfully")

	case "failed":
		// Update server status
		server.Status.State = "error"
		server.Status.LastUpdated = metav1.Now()
		server.Status.Message = status.Message
		if err := r.Status().Update(ctx, server); err != nil {
			logger.Error(err, "Failed to update Server status")
		}

		return r.updateOperationStatus(ctx, operation, api.OperationPhaseFailed, status.Message)

	default:
		// Still running, update message and requeue
		operation.Status.Message = status.Message
		if err := r.Status().Update(ctx, operation); err != nil {
			logger.Error(err, "Failed to update Operation status")
		}
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
}

// updateOperationStatus updates the operation status and returns appropriate result
func (r *OperationReconciler) updateOperationStatus(ctx context.Context, operation *api.Operation, phase api.OperationPhase, message string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	operation.Status.Phase = phase
	operation.Status.Message = message

	if phase == api.OperationPhaseSucceeded || phase == api.OperationPhaseFailed {
		now := metav1.Now()
		operation.Status.CompletionTime = &now
	}

	if err := r.Status().Update(ctx, operation); err != nil {
		logger.Error(err, "Failed to update Operation status")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, err
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager
func (r *OperationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&api.Operation{}).
		Complete(r)
}
