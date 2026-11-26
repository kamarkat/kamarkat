package secrets

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/kamarkat/kamarkat/operators/hub-operator/internal/openbao"
)

// Store is hub-operator's view of the box's secret store (ADR-117): KV v2 in
// OpenBao, one secret per scope, each value a key in it (openbao.ScopeHub,
// openbao.SpokePlatformScope, openbao.TenantScope). Every generator here writes a
// value once and never regenerates it (ADR-003 §1).
type Store struct {
	Bao *openbao.Client
	// RestoreGuard, when set, is asked before ANY value is generated. It refuses
	// while the store is an unfinished restore (ADR-117 §7): a value missing from
	// a restored store was lost, not never-created, and a regenerated one would
	// silently replace what consumers and external integrations still use.
	RestoreGuard func(context.Context) error
}

// GenerationAllowed reports whether a new value may be generated now. Every
// generator calls it, including the in-process key-pair generators.
func (c *Store) GenerationAllowed(ctx context.Context) error {
	if c.RestoreGuard == nil {
		return nil
	}
	return c.RestoreGuard(ctx)
}

// NewStore wraps an OpenBao client.
func NewStore(c *openbao.Client) *Store { return &Store{Bao: c} }

// HubScope is where the hub's own platform and application secrets live.
const HubScope = openbao.ScopeHub

// GetSecret returns scope's value for key; an error wrapping openbao.ErrNotFound when absent.
func (c *Store) GetSecret(ctx context.Context, scope, key string) (string, error) {
	return c.Bao.GetSecret(ctx, scope, key)
}

// SecretExists distinguishes absent from unreadable.
func (c *Store) SecretExists(ctx context.Context, scope, key string) (bool, error) {
	return c.Bao.SecretExists(ctx, scope, key)
}

// CreateSecret writes key only if absent. A concurrent writer having just created
// it is success: the value in place is the one in use.
func (c *Store) CreateSecret(ctx context.Context, scope, key, value string) error {
	err := c.Bao.CreateSecret(ctx, scope, key, value)
	if errors.Is(err, openbao.ErrExists) {
		return nil
	}
	return err
}

// EnsureTenantAuth authorizes one tenant application's namespace (openbao.EnsureTenantAuth).
func (c *Store) EnsureTenantAuth(ctx context.Context, cluster, tenantID, app, uid string) error {
	return c.Bao.EnsureTenantAuth(ctx, cluster, tenantID, app, uid)
}

// RemoveTenantAuth withdraws one tenant application's authority (openbao.RemoveTenantAuth).
func (c *Store) RemoveTenantAuth(ctx context.Context, cluster, app, uid string) error {
	return c.Bao.RemoveTenantAuth(ctx, cluster, app, uid)
}

