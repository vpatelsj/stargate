package main

import (
	"flag"
	"os"
	"path/filepath"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	api "github.com/vpatelsj/stargate/api/v1alpha1"
	"github.com/vpatelsj/stargate/controller"
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
	var probeAddr string

	// Bootstrap configuration flags
	var kindContainerName string
	var controlPlaneTailscaleIP string
	var controlPlaneHostname string
	var sshPrivateKeyPath string
	var sshPort int
	var adminUsername string

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8081", "The address the metric endpoint binds to.")

	// Bootstrap configuration flags
	flag.StringVar(&kindContainerName, "kind-container", "stargate-demo-control-plane", "Name of the Kind control plane Docker container.")
	flag.StringVar(&controlPlaneTailscaleIP, "control-plane-ip", "", "Tailscale IP of the Kind control plane (auto-detected if not provided).")
	flag.StringVar(&controlPlaneHostname, "control-plane-hostname", "stargate-demo-control-plane", "Hostname of the Kind control plane.")
	flag.StringVar(&sshPrivateKeyPath, "ssh-private-key", filepath.Join(os.Getenv("HOME"), ".ssh", "id_rsa"), "Path to SSH private key for server bootstrap.")
	flag.IntVar(&sshPort, "ssh-port", 22, "SSH port for server bootstrap.")
	flag.StringVar(&adminUsername, "admin-username", "ubuntu", "Admin username for SSH.")

	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
	})
	if err != nil {
		setupLog.Error(err, "unable to create manager")
		os.Exit(1)
	}

	// Set up Operation controller
	if err = (&controller.OperationReconciler{
		Client:                  mgr.GetClient(),
		Scheme:                  mgr.GetScheme(),
		KindContainerName:       kindContainerName,
		ControlPlaneTailscaleIP: controlPlaneTailscaleIP,
		ControlPlaneHostname:    controlPlaneHostname,
		SSHPrivateKeyPath:       sshPrivateKeyPath,
		SSHPort:                 sshPort,
		AdminUsername:           adminUsername,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Operation")
		os.Exit(1)
	}

	// Add health checks
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
