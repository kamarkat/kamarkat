package infisical

// Infisical REST API path constants.
// Source of truth: archived/identity-auth/infisical/backend/src/server/routes/
// V1 routes: v1/index.ts, V3 routes: v3/login-router.ts (mounted at /api/v3/auth)
const (
	ProjectSlug        = "hub-platform"
	SecretsProjectSlug = "hub-secrets"

	PathProjects               = "/api/v1/projects"
	PathIdentities             = "/api/v1/identities"
	PathAuthUniversalAuthLogin = "/api/v1/auth/universal-auth/login"
	PathAuthLoginV3            = "/api/v3/auth/login"
	PathAuthSelectOrgV3        = "/api/v3/auth/select-organization"
	PathWorkspace              = "/api/v1/workspace"

	PathAuthUniversalAuthIdentities    = "/api/v1/auth/universal-auth/identities/%s"
	PathAuthUniversalAuthClientSecrets = "/api/v1/auth/universal-auth/identities/%s/client-secrets"
	PathProjectMembershipsIdentities   = "/api/v1/projects/%s/memberships/identities/%s"
	PathOrgIdentityMemberships         = "/api/v1/organization/identity-memberships/%s"
	// The box's management secret scope, as an Infisical environment. A project is
	// created with Infisical's OWN defaults (dev, staging, prod) and the hub's scope
	// is not among them (ADR-111 Revision 7 §2), so Day-0 creates it: writing a
	// secret into an environment that does not exist is not a loud failure, it is a
	// hub whose ClusterSecretStores resolve nothing while reporting Healthy.
	//
	// /projects, not the /workspace spelling the same routes also answer to: the
	// project routes under /workspace are registered as deprecated upstream, and
	// createProject and the membership grants here already use /projects.
	PathProjectEnvironments          = "/api/v1/projects/%s/environments"
	PathProjectEnvironmentBySlug     = "/api/v1/projects/%s/environments/slug/%s"
	PathCertificateProfilesBySlug    = "/api/v1/cert-manager/certificate-profiles/slug/%s?projectId=%s"
	PathCertificateAuthorities       = "/api/v1/cert-manager/ca"
	PathCertificatePolicies          = "/api/v1/cert-manager/certificate-policies"
	PathCertificateProfiles          = "/api/v1/cert-manager/certificate-profiles"
	PathPKITemplates                 = "/api/v2/pki/certificate-templates"
	PathCertificateAuthoritiesCreate = "/api/v1/cert-manager/ca/internal"

	// The Applications model pki-issuer v1.x issues through.
	//
	// An Issuer names an APPLICATION and a PROFILE and no project (ClusterIssuer
	// spec, v1.1.0), so the server resolves the project itself -- from the
	// organisation's ACTIVE Certificate Manager project. With none set, every
	// request to the applications routes is refused before it reaches a handler:
	//
	//   400 Set an organization active Certificate Manager project before using
	//       Applications.
	//
	// which is what an unmigrated box reports, three boundaries from the cause.
	// Both routes exist in the deployed Infisical v0.160.11 (verified at that tag
	// in reference-projects: backend/src/server/routes/v1/index.ts registers the
	// instance and applications routers under the /cert-manager prefix), so this
	// needs no server upgrade.
	PathCertManagerInstance      = "/api/v1/cert-manager/instance"
	PathCertManagerActiveProject = "/api/v1/cert-manager/instance/active-project"
	PathCertManagerApplications  = "/api/v1/cert-manager/applications"

	// Membership and enrollment, under one application. Creating the application
	// and attaching profiles is not enough for a single certificate to issue:
	// the identity that asks must be a MEMBER of the application, and each
	// profile must have the API enrollment method enabled. Verified against the
	// vendored Infisical server (pki-application-membership-routers and
	// pki-application-enrollment-routers, v0.165.6+).
	//
	// Formats rather than constants, because both name an object in the path.
	PathCertManagerAppIdentityFmt   = "/api/v1/cert-manager/applications/%s/identities/%s"
	PathCertManagerAppIdentitiesFmt = "/api/v1/cert-manager/applications/%s/identities"
	PathCertManagerAppAPIEnrollFmt  = "/api/v1/cert-manager/applications/%s/profiles/%s/enrollment/api"
	PathCACertificate               = "/api/v1/cert-manager/ca/internal/%s/certificate"
	PathSecretsRaw                  = "/api/v3/secrets/raw/%s"
	// PathSecretsRawList lists a folder. Unlike PathSecretsRaw it names no key,
	// so it answers "what is here" rather than "is this here" -- which is the
	// question `kakt fleet secrets status` asks.
	PathSecretsRawList = "/api/v3/secrets/raw"

	// Used by getExistingOrgData to recover from the already-bootstrapped
	// case without destructively resetting the database. These return
	// the current organization and the identities the bootstrapper
	// created in the previous run.
	PathCurrentOrganization = "/api/v1/organization"
	PathIdentitiesByOrg     = "/api/v1/identities?orgId=%s"
)
