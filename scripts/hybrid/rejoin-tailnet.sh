#!/usr/bin/env bash
# scripts/hybrid/rejoin-tailnet.sh
# ─────────────────────────────────────────────────────────────────────────────
# Re-join a cluster node to the tailnet after its Tailscale device was removed.
#
# Why this exists: deleting a device in the Tailscale admin console revokes that
# node's key permanently — there is no undelete. tailscaled on the node is then
# logged out, so peer routing dies while the interface keeps its old address
# locally. The visible symptom is asymmetric and easy to misread:
#
#     kubectl logs <pod on that node>     -> hangs, then times out
#     kubectl logs <pod on another node>  -> instant
#
# because kubelet still reaches the apiserver outbound (public LB), but the
# apiserver can no longer reach that kubelet over the tailnet.
#
# This script re-runs the node's own bootstrap sequence — the same commands the
# ClusterClass runs at first boot (providers/hetzner/base/spokepool-clusterclass-v1.yaml)
# — rather than inventing a parallel recovery path.
#
# Delivery is through the Kubernetes API, not SSH: CAPI-provisioned control
# planes have port 22 closed, and a node that has lost the tailnet is often
# unreachable by any other route. A privileged, hostPID pod pinned to the node
# nsenters into PID 1 and runs the commands on the host.
#
# Usage:
#   ./rejoin-tailnet.sh --node <node-name> [--cluster hub|spoke] [options]
#
#   --node NAME        Kubernetes node to rejoin (required).
#   --cluster C        hub | spoke. Default: auto-detect by finding the node.
#   --authkey KEY      Non-interactive auth. Default: interactive — the script
#                      prints a login URL for you to visit.
#   --authkey-file F   Read the key from a file instead (avoids process args).
#   --restart-kubelet  Restart kubelet if the tailnet IP changed, so the node
#                      re-registers its InternalIP. Prompted for if not passed.
#   --force-reauth     Force re-registration even if the node looks connected.
#   --timeout SECONDS  How long to wait for login (default 300).
#   --image IMAGE      Helper image (default busybox:1.36).
#   --keep-pod         Leave the helper pod for inspection.
#   --dry-run          Print the manifest and the host commands; change nothing.
#
# Examples:
#   ./rejoin-tailnet.sh --node hub-hybrid-dev-h2vst-pll5z
#   ./rejoin-tailnet.sh --node flatcar-spoke-node-1 --cluster spoke
#   ./rejoin-tailnet.sh --node <n> --authkey-file "${KAMARKAT_HOME:-$HOME/.kamarkat}/tailscale/authkey"
# ─────────────────────────────────────────────────────────────────────────────
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENV_FILE="${HERE}/home-lab.env"

NODE=""
CLUSTER=""
AUTHKEY=""
AUTHKEY_FILE=""
RESTART_KUBELET="ask"
FORCE_REAUTH="0"
LOGIN_TIMEOUT="300"
HELPER_IMAGE="${TS_REJOIN_IMAGE:-busybox:1.36}"
KEEP_POD="0"
DRY_RUN="0"

usage() { sed -n '2,44p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit "${1:-0}"; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    --node)            NODE="$2"; shift 2 ;;
    --cluster)         CLUSTER="$2"; shift 2 ;;
    --authkey)         AUTHKEY="$2"; shift 2 ;;
    --authkey-file)    AUTHKEY_FILE="$2"; shift 2 ;;
    --restart-kubelet) RESTART_KUBELET="yes"; shift ;;
    --no-restart-kubelet) RESTART_KUBELET="no"; shift ;;
    --force-reauth)    FORCE_REAUTH="1"; shift ;;
    --timeout)         LOGIN_TIMEOUT="$2"; shift 2 ;;
    --image)           HELPER_IMAGE="$2"; shift 2 ;;
    --keep-pod)        KEEP_POD="1"; shift ;;
    --dry-run)         DRY_RUN="1"; shift ;;
    -h|--help)         usage 0 ;;
    *) echo "ERROR: unknown argument: $1" >&2; usage 1 ;;
  esac
done

[[ -z "$NODE" ]] && { echo "ERROR: --node is required." >&2; usage 1; }

if [[ ! -f "$ENV_FILE" ]]; then
  echo "ERROR: $ENV_FILE not found." >&2
  echo "       Copy home-lab.env.example → home-lab.env and fill in real values." >&2
  exit 1
