package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/go-logr/logr"

	client2 "github.com/kamarkat/kamarkat/operators/hub-operator/internal/client"
	"github.com/kamarkat/kamarkat/operators/hub-operator/internal/secrets"
)

// The PROVISIONING CALL SITE, not the resolver (ADR-094 invariant 2, ADR-110).
//
// The chain these assert:
//
//	dependency: waypoint -> resolve oranger's user-acting client and its
//	  machine client -> publish both, each with its kind -> waypoint's
//	  OIDC_ALLOWED_AZP
//
// and, as the other half of the same assertion, that oranger's PUBLIC client and
// its gateway's exchange client never appear in waypoint's allowlist.

const (
	orangerBrowserClientID  = "oranger-public-pkce@nutgraf"
	orangerExchangeClientID = "oranger-gateway-exchange@nutgraf"
	orangerBffClientID      = "oranger-bff@nutgraf"
	orangerServiceClientID  = "oranger-service@nutgraf"
	waypointProjectID       = "392885920103137720"
)

// fakeSecretStore records writes by path and key. It implements only
// TenantSecretStore, which is the point of that interface being narrow.
type fakeSecretStore struct {
	data map[string]string // "path\x00key" -> value
}

func newFakeSecretStore() *fakeSecretStore {
	return &fakeSecretStore{data: map[string]string{}}
}

func k(path, name string) string { return path + "\x00" + name }

func (f *fakeSecretStore) GetSecret(_ context.Context, path, name string) (string, error) {
	if v, ok := f.data[k(path, name)]; ok {
		return v, nil
	}
	return "", fmt.Errorf("absent: %s/%s", path, name)
}

func (f *fakeSecretStore) SecretExists(_ context.Context, path, name string) (bool, error) {
	_, ok := f.data[k(path, name)]
	return ok, nil
}

func (f *fakeSecretStore) CreateSecret(_ context.Context, path, name, value string) error {
	f.data[k(path, name)] = value
	return nil
}

func (f *fakeSecretStore) UpdateSecret(_ context.Context, path, name, value string) error {
	f.data[k(path, name)] = value
	return nil
}

func (f *fakeSecretStore) DeleteSecret(_ context.Context, path, name string) error {
	delete(f.data, k(path, name))
	return nil
}

func (f *fakeSecretStore) EnsureTenantFolderAndCredentials(
	_ context.Context, _, _, _ string, _ bool, _ []secrets.OAuthClient, _, _, _ bool,
) (*secrets.EnsureTenantCredentialsResult, error) {
	return &secrets.EnsureTenantCredentialsResult{}, nil
}

func tenantPath(cellId, appId string) string {
	return fmt.Sprintf(secrets.InfisicalTenantPathFormat, cellId, appId)
}

// orangerCallsWaypoint is the fixture the chain describes: oranger declares
// waypoint as a backend dependency, its backend's client ("bff") acts for its
// users, and the clients have been minted.
func orangerCallsWaypoint() (*AINativeSaaSReconciler, *fakeSecretStore, []resolvedDependency, []client2.DeclaredClient) {
	store := newFakeSecretStore()
	r := &AINativeSaaSReconciler{InfisicalClient: store}
	deps := []resolvedDependency{{AppID: "waypoint", ProjectID: waypointProjectID}}
	clients := []client2.DeclaredClient{
		{Name: "bff", ClientID: orangerBffClientID},
		{Name: gatewayExchangeClientName("oranger"), ClientID: orangerExchangeClientID},
	}
	return r, store, deps, clients
}

func waypointAllowlist(store *fakeSecretStore) string {
	return store.data[k(tenantPath("nutgraf-01", "waypoint"), allowedAzpKey)]
}

var orangerService = &client2.ServiceClient{ClientID: orangerServiceClientID, ClientSecret: "svc-secret"}

func TestTheTargetAdmitsTheUserActingClientAndTheMachineClientEachByKind(t *testing.T) {
	r, store, deps, clients := orangerCallsWaypoint()

	r.publishCrossApplicationTrust(context.Background(), logr.Discard(),
		"nutgraf-01", "nutgraf", "oranger", deps, clients, orangerService, "bff")

	// A MAP entry with a KIND: waypoint must NAME its caller (ADR-042) and know
	// whether the token carries a person (ADR-110) without reading either from
	// the request or the token's shape.
	want := orangerBffClientID + "=oranger:user " + orangerServiceClientID + "=oranger:service"
	if got := waypointAllowlist(store); got != want {
		t.Fatalf("waypoint's %s = %q; want %q", allowedAzpKey, got, want)
	}
}

