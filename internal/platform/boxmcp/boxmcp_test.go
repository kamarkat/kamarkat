package boxmcp

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	fakediscovery "k8s.io/client-go/discovery/fake"
	fakedynamic "k8s.io/client-go/dynamic/fake"
	fakekube "k8s.io/client-go/kubernetes/fake"
)

// ── the tools' contract ─────────────────────────────────────────────────────

func listTools(t *testing.T, o Options) []*mcp.Tool {
	t.Helper()
	ctx := context.Background()
	server := NewServer(o)
	st, ct := mcp.NewInMemoryTransports()
	if _, err := server.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()
	res, err := session.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	return res.Tools
}

// Every tool declares the Kubernetes access it needs, and none of it writes:
// the box is changed by commits, never by this server (ADR-116 §2).
func TestEveryToolDeclaresReadOnlyPermissions(t *testing.T) {
	tools := listTools(t, Options{EnableVerify: true})
	if len(tools) == 0 {
		t.Fatal("no tools")
	}
	for _, tool := range tools {
		perms, ok := tool.Meta[RequiredPermissionsMetaKey].([]any)
		if !ok || len(perms) == 0 {
			t.Errorf("%s declares no required permissions", tool.Name)
		}
		for _, p := range perms {
			verb := strings.Fields(p.(string))[0]
			if verb != "get" && verb != "list" && verb != "watch" {
				t.Errorf("%s requires %q: only verify may write, and verify has no tools yet", tool.Name, p)
			}
		}
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("%s is not marked read-only", tool.Name)
		}
	}
}

func TestCategoriesCanBeDisabled(t *testing.T) {
	all := ToolNames(Options{})
	without := ToolNames(Options{Disabled: []string{CategoryGitops}})
	for _, n := range without {
		if strings.HasPrefix(n, "argo_") {
			t.Fatalf("%s served with gitops disabled", n)
		}
	}
	if len(without) >= len(all) {
		t.Fatalf("disabling gitops removed nothing: %d vs %d", len(without), len(all))
	}
}

func TestNoToolReturnsSecretValues(t *testing.T) {
	for _, n := range ToolNames(Options{EnableVerify: true}) {
		if strings.Contains(n, "secret") && n != "app_secrets_status" {
			t.Errorf("unexpected secret tool %s", n)
		}
	}
}

// ── a tool against a cluster ────────────────────────────────────────────────

func fakeClients(objects ...runtime.Object) *Clients {
	scheme := runtime.NewScheme()
	apps := schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}
	dyn := fakedynamic.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{apps: "ApplicationList"}, objects...)
	kube := fakekube.NewSimpleClientset()
	disc := kube.Discovery().(*fakediscovery.FakeDiscovery)
	disc.Resources = []*metav1.APIResourceList{{
		GroupVersion: "argoproj.io/v1alpha1",
		APIResources: []metav1.APIResource{{Name: "applications", Kind: "Application", Namespaced: true}},
	}}
	return &Clients{Core: kube, Dynamic: dyn, Discovery: disc}
}

func app(name, sync, health, owner string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1", "kind": "Application",
		"metadata": map[string]any{"name": name, "namespace": "platform-ops"},
		"status":   map[string]any{"sync": map[string]any{"status": sync}, "health": map[string]any{"status": health}},
	}}
	u.SetOwnerReferences([]metav1.OwnerReference{{Kind: "ApplicationSet", Name: owner, APIVersion: "argoproj.io/v1alpha1", UID: "u"}})
	return u
}

func TestArgoAppsReportsOnlyTheUnhealthy(t *testing.T) {
	c := fakeClients(
		app("cert-manager", "Synced", "Healthy", "03-platform-services"),
		app("grafana", "OutOfSync", "Degraded", "03-platform-services"),
	)
	out, err := argoApps(context.Background(), c, appsFilterInput{Unhealthy: true})
	if err != nil {
		t.Fatal(err)
	}
	got := out.([]map[string]any)
	if len(got) != 1 || got[0]["name"] != "grafana" || got[0]["boundary"] != "03-platform-services" {
		t.Fatalf("got %v", got)
	}
}

// ── the front door ──────────────────────────────────────────────────────────

type issuer struct {
	srv *httptest.Server
	key *rsa.PrivateKey
}

