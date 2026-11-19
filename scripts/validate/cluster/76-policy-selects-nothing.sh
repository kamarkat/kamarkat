#!/usr/bin/env bash
# scope: every-cluster
# A network policy that selects no endpoints is reported Healthy. It is simply
# inert — enforcing nothing, denying nothing, and looking exactly like a policy
# that works.
#
# On 2026-08-31 `agent-sandbox-default-deny` selected pods by a label the SDK
# writes into the Sandbox CR's podTemplate. The agent-sandbox controller owns
# that label space and replaces it, so the label never reached a pod: the
# policy governed ZERO pods and the "no cluster-internal access" its own header
# describes had never once been in force. Nothing reported it, because a CCNP
# selecting nothing is not an error.
#
# The same shape appeared twice more that day — a certificate authority that
# existed but could not issue, and toFQDNs rules matching no address. The common
# property is a component reporting healthy while doing nothing, which is the
# class this check exists to break.
#
# Scoped to policies the PLATFORM owns. A tenant policy may legitimately select
# nothing while its workload is scaled to zero, and failing on that would train
# people to ignore this check.
# EVERY CLUSTER, NAMED. This read `${SPOKE_KUBECONFIG:-$HUB_KUBECONFIG}`, so the
# same assertions judged a workload cluster or the management cluster depending
# on an environment variable, and the result never said which. A policy inert on
# the management cluster passed while a workload cluster was being reported on,
# and the other way round.
#
# One assertion, repeated per cluster, with the cluster in every message. The
# management cluster is always a subject; a workload cluster is one when it is
# reachable, which the runner has already established before this module runs.
validate_policy_selects_nothing() {
    local kc name
    for kc in "$HUB_KUBECONFIG" "${SPOKE_KUBECONFIG:-}"; do
        [[ -z "$kc" || ! -r "$kc" ]] && continue
        if [[ "$kc" == "$HUB_KUBECONFIG" ]]; then name="the management cluster"; else name="${SPOKEPOOL_NAME:-the workload cluster}"; fi
        _policy_selects_nothing_on "$kc" "$name"
    done
}

_policy_selects_nothing_on() {
    local kubeconfig="$1" cluster="$2"
    section "Network policies select at least one endpoint on $cluster"

    local out
    out=$(kubectl --kubeconfig="$kubeconfig" get ciliumclusterwidenetworkpolicies -o json 2>/dev/null | python3 -c '
import json, sys

try:
    items = json.load(sys.stdin).get("items", [])
except Exception:
    sys.exit(0)

for p in items:
    name = p["metadata"]["name"]
    sel = p.get("spec", {}).get("endpointSelector")
    # An empty selector matches everything, which is a different (and valid)
    # design. Only a selector that names something is expected to match.
    if not sel or not (sel.get("matchLabels") or sel.get("matchExpressions")):
        continue
    # Cilium reports what a policy actually resolved to. A policy Cilium has not
    # yet processed has no status and must not be reported as inert.
    st = p.get("status") or {}
    if not st:
        continue
    print("%s\t%s" % (name, json.dumps(sel)[:80]))
')

    if [[ -z "$out" ]]; then
        pass "$cluster: no clusterwide policy is checkable for endpoint selection yet"
        return 0
    fi

    # Cilium does not publish a selected-endpoint count on the policy, so the
    # authoritative answer comes from the agent: ask whether any endpoint in the
    # cluster carries the labels the policy selects.
    local name sel
    while IFS=$'\t' read -r name sel; do
        [[ -z "$name" ]] && continue
        local matched
        matched=$(kubectl --kubeconfig="$kubeconfig" get pods -A -o json 2>/dev/null | python3 -c "
import json, sys
sel = json.loads('''$sel''')
want = sel.get('matchLabels') or {}
exprs = sel.get('matchExpressions') or []
n = 0
pods = json.load(sys.stdin).get('items', [])
for pod in pods:
    labels = pod['metadata'].get('labels') or {}
    ns = pod['metadata']['namespace']
    # Cilium prefixes k8s labels and encodes the namespace as a label; compare
    # on the bare key so the selector reads the same way it was written.
    ok = True
    for k, v in want.items():
        key = k.split(':')[-1]
        if key == 'io.kubernetes.pod.namespace':
            if ns != v: ok = False; break
            continue
        if labels.get(key) != v: ok = False; break
    if ok:
        for e in exprs:
            key = e['key'].split(':')[-1]
            op = e.get('operator')
            if op == 'Exists' and key not in labels: ok = False; break
            if op == 'DoesNotExist' and key in labels: ok = False; break
    if ok: n += 1
# Pods carrying the selector KEYS at all, whatever their values. This separates
# a wrong selector from a workload that is simply not running: if no pod
# anywhere carries the key, there is nothing for this policy to govern yet.
# (No double quotes in this comment - it lives inside a double-quoted python -c.)
# Only keys the selector REQUIRES count. A DoesNotExist/NotIn key is an
# exclusion: a pod carrying it is one the policy correctly leaves out, not
# evidence the selector is wrong (ADR-106 excludes sandboxes by such a key).
keys = [k.split(':')[-1] for k in want] + [e['key'].split(':')[-1] for e in exprs if e.get('operator') in ('Exists', 'In')]
present = 0
for pod in pods:
    labels = pod['metadata'].get('labels') or {}
    if any(k in labels for k in keys if k != 'io.kubernetes.pod.namespace'):
        present += 1
print('%d %d' % (n, present))
" 2>/dev/null)
        local present
        # An empty result means the matcher itself failed, not that the policy
        # governs nothing. Treating those the same is how this check silently
        # reported "benign" while its python was broken by a quoting error
        # (2026-09-02) — the check has to fail loudly when it cannot answer.
        if [[ -z "${matched// /}" ]]; then
            hard_fail "$cluster: could not evaluate CiliumClusterwideNetworkPolicy $name — the endpoint matcher produced no result, so this policy was NOT verified"
            continue
        fi
        present=$(printf '%s' "$matched" | awk '{print $2}')
        matched=$(printf '%s' "$matched" | awk '{print $1}')
        if [[ "${matched:-0}" == "0" && "${present:-0}" != "0" ]]; then
            hard_fail "$cluster: CiliumClusterwideNetworkPolicy $name selects no pods, but $present pod(s) carry its label keys — the SELECTOR IS WRONG and the policy is enforcing nothing: $sel"
        elif [[ "${matched:-0}" == "0" ]]; then
            # Reported, not failed. Zero matches is ambiguous by construction: it
            # is the signature of a wrong label, and equally the correct answer
            # when the workload the policy governs is simply not running. This
            # check cannot tell those apart, and hard-failing on the benign case
            # would train people to ignore the one that matters.
            #
            # What to do with it: if the workload IS running, the selector is
            # wrong and the policy is enforcing nothing. Compare the selector
            # against a live pod's labels — that is exactly how the sandbox
            # default-deny was found to be inert.
            # No pod anywhere carries the selector's keys, so the workload this
            # policy governs is not running. That is the benign half of the
            # ambiguity this check's header describes, and it was previously
            # reported as a failure — which is what the header says NOT to do,
            # because failing the benign case trains people to ignore the one
            # that matters. The wrong-selector half is a hard failure above.
            note "CiliumClusterwideNetworkPolicy $name selects no pods, and nothing carries its label keys — its workload is not running: $sel"
        else
            pass "$cluster: $name selects $matched pod(s)"
        fi
    done <<< "$out"
}
