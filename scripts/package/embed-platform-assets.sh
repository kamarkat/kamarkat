#!/usr/bin/env bash
# Copy the platform content a released CLI must carry into the embed package.
#
# ADR-063: a release carries a build a tenant can run, and bootstrap reads
# platform manifests. go:embed cannot reach outside its own package directory, so
# the content is copied here rather than referenced.
#
# The set is deliberately narrow. Embedding manifests/ wholesale would put a
# tenant's binary in the business of carrying every provider overlay, every
# component descriptor and every environment the platform has ever supported --
# most of which Day-0 never reads, and all of which would then need to stay
# correct in two places.
#
# Run by the release workflow, and by the pre-commit hook that verifies the copy
# has not drifted from its source.
set -euo pipefail

DEST="${1:-internal/platform/embedded}"

# Each entry is a path relative to the repository root, copied verbatim.
# Anything Day-0 reads belongs here; nothing else does.
PATHS=(
    # The HubEnvironment and its per-environment patch (hubdomain.go).
    "manifests/environments"
    # Cilium addon manifests and the config base, per provider (provider_cloud.go).
    "manifests/providers"
    # The ADR-045 artifact registry (orchestrator.go).
    "manifests/generated/artifacts.yaml"
    # Component descriptors, read to know what a boundary must generate
    # (health/boundary_inventory.go).
    "manifests/argocd/components"
    # The tenant scaffolding template (tenant/scaffold.go).
    "manifests/tenants/gitops-template"
    # The Cilium workloads a hetzner hub installs (provider_cloud.go, via
    # HetznerDriver.CiliumAddonPath). One file rather than the directory: the
    # rest of spoke-bootstrap is delivered to spokes by ClusterResourceSet and is
    # never read by the CLI, and the directory is 1.7M against this file's 0.9M.
    "manifests/spoke/spoke-bootstrap/cilium-addon-template.yaml"
    # The Support Agent's collection contract (cmd/kakt/support.go).
    #
    # `kakt support scope` and `kakt support preview` exist so a tenant can
    # read what would be sent BEFORE it is sent (ADR-077), and a released binary
    # could not: it is the only file those commands read, and it was not carried.
    # The commands failed with "release 0.1.16-rc.6 carries no
    # manifests/hub-core-services/support-agent/allowlist.yaml" -- naming a file
    # the repository plainly has, which is exactly the failure mode this embed
    # set exists to prevent.
    "manifests/hub-core-services/support-agent/allowlist.yaml"
)

rm -rf "$DEST"
mkdir -p "$DEST"

for p in "${PATHS[@]}"; do
    if [[ ! -e "$p" ]]; then
        echo "embed-platform-assets: $p does not exist; the embed set names content the repository does not have" >&2
        exit 1
    fi
    mkdir -p "$DEST/$(dirname "$p")"
    cp -R "$p" "$DEST/$(dirname "$p")/"
done

# Generated per-cluster artifacts are instance data (ADR-045). A released binary
# carrying one cluster's would hand it to every cluster, which is the defect that
# published this hub's Infisical project IDs in 0.1.2.
find "$DEST" -type d -name generated -not -path "*/manifests/generated" -exec rm -rf {} + 2>/dev/null || true

cat > "$DEST/embed.go" <<'GOEOF'
// Package embedded carries the platform content a released CLI needs at Day-0.
//
// The tree below is copied from the repository by
// scripts/package/embed-platform-assets.sh and verified by a pre-commit hook, so
// it is generated content rather than a second place to edit a manifest. Change
// the manifest; the copy follows.
package embedded

import "embed"

// FS holds the copied tree, addressed by repository-relative path so a caller
// cannot tell an embedded read from a working-tree one.
//
//go:embed all:manifests
var FS embed.FS
GOEOF

files=$(find "$DEST" -type f -not -name embed.go | wc -l | tr -d ' ')
echo "embedded $files file(s) into $DEST"
