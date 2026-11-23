#!/usr/bin/env bash
# Every Infisical endpoint we call resolves to a real route in the deployed version
# (ADR-003, ADR-031).
#
# A router-local path is not a public endpoint. On 2026-11-22 a grep for url
# strings containing "/memberships" concluded the identity-membership endpoint did
# not exist and that no project role had ever been granted; both were false,
# because the router serving it is MOUNTED at /projects/:projectId/memberships and
# declares only /identities/:identityId. The wrong conclusion moved an
# architecture review twice.
#
# So the resolved route is recorded in scripts/validate/infisical-api-contract.yaml
# and checked from both ends: our constants against the record always, and the
# record against Infisical's own source wherever reference-projects is present.
# Every negative result carries a known-positive control, because the original
# mistake was a search that could not have found what it declared absent.
validate_infisical_api_contract() {
    section "Infisical endpoints resolve to real routes (ADR-003, ADR-031)"

    local out
    out=$(cd "$VALIDATE_ROOT" && python3 "$VALIDATE_DIR/preflight/031-infisical-api-contract.py") || true

    if [[ -z "$out" ]]; then
        hard_fail "the Infisical API contract check produced no output"
        return 0
    fi

    local line
    while IFS= read -r line; do
        [[ -z "$line" ]] && continue
        case "$line" in
            OK*)   pass "$(printf '%s' "$line" | cut -f2-)" ;;
            BAD*)  hard_fail "$(printf '%s' "$line" | cut -f2-)" ;;
            NOTE*) note "$(printf '%s' "$line" | cut -f2-)" ;;
            *)     note "$line" ;;
        esac
    done <<< "$out"
}
