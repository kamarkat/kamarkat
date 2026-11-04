package interfaces

import (
	"context"

	"github.com/kamarkat/kamarkat/internal/kube-sbt/models"
)

// ITenantIdentityProvisioner is implemented by identity providers that can
// create a tenant's identity resources.
//
// OPTIONAL, and separate from IAuth on purpose. Whether a tenant is a
// first-class object in the provider is exactly what differs between providers:
// one whose tenancy is structural has an organisation to create, a project to
// hold roles, and an application to authenticate against; one where the tenant
// is merely an attribute on an identity has nothing to provision, and forcing it
// to satisfy this interface would mean writing a method that does nothing and
// returns a value it made up.
//
// Callers type-assert and skip when unimplemented, so the absence is a
// capability the platform can observe rather than a runtime failure.
type ITenantIdentityProvisioner interface {
	// knownOrgID is the IMMUTABLE binding from the tenant's record (ADR-088).
	// Empty means unbound, and an implementation must then either create --
	// when nothing exists -- or refuse. It must never adopt an organisation by
	// name: a name is a presentation attribute, and treating a rename as a new
	// tenant produces a second security principal whose users are not the
	// first's.
	//
	// EnsureTenantIdentity makes a tenant's identity resources exist and returns
	// them. Idempotent: it runs on every reconcile, and a partial failure must
	// leave the next attempt able to finish rather than to conflict.
	// ownerEmail, when set, is granted administrative access to the tenant.
	//
	// Not optional in practice. Where the issuer denies authentication to a user
	// holding no role, a tenant provisioned without one is a tenant NOBODY can
	// sign in to — the resources all exist and the first login fails with a
	// grant error that names no remedy.
	// selfRegistration is reconciled, not applied once: it is a security
	// boundary, and one that lives only in a provider console can be re-opened
	// by an upgrade or a support session with nothing to notice.
	// project selects the APPLICATION's project within the organisation
	// (ADR-094), bound by id once known, exactly as the organisation is.
	EnsureTenantIdentity(ctx context.Context, tenantID, knownOrgID string, project models.ProjectBinding, ownerEmail string, selfRegistration bool, redirectURIs, postLogoutURIs []string, oauthClients []models.OAuthClient) (*models.TenantIdentity, error)

	// EnsureConfidentialClient provisions a tenant's server-side OAuth client.
	//
	// Separate from the call above because it is not part of a tenant's identity:
	// it is a credential for one of the tenant's WORKLOADS, and its lifecycle is
	// the workload's. Folding it in would mean every reconcile of a tenant's
	// identity also touched a secret a running process is holding.
	//
	// clientSecret is returned ONLY when this call created or regenerated it. An
	// issuer discloses a generated secret once, so an empty value means "the
	// client exists and its secret is whatever you already stored", not "there is
	// no secret" — and the caller that cannot find a stored one is the only party
	// able to decide that invalidating the live credential is acceptable, which
	// is what regenerateIfExists expresses.
	//
	// orgID and projectID are the TenantRef and ProjectRef EnsureTenantIdentity
	// returned, so the client lands beside the application's browser client and
	// never in a project or organisation resolved some other way.
	//
	// redirectURIs are the client's callbacks. They are reconciled onto an
	// existing client as well as set on a new one, so a callback declared later
	// reaches a client created earlier.
	EnsureConfidentialClient(ctx context.Context, orgID, projectID, appName string, redirectURIs []string, regenerateIfExists, tokenExchange bool) (clientID, clientSecret string, err error)

	// EnsureMachineClient provisions the SERVICE identity an application calls
	// another application with (ADR-097).
	//
	// Distinct from the confidential client above, which authenticates a client
	// acting FOR A PERSON: the authorization code it completes belongs to someone
	// who logged in. A service-to-service call has no person, and modelling one
	// produces a token whose subject is a user that nobody is. A machine user's
	// subject IS the service, which is what lets invariant 8 hold -- nothing may
	// map a machine token's subject to a person.
	//
	// The implementation must ensure the identity mints JWTs rather than the
	// issuer's opaque default, and must assert that on an identity it did not
	// create. An opaque token cannot be validated locally, so a receiver holding
	// one has to introspect -- putting the issuer in the path of every
	// cross-application request, and failing in a way that reads as an
	// authorization bug rather than a provisioning one.
	//
	// clientSecret is returned ONLY when this call created or regenerated it, for
	// the same reason it is above.
	//
	// orgID is the TenantRef EnsureTenantIdentity returned. There is no project:
	// a machine user belongs to the organisation, and the audience it may request
	// is decided per token, not at provisioning (invariant 3).
	EnsureMachineClient(ctx context.Context, orgID, userName, displayName string, regenerateIfExists bool) (*models.MachineClient, error)
}
