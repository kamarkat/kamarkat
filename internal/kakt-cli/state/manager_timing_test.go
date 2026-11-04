package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// newManagerAt returns a StateManager writing inside dir. StateManager builds its
// path relative to the process working directory, so the test chdirs instead of
// reaching into the unexported field.
func newManagerAt(t *testing.T, dir, cluster string) *StateManager {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	return NewStateManager(cluster)
}

// Save previously left Timestamp at its zero value, so every state file written by
// a normal run reported 0001-01-01T00:00:00Z and end-to-end duration could not be
// recovered from it at all.
func TestSaveStampsTimestampAndStartedAt(t *testing.T) {
	mgr := newManagerAt(t, t.TempDir(), "hub-test")

	st := &BootstrapState{Version: "1.0", ClusterName: "hub-test"}
	before := time.Now().UTC().Add(-time.Second)

	if err := mgr.Save(st); err != nil {
		t.Fatalf("save: %v", err)
	}

	if st.Timestamp.IsZero() {
		t.Error("Timestamp is still zero after Save")
	}
	if st.StartedAt == nil {
		t.Fatal("StartedAt is still nil after Save")
	}
	if st.Timestamp.Before(before) {
		t.Errorf("Timestamp %v predates the call", st.Timestamp)
	}
}

// StartedAt is the origin for end-to-end duration, so a later Save must not move
// it — otherwise a resumed bootstrap reports only the time since its last phase.
func TestStartedAtSurvivesLaterSaves(t *testing.T) {
	mgr := newManagerAt(t, t.TempDir(), "hub-test")

	st := &BootstrapState{Version: "1.0", ClusterName: "hub-test"}
	if err := mgr.Save(st); err != nil {
		t.Fatalf("first save: %v", err)
	}
	first := *st.StartedAt

	time.Sleep(10 * time.Millisecond)
	if err := mgr.Save(st); err != nil {
		t.Fatalf("second save: %v", err)
	}

	if st.StartedAt == nil || !st.StartedAt.Equal(first) {
		t.Errorf("StartedAt moved: %v -> %v", first, st.StartedAt)
	}
	if !st.Timestamp.After(first) {
		t.Errorf("Timestamp did not advance on the second save: %v", st.Timestamp)
	}
}

// State files written before the timing fields existed must still load; a running
// bootstrap must never be stranded by a schema addition.
func TestLoadsPreTimingStateFile(t *testing.T) {
	dir := t.TempDir()
	legacy := `{
  "version": "1.0",
  "clusterName": "hub-hybrid-dev",
  "provider": "hybrid",
  "currentPhase": "day0-infra",
  "completedPhases": ["bootstrap-create", "day0-infra"],
  "bootstrapContext": "kind-hub-hybrid-dev",
  "timestamp": "0001-01-01T00:00:00Z",
  "metadata": null
}`
	// Written at the one supported location. This fixture used to sit in
	// .zero-ops/state, which made the test look like it covered the legacy
	// directory; what it actually asserts is that a state file predating the
	// timing fields still loads, and that is independent of where it lives.
	statePath := filepath.Join(dir, TenantStateDir)
	if err := os.MkdirAll(statePath, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(statePath, "hub-hybrid-dev.json"), []byte(legacy), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	mgr := newManagerAt(t, dir, "hub-hybrid-dev")
	st, err := mgr.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if st == nil {
		t.Fatal("load returned nil state")
	}
	if st.CurrentPhase != PhaseDayZero {
		t.Errorf("CurrentPhase = %q, want %q", st.CurrentPhase, PhaseDayZero)
	}
	if len(st.CompletedPhases) != 2 {
		t.Errorf("CompletedPhases = %v, want 2 entries", st.CompletedPhases)
	}
	if len(st.PhaseTimings) != 0 {
		t.Errorf("PhaseTimings = %v, want empty for a legacy file", st.PhaseTimings)
	}

	// Saving a legacy state adopts the current time as its origin rather than
	// leaving a zero value behind.
	if err := mgr.Save(st); err != nil {
		t.Fatalf("save: %v", err)
	}
	if st.StartedAt == nil {
		t.Error("StartedAt still nil after saving a legacy state file")
	}
}

// The timing fields are omitempty, so a state file from a run that recorded none
// stays free of misleading zero-valued dates.
func TestTimingFieldsOmittedWhenUnset(t *testing.T) {
	data, err := json.Marshal(&BootstrapState{Version: "1.0"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"startedAt", "completedAt", "phaseTimings"} {
		if _, present := raw[key]; present {
			t.Errorf("%q serialised despite being unset", key)
		}
	}
}
