package openbao

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"k8s.io/client-go/kubernetes"
)

// Escrow is the box escrow project as the trust bootstrap uses it: project-level
// artefacts under escrow.ProjectArtifactFolder. Never custody.
type Escrow = ProjectStore

// Box-project escrow artefacts written or read by this package (ADR-117 §3, §6).
const (
	ArtifactSeal                 = "openbao-seal"
	ArtifactRecoveryPGPPublic    = "openbao-recovery-pgp-public"
	ArtifactRecoveryKeyEncrypted = "openbao-recovery-key-encrypted"
	ArtifactRootCertificate      = "pki-root-certificate"
	// Custody-project artefacts: written and read only under the administrator's
	// session, never by a runtime or CI identity.
	ArtifactRootCA             = "pki-root"
	ArtifactRecoveryPGPPrivate = "openbao-recovery-pgp-private"
)

const (
	roleTrustBootstrap = "openbao-trust-bootstrap"
	rolePKISigning     = "openbao-pki-signing"
	saTrustBootstrap   = "openbao-trust-bootstrap"
	saPKISigning       = "openbao-pki-signing"
)

// EncryptedRecoveryKey is what the box escrow holds: PGP ciphertext only, inert
// without the custody-held private key.
type EncryptedRecoveryKey struct {
	Fingerprint string   `json:"fingerprint"`
	KeysBase64  []string `json:"keysBase64"`
}

// RecoveryKeySequence enrols the PGP-encrypted recovery key (ADR-117 §3), crash-safe
// and re-entrant: every step checks state before acting, and the next run resumes
// at the first step whose effect is not yet present.
//
//  1. the PGP public key is read from the box escrow (generated at kakt escrow init)
//  2. if OpenBao has no recovery key, init it encrypted to that key, with backup
//  3. read the encrypted shares back from OpenBao's backup
//  4. escrow them
//  5. verify: escrowed ciphertext equals OpenBao's backup and is bound to the
//     custody key's fingerprint
//  6. delete the backup, then the bootstrap role and policy
func RecoveryKeySequence(ctx context.Context, c *Client, esc Escrow) error {
	pub, err := esc.RestoreProjectArtifact(ctx, ArtifactRecoveryPGPPublic)
	if err != nil || strings.TrimSpace(pub) == "" {
		return fmt.Errorf("the recovery PGP public key is not in the box escrow (%s): run "+
			"`kakt escrow init` as the administrator first: %v", ArtifactRecoveryPGPPublic, err)
	}
	fp, err := PublicFingerprint(pub)
	if err != nil {
		return err
	}

	escrowed, haveEscrowed, err := readEscrowedRecovery(ctx, esc)
	if err != nil {
		return err
	}

	var status struct {
		Started  bool `json:"started"`
		Required int  `json:"required"`
	}
	if err := c.Read(ctx, "sys/rotate/recovery/init", &status); err != nil {
		return fmt.Errorf("read the recovery-key status: %w", err)
	}
	if status.Started {
		return errors.New("a recovery-key rotation is already in progress on this OpenBao, and this " +
			"bootstrap did not start it: refusing to interfere (fail closed). Inspect " +
			"sys/rotate/recovery/init before re-running")
	}

	if status.Required == 0 {
		// Step 2. No recovery key exists: this is the one moment one can be issued.
		var init struct {
			KeysBase64      []string `json:"keys_base64"`
			PGPFingerprints []string `json:"pgp_fingerprints"`
			Complete        bool     `json:"complete"`
		}
		if err := c.Write(ctx, "sys/rotate/recovery/init", map[string]any{
			"secret_shares": 1, "secret_threshold": 1, "pgp_keys": []string{pub}, "backup": true,
		}, &init); err != nil {
			return fmt.Errorf("initialize the recovery key: %w", err)
		}
		if !init.Complete || len(init.KeysBase64) != 1 {
			return errors.New("OpenBao did not issue an encrypted recovery key; check its seal configuration")
		}
	}

	// Step 3. The backup is the crash-safe copy: present until step 6 deletes it.
	backup, haveBackup, err := readRecoveryBackup(ctx, c, fp)
	if err != nil {
		return err
	}
	switch {
	case haveBackup && haveEscrowed:
		// Step 5 below verifies they agree.
	case haveBackup:
		// Step 4.
		payload, _ := json.Marshal(backup)
		if err := esc.BackupProjectArtifact(ctx, ArtifactRecoveryKeyEncrypted, string(payload)); err != nil {
			return fmt.Errorf("escrow the encrypted recovery key: %w", err)
		}
		escrowed, haveEscrowed = backup, true
	case haveEscrowed:
		// The backup was already deleted by a completed earlier run.
		if escrowed.Fingerprint != fp {
			return fmt.Errorf("the escrowed recovery key is bound to PGP key %s, not the escrowed public key %s",
				escrowed.Fingerprint, fp)
		}
		return retireTrustBootstrap(ctx, c)
	default:
		return errors.New("OpenBao has a recovery key, but neither its backup nor an escrowed copy " +
			"exists: it cannot be re-issued without the existing key. Fail closed; the store is preserved")
	}

	// Step 5.
	if escrowed.Fingerprint != fp || !sameShares(escrowed.KeysBase64, backup.KeysBase64) {
		return errors.New("the escrowed recovery key does not match OpenBao's backup for the custody " +
			"key; the backup is kept so a re-run can reconcile")
	}

	// Step 6.
	if err := c.Delete(ctx, "sys/rotate/recovery/backup"); err != nil && !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("delete the recovery-key backup: %w", err)
	}
	return retireTrustBootstrap(ctx, c)
}

