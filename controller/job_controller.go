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

// JobReconciler reconciles a Job object
type JobReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	DCClient dcclient.Client
}

// +kubebuilder:rbac:groups=stargate.io,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=stargate.io,resources=jobs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=stargate.io,resources=jobs/finalizers,verbs=update
// +kubebuilder:rbac:groups=stargate.io,resources=hardware,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=stargate.io,resources=hardware/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=stargate.io,resources=templates,verbs=get;list;watch

// Reconcile handles Job reconciliation
func (r *JobReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the Job
	var job api.Job
	if err := r.Get(ctx, req.NamespacedName, &job); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Skip if already completed
	if job.Status.Phase == api.JobPhaseSucceeded || job.Status.Phase == api.JobPhaseFailed {
		return ctrl.Result{}, nil
	}

	// Fetch referenced Hardware
	var hardware api.Hardware
	hardwareKey := client.ObjectKey{
		Namespace: job.Namespace,
		Name:      job.Spec.HardwareRef.Name,
	}
	if err := r.Get(ctx, hardwareKey, &hardware); err != nil {
		logger.Error(err, "Failed to get Hardware", "hardware", job.Spec.HardwareRef.Name)
		return r.updateJobStatus(ctx, &job, api.JobPhaseFailed, fmt.Sprintf("Hardware not found: %v", err))
	}

	// Fetch referenced Template
	var template api.Template
	templateKey := client.ObjectKey{
		Namespace: job.Namespace,
		Name:      job.Spec.TemplateRef.Name,
	}
	if err := r.Get(ctx, templateKey, &template); err != nil {
		logger.Error(err, "Failed to get Template", "template", job.Spec.TemplateRef.Name)
		return r.updateJobStatus(ctx, &job, api.JobPhaseFailed, fmt.Sprintf("Template not found: %v", err))
	}

	// Handle based on current phase
	switch job.Status.Phase {
	case "", api.JobPhasePending:
		return r.handlePending(ctx, &job, &hardware, &template)
	case api.JobPhaseRunning:
		return r.handleRunning(ctx, &job, &hardware, &template)
	default:
		return ctrl.Result{}, nil
	}
}

// handlePending initiates the repave operation
func (r *JobReconciler) handlePending(ctx context.Context, job *api.Job, hardware *api.Hardware, template *api.Template) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Initiating repave", "hardware", hardware.Name, "osVersion", template.Spec.OSVersion)

	// Update hardware status to provisioning
	hardware.Status.State = "provisioning"
	hardware.Status.LastUpdated = metav1.Now()
	if err := r.Status().Update(ctx, hardware); err != nil {
		logger.Error(err, "Failed to update Hardware status")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, err
	}

	// Call DC API to initiate repave
	resp, err := r.DCClient.Repave(ctx, dcclient.RepaveRequest{
		ServerID:  hardware.Name,
		MAC:       hardware.Spec.MAC,
		IPv4:      hardware.Spec.IPv4,
		OSVersion: template.Spec.OSVersion,
	})
	if err != nil {
		logger.Error(err, "Failed to initiate repave")
		return r.updateJobStatus(ctx, job, api.JobPhaseFailed, fmt.Sprintf("Failed to initiate repave: %v", err))
	}

	// Update job status to running
	job.Status.Phase = api.JobPhaseRunning
	job.Status.DCJobID = resp.JobID
	now := metav1.Now()
	job.Status.StartTime = &now
	job.Status.Message = "Repave initiated"

	if err := r.Status().Update(ctx, job); err != nil {
		logger.Error(err, "Failed to update Job status")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, err
	}

	// Requeue to poll for status
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// handleRunning polls the DC API for job status
func (r *JobReconciler) handleRunning(ctx context.Context, job *api.Job, hardware *api.Hardware, template *api.Template) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if job.Status.DCJobID == "" {
		return r.updateJobStatus(ctx, job, api.JobPhaseFailed, "No DC job ID found")
	}

	// Poll DC API for status
	status, err := r.DCClient.GetJobStatus(ctx, job.Status.DCJobID)
	if err != nil {
		logger.Error(err, "Failed to get job status from DC API")
		// Don't fail immediately, requeue and retry
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	logger.Info("DC job status", "phase", status.Phase, "message", status.Message)

	switch status.Phase {
	case "succeeded":
		// Update hardware status
		hardware.Status.State = "ready"
		hardware.Status.CurrentOS = template.Spec.OSVersion
		hardware.Status.LastUpdated = metav1.Now()
		if err := r.Status().Update(ctx, hardware); err != nil {
			logger.Error(err, "Failed to update Hardware status")
			return ctrl.Result{RequeueAfter: 5 * time.Second}, err
		}

		return r.updateJobStatus(ctx, job, api.JobPhaseSucceeded, "Repave completed successfully")

	case "failed":
		// Update hardware status
		hardware.Status.State = "error"
		hardware.Status.LastUpdated = metav1.Now()
		hardware.Status.Message = status.Message
		if err := r.Status().Update(ctx, hardware); err != nil {
			logger.Error(err, "Failed to update Hardware status")
		}

		return r.updateJobStatus(ctx, job, api.JobPhaseFailed, status.Message)

	default:
		// Still running, update message and requeue
		job.Status.Message = status.Message
		if err := r.Status().Update(ctx, job); err != nil {
			logger.Error(err, "Failed to update Job status")
		}
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
}

// updateJobStatus updates the job status and returns appropriate result
func (r *JobReconciler) updateJobStatus(ctx context.Context, job *api.Job, phase api.JobPhase, message string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	job.Status.Phase = phase
	job.Status.Message = message

	if phase == api.JobPhaseSucceeded || phase == api.JobPhaseFailed {
		now := metav1.Now()
		job.Status.CompletionTime = &now
	}

	if err := r.Status().Update(ctx, job); err != nil {
		logger.Error(err, "Failed to update Job status")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, err
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager
func (r *JobReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&api.Job{}).
		Complete(r)
}
