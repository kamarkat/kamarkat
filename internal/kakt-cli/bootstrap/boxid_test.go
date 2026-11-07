package bootstrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/hubaccess"
)

// The box id has to come from the declaration, because nothing else has it and
// the escrow refuses an empty one.
//
// cluster.Config.BoxID was declared, documented as "what the escrow keys
// box-scoped artefacts by", and read on both sides of the at-rest encryption key
// -- and never assigned. The escrow validates the identity, and Provision calls
// applyEncryptionConfig unconditionally before the ClusterClass, so a bootstrap
// failed there with "the escrow needs the box id" and nothing keyed by the box
// was ever escrowed.
func TestDeclaredBoxIDIsReadFromTheBoxsOwnDeclaration(t *testing.T) {
	dir := t.TempDir()
	clusterDir := filepath.Join(dir, "registry", "clusters", "acme-hub")
	if err := os.MkdirAll(clusterDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// A successor hub carries a new cluster name under the same box, so the two
	// are deliberately different here: a box id derived from the cluster name
	// would send that successor's artefacts to a path nothing reads.
	if err := os.WriteFile(filepath.Join(clusterDir, "values.yaml"),
		[]byte("boxId: acme-box\nclusterName: acme-hub\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := hubaccess.DeclaredBoxID(dir, "acme-hub")
	if err != nil {
		t.Fatalf("DeclaredBoxID: %v", err)
	}
	if got != "acme-box" {
		t.Errorf("box id is %q, want the declared acme-box", got)
	}

	// Escrow coordinates are filled in later than scaffolding, and a bootstrap
	// must not fail for want of coordinates it is not about to use.
	if _, err := hubaccess.Declaration(dir, "acme-hub"); err == nil {
		t.Error("Declaration accepted a box with no escrow coordinates")
	}
}

// An undeclared box id is refused, with the reason, rather than defaulted.
func TestDeclaredBoxIDRefusesWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	clusterDir := filepath.Join(dir, "registry", "clusters", "acme-hub")
	if err := os.MkdirAll(clusterDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clusterDir, "values.yaml"),
		[]byte("clusterName: acme-hub\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := hubaccess.DeclaredBoxID(dir, "acme-hub")
	if err == nil {
		t.Fatal("an undeclared box id was accepted")
	}
	if !strings.Contains(err.Error(), "boxId") {
		t.Errorf("the refusal does not name the field: %v", err)
	}
}
