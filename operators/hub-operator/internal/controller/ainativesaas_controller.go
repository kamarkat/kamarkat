package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	client2 "github.com/kamarkat/kamarkat/operators/hub-operator/internal/client"
	"github.com/kamarkat/kamarkat/operators/hub-operator/internal/secrets"
)

// AINativeSaaSReconciler provisions tenant resources into Infisical when an
// AINativeSaaS XR is created. This unblocks the Spoke ESO ExternalSecrets which
// wait for secrets at /spoke-pool/<cellId>/tenants/<tenantId>/<secret-name>.
//
// Currently handles:
//  1. db-credentials — PostgreSQL user credentials for the tenant database.
//     ESO remoteRef.key splits on the last '/' so secret NAME is "db-credentials"
//     and FOLDER path is /spoke-pool/<cellId>/tenants/<tenantId>.
//     ADR-003 Pattern A2a: Kube-SBT → Infisical ONLY → ESO → Spoke K8s Secret.
//  2. infisical-credentials — Machine Identity credentials for the tenant SDK
//     workload. ADR-003, ADR-019: scoped identity for runtime plugin resolution.
//
// Mirrors SpokePoolReconciler (ADR-003 Pattern A2b) for consistency.
// TenantSecretStore is the part of the Infisical client this controller uses.
//
// An INTERFACE so the provisioning path can be tested. It was
// `*secrets.InfisicalClient`, and the consequence was not abstract: the caller
// allowlist was published with the wrong client id (ADR-094 invariant 2, external
// review 2026-09-30) and no test could reach the call site that did it, because
// reaching it meant a network. The resolver could be tested and was; the WIRING
// could not, so the one line that held the defect was the one line uncovered.
//
// Narrow on purpose -- these five methods, not everything the concrete client can
// do. A fake implements what the controller needs and nothing else, so the
// interface does not become a second definition of the Infisical client that
// drifts from the first.
type TenantSecretStore interface {
	GetSecret(ctx context.Context, secretPath, secretName string) (string, error)
	SecretExists(ctx context.Context, secretPath, secretName string) (bool, error)
	CreateSecret(ctx context.Context, secretPath, secretName, secretValue string) error
	UpdateSecret(ctx context.Context, secretPath, secretName, secretValue string) error
	DeleteSecret(ctx context.Context, secretPath, secretName string) error
	EnsureTenantFolderAndCredentials(ctx context.Context, cellId, tenantId, appId string,
		isFirstTime bool, oauthClients []secrets.OAuthClient,
		cacheEnabled, cacheIsFirstTime, gatewayEnabled bool) (*secrets.EnsureTenantCredentialsResult, error)
}

type AINativeSaaSReconciler struct {
	client.Client
	InfisicalClient TenantSecretStore

	// IdentityClient asks the Tenant Identity Service to provision this tenant's
	// identity resources. Nil when the environment does not provision them, which
	// is a legitimate state rather than a misconfiguration — an issuer with no
	// notion of a tenant has nothing to create.
	IdentityClient *client2.IdentityClient
}

//+kubebuilder:rbac:groups=nutgraf.in,resources=ainativesaases,verbs=get;list;watch
//+kubebuilder:rbac:groups=nutgraf.in,resources=ainativesaases/status,verbs=get;update;patch

