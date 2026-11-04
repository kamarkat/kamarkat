package bootstrap

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// A bootstrap that cannot push its bookkeeping has still done the work. Failing
// the run because of it would turn a recoverable state into a lost one -- the
// cluster exists either way, and the next run repeats from the last phase that
// did persist.
func TestPersistTenantStateSurvivesAFailedPush(t *testing.T) {
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

	if err := o.persistTenantState(t.Context(), "capi-init"); err != nil {
		t.Errorf("a push that cannot succeed must not fail the bootstrap: %v", err)
	}

	// It committed what it could, so a re-run on this checkout resumes.
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
// `git commit` treats it as an error.
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
