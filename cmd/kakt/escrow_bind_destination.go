package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/hubaccess"
	"github.com/kamarkat/kamarkat/internal/platform/escrow"
	"github.com/spf13/cobra"
)

// newEscrowBindDestinationCmd establishes which escrow a box belongs to, inside
// that escrow.
//
// The coordinates are committed to the repository because they are names rather
// than credentials -- that is what lets any authorized workstation reach the box.
// But a committed field is editable, and an edited one would send the next
// privileged write to a project the editor controls. Nothing about that is
// visible afterwards: every later read succeeds, against the new destination.
//
// So the destination is recorded in the destination, once, by an authorized
// operator. A redirect then has nothing to show for itself, and a declaration
// that disagrees with its own escrow is refused rather than followed.
func newEscrowBindDestinationCmd() *cobra.Command {
	var gitopsDir, cluster string
	var rebind bool

	cmd := &cobra.Command{
		Use:          "bind-destination",
		Short:        "Establish which escrow project this box's recovery material belongs to",
		SilenceUsage: true,
		Long: `Record this box's escrow coordinates inside that escrow.

Run once per box, as the platform administrator, before any recovery material is
written. It records the expected escrow destination and detects configuration
inconsistencies: a project that was never bound, or a declaration edited without
a deliberate rebind. It does not independently prevent an authorized Git change
from redirecting escrow access -- both values come from the project the
repository names. Membership of the escrow project, and review on the repository
carrying the declaration, are what do that.

--rebind replaces an existing binding, which is how a deliberate move to another
escrow project is authorized. It is separate from the first binding for the same
reason enrolment is separate from rotation: an ordinary re-run must not be able
to repoint a box.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runEscrowBindDestination(cmd.Context(), gitopsDir, cluster, rebind)
		},
	}
	f := cmd.Flags()
	f.StringVar(&gitopsDir, "gitops-dir", ".", "the box's repository, which declares its escrow")
	f.StringVar(&cluster, "cluster", "", "the cluster whose declaration to read (required)")
	f.BoolVar(&rebind, "rebind", false,
		"replace an existing binding, authorizing a move to the declared coordinates")
	_ = cmd.MarkFlagRequired("cluster")
	return cmd
}

func runEscrowBindDestination(ctx context.Context, gitopsDir, cluster string, rebind bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	coords, err := hubaccess.Declaration(gitopsDir, cluster)
	if err != nil {
		return err
	}

	// Opened WITHOUT the destination check, because this command is what
	// establishes it. Everything else about the identity still applies: the
	// session is the administrator's, and the escrow decides what it may write.
	access, err := hubaccess.OpenUnbound(ctx, coords)
	if err != nil {
		return err
	}
	id := hubaccess.Identity{BoxID: coords.BoxID, ClusterName: coords.BoxID}

	existing, err := access.Artifact(ctx, id, escrow.ArtifactEscrowDestination)
	if err != nil {
		// Unreadable is not absent: binding over a record that may be there would
		// repoint a box whose escrow merely failed to answer.
		return fmt.Errorf("could not read the existing binding for box %s: %w", coords.BoxID, err)
	}
	want := hubaccess.DestinationRecord(coords)

	action, err := bindDecision(existing, want, rebind, coords.BoxID, gitopsDir, cluster)
	if err != nil {
		return err
	}
	if action == bindAlreadyBound {
		fmt.Printf("box %s is already bound to %s\n", coords.BoxID, want)
		return nil
	}

	if err := access.PutArtifact(ctx, id, escrow.ArtifactEscrowDestination, want); err != nil {
		return fmt.Errorf("could not record the binding for box %s: %w", coords.BoxID, err)
	}

	if action == bindFirst {
		fmt.Printf("box %s is now bound to %s\n", coords.BoxID, want)
	} else {
		fmt.Printf("box %s rebound\n  from %s\n    to %s\n", coords.BoxID, existing, want)
	}
	if strings.TrimSpace(coords.PlatformAdmin) == "" {
		fmt.Println("\nThis box declares no platformAdminIdentity yet, so no human session is")
		fmt.Println("authorized for it. `kakt whoami` prints the id to declare.")
	}
	return nil
}

// bindAction is what a bind call turns out to be.
//
// Named rather than inferred at the point of printing, because the whole value of
// this command is that a FIRST binding and a REPLACEMENT are distinguishable --
// to the operator reading the output, and to anyone reading the escrow afterwards.
type bindAction int

const (
	bindFirst bindAction = iota
	bindRebind
	bindAlreadyBound
)

// bindDecision decides what a bind call is, and refuses the two cases that would
// blur the distinction.
//
// Separate from the command so it can be exercised without an escrow: the
// authorization rule is this table, and a rule that is only reachable through a
// network call is a rule nothing checks.
func bindDecision(existing, want string, rebind bool, boxID, gitopsDir, cluster string) (bindAction, error) {
	switch {
	case existing == "" && rebind:
		// Quietly performing a first binding here would make --rebind and the
		// plain form indistinguishable, which is the property this command exists
		// to keep. It is also a good sign the wrong box was named.
		return 0, fmt.Errorf("box %s has no binding to rebind. The first binding is the "+
			"plain form:\n\n  kakt escrow bind-destination --gitops-dir %s --cluster %s",
			boxID, gitopsDir, cluster)
	case existing == want:
		return bindAlreadyBound, nil
	case existing != "" && !rebind:
		return 0, fmt.Errorf(`box %s is already bound to a different escrow.

Bound to:  %s
Declared:  %s

Rebinding moves where this box's recovery material is written, so it is a
deliberate act rather than a re-run. If this is the move you intend:

  kakt escrow bind-destination --gitops-dir %s --cluster %s --rebind`,
			boxID, existing, want, gitopsDir, cluster)
	case existing == "":
		return bindFirst, nil
	default:
		return bindRebind, nil
	}
}
