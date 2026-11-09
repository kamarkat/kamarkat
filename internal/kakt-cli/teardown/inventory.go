package teardown

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/hetznercloud/hcloud-go/hcloud"
)

// CAPH's ownership contract, from api/v1beta1/tags.go at v1.0.7:
//
//	NameHetznerProviderOwned = "caph-" + "cluster-"   // the label key prefix
//	ResourceLifecycleOwned   = "owned"
//	ResourceLifecycleShared  = "shared"
//
// The key names the cluster (HetznerCluster.ClusterTagKey) and the VALUE is the
// lifecycle. Both halves matter: `shared` marks a resource another cluster also
// uses, so deleting it through one cluster's cleanup takes it from the others.
const (
	ownershipLabelPrefix = "caph-cluster-"
	lifecycleOwned       = "owned"
	lifecycleShared      = "shared"
)

// Resource is one provider object, as the provider reports it.
//
// The immutable ID is carried beside the name because the name is not an
// identity: two boxes can hold servers with the same name, and an operator
// confirming a deletion is entitled to see the thing that cannot be mistaken.
type Resource struct {
	Kind   string
	Name   string
	ID     int
	Labels map[string]string
}

// Attribution is a resource and whether this teardown may delete it.
//
// Eligibility is recorded with its reason so the inventory can show what is
// being skipped and why. A resource excluded for the wrong reason is a resource
// left running and billing, and the only way anyone notices is if the exclusion
// is visible.
type Attribution struct {
	Resource
	// Cluster is the target cluster that owns it, when one does.
	Cluster string
	// Lifecycle is the ownership value found on the label, when there is one.
	Lifecycle string
	Eligible  bool
	Reason    string
}

// Attribute decides, for each resource, whether the named clusters own it.
//
// EXACT KEY, REQUIRED VALUE. Ownership is `caph-cluster-<cluster>: owned` for a
// cluster in the target set. Everything else is excluded with a reason:
//
//   - no caph-cluster-* label at all: the provider cannot attribute it, so
//     neither can this. A resource created by hand, or by something other than
//     CAPH, is not this teardown's to delete.
//   - the label names another cluster: that is the case that must never slip.
//     A box's clusters share a name prefix by construction (ADR-082), so
//     nutgraf-01's servers sit beside nutgraf-hub's and differ only here.
//   - the value is `shared`: another cluster uses it too.
//   - any other value: unexpected means unknown, and unknown is not permission.
func Attribute(resources []Resource, targets []string) []Attribution {
	want := map[string]bool{}
	for _, t := range targets {
		if t = strings.TrimSpace(t); t != "" {
			want[t] = true
		}
	}

	out := make([]Attribution, 0, len(resources))
	for _, r := range resources {
		a := Attribution{Resource: r}

		var labelled []string
		for key, value := range r.Labels {
			if !strings.HasPrefix(key, ownershipLabelPrefix) {
				continue
			}
			cluster := strings.TrimPrefix(key, ownershipLabelPrefix)
			labelled = append(labelled, cluster)
			if !want[cluster] {
				continue
			}
			a.Cluster, a.Lifecycle = cluster, value
			switch value {
			case lifecycleOwned:
				a.Eligible, a.Reason = true, ""
			case lifecycleShared:
				a.Eligible = false
				a.Reason = fmt.Sprintf("shared with other clusters (%s=%s)", key, value)
			default:
				a.Eligible = false
				a.Reason = fmt.Sprintf("unexpected ownership value %q on %s; only %q authorizes deletion",
					value, key, lifecycleOwned)
			}
			break
		}

		if a.Cluster == "" {
			sort.Strings(labelled)
			switch {
			case len(labelled) == 0:
				a.Reason = "no CAPH ownership label: the provider cannot attribute it to a cluster"
			default:
				a.Reason = fmt.Sprintf("owned by %s, which this operation does not target",
					strings.Join(labelled, ", "))
			}
		}
		out = append(out, a)
	}
	return out
}

// Inventory is the resolved target set: what was asked for, and what the
// provider actually holds.
type Inventory struct {
	// Management is the hub this operation names.
	Management string
	// Workload is every other cluster in scope, which is an explicit list and
	// never the result of matching names.
	Workload []string
	// Items is every resource the provider reported that carries any CAPH
	// ownership label, attributed.
	Items []Attribution
	// Queried records that the provider inventory was read successfully. Absence
	// is only meaningful when it was.
	Queried bool
}

// Eligible is what may be deleted.
func (i Inventory) Eligible() []Attribution {
	var out []Attribution
	for _, a := range i.Items {
		if a.Eligible {
			out = append(out, a)
		}
	}
	return out
}

