package main

import (
	"context"
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
		// THE BOX'S OPENBAO SEAL KEY (ADR-117 §3), generated before Day-0 needs it
		// and kept on every re-run: a new key would leave the store sealed.
		if err := escrowSealRecord(ctx, box); err != nil {
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
		// THE PUSH DOES NOT BUILD THIS BOX, said first and said plainly.
		//
		// On the released path the push IS the build -- that is what the
		// documentation teaches and what every other message here says -- so a
		// development box has to contradict it before it gives the commands,
		// not after. Leading with "Commit and push, then build it from here"
		// put the familiar half first and the contradiction three lines below
		// it, and it was read as the trigger it usually is: the operator pushed
		// and then asked why no workflow had run.
		fmt.Println("\nThis box pins `development`, so THE PUSH WILL NOT BUILD IT.")
		fmt.Println()
		fmt.Println("It reads its charts from a branch rather than from a published version,")
		fmt.Println("so there is nothing for a workflow to fetch and this repository carries no")
		fmt.Println("bootstrap-cluster workflow at all. Day-0 runs from this clone instead --")
		fmt.Println("the same orchestrator, admission check and escrow requirement the released")
		fmt.Println("path runs, under your own credentials (ADR-072).")
		fmt.Println()
		fmt.Println("Two steps, and the second is the one that builds:")
		fmt.Println()
		fmt.Printf("  git add registry/clusters && git commit -m \"Declare %s\" && git push\n", name)
		fmt.Println("      declares the cluster; creates nothing")
		fmt.Println()
		fmt.Printf("  kakt bootstrap-mgmt --name %s --gitops-dir . \\\n", name)
		fmt.Printf("      --provider %s --region %s\n", box.Decl.Provider, box.Decl.Region)
		fmt.Println("      builds it: costs money, takes hours. Run from a platform checkout.")
		return nil
	}

	// NO BINDING STEP HERE. It is established, not instructed.
	//
	// This printed `kakt escrow bind-destination` as step one of two, and that
	// was already untrue when it was written: `box init` escrows this box's
	// provider credential, enrolment opens the escrow with OpenEstablishing, and
	// OpenEstablishing records escrow-destination as a side effect of the first
	// authorized write. So by the time a box has a cluster to declare, the
	// binding exists -- and the instruction asked an operator to perform, as a
	// named prerequisite, something the platform had already done.
	//
	// The command stays for the one case it is for: a box whose binding is
	// missing or wrong, which `kakt escrow verify` reports and whose error paths
	// name it. Nothing in an ordinary flow should.
	fmt.Println("\nOne step, and it is the one that builds:")
	fmt.Println()
	fmt.Println("  git add registry/clusters && git commit -m \"Declare " + name + "\" && git push")
	fmt.Println("      starts Day-0 in your repository, which reads this cluster's shape")
	fmt.Println("      from the declaration you just committed. It costs money and takes")
	fmt.Println("      hours, so review the declaration before you push -- and only a")
	fmt.Println("      cluster this push ADDS is built, so editing one that exists changes")
	fmt.Println("      nothing here.")
	fmt.Println()
	fmt.Println("`kakt escrow verify` reports this box's escrow, including the binding, if")
	fmt.Println("you want to see it before you push.")

	return nil
}

// escrowSealRecord makes sure the box escrow holds the OpenBao seal record
// (ADR-117 §3) before Day-0 needs it. Generated once; a re-run keeps it.
func escrowSealRecord(ctx context.Context, box tenant.Box) error {
	secrets := tenant.Secrets{}
	secrets.Resolve(tenant.Spec{Provider: box.Decl.Provider})
	created, err := secrets.WithSealRecord(ctx)
	if err != nil {
		return err
	}
	if created {
		fmt.Println("\n[box] generated this box's OpenBao seal key (generation 1) and escrowed it")
	} else {
		fmt.Println("\n[box] the escrow already holds this box's OpenBao seal record; kept")
	}
	return nil
}
