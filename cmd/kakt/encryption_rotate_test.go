package main

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
	"text/template"

	"github.com/kamarkat/kamarkat/internal/assets"
)

// renderKeys returns the secretbox key names, in the order the API server sees them.
// The FIRST is the write key; the rest are read-only. Order is the entire rotation
// mechanism, so it is asserted rather than assumed.
func renderKeys(t *testing.T, primary, fallback string) []string {
	t.Helper()
	body, err := readEncryptionConfigTemplate()
	if err != nil {
		t.Fatal(err)
	}
	tmpl, err := template.New("enc").Parse(string(body))
	if err != nil {
		t.Fatal(err)
	}
	data := map[string]any{
		"SecretName":     "c-encryption-config",
		"Namespace":      "platform-capi",
		"PrimaryKeyName": primary,
		"PrimaryKeyB64":  "QUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUE=",
	}
	if fallback != "" {
		data["FallbackKeyName"] = fallback
		data["FallbackKeyB64"] = "QkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkI="
	}
	var out bytes.Buffer
	if err := tmpl.Execute(&out, data); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, line := range strings.Split(out.String(), "\n") {
		tl := strings.TrimSpace(line)
		if strings.HasPrefix(tl, "#") {
			continue
		}
		if strings.HasPrefix(tl, "- name: key") {
			keys = append(keys, strings.TrimSpace(strings.TrimPrefix(tl, "- name:")))
		}
	}
	return keys
}

func TestTheNewKeyIsFirstAndTheOldOneSecond(t *testing.T) {
	// THE ORDERING IS THE ROTATION. secretbox writes with the FIRST key and reads with
	// any of them, so new-first/old-second is what makes a rotation encrypt new data
	// while old data stays readable. Reversed, every write still uses the OLD key: the
	// configuration looks rotated, etcd keeps filling with the disclosed key's
	// ciphertext, and nothing reports it.
	got := renderKeys(t, "key2", "key1")
	if len(got) != 2 || got[0] != "key2" || got[1] != "key1" {
		t.Fatalf("keys render as %v, want [key2 key1] — the NEW key must be first or writes "+
			"continue under the old one", got)
	}
}

func TestWithoutAFallbackOnlyOneKeyRenders(t *testing.T) {
	// The finalised state, and the first-enable state. A stray second key here would
	// leave a disclosed key able to decrypt the cluster after the rotation "completed".
	got := renderKeys(t, "key2", "")
	if len(got) != 1 || got[0] != "key2" {
		t.Fatalf("keys render as %v, want [key2] only", got)
	}
}

func TestGenerationsAreDistinctSoEtcdCanBeAudited(t *testing.T) {
	// The key NAME is written into every stored object as
	// k8s:enc:secretbox:v1:<name>:... so two generations must never share a name.
	// If they did, "no object still carries the old key" would be unmeasurable — and
	// after a disclosure, unmeasurable is indistinguishable from unfinished.
	seen := map[string]bool{}
	for g := 1; g <= 5; g++ {
		n := assets.EncryptionKeyName(g)
		if seen[n] {
			t.Fatalf("generation %d reuses the name %q", g, n)
		}
		seen[n] = true
	}
	if assets.EncryptionKeyName(2) != "key2" {
		t.Fatalf("generation 2 is named %q; the runbook's etcd grep depends on this shape",
			assets.EncryptionKeyName(2))
	}
}

func TestTheLiveConfigurationIsParsedBackIntoOrderedKeys(t *testing.T) {
	// readLiveKeys reads the cluster's own configuration to decide the next generation,
	// because the cluster is what the API server actually uses. A rotation numbered from
	// anywhere else could reuse a name already present in etcd.
	cfg := `apiVersion: apiserver.config.k8s.io/v1
kind: EncryptionConfiguration
resources:
  - resources:
      - secrets
    providers:
      - secretbox:
          keys:
            - name: key3
              secret: QUFB
            - name: key2
              secret: QkJC
      - identity: {}
`
	names := keyLine.FindAllStringSubmatch(cfg, -1)
	secrets := secretLine.FindAllStringSubmatch(cfg, -1)
	if len(names) != 2 || len(secrets) != 2 {
		t.Fatalf("parsed %d names and %d secrets, want 2 and 2", len(names), len(secrets))
	}
	if names[0][1] != "key3" || names[1][1] != "key2" {
		t.Fatalf("parsed %v, want key3 then key2 — order must survive the round trip",
			[]string{names[0][1], names[1][1]})
	}
	if m := genFromName.FindStringSubmatch(names[0][1]); m == nil || m[1] != "3" {
		t.Fatalf("could not read the generation out of %q", names[0][1])
	}
}

func TestAnUnrecognisedKeyNameIsRefusedRatherThanRenumbered(t *testing.T) {
	// A key not of the form keyN cannot be ordered against the others, and inventing a
	// generation for it could produce a name that already appears in etcd — which is
	// the one thing that makes the audit unprovable.
	if genFromName.MatchString("legacy-key") {
		t.Fatal("a non-generational name was accepted as a generation")
	}
}

// THE COMPARISON THE TOOLING INVITES MUST ACTUALLY BE POSSIBLE.
//
// `kakt escrow verify` prints a fingerprint and tells the operator to "compare a
// fingerprint against the value the box is actually using". `kakt encryption rotate`
// prints one for the live configuration. Those two values reached the code in different
// encodings -- hex from the escrow, base64 from the configuration -- so the digests never
// matched, for the same key.
//
// Nothing caught it because each fingerprint was only ever compared against itself. The
// first real comparison happened when an operator ran both commands before a rotation and
// saw them disagree, which reads as the escrow holding the wrong key: the one conclusion
// that would stop a rotation that needed to happen.
func TestOneKeyHasOneFingerprintWhateverEncodingItArrivesIn(t *testing.T) {
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i * 7)
	}
	asHex := hex.EncodeToString(raw)                // how the escrow stores it
	asB64 := base64.StdEncoding.EncodeToString(raw) // how the provider configuration holds it

	want := keyFingerprint(raw)
	for name, got := range map[string]string{
		"hex (escrow)":            keyFingerprint([]byte(asHex)),
		"base64 (configuration)":  keyFingerprint([]byte(asB64)),
		"raw (freshly generated)": want,
	} {
		if got != want {
			t.Errorf("%s fingerprints as %s, want %s — the same key must have one digest, "+
				"or comparing the escrow against the live configuration is meaningless",
				name, got, want)
		}
	}
}

func TestTwoDifferentKeysStillDiffer(t *testing.T) {
	// The reduction must not be so eager that it collapses distinct keys — a fingerprint
	// that matched everything would be worse than one that matched nothing.
	a := make([]byte, 32)
	b := make([]byte, 32)
	b[31] = 1
	if keyFingerprint(a) == keyFingerprint(b) {
		t.Fatal("two different keys share a fingerprint")
	}
}

func TestANonKeyArtefactStillGetsAStableFingerprint(t *testing.T) {
	// escrow verify prints these for kubeconfigs and masterkeys too. Those are not 32
	// bytes in any encoding, and they must still produce a stable digest rather than
	// being refused — this is a diagnostic, not a validator.
	v := []byte("apiVersion: v1\nkind: Config\n")
	if keyFingerprint(v) != keyFingerprint(v) {
		t.Fatal("unstable")
	}
	if keyFingerprint(v) == keyFingerprint([]byte("something else")) {
		t.Fatal("distinct artefacts collided")
	}
}
