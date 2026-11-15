package embedded

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ReleasedScriptsEnv tells a materialized script that its siblings come from the
// release and nothing else.
//
// hub-bootstrap.sh's resolve_owned looks in the WORKSPACE before the platform,
// which is right in a checkout -- the two coincide -- and wrong for a released
// run, where the workspace is a tenant's repository. A file a tenant happens to
// have at the same relative path would take precedence over the versioned
// implementation. With this set, platform-owned content resolves from the
// extracted release only.
const ReleasedScriptsEnv = "KAKT_RELEASED_SCRIPTS"

// MaterializeScripts writes the embedded script tree to a private directory and
// returns its root -- the directory holding `scripts/`, which is what
// hub-bootstrap.sh derives PLATFORM_DIR from.
//
// 0700 on the directories and 0500 on the files: go:embed records no mode and
// embed.FS reports everything read-only, so the executable bit is restored here
// or nothing runs. Read-only files make the orchestration immutable for the
// invocation -- a bootstrap that rewrote its own scripts midway would be running
// something no release contains. Removal still works, because that needs write
// permission on the directory rather than on the file.
func MaterializeScripts() (root string, cleanup func(), err error) {
	root, err = os.MkdirTemp("", "kakt-release-")
	if err != nil {
		return "", nil, fmt.Errorf("no private directory to extract the platform scripts into: %w", err)
	}
	cleanup = func() { _ = os.RemoveAll(root) }

	written := 0
	err = fs.WalkDir(FS, "scripts", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		target := filepath.Join(root, filepath.FromSlash(p))
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		b, readErr := FS.ReadFile(p)
		if readErr != nil {
			return fmt.Errorf("read embedded %s: %w", p, readErr)
		}
		// Everything is executable, not only *.sh: a validator module may be
		// python, and run.sh invokes what it finds.
		if writeErr := os.WriteFile(target, b, 0o500); writeErr != nil {
			return fmt.Errorf("write %s: %w", target, writeErr)
		}
		written++
		return nil
	})
	if err != nil {
		cleanup()
		return "", nil, err
	}
	if written == 0 {
		cleanup()
		return "", nil, fmt.Errorf(
			"this build carries no platform scripts, so Day-0 has nothing to run.\n\n" +
				"The embedded tree is generated: run scripts/package/embed-platform-assets.sh, " +
				"or use a released binary. scripts/validate/runtime-script-closure.py verifies the set.")
	}
	return root, cleanup, nil
}

// RequireMaterialized refuses a release missing any of the paths a caller is
// about to run, before it runs the first of them.
//
// Named paths rather than a count: a partially generated package is the failure
// this exists to catch, and "some scripts are present" is not the question.
func RequireMaterialized(root string, rel ...string) error {
	var missing []string
	for _, r := range rel {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(r))); err != nil {
			missing = append(missing, r)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf(
			"this release does not carry %s, which Day-0 runs.\n\n"+
				"An incomplete package is never permission to skip a step: nothing was "+
				"started, and a release missing orchestration cannot bootstrap a box.",
			strings.Join(missing, ", "))
	}
	return nil
}
