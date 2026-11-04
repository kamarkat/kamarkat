package components

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The k8-secrets layout already matched the Secret keys these map to; nothing
// read it. This pins that correspondence so a renamed key fails here rather
// than as an ExternalSecret retrying forever on a live box.
func TestOptionalSecretsResolveFromCheckout(t *testing.T) {
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZERO_OPS_DIR", root)

	for _, s := range optionalSecrets() {
		t.Run(s.name, func(t *testing.T) {
			if _, err := os.Stat(filepath.Join(root, "k8-secrets", s.dir)); err != nil {
				t.Skipf("no k8-secrets/%s in this checkout", s.dir)
			}
			data, _, missing := resolveOptional(s)
			if len(missing) > 0 {
				t.Errorf("%s: keys present on disk do not cover the mapping; missing %v", s.name, missing)
			}
			if len(data) != len(s.keys) {
				t.Errorf("%s: resolved %d of %d keys", s.name, len(data), len(s.keys))
			}
		})
	}
}

// An environment variable wins, which is how a workflow supplies a value it
// holds as a repository secret rather than as a file.
func TestOptionalSecretPrefersEnvironment(t *testing.T) {
	t.Setenv("ZERO_OPS_DIR", t.TempDir())
	t.Setenv("S3_ACCESS_KEY_ID", "from-env")
	t.Setenv("S3_SECRET_ACCESS_KEY", "also-env")
	for _, s := range optionalSecrets() {
		if s.name != "s3-object-storage" {
			continue
		}
		data, _, missing := resolveOptional(s)
		if len(missing) > 0 {
			t.Fatalf("missing %v", missing)
		}
		if data["access-key-id"] != "from-env" {
			t.Errorf("got %q, want the environment value", data["access-key-id"])
		}
	}
}

// Nothing supplied resolves to nothing, which InstallPlatformCredentials turns
// into a refusal. These are required: a box without object storage is a box
// whose database has nowhere to back up to, not a box with a feature off.
func TestPlatformCredentialAbsentResolvesToNothing(t *testing.T) {
	t.Setenv("ZERO_OPS_DIR", t.TempDir())
	for _, e := range []string{
		"S3_ACCESS_KEY_ID", "S3_SECRET_ACCESS_KEY",
		"GRAFANA_IRM_ALERTMANAGER_URL", "GRAFANA_IRM_HEARTBEAT_URL",
	} {
		t.Setenv(e, "")
	}
	for _, s := range optionalSecrets() {
		data, found, missing := resolveOptional(s)
		if data != nil || found != nil || missing != nil {
			t.Errorf("%s: an unconfigured secret should resolve to nothing, got %v/%v/%v", s.name, data, found, missing)
		}
	}
}

// A tenant keeping its packages private supplies a username and a token; what
// the cluster needs is a docker config document. The composition is where that
// gap is closed, and getting it wrong yields a pull secret that authenticates as
// nobody -- which presents as ImagePullBackOff on the tenant's own workloads,
// not on anything the platform owns.
func TestRegistryPullSecretComposesDockerConfig(t *testing.T) {
	t.Setenv("ZERO_OPS_DIR", t.TempDir())
	t.Setenv("GHCR_USERNAME", "acme-bot")
	t.Setenv("GHCR_TOKEN", "ghp_example")

	var ghcr optionalSecret
	for _, s := range optionalSecrets() {
		if s.name == "ghcr-pull-secret" {
			ghcr = s
		}
	}
	if ghcr.compose == nil {
		t.Fatal("the registry pull secret must compose its docker config")
	}

	in, _, missing := resolveOptional(ghcr)
	if len(missing) > 0 {
		t.Fatalf("missing %v", missing)
	}
	out := ghcr.compose(in)

	doc, ok := out[".dockerconfigjson"]
	if !ok {
		t.Fatalf("composed keys %v, want .dockerconfigjson", out)
	}
	var parsed struct {
		Auths map[string]struct {
			Username, Password, Auth string
		} `json:"auths"`
	}
	if err := json.Unmarshal([]byte(doc), &parsed); err != nil {
		t.Fatalf("composed an invalid docker config: %v\n%s", err, doc)
	}
	entry, ok := parsed.Auths["ghcr.io"]
	if !ok {
		t.Fatalf("no entry for the default registry; got %v", parsed.Auths)
	}
	if entry.Username != "acme-bot" || entry.Password != "ghp_example" {
		t.Errorf("credential not carried through: %+v", entry)
	}
	// The auth field is what most clients actually read; a document with the
	// username and password set and this one wrong still fails to pull.
	want := base64.StdEncoding.EncodeToString([]byte("acme-bot:ghp_example"))
	if entry.Auth != want {
		t.Errorf("auth = %q, want %q", entry.Auth, want)
	}
}

// A tenant whose registry is not ghcr.io must still be able to say so.
func TestRegistryPullSecretHonoursRegistry(t *testing.T) {
	t.Setenv("ZERO_OPS_DIR", t.TempDir())
	t.Setenv("GHCR_USERNAME", "u")
	t.Setenv("GHCR_TOKEN", "t")
	t.Setenv("GHCR_REGISTRY", "registry.acme.example")

	for _, s := range optionalSecrets() {
		if s.name != "ghcr-pull-secret" {
			continue
		}
		in, _, _ := resolveOptional(s)
		if doc := s.compose(in)[".dockerconfigjson"]; !strings.Contains(doc, "registry.acme.example") {
			t.Errorf("registry not honoured: %s", doc)
		}
	}
}
