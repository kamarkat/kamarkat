#!/usr/bin/env bash
# Build the CLI a release publishes -- and the one a local test runs.
#
# ADR-068: the build declares the version, and a binary that declares anything
# other than `development` reads its platform content from the embedded tree
# rather than from a working directory. That single fact is what separates the
# released path from the development one, so the two must not be produced by two
# recipes: this script is the recipe, and both the release workflow and the
# Makefile call it.
#
# Running it with a prerelease version (0.1.16-rc.1) is how the platform exercises
# the released path without consuming the release -- ADR-063 consumes a version by
# publishing it, and a prerelease is a distinct version, so 0.1.16 stays free.
set -euo pipefail

VERSION="${1:?usage: build-cli.sh <version> [outdir]}"
OUTDIR="${2:-dist/bin}"

# Helm requires semver and so does the chart this binary asks for by version. A
# build that named something else would produce a CLI whose request no registry
# can answer, and the failure would surface at a tenant's Day-0 rather than here.
if ! printf '%s' "$VERSION" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$'; then
    echo "build-cli: '$VERSION' is not semver; a chart version must be requestable" >&2
    exit 1
fi

# Four targets for a release, one for a laptop. TARGETS=host keeps the local
# iteration loop to a single compile; the default is what a release publishes.
case "${TARGETS:-all}" in
    host) targets="$(go env GOOS)/$(go env GOARCH)" ;;
    all)  targets="linux/amd64 linux/arm64 darwin/amd64 darwin/arm64" ;;
    *)    targets="${TARGETS}" ;;
esac

# ADR-063: the binary carries the platform content Day-0 reads, so a tenant needs
# no checkout of this repository. Re-copied on every build rather than trusted:
# the checked-in tree is generated content, and building from a stale copy is the
# one failure this script is positioned to prevent.
./scripts/package/embed-platform-assets.sh

pkg=github.com/kamarkat/kamarkat/internal/kakt-cli/versions
mkdir -p "$OUTDIR"

for t in $targets; do
    os="${t%%/*}" arch="${t##*/}"
    CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath \
        -ldflags "-s -w -X ${pkg}.BundleVersion=${VERSION}" \
        -o "$OUTDIR/kakt-${os}-${arch}" ./cmd/kakt
    # Re-sign on macOS. Go writes an ad-hoc signature, and REPLACING the binary
    # in place -- which the local loop does on every rebuild -- invalidates it.
    # The kernel then kills the process with "Killed: 9" and no other
    # diagnostic, which reads like a corrupt build rather than a signature.
    # Only darwin enforces this and codesign exists nowhere else.
    if [ "$os" = darwin ] && command -v codesign >/dev/null 2>&1; then
        codesign --force -s - "$OUTDIR/kakt-${os}-${arch}" >/dev/null 2>&1 || true
    fi
    echo "built $OUTDIR/kakt-${os}-${arch}"
done

# The binary must request the version that was published. A build that reported
# "development" here would silently source from a working tree on every cluster
# it bootstrapped -- which is precisely the defect the released path exists to
# remove, and it is invisible in the binary's own output until then.
#
# Asked through `go run` rather than by executing a built artifact, because most
# of the targets above do not run on the machine that built them.
got=$(go run -ldflags "-X ${pkg}.BundleVersion=${VERSION}" \
      ./cmd/kakt bundle-version 2>/dev/null || true)
if [ "$got" != "$VERSION" ]; then
    echo "build-cli: built CLI reports '${got}', expected '${VERSION}'" >&2
    exit 1
fi

echo "CLI declares ${VERSION}; platform content read from the embedded tree"
