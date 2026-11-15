#!/usr/bin/env python3
"""The scripts a released CLI must carry, computed rather than listed.

Day-0 runs two entry points -- hub-bootstrap.sh and validate-all.sh -- and they
reach further scripts, which reach further still. A hand-written embed manifest
goes stale the first time someone adds a validator, and the failure is silent in
the worst way: validate-all.sh SKIPS a suite whose script is absent

    if [[ ! -x "$adr_script" ]]; then
        skip adr "adr-acceptance.sh is not present at $adr_script"

so a release missing it reports success with less coverage than it claims.

So the closure is derived here from the entry points, and the embedded tree is
checked against it. Two directories are taken whole rather than by reference,
because scripts/validate/run.sh discovers modules by glob

    for f in "$MODULE_DIR"/*.sh

and a module nobody names today is still run tomorrow.

Usage: runtime-script-closure.py [--list] [repo-root]
  --list   print the closure and exit 0, for building the embed set
  default  verify internal/platform/embedded carries all of it
"""
import os
import re
import subprocess
import sys

ENTRY_POINTS = ("scripts/hub-bootstrap.sh", "scripts/validate-all.sh")
GLOB_DIRS = ("scripts/validate/preflight", "scripts/validate/cluster")
# Three reference forms, because all three are in use and the first alone was
# not enough: a materialized bootstrap died on
#
#   source "$SCRIPT_DIR/lib/teardown.sh"
#
# which the closure had never seen, because it names no path beginning
# "scripts/". A fixture run found it; static analysis of one form had not.
#
#   scripts/x.sh                 a repository-relative path
#   $PLATFORM_DIR/scripts/x.sh   the platform root, which is dirname(SCRIPT_DIR)
#   $SCRIPT_DIR/lib/x.sh         relative to scripts/ itself
REFERENCE = re.compile(r"(?:\./)?(scripts/[A-Za-z0-9_./-]+\.(?:sh|py))")
PLATFORM_REL = re.compile(r"\$\{?PLATFORM_DIR\}?/(scripts/[A-Za-z0-9_./-]+\.(?:sh|py))")
SCRIPT_REL = re.compile(r"\$\{?SCRIPT_DIR\}?/([A-Za-z0-9_./-]+\.(?:sh|py))")
EMBEDDED = "internal/platform/embedded"


def closure(root):
    tracked = set(subprocess.run(
        ["git", "-C", root, "ls-files", "scripts"],
        capture_output=True, text=True, check=False).stdout.split())
    seen, queue = set(), list(ENTRY_POINTS)
    while queue:
        rel = queue.pop()
        full = os.path.join(root, rel)
        if rel in seen or not os.path.isfile(full):
            continue
        seen.add(rel)
        with open(full, encoding="utf-8", errors="ignore") as fh:
            body = fh.read()
        refs = set(REFERENCE.findall(body)) | set(PLATFORM_REL.findall(body))
        # $SCRIPT_DIR is scripts/ itself, so its matches are relative to it.
        refs |= {"scripts/" + r for r in SCRIPT_REL.findall(body)}
        queue.extend(r for r in refs if r not in seen)
    for d in GLOB_DIRS:
        full = os.path.join(root, d)
        if not os.path.isdir(full):
            continue
        for name in sorted(os.listdir(full)):
            if os.path.isfile(os.path.join(full, name)):
                seen.add(f"{d}/{name}")
    # UNTRACKED IS A FAILURE, NOT AN OMISSION. The closure is filtered to
    # git-tracked files so a scratch copy in a working tree cannot enter a
    # release. But a NEW script an entry point already sources is a different
    # thing: dropping it silently produced a green gate for a release that would
    # not carry it. lib/tool-install.sh was extracted, sourced by
    # hub-bootstrap.sh, and invisible here until it was staged.
    untracked = sorted(f for f in seen if f not in tracked and os.path.isfile(os.path.join(root, f)))
    if untracked:
        for f in untracked:
            print(f"::error::{f} is referenced by the Day-0 entry points but is not "
                  f"tracked by git, so the release would not carry it; git add it"
                  if os.environ.get("GITHUB_ACTIONS") else
                  f"FAIL {f} is referenced by the Day-0 entry points but is not tracked "
                  f"by git, so the release would not carry it; git add it", file=sys.stderr)
        raise SystemExit(1)
    return sorted(f for f in seen if f in tracked)


def main():
    args = [a for a in sys.argv[1:] if a != "--list"]
    root = args[0] if args else "."
    want = closure(root)

    if "--list" in sys.argv[1:]:
        print("\n".join(want))
        return 0

    # THE DIRECTIVE FIRST. Copying the closure onto disk is not embedding it:
    # the generator writes embed.go from a heredoc, so a run that staged every
    # script while emitting `//go:embed all:manifests` left a binary carrying
    # none of them -- and a check that only looked at disk passed.
    directive = os.path.join(root, EMBEDDED, "embed.go")
    if os.path.isfile(directive):
        with open(directive, encoding="utf-8") as fh:
            if "all:scripts" not in fh.read():
                print(f"FAIL {EMBEDDED}/embed.go does not embed scripts: the closure is "
                      f"staged on disk and compiled into nothing", file=sys.stderr)
                return 1

    missing = [f for f in want
               if not os.path.isfile(os.path.join(root, EMBEDDED, f))]
    for f in missing:
        print(f"FAIL {EMBEDDED} carries no {f}, which Day-0 reaches from "
              f"{' or '.join(ENTRY_POINTS)}", file=sys.stderr)
    if missing:
        print(f"\nRun scripts/package/embed-platform-assets.sh.\n"
              f"A released CLI without these cannot bootstrap, and a validator "
              f"it cannot find is a suite that SKIPS rather than fails.",
              file=sys.stderr)
    print(f"runtime-script-closure: {len(want)} script(s) required, "
          f"{len(missing)} missing")
    return 1 if missing else 0


if __name__ == "__main__":
    sys.exit(main())
