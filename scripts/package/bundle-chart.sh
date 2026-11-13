#!/usr/bin/env bash
# Package the environment-manager chart as the bundle's own chart.
#
# This is what the seed Application installs, and it carries the boundaries. It
# is packaged rather than referenced so a released cluster reconciles its own
# boundary definitions from the bundle instead of reaching for this repository
# (ADR-063: a tenant must be able to hold the bundle itself).
#
# The component descriptors are copied in here, not committed. They are already
# declared under manifests/argocd/components/, and a committed second copy is
# the drift ADR-063 exists to refuse -- the chart carries a build artifact, and
# the source stays the single declaration.
set -euo pipefail

VERSION="${1:?usage: bundle-chart.sh <version> [outdir]}"
OUTDIR="${2:-dist/charts}"
SRC="manifests/argocd/environment-manager"
CHART="$OUTDIR/platform-bundle"

# Staging lives in scripts/lib so the validators assemble the chart exactly as
# this does. They render it to check what a tenant installs, and a second copy
# of these few lines is how a validator ends up checking something else.
#
# It also copies the values files a boundary installs a third-party chart with.
# Those are inlined into the Application in a released bundle, so the second
# source that exists only to make $values resolve -- and which names this
# repository -- disappears.
# shellcheck source=../lib/stage-bundle-chart.sh
source "$(dirname "${BASH_SOURCE[0]}")/../lib/stage-bundle-chart.sh"

mkdir -p "$OUTDIR"
stage_bundle_chart "$SRC" "$CHART"

# Provenance: which source produced this artefact.
#
# Without it a published bundle records its own version and nothing else, so
# "what is in 0.1.16-rc.29?" has no answer that does not depend on someone
# remembering. Both callers build from a WORKING TREE -- the release workflow
# from a checkout, a developer from whatever is on their laptop -- and a bundle
# built from uncommitted edits is untraceable by construction: it is in a
# registry, tenants can pull it, and no commit corresponds to it.
#
# Recorded rather than enforced, FOR CHART CONTENT. Refusing to publish from a
# dirty tree would break the local release path this script exists to support
# (the whole point of testing a release before committing it), so the dirty state
# is stamped instead and shows up wherever the chart is inspected.
#
# THAT IS ONLY SAFE FOR DIRT THE CHART ITSELF CARRIES, and "dirty" was one word
# standing for two conditions:
#
#   manifests, and operators/*/config/   The chart content IS the dirty content.
#                                        kustomize reads those paths from this
#                                        tree, so the artefact is
#                                        self-consistent. A stamp is the right
#                                        answer and a local test is meaningful.
#
#   operator SOURCE (Go, Dockerfile,     The chart ships the dirty manifests, but
#   go.mod, cmd/, internal/, api/)       the IMAGE is built by a per-component CI
#                                        workflow from a COMMIT, which then
#                                        commits the resolved digest back. There
#                                        is no commit for uncommitted code, so no
#                                        image was ever built and the pin still
#                                        points at an older one. The artefact is
#                                        internally inconsistent: new manifests,
#                                        old binary.
#
# The second case cannot be stamped toward anything, and the warning's own advice
# -- "fine for a local release test" -- is false for it, because the local test
# exercises the old image. It is also invisible downstream: the chart renders, the
# digest is valid, and every Application reaches Synced, because an old image is a
# perfectly healthy image.
#
# It cost 0.1.16-rc.140: manager.yaml shipped a new TENANT_ID env var and the
# binary that reads it was never built. The same class cost rc.30 and rc.31, which
# is why publish.sh HARD-REFUSES a tree that is merely behind its upstream -- the
# milder version of this, where the right image exists and the tree just is not
# pointing at it. Refusing that while warning about this had the severities
# inverted against the harm.
#
# So it is split: refuse on image content, stamp chart content.
#
# Scoped to operators/ as a whole and not to a list of operators that currently
# ship. ephemeral-vm-provisioner and workspace-sync are not referenced by any
# component today; an exemption list would quietly stop covering them on the day
# they are, which is the day it matters.
GIT_SHA="$(git rev-parse HEAD 2>/dev/null || echo unknown)"
GIT_DIRTY=false
if [ -n "$(git status --porcelain 2>/dev/null)" ]; then
    GIT_DIRTY=true
fi
GIT_BRANCH="$(git rev-parse --abbrev-ref HEAD 2>/dev/null || echo unknown)"
BUILT_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

