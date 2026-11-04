package bootstrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/state"
)

// writeKubeconfig produces a kubeconfig naming exactly one context.
func writeKubeconfig(t *testing.T, dir, name, contextName, server string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	body := `apiVersion: v1
kind: Config
clusters:
- cluster: {server: ` + server + `}
  name: c
contexts:
- context: {cluster: c, user: u}
  name: ` + contextName + `
current-context: ` + contextName + `
users:
- name: u
  user: {token: x}
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A kubeconfig recorded by bootstrap-create that no longer resolves its context
// must stop the run, not answer with whatever cluster the file now names.
//
// This is the 2026-09-18 failure in one assertion. hub-bootstrap.sh exports
// KUBECONFIG to k8-secrets/kubeconfig/<cluster>.kubeconfig when that file
// exists, so kind merged kind-<cluster> into the HUB's kubeconfig and that path
// was recorded. pivot-move's SaveKubeconfig then os.WriteFile()d the same path
// with the hub's kubeconfig -- a whole-file overwrite -- and the context was
// gone. The resolver returned the unpinned path anyway, so every later
// bootstrap-cluster question was answered by the hub: "namespaces platform-capi
// not found", with a healthy kind cluster running and nothing in the output
// naming the substitution.
func TestKubeconfigPaths_RecordedButUnresolvableIsFatal(t *testing.T) {
	dir := t.TempDir()
	// The hub's kubeconfig after SaveKubeconfig overwrote it: no kind context.
	hub := writeKubeconfig(t, dir, "hub.kubeconfig", "hub-admin@hub", "https://10.0.0.1:6443")

	o := &Orchestrator{ClusterName: "acme-hub"}
	bs := &state.BootstrapState{
		BootstrapKubeconfig: hub,
		BootstrapContext:    "kind-acme-hub",
	}

	path, _, err := o.kubeconfigPaths(bs)
	if err == nil {
		t.Fatalf("a recorded kubeconfig missing its context resolved instead of failing; got path %q", path)
	}
	// The hub's path must never be handed back as the bootstrap cluster.
	if path != "" {
		t.Fatalf("returned a usable path alongside the error: %q", path)
	}
	for _, want := range []string{"kind-acme-hub", hub} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %q, so the reader cannot find the file:\n%v", want, err)
		}
	}
}

// Before bootstrap-create there is nothing recorded and no cluster to resolve.
// That is not an error -- phases needing the cluster fail on their own terms.
func TestKubeconfigPaths_NothingRecordedIsNotFatal(t *testing.T) {
	o := &Orchestrator{ClusterName: "acme-hub"}
	if _, _, err := o.kubeconfigPaths(&state.BootstrapState{}); err != nil {
		t.Fatalf("an un-created bootstrap cluster must not fail here: %v", err)
	}
}

// A kubeconfig that DOES hold the context resolves, and resolves to a pinned
// copy -- not to the recorded file itself, which later phases overwrite.
func TestKubeconfigPaths_ResolvesToPinnedCopyNotTheSourceFile(t *testing.T) {
	dir := t.TempDir()
	src := writeKubeconfig(t, dir, "ambient.kubeconfig", "kind-acme-hub", "https://127.0.0.1:61234")

	o := &Orchestrator{ClusterName: "acme-hub"}
	bs := &state.BootstrapState{BootstrapKubeconfig: src, BootstrapContext: "kind-acme-hub"}

	path, kctx, err := o.kubeconfigPaths(bs)
	if err != nil {
		t.Fatalf("a kubeconfig holding its context must resolve: %v", err)
	}
	if kctx != "kind-acme-hub" {
		t.Errorf("context = %q, want kind-acme-hub", kctx)
	}
	if path == src {
		t.Fatal("resolved to the recorded file itself; the pinned copy is what survives " +
			"SaveKubeconfig overwriting that path")
	}
	if !strings.Contains(path, "hub-bootstrap-kind-acme-hub") {
		t.Errorf("not the pinned copy: %q", path)
	}
}

// installArgoCDAndSeed must refuse an empty kubeconfig rather than pass it on.
//
// kubectl reads --kubeconfig "" as unset and falls back to $KUBECONFIG, so
// ArgoCD installed against whatever that named; client-go's loader, used by the
// GitHub-credential step two lines later, rejects it. The phase therefore
// reported "ArgoCD installed" and "boundary activated" and then died on
// "invalid configuration: no configuration has been provided", which names
// neither the missing lookup nor the cluster it half-installed to.
func TestInstallArgoCDAndSeed_RefusesEmptyKubeconfig(t *testing.T) {
	o := &Orchestrator{ClusterName: "acme-hub"}
	for _, kc := range []string{"", "   "} {
		err := o.installArgoCDAndSeed(t.Context(), kc)
		if err == nil {
			t.Fatalf("empty kubeconfig %q was accepted; ArgoCD would install against $KUBECONFIG", kc)
		}
		if !strings.Contains(err.Error(), "acme-hub-kubeconfig") {
			t.Errorf("error does not name the secret that could not be read:\n%v", err)
		}
	}
}
