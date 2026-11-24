#!/usr/bin/env bash

PUBLIC_ENDPOINTS=(
  # host                 | path                        | expect              | plaintext | label
  "id.{zone}             | /.well-known/openid-configuration | 200           | yes       | zitadal issuer discovery"
  "id.{zone}             | /oauth/v2/keys              | 200                 | no        | zitadal issuer JWKS"
  "id.{zone}             | /ui/v2/login/loginname      | 200 302 303 400     | no        | zitadal issuer login UI"
  "auth.{zone}           | /.well-known/oauth-authorization-server | 200      | yes       | auth metadata"
  "auth.{zone}           | /.well-known/openid-configuration | 200           | no        | auth discovery passthrough"
  "api.{zone}            | /                           | 200 401 403 404     | yes       | api gateway"
  "openbao.{zone}        | /v1/sys/health              | 200 429 473         | yes       | openbao api (through the Gateway)"
  "console.{zone}        | /                           | 200 302 303 404     | yes       | console"
  "argocd.{zone}         | /healthz                    | 200                 | yes       | argocd"
  "dashboard.{zone}      | /                           | 200 302 401         | yes       | headlamp"
  # ADR-078 §5: the observability query surface. Added when the capability
  # shipped without a public route at all -- Grafana answered only through
  # `kubectl port-forward`, which is not something an operator on call can use,
  # and ADR-069 grades the maintenance promise by what is observable.
  #
  # 302 is the expected answer unauthenticated: Grafana redirects to its login.
  # A 200 here would mean anonymous access is on, which is worth noticing.
  "grafana.{zone}        | /api/health                 | 200                 | yes       | grafana health"
  "grafana.{zone}        | /                           | 200 302             | yes       | grafana ui"
)

# ─── Probes ──────────────────────────────────────────────────────────────────

# _probe URL -> body, then a final line holding the HTTP code.
#
# The body is NOT truncated. An earlier version capped it with `tail -c 2048`,
# which cut an OIDC discovery document (~2.4KB) mid-JSON: the parse then yielded
# an empty issuer and the check reported a byte-for-byte mismatch against a
# document that was in fact correct. A validator that manufactures its own
# failures is worse than no validator.
#
# --resolve is deliberately NOT used, and neither is -k: resolving the name
# through public DNS and validating the certificate chain are both part of what
# is being checked.
_probe() { local u="$1"; shift; curl -s -m 15 "$@" -o /dev/stdout -w '\n%{http_code}' "$u" 2>/dev/null; }
_probe_final() { local u="$1"; shift; curl -s -L -m 20 "$@" -o /dev/null -w '%{http_code} %{url_effective}' "$u" 2>/dev/null; }
_code() { tail -1 <<< "$1"; }
_body() { sed '$d' <<< "$1"; }
_json_field() { python3 -c "import json,sys; print(json.load(sys.stdin).get('$1',''))" 2>/dev/null; }
_field() { awk -F'|' -v n="$2" '{gsub(/^[ \t]+|[ \t]+$/,"",$n); print $n}' <<< "$1"; }

# _authoritative_ns ZONE -> one nameserver authoritative for the zone, or "".
#
# Zone state is asked of the zone, never of a cache. A resolver answers with what
# it was told up to a TTL ago, which is the right answer for "what do users get"
# and the wrong one for "what does the zone say" -- and the two questions have
# different fixes. Resolved once and reused; every lookup below is cheap after it.
_authoritative_ns() {
    local z="$1"
    while [[ "$z" == *.*.* ]]; do
        local ns
        ns=$(dig +short +time=3 +tries=1 NS "$z" 2>/dev/null | head -1)
        [[ -n "$ns" ]] && { echo "$ns"; return; }
        z="${z#*.}"
    done
    dig +short +time=3 +tries=1 NS "$z" 2>/dev/null | head -1
}

# _dns_owner NAME -> the external-dns owner id recorded for a hostname, or "".
#
# external-dns records ownership in a TXT sibling of every record it manages
# (--txt-prefix=extdns-). It is published DNS, so this needs no provider
# credential and no cluster access -- which matters, because the failure it
# catches outlives the box that caused it.
#
# Asked of the authoritative server: ownership is a property of the zone, and a
# cached TXT reports the previous owner for as long as its TTL runs. That is not
# a hypothetical -- correcting an orphaned record on 2026-09-15 left this check
# reporting the old owner for another 2.5 hours while the zone was already right.
_dns_owner() {
    local h="$1" txt server="${AUTH_NS:+@$AUTH_NS}"
    for txt in "extdns-$h" "extdns-a-$h"; do
        dig +short +time=3 +tries=1 TXT "$txt" $server 2>/dev/null \
            | tr -d '"' | grep -oE 'external-dns/owner=[^,]+' | cut -d= -f2 | head -1
    done | head -1
}

