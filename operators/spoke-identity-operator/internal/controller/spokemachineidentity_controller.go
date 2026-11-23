package controller

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	logr "github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/yaml"

	identityv1alpha1 "github.com/kamarkat/kamarkat/operators/spoke-identity-operator/api/v1alpha1"
	"github.com/kamarkat/kamarkat/operators/spoke-identity-operator/internal/infisical"
)

// clusterIssuerURL is the Infisical this box's spokes get their certificates
// signed by: its own, at infisical.<the box's domain>.
//
// It was the compile-time constant "https://infisical.dev.nutgraf.in" -- the
// PLATFORM's. So every spoke on every box was issued a ClusterIssuer pointing at
// an Infisical it has no identity in and, for a tenant, cannot even reach. The
// issuer reports "healthcheck failed: ... EOF" and every Certificate naming it
// stays pending forever: argocd-agent-client-cert, alloy-client-cert and
// support-agent-client-cert among them, which surface as three unrelated
// "certificate not Ready" warnings with nothing pointing here.
//
// SECRET_STORE_URL is supplied to the operator's Deployment from the box's own
// HubEnvironment domain (ADR-051, the sole authority). No default: a wrong
// Infisical is not a degraded mode, it is another tenant's secret store.
func clusterIssuerURLFor() (string, error) {
	if v := strings.TrimSpace(os.Getenv("SECRET_STORE_URL")); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("SECRET_STORE_URL is not set, so the spoke's ClusterIssuer has no signer. " +
		"It is this box's own Infisical (infisical.<the box's domain>); there is no default, " +
		"because the one that existed pointed every box at the platform's")
}

const (
	smiFinalizer                  = "identity.kamarkat.io/finalizer"
	conditionTypeReady            = "Ready"
	conditionTypeIdentityCreated  = "IdentityProvisioned"
	conditionTypeRotationComplete = "RotationComplete"
	// conditionTypeAuthorizationInventoried reports whether this identity's
	// Infisical memberships were READ -- not whether they are correct. There is no
	// intended policy to compare them against until project-per-cluster is
	// reconciled, and a role name is not an access decision: only attempting a
	// permitted and a forbidden operation establishes effective authorization.
	// True here means observed. It never means compliant or verified.
	conditionTypeAuthorizationInventoried = "AuthorizationInventoried"
	// conditionTypeIssuerConfigured reports the ADR-048 second-stage ClusterIssuer
	// CRS wrapper delivery. The wrapper is only meaningful when both
	// clientId (rotation-stable machine identity) and projectId (fleet-constant)
	// are populated — a wrapper with empty identifiers must never be published.
	conditionTypeIssuerConfigured = "ClusterIssuerConfigured"
	// identityHealthCheckInterval caps the reconcile cadence (rotationCheckInterval
	// can be weeks when NextRotation is far out) so the Phase 2 lockout self-heal
	// always probes within a bounded window, independent of the rotation schedule.
	identityHealthCheckInterval = 5 * time.Minute
)

const (
	// clusterIssuerTemplate is the Infisical PKI certificate template backed by the
	// Fleet Intermediate CA chain (Offline Root -> Fleet Intermediate -> Leaf).
	// The certificate profile the fleet issues with. Named `profile` by
	// pki-issuer v1.x; it was `certificateTemplateName` at v0.2.0, and the
	// rename is why the value survived a schema change that the field did not.
	clusterIssuerProfile = "infrastructure-services"
)

// +kubebuilder:rbac:groups=identity.kamarkat.io,resources=spokemachineidentities,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=identity.kamarkat.io,resources=spokemachineidentities/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=identity.kamarkat.io,resources=spokemachineidentities/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch;delete

// SpokeMachineIdentityReconciler reconciles SpokeMachineIdentity CRs against Infisical.
// This is the initial implementation of the Platform Identity Domain.
// It creates Machine Identities in Infisical for Spoke clusters, tracks lifecycle state,
// and supports rotation and revocation.
//
// PKI operations are forbidden — only cert-manager may manage certificates per ADR-035.
//
// Future evolution: may add full identity lifecycle (attestation, audit, multi-provider)
// as defined in a future ADR.
type SpokeMachineIdentityReconciler struct {
	client.Client
	InfisicalClient *infisical.Client
	OrgID           string
	ProjectID       string
	// SecretsProjectID is the secret-manager project (e.g. hub-secrets) that
	// spoke machine identities are granted access to in addition to ProjectID
	// (the cert-manager project). This is required so the spoke's ESO
	// ClusterSecretStore can read tenant secrets.
	SecretsProjectID string
}

