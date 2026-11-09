package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/tenant"
)

// newRemoveWorkloadClusterCmd is add-cluster's inverse, and it edits the
// repository.
//
// ONE INVOCATION, because the declaration is the cluster. The management
// cluster's two ApplicationSets are generated from
// registry/clusters/<name>/infrastructure/spokepool.yaml, so the commit that
// removes that file stops the Applications being generated: the infrastructure
// one is deleted with Argo CD's resources-finalizer, the claim goes with it, and
// Crossplane and Cluster API unwind the cluster.
//
// IT WAS TWO, AND THE SECOND ONE WAS A WORKAROUND. The per-cluster Applications
// were flat files in the management cluster's directory, which its root applies
// with `prune: false`, so removing the files left both Applications -- and the
// cluster -- running. `--destroy` then deleted those objects over a kubeconfig,
// after reading the root's `requiresPruning` status to prove the removal had been
// pushed and synced. Every part of that existed to close a window the repository
// had already declared shut: declaration gone, cluster alive. The window is gone,
// so the step is too (ADR-114 §5).
//
// What a commit cannot do is report progress. `kakt tenant cluster-status` does
// that by observation.
func newRemoveWorkloadClusterCmd() *cobra.Command {
	var removal tenant.ClusterRemoval

	cmd := &cobra.Command{
		Use:          "remove-cluster",
		Short:        "Remove a workload cluster by deleting its declaration",
		SilenceUsage: true,
		Long: `Remove a workload cluster from a tenant's box by deleting its declaration:
registry/clusters/<name>/, which holds the SpokePool claim that declares it.

It checks that nothing still names the cluster -- not the box's own burst pool --
prints the environment's applications so you can see what stops running, and
deletes the directory. Nothing is destroyed yet, because nothing is committed yet.

COMMITTING IS THE REMOVAL. The management cluster generates the cluster's
Applications from that claim file; once it is gone from the branch the management
cluster reconciles, the Applications stop being generated, the claim is deleted
with them, and the cluster is unwound by the controllers that own it. Watch it
with kakt tenant cluster-status --name <name>.

The cluster's secrets and backups stay behind, and its name is never reused.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// What stops running, printed before anything is removed. Nothing here
			// is a precondition: an environment's application definitions are not
			// this cluster's to take with it.
			if env, apps, err := removal.AffectedDeployments(); err == nil && len(apps) > 0 {
				fmt.Printf("%s serves environment %q, which declares %d application(s):\n",
					removal.Name, env, len(apps))
				for _, a := range apps {
					fmt.Printf("  %s\n", a)
				}
				fmt.Printf("\nTheir deployments on %s stop. Their definitions stay in "+
					"environments/%s/ and deploy to every other cluster serving %q.\n\n",
					removal.Name, env, env)
			}

			removed, err := removal.RemoveDeclaration()
			if err != nil {
				return err
			}
			fmt.Printf("✓ declaration of %q removed\n", removal.Name)
			for _, p := range removed {
				fmt.Printf("  %s\n", p)
			}
			fmt.Println("\nNothing is destroyed yet: the working tree is edited, not the branch")
			fmt.Println("the management cluster reconciles. Commit and push, and that commit is")
			fmt.Printf("what destroys %s.\n\n", removal.Name)
			fmt.Printf("  kakt tenant cluster-status --name %s --mgmt-cluster %s\n",
				removal.Name, removal.MgmtCluster)
			fmt.Printf("\nIts secrets under /spoke-pool/%s/ and its backups under the %s/ prefix\n",
				removal.Name, removal.Name)
			fmt.Println("remain. The name is not reused: both outlive the cluster.")
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&removal.GitopsDir, "gitops-dir", ".", "the tenant repository")
	f.StringVar(&removal.MgmtCluster, "mgmt-cluster", "", "the management cluster that created this one (required)")
	f.StringVar(&removal.Name, "name", "", "the workload cluster to remove (required)")
	// --destroy, --kubeconfig and --timeout ARE GONE, and withdrawn rather than
	// accepted-and-ignored: a script that passes --destroy stops instead of
	// quietly doing half of what it asked for. There is nothing left for them to
	// do -- the commit destroys the cluster, and watching it is cluster-status.
	for _, required := range []string{"mgmt-cluster", "name"} {
		_ = cmd.MarkFlagRequired(required)
	}
	return cmd
}
