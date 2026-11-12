package escrow

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A box must not escrow into its own Infisical.
//
// The master keys being escrowed are the keys that decrypt that Infisical, and it
// is unreachable exactly when they are needed — so a box pointed at itself has no
// escrow while appearing to have one. Refused rather than accepted, because the
// difference only becomes visible on the day it matters.
func TestEscrowRefusesTheBoxItProtects(t *testing.T) {
	const inCluster = "http://infisical-standalone-infisical.platform-security.svc.cluster.local:8080"
	withEnv(t, map[string]string{
		"INFISICAL_ESCROW_URL":           inCluster,
		"INFISICAL_ESCROW_PROJECT_ID":    "p",
		"INFISICAL_ESCROW_CLIENT_ID":     "i",
		"INFISICAL_ESCROW_CLIENT_SECRET": "s",
	})

	_, err := NewEscrowClient(context.Background(), inCluster)
	if err == nil {
		t.Fatal("a box escrowing into its own Infisical was accepted")
	}
	if !strings.Contains(err.Error(), "not an escrow") {
		t.Errorf("the refusal does not say why:\n%v", err)
	}

	// A trailing slash is the same instance, and comparing strings naively would
	// let it through.
	withEnv(t, map[string]string{"INFISICAL_ESCROW_URL": inCluster + "/"})
	if _, err := NewEscrowClient(context.Background(), inCluster); err == nil {
		t.Error("a trailing slash defeated the check")
	}
}

// isolateFromCheckout moves the test out of any checkout, so the escrow files
// scaffolding wrote are not an input to it.
//
// Needed because escrowValue falls back to `k8-secrets/infisical/` when a setting is
// absent from the environment, which is the point for an operator running the CLI and
// a hazard here: a test clearing the environment would otherwise pick up the REAL
// credentials of whoever ran it, pass for the wrong reason, and — worse — be one
// mistake away from reaching the live escrow.
// isolateFromCheckout puts a test somewhere with no checkout AND no operator
// store of its own.
//
// KAMARKAT_HOME is part of the isolation, not an extra: settings now come from
// ~/.kamarkat, so a test that did not relocate it read the DEVELOPER'S real
// store. That is how a suite comes to pass or fail depending on whose machine it
// runs on -- and it did: a partial set left by a half-finished `escrow init` made
// a completeness test report the wrong missing value.
func isolateFromCheckout(t *testing.T) {
	t.Helper()
	t.Setenv("KAMARKAT_HOME", t.TempDir())
	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
}

