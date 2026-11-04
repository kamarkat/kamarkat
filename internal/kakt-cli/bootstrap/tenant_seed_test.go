package bootstrap

import (
	"github.com/kamarkat/kamarkat/internal/kakt-cli/tenant"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A tenant's cluster is seeded with the declaration its own repository holds
// (ADR-072). The failure this guards is quiet: a template copied into clusters/
// without hydration produces an Application whose source names a literal
// <BUNDLE_VERSION>, which ArgoCD accepts and then cannot resolve -- reported as a
// sync failure on the cluster rather than as a bad bootstrap.
func TestTenantSeedRefusesAnUnhydratedDeclaration(t *testing.T) {
	dir := t.TempDir()
	cluster := filepath.Join(dir, tenant.RegistryDir, "clusters", "acme-hub")
	if err := os.MkdirAll(cluster, 0o755); err != nil {
		t.Fatal(err)
	}
	template := `apiVersion: argoproj.io/v1alpha1
kind: Application
spec:
  sources:
    - repoURL: ghcr.io/kamarkat/charts
      chart: environment-manager
      targetRevision: <BUNDLE_VERSION>
`
	if err := os.WriteFile(filepath.Join(cluster, "bundle.yaml"), []byte(template), 0o644); err != nil {
		t.Fatal(err)
	}

	o := &Orchestrator{GitopsDir: dir, ClusterName: "acme-hub"}
	err := o.applyTenantSeed(t.Context(), "/nonexistent-kubeconfig")
	if err == nil {
		t.Fatal("an unhydrated declaration must be refused before it reaches the cluster")
	}
	if !strings.Contains(err.Error(), "<BUNDLE_VERSION>") {
		t.Errorf("the error must name the placeholder it found, got: %v", err)
	}
}

// A cluster the repository does not declare is not one the platform may invent.
func TestTenantSeedRefusesAnUndeclaredCluster(t *testing.T) {
	dir := t.TempDir()
	o := &Orchestrator{GitopsDir: dir, ClusterName: "never-scaffolded"}

	err := o.applyTenantSeed(t.Context(), "/nonexistent-kubeconfig")
	if err == nil {
		t.Fatal("a cluster with no declaration must be refused")
	}
	if !strings.Contains(err.Error(), "never-scaffolded") {
		t.Errorf("the error must name the cluster it looked for, got: %v", err)
	}
}

// Without a cluster name there is no declaration to choose, and picking one
// would mean the platform deciding which cluster a tenant meant.
func TestTenantSeedRefusesWithoutAClusterName(t *testing.T) {
	o := &Orchestrator{GitopsDir: t.TempDir()}

	if err := o.applyTenantSeed(t.Context(), "/nonexistent-kubeconfig"); err == nil {
		t.Fatal("seeding without a cluster name must be refused")
	}
}
