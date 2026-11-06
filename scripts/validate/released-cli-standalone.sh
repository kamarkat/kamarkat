#!/usr/bin/env bash
# A released CLI must bootstrap from its binary alone.
#
# ADR-063 requires a release to carry a build a tenant can run. Bootstrap read
# platform manifests from the working directory, so the published binary worked
# only inside a checkout of zero-ops -- a hidden dependency on the vendor's
# repository, in the one artefact a tenant runs before it has anything else.
#
# The check runs the released binary from a directory that is not a checkout. A
# development build is expected to fail this and is not run: it reads the working
# tree by design (ADR-068), and a build that passed here in development mode
# would be one that had stopped reading the tree at all.
set -euo pipefail

VERSION="${1:?usage: released-cli-standalone.sh <version> [binary]}"
BIN="${2:-dist/bin/kakt-$(go env GOOS)-$(go env GOARCH)}"

if [[ ! -x "$BIN" ]]; then
    echo "released-cli-standalone: $BIN is not executable" >&2
    exit 1
fi
BIN="$(cd "$(dirname "$BIN")" && pwd)/$(basename "$BIN")"

reported=$("$BIN" bundle-version 2>/dev/null || true)
if [[ "$reported" != "$VERSION" ]]; then
    echo "released-cli-standalone: binary reports '${reported}', expected '${VERSION}'" >&2
    exit 1
fi

# Somewhere with no manifests/ and no .git. If the binary reaches for either, it
# finds nothing, which is the tenant's situation exactly.
sandbox="$(mktemp -d)"
trap 'rm -rf "$sandbox"' EXIT

# --dry-run renders and writes nothing, so this asks the one question that
# matters -- can the binary resolve the platform content Day-0 needs -- without
# provisioning anything or needing a cloud credential.
if ! out=$(cd "$sandbox" && "$BIN" tenant scaffold --dry-run \
        --tenant probe --org probe-org --domain probe.example \
        --cluster probe-hub --provider hetzner \
        --bundle-version "$VERSION" --out "$sandbox/render" 2>&1); then
    echo "released-cli-standalone: scaffolding failed outside a checkout, so the" >&2
    echo "release does not carry the content it needs (ADR-063):" >&2
    sed 's/^/  /' <<<"$out" >&2
    exit 1
fi

if [[ ! -f "$sandbox/render/registry/clusters/probe-hub/bundle.yaml" ]]; then
    echo "released-cli-standalone: scaffolding reported success but rendered no" >&2
    echo "cluster; a silent empty render is the failure this check exists for" >&2
    exit 1
fi

if grep -q "<[A-Z_]*>" "$sandbox/render/registry/clusters/probe-hub/bundle.yaml"; then
    echo "released-cli-standalone: the rendered cluster still carries tokens:" >&2
    grep -o "<[A-Z_]*>" "$sandbox/render/registry/clusters/probe-hub/bundle.yaml" | sort -u | sed 's/^/  /' >&2
    exit 1
fi

echo "released CLI: bootstraps standalone at ${VERSION}, no checkout required"
