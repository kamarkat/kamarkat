package infisical

import (
	"os"

	"github.com/soloz-io/zero-ops/operators/hub-operator/internal/constant"
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
	EnvironmentSlug  = "dev"

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
	// No defaults. See the same pair in internal/soloz-cli/infisical/bootstrap.go:
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
