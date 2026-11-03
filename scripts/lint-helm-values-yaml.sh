#!/usr/bin/env bash
# Lint all rendered helmValues block scalars across AppSet templates.
#
# Checks:
# 1. Helm chart template is valid (helm template succeeds)
# 2. No literal backslash-quote sequences ("" should be ")
# 3. YAML syntax + style compliance via yamllint on helmValues content
#
# First validates that helm template renders the chart without errors
# (catches Go-template-level YAML issues). Then reads each helmValues: |
# block from source AppSet files, extracts the raw string content, and
# validates it as standalone YAML.

set -euo pipefail

# This hook runs under pre-commit's own python environment, whose PATH does not
# include where helm is installed. Without this the hook reports
# "helm template failed ... template-level YAML error" -- which reads as a
# broken chart rather than a missing binary, and sends the reader debugging
# the wrong thing entirely. See scripts/lib/tool-path.sh.
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib/tool-path.sh"
require_tools helm || exit 1


chart_dir="manifests/argocd/environment-manager"
appset_dir="$chart_dir/templates"
components_dir="manifests/argocd/components"
yamllint_config=".yamllint.yaml"

# lint_block <label> <values-content>
# Returns 0 if the block is clean, 1 otherwise. Prints its own diagnostics.
lint_block() {
  local label="$1" content="$2"

  if printf '%s' "$content" | grep -qE '\\"'; then
    echo "❌ [$label] backslash-quote sequences found in helmValues."
    echo "   Block scalars (|) do not require escape characters."
    printf '%s' "$content" | grep -nE '\\"' | head -5
    return 1
  fi

  if ! printf '%s\n' "$content" | yamllint -c "$yamllint_config" - 2>/dev/null; then
    echo "❌ [$label] helmValues failed YAML lint."
    printf '%s\n' "$content" | yamllint -c "$yamllint_config" - 2>&1 | head -8 | sed 's/^/     /'
    return 1
  fi
  return 0
}

