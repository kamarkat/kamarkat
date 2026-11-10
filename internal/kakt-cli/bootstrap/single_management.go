package bootstrap

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/tenant"
)

// A box has ONE management cluster at a time.
//
// The box may have had several over its life -- ADR-111 ss1 exists so that a
// cluster can be replaced without the box losing its identity -- but two of them
// owning management infrastructure at once is not a succession, it is two control
// planes reconciling one tenant's declarations. Both would act on the same claims
// in the same repository, so each would provision the OTHER's workload clusters
// under names already taken, and each would archive its database to a prefix the
// other is writing.
//
// THE PROVIDER IS THE AUTHORITY, and it is the only one asked. Three sources were
// available and two of them lie in opposite directions:
//
//   - the gitops repository outlives every incarnation by design (ADR-111 ss1,
//     brief ss3), so a declaration proves a cluster was WANTED, never that one
//     exists. kubefirst gates on exactly this -- its create refuses while the
//     gitops repository exists (internal/gitShim/init.go) -- which is why a
//     destroyed kubefirst installation cannot be rebuilt until a human deletes
//     the repository by hand.
//   - the escrow holds a predecessor's artefacts deliberately and for as long as
//     its recovery point matters, so their presence proves nothing about now.
//   - the Kubernetes API answers only while the cluster is healthy. An
//     unreachable API is the state of a cluster whose servers are running and
//     whose control plane is wedged, which is the single most dangerous moment to
//     conclude "absent" and build a second one. kubefirst reads its cluster
//     record from a Secret INSIDE the cluster being created
//     (internal/router/api/v1/cluster.go, PostCreateCluster), so losing the
//     cluster loses the only record that it ever existed.
//
// Provider resources carry their owning cluster's name as a label, written by
// the infrastructure controller rather than by this platform, so they answer
// "does this cluster still hold infrastructure" without a kubeconfig, a database
// or a repository. That is also what makes "verified destroyed" a real state
// rather than an assumption: the resources are gone from the provider's own
// inventory.

// OwnedCounts reports how many provider resources carry each named cluster's
// ownership label.
//
// A function rather than a provider client: the admission decision is about
// ownership, the provider's inventory is one way to establish it, and this
// package stays free of a cloud SDK. The caller passes the same inventory the
// teardown deletes from, so the two cannot disagree about what a cluster owns.
type OwnedCounts func(ctx context.Context, clusters []string) (map[string]int, error)

// ManagementAdmission decides whether the management cluster about to be
// provisioned may be provisioned.
type ManagementAdmission struct {
	// GitopsDir is the box's repository, the only place its identity is written.
	GitopsDir string
	// Creating is the management cluster this bootstrap would build.
	Creating string
	// Owned establishes what each declared cluster still holds.
	Owned OwnedCounts
	// Incarnations enumerates every management cluster this box has ever
	// declared, including any whose directory is no longer in the working tree.
	//
	// Injected so this decision can be tested, and required rather than
	// defaulted: a nil one would narrow the check back to what the tree happens
	// to show, which is the coverage gap it exists to close.
	Incarnations func(ctx context.Context, gitopsDir string) ([]string, error)
}

// Admission is what was established, for a caller that continues.
type Admission struct {
	// BoxID is the box this cluster belongs to.
	BoxID string
	// Resuming is set when the cluster being created already holds
	// infrastructure of its own: an earlier attempt at THIS incarnation that did
	// not finish. Continuing is a retry of the same operation rather than a
	// second cluster, so it is allowed -- and named, because a bootstrap that
	// silently adopts half-built infrastructure is how two attempts come to
	// disagree about what exists.
	Resuming int
}

