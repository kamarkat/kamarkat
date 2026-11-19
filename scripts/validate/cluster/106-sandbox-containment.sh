#!/usr/bin/env bash
# scope: spoke
# ADR-106: every sandbox is contained by default. These are the LIVE checks.
#
# Each one targets a way containment has failed silently on this platform
# before. A Cilium policy that admits too much reports Healthy, and so does one
# that selects nothing. So the checks read what is actually in force, not what
# the repository says.
#
#   S1  the sandbox default-deny has an ingress section (no section = allow-all),
#       the private-range egress deny, DNS through the proxy, and NO internet
#       grant of its own (a sandbox reaches only what its app declares)
#   S2  every namespace running sandboxes admits its SDK, so S1's deny cannot
#       cut a live session
#   S3  the tenant-wide grants (world:443, plain DNS) exclude sandboxes and peers
#   S4  every running sandbox or peer container carries cpu, memory and
#       ephemeral-storage limits
#   S5  name-based egress is in force: the agents' FQDN cache holds names. An
#       empty cache means every declared host is unreachable
validate_adr106_sandbox_containment() {
    section "ADR-106 sandbox containment"
    ensure_spoke_kubeconfig || { soft_fail "no spoke kubeconfig; ADR-106 is a spoke property"; return 0; }

    local out
    out=$(
        {
            kc_spoke get ccnp -o json
            echo '---SPLIT---'
            kc_spoke get cnp -A -o json
            echo '---SPLIT---'
            kc_spoke get pods -A -o json
        } | python3 -c '
import json, sys
ccnp, cnp, pods = (json.loads(x) for x in sys.stdin.read().split("---SPLIT---"))
by = {p["metadata"]["name"]: p.get("spec", {}) for p in ccnp["items"]}

s = by.get("agent-sandbox-default-deny")
if s is None:
    print("FAIL S1 agent-sandbox-default-deny is not on this spoke: sandboxes are governed by nothing")
else:
    print(("PASS" if s.get("ingress") else "FAIL") + " S1 agent-sandbox-default-deny has an ingress section"
          + ("" if s.get("ingress") else " -- without one, every pod in every tenant can reach every sandbox port"))
    world = [r for r in s.get("egress", []) if "world" in r.get("toEntities", []) or r.get("toCIDR") or r.get("toCIDRSet")]
    print(("FAIL S1 agent-sandbox-default-deny grants internet egress of its own (%d rule(s)) -- sandboxes reach only declared hosts" % len(world)) if world else "PASS S1 agent-sandbox-default-deny grants no internet egress of its own")
    dns = [r for r in s.get("egress", []) for t in r.get("toPorts", []) if (t.get("rules") or {}).get("dns")]
    print("PASS S1 sandbox DNS goes through the proxy" if dns else "FAIL S1 sandbox DNS has no L7 dns rule: the FQDN cache cannot fill and every declared host is unreachable")
    cidrs = {c.get("cidr") for r in s.get("egressDeny", []) for c in r.get("toCIDRSet", [])}
    need = {"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16"}
    print(("PASS" if need <= cidrs else "FAIL") + " S1 sandbox egress denies private and link-local ranges"
          + ("" if need <= cidrs else " (missing %s)" % ", ".join(sorted(need - cidrs))))

def is_sandbox(p):
    return "agents.x-k8s.io/sandbox" in (p["metadata"].get("labels") or {})
def is_peer(p):
    return "compute.nutgraf.in/session-peer" in (p["metadata"].get("labels") or {})

live = [p for p in pods["items"] if p.get("status", {}).get("phase") == "Running"]
sandbox_ns = sorted({p["metadata"]["namespace"] for p in live if is_sandbox(p)})
admits = {(p["metadata"]["namespace"], p["metadata"]["name"]) for p in cnp["items"]}
for ns in sandbox_ns:
    ok = (ns, "sandbox-ingress-from-sdk") in admits
    print(("PASS" if ok else "FAIL") + " S2 %s admits its SDK into its sandboxes" % ns
          + ("" if ok else " -- the sandbox ingress deny is cutting the sessions in this namespace"))
if not sandbox_ns:
    print("NOTE S2 no sandbox is running; the SDK admission is unverified on live sessions")

for name in ("tenant-identity-endpoint-egress", "tenant-dns-egress"):
    sel = (by.get(name) or {}).get("endpointSelector", {})
    ex = {e["key"] for e in sel.get("matchExpressions", []) if e.get("operator") == "DoesNotExist"}
    ok = {"agents.x-k8s.io/sandbox", "compute.nutgraf.in/session-peer"} <= ex
    print(("PASS" if ok else "FAIL") + " S3 %s excludes sandboxes and session peers" % name
          + ("" if ok else " -- its grants are added to theirs (tenant-identity-endpoint-egress carries world:443)"))

for p in live:
    if not (is_sandbox(p) or is_peer(p)):
        continue
    for c in p["spec"].get("containers", []) + p["spec"].get("initContainers", []):
        lim = (c.get("resources") or {}).get("limits") or {}
        miss = [r for r in ("cpu", "memory", "ephemeral-storage") if r not in lim]
        where = "%s/%s %s" % (p["metadata"]["namespace"], p["metadata"]["name"], c["name"])
        print(("FAIL S4 %s has no %s limit" % (where, ", ".join(miss))) if miss else ("PASS S4 %s is bounded" % where))
'
    )

    local line
    while IFS= read -r line; do
        case "$line" in
            PASS\ *) pass "${line#PASS }" ;;
            FAIL\ *) hard_fail "${line#FAIL }" ;;
            NOTE\ *) note "${line#NOTE }" ;;
        esac
    done <<< "$out"

    # S5: name-based egress works only once the agent's FQDN cache holds names.
    # Empty means DNS is not reaching the proxy, and every declared host is
    # unreachable from every sandbox: a failure, not a state.
    local cil names
    cil=$(kc_spoke -n kube-system get pods -l k8s-app=cilium -o name | head -1)
    if [[ -n "$cil" ]]; then
        names=$(kc_spoke -n kube-system exec "$cil" -c cilium-agent -- cilium-dbg fqdn cache list 2>/dev/null | tail -n +2 | wc -l | tr -d ' ')
        if [[ "${names:-0}" -gt 0 ]]; then
            pass "S5 the FQDN cache holds $names entries on ${cil#pod/}: name-based egress can enforce"
        else
            hard_fail "S5 the FQDN cache is empty on ${cil#pod/}: no sandbox can reach any declared host (ADR-106 §1; prove the datapath with probes/106-sandbox-dns-visibility-probe.yaml)"
        fi
    fi
}
