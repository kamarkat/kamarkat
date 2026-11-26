package components

import (
	"testing"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/hubaccess"
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
		// region included, because the box's own secret store takes it as a
		// discrete key and an archive configured without it fails at first use.
		// `kakt escrow enroll-store-credential` carries whatever the operator
		// supplied, so a credential enrolled before this was needed declares
		// only the two keys and has to be re-enrolled with a region line.
		escrow.ArtifactBackupStoreCredential: escrow.RenderFields(map[string]string{
			escrow.FieldAccessKeyID:     "AKIAEXAMPLE",
			escrow.FieldSecretAccessKey: "secret",
			"region":                    "hel1",
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

// Day-0 creates no registry credential: it is enrolled when an application
// first declares a private package, and hub-operator converges it from the
// escrow into OpenBao, so a box of public applications never holds one.
func TestDayZeroCreatesNoRegistryCredential(t *testing.T) {
	for _, s := range optionalSecrets() {
		if s.name == "ghcr-pull-secret" || s.artifact == escrow.ArtifactRegistryCredential {
			t.Errorf("Day-0 still creates %s", s.name)
		}
	}
}

// box init enrols the object-store key pair alone; Day-0 takes the region from
// the cluster's declaration, and a credential that is absent stays absent.
func TestBackupCredentialTakesRegionFromDeclaration(t *testing.T) {
	var s optionalSecret
	for _, c := range optionalSecrets() {
		if c.name == "s3-credentials-escrowed" {
			s = c
		}
	}
	payload := escrow.RenderFields(map[string]string{
		escrow.FieldAccessKeyID:     "AK",
		escrow.FieldSecretAccessKey: "SK",
	})
	data, found, missing := resolveOptional(s, payload)
	missing = fillDeclared(data, missing, s.declared(hubaccess.Coordinates{Region: "fsn1"}))
	if len(found) != 2 || len(missing) != 0 || data["region"] != "fsn1" {
		t.Fatalf("data=%v found=%v missing=%v", data, found, missing)
	}

	data, _, missing = resolveOptional(s, payload)
	if missing = fillDeclared(data, missing, s.declared(hubaccess.Coordinates{})); len(missing) != 1 {
		t.Fatalf("no declared region must stay missing, got %v", missing)
	}

	if _, found, _ := resolveOptional(s, ""); len(found) != 0 {
		t.Fatalf("an absent credential must resolve to nothing, got %v", found)
	}
}
