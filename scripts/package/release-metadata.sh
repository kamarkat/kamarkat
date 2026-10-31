#!/usr/bin/env bash
# Write what a tenant needs to decide whether to take this version.
#
# ADR-064 moves disclosure from the proposal to the release: a published version
# records the versions it spans from its predecessor, the security fixes among
# them, whether each step is operationally reversible, and the minimum version it
# may be taken from. Renovate surfaces release notes in the proposal it opens, so
# writing this here reaches a tenant reading the pull request AND one reading the
# release directly -- which prose generated per proposal would not.
#
# Appended to the release body rather than replacing it: the generated changelog
# is what a maintainer reads, and this is what a tenant reads.
set -euo pipefail

VERSION="${1:?usage: release-metadata.sh <version> [previous]}"
PREVIOUS="${2:-}"

if [[ -z "$PREVIOUS" ]]; then
    # Asked of GitHub rather than of git. actions/checkout does not fetch tags by
    # default, so `git tag --list` returns nothing in a release run and every
    # release reports itself as the first -- which v0.1.7 did, telling a tenant
    # it spanned from nothing while two releases stood before it.
    #
    # Falls back to git for a local run, where the tags are present.
    PREVIOUS=$(gh release list --limit 20 --json tagName -q '.[].tagName' 2>/dev/null \
        | grep -v "^v\{0,1\}${VERSION}$" | head -1 || true)
    if [[ -z "$PREVIOUS" ]]; then
        PREVIOUS=$(git tag --list 'v*' --sort=-v:refname | grep -v "^v${VERSION}$" | head -1 || true)
    fi
fi

if [[ -z "$PREVIOUS" ]]; then
    echo "release-metadata: no earlier release found for ${VERSION}." >&2
    echo "If this is genuinely the first, that is correct. If it is not, the" >&2
    echo "disclosure below will tell every tenant this version spans from" >&2
    echo "nothing, so check that tags and releases are readable here." >&2
fi

range=""
if [[ -n "$PREVIOUS" ]]; then
    range="${PREVIOUS}..HEAD"
fi

# A security fix is declared by the commit that makes it, not inferred from
# words in a subject line: inference would either miss one or cry wolf, and both
# make the marker worth ignoring.
security=""
if [[ -n "$range" ]]; then
    security=$(git log "$range" --grep='^security(' --format='- %s' || true)
fi

# Reversibility is declared per release and defaults to unknown rather than to
# reversible. A tenant told a one-way step is reversible discovers otherwise at
# the moment it needs to go back.
reversible="${BUNDLE_REVERSIBLE:-unknown}"
minimum="${BUNDLE_MINIMUM_FROM:-${PREVIOUS#v}}"

{
    echo
    echo "## Taking this version"
    echo
    echo "| | |"
    echo "|---|---|"
    echo "| Spans from | ${PREVIOUS:-no earlier release} |"
    echo "| Minimum version this may be taken from | ${minimum:-any} |"
    echo "| Predecessor restorable from this version | ${reversible} |"
    echo
    if [[ -n "$security" ]]; then
        echo "### Security fixes in this version"
        echo
        echo "$security"
        echo
        echo "ADR-069 places security fixes outside the ordinary cadence. This version"
        echo "is recommended whether or not a tenant intends to move otherwise, and"
        echo "declining it leaves the fixes above unapplied."
    else
        echo "No security fixes are declared in this version."
    fi
    echo
    echo "Nothing here is applied by the platform. A cluster changes when a tenant"
    echo "merges the proposal against its own repository and its own control plane"
    echo "reconciles the result (ADR-065)."

    # ADR-069: the window is declared when the version is published, and this is
    # that moment. Declared nowhere before this, which made "supported for a
    # stated window" a sentence no tenant could act on and the platform could
    # not answer.
    python3 "$(dirname "$0")/support-window.py" "$VERSION"

    # What moved, from the inventory publish.sh wrote into the bundle chart. The
    # full list is in the chart and attached to this release as components.json.
    inventory="${INVENTORY:-dist/charts/platform-bundle/components.json}"
    if [ -f "$inventory" ]; then
        python3 - "$inventory" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
changes = d.get("changes")
print()
print("## Components")
print()
if changes is None:
    print(f"{len(d['components'])} components. No earlier inventory to compare against.")
elif not changes:
    print(f"{len(d['components'])} components. None changed since {d['previous']}.")
else:
    print(f"{len(d['components'])} components; {len(changes)} changed since {d['previous']}.")
    print()
    groups = {g["id"]: g["title"] for g in d["groups"]}
    print("| Component | Where | Change | From | To |")
    print("|---|---|---|---|---|")
    for c in changes:
        nodes = ", ".join(n.replace("-", " ") for n in c["nodes"])
        where = groups[c["cluster"]] + (f" ({nodes})" if nodes else "")
        print(f"| {c['name']} | {where} | {c['kind']} | {c['from']} | {c['to']} |")
PY
    fi
} 