// Reconcile provisions all tenant resources into Infisical on AINativeSaaS creation or update.
//
// Idempotency contract (ADR-003):
//   - If credentials exist in Infisical → skip, set condition AlreadyExists.
//   - If credentials missing AND first-time → generate + upload, set condition Seeded.
//   - If credentials missing AND NOT first-time → FAIL, require manual intervention.
func (r *AINativeSaaSReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// 1. Fetch AINativeSaaS XR via dynamic unstructured client
	ainativesaas := &unstructured.Unstructured{}
	ainativesaas.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "nutgraf.in",
		Version: "v1alpha1",
		Kind:    "AINativeSaaS",
	})

	if err := r.Get(ctx, req.NamespacedName, ainativesaas); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	tenantId, found, err := unstructured.NestedString(ainativesaas.Object, "spec", "tenantId")
	if err != nil || !found || tenantId == "" {
		logger.Info("AINativeSaaS missing spec.tenantId, skipping", "name", req.Name)
		return ctrl.Result{}, nil
	}

	// ADR-088. Required on the XRD, so an XR without it is one written against
	// the pre-split schema -- skipping is correct: seeding it under a path built
	// from the tenant alone would put one product's credentials where another
	// product of the same customer would read them.
	appId, found, err := unstructured.NestedString(ainativesaas.Object, "spec", "appId")
	if err != nil || !found || appId == "" {
		logger.Info("AINativeSaaS missing spec.appId, skipping", "name", req.Name)
		return ctrl.Result{}, nil
	}

	cellId, found, err := unstructured.NestedString(ainativesaas.Object, "spec", "cellId")
	if err != nil || !found || cellId == "" {
		logger.Info("AINativeSaaS missing spec.cellId, skipping", "name", req.Name)
		return ctrl.Result{}, nil
	}

	logger.Info("Reconciling TenantDatabase credentials", "tenant", tenantId, "cell", cellId)

	// TODO(ADR-039): Move tenant credential generation to Kube-SBT
	// ADR-031: Delegate to InfisicalClient for tenant secret provisioning.
	//
	// ADR-053: isFirstTime is derived from this resource's own observed status, not
	// passed as a literal. It used to be a hardcoded true, which made the ADR-003
	// protection unreachable: a credential lost after provisioning was silently
	// regenerated rather than reported, invalidating whatever already held it.
	// A tenant whose credentials have been seeded is not provisioning for the first
	// time, and the seeded condition is that durable record.
	isFirstTime := !r.isConditionTrue(ainativesaas, conditionTypeDBSeeded)

	// The fleet's declared OAuth clients, read from the tenant resource.
	//
	// Read here and passed to the identity service, never invented: a client's
	// name, type and callback belong to the fleet (ADR-047 forbids a tenant
	// identifier in platform code, and "every tenant has a bff" is one).
	oauthClients := append(declaredOAuthClients(ainativesaas), gatewayExchangeClient(appId))

	// OAuth client credentials are NOT generated here (ADR-060).
	//
	// Zitadel will not accept a supplied client secret — it generates one and
	// discloses it once — so the direction is inverted and the issuer is now the
	// producer. The Tenant Identity Service captures it and publishes the same
	// two keys this used to write.
	//
	// Generating here as well would be worse than redundant: two producers writing
	// OAUTH_*_CLIENT_SECRET would take turns overwriting each other, and the
	// workload would hold whichever won last while the issuer knows the other.
	// That fails only at token exchange, with nothing naming either writer.

	// A fleet that declares no cache gets no credential generated for one.
	cacheEnabled, _, _ := unstructured.NestedBool(ainativesaas.Object, "spec", "cache", "enabled")

	// A fleet with no browser-facing gateway needs no session-cookie key.
	gatewayEnabled, _, _ := unstructured.NestedBool(ainativesaas.Object, "spec", "gateway", "enabled")

	// Per-credential, for the same reason the OAuth clients above are: a cache
	// enabled today on a tenant provisioned months ago is new, and its absent
	// credential is not evidence of loss.
	cacheIsFirstTime := !r.isConditionTrue(ainativesaas, conditionTypeCacheSeeded)

	result, err := r.InfisicalClient.EnsureTenantFolderAndCredentials(ctx, cellId, tenantId, appId, isFirstTime, nil, cacheEnabled, cacheIsFirstTime, gatewayEnabled)
	if err != nil {
		logger.Error(err, "Failed to ensure tenant credentials in Infisical", "tenant", tenantId, "cell", cellId)
		if result != nil && result.Result == secrets.EnsureMissing {
			_ = r.setCondition(ctx, ainativesaas, tenantId, conditionMissing)
		}
		return ctrl.Result{}, err
	}

	// Tenant identity, provisioned by the service that owns it (ADR-041).
	//
	// This operator orchestrates and does not create identity resources itself:
	// the matrix forbids it from managing tenant credentials, and the client id
	// the tenant's gateway needs is allocated by the issuer and published to
	// Infisical from there.
	//
	// Failure is logged and retried rather than fatal. Everything above this
	// point has already been provisioned, and returning an error would redo it
	// on every attempt; the next reconcile repeats this call, which is
	// idempotent.
	var requeue time.Duration
	if r.IdentityClient != nil {
		hosts := tenantGatewayHosts(ainativesaas)
		if len(hosts) == 0 {
			logger.V(1).Info("Tenant declares no gateway hostname; skipping identity provisioning", "tenant", tenantId)
		} else {
			redirects := make([]string, 0, len(hosts))
			postLogout := make([]string, 0, len(hosts))
			for _, h := range hosts {
				// Must match what the gateway sends, exactly — the issuer compares
				// redirect URIs with no wildcards, so a mismatch fails at the
				// authorization endpoint before any credential is entered and reads
				// as a broken login page rather than a missing registration.
				redirects = append(redirects, "https://"+h+"/oauth/callback")
				postLogout = append(postLogout, "https://"+h+"/")
			}
			// The tenant's declared owner, granted administrative access so the
			// tenant has someone who can sign in to it at all.
			ownerEmail, _, _ := unstructured.NestedString(ainativesaas.Object, "spec", "ownerEmail")
			// Fleet-declared: whether people can sign themselves up into THIS
			// tenant. Absent means false, so a fleet that says nothing gets the
			// closed behaviour rather than inheriting somebody else's choice.
			selfReg, _, _ := unstructured.NestedBool(ainativesaas.Object, "spec", "identity", "selfRegistration")
			// The IMMUTABLE binding (ADR-088). Empty on a tenant provisioned
			// before this field existed, which is exactly the state that needs
			// adopting rather than guessing: the identity service will refuse
			// to create a second organisation and say so.
			ownOrgID, _, _ := unstructured.NestedString(ainativesaas.Object, "status", "identity", "zitadelOrgId")
			ownProjectID, _, _ := unstructured.NestedString(ainativesaas.Object, "status", "identity", "zitadelProjectId")

			// The organisation is the TENANT's, not this application's (ADR-088),
			// so a second application of a tenant inherits the binding a sibling
			// already recorded. Without this it fell through to the name search,
			// found the sibling's organisation, and stopped with ErrOrgUnadopted
			// forever (ADR-094). Inheriting from a record is not adopting by name:
			// the binding was established once, by id, by a sibling of the same
			// tenantId.
			knownOrgID := ownOrgID
			if knownOrgID == "" {
				inherited, err := r.siblingOrgBinding(ctx, tenantId, ainativesaas.GetName())
				if err != nil {
					logger.Error(err, "Cannot inherit the tenant's organisation binding; will retry", "tenant", tenantId, "app", appId)
					return ctrl.Result{}, err
				}
				knownOrgID = inherited
			}

			projectName, knownProjectID := applicationProject(appId, ownOrgID, ownProjectID)

			// A service identity is provisioned only for an application that
			// declares it calls another one. Minting one for every application
			// would leave a standing credential, in every tenant, that nothing
			// uses and nobody rotates -- and a credential whose compromise has no
			// observable effect is the hardest kind to notice (ADR-097).
			declaredDeps := backendDependenciesOf(ainativesaas)
			// The platform capabilities whose declaration this application owns
			// (ADR-109, ADR-113): their caller is this application's service
			// identity.
			ownedCapabilities := r.ownedCapabilities(ctx, logger, ainativesaas)
			serviceClient := ""
			if len(declaredDeps) > 0 || len(ownedCapabilities) > 0 {
				serviceClient = serviceUserName(appId)
			}

			identity, err := r.IdentityClient.EnsureTenantIdentity(ctx, tenantId, knownOrgID, projectName, knownProjectID, ownerEmail, selfReg, redirects, postLogout, oauthClients, serviceClient)
			switch {
			case errors.Is(err, client2.ErrIdentityProvisioningUnsupported):
				logger.V(1).Info("Identity provider does not provision tenants; nothing to do", "tenant", tenantId)
			case err != nil:
				logger.Error(err, "Failed to provision tenant identity; will retry", "tenant", tenantId)
			default:
				// Provisioned, but perhaps not usable. The identifiers below are
				// real either way; this is the only place that says a tenant
				// which looks finished cannot be signed in to, so it is logged
				// at error level rather than as debug detail.
				if len(identity.Incomplete) > 0 {
					logger.Error(nil, "Tenant identity provisioned but INCOMPLETE; logins will fail until this is resolved",
						"tenant", tenantId, "incomplete", identity.Incomplete)
				}
				// Record the binding, once. From here the organisation is
				// resolved by id and a rename can never produce a second one.
				//
				// Written before the client id below on purpose: if this
				// reconcile dies between the two, the next one still resolves
				// the SAME organisation rather than creating another.
				//
				// The project binding is written in the SAME status update as the
				// organisation's, and that is load-bearing: applicationProject
				// reads "organisation bound, project not" as a legacy application.
				// A new application whose org binding landed without its project
				// would be moved onto the legacy project on the next reconcile.
				// An application bound before this field existed records the
				// legacy project's id here on its next reconcile, which pins it.
				changed := false
				if ownOrgID == "" && identity.TenantRef != "" {
					if err := unstructured.SetNestedField(ainativesaas.Object, identity.TenantRef, "status", "identity", "zitadelOrgId"); err == nil {
						_ = unstructured.SetNestedField(ainativesaas.Object, time.Now().UTC().Format(time.RFC3339), "status", "identity", "adoptedAt")
						changed = true
					}
				}
				if ownProjectID == "" && identity.ProjectRef != "" {
					if err := unstructured.SetNestedField(ainativesaas.Object, identity.ProjectRef, "status", "identity", "zitadelProjectId"); err == nil {
						changed = true
					}
				}
				if changed {
					if err := r.Status().Update(ctx, ainativesaas); err != nil {
						logger.Error(err, "Could not record the identity binding; it will be retried",
							"tenant", tenantId, "app", appId, "orgId", identity.TenantRef, "projectId", identity.ProjectRef)
					} else {
						logger.Info("Bound application to its ZITADEL organisation and project",
							"tenant", tenantId, "app", appId, "orgId", identity.TenantRef, "projectId", identity.ProjectRef)
					}
				}
				// Persist the allocated client id where the tenant's gateway
				// reads it. Nothing downstream can derive this value, and the
				// gateway will not start without it, so provisioning that did
				// not publish has achieved nothing observable.
				//
				// Written by this operator rather than by the identity service
				// because the path is the tenant's Infisical folder, which this
				// operator already creates and writes on the lines above. Giving
				// the identity service a second Infisical credential to write one
				// NON-SECRET identifier — a public PKCE client id — would add a
				// credential to protect in order to avoid a boundary that costs
				// nothing here.
				// The owner's initial credential, when one was just created.
				//
				// Written before the client id so a failure here cannot leave a
				// tenant that looks provisioned while the only account able to
				// enter it has a password nobody holds. Guarded on non-empty:
				// the field is absent on every reconcile after the first, and
				// writing it blindly would replace a stored password with "".
				// The declared clients' credentials, published under the same
				// key names the fleet's ExternalSecret reads. Without this the
				// client exists at the issuer and nothing holds its secret, so
				// the workload that authenticates with it cannot start.
				for _, dc := range identity.Clients {
					if dc.ClientID != "" {
						if err := r.publishTenantSecret(ctx, cellId, tenantId, appId,
							secrets.InfisicalOAuthClientIDKey(dc.Name), dc.ClientID); err != nil {
							logger.Error(err, "Failed to publish OAuth client id",
								"tenant", tenantId, "client", dc.Name)
						}
					}
					// Empty when the client already existed and was not
					// regenerated: the stored secret is still the live one, and
					// overwriting it with "" would destroy a working credential.
					if dc.ClientSecret != "" {
						if err := r.publishTenantSecret(ctx, cellId, tenantId, appId,
							secrets.InfisicalOAuthClientSecretKey(dc.Name), dc.ClientSecret); err != nil {
							logger.Error(err, "Failed to publish OAuth client secret",
								"tenant", tenantId, "client", dc.Name)
						}
					}
				}

				// The service identity's credentials (ADR-097), published here with
				// the declared clients' and for the same reason: the issuer
				// discloses a generated secret on the call that minted it and never
				// again. Publishing it only once this application's dependencies
				// resolved lost it whenever they did not on that call, and nothing
				// could re-read it.
				r.publishServiceIdentity(ctx, logger, cellId, tenantId, appId, identity.ServiceClient)

				// It must have landed, or this application cannot call another as
				// itself. Checked as the gateway's exchange client is, below, and
				// for the same reason: absent once minted, it cannot be re-read, and
				// regenerating a credential is never a reconcile's decision.
				if serviceClient != "" {
					servicePath := fmt.Sprintf(secrets.InfisicalTenantPathFormat, cellId, appId)
					if _, err := r.InfisicalClient.GetSecret(ctx, servicePath, serviceClientSecretKey); err != nil {
						logger.Error(err, "CRITICAL: the service identity's credential is absent. "+
							"The issuer discloses a generated secret once, so it cannot be re-read: the "+
							"machine user's secret must be regenerated at the issuer. Until it is, this "+
							"application cannot call another as itself",
							"tenant", tenantId, "app", appId, "user", serviceClient, "path", servicePath, "key", serviceClientSecretKey)
					}
				}

				// The gateway's exchange client must have landed, or this
				// application has no gateway (ADR-095).
				//
				// The loop above is silent by design: a client already
				// provisioned returns no secret, because the issuer discloses a
				// generated one exactly once, and rewriting it would invalidate
				// the credential a running workload holds. "Nothing to publish"
				// is therefore normally correct.
				//
				// It is NOT correct for this one client, because every gateway
				// now mints its backend credential rather than forwarding a
				// session token. A gateway whose exchange credential is absent
				// does not degrade -- it fails to start, and the ExternalSecret
				// reports a missing key rather than a client that was never
				// regenerated. Checked here, once, where the recovery can be
				// named.
				exchangeKey := secrets.InfisicalOAuthClientSecretKey(gatewayExchangeClientName(appId))
				exchangePath := fmt.Sprintf(secrets.InfisicalTenantPathFormat, cellId, appId)
				if _, err := r.InfisicalClient.GetSecret(ctx, exchangePath, exchangeKey); err != nil {
					logger.Error(err, "CRITICAL: the gateway exchange client credential is absent. "+
						"The issuer discloses a generated secret once, so it cannot be re-read: the client "+
						"must be regenerated at the issuer. Until it is, this application's gateway cannot "+
						"mint a backend credential and will not start",
						"tenant", tenantId, "app", appId, "path", exchangePath, "key", exchangeKey)
				}

				if identity.OwnerPassword != "" {
					if err := r.publishTenantSecret(ctx, cellId, tenantId, appId, "OWNER_INITIAL_PASSWORD", identity.OwnerPassword); err != nil {
						logger.Error(err, "Provisioned the tenant owner but could not publish their initial password",
							"tenant", tenantId, "owner", ownerEmail)
					} else {
						logger.Info("Tenant owner account created; initial password published to the tenant's secret path",
							"tenant", tenantId, "owner", ownerEmail)
					}
				}
				// The tenant's organisation id, so its gateway can SCOPE login to
				// that organisation.
				//
				// Without it a user arriving at a tenant's hostname is offered a
				// login with no organisation context, and the issuer resolves
				// that to the DEFAULT organisation — the platform's own. The
				// account is created in the wrong tenant and nothing reports it,
				// because from the issuer's side nothing went wrong.
				if err := r.publishTenantSecret(ctx, cellId, tenantId, appId, "OIDC_ORG_ID", identity.TenantRef); err != nil {
					logger.Error(err, "Provisioned tenant identity but could not publish its organisation id",
						"tenant", tenantId)
				}
				if err := r.publishTenantClientID(ctx, cellId, tenantId, appId, identity.ClientID); err != nil {
					logger.Error(err, "Provisioned tenant identity but could not publish its client id; will retry",
						"tenant", tenantId)
				} else {
					logger.Info("Tenant identity provisioned", "tenant", tenantId, "clientId", identity.ClientID)
				}

				// ADR-094 Part 2: the applications this one may call, rendered
				// into BOTH halves.
				//
				// Both, because either alone is unsound. The scope without the
				// allowlist is advisory -- Zitadel does not restrict which
				// project audiences a client may request, so any application
				// could mint a token carrying any audience. The allowlist
				// without the scope admits a caller whose tokens the receiver
				// will never see. The declaration is one fact and it renders
				// into two places.
				deps, pendingDeps, derr := r.resolveBackendDependencies(ctx, tenantId, ainativesaas.GetName(), declaredDeps)
				if derr != nil {
					// A dependency naming a stranger is a declaration error, not
					// a transient one, so it is reported rather than retried into
					// a loop. Everything published above stands.
					logger.Error(derr, "Refusing this application's backend dependencies", "tenant", tenantId, "app", appId)
				} else {
					if len(pendingDeps) > 0 {
						// Not an error: provisioning order across applications is
						// not something either controls, and the next reconcile
						// resolves it. Logged so the wait is visible.
						logger.Info("Backend dependencies waiting on the target's project",
							"tenant", tenantId, "app", appId, "pending", pendingDeps)
					}
					for _, c := range ownedCapabilities {
						if capability := r.capabilityDependency(ctx, c); capability != nil {
							deps = append(deps, *capability)
						} else {
							// Written to Infisical by the capability's own reconcile, which
							// raises no event here.
							logger.Info("A capability has no project yet (not enabled, or not provisioned); its audience waits on it",
								"tenant", tenantId, "app", appId, "capability", c.name)
							requeue = capabilityPendingRequeue
						}
					}
					actingClient, aerr := actingClientOf(oauthClients)
					if aerr != nil {
						logger.Error(aerr, "Refusing this application's user-acting client", "tenant", tenantId, "app", appId)
					} else {
						r.publishCrossApplicationTrust(ctx, logger, cellId, tenantId, appId, deps, identity.Clients, identity.ServiceClient, actingClient)
					}
				}

				// The audience the gateway's token exchange asks for (ADR-095).
				//
				// Allocated by the issuer with the project, so it cannot be
				// rendered into a chart; it travels the credential path for the
				// same reason the client id does. Not secret — it is an
				// audience — but it arrives with the values that are, and
				// splitting it into a second delivery would add a way for the
				// two to disagree.
				if err := r.publishTenantSecret(ctx, cellId, tenantId, appId, "OIDC_PROJECT_ID", identity.ProjectRef); err != nil {
					logger.Error(err, "Provisioned tenant identity but could not publish its project id; the gateway cannot exchange",
						"tenant", tenantId)
				}

				// The gateway's own confidential client, for ADR-095's exchange.
				//
				// Logged and not fatal, like the publishes above: a tenant whose
				// identity exists but whose exchange client does not is one that
				// cannot serve its API, and returning here would also lose the
				// values already published.
			}
		}
	}

	// Record the cache credential's own state BEFORE returning on the tenant-wide
	// outcome below, which short-circuits. Without this the marker never becomes
	// true, cacheIsFirstTime stays true forever, and the protection that makes a
	// LOST cache credential a fault would never engage again — trading one silent
	// failure for another.
	//
	// EnsureSkipped is deliberately not recorded: a tenant with no cache has not
	// seeded a cache credential, and saying otherwise would deadlock it the day
	// it enables one.
	if result.CacheOutcome == secrets.EnsureCreated || result.CacheOutcome == secrets.EnsureAlreadyExists {
		if err := r.setCacheSeededCondition(ctx, ainativesaas, tenantId, result.CacheOutcome); err != nil {
			logger.Error(err, "Failed to record cache credential condition; will retry", "tenant", tenantId)
		}
	}

	switch result.Result {
	case secrets.EnsureAlreadyExists:
		return ctrl.Result{RequeueAfter: requeue}, r.setCondition(ctx, ainativesaas, tenantId, conditionAlreadyExists)
	case secrets.EnsureCreated:
		return ctrl.Result{RequeueAfter: requeue}, r.setCondition(ctx, ainativesaas, tenantId, conditionSeeded)
	default:
		return ctrl.Result{RequeueAfter: requeue}, nil
	}
}