func (r *SpokeMachineIdentityReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("spokemachineidentity", req.Name)

	smi := &identityv1alpha1.SpokeMachineIdentity{}
	if err := r.Get(ctx, req.NamespacedName, smi); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	orgID := r.OrgID
	if orgID == "" {
		orgID = smi.Spec.Infisical.OrganizationID
	}
	projectID := r.ProjectID
	if projectID == "" {
		projectID = smi.Spec.Infisical.ProjectID
	}

	// Handle deletion
	if !smi.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, smi)
	}

	// Ensure finalizer
	if !controllerutil.ContainsFinalizer(smi, smiFinalizer) {
		controllerutil.AddFinalizer(smi, smiFinalizer)
		if err := r.Update(ctx, smi); err != nil {
			return ctrl.Result{}, fmt.Errorf("add finalizer: %w", err)
		}
		return ctrl.Result{}, nil
	}

	// Ensure Machine Identity exists in Infisical
	identityName := fmt.Sprintf("spoke-%s", smi.Spec.SpokeRef.Name)

	identity, err := r.InfisicalClient.GetOrCreateMachineIdentity(ctx, identityName, orgID)
	if err != nil {
		logger.Error(err, "Failed to ensure Machine Identity")
		r.setCondition(smi, conditionTypeReady, metav1.ConditionFalse, "IdentityFailed", err.Error())
		r.setCondition(smi, conditionTypeIdentityCreated, metav1.ConditionFalse, "IdentityFailed", err.Error())
		_ = r.Status().Update(ctx, smi)
		return ctrl.Result{}, err
	}

	// An identity that is new to us has no usable credential yet, and the only
	// writer of the auth Secret is handleRotation — which returns early while
	// NextRotation is in the future. Left alone, a replaced identity therefore
	// waits out the remainder of a 60-day rotation interval before anything mints
	// a client secret for it, while the wrapper keeps serving the previous
	// generation's credential.
	//
	// That is not hypothetical: reinitialising Infisical destroyed every machine
	// identity, this controller recreated the spoke's, and the wrapper still held
	// an 08-27 credential for an identity that no longer existed. The spoke's
	// ClusterSecretStore reported InvalidProviderConfig and fourteen
	// ExternalSecrets could not resolve — while status read
	// IdentityProvisioned=True, because creating the identity had genuinely
	// succeeded.
	//
	// Clearing the rotation clock makes the next handleRotation call mint and
	// publish immediately. ClientSecretIDs is cleared with it so the rotation does
	// not try to revoke a secret ID belonging to the identity that was replaced.
	identityReplaced := identity.ID != "" && smi.Status.IdentityID != identity.ID
	credentialMissing := identity.ID != "" && len(smi.Status.ClientSecretIDs) == 0

	if identityReplaced {
		smi.Status.IdentityID = identity.ID
		smi.Status.ClientID = identity.ClientID
	}
	if identityReplaced || credentialMissing {
		if identityReplaced {
			logger.Info("Machine Identity was replaced; forcing credential rotation so the wrapper stops serving the previous identity's secret",
				"identityID", identity.ID)
		} else {
			logger.Info("Machine Identity has no recorded client secret; forcing credential rotation",
				"identityID", identity.ID)
		}
		smi.Status.ClientSecretIDs = nil
		smi.Status.NextRotation = nil
	}

	secretsProjectID := r.SecretsProjectID
	if smi.Spec.Infisical.SecretsProjectID != "" {
		secretsProjectID = smi.Spec.Infisical.SecretsProjectID
	}

	// WHAT THIS IDENTITY IS AUTHORIZED TO DO, READ BEFORE ANYTHING WRITES IT.
	//
	// Ordering is the whole point. The grant below asks for an "admin" membership
	// on both projects, so an inventory taken after it would observe this
	// reconcile's own write and report it as the identity's existing state --
	// measuring the operator instead of the box.
	//
	// WHAT IT DOES AND DOES NOT ESTABLISH. Taken here, it is the state immediately
	// before THIS reconciliation's grant. It is NOT a baseline of what any earlier
	// release provisioned: this controller has reconciled these identities many
	// times, and each of those reconciliations ran the same grant. The first
	// observation on a box is simply the earliest one anybody looked at.
	//
	// READ-ONLY and NON-FATAL. It issues GETs and nothing else, and a failure to
	// read authorization must not stop an identity being reconciled: the
	// inventory's job is to say what is true, including that it could not find
	// out.
	//
	// It establishes OBSERVED and nothing further. There is no intended policy to
	// compare against until project-per-cluster is reconciled, and a role name is
	// not an access decision -- so the condition below says whether the read
	// succeeded, never that the identity is compliant.
	inventory, invErr := r.InfisicalClient.InventoryProjectMemberships(ctx, identity.ID,
		[]string{projectID, secretsProjectID})
	switch {
	case invErr != nil:
		logger.Error(invErr, "Could not inventory this identity's Infisical memberships",
			"identityID", identity.ID)
		// The previous observation's timestamp is left alone -- it records when a read
		// last succeeded, which is still true -- but it is repeated in this message so
		// a reader seeing the failure cannot take that timestamp for current state.
		r.setCondition(smi, conditionTypeAuthorizationInventoried, metav1.ConditionFalse,
			"InventoryFailed", fmt.Sprintf("%v; %s", invErr, lastObservedPhrase(smi)))
	default:
		summaries := make([]string, 0, len(inventory.Memberships))
		for _, m := range inventory.Memberships {
			summaries = append(summaries, m.Summary())
		}
		// The DETAIL goes to the log, not to the resource: a role list in a cluster
		// object is authorization metadata on the wrong side of the boundary, and
		// nothing in Kubernetes reads it.
		logger.Info("Observed this identity's Infisical memberships",
			"identityID", identity.ID,
			"memberships", summaries,
			"notInspected", inventory.NotInspected,
			"complete", inventory.Complete())
		now := metav1.Now()
		smi.Status.LastAuthorizationInventory = &now
		if unresolved := inventory.Unresolved(); len(unresolved) > 0 {
			r.setCondition(smi, conditionTypeAuthorizationInventoried, metav1.ConditionFalse,
				"PartiallyObserved",
				fmt.Sprintf("read %d membership(s); unresolved: %s",
					len(inventory.Memberships), strings.Join(unresolved, "; ")))
			// Partial is not success: the timestamp above says a read happened, and
			// this condition is what says it was incomplete.
		} else {
			r.setCondition(smi, conditionTypeAuthorizationInventoried, metav1.ConditionTrue,
				"Observed",
				fmt.Sprintf("read %d membership(s); this records what was observed, not that it is correct",
					len(inventory.Memberships)))
		}
	}

	// Grant project access. The identity is granted access to both the
	// configured cert-manager project (hub-platform) and the secrets project
	// (hub-secrets) so it can authenticate for both cert issuance and
	// ExternalSecret delivery.
	//
	// UNCHANGED, DELIBERATELY. The role this asks for is too broad and the
	// correction is designed (one secrets project per workload cluster, this
	// identity a viewer of its own project alone, PKI on a separately scoped
	// identity). It is not applied here: narrowing a live identity before every
	// ESO and PKI consumer has been moved would turn an over-privilege defect into
	// failed image pulls, database bootstrap and certificate issuance.
	for _, pid := range []string{projectID, secretsProjectID} {
		if pid == "" {
			continue
		}
		if err := r.InfisicalClient.GrantProjectAccess(ctx, identity.ID, pid, "admin"); err != nil {
			logger.Error(err, "Failed to grant project access", "projectId", pid)
			r.setCondition(smi, conditionTypeReady, metav1.ConditionFalse, "AccessFailed", err.Error())
			_ = r.Status().Update(ctx, smi)
			return ctrl.Result{}, err
		}
	}

	// Handle rotation
	if requeue, err := r.handleRotation(ctx, smi, identity.ID); err != nil {
		logger.Error(err, "Rotation failed")
		r.setCondition(smi, conditionTypeReady, metav1.ConditionFalse, "RotationFailed", err.Error())
		_ = r.Status().Update(ctx, smi)
		return ctrl.Result{}, err
	} else if requeue {
		return ctrl.Result{RequeueAfter: 1 * time.Minute}, r.Status().Update(ctx, smi)
	}

	// Ensure CRS wrapper Secret for ClusterResourceSet delivery
	if err := r.ensureCRSWrapper(ctx, smi); err != nil {
		logger.Error(err, "Failed to ensure CRS wrapper")
		r.setCondition(smi, conditionTypeReady, metav1.ConditionFalse, "CRSWrapperFailed", err.Error())
		_ = r.Status().Update(ctx, smi)
		return ctrl.Result{}, err
	}

	// THE WORKLOAD IDENTITY, narrowed to its own cluster's secrets project.
	//
	// Non-fatal and reported: a failure to narrow leaves the identity as it was,
	// which is the state the box is in today -- degraded security rather than a
	// broken cluster. Failing the reconcile here would also stop the rotation and
	// delivery below, which is a worse outcome than a membership left broad for
	// one more loop.
	if err := r.ensureWorkloadIdentityScope(ctx, smi, identity.ID); err != nil {
		logger.Error(err, "Failed to scope the workload identity to its own project")
		r.setCondition(smi, conditionTypeWorkloadScoped, metav1.ConditionFalse, "ScopingFailed", err.Error())
	}

	// THE ISSUER'S OWN IDENTITY, scoped to issuance and nothing else.
	//
	// Separate from the identity above because ESO and the certificate issuer have
	// different consequences if compromised: one reads a cluster's secrets, the
	// other mints certificates the whole fleet trusts. They shared one credential
	// -- infisical-auth, delivered to both platform-ops and platform-security --
	// so either consumer could exercise the other's authority.
	//
	// Non-fatal on its own, and reported: a cluster whose issuer is not yet scoped
	// has no certificates, which is a degraded cluster rather than one that should
	// stop being reconciled. The wrapper below publishes nothing without it.
	pki, pkiErr := r.ensurePKIIdentity(ctx, smi)
	r.setPKICondition(smi, pkiErr)
	if pkiErr != nil {
		logger.Error(pkiErr, "Failed to scope the certificate issuer's identity")
	}

	// Ensure ADR-048 second-stage ClusterIssuer CRS wrapper. The identity operator
	// GENERATES the {spoke}-cluster-issuer wrapper Secret; it never watches or
	// mutates the spoke's infisical-fleet-issuer ClusterIssuer. CAPI ClusterResourceSet
	// delivers the completed ClusterIssuer to the spoke. Fail-closed: no payload is
	// published until the issuer's own credential exists, and the reason is surfaced
	// via the ClusterIssuerConfigured condition so the Wrapper-Ready invariant holds.
	if err := r.ensureClusterIssuerCRSWrapper(ctx, smi, pki); err != nil {
		logger.Error(err, "Failed to ensure ClusterIssuer CRS wrapper")
		r.setCondition(smi, conditionTypeIssuerConfigured, metav1.ConditionFalse, "ClusterIssuerWrapperFailed", err.Error())
		_ = r.Status().Update(ctx, smi)
		return ctrl.Result{}, err
	}

	// Drift detection: verify identity still exists
	exists, err := r.InfisicalClient.IdentityExists(ctx, identity.ID)
	if err != nil {
		logger.Error(err, "Failed to verify identity existence")
	} else if !exists {
		logger.Info("Identity missing from Infisical — will recreate on next reconciliation")
		smi.Status.IdentityID = ""
		smi.Status.ClientID = ""
	}

	r.setCondition(smi, conditionTypeReady, metav1.ConditionTrue, "Reconciled", "Machine Identity reconciled")
	r.setCondition(smi, conditionTypeIdentityCreated, metav1.ConditionTrue, "Created", fmt.Sprintf("Identity %s provisioned", identity.ID))
	r.setCondition(smi, conditionTypeIssuerConfigured, metav1.ConditionTrue, "WrapperReady", "ClusterIssuer CRS wrapper rendered")

	// Phase 2 self-heal (P0 single-authority gate, see
	// .kiro/specs/completed/infisical-pki-authority/): probe the identity's
	// login state and clear an active lockout only; an "Invalid credentials"
	// response is a secret defect that must surface (Ready=False
	// AuthenticationDefect), never be cleared. Runs last so its condition
	// overrides the Ready=True above when a defect is present.
	if err := r.ensureIdentityUnlock(ctx, smi, logger); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("Machine Identity reconciled", "identityId", identity.ID)
	requeueAfter := r.rotationCheckInterval(smi)
	if requeueAfter > identityHealthCheckInterval {
		// Self-heal must never sleep for the whole rotation window (until/4 can
		// be weeks out). Cap the health probe cadence so a lockout is detected
		// and cleared within the health check interval regardless of the
		// certificate rotation schedule.
		requeueAfter = identityHealthCheckInterval
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, r.Status().Update(ctx, smi)
}

