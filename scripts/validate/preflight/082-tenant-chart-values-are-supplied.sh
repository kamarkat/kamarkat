#!/usr/bin/env bash
# Every value universal-tenant requires has exactly one supplier (ADR-082).
#
# The tenant declares its application; the platform resolves where it runs. A
# value the chart marks `required` therefore comes from the application's own
# values.yaml or from an ApplicationSet parameter -- and when it comes from
# neither, every application declared from then on fails to render while every
# one declared before keeps working. That is how clusterName was lost: ADR-082
# removed it from the application's declaration, nothing replaced it on the
# platform side, and the gap surfaced three weeks later on the first new
# application (nutgraf-waypoint-stg, 2026-11-22).
validate_tenant_chart_values_are_supplied() {
    section "Every value universal-tenant requires has one supplier (ADR-082)"

    local out
    out=$(cd "$VALIDATE_ROOT" && python3 "$VALIDATE_DIR/preflight/082-tenant-chart-values-are-supplied.py") || true

    if [[ -z "$out" ]]; then
        hard_fail "the tenant chart values check produced no output"
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
