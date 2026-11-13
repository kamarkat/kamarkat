#!/usr/bin/env bash
# Package the platform bundle, gate it, and push it to a registry.
#
# One script, two callers. The release workflow runs it; so does a developer
# testing the released path locally (docs/runbooks/local-release-path-testing.md).
# Both therefore produce the same artefact set through the same gates -- which is
# the property that made a CI dispatch the only trustworthy way to publish, and
# the reason it no longer has to be.
#
# The cost that motivated this: a dispatch takes ~9m30s, and 8m30s of that is
# work a laptop does not have to repeat. Nearly all of the packaging time is
# kustomize --enable-helm refetching infisical, headlamp, postgresql and redis
# from upstream on a cold runner; those caches persist locally between runs, and
# the workflow itself retains them as release inputs for exactly that reason.
#
#   publish.sh <version> <ghcr-owner>
#
# SKIP_PUSH=1  package and gate without publishing. Nothing is spent: ADR-063
#              consumes a version by publishing it, so this is how a packaging
#              mistake is found without burning the next -rc.
set -euo pipefail

VERSION="${1:?usage: publish.sh <version> <ghcr-owner>}"
OWNER="${2:?usage: publish.sh <version> <ghcr-owner>}"
SKIP_PUSH="${SKIP_PUSH:-}"

# The tree must not be behind its upstream.
#
# Eight components are built by their own workflow, which tags the image with the
# commit SHA and then COMMITS the resolved digest back:
#
#   chore(hub-operator): update image to 43dd4a32...
#
# That commit lands after the developer's push, so between pushing and pulling
# the working tree still pins the PREVIOUS digest. A publish in that window
# produces a bundle whose charts are new and whose image is old, and says nothing
# -- the chart renders, the digest is valid, the Application syncs Healthy. It
# cost 0.1.16-rc.30 and rc.31 in one afternoon: both shipped hub-operator's new
# manifests against its old image, so the ConfigMap the new code writes never
# appeared and public DNS stayed empty.
#
# Refused rather than recomputed. An earlier attempt derived each digest from git
# history instead of trusting the commit, and it was unsound: workflows are
# path-filtered and several share go.mod, so "the newest commit touching this
# component that has a published image" selected an image OLDER than the one
# already pinned -- kube-sbt resolved to 58b791ef when the repo correctly pinned
# a build from cd2d9ae7, an ancestor swap that would have downgraded three
# components on every publish. The source-to-image mapping is not derivable from
# history; only the build knows it, and the bot's commit is how it says so.
#
# So the invariant is simply that the commit has been pulled. That is exactly and
# only when the pinned digests are trustworthy.
#
# The permanent removal of this window is a CI change, not a script change: build
# the images and publish the bundle in one run, so there is no asynchronous
# commit to be behind.
if git rev-parse --abbrev-ref '@{u}' >/dev/null 2>&1; then
    git fetch -q origin 2>/dev/null || true
    behind="$(git rev-list --count 'HEAD..@{u}' 2>/dev/null || echo 0)"
    if [ "${behind:-0}" -ne 0 ]; then
        echo "publish: this tree is ${behind} commit(s) behind its upstream." >&2
        echo >&2
        git log --oneline 'HEAD..@{u}' | sed 's/^/  /' >&2
        echo >&2
        echo "  Image digests are committed by CI AFTER a push, so publishing from here" >&2
        echo "  would ship these charts against whatever images the tree still pins." >&2
        echo "  Run: git pull --rebase" >&2
        exit 1
    fi
fi

# Helm requires semver. A tag that is not one would publish a chart nobody can
# request by version.
if ! printf '%s' "$VERSION" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$'; then
    echo "publish: '$VERSION' is not semver; a chart version must be requestable" >&2
    exit 1
fi

# GitHub Actions renders ::error:: as an annotation; a terminal renders it as
# noise. Same message either way, addressed to whoever is reading.
if [ -n "${GITHUB_ACTIONS:-}" ]; then err() { echo "::error::$*"; }
else                                 err() { echo "error: $*" >&2; }; fi

