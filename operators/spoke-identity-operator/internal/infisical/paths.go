package infisical

// Infisical REST API path constants.
//
// Source of truth: scripts/validate/infisical-api-contract.yaml, which records each
// endpoint's COMPLETE route -- api prefix, router registration prefix and
// router-local path -- for the Infisical version the box deploys, and is checked
// against both these constants and Infisical's own source by
// scripts/validate/preflight/031-infisical-api-contract.py. The path this comment
// used to name no longer exists, and a router-local path read straight from an
// upstream router file is not a public endpoint.
//
// NOTE: This operator manages Machine Identity lifecycle only.
// No certificate paths are included — PKI is cert-manager's domain per ADR-035.
const (
	PathAuthUniversalAuthLogin         = "/api/v1/auth/universal-auth/login"
	PathIdentities                     = "/api/v1/identities"
	PathIdentityByID                   = "/api/v1/identities/%s"
	PathAuthUniversalAuthIdentities    = "/api/v1/auth/universal-auth/identities/%s"
	PathAuthUniversalAuthClientSecrets = "/api/v1/auth/universal-auth/identities/%s/client-secrets"
	PathAuthUniversalAuthClearLockout  = "/api/v1/auth/universal-auth/sign-in/clear-lockout"
	PathProjectMembershipsIdentities   = "/api/v1/projects/%s/memberships/identities/%s"

	// The per-cluster secrets project (ADR-031, ADR-037), looked up by slug.
	// hub-operator creates it; this operator resolves its id to scope an identity
	// to it. Projects are the only authorization boundary an unlicensed Infisical
	// enforces -- built-in roles are project-scoped and carry no environment or
	// path conditions, and conditioned custom roles need PlanFeature.Rbac, which
	// the default plan does not grant.
	PathProjects = "/api/v1/projects"

	// PKI authorization is an APPLICATION surface, not a project one. The issuer
	// signs through an Infisical PKI Application and a Certificate Profile, and
	// the role that permits issuance is held on the application
	// (buildApplicationOperatorPermissionRules grants CertificateActions.Create).
	// Applications live in the organization's one active Certificate Manager
	// project -- the router refuses them anywhere else -- so PKI stays central
	// while secrets move to a project per cluster.
	PathCertManagerApplicationByName = "/api/v1/cert-manager/applications/by-name/%s"
	PathCertManagerAppIdentity       = "/api/v1/cert-manager/applications/%s/identities/%s"
)