// conditionTypeDBSeeded is the condition recording that this tenant's credentials
// have been provisioned. It doubles as the durable first-provisioning marker.
const conditionTypeDBSeeded = "TenantDBCredentialsSeeded"

// conditionTypeCacheSeeded records that THIS tenant's cache credential has been
// written, separately from whether the tenant itself is new.
//
// Without it the cache reused TenantDBCredentialsSeeded, which answers a
// different question. Infisical outlives a cluster rebuild, so a tenant's DB
// credentials survive and that condition stays true — while a credential
// introduced after the tenant was first provisioned has never been written.
// The operator then refuses to create a key that never existed and reports
// "manual recovery required" for which the platform ships no recovery path.
const conditionTypeCacheSeeded = "TenantCacheCredentialSeeded"

// declaredOAuthClients reads the OAuth clients declared on the tenant resource
// (ADR-053).
//
// A client with no explicit type is treated as confidential. The safe default is
// the one that provisions a credential nothing consumes, rather than the one
// that silently skips a credential something needs.
//
// redirectPaths travels with the client because an issuer matches redirect URIs
// EXACTLY, with no wildcards. Under Hydra these reached the issuer through an
// OAuth2Client CR and never passed through here; Zitadel has no such controller,
// so a client provisioned without its callback fails at the authorization
// endpoint before any credential is entered.
func declaredOAuthClients(obj *unstructured.Unstructured) []client2.OAuthClient {
	// The same hosts the browser client's callbacks are built on, so a declared
	// path is registered on every host the gateway answers.
	hosts := tenantGatewayHosts(obj)
	raw, found, err := unstructured.NestedSlice(obj.Object, "spec", "oauth", "clients")
	if err != nil || !found {
		return nil
	}

	var clients []client2.OAuthClient
	for _, entry := range raw {
		fields, ok := entry.(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := fields["name"].(string)
		if name == "" {
			continue
		}
		clientType, _ := fields["type"].(string)
		var uris []string
		if rp, ok := fields["redirectPaths"].([]interface{}); ok {
			for _, p := range rp {
				s, ok := p.(string)
				if !ok {
					continue
				}
				for _, h := range hosts {
					uris = append(uris, "https://"+h+s)
				}
			}
		}
		actsForUser, _ := fields["actsForUser"].(bool)
		clients = append(clients, client2.OAuthClient{
			Name:         name,
			Confidential: clientType != "public",
			RedirectURIs: uris,
			ActsForUser:  actsForUser,
		})
	}
	return clients
}

// applicationProject decides which identity project an application's clients
// live in (ADR-094), from what its own record already holds:
//
//	project bound                 -> that project, by id. Never re-resolved.
//	org bound, project not        -> the legacy shared project. This application
//	                                 was provisioned before per-application
//	                                 projects; moving it would reissue its client
//	                                 id and sign everyone out, which is a
//	                                 deliberate migration, not a reconcile.
//	neither bound                 -> a project of its own, named for the app.
//
// Reads only this application's OWN bindings. An organisation inherited from a
// sibling does not make an application legacy -- that is exactly the new
// second application this exists for.
func applicationProject(appId, ownOrgID, ownProjectID string) (name, knownID string) {
	switch {
	case ownProjectID != "":
		return "", ownProjectID
	case ownOrgID != "":
		return "", ""
	default:
		return appId, ""
	}
}

// siblingOrgBinding returns the organisation another application of the same
// tenant has already bound, or "" when none has.
//
// Refuses rather than choosing when siblings disagree. Two organisation ids for
// one tenant means the ADR-088 failure has already happened once -- users split
// from their clients -- and picking either would extend it to this application.
func (r *AINativeSaaSReconciler) siblingOrgBinding(ctx context.Context, tenantId, self string) (string, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "nutgraf.in",
		Version: "v1alpha1",
		Kind:    "AINativeSaaSList",
	})
	if err := r.List(ctx, list); err != nil {
		return "", fmt.Errorf("list applications of tenant %q: %w", tenantId, err)
	}
	return orgBindingAmong(list.Items, tenantId, self)
}