func TestTheTargetNeverAdmitsTheBrowserOrTheExchangeClient(t *testing.T) {
	// The browser client's identity is any browser's; an exchanged token takes
	// whatever subject it was handed. Neither may name a caller.
	r, store, deps, clients := orangerCallsWaypoint()

	r.publishCrossApplicationTrust(context.Background(), logr.Discard(),
		"nutgraf-01", "nutgraf", "oranger", deps, clients, orangerService, "bff")

	got := waypointAllowlist(store)
	for _, id := range []string{orangerBrowserClientID, orangerExchangeClientID} {
		if strings.Contains(got, id) {
			t.Fatalf("published %s as an allowed caller: %q", id, got)
		}
	}
}

func TestTheAudienceScopeIsPublishedOntoTheCallerNotTheTarget(t *testing.T) {
	// The two halves go to different folders, and swapping them is silent: the
	// caller would ask for no audience while the target admitted nobody.
	r, store, deps, clients := orangerCallsWaypoint()

	r.publishCrossApplicationTrust(context.Background(), logr.Discard(),
		"nutgraf-01", "nutgraf", "oranger", deps, clients, orangerService, "bff")

	want := "urn:zitadel:iam:org:project:id:" + waypointProjectID + ":aud"
	if got := store.data[k(tenantPath("nutgraf-01", "oranger"), "OIDC_BACKEND_AUDIENCE_SCOPES")]; got != want {
		t.Fatalf("oranger's audience scopes = %q; want %q", got, want)
	}
	if _, onTarget := store.data[k(tenantPath("nutgraf-01", "waypoint"), "OIDC_BACKEND_AUDIENCE_SCOPES")]; onTarget {
		t.Fatal("audience scopes landed on the TARGET's folder; they belong to the caller")
	}
}

func TestAPendingUserActingClientPublishesNoAllowlist(t *testing.T) {
	// Pending writes nothing. Publishing the service entry alone would REPLACE
	// this caller's set and drop a user entry a previous reconcile published.
	store := newFakeSecretStore()
	r := &AINativeSaaSReconciler{InfisicalClient: store}
	deps := []resolvedDependency{{AppID: "waypoint", ProjectID: waypointProjectID}}

	r.publishCrossApplicationTrust(context.Background(), logr.Discard(),
		"nutgraf-01", "nutgraf", "oranger", deps, nil, orangerService, "bff")

	if got, ok := store.data[k(tenantPath("nutgraf-01", "waypoint"), allowedAzpKey)]; ok {
		t.Fatalf("published %q while the user-acting client was unknown; want no write", got)
	}
}

func TestTheUserActingClientIsReadFromInfisicalOnLaterReconciles(t *testing.T) {
	// The issuer discloses a generated secret once, so a reconcile after the mint
	// carries no Clients entry. The id published at mint time is the source then.
	store := newFakeSecretStore()
	r := &AINativeSaaSReconciler{InfisicalClient: store}
	store.data[k(tenantPath("nutgraf-01", "oranger"), secrets.InfisicalOAuthClientIDKey("bff"))] = orangerBffClientID
	deps := []resolvedDependency{{AppID: "waypoint", ProjectID: waypointProjectID}}

	r.publishCrossApplicationTrust(context.Background(), logr.Discard(),
		"nutgraf-01", "nutgraf", "oranger", deps, nil /* no Clients this reconcile */, nil, "bff")

	if got, want := waypointAllowlist(store), orangerBffClientID+"=oranger:user"; got != want {
		t.Fatalf("waypoint's %s = %q; want %q from the caller's published id", allowedAzpKey, got, want)
	}
}

func TestASecondCallerIsAddedNotSubstituted(t *testing.T) {
	// Several applications may depend on one target, each reconciled separately.
	// A write that replaced the whole list would revoke the other callers.
	store := newFakeSecretStore()
	r := &AINativeSaaSReconciler{InfisicalClient: store}
	deps := []resolvedDependency{{AppID: "waypoint", ProjectID: waypointProjectID}}

	for _, app := range []string{"oranger", "atlas"} {
		r.publishCrossApplicationTrust(context.Background(), logr.Discard(),
			"nutgraf-01", "nutgraf", app, deps, nil,
			&client2.ServiceClient{ClientID: app + "-service@nutgraf"}, "")
	}

	want := "atlas-service@nutgraf=atlas:service oranger-service@nutgraf=oranger:service"
	if got := waypointAllowlist(store); got != want {
		t.Fatalf("waypoint's %s = %q; want both callers, sorted: %q", allowedAzpKey, got, want)
	}
}

