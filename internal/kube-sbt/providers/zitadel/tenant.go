package zitadel

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/soloz-io/zero-ops/internal/kube-sbt/interfaces"
	"github.com/soloz-io/zero-ops/internal/kube-sbt/models"
)

// Role keys granted within a tenant's project.
//
// Deliberately few and generic. These are the platform's authorisation
// vocabulary, not an application's: a fleet needing finer permissions expresses
// them in its own model rather than by multiplying roles here, because every
// role added here has to be granted, revoked and audited for every tenant.
// defaultUserRole is granted to a user created without an explicit role.
//
// The least privilege that still permits authentication. It cannot be "no role":
// projectRoleCheck denies sign-in to an ungranted user, so an unspecified role
// has to mean the smallest one rather than none.
const defaultUserRole = "member"

// The access token form an application issues. JWT, so a receiver validates the
// token by its signature instead of asking the issuer about every request --
// which is what lets a spoke's vmauth authorize a viewer with no call back to
// the hub. Switched to Bearer in the console it becomes an opaque string, and
// the only symptom is 401 from every query.
const accessTokenTypeOIDCJWT = "OIDC_TOKEN_TYPE_JWT"

var defaultRoles = []struct{ Key, Display string }{
	{"admin", "Admin"},
	{"member", "Member"},
	{"viewer", "Viewer"},
}

// EnsureTenant makes a tenant's identity resources exist and returns them.
//
// Idempotent by construction: every step is find-or-create, so it can run on
// every reconcile. That matters more than it looks — this runs against an
// external system with no transaction, so a partial failure must leave the next
// attempt able to finish rather than to conflict.
//
// The order is a dependency chain, not a preference: an application belongs to a
// project, a project belongs to an organisation, and a role belongs to a
// project.
// ErrOrgUnadopted is returned when an organisation already exists under this
// tenant's name but no immutable binding records it as this tenant's.
//
// It is not a failure to fix by retrying. It means the platform cannot tell
// whether that organisation is this tenant's -- carrying its users, grants and
// OAuth clients -- or someone else's that happens to share a name. Creating a
// second one resolves the ambiguity by destroying it: a ZITADEL organisation is
// a security boundary and a user belongs to exactly one, so the tenant ends up
// with two principals and every live session on the wrong side of the split.
//
// That is what happened on 2026-09-24. Renaming waypoint to nutgraf made the
// name search miss, a second organisation was created with its own clients, and
// logins failed with an audience the gateway had never issued for.
var ErrOrgUnadopted = errors.New("zitadel: organisation exists but is not adopted")