func newIssuer(t *testing.T) *issuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	is := &issuer{key: key}
	is.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/v2/keys":
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
				"kty": "RSA", "kid": "k1", "use": "sig", "alg": "RS256",
				"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
			}}})
		case "/oidc/v1/userinfo":
			_ = json.NewEncoder(w).Encode(map[string]string{"email": "ops@acme.example"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(is.srv.Close)
	return is
}

func (is *issuer) token(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = "k1"
	s, err := tok.SignedString(is.key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (is *issuer) claims(roles ...string) jwt.MapClaims {
	r := map[string]any{}
	for _, role := range roles {
		r[role] = map[string]any{"org": "acme"}
	}
	return jwt.MapClaims{
		"iss": is.srv.URL, "sub": "u1", "aud": []string{"project-1", "client-kakt"}, "azp": "client-kakt",
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
		"urn:zitadel:iam:org:project:project-1:roles": r,
	}
}

func frontDoor(t *testing.T, is *issuer) http.Handler {
	t.Helper()
	auth, err := NewAuthenticator(AuthConfig{Issuer: is.srv.URL, ProjectID: "project-1", AllowedClients: []string{"client-kakt"}})
	if err != nil {
		t.Fatal(err)
	}
	return NewHTTPHandler(HTTPConfig{
		Auth:       auth,
		Metadata:   MetadataConfig{PublicURL: "https://mcp.acme.example", Issuer: is.srv.URL, ProjectID: "project-1", Clients: map[string]string{"kakt": "client-kakt"}, Capabilities: []string{"platform"}},
		ClientsFor: func(*Identity) (*Clients, error) { return fakeClients(), nil },
	})
}

func post(h http.Handler, token string) *httptest.ResponseRecorder {
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`
	req := httptest.NewRequest(http.MethodPost, "/platform", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestNoTokenIsChallengedWithTheMetadata(t *testing.T) {
	rec := post(frontDoor(t, newIssuer(t)), "")
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Header().Get("WWW-Authenticate"), "oauth-protected-resource/platform") {
		t.Fatalf("got %d %q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
}

func TestARoleIsRequired(t *testing.T) {
	is := newIssuer(t)
	if rec := post(frontDoor(t, is), is.token(t, is.claims())); rec.Code != http.StatusForbidden {
		t.Fatalf("no role: got %d", rec.Code)
	}
	if rec := post(frontDoor(t, is), is.token(t, is.claims(RoleViewer))); rec.Code != http.StatusOK {
		t.Fatalf("viewer: got %d %s", rec.Code, rec.Body.String())
	}
}

func TestOnlyAccessTokensFromDeclaredClientsAreAccepted(t *testing.T) {
	is := newIssuer(t)
	idToken := is.claims(RoleViewer)
	idToken["at_hash"] = "x"
	if rec := post(frontDoor(t, is), is.token(t, idToken)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("ID token: got %d", rec.Code)
	}
	other := is.claims(RoleViewer)
	other["azp"] = "someone-else"
	if rec := post(frontDoor(t, is), is.token(t, other)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("undeclared client: got %d", rec.Code)
	}
	foreign := is.claims(RoleViewer)
	foreign["aud"] = []string{"another-project"}
	if rec := post(frontDoor(t, is), is.token(t, foreign)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("other audience: got %d", rec.Code)
	}
}

func TestMetadataNamesTheIssuerAndClients(t *testing.T) {
	is := newIssuer(t)
	rec := httptest.NewRecorder()
	frontDoor(t, is).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-protected-resource/platform", nil))
	var m ResourceMetadata
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m.Resource != "https://mcp.acme.example/platform" || m.AuthorizationServers[0] != is.srv.URL ||
		m.Kamarkat.ProjectID != "project-1" || m.Kamarkat.Clients["kakt"] != "client-kakt" {
		t.Fatalf("got %+v", m)
	}
}

func TestTheIdentityIsImpersonatedWithItsRoles(t *testing.T) {
	id := &Identity{Email: "ops@acme.example", Roles: []string{RoleObserver, RoleViewer}}
	if got := strings.Join(id.Groups(), ","); got != "kamarkat-mcp:platform.observer,kamarkat-mcp:platform.viewer" {
		t.Fatalf("groups %s", got)
	}
}

// The kakt-agent is accepted only while declared, reads as itself and never as
// a person, and holds only what its roles give it (ADR-116 §3).
func TestTheAgentIsAcceptedOnlyWhileDeclared(t *testing.T) {
	is := newIssuer(t)
	claims := is.claims(RoleViewer)
	claims["sub"], claims["azp"], claims["client_id"] = "agent-1", "agent-1", "agent-1"
	token := is.token(t, claims)
	req := httptest.NewRequest(http.MethodPost, "/platform", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	declared, _ := NewAuthenticator(AuthConfig{Issuer: is.srv.URL, ProjectID: "project-1", AllowedClients: []string{"client-kakt"}, AgentUserID: "agent-1"})
	id, err := declared.Authenticate(req)
	if err != nil {
		t.Fatalf("declared agent refused: %v", err)
	}
	if id.Email != AgentUserPrefix+"agent-1" || !id.Has(RoleViewer) {
		t.Fatalf("agent identity %+v", id)
	}

	undeclared, _ := NewAuthenticator(AuthConfig{Issuer: is.srv.URL, ProjectID: "project-1", AllowedClients: []string{"client-kakt"}})
	if _, err := undeclared.Authenticate(req); err == nil {
		t.Fatal("an undeclared agent was accepted")
	}
}
