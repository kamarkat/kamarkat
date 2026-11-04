package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// BootstrapPhase represents the current phase of the bootstrap process
type BootstrapPhase string

const (
	PhasePreFlight             BootstrapPhase = "preflight"
	PhaseBootstrapCreate       BootstrapPhase = "bootstrap-create"
	PhaseDayZero               BootstrapPhase = "day0-infra"
	PhaseCAPIInit              BootstrapPhase = "capi-init"
	PhaseClusterProvision      BootstrapPhase = "cluster-provision"
	PhaseArgoCDInstall         BootstrapPhase = "argocd-install"
	PhasePivotMove             BootstrapPhase = "pivot-move"
	PhasePivotReady            BootstrapPhase = "pivot-ready"
	PhaseCleanup               BootstrapPhase = "cleanup"
	PhaseClusterClassDeploy    BootstrapPhase = "clusterclass-deploy"
	PhasePlatformPreReqs       BootstrapPhase = "platform-pre-reqs"
	PhaseOnPremJoin            BootstrapPhase = "on-prem-join"
	PhaseBoundary01            BootstrapPhase = "boundary-01"
	PhaseGenerateLocalSecrets  BootstrapPhase = "generate-local-secrets"
	PhaseBoundary02            BootstrapPhase = "boundary-02"
	PhaseInjectCACert          BootstrapPhase = "inject-ca-cert"
	PhaseBoundary03            BootstrapPhase = "boundary-03"
	PhaseBootstrapInfisicalAPI BootstrapPhase = "bootstrap-infisical-api"
	PhaseBoundary04            BootstrapPhase = "boundary-04"
	PhaseBoundary05            BootstrapPhase = "boundary-05"
	PhaseBoundary06            BootstrapPhase = "boundary-06"
	PhaseInitSecrets           BootstrapPhase = "init-secrets"    // DEPRECATED: replaced by GenerateLocalSecrets + InjectCACert + BootstrapInfisicalAPI
	PhasePlatformDeploy        BootstrapPhase = "platform-deploy" // DEPRECATED: replaced by B01/B02/B03
	PhaseFinalize              BootstrapPhase = "finalize"
	PhaseComplete              BootstrapPhase = "complete"

	// ADR-042 Platform Bootstrap States
	// These are the platform-level state machine states that parallel
	// the CAPI provisioning phases. The CLI gates transitions between
	// these states to ensure deterministic bootstrap.
	PlatformStateNew            BootstrapPhase = "platform-new"
	PlatformStateInfisicalReady BootstrapPhase = "platform-infisical-ready"
	PlatformStatePKIReady       BootstrapPhase = "platform-pki-ready"
	PlatformStateSecretsReady   BootstrapPhase = "platform-secrets-ready"
	PlatformStateGitOpsReady    BootstrapPhase = "platform-gitops-ready"
	PlatformStatePlatformReady  BootstrapPhase = "platform-ready"
)

// BootstrapState tracks the state of the bootstrap process
type BootstrapState struct {
	Version          string           `json:"version"`
	ClusterName      string           `json:"clusterName"`
	Provider         string           `json:"provider"`
	Region           string           `json:"region"`
	BootstrapID      string           `json:"bootstrapId"`
	CurrentPhase     BootstrapPhase   `json:"currentPhase"`
	CompletedPhases  []BootstrapPhase `json:"completedPhases"`
	BootstrapContext string           `json:"bootstrapContext"`
	// BootstrapKubeconfig is the FILE the bootstrap context lives in, recorded
	// when the cluster is created.
	//
	// The context name alone is not enough to find it again. kind writes its
	// context to whatever $KUBECONFIG named at creation, and hub-bootstrap.sh
	// exports KUBECONFIG to the HUB's kubeconfig as soon as that file exists --
	// so on every resumed run the ambient value names a different file than the
	// one the context is in. Resolving by the ambient value then answers a
	// bootstrap-cluster question with the hub, which reports the CAPI namespace
	// missing because CAPI does not live there until pivot.
	BootstrapKubeconfig string            `json:"bootstrapKubeconfig,omitempty"`
	MgmtKubeconfig      string            `json:"mgmtKubeconfig"`
	NetworkCIDR         string            `json:"networkCIDR"`
	SSHKeys             []string          `json:"sshKeys,omitempty"`
	Timestamp           time.Time         `json:"timestamp"`
	Metadata            map[string]string `json:"metadata"`

	// Wall-clock accounting for the bootstrap, so end-to-end cluster creation time
	// is a recorded fact rather than something reconstructed from log lines.
	// StartedAt is stamped once when the state is first created and survives
	// resumes; CompletedAt is stamped when the final phase succeeds. On a resumed
	// run, CompletedAt-StartedAt therefore spans the interruption — PhaseTimings is
	// the honest source for how much work actually happened.
	//
	// Pointers, not values: `omitempty` has no effect on time.Time (a zero time is
	// a non-empty struct), so a value field would serialise as
	// "0001-01-01T00:00:00Z" — the same meaningless date that made the original
	// Timestamp field useless. A nil pointer is simply absent from the file.
	StartedAt    *time.Time    `json:"startedAt,omitempty"`
	CompletedAt  *time.Time    `json:"completedAt,omitempty"`
	PhaseTimings []PhaseTiming `json:"phaseTimings,omitempty"`
}

