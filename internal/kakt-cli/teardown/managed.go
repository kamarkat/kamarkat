package teardown

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/tenant"
)

// THE MANAGEMENT CLUSTER GOES LAST.
//
// A management cluster is not torn down while a cluster it manages is still
// running, and this file is where that is enforced. It is an ordering, not a
// preference, and three separate things make it one:
//
//   - The workload cluster's CAPI objects live on the management cluster. Once it
//     is gone, nothing on this platform can delete that cluster: there is no
//     controller to reconcile the deletion, no provider credential mounted
//     anywhere, and no record of the cluster beyond its servers' labels.
//   - Its load balancers can only be deprovisioned by the CCM running inside it,
//     and the CCM's credential arrives by ExternalSecret from the management
//     cluster's secret store. A management cluster taken out from under it leaves
//     load balancers nothing will ever collect -- four of them once exhausted a
//     five-load-balancer project quota (ADR-046 §23).
//   - Its backups, its secret paths and its DNS records are all reclaimed through
//     the management cluster, and its name is never reused, so what is stranded
//     stays stranded.
//
// Teardown used to CASCADE instead: it asked the hub for its CAPI clusters and
// swept their resources into the same run. That reads as convenient and is the
// wrong shape -- a hub teardown then destroys production workload clusters
// nobody named on the command line, by a list the operator never sees until the
// inventory is printed, and if the sweep half-fails the hub is gone anyway and
// the remainder is unreachable. The clusters a run may destroy are now the ones
// named on it.

// ErrOrderViolation is returned when the run would destroy the management
// cluster while a cluster it manages is still there, or has just failed to go.
//
// A sentinel because the caller must stop rather than carry on: the steps after
// the provider are the DNS release and the removal of this box's local
// kubeconfig and state, and doing either for a box that still exists is how an
// operator ends up unable to reach the thing they were told was gone.
var ErrOrderViolation = errors.New("the management cluster goes last")

// ManagedCluster is one cluster this box's management cluster created, with the
// source that still says so.
//
// The source is carried because the refusal has to be answerable. "There is a
// workload cluster" invites an argument about whether there really is; "the
// management cluster's own CAPI inventory lists nutgraf-01" does not.
type ManagedCluster struct {
	Name   string
	Source string
	// Owned is how many provider resources the cluster positively owns, when the
	// provider has been asked. -1 when it has not.
	Owned int
}

// managedClusters names every cluster this box's management cluster created,
// from every source that can still answer, most authoritative first.
//
// THREE SOURCES, UNIONED, because each is the only one available in some state
// of a box: the live cluster answers until it is gone, the repository answers
// after that, and --spoke answers when neither is to hand. A gate that consulted
// only the live cluster would pass for exactly the box that most needs it -- the
// unreachable one, whose workload clusters are the ones that get stranded.
//
// Clusters named in Scope are left out: this run destroys them, first and
// before the management cluster, which is the ordering rather than a breach of
// it.
//
// includeDeclared separates the two gates, and the separation matters in both
// directions. The gate that runs before anything is destroyed takes the live
// cluster's answer only: a declaration cannot be checked without the provider,
// and a stale one -- a cluster already destroyed, its directory not yet removed
// -- would block a teardown that is entirely correct. The gate at the provider
// takes the whole union, because by then every name can be checked against what
// the project actually holds.
func (o *Orchestrator) managedClusters(reported []string, includeDeclared bool) []ManagedCluster {
	scoped := map[string]bool{}
	for _, name := range o.Scope {
		scoped[strings.TrimSpace(name)] = true
	}

	seen := map[string]string{}
	note := func(name, source string) {
		name = strings.TrimSpace(name)
		if name == "" || name == o.ClusterName || scoped[name] {
			return
		}
		if _, already := seen[name]; already {
			return
		}
		seen[name] = source
	}

	for _, name := range reported {
		note(name, "the management cluster's own CAPI inventory")
	}
	if dir := strings.TrimSpace(o.GitopsDir); includeDeclared && dir != "" {
		declared, err := tenant.DeclaredWorkloadClusters(dir, o.ClusterName)
		if err == nil {
			for _, name := range declared {
				note(name, fmt.Sprintf("declared at %s",
					filepath.Join("registry", "clusters", name, "infrastructure")))
			}
		}
	}
	if includeDeclared {
		for _, name := range o.Spokes {
			note(name, "--spoke")
		}
	}

	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]ManagedCluster, 0, len(names))
	for _, name := range names {
		out = append(out, ManagedCluster{Name: name, Source: seen[name], Owned: -1})
	}
	return out
}