// ensureIdentityUnlock implements the Phase 2 lockout self-heal against the
// single authoritative Infisical instance. Credentials come from the
// {spoke}-machine-identity CRS wrapper (platform-ops copy of infisical-auth)
// with fallback to the rotation secret smi-{spoke}-auth. Behavior:
//   - no credentials material yet: skip (fail-closed, mirrors ADR-048).
//   - probe 200: healthy, no action (success resets the lockout counter).
//   - probe 401 "temporarily locked": clear the lockout, then re-probe to
//     confirm recovery.
//   - probe 401 "Invalid credentials": set Ready=False reason
//     AuthenticationDefect and do NOT clear — clearing a defective secret
//     would re-arm the failure loop and hide the defect.
func (r *SpokeMachineIdentityReconciler) ensureIdentityUnlock(ctx context.Context, smi *identityv1alpha1.SpokeMachineIdentity, logger logr.Logger) error {
	if smi.Status.ClientID == "" || smi.Status.IdentityID == "" {
		return nil
	}

	clientID, clientSecret, found, err := r.identityCredentials(ctx, smi)
	if err != nil {
		return fmt.Errorf("read identity credentials: %w", err)
	}
	if !found {
		logger.Info("No credentials material for identity, skipping lockout self-heal", "identityId", smi.Status.IdentityID)
		return nil
	}

	locked, err := r.InfisicalClient.ProbeIdentityLockout(ctx, clientID, clientSecret)
	if err != nil {
		if errors.Is(err, infisical.ErrInvalidCredentials) {
			r.setCondition(smi, conditionTypeReady, metav1.ConditionFalse, "AuthenticationDefect", err.Error())
			logger.Error(err, "Identity auth defect detected — lockout self-heal will NOT clear it", "identityId", smi.Status.IdentityID)
			return nil
		}
		return fmt.Errorf("probe identity lockout: %w", err)
	}
	if !locked {
		return nil
	}

	logger.Info("Identity auth method locked — clearing lockout", "identityId", smi.Status.IdentityID, "clientId", clientID)
	if err := r.InfisicalClient.ClearLockout(ctx, smi.Status.IdentityID, clientID); err != nil {
		return fmt.Errorf("clear identity lockout: %w", err)
	}

	stillLocked, err := r.InfisicalClient.ProbeIdentityLockout(ctx, clientID, clientSecret)
	if err != nil {
		if errors.Is(err, infisical.ErrInvalidCredentials) {
			r.setCondition(smi, conditionTypeReady, metav1.ConditionFalse, "AuthenticationDefect", err.Error())
			return nil
		}
		return fmt.Errorf("re-probe identity lockout after clear: %w", err)
	}
	if stillLocked {
		return fmt.Errorf("identity lockout not cleared: %s", clientID)
	}
	logger.Info("Identity lockout cleared and verified", "identityId", smi.Status.IdentityID)
	return nil
}

