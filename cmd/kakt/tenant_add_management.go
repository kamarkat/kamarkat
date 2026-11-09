package main

import (
	"fmt"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/tenant"
	"github.com/spf13/cobra"
)

// newAddManagementClusterCmd declares a new incarnation of the box's management
// cluster.
//
// The box is read from the repository, never named on the command line: every
// command here runs inside a box's own gitops repository, where boxId is already
// written down. See tenant.DiscoverBox for why asking for it again is the one
// mistake that cannot be undone.
func newAddManagementClusterCmd() *cobra.Command {
	var m tenant.ManagementCluster

	cmd := &cobra.Command{
		Use:   "add-management",
		Short: "Declare a new management cluster for this box",
		Long: `Declare a new management cluster for the box this repository is.

ADR-111 §1: the box outlives the cluster. The box keeps its boxId -- the escrow
path holding its master keys, the DNS records it owns -- and the management
cluster's own name changes, because that name keys this cluster's objects, its
nodes and its database's WAL archive prefix. A replacement is therefore a new
declaration beside the old one rather than an edit of it, and during a planned
succession both exist at once.

Rendered from the management cluster already declared here, which carries this
box's domain, provider, region and escrow coordinates. Day-0's output is not
carried over: generated/ holds the predecessor's CAPI Cluster, PKI and load
balancer address, and the new cluster's Day-0 writes its own.

This declares the cluster. It is created when you dispatch the repository's
bootstrap-cluster workflow with this name.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAddManagementCluster(m)
		},
	}
	f := cmd.Flags()
	f.StringVar(&m.Name, "name", "", "the new management cluster's name (required)")
	f.StringVar(&m.From, "from", "",
		"the incarnation this one succeeds; needed only when the box declares more than one")
	f.StringVar(&m.GitopsDir, "gitops-dir", ".", "the box's gitops repository")
	_ = cmd.MarkFlagRequired("name")

	return cmd
}

func runAddManagementCluster(m tenant.ManagementCluster) error {
	box, err := tenant.DiscoverBox(m.GitopsDir)
	if err != nil {
		return err
	}

	written, err := m.Add()
	if err != nil {
		return err
	}

	fmt.Printf("box %s\n", box.ID)
	fmt.Printf("declared management cluster %s:\n", m.Name)
	for _, p := range written {
		fmt.Printf("  %s\n", p)
	}

	fmt.Printf("\nboxId stays %s. This cluster's name keys its own WAL archive prefix,\n", box.ID)
	fmt.Println("so nothing it writes can reach what the box already has.")

	fmt.Println("\nCommit and push, then create it:")
	fmt.Printf("  gh workflow run bootstrap-cluster -f cluster=%s\n", m.Name)
	fmt.Println("\nCommitting declares it. The cluster is built by that workflow, which costs")
	fmt.Println("money and takes hours, so no push creates one.")

	return nil
}
