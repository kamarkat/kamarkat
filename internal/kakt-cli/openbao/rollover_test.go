package openbao

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const rollBox = "box-proj-1"

// pkiBao is the slice of an OpenBao pki_int mount the signing and rollover paths
// use: internal keys, CSRs over them, imported issuers and the default.
type pkiBao struct {
	mu       sync.Mutex
	keys     map[string]*ecdsa.PrivateKey // key id -> key
	keyName  map[string]string            // key id -> name
	issuers  map[string]string            // issuer id -> cert PEM
	issuerKy map[string]string            // issuer id -> key id
	def      string
	n        int
}

func newPKIBao() *pkiBao {
	return &pkiBao{keys: map[string]*ecdsa.PrivateKey{}, keyName: map[string]string{},
		issuers: map[string]string{}, issuerKy: map[string]string{}}
}

func (b *pkiBao) addKey(name string) string {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	b.n++
	id := fmt.Sprintf("key-%d", b.n)
	b.keys[id], b.keyName[id] = k, name
	return id
}

func (b *pkiBao) keyByName(name string) string {
	for id, n := range b.keyName {
		if n == name {
			return id
		}
	}
	return ""
}

func (b *pkiBao) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	p := strings.TrimPrefix(r.URL.Path, "/v1/")
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	reply := func(v any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": v}) }
	list := r.Method == "LIST" || r.URL.Query().Get("list") == "true"
	switch {
	case p == "pki_int/ca/pem":
		if b.def == "" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = w.Write([]byte(b.issuers[b.def]))
	case p == "pki_int/keys" && list:
		ids, info := []string{}, map[string]any{}
		for id, n := range b.keyName {
			ids = append(ids, id)
			info[id] = map[string]any{"key_name": n}
		}
		reply(map[string]any{"keys": ids, "key_info": info})
	case p == "pki_int/keys/generate/internal":
		reply(map[string]any{"key_id": b.addKey(body["key_name"].(string))})
	case p == "pki_int/intermediate/generate/existing":
		k := b.keys[b.keyByName(body["key_ref"].(string))]
		der, _ := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
			Subject: pkix.Name{CommonName: body["common_name"].(string)}}, k)
		reply(map[string]any{"csr": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))})
	case p == "pki_int/intermediate/set-signed":
		blk, _ := pem.Decode([]byte(body["certificate"].(string)))
		cert, _ := x509.ParseCertificate(blk.Bytes)
		for id, k := range b.keys {
			if k.PublicKey.Equal(cert.PublicKey.(crypto.PublicKey)) {
				b.n++
				iid := fmt.Sprintf("issuer-%d", b.n)
				b.issuers[iid] = string(pem.EncodeToMemory(blk))
				b.issuerKy[iid] = id
				reply(map[string]any{"imported_issuers": []string{iid}})
				return
			}
		}
		http.Error(w, "no key for chain", 400)
	case p == "pki_int/issuers" && list:
		ids := []string{}
		for id := range b.issuers {
			ids = append(ids, id)
		}
		reply(map[string]any{"keys": ids})
	case p == "pki_int/config/issuers":
		b.def = body["default"].(string)
		w.WriteHeader(http.StatusNoContent)
	case strings.HasPrefix(p, "pki_int/issuer/"):
		id := strings.TrimPrefix(p, "pki_int/issuer/")
		switch r.Method {
		case http.MethodDelete:
			if id == b.def {
				http.Error(w, "cannot delete the default issuer", 400)
				return
			}
			delete(b.issuers, id)
			delete(b.issuerKy, id)
			w.WriteHeader(http.StatusNoContent)
		case http.MethodGet:
			reply(map[string]any{"certificate": b.issuers[id], "key_id": b.issuerKy[id]})
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	case strings.HasPrefix(p, "pki_int/key/") && r.Method == http.MethodDelete:
		id := strings.TrimPrefix(p, "pki_int/key/")
		for _, k := range b.issuerKy {
			if k == id {
				http.Error(w, "key in use by an issuer", 400)
				return
			}
		}
		delete(b.keys, id)
		delete(b.keyName, id)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

// A box with one intermediate signed by its custody root, as Day-0 leaves it.
func signedBox(t *testing.T) (*Client, *pkiBao, RootCA, *x509.Certificate) {
	t.Helper()
	b := newPKIBao()
	b.addKey(IntermediateKeyName)
	srv := httptest.NewServer(b)
	t.Cleanup(srv.Close)
	c := &Client{addr: srv.URL, http: srv.Client(), token: "t"}
	root, err := GenerateRootCA(rollBox, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := installIntermediate(context.Background(), c, IntermediateKeyName, root, rollBox, 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	def, _ := defaultIssuer(context.Background(), c)
	return c, b, root, def
}

func custodyRoot(t *testing.T, s *memStore) RootCA {
	t.Helper()
	var r RootCA
	if err := json.Unmarshal([]byte(s.m[ArtifactRootCA]), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestCompromiseRolloverReplacesRootAndDestroysTheCompromisedHierarchy(t *testing.T) {
	c, b, oldRoot, compromised := signedBox(t)
	custody, box := newMem(), newMem()
	serial := fmt.Sprintf("%x", compromised.SerialNumber)
	newRoot, err := RollOverCompromised(context.Background(), c, RolloverOptions{BoxID: rollBox, Root: oldRoot,
		Custody: custody, Box: box, Compromised: serial, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(newRoot.CertPEM, oldRoot.CertPEM) || !bytes.Equal(newRoot.Replaces, oldRoot.CertPEM) {
		t.Fatal("the root was not replaced, or the replacement does not record what it replaced")
	}
	if !bytes.Equal(custodyRoot(t, custody).CertPEM, newRoot.CertPEM) || box.m[ArtifactRootCertificate] != string(newRoot.CertPEM) {
		t.Fatal("custody and the box escrow do not both hold the new root")
	}
	nr, _, _ := ParseRoot(newRoot, rollBox, time.Now())
	def, _ := defaultIssuer(context.Background(), c)
	if def.CheckSignatureFrom(nr) != nil {
		t.Fatal("the default issuer does not chain to the new root")
	}
	if len(b.issuers) != 1 {
		t.Fatalf("%d issuers remain; only the new one should", len(b.issuers))
	}
	if b.keyByName(IntermediateKeyName) != "" || len(b.keys) != 1 {
		t.Fatalf("the compromised key survived: %v", b.keyName)
	}
}

// A re-run, and a run interrupted right after custody recorded the new root,
// both finish without creating another root.
func TestCompromiseRolloverIsResumableAndIdempotent(t *testing.T) {
	c, b, oldRoot, compromised := signedBox(t)
	serial := fmt.Sprintf("%X", compromised.SerialNumber.Bytes())
	// Interrupted: custody already holds the replacement; OpenBao is untouched.
	next, _ := GenerateRootCA(rollBox, time.Now())
	next.Replaces = oldRoot.CertPEM
	custody, box := newMem(), newMem()
	got, err := RollOverCompromised(context.Background(), c, RolloverOptions{BoxID: rollBox, Root: next,
		Custody: custody, Box: box, Compromised: serial, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.CertPEM, next.CertPEM) || custody.m[ArtifactRootCA] != "" {
		t.Fatal("a resumed rollover created another root")
	}
	issuers := len(b.issuers)
	keys := len(b.keys)
	if _, err := RollOverCompromised(context.Background(), c, RolloverOptions{BoxID: rollBox, Root: next,
		Custody: custody, Box: box, Compromised: serial, Now: time.Now()}); err != nil {
		t.Fatalf("a re-run after completion failed: %v", err)
	}
	if len(b.issuers) != issuers || len(b.keys) != keys || custody.m[ArtifactRootCA] != "" {
		t.Fatal("a re-run after completion changed something")
	}
}

func TestCompromiseRolloverRefusesAnUnknownSerial(t *testing.T) {
	c, b, root, _ := signedBox(t)
	custody := newMem()
	_, err := RollOverCompromised(context.Background(), c, RolloverOptions{BoxID: rollBox, Root: root,
		Custody: custody, Box: newMem(), Compromised: "0badc0de", Now: time.Now()})
	if err == nil || !strings.Contains(err.Error(), "no intermediate with serial") {
		t.Fatalf("got %v", err)
	}
	if custody.m[ArtifactRootCA] != "" || len(b.issuers) != 1 {
		t.Fatal("an unknown serial changed something")
	}
}

type fakeLeafCluster struct {
	certs    []LeafCert
	reissued map[string]int
	anchor   []byte
	converge bool
}

func (f *fakeLeafCluster) Name() string { return "c" }
func (f *fakeLeafCluster) Certificates(context.Context) ([]LeafCert, error) {
	return append([]LeafCert(nil), f.certs...), nil
}
func (f *fakeLeafCluster) Reissue(_ context.Context, c LeafCert) error {
	f.reissued[c.key()]++
	for i := range f.certs {
		if f.certs[i].key() == c.key() && f.converge {
			f.certs[i].CA, f.certs[i].Ready = f.anchor, true
		}
	}
	return nil
}

func TestReissueLeavesReissuesEachStaleCertificateOnce(t *testing.T) {
	oldR, _ := GenerateRootCA(rollBox, time.Now())
	newR, _ := GenerateRootCA(rollBox, time.Now())
	both := append(append([]byte{}, newR.CertPEM...), oldR.CertPEM...)
	f := &fakeLeafCluster{anchor: newR.CertPEM, converge: true, reissued: map[string]int{}, certs: []LeafCert{
		{Namespace: "a", Name: "old", CA: oldR.CertPEM, Ready: true},
		{Namespace: "a", Name: "both", CA: both, Ready: true},
		{Namespace: "a", Name: "done", CA: newR.CertPEM, Ready: true},
	}}
	if err := ReissueLeaves(context.Background(), []LeafCluster{f}, newR.CertPEM, time.Second, time.Millisecond, nil); err != nil {
		t.Fatal(err)
	}
	if f.reissued["a/old"] != 1 || f.reissued["a/both"] != 1 || f.reissued["a/done"] != 0 {
		t.Fatalf("reissued %v; a bundle still holding the old root must be reissued, a done one must not", f.reissued)
	}

	stuck := &fakeLeafCluster{anchor: newR.CertPEM, reissued: map[string]int{}, certs: []LeafCert{
		{Namespace: "a", Name: "old", CA: oldR.CertPEM, Ready: true}}}
	err := ReissueLeaves(context.Background(), []LeafCluster{stuck}, newR.CertPEM, 20*time.Millisecond, time.Millisecond, nil)
	if !errors.Is(err, ErrRolloverIncomplete) || !strings.Contains(err.Error(), "c:a/old") || stuck.reissued["a/old"] != 1 {
		t.Fatalf("got %v, reissued %v", err, stuck.reissued)
	}
}
