#!/usr/bin/env bash
# Every hub database role has exactly one password authority (ADR-023/ADR-112
# amendment 2026-11-24).
#
# hub-operator generates, Infisical stores, ESO delivers, CloudNativePG writes it
# into PostgreSQL. Two writers of one field is the defect this guards: CNPG
# reconciles the application owner from the Secret its Cluster declares on every
# instance reconcile, and hub-operator used to ALTER the same role from Infisical.
validate_one_password_owner_per_role() {
    section "One password authority per hub database role (ADR-023)"

    local out
    out=$(cd "$VALIDATE_ROOT" && python3 "$VALIDATE_DIR/preflight/023-one-password-owner-per-role.py") || true

    if [[ -z "$out" ]]; then
        hard_fail "the password-authority check produced no output"
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
