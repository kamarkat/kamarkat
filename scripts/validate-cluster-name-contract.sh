#!/usr/bin/env bash
# Validate the cluster label contract that binds spokes to fleet Applications.
#
# Four links carry a spoke's identity from its SpokePool claim to the
# ApplicationSets that place tenants on it:
#
#   1. the SpokePool composition patches the label onto the CAPI Cluster, from
#      the claim name
#   2. the same composition sets take-along-label.capi-to-argocd.<label>, which
#      is what tells capi2argo to copy the label onward
#   3. capi2argo writes it to the ArgoCD cluster Secret        (runtime -- see the
#      Kyverno policy in manifests/hub-core-services/security/)
#   4. the fleet ApplicationSets select on cluster-name with a clusters
#      generator matchLabels
#
# The label is CLUSTER-NAME. It was `cell-id`, which the compositions have
# always patched from the claim's own name -- so placement was already by cluster
# identity, under a key borrowed from policy grouping. A cell is the policy group
# of ADR-064, where a cluster may belong to several and receives the union of
# their policy; it never named one cluster, and `cells` in a cluster's values.yaml
# is where membership is declared.
#
# cell-id is gone: from the compositions, their take-along markers, the
# ClusterResourceSet selector that matched it, the AINativeSaaS and tenant-db
# XRD fields, and the claims the chart renders. No alias remains, so a manifest
# still naming it selects nothing and this script says which one.
#
# Break any link and the clusters generator matches nothing. The result is not an
# error: the ApplicationSet generates zero Applications and reports Healthy doing
# it, so every tenant on every affected spoke silently stops being reconciled.
# That is the same false-green shape as an ApplicationSet whose generator returns
# an empty list, and nothing in ArgoCD distinguishes it from "there are no
# tenants yet".
#
# This script covers links 1, 2 and 4 -- everything that lives in Git. Link 3 is
# runtime and belongs to the Kyverno policy; the "generated nothing" symptom
# regardless of cause belongs to the ArgoCDApplicationSetGeneratesNothing alert.
#
# Depends on: yq (mikefarah), standard POSIX tools.

set -euo pipefail

# A commit started from an editor or GUI gets a minimal PATH and would not
# find these tools even when installed. See scripts/lib/tool-path.sh.
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib/tool-path.sh"


repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# The cluster's identity, and what placement selects on. The one label key the
# whole chain agrees on: everything below is checked against this rather than
# against each other, so a rename has exactly one correct edit and this script
# names the rest.
readonly CLUSTER_LABEL="cluster-name"
readonly PUBLISHED_LABELS="$CLUSTER_LABEL"
readonly TAKE_ALONG_PREFIX="take-along-label.capi-to-argocd"

compositions=(
  "$repo_root/manifests/providers/hetzner/k8s/spokepool-hetzner-composition.yaml"
  "$repo_root/manifests/providers/hybrid/k8s/spokepool-hybrid-composition.yaml"
)

appset_dir="$repo_root/manifests/argocd/environment-manager/templates"

fail_count=0
pass() { echo "  ✅ $1"; }
fail() { echo "  ❌ $1"; fail_count=$((fail_count + 1)); }

command -v yq >/dev/null 2>&1 || { echo "❌ required tool not found: yq"; exit 1; }

echo "Validating the ${CLUSTER_LABEL} contract..."

# ─── Links 1 & 2: every SpokePool composition labels its CAPI Cluster ─────────
for comp in "${compositions[@]}"; do
  name="$(basename "$comp")"

  if [[ ! -f "$comp" ]]; then
    fail "$name: composition not found"
    continue
  fi

  for label in $PUBLISHED_LABELS; do
    # Link 1: a patch must write the label from the claim name. Without it the
    # Cluster carries the take-along marker for a label that does not exist.
    if grep -q "toFieldPath: spec.forProvider.manifest.metadata.labels.${label}\b" "$comp"; then
      pass "$name: patches metadata.labels.${label}"
    else
      fail "$name: no patch writes metadata.labels.${label} -- spokes from this composition will never match a fleet clusters generator"
    fi

    # Link 2: the take-along marker. Present without link 1 is useless; absent
    # with link 1 means the label exists on the Cluster but never reaches ArgoCD.
    if grep -q "${TAKE_ALONG_PREFIX}.${label}:" "$comp"; then
      pass "$name: sets ${TAKE_ALONG_PREFIX}.${label}"
    else
      fail "$name: missing ${TAKE_ALONG_PREFIX}.${label} -- capi2argo will not copy the label to the ArgoCD cluster Secret"
    fi
  done
