#!/usr/bin/env bash
# Validate SpokePool composition ClusterResourceSet resource alignment.
#
# Crossplane function-patch-and-transform addresses resources positionally
# (spec.forProvider.manifest.spec.resources[N].name). Inserting or removing an
# element shifts every downstream index — which previously left two base
# entries with empty names and silently renamed others. This script is the
# regression guard against that class of positional drift.
#
# For each SpokePool composition it renders the cluster-resource-set
# resources[] array through its index-targeted patches (using a fixed test
# claim name) and asserts:
#   1. every rendered name is non-empty      (no masquerading empty base entry)
#   2. rendered names are unique             (no accidental duplicates)
#   3. static namespace ConfigMaps (argocd-namespace) are NOT per-spoke renamed
#   4. per-spoke resources (observability/messaging namespaces, argocd-agent-params,
#      bootstrap-cert) resolve to {claim}-<suffix> exactly once
#
# Depends on: yq (mikefarah), standard POSIX tools.

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# Compositions to validate — add new spokepool providers here.
compositions=(
  "$repo_root/manifests/providers/hybrid/k8s/spokepool-hybrid-composition.yaml"
  "$repo_root/manifests/providers/hetzner/spokepool/spokepool-hetzner-composition.yaml"
)

TEST_CLAIM="test-spokepool"

# Namespaces that must stay static (shared across spokes, never claim-prefixed).
static_names=(
  "argocd-namespace"
)

# Per-spoke resources that must render as {claim}-<suffix> exactly once.
# NB: dynamic names must NOT also appear as bare static entries.
dynamic_suffixes=(
  "hetzner-credentials"
  "observability-namespace"
  "argocd-agent-params"
  "bootstrap-cert"
  "agent-ca"
  # ADR-046 §24: rendered by hub-operator with the spoke's control-plane endpoint.
  # It is the ONLY owner of the spoke's cilium-config, so a missing or duplicated
  # slot here means either no CNI config reaches the spoke or two do.
  "cilium-config"
)

failures=0

fail() {
  echo "  ❌ $*"
  failures=$((failures + 1))
}

cr() {
  # Emit the cluster-resource-set input resource as a yq expression fragment.
  echo '.spec.pipeline[0].input.resources[] | select(.name == "cluster-resource-set")'
}

render_name() {
  # Given a composition file and a positional index, emit the name that index
  # renders to: a %s-... Format patch if present, otherwise the base literal.
  local file="$1" idx="$2"
  local sel
  sel="$(cr)"
  local patched
  patched="$(yq "$sel | .patches[] | select(.toFieldPath == \"spec.forProvider.manifest.spec.resources[$idx].name\") | [.transforms[] | select(.string.type == \"Format\") | .string.fmt][0] // \"\"" "$file" 2>/dev/null || echo "")"

  if [ "$patched" != "null" ] && [ -n "$patched" ]; then
    printf '%s\n' "${patched//%s/$TEST_CLAIM}"
    return 0
  fi

  yq "$sel | .base.spec.forProvider.manifest.spec.resources[$idx].name // \"\"" "$file" 2>/dev/null || echo ""
}

render_manifest_name() {
  # Given a composition file and a resource name, emit the name that resource's
  # manifest renders to (for matching composition-created CRS payloads against
  # the ClusterResourceSet resources[] list): a %s-... Format patch on
  # spec.forProvider.manifest.metadata.name if present, otherwise the base literal.
  local file="$1" rname="$2"
  local sel
  sel=".spec.pipeline[0].input.resources[] | select(.name == \"$rname\")"
  local patched
  patched="$(yq "$sel | .patches[] | select(.toFieldPath == \"spec.forProvider.manifest.metadata.name\") | [.transforms[] | select(.string.type == \"Format\") | .string.fmt][0] // \"\"" "$file" 2>/dev/null || echo "")"

  if [ "$patched" != "null" ] && [ -n "$patched" ]; then
    printf '%s\n' "${patched//%s/$TEST_CLAIM}"
    return 0
  fi

  yq "$sel | .base.spec.forProvider.manifest.metadata.name // \"\"" "$file" 2>/dev/null || echo ""
}