func readEscrowedRecovery(ctx context.Context, esc Escrow) (EncryptedRecoveryKey, bool, error) {
	raw, err := esc.RestoreProjectArtifact(ctx, ArtifactRecoveryKeyEncrypted)
	if err != nil {
		return EncryptedRecoveryKey{}, false, fmt.Errorf("read the escrowed recovery key: %w", err)
	}
	if strings.TrimSpace(raw) == "" {
		return EncryptedRecoveryKey{}, false, nil
	}
	var k EncryptedRecoveryKey
	if err := json.Unmarshal([]byte(raw), &k); err != nil {
		return k, false, fmt.Errorf("the escrowed recovery key is malformed: %w", err)
	}
	return k, true, nil
}

func readRecoveryBackup(ctx context.Context, c *Client, fp string) (EncryptedRecoveryKey, bool, error) {
	var resp struct {
		Data struct {
			KeysBase64 map[string][]string `json:"keys_base64"`
		} `json:"data"`
		KeysBase64 map[string][]string `json:"keys_base64"`
	}
	err := c.Read(ctx, "sys/rotate/recovery/backup", &resp)
	if errors.Is(err, ErrNotFound) {
		return EncryptedRecoveryKey{}, false, nil
	}
	var ae *APIError
	if errors.As(err, &ae) && strings.Contains(ae.Body, "no backed-up keys found") {
		return EncryptedRecoveryKey{}, false, nil
	}
	if err != nil {
		return EncryptedRecoveryKey{}, false, fmt.Errorf("read the recovery-key backup: %w", err)
	}
	keys := resp.Data.KeysBase64
	if keys == nil {
		keys = resp.KeysBase64
	}
	shares, ok := keys[fp]
	if !ok || len(shares) == 0 {
		return EncryptedRecoveryKey{}, false, fmt.Errorf("the recovery-key backup is not encrypted to the "+
			"escrowed PGP key %s", fp)
	}
	return EncryptedRecoveryKey{Fingerprint: fp, KeysBase64: shares}, true, nil
}

func retireTrustBootstrap(ctx context.Context, c *Client) error {
	for _, p := range []string{"auth/kubernetes/role/" + roleTrustBootstrap, "sys/policies/acl/" + roleTrustBootstrap} {
		if err := c.Delete(ctx, p); err != nil && !errors.Is(err, ErrNotFound) {
			return fmt.Errorf("retire the trust-bootstrap identity (%s): %w", p, err)
		}
	}
	return nil
}

func sameShares(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	a, b = append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(a)
	sort.Strings(b)
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TrustBootstrap is Day-0's openbao-trust-bootstrap phase: wait for OpenBao to be
// initialized and unsealed, then run the recovery-key sequence as the one-shot
// identity. Re-running after completion is a no-op: the role no longer exists and
// the escrow already holds the key.
func TrustBootstrap(ctx context.Context, cs kubernetes.Interface, kubeconfig string, esc Escrow) error {
	c, err := WaitForStore(ctx, cs, kubeconfig, 15*time.Minute)
	if err != nil {
		return err
	}
	defer c.Close()
	_, escrowed, err := readEscrowedRecovery(ctx, esc)
	if err != nil {
		return err
	}
	if err := c.LoginAs(ctx, cs, saTrustBootstrap, roleTrustBootstrap); err != nil {
		// The role is deleted by the sequence's last step. Its absence together with
		// an escrowed recovery key is the positive record of a completed run; any
		// other login failure is an error, never "probably done".
		var ae *APIError
		if escrowed && errors.As(err, &ae) && strings.Contains(strings.ToLower(ae.Body), "invalid role name") {
			return nil
		}
		return err
	}
	return RecoveryKeySequence(ctx, c, esc)
}
