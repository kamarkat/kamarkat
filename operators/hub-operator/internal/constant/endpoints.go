package constant

const (
	// Infisical API Endpoints
	APIEndpointLogin                      = "/api/v3/auth/login"
	APIEndpointSelectOrg                  = "/api/v3/auth/select-organization"
	APIEndpointProjects                   = "/api/v1/projects"
	APIEndpointIdentities                 = "/api/v1/identities"
	APIEndpointUniversalAuth              = "/api/v1/auth/universal-auth/identities"
	APIEndpointUniversalAuthIdentities    = "/api/v1/auth/universal-auth/identities/%s"
	APIEndpointUniversalAuthClientSecrets = "/api/v1/auth/universal-auth/identities/%s/client-secrets"
	APIEndpointUniversalAuthLogin         = "/api/v1/auth/universal-auth/login"
	APIEndpointClientSecrets              = "/client-secrets"
	APIEndpointProjectMemberships         = "/api/v1/projects/%s/memberships/identities/%s"
	APIEndpointProjectUserMemberships     = "/api/v1/projects/%s/memberships"
	APIEndpointSecretsRaw                 = "/api/v3/secrets/raw"
	APIEndpointSecretsRawKey              = "/api/v3/secrets/raw/%s"
	APIEndpointPKITemplates               = "/api/v2/pki/certificate-templates"
	APIEndpointCertificateAuthorities     = "/api/v1/cert-manager/ca"
	// The /internal segment is required: the generic ca/%s route does not exist
	// and returns 404. Mirrors the CLI's PathCACertificate.
	APIEndpointCACertificate = "/api/v1/cert-manager/ca/internal/%s/certificate"
	APIEndpointWorkspace     = "/api/v1/workspace"
	APIEndpointFolders       = "/api/v2/folders"

	// A workload cluster's own secrets project and the one environment inside it.
	// Recorded in scripts/validate/infisical-api-contract.yaml with their complete
	// resolved routes.
	APIEndpointProjectEnvironments    = "/api/v1/projects/%s/environments"
	APIEndpointProjectEnvironmentSlug = "/api/v1/projects/%s/environments/slug/%s"
)