// Admit refuses a second management cluster for one box.
func (a ManagementAdmission) Admit(ctx context.Context) (Admission, error) {
	box, err := tenant.DiscoverBox(a.GitopsDir)
	if err != nil {
		return Admission{}, err
	}
	if strings.TrimSpace(a.Creating) == "" {
		return Admission{}, fmt.Errorf("no cluster name: there is nothing to admit")
	}

	if a.Incarnations == nil {
		return Admission{}, fmt.Errorf("no record of this box's past management clusters, so " +
			"a forgotten one could not be detected")
	}
	// BOX-WIDE, not working-tree-wide. An incarnation whose directory was removed
	// or renamed still owns its servers, and an identity that is never enumerated
	// is never asked about at the provider -- so it would be invisible to this
	// check at the one moment it matters.
	everDeclared, err := a.Incarnations(ctx, a.GitopsDir)
	if err != nil {
		return Admission{}, fmt.Errorf(`cannot establish which management clusters this box has had, so creating one is refused.

  %v

Box: %s

A management cluster whose declaration was removed still owns its servers. Without
this record those servers are invisible here, and a second management cluster
would be admitted because the first was forgotten.`, err, box.ID)
	}

	// Declared NOW, from the working tree alone: a cluster that was declared once
	// and removed has nothing to reconcile it today. History widens who is ASKED
	// ABOUT, never what counts as declared.
	declared := false
	for _, c := range box.Management {
		if c == a.Creating {
			declared = true
		}
	}

	seen := map[string]bool{}
	var others []string
	for _, c := range append(append([]string{}, box.Management...), everDeclared...) {
		if c == a.Creating {
			continue
		}
		if seen[c] {
			continue
		}
		seen[c] = true
		others = append(others, c)
	}
	sort.Strings(others)
	if !declared {
		// Provisioning something the repository does not declare would build a
		// cluster that nothing reconciles: no root Application, no bundle, and a
		// name the box has no record of.
		return Admission{}, fmt.Errorf(`%s is not declared by this repository.

A management cluster is declared before it is built, so that the box has a
record of it and ArgoCD has something to reconcile. Box %s declares: %s.

Declare it first:
  kakt tenant add-management --name %s`,
			a.Creating, box.ID, strings.Join(box.Management, ", "), a.Creating)
	}

	workload, err := tenant.DeclaredWorkloadClusters(a.GitopsDir, a.Creating)
	if err != nil {
		return Admission{}, fmt.Errorf("read this box's workload cluster declarations: %w", err)
	}

	ask := append([]string{a.Creating}, others...)
	ask = append(ask, workload...)
	counts, err := a.Owned(ctx, ask)
	if err != nil {
		// The ambiguous answer is refused rather than treated as empty. An
		// inventory that cannot be read is the same evidence as one not asked
		// for, and the whole point of asking was to avoid concluding "absent"
		// from silence.
		return Admission{}, fmt.Errorf(`cannot establish what this box already owns, so creating a management cluster is refused.

  %v

Box: %s
Creating: %s

The provider's inventory is what tells a destroyed cluster apart from an
unreachable one. Without it, building would risk a second management cluster
for this box.

Check the provider credential this run has (HCLOUD_TOKEN), then run this again.`,
			err, box.ID, a.Creating)
	}

	var blockers []string
	for _, c := range others {
		if n := counts[c]; n > 0 {
			blockers = append(blockers, fmt.Sprintf(
				"  %s still owns %d provider resource(s)", c, n))
		}
	}
	sort.Strings(blockers)

	var stillManaged []string
	for _, c := range workload {
		if n := counts[c]; n > 0 {
			stillManaged = append(stillManaged, fmt.Sprintf(
				"  %s still owns %d provider resource(s)", c, n))
		}
	}
	sort.Strings(stillManaged)

	if len(blockers) > 0 || len(stillManaged) > 0 {
		return Admission{}, admissionRefused(box, a.Creating, blockers, stillManaged)
	}

	return Admission{BoxID: box.ID, Resuming: counts[a.Creating]}, nil
}

// admissionRefused says what is in the way and which command removes it.
//
// Every blocker at once rather than the first: a run that reports one, is fixed,
// and then reports the next makes a tenant discover the shape of the rule by
// trial. kubefirst accumulates its offending repositories the same way, and it
// is the better half of its preflight.
func admissionRefused(box tenant.Box, creating string, existing, managed []string) error {
	var b strings.Builder
	b.WriteString("Cannot create the management cluster.\n\n")
	fmt.Fprintf(&b, "Box: %s\nCreating: %s\n\n", box.ID, creating)

	if len(existing) > 0 {
		b.WriteString("This box already has a management cluster:\n\n")
		b.WriteString(strings.Join(existing, "\n"))
		b.WriteString("\n\nA box has one management cluster at a time. Two of them reconcile the same\n")
		b.WriteString("declarations in this repository, so each would provision the other's workload\n")
		b.WriteString("clusters under names already taken.\n\n")
		b.WriteString("Destroy it:\n")
		for _, line := range existing {
			name := strings.Fields(strings.TrimSpace(line))[0]
			fmt.Fprintf(&b, "  kakt teardown --name %s --gitops-dir . --confirm\n", name)
		}
		b.WriteString("\n")
	}

	if len(managed) > 0 {
		b.WriteString("Workload clusters of this box are still running:\n\n")
		b.WriteString(strings.Join(managed, "\n"))
		b.WriteString("\n\nThe management cluster goes last. It holds their Cluster API objects and is\n")
		b.WriteString("the only thing that can deprovision their load balancers, so a replacement\n")
		b.WriteString("built while they run would leave them unmanaged and still billing -- and\n")
		b.WriteString("would then rebuild them from the claims still in this repository.\n\n")
		b.WriteString("Remove each one first, which is a commit:\n")
		for _, line := range managed {
			name := strings.Fields(strings.TrimSpace(line))[0]
			fmt.Fprintf(&b, "  kakt tenant remove-cluster --mgmt-cluster <management-cluster> --name %s\n", name)
		}
		b.WriteString("\n")
	}

	fmt.Fprintf(&b, `Nothing here needs deleting by hand, and nothing is lost by this refusal:
this repository is reused as it is, boxId stays %s, and the escrow artefacts and
database archives of every past incarnation stay under their own names.`, box.ID)

	return fmt.Errorf("%s", b.String())
}
