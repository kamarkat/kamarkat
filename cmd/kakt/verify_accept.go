package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/state"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/versions"
	"github.com/kamarkat/kamarkat/internal/platform/embedded"
)

// Acceptance: the claim convergence cannot make (ADR-115).
//
// `kakt verify` asks whether a cluster stopped moving. That is the cluster's own
// reported state, read from its own API, and it is not the same question as
// whether the cluster does its job. A box once passed every convergence check in
// seven seconds while a DNS record pointed at a cluster replaced weeks earlier
// and a gateway route had been dropped during programming. Nothing was still
// moving, so no amount of waiting would have surfaced either.
//
// The assertions that WOULD have are the validation modules, which this platform
// has carried in the released binary all along and never invoked from it. This
// file is that invocation. It adds no second validation engine: the modules stay
// the acceptance mechanism, the health waiter stays convergence, and the verdict
// is the runner's exit status.
const (
	// A COMPLETE assessment goes through the same entry point the bootstrap and
	// CI use, so "was this box assessed" has one answer wherever it is asked --
	// and so the assessment leaves a recorded report behind.
	acceptEntryPoint = "scripts/validate-all.sh"

	// A SUBSET goes straight to the runner, because selecting checks is a
	// check-level decision the phase-level entry point cannot express: its own
	// --only names phases. A subset also writes no report, deliberately. A
	// recorded report is a claim about a cluster, and a run that accepts nothing
	// has no such claim to record.
	acceptRunner = "scripts/validate/run.sh"
	acceptPhase  = "cluster"
)

// acceptOptions is what the caller decided, separated from how it is run so that
// Day-0 and an operator's workstation reach the same code path.
type acceptOptions struct {
	// Cluster is the cluster being assessed, as its declaration names it. It
	// reaches the modules as CLUSTER_NAME, which several of them use to read the
	// box's own declarations rather than the platform's defaults.
	Cluster string

	// Kubeconfig is the management cluster's, already resolved by the caller.
	Kubeconfig string

	// GitopsDir is the box's repository. The modules read declarations from it,
	// which is what makes applicability a property of the box rather than of the
	// run.
	GitopsDir string

	// SpokePool names WHICH workload cluster to assess when the box declares
	// several. Empty is not an error: the workload-cluster modules then report
	// that, and the rest are unaffected.
	SpokePool string

	// Only restricts the run to named checks. A subset establishes what it ran
	// and accepts nothing, which the caller is responsible for reporting as such.
	Only []string

	// Explain resolves applicability and runs nothing.
	Explain bool

	// PlatformDir is an already-materialized platform tree. Day-0 has one open
	// for the whole bootstrap, and extracting a second copy of the release to
	// assess the box it just built would be both wasteful and a chance for the
	// two copies to differ.
	PlatformDir string
}

// Subset reports whether this assessment can accept a cluster at all.
//
// Kept beside the options rather than computed at each call site, because the
// distinction decides what the verdict may say and getting it wrong is how a
// named check becomes a claim about every check that never ran.
func (o acceptOptions) Subset() bool { return len(o.Only) > 0 || o.Explain }

