#!/usr/bin/env bash
# The product-team onboarding guide names things that still exist.
#
# docs/onboarding-an-application.md is the one document a product team reads
# before touching the platform, and it was the one document nothing verified. An
# ADR gets read when its subject changes; a guide just ages, until someone
# follows it and fails at a step the platform stopped supporting.
#
# Scope, stated precisely: this asserts that everything the guide NAMES still
# exists -- labels the admission policy requires, values keys the chart or XRD
# defines, ADRs, kakt commands, exported library functions. It does not assert
# the guide is COMPLETE, because completeness is a judgement and this is a gate.
# A platform capability added without documenting it passes here, deliberately.
#
# Each check has been shown to fail against injected drift; a check that has only
# ever passed is not evidence of anything.
validate_onboarding_guide_is_current() {
    section "The onboarding guide names things that still exist"

    local out
    out=$(cd "$VALIDATE_ROOT" && python3 "$VALIDATE_DIR/preflight/93-onboarding-guide-is-current.py") || true

    if [[ -z "$out" ]]; then
        hard_fail "onboarding guide check produced no output"
        return 0
    fi
    if [[ "$out" == "NONE" ]]; then
        warn "docs/onboarding-an-application.md is absent — nothing to check"
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
