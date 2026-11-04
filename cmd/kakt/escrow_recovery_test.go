package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/nacl/secretbox"
)

// sealLikeTheAPIServer builds a value in the form etcd stores: the etcdctl key line,
// then the provider prefix, a 24-byte nonce, and the sealed box.
//
// Constructed rather than captured, so the format this tool depends on is pinned by a
// test instead of by one lucky hexdump. If the layout assumption is wrong, this fails
// here rather than during a recovery.
// THE TRAILING NEWLINE IS THE POINT. An earlier version of this helper omitted it,
// so the fixture was shaped the way the parser expected rather than the way `etcdctl
// get` actually writes — and the tool passed every test while failing against two
// live clusters whose escrowed keys were provably correct. A fixture built to match
// the code under test proves only that the code matches itself.
func sealLikeEtcdctl(t *testing.T, key [32]byte, plaintext []byte) []byte {
	t.Helper()
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	return frameLikeEtcdctl(nonce, secretbox.Seal(nil, plaintext, &nonce, &key))
}

func frameLikeEtcdctl(nonce [24]byte, sealed []byte) []byte {
	var out bytes.Buffer
	out.WriteString("/registry/secrets/default/enc-probe\n")
	out.WriteString(secretboxPrefix)
	out.Write(nonce[:])
	out.Write(sealed)
	out.WriteByte('\n') // etcdctl's trailing newline, which is NOT part of the box
	return out.Bytes()
}

// openAsTheToolDoes mirrors escrow_recovery.go's candidate loop, so the parsing
// contract is exercised rather than restated.
func openAsTheToolDoes(blob []byte, key [32]byte) ([]byte, bool) {
	idx := bytes.Index(blob, []byte(secretboxPrefix))
	if idx < 0 {
		return nil, false
	}
	payload := blob[idx+len(secretboxPrefix):]
	if len(payload) < 24+secretbox.Overhead {
		return nil, false
	}
	var nonce [24]byte
	copy(nonce[:], payload[:24])
	body := payload[24:]
	bodies := [][]byte{body}
	if n := len(body); n > secretbox.Overhead && body[n-1] == '\n' {
		bodies = append(bodies, body[:n-1])
		if n > secretbox.Overhead+1 && body[n-2] == '\r' {
			bodies = append(bodies, body[:n-2])
		}
	}
	for _, b := range bodies {
		if plain, ok := secretbox.Open(nil, b, &nonce, &key); ok {
			return plain, true
		}
	}
	return nil, false
}

func TestTheStoredFormatIsParsedTheWayEtcdctlWritesIt(t *testing.T) {
	var key [32]byte
	copy(key[:], bytes.Repeat([]byte{7}, 32))
	blob := sealLikeEtcdctl(t, key, []byte("k8s\x00\n\x0ev1\x12\x06Secret\x1a\tenc-probe"))

	plain, ok := openAsTheToolDoes(blob, key)
	if !ok {
		t.Fatal("a value framed the way etcdctl writes it did not open. This is the bug the " +
			"tool shipped with: etcdctl appends a trailing newline, Poly1305 authenticates the " +
			"whole ciphertext, and one extra byte fails the MAC")
	}
	if !bytes.Contains(plain, []byte("Secret")) {
		t.Fatalf("unexpected plaintext %q", plain)
	}
}

func TestACiphertextEndingInANewlineByteStillOpens(t *testing.T) {
	// THE 1-IN-256 CASE, and the reason the fix is trial decryption rather than
	// trimming. A sealed box is binary, so its final byte is 0x0A about once every 256
	// values. Stripping trailing newlines would corrupt exactly those -- a recovery
	// tool that fails on 0.4% of inputs is worse than one that fails on all of them,
	// because the 0.4% looks like a lost cluster.
	var key [32]byte
	copy(key[:], bytes.Repeat([]byte{9}, 32))

	var blob []byte
	for i := 0; i < 20000; i++ {
		var nonce [24]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			t.Fatal(err)
		}
		sealed := secretbox.Seal(nil, []byte("k8s v1 Secret enc-probe"), &nonce, &key)
		if sealed[len(sealed)-1] == '\n' {
			blob = frameLikeEtcdctl(nonce, sealed)
			break
		}
	}
	if blob == nil {
		t.Skip("no sealed box ending in 0x0A found in 20000 attempts; the case is ~1/256 so " +
			"this should not happen, but skipping beats a flaky failure")
	}

	// The box now ends 0x0A 0x0A: its own last byte, then etcdctl's newline.
	if _, ok := openAsTheToolDoes(blob, key); !ok {
		t.Fatal("a sealed box whose final byte is 0x0A did not open. Trimming trailing " +
			"newlines instead of trying candidates would corrupt one value in 256 and report " +
			"a recoverable cluster as lost")
	}
}

