package tenant

// Promotion: moving a box from the version it runs to a version that is
// published.
//
// It is a rewrite of this box's own references and a commit, so it needs the
// box and git and nothing else. It used to live in the platform's release
// script, which meant an operator standing in their own box could not promote it
// without a checkout of the platform -- and a tenant, who has no such checkout
// at all (ADR-065), could not promote without one either. The work was never the
// platform's to hold.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// Promotion is a move from the version a box runs to one that is published.
type Promotion struct {
	// GitopsDir is the box's checkout. Everything is read and written inside it.
	GitopsDir string
	// Version is the published version to move to.
	Version string
	// Registry is where that version is published, as a chart source --
	// "ghcr.io/<owner>/charts". Checked against what each cluster pulls from.
	Registry string
	// DryRun reports what would change and writes nothing.
	DryRun bool
}

// stateDir holds this box's local bootstrap record and belongs to no tenant's
// repository, so a promotion that staged it would commit one operator's machine
// state into the box.
const stateDir = ".state"

// Promote rewrites every reference to the version this box runs, commits it and
// pushes. It returns the version it moved from.
//
// Ordered so that nothing is written until every refusal has had its chance: a
// promotion that half-applies leaves the root on one version and its children on
// another, and ArgoCD reports that as Synced.
func (p Promotion) Promote() (string, error) {
	if strings.TrimSpace(p.Version) == "" {
		return "", fmt.Errorf("name the published version to promote to")
	}
	if strings.TrimSpace(p.Registry) == "" {
		return "", fmt.Errorf("promotion needs the registry %s is published to, so it can refuse a box that pulls from another", p.Version)
	}
	if _, err := ReadBoxDecl(p.GitopsDir); err != nil {
		return "", err
	}

	running, err := p.runningVersion()
	if err != nil {
		return "", err
	}
	if running == p.Version {
		return "", fmt.Errorf("this box already runs %s", p.Version)
	}

	// REFUSE TO REUSE A VERSION. A republished tag with different content is the
	// one failure GitOps cannot diagnose: every reference still resolves, and the
	// box runs something other than what the reference names.
	if refs, err := p.grepRegistry(p.Version); err != nil {
		return "", err
	} else if len(refs) > 0 {
		return "", fmt.Errorf("%s is already referenced in %s -- a version is published once:\n%s",
			p.Version, filepath.Join(p.GitopsDir, RegistryDir), strings.Join(refs, "\n"))
	}

	// Every cluster must pull the bundle from the registry this version is
	// published to. Promotion rewrites the version and nothing else, so a
	// bundle.yaml still naming another registry is promoted to a version that
	// registry does not hold: ArgoCD cannot pull it and every Application stays
	// on the old version. That shipped twice after the platform's registry moved.
	if err := p.everyClusterPullsFromRegistry(); err != nil {
		return "", err
	}

	files, err := p.filesReferencing(running)
	if err != nil {
		return "", err
	}
	if len(files) == 0 {
		return "", fmt.Errorf("no file under %s references %s",
			filepath.Join(p.GitopsDir, RegistryDir), running)
	}

	if p.DryRun {
		fmt.Printf("  would rewrite %d file(s) %s -> %s, commit, rebase and push:\n", len(files), running, p.Version)
		for _, f := range files {
			fmt.Printf("    %s\n", f)
		}
		return running, nil
	}

	for _, f := range files {
		path := filepath.Join(p.GitopsDir, f)
		body, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		// Every occurrence, not only environmentRevision: a bundle carries the
		// version twice, and moving one leaves the children pinned while the root
		// reports Synced.
		if err := os.WriteFile(path, []byte(strings.ReplaceAll(string(body), running, p.Version)), 0o644); err != nil {
			return "", err
		}
	}

	if left, err := p.grepRegistry(running); err != nil {
		return "", err
	} else if len(left) > 0 {
		return "", fmt.Errorf("after rewriting, %s still appears -- promotion is incomplete and has NOT been committed:\n%s",
			running, strings.Join(left, "\n"))
	}

	if err := p.commit(files, running); err != nil {
		return "", err
	}
	return running, p.push()
}

