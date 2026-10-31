#!/usr/bin/env bash
# Build the component inventory of versions published before inventories existed.
#
#   backfill-inventory.sh <out-dir> <version>...
#
# Each version's published charts are pulled from the registry and inventoried
# against the commit they were built from: the chart's zero-ops.io/source-commit
# annotation, or the release tag where there is no annotation. A version with
# neither cannot be tied to a tree and is refused rather than inventoried against
# the wrong one. Versions are compared in the order given, each against the one
# before it.
#
# Upstream renders and fetched manifests are cached in dist/inventory-cache, the
# cache publish.sh uses, so a chart rendered for one version or release is not
# downloaded again for the next.
#
# The catalog is read from THIS tree, so an older version is described with
# today's descriptions; what it ran is read from its own commit.
set -euo pipefail

OUT="${1:?usage: backfill-inventory.sh <out-dir> <version>...}"; shift
[ $# -gt 0 ] || { echo "usage: backfill-inventory.sh <out-dir> <version>..." >&2; exit 2; }
OWNER="${OWNER:-soloz-io}"
HERE="$(cd "$(dirname "$0")/../.." && pwd)"
WORK="$(mktemp -d)"
trap 'git -C "$HERE" worktree prune; rm -rf "$WORK"' EXIT
mkdir -p "$OUT"

previous=""
for v in "$@"; do
    echo "== $v"
    charts="$WORK/$v/charts"
    mkdir -p "$charts"
    helm pull "oci://ghcr.io/${OWNER}/charts/environment-manager" --version "$v" --untar -d "$charts" >/dev/null
    for c in platform platform-spoke-gateway platform-workload tenant-public-tls universal-tenant; do
        if ! err=$(helm pull "oci://ghcr.io/${OWNER}/charts/$c" --version "$v" --untar -d "$charts" 2>&1 >/dev/null); then
            grep -qiE 'not found|manifest unknown|NAME_UNKNOWN' <<<"$err" || { echo "$err" >&2; exit 1; }
            echo "   $c was not published at $v"
        fi
    done

    commit=$(awk -F'"' '/zero-ops.io\/source-commit/ {print $2}' "$charts/environment-manager/Chart.yaml")
    [ -n "$commit" ] || commit=$(awk '/zero-ops.io\/source-commit/ {print $2}' "$charts/environment-manager/Chart.yaml")
    if [ -z "$commit" ]; then
        git -C "$HERE" rev-parse -q --verify "refs/tags/v$v" >/dev/null \
            || { echo "backfill: $v records no source commit and has no tag; it cannot be tied to a tree" >&2; exit 1; }
        commit="v$v"
    fi
    echo "   source: $commit"
    git -C "$HERE" worktree add --detach -q "$WORK/$v/src" "$commit"

    args=(--repo "$WORK/$v/src" --catalog "$HERE/manifests/architecture/component-catalog.yaml"
          --expand "$HERE/dist/inventory-cache")
    if [ -n "$previous" ]; then
        args+=(--previous-version "$previous")
        [ -f "$OUT/$previous.json" ] && args+=(--previous "$OUT/$previous.json")
    fi
    python3 "$HERE/scripts/package/inventory.py" "$v" "$charts" "$OUT/$v.json" "${args[@]}"
    git -C "$HERE" worktree remove --force "$WORK/$v/src"
    previous="$v"
done
