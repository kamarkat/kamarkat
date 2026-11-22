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
# DATA, NOT ONLY EXECUTABLES. These traced .sh and .py alone, so a validator's
# data dependency was invisible: `078-collection-floor.sh` reads
# scripts/observability/mixin/metrics-allowlist.json, the file was never
# embedded, and on a released box the module crashed on FileNotFoundError before
# asserting anything -- which the acceptance run reports as unverified, correctly
# and unhelpfully, because the fault is in the packaging and not the box.
#
# A script that reads a file needs that file. The extension says whether the
# closure can follow it further, not whether it has to carry it.
REFERENCE = re.compile(r"(?:\./)?(scripts/[A-Za-z0-9_./-]+\.(?:sh|py|json|ya?ml))")
PLATFORM_REL = re.compile(r"\$\{?PLATFORM_DIR\}?/(scripts/[A-Za-z0-9_./-]+\.(?:sh|py|json|ya?ml))")
SCRIPT_REL = re.compile(r"\$\{?SCRIPT_DIR\}?/([A-Za-z0-9_./-]+\.(?:sh|py|json|ya?ml))")

# THE SOURCE A VALIDATOR READS AS ITS CONTRACT. 003-seeded-credential-sources.sh
# parses the hub-operator's own mapping table and namespace constants, so that a
# renamed namespace cannot make the check silently examine the wrong one -- a
# deliberate choice, and one that needs the files to exist wherever the check
# runs. They are outside scripts/, so no pattern above would ever see them.
SOURCE_REF = re.compile(r"(?:\./)?(operators/[A-Za-z0-9_./-]+\.go)")

# A MANIFEST A VALIDATOR READS AS ITS CONTRACT. Four support-agent checks and the
# observability scoping check parse the manifests they assert about, so that a
# renamed field cannot make a check silently examine the wrong thing -- and on a
# released box none of those files was carried, so all five warned instead of
# asserting. The ADR-077 containment claims the documentation promises had never
# run against a real box.
MANIFEST_REF = re.compile(r"(?:\./)?(manifests/[A-Za-z0-9_./-]+\.ya?ml)")

# A BUDGET, because the embedded set is deliberately narrow. Validators name 36
# manifests totalling over 4 MB, among them a 2 MB CRD bundle and two 400 KB
# datapath templates -- carrying all of them to make five checks assert is the
# wrong trade, and a release that silently grew by 4 MB is how the set stopped
# being narrow in the first place.
#
# A file over the budget is NOT skipped: it is named, and refused, so the choice
# is made deliberately -- either the check reads something smaller, or the file
# is named in the generator's own list with a reason beside it.
MANIFEST_BUDGET_BYTES = 64 * 1024
EMBEDDED = "internal/platform/embedded"


def closure(root):
    tracked = set(subprocess.run(
        ["git", "-C", root, "ls-files", "scripts", "operators", "manifests"],
        capture_output=True, text=True, check=False).stdout.split())
    # THE GLOBBED MODULES ARE TRACED, NOT JUST LISTED. Every file in GLOB_DIRS
    # used to be added to the result after the walk, so a module's own
    # dependencies were never followed -- the walk only ever read the entry
    # points and what they source. A validator that reads a data file or a source
    # file it parses therefore contributed nothing to the closure, and the file it
    # needs was absent from every release.
    #
    # Queued instead, so each module is read exactly as the entry points are.
    oversized = {}
    seen, queue = set(), list(ENTRY_POINTS)
    for d in GLOB_DIRS:
        full = os.path.join(root, d)
        if not os.path.isdir(full):
            continue
        for name in sorted(os.listdir(full)):
            if os.path.isfile(os.path.join(full, name)):
                queue.append(f"{d}/{name}")
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
        # Go sources a validator parses as its contract. Added to the closure and
        # not followed further: the closure's job is to carry what a run reads,
        # and nothing reads an import graph.
        refs |= set(SOURCE_REF.findall(body))
        for manifest in MANIFEST_REF.findall(body):
            full_manifest = os.path.join(root, manifest)
            if not os.path.isfile(full_manifest):
                continue
            size = os.path.getsize(full_manifest)
            if size > MANIFEST_BUDGET_BYTES:
                oversized.setdefault(manifest, (size, rel))
                continue
            refs.add(manifest)
        queue.extend(r for r in refs if r not in seen)
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
    # OVERSIZED IS REPORTED, NOT DROPPED. A validator reading a 2 MB CRD bundle is
    # a check that cannot run on a released box, and saying nothing about it is
    # how five checks warned on every box for months. The release is not grown
    # silently either: the choice has to be made by someone.
    if oversized:
        for manifest, (size, by) in sorted(oversized.items()):
            print(f"NOTE {manifest} is {size // 1024} KiB, over the {MANIFEST_BUDGET_BYTES // 1024} KiB "
                  f"budget, and is read by {by}. The closure does not carry it. If that check has "
                  f"to assert on a released box, name the file in "
                  f"scripts/package/embed-platform-assets.sh with a reason beside it, as the "
                  f"datapath templates already are -- or have the check read something smaller.",
                  file=sys.stderr)
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
            directive_body = fh.read()
            # Every root the closure can name. A validator's contract is a Go
            # source under operators/, so staging it without widening the
            # directive copies the file onto disk and compiles in nothing --
            # which is the failure this block already existed to catch, for a
            # root that did not exist when it was written.
            for root_dir in ("all:scripts", "all:_operators"):
                if root_dir not in directive_body:
                    print(f"FAIL {EMBEDDED}/embed.go does not embed {root_dir[4:]}: part of the "
                          f"closure is staged on disk and compiled into nothing", file=sys.stderr)
                    return 1

    # Go source is STAGED under an underscore so the go tool does not treat it as
    # a package of this module, and extracted without one. The closure names the
    # path a run reads; this looks for the path the generator writes.
    def staged(rel):
        return "_" + rel if rel.startswith("operators/") else rel

    missing = [f for f in want
               if not os.path.isfile(os.path.join(root, EMBEDDED, staged(f)))]
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