func TestACallersOldEntriesAreReplacedAndOthersAreKept(t *testing.T) {
	// The migration (ADR-110): a caller's next reconcile rewrites its own entries
	// in the typed form, removing its exchange client and its pre-kind entries.
	// Another caller's entries wait for that caller's own reconcile, and a bare
	// id names no application and is dropped.
	r, store, deps, clients := orangerCallsWaypoint()
	store.data[k(tenantPath("nutgraf-01", "waypoint"), allowedAzpKey)] =
		orangerExchangeClientID + "=oranger " + orangerServiceClientID + "=oranger atlas-gateway-exchange@nutgraf=atlas legacy-bare@nutgraf"

	r.publishCrossApplicationTrust(context.Background(), logr.Discard(),
		"nutgraf-01", "nutgraf", "oranger", deps, clients, orangerService, "bff")

	want := "atlas-gateway-exchange@nutgraf=atlas " + orangerBffClientID + "=oranger:user " + orangerServiceClientID + "=oranger:service"
	if got := waypointAllowlist(store); got != want {
		t.Fatalf("waypoint's %s = %q; want %q", allowedAzpKey, got, want)
	}
}

// ── The service identity (ADR-097) ───────────────────────────────────────────

func TestAPendingServiceIdentityLeavesTheUserEntryAlone(t *testing.T) {
	// An application with no service identity yet still acts for its users.
	r, store, deps, clients := orangerCallsWaypoint()

	r.publishCrossApplicationTrust(context.Background(), logr.Discard(),
		"nutgraf-01", "nutgraf", "oranger", deps, clients, nil, "bff")

	if got, ok := store.data[k(tenantPath("nutgraf-01", "oranger"), serviceClientIDKey)]; ok {
		t.Fatalf("published a service client id with no service identity: %q", got)
	}
	if got, want := waypointAllowlist(store), orangerBffClientID+"=oranger:user"; got != want {
		t.Fatalf("waypoint's %s = %q; want only the user caller: %q", allowedAzpKey, got, want)
	}
}

// The caller needs its own credential to mint a token with. Published onto the
// CALLER's path -- the target's folder holds only the allowlist entry, and
// keeping them apart is what lets one be revoked without the other.
func TestTheServiceCredentialIsPublishedToTheCaller(t *testing.T) {
	store := newFakeSecretStore()
	r := &AINativeSaaSReconciler{InfisicalClient: store}

	r.publishServiceIdentity(context.Background(), logr.Discard(), "nutgraf-01", "nutgraf", "oranger",
		&client2.ServiceClient{ClientID: orangerServiceClientID, ClientSecret: "svc-secret"})

	caller := tenantPath("nutgraf-01", "oranger")
	if got := store.data[k(caller, serviceClientIDKey)]; got != orangerServiceClientID {
		t.Fatalf("caller's %s = %q, want %q", serviceClientIDKey, got, orangerServiceClientID)
	}
	if got := store.data[k(caller, serviceClientSecretKey)]; got != "svc-secret" {
		t.Fatalf("caller's %s = %q, want the minted secret", serviceClientSecretKey, got)
	}
	// The credential belongs to the caller and must not appear in the target's
	// folder, which the target's workload reads.
	if got, ok := store.data[k(tenantPath("nutgraf-01", "waypoint"), serviceClientSecretKey)]; ok {
		t.Fatalf("caller's secret leaked into the target's folder: %q", got)
	}
}

// The issuer discloses a generated secret once, so most reconciles carry none.
// Writing the empty value would overwrite the credential the running workload is
// holding, and the failure lands on its next call rather than on this reconcile.
func TestALaterReconcileDoesNotBlankTheServiceSecret(t *testing.T) {
	store := newFakeSecretStore()
	r := &AINativeSaaSReconciler{InfisicalClient: store}

	r.publishServiceIdentity(context.Background(), logr.Discard(), "nutgraf-01", "nutgraf", "oranger",
		&client2.ServiceClient{ClientID: orangerServiceClientID, ClientSecret: "svc-secret"})
	// The next reconcile: the identity exists, so the issuer discloses no secret.
	r.publishServiceIdentity(context.Background(), logr.Discard(), "nutgraf-01", "nutgraf", "oranger",
		&client2.ServiceClient{ClientID: orangerServiceClientID})

	caller := tenantPath("nutgraf-01", "oranger")
	if got := store.data[k(caller, serviceClientSecretKey)]; got != "svc-secret" {
		t.Fatalf("second reconcile left %s = %q; want the stored secret untouched", serviceClientSecretKey, got)
	}
	// The id is still republished: it is what the target matches, and dropping it
	// would read as a caller that no longer exists.
	if got := store.data[k(caller, serviceClientIDKey)]; got != orangerServiceClientID {
		t.Fatalf("second reconcile lost %s = %q", serviceClientIDKey, got)
	}
}

// The name the operator sends to the identity service is the name the credential
// is published against, and the two must not drift apart.
func TestTheServiceUserNameIsDistinctFromTheExchangeClient(t *testing.T) {
	if serviceUserName("oranger") == gatewayExchangeClientName("oranger") {
		t.Fatal("the service identity and the exchange client share a name; one authenticates a user exchange, the other a service")
	}
	if got, want := serviceUserName("oranger"), "oranger-service"; got != want {
		t.Fatalf("serviceUserName = %q, want %q", got, want)
	}
}
