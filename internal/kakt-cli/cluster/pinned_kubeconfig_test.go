package cluster

import (
	"context"
	"os"
	"testing"
)

// A health check receives a kubeconfig path and nothing else, so the path must
// identify one cluster. When it does not, the check resolves whatever context
// the operator's shell last selected -- which reported "the server doesn't have
// a resource type clusters" for a cluster that was provisioned and healthy,
// while every other call here passed --context and saw it.
func TestPinnedKubeconfigNeedsNoContextWhenNoneIsSet(t *testing.T) {
	p := &Provisioner{Kubeconfig: "/tmp/does-not-matter.yaml"}

	path, cleanup, err := p.pinnedKubeconfig(context.Background())
	defer cleanup()
	if err != nil {
		t.Fatalf("no context to pin should not be an error: %v", err)
	}
	if path != "/tmp/does-not-matter.yaml" {
		t.Errorf("with no context the path is returned unchanged, got %q", path)
	}
}

func TestPinnedKubeconfigFailsLoudlyOnAnUnknownContext(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "kubeconfig-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("apiVersion: v1\nkind: Config\nclusters: []\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	p := &Provisioner{Kubeconfig: f.Name(), Context: "no-such-context"}
	_, cleanup, err := p.pinnedKubeconfig(context.Background())
	defer cleanup()

	// Silently falling back to the unpinned file would restore exactly the
	// behaviour this exists to remove, and the check would look at whichever
	// cluster the shell happened to point at.
	if err == nil {
		t.Error("a context that does not exist must fail rather than fall back")
	}
}
