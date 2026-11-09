package tenant

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// A cluster's existence is a commit; its destruction takes minutes. This file is
// what closes that gap honestly.
//
// Removing the claim file is the whole removal (ClusterRemoval), and the commit
// returns immediately while Crossplane, Cluster API and the provider spend
// several minutes unwinding the cluster. The withdrawn `--destroy` step waited
// and reported; nothing replaces the waiting, so something has to replace the
// reporting -- by OBSERVATION, never by inference from git. "The declaration is
// gone" is not "the infrastructure is destroyed", and the whole cost of getting
// that wrong is a cluster nobody is looking for any more.

// Presence is what was seen of one object, kept separate from what it means.
//
// Unknown is a first-class value and not an absence: a read that failed and an
// object that is gone are different findings, and conflating them is how a
// teardown reports success over running servers.
type Presence string

const (
	// Present: the object was read.
	Present Presence = "present"
	// Deleting: the object was read and carries a deletion timestamp.
	Deleting Presence = "deleting"
	// Absent: the object was asked for and is not there.
	Absent Presence = "absent"
	// Unknown: it could not be asked.
	Unknown Presence = "unknown"
)

// Observation is what cluster-status actually saw, with no conclusions in it.
type Observation struct {
	// Cluster is the workload cluster asked about.
	Cluster string
	// Declared is whether the claim file is in the working tree.
	Declared bool
	// Applications are the management cluster's two, by name.
	Applications map[string]Presence
	// Claim is the SpokePool on the management cluster.
	Claim Presence
	// CAPICluster is the Cluster API object.
	CAPICluster Presence
	// ProviderQueried records that the provider's inventory was read. Its count
	// means nothing unless this is true.
	ProviderQueried bool
	// ProviderOwned is how many provider resources carry this cluster's ownership
	// label.
	ProviderOwned int
	// Failures are conditions read off the objects, verbatim.
	Failures []string
}

// ClusterState is the answer, and there are five because there are five
// distinguishable situations -- not because five reads nicely.
type ClusterState string

const (
	// ClusterDeclared: the claim is in git and the platform holds the cluster.
	ClusterDeclared ClusterState = "declared"
	// ClusterDeleting: the claim is gone and the platform is unwinding the cluster.
	ClusterDeleting ClusterState = "deleting"
	// ClusterRemoved: the claim is gone, nothing holds the cluster, and the
	// provider reports nothing owned by it.
	ClusterRemoved ClusterState = "removed"
	// ClusterFailed: the deletion cannot complete, or the declaration is not being
	// honoured.
	ClusterFailed ClusterState = "failed"
	// ClusterUnknown: something could not be read, so nothing is concluded.
	ClusterUnknown ClusterState = "unknown"
)

