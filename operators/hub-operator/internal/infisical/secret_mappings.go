package infisical

import "github.com/kamarkat/kamarkat/internal/platform/escrow"

// ============================================================================
// SECRET MAPPINGS REGISTRY
// ============================================================================
// This file is the SINGLE SOURCE OF TRUTH for CLI and Bootstrap secrets uploaded to Infisical.
//
// ARCHITECTURE FLOW (HYBRID PATTERN):
//
// BOOTSTRAP SECRETS (Operator creates → uploads to Infisical → ESO syncs back):
// 1. Operator generates bootstrap secrets in K8s (Phase 1)
// 2. Operator uploads them to Infisical (making Infisical the Source of Truth)
// 3. ExternalSecrets (ESO) syncs them back with creationPolicy: Merge
// 4. Infrastructure components consume (CNPG, Infisical, Redis)
//
// APPLICATION SECRETS (Operator uploads to Infisical → ESO creates in K8s):
// 1. Operator generates passwords and uploads directly to Infisical (Phase 0)
// 2. ExternalSecrets (ESO) creates K8s secrets with creationPolicy: Owner
// 3. Applications consume ESO-managed secrets
// 4. See ApplicationSecretMappings below for application secret definitions
//
// HOW TO ADD A NEW BOOTSTRAP SECRET:
// 1. Add entry to CLISecretMappings below
// 2. Create corresponding ExternalSecret manifest with creationPolicy: Merge
// 3. Operator will automatically upload on next reconciliation
//
// HOW TO ADD A NEW APPLICATION SECRET:
// 1. Add entry to ApplicationSecretMappings below
// 2. Create ExternalSecret manifest with creationPolicy: Owner
// 3. DO NOT add to CLISecretMappings (operator uploads directly to Infisical)
// ============================================================================

// SecretMapping defines how to map a K8s secret to Infisical
type SecretMapping struct {
	SourceNamespace string // K8s namespace where secret exists
	SourceName      string // K8s secret name
	SourceKey       string // Key within the K8s secret
	InfisicalKey    string // Key name in Infisical
	Description     string // Human-readable description for logging

	// InfisicalPath is the folder the key is written to. Empty means "/". It may
	// contain TenantPlaceholder, replaced by this box's tenant id (TENANT_ID, from
	// hub-bootstrap-config) -- for a credential that belongs to the box owner and
	// so lives in their /<tenant>-mgmt folder, not at the project root.
	InfisicalPath string

	// Transform, when set, derives the uploaded value from the source value. For
	// a credential the owner keeps in a richer form than its consumer reads, such
	// as an SMTP URL whose password is the only secret part.
	Transform ValueTransform
}

// TenantPlaceholder in an InfisicalPath is replaced by the box's tenant id.
const TenantPlaceholder = "{tenant}"

// ValueTransform derives the value uploaded to Infisical from the source value.
type ValueTransform string

const (
	// TransformNone uploads the source value unchanged.
	TransformNone ValueTransform = ""
	// TransformURLPassword uploads the password component of a URL,
	// e.g. smtps://user:<password>@smtp.example.com:465.
	TransformURLPassword ValueTransform = "url-password"
)