fi
# shellcheck source=/dev/null
source "$ENV_FILE"

: "${HUB_KUBECONFIG:?HUB_KUBECONFIG not set in home-lab.env}"
: "${HYBRID_SPOKE_NAME:?HYBRID_SPOKE_NAME not set in home-lab.env}"

if [[ ! -f "$HUB_KUBECONFIG" ]]; then
  echo "ERROR: hub kubeconfig not found at $HUB_KUBECONFIG" >&2
  exit 1
fi

if [[ -n "$AUTHKEY_FILE" ]]; then
  [[ -s "$AUTHKEY_FILE" ]] || { echo "ERROR: authkey file empty or missing: $AUTHKEY_FILE" >&2; exit 1; }
  AUTHKEY="$(tr -d '[:space:]' < "$AUTHKEY_FILE")"
fi

# ── Resolve the target cluster ───────────────────────────────────────────────
# The spoke kubeconfig lives on the hub as a CAPI-generated Secret; fetch it the
# same way provision-flatcar-worker.sh does rather than expecting a local copy.
SPOKE_KC=""
cleanup_kc() { [[ -n "$SPOKE_KC" && -f "$SPOKE_KC" ]] && rm -f "$SPOKE_KC"; }
trap cleanup_kc EXIT

fetch_spoke_kubeconfig() {
  SPOKE_KC="$(mktemp /tmp/rejoin-spoke-XXXXXX)"
  if ! kubectl --kubeconfig="$HUB_KUBECONFIG" get secret "${HYBRID_SPOKE_NAME}-kubeconfig" \
        -n platform-capi -o jsonpath='{.data.value}' 2>/dev/null | base64 -d > "$SPOKE_KC" 2>/dev/null \
        || [[ ! -s "$SPOKE_KC" ]]; then
    rm -f "$SPOKE_KC"; SPOKE_KC=""
    return 1
  fi
  return 0
}

node_exists() { kubectl --kubeconfig="$1" get node "$NODE" >/dev/null 2>&1; }

case "$CLUSTER" in
  hub)
    KC="$HUB_KUBECONFIG" ;;
  spoke)
    fetch_spoke_kubeconfig || { echo "ERROR: could not read the spoke kubeconfig from the hub." >&2; exit 1; }
    KC="$SPOKE_KC" ;;
  "")
    if node_exists "$HUB_KUBECONFIG"; then
      CLUSTER="hub"; KC="$HUB_KUBECONFIG"
    elif fetch_spoke_kubeconfig && node_exists "$SPOKE_KC"; then
      CLUSTER="spoke"; KC="$SPOKE_KC"
    else
      echo "ERROR: node '$NODE' not found in the hub or the spoke." >&2
      echo "       Known nodes (hub):" >&2
      kubectl --kubeconfig="$HUB_KUBECONFIG" get nodes -o name 2>/dev/null | sed 's|^|         |' >&2 || true
      exit 1
    fi ;;
  *) echo "ERROR: --cluster must be hub or spoke" >&2; exit 1 ;;
esac

node_exists "$KC" || { echo "ERROR: node '$NODE' not found in the $CLUSTER cluster." >&2; exit 1; }

IP_BEFORE="$(kubectl --kubeconfig="$KC" get node "$NODE" \
  -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}' 2>/dev/null || true)"

echo "=== Tailnet re-join ==="
echo "    Node:            ${NODE}"
echo "    Cluster:         ${CLUSTER}"
echo "    Registered IP:   ${IP_BEFORE:-<none>}"
echo "    Auth:            $([[ -n "$AUTHKEY" ]] && echo 'auth key' || echo 'interactive (a login URL will be printed)')"
echo ""

