package hubaccess

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDeclarationResolvesBoxScopedValuesFromBoxYaml(t *testing.T) {
	dir := t.TempDir()

	// Write registry/box.yaml with box-level coordinates
	boxYaml := `boxId: nutgraf-1794929668
tenantId: nutgraf
escrow:
  url: https://app.infisical.com
  projectId: proj-12345
  organizationId: org-12345
platformAdminIdentity: admin-id-abc
`
	if err := os.MkdirAll(filepath.Join(dir, "registry"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "registry", "box.yaml"), []byte(boxYaml), 0o644); err != nil {
		t.Fatal(err)
	}

	// Workload cluster values.yaml only declares cluster-specific values
	clusterDir := filepath.Join(dir, "registry", "clusters", "nutgraf-stg-01")
	if err := os.MkdirAll(clusterDir, 0o755); err != nil {
		t.Fatal(err)
	}
	clusterYaml := `clusterName: nutgraf-stg-01
environmentSlug: stg
provider: hetzner
region: hel1
`
	if err := os.WriteFile(filepath.Join(clusterDir, "values.yaml"), []byte(clusterYaml), 0o644); err != nil {
		t.Fatal(err)
	}

	coords, err := Declaration(dir, "nutgraf-stg-01")
	if err != nil {
		t.Fatalf("Declaration failed: %v", err)
	}

	if coords.BoxID != "nutgraf-1794929668" {
		t.Errorf("got BoxID %q, want nutgraf-1794929668", coords.BoxID)
	}
	if coords.ClusterName != "nutgraf-stg-01" {
		t.Errorf("got ClusterName %q, want nutgraf-stg-01", coords.ClusterName)
	}
	if coords.EscrowURL != "https://app.infisical.com" {
		t.Errorf("got EscrowURL %q, want https://app.infisical.com", coords.EscrowURL)
	}
	if coords.EscrowProjectID != "proj-12345" {
		t.Errorf("got EscrowProjectID %q, want proj-12345", coords.EscrowProjectID)
	}
	if coords.EscrowOrgID != "org-12345" {
		t.Errorf("got EscrowOrgID %q, want org-12345", coords.EscrowOrgID)
	}
	if coords.PlatformAdmin != "admin-id-abc" {
		t.Errorf("got PlatformAdmin %q, want admin-id-abc", coords.PlatformAdmin)
	}

	boxID, err := DeclaredBoxID(dir, "nutgraf-stg-01")
	if err != nil {
		t.Fatalf("DeclaredBoxID failed: %v", err)
	}
	if boxID != "nutgraf-1794929668" {
		t.Errorf("got DeclaredBoxID %q, want nutgraf-1794929668", boxID)
	}
}
