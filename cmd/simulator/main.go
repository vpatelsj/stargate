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
		For(&api.Operation{}).
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

// SimulatorReconciler reconciles Operation objects and creates QEMU VMs
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

	// Fetch the Operation
	var operation api.Operation
	if err := r.Get(ctx, req.NamespacedName, &operation); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Skip if not a repave operation
	if operation.Spec.Operation != api.OperationTypeRepave {
		log.V(1).Info("Skipping non-repave operation", "operation", operation.Spec.Operation)
		return ctrl.Result{}, nil
	}

	// Skip if already completed or failed
	if operation.Status.Phase == api.OperationPhaseSucceeded || operation.Status.Phase == api.OperationPhaseFailed {
		return ctrl.Result{}, nil
	}

	// Fetch referenced Server
	var server api.Server
	serverKey := client.ObjectKey{
		Name:      operation.Spec.ServerRef.Name,
		Namespace: req.Namespace,
	}
	if err := r.Get(ctx, serverKey, &server); err != nil {
		log.Error(err, "Failed to fetch Server")
		return r.setOperationFailed(ctx, &operation, "Failed to fetch Server: "+err.Error())
	}

	// Fetch referenced ProvisioningProfile
	var profile api.ProvisioningProfile
	profileKey := client.ObjectKey{
		Name:      operation.Spec.ProvisioningProfileRef.Name,
		Namespace: req.Namespace,
	}
	if err := r.Get(ctx, profileKey, &profile); err != nil {
		log.Error(err, "Failed to fetch ProvisioningProfile")
		return r.setOperationFailed(ctx, &operation, "Failed to fetch ProvisioningProfile: "+err.Error())
	}

	vmName := server.Name

	switch operation.Status.Phase {
	case "", api.OperationPhasePending:
		return r.handlePending(ctx, &operation, &server, &profile, vmName)
	case api.OperationPhaseRunning:
		return r.handleRunning(ctx, &operation, &server, vmName)
	}

	return ctrl.Result{}, nil
}

func (r *SimulatorReconciler) handlePending(ctx context.Context, operation *api.Operation, server *api.Server, profile *api.ProvisioningProfile, vmName string) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)
	log.Info("Starting repave operation", "server", server.Name, "provisioningProfile", profile.Name)

	// Download base image if needed
	imageURL := profile.Spec.OSImage
	if imageURL == "" {
		imageURL = qemu.UbuntuCloudImageURL
	}

	basePath, err := r.ImageMgr.EnsureImage(ctx, imageURL)
	if err != nil {
		log.Error(err, "Failed to download base image")
		return r.setOperationFailed(ctx, operation, "Failed to download base image: "+err.Error())
	}

	// Create tap device first to allocate IP
	tapDevice, err := r.NetworkMgr.CreateTap(ctx, vmName)
	if err != nil {
		log.Error(err, "Failed to create tap device")
		return r.setOperationFailed(ctx, operation, "Failed to create tap device: "+err.Error())
	}

	// Allocate IP and generate MAC
	vmIP := r.NetworkMgr.AllocateIP(vmName)
	macAddr := r.NetworkMgr.GenerateMAC(vmName)

	// Generate cloud-init ISO with network config
	cloudInitConfig := qemu.CloudInitConfig{
		InstanceID: vmName,
		Hostname:   vmName,
		UserData:   profile.Spec.CloudInit,
		IPAddress:  vmIP,
		Gateway:    qemu.DefaultBridgeIP,
	}
	isoPath, err := r.CloudInitGen.GenerateISO(ctx, cloudInitConfig)
	if err != nil {
		log.Error(err, "Failed to generate cloud-init ISO")
		return r.setOperationFailed(ctx, operation, "Failed to generate cloud-init ISO: "+err.Error())
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
		return r.setOperationFailed(ctx, operation, "Failed to create VM: "+err.Error())
	}

	// Start VM
	if err := vm.Start(ctx); err != nil {
		log.Error(err, "Failed to start VM")
		return r.setOperationFailed(ctx, operation, "Failed to start VM: "+err.Error())
	}

	// Update operation status to Running
	now := metav1.Now()
	operation.Status.Phase = api.OperationPhaseRunning
	operation.Status.StartTime = &now
	operation.Status.Message = "VM started, waiting for provisioning"
	if err := r.Status().Update(ctx, operation); err != nil {
		log.Error(err, "Failed to update Operation status")
		return ctrl.Result{}, err
	}

	// Update server status
	server.Status.State = "provisioning"
	server.Status.Message = "VM started at " + vmIP
	server.Status.LastUpdated = now
	if err := r.Status().Update(ctx, server); err != nil {
		log.Error(err, "Failed to update Server status")
	}

	// Requeue to check completion
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

func (r *SimulatorReconciler) handleRunning(ctx context.Context, operation *api.Operation, server *api.Server, vmName string) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	vm, ok := r.VMs[vmName]
	if !ok {
		log.Info("VM not tracked, assuming completed from previous run")
		return r.setOperationSucceeded(ctx, operation, server, vmName)
	}

	status, err := vm.Status()
	if err != nil {
		log.Error(err, "Failed to get VM status")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	if !status.Running {
		log.Info("VM is no longer running, marking as failed")
		return r.setOperationFailed(ctx, operation, "VM stopped unexpectedly")
	}

	// Check if enough time has passed for provisioning (simplified check)
	if operation.Status.StartTime != nil {
		elapsed := time.Since(operation.Status.StartTime.Time)
		// After 3 minutes, assume provisioning is complete
		// In a real implementation, we would check if the node has joined the cluster
		if elapsed > 3*time.Minute {
			log.Info("Provisioning timeout reached, marking as succeeded")
			return r.setOperationSucceeded(ctx, operation, server, vmName)
		}
	}

	// Still provisioning, requeue
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

func (r *SimulatorReconciler) setOperationFailed(ctx context.Context, operation *api.Operation, message string) (ctrl.Result, error) {
	now := metav1.Now()
	operation.Status.Phase = api.OperationPhaseFailed
	operation.Status.CompletionTime = &now
	operation.Status.Message = message
	if err := r.Status().Update(ctx, operation); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *SimulatorReconciler) setOperationSucceeded(ctx context.Context, operation *api.Operation, server *api.Server, vmName string) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)
	now := metav1.Now()

	// Update operation
	operation.Status.Phase = api.OperationPhaseSucceeded
	operation.Status.CompletionTime = &now
	operation.Status.Message = "VM provisioning complete"
	if err := r.Status().Update(ctx, operation); err != nil {
		log.Error(err, "Failed to update Operation status")
		return ctrl.Result{}, err
	}

	// Update server
	vmIP, _ := r.NetworkMgr.GetIP(vmName)
	server.Status.State = "ready"
	server.Status.Message = "Provisioned successfully"
	server.Status.LastUpdated = now
	server.Spec.IPv4 = vmIP
	if err := r.Update(ctx, server); err != nil {
		log.Error(err, "Failed to update Server")
	}
	if err := r.Status().Update(ctx, server); err != nil {
		log.Error(err, "Failed to update Server status")
	}

	return ctrl.Result{}, nil
}