// orgBindingAmong is siblingOrgBinding's decision, separated so it can be tested
// without a cluster.
func orgBindingAmong(items []unstructured.Unstructured, tenantId, self string) (string, error) {
	found := ""
	for _, it := range items {
		if it.GetName() == self {
			continue
		}
		t, _, _ := unstructured.NestedString(it.Object, "spec", "tenantId")
		if t != tenantId {
			continue
		}
		org, _, _ := unstructured.NestedString(it.Object, "status", "identity", "zitadelOrgId")
		if org == "" {
			continue
		}
		if found != "" && found != org {
			return "", fmt.Errorf("applications of tenant %q are bound to different organisations (%s, %s); refusing to choose -- reconcile the split first", tenantId, found, org)
		}
		found = org
	}
	return found, nil
}

// publishAllowedCaller adds one caller's client id to a TARGET application's
// allowed-caller list (ADR-094 invariant 2).
//
// Published onto the TARGET's path, not the caller's, because the target is what
// enforces it -- and a list a caller could write is not a control. The receiver
// reads it as OIDC_ALLOWED_AZP and refuses any token whose azp is absent from it.
//
// Read-modify-write, and the read matters: several applications may depend on one
// target, each reconciled separately, so writing only this caller would remove
// the others. Sorted and de-duplicated so a reconcile that changes nothing writes
// nothing and the value does not churn.
//
// A MAP, not a list: each entry is `<clientId>=<appId>`.
//
// A list says a caller may enter and does not say who it is, so a receiver that
// needs to name its caller -- to own records per consuming application, as
// waypoint ADR-042 does -- would have to take the name from the request. That is
// exactly what ADR-094 invariant 2b forbids, and a list makes obeying it
// impossible rather than merely difficult.
//
// The platform is the only party that can supply the mapping. It knows both
// halves at this moment (the caller's appId and the client id it will present),
// and neither the caller nor the receiver can be trusted to assert it: the caller
// would be naming itself, and the receiver has nothing to derive it from.
//
// Each entry also says what the client's tokens carry (ADR-110):
// `<clientId>=<appId>:<kind>`, where kind is
//
//	user      a client whose tokens are issued for a PERSON -- the confidential
//	          client the application's backend obtains its users' tokens with
//	service   the machine client the application calls as itself (ADR-097)
//
// The receiver acts for a user only on a `user` entry. The kind comes from what
// this operator provisioned, never from the token's shape: `azp` and
// `client_id` are one field to the receiver, so the token cannot say.
//
// THIS CALLER'S ENTRIES ARE REPLACED, not merged. The set published is the whole
// of what this caller is admitted as, so an entry it no longer holds -- the
// gateway's exchange client, or a pre-kind `<clientId>=<appId>` -- is removed by
// the caller's own next reconcile. Other callers' entries are left exactly as
// they are; each is rewritten by its own reconcile. A bare `<clientId>` names no
// application and no kind, admits nothing, and is dropped.
func (r *AINativeSaaSReconciler) publishAllowedCallers(ctx context.Context, cellId, tenantId string, target resolvedDependency, callerAppID string, entries map[string]callerKind) error {
	if callerAppID == "" {
		return fmt.Errorf("caller has no application id")
	}
	for id := range entries {
		if id == "" {
			return fmt.Errorf("caller %q has an entry with no client id", callerAppID)
		}
	}
	path := fmt.Sprintf(secrets.InfisicalTenantPathFormat, cellId, target.AppID)
	existing := ""
	if got, err := r.InfisicalClient.GetSecret(ctx, path, allowedAzpKey); err == nil {
		existing = got
	}
	out := make([]string, 0, len(entries))
	for _, v := range strings.Fields(strings.ReplaceAll(existing, ",", " ")) {
		id, named, hasName := strings.Cut(v, "=")
		if !hasName {
			continue
		}
		app, _, _ := strings.Cut(named, ":")
		if app == callerAppID {
			continue
		}
		if _, ours := entries[id]; ours {
			// A client id another application's entry claims, now provisioned to
			// this one. Ours wins: a client id names one application.
			continue
		}
		out = append(out, v)
	}
	for id, kind := range entries {
		out = append(out, id+"="+callerAppID+":"+string(kind))
	}
	sort.Strings(out)
	value := strings.Join(out, " ")
	if value == existing {
		return nil // unchanged; do not rewrite
	}
	return r.publishTenantSecret(ctx, cellId, tenantId, target.AppID, allowedAzpKey, value)
}

