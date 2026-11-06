package tenant

import (
	"os"
	"path/filepath"
	"testing"
)

// add-cluster places a workload cluster's Applications where something applies
// them, and leaves its generated directory existing.
//
// Both halves were wrong. generated.yaml was hydrated into
// registry/clusters/<cluster>/, which no Application covers -- the management
// cluster's root applies its OWN directory with recurse: false -- so the
// Application was never created on any box and every ADR-045 artifact written
// into generated/ was in git and applied to nothing. And the directory it points
// at did not exist in git until something wrote a file there, which for a managed
// cluster is not Day-0 but the first app with a public host: an Application over
// a path the repository does not carry is a ComparisonError, not the empty-and-
// healthy state its own comment describes.
func TestAddClusterPlacesItsApplicationsOnTheManagementCluster(t *testing.T) {
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

	// Every Application in the management cluster's directory, named for this
	// cluster, and nothing left in the cluster's own.
	for _, app := range workloadClusterApps {
		placed := filepath.Join(dir, app.appFile(mgmt, cluster))
		if _, err := os.Stat(placed); err != nil {
			t.Errorf("%s was not placed on the management cluster: %v", app.suffix, err)
		}
		if _, err := os.Stat(filepath.Join(dir, RegistryDir, "clusters", cluster, app.template)); !os.IsNotExist(err) {
			t.Errorf("%s was left in the cluster's own directory, where nothing applies it", app.template)
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