// refuseWhileManaged refuses a management cluster teardown while the cluster
// itself still lists clusters it manages.
//
// Asked of the live cluster and answered before anything is destroyed -- before
// the drain and before the finalizer strip, both of which are already
// irreversible for the box. By the time the provider is reached the management
// cluster's own Kubernetes cleanup has run, and a refusal there would be a
// refusal after the damage.
func (o *Orchestrator) refuseWhileManaged(managed []ManagedCluster) error {
	if len(managed) == 0 {
		return nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s manages %s that %s still there:\n\n",
		o.ClusterName, plural(len(managed), "a cluster", fmt.Sprintf("%d clusters", len(managed))),
		plural(len(managed), "is", "are"))
	for _, m := range managed {
		fmt.Fprintf(&b, "  %-32s %s\n", m.Name, m.Source)
	}
	fmt.Fprintf(&b, "\nA management cluster is not torn down while a cluster it manages is running.\n"+
		"It holds that cluster's CAPI objects and the only controller that can deprovision\n"+
		"its load balancers, so once this box is gone the platform cannot delete the cluster\n"+
		"at all: its servers keep running, and keep billing, with nothing left that knows\n"+
		"they exist.\n\n")
	fmt.Fprintf(&b, "Remove each one first. That is a declaration change and then a destroy:\n\n")
	for _, m := range managed {
		fmt.Fprintf(&b, "  kakt tenant remove-cluster --name %s --mgmt-cluster %s --gitops-dir %s\n",
			m.Name, o.ClusterName, o.gitopsDirForMessage())
		fmt.Fprintf(&b, "  # commit and push, wait for the management cluster to sync, then:\n")
		fmt.Fprintf(&b, "  kakt tenant remove-cluster --name %s --mgmt-cluster %s --gitops-dir %s --destroy\n\n",
			m.Name, o.ClusterName, o.gitopsDirForMessage())
	}
	fmt.Fprintf(&b, "Then tear this box down.")
	return fmt.Errorf("%w\n\n%s", ErrOrderViolation, b.String())
}

// refuseWhileManagedResourcesRemain refuses on the provider's answer rather than
// the cluster's, for the clusters this run is not destroying.
//
// Both checks exist because they fail in opposite directions. The cluster-side
// one is blind to a workload cluster whose CAPI objects were deleted while its
// servers survived -- a half-finished removal, which is a state this platform
// reaches often enough to have a command for. The provider-side one is blind to
// a cluster that was declared and never provisioned. Neither answer is the other.
//
// A cluster the provider says owns nothing is gone for this purpose, stated as
// what was established: no resource in the project carries its ownership label.
func (o *Orchestrator) refuseWhileManagedResourcesRemain(inv Inventory, managed []ManagedCluster) error {
	if len(managed) == 0 || !inv.Queried {
		return nil
	}

	names := make([]string, 0, len(managed))
	for _, m := range managed {
		names = append(names, m.Name)
	}
	counts := OwnedByCluster(inv.Resources(), names)

	var remaining []ManagedCluster
	for _, m := range managed {
		if n := counts[m.Name]; n > 0 {
			m.Owned = n
			remaining = append(remaining, m)
		}
	}
	if len(remaining) == 0 {
		return nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "nothing was deleted. %s still %s provider resources, and this run would\n"+
		"strand them:\n\n",
		plural(len(remaining), "A cluster this box manages", "Clusters this box manages"),
		plural(len(remaining), "has", "have"))
	for _, m := range remaining {
		fmt.Fprintf(&b, "  %-32s %d resource(s) labelled %s%s=%s   (%s)\n",
			m.Name, m.Owned, ownershipLabelPrefix, m.Name, lifecycleOwned, m.Source)
	}
	fmt.Fprintf(&b, "\nThe management cluster is destroyed last, and only once the clusters it\n"+
		"manages are gone. Destroy each of these first:\n\n")
	for _, m := range remaining {
		fmt.Fprintf(&b, "  kakt spoke teardown --name %s --mgmt-cluster %s --force\n", m.Name, o.ClusterName)
	}
	fmt.Fprintf(&b, "\nIf the management cluster still holds CAPI objects for one of them, remove it\n"+
		"through its declaration instead -- kakt tenant remove-cluster --destroy -- so the\n"+
		"platform deletes the cluster rather than its servers being deleted from under it.\n")
	fmt.Fprintf(&b, "\nOr, if this run is meant to take them too, name them -- they are then\n"+
		"destroyed before the management cluster, in that order:\n\n"+
		"  --infrastructure-only %s,%s", o.ClusterName, strings.Join(names, ","))
	return fmt.Errorf("%w\n\n%s", ErrOrderViolation, b.String())
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// managedNames is the names alone, for a one-line report.
func managedNames(managed []ManagedCluster) []string {
	out := make([]string, 0, len(managed))
	for _, m := range managed {
		out = append(out, m.Name)
	}
	return out
}

// unclaimedFrom names the resources CAPH attributed to a cluster this run does
// not target: reported, never deleted.
//
// The label proves a resource belongs to SOME cluster, not to this box, and a
// Hetzner project can hold more than one box. Deleting on that evidence would
// make a teardown capable of destroying a cluster it was never pointed at.
func unclaimedFrom(inv Inventory) []string {
	var out []string
	for _, a := range inv.Excluded() {
		for key := range a.Labels {
			if strings.HasPrefix(key, ownershipLabelPrefix) {
				out = append(out, a.Name)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}
