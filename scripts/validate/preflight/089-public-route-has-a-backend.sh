#!/usr/bin/env bash
# A tenant's public HTTPRoute reaches a backend some chart actually renders (ADR-088).
#
# tenant-public-tls renders the route in platform-ops; universal-tenant renders the
# Service it targets AND the ReferenceGrant permitting the cross-namespace reference,
# both under gateway.enabled. Neither chart could see the other, so an app with
# public.hosts and no gateway.enabled published a route to a Service nobody asked
# for -- reported as RefNotPermitted, which names the wrong cause.
validate_public_route_has_a_backend() {
    section "A tenant's public route reaches a rendered backend (ADR-088)"

    local out
    out=$(cd "$VALIDATE_ROOT" && python3 "$VALIDATE_DIR/preflight/089-public-route-has-a-backend.py") || true

    if [[ -z "$out" ]]; then
        hard_fail "the public-route backend check produced no output"
        return 0
    fi

    local line
    while IFS= read -r line; do
        [[ -z "$line" ]] && continue
        case "$line" in
            OK*)  pass "$(printf '%s' "$line" | cut -f2-)" ;;
            BAD*) hard_fail "$(printf '%s' "$line" | cut -f2-)" ;;
            *)    note "$line" ;;
        esac
    done <<< "$out"
}
