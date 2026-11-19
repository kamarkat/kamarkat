#!/usr/bin/env bash
# scope: path
# The public tenant path under ADR-051 is:
#   browser -> Hetzner LB -> spoke Gateway -> AgentGateway (OIDC) -> workload
#
# Every hop below has a failure mode that reports Healthy while serving nothing,
# so each is asserted directly rather than inferred from pod status.
validate_tenant_ingress() {
    section "Tenant ingress path (ADR-050/051)"
    # Every hostname below derives from the zone; without it there is nothing
    # to check against, and the platform's own zone is not a substitute.
    require_zone || return 0

    if ! ensure_spoke_kubeconfig; then
        soft_fail "spoke unreachable — tenant ingress path not verified"
        return 0
    fi

    # AgentGateway is the only thing enforcing OIDC in front of tenant workloads.
    #
    # ONE PER TENANT, in the tenant's own namespace. This asserted a single
    # deployment named "agentgateway" in platform-ops until 2026-09-02, which
    # d3b7303d had already replaced: that commit moved the gateway to one
    # instance per tenant rendered by the universal-tenant chart, deleted the
    # shared one, and updated 80-ingress-topology.sh and the tenant-identifier
    # validator — but not this. So it read readyReplicas of a deployment that no
    # longer exists, reported 0, and failed a fleet whose gateway was running
    # fine (agentgateway-waypoint, 1/1, for two and a half hours).
    #
    # Enumerating by label rather than by name means adding a tenant does not
    # need another edit here, and a tenant whose gateway is genuinely down is
    # still caught.
    local ag_json total ready
    ag_json=$(kc_spoke get deployment -A -l app.kubernetes.io/name=agentgateway -o json 2>/dev/null)
    total=$(printf '%s' "$ag_json" | jq -r '[.items[]?] | length' 2>/dev/null)
    if [[ "${total:-0}" -eq 0 ]]; then
        # Fall back to a name prefix: the chart may not carry the label.
        ag_json=$(kc_spoke get deployment -A -o json 2>/dev/null \
            | jq '{items: [.items[]? | select(.metadata.name|startswith("agentgateway"))]}')
        total=$(printf '%s' "$ag_json" | jq -r '[.items[]?] | length' 2>/dev/null)
    fi

    if [[ "${total:-0}" -eq 0 ]]; then
        # Zero gateways is only a fault if a tenant exists to need one.
        #
        # The comment above already says ONE PER TENANT, rendered by the
        # universal-tenant chart into the tenant's own namespace -- so on a box
        # with no tenants, zero is the correct number and this reported "nothing
        # enforces OIDC in front of tenant workloads" about workloads that do not
        # exist. Every fresh box failed here, and the message described a security
        # hole rather than an empty fleet.
        #
        # Tenants are AINativeSaaS on the HUB, which is the same source check 30
        # reads; it printed "no tenants provisioned yet" on the same run this
        # failed.
        local tenant_count
        tenant_count=$(kc get ainativesaas -A -o json 2>/dev/null \
            | jq -r '[.items[]?] | length' 2>/dev/null)
        if [[ "${tenant_count:-0}" -eq 0 ]]; then
            pass "no tenants declared, so no tenant AgentGateway is expected"
        else
            soft_fail "$tenant_count tenant(s) declared but no AgentGateway deployment exists on the spoke — nothing enforces OIDC in front of their workloads"
        fi
    else
        local notready
        notready=$(printf '%s' "$ag_json" | jq -r '[.items[]? | select((.status.readyReplicas // 0) < 1) | .metadata.namespace + "/" + .metadata.name] | join(", ")')
        if [[ -z "$notready" ]]; then
            pass "all $total tenant AgentGateway(s) ready"
        else
            soft_fail "AgentGateway not ready: $notready — nothing enforces OIDC in front of those tenants' workloads"
        fi
    fi

    _validate_external_dns_runtime
    _validate_gateway_programmed
    _validate_one_gateway_per_port
    _validate_routes_accepted
    _validate_spoke_certificates
}

# external-dns publishes the tenant hostname. Its args are patched BY INDEX by the
# spoke-catalog ApplicationSet; an unapplied patch leaves PLACEHOLDER, and
# external-dns then runs healthy and publishes nothing at all — the hostname
# simply never resolves, with no unhealthy resource anywhere.
_validate_external_dns_runtime() {
    local args
    args=$(kc_spoke get deployment external-dns -n platform-ops \
        -o jsonpath='{.spec.template.spec.containers[0].args[0]} {.spec.template.spec.containers[0].args[1]}')

    if [[ -z "$args" ]]; then
        soft_fail "external-dns not deployed on the spoke — tenant hostnames are never published"
        return 0
    fi
    if [[ "$args" == *PLACEHOLDER* ]]; then
        hard_fail "external-dns args still contain PLACEHOLDER ($args) — the spoke-catalog index patch did not apply; it runs healthy and writes no records"
        return 0
    fi
    pass "external-dns args substituted per-spoke: $args"

    if [[ "$args" == *"--domain-filter=${ENV_ZONE}"* ]]; then
        pass "external-dns scoped to ${ENV_ZONE}"
    else
        hard_fail "external-dns --domain-filter is not ${ENV_ZONE} (got: $args) — this spoke can write another environment's DNS records"
    fi
}