// CLISecretMappings is the registry of CLI and Bootstrap secrets to upload to Infisical
//
// ┌─────────────────────────────────────────────────────────────────────────┐
// │ BOOTSTRAP SECRETS (Operator creates, uploads to Infisical)             │
// ├─────────────────────────────────────────────────────────────────────────┤
// │ 1. infisical-db-username      → Infisical database user               │
// │ 2. infisical-db-password      → Infisical database user               │
// ├─────────────────────────────────────────────────────────────────────────┤
// │ CLI-INJECTED SECRETS (CLI creates, operator uploads)                   │
// ├─────────────────────────────────────────────────────────────────────────┤
// │ 5. hetzner-dns                → Used by external-dns, cert-manager     │
// │ 6. ghcr-pull-secret           → Used by mcp-server (image pulls)       │
// │ 7. github-username            → Used by ArgoCD (repo access)           │
// │ 8. github-token               → Used by ArgoCD (repo access)           │
// └─────────────────────────────────────────────────────────────────────────┘
//
// ┌─────────────────────────────────────────────────────────────────────────┐
// │ APPLICATION SECRETS (see ApplicationSecretMappings below)               │
// ├─────────────────────────────────────────────────────────────────────────┤
// │ • control-plane-db-credentials  → Uploaded directly to Infisical       │
// │ • hub-db-credentials            → Uploaded directly to Infisical       │
// └─────────────────────────────────────────────────────────────────────────┘
var CLISecretMappings = []SecretMapping{
	// ========================================================================
	// BOOTSTRAP SECRETS (Operator creates in Phase 1)
	// ========================================================================

	// ========================================================================
	// 1. INFISICAL-DB USERNAME (Infisical Bootstrap)
	// ========================================================================
	// Purpose: Infisical database user credentials
	// Source: infisical-db-credentials (generated by operator in Phase 1)
	// Consumers: Infisical deployment
	{
		SourceNamespace: NamespaceData,              // platform-data
		SourceName:      "infisical-db-credentials", // infisical-db-credentials
		SourceKey:       "username",                 // username
		InfisicalKey:    "infisical-db-username",    // infisical-db-username
		Description:     "Infisical DB username (Infisical bootstrap)",
	},

	// ========================================================================
	// 2. INFISICAL-DB PASSWORD (Infisical Bootstrap)
	// ========================================================================
	{
		SourceNamespace: NamespaceData,              // platform-data
		SourceName:      "infisical-db-credentials", // infisical-db-credentials
		SourceKey:       "password",                 // password
		InfisicalKey:    "infisical-db-password",    // infisical-db-password
		Description:     "Infisical DB password (Infisical bootstrap)",
	},

	// ========================================================================
	// CLI-INJECTED SECRETS (Deployed by CLI/ClusterResourceSet)
	// ========================================================================

	// ========================================================================
	// 5. HETZNER DNS API TOKEN
	// ========================================================================
	// Purpose: DNS management for external-dns and cert-manager ACME DNS-01
	// Source: hcloud secret (deployed by CLI via ClusterResourceSet)
	// Consumers: external-dns, cert-manager-webhook-hetzner
	// ExternalSecret: manifests/platform-external-dns/hetzner-dns-externalsecret.yaml
	{
		SourceNamespace: NamespaceCloudSystem, // kube-system
		SourceName:      "hetzner",            // Day-0 secret name
		SourceKey:       "hcloud",             // Key in the secret
		InfisicalKey:    "hcloud-token",       // Unified SOT key in Infisical
		Description:     "Hetzner API token",
	},

	// ========================================================================
	// 5b. EXTERNALLY-ISSUED CREDENTIALS (seeded from k8-secrets/)
	// ========================================================================
	// Purpose: credentials issued by systems OUTSIDE the cluster, which nothing
	// here can generate. They previously had no producer at all: a rebuilt
	// Infisical came up without them, their ExternalSecrets reported only
	// "could not get secret data from provider", and the consuming pods sat in
	// CreateContainerConfigError. Seeding was folklore — an operator had to know
	// which keys to paste into the console.
	// Source: created by scripts/hub-bootstrap.sh (step 6b) from k8-secrets/,
	// which is gitignored. A key whose file is absent or empty is never turned
	// into a Secret, so it is skipped here rather than uploaded blank.
	{
		SourceNamespace: NamespaceOps,
		SourceName:      "s3-object-storage",
		SourceKey:       "access-key-id",
		InfisicalKey:    "S3_ACCESS_KEY_ID",
		Description:     "Hetzner Object Storage access key (CNPG backups, Loki log storage)",
	},
	{
		SourceNamespace: NamespaceOps,
		SourceName:      "s3-object-storage",
		SourceKey:       "secret-access-key",
		InfisicalKey:    "S3_SECRET_ACCESS_KEY",
		Description:     "Hetzner Object Storage secret key (CNPG backups, Loki log storage)",
	},
	{
		SourceNamespace: NamespaceOps,
		SourceName:      "grafana-cloud",
		SourceKey:       "irm-alertmanager-url",
		InfisicalKey:    "GRAFANA_IRM_ALERTMANAGER_URL",
		Description:     "Grafana IRM Alertmanager integration URL (critical and warning alerts)",
	},
	{
		SourceNamespace: NamespaceOps,
		SourceName:      "grafana-cloud",
		SourceKey:       "irm-heartbeat-url",
		InfisicalKey:    "GRAFANA_IRM_HEARTBEAT_URL",
		Description:     "Grafana IRM heartbeat URL (the alert path's dead-man's switch)",
	},

	// ========================================================================
	// 5b. THE HUB GRAFANA'S ADMIN CREDENTIAL
	// ========================================================================
	// grafana-operator generates it and owns the Secret (ownerReferences:
	// Grafana/platform), which is why it is published from the cluster rather
	// than generated here -- the same shape as the CNPG-created database
	// credentials above.
	//
	// It is published because a credential that exists only in a Kubernetes
	// Secret has no access path but `kubectl get secret | base64 -d`: no
	// per-person access, no audit of who read it, and nothing to revoke. ADR-003
	// makes Infisical the system of record for exactly that reason, and this
	// credential is the only way into the hub's Grafana until the OIDC route of
	// ADR-078 add.5 §7 arrives.
	//
	// Deleting the Secret rotates it: grafana-operator regenerates, and the next
	// reconcile republishes the new value here.
	// ========================================================================
	// 5c. THE LOGIN SERVICE'S SIGNING KEYPAIR
	// ========================================================================
	// Published from the Secret the chart generated, so the material Infisical
	// holds is the pair Zitadel ALREADY trusts. Switching the chart to the
	// managed Secret then rotates nothing: no new key, no second rollout, no
	// session disruption.
	//
	// Why it is published at all: ArgoCD renders with `helm template`, where
	// Helm's `lookup` always returns empty, so the chart's reuse branch could
	// never fire and every sync minted a new keypair (2026-10-30, AUTH-7fs1e for
	// five hours). The platform owns this credential now.
	//
	// The source Secret disappears once login.loginServiceKeySecretName points
	// elsewhere and the chart stops rendering it. That is not a problem: the
	// uploader skips a source Secret that does not exist, and Infisical keeps
	// what was published.
	{
		SourceNamespace: NamespaceIdentity,
		SourceName:      "zitadel-login-service-key",
		SourceKey:       "tls.crt",
		InfisicalKey:    "hub-zitadel-login-service-cert",
		Description:     "Zitadel login service certificate (the pair the issuer trusts)",
	},
	{
		SourceNamespace: NamespaceIdentity,
		SourceName:      "zitadel-login-service-key",
		SourceKey:       "tls.key",
		InfisicalKey:    "hub-zitadel-login-service-key",
		Description:     "Zitadel login service private key (the pair the issuer trusts)",
	},

	{
		SourceNamespace: NamespaceObservability,
		SourceName:      "platform-admin-credentials",
		SourceKey:       "GF_SECURITY_ADMIN_USER",
		InfisicalKey:    "hub-grafana-admin-username",
		Description:     "Hub Grafana admin username (grafana-operator generated)",
	},
	{
		SourceNamespace: NamespaceObservability,
		SourceName:      "platform-admin-credentials",
		SourceKey:       "GF_SECURITY_ADMIN_PASSWORD",
		InfisicalKey:    "hub-grafana-admin-password",
		Description:     "Hub Grafana admin password (grafana-operator generated)",
	},

	// ========================================================================
	// 6. GITHUB CONTAINER REGISTRY PULL SECRET
	// ========================================================================
	// Purpose: Pull private images from ghcr.io
	// Source: ghcr-pull-secret (deployed by CLI/manually in platform-ops)
	// Consumers: mcp-server, other private image deployments
	// ExternalSecret: manifests/api-gateway/mcp-server/ghcr-pull-secret-externalsecret.yaml
	{
		SourceNamespace: NamespaceOps,        // platform-ops
		SourceName:      "ghcr-pull-secret",  // ghcr-pull-secret
		SourceKey:       ".dockerconfigjson", // .dockerconfigjson
		InfisicalKey:    "ghcr-pull-secret",  // ghcr-pull-secret
		Description:     "GitHub Container Registry pull secret",
	},

	// ========================================================================
	// 7. GITHUB USERNAME (for ArgoCD)
	// ========================================================================
	// Purpose: GitHub authentication for ArgoCD repository access
	// Source: platform-git-secret (deployed by CLI/bootstrap)
	// Consumers: ArgoCD
	// ExternalSecret: manifests/argocd/argocd-github-creds-externalsecret.yaml
	{
		SourceNamespace: NamespaceOps,          // platform-ops
		SourceName:      "platform-git-secret", // platform-git-secret
		SourceKey:       "username",            // username
		InfisicalKey:    "github-username",     // github-username
		Description:     "GitHub username for ArgoCD",
	},

	// ========================================================================
	// 8. GITHUB TOKEN (for ArgoCD)
	// ========================================================================
	// Purpose: GitHub authentication token for ArgoCD repository access
	// Source: platform-git-secret (deployed by CLI/bootstrap)
	// Consumers: ArgoCD
	// ExternalSecret: manifests/argocd/argocd-github-creds-externalsecret.yaml
	{
		SourceNamespace: NamespaceOps,          // platform-ops
		SourceName:      "platform-git-secret", // platform-git-secret
		SourceKey:       "password",            // password
		InfisicalKey:    "github-token",        // github-token
		Description:     "GitHub token for ArgoCD",
	},

	// ========================================================================
	// ADD NEW BOOTSTRAP/CLI SECRETS BELOW THIS LINE
	// ========================================================================

	// ========================================================================
	// 12. TAILSCALE AUTHKEY (hybrid spoke CP tailnet join)
	// ========================================================================
	// Purpose: Tailscale auth key for hybrid spoke control-plane nodes (ADR-046)
	// Source: tailscale-hybrid-psk (created by `hub configure-tailscale` from
	//        k8-secrets/tailscale/authkey)
	// Consumers: shared ClusterClass Tailscale pre-kubeadm hook (contentFrom.secret)
	// ExternalSecret: manifests/providers/hybrid/k8s/tailscale-psk-es.yaml
	{
		SourceNamespace: NamespaceCAPI,          // platform-capi
		SourceName:      "tailscale-hybrid-psk", // tailscale-hybrid-psk
		SourceKey:       "authkey",              // authkey
		InfisicalKey:    KeyTailscaleAuthkey,    // tailscale-authkey
		Description:     "Tailscale auth key for hybrid spoke CP",
	},

	// ========================================================================
	// 13. TAILSCALE HOSTNAME (hybrid spoke CP tailnet hostname)
	// ========================================================================
	{
		SourceNamespace: NamespaceCAPI,          // platform-capi
		SourceName:      "tailscale-hybrid-psk", // tailscale-hybrid-psk
		SourceKey:       "hostname",             // hostname
		InfisicalKey:    KeyTailscaleHostname,   // tailscale-hostname
		Description:     "Tailscale hostname for hybrid spoke CP",
	},

	// ========================================================================
	// Template:
	// {
	//     SourceNamespace: "namespace-where-secret-exists",
	//     SourceName:      "k8s-secret-name",
	//     SourceKey:       "key-in-secret-data",
	//     InfisicalKey:    "key-in-infisical",
	//     Description:     "Human readable description",
	// },
	{
		// The identity service's credential for managing the issuer.
		//
		// Uploaded rather than read directly because the two live in different
		// namespaces: the issuer's setup creates this in platform-identity, and
		// the service that needs it runs in platform-ops. A Kubernetes Secret
		// cannot be read across that boundary, and copying one between namespaces
		// is the secret traversal ADR-041 prohibits outright.
		//
		// Infisical is the sanctioned path: the operator uploads, ESO delivers.
		// That keeps a single System of Record for the credential and leaves the
		// copy where the platform can rotate it.
		SourceNamespace: NamespaceIdentity,
		SourceName:      "iam-admin-pat",
		SourceKey:       "pat",
		InfisicalKey:    "hub-identity-service-token",
		Description:     "Identity service credential for provisioning tenants at the issuer",
	},

	// ========================================================================
	// ZITADEL SMTP PASSWORD (box owner's credential, ADR-060 amendment)
	// ========================================================================
	// The issuer sends verification and password-reset mail through the box
	// owner's own sending service (Resend). Only the password is secret; host,
	// user and sender are in the Zitadel values. The owner keeps the whole SMTP
	// URL in k8-secrets/smtp/resend (or ZITADEL_SMTP_URL), hub-bootstrap step 6b
	// turns it into this Secret, and the password component is uploaded to the
	// owner's /<tenant>-mgmt folder, where zitadel-smtp-config reads it through
	// the infisical-tenant-mgmt store.
	{
		SourceNamespace: NamespaceOps,
		SourceName:      "zitadel-smtp",
		SourceKey:       "resend",
		InfisicalKey:    "zitadel_smtp_password",
		InfisicalPath:   "/" + TenantPlaceholder + "-mgmt",
		Transform:       TransformURLPassword,
		Description:     "Zitadel SMTP password (Resend), from the owner's SMTP URL",
	},
	// ADR-078 add.5 §5: every cluster's Alertmanager mails through the same
	// outbound account. Uploaded again at the root, where the hub's Alertmanager
	// reads it and from where FleetCredentialKeys copies it into each cell.
	{
		SourceNamespace: NamespaceOps,
		SourceName:      "zitadel-smtp",
		SourceKey:       "resend",
		InfisicalKey:    "ALERT_SMTP_PASSWORD",
		Transform:       TransformURLPassword,
		Description:     "Outbound-mail password for every cluster's Alertmanager, from the owner's SMTP URL",
	},
}

