package main

import (
	"fmt"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/spoke"
	"github.com/spf13/cobra"
)

var (
	spokeForce       bool
	spokeClusterName string
	spokeMgmtCluster string
)

func newSpokeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "spoke",
		Short: "Manage Spoke Clusters",
		Long: `Manage and teardown Spoke Clusters provisioned via Zero-Ops.
This command works independently of the hub cluster and can delete spoke clusters
even when the hub cluster is no longer available.`,
	}

	// Add subcommands
	cmd.AddCommand(newSpokeTeardownCmd())
	cmd.AddCommand(newSpokeRestorePKICmd())
	cmd.AddCommand(newSpokeMatchInventoryCmd())

	return cmd
}

func newSpokeTeardownCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "teardown",
		Short: "Teardown Spoke Clusters",
		Long: `Teardown Spoke Clusters by deleting resources directly via Hetzner API.
This discovers spoke clusters by examining server labels and names in Hetzner.`,
		PreRunE: validateSpokeTeardownFlags,
		RunE:    runSpokeTeardown,
	}

	cmd.Flags().StringVar(&spokeClusterName, "name", "", "Specific spoke cluster name to delete (optional, deletes all if not specified)")
	cmd.Flags().StringVar(&spokeMgmtCluster, "mgmt-cluster", "",
		"the management cluster, excluded from deletion (kubefirst's mgmt/workload vocabulary; "+
			"same flag name as `tenant add-cluster`)")
	cmd.Flags().BoolVar(&spokeForce, "force", false, "Force deletion via Hetzner API (required since graceful deletion needs the management cluster)")
	cmd.Flags().BoolVar(&debug, "debug", false, "Enable verbose logging")

	return cmd
}

func validateSpokeTeardownFlags(cmd *cobra.Command, args []string) error {
	// Deleting EVERY workload cluster requires naming the management cluster.
	//
	// Discovery reads CAPH's caph-cluster-<name>=owned label, which the
	// management cluster's servers carry exactly as a workload cluster's do.
	// Without a name to exclude there is nothing in the label to tell them
	// apart, and "delete all workload clusters" would take the management
	// cluster with it -- the one machine that cannot be rebuilt from what is
	// left, since it holds the CAPI resources every other cluster is defined by.
	//
	// Refused rather than defaulted: any name guessed here would be some other
	// box's, and the cost of guessing wrong is the whole environment.
	if spokeClusterName == "" && spokeMgmtCluster == "" {
		return fmt.Errorf("deleting every workload cluster needs --mgmt-cluster, so the " +
			"management cluster can be excluded.\n\n" +
			"Its servers carry the same caph-cluster-<name> label a workload cluster's do, " +
			"so nothing distinguishes them without it.\n\n" +
			"Either pass --mgmt-cluster <name>, or name a single cluster with --name")
	}
	return nil
}

func runSpokeTeardown(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()

	if spokeClusterName != "" {
		fmt.Printf("🗑️  Tearing down Spoke Cluster: %s\n", spokeClusterName)
	} else {
		fmt.Println("🗑️  Tearing down ALL Spoke Clusters")
	}

	orchestrator := &spoke.Orchestrator{
		ClusterName: spokeClusterName,
		MgmtCluster: spokeMgmtCluster,
		Force:       spokeForce,
		Debug:       debug,
	}

	if err := orchestrator.Run(ctx); err != nil {
		return fmt.Errorf("spoke teardown failed: %w", err)
	}

	if spokeClusterName != "" {
		fmt.Printf("\n✓ Spoke Cluster '%s' successfully torn down\n", spokeClusterName)
	} else {
		fmt.Println("\n✓ All Spoke Clusters successfully torn down")
	}

	return nil
}
