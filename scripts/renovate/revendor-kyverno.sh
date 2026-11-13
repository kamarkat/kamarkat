#!/usr/bin/env bash
set -euo pipefail

# Re-vendor Kyverno Helm chart into Git-spoke manifests.
#
# Called by Renovate postUpgradeTasks after bumping targetRevision.
# Reads the new version from RENOVATE_POST_UPGRADE_COMMAND_DATA_FILE
# (Renovate's upgrade context JSON) or falls back to the AppSet file.
#
# Requires: helm, kustomize, python3 (with pyyaml), sha256sum
#
# Usage:
#   bash scripts/renovate/revendor-kyverno.sh          # auto-detect version
#   bash scripts/renovate/revendor-kyverno.sh 3.3.9    # explicit version

PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

if [ $# -ge 1 ]; then
  NEW_VERSION="$1"
else
  DATA_FILE="${RENOVATE_POST_UPGRADE_COMMAND_DATA_FILE:-}"

  if [ -n "$DATA_FILE" ] && [ -f "$DATA_FILE" ]; then
    NEW_VERSION=$(python3 -c "
import json, sys
data = json.load(open('$DATA_FILE'))
for item in data:
    if item.get('depName') == 'kyverno':
        print(item['newValue'])
        sys.exit(0)
print('ERROR: kyverno not found in data file', file=sys.stderr)
sys.exit(1)
")
  else
    APPSET="${PROJECT_ROOT}/manifests/argocd/environment-manager/templates/01-platform-infra-appset.yaml"
    # The element's targetRevision is tens of lines below its repoURL, not the
    # next line: `grep -A1` found nothing, and under `set -o pipefail` the
    # assignment aborted the script before the guard below could name the
    # reason. Read from the repoURL to the first targetRevision after it.
    NEW_VERSION=$(awk "/repoURL: 'https:\/\/kyverno.github.io\/kyverno'/,0" "$APPSET" \
      | grep -m1 targetRevision | sed "s/.*targetRevision: '//;s/'.*//") || true
  fi
fi

if [ -z "$NEW_VERSION" ]; then
  echo "ERROR: Could not determine new Kyverno version" >&2
  exit 1
fi

# Normalise once. Callers pass `3.9.1`, Renovate's data file passes `3.9.1`,
# and a human types `v3.9.1`; every use below assumes the bare form and adds
# its own `v` where one belongs.
NEW_VERSION="${NEW_VERSION}"

echo "==> Re-vendoring Kyverno v${NEW_VERSION}..."

# THE KUBERNETES VERSION COMES FROM THE WORKLOAD CLUSTERS' OWN DECLARATION.
#
# This script renders manifests for SPOKES, so the API target is the version
# workload clusters run -- `spec.topology.version` in the provider
# compositions -- not `versions.KubernetesVersion`, which is the MANAGEMENT
# cluster's. The two are independently owned and are not required to match; an
# earlier revision of this script read the management constant, which happened
# to agree today and would have coupled two declarations that may diverge.
#
# Read from every supported provider and require agreement. A provider-neutral
# render cannot target two Kubernetes versions, so a divergence is an explicit
# refusal here rather than an arbitrary choice made silently.
COMPOSITIONS=$(ls "${PROJECT_ROOT}"/manifests/providers/*/k8s/spokepool-*composition.yaml 2>/dev/null)
if [ -z "$COMPOSITIONS" ]; then
  echo "ERROR: no provider spokepool compositions found; the workload Kubernetes version is undeclared" >&2
  exit 1
fi
# shellcheck disable=SC2086 # COMPOSITIONS is a newline-separated list of paths
WORKLOAD_VERSIONS=$(awk '/^ *topology:/,/^ *workers:/' $COMPOSITIONS \
  | grep -oE 'version: v?[0-9]+\.[0-9]+\.[0-9]+' | sed 's/version: //;s/^v//' | sort -u)
if [ "$(printf '%s\n' "$WORKLOAD_VERSIONS" | grep -c .)" -ne 1 ]; then
  echo "ERROR: provider compositions declare more than one workload Kubernetes version:" >&2
  printf '  %s\n' $WORKLOAD_VERSIONS >&2
  echo "       A provider-neutral spoke render cannot target both. Reconcile them, or" >&2
  echo "       render per provider." >&2
  exit 1
fi
K8S_VERSION="$WORKLOAD_VERSIONS"
echo "    rendering against Kubernetes v${K8S_VERSION} (the workload clusters' declared version)"

CHART_REPO="https://kyverno.github.io/kyverno"
SPOKE_DIR="${PROJECT_ROOT}/manifests/spoke/spoke-catalog/infra/kyverno"
WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"' EXIT

# --- Render ---
echo "    Rendering Helm chart..."
# Use curl + local helm template for reliability (helm pull/template --repo
# has intermittent resolution issues with some Helm repos).
CHART_URL="${CHART_REPO}/kyverno-${NEW_VERSION}.tgz"
curl -fsSL "$CHART_URL" -o "$WORK_DIR/kyverno.tgz"
tar -xzf "$WORK_DIR/kyverno.tgz" -C "$WORK_DIR"
# THE CLEANUP JOBS ARE NOT DELIVERED, so their image is not overridden here.
#
# The chart defaults all seven cleanup jobs to docker.io/bitnami/kubectl, a
# namespace Bitnami withdrew from Docker Hub, and this script used to repoint
# webhooksCleanup at registry.k8s.io/kubectl with a tag pinned to the cluster
# version. That override reached nothing: every cleanup job is a helm lifecycle
# hook, and the hooks are filtered out below -- the vendored render contains no
# webhooksCleanup object and no kubectl image at all. Keeping it would have been
# a version dependency on a Job this file does not ship.
#
# The six cleanupJobs flags below stay disabled for the same reason they always
# were: they are hooks, and a left-in hook Job wedges the next upgrade.
helm template kyverno "$WORK_DIR/kyverno" \
  --namespace kyverno \
  --kube-version "${K8S_VERSION}" \
  --set crds.install=true \
  --set backgroundController.enabled=true \
  --set cleanupController.enabled=true \
  --set reportsController.enabled=true \
  --set webhooksCleanup.enabled=true \
  --set templating.enabled=false \
  --set policyReportsCleanup.enabled=false \
  --set cleanupJobs.admissionReports.enabled=false \
  --set cleanupJobs.clusterAdmissionReports.enabled=false \
  --set cleanupJobs.ephemeralReports.enabled=false \
  --set cleanupJobs.clusterEphemeralReports.enabled=false \
  --set cleanupJobs.updateRequests.enabled=false \
  > "$WORK_DIR/rendered.yaml"

# --- Split CRDs and controller resources (Python, not awk) ---
echo "    Splitting CRDs and controller resources..."
python3 -c "
import yaml, sys

with open('${WORK_DIR}/rendered.yaml') as f:
    docs = list(yaml.safe_load_all(f))

# Drop every helm lifecycle hook. These manifests are applied by ArgoCD from a
# kustomize build -- there is no helm, no release, and no moment for a hook to
# run at. What survives vendoring is an ordinary object applied at INSTALL
# time, which for these is actively wrong: the pre-delete hooks scale kyverno
# to zero and remove its webhooks, and a Job's pod template is immutable, so a
# left-in hook Job wedges the next upgrade. The reasoning is recorded at length
# in the vendored controller.yaml header; this is where it is enforced.
docs = [d for d in docs
        if d and not (d.get('metadata') or {}).get('annotations', {}).get('helm.sh/hook')]

crds = [d for d in docs if d and d.get('kind') == 'CustomResourceDefinition']
others = [d for d in docs if d and d.get('kind') != 'CustomResourceDefinition']

with open('${WORK_DIR}/crds.yaml', 'w') as f:
    yaml.dump_all(crds, f, default_flow_style=False, sort_keys=False, width=4096)

with open('${WORK_DIR}/controller.yaml', 'w') as f:
    yaml.dump_all(others, f, default_flow_style=False, sort_keys=False, width=4096)
"

# --- Apply Kustomize patch (sync-wave on CRDs) ---
# Control-plane tolerations are already applied by the root kustomization
# (spoke-catalog/infra/kustomization.yaml lines 98-125), so we only
# add the sync-wave annotation here.
echo "    Applying Kustomize patches..."
cat > "$WORK_DIR/kustomization.yaml" <<EOF
resources:
  - controller.yaml
  - crds.yaml
patches:
  - target:
      kind: CustomResourceDefinition
    patch: |-
      - op: add
        path: /metadata/annotations/argocd.argoproj.io~1sync-wave
        value: "-5"
EOF

cd "$WORK_DIR"
kustomize build . > "$WORK_DIR/patched.yaml"

# --- Re-split after patching ---
echo "    Re-splitting patched output..."
python3 -c "
import yaml

with open('${WORK_DIR}/patched.yaml') as f:
    docs = list(yaml.safe_load_all(f))

crds = [d for d in docs if d and d.get('kind') == 'CustomResourceDefinition']
others = [d for d in docs if d and d.get('kind') != 'CustomResourceDefinition']

with open('${WORK_DIR}/final-crds.yaml', 'w') as f:
    yaml.dump_all(crds, f, default_flow_style=False, sort_keys=False, width=4096)

with open('${WORK_DIR}/final-controller.yaml', 'w') as f:
    yaml.dump_all(others, f, default_flow_style=False, sort_keys=False, width=4096)
"

# --- Version labels ---
# Applied before the provenance headers, not after. These substitutions edit
# the content the digest covers, so running them afterwards recorded a hash
# of a file that no longer existed and left the verifier failing on a
# correctly vendored artifact.
echo "    Updating version labels..."
# THE CHART VERSION AND THE APPLICATION VERSION ARE DIFFERENT FACTS.
#
# app.kubernetes.io/version is "the current version of the application" by
# Kubernetes convention, and this rewrote it to the CHART version -- so a spoke
# running kyverno v1.19.2 was labelled 3.9.2, and anything reading the label to
# answer "which kyverno is running" got the wrong answer while the images beside
# it said otherwise. helm.sh/chart is the chart's own label and stays the chart
# version.
#
# Read from the chart's appVersion rather than from a second list here, so the
# upstream chart remains the only place the pairing is declared.
APP_VERSION=$(grep -m1 '^appVersion:' "$WORK_DIR/kyverno/Chart.yaml" \
  | sed 's/^appVersion:[[:space:]]*//;s/^v//;s/["'"'"']//g')
if [ -z "$APP_VERSION" ]; then
  echo "ERROR: could not read appVersion from the kyverno chart" >&2
  exit 1
fi
echo "    chart ${NEW_VERSION} packages application v${APP_VERSION}"
perl -pi -e "s|app\.kubernetes\.io/version: .*|app.kubernetes.io/version: v${APP_VERSION}|g" \
  "$WORK_DIR/final-controller.yaml"
perl -pi -e "s|helm\.sh/chart: kyverno-.*|helm.sh/chart: kyverno-${NEW_VERSION}|g" \
  "$WORK_DIR/final-controller.yaml"

# --- Provenance headers ---
echo "    Updating provenance headers..."
# The `v` prefix is load-bearing: renovate.json matches the provenance header
# with `# Vendored from helm chart (?<depName>\S+) v(?<currentValue>\S+)`.
# Writing the version bare here would silently drop these files out of
# Renovate's view, so strip any caller-supplied `v` and add exactly one.
# The curated comment block in each destination file is PRESERVED. It records
# why the hooks are excluded, why the report CRDs went away and which incidents
# produced those decisions -- knowledge that does not survive a re-render and
# cannot be recovered from the chart. Only the two machine-owned lines are
# rewritten: the `# Vendored from` line (renovate.json matches it with
# `# Vendored from helm chart (?<depName>\S+) v(?<currentValue>\S+)`, so the
# `v` prefix is load-bearing) and the `# sha256:` digest.
#
# A first-time vendor has no block to preserve, so a minimal one is written.
python3 - "$NEW_VERSION" "$SPOKE_DIR" "$WORK_DIR" <<'PYEOF'
import sys, hashlib, os

version, spoke_dir, work_dir = sys.argv[1], sys.argv[2], sys.argv[3]
header = f"# Vendored from helm chart kyverno v{version} (https://kyverno.github.io/kyverno)"
crd_note = ("# CRDs applied at sync-wave -5 so ArgoCD establishes them before\n"
            "# operators/resources that depend on them (ADR-023).")

for src, dst_name, default_note in (
    (f"{work_dir}/final-controller.yaml", "controller.yaml", None),
    (f"{work_dir}/final-crds.yaml", "crd.yaml", crd_note),
):
    body = open(src).read()

    dst = os.path.join(spoke_dir, dst_name)
    kept = []
    if os.path.exists(dst):
        for line in open(dst).read().split("\n"):
            if not line.startswith("#"):
                break
            kept.append(line)
    if kept:
        head = [header if l.startswith("# Vendored from helm chart")
                else ("# sha256: PLACEHOLDER" if l.startswith("# sha256:") else l)
                for l in kept]
    else:
        head = [header] + ([default_note] if default_note else []) + ["# sha256: PLACEHOLDER"]

    assembled = "\n".join(head) + "\n" + body

    # Hash exactly what verify-vendor-digest.sh hashes: `grep -v '^# '`, which
    # DROPS "# " prose but KEEPS bare "#" separator lines -- of which the
    # preserved header has several. Hashing the body alone therefore disagreed
    # with the verifier on every run. The digest line itself starts with "# ",
    # so substituting it afterwards cannot change the hash.
    lines = assembled.split("\n")
    if lines and lines[-1] == "":
        lines.pop()
    digest = hashlib.sha256(
        ("\n".join(l for l in lines if not l.startswith("# ")) + "\n").encode()
    ).hexdigest()

    assembled = assembled.replace("# sha256: PLACEHOLDER", f"# sha256: {digest}", 1)

    open(src, "w").write(assembled)
PYEOF

# --- Write ---
echo "    Writing vendored files..."
cp "$WORK_DIR/final-controller.yaml" "${SPOKE_DIR}/controller.yaml"
cp "$WORK_DIR/final-crds.yaml" "${SPOKE_DIR}/crd.yaml"

echo "==> Done. Vendored Kyverno v${NEW_VERSION}"
echo "    Verify with: bash scripts/validate/verify-vendor-digest.sh"
