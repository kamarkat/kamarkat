package main

import (
	"context"
	"fmt"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/session"
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
			return runAddManagementCluster(cmd.Context(), m)
		},
	}
	f := cmd.Flags()
	f.StringVar(&m.Name, "name", "",
		"the management cluster's name. Omit it for this box's first, which takes "+
			"the name the box declares; required for a replacement, and never a name "+
			"this box has used")
	f.StringVar(&m.From, "from", "",
		"the incarnation this one succeeds; needed only when the box declares more than one")
	f.StringVar(&m.GitopsDir, "gitops-dir", ".", "the box's gitops repository")
	// What this cluster differs from its box in, and nothing more. A cluster
	// that differed from its box in its domain, its escrow or its administrator
	// would be a second box sharing one repository.
	f.StringVar(&m.Overrides.Provider, "provider", "",
		"cloud provider for this cluster (default: the box's)")
	f.StringVar(&m.Overrides.Region, "region", "",
		"cloud region for this cluster (default: the box's)")
	f.StringVar(&m.Overrides.BundleVersion, "bundle-version", "",
		"platform bundle version this cluster starts on (default: the box's)")
	f.IntVar(&addManagementWorkers, "workers", -1,
		"cloud worker nodes this cluster starts with (default: the box's)")

	return cmd
}

// addManagementWorkers is read through a -1 sentinel, so "not passed" stays
// distinguishable from a deliberate zero -- a cluster whose capacity is the
// tenant's own hardware (ADR-075).
var addManagementWorkers int

func runAddManagementCluster(ctx context.Context, m tenant.ManagementCluster) error {
	if addManagementWorkers >= 0 {
		w := addManagementWorkers
		m.Overrides.Workers = &w
	}
	box, err := tenant.DiscoverBox(m.GitopsDir)
	if err != nil {
		return err
	}
	first := len(box.Management) == 0

	written, err := m.Add()
	if err != nil {
		return err
	}

	fmt.Printf("box %s\n", box.ID)
	fmt.Printf("declared management cluster %s:\n", m.Name)
	for _, p := range written {
		fmt.Printf("  %s\n", p)
	}

	if first {
		// THE BOX'S OWN INFISICAL ADMINISTRATOR, generated here rather than at
		// box creation, because its escrow artefact is keyed by box AND cluster
		// -- and until this moment the box has no cluster to key it by. It is
		// never asked for: the address comes from the session that created the
		// box, the password is generated, and the escrow is where it is
		// recovered from on a re-run.
		if err := deliverAdminCredential(ctx, box, m.Name); err != nil {
			return err
		}
	} else {
		fmt.Printf("\nboxId stays %s. This cluster's name keys its own WAL archive prefix,\n", box.ID)
		fmt.Println("so nothing it writes can reach what the box already has.")
		fmt.Println("\nADR-111 Revision 8: at most one management cluster of a box holds")
		fmt.Println("infrastructure. The bootstrap refuses this one while the predecessor")
		fmt.Println("still owns servers, so declaring it now is safe and building it is not")
		fmt.Println("until the predecessor is retired.")
	}

	fmt.Println("\nCommit and push, then create it:")
	fmt.Printf("  gh workflow run bootstrap-cluster -f cluster=%s\n", m.Name)
	fmt.Println("\nCommitting declares it. The cluster is built by that workflow, which costs")
	fmt.Println("money and takes hours, so no push creates one.")

	return nil
}

// deliverAdminCredential generates this box's own Infisical administrator,
// escrows it, and sets it on the repository.
//
// Two secrets, and neither is asked for. The address is the account that created
// the box; the password is generated and written to the escrow before it is
// written anywhere else, because a password that reached the repository and not
// the escrow could not be recovered by anyone -- and a re-run reads back the one
// the instance was created with rather than minting a second over the live one.
func deliverAdminCredential(ctx context.Context, box tenant.Box, cluster string) error {
	var email string
	if sess, err := session.Current(ctx, box.Decl.Escrow.URL); err == nil {
		email = sess.Email
	}

	secrets := tenant.Secrets{}
	secrets.Resolve(tenant.Spec{Provider: box.Decl.Provider})
	secrets, generated, err := secrets.WithAdminCredential(ctx, box.ID, cluster, email)
	if err != nil {
		return err
	}
	if generated {
		fmt.Printf("\n[box] generated this box's Infisical administrator (%s) and escrowed it\n",
			secrets.InfisicalAdminEmail)
	} else {
		fmt.Printf("\n[box] reusing the escrowed Infisical administrator (%s)\n",
			secrets.InfisicalAdminEmail)
	}
	return tenant.SetAdminSecrets(ctx, box.Decl.GitOrg, box.Decl.TenantID, secrets)
}
