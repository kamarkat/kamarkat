#!/usr/bin/env bash
# What a released bundle still fetches from the platform's own repository.
#
# ADR-063 requires a tenant to be able to hold the bundle it runs, and ADR-065
# that revoking the platform's access stops proposals arriving and stops nothing
# running. Both are false for anything a released cluster still reaches into
# this repository for: the bundle would be portable only in appearance, and the
# gap is invisible until the day the repository is not reachable.
#
# Fails rather than reports. ADR-063 makes mirroring a condition of a supported
# runtime, so a bundle that cannot be mirrored cannot be released -- a reference
# left here would make the custody claim untrue for every tenant that received
# the bundle, and the gap stays invisible until the platform is unreachable.
set -euo pipefail
# pipefail alone is not enough: the exit code that matters is the checker's, and
# it is the last stage of the pipe, so `set -e` propagates it.

VERSION="${1:-0.0.0-audit}"
CHART=$(mktemp -d)/platform-bundle
scripts/package/bundle-chart.sh "$VERSION" "$(dirname "$CHART")" >/dev/null

# A FAILED RENDER IS NOT A CLEAN BUNDLE. This piped into the checker with
# stderr discarded, so a render that could not run at all produced an empty
# stream, the checker found no portability reference in it, and the gate passed
# -- reporting the one thing it exists to disprove. Rendered to a file first, so
# the failure is the script's.
rendered=$(mktemp)
trap 'rm -f "$rendered"' EXIT
if ! helm template platform-bundle "$CHART" \
    --set environmentSlug=dev --set boxId=example-box --set provider=hybrid \
    --set management.infisicalEnvironmentSlug=management \
    --set instanceRepoURL=https://github.com/example-org/example-gitops \
    --set hubDomain=dev.example.test \
    --set previewDomain=example-preview.test \
    --set 'alerting.emailTo[0]=alerts@example.test' \
    --set publicTlsIssuer=letsencrypt-prod --set bundleVersion="$VERSION" >"$rendered"; then
    echo "bundle-portability: the bundle did not render, so nothing was checked" >&2
    exit 1
fi
# The platform repository URL comes from the chart value that declares it, not
# from a copy inside the checker. A copy went stale when the repository was
# renamed and the gate passed unconditionally from then on.
platform_repo=$(yq -r '.platformRepoURL // ""' manifests/argocd/environment-manager/values.yaml)
if [ -z "$platform_repo" ]; then
    echo "bundle-portability: platformRepoURL is not set in the environment-manager" >&2
    echo "                   values, so there is nothing to check portability against" >&2
    exit 1
fi
python3 scripts/validate/bundle-portability.py "$platform_repo" <"$rendered"