// Excluded is what may not, each with its reason.
func (i Inventory) Excluded() []Attribution {
	var out []Attribution
	for _, a := range i.Items {
		if !a.Eligible {
			out = append(out, a)
		}
	}
	return out
}

// Resources returns the raw provider objects the inventory was built from, so
// another question can be asked of the same read.
//
// One query, several questions. The ordering gate has to know whether a cluster
// OUTSIDE this operation's target set still has resources, and re-listing the
// project to find out would be a second inventory that can disagree with the one
// the operator was shown.
func (i Inventory) Resources() []Resource {
	out := make([]Resource, 0, len(i.Items))
	for _, a := range i.Items {
		out = append(out, a.Resource)
	}
	return out
}

// OwnedByCluster counts, for each named cluster, the resources it positively
// owns -- by the same rule that authorizes deletion, so a cluster this reports
// as having none is a cluster a teardown of it would find nothing to delete.
func OwnedByCluster(resources []Resource, clusters []string) map[string]int {
	counts := map[string]int{}
	for _, cluster := range clusters {
		counts[cluster] = 0
	}
	for _, a := range Attribute(resources, clusters) {
		if a.Eligible {
			counts[a.Cluster]++
		}
	}
	return counts
}

// State is what the provider and the cluster together say about this box.
type State string

const (
	// StateReachable: the infrastructure exists and its Kubernetes API answers.
	StateReachable State = "infrastructure present, Kubernetes reachable"
	// StateInfrastructureOnly: the infrastructure exists, the API does not answer.
	StateInfrastructureOnly State = "infrastructure present, Kubernetes unavailable"
	// StateNoOwnedResources: the inventory was read and nothing in it is
	// positively owned by the clusters in scope.
	//
	// WORDED AS A STATEMENT ABOUT OWNERSHIP, not about existence. "Already absent"
	// claimed more than the query supports: unlabelled and ambiguous resources may
	// well be sitting there, and they are reported separately rather than
	// described as gone. What is established is that nothing carries
	// caph-cluster-<target>=owned.
	StateNoOwnedResources State = "no owned resources found"
	// StateUncertain: the inventory could not be read, so nothing is established.
	StateUncertain State = "infrastructure state unknown"
)

// Classify is the one place these four are decided.
//
// A MISSING KUBECONFIG SAYS NOTHING ABOUT THE SERVERS. That inference is what
// made a teardown destroy nothing and report success: no local kubeconfig was
// read as "already gone". Absence is a statement about the provider's inventory,
// so it requires a successful query, and an unsuccessful one is uncertainty
// rather than emptiness.
func Classify(inv Inventory, kubernetesReachable bool) State {
	if !inv.Queried {
		return StateUncertain
	}
	// Nothing positively owned is the same answer whether or not other resources
	// are present: there is nothing this operation may delete. The excluded ones
	// are still reported, so an operator can see what was found and decide.
	if len(inv.Eligible()) == 0 {
		return StateNoOwnedResources
	}
	if kubernetesReachable {
		return StateReachable
	}
	return StateInfrastructureOnly
}

// Render prints the resolved target set an operator is about to confirm.
//
// Names AND ids, grouped by kind, with the ownership label that admits each one
// and the reason for each exclusion. The point is that the operator can see that
// a workload cluster's resources are not in the list before anything is deleted,
// rather than reading it afterwards in the output of what was destroyed.
func (i Inventory) Render(w io.Writer, state State, outstanding []string) {
	fmt.Fprintf(w, "\nTarget\n")
	fmt.Fprintf(w, "  management cluster  %s\n", i.Management)
	if len(i.Workload) > 0 {
		fmt.Fprintf(w, "  workload clusters   %s\n", strings.Join(i.Workload, ", "))
	} else {
		fmt.Fprintf(w, "  workload clusters   none in scope\n")
	}
	fmt.Fprintf(w, "  state               %s\n", state)

	eligible, excluded := i.Eligible(), i.Excluded()

	fmt.Fprintf(w, "\nTo delete (%d)\n", len(eligible))
	if len(eligible) == 0 {
		fmt.Fprintf(w, "  nothing: no resource is positively owned by the clusters above\n")
	}
	for _, a := range byKind(eligible) {
		fmt.Fprintf(w, "  %-16s %-40s id=%-12d %s%s=%s\n",
			a.Kind, a.Name, a.ID, ownershipLabelPrefix, a.Cluster, a.Lifecycle)
	}

	if len(excluded) > 0 {
		fmt.Fprintf(w, "\nExcluded (%d) -- not deleted\n", len(excluded))
		for _, a := range byKind(excluded) {
			fmt.Fprintf(w, "  %-16s %-40s id=%-12d %s\n", a.Kind, a.Name, a.ID, a.Reason)
		}
	}

	if len(outstanding) > 0 {
		fmt.Fprintf(w, "\nWill NOT be done (%d)\n", len(outstanding))
		for _, item := range outstanding {
			fmt.Fprintf(w, "  - %s\n", item)
		}
	}
}

