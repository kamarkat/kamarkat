package embedded

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The properties a released bootstrap depends on, asserted outside any checkout:
// the tree arrives whole, its scripts are executable, and they cannot be
// rewritten while the bootstrap that is running them runs.
func TestMaterializeScriptsIsRunnableAndImmutable(t *testing.T) {
	root, cleanup, err := MaterializeScripts()
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	defer cleanup()

	// PLATFORM_DIR is dirname(SCRIPT_DIR) in hub-bootstrap.sh, so the root has to
	// be the directory HOLDING scripts/ rather than scripts/ itself.
	entry := filepath.Join(root, "scripts", "hub-bootstrap.sh")
	fi, err := os.Stat(entry)
	if err != nil {
		t.Fatalf("the entry point was not extracted: %v", err)
	}
	if fi.Mode()&0o100 == 0 {
		t.Errorf("%s is mode %v: go:embed records no mode, so the executable bit "+
			"has to be restored or nothing runs", entry, fi.Mode())
	}
	if fi.Mode()&0o200 != 0 {
		t.Errorf("%s is writable (%v); the orchestration must not be rewritable by "+
			"the bootstrap that is running it", entry, fi.Mode())
	}

	// A nested validator, because run.sh discovers modules by glob and a
	// flattened extraction would leave it finding none.
	if _, err := os.Stat(filepath.Join(root, "scripts", "validate", "run.sh")); err != nil {
		t.Errorf("relative paths were not preserved: %v", err)
	}

	cleanup()
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Errorf("cleanup left %s behind; read-only files must not block removal", root)
	}
}

// An incomplete package fails, and names what is absent.
func TestRequireMaterializedNamesWhatIsMissing(t *testing.T) {
	root, cleanup, err := MaterializeScripts()
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	defer cleanup()

	if err := RequireMaterialized(root,
		"scripts/hub-bootstrap.sh", "scripts/validate-all.sh"); err != nil {
		t.Errorf("a complete release was refused: %v", err)
	}
	err = RequireMaterialized(root, "scripts/not-in-any-release.sh")
	if err == nil {
		t.Fatal("a release missing a script Day-0 runs was accepted")
	}
	if !strings.Contains(err.Error(), "scripts/not-in-any-release.sh") {
		t.Errorf("the refusal does not name the missing script: %s", err)
	}
}
