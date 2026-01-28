package main

import (
	"encoding/base64"
	"flag"
	"log/slog"
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

func main() {
	var metricsAddr string
	var probeAddr string

	// Bootstrap configuration flags
	var kindContainerName string
	var controlPlaneTailscaleIP string
	var controlPlaneHostname string
	var controlPlaneMode string
	var controlPlaneSSHUser string
	var sshPrivateKeyPath string
	var sshPort int
	var adminUsername string

	// AKS configuration flags
	var aksAPIServer string
	var aksClusterName string
	var aksResourceGroup string
	var aksClusterDNS string
	var aksSubscriptionID string
	var aksVMResourceGroup string
	var aksAPIServerPrivateIP string

	// Routing configuration flags
	var dcRouterTailscaleIP string
	var aksRouterTailscaleIP string
	var azureRouteTableName string
	var azureVNetName string
	var azureSubnetName string

	// Route sync configuration flags
	var enableRouteSync bool
	var aksNodeResourceGroup string
	var routerSubnetName string
	var dcSubnetCIDR string
	var dcPodCIDR string
	var tailscaleAPIKey string
	var tailnetName string

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8081", "The address the metric endpoint binds to.")

	// Bootstrap configuration flags
	flag.StringVar(&kindContainerName, "kind-container", "stargate-demo-control-plane", "Name of the Kind control plane Docker container.")
	flag.StringVar(&controlPlaneTailscaleIP, "control-plane-ip", "", "Tailscale IP or hostname of the control plane (auto-detected for Kind if not provided).")
	flag.StringVar(&controlPlaneHostname, "control-plane-hostname", "stargate-demo-control-plane", "Hostname of the control plane.")
	flag.StringVar(&controlPlaneMode, "control-plane-mode", "kind", "Mode to access control plane: 'kind' (docker exec), 'tailscale' (SSH via tailscale), or 'aks' (AKS TLS bootstrap).")
	flag.StringVar(&controlPlaneSSHUser, "control-plane-ssh-user", "azureuser", "SSH user for control plane when using tailscale mode.")
	flag.StringVar(&sshPrivateKeyPath, "ssh-private-key", filepath.Join(os.Getenv("HOME"), ".ssh", "id_rsa"), "Path to SSH private key for server bootstrap.")
	flag.IntVar(&sshPort, "ssh-port", 22, "SSH port for server bootstrap.")
	flag.StringVar(&adminUsername, "admin-username", "ubuntu", "Admin username for SSH.")

	// AKS configuration flags (SA token and CA cert are fetched automatically from kubeconfig)
	flag.StringVar(&aksAPIServer, "aks-api-server", "", "AKS API server URL (auto-detected from kubeconfig if empty).")
	flag.StringVar(&aksClusterName, "aks-cluster-name", "", "AKS cluster name (used for node labels).")
	flag.StringVar(&aksResourceGroup, "aks-resource-group", "", "AKS cluster resource group (used for node labels).")
	flag.StringVar(&aksClusterDNS, "aks-cluster-dns", "10.0.0.10", "AKS cluster DNS service IP.")
	flag.StringVar(&aksSubscriptionID, "aks-subscription-id", "", "Azure subscription ID for provider-id.")
	flag.StringVar(&aksVMResourceGroup, "aks-vm-resource-group", "", "Resource group containing the worker VMs.")
	flag.StringVar(&aksAPIServerPrivateIP, "aks-api-server-private-ip", "", "Private IP of AKS API server (via Tailscale mesh). When set, kubelet connects through this IP instead of public FQDN.")

	// Routing configuration flags
	flag.StringVar(&dcRouterTailscaleIP, "dc-router-tailscale-ip", "", "Tailscale IP of the DC router for route updates.")
	flag.StringVar(&aksRouterTailscaleIP, "aks-router-tailscale-ip", "", "Tailscale IP of the AKS router for route updates.")
	flag.StringVar(&azureRouteTableName, "azure-route-table-name", "", "Azure route table name for pod CIDR routes.")
	flag.StringVar(&azureVNetName, "azure-vnet-name", "", "Azure VNet name containing the subnets.")
	flag.StringVar(&azureSubnetName, "azure-subnet-name", "", "Azure subnet name where AKS nodes reside.")

	// Route sync controller flags
	var aksRouterPrivateIP string
	var routerRouteTableName string
	var tailscaleClientID string
	var tailscaleClientSecret string
	flag.BoolVar(&enableRouteSync, "enable-route-sync", false, "Enable the route sync controller to automatically update Azure routes when nodes join.")
	flag.StringVar(&aksNodeResourceGroup, "aks-node-resource-group", "", "Resource group containing AKS managed infrastructure (MC_*). Required for route sync.")
	flag.StringVar(&routerSubnetName, "router-subnet-name", "", "Subnet name where the Tailscale router lives.")
	flag.StringVar(&aksRouterPrivateIP, "aks-router-private-ip", "", "Private IP of the AKS router VM (e.g., 10.237.0.4). Used as next-hop for Azure route tables.")
	flag.StringVar(&routerRouteTableName, "router-route-table-name", "stargate-router-rt", "Route table name for router subnet (return traffic). Created if doesn't exist.")
	flag.StringVar(&dcSubnetCIDR, "dc-subnet-cidr", "10.50.0.0/16", "DC subnet CIDR to route through the router.")
	flag.StringVar(&dcPodCIDR, "dc-pod-cidr", "10.244.50.0/20", "DC pod CIDR range to route through the router.")
	flag.StringVar(&tailscaleAPIKey, "tailscale-api-key", "", "Tailscale API key for route management (or set TAILSCALE_API_KEY env).")
	flag.StringVar(&tailscaleClientID, "tailscale-client-id", "", "Tailscale OAuth client ID (or set TAILSCALE_CLIENT_ID env).")
	flag.StringVar(&tailscaleClientSecret, "tailscale-client-secret", "", "Tailscale OAuth client secret (or set TAILSCALE_CLIENT_SECRET env).")
	flag.StringVar(&tailnetName, "tailnet-name", "", "Tailscale tailnet name (defaults to API key's tailnet).")

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
	if aksAPIServer == "" && controlPlaneMode == "aks" {
		aksAPIServer = restConfig.Host
	}

	// Set up Operation controller
	if err = (&controller.OperationReconciler{
		Client:                  mgr.GetClient(),
		Scheme:                  mgr.GetScheme(),
		KindContainerName:       kindContainerName,
		ControlPlaneTailscaleIP: controlPlaneTailscaleIP,
		ControlPlaneHostname:    controlPlaneHostname,
		ControlPlaneMode:        controlPlaneMode,
		ControlPlaneSSHUser:     controlPlaneSSHUser,
		SSHPrivateKeyPath:       sshPrivateKeyPath,
		SSHPort:                 sshPort,
		AdminUsername:           adminUsername,
		AKSAPIServer:            aksAPIServer,
		AKSClusterName:          aksClusterName,
		AKSResourceGroup:        aksResourceGroup,
		AKSClusterDNS:           aksClusterDNS,
		AKSSubscriptionID:       aksSubscriptionID,
		AKSVMResourceGroup:      aksVMResourceGroup,
		AKSAPIServerPrivateIP:   aksAPIServerPrivateIP,
		DCRouterTailscaleIP:     dcRouterTailscaleIP,
		AKSRouterTailscaleIP:    aksRouterTailscaleIP,
		AzureRouteTableName:     azureRouteTableName,
		AzureVNetName:           azureVNetName,
		AzureSubnetName:         azureSubnetName,
		Clientset:               clientset,
		CACertBase64:            caCertBase64,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Operation")
		os.Exit(1)
	}

	// Set up Route Sync controller (if enabled)
	if enableRouteSync {
		// Use env vars if flags not provided
		tsAPIKey := tailscaleAPIKey
		if tsAPIKey == "" {
			tsAPIKey = os.Getenv("TAILSCALE_API_KEY")
		}
		tsClientID := tailscaleClientID
		if tsClientID == "" {
			tsClientID = os.Getenv("TAILSCALE_CLIENT_ID")
		}
		tsClientSecret := tailscaleClientSecret
		if tsClientSecret == "" {
			tsClientSecret = os.Getenv("TAILSCALE_CLIENT_SECRET")
		}

		if err = (&controller.RouteSyncReconciler{
			Client:                mgr.GetClient(),
			Scheme:                mgr.GetScheme(),
			Logger:                slog.Default(),
			SubscriptionID:        aksSubscriptionID,
			AKSResourceGroup:      aksNodeResourceGroup,
			ClusterResourceGroup:  aksResourceGroup,
			ClusterName:           aksClusterName,
			RouteTableName:        azureRouteTableName,
			RouterRouteTableName:  routerRouteTableName,
			RouterSubnetName:      routerSubnetName,
			AKSSubnetName:         azureSubnetName,
			DCSubnetCIDR:          dcSubnetCIDR,
			DCPodCIDR:             dcPodCIDR,
			DCResourceGroup:       aksVMResourceGroup,
			AKSRouterIP:           aksRouterPrivateIP,
			AKSRouterTSIP:         aksRouterTailscaleIP,
			DCRouterTSIP:          dcRouterTailscaleIP,
			SSHPrivateKeyPath:     sshPrivateKeyPath,
			TailscaleAPIKey:       tsAPIKey,
			TailscaleClientID:     tsClientID,
			TailscaleClientSecret: tsClientSecret,
			TailnetName:           tailnetName,
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "RouteSync")
			os.Exit(1)
		}
		setupLog.Info("Route sync controller enabled",
			"resourceGroup", aksNodeResourceGroup,
			"routeTable", azureRouteTableName,
			"routerRouteTable", routerRouteTableName,
			"aksRouterPrivateIP", aksRouterPrivateIP,
			"aksRouterTSIP", aksRouterTailscaleIP,
			"dcRouterTSIP", dcRouterTailscaleIP,
			"tailscaleOAuth", tsClientID != "")
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