# ADR-051 (amendment 2026-09-21): a port is claimed by exactly one Gateway.
#
# A Cilium Gateway in hostNetwork mode (ADR-046 §8) binds a REAL host port, so
# two Gateways on one port is a port conflict rather than a merge. Envoy keeps
# the first and rejects the rest:
#
#   has duplicate address '0.0.0.0:443' as existing listener
#
# Checked LIVE and not only against the manifests, because the manifest gate
# (TestOneGatewayPerHostPortInTheSpokeCatalog) can only prove that the sources
# it knows about agree. It cannot see a Gateway left behind by an older bundle,
# created by hand, or added by a chart introduced later -- which is how this
# arrived: one claimant per source tree, each correct on its own.
#
# A purpose gets a LISTENER on the shared Gateway, never a Gateway of its own.
_validate_one_gateway_per_port() {
    # jsonpath cannot carry the parent's name into a nested range over its
    # listeners, so the (port, gateway) pairing is built here instead.
    local rows dupes
    rows=""
    local g p
    while IFS= read -r g; do
        [[ -n "$g" ]] || continue
        for p in $(kc_spoke get gateway -A -o jsonpath="{range .items[?(@.metadata.name=='$g')]}{range .spec.listeners[*]}{.port}{' '}{end}{end}"); do
            rows+="$p $g"$'\n'
        done
    done <<< "$(kc_spoke get gateway -A -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')"
    rows=$(echo "$rows" | awk 'NF==2' | sort -u)

    if [[ -z "$rows" ]]; then
        soft_fail "no Gateway listeners found on the spoke -- nothing terminates tenant traffic"
        return
    fi

    dupes=$(echo "$rows" | awk '{print $1}' | sort | uniq -d)
    if [[ -z "$dupes" ]]; then
        pass "every Gateway port has exactly one claimant (no envoy listener conflict)"
    else
        local port names
        while IFS= read -r port; do
            [[ -n "$port" ]] || continue
            names=$(echo "$rows" | awk -v x="$port" '$1==x {print $2}' | sort -u | tr '\n' ' ')
            hard_fail "port $port is claimed by more than one Gateway ($names) -- envoy NACKs all but the first, and the losing hostname resets every connection while still reporting Programmed=True. Give each purpose a LISTENER on one Gateway (ADR-051 amendment 2026-09-21)"
        done <<< "$dupes"
    fi

    # Listener names are the ServerSideApply merge key across contributors, so a
    # duplicate does not conflict -- it silently replaces, which is the same
    # outage reached another way.
    local dupnames d
    dupnames=$(kc_spoke get gateway -A \
        -o jsonpath='{range .items[*]}{.metadata.name}{"/"}{range .spec.listeners[*]}{.name}{"\n"}{end}{end}' \
        | grep -v '^$' | sort | uniq -d)
    if [[ -n "$dupnames" ]]; then
        while IFS= read -r d; do
            [[ -n "$d" ]] || continue
            hard_fail "listener $d is declared twice -- the name is the ServerSideApply merge key, so one contributor silently replaces the other"
        done <<< "$dupnames"
    fi
}

# Accepted alone only means the config parsed.
#
# Programmed does NOT mean the listeners are live, and this comment used to say
# it did. On 2026-09-21 two Gateways claimed :443 on one spoke; envoy accepted
# the first and NACKed the second for the life of the cluster, while BOTH
# reported Programmed=True with their routes Accepted, ResolvedRefs=True and
# their certificates issued. The losing hostname reset every TLS ClientHello.
# Programmed is written by the Gateway controller from its own intent; it does
# not survive a round trip through envoy. _validate_one_gateway_per_port
# asserts what it cannot.
_validate_gateway_programmed() {
    local st
    st=$(kc_spoke get gateway -n platform-ops \
        -o jsonpath='{.items[0].status.conditions[?(@.type=="Programmed")].status}')
    if [[ "$st" == "True" ]]; then
        pass "spoke Gateway is Programmed (address assigned, listeners live)"
    else
        soft_fail "spoke Gateway not Programmed (status=${st:-none}) — no listener is serving tenant traffic"
    fi

    # ADR-050 hardening 5: a Gateway outside platform-ops lets a tenant publish a
    # route that reaches its workload without passing AgentGateway.
    local rogue
    rogue=$(kc_spoke get gateway -A \
        -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}{"\n"}{end}' \
        | grep -v '^platform-ops/' | grep -v '^$' || true)
    if [[ -z "$rogue" ]]; then
        pass "no Gateway outside platform-ops (OIDC policy cannot be bypassed)"
    else
        local g
        while IFS= read -r g; do
            hard_fail "Gateway $g is outside platform-ops — it bypasses AgentGateway's OIDC policy (ADR-050 hardening 5)"
        done <<< "$rogue"
    fi
}

# An HTTPRoute whose parent never accepted it is inert: it exists, ArgoCD reports
# it Synced, and it routes nothing.
_validate_routes_accepted() {
    local bad
    bad=$(kc_spoke get httproute -A -o json 2>/dev/null \
        | jq -r '.items[] | select([.status.parents[]?.conditions[]? | select(.type=="Accepted" and .status=="True")] | length == 0) | "\(.metadata.namespace)/\(.metadata.name)"' 2>/dev/null || true)
    if [[ -z "$bad" ]]; then
        pass "all HTTPRoutes accepted by their parent Gateway"
    else
        local r
        while IFS= read -r r; do
            [[ -z "$r" ]] && continue
            soft_fail "HTTPRoute $r not Accepted by any parent — it exists but routes nothing"
        done <<< "$bad"
    fi
}

# Without a Ready certificate the browser gets a TLS error, which no pod-level
# check ever surfaces.
_validate_spoke_certificates() {
    local bad
    bad=$(kc_spoke get certificate -A --no-headers 2>/dev/null | awk '$3 != "True" {print $1"/"$2}' || true)
    if [[ -z "$bad" ]]; then
        pass "all spoke certificates Ready"
    else
        local c
        while IFS= read -r c; do
            [[ -z "$c" ]] && continue
            soft_fail "spoke certificate not Ready: $c — the browser gets a TLS error"
        done <<< "$bad"
    fi
}
