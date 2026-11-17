package components

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kamarkat/kamarkat/internal/platform/escrow"
)

// Each secret's keys must map onto fields its escrowed artefact actually carries.
//
// This used to resolve from a checkout and then from the environment, which is
// what the credentials no longer come from: `kakt box init` enrols them and Day-0
// reads the escrow. What still has to hold is the mapping -- a renamed field here
// is an ExternalSecret retrying forever on a live box, which is the failure this
// has always been about.
func TestOptionalSecretsMapOntoTheirArtefactFields(t *testing.T) {
	payloads := map[string]string{
		escrow.ArtifactBackupStoreCredential: escrow.RenderFields(map[string]string{
			escrow.FieldAccessKeyID:     "AKIAEXAMPLE",
			escrow.FieldSecretAccessKey: "secret",
		}),
		escrow.ArtifactAlertDestination: escrow.RenderFields(map[string]string{
			escrow.FieldAlertmanagerURL: "https://alerts.example.test",
			escrow.FieldHeartbeatURL:    "https://heartbeat.example.test",
		}),
		escrow.ArtifactRegistryCredential: escrow.RenderFields(map[string]string{
			escrow.FieldRegistryUser:  "acme-bot",
			escrow.FieldRegistryToken: "ghp_example",
		}),
		// A single-value credential is the whole payload.
		escrow.ArtifactMailTransportURL: "smtps://user:key@smtp.example.test:465",
	}
	for _, sec := range optionalSecrets() {
		t.Run(sec.name, func(t *testing.T) {
			payload, ok := payloads[sec.artifact]
			if !ok {
				t.Fatalf("%s reads artefact %q, and this test has no payload for it: add one, "+
					"or the mapping is unasserted", sec.name, sec.artifact)
			}
			data, _, missing := resolveOptional(sec, payload)
			if len(missing) > 0 {
				t.Errorf("%s: %v not found in %s", sec.name, missing, sec.artifact)
			}
			if len(data) != len(sec.keys) {
				t.Errorf("%s: resolved %d of %d keys", sec.name, len(data), len(sec.keys))
			}
		})
	}
}

func TestPlatformCredentialAbsentResolvesToNothing(t *testing.T) {
	// An escrow that holds nothing under the artefact. Nothing reads the
	// environment or the operator's store any more, so there is nothing to clear.
	for _, s := range optionalSecrets() {
		data, found, missing := resolveOptional(s, "")
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
	registry := escrow.RenderFields(map[string]string{
		escrow.FieldRegistryUser:  "acme-bot",
		escrow.FieldRegistryToken: "ghp_example",
	})

	var ghcr optionalSecret
	for _, s := range optionalSecrets() {
		if s.name == "ghcr-pull-secret" {
			ghcr = s
		}
	}
	if ghcr.compose == nil {
		t.Fatal("the registry pull secret must compose its docker config")
	}

	in, _, missing := resolveOptional(ghcr, registry)
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
	registry := escrow.RenderFields(map[string]string{
		escrow.FieldRegistryUser:  "u",
		escrow.FieldRegistryToken: "t",
	})
	// The registry host stays an environment value: it has a correct default, and
	// requiring it would make a tenant using ghcr supply it or be told their
	// credential was half-configured.
	t.Setenv("GHCR_REGISTRY", "registry.acme.example")

	for _, s := range optionalSecrets() {
		if s.name != "ghcr-pull-secret" {
			continue
		}
		in, _, _ := resolveOptional(s, registry)
		if doc := s.compose(in)[".dockerconfigjson"]; !strings.Contains(doc, "registry.acme.example") {
			t.Errorf("registry not honoured: %s", doc)
		}
	}
}
