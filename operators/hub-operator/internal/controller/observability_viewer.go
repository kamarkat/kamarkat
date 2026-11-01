package controller

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// The viewer identity for the box's observability query path (ADR-078 add.5 §7).
//
// ONE project for the box, not one per spoke. Every spoke's VMUser matches the
// same audience, because the thing being authorised is a person's access to this
// box's telemetry rather than to one cluster of it. That is what lets the project
// id live once in the box's Git registry and be rendered into every spoke
// (approved 2026-10-31); a per-spoke project would put a different id in each
// spoke's configuration and give the mismatch nowhere to be noticed.
//
// A DEDICATED project, not the shared platform one, because `projectRoleCheck`
// is what makes a role mean something: the issuer refuses a token to a user with
// no role in the project. In the shared project that would admit any platform
// user. In this one it admits only those granted an observability role — and
// VMAuth then matches the viewer role specifically, because projectRoleCheck
// alone admits `member` too.
//
// The client is CONFIDENTIAL. Grafana runs the browser flow from a server it
// controls, so a public client buys nothing its OIDC library does not expect.
const (
	observabilityProjectName = "observability"
	observabilityAppName     = "grafana"

	// Rendered only while the capability is enabled: the whole grafana component
	// sits behind capabilities.observability.enabled, so the marker's presence IS
	// the enablement, the same way the Credit Authority's ConfigMap is.
	//
	// It carries the box's Grafana URL because the redirect URI is a fact about
	// the box, and templated-fields already substitutes the domain at render
	// time. Reading it here means this controller needs no HubEnvironment and
	// cannot disagree with what the component actually published.
	observabilityViewerMarkerName = "observability-viewer"
	observabilityViewerNamespace  = "platform-observability"

	// Hub keys, beside the platform services' own.
	observabilityProjectIDKey  = "hub-observability-project-id"
	grafanaOIDCClientIDKey     = "hub-grafana-oidc-client-id"
	grafanaOIDCClientSecretKey = "hub-grafana-oidc-client-secret"
	// The role the box's operators hold in the observability project.
	//
	// NOT what authorizes a query -- the spoke's VMAuth matches the project
	// `aud` alone, because the project is dedicated and a token for it is
	// already a token belonging to someone granted access. This role exists
	// because the ISSUER requires one: every project this platform creates
	// carries projectRoleCheck, so without a grant every sign-in is refused
	// with Errors.User.GrantRequired, before Grafana sees anything.
	//
	// One role, deliberately. A second one -- an editor beside a viewer -- is
	// the first thing that would make the role claim worth matching, and
	// dashboards come from Git, so there is nothing to edit yet.
	observabilityViewerRole        = "viewer"
	observabilityViewerRoleDisplay = "Observability Viewer"
)

// ObservabilityViewerReconciler provisions the box's observability project and
// the Grafana client that signs viewers in.
type ObservabilityViewerReconciler struct {
	*AINativeSaaSReconciler
}

//+kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch

// Reconcile provisions the identity when the capability is on, and does nothing
// when it is off.
//
// Nothing is revoked when the marker disappears. The project and the client
// outlive a capability being switched off: deleting them would invalidate every
// viewer's sign-in and, on a box that turns observability back on, mint a NEW
// project id — which the box's registry would then disagree with, silently, until
// someone noticed queries failing. Removal is an explicit act, as it is for every
// other platform identity.
func (r *ObservabilityViewerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	marker := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      observabilityViewerMarkerName,
		Namespace: observabilityViewerNamespace,
	}, marker)
	switch {
	case apierrors.IsNotFound(err):
		return ctrl.Result{}, nil
	case err != nil:
		return ctrl.Result{}, err
	}

	if r.IdentityClient == nil {
		logger.Error(nil, "Observability is enabled but this environment provisions no identity; its query endpoint admits nobody")
		return ctrl.Result{}, nil
	}

	grafanaURL := strings.TrimSpace(marker.Data["grafanaUrl"])
	if grafanaURL == "" {
		return ctrl.Result{}, fmt.Errorf(
			"the %s marker carries no grafanaUrl; the client's redirect URI is a fact about this box and cannot be guessed",
			observabilityViewerMarkerName)
	}

	orgID, projectID, err := r.IdentityClient.EnsurePlatformProject(ctx, observabilityProjectName)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("provision the observability project: %w", err)
	}
	if err := r.publishHubSecret(ctx, observabilityProjectIDKey, projectID); err != nil {
		return ctrl.Result{}, fmt.Errorf("publish the observability project id: %w", err)
	}

	// Who may be issued a token for the client below. The project refuses one to
	// a user holding no role in it, so this is not a convenience: without it the
	// client is registered, its credentials are published, Grafana is configured
	// and every sign-in is refused at the issuer.
	//
	// Reconciled rather than event-driven, because there is no signup hook to
	// hang it on -- so an operator added between two passes is refused until the
	// next one, the same bounded delay a tenant's members have (ADR-060).
	if err := r.IdentityClient.EnsurePlatformProjectAccess(
		ctx, orgID, projectID, observabilityViewerRole, observabilityViewerRoleDisplay); err != nil {
		return ctrl.Result{}, fmt.Errorf("admit the platform's operators to the observability project: %w", err)
	}

	// Grafana's generic OAuth callback. One URI: the box has one Grafana, and a
	// redirect list is an allowlist -- an extra entry is an extra place a code
	// may be delivered.
	redirect := strings.TrimSuffix(grafanaURL, "/") + "/login/generic_oauth"
	clientID, clientSecret, err := r.IdentityClient.EnsurePlatformConfidentialClient(
		ctx, orgID, projectID, observabilityAppName, []string{redirect})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("provision the Grafana viewer client: %w", err)
	}
	if err := r.publishHubSecret(ctx, grafanaOIDCClientIDKey, clientID); err != nil {
		return ctrl.Result{}, fmt.Errorf("publish the Grafana client id: %w", err)
	}
	// Empty means the client already existed and the issuer did not disclose its
	// secret again. Publishing an empty value would delete the credential Grafana
	// is holding, so an absent secret is left exactly as it is.
	if clientSecret != "" {
		if err := r.publishHubSecret(ctx, grafanaOIDCClientSecretKey, clientSecret); err != nil {
			return ctrl.Result{}, fmt.Errorf("publish the Grafana client secret: %w", err)
		}
	}

	logger.Info("The box's observability viewer identity is provisioned",
		"project", observabilityProjectName, "projectId", projectID,
		"client", observabilityAppName, "clientId", clientID,
		"secretMinted", clientSecret != "")
	return ctrl.Result{}, nil
}

// SetupWithManager watches the marker the capability renders.
func (r *ObservabilityViewerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.ConfigMap{}).
		Named("observability-viewer").
		Complete(r)
}
