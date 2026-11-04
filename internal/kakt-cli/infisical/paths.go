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
	PathCertificateProfilesBySlug      = "/api/v1/cert-manager/certificate-profiles/slug/%s?projectId=%s"
	PathCertificateAuthorities         = "/api/v1/cert-manager/ca"
	PathCertificatePolicies            = "/api/v1/cert-manager/certificate-policies"
	PathCertificateProfiles            = "/api/v1/cert-manager/certificate-profiles"
	PathPKITemplates                   = "/api/v2/pki/certificate-templates"
	PathCertificateAuthoritiesCreate   = "/api/v1/cert-manager/ca/internal"
	PathCACertificate                  = "/api/v1/cert-manager/ca/internal/%s/certificate"
	PathSecretsRaw                     = "/api/v3/secrets/raw/%s"
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
