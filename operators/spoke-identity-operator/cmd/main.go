package main

import (
	"crypto/tls"
	"flag"
	"os"

	_ "k8s.io/client-go/plugin/pkg/client/auth"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	identityv1alpha1 "github.com/kamarkat/kamarkat/operators/spoke-identity-operator/api/v1alpha1"
	"github.com/kamarkat/kamarkat/operators/spoke-identity-operator/internal/controller"
	"github.com/kamarkat/kamarkat/operators/spoke-identity-operator/internal/infisical"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(corev1.AddToScheme(scheme))
	utilruntime.Must(identityv1alpha1.AddToScheme(scheme))
}

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var infisicalURL string
	var orgID string
	var projectID string
	var secretsProjectID string

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false, "Enable leader election for controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", false, "If set the metrics endpoint is served securely")
	flag.BoolVar(&enableHTTP2, "enable-http2", false, "If set, HTTP/2 will be enabled for the metrics and webhook servers")
	flag.StringVar(&infisicalURL, "infisical-url", "http://infisical-standalone-infisical.platform-security.svc:8080", "Infisical API base URL")
	flag.StringVar(&orgID, "infisical-org-id", "", "Infisical organization ID")
	flag.StringVar(&projectID, "infisical-project-id", "", "Infisical project ID")
	flag.StringVar(&secretsProjectID, "infisical-secrets-project-id", "", "Infisical secret-manager project ID (e.g. hub-secrets) granted to spoke identities")

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	disableHTTP2 := func(c *tls.Config) {
		if enableHTTP2 {
			return
		}
		c.NextProtos = []string{"http/1.1"}
	}

	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       []func(*tls.Config){disableHTTP2},
	}

	if secureMetrics {
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	webhookServerOptions := webhook.Options{
		TLSOpts: []func(*tls.Config){disableHTTP2},
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                        scheme,
		Metrics:                       metricsServerOptions,
		WebhookServer:                 webhook.NewServer(webhookServerOptions),
		HealthProbeBindAddress:        probeAddr,
		LeaderElection:                enableLeaderElection,
		LeaderElectionID:              "spoke-identity-operator.zeroops.io",
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	infisicalClient := infisical.NewClient(infisicalURL)

	if err = (&controller.SpokeMachineIdentityReconciler{
		Client:           mgr.GetClient(),
		InfisicalClient:  infisicalClient,
		OrgID:            orgID,
		ProjectID:        projectID,
		SecretsProjectID: secretsProjectID,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "SpokeMachineIdentity")
		os.Exit(1)
	}

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