# The packaging toolchain, pinned.
#
# kustomize's HelmChartInflationGenerator shells out to `helm version -c`, which
# Helm 4 removed -- so on a machine with Helm 4 on PATH every component that
# inflates a chart from its kustomization fails with "unknown shorthand flag:
# 'c'", and it reads like a broken manifest rather than a toolchain fault. The
# release workflow pins Helm 3 for the same reason; this makes a local run use
# the same one, which is the property that lets a local package stand in for a
# released one at all (ADR-068 addendum 1).
#
# Skipped in CI, where the workflow has already installed the pinned Helm.
if [ -z "${GITHUB_ACTIONS:-}" ]; then
    # shellcheck source=scripts/dev/helm3.sh
    . "$(cd "$(dirname "$0")/../dev" && pwd)/helm3.sh"
    require_helm3 || {
        echo "publish: no Helm 3 available; packaging would differ from a release" >&2
        exit 1
    }
fi

rm -rf dist/charts dist/dist dist/pkg
mkdir -p dist/charts

# ── Package ─────────────────────────────────────────────────────────────────

published=0 refused=0 broken=0
for d in manifests/argocd/components/*/*.yaml; do
    rc=0
    ./scripts/package/component-chart.sh "$d" "$VERSION" dist/charts || rc=$?
    case "$rc" in
        0) published=$((published+1)) ;;
        # component-chart.sh exits 3 when a component's source reaches into a
        # generated/ directory. That is per-cluster instance data, and publishing
        # it would hand every cluster one cluster's values, so the build fails
        # rather than shipping it.
        3) refused=$((refused+1)) ;;
        # Anything else is a component that did not build. This used to be
        # counted as neither published nor refused, so the step reported success
        # having silently dropped it: the distribution shipped without a
        # component the boundaries enable, and the first thing to notice was the
        # release gate two steps later -- or nothing at all, for a component no
        # boundary happens to enable.
        *) err "$d failed to package (exit $rc)"; broken=$((broken+1)) ;;
    esac
done
echo "packaged=$published refused=$refused broken=$broken"
[ "$refused" -eq 0 ] || { err "$refused component(s) carry per-cluster data and cannot be published"; exit 1; }
[ "$broken" -eq 0 ]  || { err "$broken component(s) failed to build; the distribution would be incomplete"; exit 1; }

# Boundary 02's components are provider overlays, not descriptors (ADR-061), and
# the bundle chart carries the boundaries themselves plus the descriptors copied
# in at package time. Without these a released cluster reaches back into this
# repository for content it should have received in the bundle.
./scripts/package/provider-charts.sh            "$VERSION" dist/charts
./scripts/package/tenant-and-catalog-charts.sh  "$VERSION" dist/charts
./scripts/package/inline-charts.sh              "$VERSION" dist/charts

# ADR-063: one published artefact. The per-component charts above are internal
# structure and are folded in here; only the distribution and the bundle are
# pushed.
python3 scripts/package/distribution.py "$VERSION" dist/charts dist/dist
python3 scripts/package/verify-distribution.py dist/dist/platform dist/charts
# Carry the tenant charts across the swap below, DERIVED from the same glob that
# packaged them rather than named here.
#
# This was `cp -R dist/charts/universal-tenant dist/charts/tenant-public-tls`.
# tenant-and-catalog-charts.sh packages every directory under
# manifests/tenants/charts/, so a third chart was built at line 138 and then
# deleted by the `rm -rf dist/charts` below -- silently, because the copy named
# the two it knew about and cannot notice a third. The release gate caught it
# ("names chart 'platform-spoke-gateway', which this release does not publish"),
# which is the gate working; the packaging step should not have needed it.
for src in manifests/tenants/charts/*/; do
    cp -R "dist/charts/$(basename "$src")" dist/dist/
done
rm -rf dist/charts && mv dist/dist dist/charts
./scripts/package/bundle-chart.sh               "$VERSION" dist/charts

