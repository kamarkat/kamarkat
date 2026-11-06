package infisical

import (
	"os"

	"github.com/kamarkat/kamarkat/operators/hub-operator/internal/constant"
)

const (
	// Infisical Service Configuration
	InfisicalServiceName      = "infisical-standalone-infisical"
	InfisicalServiceNamespace = "platform-security"
	InfisicalServicePort      = 8080
	InfisicalBaseURL          = "http://infisical-standalone-infisical.platform-security.svc.cluster.local:8080"

	// Bootstrap Configuration
	ProjectName      = "Hub-Platform"
	ProjectSlug      = "hub-platform"
	OrganizationName = "hub-ops"
	IdentityName     = "hub-operator"
	IdentityRole     = "admin"
	// No EnvironmentSlug here. It was `"dev"`, and both uploaders used it as the
	// Infisical environment they write to -- while every READER takes
	// INFISICAL_ENVIRONMENT_SLUG from hub-bootstrap-config, rendered from the box's
	// declared management.infisicalEnvironmentSlug. The two agreed only on a box
	// whose scope happened to be `dev`: anywhere else the writers filled `dev` and
	// the ExternalSecrets read the declared scope, so every secret these uploaders
	// own was absent from the environment its consumer looks in, and an
	// ExternalSecret that never syncs reports nothing. See managementEnvironmentSlug
	// in secret_uploader.go.

	// Secret Names
	SecretInfisicalAuth      = "infisical-auth"
	SecretInfisicalAdmin     = "infisical-admin"
	SecretHetznerDNS         = "hetzner-dns"         // For external-dns and cert-manager
	SecretHetznerCredentials = "hetzner-credentials" // For CAPI/CCM/CSI
	SecretHCloudDeprecated   = "hcloud"              // Deprecated: use SecretHetznerCredentials
	SecretInfisicalDB        = "infisical-db-credentials"
	SecretGHCRPull           = "ghcr-pull-secret"
	SecretPlatformGit        = "platform-git-secret"

	// Namespaces - unified across Hub and Spoke
	NamespaceCAPI          = "platform-capi"
	NamespaceOps           = "platform-ops"
	NamespaceCloudSystem   = "kube-system"
	NamespaceEdge          = "platform-edge"
	NamespaceData          = "platform-data"
	NamespaceIdentity      = "platform-identity"
	NamespaceSecurity      = "platform-security"
	NamespaceObservability = "platform-observability"

	// Infisical Secret Keys
	KeyHetznerDNSAPIKey    = "hetzner-dns-api-key"
	KeyHCloudToken         = "hcloud-token"
	KeyTailscaleAuthkey    = "tailscale-authkey"
	KeyTailscaleHostname   = "tailscale-hostname"
	KeyClientID            = constant.KeyClientID
	KeyClientSecret        = constant.KeyClientSecret
	KeyAdminToken          = "admin-token"
	KeyAdminEmail          = "admin-email"
	KeyOrgID               = "org-id"
	KeyProjectID           = constant.KeyProjectID
	KeyProjectSlug         = "project-slug"
	KeyAPIKey              = "api-key"
	KeyToken               = "token"
	KeyInfisicalDBUsername = "infisical-db-username"
	KeyInfisicalDBPassword = "infisical-db-password"
	KeyGHCRPullSecret      = "ghcr-pull-secret"
	KeyGitHubUsername      = "github-username"
	KeyGitHubToken         = "github-token"

	// Application Secret Keys (Operator uploads directly to Infisical)

	// Labels
	LabelManagedBy = "app.kubernetes.io/managed-by"
	LabelComponent = "app.kubernetes.io/component"
	ValueManagedBy = "hub-operator"
	ValueBootstrap = "infisical-bootstrap"
)

// Admin Credentials — overridable via environment variables.
// The .env file at the project root is sourced by hub-bootstrap.sh.
var (
	// No defaults. See the same pair in internal/kakt-cli/infisical/bootstrap.go:
	// these were a personal email and the literal "Password@123" on every box,
	// which is a shared administrator credential for every tenant's secret store.
	// Empty here, and the operator refuses to use an empty one rather than falling
	// back to a value the platform chose.
	AdminEmail    = os.Getenv("INFISICAL_ADMIN_EMAIL")
	AdminPassword = os.Getenv("INFISICAL_ADMIN_PASSWORD")
)

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
