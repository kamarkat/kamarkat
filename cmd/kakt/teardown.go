package main

import (
	"fmt"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/teardown"
	"github.com/spf13/cobra"
)

var (
	teardownDNSOnly  bool
	teardownDNSOwner string
	teardownDNSZone  string
	teardownForce    bool
	teardownConfirm  bool
	teardownSpokes   []string
	teardownGitops   string
	teardownTenant   string
)

func newTeardownCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "teardown",
		Short: "Teardown a Hub Cluster",
		Long: `Teardown a Hub Cluster by forcefully deleting all cloud infrastructure,
Kubernetes CAPI resources, Kind/Docker artifacts, and local state.`,
		PreRunE: validateTeardownFlags,
		RunE:    runTeardown,
	}

	cmd.Flags().StringVar(&clusterName, "name", "", "Hub Cluster name (required)")
	cmd.Flags().BoolVar(&teardownForce, "force", true, "Force deletion of resources (default true)")
	cmd.Flags().BoolVar(&teardownConfirm, "confirm", false, "Confirm deletion (required for safety)")
	cmd.Flags().BoolVar(&debug, "debug", false, "Enable verbose logging")
	// For a box that is already gone and left records behind. external-dns
	// ignores a record whose owner does not match, silently, so an orphan cannot
	// be reclaimed by any later box -- the hostname resolves to a decommissioned
	// address until someone deletes it.
	cmd.Flags().BoolVar(&teardownDNSOnly, "dns-only", false,
		"release this box's DNS records and nothing else")
	cmd.Flags().StringVar(&teardownDNSOwner, "dns-owner", "",
		"external-dns txt-owner-id whose records to release (default: read from the cluster)")
	cmd.Flags().StringVar(&teardownDNSZone, "dns-zone", "",
		"the zone those records live in, e.g. dev.nutgraf.in (default: read from the cluster)")
	// The spoke list normally comes from the hub. A hub that is already gone
	// answers nothing, which is exactly when its spokes get stranded: the servers
	// carry the spoke's name, mention the hub nowhere, and survive the teardown
	// that was supposed to take them. Naming the pool here is what the box
	// declares, and it outlives the cluster.
	// Where the box's state and kubeconfig actually are. bootstrap writes both
	// under --gitops-dir; teardown resolved them against its own working
	// directory, so a teardown run from anywhere else read an empty cluster and
	// tore down nothing while reporting success.
	cmd.Flags().StringVar(&teardownGitops, "gitops-dir", "",
		"the tenant repository this box was bootstrapped from (holds its state and kubeconfig)")
	// The box's own name. Every cluster it creates derives from the domain
	// (ADR-082), so one prefix names every server it owns -- and unlike the hub's
	// spoke list or the tenant repository, it is still available once both are
	// gone, which is precisely when a teardown needs it.
	cmd.Flags().StringVar(&teardownTenant, "tenant", "",
		"the box's name prefix; every server whose name or caph-cluster label starts with it is this box's")
	cmd.Flags().StringSliceVar(&teardownSpokes, "spoke", nil,
		"spoke cluster(s) this box declares, used when the hub cannot be reached (repeatable)")

	// --name is required for a teardown, which destroys a named cluster. A
	// --dns-only run destroys no cluster and names records by owner instead.
	cmd.MarkFlagRequired("name")

	return cmd
}

func validateTeardownFlags(cmd *cobra.Command, args []string) error {
	if clusterName == "" {
		return fmt.Errorf("--name is required")
	}

	if !teardownConfirm {
		return fmt.Errorf("teardown requires --confirm flag for safety")
	}

	return nil
}

func runTeardown(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()

	if teardownDNSOnly {
		fmt.Printf("🧹 Releasing DNS records owned by %q in %s\n", teardownDNSOwner, teardownDNSZone)
	} else {
		fmt.Printf("🗑️  Tearing down Hub Cluster: %s\n", clusterName)
	}

	orchestrator := &teardown.Orchestrator{
		ClusterName:  clusterName,
		Force:        teardownForce,
		Debug:        debug,
		DNSOnly:      teardownDNSOnly,
		GitopsDir:    teardownGitops,
		TenantPrefix: teardownTenant,
		Spokes:       teardownSpokes,
		DNS: teardown.DNSOverride{
			Owner: teardownDNSOwner,
			Zone:  teardownDNSZone,
		},
	}

	if err := orchestrator.Run(ctx); err != nil {
		return fmt.Errorf("teardown failed: %w", err)
	}

	// Says what happened, not what the command is called. A --dns-only run tore
	// down no cluster, and announcing that it did is how a report stops being
	// evidence -- the same defect as a bootstrap banner listing gates it no
	// longer runs.
	if teardownDNSOnly {
		n := orchestrator.ReleasedDNSRecords
		if n == 0 {
			fmt.Printf("\n✓ No records in %s are owned by '%s'. Nothing to release, and no cluster was torn down.\n",
				teardownDNSZone, teardownDNSOwner)
		} else {
			fmt.Printf("\n✓ Released %d DNS record(s) owned by '%s'. No cluster was torn down.\n", n, teardownDNSOwner)
			fmt.Println("  Resolvers may serve the old answers until their TTL expires.")
		}
	} else {
		fmt.Printf("\n✓ Cluster '%s' successfully torn down\n", clusterName)
	}
	return nil
}
