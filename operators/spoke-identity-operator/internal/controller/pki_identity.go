package controller

import (
	"context"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	identityv1alpha1 "github.com/kamarkat/kamarkat/operators/spoke-identity-operator/api/v1alpha1"
	"github.com/kamarkat/kamarkat/operators/spoke-identity-operator/internal/infisical"
)

const (
	// pkiApplicationName is the Infisical PKI Application the fleet issues
	// through. It must equal the application the hub creates
	// (internal/kakt-cli/infisical/cert_profiles.go pkiApplicationName): the
	// issuer resolves it BY NAME, so two spellings mean a ClusterIssuer that
	// cannot find its application.
	pkiApplicationName = "kamarkat"

	// pkiApplicationRole permits issuance and nothing else on that application:
	// buildApplicationOperatorPermissionRules grants CertificateActions.Create,
	// Read and List plus ApplicationActions.Read. Not `admin`, which would also
	// let the holder reshape the application and its profiles.
	pkiApplicationRole = "operator"

	// pkiProjectRole is the minimum built-in role that lets the issuer resolve its
	// certificate profile by slug. That read authorizes through
	// getProjectPermission and needs CertificateProfileActions.Read on the
	// project, which `viewer` does not grant and `member` does. Broader than the
	// one read it is for, and an unlicensed Infisical cannot narrow it --
	// conditioning a role needs PlanFeature.Rbac. What bounds it is the project's
	// content: the Certificate Manager project holds PKI objects, and this
	// platform's secrets live in hub-secrets and in each cluster's own project.
	pkiProjectRole = "member"

	// pkiAuthSecretName is the Secret the ClusterIssuer reads its credential from,
	// on the spoke. Separate from ESO's infisical-auth deliberately: the two
	// consumers have different consequences if compromised -- one reads a
	// cluster's secrets, the other mints certificates the fleet trusts -- so
	// neither holds the other's credential.
	pkiAuthSecretName = "infisical-pki-auth"

	// pkiAuthNamespace is the namespace the ISSUER resolves credentials in, and it
	// is not a free choice.
	//
	// pki-issuer v1.1.0 confines a cluster-scoped issuer to one namespace:
	// --cluster-resource-namespace, defaulting to the controller's own (cmd/main.go
	// derives it when the flag is empty, which is what POD_NAMESPACE is for). Its
	// resolver then REFUSES anything else outright --
	// "cross-namespace references are not allowed" (internal/util/kubernetes.go
	// confineNamespace) -- and fetches from the confined namespace regardless of
	// what the reference says.
	//
	// So the credential lives where the issuer runs: spoke-catalog/infra/
	// infisical-issuer.yaml deploys it into cert-manager. Naming any other
	// namespace would be rejected, and passing --cluster-resource-namespace to
	// move the boundary would trade a guard for a flag: as it stands, upstream's
	// rule confines this issuer to its own namespace, which is a containment worth
	// having for a credential that mints certificates the fleet trusts.
	pkiAuthNamespace = "cert-manager"

	conditionTypePKIIdentityProvisioned = "PKIIdentityProvisioned"
)

// pkiIdentityName is the issuer's own identity for a cluster.
func pkiIdentityName(cluster string) string {
	return fmt.Sprintf("spoke-%s-pki", strings.TrimSpace(cluster))
}

// pkiIdentity is what the ClusterIssuer payload needs to authenticate.
type pkiIdentity struct {
	ID           string
	ClientID     string
	ClientSecret string
}

