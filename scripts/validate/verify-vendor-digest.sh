#!/usr/bin/env bash
set -euo pipefail

# Verify every vendored chart render against its own provenance, and against the
# management cluster's pin of the same chart. Catches:
#   - manual edits to vendored manifests without re-rendering
#   - tampered artifacts
#   - Renovate PRs that bump the version but don't re-vendor
#   - a chart moved on the management cluster and not on the spokes, or vice versa
#
# The third is completed rather than merely refused:
# .github/workflows/revendor-kyverno.yml re-renders on Renovate's branch and
# commits, so a proposal has something to pass. Refusing without that left a
# pull request that could only ever fail, which teaches people to merge past the
# check rather than to trust it.
#
# NOTHING HERE IS A LIST. Both sides are discovered:
#
#   the vendored renders   every file under manifests/ carrying a `# sha256:`
#                          provenance line
#   the management pins    BOTH mechanisms that pin a remote chart, which
#                          references/pin-locations.md states in one sentence --
#                          "a component descriptor under
#                          manifests/argocd/components/<boundary>/, OR an element
#                          in a boundary appset under
#                          manifests/argocd/environment-manager/templates/"
#
# This script used to name four components and read only the appset. That is how
# Reloader was moved to chart 2.2.20 on the spokes while the hub stayed on
# 2.2.19: its pin is a component descriptor, the gate did not look there, and the
# release's reasons check caught the split instead (2026-11-14). A gate that
# enumerates the locations it was told about protects only those; a fifth
# component, or a chart that changes mechanism, now lands inside the check
# without anyone noticing it needs to.
#
# Usage: bash scripts/validate/verify-vendor-digest.sh

FAIL=0

# ─── 1. every vendored render matches its own digest ─────────────────────────
#
# Hash everything except the provenance header lines. `grep -v '^# '` DROPS
# "# " prose and KEEPS bare "#" separators, which the headers contain; the
# re-vendor scripts write the digest under this same rule, and the digest line
# itself starts with "# " so it cannot affect its own hash.
VENDORED=$(grep -rl '^# sha256: ' manifests 2>/dev/null | grep -v '/embedded/' | sort || true)
if [ -z "$VENDORED" ]; then
  echo "FAIL: no vendored render carries a provenance digest -- this gate would pass vacuously"
  exit 1
fi

while IFS= read -r file; do
  [ -n "$file" ] || continue
  RECORDED=$(grep -m1 '^# sha256: ' "$file" | sed 's/^# sha256: //' || true)
  if [ -z "$RECORDED" ]; then
    echo "FAIL: no sha256 digest found in $file"
    FAIL=1
    continue
  fi
  ACTUAL=$(grep -v '^# ' "$file" | sha256sum | cut -d' ' -f1)
  if [ "$RECORDED" = "$ACTUAL" ]; then
    echo "OK: $file digest matches"
  else
    echo "FAIL: $file digest mismatch (recorded=$RECORDED actual=$ACTUAL)"
    FAIL=1
  fi
done <<EOF
$VENDORED
EOF

