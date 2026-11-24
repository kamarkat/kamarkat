package openbao

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// memStore is a project-level escrow in memory.
type memStore struct {
	mu sync.Mutex
	m  map[string]string
}

func newMem() *memStore { return &memStore{m: map[string]string{}} }
func (s *memStore) BackupProjectArtifact(_ context.Context, n, v string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[n] = v
	return nil
}
func (s *memStore) RestoreProjectArtifact(_ context.Context, n string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[n], nil
}

// fakeBao models the recovery-key endpoints OpenBao 2.7.3 exposes.
type fakeBao struct {
	mu        sync.Mutex
	required  int
	started   bool
	backup    map[string][]string // fingerprint -> keys_base64
	deleted   []string
	initCalls int
}

func (f *fakeBao) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		p := strings.TrimPrefix(r.URL.Path, "/v1/")
		switch {
		case p == "sys/rotate/recovery/init" && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(map[string]any{"started": f.started, "required": f.required})
		case p == "sys/rotate/recovery/init" && r.Method == http.MethodPost:
			f.initCalls++
			var body struct {
				PGPKeys []string `json:"pgp_keys"`
			}
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &body)
			fp, err := PublicFingerprint(body.PGPKeys[0])
			if err != nil {
				t.Errorf("init received an unusable PGP key: %v", err)
			}
			f.required = 1
			f.backup = map[string][]string{fp: {"Y2lwaGVydGV4dA=="}}
			json.NewEncoder(w).Encode(map[string]any{"complete": true, "keys_base64": []string{"Y2lwaGVydGV4dA=="}, "pgp_fingerprints": []string{fp}})
		case p == "sys/rotate/recovery/backup" && r.Method == http.MethodGet:
			if f.backup == nil {
				w.WriteHeader(http.StatusBadRequest)
				io.WriteString(w, `{"errors":["no backed-up keys found"]}`)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"keys_base64": f.backup}})
		case r.Method == http.MethodDelete:
			f.deleted = append(f.deleted, p)
			if p == "sys/rotate/recovery/backup" {
				f.backup = nil
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func setup(t *testing.T, f *fakeBao) (*Client, *memStore, RecoveryPGP) {
	t.Helper()
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	k, err := GenerateRecoveryPGP("proj-1")
	if err != nil {
		t.Fatal(err)
	}
	esc := newMem()
	_ = esc.BackupProjectArtifact(context.Background(), ArtifactRecoveryPGPPublic, k.PublicB64)
	return &Client{addr: srv.URL, http: srv.Client()}, esc, k
}

func TestRecoveryKeySequenceFreshRun(t *testing.T) {
	f := &fakeBao{}
	c, esc, k := setup(t, f)
	if err := RecoveryKeySequence(context.Background(), c, esc); err != nil {
		t.Fatal(err)
	}
	var got EncryptedRecoveryKey
	if err := json.Unmarshal([]byte(esc.m[ArtifactRecoveryKeyEncrypted]), &got); err != nil || got.Fingerprint != k.Fingerprint {
		t.Fatalf("escrowed recovery key %+v (%v)", got, err)
	}
	if f.backup != nil {
		t.Fatal("the backup must be deleted once the escrowed copy is verified")
	}
	if !contains(f.deleted, "auth/kubernetes/role/openbao-trust-bootstrap") || !contains(f.deleted, "sys/policies/acl/openbao-trust-bootstrap") {
		t.Fatalf("the bootstrap identity must retire itself: %v", f.deleted)
	}
}

func TestRecoveryKeySequenceResumesAfterCrashBeforeEscrow(t *testing.T) {
	f := &fakeBao{}
	c, esc, k := setup(t, f)
	// The earlier run issued the key (backup present) and died before escrowing.
	f.required = 1
	f.backup = map[string][]string{k.Fingerprint: {"Y2lwaGVydGV4dA=="}}
	if err := RecoveryKeySequence(context.Background(), c, esc); err != nil {
		t.Fatal(err)
	}
	if f.initCalls != 0 {
		t.Fatal("an existing recovery key must never be re-initialized")
	}
	if esc.m[ArtifactRecoveryKeyEncrypted] == "" {
		t.Fatal("the resumed run must escrow the key from OpenBao's backup")
	}
}

func TestRecoveryKeySequenceFailsClosed(t *testing.T) {
	t.Run("key exists with neither backup nor escrow", func(t *testing.T) {
		f := &fakeBao{required: 1}
		c, esc, _ := setup(t, f)
		err := RecoveryKeySequence(context.Background(), c, esc)
		if err == nil || !strings.Contains(err.Error(), "Fail closed") {
			t.Fatalf("want fail closed, got %v", err)
		}
	})
	t.Run("rotation started by someone else", func(t *testing.T) {
		f := &fakeBao{started: true}
		c, esc, _ := setup(t, f)
		err := RecoveryKeySequence(context.Background(), c, esc)
		if err == nil || !strings.Contains(err.Error(), "fail closed") || f.initCalls != 0 {
			t.Fatalf("want fail closed without init, got %v (init %d)", err, f.initCalls)
		}
	})
}

func TestEstablishCustodyIsIdempotent(t *testing.T) {
	ctx := context.Background()
	box, custody := newMem(), newMem()
	r1, err := EstablishCustody(ctx, box, custody, "proj-1", now)
	if err != nil || !r1.RootCreated || !r1.PGPCreated {
		t.Fatalf("first run must create both: %+v %v", r1, err)
	}
	root := custody.m[ArtifactRootCA]
	r2, err := EstablishCustody(ctx, box, custody, "proj-1", now)
	if err != nil || r2.RootCreated || r2.PGPCreated {
		t.Fatalf("second run must create nothing: %+v %v", r2, err)
	}
	if custody.m[ArtifactRootCA] != root {
		t.Fatal("an existing root must never be regenerated")
	}
	if strings.Contains(box.m[ArtifactRootCertificate], "PRIVATE KEY") || strings.Contains(box.m[ArtifactRecoveryPGPPublic], "PRIVATE") {
		t.Fatal("the box project must hold only public material")
	}
	if _, err := EstablishCustody(ctx, box, custody, "proj-2", now); err == nil {
		t.Fatal("a custody root bound to another project must be refused")
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