// runningVersion is the version every cluster in this box declares.
//
// Sorted unique, and a disagreement is a refusal rather than a guess: clusters
// on different versions is a real state -- a promotion that half-applied -- and
// picking either one silently completes it in a direction nobody chose.
func (p Promotion) runningVersion() (string, error) {
	pattern := filepath.Join(p.GitopsDir, RegistryDir, "clusters", "*", "values.yaml")
	decls, err := filepath.Glob(pattern)
	if err != nil {
		return "", err
	}
	line := regexp.MustCompile(`(?m)^environmentRevision:[ \t]*(\S+)`)
	seen := map[string][]string{}
	for _, d := range decls {
		body, err := os.ReadFile(d)
		if err != nil {
			return "", err
		}
		if m := line.FindStringSubmatch(string(body)); m != nil {
			seen[m[1]] = append(seen[m[1]], d)
		}
	}
	switch len(seen) {
	case 0:
		return "", fmt.Errorf("no environmentRevision found under %s", pattern)
	case 1:
		for v := range seen {
			return v, nil
		}
	}
	var detail []string
	for v, where := range seen {
		detail = append(detail, fmt.Sprintf("  %s: %s", v, strings.Join(where, ", ")))
	}
	return "", fmt.Errorf("clusters are on different versions, so there is no single current version:\n%s\n   Bring them to one version first.",
		strings.Join(detail, "\n"))
}

// everyClusterPullsFromRegistry checks each cluster's bundle source against the
// registry the version is published to.
func (p Promotion) everyClusterPullsFromRegistry() error {
	bundles, err := filepath.Glob(filepath.Join(p.GitopsDir, RegistryDir, "clusters", "*", "bundle.yaml"))
	if err != nil {
		return err
	}
	// The chart source is a LIST ITEM, so repoURL is preceded by "- ". Anchoring
	// on the key at line start matched the tenant-repository source further down
	// and never the bundle's own, which read as "no environment-manager chart
	// source" for every correctly configured box.
	repoURL := regexp.MustCompile(`(?m)^[\s-]*repoURL:\s*(\S+)`)
	var wrong []string
	for _, b := range bundles {
		body, err := os.ReadFile(b)
		if err != nil {
			return err
		}
		// The repoURL that precedes the environment-manager chart, which is the
		// bundle's own source.
		// Only the repoURL that a `chart: environment-manager` line follows. A box
		// pinned to "development" names the platform's repository at a path and
		// carries no chart key at all, so without this the scan fell through to
		// the LAST repoURL in the file -- the tenant's own repository, included
		// so the values file resolves -- and the refusal blamed the one source
		// that was correct.
		got, found := "", false
		for _, line := range strings.Split(string(body), "\n") {
			if m := repoURL.FindStringSubmatch(line); m != nil {
				got = m[1]
			}
			if strings.Contains(line, "chart: environment-manager") {
				found = true
				break
			}
		}
		if !found {
			got = ""
		}
		got = strings.Trim(got, `"'`)
		if got != p.Registry {
			if got == "" {
				got = "no environment-manager chart source"
			}
			wrong = append(wrong, fmt.Sprintf("  %s: %s", b, got))
		}
	}
	if len(wrong) > 0 {
		return fmt.Errorf("these clusters do not pull the bundle from %s, where %s is published:\n%s\n   Set their environment-manager repoURL to %s, commit and push, then run this again.",
			p.Registry, p.Version, strings.Join(wrong, "\n"), p.Registry)
	}
	return nil
}

