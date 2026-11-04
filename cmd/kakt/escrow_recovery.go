package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/nacl/secretbox"

	"github.com/kamarkat/kamarkat/internal/platform/escrow"
)

// newEscrowVerifyRecoveryCmd proves the escrowed encryption key can actually DECRYPT
// a cluster, rather than merely being present under the right name.
//
// WHY PRESENCE IS NOT THE PROPERTY THAT MATTERS.
//
// `kakt escrow verify` answers "is there a value under secret-encryption-key, and
// what is its fingerprint". That is worth asserting and it is not recovery. Both
// clusters' etcd is now encrypted (ADR-003 section 6), so on a rebuild the escrowed
// key is the only thing standing between a restored etcd and total loss -- and the
// question nobody has answered is whether the value in the escrow is the key the API
// server is actually using. A 64-byte string under the right name that decrypts
// nothing looks identical in a verify listing.
//
// The failure mode is the worst shape available: it is discovered during a recovery,
// which is the one moment it cannot be repaired. ADR-076 makes the escrow mandatory
// for exactly that reason, and this closes the gap between "escrowed" and "usable".
//
// So this takes one value as etcd stores it, and decrypts it with the escrowed key.
// If that works, the escrow recovers the cluster. If it does not, the escrow holds
// something else and we would rather know now.
//
// IT PRINTS NEITHER THE KEY NOR THE PLAINTEXT. The key opens every Secret on the box
// and the plaintext is a Secret. Both are reported as sha256 fingerprints, which is
// enough to compare two runs and useless to anyone reading the terminal over a
// shoulder or a transcript afterwards.
func newEscrowVerifyRecoveryCmd() *cobra.Command {
	var (
		box        string
		cluster    string
		fromFile   string
		expectKind bool
	)
	cmd := &cobra.Command{
		Use:   "verify-recovery",
		Short: "Prove the escrowed encryption key decrypts this cluster's stored Secrets",
		Long: `Decrypt one value, exactly as etcd stores it, using the key held in the escrow.

Presence is not recovery. ` + "`escrow verify`" + ` reports that a value exists under
secret-encryption-key; this reports whether that value is the key the cluster's API
server is actually encrypting with. A key that decrypts nothing is indistinguishable
from a working one in a presence check, and the difference surfaces during a rebuild,
which is the one moment it cannot be fixed.

Obtain the input on a control-plane node, where it never leaves the host:

  kubectl -n kube-system exec etcd-$NODE -- sh -c 'ETCDCTL_API=3 etcdctl \
    --cacert /etc/kubernetes/pki/etcd/ca.crt \
    --cert /etc/kubernetes/pki/etcd/server.crt \
    --key /etc/kubernetes/pki/etcd/server.key \
    get /registry/secrets/default/enc-probe' > /tmp/probe.bin

Use a PROBE Secret you created for this, not a real one: the file is ciphertext, but
it is one command away from plaintext and there is no reason for it to be a credential.

Neither the key nor the decrypted value is printed.`,
		RunE: func(c *cobra.Command, _ []string) error {
			return runEscrowVerifyRecovery(c.Context(), box, cluster, fromFile, expectKind)
		},
	}
	f := cmd.Flags()
	f.StringVar(&cluster, "cluster", "", "cluster whose escrowed key to test (required)")
	f.StringVar(&box, "box", "", "the logical hub the escrow is keyed by; box-scoped artefacts live under it (required)")
	f.StringVar(&fromFile, "from", "", "file holding one etcd value, as etcdctl wrote it (required)")
	f.BoolVar(&expectKind, "expect-secret", true,
		"also require the decrypted bytes to look like a serialised Secret, which proves the "+
			"key produced the right plaintext rather than merely succeeding")
	_ = cmd.MarkFlagRequired("cluster")
	_ = cmd.MarkFlagRequired("box")
	_ = cmd.MarkFlagRequired("from")
	return cmd
}

// secretboxPrefix is what the API server writes ahead of the nonce and ciphertext.
// The trailing key name must match the one in the provider configuration.
const secretboxPrefix = "k8s:enc:secretbox:v1:key1:"

func fingerprint(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])[:12]
}

// keyFingerprint fingerprints KEY MATERIAL, always from its raw bytes.
//
// THE WHOLE POINT OF PRINTING A FINGERPRINT IS COMPARING TWO OF THEM, and that stopped
// being possible because the same 32 bytes reach different commands in different
// encodings. `kakt escrow verify` read the escrow's hex string, `rotate` read the
// configuration's base64, and `rotate` fingerprinted a freshly generated key from its raw
// bytes -- three representations, three different digests, one key.
//
// Found when an operator compared the escrow against the live configuration before a
// rotation and the values disagreed. Nothing was wrong with the key; the comparison the
// tooling invites was meaningless, which is worse than printing nothing: it reads as
// evidence that the escrow holds the wrong key, on the one procedure where believing that
// would stop a rotation that needed to happen.
//
// Accepts base64, hex, or raw, and reduces all three to the same digest. A value that is
// none of those is fingerprinted as-is rather than refused -- this is a diagnostic, and an
// artefact that is not key material still deserves a stable digest.
func keyFingerprint(v []byte) string {
	if raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(v))); err == nil && len(raw) == 32 {
		return fingerprint(raw)
	}
	if raw, err := hex.DecodeString(strings.TrimSpace(string(v))); err == nil && len(raw) == 32 {
		return fingerprint(raw)
	}
	return fingerprint(v)
}

