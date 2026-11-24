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
	"crypto/tls"
	"flag"
	"os"

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
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	opsv1alpha1 "github.com/kamarkat/kamarkat/operators/hub-operator/api/v1alpha1"
	hubclient "github.com/kamarkat/kamarkat/operators/hub-operator/internal/client"
	"github.com/kamarkat/kamarkat/operators/hub-operator/internal/controller"
	"github.com/kamarkat/kamarkat/operators/hub-operator/internal/openbao"
	"github.com/kamarkat/kamarkat/operators/hub-operator/internal/secrets"
	hubwebhook "github.com/kamarkat/kamarkat/operators/hub-operator/internal/webhook"
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

	// The box's OpenBao (ADR-117). The client reads the listener CA from the
	// mounted openbao-listener-trust ConfigMap and logs in lazily with the
	// projected ServiceAccount token, so a store that is not up yet is waited for
	// by the reconcilers rather than crash-looped on here.
	baoClient, err := openbao.NewFromEnv()
	if err != nil {
		setupLog.Error(err, "operator misconfigured: cannot build the OpenBao client")
		os.Exit(1)
	}
	store := secrets.NewStore(baoClient)

	if err := (&controller.HubEnvironmentReconciler{
		Client:         mgr.GetClient(),
		UncachedClient: uncachedClient,
		Scheme:         mgr.GetScheme(),
		Store:          store,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "HubEnvironment")
		os.Exit(1)
	}

	ctx := ctrl.SetupSignalHandler()

	if err := (&controller.SpokePoolReconciler{
		Client:         mgr.GetClient(),
		UncachedClient: uncachedClient,
		Store:          store,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "SpokePool")
		os.Exit(1)
	}

	// Nil when unset, and the reconciler skips identity provisioning entirely.
	// An environment whose issuer has no notion of a tenant has nothing to
	// provision, so absence is a configuration rather than a fault.
	identityClient := hubclient.NewIdentityClient(os.Getenv("IDENTITY_SERVICE_URL"))

	tenantResources := &controller.AINativeSaaSReconciler{
		Client:         mgr.GetClient(),
		Store:          store,
		IdentityClient: identityClient,
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

	// The box's MCP server identity (ADR-116 §3): its project, roles and the
	// public clients kakt signs people in with, while capabilities.platformMcp is on.
	if err := (&controller.PlatformMcpReconciler{AINativeSaaSReconciler: tenantResources}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "PlatformMcp")
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

	// SpokePool admission: the declared provider resolves to ONE composition.
	//
	// WHAT THIS CLOSES. The defaulter and validator have existed since a56502c4
	// and were never registered here, so the invariant they describe was not in
	// force on any box: box 7 carried the admission implementation, a cert-manager
	// Certificate for a webhook Service that was never created, and no
	// MutatingWebhookConfiguration at all. A webhook source file without a
	// registered endpoint is not an enforced contract, and this is where it is
	// registered.
	//
	// BEFORE THE MANAGER STARTS, with the API reader rather than the manager's
	// client: the cached client cannot serve a read until the cache has started,
	// and the registry has to be in hand before the first admission request. A
	// failure here exits, because every path that follows -- the defaulter's
	// resolution, the validator's refusal, the XRD's CEL rule -- depends on this
	// mapping, and an operator serving admission without it would reject every
	// workload cluster with a message about itself.
	if err := hubwebhook.LoadProviderRegistry(ctx, mgr.GetAPIReader()); err != nil {
		setupLog.Error(err, "Failed to load the provider registry, without which no workload "+
			"cluster can be admitted")
		os.Exit(1)
	}

	// REGISTERED ON mgr.GetWebhookServer() DIRECTLY, not through ctrl.NewWebhookManagedBy.
	//
	// Calling mgr.GetWebhookServer() ensures that controller-runtime's internal
	// webhookServerOnce runs, which calls cm.Add(cm.webhookServer) so that
	// mgr.Start actually starts the webhook server on its listening port.
	//
	// The builder's Complete() ends in registerConversionWebhook, which asks
	// conversion.IsConvertible whether this type has hub/spoke versions. That
	// resolves the object through the SCHEME: objectGVKs keeps only GVKs present
	// in scheme.AllKnownTypes(), and a SpokePool is an unstructured XR whose CRD
	// Crossplane generates -- it is in no Go scheme, so the filter leaves nothing
	// and it returns "error retrieving gvks for object".
	//
	// Registering the handlers is the whole of what was wanted. There is no
	// conversion webhook to install: one served version, generated by Crossplane
	// from the XRD, and nothing converts between versions of it. The paths are
	// written here rather than derived, and config/webhook/manifests.yaml carries
	// the same two from the +kubebuilder:webhook markers -- controller-gen builds
	// them by the same rule (group dots to hyphens, version, lower-case kind).
	srv := mgr.GetWebhookServer()
	srv.Register("/mutate-nutgraf-in-v1alpha1-spokepool",
		admission.WithDefaulter(mgr.GetScheme(), &hubwebhook.SpokePoolDefaulter{}))
	// The API reader again, and for the same reason it is used above: the
	// validator reads the Composition the registry names, which is read a handful
	// of times in a box's life and does not earn an informer.
	srv.Register("/validate-nutgraf-in-v1alpha1-spokepool",
		admission.WithValidator(mgr.GetScheme(), hubwebhook.NewSpokePoolValidator(mgr.GetAPIReader())))
	setupLog.Info("SpokePool admission registered",
		"mutate", "/mutate-nutgraf-in-v1alpha1-spokepool",
		"validate", "/validate-nutgraf-in-v1alpha1-spokepool")

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
