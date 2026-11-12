#!/usr/bin/env bash
# ADR-107: Rollback sandboxed runtime from a node.
# Removes the zero-ops runtime layer on Flatcar (read-only config)
# or the managed block on writable configs (Kind/Ubuntu).
set -euo pipefail

NODE="${1:?usage: $0 <node-name>}"
KC="${KUBECONFIG:-${SPOKE_KUBECONFIG:-${KAMARKAT_HOME:-$HOME/.kamarkat}/run/kubeconfig/nutgraf-01.kubeconfig}}"

echo "=== Rolling back sandbox runtime on node: $NODE ==="

ROLLBACK_CMD='
BEGIN="# BEGIN zero-ops ADR-107 sandboxed runtime"
END="# END zero-ops ADR-107 sandboxed runtime"
LEGACY="# zero-ops ADR-107 sandboxed runtime"

FLATCAR_CHANGED=0
if [ -f /host/etc/containerd/zero-ops-runtime.toml ] || [ -f /host/etc/containerd/zero-ops.d/runsc.toml ] || [ -f /host/etc/systemd/system/containerd.service.d/10-zero-ops-runtime.conf ]; then
  echo "Removing zero-ops runtime layer files on Flatcar..."
  rm -f /host/etc/containerd/zero-ops-runtime.toml
  rm -f /host/etc/containerd/zero-ops.d/runsc.toml
  rm -f /host/etc/systemd/system/containerd.service.d/10-zero-ops-runtime.conf
  rmdir /host/etc/containerd/zero-ops.d 2>/dev/null || true
  rmdir /host/etc/systemd/system/containerd.service.d 2>/dev/null || true
  FLATCAR_CHANGED=1
fi

WRITABLE_CHANGED=0
for cfg in /host/etc/containerd/config.toml /host/etc/containerd/config.k8s.toml; do
  if [ -f "$cfg" ] && [ -w "$cfg" ]; then
    if grep -q "$BEGIN\|$LEGACY" "$cfg"; then
      echo "Removing managed block from $cfg..."
      awk -v b="$BEGIN" -v e="$END" -v l="$LEGACY" '\''
        $0==b {skip=1} $0==l {legacy=1}
        !skip && !legacy {print}
        $0==e {skip=0}'\'' "$cfg" > "$cfg.tmp"
      mv "$cfg.tmp" "$cfg"
      WRITABLE_CHANGED=1
    fi
  fi
done

if [ "$FLATCAR_CHANGED" -eq 1 ] || [ "$WRITABLE_CHANGED" -eq 1 ]; then
  echo "Reloading systemd and restarting containerd..."
  chroot /host systemctl daemon-reload 2>/dev/null || true
  chroot /host systemctl restart containerd
  echo "Containerd restarted."
else
  echo "No zero-ops runtime configuration found to roll back."
fi
'

echo "Launching sysadmin debug pod on node $NODE..."
kubectl --kubeconfig "$KC" debug "node/$NODE" --image=busybox:1.37.0 --profile=sysadmin -q -- sh -c "$ROLLBACK_CMD"

# Delete the debug pod
DEBUG_POD=$(kubectl --kubeconfig "$KC" get pods -A | grep "node-debugger-$NODE" | awk '{print $2}' | tail -1 || true)
if [ -n "$DEBUG_POD" ]; then
  echo "Deleting debug pod $DEBUG_POD..."
  kubectl --kubeconfig "$KC" delete pod "$DEBUG_POD" --ignore-not-found=true >/dev/null 2>&1
fi
echo "Rollback completed on $NODE."
