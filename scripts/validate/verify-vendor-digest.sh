#!/usr/bin/env bash
set -euo pipefail

# Verify that the SHA-256 digest in the provenance header matches
# the actual rendered content. Catches:
#   - Manual edits to vendored manifests without re-rendering
#   - Tampered artifacts
#   - Renovate PRs that bump the version but don't re-vendor
#
# The last of those is completed rather than merely refused:
# .github/workflows/revendor-kyverno.yml re-renders on Renovate's branch and
# commits, so a proposal has something to pass. Refusing without that left a
# pull request that could only ever fail, which teaches people to merge past the
# check rather than to trust it.
#
# Usage: bash scripts/validate/verify-vendor-digest.sh

SPOKE_DIR="manifests/spoke/spoke-catalog/infra/kyverno"
CNPG_DIR="manifests/spoke/spoke-catalog/infra/cloudnative-pg"
ESO_DIR="manifests/spoke/spoke-catalog/infra/external-secrets"
RELOADER="manifests/spoke/spoke-catalog/infra/reloader/controller.yaml"
FAIL=0

for file in "$SPOKE_DIR"/crd.yaml "$SPOKE_DIR"/controller.yaml \
            "$CNPG_DIR"/crd.yaml "$CNPG_DIR"/controller.yaml \
            "$ESO_DIR"/crd.yaml "$ESO_DIR"/controller.yaml \
            "$RELOADER"; do
  if [ ! -f "$file" ]; then
    echo "ERROR: $file not found"
    FAIL=1
    continue
  fi

  # `|| true` matters: under `set -euo pipefail` a grep that matches nothing
  # kills the script here, before the branch below can say why. A gate that
  # dies without a message is indistinguishable from an infrastructure fault.
  RECORDED_HASH=$(grep '^# sha256:' "$file" | head -1 | sed 's/^# sha256: //' || true)
  if [ -z "$RECORDED_HASH" ]; then
    echo "ERROR: No sha256 digest found in $file"
    FAIL=1
    continue
  fi

  # Hash everything except the provenance header lines (# comments).
  # scripts/renovate/revendor-kyverno.sh writes the digest under this same
  # rule; the two must agree or every re-vendor produces a failing gate.
  ACTUAL_HASH=$(grep -v '^# ' "$file" | sha256sum | cut -d' ' -f1)

  if [ "$RECORDED_HASH" = "$ACTUAL_HASH" ]; then
    echo "OK: $file digest matches"
  else
    echo "FAIL: $file digest mismatch (recorded=$RECORDED_HASH actual=$ACTUAL_HASH)"
    FAIL=1
  fi
done

# ── the two places kyverno's version is declared must agree ──────────────
#
# ONE VERSION PER COMPONENT, EVERYWHERE IT RUNS. The management cluster pulls
# the kyverno chart by targetRevision; the workload clusters apply the vendored
# render of that same chart. Nothing connected the two, so the hub could be
# moved to a new chart while every spoke kept applying the old render -- the
# release would report kyverno twice at two versions with nobody having decided
# that, which is the split the upgrade policy exists to prevent.
#
# Read from the provenance header rather than from a second list, so there is no
# third place to keep in step.
APPSET="manifests/argocd/environment-manager/templates/01-platform-infra-appset.yaml"
VENDORED=$(grep -m1 '^# Vendored from helm chart kyverno v' "$SPOKE_DIR/controller.yaml" \
  | sed 's/.*kyverno v//;s/ .*//' || true)
# The element's targetRevision is tens of lines below its repoURL, so read from
# the repoURL to the first targetRevision after it.
PINNED=$(awk "/repoURL: 'https:\/\/kyverno.github.io\/kyverno'/,0" "$APPSET" \
  | grep -m1 targetRevision | sed "s/.*targetRevision: '//;s/'.*//" || true)

if [ -z "$VENDORED" ] || [ -z "$PINNED" ]; then
  echo "FAIL: could not read kyverno's version from both places (vendored='$VENDORED' pinned='$PINNED')"
  FAIL=1
elif [ "$VENDORED" != "$PINNED" ]; then
  echo "FAIL: KYVERNO VERSION SPLIT — the management cluster pulls chart $PINNED and the"
  echo "      spokes apply a render of chart $VENDORED. Move both together:"
  echo "      bash scripts/renovate/revendor-kyverno.sh $PINNED"
  FAIL=1
else
  echo "OK: kyverno chart $PINNED is the version vendored for spokes"
fi