# ── Gate ────────────────────────────────────────────────────────────────────

# A release invariant, not a report. ADR-063 makes mirroring a condition of a
# supported runtime, so a bundle that reaches back into this repository cannot be
# mirrored -- and shipping it would make the custody claim false for every tenant
# that received it.
./scripts/validate/bundle-portability.sh "$VERSION"

# The gate on the conversion, not a convenience: a published chart that renders
# different objects is a silent change to every cluster that pulls it.
for d in manifests/argocd/components/*/*.yaml; do
    app=$(grep -m1 '^appName:' "$d" | sed 's/appName: *//')
    [ -d "dist/charts/$app" ] || continue
    ./scripts/package/verify-equivalence.py "$d" "dist/charts/$app"
done

# Rendering is not deploying. Three defects reached published releases through
# that gap -- Applications naming charts no release published, a chart carrying
# one cluster's Infisical project IDs, and an Application naming a Helm chart and
# a Kustomize block together, which ArgoCD refuses outright. Each rendered
# perfectly.
#
# The combinations are READ from supportedMatrix, not listed here. They were
# listed -- "dev hybrid", "stg hybrid", "prod hetzner" -- and the list is a copy
# of a decision made elsewhere, so it went stale the moment the matrix changed:
# moving stg from hybrid to hetzner left this loop rendering stg+hybrid, which
# the chart now refuses by design, and the publish failed on its own gate having
# found exactly what it was supposed to find. A gate that has to be edited in
# lockstep with the thing it checks is a second source of truth.
#
# Each combination is rendered twice: as a box gets it by default, and with every
# optional component on. Only an Application that renders is checked, so a
# component off by default was never checked at all: the cluster autoscaler's
# element named a chart no release carried, and every release passed, because no
# default box has a burst pool. The switches are READ from values.yaml, every
# boolean `enabled`, for the reason above. burstSpokePool names a workload cluster
# rather than switching, so it has no switch to find and is set here.
every_optional="--set burstSpokePool=example-burst $(python3 -c "
import yaml
def walk(o, p):
    for k, v in (o.items() if isinstance(o, dict) else ()):
        if k == 'enabled' and isinstance(v, bool):
            yield f'--set {p}enabled=true'
        yield from walk(v, f'{p}{k}.')
print(' '.join(walk(yaml.safe_load(open('manifests/argocd/environment-manager/values.yaml')), '')))
")"
while read -r env provider; do
    [ -n "$env" ] || continue
    for optional in "" "$every_optional"; do
        echo "  $env+$provider${optional:+, every optional component on}"
        # shellcheck disable=SC2086 # $optional is a list of --set flags
        helm template platform-bundle dist/charts/platform-bundle \
            --set environmentSlug="$env" --set boxId=example-box --set provider="$provider" \
            --set management.infisicalEnvironmentSlug=management \
            --set publicTlsIssuer=letsencrypt-prod --set hubIngressAddress=127.0.0.1 \
            --set instanceRepoURL=https://github.com/example-org/example-gitops \
            --set hubDomain=dev.example.test \
            --set previewDomain=example-preview.test \
            --set 'alerting.emailTo[0]=alerts@example.test' \
            --set bundleVersion="$VERSION" $optional \
        | python3 scripts/validate/application-specs.py dist/charts
    done
