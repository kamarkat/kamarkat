package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/kamarkat/kamarkat/internal/platform/escrow"
	"github.com/spf13/cobra"
)

// The object-store credential is the tenant's, so the platform takes it rather than
// making it.
//
// It cannot be generated here: an access key at the provider is created in the
// tenant's own account, and a platform that invented one would be inventing an
// identity it has no authority to create. So enrolment is an explicit, one-time act,
// and the value arrives on STDIN -- never an argument, because arguments are visible
// in a process list and land in shell history.
//
// WHY THIS ONE ROTATES AND THE KEY BESIDE IT DOES NOT. Encryption and authentication
// have different lifetimes (ADR-111 §10). An SSE-C key is immutable while any backup
// written under it is retained, because a replacement cannot decrypt a single existing
// object. An access credential is the opposite: revoking and replacing it is how a
// replaced hub is fenced (§6), and doing so changes nothing about the readability of
// what is already stored. Conflating the two would either make fencing impossible or
// make the backups unreadable.
type enrollOpts struct {
	box    string
	rotate bool
}

func newEscrowEnrollStoreCredentialCmd() *cobra.Command {
	var o enrollOpts

	cmd := &cobra.Command{
		Use:   "enroll-store-credential",
		Short: "take the tenant's object-store credential on stdin and escrow it",
		Long: `Reads the object-store credential for this box's management-state backups from
stdin and preserves it in tenant-controlled external escrow.

The credential is the tenant's and is never generated here. It arrives on stdin, so it
is not in a command argument, a shell history or a process list, and it is never
printed back.

Enrolment is one-time. A credential already escrowed is left alone unless --rotate is
given, which is the supported path for replacing one -- rotating the access credential
does not affect the readability of existing backups, which depends on the SSE-C key
and not on this.

  kakt escrow enroll-store-credential --box nutgraf-hub < credential.ini`,
		RunE: func(c *cobra.Command, _ []string) error {
			return runEnrollStoreCredential(c.Context(), o, c.InOrStdin())
		},
	}

	f := cmd.Flags()
	f.StringVar(&o.box, "box", "", "the logical hub this credential belongs to (required)")
	f.BoolVar(&o.rotate, "rotate", false,
		"replace an already-escrowed credential. Deliberate, and separate from enrolment, "+
			"so an ordinary re-run can never overwrite one by accident")
	_ = cmd.MarkFlagRequired("box")
	return cmd
}

func runEnrollStoreCredential(ctx context.Context, o enrollOpts, in io.Reader) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(o.box) == "" {
		return fmt.Errorf("--box is required: the credential is the box's and outlives every " +
			"cluster in it (ADR-111 §1)")
	}

	payload, err := io.ReadAll(bufio.NewReader(in))
	if err != nil {
		return fmt.Errorf("reading the credential from stdin: %w", err)
	}
	cred := strings.TrimSpace(string(payload))
	if cred == "" {
		return fmt.Errorf("stdin was empty. The credential is read from stdin rather than a " +
			"flag, so it has to be piped in:\n\n  " +
			"kakt escrow enroll-store-credential --box <box> < credential.ini")
	}
	if err := validateStoreCredential(cred); err != nil {
		return err
	}

	store, err := escrow.NewEscrowClient(ctx, "")
	if err != nil {
		return fmt.Errorf("no escrow to enrol into: %w", err)
	}
	id := escrow.ForCluster(o.box, o.box)

	existing, err := store.RestoreArtifact(ctx, id, escrow.ArtifactBackupStoreCredential)
	if err != nil {
		// Unreadable is not absent. Enrolling over a credential that may exist is
		// how the one a successor needs gets lost.
		return fmt.Errorf("reading the escrow: %w", err)
	}
	switch {
	case existing == cred:
		fmt.Printf("[enroll] the escrowed credential for %s is already this one (%s); "+
			"nothing written\n", o.box, fingerprintData(map[string][]byte{"cloud": []byte(cred)}))
		return nil
	case existing != "" && !o.rotate:
		return fmt.Errorf("box %s already has an escrowed %s (%s), and this is a different "+
			"one (%s). Refusing to replace it: pass --rotate if replacing it is what you "+
			"intend, and revoke the old credential at the provider afterwards",
			o.box, escrow.ArtifactBackupStoreCredential,
			fingerprintData(map[string][]byte{"cloud": []byte(existing)}),
			fingerprintData(map[string][]byte{"cloud": []byte(cred)}))
	}

	if err := store.BackupArtifact(ctx, id, escrow.ArtifactBackupStoreCredential, cred); err != nil {
		return fmt.Errorf("escrow the credential: %w", err)
	}

	what := "enrolled"
	if existing != "" {
		what = "rotated"
	}
	fmt.Printf("[enroll] %s the object-store credential for %s (%s)\n",
		what, o.box, fingerprintData(map[string][]byte{"cloud": []byte(cred)}))
	if existing != "" {
		// Order matters, and an earlier version of this message had it wrong by
		// telling the operator to revoke first. Revoking before the consumer has
		// switched breaks the backups that are still using the old credential.
		fmt.Println("[enroll] the previous credential is no longer escrowed, but it is still " +
			"valid at the provider -- and must stay valid for now.\n" +
			"           Order: deliver it with `kakt escrow deliver-backup-credentials " +
			"--rotate-store-credential`,\n" +
			"           confirm Velero is using it, and revoke the previous credential at the " +
			"provider only then.")
	} else {
		fmt.Println("[enroll] deliver it with `kakt escrow deliver-backup-credentials`.")
	}
	return nil
}

// validateStoreCredential checks the shape the plugin expects without ever echoing a
// value.
//
// The plugin reads an INI credentials file: a profile section, and within it an access
// key id and a secret access key. Checked here rather than left to the first backup,
// because a malformed credential enrols silently and fails later as "the bucket is
// unreachable" -- during a recovery, which is the worst moment to be debugging a
// credential's formatting.
//
// Every error names what is missing and never what was supplied.
func validateStoreCredential(cred string) error {
	var (
		profile bool
		keyID   bool
		secret  bool
	)
	for _, line := range strings.Split(cred, "\n") {
		t := strings.TrimSpace(line)
		switch {
		case t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, ";"):
			continue
		case strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]"):
			if len(t) > 2 {
				profile = true
			}
			continue
		}
		k, v, found := strings.Cut(t, "=")
		if !found {
			continue
		}
		k = strings.ToLower(strings.TrimSpace(k))
		if strings.TrimSpace(v) == "" {
			continue
		}
		switch k {
		case "aws_access_key_id":
			keyID = true
		case "aws_secret_access_key":
			secret = true
		}
	}

	var missing []string
	if !profile {
		missing = append(missing, "a profile section such as [default]")
	}
	if !keyID {
		missing = append(missing, "a non-empty aws_access_key_id")
	}
	if !secret {
		missing = append(missing, "a non-empty aws_secret_access_key")
	}
	if len(missing) > 0 {
		return fmt.Errorf("the credential on stdin is not the INI file the plugin reads; "+
			"it is missing %s. Nothing was escrowed", strings.Join(missing, ", "))
	}
	return nil
}