// filesReferencing lists the files under registry/ that name a version, as paths
// relative to the box.
func (p Promotion) filesReferencing(version string) ([]string, error) {
	var found []string
	root := filepath.Join(p.GitopsDir, RegistryDir)
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".yaml") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if containsVersion(string(body), version) {
			rel, err := filepath.Rel(p.GitopsDir, path)
			if err != nil {
				return err
			}
			found = append(found, rel)
		}
		return nil
	})
	return found, err
}

// grepRegistry reports where a version appears, as file:line, for a message that
// says what to go and look at.
func (p Promotion) grepRegistry(version string) ([]string, error) {
	files, err := p.filesReferencing(version)
	if err != nil {
		return nil, err
	}
	var hits []string
	for _, f := range files {
		body, err := os.ReadFile(filepath.Join(p.GitopsDir, f))
		if err != nil {
			return nil, err
		}
		for i, line := range strings.Split(string(body), "\n") {
			if containsVersion(line, version) {
				hits = append(hits, fmt.Sprintf("  %s:%d: %s", f, i+1, strings.TrimSpace(line)))
			}
		}
	}
	return hits, nil
}

// containsVersion reports whether content contains version as an exact version token
// rather than as a substring of a longer prerelease or version string (e.g. "0.3.38"
// inside "0.3.38-rc.1").
func containsVersion(content, version string) bool {
	if version == "" {
		return false
	}
	start := 0
	for {
		idx := strings.Index(content[start:], version)
		if idx == -1 {
			return false
		}
		pos := start + idx
		end := pos + len(version)
		start = end

		// Check predecessor character
		if pos > 0 {
			prev := content[pos-1]
			if isVersionChar(prev) {
				continue
			}
		}

		// Check successor character
		if end < len(content) {
			next := content[end]
			if isVersionChar(next) {
				continue
			}
		}

		return true
	}
}

func isVersionChar(b byte) bool {
	return (b >= '0' && b <= '9') ||
		(b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z') ||
		b == '.' || b == '-'
}

// commit stages the rewritten files BY NAME and commits them.
//
// By name because `git add -A` here commits .state/, which is this box's local
// bootstrap state and belongs to no tenant's repository. The staged set is
// checked afterwards rather than trusted, because the cost of being wrong is a
// commit that has already been made.
func (p Promotion) commit(files []string, running string) error {
	if err := p.git(append([]string{"add", "--"}, files...)...); err != nil {
		return err
	}
	staged, err := p.gitOut("diff", "--cached", "--name-only")
	if err != nil {
		return err
	}
	for _, f := range strings.Fields(staged) {
		if strings.HasPrefix(f, stateDir+"/") || f == stateDir {
			return fmt.Errorf("refusing to commit: %s is staged, and it is this machine's bootstrap state rather than this box's declaration", f)
		}
	}
	message := fmt.Sprintf(`chore(promote): %s -> %s

Every reference under %s, not only environmentRevision: a bundle carries the
version twice, and moving one leaves the children pinned while the root
reports Synced.`, running, p.Version, RegistryDir)
	return p.git("commit", "-q", "-m", message)
}

// push rebases onto the remote and pushes.
//
// Rebased first, and NEVER forced: a rejection here is the other party's commit
// -- a CI bot writing a chart bump is ordinary in this window -- and discarding
// it is the failure, not the rejection.
func (p Promotion) push() error {
	if err := p.git("-c", "rebase.autoStash=true", "pull", "--rebase", "-q"); err != nil {
		return fmt.Errorf(`the remote moved and the rebase did not apply cleanly.
   The promotion is committed in %s but NOT pushed. Resolve it there and push.`, p.GitopsDir)
	}
	if err := p.git("push", "-q"); err != nil {
		return fmt.Errorf("push rejected. The promotion is committed in %s but not pushed -- resolve it there and push", p.GitopsDir)
	}
	return nil
}

func (p Promotion) git(args ...string) error {
	c := exec.Command("git", append([]string{"-C", p.GitopsDir}, args...)...)
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	return c.Run()
}

func (p Promotion) gitOut(args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", p.GitopsDir}, args...)...).Output()
	return string(out), err
}