done

# ─── Link 4: every clusters generator selects on the same key ────────────────
# Read the RENDERED chart rather than the templates: the templates are Helm, not
# YAML, and a clusters generator can sit at any depth (these are inside matrix
# generators). Rendering lets yq walk the structure instead of guessing at
# indentation, and it checks what ArgoCD will actually be given.
command -v helm >/dev/null 2>&1 || { echo "❌ required tool not found: helm"; exit 1; }

rendered="$(mktemp)"
trap 'rm -f "$rendered"' EXIT

helm template cell-id-contract "$repo_root/manifests/argocd/environment-manager" \
  --set environmentSlug=dev --set boxId=example-box \
  --set management.infisicalEnvironmentSlug=management \
  --set provider=hybrid \
  --set instanceRepoURL=https://github.com/example-org/example-gitops \
  --set hubDomain=dev.example.test \
  --set previewDomain=example-preview.test \
  --set 'alerting.emailTo[0]=alerts@example.test' \
  --set topology=single \
  --set publicTlsIssuer=letsencrypt-prod \
  --set oidcIssuer=https://auth.example.invalid \
  >"$rendered"

# Selector keys are legitimately not all the same. platform-spoke-catalog
# targets every pool-type spoke and so selects on spoke-type; only the
# ApplicationSets that place a tenant on ONE specific cell select on cell-id.
# Requiring cell-id everywhere would be wrong, and requiring it somewhere is too
# weak -- it passes while a tenant ApplicationSet quietly stops using it. So:
# every key must be one the platform actually publishes, and every ApplicationSet
# that places tenants must select on this one.
readonly ALLOWED_SELECTOR_KEYS="$PUBLISHED_LABELS spoke-type"

pairs="$(yq '
  select(.kind == "ApplicationSet") as $a
  | $a.metadata.name as $n
  | [$a | .. | select(type == "!!map" and has("clusters"))]
  | .[]
  | (.clusters.selector.matchLabels // {} | keys | .[]) as $k
  | $n + " " + $k
' "$rendered" 2>/dev/null | grep -v '^---$' | grep -v '^\s*$' | sort -u || true)"

if [[ -z "$pairs" ]]; then
  fail "no clusters-generator matchLabels found in the rendered chart -- the fleet ApplicationSets place tenants by cluster label, so finding none means this check is looking in the wrong place"
else
  # 4a. No unknown selector key. A typo of cell-id and a deliberate new dimension
  #     are indistinguishable at runtime (both match nothing), so a new key has to
  #     be added here consciously.
  unknown=""
  while IFS=' ' read -r appset key; do
    [[ -z "$key" ]] && continue
    case " $ALLOWED_SELECTOR_KEYS " in
      *" $key "*) ;;
      *) unknown="$unknown $appset:$key" ;;
    esac
  done <<EOF
$pairs
EOF

  if [[ -z "$unknown" ]]; then
    pass "all clusters-generator keys are published labels (${ALLOWED_SELECTOR_KEYS// /, })"
  else
    fail "unknown clusters-generator selector key(s):${unknown} -- capi2argo publishes only ${ALLOWED_SELECTOR_KEYS// /, }, so these match nothing. Add the label to the compositions and this allowlist, or fix the typo"
  fi

  # 4b. Every tenant-placing ApplicationSet selects on cell-id.
  tenant_appsets="$(printf '%s\n' "$pairs" | awk '{print $1}' | grep -E 'tenant|fleet' | sort -u || true)"
  if [[ -z "$tenant_appsets" ]]; then
    fail "no tenant/fleet ApplicationSet has a clusters generator -- tenant placement is what this label exists for"
  else
    while IFS= read -r appset; do
      [[ -z "$appset" ]] && continue
      if printf '%s\n' "$pairs" | grep -qx "$appset $CLUSTER_LABEL"; then
        pass "$appset selects on ${CLUSTER_LABEL}"
      else
        got="$(printf '%s\n' "$pairs" | awk -v a="$appset" '$1==a {print $2}' | tr '\n' ' ')"
        fail "$appset places tenants but does not select on ${CLUSTER_LABEL} (selects on: ${got:-nothing}) -- it will generate zero Applications and report Healthy"
      fi
    done <<EOF
$tenant_appsets
EOF
  fi
fi

echo
if [[ "$fail_count" -gt 0 ]]; then
  echo "❌ ${CLUSTER_LABEL} contract: $fail_count check(s) failed"
  exit 1
fi
echo "✅ ${CLUSTER_LABEL} contract: all checks passed"
