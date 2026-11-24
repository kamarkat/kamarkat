package secrets

import "testing"

// The key name is a CONTRACT with the application charts, not an internal
// detail: oranger-bff and oranger-sdk read ORANGER_INTERNAL_TOKEN out of their
// ExternalSecret, and the platform seeds whatever this function returns. Drift
// between the two is invisible until a workload sits in
// CreateContainerConfigError naming a Kubernetes Secret that was never created,
// which is exactly how this credential was found missing on 2026-09-30.
func TestInternalTokenKeyMatchesWhatTheChartsRead(t *testing.T) {
	for appId, want := range map[string]string{
		"oranger":  "ORANGER_INTERNAL_TOKEN",
		"waypoint": "WAYPOINT_INTERNAL_TOKEN",
		// Hyphens become underscores, as everywhere else an appId becomes a key.
		"code-builder": "CODE_BUILDER_INTERNAL_TOKEN",
	} {
		if got := InternalTokenKey(appId); got != want {
			t.Errorf("InternalTokenKey(%q) = %q; the chart reads %q", appId, got, want)
		}
	}
}

// Two applications of one customer must not share this credential. They hold
// separate folders, so the same key name is correct -- but each must still name
// its own application, because a workload's env is read by people.
func TestEachApplicationGetsItsOwnKeyName(t *testing.T) {
	if a, b := InternalTokenKey("oranger"), InternalTokenKey("waypoint"); a == b {
		t.Fatalf("both applications resolve to %q", a)
	}
}
