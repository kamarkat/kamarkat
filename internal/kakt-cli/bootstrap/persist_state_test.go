package bootstrap

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A push that cannot succeed is REPORTED, because a commit that never left the
// runner is not a checkpoint: that disk ends with the job.
//
// The phase itself still succeeds -- runPhase records this and carries on, and
// refuses only at the phase that creates infrastructure (requireDurableState).
// Reporting it here is what makes that decision possible; returning nil, as this
// did, is what let a runner with no git identity fail every checkpoint of every
// run while the bootstrap claimed it could be resumed.
func TestPersistTenantStateReportsAFailedPush(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "t@example.invalid"},
		{"config", "user.name", "t"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	// A repository with no remote: staging and committing succeed, the push
	// cannot, and the bootstrap must continue regardless.
	o := &Orchestrator{GitopsDir: dir, ClusterName: "acme-hub"}
	if err := writeState(t, dir, "acme-hub"); err != nil {
		t.Fatal(err)
	}

	err := o.persistTenantState(t.Context(), "capi-init")
	if err == nil {
		t.Fatal("a state that never reached the repository was reported as checkpointed")
	}
	if !strings.Contains(err.Error(), "push") && !strings.Contains(err.Error(), "rebase") {
		t.Errorf("the failure does not say which step could not reach the repository: %v", err)
	}

	// It still committed what it could, so a re-run on THIS checkout resumes;
	// that is why the phase is not failed over it.
	cmd := exec.Command("git", "log", "--oneline")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("reading the log: %v\n%s", err, out)
	}
	if len(out) == 0 {
		t.Error("the state was not committed, so a run resumed elsewhere would " +
			"start from nothing")
	}
}

// Nothing to commit is the normal case for a phase that changed no state, and
// must not be reported as a failed checkpoint -- neither `git commit` on an empty
// index nor `git add` on a state directory that does not exist yet, both of which
// exit non-zero.
func TestPersistTenantStateIsQuietWhenNothingChanged(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q"},
		{"config", "user.email", "t@example.invalid"}, {"config", "user.name", "t"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	o := &Orchestrator{GitopsDir: dir, ClusterName: "acme-hub"}
	if err := o.persistTenantState(t.Context(), "preflight"); err != nil {
		t.Errorf("a phase that wrote no state must not be an error: %v", err)
	}
}

// Without a tenant repository there is nothing to push to, and the local file is
// the state.
func TestPersistTenantStateDoesNothingWithoutATenantRepository(t *testing.T) {
	o := &Orchestrator{}
	if err := o.persistTenantState(t.Context(), "preflight"); err != nil {
		t.Errorf("the platform's own path must be unaffected: %v", err)
	}
}

func writeState(t *testing.T, dir, cluster string) error {
	t.Helper()
	p := filepath.Join(dir, ".state", "bootstrap")
	if err := exec.Command("mkdir", "-p", p).Run(); err != nil {
		return err
	}
	return exec.Command("sh", "-c",
		"printf '{}' > "+filepath.Join(p, cluster+".json")).Run()
}