# ── the same contract for CloudNativePG ──────────────────────────────────
#
# Its hub Application pulls the chart; the spokes apply a vendored render of it.
# Until now neither half was checked: the render carried no digest and nothing
# compared the two versions, so a chart bump on the hub would have left every
# spoke on the old operator with nothing reporting it.
CNPG_APP="manifests/hub-core-services/cloudnative-pg/application.yaml"
CNPG_VENDORED=$(grep -m1 '^# Vendored from helm chart cloudnative-pg v' "$CNPG_DIR/controller.yaml" \
  | sed 's/.*cloudnative-pg v//;s/ .*//' || true)
CNPG_PINNED=$(grep -m1 'targetRevision:' "$CNPG_APP" \
  | sed 's/.*targetRevision:[[:space:]]*//' || true)

if [ -z "$CNPG_VENDORED" ] || [ -z "$CNPG_PINNED" ]; then
  echo "FAIL: could not read cloudnative-pg's version from both places (vendored='$CNPG_VENDORED' pinned='$CNPG_PINNED')"
  FAIL=1
elif [ "$CNPG_VENDORED" != "$CNPG_PINNED" ]; then
  echo "FAIL: CLOUDNATIVE-PG VERSION SPLIT — the management cluster pulls chart $CNPG_PINNED and"
  echo "      the spokes apply a render of chart $CNPG_VENDORED. Move both together:"
  echo "      bash scripts/renovate/revendor-cnpg.sh $CNPG_PINNED"
  FAIL=1
else
  echo "OK: cloudnative-pg chart $CNPG_PINNED is the version vendored for spokes"
fi

# ── and for External Secrets, whose hub Application pulls the same chart ─────
ESO_VENDORED=$(grep -m1 '^# Vendored from helm chart external-secrets v' "$ESO_DIR/controller.yaml" \
  | sed 's/.*external-secrets v//;s/ .*//' || true)
ESO_PINNED=$(awk "/repoURL: 'https:\/\/charts.external-secrets.io'/,0" "$APPSET" \
  | grep -m1 targetRevision | sed "s/.*targetRevision: '//;s/'.*//" || true)
if [ -z "$ESO_VENDORED" ] || [ -z "$ESO_PINNED" ]; then
  echo "FAIL: could not read external-secrets' version from both places (vendored='$ESO_VENDORED' pinned='$ESO_PINNED')"
  FAIL=1
elif [ "$ESO_VENDORED" != "$ESO_PINNED" ]; then
  echo "FAIL: EXTERNAL-SECRETS VERSION SPLIT — the management cluster pulls chart $ESO_PINNED"
  echo "      and the spokes apply a render of chart $ESO_VENDORED. Move both together:"
  echo "      bash scripts/renovate/revendor-eso.sh $ESO_PINNED"
  FAIL=1
else
  echo "OK: external-secrets chart $ESO_PINNED is the version vendored for spokes"
fi

# ── and for Reloader, whose hub pin is a COMPONENT DESCRIPTOR ───────────────
#
# Not the appset. The hub installs Reloader as an Argo component, so its chart
# version lives in manifests/argocd/components/01/, and a parity check that only
# knew about the appset missed it: the spoke render was moved to 2.2.20 while the
# hub stayed on 2.2.19, and the release's own reasons check caught it rather than
# this gate (2026-11-14).
RELOADER_COMPONENT="manifests/argocd/components/01/stakater-reloader.yaml"
RELOADER_VENDORED=$(grep -m1 '^# Source:.*chart reloader ' "$RELOADER" \
  | sed 's/.*chart reloader //;s/,.*//' || true)
RELOADER_PINNED=$(grep -m1 '^targetRevision:' "$RELOADER_COMPONENT" \
  | sed 's/^targetRevision:[[:space:]]*//' || true)
if [ -z "$RELOADER_VENDORED" ] || [ -z "$RELOADER_PINNED" ]; then
  echo "FAIL: could not read reloader's version from both places (vendored='$RELOADER_VENDORED' pinned='$RELOADER_PINNED')"
  FAIL=1
elif [ "$RELOADER_VENDORED" != "$RELOADER_PINNED" ]; then
  echo "FAIL: RELOADER VERSION SPLIT — the management cluster pulls chart $RELOADER_PINNED and"
  echo "      the spokes apply a render of chart $RELOADER_VENDORED. Move both together:"
  echo "      bash scripts/renovate/revendor-reloader.sh $RELOADER_PINNED"
  FAIL=1
else
  echo "OK: reloader chart $RELOADER_PINNED is the version vendored for spokes"
fi

if [ "$FAIL" -ne 0 ]; then
  echo ""
  echo "Vendored manifests have been modified without re-rendering."
  echo "Run: bash scripts/renovate/revendor-kyverno.sh"
  exit 1
fi

echo "All vendor digests verified."