// identityCredentials returns the machine identity's client credentials from
// the CRS wrapper secret ({spoke}-machine-identity, identity.yaml) or, if the
// wrapper is not yet delivered, from the rotation secret (smi-{spoke}-auth).
func (r *SpokeMachineIdentityReconciler) identityCredentials(ctx context.Context, smi *identityv1alpha1.SpokeMachineIdentity) (clientID, clientSecret string, found bool, err error) {
	wrapperName := smi.Spec.SecretName
	if wrapperName == "" {
		wrapperName = fmt.Sprintf("%s-machine-identity", smi.Spec.SpokeRef.Name)
	}
	wrapper := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Name: wrapperName, Namespace: smi.Namespace}, wrapper); err == nil {
		payload := string(wrapper.Data["identity.yaml"])
		if payload == "" {
			payload = wrapper.StringData["identity.yaml"]
		}
		if id, secret, ok := parseIdentityAuthDoc(payload); ok {
			return id, secret, true, nil
		}
	} else if !apierrors.IsNotFound(err) {
		return "", "", false, fmt.Errorf("read identity CRS wrapper: %w", err)
	}

	authSecret := &corev1.Secret{}
	authSecretName := fmt.Sprintf("smi-%s-auth", smi.Spec.SpokeRef.Name)
	if err := r.Get(ctx, client.ObjectKey{Name: authSecretName, Namespace: smi.Namespace}, authSecret); err == nil {
		id := string(authSecret.Data["clientId"])
		secret := string(authSecret.Data["clientSecret"])
		if id != "" && secret != "" {
			return id, secret, true, nil
		}
	} else if !apierrors.IsNotFound(err) {
		return "", "", false, fmt.Errorf("read identity auth secret: %w", err)
	}

	return "", "", false, nil
}

