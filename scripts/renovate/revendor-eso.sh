#!/usr/bin/env bash
set -euo pipefail

# Re-vendor the External Secrets Operator chart into the spoke manifests.
#
# WHY THIS EXISTS. The vendored artefact had no recorded recipe and no
# provenance digest, so it could be neither reproduced before replacement nor
# protected from a hand edit -- and "no recipe" had been accepted as the reason
# to leave the component behind. The recipe below was recovered by rendering the
# pinned chart and diffing object by object: 44 objects, names identical once
# the release name is right, and every difference accounted for.
#
# THE RECIPE, in full:
#   1. helm template with release name `eso` in namespace `external-secrets`
#      (the names are eso-external-secrets-*, which is where `eso` comes from).
#   2. Split the 25 CRDs from the 19 controller objects.
#   3. ADR-023: sync-wave -5 on each CRD, so they are Established before
#      anything that depends on them.
#   4. Three hand patches on the controller side, re-applied here because a
#      re-vendor would otherwise silently drop them:
#        - the control-plane toleration on all three Deployments;
#        - pod anti-affinity on all three, spreading replicas across hosts;
#        - failurePolicy Ignore on both ValidatingWebhookConfigurations.
#
# PATCH 3 IS LOAD-BEARING AND IS NOT HARDENING. Upstream ships
# failurePolicy: Fail, which makes ESO's own webhook a hard dependency of every
# ExternalSecret and SecretStore write. On a spoke that is a bootstrap deadlock:
# the webhook cannot run until its Deployment is up, and its Deployment waits on
# secrets that ExternalSecrets create. Ignore breaks that cycle. Changing it back
# is a deliberate decision, not a re-vendor artefact.
#
# Requires: curl, helm, python3 (with pyyaml), sha256sum
#
# Usage:
#   bash scripts/renovate/revendor-eso.sh            # the version the hub pins
#   bash scripts/renovate/revendor-eso.sh 2.15.0     # an explicit chart version

PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SPOKE_DIR="${PROJECT_ROOT}/manifests/spoke/spoke-catalog/infra/external-secrets"
APPSET="${PROJECT_ROOT}/manifests/argocd/environment-manager/templates/01-platform-infra-appset.yaml"
CRD_SYNC_WAVE="-5"
RELEASE="eso"
NAMESPACE="external-secrets"

