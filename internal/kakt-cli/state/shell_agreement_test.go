package state

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// repoRoot walks up until it finds the checkout, so the test can read the shell
// half of Day-0 regardless of where `go test` was invoked from.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s", dir)
		}
		dir = parent
	}
}

// The CLI writes this box's state and hub-bootstrap.sh reads it, so the two must
// name the same file. Nothing made them.
//
// They disagreed: the shell was moved to .state/<cluster>.json and the Go
// constant stayed at .state/bootstrap/, so the CLI wrote a file the shell never
// looked at. It surfaced twice, differently. read_kubeconfig_from_state
// error_exits on the missing file, which ended a run AFTER the bootstrap had
// completed successfully and blamed it on the cluster "not having been
// bootstrapped from this workspace". is_static_preflight_checkpointed only
// `return 1`s, so it silently reported "not checkpointed" forever and re-ran the
// full static preflight on every run, and nothing ever said so.
//
// Fixing the constant fixed that instance. This is what stops the next one: the
// shell reconstructs the path by hand in three places, and a test is the only
// thing standing between an edit to either side and a repeat.
func TestShellAndCLIAgreeOnTheStatePath(t *testing.T) {
	script := filepath.Join(repoRoot(t), "scripts", "hub-bootstrap.sh")
	raw, err := os.ReadFile(script)
	if err != nil {
		t.Fatalf("reading %s: %v", script, err)
	}

	// Every place the shell builds the CLI's per-cluster state file.
	re := regexp.MustCompile(`\$ZERO_OPS_DIR/([^"'\s]*)/\$\{CLUSTER_NAME\}\.json`)
	found := re.FindAllStringSubmatch(string(raw), -1)
	if len(found) == 0 {
		t.Fatal("hub-bootstrap.sh names no $ZERO_OPS_DIR/<dir>/${CLUSTER_NAME}.json; " +
			"either the shell stopped reading the CLI's state or this pattern went " +
			"stale, and in both cases the agreement is no longer being checked")
	}

	for _, m := range found {
		if got := m[1]; got != TenantStateDir {
			t.Errorf("hub-bootstrap.sh reads $ZERO_OPS_DIR/%s/${CLUSTER_NAME}.json "+
				"but the CLI writes %s/<cluster>.json; the shell would find no state "+
				"for a box the CLI had just bootstrapped", got, TenantStateDir)
		}
	}

	// The per-cluster file the CLI writes must be the one the shell reads, and
	// statePathFor is what decides that.
	want := filepath.Join("/repo", TenantStateDir, "acme-hub.json")
	if got := NewTenantStateManager("/repo", "acme-hub").statePath; got != want {
		t.Errorf("state path is %q, want %q", got, want)
	}

	// The sibling files the shell owns live in the same directory, so a change to
	// TenantStateDir that left them behind would split the box's state in two.
	for _, sibling := range []string{"bootstrap-mgmt.json", "bootstrap-workload.json"} {
		needle := "$ZERO_OPS_DIR/" + TenantStateDir + "/" + sibling
		if !strings.Contains(string(raw), needle) {
			t.Errorf("hub-bootstrap.sh does not write %s; the CLI's state and the "+
				"shell's would be in different directories", needle)
		}
	}
}