// callerKind is what a caller-map entry's tokens carry (ADR-110).
type callerKind string

const (
	callerKindUser    callerKind = "user"
	callerKindService callerKind = "service"
)

// actingClientOf is the declared client this application acts for its users
// with, or "" when it declares none.
//
// At most one, confidential, and without the token-exchange grant. Two would
// leave a target unable to say which one the backend holds; a public client's
// identity is any browser's, which is the defect ADR-094 invariant 2 records;
// and an exchanged token takes whatever subject it was handed, a machine's
// included, so it cannot be admitted as carrying a person.
func actingClientOf(clients []client2.OAuthClient) (string, error) {
	name := ""
	for _, c := range clients {
		if !c.ActsForUser {
			continue
		}
		if !c.Confidential {
			return "", fmt.Errorf("client %q acts for users but is public; only a confidential client may", c.Name)
		}
		if c.TokenExchange {
			return "", fmt.Errorf("client %q acts for users but holds the token-exchange grant, so its tokens can carry a machine subject", c.Name)
		}
		if name != "" {
			return "", fmt.Errorf("clients %q and %q both act for users; declare one", name, c.Name)
		}
		name = c.Name
	}
	return name, nil
}

// allowedAzpKey is where a receiver reads the callers it admits.
const allowedAzpKey = "OIDC_ALLOWED_AZP"

// The service identity an application calls other applications with (ADR-097).
//
// Its own folder, never a target's: this is the CALLER's credential. The
// allowlist entry that lets it in is published onto the target separately, and
// keeping the two apart is what makes a revocation mean something -- deleting
// the credential stops the caller, deleting the entry stops the target admitting
// it, and neither is the other.
const (
	serviceClientIDKey     = "OIDC_SERVICE_CLIENT_ID"
	serviceClientSecretKey = "OIDC_SERVICE_CLIENT_SECRET"
)

// publishCrossApplicationTrust renders one application's resolved backend
// dependencies into the two places they must appear (ADR-094 Part 2).
//
// EXTRACTED FROM Reconcile so it can be tested. The external review of
// 2026-09-30 asked for a regression test at the provisioning call site, not only
// on the resolver, because the defect was the wiring: the resolver was never
// wrong, the argument handed to it was. A call site reachable only through a full
// reconcile is a call site no test names.
//
// Errors are logged rather than returned, as they were inline: each publish is
// independent, and one target's folder being unavailable must not stop the others
// from learning their callers.
func (r *AINativeSaaSReconciler) publishCrossApplicationTrust(
	ctx context.Context, logger logr.Logger, cellId, tenantId, appId string,
	deps []resolvedDependency, clients []client2.DeclaredClient,
	service *client2.ServiceClient, actingClient string,
) {
	if err := r.publishTenantSecret(ctx, cellId, tenantId, appId,
		"OIDC_BACKEND_AUDIENCE_SCOPES", audienceScopesFor(deps)); err != nil {
		logger.Error(err, "Could not publish this application's backend audience scopes",
			"tenant", tenantId, "app", appId)
	}

	// Per dependency, for the chart to render one list entry each. See
	// InfisicalBackendScopeKey for why the aggregate above cannot serve this.
	for _, d := range deps {
		if err := r.publishTenantSecret(ctx, cellId, tenantId, appId,
			InfisicalBackendScopeKey(d.AppID), audienceScopeFor(d.ProjectID)); err != nil {
			logger.Error(err, "Could not publish a backend audience scope",
				"tenant", tenantId, "app", appId, "target", d.AppID)
		}
		if err := r.publishTenantSecret(ctx, cellId, tenantId, appId,
			InfisicalBackendProjectKey(d.AppID), d.ProjectID); err != nil {
			logger.Error(err, "Could not publish a backend project id",
				"tenant", tenantId, "app", appId, "target", d.AppID)
		}
	}

	// The other half: each TARGET learns which of this application's clients it
	// admits, and what each one's tokens carry (ADR-110). Published onto the
	// target's own path, because it is the target that enforces it.
	//
	//	user      the client the backend obtains its users' tokens with
	//	service   the machine client it calls as itself (ADR-097)
	//
	// The gateway's exchange client is NOT admitted. An exchanged token takes
	// whatever subject it was handed, a machine's included, so it cannot be a
	// `user` entry; and the exchange cannot run on the deployed issuer (ADR-097),
	// so no token carries it.
	entries := map[string]callerKind{}
	if actingClient != "" {
		id := r.declaredClientID(ctx, cellId, appId, clients, actingClient)
		if id == "" {
			// Pending, and pending writes NOTHING: publishing the service entry
			// alone would replace this caller's set and drop a `user` entry a
			// previous reconcile published. The next reconcile has the value.
			logger.Info("Backend dependencies wait on this application's user-acting client id",
				"tenant", tenantId, "app", appId, "client", actingClient)
			return
		}
		entries[id] = callerKindUser
	}
	if id := r.serviceClientID(ctx, cellId, appId, service); id != "" {
		entries[id] = callerKindService
	}
	if len(entries) > 0 {
		for _, d := range deps {
			// A platform capability admits ONE client, which its own reconcile
			// publishes from the declaration (ADR-109, ADR-113). It has no
			// caller map, and a `user` entry must never reach it.
			if isPlatformCapabilityAudience(d.AppID) {
				continue
			}
			if err := r.publishAllowedCallers(ctx, cellId, tenantId, d, appId, entries); err != nil {
				logger.Error(err, "Could not publish the allowed callers onto the target",
					"tenant", tenantId, "caller", appId, "target", d.AppID)
			}
		}
	}
}

