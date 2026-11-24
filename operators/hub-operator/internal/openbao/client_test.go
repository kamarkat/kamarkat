package openbao

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeBao is the slice of OpenBao the client uses: Kubernetes login, KV v2 with
// check-and-set, and password-policy generation.
type fakeBao struct {
	mu       sync.Mutex
	kv       map[string]map[string]any
	version  map[string]int
	logins   int
	casFails int // the next N writes lose their check-and-set race
	writes   map[string]map[string]any
}

func newFake() *fakeBao {
	return &fakeBao{kv: map[string]map[string]any{}, version: map[string]int{}, writes: map[string]map[string]any{}}
}

func (f *fakeBao) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := strings.TrimPrefix(r.URL.Path, "/v1/")
	if p == "auth/kubernetes/login" {
		f.logins++
		_ = json.NewEncoder(w).Encode(map[string]any{"auth": map[string]any{"client_token": "t", "lease_duration": 600}})
		return
	}
	if r.Header.Get("X-Vault-Token") != "t" {
		http.Error(w, `{"errors":["permission denied"]}`, http.StatusForbidden)
		return
	}
	switch {
	case p == "sys/auth" && r.Method == http.MethodGet:
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{}})
	case strings.HasPrefix(p, "sys/policies/password/") && strings.HasSuffix(p, "/generate"):
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"password": "generated-by-openbao"}})
	case strings.HasPrefix(p, "kv/data/") && r.Method == http.MethodGet:
		k := strings.TrimPrefix(p, "kv/data/")
		d, ok := f.kv[k]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"data": d, "metadata": map[string]any{"version": f.version[k]}}})
	case strings.HasPrefix(p, "kv/data/") && r.Method == http.MethodPost:
		k := strings.TrimPrefix(p, "kv/data/")
		var body struct {
			Data    map[string]any `json:"data"`
			Options struct {
				CAS *int `json:"cas"`
			} `json:"options"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Options.CAS != nil && (*body.Options.CAS != f.version[k] || f.casFails > 0) {
			if f.casFails > 0 {
				f.casFails--
			}
			http.Error(w, `{"errors":["check-and-set parameter did not match the current version"]}`, http.StatusBadRequest)
			return
		}
		f.kv[k] = body.Data
		f.version[k]++
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost:
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.writes[p] = body
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func testClient(t *testing.T, f *fakeBao) *Client {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	tok := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tok, []byte("jwt"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &Client{addr: srv.URL, role: "hub-operator", tokenFile: tok, http: srv.Client()}
}

func TestCreateSecretNeverOverwrites(t *testing.T) {
	ctx := context.Background()
	c := testClient(t, newFake())
	if err := c.CreateSecret(ctx, ScopeHub, "k", "first"); err != nil {
		t.Fatal(err)
	}
	if err := c.CreateSecret(ctx, ScopeHub, "k", "second"); !errors.Is(err, ErrExists) {
		t.Fatalf("second create: got %v, want ErrExists", err)
	}
	if v, _ := c.GetSecret(ctx, ScopeHub, "k"); v != "first" {
		t.Fatalf("value in use was replaced: %q", v)
	}
}

// Two keys of one scope written by different controllers must both land: a lost
// check-and-set race is retried on the fresh version, not dropped.
func TestScopeWritesSurviveARace(t *testing.T) {
	ctx := context.Background()
	f := newFake()
	c := testClient(t, f)
	if err := c.CreateSecret(ctx, ScopeHub, "a", "1"); err != nil {
		t.Fatal(err)
	}
	f.casFails = 2
	if err := c.UpdateSecret(ctx, ScopeHub, "b", "2"); err != nil {
		t.Fatalf("write after a lost race: %v", err)
	}
	all, err := c.ReadScope(ctx, ScopeHub)
	if err != nil || all["a"] != "1" || all["b"] != "2" {
		t.Fatalf("scope after race = %v, %v", all, err)
	}
}

func TestAbsentIsNotFoundNotAnError(t *testing.T) {
	ctx := context.Background()
	c := testClient(t, newFake())
	if _, err := c.GetSecret(ctx, TenantScope("c1", "app"), "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("absent scope: %v", err)
	}
	if ok, err := c.SecretExists(ctx, ScopeHub, "x"); ok || err != nil {
		t.Fatalf("SecretExists on absent = %v, %v", ok, err)
	}
	if err := c.CreateSecret(ctx, ScopeHub, "k", ""); err == nil {
		t.Fatal("an empty value was written")
	}
}

// The value comes from OpenBao's password policy, not from this process.
func TestGeneratePasswordAsksOpenBao(t *testing.T) {
	c := testClient(t, newFake())
	v, err := c.GeneratePassword(context.Background(), "hex-32")
	if err != nil || v != "generated-by-openbao" {
		t.Fatalf("GeneratePassword = %q, %v", v, err)
	}
}

// A token revoked underneath the client (restored-store recovery) is replaced by
// logging in again, once.
func TestRevokedTokenIsReplaced(t *testing.T) {
	ctx := context.Background()
	f := newFake()
	c := testClient(t, f)
	c.token, c.expires = "revoked", time.Now().Add(time.Hour)
	if _, err := c.GeneratePassword(ctx, "hex-32"); err != nil {
		t.Fatalf("after revocation: %v", err)
	}
	if f.logins != 1 {
		t.Fatalf("logins = %d, want exactly one re-login", f.logins)
	}
}

func TestStaticRoleNamesAndConnection(t *testing.T) {
	if got := StaticRoleName("hub_zitadel"); got != "hub-zitadel" {
		t.Fatalf("StaticRoleName = %q", got)
	}
	u := ConnectionURL("zitadel-db", "platform-data", "hub_zitadel")
	for _, want := range []string{"hub_zitadel@zitadel-db-rw.platform-data.svc:5432/postgres?",
		"sslmode=verify-full", "/openbao/db/zitadel-db/client/tls.key"} {
		if !strings.Contains(u, want) {
			t.Errorf("connection URL %q lacks %q", u, want)
		}
	}
	if strings.Contains(u, "password") {
		t.Errorf("connection URL carries a password: %q", u)
	}
}

// A spoke's mount trusts only its own key; its secret reader and its certificate
// signer are separate identities with separate, UID-named policies.
func TestSpokeAuthSeparatesReaderAndSigner(t *testing.T) {
	f := newFake()
	c := testClient(t, f)
	if err := c.EnsureSpokeAuth(context.Background(), "c1", "uid-1", "-----BEGIN PUBLIC KEY-----\nx\n-----END PUBLIC KEY-----"); err != nil {
		t.Fatal(err)
	}
	if f.writes["sys/auth/spoke-c1"]["type"] != "jwt" {
		t.Fatalf("mount not a JWT mount: %v", f.writes["sys/auth/spoke-c1"])
	}
	keys, _ := f.writes["auth/spoke-c1/config"]["jwt_validation_pubkeys"].([]any)
	if len(keys) != 1 {
		t.Fatalf("mount trusts %d keys, want exactly the cluster's own", len(keys))
	}
	reader := f.writes["sys/policies/acl/spoke-uid-1"]["policy"].(string)
	signer := f.writes["sys/policies/acl/spoke-pki-uid-1"]["policy"].(string)
	if !strings.Contains(reader, "kv/data/spokes/c1/platform") || strings.Contains(reader, "pki_int") {
		t.Errorf("reader policy = %q", reader)
	}
	if !strings.Contains(signer, "pki_int/sign/infrastructure-services") || strings.Contains(signer, "kv/") {
		t.Errorf("signer policy = %q", signer)
	}
	for role, sub := range map[string]string{
		"platform":     "system:serviceaccount:external-secrets:openbao-platform-reader",
		"cert-manager": "system:serviceaccount:cert-manager:openbao-issuer",
	} {
		r := f.writes["auth/spoke-c1/role/"+role]
		if r["bound_subject"] != sub {
			t.Errorf("role %s bound to %v, want %s", role, r["bound_subject"], sub)
		}
		if r["token_explicit_max_ttl"] != SpokeTokenTTL {
			t.Errorf("role %s token outlives %s: %v", role, SpokeTokenTTL, r["token_explicit_max_ttl"])
		}
	}
}
