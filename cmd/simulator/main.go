package main

import (
	"context"
	"flag"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	api "github.com/vpatelsj/stargate/api/v1alpha1"
	"github.com/vpatelsj/stargate/pkg/qemu"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(api.AddToScheme(scheme))
}

func main() {
	var metricsAddr string
	var workDir string

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8083", "The address the metric endpoint binds to.")
	flag.StringVar(&workDir, "work-dir", "/var/lib/stargate", "Working directory for VM storage.")

	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: metricsAddr},
	})
	if err != nil {
		setupLog.Error(err, "unable to create manager")
		os.Exit(1)
	}

	// Initialize QEMU components
	logger := ctrl.Log.WithName("simulator")
	networkMgr := qemu.NewNetworkManager(logger)
	imageMgr := qemu.NewImageManager(workDir+"/images", logger)
	cloudInitGen := qemu.NewCloudInitGenerator(workDir+"/vms", logger)

	// Setup bridge network
	ctx := context.Background()
	if err := networkMgr.SetupBridge(ctx); err != nil {
		setupLog.Error(err, "failed to setup bridge network")
		os.Exit(1)
	}

	// Setup controller
	reconciler := &SimulatorReconciler{
		Client:       mgr.GetClient(),
		Scheme:       mgr.GetScheme(),
		NetworkMgr:   networkMgr,
		ImageMgr:     imageMgr,
		CloudInitGen: cloudInitGen,
		WorkDir:      workDir,
		VMs:          make(map[string]*qemu.VM),
	}

	if err := ctrl.NewControllerManagedBy(mgr).
		For(&api.Job{}).
		Complete(reconciler); err != nil {
		setupLog.Error(err, "unable to create controller")
		os.Exit(1)
	}

	setupLog.Info("starting simulator controller")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

// SimulatorReconciler reconciles Job objects and creates QEMU VMs
type SimulatorReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	NetworkMgr   *qemu.NetworkManager
	ImageMgr     *qemu.ImageManager
	CloudInitGen *qemu.CloudInitGenerator
	WorkDir      string
	VMs          map[string]*qemu.VM
}

func (r *SimulatorReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	// Fetch the Job
	var job api.Job
	if err := r.Get(ctx, req.NamespacedName, &job); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Skip if not a repave operation
	if job.Spec.Operation != api.JobOperationRepave {
		log.V(1).Info("Skipping non-repave job", "operation", job.Spec.Operation)
		return ctrl.Result{}, nil
	}

	// Skip if already completed or failed
	if job.Status.Phase == api.JobPhaseSucceeded || job.Status.Phase == api.JobPhaseFailed {
		return ctrl.Result{}, nil
	}

	// Fetch referenced Hardware
	var hardware api.Hardware
	hardwareKey := client.ObjectKey{
		Name:      job.Spec.HardwareRef.Name,
		Namespace: req.Namespace,
	}
	if err := r.Get(ctx, hardwareKey, &hardware); err != nil {
		log.Error(err, "Failed to fetch Hardware")
		return r.setJobFailed(ctx, &job, "Failed to fetch Hardware: "+err.Error())
	}

	// Fetch referenced Template
	var template api.Template
	templateKey := client.ObjectKey{
		Name:      job.Spec.TemplateRef.Name,
		Namespace: req.Namespace,
	}
	if err := r.Get(ctx, templateKey, &template); err != nil {
		log.Error(err, "Failed to fetch Template")
		return r.setJobFailed(ctx, &job, "Failed to fetch Template: "+err.Error())
	}

	vmName := hardware.Name

	switch job.Status.Phase {
	case "", api.JobPhasePending:
		return r.handlePending(ctx, &job, &hardware, &template, vmName)
	case api.JobPhaseRunning:
		return r.handleRunning(ctx, &job, &hardware, vmName)
	}

	return ctrl.Result{}, nil
}

