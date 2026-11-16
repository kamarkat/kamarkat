package bootstrap

import (
	"fmt"
	"strings"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/state"
)

// bootstrapClusterPhases are the phases whose entire effect lives inside the
// ephemeral bootstrap cluster, and nowhere else.
//
// They are invalidated together because they are one thing: a kind cluster with
// cert-manager and the CAPI operator in it. Withdrawing bootstrap-create alone
// would rebuild the cluster and then skip the two phases that make it useful,
// and cluster-provision would ask an empty cluster to provision servers.
//
// cluster-provision is deliberately NOT here. It runs against the bootstrap
// cluster too, but what it creates is billed infrastructure at the provider,
// which no amount of rebuilding kind brings back under management.
var bootstrapClusterPhases = []state.BootstrapPhase{
	state.PhaseBootstrapCreate,
	state.PhaseDayZero,
	state.PhaseCAPIInit,
}

// reconcileBootstrapCluster settles what a resumed run may believe about the
// bootstrap cluster its state record names.
//
// THE RECORD IS DURABLE AND THE CLUSTER IS NOT. bs.BootstrapKubeconfig is a path
// under $TMPDIR, written by this binary on the machine that ran the previous
// attempt, and committed to the tenant's repository with the rest of the phase
// record. Every CI resume therefore starts on a runner whose /tmp is empty and
// whose kind cluster never existed, holding a record that says both are there.
//
// Before this, that cost every resumed tenant run. kubeconfigPaths raised "the
// bootstrap cluster's kubeconfig no longer resolves context ..." and the run
// died in under a minute -- whatever phase had actually failed, and including the
// ten phases from clusterclass-deploy onwards that do not use the bootstrap
// cluster at all and run after cleanup has deliberately deleted it. A box could
// be resumed on the operator's own machine, where the pinned file is still in
// /tmp, and not in the CI that builds it.
//
// Four answers, and which one applies is decided by what the run still needs and
// what the provider actually holds -- never by the phase list alone, because a
// phase that failed part way records nothing while having created servers:
//
//	the context resolves         nothing to do; the cluster is there
//	pivot-move is complete       forget the record. The bootstrap cluster has
//	                             been deleted by cleanup, on purpose, and no
//	                             remaining phase asks it anything
//	provisioning happened        refuse. The CAPI objects that managed those
//	                             servers were in the lost cluster, so they are
//	                             unreachable and rebuilding kind does not help
//	nothing was provisioned      withdraw the three bootstrap-cluster phases and
//	                             let the ordinary resume rebuild them
//
// The last is the common case and it costs about three minutes of kind. The
// third is the one worth stopping for, and it stops with the teardown
// instruction rather than a silent rebuild that would orphan billing servers.
func (o *Orchestrator) reconcileBootstrapCluster(stateMgr *state.StateManager, bs *state.BootstrapState) error {
	recorded := strings.TrimSpace(bs.BootstrapKubeconfig)
	if recorded == "" {
		// Nothing claimed, nothing to reconcile: either a fresh build or an
		// attempt that stopped before bootstrap-create recorded its file.
		return nil
	}

	kubeContext := strings.TrimSpace(bs.BootstrapContext)
	if kubeContext == "" {
		kubeContext = "kind-" + o.ClusterName
	}

	// The same question kubeconfigPaths asks, asked here first so the answer can
	// be acted on instead of only reported.
	if _, err := pinKubeconfig(recorded, kubeContext); err == nil {
		return nil
	}

	if o.phaseDone(bs, state.PhasePivotMove) {
		fmt.Printf("[recovery] The bootstrap cluster recorded at %s is gone, and this run does not\n", recorded)
		fmt.Println("[recovery]   need it: the pivot moved CAPI to the management cluster and cleanup")
		fmt.Println("[recovery]   deletes the bootstrap cluster. Forgetting the record.")
		bs.BootstrapKubeconfig = ""
		bs.BootstrapContext = ""
		return stateMgr.Save(bs)
	}

	// INFRASTRUCTURE MAY EXIST, established two ways because neither is
	// sufficient alone. The record says whether cluster-provision was recorded
	// complete; the provider says what this cluster owns right now, which is the
	// only account that covers a cluster-provision that created servers and then
	// failed before it could record anything.
	if o.phaseDone(bs, state.PhaseClusterProvision) || o.ProviderResourcesOwned > 0 {
		owned := "the provider reports no resources for it, so what it built is already gone"
		if o.ProviderResourcesOwned > 0 {
			owned = fmt.Sprintf("the provider still reports %d resource(s) owned by this cluster",
				o.ProviderResourcesOwned)
		}
		return fmt.Errorf("this cluster provisioned infrastructure from a bootstrap cluster that no longer exists.\n\n"+
			"state records %s for context %q, and it is not there -- so the CAPI\n"+
			"objects that manage this cluster's servers went with it, and %s.\n\n"+
			"Rebuilding the bootstrap cluster would not recover them: a new one knows\n"+
			"nothing about servers it did not create. Tear this cluster down and build\n"+
			"it again:\n\n"+
			"  kakt teardown --name %s --gitops-dir <this repository> --confirm",
			recorded, kubeContext, owned, o.ClusterName)
	}

	withdrawn := []string{}
	for _, phase := range bootstrapClusterPhases {
		if o.phaseDone(bs, phase) {
			bs.CompletedPhases = removePhase(bs.CompletedPhases, phase)
			withdrawn = append(withdrawn, string(phase))
		}
	}
	bs.BootstrapKubeconfig = ""
	bs.BootstrapContext = ""
	if len(bs.CompletedPhases) > 0 {
		bs.CurrentPhase = bs.CompletedPhases[len(bs.CompletedPhases)-1]
	} else {
		bs.CurrentPhase = state.PhasePreFlight
	}

	fmt.Printf("[recovery] The bootstrap cluster recorded at %s is gone.\n", recorded)
	fmt.Println("[recovery]   It is ephemeral and machine-local -- a resumed run on another runner")
	fmt.Println("[recovery]   never has it -- and nothing was provisioned from it, so it is rebuilt.")
	if len(withdrawn) > 0 {
		fmt.Printf("[recovery]   Withdrawing %s; every later phase is skipped as usual.\n",
			strings.Join(withdrawn, ", "))
	}
	return stateMgr.Save(bs)
}
