#!/usr/bin/env bash
# The platform repository URL that every published artefact must carry.
#
# WHY A PUBLISHED ARTEFACT NEEDS IT. GHCR links a package to a repository only
# when the push is authenticated as that repository, or when the artefact itself
# carries org.opencontainers.image.source. An org package with no repository link
# grants Actions access to NO repository, so CI is refused write -- "403
# Forbidden" on POST .../blobs/uploads/ -- however the workflow declares
# `packages: write`. That is what the 2026-11-04 rename to the kamarkat org
# produced: all 17 packages were re-created by local PAT pushes rather than by
# the workflow, none was linked, and the first CI publish after the rename could
# not write a single blob. The September releases (v0.1.11-v0.1.15) had succeeded
# because CI created those packages itself, under the old org, and GHCR linked
# them on creation.
#
# Helm sets that annotation from Chart.yaml's FIRST `sources:` entry -- not from
# `annotations:` -- see helm v4 pkg/registry/chart.go, generateChartOCIAnnotations,
# which maps meta.Sources[0] to ocispec.AnnotationSource. So charts declare it as
# `sources:` and images as a build label.
#
# READ, NOT RESTATED. This is the same value scripts/validate/bundle-portability.sh
# compares references against. A copy of a repository name is what went stale at
# the rename and left that gate unable to fail, so there is one declaration and
# everything reads it.
platform_source_url() {
    local f="manifests/argocd/environment-manager/values.yaml" url
    command -v yq >/dev/null 2>&1 || {
        echo "platform-source-url: yq is required to read platformRepoURL from $f" >&2
        echo "                     (brew install yq)" >&2
        return 1
    }
    url=$(yq -r '.platformRepoURL // ""' "$f" 2>/dev/null)
    [ -n "$url" ] || {
        echo "platform-source-url: platformRepoURL is not set in $f." >&2
        echo "  Published charts would carry no source, GHCR would not link the" >&2
        echo "  package to its repository, and CI would be refused write on the" >&2
        echo "  next release with a 403 that names neither cause." >&2
        return 1
    }
    printf '%s\n' "$url"
}