# ── Host command, run inside PID 1's namespaces ──────────────────────────────
# Mirrors the ClusterClass bootstrap: prefer /etc/tailscale-hostname when the
# node has one (all CAPI-provisioned nodes do), else fall back to the node name.
# BackendState is NOT sufficient on its own: 'tailscale status' still exits
# 0 while logged out, which is what makes this failure quiet.
# HAZARD: this heredoc is deliberately UNQUOTED so ${NODE}, ${AUTHKEY} and
# ${FORCE_REAUTH} expand into the host script. That means backticks and
# unescaped $( ) also execute -- on the OPERATOR'S machine, at render time,
# including during --dry-run. Escape every runtime substitution as \$( ), and
# never use backticks in this block, not even inside a comment.
read -r -d '' HOST_SCRIPT <<HOSTEOF || true
set -e
# Node classes install tailscale in different places, and nsenter drops us into
# a login-less shell whose PATH may not include /opt/bin:
#   CAPI Ubuntu (ClusterClass) -> on PATH, /usr/bin/tailscale
#   Flatcar home worker        -> /opt/bin/tailscale
# Same resolution idiom the provisioner already uses (provision-flatcar-worker.sh).
export PATH="/opt/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:\$PATH"
TS="\$(command -v tailscale 2>/dev/null || true)"
for _c in /opt/bin/tailscale /usr/bin/tailscale /usr/local/bin/tailscale; do
  [ -n "\$TS" ] && break
  [ -x "\$_c" ] && TS="\$_c"
done
if [ -z "\$TS" ]; then
  echo "ERROR: no tailscale binary found on this node."
  echo "       Looked on PATH and in /opt/bin, /usr/bin, /usr/local/bin."
  exit 1
fi
echo "tailscale binary: \$TS"

echo "--- current state ---"
CUR="\$("\$TS" ip -4 2>/dev/null || true)"
echo "tailnet IP: \${CUR:-<none>}"
JSON="\$("\$TS" status --json 2>/dev/null | tr -d ' \\r' || true)"
STATE="\$(printf '%s' "\$JSON" | grep -o '"BackendState":"[A-Za-z]*"' | head -1 | cut -d'"' -f4 || true)"
ONLINE="\$(printf '%s' "\$JSON" | grep -o '"Online":[a-z]*' | head -1 | cut -d: -f2 || true)"
echo "BackendState: \${STATE:-<unknown>}"
echo "Self.Online:  \${ONLINE:-<unknown>}"
HEALTH="\$(printf '%s' "\$JSON" | grep -o '"Health":\[[^]]*\]' | head -1 || true)"
[ -n "\$HEALTH" ] && [ "\$HEALTH" != '"Health":[]' ] && echo "Health: \$HEALTH"

# BackendState alone is NOT proof of authorisation. A device deleted in the
# admin console keeps serving from its cached netmap -- interface up, old IP
# held, state still "Running" -- until tailscaled next reaches the coordination
# server and is rejected. If that sync is failing, it may never notice. So
# require Self.Online too, and when in doubt fall through and re-run
# 'tailscale up', which is idempotent on a genuinely healthy node.
if [ "\${STATE}" = "Running" ] && [ "\${ONLINE}" = "true" ] && [ "${FORCE_REAUTH}" != "1" ]; then
  echo "ALREADY_CONNECTED"
  echo "This node is on the tailnet AND the control server confirms it online."
  echo "(Re-run with --force-reauth to re-register anyway.)"
  exit 0
fi

if [ "\${STATE}" = "Running" ]; then
  echo "NOTE: BackendState is Running but the control server does not confirm"
  echo "      this node online -- consistent with a device deleted in the admin"
  echo "      console. Re-authenticating."
fi

if [ -s /etc/tailscale-hostname ]; then
  TS_HOST="\$(cat /etc/tailscale-hostname)"
else
  TS_HOST="${NODE}"
fi
echo "--- re-authenticating as \${TS_HOST} ---"

set -- --hostname="\${TS_HOST}" --accept-dns=false
[ "${FORCE_REAUTH}" = "1" ] && set -- "\$@" --force-reauth
if [ -n "${AUTHKEY}" ]; then
  set -- "\$@" --authkey="${AUTHKEY}"
  echo "(using the auth key supplied on the command line)"
elif [ -s /etc/tailscale-authkey ]; then
  set -- "\$@" --authkey="\$(cat /etc/tailscale-authkey)"
  echo "(using /etc/tailscale-authkey from the node)"
else
  echo ">>> No auth key available. Visit the URL below to authenticate. <<<"
fi

"\$TS" up "\$@"

for i in \$(seq 1 30); do "\$TS" ip -4 >/dev/null 2>&1 && break; sleep 2; done
NEW="\$("\$TS" ip -4 2>/dev/null || true)"
echo "--- result ---"
echo "TAILNET_IP=\${NEW:-<none>}"
HOSTEOF

POD_NAME="ts-rejoin-$(echo "$NODE" | tr -cd 'a-z0-9-' | tail -c 40 | sed 's/^-*//')-$$"
POD_NAME="${POD_NAME:0:63}"