// runAcceptance assesses one cluster and returns the runner's verdict.
//
// The platform content comes from the same place Day-0 takes it: the release's
// own embedded tree, or the checkout for a development build. A tenant's runner
// needs no platform checkout, which is the property that lets this run at the
// end of a bootstrap rather than only on a maintainer's machine.
func runAcceptance(ctx context.Context, o acceptOptions) error {
	platformDir := o.PlatformDir
	if platformDir == "" {
		resolved, cleanup, err := dayZeroScripts()
		if err != nil {
			return fmt.Errorf("the platform's checks could not be located: %w", err)
		}
		defer cleanup()
		platformDir = resolved
	}

	entry := acceptEntryPoint
	if o.Subset() {
		entry = acceptRunner
	}
	if err := embedded.RequireMaterialized(platformDir, entry); err != nil {
		return err
	}

	args := []string{filepath.Join(platformDir, entry)}
	if o.Subset() {
		args = append(args, acceptPhase, "--mode=final")
		if len(o.Only) > 0 {
			args = append(args, "--only="+strings.Join(o.Only, ","))
		}
		if o.Explain {
			args = append(args, "--explain")
		}
	} else {
		// The platform phase is the cluster assessment. Its own --only names a
		// phase, which is why a check subset cannot come through here.
		args = append(args, "--only=platform")
	}

	// HUB_KUBECONFIG is what the modules read; KUBECONFIG is set as well because
	// a module that shells out to a tool rather than using the helper inherits it.
	env := append(os.Environ(),
		"HUB_KUBECONFIG="+o.Kubeconfig,
		"KUBECONFIG="+o.Kubeconfig,
		"CLUSTER_NAME="+o.Cluster,
		"ZERO_OPS_DIR="+o.GitopsDir,
	)
	if o.SpokePool != "" {
		env = append(env, "SPOKEPOOL_NAME="+o.SpokePool)
	}
	if versions.IsReleaseBuild() {
		env = append(env, embedded.ReleasedScriptsEnv+"=1")
	}

	c := exec.CommandContext(ctx, "bash", args...)
	c.Env = env
	c.Dir = platformDir
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("%w", err)
	}
	return nil
}

// clusterLabel names the subject of a message when the caller passed no name.
//
// A verdict that reads "accepted" with nothing to say WHAT was accepted is the
// kind of line that gets pasted into an incident channel and believed about the
// wrong cluster.
func clusterLabel(name, fallback string) string {
	if name == "" {
		return fallback
	}
	return name
}

// reportAcceptanceVerdict says what the run established, and what it did not.
//
// A subset reports its own narrowness in the words the contract requires, because
// a green line that reads like acceptance is the failure this whole change
// exists to remove. Only a complete assessment may say a cluster is accepted.
func reportAcceptanceVerdict(o acceptOptions, err error) error {
	switch {
	case o.Explain:
		fmt.Println("[verify] nothing was asserted — applicability only")
		return err
	case err != nil && o.Subset():
		return fmt.Errorf("selected validation failed on %s: %w", clusterLabel(o.Cluster, "the cluster"), err)
	case err != nil:
		return fmt.Errorf("%s is not accepted: %w", clusterLabel(o.Cluster, "the cluster"), err)
	case o.Subset():
		fmt.Printf("[verify] selected validation passed — complete cluster acceptance not assessed\n")
		return nil
	default:
		fmt.Printf("[verify] ✓ %s accepted\n", clusterLabel(o.Cluster, "the cluster"))
		return nil
	}
}

// recordAcceptance writes the completion record for the assessment that just
// passed, into the same state file the bootstrap's phases use.
//
// It is recorded AFTER the assessment, never consulted before one. Acceptance is
// the one claim in a bootstrap that cannot be inherited from a record: every
// other phase builds something and the thing is still there on a re-run, while a
// cluster accepted last week can be failing today through a DNS record edited by
// hand or a credential expired at a provider. So a re-run assesses again, and
// this is the audit trail of when it last held -- not a reason to skip.
//
// A failure to record is reported and does not fail the run. The assessment
// passed; losing the note about it is not the same as the cluster being
// unacceptable, and failing here would turn a bookkeeping problem into "your box
// does not work".
func recordAcceptance(gitopsDir, cluster string) {
	mgr := state.NewTenantStateManager(gitopsDir, cluster)
	bs, err := mgr.Load()
	if err != nil || bs == nil {
		fmt.Printf("[verify] note: the acceptance could not be recorded (%v); the assessment itself passed\n", err)
		return
	}
	for _, p := range bs.CompletedPhases {
		if p == state.PhaseClusterAccepted {
			if err := mgr.Save(bs); err != nil {
				fmt.Printf("[verify] note: the acceptance record could not be refreshed (%v)\n", err)
			}
			return
		}
	}
	bs.CompletedPhases = append(bs.CompletedPhases, state.PhaseClusterAccepted)
	if err := mgr.Save(bs); err != nil {
		fmt.Printf("[verify] note: the acceptance could not be recorded (%v); the assessment itself passed\n", err)
	}
}