// ensurePKIIdentity provisions the certificate issuer's own Machine Identity and
// scopes it to issuance.
//
// TWO SURFACES, BOTH REQUIRED, AND NOT INTERCHANGEABLE:
//
//	application `operator`  -> POST /cert-manager/certificates     (issue)
//	project     `member`    -> GET  /cert-manager/certificate-profiles/slug/..
//
// An identity with the first and not the second cannot resolve the profile its
// ClusterIssuer names; with the second and not the first it cannot issue. They
// are different endpoints on different membership routes, so neither substitutes
// for the other.
//
// It never touches the workload identity's memberships, and the workload identity
// never gains either of these.
func (r *SpokeMachineIdentityReconciler) ensurePKIIdentity(ctx context.Context, smi *identityv1alpha1.SpokeMachineIdentity) (*pkiIdentity, error) {
	logger := log.FromContext(ctx).WithValues("spoke", smi.Spec.SpokeRef.Name)

	orgID := r.OrgID
	if smi.Spec.Infisical.OrganizationID != "" {
		orgID = smi.Spec.Infisical.OrganizationID
	}
	certProject := r.ProjectID
	if smi.Spec.Infisical.ProjectID != "" {
		certProject = smi.Spec.Infisical.ProjectID
	}
	if orgID == "" || certProject == "" {
		return nil, fmt.Errorf("the organization and the Certificate Manager project are both " +
			"required to scope certificate issuance; defaulting either would attach the " +
			"issuer's identity to the wrong place")
	}

	name := pkiIdentityName(smi.Spec.SpokeRef.Name)
	identity, err := r.InfisicalClient.GetOrCreateMachineIdentity(ctx, name, orgID)
	if err != nil {
		return nil, fmt.Errorf("provision the issuer's identity %s: %w", name, err)
	}

	// The profile read, on the Certificate Manager project. Exactly one role, so
	// an identity that previously held something broader is narrowed rather than
	// added to.
	changes, err := r.InfisicalClient.ReconcileSoleMembership(ctx, identity.ID, certProject, pkiProjectRole, nil)
	if err != nil {
		return nil, fmt.Errorf("scope the issuer's identity %s on the Certificate Manager "+
			"project: %w", name, err)
	}

	// Issuance, on the application.
	appID, err := r.InfisicalClient.FindApplicationByName(ctx, pkiApplicationName)
	if err != nil {
		return nil, fmt.Errorf("resolve PKI application %q: %w", pkiApplicationName, err)
	}
	if appID == "" {
		// The hub creates the application; an absent one is a hub that has not
		// finished rather than something this operator should invent, and inventing
		// it would produce an application with no profiles that every issuance then
		// fails against.
		return nil, fmt.Errorf("PKI application %q does not exist yet, so the issuer's identity "+
			"cannot be scoped to it", pkiApplicationName)
	}
	created, err := r.InfisicalClient.EnsureApplicationRole(ctx, appID, identity.ID, pkiApplicationRole)
	if err != nil {
		return nil, fmt.Errorf("grant %s on application %q to %s: %w",
			pkiApplicationRole, pkiApplicationName, name, err)
	}

	logger.Info("The certificate issuer's identity is scoped to issuance",
		"identity", name,
		"applicationRole", pkiApplicationRole,
		"applicationMembershipCreated", created,
		"projectRole", pkiProjectRole,
		"projectChanges", changeStrings(changes))

	return &pkiIdentity{ID: identity.ID, ClientID: identity.ClientID, ClientSecret: identity.ClientSecret}, nil
}

func changeStrings(changes []infisical.MembershipChange) []string {
	out := make([]string, 0, len(changes))
	for _, c := range changes {
		out = append(out, c.String())
	}
	return out
}

// setPKICondition records whether the issuer's identity is scoped, without
// publishing any of its material.
func (r *SpokeMachineIdentityReconciler) setPKICondition(smi *identityv1alpha1.SpokeMachineIdentity, err error) {
	if err != nil {
		r.setCondition(smi, conditionTypePKIIdentityProvisioned, metav1.ConditionFalse,
			"ScopingFailed", err.Error())
		return
	}
	r.setCondition(smi, conditionTypePKIIdentityProvisioned, metav1.ConditionTrue, "Scoped",
		fmt.Sprintf("%s holds %s on application %q and %s on the Certificate Manager project",
			pkiIdentityName(smi.Spec.SpokeRef.Name), pkiApplicationRole, pkiApplicationName, pkiProjectRole))
}
