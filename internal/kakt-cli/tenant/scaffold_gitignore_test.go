package tenant

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A scaffolded repository must ignore the credentials Day-0 writes into it.
//
// It did not. There was no .gitignore at all, so the cluster-admin kubeconfig,
// the GitHub PAT, the Hetzner token and the Day-0 Infisical client secret sat
// untracked in a git repository -- one `git add -A` from being committed, and
// git history is permanent (ADR-076). ADR-076 itself records the directory as
// "gitignored", which was true only of the platform's own checkout; every
// tenant repository was scaffolded without it, and the platform's own operation
// hid that for the same reason the ADR describes.
//
// Asserted through `git check-ignore` against a real render rather than by
// reading the file, because what matters is the rule's EFFECT on the paths Day-0
// actually writes. A pattern that looks right and does not match -- k8-secrets
// vs k8-secrets/, a leading slash -- reads as protection and is none.
func TestScaffoldedRepoIgnoresTheCredentialsDayZeroWrites(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dst := render(t, testSpec())

	if _, err := os.Stat(filepath.Join(dst, ".gitignore")); err != nil {
		t.Fatalf("a scaffolded repository has no .gitignore: %v", err)
	}

	if out, err := exec.Command("git", "-C", dst, "init", "-q", "-b", "main").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}

	// Written as files, because check-ignore matches paths and a directory rule
	// must still cover what is inside it.
	secrets := []string{
		"k8-secrets/kubeconfig/acme-hub.kubeconfig",
		"k8-secrets/github/github-pat-token",
		"k8-secrets/hetzner/token",
		"k8-secrets/tailscale/authkey",
		".state/infisical-bootstrap.json",
		".state/logs/bootstrap.log",
	}
	for _, rel := range secrets {
		p := filepath.Join(dst, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(p, []byte("secret"), 0o600); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}

	for _, rel := range secrets {
		if err := exec.Command("git", "-C", dst, "check-ignore", "-q", rel).Run(); err != nil {
			t.Errorf("%s is NOT ignored; a scaffolded repo would commit it", rel)
		}
	}

	// The other half of the contract. .state/<cluster>.json records
	// which phases completed and is committed on purpose -- the orchestrator
	// adds it explicitly, and an ignore rule covering it would both drop the
	// record a resumed bootstrap reads and make that `git add` fail.
	resume := filepath.Join(dst, ".state", "bootstrap", "acme-hub.json")
	if err := os.MkdirAll(filepath.Dir(resume), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(resume, []byte(`{"completedPhases":[]}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := exec.Command("git", "-C", dst, "check-ignore", "-q", ".state/acme-hub.json").Run(); err == nil {
		t.Error(".state/<cluster>.json is ignored; a resumed bootstrap would start the box over")
	}

	// And nothing that must ship was caught by the same rules.
	for _, rel := range []string{"clusters/acme-hub/bundle.yaml", ".github/workflows/bootstrap-cluster.yml"} {
		if _, err := os.Stat(filepath.Join(dst, filepath.FromSlash(rel))); err != nil {
			continue // not every template carries every path
		}
		if err := exec.Command("git", "-C", dst, "check-ignore", "-q", rel).Run(); err == nil {
			t.Errorf("%s is ignored; the tenant's repository would not carry it", rel)
		}
	}
}

// The rendered tree must not already contain a credential. The ignore rules
// above stop Day-0's output being committed; they do nothing about a secret
// baked into the template itself, which would be committed by the scaffold's
// own `git add .` before Day-0 ever runs.
func TestScaffoldedRepoShipsNoCredentialDirectory(t *testing.T) {
	dst := render(t, testSpec())
	if _, err := os.Stat(filepath.Join(dst, "k8-secrets")); err == nil {
		t.Error("the template ships a k8-secrets/ directory; scaffold commits the tree before Day-0 runs")
	}
	b, err := os.ReadFile(filepath.Join(dst, ".gitignore"))
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	// Guards the comment that explains why .state/ is excluded from
	// the rules. Someone adding `.state/` wholesale would pass every assertion
	// above except the resume one, and this says why before they get there.
	if !strings.Contains(string(b), ".state/") {
		t.Error(".gitignore does not mention .state/; the exclusion that keeps resume working is undocumented")
	}
}

// A BOX'S REPOSITORY SHIPS NO SCRIPTS.
//
// It shipped one: scripts/seed-secrets.sh, the first-time entrypoint for
// supplying an application's secrets. It is gone, and the CLI is the only way --
// `kakt app secrets template`, `import`, `set`, `status`.
//
// A script in the tenant's repository is a second answer to "which keys does
// this application declare", pinned to the platform version that rendered it. It
// drifts from the declaration the moment either changes, and it drifts silently:
// the script still runs.
func TestScaffoldedRepoShipsNoScripts(t *testing.T) {
	dir := render(t, testSpec())

	var found []string
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(dir, p)
		if relErr != nil {
			return relErr
		}
		if info.IsDir() {
			if rel == "scripts" {
				found = append(found, rel+"/")
			}
			return nil
		}
		// Any executable, and anything that looks like one by name. The
		// executable bit is the part a tenant would act on; the suffix is the
		// part a reviewer would notice.
		if strings.HasSuffix(rel, ".sh") || info.Mode().Perm()&0o111 != 0 {
			found = append(found, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) > 0 {
		t.Errorf("a box's repository ships no scripts, and this one carries %v.\n"+
			"Supplying secrets is `kakt app secrets`; a script here is a second answer "+
			"to what an application declares.", found)
	}
}
