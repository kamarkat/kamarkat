#!/usr/bin/env bash
# scope: spoke
# ADR-107: the sandboxed runtime is in force where it is offered. LIVE checks.
#
#   R1  RuntimeClass `sandboxed` exists, with handler runsc and a node selector
#   R2  every node offering it (label sandbox.kamarkat.io/runtime=sandboxed) has
#       a Ready installer pod, i.e. runsc and its shim are on the host and
#       containerd reports the handler
#   R3  every offering node's Cilium agent runs with the socket-LB bypass: the
#       CiliumNodeConfig takes effect only after the agent restarts, and a
#       gVisor pod on a node without it cannot resolve a Service address
#   R4  every running sandboxed or peer pod is under the RuntimeClass
validate_adr107_sandbox_runtime() {
    section "ADR-107 sandboxed runtime"
    ensure_spoke_kubeconfig || { soft_fail "no spoke kubeconfig; ADR-107 is a spoke property"; return 0; }

    local rc
    rc=$(kc_spoke get runtimeclass sandboxed -o jsonpath='{.handler} {.scheduling.nodeSelector.sandbox\.zero-ops\.io/runtime}')
    if [[ "$rc" == "runsc sandboxed" ]]; then
        pass "R1 RuntimeClass sandboxed: handler runsc, scheduled onto offering nodes only"
    else
        hard_fail "R1 RuntimeClass sandboxed is missing or wrong (got: '${rc:-absent}'): Sandboxed jobs and every session peer stay Pending"
        return 0
    fi

    local nodes node
    nodes=$(kc_spoke get nodes -l sandbox.kamarkat.io/runtime=sandboxed -o jsonpath='{.items[*].metadata.name}')
    if [[ -z "$nodes" ]]; then
        soft_fail "R2 no node offers the sandboxed runtime yet (label sandbox.kamarkat.io/runtime=sandboxed): Sandboxed jobs stay Pending by design"
        return 0
    fi
    for node in $nodes; do
        local ready
        ready=$(kc_spoke -n kube-system get pods -l app.kubernetes.io/name=sandboxed-runtime-installer \
            --field-selector "spec.nodeName=$node" -o jsonpath='{.items[0].status.containerStatuses[0].ready}')
        if [[ "$ready" == "true" ]]; then
            pass "R2 $node: runtime installed and registered with containerd"
        else
            hard_fail "R2 $node offers the sandboxed runtime but its installer is not Ready (${ready:-no pod}): read its 'install' init container log"
        fi

        local cil cov
        cil=$(kc_spoke -n kube-system get pods -l k8s-app=cilium --field-selector "spec.nodeName=$node" -o name | head -1)
        cov=$(kc_spoke -n kube-system exec "$cil" -c cilium-agent -- cilium-dbg status --verbose 2>/dev/null | grep -i "Socket LB Coverage" | head -1)
        if grep -qi "hostns" <<< "$cov"; then
            pass "R3 $node: Cilium resolves Services per packet for pods (${cov// /})"
        else
            hard_fail "R3 $node: Cilium socket-LB coverage is '${cov:-unknown}', not host-namespace-only. Delete ${cil#pod/} so the CiliumNodeConfig applies; until then gVisor pods there cannot reach any Service, DNS included"
        fi
    done

    local out
    out=$(kc_spoke get pods -A -o json | python3 -c '
import json, sys
for p in json.load(sys.stdin)["items"]:
    if p.get("status", {}).get("phase") != "Running":
        continue
    labels = p["metadata"].get("labels") or {}
    if "compute.nutgraf.in/session-peer" not in labels:
        continue
    rc = p["spec"].get("runtimeClassName")
    where = "%s/%s" % (p["metadata"]["namespace"], p["metadata"]["name"])
    print(("PASS R4 %s runs sandboxed" % where) if rc == "sandboxed" else ("FAIL R4 session peer %s runs under %r, not sandboxed" % (where, rc)))
')
    local line
    while IFS= read -r line; do
        case "$line" in
            PASS\ *) pass "${line#PASS }" ;;
            FAIL\ *) hard_fail "${line#FAIL }" ;;
        esac
    done <<< "$out"
}
