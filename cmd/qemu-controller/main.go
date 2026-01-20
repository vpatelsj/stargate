package main

import (
	"flag"
	"os"
	"path/filepath"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
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
	var kubeconfig string

	// Control plane configuration
	var controlPlaneTailscaleIP string
	var controlPlaneHostname string

	// SSH configuration
	var sshPrivateKeyPath string
	var sshPort int
	var adminUsername string

	// Handle kubeconfig path when running as sudo
	defaultKubeconfig := filepath.Join(os.Getenv("HOME"), ".kube", "config")
	if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" {
		defaultKubeconfig = filepath.Join("/home", sudoUser, ".kube", "config")
	}

	if flag.CommandLine.Lookup("kubeconfig") == nil {
		flag.StringVar(&kubeconfig, "kubeconfig", defaultKubeconfig, "Path to kubeconfig file.")
	} else {
		// If another package already registered this flag, defer to its value
		kubeconfig = flag.CommandLine.Lookup("kubeconfig").Value.String()
	}
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8083", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8084", "The address the probe endpoint binds to.")

	// Control plane configuration flags
	flag.StringVar(&controlPlaneTailscaleIP, "control-plane-ip", "", "Tailscale IP of the control plane (auto-detected if not provided).")
	flag.StringVar(&controlPlaneHostname, "control-plane-hostname", "", "Hostname of the control plane (auto-detected if not provided).")

	// SSH configuration flags
	flag.StringVar(&sshPrivateKeyPath, "ssh-private-key", "", "Path to SSH private key for VM bootstrap (default: ~/.ssh/id_rsa).")
	flag.IntVar(&sshPort, "ssh-port", 22, "SSH port for VM bootstrap.")
	flag.StringVar(&adminUsername, "admin-username", "ubuntu", "Admin username for SSH.")

	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	if kubeconfig == "" {
		if existing := flag.CommandLine.Lookup("kubeconfig"); existing != nil {
			kubeconfig = existing.Value.String()
		}
	}
	if kubeconfig == "" {
		kubeconfig = defaultKubeconfig
	}

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// Resolve SSH private key path
	if sshPrivateKeyPath == "" {
		home := os.Getenv("HOME")
		if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" {
			home = filepath.Join("/home", sudoUser)
		}
		sshPrivateKeyPath = filepath.Join(home, ".ssh", "id_rsa")
	}

	// Load kubeconfig
	restConfig, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		setupLog.Error(err, "unable to load kubeconfig", "kubeconfig", kubeconfig)
		os.Exit(1)
	}

	// Create manager
	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
	})
	if err != nil {
		setupLog.Error(err, "unable to create manager")
		os.Exit(1)
	}

	// Set up QEMU Operation controller
	if err = (&controller.QemuOperationReconciler{
		Client:                  mgr.GetClient(),
		Scheme:                  mgr.GetScheme(),
		ControlPlaneTailscaleIP: controlPlaneTailscaleIP,
		ControlPlaneHostname:    controlPlaneHostname,
		SSHPrivateKeyPath:       sshPrivateKeyPath,
		SSHPort:                 sshPort,
		AdminUsername:           adminUsername,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "QemuOperation")
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

	setupLog.Info("starting qemu-controller manager",
		"controlPlaneTailscaleIP", controlPlaneTailscaleIP,
		"sshPrivateKeyPath", sshPrivateKeyPath,
		"adminUsername", adminUsername,
	)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
