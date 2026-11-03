package infisical

import (
	"encoding/hex"
	"github.com/soloz-io/zero-ops/internal/platform/escrow"
	"testing"
)

// agentgateway hex-decodes OIDC_COOKIE_SECRET and requires exactly 32 bytes for
// AES-256-GCM. A value from the default URL-safe charset is the right LENGTH but
// the wrong alphabet, and the failure surfaces only at gateway startup as
// "Invalid character 'G' at position 0" — which names neither hex nor the key.
func TestHexKeyConsumersDeclareTheirByteLength(t *testing.T) {
	want := map[string]int{
		"agentgateway-oidc-cookie-secret": 32,
		// Zitadel measures len() of the raw string, so 16 bytes hex-encoded is
		// the 32 CHARACTERS it wants. Without this the default branch gives a
		// system secret 64 chars and Zitadel refuses to start.
		"hub-zitadel-masterkey": 16,
	}
	for key, bytes := range want {
		found := false
		for _, d := range ApplicationSecretMappings {
			if d.PasswordKey != key {
				continue
			}
			found = true
			if d.HexBytes != bytes {
				t.Errorf("%s: HexBytes=%d, want %d — a non-hex value fails to parse at startup",
					key, d.HexBytes, bytes)
			}
		}
		if !found {
			t.Errorf("%s missing from ApplicationSecretMappings — nothing would produce it", key)
		}
	}
}

// Two producers for one key means last writer wins, silently.
func TestNoDuplicateInfisicalKeys(t *testing.T) {
	seen := map[string]string{}
	claim := func(key, owner string) {
		if key == "" {
			return
		}
		if prev, ok := seen[key]; ok {
			t.Errorf("Infisical key %q produced by both %q and %q", key, prev, owner)
		}
		seen[key] = owner
	}
	for _, d := range ApplicationSecretMappings {
		claim(d.UsernameKey, "ApplicationSecretMappings:"+d.Description)
		claim(d.PasswordKey, "ApplicationSecretMappings:"+d.Description)
	}
	for _, m := range CLISecretMappings {
		claim(m.InfisicalKey, "CLISecretMappings:"+m.Description)
	}
}

// Guards the generator itself: right length, and actually decodable as hex.
func TestGeneratedHexKeysDecodeToRequestedLength(t *testing.T) {
	for _, d := range ApplicationSecretMappings {
		if d.HexBytes == 0 {
			continue
		}
		raw, err := hex.DecodeString(mustHex(t, d.HexBytes))
		if err != nil {
			t.Fatalf("%s: generated key is not valid hex: %v", d.PasswordKey, err)
		}
		if len(raw) != d.HexBytes {
			t.Errorf("%s: decoded to %d bytes, want %d", d.PasswordKey, len(raw), d.HexBytes)
		}
	}
}

// The value that was actually live on spoke-pool-hybrid-dev-01: base64 of 32
// random bytes. It carries the correct entropy, so nothing about it looks wrong
// until agentgateway hex-decodes it and dies. An existence check accepts it
// forever; only a format check repairs it.
func TestMalformedHexValuesAreDetectedNotPreserved(t *testing.T) {
	const hexBytes = 32

	cases := []struct {
		name  string
		value string
		want  bool
	}{
		{"correct hex", "3b1f" + hex.EncodeToString(make([]byte, hexBytes-2)), true},
		{"base64 of the same 32 bytes", "Zm9vYmFyYmF6cXV4Zm9vYmFyYmF6cXV4Zm9vYmFyYmE=", false},
		{"right length, non-hex alphabet", "G" + hex.EncodeToString(make([]byte, hexBytes))[1:], false},
		{"empty", "", false},
		{"half length", hex.EncodeToString(make([]byte, hexBytes/2)), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hexValueIsWellFormed(tc.value, hexBytes); got != tc.want {
				t.Fatalf("hexValueIsWellFormed(%q, %d) = %v, want %v (len=%d)",
					tc.value, hexBytes, got, tc.want, len(tc.value))
			}
		})
	}
}

