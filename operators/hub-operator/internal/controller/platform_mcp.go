package controller

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	hubclient "github.com/kamarkat/kamarkat/operators/hub-operator/internal/client"
)

// The identity of the box's MCP server (ADR-116 §3): a dedicated project whose
// roles decide what a person may read through it, and the PUBLIC clients kakt
// signs people in with.
//
// Dedicated for the reason the observability project is: projectRoleCheck makes
// a role mean something only in a project that exists for one purpose, and the
// project id is the audience kamarkat-mcp accepts.
//
// PUBLIC clients, because a confidential client's secret is minted here and
// would have to be carried to wherever kakt runs -- a box secret leaving the box
// (ADR-065). The ids published below are not credentials.
const (
	platformMcpProjectName = "kamarkat-mcp"

	// Rendered by the capability (capabilities.platformMcp). Its presence IS the
	// enablement, as the observability viewer's marker is; its data is the
	// tenant's declaration of which kakt deployments may receive a code.
	platformMcpMarkerName      = "kamarkat-mcp"
	platformMcpMarkerNamespace = "platform-mcp"

	platformMcpProjectIDKey  = "hub-kamarkat-mcp-project-id"
	platformMcpOrgIDKey      = "hub-kamarkat-mcp-org-id"
	platformMcpKaktClientKey = "hub-kamarkat-mcp-kakt-client-id"
	platformMcpCLIClientKey  = "hub-kamarkat-mcp-kakt-cli-client-id"
	platformMcpBootstrapKey  = "hub-kamarkat-mcp-bootstrapped"
	platformMcpAgentUserKey  = "hub-kamarkat-mcp-agent-user-id"
	platformMcpAgentKeyIDKey = "hub-kamarkat-mcp-agent-key-id"
	// "<sha256 of the declared public key>:<key id>", so an unchanged key is not
	// registered again and a replaced one is revoked.
	platformMcpAgentKeyRecord = "hub-kamarkat-mcp-agent-key-record"
	platformMcpAgentName      = "kakt-agent"
	platformMcpKaktClientName = "kakt"
	platformMcpCLIClientName  = "kakt-cli"
)

// The roles of the project. Owners of the platform organisation are granted
// all three once; anyone else is granted in the issuer by one of them.
var platformMcpRoles = []hubclient.PlatformRole{
	{Key: "platform.viewer", Display: "Platform Viewer (read the box through MCP)"},
	{Key: "platform.observer", Display: "Platform Observer (logs and metrics through MCP)"},
	{Key: "platform.verifier", Display: "Platform Verifier (run acceptance through MCP)"},
}

// PlatformMcpReconciler provisions the MCP server's project and clients.
type PlatformMcpReconciler struct {
	*AINativeSaaSReconciler
}