// The checkout is a real source of settings, and a test must say which it is using.
func TestSettingsComeFromTheOperatorStoreWhenTheEnvironmentIsSilent(t *testing.T) {
	isolateFromCheckout(t)
	withEnv(t, map[string]string{})

	// The operator's store, relocated for the test the same way a CI runner
	// relocates it.
	home := t.TempDir()
	t.Setenv("KAMARKAT_HOME", home)
	dir := filepath.Join(home, StoreEscrowSubdir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for name, value := range map[string]string{
		"INFISICAL_ESCROW_URL":           "https://app.infisical.com\n",
		"INFISICAL_ESCROW_PROJECT_ID":    "proj-1\n",
		"INFISICAL_ESCROW_CLIENT_ID":     "cid-1\n",
		"INFISICAL_ESCROW_CLIENT_SECRET": "sec-1\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	// Trailing newlines are trimmed: a file written by a shell redirect has one, and
	// a client id with a newline on the end authenticates as nobody.
	if got := escrowValue("INFISICAL_ESCROW_CLIENT_ID"); got != "cid-1" {
		t.Fatalf("read %q from the operator store, want %q", got, "cid-1")
	}

	// The environment still wins, because an in-cluster caller has that and no
	// store at all.
	withEnv(t, map[string]string{"INFISICAL_ESCROW_CLIENT_ID": "from-env"})
	t.Setenv("KAMARKAT_HOME", home)
	if got := escrowValue("INFISICAL_ESCROW_CLIENT_ID"); got != "from-env" {
		t.Fatalf("the store overrode the environment: got %q", got)
	}
}

// A checkout is no longer a source, and saying so is the point.
//
// The old location is looked for ONLY to tell an operator where the files have
// to move. Reading it as well would leave two copies of a client secret, one of
// which goes stale without anything noticing.
func TestTheOldCheckoutLocationIsNotRead(t *testing.T) {
	isolateFromCheckout(t)
	withEnv(t, map[string]string{})
	t.Setenv("KAMARKAT_HOME", t.TempDir())

	dir := filepath.Join(".", "k8-secrets", "infisical")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "INFISICAL_ESCROW_CLIENT_ID"),
		[]byte("from-checkout"), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := escrowValue("INFISICAL_ESCROW_CLIENT_ID"); got != "" {
		t.Errorf("a checkout still supplies escrow settings: got %q", got)
	}
	// And the operator is told where they are, rather than only that they are missing.
	hint := MoveHint("INFISICAL_ESCROW_CLIENT_ID")
	if !strings.Contains(hint, "no longer read") || !strings.Contains(hint, "mv ") {
		t.Errorf("the hint does not say how to move them:\n%s", hint)
	}
}

// Complete or absent, never partial. A partial set produces a caller that attempts
// a backup on every reconcile and fails — an escrow that appears to exist.
func TestEscrowIsCompleteOrAbsent(t *testing.T) {
	isolateFromCheckout(t)
	withEnv(t, map[string]string{})
	if _, err := NewEscrowClient(context.Background(), ""); err == nil ||
		!strings.Contains(err.Error(), "no escrow configured") {
		t.Errorf("an unconfigured escrow was not reported as absent: %v", err)
	}

	withEnv(t, map[string]string{
		"INFISICAL_ESCROW_URL":       "https://app.infisical.com",
		"INFISICAL_ESCROW_CLIENT_ID": "i",
	})
	_, err := NewEscrowClient(context.Background(), "")
	if err == nil {
		t.Fatal("a partially configured escrow was accepted")
	}
	for _, want := range []string{"INFISICAL_ESCROW_PROJECT_ID", "INFISICAL_ESCROW_CLIENT_SECRET"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name the missing %s:\n%v", want, err)
		}
	}
}

// What is written must be what comes back, and a cluster that has never been
// escrowed must read as empty rather than as an error — the caller treats those
// differently, and confusing them would overwrite a backup that exists.
func TestBackupAndRestoreRoundTrip(t *testing.T) {
	store := map[string]string{}
	srv := fakeInfisical(t, store)
	defer srv.Close()

	withEnv(t, map[string]string{
		"INFISICAL_ESCROW_URL":           srv.URL,
		"INFISICAL_ESCROW_PROJECT_ID":    "proj",
		"INFISICAL_ESCROW_CLIENT_ID":     "id",
		"INFISICAL_ESCROW_CLIENT_SECRET": "secret",
	})

	e, err := NewEscrowClient(context.Background(), "")
	if err != nil {
		t.Fatalf("NewEscrowClient: %v", err)
	}
	ctx := context.Background()

	if got, err := e.RestoreArtifact(ctx, ForCluster("devbox", "devbox"), ArtifactKubeconfig); err != nil || got != "" {
		t.Fatalf("an un-escrowed cluster read as %q, %v; want empty and no error", got, err)
	}

	if err := e.BackupArtifact(ctx, ForCluster("devbox", "devbox"), ArtifactKubeconfig, "apiVersion: v1"); err != nil {
		t.Fatalf("BackupArtifact: %v", err)
	}
	got, err := e.RestoreArtifact(ctx, ForCluster("devbox", "devbox"), ArtifactKubeconfig)
	if err != nil || got != "apiVersion: v1" {
		t.Fatalf("round trip returned %q, %v", got, err)
	}

	// The second backup of a cluster is the normal case, not a conflict.
	if err := e.BackupArtifact(ctx, ForCluster("devbox", "devbox"), ArtifactKubeconfig, "apiVersion: v2"); err != nil {
		t.Fatalf("re-escrowing failed: %v", err)
	}
	if got, _ := e.RestoreArtifact(ctx, ForCluster("devbox", "devbox"), ArtifactKubeconfig); got != "apiVersion: v2" {
		t.Errorf("re-escrow left %q", got)
	}

	// Clusters must not collide.
	if got, _ := e.RestoreArtifact(ctx, ForCluster("other", "other"), ArtifactKubeconfig); got != "" {
		t.Errorf("another cluster's escrow returned %q", got)
	}
}