# Phase 0: Pre-flight check — helm template must succeed
echo "Checking chart renders successfully..."
# ADR-055: every boundary renders on every render, so the lint must supply the
# same environment values the seed Application does. publicTlsIssuer has no
# default by design (ADR-051) and must be named here rather than defaulted.
lint_values=(--set environmentRevision=dry-run --set environmentSlug=prod --set boxId=example-box \
             --set provider=hetzner --set topology=single \
             --set publicTlsIssuer=letsencrypt-prod \
             --set instanceRepoURL=https://github.com/example-org/example-gitops --set hubDomain=dev.example.test --set previewDomain=example-preview.test --set 'alerting.emailTo[0]=alerts@example.test')

# Any value but "development" selects the released path. Synthetic on purpose:
# pinning a real version here would make the linter fail on the day it is bumped.
lint_bundle_version="0.0.0-lint"

# Rendered from a STAGED copy. The descriptors live under
# manifests/argocd/components/ and are copied into the chart at package time, so
# rendering the source tree shows this script none of them -- which is why it
# used to raw-parse the descriptor files instead. A descriptor's helmValues is
# run through `tpl` (_distribution.tpl), so it may carry Helm actions and is not
# plain YAML; parsing the file reported template text as a syntax error while
# the thing that ships renders clean.
# shellcheck source=lib/stage-bundle-chart.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/stage-bundle-chart.sh"
staged_chart=$(mktemp -d)/environment-manager
stage_bundle_chart "$chart_dir" "$staged_chart"

# Rendered TWICE, because the boundaries carry two element shapes and linting one
# leaves the other unchecked. `bundleVersion: development` takes the inline path
# that names this repository; any other value takes the released path, which
# resolves the published distribution and is the only one that reads descriptors.
# A tenant installs the released form, so it is not optional here.
render_mode() {
  helm template environment-manager "$staged_chart" "${lint_values[@]}" \
    --set bundleVersion="$1" 2>&1
}

if ! rendered_dev=$(render_mode development); then
    echo "❌ helm template failed (bundleVersion=development) — template-level YAML error."
    echo "   Run the following to debug:"
    echo "     helm template environment-manager $staged_chart ${lint_values[*]} --set bundleVersion=development --debug"
    exit 1
fi
if ! rendered_released=$(render_mode "$lint_bundle_version"); then
    echo "❌ helm template failed (bundleVersion=$lint_bundle_version) — template-level YAML error."
    echo "   Run the following to debug:"
    echo "     helm template environment-manager $staged_chart ${lint_values[*]} --set bundleVersion=$lint_bundle_version --debug"
    exit 1
fi
rendered=$(printf '%s\n---\n%s\n' "$rendered_dev" "$rendered_released")
echo "✓ Chart renders successfully (development and released)"

passed=true
file_count=0
element_count=0
descriptor_files=0

# Inline list elements, read from the RENDERED chart rather than the raw
# templates. The templates are Helm, not YAML -- a `{{- if }}` at column zero
# makes yq return nothing for the whole file, so scanning the sources found no
# inline blocks at all and reported that only by counting zero.
#
# Extraction is done in Python: the render is a multi-document stream and
# indexing into a filtered set across documents is not something yq expresses
# reliably.
rendered_file=$(mktemp)
extract_dir=$(mktemp -d)
trap 'rm -rf "$rendered_file" "$extract_dir"' EXIT
printf '%s\n' "$rendered" >"$rendered_file"

python3 - "$rendered_file" "$extract_dir" <<'PYEOF'
import sys, yaml, os, re
src, out = sys.argv[1], sys.argv[2]
n = 0
for doc in yaml.safe_load_all(open(src)):
    if not doc or doc.get("kind") != "ApplicationSet":
        continue
    for g in doc.get("spec", {}).get("generators", []) or []:
        lst = (g or {}).get("list")
        if not lst:
            continue
        for e in lst.get("elements", []) or []:
            v = e.get("helmValues")
            if not v or not str(v).strip():
                continue
            name = re.sub(r"[^A-Za-z0-9._-]", "_", str(e.get("appName", "unknown")))
            with open(os.path.join(out, f"{n:03d}__{name}"), "w") as f:
                f.write(str(v))
            n += 1
print(n)
PYEOF

for blk in "$extract_dir"/*; do
  [ -f "$blk" ] || continue
  app_name="${blk##*__}"
  element_count=$((element_count + 1))
  file_count=$((file_count + 1))
  lint_block "rendered:$app_name" "$(cat "$blk")" || passed=false
done

# Component descriptors (ADR-061) reach the render through descriptors/<boundary>/,
# so they were linted above as `rendered:<appName>` blocks -- already templated,
# which is the form the cluster receives.
#
# What is asserted here is that they got there. Staging is the only thing putting
# a descriptor in front of this linter, and staging that silently copies nothing
# would leave every descriptor unchecked while the script still reported success.
# So: every descriptor declaring helmValues must appear in the render by name.
if [ -d "$components_dir" ]; then
  rendered_names=$(printf '%s\n' "$rendered_released" | yq -r \
    'select(.kind == "ApplicationSet") | .spec.generators[]? | select(has("list"))
     | .list.elements[]? | select(.helmValues != null and .helmValues != "") | .appName' \
    2>/dev/null | sort -u)

  for desc in "$components_dir"/*/*.yaml; do
    [ -f "$desc" ] || continue
    helm_values=$(yq eval -r '.helmValues // ""' "$desc" 2>/dev/null || echo "")
    if [ "$helm_values" = "" ] || [ "$helm_values" = "null" ]; then
      continue
    fi
    app_name=$(yq eval -r '.appName // "unknown"' "$desc" 2>/dev/null)
    descriptor_files=$((descriptor_files + 1))
    if ! printf '%s\n' "$rendered_names" | grep -qxF "$app_name"; then
      echo "❌ [$desc:$app_name] declares helmValues but no rendered element carries them."
      echo "   The descriptor never reached the chart, so nothing linted it."
      passed=false
    fi
  done
fi

if [ "$passed" = false ]; then
  echo ""
  echo "FAIL: One or more helmValues blocks failed validation."
  exit 1
fi

# A values linter that found nothing must not report success. See the header.
if [ "$element_count" -eq 0 ]; then
  echo ""
  echo "FAIL: no helmValues blocks were found at all."
  echo "  Looked in: the rendered chart -- inline list elements of"
  echo "             $appset_dir/*-appset.yaml, plus the descriptors under"
  echo "             $components_dir/*/ staged in as the bundle stages them."
  echo "  The platform declares Helm values somewhere; finding none means this"
  echo "  script is looking in the wrong place, not that there is nothing to check."
  exit 1
fi

echo "✅ All $element_count rendered helmValues blocks passed linting ($descriptor_files descriptor(s) confirmed present)."
exit 0
