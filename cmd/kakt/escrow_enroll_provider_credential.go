package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/hubaccess"
	"github.com/spf13/cobra"
)

// The provider credential is the tenant's, like the object-store credential this
// mirrors, and it is escrowed for the same reason: the operations that need it
// most are the ones where the box cannot be asked.
//
// WHY IT HAS TO BE IN THE ESCROW. Destroying a box's servers is a call to the
// provider's API, and until now the credential for that lived in HCLOUD_TOKEN or
// in k8-secrets/hetzner/token on whichever machine bootstrapped the box. So the
// one workstation that could delete the servers was the one that created them,
// and a teardown from anywhere else printed "skipping Hetzner cloud resource
// cleanup" and reported success with every server still running.
//
// It is also the ordering the brief insists on: the credential must be enrolled
// BEFORE a teardown makes its original source unreachable. The in-cluster
// hetzner-credentials Secret dies with the cluster.
type enrollProviderOpts struct {
	box      string
	cluster  string
	provider string
	gitops   string
	rotate   bool
}

func newEscrowEnrollProviderCredentialCmd() *cobra.Command {
	var o enrollProviderOpts

	cmd := &cobra.Command{
		Use:          "enroll-provider-credential",
		Short:        "take this box's cloud credential on stdin and escrow it",
		SilenceUsage: true,
		Long: `Reads the cloud provider credential this box was built with from stdin and
preserves it in the box's escrow.

Lifecycle operations read it from there, so a box can be inspected or destroyed from
any authorized workstation rather than only from the one that bootstrapped it. Enrol it
while the box is healthy: a teardown is exactly when its other copies -- the in-cluster
Secret, the repository's CI secret -- stop being reachable.

The credential arrives on stdin, so it is not in a command argument, a shell history or
a process list, and it is never printed back.

  kakt escrow enroll-provider-credential --gitops-dir . --cluster nutgraf-hub < token`,
		RunE: func(c *cobra.Command, _ []string) error {
			return runEnrollProviderCredential(c.Context(), o, c.InOrStdin())
		},
	}

	f := cmd.Flags()
	f.StringVar(&o.gitops, "gitops-dir", "",
		"the box's repository, which declares its escrow and its box id")
	f.StringVar(&o.cluster, "cluster", "", "the cluster whose declaration to read (required)")
	f.StringVar(&o.box, "box", "", "the logical hub, if it is not the one the declaration names")
	f.StringVar(&o.provider, "provider", "hetzner", "the provider this credential is for")
	f.BoolVar(&o.rotate, "rotate", false,
		"replace an already-escrowed credential. Deliberate, and separate from enrolment, "+
			"so an ordinary re-run can never overwrite one by accident")
	_ = cmd.MarkFlagRequired("cluster")
	return cmd
}

func runEnrollProviderCredential(ctx context.Context, o enrollProviderOpts, in io.Reader) error {
	if ctx == nil {
		ctx = context.Background()
	}
	provider := strings.ToLower(strings.TrimSpace(o.provider))
	if provider == "" {
		return fmt.Errorf("--provider is required: one artefact per provider, because a box " +
			"may buy capacity from more than one and a single name would overwrite the first")
	}

	payload, err := io.ReadAll(bufio.NewReader(in))
	if err != nil {
		return fmt.Errorf("reading the credential from stdin: %w", err)
	}
	cred := strings.TrimSpace(string(payload))
	if cred == "" {
		return fmt.Errorf("stdin was empty. The credential is read from stdin rather than a "+
			"flag, so it has to be piped in:\n\n  "+
			"kakt escrow enroll-provider-credential --gitops-dir . --cluster %s < token", o.cluster)
	}

	// The escrow is reached the way every other hub-facing command reaches it:
	// the box declares where it is, and the operator's own login authorizes the
	// write. The environment's machine identity still wins when it is set, so
	// automation enrols the same way.
	boxID, clusterName := strings.TrimSpace(o.box), strings.TrimSpace(o.cluster)
	coords := hubaccess.Coordinates{Identity: hubaccess.Identity{BoxID: boxID, ClusterName: clusterName}}
	if strings.TrimSpace(o.gitops) != "" {
		declared, declErr := hubaccess.Declaration(o.gitops, clusterName)
		if declErr != nil {
			return declErr
		}
		coords = declared
		if boxID != "" {
			coords.BoxID = boxID
		}
	} else {
		// Without a declaration the coordinates come from the environment, which
		// is the automation path. A human should pass --gitops-dir and let the box
		// say where its escrow is.
		if boxID == "" {
			return fmt.Errorf("--box is required without --gitops-dir: artefacts are escrowed " +
				"under the logical hub (ADR-111 §1), and it cannot be inferred from the cluster name")
		}
		coords.EscrowURL = escrowEnvValue("ESCROW_PROVIDER_URL")
		coords.EscrowProjectID = escrowEnvValue("ESCROW_PROVIDER_PROJECT_ID")
	}

	// One implementation, in hubaccess: this command and `kakt box init` enrol the
	// same way, so a box created by either is in the same state afterwards.
	outcome, err := hubaccess.EnrollProviderCredential(ctx, coords, provider, cred, o.rotate)
	if err != nil {
		return err
	}
	boxID = coords.BoxID
	switch outcome {
	case hubaccess.EnrolledUnchanged:
		fmt.Printf("[enroll] the escrowed %s credential for %s is already this one; nothing written\n",
			provider, boxID)
		return nil
	case hubaccess.EnrolledRotated:
		fmt.Printf("[enroll] rotated the %s credential for %s (%d chars)\n", provider, boxID, len(cred))
	default:
		fmt.Printf("[enroll] enrolled the %s credential for %s (%d chars)\n", provider, boxID, len(cred))
	}
	fmt.Println("  Lifecycle commands read it from the escrow now, so this box can be operated")
	fmt.Println("  from any authorized workstation.")
	return nil
}

// escrowEnvValue is the automation path's coordinates, read the way the escrow
// client reads them.
func escrowEnvValue(name string) string { return strings.TrimSpace(os.Getenv(name)) }