// PhaseTiming records how long one bootstrap phase took. Phases skipped on a
// resume are not re-timed, so an entry always describes work that really ran.
type PhaseTiming struct {
	Phase       BootstrapPhase `json:"phase"`
	StartedAt   time.Time      `json:"startedAt"`
	CompletedAt time.Time      `json:"completedAt"`
	Seconds     float64        `json:"seconds"`
}

// StateManager handles persistence of bootstrap state
type StateManager struct {
	statePath string
}

// NewStateManager creates a new StateManager for the given cluster.
//
// State goes under .state, the same root NewTenantStateManager uses. These were
// two different directories -- .zero-ops/state here, .state there --
// chosen by whether --gitops-dir was passed, so "where is this box's state" had
// two answers and every reader had to know which call had been made. A shell
// helper that looked in only one of them silently fell through to a convention
// path and never used the recorded value.
func NewStateManager(clusterName string) *StateManager {
	return &StateManager{
		statePath: statePathFor("", clusterName),
	}
}

// statePathFor resolves where a cluster's state lives.
//
// ONE location. It briefly read .zero-ops/state as well, for boxes written
// before the move -- which meant the answer to "where is this box's state"
// depended on what happened to exist on disk, and a stale file left in the old
// directory silently won over the current one. Nothing has written there since
// the move, so the only thing the second lookup could still find is a file that
// should not be acted on.
func statePathFor(root, clusterName string) string {
	return filepath.Join(root, TenantStateDir, clusterName+".json")
}

// NewTenantStateManager keeps a cluster's bootstrap state in the tenant's own
// repository.
//
// The default path is relative to the working directory, which on a CI runner is
// a checkout discarded when the job ends. A bootstrap that took two hours and
// failed at the last phase would restart from nothing, which is the behaviour
// modular execution exists to avoid (ADR-072).
//
// It sits beside the cluster's generated artifacts because it is the same kind
// of thing: instance data produced by Day-0, belonging to one cluster, and
// useful to a tenant asking how far its bootstrap got.
func NewTenantStateManager(gitopsDir, clusterName string) *StateManager {
	return &StateManager{
		statePath: statePathFor(gitopsDir, clusterName),
	}
}

// TenantStateDir is where a tenant's repository keeps the state of what the
// platform's commands have done to it.
//
// Outside clusters/<name>/generated/ deliberately. That directory is reconciled
// by an Application of its own, so its contract is "objects to apply"; a state
// file there would be handed to ArgoCD, and the next thing put beside it might
// not be as harmlessly ignored.
//
// .state/ and not .state/. The box's state is one directory: the shell
// half of Day-0 writes .state/bootstrap-mgmt.json and .state/bootstrap-workload.json
// there, and hub-bootstrap.sh reads the CLI's record from .state/<cluster>.json --
// in is_static_preflight_checkpointed, which silently answered "not checkpointed"
// for a file that was never at that path, and in read_kubeconfig_from_state, which
// error_exits on it. The extra path segment here was the only thing still
// disagreeing, so the CLI wrote .state/<cluster>.json and the shell
// looked for .state/<cluster>.json and found nothing.
//
// The sensitive contents of .state/ are covered by the tenant template's
// .gitignore -- infisical-bootstrap.json holds a client secret, logs/ echo values
// under --debug -- which is what makes staging this directory by name safe.
const TenantStateDir = ".state"

// Save persists the bootstrap state to disk using atomic write
func (m *StateManager) Save(state *BootstrapState) error {
	// Timestamp used to be written only by the recovery paths, so a state file
	// written by a normal run reported 0001-01-01T00:00:00Z and said nothing about
	// when the bootstrap actually ran. Stamp it on every save, and backfill
	// StartedAt on first write so a resumed run still has an origin.
	now := time.Now().UTC()
	state.Timestamp = now
	if state.StartedAt == nil {
		started := now
		state.StartedAt = &started
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal state: %w", err)
	}

	dir := filepath.Dir(m.statePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create state directory: %w", err)
	}

	// Atomic write: write to temp file, then rename
	tmpPath := m.statePath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0600); err != nil {
		return fmt.Errorf("failed to write temp state file: %w", err)
	}

	// Atomic rename (POSIX guarantee)
	if err := os.Rename(tmpPath, m.statePath); err != nil {
		return fmt.Errorf("failed to rename state file: %w", err)
	}

	return nil
}

// Load reads the bootstrap state from disk
func (m *StateManager) Load() (*BootstrapState, error) {
	data, err := os.ReadFile(m.statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // No state file
		}
		return nil, fmt.Errorf("failed to read state file: %w", err)
	}

	var state BootstrapState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("failed to unmarshal state: %w", err)
	}

	return &state, nil
}

// Delete removes the state file
func (m *StateManager) Delete() error {
	err := os.Remove(m.statePath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to delete state file: %w", err)
	}
	return nil
}

// Exists checks if a state file exists for the cluster
func (m *StateManager) Exists() bool {
	_, err := os.Stat(m.statePath)
	return err == nil
}

// GetStatePath returns the path to the state file
func (m *StateManager) GetStatePath() string {
	return m.statePath
}
