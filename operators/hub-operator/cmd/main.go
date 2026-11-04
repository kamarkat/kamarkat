/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/go-logr/logr"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	cnpgv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	opsv1alpha1 "github.com/kamarkat/kamarkat/operators/hub-operator/api/v1alpha1"
	hubclient "github.com/kamarkat/kamarkat/operators/hub-operator/internal/client"
	"github.com/kamarkat/kamarkat/operators/hub-operator/internal/controller"
	"github.com/kamarkat/kamarkat/operators/hub-operator/internal/secrets"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(opsv1alpha1.AddToScheme(scheme))

	// Register CNPG API for watching Cluster resources
	utilruntime.Must(cnpgv1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// nolint:gocyclo
// waitForInfisicalConfig blocks until INFISICAL_PROJECT_ID is present.
//
// The value arrives from the hub-bootstrap-config ConfigMap, which the ADR-045
// bootstrap phase fills two phases after this operator is reconciled. Env vars
// sourced from a ConfigMap do not update in a running container, so the restart
// that picks the value up is performed by the reloader watching that ConfigMap;
// this wait exists so the pod stays alive and Ready-less until then instead of
// crash-looping, which is what exiting produced.
func waitForInfisicalConfig(ctx context.Context, log logr.Logger) {
	const every = 15 * time.Second
	for attempt := 1; ; attempt++ {
		select {
		case <-ctx.Done():
			log.Info("Shutting down while waiting for INFISICAL_PROJECT_ID")
			os.Exit(0)
		case <-time.After(every):
		}
		if v := os.Getenv("INFISICAL_PROJECT_ID"); v != "" && v != "PLACEHOLDER_PROJECT_ID" {
			log.Info("INFISICAL_PROJECT_ID now present", "attempts", attempt)
			return
		}
		if attempt%20 == 0 {
			log.Info("Still waiting for INFISICAL_PROJECT_ID from hub-bootstrap-config",
				"waited", time.Duration(attempt)*every)
		}
	}
}

func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var tlsOpts []func(*tls.Config)
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("Disabling HTTP/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts
	webhookServerOptions := webhook.Options{
		TLSOpts: webhookTLSOpts,
	}

	if len(webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		webhookServerOptions.CertDir = webhookCertPath
		webhookServerOptions.CertName = webhookCertName
		webhookServerOptions.KeyName = webhookCertKey
	}

	webhookServer := webhook.NewServer(webhookServerOptions)

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.23.3/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.23.3/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	//
	// TODO(user): If you enable certManager, uncomment the following lines:
	// - [METRICS-WITH-CERTS] at config/default/kustomization.yaml to generate and use certificates
	// managed by cert-manager for the metrics server.
	// - [PROMETHEUS-WITH-CERTS] at config/prometheus/kustomization.yaml for TLS certification.
	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		metricsServerOptions.CertDir = metricsCertPath
		metricsServerOptions.CertName = metricsCertName
		metricsServerOptions.KeyName = metricsCertKey
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "6b46dbfb.nutgraf.in",
		// Task 13 / Requirement 19: Strip Secret .data payloads from cache to reduce memory ~90%
		// Secrets managed by zero-ops-hub-cli (label: app.kubernetes.io/managed-by=zero-ops-hub-cli)
		// are bypassed via UncachedClient. Operational secrets also use UncachedClient.
		Cache: cache.Options{
			ByObject: map[client.Object]cache.ByObject{
				&corev1.Secret{}: {
					// AC 19.4: StripDataFromSecretOrConfigMapTransform - strips .data payload
					// preserving metadata for watch/list operations
					Transform: func(obj interface{}) (interface{}, error) {
						secret, ok := obj.(*corev1.Secret)
						if !ok {
							return obj, nil
						}
						// AC 19.3: Bypass strip for secrets managed by zero-ops-hub-cli
						if labels := secret.GetLabels(); labels != nil {
							if labels["app.kubernetes.io/managed-by"] == "zero-ops-hub-cli" {
								return obj, nil
							}
						}
						// Strip .data payload to reduce memory usage
						secret.Data = nil
						secret.StringData = nil
						return secret, nil
					},
				},
			},
		},
	})
	if err != nil {
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}

	// Task 13: Create uncached client for operational secrets
	// Reads directly from API server, bypassing the cache transformer
	uncachedClient, err := client.New(mgr.GetConfig(), client.Options{Scheme: mgr.GetScheme()})
	if err != nil {
		setupLog.Error(err, "Failed to create uncached client")
		os.Exit(1)
	}

	if err := (&controller.HubEnvironmentReconciler{
		Client:         mgr.GetClient(),
		UncachedClient: uncachedClient,
		Scheme:         mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "HubEnvironment")
		os.Exit(1)
	}

	// Established here rather than below because the Infisical configuration may
	// have to be waited for, and a wait that ignores SIGTERM outlives a delete.
	ctx := ctrl.SetupSignalHandler()

	// Initialize the Infisical Client for ADR-031 topology management
	// These env vars are injected by your hub-operator Deployment manifest
	projectID := os.Getenv("INFISICAL_PROJECT_ID")
	secretsProjectID := os.Getenv("INFISICAL_SECRETS_PROJECT_ID")
	orgID := os.Getenv("INFISICAL_ORGANIZATION_ID")
	clientID := os.Getenv("INFISICAL_CLIENT_ID")
	clientSecret := os.Getenv("INFISICAL_CLIENT_SECRET")
	baseURL := os.Getenv("INFISICAL_BASE_URL")

	// An absent project id and a wrong one are different states, and only the
	// second is misconfiguration.
	//
	// This operator is reconciled in boundary 03, and Infisical is bootstrapped
	// two phases later: on a cluster being built for the first time there is a
	// window in which the id genuinely does not exist yet. Exiting through it
	// turns that window into a crash loop.
	//
	// Until now the window was hidden rather than handled. The ADR-045 patch that
	// supplies the id is committed to the repository, so a rebuilt cluster
	// reconciled the PREVIOUS cluster's project until Day-0 overwrote it: the
	// operator started against a live-looking id that pointed at someone else's
	// project, which is worse than starting against none.
	//
	// So: a placeholder is still refused, because that is a manifest that was
	// never rendered. An empty value waits.
	if projectID == "PLACEHOLDER_PROJECT_ID" {
		setupLog.Error(fmt.Errorf("project id is the unrendered placeholder"),
			"operator misconfigured: INFISICAL_PROJECT_ID was never substituted")
		os.Exit(1)
	}
	if projectID == "" {
		setupLog.Info("INFISICAL_PROJECT_ID is not set yet; Infisical-backed controllers stay idle " +
			"until the bootstrap config carries it. This is expected before the Infisical " +
			"bootstrap phase and is not an error.")
		waitForInfisicalConfig(ctx, setupLog)
		projectID = os.Getenv("INFISICAL_PROJECT_ID")
		secretsProjectID = os.Getenv("INFISICAL_SECRETS_PROJECT_ID")
		orgID = os.Getenv("INFISICAL_ORGANIZATION_ID")
	}
	if secretsProjectID == "" {
		setupLog.Info("INFISICAL_SECRETS_PROJECT_ID is missing, falling back to INFISICAL_PROJECT_ID for backward compatibility")
		secretsProjectID = projectID
	}

	setupLog.Info("Loaded Infisical configuration", "projectID", projectID)

	if clientSecret == "" {
		setupLog.Error(fmt.Errorf("missing client secret"), "operator misconfigured: INFISICAL_CLIENT_SECRET is missing")
		os.Exit(1)
	}

	pkiInfisicalClient := secrets.NewInfisicalClient(
		baseURL,
		clientID,
		clientSecret,
		projectID,
		orgID,
	)
	// The management scope: the environment the HUB's own secrets live in.
	//
	// Assigned unconditionally. The guard here was `if envSlug != ""`, which left
	// the client on its old built-in "dev" whenever hub-bootstrap-config had not
	// been patched -- so a stg hub wrote its secrets into dev and read them from
	// stg. There is no default now, and an empty value fails on the first call
	// naming the key rather than succeeding against the wrong environment.
	//
	// Not fatal at startup: this operator also reconciles paths that need no
	// Infisical at all (the hybrid home-worker join runs before the Infisical
	// gate for exactly that reason), and INFISICAL_PROJECT_ID above already
	// establishes that Infisical configuration can arrive after the operator does.
	mgmtEnvironmentSlug := os.Getenv("INFISICAL_ENVIRONMENT_SLUG")
	if mgmtEnvironmentSlug == "" {
		setupLog.Error(fmt.Errorf("INFISICAL_ENVIRONMENT_SLUG is empty"),
			"the hub's own secrets have no environment; every Infisical write will be "+
				"refused until hub-bootstrap-config carries it (patched by "+
				"manifests/environments/<slug>/patch-config.yaml)")
	}
	pkiInfisicalClient.EnvironmentSlug = mgmtEnvironmentSlug

	secretsInfisicalClient := secrets.NewInfisicalClient(
		baseURL,
		clientID,
		clientSecret,
		secretsProjectID,
		orgID,
	)
	secretsInfisicalClient.EnvironmentSlug = mgmtEnvironmentSlug

	if err := (&controller.SpokePoolReconciler{
		Client:          mgr.GetClient(),
		UncachedClient:  uncachedClient,
		InfisicalClient: secretsInfisicalClient,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "SpokePool")
		os.Exit(1)
	}

	// Nil when unset, and the reconciler skips identity provisioning entirely.
	// An environment whose issuer has no notion of a tenant has nothing to
	// provision, so absence is a configuration rather than a fault.
	identityClient := hubclient.NewIdentityClient(os.Getenv("IDENTITY_SERVICE_URL"))

	tenantResources := &controller.AINativeSaaSReconciler{
		Client:          mgr.GetClient(),
		InfisicalClient: secretsInfisicalClient,
		IdentityClient:  identityClient,
	}
	if err := tenantResources.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "AINativeSaaS")
		os.Exit(1)
	}
	// The box's platform capabilities with one caller -- the Credit Authority
	// (ADR-109) and the billing engine (ADR-113): each one's identity while
	// enabled and its one caller, with the same clients as the applications
	// they serve.
	for _, capability := range controller.PlatformCapabilityReconcilers(tenantResources) {
		if err := capability.SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "Failed to create controller", "controller", capability.Name())
			os.Exit(1)
		}
	}

	// The box's observability viewer identity (ADR-078 add.5 §7): one project for
	// the box and the Grafana client that signs viewers in, provisioned while the
	// capability is enabled.
	if err := (&controller.ObservabilityViewerReconciler{AINativeSaaSReconciler: tenantResources}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "ObservabilityViewer")
		os.Exit(1)
	}

	// The UserGroups controller was removed with ADR-059.
	//
	// It reconciled an identity-user-groups ConfigMap into Kratos
	// metadata_public.groups so the auth-proxy could inject a groups claim. Every
	// component in that chain is gone, and the mechanism was ours rather than the
	// provider's: Zitadel models authorisation as project roles held in an
	// Organization, and an Organization owns the user, so there is no tenant-less
	// identity for a platform_admins group to describe.
	//
	// It also made KRATOS_ADMIN_URL a hard startup requirement, so the operator
	// could not run at all without a Kratos to point at.

	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("Starting manager")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "Failed to run manager")
		os.Exit(1)
	}
}
