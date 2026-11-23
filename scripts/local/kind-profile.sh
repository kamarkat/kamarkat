#!/usr/bin/env bash
# The local Kind profile: everything a local stack needs from the platform
# beyond the operator itself (see scripts/local/README.md).
#
#   1-4  ADR-107: the sandboxed runtime on every Kind node, so Isolation:
#        Sandboxed jobs and session peers (renderers) run isolated locally.
#   5-6  ADR-108: preview-access in local development mode.
#
#   KUBECONFIG=<file> OPERATOR_IMAGE=<image> \
#   LOCAL_NAMESPACE=<ns> LOCAL_APP_ORIGINS=<origin>[,<origin>...] LOCAL_SUBJECT=<sub> \
#   LOCAL_PREVIEW_BASE='http://{host}:<port>' scripts/local/kind-profile.sh
#
# Every variable is required and named if missing. Idempotent. Fails, naming the
# step, if anything cannot be provided -- it never leaves a cluster that claims a
# capability it cannot serve.
#
# The operator for this profile is scripts/local/kind/operator (config/default
# with --network-policy=none); install that instead of config/default.
set -euo pipefail

ZERO_OPS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
LABEL="sandbox.kamarkat.io/runtime=sandboxed"
log() { echo "[kind-profile] $*"; }
fail() { echo "[kind-profile] FAILED: $*" >&2; exit 1; }

for v in OPERATOR_IMAGE LOCAL_NAMESPACE LOCAL_APP_ORIGINS LOCAL_SUBJECT LOCAL_PREVIEW_BASE; do
    [[ -n "${!v:-}" ]] || fail "$v is required (see scripts/local/README.md)"
done
[[ "$LOCAL_PREVIEW_BASE" == *"{host}"* ]] || fail "LOCAL_PREVIEW_BASE must contain {host}, e.g. http://{host}:3010"

ctx="$(kubectl config current-context)"
[[ "$ctx" == kind-* ]] || fail "current context '$ctx' is not a Kind cluster; this profile is for local Kind only"

log "1/6 sandboxed runtime component"
kubectl kustomize "$ZERO_OPS_DIR/scripts/local/kind/runtime" | kubectl apply -f -

log "2/6 offering the runtime on every Kind node ($LABEL)"
kubectl label nodes --all "$LABEL" --overwrite >/dev/null

log "3/6 waiting for the installer on every node"
if ! kubectl -n kube-system rollout status ds/sandboxed-runtime-installer --timeout=300s; then
    kubectl -n kube-system logs -l app.kubernetes.io/name=sandboxed-runtime-installer -c install --tail=40 >&2 || true
    fail "the runtime installer did not become Ready (its log is above)"
fi

log "4/6 a sandboxed pod runs under the user-space kernel"
probe=adr107-runtime-check
kubectl -n kube-system delete pod "$probe" --ignore-not-found --wait >/dev/null
kubectl -n kube-system apply -f - >/dev/null <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: $probe
spec:
  runtimeClassName: sandboxed
  restartPolicy: Never
  containers:
    - name: check
      image: busybox:1.37.0
      command: ["dmesg"]
      resources:
        limits: {cpu: 100m, memory: 64Mi}
YAML
if ! kubectl -n kube-system wait pod/"$probe" --for=jsonpath='{.status.phase}'=Succeeded --timeout=120s >/dev/null; then
    kubectl -n kube-system describe pod "$probe" | tail -20 >&2 || true
    kubectl -n kube-system delete pod "$probe" --ignore-not-found >/dev/null
    fail "a pod under RuntimeClass sandboxed did not complete (events above)"
fi
out="$(kubectl -n kube-system logs "$probe")"
kubectl -n kube-system delete pod "$probe" --ignore-not-found >/dev/null
grep -qi "gvisor" <<< "$out" || fail "the sandboxed pod ran, but not under gVisor: dmesg shows no gVisor banner"
log "ready: RuntimeClass sandboxed is served by gVisor on: $(kubectl get nodes -l "$LABEL" -o jsonpath='{.items[*].metadata.name}')"

log "5/6 preview-access in local development mode"
# The application's values are added in a generated layer over the static
# overlay, so the overlay in the repository names no product.
layer="$(mktemp -d)"
trap 'rm -rf "$layer"' EXIT
# Kustomize accepts a base only by a path relative to the kustomization.
overlay_rel="$(python3 -c 'import os,sys; r=os.path.realpath; print(os.path.relpath(r(sys.argv[1]), r(sys.argv[2])))' \
    "$ZERO_OPS_DIR/scripts/local/kind/preview-access" "$layer")"
image_name="${OPERATOR_IMAGE%:*}"; image_tag="${OPERATOR_IMAGE##*:}"
[[ "$image_name" != "$OPERATOR_IMAGE" ]] || fail "OPERATOR_IMAGE '$OPERATOR_IMAGE' must be <name>:<tag>"
cat > "$layer/kustomization.yaml" <<YAML
resources:
  - $overlay_rel
images:
  - name: ephemeral-job-operator
    newName: $image_name
    newTag: "$image_tag"
patches:
  - target: {kind: Deployment, name: preview-access}
    patch: |
      - {op: add, path: /spec/template/spec/containers/0/env/-, value: {name: LOCAL_NAMESPACE, value: "$LOCAL_NAMESPACE"}}
      - {op: add, path: /spec/template/spec/containers/0/env/-, value: {name: LOCAL_APP_ORIGINS, value: "$LOCAL_APP_ORIGINS"}}
      - {op: add, path: /spec/template/spec/containers/0/env/-, value: {name: LOCAL_SUBJECT, value: "$LOCAL_SUBJECT"}}
      - {op: add, path: /spec/template/spec/containers/0/env/-, value: {name: LOCAL_PREVIEW_BASE, value: "$LOCAL_PREVIEW_BASE"}}
YAML
kubectl kustomize --load-restrictor LoadRestrictionsNone "$layer" | kubectl apply -f -
if ! kubectl -n platform-ops rollout status deploy/preview-access --timeout=180s; then
    kubectl -n platform-ops logs deploy/preview-access --tail=30 >&2 || true
    fail "preview-access did not become Ready in local mode (its log is above)"
fi

log "6/6 preview-access answers on its preview port"
pf_port=18081
kubectl -n platform-ops port-forward svc/preview-access "$pf_port:8081" >/dev/null 2>&1 &
pf=$!
trap 'kill $pf 2>/dev/null || true; rm -rf "$layer"' EXIT
code=""
for _ in $(seq 1 20); do
    code="$(curl -s -o /dev/null -w '%{http_code}' -H 'Host: p-none.preview.localhost' "http://127.0.0.1:$pf_port/" || true)"
    [[ "$code" != "000" && -n "$code" ]] && break
    sleep 1
done
# An unknown preview host is `ended`: 403 with X-Preview-Access. Anything else
# means the service is not answering as preview-access.
[[ "$code" == "403" ]] || fail "preview-access answered $code for an unknown preview host, not 403"
log "ready: preview-access in local mode; previews at $LOCAL_PREVIEW_BASE (forward svc/preview-access <port>:8081)"