// ============================================================================
// APPLICATION SECRET DEFINITIONS
// ============================================================================
// Application secrets are generated by the operator and uploaded directly to Infisical
// (never created in K8s by the operator). ESO creates them in K8s with creationPolicy: Owner.

// ApplicationSecretDefinition defines an application secret to create in Infisical
type ApplicationSecretDefinition struct {
	UsernameKey string // Infisical key for username (e.g., "control-plane-db-username") - empty for non-DB secrets
	PasswordKey string // Infisical key for password/secret (e.g., "control-plane-db-password")
	Username    string // Username value (e.g., "mcp_server") - empty for non-DB secrets
	Description string // Human-readable description

	// HexBytes, when > 0, generates this many random bytes hex-encoded instead of a
	// password from the URL-safe charset. Required by consumers that hex-decode the
	// value and demand an exact key length; for them the default charset produces a
	// string of the right length that still fails to parse.
	HexBytes int

	// KeyType, when non-empty, triggers asymmetric key pair generation instead of
	// password generation. Supported values: "ed25519". When KeyType is set,
	// PrivateKeyKey and PublicKeyKey specify the two Infisical keys for the pair.
	// PasswordKey and UsernameKey are ignored for key-pair entries.
	KeyType string

	// PrivateKeyKey is the Infisical key for the PEM-encoded private key
	// (only used when KeyType != "")
	PrivateKeyKey string

	// PublicKeyKey is the Infisical key for the base64-encoded public key
	// (only used when KeyType != "")
	PublicKeyKey string

	// Selector is the DKIM selector (e.g., "s1") — configuration/identity, not
	// secret material. Stored in the mapping for downstream components to consume.
	// Only used when KeyType != ""
	Selector string

	// EscrowArtifact, when non-empty, names the escrow artefact this value is kept
	// under outside the box (ADR-076).
	//
	// Set for a value that CANNOT BE REGENERATED WITHOUT LOSS -- a key something
	// was encrypted with, as opposed to a credential that can be rotated. That is
	// the membership test ADR-076 states, and it is the whole of why this field
	// exists: for a rotatable secret an escrow is redundant, and for an unrotatable
	// one its absence is undetectable until a recovery.
	//
	// Setting it changes three things about how the value is handled:
	//
	//   generation      the escrow is consulted FIRST. A rebuilt box adopts the
	//                   escrowed value instead of minting a new one, which is the
	//                   only reason the escrow is worth having.
	//   absence         generation is REFUSED when no escrow is reachable, rather
	//                   than producing a value that exists nowhere else.
	//   malformed       repair by regeneration is REFUSED. For a rotatable secret
	//                   regenerating a malformed value is a fix; here it destroys
	//                   what the value protected.
	EscrowArtifact string

	// CellScopedKey, when non-empty, ALSO produces this secret once per SpokePool at
	// /spoke-pool/<cellId>/shared under this key name, with an independently
	// generated value. The root entry is still uploaded — it is not diverted.
	//
	// Both halves are load-bearing and they are not the same secret. A spoke's
	// SecretStore is authorised for its own prefix ONLY (ADR-031), so a value at the
	// root is unreadable from a spoke however correct it is: that is why the
	// agentgateway cookie secret existed at the root and the spoke ExternalSecret
	// still reported "could not get secret data from provider". Meanwhile the HUB
	// agentgateway reads the root key, so removing it takes the hub gateway down.
	//
	// The values MUST NOT be shared across cells (ADR-050 P1-4): this signs OIDC
	// session cookies, and one key across the fleet would make a session minted on
	// one spoke valid on every other.
	//
	// Note the two spellings are deliberate, matching each consumer's ExternalSecret:
	// kebab-case at the root, SCREAMING_SNAKE in the cell path.
	CellScopedKey string
}