validate_payload_keys() {
  # CRS contract guard: every value in a resource-set Secret/ConfigMap must be an
  # applyable Kubernetes manifest (a YAML doc with apiVersion + kind). A value that
  # parses as a scalar — e.g. a bare configuration parameter such as claim-name or
  # cp-replicas — is rejected. This is the regression guard against the
  # Peek(32)/io.EOF class of bug where a short scalar crashed CAPI's CRS apply
  # with a bare "EOF" reconcile error.
  local file="$1" rname="$2" mname="$3"
  local sel sel_patches
  sel=".spec.pipeline[0].input.resources[] | select(.name == \"$rname\") | .base.spec.forProvider.manifest"
  sel_patches=".spec.pipeline[0].input.resources[] | select(.name == \"$rname\") | .patches"

  local keys key valfile ok=1
  keys="$(yq "$sel | (.stringData // .data) | keys[]" "$file" 2>/dev/null || echo "")"

  if [ -z "$keys" ]; then
    fail "CRS payload resource '$rname' ($mname) has no stringData/data keys"
    return
  fi

  while IFS= read -r key; do
    [ -z "$key" ] && continue
    # Values populated at runtime by a patch (e.g. argocd-agent-config.yaml
    # rendered by a FromCompositeFieldPath/CombineFromComposite transform) are
    # trusted; only base-literal values are subject to the contract check.
    if yq "$sel_patches | .[].toFieldPath" "$file" 2>/dev/null | grep -qE "manifest\.(data|stringData)\[\"?$key\"?\]"; then
      continue
    fi
    valfile="$(mktemp)"
    yq "$sel | (.stringData // .data)[\"$key\"]" "$file" > "$valfile" 2>/dev/null || true
    # Every document in the value must be a mapping carrying apiVersion + kind.
    local parsed
    parsed="$(yq '.apiVersion != null and .kind != null' "$valfile" 2>/dev/null || echo false)"
    if [ -z "$parsed" ] || echo "$parsed" | grep -q false; then
      fail "CRS payload '$mname' key '$key' is not an applyable Kubernetes manifest (scalar or missing apiVersion/kind)"
      ok=0
    fi
    rm -f "$valfile"
  done <<<"$keys"

  if [ "$ok" -eq 1 ]; then
    echo "  ✓ CRS payload '$mname': all values are applyable Kubernetes manifests"
  fi
}

validate_crs_contract() {
  # Cross-check every composition-created Secret/ConfigMap whose rendered name
  # appears in the ClusterResourceSet resources[] list, and assert its payload
  # contract (validate_payload_keys).
  local file="$1"
  local base_count i
  local -a crs_names=()
  base_count="$(yq "$(cr) | (.base.spec.forProvider.manifest.spec.resources | length)" "$file" 2>/dev/null || echo 0)"
  for ((i = 0; i < base_count; i++)); do
    crs_names+=("$(render_name "$file" "$i")")
  done

  local rname mkind mname
  while IFS= read -r rname; do
    [ -z "$rname" ] && continue
    mkind="$(yq ".spec.pipeline[0].input.resources[] | select(.name == \"$rname\") | .base.spec.forProvider.manifest.kind // \"\"" "$file" 2>/dev/null || echo "")"
    case "$mkind" in
      Secret|ConfigMap)
        mname="$(render_manifest_name "$file" "$rname")"
        if printf '%s\n' "${crs_names[@]}" | grep -qx "$mname"; then
          validate_payload_keys "$file" "$rname" "$mname"
        fi
        ;;
    esac
  done <<<"$(yq '.spec.pipeline[0].input.resources[] | .name' "$file")"
}