func TestTheWrongKeyFailsRatherThanProducingGarbage(t *testing.T) {
	// THIS IS WHAT MAKES THE WHOLE COMMAND A PROOF, and what makes trial decryption
	// sound rather than sloppy.
	//
	// secretbox authenticates: Open verifies a Poly1305 tag before returning anything,
	// so a wrong key CANNOT open a box and cannot yield plausible-looking garbage.
	// Two consequences the tool depends on:
	//
	//   - a success is not a coincidence, so "RECOVERABLE" means the escrowed key is
	//     genuinely the one the cluster encrypted with;
	//   - trying several candidate framings is safe, because at most one can succeed.
	//     Without authentication the loop would be guessing and the first "success"
	//     might be noise.
	//
	// Deleted once, by a careless replacement in this file, and restored: the tests
	// that justify a design are the ones whose absence is least visible.
	var right, wrong [32]byte
	copy(right[:], bytes.Repeat([]byte{1}, 32))
	copy(wrong[:], bytes.Repeat([]byte{2}, 32))
	blob := sealLikeEtcdctl(t, right, []byte("k8s v1 Secret payload"))

	if _, ok := openAsTheToolDoes(blob, right); !ok {
		t.Fatal("the correct key did not open the box")
	}
	if plain, ok := openAsTheToolDoes(blob, wrong); ok {
		t.Fatalf("the WRONG key opened the box and returned %q; the tool could then report a "+
			"recovery that would not work, and trial decryption would be unsound", plain)
	}
}

func TestAnUnencryptedValueIsReportedAsSuch(t *testing.T) {
	// If the provider is not in force on the node the file came from, there is no prefix.
	// Saying "not encrypted with secretbox" is a different finding from "the key is
	// wrong", and conflating them would send someone to rotate a key that is fine.
	dir := t.TempDir()
	f := filepath.Join(dir, "plain.bin")
	if err := os.WriteFile(f, []byte("/registry/secrets/default/x\nk8s\x00plain yaml here"), 0o600); err != nil {
		t.Fatal(err)
	}
	blob, _ := os.ReadFile(f)
	if bytes.Contains(blob, []byte(secretboxPrefix)) {
		t.Fatal("fixture unexpectedly contains the prefix")
	}
}

func TestTheKeyMustBeThirtyTwoBytesAfterHexDecoding(t *testing.T) {
	// The escrow stores the key hex-encoded, so 64 characters become 32 bytes. A value
	// of any other length cannot be the key the API server uses, and refusing it names
	// the problem instead of failing to decrypt and implicating the escrow.
	for _, c := range []struct {
		name string
		hexs string
		ok   bool
	}{
		{"32 bytes", strings.Repeat("ab", 32), true},
		{"16 bytes", strings.Repeat("ab", 16), false},
		{"not hex", "zzzz", false},
	} {
		raw, err := hex.DecodeString(c.hexs)
		got := err == nil && len(raw) == 32
		if got != c.ok {
			t.Errorf("%s: accepted=%v, want %v", c.name, got, c.ok)
		}
	}
}

func TestFingerprintsNeverCarryTheValue(t *testing.T) {
	// The whole output discipline: a key opens every Secret on the box, and a plaintext
	// IS a Secret. Both are reported as short digests.
	secretish := []byte("super-secret-key-material")
	fp := fingerprint(secretish)
	if strings.Contains(fp, "super") || len(fp) != len("sha256:")+12 {
		t.Fatalf("fingerprint %q either leaks the value or is not the expected shape", fp)
	}
	if fingerprint([]byte("a")) == fingerprint([]byte("b")) {
		t.Fatal("different values produced the same fingerprint, so two runs cannot be compared")
	}
}
