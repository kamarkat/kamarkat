package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/hubaccess"
	"github.com/kamarkat/kamarkat/internal/platform/escrow"
	"github.com/spf13/cobra"
)

// The credentials a box is built with, enrolled one at a time.
//
// `kakt box init` enrols all of them from the values it collects, so these exist
// for the two cases it cannot serve: adding a capability to a box that already
// runs, and rotating a credential whose source has issued a new one.
//
// They were documented before they existed. Day-0's refusal named
// `kakt escrow enroll-registry-credential` and the CLI reference described all
// four, while only the provider and store credentials had commands -- so a box
// that declined private images told its operator to run something that did not
// exist. These are those commands.
//
// Each reads the value on STDIN and never from an argument: an argument is
// visible in the process list for as long as the call runs, and it lands in shell
// history. Each is one-time, refusing to replace a different credential unless
// --rotate says so, because an ordinary re-run must never repoint a box.
type enrollBoxOpts struct {
	box    string
	rotate bool
}

// boxCredentialCommand describes one enrolment command.
type boxCredentialCommand struct {
	use      string
	artifact string
	short    string
	// long says what the credential is for and what a box without it does, so
	// `--help` answers "do I need this?" rather than only "what is the syntax".
	long string
	// fields are the `key=value` names a multi-value credential must carry. Empty
	// for a single value -- a URL or an auth key is the whole payload.
	fields []string
	// example is the shape of a correct stdin, shown in --help.
	example string
}

func boxCredentialCommands() []boxCredentialCommand {
	return []boxCredentialCommand{
		{
			use:      "enroll-registry-credential",
			artifact: escrow.ArtifactRegistryCredential,
			short:    "take the registry credential that pulls this tenant's private images",
			long: `Escrow the credential that pulls this tenant's own private images.

The platform's images are public, so a box without this one builds and runs; what
it cannot do is pull the tenant's private packages (ADR-066: the workloads are
theirs). Day-0 reports the capability as absent rather than refusing.`,
			fields:  []string{escrow.FieldRegistryUser, escrow.FieldRegistryToken},
			example: "username=acme-bot\ntoken=ghp_...",
		},
		{
			use:      "enroll-alert-destination",
			artifact: escrow.ArtifactAlertDestination,
			short:    "take the URLs this box's alerts page through",
			long: `Escrow where this box's critical and warning alerts page, and the heartbeat
that pages when the management cluster stops reporting (ADR-078 add.5 §5).

Metrics, logs and dashboards run inside the box and need none of this. Without it
the box collects everything and sends nothing anywhere remote, which a tenant may
well intend.`,
			fields:  []string{escrow.FieldAlertmanagerURL, escrow.FieldHeartbeatURL},
			example: "alertmanager_url=https://...\nheartbeat_url=https://...",
		},
		{
			use:      "enroll-mail-transport",
			artifact: escrow.ArtifactMailTransportURL,
			short:    "take this box's outbound mail transport URL",
			long: `Escrow the transport this box sends mail through: the identity provider's
verification and password-reset messages, and alert email.

One value, the whole URL. The identity provider needs it while it is being
created, which is before anything inside the box can hold it -- which is why it
is escrowed rather than kept in the box's own store.`,
			example: "smtps://user:key@smtp.example.test:465",
		},
		{
			use:      "enroll-tailnet-authkey",
			artifact: escrow.ArtifactTailnetAuthkey,
			short:    "take the tailnet auth key on-premises nodes join with",
			long: `Escrow the auth key this box's on-premises nodes join the tenant's tailnet with.

On-premises nodes are capacity for a managed cluster serving dev (ADR-075), and
that cluster's own declaration lists the machines. This is the key they
authenticate with, escrowed because a control plane joins the tailnet from
cloud-init during its first boot, with no cluster yet to read a Secret from.`,
			example: "tskey-auth-...",
		},
	}
}

func newEscrowEnrollBoxCredentialCmds() []*cobra.Command {
	var cmds []*cobra.Command
	for _, c := range boxCredentialCommands() {
		c := c
		var o enrollBoxOpts
		cmd := &cobra.Command{
			Use:          c.use,
			Short:        c.short,
			Long:         c.long + "\n\nRead from stdin:\n\n  " + strings.ReplaceAll(c.example, "\n", "\n  "),
			SilenceUsage: true,
			RunE: func(cc *cobra.Command, _ []string) error {
				return runEnrollBoxCredential(cc.Context(), c, o, cc.InOrStdin())
			},
		}
		f := cmd.Flags()
		f.StringVar(&o.box, "box", "", "the box this credential belongs to (required)")
		f.BoolVar(&o.rotate, "rotate", false,
			"replace an already-escrowed credential. Deliberate, and separate from enrolment, "+
				"so an ordinary re-run can never overwrite one by accident")
		_ = cmd.MarkFlagRequired("box")
		cmds = append(cmds, cmd)
	}
	return cmds
}

