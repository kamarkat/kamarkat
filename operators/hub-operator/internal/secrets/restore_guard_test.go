package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/kamarkat/kamarkat/operators/hub-operator/internal/openbao"
)

// kvServer is the slice of OpenBao the tenant producer uses: KV v2 with
// check-and-set, and password-policy generation (counted, so a test can prove
// that nothing was generated).
type kvServer struct {
	mu        sync.Mutex
	kv        map[string]map[string]any
	version   map[string]int
	generated int
}

func (s *kvServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := strings.TrimPrefix(r.URL.Path, "/v1/")
	switch {
	case strings.HasSuffix(p, "/generate"):
		s.generated++
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"password": "fresh-value"}})
	case strings.HasPrefix(p, "kv/data/") && r.Method == http.MethodGet:
		k := strings.TrimPrefix(p, "kv/data/")
		d, ok := s.kv[k]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"data": d, "metadata": map[string]any{"version": s.version[k]}}})
	case strings.HasPrefix(p, "kv/data/"):
		k := strings.TrimPrefix(p, "kv/data/")
		var body struct {
			Data map[string]any `json:"data"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.kv[k] = body.Data
		s.version[k]++
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

const (
	tenantScope = "spokes/c1/tenants/shop"
	internalKey = "SHOP_INTERNAL_TOKEN"
)

func storeWith(t *testing.T, values map[string]any, guard func(context.Context) error) (*Store, *kvServer) {
	t.Helper()
	srv := &kvServer{kv: map[string]map[string]any{}, version: map[string]int{}}
	if values != nil {
		srv.kv[tenantScope] = values
		srv.version[tenantScope] = 1
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	s := NewStore(openbao.NewWithClient(ts.URL, ts.Client(), "t"))
	s.RestoreGuard = guard
	return s, srv
}

var unfinished = func(context.Context) error {
	return errors.New("refusing to generate a secret value during an unfinished restore: openbao-db incarnation u2")
}

func TestEveryGeneratorRefusesDuringAnUnfinishedRestore(t *testing.T) {
	s, srv := storeWith(t, nil, unfinished)
	ctx := context.Background()
	if _, err := s.Password(ctx); err == nil {
		t.Error("Password generated during an unfinished restore")
	}
	if _, err := s.HexKey(ctx, 32); err == nil {
		t.Error("HexKey generated during an unfinished restore")
	}
	if _, err := s.URLSafe(ctx, 32); err == nil {
		t.Error("URLSafe generated during an unfinished restore")
	}
	if srv.generated != 0 {
		t.Errorf("OpenBao was asked to generate %d value(s)", srv.generated)
	}
}

// A restore that brought every value back succeeds without generating anything.
func TestRestoreWithEveryValuePresentGeneratesNothing(t *testing.T) {
	s, srv := storeWith(t, map[string]any{TenantDBPasswordKey: "restored-db", CachePasswordKey: "restored-cache", internalKey: "restored-token"}, unfinished)
	res, err := s.EnsureTenantFolderAndCredentials(context.Background(), "c1", "acme", "shop",
		false, nil, false, false, false)
	if err != nil {
		t.Fatalf("a complete restore was refused: %v", err)
	}
	if res.Result != EnsureAlreadyExists || srv.generated != 0 {
		t.Fatalf("result %v, generated %d; want AlreadyExists and nothing generated", res.Result, srv.generated)
	}
	if srv.kv[tenantScope][TenantDBPasswordKey] != "restored-db" {
		t.Fatal("a restored value was replaced")
	}
}

// A restore missing an authoritative credential fails, names it, and writes
// nothing in its place -- even for a tenant whose first-time flag says generate.
func TestRestoreMissingACredentialFailsAndNamesIt(t *testing.T) {
	s, srv := storeWith(t, map[string]any{TenantDBPasswordKey: "restored-db", CachePasswordKey: "restored-cache"}, unfinished)
	_, err := s.EnsureTenantFolderAndCredentials(context.Background(), "c1", "acme", "shop",
		true, nil, false, false, false)
	if err == nil {
		t.Fatal("a missing credential was regenerated during an unfinished restore")
	}
	if !strings.Contains(err.Error(), internalKey) || !strings.Contains(err.Error(), "unfinished restore") {
		t.Fatalf("the failure does not name the missing credential and the reason: %v", err)
	}
	if _, present := srv.kv[tenantScope][internalKey]; present || srv.generated != 0 {
		t.Fatal("a replacement value was written for the missing credential")
	}
}

// Once recovery is complete, a genuinely new value is generated as usual.
func TestGenerationResumesOnceRecoveryIsComplete(t *testing.T) {
	s, srv := storeWith(t, map[string]any{TenantDBPasswordKey: "restored-db", CachePasswordKey: "restored-cache"}, nil)
	if _, err := s.EnsureTenantFolderAndCredentials(context.Background(), "c1", "acme", "shop",
		true, nil, false, false, false); err != nil {
		t.Fatalf("ordinary generation refused: %v", err)
	}
	if srv.kv[tenantScope][internalKey] != "fresh-value" || srv.generated != 1 {
		t.Fatalf("the new value was not generated: %v (generated %d)", srv.kv[tenantScope], srv.generated)
	}
}

// OpenBao's password policies stop at 100 characters, so a 64-byte key is two
// hex-64 draws and no policy longer than that is asked for.
func TestA64ByteHexKeyIsTwoDraws(t *testing.T) {
	s, srv := storeWith(t, nil, nil)
	v, err := s.HexKey(context.Background(), 64)
	if err != nil || v != "fresh-valuefresh-value" || srv.generated != 2 {
		t.Fatalf("HexKey(64) = %q, %v after %d draw(s)", v, err, srv.generated)
	}
}
