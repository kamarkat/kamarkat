package escrow

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// A box's escrow coordinates belong to the OPERATOR, not to a checkout.
//
// They used to be written to `k8-secrets/infisical/` inside the platform
// repository, and every reader found them by walking up from the working
// directory until a checkout appeared. That made three things true, and all
// three were wrong:
//
//   - The values lived or died with a clone. Deleting a checkout deleted the
//     only copy of a client secret Infisical returns exactly once, and nothing
//     warned that it was about to happen.
//   - A command run from outside any checkout could not find them, so operating
//     a box meant standing in the right directory -- or exporting four variables
//     by hand and remembering to.
//   - They sat beside a repository, which is the one place credentials must
//     never drift into.
//
// They now live under the operator's home, once per machine. Every command
// reads the same place from anywhere, and `kakt box init` picks them up
// without four flags or four exports.
//
// The environment still wins, because automation sets it.
const (
	// StoreDirName is the per-operator directory under the home directory.
	StoreDirName = ".kamarkat"
	// StoreEscrowSubdir holds the INFISICAL_ESCROW_* values.
	StoreEscrowSubdir = "infisical"
	// legacyDir is where these files used to live, inside a checkout. It is
	// never read -- it is looked for only to tell an operator where its
	// contents have to move, because a silent "not found" when the values are
	// sitting right there is the least useful thing this could do.
	legacyDir = "k8-secrets/infisical"
)

// StorePath is the directory holding this operator's escrow settings.
//
// KAMARKAT_HOME relocates it whole, for a machine where the home directory is
// not where state belongs -- a CI runner, or a shared workstation.
func StorePath() (string, error) {
	if root := strings.TrimSpace(os.Getenv("KAMARKAT_HOME")); root != "" {
		return filepath.Join(root, StoreEscrowSubdir), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("no home directory to keep escrow settings in: %w", err)
	}
	return filepath.Join(home, StoreDirName, StoreEscrowSubdir), nil
}

// StoredValue reads one setting: the environment first, then the store.
//
// Returns "" when there is none. Callers decide whether that is fatal, because
// it is for some settings and expected for others.
func StoredValue(name string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	dir, err := StorePath()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// WriteStoredValues records settings for this operator, replacing what is there.
//
// 0700 on the directory and 0600 on each file: one of these is a client secret,
// and the others identify where a box's recovery material lives.
func WriteStoredValues(values map[string]string) (string, error) {
	dir, err := StorePath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	for name, value := range values {
		if strings.TrimSpace(value) == "" {
			continue
		}
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(value), 0o600); err != nil {
			return "", fmt.Errorf("write %s: %w", p, err)
		}
	}
	return dir, nil
}

// LegacyLocation reports a checkout still holding the old files, so a command
// that found nothing can say where they are instead of only that they are absent.
//
// Not a fallback. Nothing reads these; the message asks for them to be moved,
// because a copy in two places is a copy that goes stale in one of them.
func LegacyLocation(names ...string) string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	dir := wd
	for i := 0; i < 8; i++ {
		candidate := filepath.Join(dir, filepath.FromSlash(legacyDir))
		for _, n := range names {
			if _, err := os.Stat(filepath.Join(candidate, n)); err == nil {
				return candidate
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

// MoveHint is the one message every caller gives when settings are missing.
func MoveHint(names ...string) string {
	store, err := StorePath()
	if err != nil {
		store = "~/" + StoreDirName + "/" + StoreEscrowSubdir
	}
	if old := LegacyLocation(names...); old != "" {
		return fmt.Sprintf("\n\nThese settings are in %s, which is no longer read:\n"+
			"a checkout is not where they belong, and deleting one took the only copy\n"+
			"of a client secret Infisical issues once. Move them:\n\n"+
			"  mkdir -p %s && mv %s/INFISICAL_ESCROW_* %s/",
			old, store, old, store)
	}
	return fmt.Sprintf("\n\nThey are kept in %s, written by `kakt escrow init`.", store)
}
