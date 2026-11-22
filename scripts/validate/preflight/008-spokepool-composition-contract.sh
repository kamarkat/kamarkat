#!/usr/bin/env bash
# A declared provider resolves to one existing composition (ADR-008, ADR-046).
#
# The tenant declares spec.provider; the platform decides which Crossplane
# Composition builds the cluster. That requires the XRD, the claim templates, the
# provider registry, the per-provider cells and hub-operator's admission path to
# agree, and on 2026-11-22 none of them did: the Crossplane v2 migration moved the
# machinery to spec.crossplane, two templates and the webhook kept writing
# spec.compositionSelector, and the webhook was never registered.
#
# IT CANNOT BE CAUGHT AT ADMISSION. The API server prunes an undeclared field when
# it decodes the request, before any webhook runs, so a validating webhook never
# sees spec.compositionSelector. It is caught in the rendered source, here.
validate_spokepool_composition_contract() {
    section "A declared provider resolves to one existing composition (ADR-008)"

    # THE SUBJECT IS THE PLATFORM REPOSITORY, not the box. This renders the
    # per-provider cells and hub-operator's own kustomization to establish what a
    # box RECEIVES, which needs those trees -- and a released run reads a narrow
    # embedded set that does not include them. Reported as a warning there rather
    # than as a pass, and gated for real in CI, where the checkout exists.
    platform_source_available "operators/hub-operator/config/default" \
        "operators/hub-operator/config/manager/provider-registry.yaml" \
        "manifests/providers/hetzner" "manifests/providers/hybrid" \
        "internal/kakt-cli/tenant/render_box.go" || return 0

    local out
    out=$(cd "$VALIDATE_ROOT" && python3 "$VALIDATE_DIR/preflight/008-spokepool-composition-contract.py") || true

    if [[ -z "$out" ]]; then
        hard_fail "the SpokePool composition contract check produced no output"
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