// parseIdentityAuthDoc extracts client-id/client-secret from the first
// infisical-auth Secret document inside an identity.yaml CRS payload.
func parseIdentityAuthDoc(payload string) (clientID, clientSecret string, ok bool) {
	if payload == "" {
		return "", "", false
	}
	for _, doc := range strings.Split(payload, "---") {
		var decoded struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			StringData map[string]string `json:"stringData"`
		}
		if err := yaml.Unmarshal([]byte(doc), &decoded); err != nil {
			continue
		}
		if decoded.Metadata.Name != "infisical-auth" {
			continue
		}
		id := decoded.StringData["client-id"]
		secret := decoded.StringData["client-secret"]
		if id != "" && secret != "" {
			return id, secret, true
		}
	}
	return "", "", false
}

func (r *SpokeMachineIdentityReconciler) handleDeletion(ctx context.Context, smi *identityv1alpha1.SpokeMachineIdentity) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(smi, smiFinalizer) {
		return ctrl.Result{}, nil
	}

	// Revoke and delete if identity exists
	if smi.Status.IdentityID != "" && smi.Spec.RevocationPolicy != nil && smi.Spec.RevocationPolicy.RevokeOnDelete {
		if len(smi.Status.ClientSecretIDs) > 0 {
			if err := r.InfisicalClient.RevokeAllClientSecrets(ctx, smi.Status.IdentityID, smi.Status.ClientSecretIDs); err != nil {
				logger.Error(err, "Failed to revoke client secrets during deletion")
				return ctrl.Result{}, err
			}
		}
		if err := r.InfisicalClient.DeleteIdentity(ctx, smi.Status.IdentityID); err != nil {
			logger.Error(err, "Failed to delete Machine Identity during deletion")
			return ctrl.Result{}, err
		}
	}

	controllerutil.RemoveFinalizer(smi, smiFinalizer)
	return ctrl.Result{}, r.Update(ctx, smi)
}