// serviceClientID is this application's service client id: from the mint when
// this reconcile created it, and from the caller's own Infisical folder
// otherwise -- the issuer discloses it with the secret and never again, exactly
// as for a declared client (declaredClientID). "" when neither has it, which the
// caller treats as pending rather than publish something else.
func (r *AINativeSaaSReconciler) serviceClientID(ctx context.Context, cellId, appId string, service *client2.ServiceClient) string {
	if service != nil && service.ClientID != "" {
		return service.ClientID
	}
	path := fmt.Sprintf(secrets.InfisicalTenantPathFormat, cellId, appId)
	if got, err := r.InfisicalClient.GetSecret(ctx, path, serviceClientIDKey); err == nil {
		return got
	}
	return ""
}

// publishServiceIdentity puts the CALLER's client-credentials identity where the
// caller can read it (ADR-097), when the issuer disclosed it on this call. Its
// client id reaches each TARGET as a `service` entry through
// publishCrossApplicationTrust, beside the `user` entry.
//
// The ADR is explicit that client credentials does NOT produce `azp`; an
// implementation written to that belief admits nobody. The id published is the
// one the issuer actually stamps, which for a machine user is `client_id`.
func (r *AINativeSaaSReconciler) publishServiceIdentity(
	ctx context.Context, logger logr.Logger, cellId, tenantId, appId string,
	service *client2.ServiceClient,
) {
	if service == nil || service.ClientID == "" {
		// Nothing disclosed: no service identity was asked for, or it already
		// existed and its stored credential is the live one. Silent, as for a
		// declared client; a credential that never landed is reported where it
		// is checked. NOT a fallback to the exchange client, which cannot
		// authenticate client credentials and would publish an allowlist entry
		// no service token can match.
		return
	}

	// The caller's own credential. The id is republished every reconcile because
	// it is stable and cheap; the secret only when the issuer disclosed one, since
	// writing an empty value over a stored secret is how a running workload loses
	// the credential it is holding.
	if err := r.publishTenantSecret(ctx, cellId, tenantId, appId,
		serviceClientIDKey, service.ClientID); err != nil {
		logger.Error(err, "Could not publish this application's service client id",
			"tenant", tenantId, "app", appId)
	}
	if service.ClientSecret != "" {
		if err := r.publishTenantSecret(ctx, cellId, tenantId, appId,
			serviceClientSecretKey, service.ClientSecret); err != nil {
			logger.Error(err, "Could not publish this application's service client secret",
				"tenant", tenantId, "app", appId)
		}
	}
}

// declaredClientID is the client id the issuer allocated to one of this
// application's declared clients, by name -- the id a receiver sees as the
// token's client when this application calls it.
//
// NEVER THE BROWSER CLIENT. A public client's identity is any browser's: admitted
// as a caller, any token obtained through the application's own login -- an ID
// token included -- would satisfy the target's allowlist. The allowlist exists to
// distinguish applications, and that value cannot do it.
//
// Resolved from the mint when this reconcile created the client, and from the
// caller's own Infisical folder otherwise: the issuer discloses a generated
// secret once, so a later reconcile carries no Clients entry, while the id
// published at mint time persists. Returns "" when neither has it yet, which the
// caller must treat as pending rather than publish something else.
func (r *AINativeSaaSReconciler) declaredClientID(
	ctx context.Context, cellId, appId string, clients []client2.DeclaredClient, name string,
) string {
	if id := exchangeClientIDFrom(clients, name); id != "" {
		return id
	}

	// ONE key, and no second place to look.
	//
	// There was a fallback here reading OIDC_EXCHANGE_CLIENT_ID, which a second
	// publisher used to write beside this one -- the same value, from the same
	// reconcile, into the same folder, under a different name. Two names for one
	// fact is two things to keep in step, and a reader of either one cannot tell
	// which is authoritative. The second publisher is gone and so is the
	// fallback: a confidential client's credentials live under the OAUTH_ key
	// grammar, derived from the client's own name, like every other declared
	// client's.
	path := fmt.Sprintf(secrets.InfisicalTenantPathFormat, cellId, appId)
	if got, err := r.InfisicalClient.GetSecret(ctx, path, secrets.InfisicalOAuthClientIDKey(name)); err == nil {
		return got
	}
	return ""
}

// exchangeClientIDFrom picks ONE client out of a mint response, by exact name.
//
// Exact, and never a "closest" match or a positional one. The slice holds every
// confidential client the fleet declared alongside the exchange client the
// platform appends, and returning the wrong element here is the whole defect
// this guards: the value becomes a target's allowlist entry, so a near miss
// admits an application nobody authorised.
func exchangeClientIDFrom(clients []client2.DeclaredClient, name string) string {
	for i := range clients {
		if clients[i].Name == name {
			return clients[i].ClientID
		}
	}
	return ""
}

// backendDependenciesOf reads the applications this one may call (ADR-094 Part 2).
func backendDependenciesOf(obj *unstructured.Unstructured) []string {
	raw, found, err := unstructured.NestedStringSlice(obj.Object, "spec", "identity", "backendDependencies")
	if err != nil || !found {
		return nil
	}
	return raw
}

// resolvedDependency is one declared backend dependency, resolved to the facts
// the platform renders from it.
type resolvedDependency struct {
	AppID     string
	ProjectID string // the TARGET's Zitadel project
	XRName    string // the target's AINativeSaaS, for publishing its allowlist
}

// resolveBackendDependencies turns declared appIds into the target projects this
// application's client must request an audience for (ADR-094 Part 2).
//
// SAME TENANT ONLY, and refused rather than skipped otherwise. A dependency
// naming another tenant's application is not a typo to tolerate: cross-tenant
// access is a different decision with a different blast radius and ADR-088 gives
// it no mechanism, so rendering something that half-works would be worse than
// failing.
//
// A dependency whose target has no project yet is NOT an error. Provisioning
// order across applications is not something either one controls, and the next
// reconcile resolves it -- whereas failing here would wedge the caller behind
// the callee forever. It is reported so the wait is visible rather than silent.
func (r *AINativeSaaSReconciler) resolveBackendDependencies(
	ctx context.Context, tenantId, self string, deps []string,
) (resolved []resolvedDependency, pending []string, err error) {
	if len(deps) == 0 {
		return nil, nil, nil
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "nutgraf.in", Version: "v1alpha1", Kind: "AINativeSaaSList",
	})
	if err := r.List(ctx, list); err != nil {
		return nil, nil, fmt.Errorf("list applications of tenant %q: %w", tenantId, err)
	}

	byApp := map[string]unstructured.Unstructured{}
	for _, it := range list.Items {
		t, _, _ := unstructured.NestedString(it.Object, "spec", "tenantId")
		a, _, _ := unstructured.NestedString(it.Object, "spec", "appId")
		if t == tenantId && a != "" {
			byApp[a] = it
		}
	}

	for _, dep := range deps {
		if dep == "" {
			continue
		}
		target, ok := byApp[dep]
		if !ok {
			return nil, nil, fmt.Errorf(
				"application %q declares a backend dependency on %q, which is not an application of tenant %q; "+
					"a dependency names an appId of the SAME tenant, and cross-tenant access has no mechanism (ADR-088)",
				self, dep, tenantId)
		}
		if target.GetName() == self {
			return nil, nil, fmt.Errorf("application %q declares a backend dependency on itself", self)
		}
		pid, _, _ := unstructured.NestedString(target.Object, "status", "identity", "zitadelProjectId")
		if pid == "" {
			pending = append(pending, dep)
			continue
		}
		resolved = append(resolved, resolvedDependency{AppID: dep, ProjectID: pid, XRName: target.GetName()})
	}
	sort.Slice(resolved, func(i, j int) bool { return resolved[i].AppID < resolved[j].AppID })
	sort.Strings(pending)
	return resolved, pending, nil
}

