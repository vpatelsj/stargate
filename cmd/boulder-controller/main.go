package main

import (
	"encoding/base64"
	"flag"
	"os"
	"path/filepath"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
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

// Boulder Controller
//
// This controller manages physical servers in the Boulder lab datacenter.
// Unlike azure-controller, it doesn't manage Azure infrastructure - it only
// bootstraps kubelet on physical servers that already exist on the network.
//
// Architecture:
//   - Physical servers are on VLAN 10 (172.18.10.0/24)
//   - Tailscale subnet router (stargate-boulderlab-gateway) advertises lab routes
//   - Workers connect to AKS API server via Tailscale mesh
//   - No Azure route tables needed - Tailscale handles routing
//
// Usage:
//   ./bin/boulder-controller \
//     -control-plane-mode aks \
//     -aks-api-server "https://<aks-fqdn>:443" \
//     -aks-cluster-name <cluster> \
//     -namespace boulder-dc

func main() {
	var metricsAddr string
	var probeAddr string
	var namespace string

	// Bootstrap configuration flags
	var controlPlaneMode string
	var sshPrivateKeyPath string
	var sshPort int
	var adminUsername string

	// AKS configuration flags
	var aksAPIServer string
	var aksClusterName string
	var aksResourceGroup string
	var aksClusterDNS string
	var aksSubscriptionID string

	// Boulder-specific flags
	var boulderGatewayIP string // Tailscale IP of stargate-boulderlab-gateway
	var aksRouterTailscaleIP string
	var dcRouterTailscaleIP string
	var azureRouteTableName string
	var azureRouteTableRG string
	var aksRouterPrivateIP string

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8082", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8083", "The address the probe endpoint binds to.")
	flag.StringVar(&namespace, "namespace", "boulder-dc", "Namespace to watch for Operation CRs.")

	// Bootstrap configuration flags
	flag.StringVar(&controlPlaneMode, "control-plane-mode", "aks", "Mode to access control plane: 'aks' (AKS TLS bootstrap).")
	flag.StringVar(&sshPrivateKeyPath, "ssh-private-key", filepath.Join(os.Getenv("HOME"), ".ssh", "id_rsa"), "Path to SSH private key for server bootstrap.")
	flag.IntVar(&sshPort, "ssh-port", 22, "SSH port for server bootstrap.")
	flag.StringVar(&adminUsername, "admin-username", "ubuntu", "Admin username for SSH to physical servers.")

	// AKS configuration flags
	flag.StringVar(&aksAPIServer, "aks-api-server", "", "AKS API server URL (auto-detected from kubeconfig if empty).")
	flag.StringVar(&aksClusterName, "aks-cluster-name", "", "AKS cluster name (used for node labels).")
	flag.StringVar(&aksResourceGroup, "aks-resource-group", "", "AKS cluster resource group (used for node labels).")
	flag.StringVar(&aksClusterDNS, "aks-cluster-dns", "10.0.0.10", "AKS cluster DNS service IP.")
	flag.StringVar(&aksSubscriptionID, "aks-subscription-id", "", "Azure subscription ID for provider-id.")

	// Boulder-specific flags
	flag.StringVar(&boulderGatewayIP, "boulder-gateway-ip", "", "Tailscale IP of stargate-boulderlab-gateway (for connectivity verification).")
	flag.StringVar(&aksRouterTailscaleIP, "aks-router-tailscale-ip", "", "Tailscale IP of the AKS router (for route configuration).")
	flag.StringVar(&dcRouterTailscaleIP, "dc-router-tailscale-ip", "", "Tailscale IP of the DC router (for route configuration).")
	flag.StringVar(&azureRouteTableName, "azure-route-table-name", "stargate-workers-rt", "Azure route table name for pod CIDR routes.")
	flag.StringVar(&azureRouteTableRG, "azure-route-table-rg", "", "Resource group containing the Azure route table (MC_* group).")
	flag.StringVar(&aksRouterPrivateIP, "aks-router-private-ip", "10.237.0.4", "Private IP of the AKS router (next hop for Azure routes).")

	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	restConfig := ctrl.GetConfigOrDie()

	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
	})
	if err != nil {
		setupLog.Error(err, "unable to create manager")
		os.Exit(1)
	}

	// Create kubernetes clientset for SA token creation
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		setupLog.Error(err, "unable to create kubernetes clientset")
		os.Exit(1)
	}

	// Extract CA cert from rest config
	var caCertBase64 string
	if len(restConfig.CAData) > 0 {
		caCertBase64 = base64.StdEncoding.EncodeToString(restConfig.CAData)
	}

	// Auto-detect API server from rest config if not provided
	if aksAPIServer == "" {
		aksAPIServer = restConfig.Host
	}

	setupLog.Info("Boulder controller starting",
		"namespace", namespace,
		"aksAPIServer", aksAPIServer,
		"aksClusterName", aksClusterName,
		"boulderGatewayIP", boulderGatewayIP,
		"controlPlaneMode", controlPlaneMode,
	)

	// Set up Operation controller
	// Note: We reuse the same OperationReconciler from azure-controller
	// The key difference is:
	//   - No Azure route table management (routes via Tailscale)
	//   - No AKSVMResourceGroup (physical servers, not Azure VMs)
	//   - Workers accessed via their VLAN IPs (172.18.10.x) through Tailscale mesh
	if err = (&controller.OperationReconciler{
		Client:            mgr.GetClient(),
		Scheme:            mgr.GetScheme(),
		ControlPlaneMode:  controlPlaneMode,
		SSHPrivateKeyPath: sshPrivateKeyPath,
		SSHPort:           sshPort,
		AdminUsername:     adminUsername,
		AKSAPIServer:      aksAPIServer,
		AKSClusterName:    aksClusterName,
		AKSResourceGroup:  aksResourceGroup,
		AKSClusterDNS:     aksClusterDNS,
		AKSSubscriptionID: aksSubscriptionID,
		// Physical servers don't need Azure VM resource group for VM management
		// but we need the MC_* resource group for route table management
		AKSVMResourceGroup: azureRouteTableRG,
		// Routing configuration - Boulder uses Tailscale mesh + Azure route tables
		DCRouterTailscaleIP:   dcRouterTailscaleIP,
		AKSRouterTailscaleIP:  aksRouterTailscaleIP,
		AzureRouteTableName:   azureRouteTableName,
		AKSAPIServerPrivateIP: aksRouterPrivateIP, // Used as next hop in Azure route table
		AzureVNetName:         "",
		AzureSubnetName:       "",
		// Boulder-specific: the Tailscale IP of the Boulder gateway (used for route config)
		BoulderGatewayIP: boulderGatewayIP,
		// Boulder controller handles "boulder" provider servers
		ProviderFilter: "boulder",
		Clientset:      clientset,
		CACertBase64:   caCertBase64,
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
