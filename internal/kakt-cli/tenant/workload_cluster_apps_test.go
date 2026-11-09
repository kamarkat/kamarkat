package tenant

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// add-cluster writes ONE declaration: the cluster's own directory, with a claim
// in it and a generated directory git carries.
//
// Nothing is written into the management cluster's directory any more, and that
// is the lifecycle: the two Applications are generated from the claim, so the
// file that declares the cluster is the only thing that has to exist -- and the
// only thing that has to go.
//
// The generated directory still has to exist from the start. An Application over
// an empty directory syncs nothing and reports healthy, which is the state ADR-045
// artifacts arrive into; one over a path the repository does not carry is a
// ComparisonError, and for a managed cluster nothing writes there until the first
// app declares a public host.
func TestAddClusterWritesOnlyTheDeclaration(t *testing.T) {
	const mgmt, cluster = "acme-hub", "acme-stg-01"
	dir := t.TempDir()

	// The real template, so this breaks if the template stops carrying one.
	root := repoRootForTest(t)
	if err := copyDir(
		filepath.Join(root, "manifests", "tenants", "gitops-template", "templates", "workload-cluster"),
		filepath.Join(dir, "templates", "workload-cluster"),
	); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, RegistryDir, "clusters", mgmt), 0o755); err != nil {
		t.Fatal(err)
	}

	w := WorkloadCluster{
		GitopsDir: dir, MgmtCluster: mgmt, Name: cluster,
		Provider: "hetzner", Region: "hel1", Environment: "stg", Workers: 2,
		BundleVersion: "0.1.16", TenantID: "acme", GitopsRepoURL: "https://github.com/acme-inc/acme-gitops",
		PublicTLSIssuer: "letsencrypt-prod", ChartSource: "registry", PlatformRepoURL: "https://github.com/kamarkat/kamarkat",
		BundleRegistry: "ghcr.io/kamarkat/kamarkat",
	}
	written, err := w.Add()
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	// The claim, single-document and under the name the generators match.
	claim := filepath.Join(dir, RegistryDir, "clusters", cluster, "infrastructure", "spokepool.yaml")
	raw, err := os.ReadFile(claim)
	if err != nil {
		t.Fatalf("the claim that declares this cluster was not written: %v", err)
	}
	if n := strings.Count(string(raw), "\n---"); n != 0 {
		t.Errorf("the claim holds %d extra document(s); a git files generator parses the "+
			"matched file as one, so the cluster would never be generated", n)
	}
	if !strings.Contains(string(raw), "kind: SpokePool") {
		t.Error("the claim does not declare a SpokePool")
	}

	// Nothing in the management cluster's directory. A file placed there is
	// applied by a root that does not prune, which is what made a second,
	// imperative destroy step necessary.
	entries, err := os.ReadDir(filepath.Join(dir, RegistryDir, "clusters", mgmt))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), cluster) {
			t.Errorf("add-cluster placed %s in the management cluster's directory", e.Name())
		}
	}

	// The paths Add reports are the paths a tenant commits.
	for _, p := range written {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			t.Errorf("Add reported %s, which does not exist: %v", p, err)
		}
	}

	// The generated Application's source path exists in the repository.
	keep := filepath.Join(dir, RegistryDir, "clusters", cluster, "generated", ".gitkeep")
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("the generated directory is not carried by the repository: %v", err)
	}
}