# Unstaged, staged and untracked, because all three produce an image that does not
# exist. `config/` is excluded as chart content per the reasoning above.
IMAGE_DIRT="$(
    {
        git diff --name-only -- operators 2>/dev/null
        git diff --cached --name-only -- operators 2>/dev/null
        git ls-files --others --exclude-standard -- operators 2>/dev/null
    } | sort -u | grep -vE '^operators/[^/]+/config/' || true
)"

if [ -n "$IMAGE_DIRT" ]; then
    echo "platform-bundle: REFUSING to build -- operator SOURCE is uncommitted." >&2
    echo >&2
    printf '%s\n' "$IMAGE_DIRT" | sed 's/^/    /' >&2
    echo >&2
    echo "  The chart would be built from this tree, but operator images are built by" >&2
    echo "  CI from a COMMIT and the digest is committed back afterwards. Nothing has" >&2
    echo "  built the code above, so the bundle would pin an OLDER image: new" >&2
    echo "  manifests, old binary, every Application Synced and the change absent." >&2
    echo "  That is what happened to 0.1.16-rc.140." >&2
    echo >&2
    echo "  Commit and push the operator changes, wait for the" >&2
    echo "  'chore(<operator>): update image to <sha>' commit, then:" >&2
    echo "    git pull --rebase && ./scripts/release.sh" >&2
    echo >&2
    echo "  Dirt in manifests/ or operators/*/config/ does NOT trigger this: the" >&2
    echo "  chart carries that content itself, so it is stamped rather than refused." >&2
    exit 1
fi

if [ "$GIT_DIRTY" = true ]; then
    echo "platform-bundle: WARNING -- built from a dirty working tree." >&2
    echo "  The artefact records commit ${GIT_SHA} plus uncommitted changes that" >&2
    echo "  nothing else captures. Fine for a local release test; commit before" >&2
    echo "  publishing a version anyone else will consume." >&2
    echo "  Operator SOURCE is checked separately and refuses; this is chart content." >&2
fi

python3 - "$CHART/Chart.yaml" "$VERSION" "$GIT_SHA" "$GIT_DIRTY" "$GIT_BRANCH" "$BUILT_AT" <<'PY'
import sys, re, pathlib
p, version, sha, dirty, branch, built = (pathlib.Path(sys.argv[1]),) + tuple(sys.argv[2:7])
t = p.read_text()
t = re.sub(r"(?m)^version:.*$", f"version: {version}", t)
t = re.sub(r"(?m)^appVersion:.*$", f'appVersion: "{version}"', t)

# Annotations, not new top-level keys: Helm rejects fields it does not know, and
# annotations are the documented place for metadata of this kind.
t = re.sub(r"(?ms)^annotations:\n(?:[ \t]+.*\n?)*", "", t)
t = t.rstrip("\n") + "\n"
t += (
    "annotations:\n"
    + f'  zero-ops.io/source-commit: "{sha}"\n'
    + f'  zero-ops.io/source-dirty: "{dirty}"\n'
    + f'  zero-ops.io/source-branch: "{branch}"\n'
    + f'  zero-ops.io/built-at: "{built}"\n'
)
p.write_text(t)
PY

# A released render must produce the boundaries. The chart fails loudly when its
# descriptors are missing, so this proves they were copied AND that the released
# path renders at all -- neither of which the development path exercises.
# management.infisicalEnvironmentSlug is supplied like every other required
# value here: `management` is what a box declares (gitops-template's
# templates/mgmt/values.yaml) and what Day-0 seeds, so the smoke test renders
# what ships. It has no chart default on purpose -- ADR-111 Revision 7 refuses to
# guess which environment someone else's secrets are in, because guessing wrong
# resolves silently from the wrong scope -- and a gate that renders without it is
# testing a shape no box ever has.
if ! helm template platform-bundle "$CHART" \
        --set environmentSlug=dev --set boxId=example-box --set provider=hybrid --set publicTlsIssuer=letsencrypt-prod \
        --set management.infisicalEnvironmentSlug=management \
        --set instanceRepoURL=https://github.com/example-org/example-gitops \
        --set hubDomain=dev.example.test \
        --set previewDomain=example-preview.test \
        --set 'alerting.emailTo[0]=alerts@example.test' \
        --set bundleVersion="$VERSION" > /dev/null 2>"$CHART/.err"; then
    echo "platform-bundle: released render failed:" >&2; sed 's/^/  /' "$CHART/.err" >&2
    rm -rf "$CHART"; exit 1
fi
rm -f "$CHART/.err"

echo "packaged platform-bundle -> $CHART ($(find "$CHART/descriptors" -name '*.yaml' | wc -l | tr -d ' ') descriptor(s))"