done <<EOF
$(python3 -c "
import yaml
m = yaml.safe_load(open('manifests/argocd/environment-manager/values.yaml')).get('supportedMatrix') or {}
print('\n'.join(f'{e} {p}' for e, ps in m.items() for p in ps))
")
EOF

# Asserts the whole support matrix, both halves. Every environment crossed with
# every provider: a supported combination must render and must name only charts
# this release publishes; an unsupported one must be refused by name.
./scripts/validate/support-matrix.sh dist/charts/platform-bundle "$VERSION" dist/charts

# ── Inventory ───────────────────────────────────────────────────────────────

# What this version installs, where, and at what version, written into the bundle
# chart itself. It travels with the chart rather than with a GitHub release
# because a prerelease has no release, and a tenant mirroring the bundle should
# not need anything the bundle does not contain. Every artefact must be in
# manifests/architecture/component-catalog.yaml or the release stops here.
#
# PREVIOUS_VERSION names the version this one is compared against. Its inventory
# is read from its own published chart; a version published before inventories
# existed carries none, and is said to rather than compared against nothing.
# dist/_inventory-cache, with the underscore. The scraper writes UPSTREAM GO
# SOURCE here -- kubeadm's constants, fetched to read the etcd version -- and
# dist/ is gitignored but Go walks the filesystem, not git. Under any name Go
# would compile, `go mod tidy` takes that file for part of this module: it added
# k8s.io/kubernetes and k8s.io/component-base as DIRECT requirements and raised
# some forty transitive versions through MVS, including the test-only ginkgo and
# gomega, whose full hashes it then did not write because no test was built. The
# next `go vet ./...` failed on the missing go.sum entry. Go ignores a directory
# whose name begins with `_` outright, which is the only part of this that
# cannot be forgotten.
inventory_args=(--expand dist/_inventory-cache)
if [ -n "${PREVIOUS_VERSION:-}" ]; then
    rm -rf dist/previous && mkdir -p dist/previous
    helm pull "oci://ghcr.io/${OWNER}/charts/environment-manager" --version "$PREVIOUS_VERSION" \
        --untar -d dist/previous >/dev/null
    inventory_args+=(--previous-version "$PREVIOUS_VERSION")
    if [ -f dist/previous/environment-manager/components.json ]; then
        inventory_args+=(--previous dist/previous/environment-manager/components.json)
    else
        echo "inventory: ${PREVIOUS_VERSION} was published before inventories; nothing to compare against"
    fi
fi
python3 scripts/package/inventory.py "$VERSION" dist/charts \
    dist/charts/platform-bundle/components.json "${inventory_args[@]}"

# ── Push ────────────────────────────────────────────────────────────────────

for c in dist/charts/*/; do
    helm package "$c" -d dist/pkg >/dev/null
done

if [ -n "$SKIP_PUSH" ]; then
    echo "packaged and gated ${VERSION}; not pushed (SKIP_PUSH). ${VERSION} is still free."
    exit 0
fi

# ADR-063: a version is consumed by any release that begins publishing it, so
# re-running one that already put charts in the registry would re-point tags that
# already resolve. Publishing is not atomic -- charts push one at a time -- so a
# failure part-way leaves exactly this state, and the next attempt must take the
# next version.
#
# Every chart is probed, not just one: which charts a partial run left behind
# depends on where it stopped, and the set changes between releases, so there is
# no single name whose absence proves the version is free.
existing=""
for pkg in dist/pkg/*.tgz; do
    name=$(basename "$pkg" "-${VERSION}.tgz")
    ref="oci://ghcr.io/${OWNER}/charts/${name}"
    if err_out=$(helm show chart "$ref" --version "$VERSION" 2>&1 >/dev/null); then
        existing="${existing}  ${name}"$'\n'
    elif ! grep -qiE ': not found$|manifest unknown|NAME_UNKNOWN' <<<"$err_out"; then
        # Anything other than a clean absence is unknown, not free. Treating a
        # network or auth failure as "nothing is there" would let a re-publish
        # through at exactly the moment the check stopped working.
        err "could not determine whether ${name} is already published: ${err_out}"
        exit 1
    fi
done
if [ -n "$existing" ]; then
    err "version ${VERSION} is already published; a version is consumed by any release that begins publishing it (ADR-063). Release the next version instead."
    printf 'already in the registry at this version:\n%s' "$existing"
    exit 1
fi

for pkg in dist/pkg/*.tgz; do
    helm push "$pkg" "oci://ghcr.io/${OWNER}/charts"
done

echo "published ${VERSION} to oci://ghcr.io/${OWNER}/charts"
