#!/usr/bin/env bash
# Fail when a Day-0 script reads a variable nothing assigns.
#
# `set -u` turns such a reference into an immediate abort, and the abort happens
# on whichever path first reaches the line -- which for this platform means a
# tenant's CI, hours into a bootstrap, on a released binary.
#
# This is what shipped as 0.3.2:
#
#   Step 1: Bootstrapping Hub Cluster...
#   hub-bootstrap.sh: line 1053: env_flag: unbound variable
#
# ADR-111 Revision 7 made the management hub environment-independent and the
# commit that implemented it removed `env_flag=`. Four expansions stayed. The
# first was inside a log line, so the run died before the CLI was invoked -- and
# `bash -n` passes, because the syntax is perfectly valid.
#
# WHAT IS CHECKED. shellcheck SC2154 over every shell script in the closure
# runtime-script-closure.py computes -- the scripts a released box actually runs.
# Nothing else is enabled: this gate is about one failure mode, and widening it
# to shellcheck's full warning set is a separate decision with its own cleanup.
#
# It REFUSES rather than skips when shellcheck is absent. A gate that quietly
# passes when its tool is missing is how an unvalidated change reaches a cluster,
# and this repository has the scar to prove it: an absent validator made
# validate-all.sh SKIP a suite, which reads as a pass. GitHub's ubuntu-24.04
# image ships shellcheck; `brew install shellcheck` covers a workstation.
#
# Usage: shell-unassigned-variables.sh [repo-root]
set -euo pipefail

ROOT="${1:-.}"
cd "$ROOT"

if ! command -v shellcheck >/dev/null 2>&1; then
    echo "shell-unassigned-variables: shellcheck is not installed, so this gate" >&2
    echo "  cannot run. Install it (brew install shellcheck) and re-run; it is not" >&2
    echo "  skippable, because the defect it catches only appears at run time." >&2
    exit 1
fi

# One invocation, so shellcheck resolves `source` directives between them rather
# than judging each file as though its libraries did not exist.
scripts=()
while IFS= read -r f; do
    [[ "$f" == *.sh ]] && scripts+=("$f")
done < <(python3 -I scripts/validate/runtime-script-closure.py --list .)

if (( ${#scripts[@]} == 0 )); then
    echo "shell-unassigned-variables: the closure resolved no shell scripts, so" >&2
    echo "  this gate would check nothing and pass." >&2
    exit 1
fi

if ! shellcheck -S warning --include=SC2154 "${scripts[@]}"; then
    echo "" >&2
    echo "Each name above is read and never assigned. Under set -u that is not a" >&2
    echo "warning -- it ends the run at that line. Assign it, or remove the" >&2
    echo "reference if what used to set it is gone." >&2
    exit 1
fi

echo "shell-unassigned-variables: ${#scripts[@]} script(s), 0 unassigned reference(s)"