validate_public_api_endpoints() {
    section "Public endpoints answer over their published hostnames"
    # Every hostname below derives from the zone; without it there is nothing
    # to check against, and the platform's own zone is not a substitute.
    require_zone || return 0

    local AUTH_NS
    AUTH_NS="$(_authoritative_ns "$ENV_ZONE")"
    AUTH_NS="${AUTH_NS%.}"

    local zone="$ENV_ZONE"
    local apex="${zone#*.}"
    [[ "$zone" != *.*.* ]] && apex="$zone"

    local row host path expect plaintext label url code resolved tls_out seen_hosts=""
    local owner_id record_owner authoritative ttl connect
    local -a pin=()
    owner_id="$(kc -n platform-edge get deploy external-dns -o jsonpath='{.spec.template.spec.containers[0].args}' \
        | tr ',' '\n' | grep -oE '\-\-txt-owner-id=[^"]+' | cut -d= -f2 | head -1)"
    for row in "${PUBLIC_ENDPOINTS[@]}"; do
        host=$(_field "$row" 1); path=$(_field "$row" 2)
        expect=$(_field "$row" 3); plaintext=$(_field "$row" 4); label=$(_field "$row" 5)
        host="${host//\{zone\}/$zone}"; host="${host//\{apex\}/$apex}"
        url="https://${host}${path}"

        # DNS and TLS are asserted ONCE per host, not once per row, so a broken
        # hostname is reported as one fault rather than as many.
        if [[ " $seen_hosts " != *" $host "* ]]; then
            seen_hosts="$seen_hosts $host"
            pin=(); connect="$host"

            resolved="$(dig +short +time=3 +tries=1 "$host" 2>/dev/null | tail -1)"
            if [[ -z "$resolved" ]]; then
                hard_fail "$label: $host does not resolve — external-dns has not published its record"
                continue
            fi

            # Reachability and certificate validity are separate faults, and
            # reporting them as one cost an afternoon on 2026-09-15:
            # dashboard.dev.nutgraf.in resolved to a decommissioned address left
            # behind by a previous box, nothing answered there, and this check
            # said "the ACME certificate is missing, expired, or does not cover
            # this name". The certificate was a valid wildcard covering the name,
            # issued and served correctly on the address the record SHOULD have
            # named. So connect first and only then verify: a record pointing
            # somewhere dead must not read as a cert-manager fault.
            #
            # openssl prints CONNECTED(...) the moment TCP is established, before
            # any handshake, which is exactly the boundary between the two. The
            # timeout is here because s_client's own -timeout applies to DTLS
            # only and a black-holed address otherwise hangs the whole module.
            # Ownership BEFORE reachability, because it explains reachability.
            # external-dns will not adopt, update or delete a record whose owner
            # id is not its own -- it ignores it and logs nothing -- so a record
            # left by an earlier box pins the hostname to whatever it last
            # resolved to, permanently. Checked here rather than in the
            # external-dns module because the orphan survives the box that made
            # it: the deployment's args can be perfectly correct while the zone
            # still carries records no one can claim.
            if [[ -n "$owner_id" ]]; then
                record_owner="$(_dns_owner "$host")"
                if [[ -n "$record_owner" && "$record_owner" != "$owner_id" ]]; then
                    hard_fail "$label: $host is owned by external-dns id '$record_owner', not this box's '$owner_id' — external-dns silently ignores it, so the record is frozen at $resolved until it is deleted from the zone by hand"
                    continue
                fi
            fi

            # A stale cache and a wrong record look identical from here, and they
            # have different fixes: one needs nothing, the other needs a human in
            # the zone. Comparing the two answers tells them apart, so a record
            # that was just corrected reports as propagating rather than as a
            # platform fault that will "come back" on its own.
            if [[ -n "$AUTH_NS" ]]; then
                authoritative="$(dig +short +time=3 +tries=1 A "$host" "@$AUTH_NS" 2>/dev/null | tail -1)"
                if [[ -n "$authoritative" && "$authoritative" != "$resolved" ]]; then
                    ttl="$(dig +time=3 +tries=1 A "$host" 2>/dev/null | awk -v h="$host." '$1==h {print $2; exit}')"
                    # Pin the rest of this host's checks to the address the ZONE
                    # names, and keep running them. Downgrading to a warning and
                    # skipping the probes would make "propagating" mean "not
                    # checked" -- the endpoint could be genuinely broken and this
                    # would still report only the cache. Everything below now
                    # asserts the platform side properly, and the warning says
                    # only what is actually true: this resolver is behind.
                    pin=(--resolve "${host}:443:${authoritative}" --resolve "${host}:80:${authoritative}")
                    connect="$authoritative"
                    warn "$label: $host is $authoritative in the zone but this resolver still has $resolved${ttl:+ for another ${ttl}s} — cache lag, not a platform fault; checks below run against the zone's address"
                fi
            fi

            tls_out="$(echo | timeout 15 openssl s_client -servername "$host" -connect "${connect}:443" -verify_return_error 2>&1)"
            if ! grep -q 'CONNECTED(' <<< "$tls_out"; then
                hard_fail "$label: $host resolves to $resolved but nothing accepts TLS on :443 there — the published record points where this box is not"
                continue
            fi

            # TLS separately from HTTP, so an expired or wrong-SAN certificate is
            # named as such rather than surfacing as a bare connection failure.
            if ! grep -qE 'Verify return code: 0 \(ok\)' <<< "$tls_out"; then
                hard_fail "$label: TLS to https://$host failed certificate verification ($(grep -oE 'Verify return code: .*' <<< "$tls_out" | head -1)) — served $(grep -oE 'subject=.*' <<< "$tls_out" | head -1)"
                continue
            fi

            # Plaintext must never serve content. Every listener is fronted by an
            # http-to-https redirect; a non-3xx here means one hostname answers
            # unencrypted, which no object-level check sees.
            if [[ "$plaintext" == "yes" ]]; then
                code=$(_code "$(_probe "http://${host}/" ${pin[@]+"${pin[@]}"})")
                case "$code" in
                    3??) pass "$label: http://${host} redirects to HTTPS (HTTP $code)" ;;
                    *)   hard_fail "$label: http://${host} returned HTTP ${code:-no-response}, expected a 3xx redirect — plaintext must never serve content" ;;
                esac
            fi
        fi

        code=$(_code "$(_probe "$url" ${pin[@]+"${pin[@]}"})")
        if [[ " $expect " == *" $code "* ]]; then
            pass "$label: $url → HTTP $code"
        else
            hard_fail "$label: $url returned HTTP ${code:-no-response}, expected one of [$expect]"
        fi
    done

    # ─── Contracts the status code alone cannot express ──────────────────────

    local id_host="id.${zone}" auth_host="auth.${zone}"

    # The issuer must NAME itself as the URL it is served at. Relying parties
    # compare this byte-for-byte and reject a mismatch as a spoofed provider —
    # at login, long after every pod reports Ready.
    local disc stated advertised jwks nkeys
    disc=$(_probe "https://${id_host}/.well-known/openid-configuration")
    if [[ "$(_code "$disc")" == "200" ]]; then
        stated=$(_body "$disc" | _json_field issuer)
        if [[ "$stated" == "https://${id_host}" ]]; then
            pass "issuer identity: discovery states https://${id_host}"
        else
            hard_fail "issuer identity: discovery states ${stated:-<empty>} but is served at https://${id_host} — relying parties reject the mismatch as a spoofed provider"
        fi

        # The advertised jwks_uri must itself work: a consumer takes this value
        # verbatim, so advertising an unreachable path is broken even though the
        # document parsed.
        advertised=$(_body "$disc" | _json_field jwks_uri)
        jwks=$(_probe "$advertised")
        nkeys=$(_body "$jwks" | python3 -c "import json,sys; print(len(json.load(sys.stdin).get('keys',[])))" 2>/dev/null || echo 0)
        if [[ "$(_code "$jwks")" == "200" && "${nkeys:-0}" -gt 0 ]]; then
            pass "issuer keys: advertised jwks_uri serves $nkeys key(s)"
        else
            hard_fail "issuer keys: advertised jwks_uri ${advertised:-<empty>} returned HTTP $(_code "$jwks") with ${nkeys:-0} key(s) — no consumer can validate a token"
        fi
    fi

    # auth.<zone> is SPLIT: /.well-known/ and /internal/ are served by
    # auth-proxy, everything else redirects to the issuer. Both halves are
    # asserted — a redirect on a machine surface breaks OAuth clients, which
    # cannot follow one on a metadata or token endpoint.
    local meta adv browser
    meta=$(_probe "https://${auth_host}/.well-known/oauth-authorization-server")
    if [[ "$(_code "$meta")" == "200" ]]; then
        adv=$(_body "$meta" | _json_field issuer)
        if [[ "$adv" == "https://${id_host}" ]]; then
            pass "auth metadata: advertises the issuer https://${id_host}"
        else
            hard_fail "auth metadata: advertises issuer ${adv:-<empty>}, not https://${id_host} — an MCP client discovering this rejects every token the issuer mints"
        fi
    fi

    browser=$(_probe_final "https://${auth_host}/")
    if [[ "$browser" == *"${id_host}"* ]]; then
        pass "auth split: browser traffic redirects to the issuer ($browser)"
    else
        hard_fail "auth split: browser traffic on ${auth_host} ended at [$browser], not ${id_host} — the machine/browser split is not in effect"
    fi
}
