package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/soloz-io/zero-ops/internal/platform/escrow"
	"github.com/spf13/cobra"
)

// The one-time copy of a box's cluster-scoped artefacts onto their scoped paths.
//
// Before ADR-111 every artefact lived in one flat folder per cluster name. Scoping
// them moved three of them -- the admin kubeconfig and the two at-rest encryption
// key generations -- under the box that owns the cluster, so a successor hub can no
// longer overwrite its predecessor's. Nothing reads the old path any more, so until
// this has run, those artefacts are present in the escrow and invisible to every
// reader of it.
//
// A command rather than a reconcile, and a command that copies rather than one that
// re-derives, because the two artefacts that did not move themselves cannot be
// re-derived at all:
//
//   - the kubeconfigs ARE rewritten by hub-operator on each pass, so they land on
//     the new path without this. They are included anyway, because a box whose
//     operator is not running is exactly the box that needs its break-glass
//     credential findable.
//   - the at-rest encryption keys are written once, at provisioning. `soloz
//     encryption` is NOT a migration for them: finding nothing under the scoped
//     path it generates a new 32-byte key and applies it, and the first thing that
//     key does is fail to decrypt every Secret the cluster has already written.
//
// Run it for the hub and for every spoke. It is idempotent, it never overwrites a
// destination that differs, and it leaves the old copies in place.
func newEscrowMigratePathsCmd() *cobra.Command {
	var (
		box      string
		clusters []string
	)

	cmd := &cobra.Command{
		Use:   "migrate-paths",
		Short: "copy a box's cluster-scoped escrow artefacts onto their scoped paths",
		Long: `Copies the admin kubeconfig and both at-rest encryption key generations from
the pre-ADR-111 flat path to the scoped path a cluster now has.

Copies; never generates, derives or rotates. Refuses to overwrite a destination
holding different bytes. Reads every destination back and compares it with its
source before reporting the artefact migrated. Leaves the old copies in place.

Reports fingerprints, never values.`,
		RunE: func(c *cobra.Command, _ []string) error {
			return runEscrowMigratePaths(c.Context(), box, clusters)
		},
	}

	f := cmd.Flags()
	f.StringVar(&box, "box", "", "the logical hub that owns these clusters (required)")
	f.StringSliceVar(&clusters, "cluster", nil,
		"a cluster in that box, repeatable: the hub and every spoke (required)")
	_ = cmd.MarkFlagRequired("box")
	_ = cmd.MarkFlagRequired("cluster")
	return cmd
}

func runEscrowMigratePaths(ctx context.Context, box string, clusters []string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(box) == "" {
		return fmt.Errorf("--box is required: the scoped path is keyed by it")
	}
	if len(clusters) == 0 {
		return fmt.Errorf("--cluster is required: name the hub and every spoke, " +
			"because each has its own artefacts and its own path")
	}

	store, err := escrow.NewEscrowClient(ctx, "")
	if err != nil {
		return fmt.Errorf("no escrow to migrate: %w", err)
	}
	migrator, ok := store.(escrow.PathMigrator)
	if !ok {
		return fmt.Errorf("this escrow cannot migrate paths")
	}

	// Both generations, because a rotation in flight leaves etcd holding objects
	// under each and a box that recovered with only the new one would be unable to
	// read everything written before the rotation was finalised.
	artifacts := []string{
		escrow.ArtifactKubeconfig,
		escrow.ArtifactSecretEncryptionKey,
		escrow.ArtifactSecretEncryptionKeyPrevious,
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "CLUSTER\tARTEFACT\tACTION\tFINGERPRINT")

	var failures []string
	copied, present, absent := 0, 0, 0

	for _, cluster := range clusters {
		cluster = strings.TrimSpace(cluster)
		if cluster == "" {
			continue
		}
		id := escrow.ForCluster(box, cluster)
		for _, a := range artifacts {
			m, err := migrator.MigrateArtifactPath(ctx, id, a)
			if err != nil {
				fmt.Fprintf(w, "%s\t%s\tFAILED\t-\n", cluster, a)
				failures = append(failures, fmt.Sprintf("%s/%s: %v", cluster, a, err))
				continue
			}
			switch m.Action {
			case escrow.MigrationCopied:
				copied++
			case escrow.MigrationAlreadyPresent:
				present++
			case escrow.MigrationAbsent:
				absent++
			}
			fp := m.Digest
			if fp == "" {
				fp = "-"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", cluster, a, m.Action, fp)
		}
	}
	_ = w.Flush()

	fmt.Printf("\n%d copied, %d already present, %d absent.\n", copied, present, absent)

	// Absent is reported, not judged. secret-encryption-key-previous is absent
	// unless a rotation is in flight, and an absent kubeconfig means CAPI had not
	// written one when the box last escrowed -- neither is this command's to fix.
	if absent > 0 {
		fmt.Println("An absent artefact was never at the old path either. " +
			"`soloz escrow verify` is what says whether it is required.")
	}

	if len(failures) > 0 {
		return fmt.Errorf("%d artefact(s) did not migrate, so the escrow's recovery "+
			"contract is still incomplete for them:\n  %s",
			len(failures), strings.Join(failures, "\n  "))
	}
	return nil
}
