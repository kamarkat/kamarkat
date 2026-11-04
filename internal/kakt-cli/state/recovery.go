package state

import (
	"context"
	"fmt"
	"time"
)

// RecoveryManager handles failure recovery for bootstrap operations
type RecoveryManager struct {
	stateManager *StateManager
	clusterName  string
}

// NewRecoveryManager creates a new RecoveryManager
func NewRecoveryManager(clusterName string) *RecoveryManager {
	return &RecoveryManager{
		stateManager: NewStateManager(clusterName),
		clusterName:  clusterName,
	}
}

// CheckRecoverable determines if a failed bootstrap can be recovered
func (r *RecoveryManager) CheckRecoverable(ctx context.Context) (*BootstrapState, bool, error) {
	state, err := r.stateManager.Load()
	if err != nil {
		return nil, false, fmt.Errorf("failed to load state: %w", err)
	}
	
	if state == nil {
		return nil, false, nil // No previous state
	}
	
	// Check if bootstrap is already complete
	if state.CurrentPhase == PhaseComplete {
		return state, false, fmt.Errorf("cluster '%s' already exists and is complete", r.clusterName)
	}
	
	// Check if state is stale (older than 24 hours)
	if time.Since(state.Timestamp) > 24*time.Hour {
		return state, false, fmt.Errorf("state is stale (older than 24 hours), manual cleanup required")
	}
	
	// State exists and is recent, recovery is possible
	return state, true, nil
}

// GetRecoveryPhase determines which phase to resume from
func (r *RecoveryManager) GetRecoveryPhase(state *BootstrapState) BootstrapPhase {
	// Resume from the current phase (retry the failed phase)
	return state.CurrentPhase
}

// UpdatePhase updates the current phase and marks previous phase as completed
func (r *RecoveryManager) UpdatePhase(state *BootstrapState, newPhase BootstrapPhase) error {
	// Mark current phase as completed if moving to a new phase
	if state.CurrentPhase != "" && state.CurrentPhase != newPhase {
		state.CompletedPhases = append(state.CompletedPhases, state.CurrentPhase)
	}
	
	state.CurrentPhase = newPhase
	state.Timestamp = time.Now()
	
	return r.stateManager.Save(state)
}

// IsPhaseCompleted checks if a phase has been completed
func (r *RecoveryManager) IsPhaseCompleted(state *BootstrapState, phase BootstrapPhase) bool {
	for _, completed := range state.CompletedPhases {
		if completed == phase {
			return true
		}
	}
	return false
}

// MarkComplete marks the bootstrap as complete
func (r *RecoveryManager) MarkComplete(state *BootstrapState) error {
	state.CurrentPhase = PhaseComplete
	state.CompletedPhases = append(state.CompletedPhases, PhaseComplete)
	state.Timestamp = time.Now()
	
	return r.stateManager.Save(state)
}

// Cleanup removes the state file after successful completion or manual cleanup
func (r *RecoveryManager) Cleanup() error {
	return r.stateManager.Delete()
}

// GetStateManager returns the underlying state manager
func (r *RecoveryManager) GetStateManager() *StateManager {
	return r.stateManager
}
