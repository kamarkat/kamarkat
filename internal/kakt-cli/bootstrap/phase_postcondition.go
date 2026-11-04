package bootstrap

import (
	"context"
	"fmt"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/state"
)

// What must be TRUE for a phase to count as done, as distinct from what the
// phase does.
//
// A registry rather than a check written at the call site, because two readers
// need the same answer and they must not be able to disagree:
//
//	runFresh            -- skip this phase, or run it again?
//	handleExistingState -- is this bootstrap actually finished?
//
// The second is where the absence of this cost the most. A bootstrap that
// recorded `complete` refused every later run with "Cluster already
// bootstrapped. No further CLI operations permitted per ADR-040" -- so a box
// whose identity provider had never served could not be repaired by the tool
// that built it, and the phase-level check below would never be reached to
// notice. Completion has to be verified at the point it is claimed, and the
// outermost claim is the one that shuts the door.
//
// ADR-040 is not weakened by this. It bars the CLI from operating a cluster that
// is built; it does not require the CLI to believe a record over the cluster it
// can see. A phase whose postcondition fails was never built in the first place.
//
// Sparse on purpose. A phase absent from this map keeps exactly its previous
// behaviour, so postconditions are adopted where they earn their keep rather
// than invented for twenty-four phases at once. The bar for adding one: it is
// cheap, it is read-only, and it answers "did this phase's effect actually
// happen" rather than "did the command exit zero".
func (o *Orchestrator) phasePostconditions(kubeconfig string) map[state.BootstrapPhase]func(context.Context) error {
	return map[state.BootstrapPhase]func(context.Context) error{
		// Everything downstream of boundary 04 -- iam-admin-pat, the identity
		// token, identity-service-credentials, kube-sbt -- waits on Zitadel. The
		// boundary having been applied is not the same claim as Zitadel serving,
		// and the gap between them is where a bootstrap reports success over a
		// box that cannot authenticate anyone.
		state.PhaseBoundary04: func(ctx context.Context) error {
			return o.zitadelServing(ctx, kubeconfig)
		},

		// PivotReady stages the tailnet credential on the hub, because
		// `clusterctl move` does not carry a plain Secret across the pivot.
		// Boxes built before it did that recorded this phase complete anyway, and
		// their hub-operator logs "Source secret not found, skipping" for ever.
		state.PhasePivotReady: func(ctx context.Context) error {
			return o.tailnetCredentialStagedOnHub(ctx, kubeconfig)
		},

		// A box that asked for on-prem nodes and has none did not complete this
		// phase, whatever the record says -- most often because it was recorded by
		// a run under a different provider, where "no home workers" was correct.
		state.PhaseOnPremJoin: func(ctx context.Context) error {
			return o.onPremWorkersPresent(ctx, kubeconfig)
		},

		// Every public URL for this box resolves to one address, and the platform
		// resolves it in-cluster (hub-operator, from kube-system/kubeadm-config).
		// When it cannot, no hub hostname is published and ACME issues nothing --
		// a state that used to be a warning the bootstrap printed and walked past,
		// then reported success over. The operator reports it as a condition; this
		// is what makes the condition matter.
		state.PhaseBoundary06: func(ctx context.Context) error {
			return o.hubIngressAddressResolved(ctx, kubeconfig)
		},
	}
}

// unmetPostconditions returns the completed phases whose postconditions no
// longer hold, in the order the map declares nothing about -- callers report
// them, they do not sequence from them.
func (o *Orchestrator) unmetPostconditions(ctx context.Context, bs *state.BootstrapState, kubeconfig string) map[state.BootstrapPhase]error {
	unmet := map[state.BootstrapPhase]error{}
	for phase, check := range o.phasePostconditions(kubeconfig) {
		if !o.phaseDone(bs, phase) {
			continue
		}
		if err := check(ctx); err != nil {
			unmet[phase] = err
		}
	}
	return unmet
}

// withdrawCompletion removes a bootstrap's `complete` record and the records of
// the phases that did not hold, so the ordinary resume path re-runs them.
//
// `complete` goes too, and must: it is the record handleExistingState reads, so
// leaving it would withdraw the phase and then refuse the run that would redo it.
func withdrawCompletion(bs *state.BootstrapState, phases map[state.BootstrapPhase]error) {
	bs.CompletedPhases = removePhase(bs.CompletedPhases, state.PhaseComplete)
	for phase := range phases {
		bs.CompletedPhases = removePhase(bs.CompletedPhases, phase)
	}
	if len(bs.CompletedPhases) > 0 {
		bs.CurrentPhase = bs.CompletedPhases[len(bs.CompletedPhases)-1]
	}
}

// describeUnmet renders why a bootstrap that called itself complete is not.
func describeUnmet(unmet map[state.BootstrapPhase]error) string {
	out := ""
	for phase, err := range unmet {
		out += fmt.Sprintf("[recovery]   %s: %v\n", phase, err)
	}
	return out
}
