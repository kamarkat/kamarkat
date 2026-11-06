package main

import (
	"fmt"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/bootstrap"
	"github.com/spf13/cobra"
)

var (
	reseedKubeconfig    string
	reseedBundleVersion string
)

// newReseedCmd re-applies the seed Application to a cluster that already exists.
//
// The seed Application is written once, at bootstrap, and carries the parameters
// the environment-manager chart renders from. A parameter added to the renderer
// afterwards reaches new clusters only — on a running one the chart renders
// without it and every Application generated from it fails comparison, which
// ArgoCD reports as sync status Unknown rather than as an error. Auto-sync fires
// only on OutOfSync, so the change appears pushed and nothing deploys.
//
// This command exists so that state is corrected from the renderer rather than
// by patching the live Application, which would leave the repository disagreeing
// with the cluster and the fix lost to whoever reads the repository next.
//
// The cluster's identity is read back from the seed rather than accepted as
// flags. Supplying it by hand invites supplying it differently, and topology in
// particular is a path segment: a plausible-looking value repoints an
// ApplicationSet at a directory that does not exist, and its Applications go
// missing rather than failing.
func newReseedCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "reseed",
		Short: "Re-apply the seed Application to an existing Hub cluster",
		Long: `Re-render the Day-0 seed Application from the current CLI and apply it.

Use after adding or changing a seed parameter, which otherwise reaches only
newly bootstrapped clusters.

The environment, provider and topology are read from the cluster's existing seed
Application, so a reseed cannot change which cluster the seed describes.

Boundary activation state is not touched: only the seed Application is applied,
never the boundary AppProjects, so an opened boundary is not re-gated.

The bundle version is left as the cluster records it, so a reseed cannot return
a cluster to whatever bundle the operator's binary happens to carry. Pass
--bundle-version to move it deliberately; the move is printed against the
version it replaces.`,
		RunE: runReseed,
	}

	cmd.Flags().StringVar(&reseedKubeconfig, "kubeconfig", "", "Path to the hub cluster kubeconfig")
	cmd.Flags().StringVar(&reseedBundleVersion, "bundle-version", "",
		"Move the cluster to this bundle version instead of keeping the one it records")
	cmd.MarkFlagRequired("kubeconfig")

	return cmd
}

func runReseed(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()

	managementScope, provider, topology, err := bootstrap.ReadSeedIdentity(ctx, reseedKubeconfig)
	if err != nil {
		return err
	}

	o := &bootstrap.Orchestrator{
		ManagementScope:       managementScope,
		Topology:              topology,
		ProviderName:          provider,
		BundleVersionOverride: reseedBundleVersion,
	}

	fmt.Printf("[reseed] Applying the seed Application (management scope=%s provider=%s topology=%q)\n",
		managementScope, provider, topology)

	if err := o.ReapplySeed(ctx, reseedKubeconfig); err != nil {
		return fmt.Errorf("reseed failed: %w", err)
	}

	fmt.Println("[reseed] ✓ Seed Application applied")
	return nil
}