func (r *SimulatorReconciler) handlePending(ctx context.Context, job *api.Job, hardware *api.Hardware, template *api.Template, vmName string) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)
	log.Info("Starting repave operation", "hardware", hardware.Name, "template", template.Name)

	// Download base image if needed
	imageURL := template.Spec.OSImage
	if imageURL == "" {
		imageURL = qemu.UbuntuCloudImageURL
	}

	basePath, err := r.ImageMgr.EnsureImage(ctx, imageURL)
	if err != nil {
		log.Error(err, "Failed to download base image")
		return r.setJobFailed(ctx, job, "Failed to download base image: "+err.Error())
	}

	// Create tap device first to allocate IP
	tapDevice, err := r.NetworkMgr.CreateTap(ctx, vmName)
	if err != nil {
		log.Error(err, "Failed to create tap device")
		return r.setJobFailed(ctx, job, "Failed to create tap device: "+err.Error())
	}

	// Allocate IP and generate MAC
	vmIP := r.NetworkMgr.AllocateIP(vmName)
	macAddr := r.NetworkMgr.GenerateMAC(vmName)

	// Generate cloud-init ISO with network config
	cloudInitConfig := qemu.CloudInitConfig{
		InstanceID: vmName,
		Hostname:   vmName,
		UserData:   template.Spec.CloudInit,
		IPAddress:  vmIP,
		Gateway:    qemu.DefaultBridgeIP,
	}
	isoPath, err := r.CloudInitGen.GenerateISO(ctx, cloudInitConfig)
	if err != nil {
		log.Error(err, "Failed to generate cloud-init ISO")
		return r.setJobFailed(ctx, job, "Failed to generate cloud-init ISO: "+err.Error())
	}

	// Create VM
	vmConfig := qemu.VMConfig{
		Name:         vmName,
		BaseImage:    basePath,
		CloudInitISO: isoPath,
		TapDevice:    tapDevice,
		MACAddress:   macAddr,
		WorkDir:      r.WorkDir + "/vms",
	}

	vm := qemu.NewVM(vmConfig, ctrl.Log.WithName("vm"))
	r.VMs[vmName] = vm

	// Create VM disk
	if err := vm.Create(ctx); err != nil {
		log.Error(err, "Failed to create VM disk")
		return r.setJobFailed(ctx, job, "Failed to create VM: "+err.Error())
	}

	// Start VM
	if err := vm.Start(ctx); err != nil {
		log.Error(err, "Failed to start VM")
		return r.setJobFailed(ctx, job, "Failed to start VM: "+err.Error())
	}

	// Update job status to Running
	now := metav1.Now()
	job.Status.Phase = api.JobPhaseRunning
	job.Status.StartTime = &now
	job.Status.Message = "VM started, waiting for provisioning"
	if err := r.Status().Update(ctx, job); err != nil {
		log.Error(err, "Failed to update Job status")
		return ctrl.Result{}, err
	}

	// Update hardware status
	hardware.Status.State = "provisioning"
	hardware.Status.Message = "VM started at " + vmIP
	hardware.Status.LastUpdated = now
	if err := r.Status().Update(ctx, hardware); err != nil {
		log.Error(err, "Failed to update Hardware status")
	}

	// Requeue to check completion
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

func (r *SimulatorReconciler) handleRunning(ctx context.Context, job *api.Job, hardware *api.Hardware, vmName string) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	vm, ok := r.VMs[vmName]
	if !ok {
		log.Info("VM not tracked, assuming completed from previous run")
		return r.setJobSucceeded(ctx, job, hardware, vmName)
	}

	status, err := vm.Status()
	if err != nil {
		log.Error(err, "Failed to get VM status")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	if !status.Running {
		log.Info("VM is no longer running, marking as failed")
		return r.setJobFailed(ctx, job, "VM stopped unexpectedly")
	}

	// Check if enough time has passed for provisioning (simplified check)
	if job.Status.StartTime != nil {
		elapsed := time.Since(job.Status.StartTime.Time)
		// After 3 minutes, assume provisioning is complete
		// In a real implementation, we would check if the node has joined the cluster
		if elapsed > 3*time.Minute {
			log.Info("Provisioning timeout reached, marking as succeeded")
			return r.setJobSucceeded(ctx, job, hardware, vmName)
		}
	}

	// Still provisioning, requeue
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

func (r *SimulatorReconciler) setJobFailed(ctx context.Context, job *api.Job, message string) (ctrl.Result, error) {
	now := metav1.Now()
	job.Status.Phase = api.JobPhaseFailed
	job.Status.CompletionTime = &now
	job.Status.Message = message
	if err := r.Status().Update(ctx, job); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *SimulatorReconciler) setJobSucceeded(ctx context.Context, job *api.Job, hardware *api.Hardware, vmName string) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)
	now := metav1.Now()

	// Update job
	job.Status.Phase = api.JobPhaseSucceeded
	job.Status.CompletionTime = &now
	job.Status.Message = "VM provisioning complete"
	if err := r.Status().Update(ctx, job); err != nil {
		log.Error(err, "Failed to update Job status")
		return ctrl.Result{}, err
	}

	// Update hardware
	vmIP, _ := r.NetworkMgr.GetIP(vmName)
	hardware.Status.State = "ready"
	hardware.Status.Message = "Provisioned successfully"
	hardware.Status.LastUpdated = now
	hardware.Spec.IPv4 = vmIP
	if err := r.Update(ctx, hardware); err != nil {
		log.Error(err, "Failed to update Hardware")
	}
	if err := r.Status().Update(ctx, hardware); err != nil {
		log.Error(err, "Failed to update Hardware status")
	}

	return ctrl.Result{}, nil
}