// EnsureTenantIdentity reconciles one tenant's identity against a KNOWN
// organisation.
//
// knownOrgID is the immutable binding (ADR-088), held in the tenant record.
// Empty means the tenant has none yet, and the only safe outcomes are then:
// create an organisation because none exists, or return ErrOrgUnadopted because
// one does. The name is never used to decide that an existing organisation is
// this tenant's -- a name is a presentation attribute and changes.
//
// project selects the APPLICATION's project inside that organisation (ADR-094):
// its clients, redirect URIs, roles and grants live there, so a second
// application of one tenant gets its own client rather than being handed the
// first one's.
func (a *Auth) EnsureTenantIdentity(ctx context.Context, tenantID, knownOrgID string, project models.ProjectBinding, ownerEmail string, selfRegistration bool, redirectURIs, postLogoutURIs []string, oauthClients []models.OAuthClient) (*models.TenantIdentity, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("zitadel: tenantID is required")
	}

	// What did not finish. Every step below that is non-fatal -- the tenant is
	// still worth returning, because its client id must not be lost -- appends
	// here rather than discarding its error, and the function returns them with
	// the partial identity. A caller receives both: the identifiers it can
	// publish, and the reason the tenant is not yet usable.
	var incomplete []string

	orgID, err := a.resolveOrg(ctx, tenantID, knownOrgID)
	if err != nil {
		return nil, fmt.Errorf("resolve organisation for %q: %w", tenantID, err)
	}

	projectID, err := a.resolveProject(ctx, orgID, project)
	if err != nil {
		return nil, fmt.Errorf("resolve project for %q: %w", tenantID, err)
	}

	// Roles must exist before any grant can reference them, and the project must
	// assert them before any token carries them. Asserting is a project setting
	// rather than a per-app one: the application flag alone is not sufficient,
	// which is only discoverable by finding tokens that carry no roles despite
	// the scope being requested and the grant existing.
	if err := a.ensureRoles(ctx, orgID, projectID); err != nil {
		return nil, fmt.Errorf("ensure roles for %q: %w", tenantID, err)
	}
	if err := a.ensureRoleAssertion(ctx, orgID, projectID); err != nil {
		return nil, fmt.Errorf("ensure role assertion for %q: %w", tenantID, err)
	}

	// The browser client's NAME comes from the fleet when the fleet declared one.
	//
	// A fleet chooses what its clients are called (ADR-047), so a name this
	// package picked would be one the fleet could not change. The fallback below
	// is a generic default for a fleet that declares nothing, not an assumption
	// about any tenant's architecture.
	publicName := "public-client"
	for _, c := range oauthClients {
		if !c.Confidential && c.Name != "" {
			publicName = c.Name
			break
		}
	}

	clientID, err := a.ensureOIDCApp(ctx, orgID, projectID, tenantID+"-"+publicName, redirectURIs, postLogoutURIs)
	if err != nil {
		return nil, fmt.Errorf("ensure application for %q: %w", tenantID, err)
	}

	// Grant the owner, or the tenant has no one who can enter it.
	//
	// projectRoleCheck denies authentication to a user holding no role in the
	// project, so provisioning that stops at the resources produces a tenant
	// whose every login fails with a grant error. The owner is made an
	// administrator of their own organisation as well, so they can add the next
	// user without the platform doing it for them — delegated tenant
	// administration is the provider's own model, not something built on top.
	//
	// Best-effort and non-fatal: the identity resources are correct either way,
	// and the next reconcile repeats this. Failing the whole call would discard a
	// client id that was already allocated.
	// Everyone in this organisation gets a role, or they cannot sign in.
	//
	// Self-service registration creates the account and nothing else, so without
	// this a person registers successfully and is then refused with
	// GrantRequired the moment they try to use it.
	if selfRegistration {
		if gerr := a.grantUngrantedMembers(ctx, orgID, projectID); gerr != nil {
			incomplete = append(incomplete, fmt.Sprintf("grant self-registered members: %v", gerr))
		}
	}

	// Reconciled on every pass so a policy changed by hand converges back.
	// Non-fatal: the tenant's identity is correct either way, and failing here
	// would discard a client id that was already allocated.
	if rerr := a.EnsureTenantSelfRegistration(ctx, orgID, selfRegistration); rerr != nil {
		// Reported, not discarded. This policy is what the login UI reads to
		// decide whether to offer a register link at all -- Zitadel's
		// apps/login/src/components/username-form.tsx gates it on allowRegister
		// -- so losing this error leaves a tenant whose users have no way in and
		// nothing saying why.
		incomplete = append(incomplete, fmt.Sprintf("set self-registration=%v: %v", selfRegistration, rerr))
	}

	out := &models.TenantIdentity{TenantRef: orgID, ProjectRef: projectID, ClientID: clientID}
	if ownerEmail == "" {
		return out, nil
	}

	owner, err := a.findUserByEmail(ctx, ownerEmail)
	if err != nil && !isNotFound(err) {
		// Could not tell whether the owner exists. Creating one now risks a
		// duplicate account, so the tenant is returned without an owner and the
		// next reconcile retries -- but INCOMPLETE, because a tenant with no
		// owner account refuses every login.
		incomplete = append(incomplete, fmt.Sprintf("look up owner %q: %v", ownerEmail, err))
		out.Incomplete = incomplete
		return out, nil
	}

	// THIS organisation's user, or none. The search is global -- it carries no
	// organisation -- so it answers "does this email exist anywhere in the
	// issuer", which is a different question from the one being asked.
	//
	// Adopting whatever it returns has two consequences and both are silent. The
	// tenant gets no owner account of its own, so its organisation stays empty
	// and every login at its hostname is refused with "User not found in the
	// system" while the tenant looks fully provisioned from every other side.
	// And the grant below is then applied to a user belonging to SOMEONE ELSE's
	// organisation -- most likely the platform's own, since that is where an
	// operator's address already exists -- which hands a foreign account admin
	// on this tenant's project.
	//
	// The response already carries the owning organisation; the caller discarded
	// it. Compared here, so a user found outside this organisation is treated as
	// absent and one is created in the right place.
	if owner != nil && owner.TenantID != orgID {
		owner = nil
	}

	if owner == nil {
		// The owner does not exist yet, which is the NORMAL case for a new
		// tenant. Creating them here is what makes a freshly provisioned tenant
		// usable: the issuer refuses anyone holding no role, so a tenant with no
		// owner account is a tenant whose every login fails.
		//
		// Created WITHOUT a password, so the ISSUER owns the credential from here
		// (ADR-060, Amendment 2026-09-26).
		//
		// This generated one until 2026-09-26, because the issuer had no mail
		// sender and an invitation could not be delivered. That reasoning was
		// sound and its consequence was not: the generated value was filed in the
		// tenant's secret store as OWNER_INITIAL_PASSWORD, which nothing in the
		// repository ever read, and it was sent with changeRequired false — so a
		// random string nobody could retrieve became the account's standing
		// credential. Observed on an owner created 2026-09-22: the bootstrap hash
		// still in place four days later, exactly one successful password check in
		// the account's entire history, and only failures after it. A browser
		// holding the session from that one success kept working, so it presented
		// as intermittent rather than as permanently un-enterable.
		//
		// Omitting the password is precisely what makes the issuer send its
		// initialisation mail: shouldAddInitCode requires no password supplied OR
		// an unverified address (internal/command/user_human.go:437), so the empty
		// password satisfies it on its own and the address can stay verified. The
		// user follows the mail and chooses their own credential; the platform
		// never holds one.
		//
		// This depends on the instance having a mail sender. EnsureSMTP reconciles
		// that (smtp.go) and the chart carries the same intent for a rebuilt box.
		// With no sender the account is created and no mail arrives, which is why
		// that reconcile is not optional.
		created, cerr := a.CreateUser(ctx, models.User{
			Email:    ownerEmail,
			Name:     ownerEmail,
			TenantID: orgID,
			Roles:    []string{"admin"},
		})
		if cerr != nil {
			// A 409 here is NOT "the same person, attach them". A user belongs
			// to exactly one ZITADEL organisation and cannot be moved between
			// them; the same address in two organisations is two accounts, not
			// one principal. Email equality is not an identity key, and reading
			// it as one would let a rename silently re-home a tenant's owner.
			//
			// So it is reported, with what it actually means, and the tenant is
			// returned incomplete. Resolving it is an explicit decision about
			// WHICH organisation is this tenant's (ADR-088).
			detail := cerr.Error()
			if strings.Contains(detail, "409") || strings.Contains(detail, "already exists") {
				detail = fmt.Sprintf("%v -- this address already has an account in ANOTHER organisation. "+
					"A user cannot be moved between organisations, so this is not resolved by retrying: "+
					"either this tenant is bound to the wrong organisation (check status.identity.zitadelOrgId) "+
					"or the owner needs a distinct account here", cerr)
			}
			incomplete = append(incomplete, fmt.Sprintf("create owner %q: %s", ownerEmail, detail))
			out.Incomplete = incomplete
			return out, nil
		}
		owner = created
	}

	// Idempotent, and repeated on every reconcile so a grant removed by hand
	// converges back rather than leaving the owner locked out of their own
	// tenant.
	// An owner holding no role is refused at login with GrantRequired, so these
	// are part of "the tenant is usable" rather than decoration.
	if gerr := a.GrantRole(ctx, orgID, projectID, owner.ID, []string{"admin"}); gerr != nil {
		incomplete = append(incomplete, fmt.Sprintf("grant the owner a project role: %v", gerr))
	}
	if oerr := a.EnsureOrgOwner(ctx, orgID, owner.ID); oerr != nil {
		incomplete = append(incomplete, fmt.Sprintf("make the owner an organisation manager: %v", oerr))
	}

	out.Incomplete = incomplete

	return out, nil
}