// Classify turns observations into one state, and says which observation decided
// it.
//
// THE ORDER OF THESE CASES IS THE ARGUMENT. Unknown comes first, because a state
// derived from a failed read is worse than no state. Failed comes before
// deleting, because "still going" is the comfortable reading of a deletion that
// has nothing left to finish it. Removed is last and requires the provider to
// have been asked, since it is the only authority on whether the servers exist.
func Classify(o Observation) (ClusterState, []string) {
	var why []string
	note := func(format string, args ...any) { why = append(why, fmt.Sprintf(format, args...)) }

	appNames := make([]string, 0, len(o.Applications))
	for name := range o.Applications {
		appNames = append(appNames, name)
	}
	sort.Strings(appNames)

	var present, deleting, absent, unknown int
	for _, name := range appNames {
		switch o.Applications[name] {
		case Present:
			present++
		case Deleting:
			deleting++
			note("%s is being deleted", name)
		case Absent:
			absent++
		default:
			unknown++
		}
	}

	infra := o.Cluster + "-infrastructure"
	infraState := o.Applications[infra]
	if infraState == "" {
		infraState = Unknown
	}

	for _, f := range o.Failures {
		note("%s", f)
	}

	switch {
	case unknown > 0 || infraState == Unknown || o.Claim == Unknown || o.CAPICluster == Unknown:
		note("the management cluster could not be asked about this cluster")
		return ClusterUnknown, why

	case o.Declared && infraState == Absent:
		// The declaration is in git and nothing is generating from it. On a
		// repository migrated to the ApplicationSets this means the generator is
		// not matching the claim; on one that was not, it means the ApplicationSets
		// are not there at all.
		note("%s declares this cluster, but %s does not exist on the management cluster",
			RegistryDir+"/clusters/"+o.Cluster+"/infrastructure/spokepool.yaml", infra)
		return ClusterFailed, why

	case o.Declared:
		note("%s holds the claim; %s exists", RegistryDir+"/clusters/"+o.Cluster, infra)
		if deleting > 0 {
			// Declared and being deleted at once: someone deleted the Application by
			// hand, and the ApplicationSet will recreate it -- which for the
			// infrastructure one means a NEW cluster under a name that is not meant
			// to be reused.
			note("an Application is being deleted while the claim is still declared; " +
				"the ApplicationSet will recreate it")
			return ClusterFailed, why
		}
		return ClusterDeclared, why

	case present > 0 || deleting > 0 || o.Claim != Absent || o.CAPICluster != Absent:
		note("the claim is no longer declared; the platform is still unwinding the cluster")
		return ClusterDeleting, why

	case !o.ProviderQueried:
		note("the provider was not asked, so whether its resources are gone is not established")
		return ClusterUnknown, why

	case o.ProviderOwned > 0:
		// Nothing in the cluster holds it any more, and the servers are still
		// there. Nothing will collect them: this is the state the ordering gate in
		// `kakt teardown` exists to prevent reaching.
		note("%d provider resource(s) still carry this cluster's ownership label, and "+
			"nothing on the management cluster is left to delete them", o.ProviderOwned)
		return ClusterFailed, why

	default:
		note("no Application, no claim, no Cluster API object, and nothing in the " +
			"provider carries this cluster's ownership label")
		return ClusterRemoved, why
	}
}

// Render prints the observations and then the state, in that order.
//
// Evidence before conclusion, so an operator reading it can disagree with the
// conclusion. A status that printed only the verdict would be a thing to trust
// rather than a thing to check.
func (o Observation) Render(w io.Writer, state ClusterState, why []string) {
	fmt.Fprintf(w, "\ncluster  %s\n", o.Cluster)
	fmt.Fprintf(w, "\nObserved\n")
	fmt.Fprintf(w, "  %-34s %s\n", "declaration in git",
		map[bool]string{true: "present", false: "absent"}[o.Declared])

	names := make([]string, 0, len(o.Applications))
	for name := range o.Applications {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(w, "  %-34s %s\n", "Application "+name, o.Applications[name])
	}
	fmt.Fprintf(w, "  %-34s %s\n", "SpokePool "+o.Cluster, o.Claim)
	fmt.Fprintf(w, "  %-34s %s\n", "Cluster "+o.Cluster, o.CAPICluster)
	if o.ProviderQueried {
		fmt.Fprintf(w, "  %-34s %d owned\n", "provider resources", o.ProviderOwned)
	} else {
		fmt.Fprintf(w, "  %-34s %s\n", "provider resources", Unknown)
	}

	fmt.Fprintf(w, "\nstate    %s\n", state)
	for _, line := range why {
		fmt.Fprintf(w, "  - %s\n", line)
	}
	if state == ClusterRemoved {
		fmt.Fprintf(w, "\nIts secrets under /spoke-pool/%s/ and its backups under the %s/ prefix "+
			"remain.\nThe name is not reused: both outlive the cluster.\n", o.Cluster, o.Cluster)
	}
}

// ApplicationPresence maps a kubectl read of one Application to a Presence.
//
// The three inputs are the three answers kubectl gives: the object with a
// deletion timestamp, the object without one, and nothing at all. A read ERROR is
// not one of them and must not reach here -- the caller reports Unknown, because
// an unreadable management cluster is the one case where every conclusion below
// would be invented.
func ApplicationPresence(found bool, deletionTimestamp string) Presence {
	switch {
	case !found:
		return Absent
	case strings.TrimSpace(deletionTimestamp) != "":
		return Deleting
	default:
		return Present
	}
}
