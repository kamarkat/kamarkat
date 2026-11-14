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

	// The NAME Add RESOLVED, not the flag. Add defaults the first cluster's name
	// from the box's declaration, and it has a value receiver -- so the default
	// never reached this function, which printed an empty name and then asked
	// the escrow for an artefact keyed by no cluster.
	name, written, err := m.Add()
	if err != nil {
		return err
	}

	fmt.Printf("box %s\n", box.ID)
	fmt.Printf("declared management cluster %s:\n", name)
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
		if err := deliverAdminCredential(ctx, box, name); err != nil {
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

	// WHERE DAY-0 RUNS, and it is a property of the box rather than a choice
	// made here. A released box is built by the workflow its repository
	// carries; a development box reads its charts from a branch, carries no
	// workflow, and is built from this checkout by the same CLI. Both run the
	// same orchestrator, the same admission check and the same escrow
	// requirement -- what differs is only where it executes (ADR-072).
	if box.Decl.IsDevelopmentBox() {
		fmt.Println("\nCommit and push, then build it from here:")
		fmt.Println()
		fmt.Printf("  git add registry/clusters && git commit -m \"Declare %s\" && git push\n", name)
		fmt.Printf("  kakt bootstrap-mgmt --name %s --gitops-dir . \\\n", name)
		fmt.Printf("      --provider %s --region %s\n", box.Decl.Provider, box.Decl.Region)
		fmt.Println()
		fmt.Println("This box pins `development`: it reads its charts from a branch, so there is")
		fmt.Println("no published version for a workflow to fetch and it carries no")
		fmt.Println("bootstrap-cluster workflow. The commands above run the same Day-0 the")
		fmt.Println("released path runs, from this clone, under your own credentials.")
		fmt.Println()
		fmt.Println("Committing declares the cluster. Building one costs money and takes hours,")
		fmt.Println("so nothing is created until you run that second command.")
		return nil
	}

	fmt.Println("\nCommit and push to build it:")
	fmt.Println("  git add registry/clusters && git commit -m \"Declare " + name + "\" && git push")
	fmt.Println("\nThe push starts Day-0 in your repository, which reads this cluster's shape")
	fmt.Println("from the declaration you just committed. It costs money and takes hours, so")
	fmt.Println("review the declaration before you push -- and only a cluster this push ADDS is")
	fmt.Println("built, so editing one that exists changes nothing here.")

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