// resolveOrg turns a tenant into its organisation id WITHOUT trusting the name.
//
//	knownOrgID set   -> verify it still exists and use it. The name may have
//	                    changed; that is expected and is not this function's
//	                    business.
//	knownOrgID empty -> a name search decides only between "create" and
//	                    "stop". A hit is NOT adoption: it is an organisation
//	                    whose ownership the platform cannot establish, so it
//	                    returns ErrOrgUnadopted and the operator reports it.
//
// The asymmetry is the point. Creating on a miss is safe -- nothing exists to
// conflict with. Adopting on a hit is not, because a name is not an identity.
func (a *Auth) resolveOrg(ctx context.Context, name, knownOrgID string) (string, error) {
	if knownOrgID != "" {
		ok, err := a.orgExists(ctx, knownOrgID)
		if err != nil {
			return "", fmt.Errorf("verify bound organisation %s: %w", knownOrgID, err)
		}
		if !ok {
			// Deleted upstream. Silently creating a replacement would strip
			// every user and grant the tenant had; say so instead.
			return "", fmt.Errorf("%w: bound organisation %s no longer exists and will not be recreated automatically", ErrOrgUnadopted, knownOrgID)
		}
		return knownOrgID, nil
	}

	existing, err := a.findOrgByName(ctx, name)
	if err != nil {
		return "", err
	}
	if existing != "" {
		return "", fmt.Errorf("%w: %q is organisation %s, which no tenant record claims. Adopt it explicitly (record status.identity.zitadelOrgId) or rename it; a second organisation would split this tenant's users from its clients", ErrOrgUnadopted, name, existing)
	}
	return a.createOrg(ctx, name)
}

