// Package main implements the Azure controller for route synchronization.
// This controller watches CiliumNodes and synchronizes Azure route tables
// to enable pod-to-pod connectivity between AKS and DC workers.
package main

import (
	"flag"
	"log/slog"
	"os"
	"path/filepath"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/vpatelsj/stargate/controller"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
}

func main() {
	var metricsAddr string
	var probeAddr string

	// AKS configuration flags
	var aksClusterName string
	var aksResourceGroup string
	var aksSubscriptionID string
	var aksVMResourceGroup string
	var sshPrivateKeyPath string

	// Routing configuration flags
	var dcRouterTailscaleIP string
	var aksRouterTailscaleIP string
	var azureRouteTableName string
	var azureSubnetName string

	// Route sync configuration flags
	var aksNodeResourceGroup string
	var routerSubnetName string
	var dcSubnetCIDR string
	var dcPodCIDR string
	var tailscaleAPIKey string
	var tailnetName string
	var aksRouterPrivateIP string
	var routerRouteTableName string
	var tailscaleClientID string
	var tailscaleClientSecret string

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8081", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8082", "The address the health probe binds to.")

	// AKS configuration flags
	flag.StringVar(&aksClusterName, "aks-cluster-name", "", "AKS cluster name (used for node labels).")
	flag.StringVar(&aksResourceGroup, "aks-resource-group", "", "AKS cluster resource group (used for node labels).")
	flag.StringVar(&aksSubscriptionID, "aks-subscription-id", "", "Azure subscription ID for provider-id.")
	flag.StringVar(&aksVMResourceGroup, "aks-vm-resource-group", "", "Resource group containing the worker VMs.")
	flag.StringVar(&sshPrivateKeyPath, "ssh-private-key", filepath.Join(os.Getenv("HOME"), ".ssh", "id_rsa"), "Path to SSH private key.")

	// Routing configuration flags
	flag.StringVar(&dcRouterTailscaleIP, "dc-router-tailscale-ip", "", "Tailscale IP of the DC router for route updates.")
	flag.StringVar(&aksRouterTailscaleIP, "aks-router-tailscale-ip", "", "Tailscale IP of the AKS router for route updates.")
	flag.StringVar(&azureRouteTableName, "azure-route-table-name", "", "Azure route table name for pod CIDR routes.")
	flag.StringVar(&azureSubnetName, "azure-subnet-name", "", "Azure subnet name where AKS nodes reside.")

	// Route sync controller flags
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

	// Set up Route Sync controller
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
