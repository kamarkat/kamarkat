package bootstrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ADR-045 artifacts are per-cluster instance data, so a tenant's belong in the
// tenant's repository (ADR-062, ADR-072). Writing them into zero-ops would put
// one tenant's Infisical coordinates in the types repository -- the defect
// removed from the published charts, reappearing in the bootstrap that produces
// them.
func TestADR045ArtifactsBelongToTheTenantRepository(t *testing.T) {
	fromRepoRoot(t)

	platform := &Orchestrator{}
	reg, err := platform.readADR045Registry()
	if err != nil {
		t.Fatalf("reading the registry: %v", err)
	}
	for _, a := range reg.Artifacts {
		if !strings.HasPrefix(a.File, "manifests/") {
			t.Errorf("without a tenant repository an artifact belongs under "+
				"manifests/, got %q", a.File)
		}
	}

	tenant := &Orchestrator{GitopsDir: "/somewhere", ClusterName: "acme-hub"}
	treg, err := tenant.readADR045Registry()
	if err != nil {
		t.Fatalf("reading the registry for a tenant: %v", err)
	}
	if len(treg.Artifacts) == 0 {
		t.Skip("the registry declares no artifacts, so there is nothing to relocate")
	}
	for _, a := range treg.Artifacts {
		// Under clusters/<cluster>/generated/, not directly in it.
		//
		// This asserted the directory exactly, which made the flat layout the
		// contract: relocation had to flatten generated/values/x.yaml to
		// generated/x.yaml to satisfy it, one directory above where the writers
		// put the file and where bundle.yaml's valueFiles reads it. The test
		// passed and the bootstrap reported the artifact missing.
		//
		// What ADR-062 and ADR-072 actually require is the repository and the
		// cluster, which is what is checked here. How the artifacts are arranged
		// beneath that is the registry's business.
		want := filepath.Join("registry", "clusters", "acme-hub", "generated") + "/"
		if !strings.HasPrefix(a.File, want) {
			t.Errorf("a tenant's artifact belongs under %s, got %q", want, a.File)
		}
		if strings.Contains(a.File, "manifests/") {
			t.Errorf("a tenant's repository has no manifests/ tree, got %q", a.File)
		}
	}
}

// The cluster name is what places the artifact. Without it the location cannot
// be resolved, and guessing would put one cluster's coordinates where another
// reads them.
func TestADR045TenantArtifactsNeedAClusterName(t *testing.T) {
	fromRepoRoot(t)
	o := &Orchestrator{GitopsDir: "/somewhere"}

	if _, err := o.readADR045Registry(); err == nil {
		t.Error("a tenant repository with no cluster name must be refused")
	}
}

// EnsureTopologyRecorded records clusters/<name>/generated/hub-cluster.yaml
// into the tenant repository so worker capacity is editable via GitOps (ADR-045, ADR-075).
func TestEnsureTopologyRecordedWritesHubCluster(t *testing.T) {
	dir := t.TempDir()
	p := NewCloudProvider(&HetznerDriver{Region: "fsn1", OS: "ubuntu", NetworkCIDR: "10.0.0.0/16"}, "nutgraf-mgmt-1795584132", false)
	if err := p.EnsureTopologyRecorded(dir); err != nil {
		t.Fatalf("EnsureTopologyRecorded failed: %v", err)
	}
	target := filepath.Join(dir, "registry", "clusters", "nutgraf-mgmt-1795584132", "generated", "hub-cluster.yaml")
	b, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("expected %s to be written, got: %v", target, err)
	}
	content := string(b)
	if !strings.Contains(content, "name: nutgraf-mgmt-1795584132") {
		t.Errorf("content missing cluster name:\n%s", content)
	}
	if !strings.Contains(content, "namespace: platform-capi") {
		t.Errorf("content missing namespace platform-capi:\n%s", content)
	}
	if !strings.Contains(content, "value: fsn1") {
		t.Errorf("content missing region fsn1:\n%s", content)
	}
}

// In a tenant repository, adr045GeneratedDirs must stage registry/clusters/<name>/generated/
// so hub-cluster.yaml reaches git, and adr045Consumers must wait for <name>-generated
// so the topology object is reconciled by ArgoCD.
func TestTenantADR045StagesGeneratedDirectoryAndReconciles(t *testing.T) {
	fromRepoRoot(t)
	o := &Orchestrator{GitopsDir: "/tmp/gitops", ClusterName: "acme-hub"}
	dirs, err := o.adr045GeneratedDirs()
	if err != nil {
		t.Fatalf("adr045GeneratedDirs failed: %v", err)
	}
	wantDir := filepath.Join("registry", "clusters", "acme-hub", "generated") + "/"
	foundDir := false
	for _, d := range dirs {
		if d == wantDir {
			foundDir = true
			break
		}
	}
	if !foundDir {
		t.Errorf("adr045GeneratedDirs() = %v; want %s to be staged so hub-cluster.yaml is committed", dirs, wantDir)
	}

	apps, err := o.adr045Consumers()
	if err != nil {
		t.Fatalf("adr045Consumers failed: %v", err)
	}
	wantApp := "acme-hub-generated"
	foundApp := false
	for _, a := range apps {
		if a == wantApp {
			foundApp = true
			break
		}
	}
	if !foundApp {
		t.Errorf("adr045Consumers() = %v; want %s to be waited on", apps, wantApp)
	}
}
