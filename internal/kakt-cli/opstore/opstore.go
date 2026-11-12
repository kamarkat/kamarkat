// Package opstore holds the credentials an OPERATOR gathers, per machine.
//
// These are the values that exist before a box does: the cloud token a cluster
// is created with, the object-storage keys its backups go to, the git token its
// repository is written with, a tailnet auth key. Nothing inside the platform can
// hold them yet -- that is the chicken-and-egg Day-0 exists to break -- so they
// sit on the operator's machine until they are delivered into the box and into
// the tenant repository's own secrets.
//
// They used to sit in `k8-secrets/` inside a checkout, found by walking up from
// the working directory. That made them a property of a clone:
//
//   - deleting a checkout deleted them, including credentials that are issued
//     once and cannot be read back;
//   - a command run from outside a checkout could not find them, so operating a
//     box meant standing in the right directory or exporting values by hand;
//   - and they lived beside a repository, which is the one place credentials must
//     never drift into.
//
// They now live under the operator's home, once per machine, reachable from
// anywhere. The environment still wins, because automation sets it and has no
// home directory worth reading.
//
// NOT a secret store. This is a staging area with a narrow job and a short life:
// once a box exists, the box's own Infisical holds what it runs on and the escrow
// holds what it is rebuilt from. Nothing here is the authority for anything.
package opstore

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	// DirName is the per-operator directory under the home directory.
	DirName = ".kamarkat"
	// legacyDir is where these files used to live, inside a checkout. Never
	// read -- looked for only so a command that found nothing can say where
	// they are, because a silent "not found" while they sit in the next
	// directory up is the least useful answer available.
	legacyDir = "k8-secrets"
)

// Root is this operator's store.
//
// KAMARKAT_HOME relocates it whole, for a machine where the home directory is
// not where state belongs -- a CI runner, or a shared workstation.
func Root() (string, error) {
	if root := strings.TrimSpace(os.Getenv("KAMARKAT_HOME")); root != "" {
		return root, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("no home directory to keep operator credentials in: %w", err)
	}
	return filepath.Join(home, DirName), nil
}

// Path is where one credential lives, by its relative name -- "hetzner/token".
func Path(rel string) (string, error) {
	root, err := Root()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, filepath.FromSlash(rel)), nil
}

// Value reads one credential, trimmed. "" when there is none.
//
// Trimmed because a file written by a shell redirect carries a newline, and a
// token with a newline on the end authenticates as nobody.
func Value(rel string) string {
	p, err := Path(rel)
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// ValueOrEnv prefers the environment, then the store.
//
// The order is deliberate: in CI the value arrives as a variable and there is no
// store, and on a workstation the store is what means the operator does not have
// to export anything.
func ValueOrEnv(envName, rel string) string {
	if v := strings.TrimSpace(os.Getenv(envName)); v != "" {
		return v
	}
	return Value(rel)
}

// Write records one credential for this operator.
//
// 0700 on the directories and 0600 on the file, because every one of these opens
// something that costs money or holds data.
func Write(rel, value string) (string, error) {
	p, err := Path(rel)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", filepath.Dir(p), err)
	}
	if err := os.WriteFile(p, []byte(strings.TrimSpace(value)+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("write %s: %w", p, err)
	}
	return p, nil
}

// LegacyLocation reports a checkout still holding the old files, so a command
// that found nothing can name them instead of only their absence.
//
// Not a fallback. Nothing reads these: a copy in two places is a copy that goes
// stale in one of them, and the stale one is the one that gets used at the worst
// moment.
func LegacyLocation(rel string) string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	dir := wd
	for i := 0; i < 8; i++ {
		candidate := filepath.Join(dir, legacyDir, filepath.FromSlash(rel))
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

// Missing is the one message every caller gives when a credential is absent.
func Missing(what, rel string) error {
	want, err := Path(rel)
	if err != nil {
		want = "~/" + DirName + "/" + rel
	}
	if old := LegacyLocation(rel); old != "" {
		return fmt.Errorf("no %s.\n\nIt is in %s, which is no longer read: a checkout is not "+
			"where credentials belong, and deleting one takes them with it. Move it:\n\n"+
			"  mkdir -p %s && mv %s %s",
			what, old, filepath.Dir(want), old, want)
	}
	return fmt.Errorf("no %s.\n\nPut it in %s, or set the matching environment variable for "+
		"this run.", what, want)
}

// RunDir holds TEMPORARY execution material, kept apart from credentials.
//
// A Day-0 kubeconfig is the case this exists for. Bootstrap needs one before the
// escrow copy exists -- the on-prem join runs before the pivot, and the pivot is
// what writes it -- so there is a window where the file is the only way to reach
// the cluster being built. It is a working file for that window, not a second
// recovery store: once a box is up, the escrowed copy is the authority, and
// `kakt kubeconfig` is how an operator gets one.
//
// Separated from the credential directories above so the distinction is visible
// in the filesystem rather than only in a comment: ~/.kamarkat/hetzner/token is
// something an operator gathered and keeps, ~/.kamarkat/run/ is something a
// command produced and may delete.
//
// Outside every checkout, for the same reason the credentials are: it is reachable
// from any directory, it cannot be committed, and deleting a clone cannot take it
// at a moment when it is the only way in.
func RunDir() (string, error) {
	root, err := Root()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "run"), nil
}

// KubeconfigPath is where a cluster's working kubeconfig lives during bootstrap.
//
// The directory is created 0700 on the way: this file is cluster-admin for as
// long as it exists, and a kubeconfig readable by other accounts on a shared
// machine is the credential leak nobody notices.
func KubeconfigPath(cluster string) (string, error) {
	cluster = strings.TrimSpace(cluster)
	if cluster == "" {
		return "", fmt.Errorf("no cluster name: a working kubeconfig belongs to one cluster")
	}
	run, err := RunDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(run, "kubeconfig")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	return filepath.Join(dir, cluster+".kubeconfig"), nil
}
