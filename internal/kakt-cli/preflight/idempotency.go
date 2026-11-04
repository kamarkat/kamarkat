package preflight

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/state"
)

// IdempotencyValidator checks for existing cluster
type IdempotencyValidator struct {
	ClusterName      string
	Namespace        string
	BootstrapContext string
	Upgrade          bool
}

func (v *IdempotencyValidator) Validate(ctx context.Context) error {
	mgr := state.NewStateManager(v.ClusterName)
	
	if !mgr.Exists() {
		return nil // No existing cluster
	}
	
	if v.Upgrade {
		return nil // Upgrade mode - allow to proceed
	}
	
	// Load state to check progress
	bootstrapState, err := mgr.Load()
	if err != nil {
		fmt.Printf("[preflight] Warning: failed to load state: %v\n", err)
		return nil
	}
	
	// If cluster provisioning or later phases in progress/completed, allow recovery
	if bootstrapState.CurrentPhase == state.PhaseClusterProvision || 
	   bootstrapState.CurrentPhase == state.PhasePivotMove || 
	   bootstrapState.CurrentPhase == state.PhasePivotReady || 
	   bootstrapState.CurrentPhase == state.PhaseClusterClassDeploy || 
	   bootstrapState.CurrentPhase == state.PhasePlatformDeploy || 
	   bootstrapState.CurrentPhase == state.PhaseComplete {
		fmt.Printf("[preflight] Found existing state at phase '%s' - will resume\n", bootstrapState.CurrentPhase)
		return nil
	}
	
	// Also check completed phases
	if len(bootstrapState.CompletedPhases) > 0 {
		lastPhase := bootstrapState.CompletedPhases[len(bootstrapState.CompletedPhases)-1]
		if lastPhase == state.PhaseClusterProvision || 
		   lastPhase == state.PhasePivotMove || 
		   lastPhase == state.PhasePivotReady || 
		   lastPhase == state.PhaseClusterClassDeploy || 
		   lastPhase == state.PhasePlatformDeploy {
			fmt.Printf("[preflight] Found existing state at phase '%s' - will resume\n", lastPhase)
			return nil
		}
	}
	
	// Clean up stale resources automatically (only for early phases)
	fmt.Printf("[preflight] Cleaning up stale resources for cluster '%s'...\n", v.ClusterName)
	
	kubectlContext := v.BootstrapContext
	if kubectlContext == "" {
		kubectlContext = "kind-bootstrap-zero-ops"
	}
	
	// Delete cluster first (required before ClusterClass can be deleted)
	// Fully qualified, and here it matters most: an unqualified "cluster" can
	// resolve to CNPG's kind, so this would delete nothing and report success
	// through --ignore-not-found, leaving the CAPI Cluster in place for the
	// ClusterClass deletion below to fail on.
	cmd := exec.CommandContext(ctx, "kubectl", "delete", "clusters.cluster.x-k8s.io", v.ClusterName,
		"--context", kubectlContext, "-n", v.Namespace, "--wait=true", "--timeout=60s", "--ignore-not-found=true")
	if err := cmd.Run(); err != nil {
		fmt.Printf("[preflight] Warning: failed to delete cluster: %v\n", err)
	}
	
	// Delete state file
	if err := mgr.Delete(); err != nil {
		fmt.Printf("[preflight] Warning: failed to delete state: %v\n", err)
	}
	
	fmt.Println("[preflight] ✓ Stale resources cleaned up")
	return nil
}