render_manifest() {
  cat <<PODEOF
apiVersion: v1
kind: Pod
metadata:
  name: ${POD_NAME}
  namespace: kube-system
  labels:
    app.kubernetes.io/name: rejoin-tailnet
    app.kubernetes.io/managed-by: rejoin-tailnet.sh
spec:
  nodeName: ${NODE}
  hostPID: true
  hostNetwork: true
  restartPolicy: Never
  tolerations:
    - operator: Exists
  containers:
    - name: rejoin
      image: ${HELPER_IMAGE}
      securityContext:
        privileged: true
      command: ["nsenter", "-t", "1", "-m", "-u", "-n", "-i", "/bin/sh", "-c"]
      args:
        - |
$(printf '%s\n' "$HOST_SCRIPT" | sed 's/^/          /')
PODEOF
}

if [[ "$DRY_RUN" == "1" ]]; then
  echo "── manifest (dry run, nothing applied) ──"
  render_manifest
  exit 0
fi

echo "!! This runs a privileged, hostPID pod on ${NODE} and nsenters into PID 1."
echo "!! That is a host-level operation. It is the only route available when a"
echo "!! node has lost the tailnet and SSH is closed."
read -r -p "Proceed? [y/N] " reply
[[ "$reply" =~ ^[Yy]$ ]] || { echo "Aborted."; exit 1; }

cleanup_pod() {
  if [[ "$KEEP_POD" == "1" ]]; then
    echo "Helper pod kept: kubectl --kubeconfig=... -n kube-system delete pod ${POD_NAME}"
  else
    kubectl --kubeconfig="$KC" -n kube-system delete pod "$POD_NAME" \
      --ignore-not-found=true --wait=false >/dev/null 2>&1 || true
  fi
  cleanup_kc
}
trap cleanup_pod EXIT

kubectl --kubeconfig="$KC" -n kube-system delete pod "$POD_NAME" --ignore-not-found=true >/dev/null 2>&1 || true
render_manifest | kubectl --kubeconfig="$KC" apply -f - >/dev/null

echo "→ Helper pod ${POD_NAME} applied. Streaming host output..."
echo "  (If a login URL appears, open it in a browser — the node waits for you.)"
echo "─────────────────────────────────────────────────────────────────────────"

for _ in $(seq 1 60); do
  phase="$(kubectl --kubeconfig="$KC" -n kube-system get pod "$POD_NAME" \
            -o jsonpath='{.status.phase}' 2>/dev/null || true)"
  [[ "$phase" == "Running" || "$phase" == "Succeeded" || "$phase" == "Failed" ]] && break
  sleep 2
done

OUT_FILE="$(mktemp /tmp/rejoin-out-XXXXXX)"
timeout "$LOGIN_TIMEOUT" kubectl --kubeconfig="$KC" -n kube-system logs -f "$POD_NAME" 2>&1 | tee "$OUT_FILE" || true
echo "─────────────────────────────────────────────────────────────────────────"

if grep -q "ALREADY_CONNECTED" "$OUT_FILE"; then
  rm -f "$OUT_FILE"
  echo "✓ Node was already on the tailnet — nothing changed."
  exit 0
fi

IP_AFTER="$(grep -o 'TAILNET_IP=[0-9.]*' "$OUT_FILE" | tail -1 | cut -d= -f2 || true)"
rm -f "$OUT_FILE"

if [[ -z "$IP_AFTER" ]]; then
  echo "✗ Could not confirm a tailnet IP. The login may not have completed, or the"
  echo "  auth key was expired or single-use. Re-run, or pass --authkey with a fresh"
  echo "  reusable key from the Tailscale admin console."
  exit 1
fi

echo "✓ Node is back on the tailnet at ${IP_AFTER}"

if [[ "$IP_AFTER" == "$IP_BEFORE" ]]; then
  echo "✓ Address unchanged (${IP_BEFORE}) — the node's InternalIP is still correct."
  echo "  No kubelet restart needed."
  exit 0
fi