if [ $# -ge 1 ]; then
  NEW_VERSION="$1"
else
  NEW_VERSION=$(awk "/repoURL: 'https:\/\/charts.external-secrets.io'/,0" "$APPSET" \
    | grep -m1 targetRevision | sed "s/.*targetRevision: '//;s/'.*//") || true
fi
[ -n "$NEW_VERSION" ] || { echo "ERROR: could not determine the chart version" >&2; exit 1; }

echo "==> Re-vendoring External Secrets chart ${NEW_VERSION}..."
WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"' EXIT

# The index publishes tarballs on GitHub releases, not under the index's own
# path, so the URL is read from the index rather than constructed.
CHART_URL=$(curl -fsSL https://charts.external-secrets.io/index.yaml \
  | python3 -c "
import sys, yaml
want = '${NEW_VERSION}'
for c in yaml.safe_load(sys.stdin)['entries']['external-secrets']:
    if c['version'] == want:
        print(c['urls'][0]); break
else:
    sys.exit('chart version %s is not in the index' % want)
")
curl -fsSL "$CHART_URL" -o "$WORK_DIR/chart.tgz"
tar -xzf "$WORK_DIR/chart.tgz" -C "$WORK_DIR"
APP_VERSION=$(grep -m1 '^appVersion:' "$WORK_DIR/external-secrets/Chart.yaml" \
  | sed 's/^appVersion:[[:space:]]*//;s/^v//;s/["'"'"']//g')
[ -n "$APP_VERSION" ] || { echo "ERROR: could not read appVersion" >&2; exit 1; }
echo "    chart ${NEW_VERSION} packages application v${APP_VERSION}"

helm template "$RELEASE" "$WORK_DIR/external-secrets" -n "$NAMESPACE" > "$WORK_DIR/rendered.yaml"

echo "    Splitting, annotating CRDs and re-applying the hand patches..."
python3 -c "
import yaml, io

TOLERATION = {'key': 'node-role.kubernetes.io/control-plane',
              'operator': 'Exists', 'effect': 'NoSchedule'}
AFFINITY = {'podAntiAffinity': {'preferredDuringSchedulingIgnoredDuringExecution': [
    {'weight': 100, 'podAffinityTerm': {
        'labelSelector': {'matchExpressions': [
            {'key': 'app.kubernetes.io/instance', 'operator': 'In', 'values': ['${RELEASE}']}]},
        'topologyKey': 'kubernetes.io/hostname'}}]}}

docs = [d for d in yaml.safe_load_all(io.open('${WORK_DIR}/rendered.yaml', encoding='utf-8')) if d]
crds = [d for d in docs if d['kind'] == 'CustomResourceDefinition']
rest = [d for d in docs if d['kind'] != 'CustomResourceDefinition']
if not crds or not rest:
    raise SystemExit('render produced %d CRDs and %d other objects' % (len(crds), len(rest)))

for d in crds:
    d['metadata'].setdefault('annotations', {})['argocd.argoproj.io/sync-wave'] = '${CRD_SYNC_WAVE}'

deployments = webhooks = 0
for d in rest:
    if d['kind'] == 'Deployment':
        spec = d['spec']['template']['spec']
        spec['tolerations'] = [dict(TOLERATION)]
        spec['affinity'] = AFFINITY
        deployments += 1
    elif d['kind'] == 'ValidatingWebhookConfiguration':
        for w in d.get('webhooks') or []:
            w['failurePolicy'] = 'Ignore'
        webhooks += 1
# The counts are asserted: a chart that renames or adds a Deployment would
# otherwise leave one unpatched, and an unpatched webhook reintroduces the
# bootstrap deadlock silently.
if deployments != 3 or webhooks != 2:
    raise SystemExit('expected 3 Deployments and 2 ValidatingWebhookConfigurations, '
                     'found %d and %d -- the chart changed shape; re-derive the patches'
                     % (deployments, webhooks))

for name, ds in (('crd', crds), ('controller', rest)):
    with io.open('${WORK_DIR}/final-%s.yaml' % name, 'w', encoding='utf-8') as f:
        f.write(yaml.safe_dump_all(ds, default_flow_style=False, sort_keys=False))
print('    %d CRDs, %d controller objects, %d Deployments and %d webhooks patched'
      % (len(crds), len(rest), deployments, webhooks))
"

echo "    Writing provenance headers..."
# Hash exactly what verify-vendor-digest.sh hashes: `grep -v '^# '`, which keeps
# bare "#" separator lines. The digest line starts with "# ", so writing it
# afterwards cannot change the hash.
for name in crd controller; do
  out="${SPOKE_DIR}/${name}.yaml"
  {
    echo "# Vendored from helm chart external-secrets v${NEW_VERSION} (charts.external-secrets.io)"
    echo "# sha256: PLACEHOLDER"
    echo "#"
    echo "# Generated by scripts/renovate/revendor-eso.sh -- do not edit. That script"
    echo "# records the release name, the namespace, the CRD sync-wave and the three hand"
    echo "# patches (control-plane toleration, pod anti-affinity, webhook failurePolicy"
    echo "# Ignore), and the digest above covers everything below it."
    cat "$WORK_DIR/final-${name}.yaml"
  } > "$out"
  digest=$(grep -v '^# ' "$out" | sha256sum | cut -d' ' -f1)
  perl -pi -e "s|^# sha256: PLACEHOLDER\$|# sha256: ${digest}|" "$out"
done

echo "==> Done. External Secrets chart ${NEW_VERSION} (application v${APP_VERSION})"
echo "    Verify with: bash scripts/validate/verify-vendor-digest.sh"
