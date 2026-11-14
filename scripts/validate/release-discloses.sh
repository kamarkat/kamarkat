#!/usr/bin/env bash
# A published version must say where to read what taking it means.
#
# ADR-064 revision 2 keeps the obligation and moves where it is met. The version
# page in the documentation records the versions this one spans from its
# predecessor, the security fixes among them, whether the predecessor is
# restorable from it, the minimum version it may be taken from, and the support
# window ADR-069 declares -- all rendered from the inventory the release
# publishes. The release body carries the link to that page and nothing else.
#
# So this checks the link, which is the whole body. A release with no body, or
# one whose body does not name this version's page, leaves a tenant deciding
# whether to take the version with nowhere to look -- and the generator that
# writes the body can fail quietly, which is the case this exists for.
set -euo pipefail

TAG="${1:?usage: release-discloses.sh <tag>}"

body=$(gh release view "$TAG" --json body -q .body 2>/dev/null || true)
if [[ -z "$body" ]]; then
    echo "release-discloses: ${TAG} has no release body, so it says nothing about" >&2
    echo "  what taking this version means and points nowhere that does." >&2
    exit 1
fi

# The page for THIS version, not merely some documentation link. A body left
# over from another release, or a link to the documentation root, would pass a
# check that only looked for a URL.
version="${TAG#v}"
slug=$(printf '%s' "$version" | tr -c '[:alnum:]' '-' | tr -s '-' | sed 's/-$//')
if ! grep -q "/versions/releases/${slug}" <<<"$body"; then
    echo "release-discloses: ${TAG}'s body does not link its own version page." >&2
    echo "" >&2
    echo "  expected to find: /versions/releases/${slug}" >&2
    echo "  body was:" >&2
    sed 's/^/    /' <<<"$body" >&2
    echo "" >&2
    echo "  The body is written by scripts/package/release-metadata.sh in the step" >&2
    echo "  before this one. Check that it ran and that DOCS_BASE_URL is set." >&2
    exit 1
fi

echo "release discloses: ${TAG} links its version page"