// fakeInfisical is the smallest server that behaves like the parts used here.
//
// It models the two refusals a real Infisical makes and a permissive fake would
// hide: a write to a path no folder exists at is a 404, and a create over an
// existing secret is a conflict.
func fakeInfisical(t *testing.T, store map[string]string) *httptest.Server {
	t.Helper()
	folders := map[string]bool{"": true, "/": true}
	key := func(r *http.Request) string {
		return r.URL.Query().Get("secretPath") + "/" + strings.TrimPrefix(r.URL.Path, "/api/v3/secrets/raw/")
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == pathLogin:
			json.NewEncoder(w).Encode(map[string]string{"accessToken": "t"})

		// Creating a folder fills in any missing parent of its path, and a folder
		// that already exists comes back as a 400.
		case r.URL.Path == pathFolders && r.Method == http.MethodPost:
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			full := strings.TrimSuffix(body["path"], "/") + "/" + body["name"]
			if folders[full] {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			acc := ""
			for _, seg := range strings.Split(strings.Trim(full, "/"), "/") {
				acc += "/" + seg
				folders[acc] = true
			}
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodGet:
			v, ok := store[key(r)]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"secret": map[string]string{"secretValue": v}})

		case r.Method == http.MethodPost, r.Method == http.MethodPatch:
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			// Infisical does not create a secret path on write.
			if !folders[body["secretPath"]] {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			k := body["secretPath"] + "/" + strings.TrimPrefix(r.URL.Path, "/api/v3/secrets/raw/")
			// Infisical refuses a create over an existing secret; the client is
			// expected to fall back to a patch.
			if _, exists := store[k]; exists && r.Method == http.MethodPost {
				w.WriteHeader(http.StatusConflict)
				return
			}
			store[k] = body["secretValue"]
			w.WriteHeader(http.StatusOK)

		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
}

func withEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for _, name := range []string{
		"INFISICAL_ESCROW_URL", "INFISICAL_ESCROW_PROJECT_ID",
		"INFISICAL_ESCROW_CLIENT_ID", "INFISICAL_ESCROW_CLIENT_SECRET",
	} {
		os.Unsetenv(name)
	}
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

// Every artefact whose misplacement loses or overwrites irreplaceable material must
// have a DECLARED scope, not a defaulted one.
//
// The default exists for the per-application credentials the secret uploader names at
// runtime, and it must never be what places a key: the difference between declared
// and defaulted is invisible in the path, so it is asserted here instead.
func TestScopeIsDeclaredForSecurityCriticalArtefacts(t *testing.T) {
	cluster := []string{
		ArtifactKubeconfig,
		ArtifactSecretEncryptionKey,
		ArtifactSecretEncryptionKeyPrevious,
		SpokePKIArtifact("ca"),
		SpokePKIArtifact("etcd"),
		SpokePKIArtifact("sa"),
		SpokePKIArtifact("proxy"),
		// A purpose this platform did not anticipate still belongs to its cluster:
		// two spokes sharing a trust-root path would overwrite each other's.
		SpokePKIArtifact("apiserver-etcd-client"),
	}
	for _, a := range cluster {
		scope, declared := DeclaredScope(a)
		if scope != ScopeCluster {
			t.Errorf("%s is box-scoped; it must be keyed to its cluster", a)
		}
		if !declared {
			t.Errorf("%s falls through to the default rather than being declared", a)
		}
	}

	box := []string{escrowSecretName, ArtifactZitadelMasterkey, ArtifactBackupSSECKey}
	for _, a := range box {
		scope, declared := DeclaredScope(a)
		if scope != ScopeBox {
			t.Errorf("%s is cluster-scoped; it would be lost when the cluster is replaced", a)
		}
		if !declared {
			t.Errorf("%s falls through to the default rather than being declared", a)
		}
	}

	// A runtime-composed application credential: box-scoped, and openly undeclared.
	scope, declared := DeclaredScope("waypoint-db-password")
	if scope != ScopeBox {
		t.Error("a dynamically named application credential must stay box-scoped")
	}
	if declared {
		t.Error("a dynamically named credential reports as declared, which would let a " +
			"verifier treat the default as a decision")
	}
}

// A successor hub must not overwrite its predecessor's recovery material, and one
// spoke must not overwrite another's. Box-scoped material is shared on purpose.
func TestClusterScopedArtefactsDoNotCollide(t *testing.T) {
	store := map[string]string{}
	srv := fakeInfisical(t, store)
	defer srv.Close()
	e := testEscrow(t, srv.URL)
	ctx := context.Background()

	first := ForCluster("acme", "acme-hub")
	second := ForCluster("acme", "acme-hub-v2")
	spokeA := ForCluster("acme", "acme-01")
	spokeB := ForCluster("acme", "acme-02")

	for id, want := range map[Identity]string{
		first: "first-incarnation", second: "second-incarnation",
	} {
		if err := e.BackupArtifact(ctx, id, ArtifactSecretEncryptionKey, want); err != nil {
			t.Fatalf("BackupArtifact(%s): %v", id.ClusterName, err)
		}
	}
	for id, want := range map[Identity]string{spokeA: "ca-A", spokeB: "ca-B"} {
		if err := e.BackupArtifact(ctx, id, SpokePKIArtifact("ca"), want); err != nil {
			t.Fatalf("BackupArtifact(%s): %v", id.ClusterName, err)
		}
	}

	for id, want := range map[Identity]string{
		first: "first-incarnation", second: "second-incarnation",
	} {
		if got, _ := e.RestoreArtifact(ctx, id, ArtifactSecretEncryptionKey); got != want {
			t.Errorf("%s's encryption key read back as %q, want %q — a hub incarnation "+
				"overwrote another's", id.ClusterName, got, want)
		}
	}
	for id, want := range map[Identity]string{spokeA: "ca-A", spokeB: "ca-B"} {
		if got, _ := e.RestoreArtifact(ctx, id, SpokePKIArtifact("ca")); got != want {
			t.Errorf("%s's CA read back as %q, want %q — one spoke's trust root "+
				"overwrote another's", id.ClusterName, got, want)
		}
	}

	// The box's own material is deliberately one copy, reached from either
	// incarnation: that is what lets a successor decrypt the restored secret store.
	if err := e.BackupArtifact(ctx, first, escrowSecretName, "master"); err != nil {
		t.Fatalf("BackupArtifact(master keys): %v", err)
	}
	if got, _ := e.RestoreArtifact(ctx, second, escrowSecretName); got != "master" {
		t.Errorf("the successor read the box's master keys as %q; it must find its "+
			"predecessor's", got)
	}
}

// The migration copies exact bytes, is idempotent, leaves the old copy in place, and
// reports a fingerprint rather than the value.
func TestMigratePreservesBytesAndIsIdempotent(t *testing.T) {
	store := map[string]string{}
	srv := fakeInfisical(t, store)
	defer srv.Close()
	e := testEscrow(t, srv.URL)
	ctx := context.Background()

	id := ForCluster("acme", "acme-01")
	const key = "d34db33f00000000000000000000000000000000000000000000000000000000"
	store[legacyPath(id.ClusterName)+"/"+ArtifactSecretEncryptionKey] = key

	m, err := e.MigrateArtifactPath(ctx, id, ArtifactSecretEncryptionKey)
	if err != nil {
		t.Fatalf("MigrateArtifactPath: %v", err)
	}
	if m.Action != MigrationCopied {
		t.Errorf("action is %q, want %q", m.Action, MigrationCopied)
	}
	if m.Digest == "" || strings.Contains(m.Digest, key) {
		t.Errorf("the report must carry a fingerprint and not the value, got %q", m.Digest)
	}
	if got, _ := e.RestoreArtifact(ctx, id, ArtifactSecretEncryptionKey); got != key {
		t.Fatalf("the migrated key reads back as %q; the bytes were not preserved", got)
	}
	if store[legacyPath(id.ClusterName)+"/"+ArtifactSecretEncryptionKey] != key {
		t.Error("the legacy copy was removed; it is retained until the new path has " +
			"actually been used to recover something")
	}

	again, err := e.MigrateArtifactPath(ctx, id, ArtifactSecretEncryptionKey)
	if err != nil || again.Action != MigrationAlreadyPresent {
		t.Errorf("a second run gave %q, %v; want %q and no error",
			again.Action, err, MigrationAlreadyPresent)
	}
}

// A destination holding DIFFERENT bytes is a conflict, never an overwrite: the ways
// to reach it are a successor that has written its own or a run aimed at the wrong
// box, and both become unrecoverable after a blind overwrite.
func TestMigrateRefusesAConflictingDestination(t *testing.T) {
	store := map[string]string{}
	srv := fakeInfisical(t, store)
	defer srv.Close()
	e := testEscrow(t, srv.URL)
	ctx := context.Background()

	id := ForCluster("acme", "acme-01")
	store[legacyPath(id.ClusterName)+"/"+ArtifactSecretEncryptionKey] = "the-original"
	if err := e.BackupArtifact(ctx, id, ArtifactSecretEncryptionKey, "something-else"); err != nil {
		t.Fatalf("seeding the destination: %v", err)
	}

	if _, err := e.MigrateArtifactPath(ctx, id, ArtifactSecretEncryptionKey); err == nil {
		t.Fatal("a conflicting destination was overwritten")
	} else if !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Errorf("the refusal does not say why:\n%v", err)
	}
	if got, _ := e.RestoreArtifact(ctx, id, ArtifactSecretEncryptionKey); got != "something-else" {
		t.Errorf("the destination changed to %q despite the refusal", got)
	}
}

