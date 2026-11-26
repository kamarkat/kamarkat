package hubaccess

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCluster(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, "registry", "clusters", name, "values.yaml")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A command run in the box's repository without --cluster addresses its one
// management cluster; a managed cluster never qualifies, and two are refused.
func TestSoleManagementCluster(t *testing.T) {
	dir := t.TempDir()
	writeCluster(t, dir, "acme-mgmt-1", "clusterName: acme-mgmt-1\n")
	writeCluster(t, dir, "acme-01", "clusterName: acme-01\nenvironmentSlug: stg\n")
	if got, err := soleManagementCluster(dir); err != nil || got != "acme-mgmt-1" {
		t.Fatalf("got %q, %v; want acme-mgmt-1", got, err)
	}

	writeCluster(t, dir, "acme-mgmt-2", "clusterName: acme-mgmt-2\n")
	if _, err := soleManagementCluster(dir); err == nil || !strings.Contains(err.Error(), "pass --cluster") {
		t.Fatalf("two management clusters accepted: %v", err)
	}

	if _, err := soleManagementCluster(t.TempDir()); err == nil {
		t.Fatal("a directory with no clusters was accepted")
	}
}
