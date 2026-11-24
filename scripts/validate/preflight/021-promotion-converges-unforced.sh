#!/usr/bin/env bash
# A promotion converges without being driven (ADR-021 addendum, 2026-11-24).
#
# Git declares the desired state, ArgoCD reconciles it, the CLI observes. Each fact
# below was verified against the pinned upstream and each can drift silently: an
# unset timeout is argo-cd's own default, a late sync wave looks like ordering, a
# wrong metric label reports healthy by matching nothing, and a forced sync in the
# release path reads as thoroughness.
validate_promotion_converges_unforced() {
    section "A promotion converges without being driven (ADR-021 addendum)"

    local out
    out=$(cd "$VALIDATE_ROOT" && python3 "$VALIDATE_DIR/preflight/021-promotion-converges-unforced.py") || true

    if [[ -z "$out" ]]; then
        hard_fail "the promotion-convergence check produced no output"
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