// audienceScopesFor renders the scopes this application's client must request so
// its tokens are accepted by the applications it depends on.
//
// The fleet never writes this string -- it declares a relationship and the
// platform decides what that renders into, which is the rule the gateway chart
// already states about scopes. Deterministic order so a reconcile that changes
// nothing publishes nothing.
func audienceScopesFor(deps []resolvedDependency) string {
	if len(deps) == 0 {
		return ""
	}
	out := make([]string, 0, len(deps))
	for _, d := range deps {
		out = append(out, audienceScopeFor(d.ProjectID))
	}
	return strings.Join(out, " ")
}

// audienceScopeFor is the scope that puts one project into a token's audience.
func audienceScopeFor(projectID string) string {
	return "urn:zitadel:iam:org:project:id:" + projectID + ":aud"
}

// InfisicalBackendScopeKey and InfisicalBackendProjectKey name ONE dependency's
// facts in the caller's own Infisical folder.
//
// PER DEPENDENCY, beside the space-joined aggregate rather than instead of it,
// and the reason is a seam between what git knows and what the issuer allocates.
//
// A chart is rendered from the tenant's repository, which declares dependencies
// by application id -- so at render time the chart knows HOW MANY there are and
// what each is called. It cannot know the target's project id, which the issuer
// allocates at provisioning time and which therefore travels the credential path
// (the same reason OIDC_CLIENT_ID does).
//
// The aggregate cannot close that gap on its own. A gateway's `scopes:` is a YAML
// LIST, and a single space-delimited variable expands into one list entry
// containing spaces, not into several entries. Splitting it would mean the chart
// parsing a string the platform joined -- two representations of one fact, free
// to disagree.
//
// One key per declared dependency removes the seam instead of bridging it: the
// COUNT and the NAMES come from git, where they are declared, and each VALUE
// comes from the platform, where it is allocated. The chart renders exactly as
// many entries as the fleet declared, each naming its own variable, and neither
// side parses anything the other wrote.
func InfisicalBackendScopeKey(targetAppID string) string {
	return "OIDC_BACKEND_AUDIENCE_SCOPE_" + secrets.OAuthKeySegment(targetAppID)
}

// InfisicalBackendProjectKey is the target's project id on its own, which the
// exchange sends as the RFC 8693 `audience` parameter. Published separately from
// the scope because they are consumed at different moments -- the scope at login,
// the audience at the exchange -- and deriving one from the other at the consumer
// would put the issuer's scope grammar into a chart (ADR-059).
func InfisicalBackendProjectKey(targetAppID string) string {
	return "OIDC_BACKEND_PROJECT_ID_" + secrets.OAuthKeySegment(targetAppID)
}

// gatewayExchangeClient is the confidential client a tenant's gateway
// authenticates as when it exchanges a browser session for an access token
// (ADR-095).
//
// APPENDED by the platform to whatever the fleet declared, never read from the
// XR. ADR-053 keeps client naming platform-owned — a fleet able to name this
// client could name another fleet's, and this one holds a credential that mints
// tokens for an API. It is also not a fleet CONCERN: the exchange is how the
// platform delivers a credential to the backend, not a feature a tenant asked
// for, and ADR-047 forbids the reverse dependency of the platform requiring a
// tenant to declare it.
//
// No redirect paths. It never runs a browser flow; it presents client
// credentials at the token endpoint and nothing else.
func gatewayExchangeClient(appId string) client2.OAuthClient {
	return client2.OAuthClient{
		Name:         gatewayExchangeClientName(appId),
		Confidential: true,
		// The capability the whole design turns on. Zitadel gates the RFC 8693
		// grant on the client, so without this the token endpoint refuses the
		// exchange and every API request fails after the gateway starts cleanly.
		TokenExchange: true,
	}
}

// gatewayExchangeClientName is the name the platform registers that client
// under. The issuer registers it as "<tenantId>-<Name>", so this is the suffix,
// not the whole identifier.
//
// PER-APP, like every other derived name (ADR-088): a gateway and its OAuth
// client belong to one product, and two products of one customer share neither.
// A constant here would give a tenant's second app the first app's exchange
// credential — one client minting tokens for two APIs, with the audience of
// whichever project it happens to sit in.
func gatewayExchangeClientName(appId string) string {
	return appId + "-gateway-exchange"
}

// serviceUserName is the login name of the machine user an application calls
// other applications as (ADR-097).
//
// Derived here and sent to the identity service rather than invented there,
// because the same name has to agree with the Infisical key the credential is
// published under. Two derivations of one name is two things to keep in step,
// and the OIDC_ALLOWED_AZP grammar already cost us that lesson once.
//
// Distinct from the exchange client above, and deliberately not a variant of it:
// one authenticates a token exchange carrying a USER, the other authenticates a
// service acting as itself. Naming them alike would invite the reading that a
// machine token is a user token with the user left out.
func serviceUserName(appId string) string {
	return appId + "-service"
}

// conditionState enumerates the three terminal states for TenantDBCredentialsSeeded.
type conditionState int

const (
	conditionSeeded        conditionState = iota // first-time: generated and uploaded
	conditionAlreadyExists                       // idempotent: already present in Infisical
	conditionMissing                             // error: missing post-provisioning
)

// isConditionTrue checks whether a named condition is Status=True on the unstructured object.
func (r *AINativeSaaSReconciler) isConditionTrue(obj *unstructured.Unstructured, condType string) bool {
	conditions, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil || !found {
		return false
	}
	for _, c := range conditions {
		condMap, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if condMap["type"] == condType && condMap["status"] == string(metav1.ConditionTrue) {
			return true
		}
	}
	return false
}