func (r *SpokeMachineIdentityReconciler) handleRotation(ctx context.Context, smi *identityv1alpha1.SpokeMachineIdentity, identityID string) (requeue bool, err error) {
	if smi.Spec.RotationPolicy == nil || !smi.Spec.RotationPolicy.Enabled {
		return false, nil
	}

	now := time.Now()
	if smi.Status.NextRotation != nil && now.Before(smi.Status.NextRotation.Time) {
		return false, nil // Not due yet
	}

	logger := log.FromContext(ctx)
	logger.Info("Rotating client secret", "identityId", identityID)

	oldSecretID := ""
	if len(smi.Status.ClientSecretIDs) > 0 {
		oldSecretID = smi.Status.ClientSecretIDs[0]
	}

	_, newSecret, err := r.InfisicalClient.RotateClientSecret(ctx, identityID, oldSecretID)
	if err != nil {
		return false, fmt.Errorf("rotate client secret: %w", err)
	}

	nowTime := metav1.Now()
	smi.Status.LastRotated = &nowTime

	interval := 60 * 24 * time.Hour // default 60d
	if smi.Spec.RotationPolicy.Interval != "" {
		if d, err := time.ParseDuration(smi.Spec.RotationPolicy.Interval); err == nil {
			interval = d
		}
	}
	nextRotation := metav1.NewTime(nowTime.Add(interval))
	smi.Status.NextRotation = &nextRotation

	// Store new secret in Kubernetes for ESO delivery
	if newSecret != "" {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("smi-%s-auth", smi.Spec.SpokeRef.Name),
				Namespace: smi.Namespace,
				Labels: map[string]string{
					"identity.kamarkat.io/spoke": smi.Spec.SpokeRef.Name,
					"identity.kamarkat.io/type":  "machine-identity",
				},
				Annotations: map[string]string{
					"identity.kamarkat.io/identityId": identityID,
				},
			},
			StringData: map[string]string{
				"clientId":     smi.Status.ClientID,
				"clientSecret": newSecret,
			},
		}
		existing := &corev1.Secret{}
		if err := r.Get(ctx, client.ObjectKey{Name: secret.Name, Namespace: secret.Namespace}, existing); err == nil {
			existing.StringData = secret.StringData
			existing.Labels = secret.Labels
			existing.Annotations = secret.Annotations
			if err := r.Update(ctx, existing); err != nil {
				return false, fmt.Errorf("update auth secret: %w", err)
			}
		} else {
			if err := r.Create(ctx, secret); err != nil {
				return false, fmt.Errorf("create auth secret: %w", err)
			}
		}
	}

	r.setCondition(smi, conditionTypeRotationComplete, metav1.ConditionTrue, "Rotated", "Client secret rotated")
	return false, nil
}

func (r *SpokeMachineIdentityReconciler) rotationCheckInterval(smi *identityv1alpha1.SpokeMachineIdentity) time.Duration {
	if smi.Status.NextRotation == nil {
		return 5 * time.Minute
	}
	until := time.Until(smi.Status.NextRotation.Time)
	if until < 5*time.Minute {
		return 1 * time.Minute
	}
	return until / 4 // Check 4 times per rotation interval
}