func runEscrowVerifyRecovery(ctx context.Context, box, cluster, fromFile string, expectKind bool) error {
	if ctx == nil {
		ctx = context.Background()
	}

	store, err := escrow.NewEscrowClient(ctx, "")
	if err != nil {
		return fmt.Errorf("reaching the escrow: %w", err)
	}
	keyHex, err := store.RestoreArtifact(ctx, escrow.ForCluster(box, cluster), escrow.ArtifactSecretEncryptionKey)
	if err != nil {
		return fmt.Errorf("reading %s for %q from the escrow: %w",
			escrow.ArtifactSecretEncryptionKey, cluster, err)
	}
	if keyHex == "" {
		return fmt.Errorf("the escrow holds no %s for %q, so this cluster cannot be recovered "+
			"at all: a restored etcd would be undecryptable (ADR-076)",
			escrow.ArtifactSecretEncryptionKey, cluster)
	}

	raw, err := hex.DecodeString(keyHex)
	if err != nil || len(raw) != 32 {
		return fmt.Errorf("the escrowed key is %d bytes after hex-decoding (error: %v); secretbox "+
			"takes exactly 32, so this value cannot be the key the API server is using",
			len(raw), err)
	}
	var key [32]byte
	copy(key[:], raw)
	fmt.Printf("[recovery] escrowed key for %q: %s (32 bytes)\n", cluster, fingerprint(raw))

	blob, err := os.ReadFile(fromFile)
	if err != nil {
		return fmt.Errorf("reading %s: %w", fromFile, err)
	}

	// etcdctl writes the KEY on one line and the VALUE after it. Only the value is
	// encrypted, so the prefix is located rather than assumed to start the file.
	idx := bytes.Index(blob, []byte(secretboxPrefix))
	if idx < 0 {
		return fmt.Errorf("%s contains no %q, so this value is NOT encrypted with secretbox. "+
			"Either the provider is not in force on the node this came from, or the file holds "+
			"something other than an etcd value", fromFile, secretboxPrefix)
	}
	payload := blob[idx+len(secretboxPrefix):]
	if len(payload) < 24+secretbox.Overhead {
		return fmt.Errorf("only %d bytes follow the prefix, which cannot hold a 24-byte nonce "+
			"and a sealed box", len(payload))
	}

	var nonce [24]byte
	copy(nonce[:], payload[:24])

	// TRIAL DECRYPTION, AND WHY IT IS SOUND HERE RATHER THAN SLOPPY.
	//
	// `etcdctl get` prints the key, a newline, the value, and a TRAILING NEWLINE. That
	// last byte is not part of the sealed box, and Poly1305 authenticates the whole
	// ciphertext -- so one extra byte fails the MAC and this tool reported a correct
	// escrow as unrecoverable. Found by running it against two live clusters whose
	// escrowed keys were provably right.
	//
	// The obvious fix -- strip trailing newlines from the payload -- is wrong. The
	// sealed box is binary, so its final byte is 0x0A about one time in 256, and
	// trimming would corrupt a correct value at that rate. A bug that appears in 0.4%
	// of runs of a recovery tool is worse than one that appears always.
	//
	// So each candidate framing is TRIED. That is legitimate precisely because
	// secretbox is authenticated: Open verifies the MAC, so at most one candidate can
	// succeed and a success cannot be a coincidence. This is not guessing -- it is
	// asking the cryptography which framing was right, and it is the same property
	// that makes this command a proof of recovery at all.
	type candidate struct {
		what string
		body []byte
	}
	body := payload[24:]
	candidates := []candidate{{"as written", body}}
	if n := len(body); n > secretbox.Overhead && body[n-1] == '\n' {
		candidates = append(candidates, candidate{"less etcdctl's trailing newline", body[:n-1]})
		if n > secretbox.Overhead+1 && body[n-2] == '\r' {
			candidates = append(candidates, candidate{"less a trailing CRLF", body[:n-2]})
		}
	}

	var plain []byte
	var ok bool
	for _, c := range candidates {
		if plain, ok = secretbox.Open(nil, c.body, &nonce, &key); ok {
			if c.what != "as written" {
				fmt.Printf("[recovery] opened the box %s\n", c.what)
			}
			break
		}
	}
	if !ok {
		return fmt.Errorf("the escrowed key did NOT decrypt this value.\n\n"+
			"  The escrow holds a 32-byte value under %s for %q and it is not the key this\n"+
			"  cluster's API server encrypted with. A rebuild from this escrow would restore an\n"+
			"  etcd nobody can read. This is the gap ADR-076 exists to prevent and it cannot be\n"+
			"  closed after the cluster is gone.\n\n"+
			"  Check that --cluster names the cluster the value came from before concluding the\n"+
			"  escrow is wrong: the spoke's key does not decrypt the hub's.",
			escrow.ArtifactSecretEncryptionKey, cluster)
	}

	fmt.Printf("[recovery] decrypted %d bytes: %s\n", len(plain), fingerprint(plain))

	if expectKind {
		// Succeeding is not quite enough: secretbox authenticates, so a wrong key fails
		// rather than producing garbage -- but a value that decrypts to something which is
		// not a Secret would mean the file held the wrong object, and reporting that as a
		// recovery proof would be wrong.
		if !bytes.Contains(plain, []byte("v1")) || !bytes.Contains(plain, []byte("Secret")) {
			return fmt.Errorf("the value decrypted but does not look like a serialised Secret, "+
				"so %s probably holds a different object. The key is fine; the input is not",
				fromFile)
		}
		fmt.Println("[recovery] the plaintext is a serialised v1 Secret")
	}

	fmt.Println()
	fmt.Printf("RECOVERABLE  the escrowed %s for %q decrypts what this cluster stored.\n",
		escrow.ArtifactSecretEncryptionKey, cluster)
	fmt.Println("A rebuild restores an etcd this key can read, which is what ADR-076 promises.")
	return nil
}