// byKind groups for reading: an operator scans a list of servers, then a list of
// volumes, not an interleaving in whatever order the API answered.
func byKind(in []Attribution) []Attribution {
	out := append([]Attribution(nil), in...)
	sort.SliceStable(out, func(x, y int) bool {
		if out[x].Kind != out[y].Kind {
			return out[x].Kind < out[y].Kind
		}
		return out[x].Name < out[y].Name
	})
	return out
}

// discoverInventory asks the provider what this box actually has.
//
// Every resource type the deletion path below handles is enumerated here, so the
// report an operator confirms against is the same set that will be considered --
// a type listed there and missing here would be deleted without having been
// shown.
//
// hcloud-go's All* calls follow pagination internally, which is why they are used
// in preference to the page-by-page form: a box larger than one page would
// otherwise report a truncated inventory and look partly absent.
func (o *Orchestrator) discoverInventory(ctx context.Context, client *hcloud.Client, targets []string) (Inventory, error) {
	var resources []Resource

	servers, err := client.Server.All(ctx)
	if err != nil {
		return Inventory{}, fmt.Errorf("list servers: %w", err)
	}
	for _, s := range servers {
		resources = append(resources, Resource{Kind: "server", Name: s.Name, ID: s.ID, Labels: s.Labels})
	}

	lbs, err := client.LoadBalancer.All(ctx)
	if err != nil {
		return Inventory{}, fmt.Errorf("list load balancers: %w", err)
	}
	for _, lb := range lbs {
		resources = append(resources, Resource{Kind: "load-balancer", Name: lb.Name, ID: lb.ID, Labels: lb.Labels})
	}

	volumes, err := client.Volume.All(ctx)
	if err != nil {
		return Inventory{}, fmt.Errorf("list volumes: %w", err)
	}
	for _, v := range volumes {
		resources = append(resources, Resource{Kind: "volume", Name: v.Name, ID: v.ID, Labels: v.Labels})
	}

	groups, err := client.PlacementGroup.All(ctx)
	if err != nil {
		return Inventory{}, fmt.Errorf("list placement groups: %w", err)
	}
	for _, g := range groups {
		resources = append(resources, Resource{Kind: "placement-group", Name: g.Name, ID: g.ID, Labels: g.Labels})
	}

	firewalls, err := client.Firewall.All(ctx)
	if err != nil {
		return Inventory{}, fmt.Errorf("list firewalls: %w", err)
	}
	for _, f := range firewalls {
		resources = append(resources, Resource{Kind: "firewall", Name: f.Name, ID: f.ID, Labels: f.Labels})
	}

	floating, err := client.FloatingIP.All(ctx)
	if err != nil {
		return Inventory{}, fmt.Errorf("list floating IPs: %w", err)
	}
	for _, ip := range floating {
		resources = append(resources, Resource{Kind: "floating-ip", Name: ip.Name, ID: ip.ID, Labels: ip.Labels})
	}

	// Networks are enumerated because step 8 of the deletion deletes them. A kind
	// the deleter handles and the inventory does not would be destroyed without
	// having been shown -- which is the one thing this report exists to prevent.
	networks, err := client.Network.All(ctx)
	if err != nil {
		return Inventory{}, fmt.Errorf("list networks: %w", err)
	}
	for _, n := range networks {
		resources = append(resources, Resource{Kind: "network", Name: n.Name, ID: n.ID, Labels: n.Labels})
	}

	primary, err := client.PrimaryIP.All(ctx)
	if err != nil {
		return Inventory{}, fmt.Errorf("list primary IPs: %w", err)
	}
	for _, ip := range primary {
		resources = append(resources, Resource{Kind: "primary-ip", Name: ip.Name, ID: ip.ID, Labels: ip.Labels})
	}

	// Only resources CAPH labelled are reported: the rest of a Hetzner project
	// belongs to whatever else the tenant runs there, and listing it as
	// "excluded" would bury the exclusions that matter in an account's worth of
	// unrelated objects.
	var labelled []Resource
	for _, r := range resources {
		for key := range r.Labels {
			if strings.HasPrefix(key, ownershipLabelPrefix) {
				labelled = append(labelled, r)
				break
			}
		}
	}

	return Inventory{Items: Attribute(labelled, targets), Queried: true}, nil
}
