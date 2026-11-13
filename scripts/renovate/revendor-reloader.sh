#!/usr/bin/env bash
set -euo pipefail

# Re-vendor the Stakater Reloader chart into the spoke manifests (ADR-030 #3).
#
# The header of the vendored file already recorded the render command and the
# customisations; what it did not have was a SCRIPT, so nobody could reproduce
# the artefact before replacing it, and no provenance digest protected it from a
# hand edit. The recipe was confirmed by rendering chart 2.2.19 and diffing
# object by object: six objects, every difference accounted for by the three
# transformations below.
#
# THE RECIPE:
#   1. helm template, release `reloader`, namespace platform-ops, with
#      reloader.watchGlobally=true (the hub's own value).
#   2. Strip Helm's bookkeeping -- the meta.helm.sh/* annotations and the
#      managed-by/chart/heritage/release labels -- from every object and from the
#      pod template. These describe a Helm release, and ArgoCD applies this from
#      a kustomize build: there is no release.
#   3. Pin the image by digest (ADR-021) and add the container hardening and
#      resources the chart omits (upstream renders an empty securityContext and
#      no resources at all).
#
# The control-plane tolerations are appended by the spoke kustomization, not
# here, which is why they are absent from this render and must stay absent.
#
# UPSTREAM PUBLISHES TWO LINES. Chart 3.0.0-beta.* carries application
# v2.0.0-beta.*; the stable line is chart 2.2.x with application v1.4.x. Default
# to the newest STABLE chart rather than the newest release.
#
# Requires: curl, helm, python3 (with pyyaml), sha256sum
#
# Usage:
#   bash scripts/renovate/revendor-reloader.sh                     # newest stable chart
#   bash scripts/renovate/revendor-reloader.sh 2.2.20 <digest>     # explicit

PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
OUT="${PROJECT_ROOT}/manifests/spoke/spoke-catalog/infra/reloader/controller.yaml"
REPO="https://stakater.github.io/stakater-charts"