// enrollThroughStore applies EnrollBoxCredential's rules to a store opened from
// the environment.
//
// The same five: the box on both halves of the identity, unreadable is not
// absent, identical bytes are unchanged rather than rotated, different bytes are
// refused without --rotate, and the scope must be declared rather than defaulted.
// hubaccess.EnrollBoxCredential is the path `box init` takes, where a declaration
// supplies the coordinates and the binding is established in the same act; this
// is the same contract for a caller that has only a box id.
func enrollThroughStore(ctx context.Context, store escrow.EscrowClient, box, artifact, cred string, rotate bool) (hubaccess.Enrolment, error) {
	if strings.TrimSpace(box) == "" {
		return hubaccess.EnrolledFirst, fmt.Errorf("--box is required: the credential is the " +
			"box's and outlives every cluster in it (ADR-111 §1)")
	}
	if _, declared := escrow.DeclaredScope(artifact); !declared {
		return hubaccess.EnrolledFirst, fmt.Errorf("%s has no declared escrow scope, so where "+
			"it would be written is an accident of the default", artifact)
	}
	id := escrow.ForCluster(box, box)
	existing, err := store.RestoreArtifact(ctx, id, artifact)
	if err != nil {
		return hubaccess.EnrolledFirst, fmt.Errorf("reading the escrowed %s for box %s: %w",
			artifact, box, err)
	}
	switch {
	case strings.TrimSpace(existing) == cred:
		return hubaccess.EnrolledUnchanged, nil
	case strings.TrimSpace(existing) != "" && !rotate:
		return hubaccess.EnrolledUnchanged, fmt.Errorf("box %s already has an escrowed %s, and "+
			"this is a different one.\n\nRefusing to replace it: pass --rotate if that is what "+
			"you intend, and revoke the old credential at its source afterwards", box, artifact)
	}
	if err := store.BackupArtifact(ctx, id, artifact, cred); err != nil {
		return hubaccess.EnrolledFirst, fmt.Errorf("escrow %s for box %s: %w", artifact, box, err)
	}
	if strings.TrimSpace(existing) != "" {
		return hubaccess.EnrolledRotated, nil
	}
	return hubaccess.EnrolledFirst, nil
}

func runEnrollBoxCredential(ctx context.Context, c boxCredentialCommand, o enrollBoxOpts, in io.Reader) error {
	if ctx == nil {
		ctx = context.Background()
	}
	payload, err := io.ReadAll(bufio.NewReader(in))
	if err != nil {
		return fmt.Errorf("reading the credential from stdin: %w", err)
	}
	cred := strings.TrimSpace(string(payload))
	if cred == "" {
		return fmt.Errorf("stdin was empty. The credential is read from stdin rather than a "+
			"flag, so it has to be piped in:\n\n  printf '%s' | kakt escrow %s --box %s",
			strings.ReplaceAll(c.example, "\n", "\\n"), c.use, o.box)
	}
	// All of its fields or none, checked here rather than at use: a credential
	// built from half of what it needs authenticates as nobody, and does so at
	// the moment a workload needs it rather than at the moment it was supplied.
	if len(c.fields) > 0 {
		if missing := escrow.RequireFields(cred, c.fields...); len(missing) > 0 {
			return fmt.Errorf("this credential is missing %s.\n\nIt is read as `key=value` "+
				"lines, one per field:\n\n  %s", strings.Join(missing, ", "),
				strings.ReplaceAll(c.example, "\n", "\n  "))
		}
	}

	// The escrow from the environment, and the box from the flag -- the same two
	// inputs `enroll-store-credential` takes. There is no declaration to read
	// here: this runs for a box whose repository the operator may not have
	// checked out, which is the case it exists for.
	store, err := escrow.NewEscrowClient(ctx, "")
	if err != nil {
		return fmt.Errorf("no escrow to enrol into: %w", err)
	}
	outcome, err := enrollThroughStore(ctx, store, o.box, c.artifact, cred, o.rotate)
	if err != nil {
		return err
	}
	switch outcome {
	case hubaccess.EnrolledUnchanged:
		fmt.Printf("[enroll] the escrowed %s for %s is already this one; nothing written\n",
			c.artifact, o.box)
	case hubaccess.EnrolledRotated:
		fmt.Printf("[enroll] ✓ rotated %s for %s. Revoke the previous credential at its source\n",
			c.artifact, o.box)
	default:
		fmt.Printf("[enroll] ✓ escrowed %s for %s\n", c.artifact, o.box)
	}
	return nil
}