# ─── 2. the management pin and the vendored render name one chart version ────
#
# ONE VERSION PER COMPONENT, EVERYWHERE IT RUNS. The management cluster pulls
# the chart; the spokes apply a render of that chart. Nothing else connects them,
# so a split is silent: the release reports the component twice at two versions
# with nobody having decided that.
if ! PARITY=$(python3 - "$VENDORED" <<'PY'
import glob, os, re, sys, yaml

vendored_files = [f for f in sys.argv[1].split("\n") if f.strip()]

# The vendored side: chart name and version, in either header spelling.
HEADERS = (
    re.compile(r'^#\s*Vendored from helm chart\s+(?P<chart>[\w.-]+)\s+v?(?P<ver>[\w.+-]+)', re.M),
    re.compile(r'^#\s*Source:.*?\bchart\s+(?P<chart>[\w.-]+)\s+v?(?P<ver>[\d][\w.+-]*)', re.M),
)
vendored = {}
for path in vendored_files:
    head = "".join(l for l in open(path, encoding="utf-8") if l.startswith("#"))
    for pat in HEADERS:
        m = pat.search(head)
        if m:
            vendored.setdefault(m.group("chart"), {})[m.group("ver")] = path
            break
    else:
        print(f"FAIL: {path} carries a digest but no readable chart/version header, "
              f"so its version cannot be compared with the management pin")
        sys.exit(1)

# The management side: BOTH mechanisms from pin-locations.md.
pins = {}

# FOUND BY SHAPE, NOT BY PATH. pin-locations.md names two mechanisms -- a
# component descriptor and an appset element -- and there is a third it does not:
# an ArgoCD Application manifest, which is how CloudNativePG is pinned
# (manifests/hub-core-services/cloudnative-pg/application.yaml). Scanning the two
# documented directories reported CNPG as having no management pin at all, which
# is a false OK and worse than the hardcoded list it replaced. So the rule is the
# SHAPE of a remote-chart pin -- a repoURL beginning http, a chart, and a
# targetRevision -- wherever it appears under manifests/.
for path in sorted(glob.glob("manifests/**/*.yaml", recursive=True)):
    if "/embedded/" in path or "/environment-manager/templates/" in path:
        continue
    try:
        docs = [d for d in yaml.safe_load_all(open(path, encoding="utf-8")) if isinstance(d, dict)]
    except yaml.YAMLError:
        continue
    for d in docs:
        # a component descriptor holds the triple at the top level; an ArgoCD
        # Application holds it under spec.source (or each of spec.sources).
        candidates = [d]
        spec = d.get("spec") if isinstance(d.get("spec"), dict) else {}
        if isinstance(spec.get("source"), dict):
            candidates.append(spec["source"])
        for src in spec.get("sources") or []:
            if isinstance(src, dict):
                candidates.append(src)
        for c in candidates:
            repo, chart, rev = c.get("repoURL") or "", c.get("chart") or "", c.get("targetRevision")
            if str(repo).startswith("http") and chart and rev not in (None, ""):
                pins.setdefault(str(chart), {})[str(rev)] = path

# An appset element spreads repoURL, chart and targetRevision over tens of
# lines, so the triple is accumulated per element rather than matched on one.
for path in sorted(glob.glob("manifests/argocd/environment-manager/templates/*.yaml")):
    text = open(path, encoding="utf-8").read()
    cur = {}
    for line in text.splitlines():
        for key in ("repoURL", "chart", "targetRevision"):
            m = re.match(rf"\s*{key}:\s*'?\"?([^'\"#]+?)'?\"?\s*$", line)
            if m:
                if key == "repoURL" and cur.get("repoURL"):
                    cur = {}            # a new element begins at its repoURL
                cur[key] = m.group(1).strip()
        if {"repoURL", "chart", "targetRevision"} <= cur.keys():
            if cur["repoURL"].startswith("http") and cur["chart"] and cur["targetRevision"]:
                pins.setdefault(cur["chart"], {})[cur["targetRevision"]] = path
            cur = {}

failed = False
for chart in sorted(vendored):
    vers = vendored[chart]
    if len(vers) > 1:
        print(f"FAIL: {chart} is vendored at more than one version: "
              f"{', '.join(f'{v} ({os.path.basename(p)})' for v, p in sorted(vers.items()))}")
        failed = True
        continue
    vver, vpath = next(iter(vers.items()))
    if chart not in pins:
        # Spoke-only by design for some components; stated rather than assumed.
        print(f"OK: {chart} chart {vver} is vendored for spokes; the management cluster pins no chart for it")
        continue
    pvers = pins[chart]
    if len(pvers) > 1:
        print(f"FAIL: {chart} is pinned at more than one version on the management cluster: "
              f"{', '.join(f'{v} ({os.path.basename(p)})' for v, p in sorted(pvers.items()))}")
        failed = True
        continue
    pver, ppath = next(iter(pvers.items()))
    if pver != vver:
        print(f"FAIL: {chart.upper()} VERSION SPLIT — the management cluster pulls chart {pver}")
        print(f"      ({ppath}) and the spokes apply a render of chart {vver}.")
        print(f"      Move both together, then re-vendor:")
        print(f"        bash scripts/renovate/revendor-{chart}.sh {pver}")
        failed = True
    else:
        print(f"OK: {chart} chart {pver} is the version vendored for spokes")

sys.exit(1 if failed else 0)
PY
); then
  FAIL=1
fi
printf '%s\n' "$PARITY"

if [ "$FAIL" -ne 0 ]; then
  echo ""
  echo "Vendored manifests have been modified without re-rendering, or a chart's"
  echo "management pin and its vendored render disagree."
  echo "Re-vendor with the script for that component under scripts/renovate/."
  exit 1
fi

echo "All vendor digests and chart-version parity verified."