// Nothing at the old path is "absent", not a failure: an unrotated cluster has no
// previous-generation key, and a cluster mid-provision has no kubeconfig yet.
// Whether an absent artefact is REQUIRED is `escrow verify`'s question, not this one.
func TestMigrateReportsAbsentWithoutFailing(t *testing.T) {
	store := map[string]string{}
	srv := fakeInfisical(t, store)
	defer srv.Close()
	e := testEscrow(t, srv.URL)
	ctx := context.Background()

	id := ForCluster("acme", "acme-01")
	m, err := e.MigrateArtifactPath(ctx, id, ArtifactSecretEncryptionKeyPrevious)
	if err != nil {
		t.Fatalf("an absent artefact was reported as an error: %v", err)
	}
	if m.Action != MigrationAbsent {
		t.Errorf("action is %q, want %q", m.Action, MigrationAbsent)
	}
	if got, _ := e.RestoreArtifact(ctx, id, ArtifactSecretEncryptionKeyPrevious); got != "" {
		t.Errorf("migrating an absent artefact wrote %q to the new path", got)
	}
}

// An identity missing either half is refused, because both halves place the path and
// neither has a safe value to assume.
func TestIdentityRefusesAMissingHalf(t *testing.T) {
	store := map[string]string{}
	srv := fakeInfisical(t, store)
	defer srv.Close()
	e := testEscrow(t, srv.URL)
	ctx := context.Background()

	for _, id := range []Identity{ForCluster("", "acme-01"), ForCluster("acme", "")} {
		if err := e.BackupArtifact(ctx, id, ArtifactKubeconfig, "x"); err == nil {
			t.Errorf("an incomplete identity %+v was accepted for a write", id)
		}
		if _, err := e.RestoreArtifact(ctx, id, ArtifactKubeconfig); err == nil {
			t.Errorf("an incomplete identity %+v was accepted for a read", id)
		}
	}
}

// testEscrow builds a client against the fake, with the concrete type's migration
// capability available.
func testEscrow(t *testing.T, url string) *infisicalEscrow {
	t.Helper()
	withEnv(t, map[string]string{
		"INFISICAL_ESCROW_URL":           url,
		"INFISICAL_ESCROW_PROJECT_ID":    "proj",
		"INFISICAL_ESCROW_CLIENT_ID":     "id",
		"INFISICAL_ESCROW_CLIENT_SECRET": "secret",
	})
	c, err := NewEscrowClient(context.Background(), "")
	if err != nil {
		t.Fatalf("NewEscrowClient: %v", err)
	}
	e, ok := c.(*infisicalEscrow)
	if !ok {
		t.Fatalf("NewEscrowClient returned %T, not the concrete escrow", c)
	}
	return e
}