// Every CellScopedKey must also declare HexBytes if its consumer hex-decodes it;
// without HexBytes the repair path above cannot run, since there is nothing to
// validate the stored form against.
func TestCellScopedKeysAreRepairable(t *testing.T) {
	for _, def := range ApplicationSecretMappings {
		if def.CellScopedKey == "" {
			continue
		}
		if def.HexBytes == 0 {
			t.Errorf("%s is cell-scoped but declares no HexBytes: a malformed value there can never be detected", def.CellScopedKey)
		}
	}
}

// ── Escrowed secrets (ADR-076, "What the escrow holds") ──────────────────────
//
// The membership test is whether a value can be regenerated WITHOUT LOSS. These
// pin the registry against that test, because every way of getting it wrong is
// silent: an unescrowed unrotatable key behaves identically until the day the
// cluster is gone, and an escrowed rotatable one adds a credential outside the box
// for no benefit.

func TestOnlyUnrotatableSecretsAreEscrowed(t *testing.T) {
	// Encodes the test as a decision per entry, so adding a secret to the registry
	// forces the question rather than inheriting a default. A new key that belongs
	// in the escrow fails here instead of being discovered during a recovery.
	regenerableWithoutLoss := map[string]bool{
		"hub-control-plane-db-password":   true,  // rotate, update the role
		"hub-centralized-db-password":     true,  // same
		"hub-zitadel-db-password":         true,  // same
		"hub-credits-db-password":         true,  // same: the ledger's records are in the database, not the credential
		"hub-grafana-db-password":         true,  // same: Grafana's state is in the database, not the credential
		"hub-billing-db-password":         true,  // set ALTER ROLE as the cluster superuser; the engine's records are in the database
		"hub-zitadel-masterkey":           false, // encrypts every column written
		"agentgateway-oidc-cookie-secret": true,  // rotating only ends sessions
		"hub-secret-encryption-key":       false, // etcd AND every etcd backup are encrypted with it
	}

	for _, d := range ApplicationSecretMappings {
		if d.PasswordKey == "" {
			continue // key-pair entries carry no password
		}
		rotatable, known := regenerableWithoutLoss[d.PasswordKey]
		if !known {
			t.Fatalf("%q is in the registry but not classified here: decide whether it can be "+
				"regenerated without loss, and escrow it if it cannot (ADR-076)", d.PasswordKey)
		}
		switch {
		case rotatable && d.EscrowArtifact != "":
			t.Fatalf("%q is rotatable but escrowed as %q; an escrow for a value that can be "+
				"regenerated is a credential outside the box for no benefit", d.PasswordKey, d.EscrowArtifact)
		case !rotatable && d.EscrowArtifact == "":
			t.Fatalf("%q cannot be regenerated without loss and is escrowed nowhere; a box that "+
				"loses its secret store cannot recover it (ADR-076)", d.PasswordKey)
		}
	}
}

func TestTheZitadelMasterkeyIsEscrowedUnderTheNameTheAdrLists(t *testing.T) {
	// The name is the contract: a recovery reads the escrow by it, and ADR-076's
	// list is checked against this constant by preflight 91. A rename on one side
	// only produces an escrow nobody restores from.
	for _, d := range ApplicationSecretMappings {
		if d.PasswordKey != "hub-zitadel-masterkey" {
			continue
		}
		if d.EscrowArtifact != escrow.ArtifactZitadelMasterkey {
			t.Fatalf("escrow artefact = %q, want %q", d.EscrowArtifact, escrow.ArtifactZitadelMasterkey)
		}
		// Unrotatable and hex-shaped: both are why it is escrowed and validated.
		if d.HexBytes != 16 {
			t.Fatalf("HexBytes = %d, want 16 (32 characters, which is what the consumer counts)", d.HexBytes)
		}
		return
	}
	t.Fatal("hub-zitadel-masterkey is no longer in the registry")
}