validate_composition() {
  local file="$1"
  local sel base_count i name
  local -a rendered=()

  echo "── $file ──"

  if ! command -v yq >/dev/null 2>&1; then
    fail "yq not installed (required for this check)"
    return
  fi

  sel="$(cr)"
  base_count="$(yq "$sel | (.base.spec.forProvider.manifest.spec.resources | length)" "$file" 2>/dev/null || echo 0)"

  if [ -z "$base_count" ] || [ "$base_count" -lt 1 ]; then
    fail "cluster-resource-set entry not found or resources[] empty"
    return
  fi

  # 1 + 2. Render every slot and check non-empty + unique.
  for ((i = 0; i < base_count; i++)); do
    rendered+=("$(render_name "$file" "$i")")
  done

  local empty_found=0
  for ((i = 0; i < base_count; i++)); do
    name="${rendered[$i]}"
    if [ -z "$name" ] || [ "$name" = "null" ]; then
      fail "resource index $i renders to an EMPTY name"
      empty_found=1
    fi
  done
  [ "$empty_found" -eq 0 ] && echo "  ✓ all $base_count resource names non-empty"

  local duplicates
  duplicates="$(printf '%s\n' "${rendered[@]}" | grep -v '^$' | sort | uniq -d)"
  if [ -n "$duplicates" ]; then
    fail "duplicate rendered names:"
    echo "$duplicates" | sed 's/^/      /'
  else
    echo "  ✓ no duplicate rendered names"
  fi

  local rendered_all
  rendered_all="$(printf '%s\n' "${rendered[@]}")"

  # 3. Static names present exactly once, and never claim-prefixed.
  local s count
  for s in "${static_names[@]}"; do
    count="$(echo "$rendered_all" | grep -cx "$s" || true)"
    if [ "$count" -ne 1 ]; then
      fail "static name '$s' expected exactly once, found $count"
    else
      echo "  ✓ static name '$s' (not per-spoke renamed)"
    fi
    if echo "$rendered_all" | grep -qx "$TEST_CLAIM-$s"; then
      fail "static name '$s' must NOT be per-spoke prefixed"
    fi
  done

  # 4. Dynamic suffixes render to {claim}-<suffix> exactly once.
  local d expected hits
  local -a comp_dynamic_suffixes=("${dynamic_suffixes[@]}")
  for d in "${comp_dynamic_suffixes[@]}"; do
    expected="$TEST_CLAIM-$d"
    hits="$(echo "$rendered_all" | grep -cx "$expected" || true)"
    if [ "$hits" -eq 1 ]; then
      echo "  ✓ dynamic name '$expected' exactly once"
    else
      fail "dynamic name '$expected' expected exactly once, found $hits"
    fi
    if echo "$rendered_all" | grep -qx "$d"; then
      fail "dynamic suffix '$d' must not render as a bare (unprefixed) name"
    fi
  done

  # 5. CRS payload contract: every composition-created resource-set value must be
  #    an applyable Kubernetes manifest (no scalar configuration parameters).
  validate_crs_contract "$file"

  echo
}

echo "SpokePool ClusterResourceSet alignment guard"
echo "Test claim: $TEST_CLAIM"
echo "Validated: ${#compositions[@]} composition(s)"
echo

if ! command -v yq >/dev/null 2>&1; then
  echo "FAIL: yq not installed. Install https://github.com/mikefarah/yq"
  exit 1
fi

for comp in "${compositions[@]}"; do
  if [ -f "$comp" ]; then
    validate_composition "$comp"
  else
    echo "── $comp ──"
    fail "composition file missing"
  fi
done

if [ "$failures" -ne 0 ]; then
  echo "FAIL: $failures validation check(s) failed."
  echo ""
  echo "This is a positional-alignment regression guard. If you added/removed a"
  echo "ClusterResourceSet resource in a SpokePool composition, every downstream"
  echo "patch index (spec.forProvider.manifest.spec.resources[N].name) must be"
  echo "re-indexed. Static contract: $TEST_CLAIM vs literal names — see the"
  echo "static_names / dynamic_suffixes arrays in this script."
  exit 1
fi

echo "✅ All SpokePool ClusterResourceSet compositions aligned and valid."
exit 0