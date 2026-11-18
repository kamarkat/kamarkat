#!/usr/bin/env bash
# The API server must be able to reach every node's kubelet on :10250.
#
# On a hybrid cell each node's InternalIP is its TAILNET address (ADR-046
# invariant 6), so this one reachability property sits underneath three things
# that otherwise fail with unrelated-looking symptoms:
#
#   - kubectl logs/exec/port-forward against the node (i/o timeout);
#   - Cilium's VXLAN tunnel, whose endpoint is derived from that same
#     InternalIP — break it and cross-node pod traffic silently stops;
#   - consequently CoreDNS, when a pod and the DNS replicas sit on different
#     nodes. That surfaces as EAI_AGAIN inside application containers.
#
# This has happened: a tailscale DaemonSet ran alongside the tailscaled that the
# ClusterClass installs natively on the control plane. Both share the host netns
# and therefore the same tailscale0, so the DaemonSet's daemon stripped the
# tailnet addresses off the interface. The node kept ADVERTISING its tailnet
# InternalIP while that address was no longer configured anywhere, so every
# connection to it timed out. Nothing in the failing components named tailscale:
# what was visible was Infisical CrashLoopBackOff on "Boot up migration failed",
# with a healthy database and a healthy pooler.
#
# Checking node Ready is not a substitute — kubelet reports Ready over its own
# OUTBOUND connection to the API server, which keeps working the whole time.
validate_kubelet_reachability() {
    section "API server → kubelet reachability (ADR-046 invariant 6)"

    local nodes node ip unreachable=0
    nodes=$(kc get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')

    if [[ -z "$nodes" ]]; then
        hard_fail "no nodes returned — cannot validate kubelet reachability"
        return 0
    fi

    while IFS= read -r node; do
        [[ -z "$node" ]] && continue
        ip=$(kc get node "$node" -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')

        # proxy/healthz goes API server -> kubelet, the exact path that breaks.
        if kc get --raw "/api/v1/nodes/${node}/proxy/healthz" >/dev/null; then
            pass "kubelet reachable on $node (InternalIP ${ip:-unknown})"
        else
            unreachable=1
            soft_fail "API server cannot reach kubelet on $node (InternalIP ${ip:-unknown})"
        fi
    done <<< "$nodes"

    if (( unreachable )); then
        note "the node still reports Ready — that is its outbound path and proves nothing here"
        note "on hybrid, confirm the InternalIP is actually configured: ip -4 addr show tailscale0"
        note "check for a second tailscaled (a DaemonSet) competing with the ClusterClass one"
    fi
}
