package bootstrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The spoke's workers must be provisioned before its database is waited on.
//
// This is an ordering invariant, not a preference. Step 10f waits for
// shared-cnpg on the spoke; step 10e runs provision-flatcar-worker.sh, which
// brings up the ONLY general capacity a hybrid spoke has (ADR-075: hybrid buys
// no cloud workers, and its burst pool is tainted workload-class=burst). A
// database cannot become ready on a cluster whose single node is a tainted
// control plane.
//
// It shipped inverted. step10_wait_spokepool -- which contained 10f -- is
// called at main():2221, and step10e_spoke_home_worker at 2234, so the wait sat
// ahead of the step that satisfies it. Every fresh hybrid spoke failed the same
// way: 900 seconds, then "the spoke's shared-cnpg has no ready instance", a
// message naming CNPG for a fault that was an empty node pool an hour upstream.
// nutgraf-01 on 2026-09-18 took roughly an hour to produce it.
//
// Asserted on the script text because the failure is invisible to every other
// check: both steps exist, both are called, both succeed in isolation, and only
// their order is wrong.
func TestSpokeWorkersAreProvisionedBeforeTheDatabaseWait(t *testing.T) {
	root := repoRoot(t)

	for _, script := range []string{
		filepath.Join(root, "scripts", "hub-bootstrap.sh"),
		filepath.Join(root, "scripts", "dev", "local-e2e.sh"),
	} {
		raw, err := os.ReadFile(script)
		if err != nil {
			t.Fatalf("read %s: %v", script, err)
		}
		body := string(raw)
		rel, _ := filepath.Rel(root, script)

		// Call sites, not the definitions.
		workers := lastCallIndex(body, "step10e_spoke_home_worker")
		database := lastCallIndex(body, "step10f_spoke_database")

		if workers < 0 || database < 0 {
			t.Errorf("%s: expected both step10e_spoke_home_worker and "+
				"step10f_spoke_database to be called (workers=%d database=%d)",
				rel, workers, database)
			continue
		}
		if database < workers {
			t.Errorf("%s: the spoke's database is waited on BEFORE its workers are "+
				"provisioned (10f at byte %d, 10e at %d).\n"+
				"A hybrid spoke's only general capacity comes from 10e, so 10f would "+
				"wait out its full deadline on a cluster that cannot schedule, then "+
				"report a missing database rather than a missing node.",
				rel, database, workers)
		}
	}
}

// The database wait must not live inside step10_wait_spokepool, which runs
// before the worker step. Extracting it is what makes the ordering above
// expressible at all; folding it back in would silently restore the inversion
// while leaving both call sites looking correct.
func TestDatabaseWaitIsNotInsideTheSpokePoolWait(t *testing.T) {
	root := repoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "scripts", "hub-bootstrap.sh"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)

	start := strings.Index(body, "step10_wait_spokepool() {")
	if start < 0 {
		t.Fatal("step10_wait_spokepool is gone; this gate needs rewriting")
	}
	// The function ends at the first line-start "}" after it.
	end := strings.Index(body[start:], "\n}\n")
	if end < 0 {
		t.Fatal("could not find the end of step10_wait_spokepool")
	}
	fn := body[start : start+end]

	if strings.Contains(fn, `log "Step 10f:`) {
		t.Error("the Step 10f database wait is back inside step10_wait_spokepool, " +
			"which main() calls BEFORE step10e_spoke_home_worker. That is the " +
			"inversion that made every fresh hybrid spoke fail after 900s with a " +
			"message about CNPG instead of about capacity.")
	}
}

// lastCallIndex finds where a shell function is CALLED, skipping its definition.
func lastCallIndex(body, name string) int {
	best := -1
	for i := 0; ; {
		j := strings.Index(body[i:], name)
		if j < 0 {
			return best
		}
		at := i + j
		i = at + len(name)
		// Skip "name() {" -- the definition.
		if strings.HasPrefix(body[at:], name+"()") {
			continue
		}
		// Skip comments.
		lineStart := strings.LastIndex(body[:at], "\n") + 1
		if strings.HasPrefix(strings.TrimSpace(body[lineStart:at]), "#") {
			continue
		}
		best = at
	}
}