// orgExists answers whether an organisation id is still live.
func (a *Auth) orgExists(ctx context.Context, orgID string) (bool, error) {
	var got struct {
		Org struct {
			ID string `json:"id"`
		} `json:"org"`
	}
	if err := a.api.do(ctx, http.MethodGet, "/management/v1/orgs/me", orgID, nil, &got); err != nil {
		if strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), "NotFound") {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// findOrgByName returns an organisation id, or "" when none carries the name.
//
// Split out of the old ensureOrg deliberately. Finding and creating were one
// call, so "no organisation of this name" and "this tenant has no organisation"
// were the same answer -- and a rename made the first true while the second was
// false. Separating them lets the caller decide, which is where the decision
// belongs (ADR-088).
func (a *Auth) findOrgByName(ctx context.Context, name string) (string, error) {
	var found struct {
		Result []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"result"`
	}
	q := map[string]any{
		"queries": []any{map[string]any{"nameQuery": map[string]any{"name": name, "method": "TEXT_QUERY_METHOD_EQUALS"}}},
	}
	if err := a.api.do(ctx, http.MethodPost, "/admin/v1/orgs/_search", "", q, &found); err != nil {
		return "", err
	}
	for _, o := range found.Result {
		if o.Name == name {
			return o.ID, nil
		}
	}
	return "", nil
}

func (a *Auth) createOrg(ctx context.Context, name string) (string, error) {
	var created struct {
		ID string `json:"id"`
	}
	if err := a.api.do(ctx, http.MethodPost, "/management/v1/orgs", "", map[string]any{"name": name}, &created); err != nil {
		return "", err
	}
	return created.ID, nil
}

// RenameOrg changes an organisation's NAME, leaving its id untouched.
//
// This is how a tenant rename reaches the identity provider: the binding is the
// id, the name is presentation. Note that ZITADEL may derive the default domain
// from the name, which can affect login names -- so this is an explicit
// migration step with its own verification, never a side effect of
// reconciliation.
func (a *Auth) RenameOrg(ctx context.Context, orgID, name string) error {
	return a.api.do(ctx, http.MethodPut, "/management/v1/orgs/me", orgID, map[string]any{"name": name}, nil)
}

// resolveProject turns an application's project binding into a project id.
//
//	KnownID set  -> verify it still exists and use it. Deleted upstream is an
//	                error, not a recreate: a new project has none of the
//	                application's clients, roles or grants, and recreating it
//	                would sign everyone out under a new client id.
//	KnownID empty -> find or create by Name within the bound organisation, or
//	                the configured legacy project when Name is empty.
func (a *Auth) resolveProject(ctx context.Context, orgID string, b models.ProjectBinding) (string, error) {
	if b.KnownID != "" {
		ok, err := a.projectExists(ctx, orgID, b.KnownID)
		if err != nil {
			return "", fmt.Errorf("verify bound project %s: %w", b.KnownID, err)
		}
		if !ok {
			return "", fmt.Errorf("bound project %s no longer exists and will not be recreated automatically", b.KnownID)
		}
		return b.KnownID, nil
	}
	name := b.Name
	if name == "" {
		name = a.cfg.ProjectName
	}
	return a.ensureProject(ctx, orgID, name)
}

// projectExists answers whether a project id is live in this organisation.
func (a *Auth) projectExists(ctx context.Context, orgID, projectID string) (bool, error) {
	var got struct {
		Project struct {
			ID string `json:"id"`
		} `json:"project"`
	}
	err := a.api.do(ctx, http.MethodGet, "/management/v1/projects/"+projectID, orgID, nil, &got)
	if isNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return got.Project.ID == projectID, nil
}

func (a *Auth) ensureProject(ctx context.Context, orgID, name string) (string, error) {
	var found struct {
		Result []struct {
			ID    string `json:"id"`
			Name  string `json:"name"`
			State string `json:"state"`
		} `json:"result"`
	}
	q := map[string]any{
		"queries": []any{map[string]any{"nameQuery": map[string]any{"name": name, "method": "TEXT_QUERY_METHOD_EQUALS"}}},
	}
	if err := a.api.do(ctx, http.MethodPost, "/management/v1/projects/_search", orgID, q, &found); err != nil {
		return "", err
	}
	// An inactive project takes its applications out of service with it, so the
	// same reasoning as ensureApp applies: existence in the search index is not
	// usability, and a project accepted without checking its state yields
	// applications whose client ids the issuer refuses. Reactivated rather than
	// replaced, because a new project would strip every grant and role the
	// tenant's users hold.
	for _, p := range found.Result {
		if p.Name != name {
			continue
		}
		if p.State == "PROJECT_STATE_INACTIVE" {
			if err := a.api.do(ctx, http.MethodPost,
				"/management/v1/projects/"+p.ID+"/_reactivate", orgID, map[string]any{}, nil); err != nil {
				return "", fmt.Errorf("reactivate project %q (%s): %w", name, p.ID, err)
			}
		}
		return p.ID, nil
	}

	var created struct {
		ID string `json:"id"`
	}
	if err := a.api.do(ctx, http.MethodPost, "/management/v1/projects", orgID, projectSettings(name), &created); err != nil {
		return "", err
	}
	return created.ID, nil
}

// projectSettings is the tenancy policy, enforced BY THE ISSUER.
//
// All three matter, and the two checks are what make this multi-tenant natively
// rather than by convention:
//
//	projectRoleAssertion  put the granted roles in the token
//	projectRoleCheck      "deny authentication if the user has no roles assigned
//	                      to this project"
//	hasProjectCheck       "verified that their affiliated organization has been
//	                      granted access to this project. Authentication is not
//	                      permitted for users from unauthorized organizations"
//
// With these on, tenant isolation is enforced BEFORE a token is minted. A user
// of another organisation is refused at the authorization endpoint, so the
// application never sees a token it has to reason about — which is a stronger
// guarantee than checking a tenant claim after the fact, because a check that
// lives in an application is one an application can forget to make.
//
// The owning organisation always passes hasProjectCheck (auth_request.go
// compares the project's resource owner against the user's organisation), so
// this costs a tenant nothing for its own project and denies everyone else by
// default. Cross-tenant access is then an explicit project GRANT rather than an
// absence of enforcement — see GrantProjectToOrg.
//
// The consequence is deliberate and worth stating: a user with no role cannot
// log in at all. That is why provisioning grants a role when it creates a user;
// an ungranted user is not a user with reduced access, it is a user who is
// refused.
func projectSettings(name string) map[string]any {
	return map[string]any{
		"name":                   name,
		"projectRoleAssertion":   true,
		"projectRoleCheck":       true,
		"hasProjectCheck":        true,
		"privateLabelingSetting": "PRIVATE_LABELING_SETTING_UNSPECIFIED",
	}
}

// ensureRoleAssertion turns on role assertion for an EXISTING project.
//
// Separate from creation because a project provisioned before this setting
// existed would otherwise never gain it, and the symptom is silent: tokens carry
// no roles even though the scope was requested and the grant is in place.
func (a *Auth) ensureRoleAssertion(ctx context.Context, orgID, projectID string) error {
	// The project's OWN name, not the configured one.
	//
	// Zitadel's PUT replaces the whole project, name included, so sending
	// a.cfg.ProjectName here RENAMES whatever project this is to the shared
	// default. That was invisible while every application lived in that one
	// project -- the PUT set the name to what it already was. The moment
	// ADR-094 gave an application a project of its own it became a rename into
	// a name the organisation already holds:
	//
	//   PUT /management/v1/projects/<oranger's project>
	//   409 Project already exists on organization (V3-DKcYh)
	//
	// which surfaced as a 502 from the identity service, an AINativeSaaS whose
	// status.identity stayed empty, no OIDC_CLIENT_ID in Infisical, an
	// ExternalSecret in SecretSyncedError, and finally a gateway pod stuck in
	// CreateContainerConfigError behind "no healthy upstream". Six symptoms, one
	// field.
	name, err := a.projectName(ctx, orgID, projectID)
	if err != nil {
		return fmt.Errorf("read project %s before updating it: %w", projectID, err)
	}
	return a.api.do(ctx, http.MethodPut, "/management/v1/projects/"+projectID, orgID,
		projectSettings(name), nil)
}

// projectName reads a project's current name, so an update can preserve it.
func (a *Auth) projectName(ctx context.Context, orgID, projectID string) (string, error) {
	var got struct {
		Project struct {
			Name string `json:"name"`
		} `json:"project"`
	}
	if err := a.api.do(ctx, http.MethodGet, "/management/v1/projects/"+projectID, orgID, nil, &got); err != nil {
		return "", err
	}
	if got.Project.Name == "" {
		// Refused rather than defaulted. Falling back to the configured name is
		// exactly the bug above, and a project with no name is a response this
		// code does not understand.
		return "", fmt.Errorf("zitadel: project %s returned no name", projectID)
	}
	return got.Project.Name, nil
}

// GrantProjectToOrg delegates a project to ANOTHER organisation.
//
// This is the provider's own answer to cross-tenant access, and the reason
// hasProjectCheck can be left on: access from outside the owning organisation is
// expressed as a grant that someone made, rather than as enforcement nobody
// enabled. Idempotent, so it is safe to reconcile.
func (a *Auth) GrantProjectToOrg(ctx context.Context, ownerOrgID, projectID, grantedOrgID string, roles []string) error {
	var existing struct {
		Result []struct {
			GrantID      string `json:"grantId"`
			GrantedOrgID string `json:"grantedOrgId"`
		} `json:"result"`
	}
	if err := a.api.do(ctx, http.MethodPost,
		"/management/v1/projects/"+projectID+"/grants/_search", ownerOrgID,
		map[string]any{"query": map[string]any{"limit": 100}}, &existing); err != nil && !isNotFound(err) {
		return err
	}
	for _, g := range existing.Result {
		if g.GrantedOrgID == grantedOrgID {
			return a.api.do(ctx, http.MethodPut,
				"/management/v1/projects/"+projectID+"/grants/"+g.GrantID, ownerOrgID,
				map[string]any{"roleKeys": roles}, nil)
		}
	}
	return a.api.do(ctx, http.MethodPost,
		"/management/v1/projects/"+projectID+"/grants", ownerOrgID,
		map[string]any{"grantedOrgId": grantedOrgID, "roleKeys": roles}, nil)
}

// EnsureOrgDomain gives a tenant a domain of its own.
//
// This is what lets the hosted login discover a tenant from the user's address
// instead of the platform having to ask which tenant they meant, and it is the
// provider's mechanism rather than one the platform builds. Verification is not
// requested: the domain is derived from the instance's own zone, so it is
// already ours.
func (a *Auth) EnsureOrgDomain(ctx context.Context, orgID, domain string) error {
	err := a.api.do(ctx, http.MethodPost, "/management/v1/orgs/me/domains", orgID,
		map[string]any{"domain": domain}, nil)
	if err == nil {
		return nil
	}
	// Already present is the normal case on every reconcile after the first.
	var ae *apiError
	if asAPIError(err, &ae) && (ae.Status == http.StatusConflict || ae.Status == http.StatusPreconditionFailed) {
		return nil
	}
	return err
}

// EnsureOrgOwner makes a user an administrator OF THEIR OWN organisation.
//
// Delegated tenant administration, using the provider's org membership model
// rather than a platform-specific "tenant admin" notion. The authority is scoped
// to the organisation, so a tenant administrator cannot reach another tenant —
// which is the property a platform-level admin flag would have to re-implement
// and could get wrong.
func (a *Auth) EnsureOrgOwner(ctx context.Context, orgID, userID string) error {
	err := a.api.do(ctx, http.MethodPost, "/management/v1/orgs/me/members", orgID,
		map[string]any{"userId": userID, "roles": []string{"ORG_OWNER"}}, nil)
	if err == nil {
		return nil
	}
	var ae *apiError
	if asAPIError(err, &ae) && (ae.Status == http.StatusConflict || ae.Status == http.StatusPreconditionFailed) {
		return a.api.do(ctx, http.MethodPut, "/management/v1/orgs/me/members/"+userID, orgID,
			map[string]any{"roles": []string{"ORG_OWNER"}}, nil)
	}
	return err
}

func (a *Auth) ensureRoles(ctx context.Context, orgID, projectID string) error {
	var existing struct {
		Result []struct {
			Key string `json:"key"`
		} `json:"result"`
	}
	if err := a.api.do(ctx, http.MethodPost,
		"/management/v1/projects/"+projectID+"/roles/_search", orgID,
		map[string]any{"query": map[string]any{"limit": 100}}, &existing); err != nil {
		return err
	}
	have := map[string]bool{}
	for _, r := range existing.Result {
		have[r.Key] = true
	}

	for _, want := range defaultRoles {
		if have[want.Key] {
			continue
		}
		body := map[string]any{"roleKey": want.Key, "displayName": want.Display, "group": a.cfg.ProjectName}
		if err := a.api.do(ctx, http.MethodPost,
			"/management/v1/projects/"+projectID+"/roles", orgID, body, nil); err != nil {
			return err
		}
	}
	return nil
}

func (a *Auth) ensureOIDCApp(ctx context.Context, orgID, projectID, appName string, redirectURIs, postLogoutURIs []string) (string, error) {
	// The CLIENT id, which is the second return value. ensureApp returns
	// (appID, clientID, clientSecret): taking the first hands back Zitadel's
	// internal application id, and the two are indistinguishable by inspection --
	// both are numeric strings of the same shape.
	//
	// Published as a client id it is refused at the authorization endpoint with
	// `Errors.App.NotFound`, the same answer an identifier that was never
	// allocated receives, so nothing in the refusal says the value is of the
	// wrong KIND. Every layer above reports success: the application really was
	// created, the operator really did publish what it was given, the
	// ExternalSecret resolves, and the tenant's gateway starts. Only a login
	// fails, and it fails identically to a tenant that was never provisioned.
	_, clientID, _, err := a.ensureApp(ctx, orgID, projectID, appName, redirectURIs, postLogoutURIs, authMethodNone, false)
	return clientID, err
}

// OIDC client authentication methods, in the issuer's own vocabulary.
const (
	// authMethodNone is a PUBLIC client: no secret, PKCE carries the proof.
	authMethodNone = "OIDC_AUTH_METHOD_TYPE_NONE"
	// authMethodBasic is a CONFIDENTIAL client: it authenticates with a secret
	// the issuer generates and returns exactly once.
	authMethodBasic = "OIDC_AUTH_METHOD_TYPE_BASIC"
)

// ensureApp is find-or-create for one OIDC application.
//
// It returns the app's id, its client id, and — for a confidential client that
// this call CREATED — the generated secret. The secret is empty when the app
// already existed, because the issuer returns it once at creation and will not
// disclose it again; recovering it means regenerating, which is a separate and
// deliberately explicit act (see RegenerateClientSecret).
func (a *Auth) ensureApp(ctx context.Context, orgID, projectID, appName string, redirectURIs, postLogoutURIs []string, authMethod string, tokenExchange bool) (appID, clientID, clientSecret string, err error) {

	// The token-exchange grant is added ONLY when asked for. Zitadel gates the
	// grant on the client -- without it the token endpoint refuses the exchange
	// -- and its own guidance is to put it on confidential clients only,
	// because a client holding it can turn a token it was handed into one for a
	// different audience. Granting it to every confidential client would hand
	// that to fleet-declared back-ends that never asked for it; granting it to
	// a PUBLIC client would hand it to anything that can read a browser bundle.
	//
	// It also decides what a client's tokens can carry (ADR-110). A client with
	// only the authorization-code and refresh grants is issued tokens for people
	// alone: the login refuses a machine user, and client credentials and JWT
	// profile tokens name the machine user as their client, never an
	// application. The exchange grant breaks that, since an exchanged token takes
	// whatever subject it was handed -- which is why a receiver admits only a
	// client without it as one that carries a user.
	grantTypes := []string{"OIDC_GRANT_TYPE_AUTHORIZATION_CODE", "OIDC_GRANT_TYPE_REFRESH_TOKEN"}
	if tokenExchange {
		if authMethod != authMethodBasic {
			return "", "", "", fmt.Errorf("zitadel: refusing token exchange on non-confidential application %q", appName)
		}
		grantTypes = append(grantTypes, "OIDC_GRANT_TYPE_TOKEN_EXCHANGE")
	}

	// A PUBLIC client: authMethodType NONE, no secret, PKCE carries the proof.
	// Anything shipped to a browser is readable, so a secret here would be a
	// published secret.
	//
	// Role assertion is set on the app as well as the project because both gate
	// the claim, and only the pair produces a token carrying roles.
	//
	// The same body creates the application and updates it: the issuer's update
	// REPLACES the configuration, so a field left out of an update is reset --
	// the access token type falls back to opaque, which a receiver cannot
	// validate without introspection (ADR-097 invariant 2).
	config := map[string]any{
		"redirectUris":             redirectURIs,
		"postLogoutRedirectUris":   postLogoutURIs,
		"responseTypes":            []string{"OIDC_RESPONSE_TYPE_CODE"},
		"grantTypes":               grantTypes,
		"appType":                  "OIDC_APP_TYPE_WEB",
		"authMethodType":           authMethod,
		"accessTokenType":          accessTokenTypeOIDCJWT,
		"accessTokenRoleAssertion": true,
		"idTokenRoleAssertion":     true,
		"idTokenUserinfoAssertion": true,
		"devMode":                  false,
	}

	var existing struct {
		Result []struct {
			ID         string `json:"id"`
			Name       string `json:"name"`
			State      string `json:"state"`
			OIDCConfig struct {
				ClientID               string   `json:"clientId"`
				RedirectURIs           []string `json:"redirectUris"`
				PostLogoutRedirectURIs []string `json:"postLogoutRedirectUris"`
				GrantTypes             []string `json:"grantTypes"`
				// Read back because it is the difference between a token a
				// receiver can validate by itself and an opaque string it
				// cannot. Nothing else in this platform would notice it
				// changing: the spoke's vmauth simply fails to parse what it
				// is given and answers 401, naming neither the token type nor
				// the application it came from.
				AccessTokenType string `json:"accessTokenType"`
			} `json:"oidcConfig"`
		} `json:"result"`
	}
	if err := a.api.do(ctx, http.MethodPost,
		"/management/v1/projects/"+projectID+"/apps/_search", orgID,
		map[string]any{"query": map[string]any{"limit": 100}}, &existing); err != nil {
		return "", "", "", err
	}
	// STATE, not merely existence. The search returns inactive applications
	// alongside active ones, and an inactive application is not resolvable for
	// authorization: the issuer answers `Errors.App.NotFound` for its client id,
	// the same answer it gives for an identifier that was never allocated.
	//
	// Accepting one produced a loop that could not repair itself. This function
	// returned the inactive application's client id, the caller published it as
	// though provisioning had succeeded, the tenant's gateway presented it, and
	// every login was refused. Each reconciliation found the same application
	// and repeated the same answer, so the condition survived indefinitely while
	// every layer above reported success — the Kubernetes resources healthy, the
	// ExternalSecret resolving, the operator logging "Tenant identity
	// provisioned" on every pass.
	//
	// An inactive application is reactivated rather than replaced. Its client id
	// is already held by the tenant's gateway and by anything else configured
	// from it, so replacing it would require every consumer to converge on a new
	// identifier; reactivation restores the one they hold. A reactivation that
	// fails leaves the application inactive and returns the error, because
	// continuing would publish an identifier that still does not authorize.
	for _, app := range existing.Result {
		if app.Name != appName || app.OIDCConfig.ClientID == "" {
			continue
		}
		if app.State == "APP_STATE_INACTIVE" {
			if err := a.api.do(ctx, http.MethodPost,
				"/management/v1/projects/"+projectID+"/apps/"+app.ID+"/_reactivate",
				orgID, map[string]any{}, nil); err != nil {
				return "", "", "", fmt.Errorf("reactivate application %q (%s): %w", appName, app.ID, err)
			}
		}
		// A CONFIDENTIAL client is brought to its declared configuration, not
		// returned as found. A callback declared after the client was created
		// otherwise never reaches it, and the code flow fails at the
		// authorization endpoint while every layer above reports the client
		// provisioned. Its grants are asserted too, because they decide whether
		// its tokens can carry anyone but a person (above).
		//
		// Public clients are left as found: their configuration is not what
		// this reconciliation was extended to own.
		if authMethod == authMethodBasic &&
			(!sameSet(app.OIDCConfig.RedirectURIs, redirectURIs) ||
				!sameSet(app.OIDCConfig.PostLogoutRedirectURIs, postLogoutURIs) ||
				!sameSet(app.OIDCConfig.GrantTypes, grantTypes) ||
				app.OIDCConfig.AccessTokenType != accessTokenTypeOIDCJWT) {
			err := a.api.do(ctx, http.MethodPut,
				"/management/v1/projects/"+projectID+"/apps/"+app.ID+"/oidc_config",
				orgID, config, nil)
			if err != nil && !isUnchanged(err) {
				return "", "", "", fmt.Errorf("reconcile configuration of application %q (%s): %w", appName, app.ID, err)
			}
		}
		return app.ID, app.OIDCConfig.ClientID, "", nil
	}

	body := map[string]any{"name": appName}
	for k, v := range config {
		body[k] = v
	}
	var created struct {
		AppID        string `json:"appId"`
		ClientID     string `json:"clientId"`
		ClientSecret string `json:"clientSecret"`
	}
	if err := a.api.do(ctx, http.MethodPost,
		"/management/v1/projects/"+projectID+"/apps/oidc", orgID, body, &created); err != nil {
		return "", "", "", err
	}
	if created.ClientID == "" {
		return "", "", "", fmt.Errorf("zitadel: application %q created without a client id", appName)
	}
	if authMethod == authMethodBasic && created.ClientSecret == "" {
		return "", "", "", fmt.Errorf("zitadel: confidential application %q created without a client secret", appName)
	}
	return created.AppID, created.ClientID, created.ClientSecret, nil
}

// sameSet reports whether two lists hold the same values, in any order. The
// issuer returns a configuration's lists in its own order, so comparing them as
// sequences would rewrite an unchanged client on every reconcile.
func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, v := range a {
		seen[v]++
	}
	for _, v := range b {
		if seen[v] == 0 {
			return false
		}
		seen[v]--
	}
	return true
}

// GrantRole gives a user a role within a tenant's project.
//
// Idempotent: an existing grant is updated rather than rejected, so re-running
// reconciliation does not fail on work already done.
func (a *Auth) GrantRole(ctx context.Context, orgID, projectID, userID string, roles []string) error {
	var existing struct {
		Result []struct {
			ID        string `json:"id"`
			ProjectID string `json:"projectId"`
		} `json:"result"`
	}
	if err := a.api.do(ctx, http.MethodPost, "/management/v1/users/grants/_search", orgID,
		map[string]any{"queries": []any{map[string]any{"userIdQuery": map[string]any{"userId": userID}}}},
		&existing); err != nil && !isNotFound(err) {
		return err
	}
	for _, g := range existing.Result {
		if g.ProjectID == projectID {
			err := a.api.do(ctx, http.MethodPut,
				"/management/v1/users/"+userID+"/grants/"+g.ID, orgID,
				map[string]any{"roleKeys": roles}, nil)
			if isUnchanged(err) {
				// Already holds exactly these roles. The steady state, and the
				// common one, since this runs on every reconcile.
				return nil
			}
			return err
		}
	}
	return a.api.do(ctx, http.MethodPost, "/management/v1/users/"+userID+"/grants", orgID,
		map[string]any{"projectId": projectID, "roleKeys": roles}, nil)
}

var _ interfaces.ITenantIdentityProvisioner = (*Auth)(nil)

// EnsurePlatformApp provisions an OIDC application in the PLATFORM's own
// organisation and returns its client id.
//
// Separate from EnsureTenantIdentity because it is not a tenant. A tenant gets
// its own organisation so its users are owned by it; the platform's own clients
// — the Kubernetes API server's among them — belong where platform
// administrators already are, or those administrators could not authenticate to
// them at all: the issuer refuses a user whose organisation was never granted
// the project.
//
// Idempotent, like everything else here: it is called on every reconcile.
func (a *Auth) EnsurePlatformApp(ctx context.Context, appName string, redirectURIs, postLogoutURIs []string) (string, error) {
	if appName == "" {
		return "", fmt.Errorf("zitadel: appName is required")
	}
	orgID, err := a.platformOrg(ctx)
	if err != nil {
		return "", err
	}

	projectID, err := a.ensureProject(ctx, orgID, a.cfg.ProjectName)
	if err != nil {
		return "", fmt.Errorf("ensure platform project: %w", err)
	}
	if err := a.ensureRoles(ctx, orgID, projectID); err != nil {
		return "", fmt.Errorf("ensure platform roles: %w", err)
	}
	if err := a.ensureRoleAssertion(ctx, orgID, projectID); err != nil {
		return "", fmt.Errorf("ensure platform role assertion: %w", err)
	}

	clientID, err := a.ensureOIDCApp(ctx, orgID, projectID, appName, redirectURIs, postLogoutURIs)
	if err != nil {
		return "", fmt.Errorf("ensure platform application %q: %w", appName, err)
	}
	return clientID, nil
}

// EnsurePlatformProject makes a project exist in the PLATFORM's own
// organisation and returns the organisation and project ids (ADR-109).
//
// For a platform capability other applications are admitted to by audience --
// the Credit Authority -- which is not a tenant and has no client of its own.
// The issuer puts any requested project into a token's audience whatever
// organisation the client belongs to, so a tenant application's service client
// can carry this project's audience without the project living in its tenant.
//
// Idempotent: found by name, created when absent.
func (a *Auth) EnsurePlatformProject(ctx context.Context, name string) (orgID, projectID string, err error) {
	if name == "" {
		return "", "", fmt.Errorf("zitadel: project name is required")
	}
	orgID, err = a.platformOrg(ctx)
	if err != nil {
		return "", "", err
	}
	projectID, err = a.ensureProject(ctx, orgID, name)
	if err != nil {
		return "", "", fmt.Errorf("ensure platform project %q: %w", name, err)
	}
	// RECONCILED, not assumed from creation. projectSettings is sent by the
	// CREATE alone, so a project made before these settings existed -- or one an
	// operator edited in the console -- keeps whatever it has. That matters more
	// here than anywhere else in this file: projectRoleCheck and hasProjectCheck
	// are the properties that make a token for this project mean "someone
	// granted access to it", which is what the spoke's vmauth relies on. Lost,
	// they fail OPEN, and the box's telemetry becomes readable by any client of
	// the issuer -- while sign-in starts working better, not worse.
	if err := a.ensureProjectSettings(ctx, orgID, projectID, name); err != nil {
		return "", "", fmt.Errorf("reconcile settings of platform project %q: %w", name, err)
	}
	return orgID, projectID, nil
}

// ensureProjectSettings brings an existing project to the tenancy policy.
//
// The project's OWN name is sent, read back from the issuer, for the same reason
// ensureRoleAssertion does it: this PUT replaces the whole project, so a name
// taken from configuration renames whatever project this is.
func (a *Auth) ensureProjectSettings(ctx context.Context, orgID, projectID, name string) error {
	var current struct {
		Project struct {
			Name                 string `json:"name"`
			ProjectRoleAssertion bool   `json:"projectRoleAssertion"`
			ProjectRoleCheck     bool   `json:"projectRoleCheck"`
			HasProjectCheck      bool   `json:"hasProjectCheck"`
		} `json:"project"`
	}
	if err := a.api.do(ctx, http.MethodGet,
		"/management/v1/projects/"+projectID, orgID, nil, &current); err != nil {
		return fmt.Errorf("read project %q (%s): %w", name, projectID, err)
	}
	if current.Project.ProjectRoleAssertion &&
		current.Project.ProjectRoleCheck &&
		current.Project.HasProjectCheck {
		return nil
	}
	settings := projectSettings(name)
	if current.Project.Name != "" {
		settings["name"] = current.Project.Name
	}
	err := a.api.do(ctx, http.MethodPut,
		"/management/v1/projects/"+projectID, orgID, settings, nil)
	if err != nil && !isUnchanged(err) {
		return err
	}
	return nil
}

// platformOrg resolves the organisation the platform's own clients belong to.
//
// Configured when set, DISCOVERED otherwise: the service credential this
// provider authenticates with belongs to the instance's own organisation, which
// is where platform administrators are, so asking the issuer who we are answers
// the question without a second value to keep in step.
//
// That matters beyond tidiness. The id is allocated by the issuer at
// installation, so any configured copy is transcribed by hand from a UI — and a
// wrong one does not fail loudly. It creates the platform's applications inside
// some other organisation, where the administrators who need them are refused
// because their organisation was never granted the project.
func (a *Auth) platformOrg(ctx context.Context) (string, error) {
	if a.cfg.PlatformOrgID != "" {
		return a.cfg.PlatformOrgID, nil
	}
	var resp struct {
		Org struct {
			ID string `json:"id"`
		} `json:"org"`
	}
	if err := a.api.do(ctx, http.MethodGet, "/management/v1/orgs/me", "", nil, &resp); err != nil {
		return "", fmt.Errorf("zitadel: resolve the platform organisation: %w", err)
	}
	if resp.Org.ID == "" {
		return "", fmt.Errorf("zitadel: the issuer reported no organisation for this credential")
	}
	return resp.Org.ID, nil
}

// grantUngrantedMembers gives the default role to every member of a tenant's
// organisation that holds none.
//
// Needed because self-service registration creates a user and stops there. The
// tenant's project denies authentication to anyone holding no role
// (projectRoleCheck), and the issuer has no hook that grants one on signup — so
// a person completes registration, is created in the right organisation, and is
// then refused at the moment they try to use the account:
//
//	Errors.User.GrantRequired
//
// which names no remedy and arrives after they have already chosen a password.
//
// Reconciled rather than event-driven, because there is no event to hook. That
// makes the grant eventually-consistent: someone who registers between two
// reconciles is refused until the next one. Accepted deliberately — the
// alternative is turning projectRoleCheck off, which would admit users of OTHER
// organisations to this tenant, and a brief delay is a far better failure than a
// silent hole in tenant isolation.
//
// Only users with NO grant are touched. Anyone already holding a role keeps
// exactly what they were given, so an operator who granted admin by hand does
// not have it replaced by member on the next pass.
func (a *Auth) grantUngrantedMembers(ctx context.Context, orgID, projectID string) error {
	var users struct {
		Result []struct {
			UserID string `json:"userId"`
		} `json:"result"`
	}
	q := map[string]any{
		"query":   map[string]any{"limit": 500},
		"queries": []any{map[string]any{"organizationIdQuery": map[string]any{"organizationId": orgID}}},
	}
	if err := a.api.do(ctx, http.MethodPost, "/v2/users", orgID, q, &users); err != nil {
		return fmt.Errorf("list members of %q: %w", orgID, err)
	}

	var grants struct {
		Result []struct {
			UserID    string `json:"userId"`
			ProjectID string `json:"projectId"`
		} `json:"result"`
	}
	if err := a.api.do(ctx, http.MethodPost, "/management/v1/users/grants/_search", orgID,
		map[string]any{"query": map[string]any{"limit": 500}}, &grants); err != nil && !isNotFound(err) {
		return fmt.Errorf("list grants in %q: %w", orgID, err)
	}
	granted := map[string]bool{}
	for _, g := range grants.Result {
		if g.ProjectID == projectID {
			granted[g.UserID] = true
		}
	}

	for _, u := range users.Result {
		if granted[u.UserID] {
			continue
		}
		// Best effort per user: one failure must not stop the rest, or a single
		// bad account would keep every later registrant locked out.
		_ = a.GrantRole(ctx, orgID, projectID, u.UserID, []string{defaultUserRole})
	}
	return nil
}

// EnsureConfidentialClient provisions the tenant's server-side OAuth client and
// returns a credential that can be used immediately.
//
// This exists because a browser client is not sufficient. The tenant's gateway
// authenticates a PERSON with a public PKCE client, but the BFF then exchanges
// that session for a token of its own to call the platform API with — a
// server-side flow, which by definition authenticates the CLIENT and therefore
// needs a secret. Hydra supplied this through hydra-maester reconciling an
// OAuth2Client CR from a platform-generated secret; Zitadel has no such
// controller and generates the secret itself, so the direction of the credential
// is inverted and it must be captured here.
//
// The secret is returned ONLY when this call created or regenerated it. The
// issuer discloses it once, so an existing app cannot be read back — which is
// why regenerate is gated on the caller, not decided here. Regenerating on every
// reconcile would invalidate the credential the running workload is holding, and
// the failure lands on the next token exchange rather than on the reconcile that
// caused it.
//
// orgID and projectID are the ones EnsureTenantIdentity just resolved, never
// looked up again here. This used to find the organisation BY NAME and use the
// configured project, so an application in its own project (ADR-094) had its
// confidential clients -- the gateway's exchange client among them -- created in
// a different project from its browser client, and a renamed tenant had them
// created in no organisation at all.
func (a *Auth) EnsureConfidentialClient(ctx context.Context, orgID, projectID, appName string, redirectURIs []string, regenerateIfExists, tokenExchange bool) (clientID, clientSecret string, err error) {
	if orgID == "" || projectID == "" {
		return "", "", fmt.Errorf("zitadel: a confidential client needs the application's resolved organisation and project; provision its identity first")
	}
	if appName == "" {
		return "", "", fmt.Errorf("zitadel: appName is required")
	}

	// The authorization code is delivered to the workload's callback, which the
	// fleet declares and the caller resolves onto the tenant's public hosts.
	// Without it the code flow fails at the authorization endpoint (ADR-110). A
	// client that runs no browser flow -- the gateway's exchange client --
	// declares none and is given none.
	appID, clientID, secret, err := a.ensureApp(ctx, orgID, projectID, appName, redirectURIs, nil, authMethodBasic, tokenExchange)
	if err != nil {
		return "", "", fmt.Errorf("ensure confidential application %q: %w", appName, err)
	}
	if secret != "" {
		return clientID, secret, nil
	}

	// The app already existed, so the issuer will not disclose its secret.
	if !regenerateIfExists {
		return clientID, "", nil
	}
	secret, err = a.regenerateClientSecret(ctx, orgID, projectID, appID)
	if err != nil {
		return "", "", fmt.Errorf("regenerate client secret for %q: %w", appName, err)
	}
	return clientID, secret, nil
}

// regenerateClientSecret issues a new secret for an existing application,
// invalidating the previous one.
func (a *Auth) regenerateClientSecret(ctx context.Context, orgID, projectID, appID string) (string, error) {
	var out struct {
		ClientSecret string `json:"clientSecret"`
	}
	if err := a.api.do(ctx, http.MethodPost,
		"/management/v1/projects/"+projectID+"/apps/"+appID+"/oidc_config/_generate_client_secret",
		orgID, map[string]any{}, &out); err != nil {
		return "", err
	}
	if out.ClientSecret == "" {
		return "", fmt.Errorf("zitadel: client secret regenerated empty for application %q", appID)
	}
	return out.ClientSecret, nil
}
