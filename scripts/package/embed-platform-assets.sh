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
    # The tenant template a box is rendered from (tenant/render_box.go).
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
    # The issuer CRD Day-0 keeps current (bootstrap/vendored_crds.go).
    #
    # Carried because the repair has to work on a released box: ArgoCD cannot
    # sync an Application whose CRD schema change and whose resources using the
    # new fields arrive together, so Day-0 applies this definition directly
    # before that boundary. A binary without it can read the stale schema and do
    # nothing about it.
    "manifests/hub-core-services/security/files/infisical-issuer-crd.yaml"
)

# THE RUNTIME SCRIPT CLOSURE, computed rather than listed.
#
# Day-0's orchestration is shell, and a released binary has to carry it or fetch
# it. Fetching is what the tenant workflow did -- a second checkout of this
# repository at the release tag -- while the step above it claimed "the binary
# carries the platform content Day-0 needs (ADR-063), so nothing here checks out
# the platform's repository". Carrying it makes that true, and makes "the scripts
# belong to this release" true by construction rather than by a matching ref.
#
# Derived, because a hand-written list goes stale on the next validator and the
# failure is silent: validate-all.sh SKIPS a suite whose script is absent, so a
# release missing one reports success with less coverage than it claims.
# scripts/validate/runtime-script-closure.py computes it from the two entry
# points the tenant CI invokes, and gates on this copy carrying all of it.
# A read loop, not `mapfile`: this runs on the operator's machine as well as in
# CI, and macOS ships bash 3.2, which has no mapfile.
SCRIPT_CLOSURE=()
while IFS= read -r line; do
    [[ -n "$line" ]] && SCRIPT_CLOSURE+=("$line")
done < <(python3 scripts/validate/runtime-script-closure.py --list)
if [[ ${#SCRIPT_CLOSURE[@]} -eq 0 ]]; then
    echo "embed-platform-assets: the runtime script closure came back empty; refusing to" >&2
    echo "  embed a release that carries no Day-0 orchestration" >&2
    exit 1
fi
PATHS+=("${SCRIPT_CLOSURE[@]}")

# ONLY THE GENERATED SUBTREES. `rm -rf "$DEST"` deleted the whole package
# directory, so every Go file in it -- the materializer and its tests -- was
# destroyed by `make build`, which runs this. The package legitimately holds
# source beside the generated tree, because the code that extracts the embedded
# content is the code that owns the FS.
rm -rf "$DEST/manifests" "$DEST/scripts"
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
// GENERATED by scripts/package/embed-platform-assets.sh, including this file.
// The copied tree is git-ignored: committing it made it a second place a
// manifest could be edited and carried every script twice. Change the source
// under manifests/ or scripts/; the copy follows at build time.
//
// A clean checkout therefore has no copy and `go build` alone cannot satisfy
// the directive below. `make build`, `make test` and
// scripts/package/build-cli.sh regenerate first.
package embedded

import "embed"

// FS holds the copied tree, addressed by repository-relative path so a caller
// cannot tell an embedded read from a working-tree one.
//
// scripts/ is here because Day-0's orchestration is shell and a released binary
// has to own it rather than fetch it: the tenant workflow used to take a second
// checkout of this repository at the release tag, which made "the scripts belong
// to this release" a property of a matching ref instead of of the artefact.
//
// PERMISSIONS: go:embed records no file mode and embed.FS reports everything
// read-only. Anything that materializes a script from here sets the executable
// bit itself; a caller that copies the mode out of the FS writes a file nothing
// can run.
//
//go:embed all:manifests all:scripts
var FS embed.FS
GOEOF

files=$(find "$DEST" -type f -not -name embed.go | wc -l | tr -d ' ')
echo "embedded $files file(s) into $DEST"
