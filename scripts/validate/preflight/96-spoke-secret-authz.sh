#!/usr/bin/env bash
# Every spoke ExternalSecret must resolve INSIDE its cell's authorised prefix
# (ADR-031).
#
# The cell Machine Identity is granted read access to exactly two paths:
#
#     /spoke-pool/<cell-id>/shared/*
#     /spoke-pool/<cell-id>/tenants/*
#
# A spoke ExternalSecret pointing anywhere else is not a seeding gap that will
# resolve once someone adds the key — it is unreadable BY DESIGN and will fail on
# every spoke, forever.
#
# Scope, stated precisely: this catches a LATENT contract violation. It does not
# diagnose a live secret outage. When the spoke ClusterSecretStore itself is
# Ready=False every ExternalSecret under it fails identically regardless of path,
# and reading that as an authorization problem is a wrong turn this check must not
# encourage. Store health first, paths second.
#
# 95-secret-key-producers.sh does NOT cover this: it proves a producer exists for
# a key, not that the consumer is authorised for the path it asks for. A key can be
# correctly produced and still be unreachable from the cell asking for it.
#
# The check resolves the ApplicationSet patches structurally rather than reading the
# manifests as written: the files in Git carry an unreachable PLACEHOLDER on purpose,
# and the effective path only exists after the spoke-catalog kustomize patch
# substitutes the cell id. Validating the source text alone would pass a manifest
# whose patch was missing — the more likely regression.
#
# The patches are parsed as JSON6902 documents from the RENDERED chart. An earlier
# version scraped them with a regex whose block lookahead stopped at "- op:", so it
# read only the first operation per target and falsely reported the rest missing.
validate_spoke_secret_authz() {
    section "Spoke ExternalSecrets stay inside their cluster's OpenBao scope (ADR-117 §4)"

    local out
    out=$(cd "$VALIDATE_ROOT" && python3 "$VALIDATE_DIR/preflight/96-spoke-secret-authz.py") || true

    if [[ -z "$out" ]]; then
        hard_fail "spoke secret authorization check produced no output"
        return 0
    fi
    if [[ "$out" == "NONE" ]]; then
        warn "no spoke ExternalSecrets rendered — nothing to check"
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