# The address moved. kubelet registered the OLD one, so the apiserver still has
# a stale InternalIP and cannot reach this kubelet. dynamic-node-ip.sh re-derives
# --node-ip from the live tailnet address; it runs as a kubelet ExecStartPre, so
# restarting kubelet is what applies it.
echo ""
echo "!  The tailnet address CHANGED: ${IP_BEFORE:-<none>} → ${IP_AFTER}"
echo "!  The node still advertises the old InternalIP, so the apiserver cannot"
echo "!  reach its kubelet. Restarting kubelet re-registers it via the node's"
echo "!  dynamic-node-ip.sh helper (/opt/bin or /usr/local/bin, per node class)."
if kubectl --kubeconfig="$KC" get node "$NODE" \
     -o jsonpath='{.metadata.labels}' 2>/dev/null | grep -q 'control-plane'; then
  echo "!"
  echo "!  ${NODE} is a CONTROL-PLANE node. Restarting kubelet bounces the static"
  echo "!  pods, so the apiserver and etcd restart with it. On a single-replica"
  echo "!  control plane expect a short API outage."
fi

if [[ "$RESTART_KUBELET" == "ask" ]]; then
  read -r -p "Restart kubelet on ${NODE} now? [y/N] " kreply
  [[ "$kreply" =~ ^[Yy]$ ]] && RESTART_KUBELET="yes" || RESTART_KUBELET="no"
fi

if [[ "$RESTART_KUBELET" != "yes" ]]; then
  echo ""
  echo "Skipped. Until kubelet restarts, 'kubectl logs/exec' against pods on"
  echo "${NODE} will keep timing out. Re-run with --restart-kubelet when ready."
  exit 0
fi

RESTART_POD="${POD_NAME}-kubelet"
cat <<PODEOF | kubectl --kubeconfig="$KC" apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: ${RESTART_POD}
  namespace: kube-system
  labels:
    app.kubernetes.io/name: rejoin-tailnet
    app.kubernetes.io/managed-by: rejoin-tailnet.sh
spec:
  nodeName: ${NODE}
  hostPID: true
  hostNetwork: true
  restartPolicy: Never
  tolerations:
    - operator: Exists
  containers:
    - name: restart-kubelet
      image: ${HELPER_IMAGE}
      securityContext:
        privileged: true
      command: ["nsenter", "-t", "1", "-m", "-u", "-n", "-i", "/bin/sh", "-c"]
      args:
        - |
          set -e
          # Flatcar home workers keep it in /opt/bin; CAPI Ubuntu nodes in
          # /usr/local/bin. Probe both rather than assume a node class.
          NODEIP=""
          for c in /opt/bin/dynamic-node-ip.sh /usr/local/bin/dynamic-node-ip.sh; do
            [ -x "$c" ] && NODEIP="$c" && break
          done
          if [ -n "$NODEIP" ]; then
            echo "node-ip helper: $NODEIP"
            systemctl daemon-reload
            "$NODEIP"
          else
            echo "WARNING: no dynamic-node-ip.sh found in /opt/bin or /usr/local/bin;"
            echo "         kubelet may re-register with the wrong --node-ip."
          fi
          systemctl restart kubelet
          echo "kubelet restarted"
PODEOF

echo "→ Restarting kubelet on ${NODE}..."
sleep 5
timeout 120 kubectl --kubeconfig="$KC" -n kube-system logs -f "$RESTART_POD" 2>&1 | sed 's/^/    /' || true
kubectl --kubeconfig="$KC" -n kube-system delete pod "$RESTART_POD" \
  --ignore-not-found=true --wait=false >/dev/null 2>&1 || true

echo ""
echo "→ Waiting for the node to re-register..."
for _ in $(seq 1 30); do
  NOW="$(kubectl --kubeconfig="$KC" get node "$NODE" \
    -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}' 2>/dev/null || true)"
  [[ "$NOW" == "$IP_AFTER" ]] && break
  sleep 4
done

echo ""
echo "=== Result ==="
kubectl --kubeconfig="$KC" get node "$NODE" \
  -o custom-columns='NAME:.metadata.name,STATUS:.status.conditions[-1].type,INTERNAL-IP:.status.addresses[?(@.type=="InternalIP")].address' 2>&1
echo ""
echo "Verify the path that was broken — this should return promptly, not hang:"
echo "  kubectl --kubeconfig=<${CLUSTER}> logs -n kube-system \\"
echo "    \$(kubectl --kubeconfig=<${CLUSTER}> get pods -n kube-system \\"
echo "        --field-selector spec.nodeName=${NODE} -o name | head -1) --tail=2"
