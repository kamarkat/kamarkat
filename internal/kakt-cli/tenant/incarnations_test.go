package tenant

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// A REMOVED DECLARATION IS STILL AN INCARNATION THIS BOX HAD.
//
// The failure this closes: someone deletes a management cluster's directory
// believing the cluster is gone, its servers are still running, and the working
// tree no longer names it -- so nothing asks the provider about it and a second
// management cluster is admitted. The repository's history cannot be edited by
// deleting a file, which is what makes it the record.
func TestIncarnationsOutliveTheirDeclaration(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t",
			"GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	declare := func(name string) {
		t.Helper()
		base := filepath.Join(dir, RegistryDir, "clusters", name)
		if err := os.MkdirAll(base, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(base, managementMarker),
			[]byte("kind: Application\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(base, "values.yaml"),
			[]byte("boxId: acme-hub\nclusterName: "+name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	git("init", "-q", "-b", "main")
	declare("acme-hub")
	git("add", ".")
	git("commit", "-qm", "first incarnation")

	declare("acme-mgmt-02")
	git("add", ".")
	git("commit", "-qm", "second incarnation")

	// The predecessor's directory is removed, as someone tidying up would.
	if err := os.RemoveAll(filepath.Join(dir, RegistryDir, "clusters", "acme-hub")); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-qm", "remove the old one")

	// The tree has forgotten it.
	box, err := DiscoverBox(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(box.Management) != 1 || box.Management[0] != "acme-mgmt-02" {
		t.Fatalf("the working tree declares %v, want only acme-mgmt-02", box.Management)
	}
	if box.ID != "acme-hub" {
		t.Errorf("box id = %q, want acme-hub: the box keeps its identity", box.ID)
	}

	// The box has not.
	ever, err := IncarnationsEverDeclared(context.Background(), dir)
	if err != nil {
		t.Fatalf("IncarnationsEverDeclared: %v", err)
	}
	found := map[string]bool{}
	for _, n := range ever {
		found[n] = true
	}
	for _, want := range []string{"acme-hub", "acme-mgmt-02"} {
		if !found[want] {
			t.Errorf("%s is absent from %v; its servers would be invisible to the "+
				"duplicate-cluster check", want, ever)
		}
	}
}

// A directory that is not a repository is an error, not an empty answer.
func TestIncarnationsRefuseToGuessWithoutHistory(t *testing.T) {
	if _, err := IncarnationsEverDeclared(context.Background(), t.TempDir()); err == nil {
		t.Fatal("a directory with no history reported no incarnations, which reads as " +
			"a box that never had one")
	}
}