// GetSecretValue is GetSecret reporting absence as found=false rather than an error.
func (c *Store) GetSecretValue(ctx context.Context, scope, key string) (string, bool, error) {
	v, err := c.Bao.GetSecret(ctx, scope, key)
	if errors.Is(err, openbao.ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

// Password policies defined at OpenBao initialisation (values.yaml, self-init
// "password-policies"). Each fixes an alphabet and a length; the value is drawn
// by OpenBao. Hex policies serve consumers that hex-decode a key of exact size.
const (
	policyPassword = "hex-32" // the default generated credential: 128 bits, hex
)

// Password returns a new default credential (32 hex characters) from OpenBao.
func (c *Store) Password(ctx context.Context) (string, error) {
	if err := c.GenerationAllowed(ctx); err != nil {
		return "", err
	}
	return c.Bao.GeneratePassword(ctx, policyPassword)
}

// HexKey returns nbytes of randomness, hex-encoded (2*nbytes characters), from
// the matching OpenBao password policy.
func (c *Store) HexKey(ctx context.Context, nbytes int) (string, error) {
	if err := c.GenerationAllowed(ctx); err != nil {
		return "", err
	}
	switch nbytes {
	case 16, 32:
		return c.Bao.GeneratePassword(ctx, fmt.Sprintf("hex-%d", 2*nbytes))
	case 64:
		// OpenBao password policies stop at 100 characters, so 128 hex characters
		// are two independent hex-64 draws.
		var key string
		for range 2 {
			half, err := c.Bao.GeneratePassword(ctx, "hex-64")
			if err != nil {
				return "", err
			}
			key += half
		}
		return key, nil
	}
	return "", fmt.Errorf("no OpenBao password policy yields a %d-byte hex key (16, 32 or 64)", nbytes)
}

// URLSafe returns length characters from [A-Za-z0-9-._~], from OpenBao.
func (c *Store) URLSafe(ctx context.Context, length int) (string, error) {
	if err := c.GenerationAllowed(ctx); err != nil {
		return "", err
	}
	switch length {
	case 32, 64:
		return c.Bao.GeneratePassword(ctx, fmt.Sprintf("urlsafe-%d", length))
	}
	return "", fmt.Errorf("no OpenBao password policy yields %d URL-safe characters (urlsafe-32, urlsafe-64)", length)
}

// UpdateSecret sets key unconditionally (an authority converging a copy).
func (c *Store) UpdateSecret(ctx context.Context, scope, key, value string) error {
	return c.Bao.UpdateSecret(ctx, scope, key, value)
}

// DeleteSecret removes key from scope.
func (c *Store) DeleteSecret(ctx context.Context, scope, key string) error {
	return c.Bao.DeleteSecret(ctx, scope, key)
}

type EnsureResult int

const (
	EnsureCreated EnsureResult = iota
	EnsureAlreadyExists
	EnsureMissing
	// EnsureRotated indicates stored credentials were found to be invalid
	// (401) and were rotationally self-healed by the operator.
	EnsureRotated
	// EnsureSkipped indicates the credential was not applicable — the feature
	// that needs it is not enabled for this tenant. Distinct from
	// EnsureAlreadyExists, which asserts the credential is present: a caller
	// recording "seeded" must not do so for a credential nobody asked for.
	//
	// Appended last on purpose: these are iota values, and inserting above would
	// silently renumber every existing outcome.
	EnsureSkipped
)

// EnsureTenantCredentialsResult is the outcome of EnsureTenantFolderAndCredentials.
type EnsureTenantCredentialsResult struct {
	Result       EnsureResult
	OAuthOutcome EnsureResult
	// CacheOutcome is reported separately so the controller can record that THIS
	// credential has been seeded. Folding it into Result made the cache
	// indistinguishable from the tenant's other credentials, which is what let a
	// per-tenant marker stand in for a per-credential one.
	CacheOutcome EnsureResult
}

// OAuthClient is a declared OAuth client, reduced to what the credential producer
// needs (ADR-053). Grant types, scopes and redirect URIs are absent because they
// belong to the registration controller and never reach this boundary.
type OAuthClient struct {
	// Name is unique within the tenant. The client is registered under
	// "<tenantId>-<Name>".
	Name string

	// Confidential is true when the client authenticates with a secret. A public
	// client has no credential to generate, store or rotate.
	Confidential bool

	// PreviouslyProvisioned records that this client's credential has been
	// generated before, taken from the tenant resource's observed status. It
	// governs whether a missing credential is generated or reported as a fault,
	// and is per client rather than per tenant: a client declared today on an
	// existing tenant is new, not lost.
	PreviouslyProvisioned bool
}

// EnsureCrossplaneAdminPassword provisions the per-spoke crossplane_admin
// password that backs platform bootstrap (CNPG managed role + provider-sql
// ProviderConfig) on the spoke. The spoke ESO ExternalSecret
// (crossplane-admin-eso.yaml) reads exactly this key from the same secrets
// project, so the key and path MUST stay aligned with the AppSet-injected
// remoteRef (see platform-spoke-catalog AppSet kustomize patch).
//
// Idempotency contract:
//   - If <spoke>-crossplane-admin-password exists at root path → AlreadyExists.
//   - If missing AND isFirstTime → generate a secure password and upload.
//   - If missing AND !isFirstTime → Missing (manual recovery required; the
//     password was deleted after provisioning and the spoke CNPG cannot
//     silently pick up a regenerated one without coordinated bootstrap).
func (c *Store) EnsureCrossplaneAdminPassword(ctx context.Context, spokeName string, isFirstTime bool) (EnsureResult, error) {
	logger := log.FromContext(ctx)

	logger.Info("Ensuring crossplane-admin password", "spoke", spokeName)
	scope, key := openbao.SpokePlatformScope(spokeName), CrossplaneAdminPasswordKey
	exists, err := c.SecretExists(ctx, scope, key)
	if err != nil {
		logger.Error(err, "Failed to check OpenBao for existing crossplane-admin password", "spoke", spokeName, "key", key)
		return EnsureMissing, fmt.Errorf("failed to check OpenBao for crossplane-admin password: %w", err)
	}

	if exists {
		logger.Info("Crossplane-admin password already exists in OpenBao, skipping generation", "spoke", spokeName, "key", key)
		return EnsureAlreadyExists, nil
	}

	if !isFirstTime {
		logger.Error(nil, "CRITICAL: crossplane-admin password missing from OpenBao but SpokePool was already provisioned. Manual recovery required.",
			"spoke", spokeName, "key", key)
		return EnsureMissing, fmt.Errorf("crossplane-admin password missing from OpenBao for already-provisioned SpokePool %s - manual recovery required", spokeName)
	}

	password, err := c.Password(ctx)
	if err != nil {
		logger.Error(err, "Failed to generate crossplane-admin password", "spoke", spokeName)
		return EnsureMissing, fmt.Errorf("failed to generate %s/%s: %w", scope, key, err)
	}

	logger.Info("Uploading crossplane-admin password to OpenBao", "spoke", spokeName, "key", key, "passwordLength", len(password))
	if err := c.CreateSecret(ctx, scope, key, password); err != nil {
		logger.Error(err, "Failed to upload crossplane-admin password to OpenBao", "spoke", spokeName, "key", key)
		return EnsureMissing, fmt.Errorf("failed to upload crossplane-admin password: %w", err)
	}

	logger.Info("Crossplane-admin password seeded in OpenBao", "spoke", spokeName, "key", key)
	return EnsureCreated, nil
}

// EnsureTenantFolderAndCredentials creates the tenant folder, syncs shared
// Machine Identity credentials into the tenant's path (for SDK identity resolution
// per ADR-019), generates DB credentials for the tenant database, and materialises
// the identifier and credential of each declared OAuth confidential client
// (ADR-053).
//
// The application's ESO identity is not a stored credential any more: it is the
// tenant namespace's ServiceAccount, logging in through the cluster's JWT mount
// (EnsureTenantAuth). Nothing here is copied between scopes.
//
// Idempotency contract (ADR-003):
//   - If the database password exists in the tenant scope → AlreadyExists.
//   - If missing AND isFirstTime → generate it, write it, return Created.
//   - If missing AND !isFirstTime → return Missing (manual intervention required).
func (c *Store) EnsureTenantFolderAndCredentials(ctx context.Context, clusterName, tenantId, appId string, isFirstTime bool, oauthClients []OAuthClient, cacheEnabled, cacheIsFirstTime, gatewayEnabled, privateImages bool) (*EnsureTenantCredentialsResult, error) {
	logger := log.FromContext(ctx).WithValues("tenant", tenantId, "app", appId, "cluster", clusterName)
	tenantPath := openbao.TenantScope(clusterName, appId)

	// Declared with the other outcomes so every early return reports it. A
	// tenant with no cache reports Skipped, never AlreadyExists — the controller
	// must not record "seeded" for a credential nobody asked for.
	cacheOutcome := EnsureSkipped

	// The box's registry credential, copied into the tenant's own scope ONLY when
	// the application declares private images, and withdrawn when it does not: a
	// public application holds no registry credential at all. Centrally rotated:
	// a changed hub value converges here.
	if privateImages {
		if pull, err := c.GetSecret(ctx, HubScope, RegistryPullSecretKey); err == nil {
			if err := c.UpdateSecret(ctx, tenantPath, RegistryPullSecretKey, pull); err != nil {
				return nil, fmt.Errorf("copy %s into tenant %s: %w", RegistryPullSecretKey, tenantId, err)
			}
		} else if !errors.Is(err, openbao.ErrNotFound) {
			return nil, fmt.Errorf("read %s: %w", RegistryPullSecretKey, err)
		}
	} else if err := c.DeleteSecret(ctx, tenantPath, RegistryPullSecretKey); err != nil && !errors.Is(err, openbao.ErrNotFound) {
		return nil, fmt.Errorf("withdraw %s from tenant %s: %w", RegistryPullSecretKey, tenantId, err)
	}

	dbSecretName := TenantDBPasswordKey
	dbExists, err := c.SecretExists(ctx, tenantPath, dbSecretName)
	if err != nil {
		logger.Error(err, "Failed to check OpenBao for db-credentials", "path", tenantPath)
		return nil, fmt.Errorf("failed to check OpenBao for %s: %w", dbSecretName, err)
	}

	dbOutcome := EnsureAlreadyExists
	switch {
	case dbExists:
		logger.Info("DB credentials already exist in OpenBao, skipping generation", "path", tenantPath, "secret", dbSecretName)

	case !isFirstTime:
		// Absence after first provisioning is a fault, never a trigger to
		// regenerate: a credential that was never created cannot be told apart
		// from one that was lost, and only the first is safe to replace.
		logger.Error(nil, "CRITICAL: DB credentials missing from OpenBao for already-provisioned tenant. Manual intervention required.", "path", tenantPath)
		return &EnsureTenantCredentialsResult{
			Result:       EnsureMissing,
			CacheOutcome: cacheOutcome,
		}, fmt.Errorf("db-credentials missing from OpenBao for already-provisioned tenant %s — manual recovery required", tenantId)

	default:
		password, err := c.Password(ctx)
		if err != nil {
			logger.Error(err, "Failed to generate password for tenant")
			return nil, fmt.Errorf("failed to generate %s/%s for tenant %s: %w", tenantPath, dbSecretName, tenantId, err)
		}

		logger.Info("Generated database password for tenant", "passwordLength", len(password))

		// THE PASSWORD, AND NOTHING ELSE.
		//
		// This used to write a username too, built as "tenant-<tenantId>-user".
		// The role it named is created by the spoke composition from a DIFFERENT
		// axis, and nothing compared the two, so the credential named a role that
		// did not exist. The failure surfaced only as
		//
		//	psql: FATAL: password authentication failed for user "tenant-<tenant>-user"
		//
		// inside the tenant baseline migration Job, three systems away from either
		// half of the name, and its visible effect was a fleet with no users table.
		// It had passed for a year because the first tenant's tenantId WAS its
		// appId; two apps of one tenant would additionally have COLLIDED on one
		// role, since a postgres role is cluster-wide.
		//
		// The name now has exactly one definition -- spec.dbRoleName on the XR,
		// composed by the chart -- and the composition patches it into the
		// ExternalSecret's template. A username is a derived NAME, not a secret,
		// and it only lived here because it was generated beside a password.
		//
		// A key left over from before is simply unread: nothing fetches
		// `property: username` any more.
		//
		logger.Info("Uploading db-credentials to OpenBao", "path", tenantPath)
		if err := c.CreateSecret(ctx, tenantPath, dbSecretName, password); err != nil {
			logger.Error(err, "Failed to upload db-credentials to OpenBao", "path", tenantPath)
			return nil, fmt.Errorf("failed to push db-credentials for tenant %s: %w", tenantId, err)
		}
		logger.Info("Tenant DB credentials seeded in OpenBao", "path", tenantPath, "secret", dbSecretName)
		dbOutcome = EnsureCreated
	}

	// Step 3: cache credential.
	//
	// A password, not an ACL file. The file is rendered from it by the delivering
	// ExternalSecret's template, so the platform owns the ACL's shape in one place
	// rather than each fleet hand-writing a file whose format nothing validates.
	// Generated once under the same rule as everything else here: its later absence
	// is a fault, because regenerating locks out a running cache's clients.
	//
	// SEEDED WHATEVER cacheEnabled SAYS, and the parameter is kept only to report
	// the outcome. This was `if cacheEnabled`, and it disagreed with the other half
	// of the platform: the AINativeSaaS composition renders the cache Deployment,
	// Service, Certificate, NetworkPolicy and ExternalSecret for EVERY tenant. Its
	// own comment claims they are "rendered only when the fleet declares
	// spec.cache.enabled", but it is a classic Composition with no pipeline and no
	// conditional function, so nothing gates them and nothing ever did.
	//
	// The two halves therefore contradicted each other, and the contradiction was
	// not inert: a tenant with cache.enabled=false received a cache ExternalSecret
	// naming a CACHE_PASSWORD the operator refused to write, so it failed
	// SecretSyncedError forever and the cache Deployment never became ready. That
	// is oranger's state on 2026-09-30, and it is the state of every future tenant
	// that leaves the cache off.
	//
	// Seeding unconditionally is the cheap half of the fix and it is sound: the
	// platform itself always asks for this credential, so writing it is not
	// "seeding something nobody asked for". An unused password costs nothing.
	//
	// The other half -- actually gating those resources on spec.cache.enabled --
	// needs the composition converted to a pipeline, which is a larger change than
	// this file. Until then a tenant with the cache off gets a working cache it
	// does not use, which is waste rather than breakage.
	{
		exists, err := c.SecretExists(ctx, tenantPath, CachePasswordKey)
		if err != nil {
			return nil, fmt.Errorf("failed to check OpenBao for %s: %w", CachePasswordKey, err)
		}
		cacheOutcome = EnsureAlreadyExists
		if !exists {
			// PER-CREDENTIAL, not per-tenant. The tenant-wide isFirstTime is the
			// wrong question for this key, and asking it deadlocked waypoint on
			// 2026-09-02: the tenant's DB credentials survived a cluster rebuild
			// in OpenBao, so TenantDBCredentialsSeeded stayed true and every
			// reconcile thereafter concluded the tenant was already provisioned
			// — while CACHE_PASSWORD, a credential introduced AFTER that tenant
			// was first provisioned, had never been written at all. The operator
			// then refused to seed a key that had never existed, the cache
			// ExternalSecret stayed SecretSyncedError, and bff and the cache
			// could not start for want of a secret nothing was allowed to create.
			//
			// This is the same argument the OAuth clients above already make:
			// "a client declared today on a tenant provisioned months ago is
			// new, and treating its absent credential as a fault would make
			// declaring a second client impossible." A cache enabled today on a
			// tenant provisioned months ago is new in exactly that way.
			//
			// The protection is unchanged where it matters: once this credential
			// HAS been seeded, its later absence is still a fault requiring
			// manual recovery, because regenerating it locks out a running
			// cache's clients.
			if !cacheIsFirstTime {
				logger.Error(nil, "CRITICAL: cache credential missing for a tenant whose cache credential was already seeded. Manual recovery required.", "path", tenantPath)
				return &EnsureTenantCredentialsResult{
					Result:       dbOutcome,
					CacheOutcome: cacheOutcome,
				}, fmt.Errorf("%s missing from OpenBao for already-provisioned tenant %s — manual recovery required", CachePasswordKey, tenantId)
			}
			value, err := c.Password(ctx)
			if err != nil {
				return nil, fmt.Errorf("failed to generate %s/%s for tenant %s: %w", tenantPath, CachePasswordKey, tenantId, err)
			}
			if err := c.CreateSecret(ctx, tenantPath, CachePasswordKey, value); err != nil {
				return nil, fmt.Errorf("failed to write %s for tenant %s: %w", CachePasswordKey, tenantId, err)
			}
			logger.Info("Seeded tenant cache credential", "path", tenantPath,
				"key", CachePasswordKey, "cacheEnabled", cacheEnabled)
			dbOutcome = EnsureCreated
			cacheOutcome = EnsureCreated
		}
	}

	// Step 4: gateway session-cookie key.
	//
	// Written on absence whatever the tenant's age, like the OAuth credentials in
	// step 5 and for the same reason: enabling the gateway is a thing a tenant
	// does long after it is provisioned, and refusing to seed the key then would
	// make the feature unreachable for every tenant that already exists.
	//
	// The value this protects — never invalidating live sessions by replacing a
	// key in use — is held by the existence check alone. An absent key means no
	// gateway has ever started with one, so there is no session to lose.
	if gatewayEnabled {
		exists, err := c.SecretExists(ctx, tenantPath, GatewayCookieSecretKey)
		if err != nil {
			return nil, fmt.Errorf("failed to check OpenBao for %s: %w", GatewayCookieSecretKey, err)
		}
		if !exists {
			value, err := c.HexKey(ctx, 32)
			if err != nil {
				return nil, fmt.Errorf("failed to generate %s/%s for tenant %s: %w", tenantPath, GatewayCookieSecretKey, tenantId, err)
			}
			if err := c.CreateSecret(ctx, tenantPath, GatewayCookieSecretKey, value); err != nil {
				return nil, fmt.Errorf("failed to write %s for tenant %s: %w", GatewayCookieSecretKey, tenantId, err)
			}
			logger.Info("Seeded tenant gateway cookie key", "path", tenantPath, "key", GatewayCookieSecretKey)
			dbOutcome = EnsureCreated
		}
	}

	// Step 4b: the application's internal service token (ADR-057).
	//
	// PLATFORM-SEEDED, like the cookie key above and for the same reason: nobody
	// chooses its value. The fleet's own declaration says so in as many words --
	// "any long random value" -- and a secret whose value nobody chooses is
	// plumbing, not configuration.
	//
	// It was supplied by hand, once per application, and the cost of that showed
	// up exactly where a manual step always does. oranger's was never written, so
	// both ExternalSecrets failed, so neither Secret was created, so bff-workload
	// and sdk-workload sat in CreateContainerConfigError for a day naming a
	// Kubernetes Secret rather than the OpenBao key that was actually missing.
	// Nothing in that chain says "a human forgot to run a command", and every new
	// application would have met the same wall.
	//
	// Written on absence whatever the tenant's age. An absent token means neither
	// workload has ever started holding one, so there is no live channel to break
	// -- the same argument the cookie key makes. Once present it is never
	// rewritten here: rotating it is a deliberate act that must restart both
	// sides, and a reconcile must not do that to a running application.
	internalTokenKey := InternalTokenKey(appId)
	exists, err := c.SecretExists(ctx, tenantPath, internalTokenKey)
	if err != nil {
		return nil, fmt.Errorf("failed to check OpenBao for %s: %w", internalTokenKey, err)
	}
	if !exists {
		value, err := c.HexKey(ctx, 32)
		if err != nil {
			return nil, fmt.Errorf("failed to generate %s/%s for tenant %s: %w", tenantPath, internalTokenKey, tenantId, err)
		}
		if err := c.CreateSecret(ctx, tenantPath, internalTokenKey, value); err != nil {
			return nil, fmt.Errorf("failed to write %s for tenant %s: %w", internalTokenKey, tenantId, err)
		}
		logger.Info("Seeded application internal service token", "path", tenantPath, "key", internalTokenKey)
		dbOutcome = EnsureCreated
	}

	// Step 5: OAuth confidential client credentials (ADR-053).
	//
	// Reached whether or not step 2 generated anything, because a tenant
	// provisioned before it declared a client must still receive that client's
	// credential. The db-credentials step used to return here, which is why this
	// is a switch above rather than an early return.
	oauthOutcome, err := c.ensureOAuthClientCredentials(ctx, tenantPath, tenantId, oauthClients)
	if err != nil {
		return &EnsureTenantCredentialsResult{
			Result:       dbOutcome,
			CacheOutcome: cacheOutcome,
			OAuthOutcome: EnsureMissing,
		}, err
	}

	return &EnsureTenantCredentialsResult{
		Result:       dbOutcome,
		CacheOutcome: cacheOutcome,
		OAuthOutcome: oauthOutcome,
	}, nil
}

// CachePasswordKey names the tenant cache credential. The delivering
// ExternalSecret templates both the ACL file and the connection URL from this one
// value, so nothing downstream stores a second copy of it.
const CachePasswordKey = "CACHE_PASSWORD"

// RegistryPullSecretKey is the box's image registry credential (hub scope),
// copied into every tenant scope.
const RegistryPullSecretKey = "ghcr-pull-secret"

// TenantDBPasswordKey is the tenant database role's password in the tenant
// scope, read by the spoke's tenant-db composition (property db-password).
const TenantDBPasswordKey = "db-password"

// CrossplaneAdminPasswordKey is the spoke's crossplane-admin role password in its
// platform scope (openbao.SpokePlatformScope).
const CrossplaneAdminPasswordKey = "crossplane-admin-password"

// InternalTokenKey names an application's internal service token: the
// credential its BFF presents to its own SDK (ADR-057's forwarding rule).
//
// DERIVED FROM THE APPLICATION ID, and that derivation is the platform contract:
// an application whose workloads want an internal token read
// `<APPID>_INTERNAL_TOKEN`, and the platform seeds it. Both applications on this
// platform already spell it that way -- ORANGER_INTERNAL_TOKEN,
// WAYPOINT_INTERNAL_TOKEN -- so this names an existing convention rather than
// imposing a new one, and a fleet that spells it differently is simply not served
// by the platform and supplies its own.
//
// Not a tenant identifier in platform code (ADR-047): the appId is an input, and
// this is a function of it. A constant here would give every application of every
// customer the same key name in its own folder, which is fine, and the same VALUE
// only if the folder were shared, which it is not -- but it would also stop the
// key naming the application it belongs to, which is what makes a workload's env
// readable.
func InternalTokenKey(appId string) string {
	return OAuthKeySegment(appId) + "_INTERNAL_TOKEN"
}

// GatewayCookieSecretKey names the tenant gateway's session-cookie
// encryption key.
//
// Per tenant, not shared. Each tenant runs its own gateway instance (ADR-050
// amendment 2026-08-31), and the gateway encrypts its OIDC session cookie with
// this value — so a shared key would let one tenant's gateway decrypt another's
// sessions. That is currently harmless, because cookies are host-only and never
// presented across hostnames, and it is exactly the kind of latent dependency
// that stops being harmless without anyone noticing.
//
// 32 bytes hex-encoded: the gateway hex-decodes it and requires exactly 32 bytes
// for AES-256-GCM. A password from the general generator is the right length in
// characters and the wrong alphabet, and fails at startup complaining about an
// invalid character rather than about encoding.
const GatewayCookieSecretKey = "AGENTGATEWAY_OIDC_COOKIE_SECRET"

// OAuthClientIDKey and OAuthClientSecretKey name the two scalars
// an OAuth confidential client occupies in a tenant's OpenBao scope.
//
// Scalars rather than one composite JSON value, matching how the tenant's other
// application secrets are stored and read: the fleet's ExternalSecret maps each
// key by name. These are consumed as whole values in two different
// places — the registration controller wants them under fixed key names, the
// workload wants them under whatever names it reads — so a scalar each keeps both
// projections a plain rename.
//
// The tenant is not part of the key. The folder path already scopes it, and
// repeating it here would put a tenant identifier in platform code (ADR-047).
func OAuthClientIDKey(clientName string) string {
	return "OAUTH_" + OAuthKeySegment(clientName) + "_CLIENT_ID"
}

func OAuthClientSecretKey(clientName string) string {
	return "OAUTH_" + OAuthKeySegment(clientName) + "_CLIENT_SECRET"
}

// OAuthKeySegment turns an application or client name into the form a key name
// uses: upper case, hyphens as underscores.
//
// Exported because the key GRAMMAR is one rule and several producers follow it --
// the OAuth client keys here, and the per-dependency backend keys the controller
// publishes. A second copy would be a second grammar that drifts silently, since
// a key nobody writes is a key nobody reads and the failure is an ExternalSecret
// that never resolves.
func OAuthKeySegment(clientName string) string {
	return strings.ToUpper(strings.ReplaceAll(clientName, "-", "_"))
}

// OAuthClientIdentifier is the identifier a client is registered under: derived
// from the tenant and the client name, declared before registration and stable for
// the client's life (ADR-053). Changing it is not an in-place update — the
// registration is removed and recreated — so it is derived, never stored as an
// independent input.
func OAuthClientIdentifier(tenantId, clientName string) string {
	return fmt.Sprintf("%s-%s", tenantId, clientName)
}

// ensureOAuthClientCredentials materialises each declared confidential client's
// identifier and credential in the tenant's OpenBao scope.
//
// The two halves obey different rules on purpose. The identifier is derived and
// public, so it is written convergently and a drifted value is repaired. The
// credential is secret material under ADR-003: generated once, and its later
// absence is a fault rather than a trigger to regenerate, because regenerating
// would invalidate a credential that may already be registered and in use.
//
// PreviouslyProvisioned is per client, not per tenant. A client declared today on
// a tenant provisioned months ago is being provisioned for the first time even
// though the tenant is not, and treating that as a fault would make declaring a
// second client impossible.
func (c *Store) ensureOAuthClientCredentials(ctx context.Context, tenantPath, tenantId string, clients []OAuthClient) (EnsureResult, error) {
	logger := log.FromContext(ctx).WithValues("tenant", tenantId, "path", tenantPath)

	outcome := EnsureAlreadyExists
	for _, client := range clients {
		if !client.Confidential {
			// A public client authenticates with no secret; there is nothing to
			// generate, store or rotate. It is declared so the set is complete.
			continue
		}

		idKey := OAuthClientIDKey(client.Name)
		secretKey := OAuthClientSecretKey(client.Name)
		identifier := OAuthClientIdentifier(tenantId, client.Name)

		// Identifier: converge, repairing drift.
		idFound, err := c.SecretExists(ctx, tenantPath, idKey)
		if err != nil {
			return outcome, fmt.Errorf("failed to check OpenBao for %s: %w", idKey, err)
		}
		storedID := ""
		if idFound {
			if storedID, err = c.GetSecret(ctx, tenantPath, idKey); err != nil {
				return outcome, fmt.Errorf("failed to read %s for tenant %s: %w", idKey, tenantId, err)
			}
		}
		switch {
		case !idFound:
			if err := c.CreateSecret(ctx, tenantPath, idKey, identifier); err != nil {
				return outcome, fmt.Errorf("failed to write %s for tenant %s: %w", idKey, tenantId, err)
			}
			logger.Info("Seeded OAuth client identifier", "client", client.Name, "key", idKey)
			outcome = EnsureCreated
		case storedID != identifier:
			if err := c.UpdateSecret(ctx, tenantPath, idKey, identifier); err != nil {
				return outcome, fmt.Errorf("failed to repair %s for tenant %s: %w", idKey, tenantId, err)
			}
			logger.Info("Repaired drifted OAuth client identifier", "client", client.Name, "key", idKey)
			outcome = EnsureCreated
		}

		// Credential: generate once, never regenerate.
		secretExists, err := c.SecretExists(ctx, tenantPath, secretKey)
		if err != nil {
			return outcome, fmt.Errorf("failed to check OpenBao for %s: %w", secretKey, err)
		}
		if secretExists {
			continue
		}
		if client.PreviouslyProvisioned {
			logger.Error(nil, "CRITICAL: OAuth client credential missing for an already-provisioned client. Manual recovery required.",
				"client", client.Name, "key", secretKey)
			return EnsureMissing, fmt.Errorf(
				"%s missing from OpenBao for already-provisioned client %s of tenant %s — manual recovery required",
				secretKey, client.Name, tenantId)
		}

		value, err := c.Password(ctx)
		if err != nil {
			return outcome, fmt.Errorf("failed to generate %s/%s for client %s of tenant %s: %w", tenantPath, secretKey, client.Name, tenantId, err)
		}
		if err := c.CreateSecret(ctx, tenantPath, secretKey, value); err != nil {
			return outcome, fmt.Errorf("failed to write %s for tenant %s: %w", secretKey, tenantId, err)
		}
		logger.Info("Seeded OAuth confidential client credential", "client", client.Name, "key", secretKey)
		outcome = EnsureCreated
	}

	return outcome, nil
}