// ApplicationSecretMappings defines all application secrets to create in Infisical
// These secrets are created directly in Infisical (not in K8s first)
// ESO will sync them to K8s with creationPolicy: Owner
//
// ┌─────────────────────────────────────────────────────────────────────────┐
// │ APPLICATION SECRETS (Operator uploads to Infisical → ESO creates in K8s)│
// ├─────────────────────────────────────────────────────────────────────────┤
// │ 1. control-plane-db-credentials  → mcp_server database user            │
// │ 2. hub-db-credentials            → centralized hub database user       │
// └─────────────────────────────────────────────────────────────────────────┘
var ApplicationSecretMappings = []ApplicationSecretDefinition{
	{
		UsernameKey: "hub-control-plane-db-username",
		PasswordKey: "hub-control-plane-db-password",
		Username:    "hub_control_plane",
		Description: "Control Plane DB credentials (mcp-server)",
	},

	// hub_centralized is declared in HubEnvironment.spec.database.roles and its
	// credential secret (hub-db-credentials) is named by mapRoleToSecretName, but
	// this table had no entry for it — so nothing ever produced
	// hub-centralized-db-username/password. The gap stayed invisible while the
	// values happened to exist in Infisical from an earlier bootstrap; once that
	// store was reinitialised they were gone for good, the ExternalSecret failed
	// with "could not get secret data from provider", and RoleManager could not
	// create the role. Because role provisioning is all-or-nothing, one missing
	// producer left every hub_* role absent and the whole auth tier in init.
	{
		UsernameKey: "hub-centralized-db-username",
		PasswordKey: "hub-centralized-db-password",
		Username:    "hub_centralized",
		Description: "Centralized hub DB credentials",
	},
	{
		// The identity provider's database credential (ADR-060). The Ory entries
		// that stood beside this one were removed with their stack.
		UsernameKey: "hub-zitadel-db-username",
		PasswordKey: "hub-zitadel-db-password",
		Username:    "hub_zitadel",
		Description: "Zitadel DB credentials",
	},
	{
		// The Credit Authority's ledger role (ADR-109). Generated on every box:
		// the role is a platform prerequisite whether or not the capability runs,
		// because RoleManager stops at the first role whose credential is missing.
		UsernameKey: "hub-credits-db-username",
		PasswordKey: "hub-credits-db-password",
		Username:    "hub_credits",
		Description: "Credit Authority ledger DB credentials",
	},
	{
		// The billing engine's database role (ADR-113): the initdb owner of
		// billing-db, which reads it at creation. Generated on every box, for
		// the reason hub_credits is: its cluster exists whether or not the
		// capability runs, and cannot initialise without it.
		UsernameKey: "hub-billing-db-username",
		PasswordKey: "hub-billing-db-password",
		Username:    "hub_billing",
		Description: "Billing engine DB credentials",
	},
	{
		// The hub Grafana's state and sessions (ADR-078 add.5 §6): Grafana's HA
		// model keeps both in a shared database. Generated on every box with the
		// other roles, for the same all-or-nothing reason as hub_credits.
		UsernameKey: "hub-grafana-db-username",
		PasswordKey: "hub-grafana-db-password",
		Username:    "hub_grafana",
		Description: "Hub Grafana DB credentials",
	},
	{
		// Zitadel's masterkey encrypts everything it stores at rest, and
		// checkMasterKeyLength compares len() of the RAW STRING against 32 —
		// it does not decode it. So what matters is the character count.
		//
		// HexBytes is required here, and 16 is not a typo: GenerateHexKey(16)
		// emits 16 random bytes hex-encoded, which is exactly 32 CHARACTERS.
		// Without HexBytes this entry falls to the default branch, which gives
		// a system secret (UsernameKey == "") 64 characters and fails at
		// startup with "masterkey must be 32 bytes, but is 64" — a message
		// that says bytes while measuring characters, so the arithmetic only
		// works out if you already know it never decodes.
		//
		// Generated once and never rotated in place: re-keying would orphan
		// every encrypted column Zitadel has written.
		UsernameKey: "",
		PasswordKey: "hub-zitadel-masterkey",
		Username:    "",
		HexBytes:    16,
		Description: "Zitadel masterkey for encryption at rest",
		// Escrowed because it is never rotated: see EscrowArtifact, and ADR-076's
		// membership test. Without this the box's secret store is recoverable and
		// its identity data is not.
		EscrowArtifact: escrow.ArtifactZitadelMasterkey,
	},
	// The API server's at-rest encryption key is NOT in this registry, and that
	// absence is a decision (2026-10-04).
	//
	// It was added here on 2026-10-02 and removed two days later without ever
	// having encrypted anything, because it created a SECOND WRITER for a value
	// that must have exactly one. This registry writes to the Infisical project
	// ROOT -- one fleet-wide value for every cluster -- while ADR-100 decides the
	// encryption key is PER CLUSTER, held in the escrow and scoped by cluster id.
	// Both paths existed, neither knew about the other, and on this box they had
	// already diverged to two different keys.
	//
	// Had a control plane rolled in that state, it would have encrypted with one
	// key while the ExternalSecret reasserted the other, and everything written in
	// between would have become unreadable at the next restart.
	//
	// The encryption key's lifecycle is therefore entirely outside this registry:
	// the escrow is the recovery source, `kakt encryption enable` and the Day-0
	// provisioner are the only writers, and there is no Infisical copy to disagree
	// with them (ADR-003 section 6, ADR-100).
	// AgentGateway's OIDC session cookie encryption key. It belongs here because
	// it is a random value with no external source, so leaving it out of this
	// registry made it the one cookie secret an operator had to seed by hand into
	// a freshly rebuilt Infisical — and nothing said so. The symptom was
	// agentgateway stuck in CreateContainerConfigError while its ExternalSecret
	// reported only "could not get secret data from provider".
	// Consumed as OIDC_COOKIE_SECRET via ExternalSecret
	// manifests/hub-core-services/api-gateway/oidc-cookie-secret-es.yaml
	// (creationPolicy: Owner), which needs no change.
	{
		UsernameKey: "",
		PasswordKey: "agentgateway-oidc-cookie-secret",
		Username:    "",
		// AES-256-GCM: agentgateway hex-decodes this and requires exactly 32 bytes.
		// A 44-character base64 value of the same 32 bytes is the wrong ENCODING and
		// fails at startup with `Invalid character 'Z' at position 1`, which reads as
		// a corrupt secret rather than a format mismatch. HexBytes is what keeps the
		// generated form and the consumer's parser in agreement.
		HexBytes: 32,
		// agentgateway runs on the hub AND on every spoke. The hub reads the root key
		// above; each spoke reads its own cell copy under the name below.
		CellScopedKey: "AGENTGATEWAY_OIDC_COOKIE_SECRET",
		Description:   "AgentGateway OIDC session cookie encryption secret (32-byte AES-256-GCM key, hex)",
	},
}
