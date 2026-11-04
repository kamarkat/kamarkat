package state

import (
	"path/filepath"
	"strings"
	"testing"
)

// A tenant's bootstrap state lives outside anything the cluster reconciles.
//
// It was under clusters/<name>/generated/, which an Application of its own
// applies as Kubernetes manifests -- so the state file was sitting in a
// directory whose contract is "objects to apply". ArgoCD ignores a file it
// cannot parse as one, which is why this was harmless and would not have stayed
// harmless.
func TestTenantStateIsOutsideWhatTheClusterReconciles(t *testing.T) {
	m := NewTenantStateManager("/repo", "acme-hub")

	if strings.Contains(m.statePath, "/generated/") {
		t.Errorf("bootstrap state must not sit in a reconciled directory, got %q",
			m.statePath)
	}
	// .state/, the one directory the box's state lives in. It asserted
	// .state/, which was the only thing still saying so: the shell half
	// of Day-0 writes .state/bootstrap-mgmt.json and reads the CLI's record from
	// .state/<cluster>.json, so the extra segment put the writer and the reader in
	// different directories -- and this test held it there.
	if !strings.HasPrefix(m.statePath, filepath.Join("/repo", ".state")+"/") {
		t.Errorf("state belongs under .state/, got %q", m.statePath)
	}
	if !strings.HasSuffix(m.statePath, "acme-hub.json") {
		t.Errorf("state is per cluster, got %q", m.statePath)
	}
}