// Reconcile provisions the identity while the capability is on, and does
// nothing when it is off. Nothing is revoked when the marker goes: as for the
// observability project, deleting the project would mint a new id on the next
// enablement and invalidate every connected person silently. Removal is an
// explicit act in the issuer.
func (r *PlatformMcpReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	marker := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{Name: platformMcpMarkerName, Namespace: platformMcpMarkerNamespace}, marker)
	switch {
	case apierrors.IsNotFound(err):
		return ctrl.Result{}, nil
	case err != nil:
		return ctrl.Result{}, err
	}
	if r.IdentityClient == nil {
		logger.Error(nil, "The MCP capability is enabled but this environment provisions no identity; its server admits nobody")
		return ctrl.Result{}, nil
	}

	orgID, projectID, err := r.IdentityClient.EnsurePlatformProject(ctx, platformMcpProjectName)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("provision the kamarkat-mcp project: %w", err)
	}
	if err := r.publishHubSecret(ctx, platformMcpProjectIDKey, projectID); err != nil {
		return ctrl.Result{}, fmt.Errorf("publish the kamarkat-mcp project id: %w", err)
	}
	if err := r.publishHubSecret(ctx, platformMcpOrgIDKey, orgID); err != nil {
		return ctrl.Result{}, fmt.Errorf("publish the kamarkat-mcp organisation id: %w", err)
	}

	// Once per project, recorded by project id, for the reason the observability
	// viewer gives: a grant restored on every reconcile would undo a revocation.
	bootstrapped, err := r.InfisicalClient.GetSecret(ctx, hubSecretPath, platformMcpBootstrapKey)
	if err != nil || bootstrapped != projectID {
		if err := r.IdentityClient.EnsurePlatformProjectRoles(ctx, orgID, projectID, platformMcpRoles); err != nil {
			return ctrl.Result{}, fmt.Errorf("admit the platform organisation's owners to the kamarkat-mcp project: %w", err)
		}
		if err := r.publishHubSecret(ctx, platformMcpBootstrapKey, projectID); err != nil {
			return ctrl.Result{}, fmt.Errorf("record the kamarkat-mcp bootstrap: %w", err)
		}
		logger.Info("The platform organisation's owners were granted the kamarkat-mcp roles", "projectId", projectID)
	}

	// kakt: only the deployments the tenant declared may receive a code. No
	// declared URI means no kakt client -- `kakt mcp` and other local clients
	// still work through kakt-cli.
	if uris := lines(marker.Data["kaktRedirectUris"]); len(uris) > 0 {
		id, err := r.IdentityClient.EnsurePlatformPublicClient(ctx, orgID, projectID, platformMcpKaktClientName, uris, false)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("provision the kakt client: %w", err)
		}
		if err := r.publishHubSecret(ctx, platformMcpKaktClientKey, id); err != nil {
			return ctrl.Result{}, fmt.Errorf("publish the kakt client id: %w", err)
		}
	}

	cli := lines(marker.Data["cliRedirectUris"])
	if len(cli) == 0 {
		cli = []string{"http://127.0.0.1:8765/callback", "http://localhost:8765/callback"}
	}
	cliID, err := r.IdentityClient.EnsurePlatformPublicClient(ctx, orgID, projectID, platformMcpCLIClientName, cli, true)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("provision the kakt-cli client: %w", err)
	}
	if err := r.publishHubSecret(ctx, platformMcpCLIClientKey, cliID); err != nil {
		return ctrl.Result{}, fmt.Errorf("publish the kakt-cli client id: %w", err)
	}

	// kakt-agent: agents that run with no person present (a schedule, an alert)
	// read the box as this machine user, platform.viewer only. The tenant
	// declares kakt's PUBLIC key; the private half never exists here. Removing
	// the declaration stops kamarkat-mcp accepting the agent (it reads the same
	// marker), whatever tokens are outstanding.
	if pem := decodePublicKey(marker.Data["agentPublicKey"]); pem != "" {
		sum := sha256.Sum256([]byte(pem))
		hash := hex.EncodeToString(sum[:])
		var known, revoke string
		if prev, err := r.InfisicalClient.GetSecret(ctx, hubSecretPath, platformMcpAgentKeyRecord); err == nil {
			if h, id, ok := strings.Cut(prev, ":"); ok {
				if h == hash {
					known = id
				} else {
					revoke = id
				}
			}
		}
		userID, keyID, err := r.IdentityClient.EnsurePlatformMachineKeyUser(ctx, orgID, projectID,
			platformMcpAgentName, pem, known, revoke, []string{"platform.viewer"})
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("provision the kakt-agent identity: %w", err)
		}
		for key, value := range map[string]string{
			platformMcpAgentUserKey:   userID,
			platformMcpAgentKeyIDKey:  keyID,
			platformMcpAgentKeyRecord: hash + ":" + keyID,
		} {
			if err := r.publishHubSecret(ctx, key, value); err != nil {
				return ctrl.Result{}, fmt.Errorf("publish %s: %w", key, err)
			}
		}
	}

	logger.Info("The box's MCP identity is provisioned", "projectId", projectID, "kaktRedirectUris", len(lines(marker.Data["kaktRedirectUris"])))
	return ctrl.Result{}, nil
}

// decodePublicKey reads the declared key: base64 of a PEM as the capability
// renders it (a templated value must be one line), or a PEM as written by hand.
func decodePublicKey(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || strings.Contains(v, "BEGIN") {
		return v
	}
	raw, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// lines splits a declaration of one value per line (or comma), dropping blanks.
func lines(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == ',' }) {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// SetupWithManager watches the marker the capability renders, and nothing else.
func (r *PlatformMcpReconciler) SetupWithManager(mgr ctrl.Manager) error {
	isMarker := predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetName() == platformMcpMarkerName && o.GetNamespace() == platformMcpMarkerNamespace
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.ConfigMap{}, builder.WithPredicates(isMarker)).
		Named("platform-mcp").
		Complete(r)
}