// setCondition writes the TenantDBCredentialsSeeded condition back to the AINativeSaaS status.
func (r *AINativeSaaSReconciler) setCondition(ctx context.Context, obj *unstructured.Unstructured, tenantId string, state conditionState) error {
	logger := log.FromContext(ctx)

	// Re-fetch the latest version to avoid resource version conflicts.
	latest := &unstructured.Unstructured{}
	latest.SetGroupVersionKind(obj.GroupVersionKind())
	if err := r.Get(ctx, client.ObjectKeyFromObject(obj), latest); err != nil {
		logger.Error(err, "Failed to re-fetch AINativeSaaS before status update", "tenant", tenantId)
		return err
	}

	existing, found, err := unstructured.NestedSlice(latest.Object, "status", "conditions")
	if err != nil || !found {
		existing = []interface{}{}
	}

	// Deserialise existing conditions
	var metaConditions []metav1.Condition
	for _, c := range existing {
		condMap, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		typeVal, _ := condMap["type"].(string)
		statusVal, _ := condMap["status"].(string)
		reasonVal, _ := condMap["reason"].(string)
		messageVal, _ := condMap["message"].(string)
		if typeVal == "" || statusVal == "" {
			continue
		}
		metaConditions = append(metaConditions, metav1.Condition{
			Type:    typeVal,
			Status:  metav1.ConditionStatus(statusVal),
			Reason:  reasonVal,
			Message: messageVal,
		})
	}

	var cond metav1.Condition
	switch state {
	case conditionSeeded:
		cond = metav1.Condition{
			Type:    conditionTypeDBSeeded,
			Status:  metav1.ConditionTrue,
			Reason:  "Seeded",
			Message: fmt.Sprintf("DB credentials generated and uploaded to Infisical for tenant %s", tenantId),
		}
	case conditionAlreadyExists:
		cond = metav1.Condition{
			Type:    conditionTypeDBSeeded,
			Status:  metav1.ConditionTrue,
			Reason:  "AlreadyExists",
			Message: fmt.Sprintf("DB credentials already present in Infisical for tenant %s", tenantId),
		}
	case conditionMissing:
		cond = metav1.Condition{
			Type:    conditionTypeDBSeeded,
			Status:  metav1.ConditionFalse,
			Reason:  "CredentialsMissing",
			Message: fmt.Sprintf("CRITICAL: DB credentials missing from Infisical for already-provisioned tenant %s. Manual recovery required.", tenantId),
		}
	}

	return r.mergeCondition(ctx, obj, cond)
}

// mergeCondition re-fetches the resource, merges one condition into its status
// and writes it back.
//
// Extracted from setCondition so a second condition can be recorded without a
// second copy of the re-fetch/serialise/update sequence. Crossplane stores
// conditions as unstructured maps, so this converts in both directions rather
// than using the typed helpers directly.
func (r *AINativeSaaSReconciler) mergeCondition(ctx context.Context, obj *unstructured.Unstructured, cond metav1.Condition) error {
	logger := log.FromContext(ctx)

	// Re-fetch the latest version to avoid resource version conflicts.
	latest := &unstructured.Unstructured{}
	latest.SetGroupVersionKind(obj.GroupVersionKind())
	if err := r.Get(ctx, client.ObjectKeyFromObject(obj), latest); err != nil {
		logger.Error(err, "Failed to re-fetch AINativeSaaS before status update")
		return err
	}

	existing, found, err := unstructured.NestedSlice(latest.Object, "status", "conditions")
	if err != nil || !found {
		existing = []interface{}{}
	}

	var metaConditions []metav1.Condition
	for _, c := range existing {
		condMap, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		typeVal, _ := condMap["type"].(string)
		statusVal, _ := condMap["status"].(string)
		reasonVal, _ := condMap["reason"].(string)
		messageVal, _ := condMap["message"].(string)
		if typeVal == "" || statusVal == "" {
			continue
		}
		metaConditions = append(metaConditions, metav1.Condition{
			Type:    typeVal,
			Status:  metav1.ConditionStatus(statusVal),
			Reason:  reasonVal,
			Message: messageVal,
		})
	}

	meta.SetStatusCondition(&metaConditions, cond)

	// Serialise back to unstructured map format for Crossplane.
	var updated []interface{}
	for _, c := range metaConditions {
		updated = append(updated, map[string]interface{}{
			"type":               c.Type,
			"status":             string(c.Status),
			"reason":             c.Reason,
			"message":            c.Message,
			"lastTransitionTime": metav1.Now().Format("2006-01-02T15:04:05Z"),
		})
	}

	if err := unstructured.SetNestedSlice(latest.Object, updated, "status", "conditions"); err != nil {
		logger.Error(err, "Failed to set status conditions")
		return err
	}

	if err := r.Status().Update(ctx, latest); err != nil {
		logger.Error(err, "Failed to update status")
		return err
	}

	return nil
}

// SetupWithManager registers the controller to watch AINativeSaaS XRs.
func (r *AINativeSaaSReconciler) SetupWithManager(mgr ctrl.Manager) error {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "nutgraf.in",
		Version: "v1alpha1",
		Kind:    "AINativeSaaS",
	})

	// A platform capability being enabled or disabled changes its owner's
	// audience, so its ConfigMap's changes reconcile the declarers.
	return ctrl.NewControllerManagedBy(mgr).
		For(u).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(r.capabilityDeclarers),
			builder.WithPredicates(predicate.NewPredicateFuncs(isCapabilityMarker))).
		Complete(r)
}

// setCacheSeededCondition records TenantCacheCredentialSeeded on the XR.
//
// Separate from setCondition, which owns TenantDBCredentialsSeeded: the two
// answer different questions and conflating them is what let a credential
// introduced after a tenant's provisioning become unseedable.
func (r *AINativeSaaSReconciler) setCacheSeededCondition(ctx context.Context, obj *unstructured.Unstructured, tenantId string, outcome secrets.EnsureResult) error {
	reason, message := "Seeded", fmt.Sprintf("Cache credential generated and uploaded to Infisical for tenant %s", tenantId)
	if outcome == secrets.EnsureAlreadyExists {
		reason, message = "AlreadyExists", fmt.Sprintf("Cache credential already present in Infisical for tenant %s", tenantId)
	}
	return r.mergeCondition(ctx, obj, metav1.Condition{
		Type:    conditionTypeCacheSeeded,
		Status:  metav1.ConditionTrue,
		Reason:  reason,
		Message: message,
	})
}

// tenantGatewayHosts reads the hostnames a tenant answers on.
//
// The FIRST is the browser-facing host and the one an OAuth flow returns to, but
// every declared host is registered: a tenant reachable on a name whose redirect
// was never registered fails to log in on that name alone, which presents as an
// intermittent fault rather than a missing entry.
func tenantGatewayHosts(obj *unstructured.Unstructured) []string {
	raw, found, err := unstructured.NestedStringSlice(obj.Object, "spec", "gateway", "hostnames")
	if err != nil || !found {
		return nil
	}
	return raw
}

// publishTenantClientID writes the allocated OAuth client id to the tenant's
// Infisical folder, where an ExternalSecret delivers it to the gateway.
//
// Idempotent: create when absent, update when present. Reconciliation repeats,
// and an "already exists" answer must be success rather than a conflict — but
// the value is also re-asserted rather than left, so a client id that changed
// upstream converges instead of being pinned by whatever ran first.
func (r *AINativeSaaSReconciler) publishTenantClientID(ctx context.Context, cellId, tenantId, appId, clientID string) error {
	return r.publishTenantSecret(ctx, cellId, tenantId, appId, "OIDC_CLIENT_ID", clientID)
}

// publishTenantSecret writes one value to the tenant's Infisical folder.
//
// Idempotent: create when absent, update when present. Reconciliation repeats,
// so "already exists" must be success rather than a conflict the caller has to
// interpret.
func (r *AINativeSaaSReconciler) publishTenantSecret(ctx context.Context, cellId, tenantId, appId, key, value string) error {
	path := fmt.Sprintf(secrets.InfisicalTenantPathFormat, cellId, appId)

	exists, err := r.InfisicalClient.SecretExists(ctx, path, key)
	if err != nil {
		return fmt.Errorf("check %s/%s: %w", path, key, err)
	}
	if exists {
		return r.InfisicalClient.UpdateSecret(ctx, path, key, value)
	}
	return r.InfisicalClient.CreateSecret(ctx, path, key, value)
}
