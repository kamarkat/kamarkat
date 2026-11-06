#!/usr/bin/env bash
# ADR-051: public tenant traffic terminates on the SPOKE, not the hub.
#
# Two gateways claiming one hostname is a split-brain that resolves by whichever
# DNS record happens to win — untestable, and it fails differently on each
# lookup. The hub must therefore claim no tenant hostname at all.
#
# ADR-050 hardening 5: the Gateway and the hostname HTTPRoute are platform-owned,
# so a tenant cannot author a route that reaches its workload without passing
# AgentGateway's OIDC policy.
validate_ingress_topology() {
    section "ADR-051 spoke-terminated ingress"

    local hub_gw="$VALIDATE_ROOT/manifests/hub-core-services/api-gateway/agentgateway-config.yaml"
    if grep -qE "waypoint(\.[a-z]+)*\.nutgraf\.in" "$hub_gw"; then
        hard_fail "hub agentgateway-config still claims a waypoint hostname — ADR-051 terminates tenant ingress on the spoke"
    else
        pass "hub agentgateway-config claims no tenant hostnames"
    fi

    local kust="$VALIDATE_ROOT/manifests/spoke/spoke-catalog/infra/kustomization.yaml"
    local f
    # agentgateway.yaml / agentgateway-config.yaml are deliberately ABSENT here.
    # d3b7303d replaced the single spoke-wide gateway with one per tenant,
    # rendered by manifests/tenants/charts/universal-tenant (asserted below), so
    # requiring the spoke-catalog to deliver them asserted the pre-d3b7303d
    # topology against a tree that no longer has the files.
    for f in tenant-gateway.yaml external-dns.yaml; do
        if grep -q "  - $f" "$kust"; then
            pass "spoke-catalog delivers $f"
        else
            hard_fail "spoke-catalog kustomization.yaml does not list $f — it is never applied"
        fi
    done

    local tenant_gw="$VALIDATE_ROOT/manifests/tenants/charts/universal-tenant/templates/agentgateway.yaml"
    if [[ -f "$tenant_gw" ]]; then
        pass "universal-tenant chart renders the per-tenant AgentGateway"
    else
        hard_fail "universal-tenant chart does not render agentgateway.yaml — no tenant gets a gateway"
    fi

    if (cd "$VALIDATE_ROOT" && python3 - <<'PY'
import sys, yaml
found = False
for doc in yaml.safe_load_all(open("manifests/spoke/spoke-catalog/infra/tenant-gateway.yaml")):
    if not doc or doc.get("kind") != "Gateway":
        continue
    ns = doc["metadata"].get("namespace")
    for l in doc["spec"].get("listeners", []):
        frm = l.get("allowedRoutes", {}).get("namespaces", {}).get("from")
        if ns != "platform-ops" or frm != "Same":
            sys.exit(1)
        found = True
sys.exit(0 if found else 1)
PY
    ); then
        pass "tenant Gateway is platform-owned (platform-ops) with allowedRoutes.from=Same"
    else
        hard_fail "tenant Gateway must be in platform-ops with allowedRoutes.namespaces.from=Same, else a tenant can bind a route that bypasses the OIDC policy"
    fi
}

# ADR-051: every public hostname carries the environment label. A bare-zone
# hostname in a dev manifest points dev traffic at prod DNS.
#
# In-cluster *.svc.cluster.local URLs are deliberately NOT zoned — each
# environment is a separate cluster, so the service DNS name is identical in all
# of them. Only public hostnames are checked.
validate_hostname_zoning() {
    section "Public hostnames carry the box's subdomain once"

    # Derived from HUB_DOMAIN alone (ADR-111 Revision 7). It read $ENVIRONMENT and
    # returned early for prod, because the leftmost label WAS the environment under
    # the env-as-zone scheme. ADR-051's Amendment 2026-09-18 replaced that with a
    # declared subdomain, so the label is whatever the box declares -- and a box
    # that declares none publishes on the apex, which is the case the prod
    # early-return was really describing.
    #
    # `:-` first: under `set -u` a bare ${HUB_DOMAIN#...} aborts the run before the
    # warning below can say why the check did not run.
    local hub_domain="${HUB_DOMAIN:-}"
    if [[ -z "$hub_domain" ]]; then
        warn "HUB_DOMAIN is not set, so doubled hostnames cannot be looked for (preflight has no cluster to read the box's domain from)"
        return 0
    fi

    local subdomain="${hub_domain%%.*}"
    local zone_suffix="${hub_domain#*.}"
    if [[ "$subdomain" == "$hub_domain" || "$zone_suffix" == "$hub_domain" ]]; then
        pass "this box publishes on the apex ($hub_domain) — there is no label to double"
        return 0
    fi

    local double
    # The pattern is derived, not written out. It was "<env>.<env>.nutgraf.in",
    # which finds a doubled label only on the PLATFORM's own domain -- a tenant box
    # doubling its own would have passed. Preflight has no cluster to read
    # HubEnvironment from, so a literal zone is supplied or the check says it
    # could not run.
    double=$(cd "$VALIDATE_ROOT" && grep -rn "${subdomain}\.${subdomain}\.${zone_suffix}" manifests/ 2>/dev/null || true)
    if [[ -z "$double" ]]; then
        pass "no double-zoned hostnames (${subdomain}.${subdomain}.${zone_suffix})"
    else
        while IFS= read -r line; do hard_fail "double-zoned hostname — $line"; done <<< "$double"
    fi

    local unzoned
    unzoned=$(cd "$VALIDATE_ROOT" && grep -rnoE "https?://[a-z0-9-]+\.nutgraf\.in" \
        manifests/spoke/spoke-catalog/infra/agentgateway-config.yaml 2>/dev/null || true)
    if [[ -z "$unzoned" ]]; then
        pass "identity + gateway hostnames are all environment-zoned"
    else
        while IFS= read -r line; do hard_fail "un-zoned public hostname — $line"; done <<< "$unzoned"
    fi
}
