#!/usr/bin/env bash
# scope: spoke
# ADR-050 (Amendment 2026-10-21): an unauthenticated API call gets 401, never a
# login redirect. LIVE checks, read-only, against every public application host.
#
#   A1  a sessionless request a page makes itself (Sec-Fetch-Mode: cors) gets
#       401 and sets NO cookie. A 302 here is the v1.5.0 defect: the page's fetch
#       follows it cross-origin into CORS, the app never sees a 401, and every
#       such call leaves a ~500-byte login-transaction cookie until the next real
#       login fails at /oauth/callback with "Request Header Or Cookie Too Large".
#   A2  a sessionless document navigation still gets 302 into the login flow.
#
# Hosts are read from the platform's public routes (tenant-public-tls), so every
# publicly reachable application is covered without naming one. Only hosts whose
# gateway runs the login policy are checked: an app with gateway.oidc.enabled
# false (a public site) has no login for this contract to govern.
validate_adr050_unauthenticated_api_gets_401() {
    section "ADR-050 unauthenticated API calls get 401"
    ensure_spoke_kubeconfig || { soft_fail "no spoke kubeconfig; tenant gateways run on the spoke"; return 0; }

    # "<host> <namespace>/<gateway>" for every public route.
    local routes hosts="" host gw ns name cfg
    routes=$(kc_spoke -n platform-ops get httproutes -l app.kubernetes.io/managed-by=tenant-public-tls \
        -o jsonpath='{range .items[*]}{range .spec.hostnames[*]}{@}{" "}{end}{"|"}{.spec.rules[0].backendRefs[0].namespace}{"/"}{.spec.rules[0].backendRefs[0].name}{"\n"}{end}')
    if [[ -z "$routes" ]]; then
        hard_fail "no public application routes found (tenant-public-tls HTTPRoutes): nothing was checked"
        return 0
    fi
    while IFS='|' read -r hs gw; do
        [[ -z "$gw" ]] && continue
        ns=${gw%/*}; name=${gw#*/}
        cfg=$(kc_spoke -n "$ns" get configmap "${name}-config" -o jsonpath='{.data.config\.yaml}') || {
            hard_fail "could not read ${ns}/${name}-config: the login policy of ${hs} was not determined"; continue; }
        if grep -Eq '^[[:space:]]+oidc:' <<< "$cfg"; then
            for host in $hs; do hosts+="${host}"$'\n'; done
        else
            note "skipped ${hs}: ${ns}/${name} runs no login policy (gateway.oidc.enabled=false)"
        fi
    done <<< "$routes"
    if [[ -z "${hosts//[$'\n']/}" ]]; then
        hard_fail "no public host runs the gateway login policy: nothing was checked"
        return 0
    fi

    local host out code cookies
    while IFS= read -r host; do
        [[ -z "$host" ]] && continue
        # A path under /api so the request crosses the app's protected listener;
        # the gateway answers before any backend sees it.
        out=$(curl -sS -o /dev/null -D - --max-time 15 \
            -H "Sec-Fetch-Mode: cors" -H "Sec-Fetch-Dest: empty" -H "Accept: application/json" \
            "https://${host}/api/" 2>&1) || { hard_fail "A1 ${host}: request failed: ${out##*$'\n'}"; continue; }
        code=$(awk 'toupper($1) ~ /^HTTP\// {c=$2} END {print c}' <<< "$out")
        cookies=$(grep -ci '^set-cookie:' <<< "$out" || true)
        if [[ "$code" == "401" && "$cookies" == "0" ]]; then
            pass "A1 ${host}: unauthenticated fetch gets 401 and no cookie"
        else
            hard_fail "A1 ${host}: unauthenticated fetch got ${code:-no status} with ${cookies} Set-Cookie header(s); ADR-050 requires 401 and none"
        fi

        out=$(curl -sS -o /dev/null -D - --max-time 15 -H "Sec-Fetch-Mode: navigate" -H "Sec-Fetch-Dest: document" \
            "https://${host}/" 2>&1) || { hard_fail "A2 ${host}: request failed: ${out##*$'\n'}"; continue; }
        code=$(awk 'toupper($1) ~ /^HTTP\// {c=$2} END {print c}' <<< "$out")
        if [[ "$code" == "302" ]] && grep -qi '^location:.*/oauth/v2/authorize' <<< "$out"; then
            pass "A2 ${host}: unauthenticated navigation is redirected to login"
        else
            hard_fail "A2 ${host}: unauthenticated navigation got ${code:-no status}, not a 302 into the login flow"
        fi
    done <<< "$hosts"
}
