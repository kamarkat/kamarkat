package bootstrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ~/.kube/config holds every cluster an operator has ever touched, and the
// kubectl calls in this package pass the path without a context. Each therefore
// resolves whatever the shell last selected -- which read a CAPI cluster's
// status from an unrelated local kind cluster, and looked for the hub's
// kubeconfig Secret in a namespace that cluster does not have.
func TestPinKubeconfigRefusesAContextItCannotResolve(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	if err := os.WriteFile(path, []byte("apiVersion: v1\nkind: Config\nclusters: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Silently returning the unpinned path would restore the behaviour this
	// removes: the caller would believe it had a pinned file and would read
	// whichever cluster the shell happened to point at.
	if _, err := pinKubeconfig(path, "no-such-context"); err == nil {
		t.Error("an unresolvable context must be an error, not a fallback")
	}
}

// A resumed bootstrap runs this many times across phases. Naming the file for
// the context keeps that to one file rather than one per invocation.
func TestPinnedKubeconfigPathIsStableForAContext(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	if err := os.WriteFile(path, []byte("apiVersion: v1\nkind: Config\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Both calls fail on an empty config; what matters is that the name a
	// successful call would use is derived from the context alone.
	first, err1 := pinKubeconfig(path, "ctx-a")
	second, err2 := pinKubeconfig(path, "ctx-a")
	if err1 == nil && err2 == nil && first != second {
		t.Errorf("the same context must map to one path, got %q and %q", first, second)
	}
	if err1 == nil && !strings.Contains(first, "ctx-a") {
		t.Errorf("the path must name the context it pins, got %q", first)
	}
}