func (r *SpokeMachineIdentityReconciler) ensureCRSWrapper(ctx context.Context, smi *identityv1alpha1.SpokeMachineIdentity) error {
	wrapperName := smi.Spec.SecretName
	if wrapperName == "" {
		wrapperName = fmt.Sprintf("%s-machine-identity", smi.Spec.SpokeRef.Name)
	}

	authSecret := &corev1.Secret{}
	authSecretName := fmt.Sprintf("smi-%s-auth", smi.Spec.SpokeRef.Name)
	if err := r.Get(ctx, client.ObjectKey{Name: authSecretName, Namespace: smi.Namespace}, authSecret); err != nil {
		if apierrors.IsNotFound(err) {
			// Auth secret not yet available (e.g. rotation disabled or first
			// reconcile). Keep an existing wrapper untouched; when there is no
			// wrapper yet there is nothing deliverable, so this is a no-op.
			return nil
		}
		return fmt.Errorf("read auth secret %s: %w", authSecretName, err)
	}

	clientID := string(authSecret.Data["clientId"])
	clientSecret := string(authSecret.Data["clientSecret"])

	// THE WORKLOAD IDENTITY, DELIVERED ONCE, TO ITS ONE CONSUMER.
	//
	// This is ESO's credential: the spoke's ClusterSecretStore authenticates with
	// it to resolve ExternalSecrets. Nothing else reads it.
	//
	// Two things used to be here for the certificate issuer and are gone. A
	// SECOND COPY in cert-manager, because the issuer authenticated as this same
	// identity -- it now has its own, infisical-pki-auth, so one credential no
	// longer serves two consumers with different consequences if compromised. And
	// a DUPLICATE `clientSecret` key beside `client-secret`, because pki-issuer
	// v0.2.0 read secretData["clientSecret"] hardcoded whatever the ClusterIssuer
	// declared; v1.1.0 resolves both halves through ResolveSecretReference and
	// honours the key it is told (internal/auth/universal.go), so the second
	// spelling has no reader. The issuer Deployment's SECRET_STORE_* env went with
	// it: v1.1.0's main reads POD_NAMESPACE and nothing else.
	identityYAML := fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: infisical-auth
  namespace: platform-ops
type: Opaque
stringData:
  client-id: %s
  client-secret: %s
`, clientID, clientSecret)

	wrapper := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      wrapperName,
			Namespace: smi.Namespace,
			Labels:    map[string]string{"addons.cluster.x-k8s.io/resource-set": "true"},
		},
		Type:       "addons.cluster.x-k8s.io/resource-set",
		StringData: map[string]string{"identity.yaml": identityYAML},
	}

	wrapper.SetOwnerReferences(smi.GetOwnerReferences())

	existing := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Name: wrapperName, Namespace: smi.Namespace}, existing); err == nil {
		if existing.Type == "addons.cluster.x-k8s.io/resource-set" {
			// Rotation keeps the wrapper up to date with the current client
			// secret so spokes (re)provisioned from the CRS get fresh creds.
			if string(existing.Data["identity.yaml"]) == identityYAML {
				return nil
			}
			existing.StringData = map[string]string{"identity.yaml": identityYAML}
			existing.Data = nil
			if err := r.Update(ctx, existing); err != nil {
				return fmt.Errorf("update identity CRS wrapper: %w", err)
			}
			return nil
		}
	}

	if err := r.Create(ctx, wrapper); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("create identity CRS wrapper: %w", err)
	}

	return nil
}

// ensureClusterIssuerCRSWrapper renders the ADR-048 second-stage completed
// ClusterIssuer into a ClusterResourceSet wrapper Secret for delivery to the
// spoke. Ownership contract (ADR-048): the identity operator GENERATES the
// {spoke}-cluster-issuer wrapper only; it never watches or mutates the spoke's
// infisical-fleet-issuer ClusterIssuer. CAPI ClusterResourceSet owns delivery to
// the spoke, and cert-manager + the Infisical issuer own certificate issuance.
//
// Fail-closed: when clientId or projectId is empty — i.e. the identity stage has
// not produced authoritative material yet — NO payload is published. This mirrors
// the bootstrap-cert "defer rather than write empty" pattern (spokepool_controller.go).
// A wrapper published with empty identifiers would be applied once by CRS ApplyOnce
// and permanently break the spoke's cert issuance. The empty-input case is silent
// (not an error): the ClusterIssuerConfigured condition is set by the caller after
// a successful reconcile, so "ClusterIssuerConfigured=True" is the invariant for
// "wrapper present AND identifiers populated".
func (r *SpokeMachineIdentityReconciler) ensureClusterIssuerCRSWrapper(ctx context.Context, smi *identityv1alpha1.SpokeMachineIdentity, pki *pkiIdentity) error {
	logger := log.FromContext(ctx)

	wrapperName := fmt.Sprintf("%s-cluster-issuer", smi.Spec.SpokeRef.Name)

	// Fail-closed (ADR-048, constraint 4): never publish a half-rendered payload.
	// CRS delivery is ApplyOnce, so a ClusterIssuer that reaches a spoke without a
	// resolvable credential breaks that spoke's certificate issuance permanently.
	if pki == nil || pki.ClientID == "" || pki.ClientSecret == "" {
		logger.Info("Deferring ClusterIssuer CRS wrapper until the issuer's own identity is scoped",
			"wrapper", wrapperName)
		return nil
	}

	issuerURL, err := clusterIssuerURLFor()
	if err != nil {
		// Deferred, not defaulted -- same reasoning as the empty clientId above:
		// a half-rendered artifact must not reach the spoke, and an issuer naming
		// the wrong Infisical is worse than none, because it looks configured.
		logger.Error(err, "Deferring ClusterIssuer CRS wrapper", "wrapper", wrapperName)
		return err
	}

	// THE ISSUER'S CREDENTIAL AND THE ISSUER, IN ONE PAYLOAD.
	//
	// Delivered together because they are useless apart: a ClusterIssuer whose
	// secret references resolve to nothing leaves every CertificateRequest
	// pending, and the credential alone configures nothing. One CRS slot keeps
	// them atomic and needs no new resource in the SpokePool composition.
	//
	// THE SCHEMA IS pki-issuer v1.1.0's, which is the image
	// spoke-catalog/infra/infisical-issuer.yaml deploys. This rendered the v0.2.0
	// shape until 2026-11-23 -- `authentication.universalAuth` with an inlined
	// clientId, plus `certificateTemplateName` and `projectId` -- and v1.1.0's
	// IssuerSpec declares none of those, while REQUIRING `application` and
	// `profile`. So the object could not be admitted at all: a CRD rejection for
	// missing required fields, on the only producer of the issuer three spoke
	// certificates name.
	//
	// `profile` carries the value the old `certificateTemplateName` did. v1.x
	// renamed the concept -- a certificate profile of a PKI application -- so the
	// value was right and the field was not.
	//
	// SECRET REFERENCES, not an inlined client id, and no duplicate key. v0.2.0
	// read secretData["clientSecret"] regardless of what the issuer declared,
	// which is why the identity wrapper carries that second copy; v1.1.0 resolves
	// both halves through ResolveSecretReference and honours Name, Namespace and
	// Key (internal/auth/universal.go). So the credential is written once, under
	// the names the issuer is told to read.
	clusterIssuerYAML := fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: %s
  namespace: %s
type: Opaque
stringData:
  client-id: %s
  client-secret: %s
---
apiVersion: infisical-issuer.infisical.com/v1alpha1
kind: ClusterIssuer
metadata:
  name: infisical-fleet-issuer
spec:
  url: %s
  application: %s
  profile: %s
  authentication:
    method: universal
    universal:
      clientIdRef:
        name: %s
        namespace: %s
        key: client-id
      clientSecretRef:
        name: %s
        namespace: %s
        key: client-secret
`, pkiAuthSecretName, pkiAuthNamespace, pki.ClientID, pki.ClientSecret,
		issuerURL, pkiApplicationName, clusterIssuerProfile,
		pkiAuthSecretName, pkiAuthNamespace, pkiAuthSecretName, pkiAuthNamespace)

	wrapper := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      wrapperName,
			Namespace: smi.Namespace,
			Labels:    map[string]string{"addons.cluster.x-k8s.io/resource-set": "true"},
		},
		Type:       "addons.cluster.x-k8s.io/resource-set",
		StringData: map[string]string{"cluster-issuer.yaml": clusterIssuerYAML},
	}
	wrapper.SetOwnerReferences(smi.GetOwnerReferences())

	existing := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Name: wrapperName, Namespace: smi.Namespace}, existing); err == nil {
		if existing.Type == "addons.cluster.x-k8s.io/resource-set" {
			// Real API servers return the payload under .data (StringData is
			// write-only); test/in-memory clients may still expose StringData.
			current := string(existing.Data["cluster-issuer.yaml"])
			if current == "" {
				current = existing.StringData["cluster-issuer.yaml"]
			}
			// clientId is rotation-stable (only the secret rotates), so a matching
			// payload means no regeneration is needed. If the clientId truly changed
			// (identity recreated), update the wrapper so future (re)provisioned
			// spokes get the new identifier. Live spokes are ApplyOnce-bound and are
			// not re-rendered by a wrapper content change.
			if current == clusterIssuerYAML {
				return nil
			}
			existing.StringData = map[string]string{"cluster-issuer.yaml": clusterIssuerYAML}
			existing.Data = nil
			if err := r.Update(ctx, existing); err != nil {
				return fmt.Errorf("update ClusterIssuer CRS wrapper: %w", err)
			}
			return nil
		}
	}

	if err := r.Create(ctx, wrapper); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("create ClusterIssuer CRS wrapper: %w", err)
	}

	logger.Info("Created ClusterIssuer CRS wrapper", "wrapper", wrapperName, "spoke", smi.Spec.SpokeRef.Name)
	return nil
}

