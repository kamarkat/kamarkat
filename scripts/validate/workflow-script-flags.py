#!/usr/bin/env python3
"""Fail when a workflow passes a flag the script it calls does not accept.

Three releases have been broken by exactly this, each discovered by a tenant
hours into a bootstrap rather than by a gate:

  --box        renamed to --git-org in the CLI; the released validator and the
               local loop still passed the old name
  --workers    passed by the tenant template to a workflow input that was
               forwarded to hub-bootstrap.sh, which never accepted it; every
               bootstrap whose declaration named a worker count died before
               provisioning anything
  --debug      copied one level too high: hub-bootstrap.sh passes --debug to
               `kakt bootstrap-mgmt`, and tenant-bootstrap.yml passed it to
               hub-bootstrap.sh, which refuses unknown arguments. No released
               box ever got past that step

None of them could fail locally, because the local loop invokes these scripts
with its own argument list. The caller and the callee are in one repository and
disagree in writing, so the disagreement is checkable here.

WHAT IS CHECKED. For every `scripts/**.sh` invocation inside a workflow's `run:`
block, every literal long flag on that command line must appear as a case arm in
the script it calls. Flags that come from shell arrays or variables are not
literal and are not checked -- those expand at run time, and a validator that
guessed at them would report failures nobody can act on.

AND, the same disagreement one level up: every `workflow_dispatch` input a
workflow declares must be read somewhere in that workflow. bootstrap-cluster.yml
asked an operator for provider, region, on-prem and tailnet -- two of them
required -- and the resolver read all four from the cluster's declaration and
discarded the inputs. Nothing failed; the values simply had no effect, so a
dispatch could name a provider that disagreed with git while appearing to have
chosen it.

Usage: workflow-script-flags.py [repo-root]
"""
import os
import re
import sys

WORKFLOW_DIRS = (
    ".github/workflows",
    "manifests/tenants/gitops-template/.github/workflows",
)

# `bash .platform-validators/scripts/hub-bootstrap.sh \` or `./scripts/x.sh`
INVOCATION = re.compile(r"(?:bash\s+)?(?:\./)?(?:[\w./-]*?)(scripts/[\w./-]+\.sh)")
# A case arm's whole pattern, up to its `)`: `--name)`, `--gitops-dir=*)`,
# `--on-prem|--home-worker-enabled)`, `--yes|-y)`, `*)`.
#
# The pattern is captured and every long flag in it taken, rather than one flag
# per arm. A first attempt captured only the LAST alternative, so
# `--on-prem|--home-worker-enabled)` registered one of the two and `--yes|-y)`
# neither -- a validator that would have reported a caller passing --on-prem as
# a failure. Over-collecting here only makes the allowlist wider, which is the
# safe direction for a check that fails a release.
CASE_ARM = re.compile(r"^\s*([^()\s]+(?:\|[^()\s]+)*)\)", re.M)
LONG_FLAG = re.compile(r"(?<![\w-])--[a-z][\w-]*")


# A flag tested directly rather than through a case arm:
#   CHECK="${1:-}" ... [[ "$CHECK" == "--check" ]]
#   [[ "$1" = "--yes" ]]
# Only COMPARISONS count. A flag merely mentioned in the file does not: both
# flags this validator was written for appear in the script that rejects them --
# hub-bootstrap.sh passes --debug onward to `kakt bootstrap-mgmt` and names it in
# a log line -- so "appears anywhere" would have called the bug accepted.
COMPARED = re.compile(r"""(?:==|=|!=)\s*["']?(--[\w-]+)|["']?(--[\w-]+)["']?\s*(?:==|!=)""")


def accepted_flags(path):
    """The long flags a script's own argument parser handles.

    Two forms, because both are in use here: a `case` arm, and a direct
    comparison for a script that takes one flag and no parser.
    """
    with open(path, encoding="utf-8") as fh:
        body = fh.read()
    flags = {flag for arm in CASE_ARM.findall(body) for flag in LONG_FLAG.findall(arm)}
    for a, b in COMPARED.findall(body):
        flags.add(a or b)
    return flags - {""}


def invocation_span(lines, start):
    """The command beginning at `start`, following backslash continuations."""
    span = [lines[start]]
    i = start
    while i < len(lines) - 1 and lines[i].rstrip().endswith("\\"):
        i += 1
        span.append(lines[i])
    return "\n".join(span)


def dead_dispatch_inputs(body):
    """Inputs the workflow declares and never reads, in declaration order.

    Parsed from the text rather than with a YAML loader so the line number can
    be reported, and because `on:` loads as the boolean True.
    """
    inputs, dead = [], []
    in_dispatch = in_inputs = False
    for n, line in enumerate(body.splitlines(), 1):
        if re.match(r"^\s{0,2}workflow_dispatch:", line):
            in_dispatch, in_inputs = True, False
            continue
        if in_dispatch and re.match(r"^\s{2,6}inputs:\s*$", line):
            in_inputs = True
            continue
        if in_inputs:
            m = re.match(r"^(\s+)([A-Za-z0-9_-]+):\s*$", line)
            if m and len(m.group(1)) <= 8:
                inputs.append((m.group(2), n))
                continue
            # Dedented back out of the inputs block.
            if line.strip() and not line.startswith(" " * 8):
                in_dispatch = in_inputs = False
    for name, n in inputs:
        read = (
            re.search(r"(?:github\.event\.)?inputs\.%s\b" % re.escape(name), body)
            or re.search(r"inputs\[['\"]%s['\"]\]" % re.escape(name), body)
        )
        if not read:
            dead.append((name, n))
    return dead


def main():
    root = sys.argv[1] if len(sys.argv) > 1 else "."
    failures = []
    checked = 0

    for d in WORKFLOW_DIRS:
        wd = os.path.join(root, d)
        if not os.path.isdir(wd):
            continue
        for name in sorted(os.listdir(wd)):
            if not name.endswith((".yml", ".yaml")):
                continue
            wf = os.path.join(wd, name)
            body = open(wf, encoding="utf-8").read()
            lines = body.splitlines()
            for inp, n in dead_dispatch_inputs(body):
                failures.append(
                    f"{d}/{name}:{n} declares the dispatch input '{inp}', which "
                    f"nothing in the workflow reads; remove it or read it")
            for n, line in enumerate(lines):
                m = INVOCATION.search(line)
                if not m:
                    continue
                script = m.group(1)
                target = os.path.join(root, script)
                if not os.path.isfile(target):
                    # A path this repository does not hold is its own problem,
                    # reported rather than skipped: a workflow calling a script
                    # that is not here cannot work either.
                    failures.append(
                        f"{d}/{name}:{n + 1} calls {script}, which does not exist")
                    continue
                checked += 1
                allowed = accepted_flags(target)
                passed = set(LONG_FLAG.findall(invocation_span(lines, n)))
                for flag in sorted(passed - allowed):
                    failures.append(
                        f"{d}/{name}:{n + 1} passes {flag} to {script}, "
                        f"which accepts: {', '.join(sorted(allowed)) or '(none)'}")

    for f in failures:
        print(f"::error::{f}" if os.environ.get("GITHUB_ACTIONS") else f"FAIL {f}",
              file=sys.stderr)
    print(f"workflow-script-flags: {checked} invocation(s), {len(failures)} failure(s)")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