NEW_VERSION="${1:-}"
IMAGE_DIGEST="${2:-}"
if [ -z "$NEW_VERSION" ]; then
  NEW_VERSION=$(curl -fsSL "$REPO/index.yaml" | python3 -c "
import sys, yaml, re
cs = yaml.safe_load(sys.stdin)['entries']['reloader']
stable = [c for c in cs
          if re.fullmatch(r'\d+\.\d+\.\d+', c['version'])
          and 'beta' not in str(c.get('appVersion', ''))]
print(stable[0]['version'] if stable else '')
")
fi
[ -n "$NEW_VERSION" ] || { echo "ERROR: could not determine the chart version" >&2; exit 1; }

echo "==> Re-vendoring Reloader chart ${NEW_VERSION}..."
WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"' EXIT

curl -fsSL "$REPO/reloader-${NEW_VERSION}.tgz" -o "$WORK_DIR/chart.tgz"
tar -xzf "$WORK_DIR/chart.tgz" -C "$WORK_DIR"
APP_VERSION=$(grep -m1 '^appVersion:' "$WORK_DIR/reloader/Chart.yaml" \
  | sed 's/^appVersion:[[:space:]]*//;s/["'"'"']//g')
[ -n "$APP_VERSION" ] || { echo "ERROR: could not read appVersion" >&2; exit 1; }
echo "    chart ${NEW_VERSION} packages application ${APP_VERSION}"

if [ -z "$IMAGE_DIGEST" ]; then
  TOKEN=$(curl -fsSL "https://ghcr.io/token?service=ghcr.io&scope=repository:stakater/reloader:pull" \
    | python3 -c "import sys, json; print(json.load(sys.stdin)['token'])")
  IMAGE_DIGEST=$(curl -fsSI -H "Authorization: Bearer $TOKEN" \
    -H "Accept: application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json" \
    "https://ghcr.io/v2/stakater/reloader/manifests/${APP_VERSION}" \
    | grep -i '^docker-content-digest' | tr -d '\r' | sed 's/.*: //')
fi
[ -n "$IMAGE_DIGEST" ] || { echo "ERROR: could not resolve the image digest for ${APP_VERSION}" >&2; exit 1; }
echo "    image ghcr.io/stakater/reloader:${APP_VERSION}@${IMAGE_DIGEST}"

helm template reloader "$WORK_DIR/reloader" -n platform-ops \
  --set reloader.watchGlobally=true > "$WORK_DIR/rendered.yaml"

python3 -c "
import yaml, io

HELM_LABELS = ('app.kubernetes.io/managed-by', 'chart', 'heritage', 'release')
RESOURCES = {'requests': {'cpu': '10m', 'memory': '64Mi'},
             'limits': {'cpu': '1', 'memory': '256Mi'}}
SECURITY = {'allowPrivilegeEscalation': False,
            'readOnlyRootFilesystem': True,
            'capabilities': {'drop': ['ALL']}}

def strip(meta):
    for a in list((meta.get('annotations') or {})):
        if a.startswith('meta.helm.sh/'):
            del meta['annotations'][a]
    if not meta.get('annotations'):
        meta.pop('annotations', None)
    for l in HELM_LABELS:
        (meta.get('labels') or {}).pop(l, None)

docs = [d for d in yaml.safe_load_all(io.open('${WORK_DIR}/rendered.yaml', encoding='utf-8')) if d]
if len(docs) != 6:
    raise SystemExit('render produced %d objects, expected 6 -- the chart changed shape' % len(docs))

patched = 0
for d in docs:
    strip(d['metadata'])
    if d['kind'] == 'Deployment':
        strip(d['spec']['template']['metadata'])
        c = d['spec']['template']['spec']['containers'][0]
        c['image'] = 'ghcr.io/stakater/reloader:${APP_VERSION}@${IMAGE_DIGEST}'
        c['resources'] = RESOURCES
        c['securityContext'] = SECURITY
        patched += 1
if patched != 1:
    raise SystemExit('expected one Deployment to patch, found %d' % patched)

with io.open('${WORK_DIR}/final.yaml', 'w', encoding='utf-8') as f:
    f.write(yaml.safe_dump_all(docs, default_flow_style=False, sort_keys=False))
print('    6 objects, Helm bookkeeping stripped, image pinned, hardening applied')
"

# The prose header is preserved: it carries the ADR-030 reasoning and the
# incident record, which a generator must not throw away. Only the version line
# and the digest are rewritten.
{
  echo "# ADR-030 #3: Stakater Reloader ${APP_VERSION} on the spoke -- what makes every"
  echo "# \`*.reloader.stakater.com/reload\` annotation in this catalogue act."
  echo "# Source: ${REPO} (chart reloader ${NEW_VERSION},"
  echo "#         values: reloader.watchGlobally=true -- the hub's own values)"
  echo "# sha256: PLACEHOLDER"
  echo "#"
  echo "# Generated by scripts/renovate/revendor-reloader.sh -- do not edit. It records"
  echo "# the render command, the Helm-bookkeeping strip, the digest pin and the container"
  echo "# hardening; the digest above covers everything below it."
  echo "# Tolerations for the cp taint are appended by the spoke kustomization."
  sed -n '/^# Why it is here:/,$p' "$OUT" | sed -n '/^[^#]/q;p'
  cat "$WORK_DIR/final.yaml"
} > "$WORK_DIR/out.yaml"
mv "$WORK_DIR/out.yaml" "$OUT"
digest=$(grep -v '^# ' "$OUT" | sha256sum | cut -d' ' -f1)
perl -pi -e "s|^# sha256: PLACEHOLDER\$|# sha256: ${digest}|" "$OUT"

echo "==> Done. Reloader chart ${NEW_VERSION} (application ${APP_VERSION})"