func (r *SpokeMachineIdentityReconciler) setCondition(smi *identityv1alpha1.SpokeMachineIdentity, condType string, status metav1.ConditionStatus, reason, message string) {
	now := metav1.Now()
	cond := metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
		ObservedGeneration: smi.Generation,
	}

	for i, existing := range smi.Status.Conditions {
		if existing.Type == condType {
			if existing.Status != status || existing.Reason != reason {
				smi.Status.Conditions[i] = cond
			}
			return
		}
	}
	smi.Status.Conditions = append(smi.Status.Conditions, cond)
}

func (r *SpokeMachineIdentityReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&identityv1alpha1.SpokeMachineIdentity{}).
		Named("spokemachineidentity").
		Complete(r)
}

// lastObservedPhrase says when this identity's authorization was last read
// successfully, for a message reporting that the current read was not. Without it,
// status.lastAuthorizationInventory sits beside a failure looking like the state
// of the world now.
func lastObservedPhrase(smi *identityv1alpha1.SpokeMachineIdentity) string {
	if smi.Status.LastAuthorizationInventory == nil {
		return "this identity's authorization has never been read successfully"
	}
	return fmt.Sprintf("the last successful read was %s and is now stale",
		smi.Status.LastAuthorizationInventory.UTC().Format(time.RFC3339))
}
