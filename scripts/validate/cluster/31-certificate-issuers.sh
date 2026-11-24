#!/usr/bin/env bash
# The box's own PKI issuers exist and are Ready (ADR-035, ADR-117 §8).
#
# openbao-fleet signs every internal identity on this box -- the argocd-agent
# mTLS identities and the Support Agent's client certificate, which IS its
# enrolment (ADR-077) -- and openbao-signing is the second half of the ADR-035
# audience split. Both are cert-manager Vault ClusterIssuers against the box's
# OpenBao, signing through pki_int roles whose intermediate the administrator
# signed with the custody-held root (`kakt pki sign-intermediate`).
#
# Absent is checked separately from not-Ready: absent means the security chart
# did not render; not-Ready almost always means the intermediate is unsigned or
# cert-manager cannot log in to OpenBao, and the two have different fixes.
validate_fleet_pki_issuers() {
    section "The box's OpenBao PKI issuers (ADR-035, ADR-077, ADR-117)"

    local issuer ready reason
    for issuer in openbao-fleet openbao-signing; do
        if ! kc get clusterissuer "$issuer" >/dev/null 2>&1; then
            hard_fail "$issuer does not exist — the security chart (platform-security-infra) has not rendered it"
            continue
        fi

        ready=$(kc get clusterissuer "$issuer" \
            -o jsonpath='{range .status.conditions[?(@.type=="Ready")]}{.status}{end}' 2>/dev/null)
        if [[ "$ready" == "True" ]]; then
            pass "$issuer: Ready"
            continue
        fi

        reason=$(kc get clusterissuer "$issuer" \
            -o jsonpath='{range .status.conditions[?(@.type=="Ready")]}{.reason}: {.message}{end}' 2>/dev/null)
        hard_fail "$issuer exists but is not Ready (${reason:-no Ready condition reported}) — check that the intermediate is signed (kakt pki sign-intermediate) and that cert-manager can log in to OpenBao as role cert-manager"
    done

    # No certificate may still name the retired Infisical issuer.
    local stale
    stale=$(kc get certificates -A \
        -o jsonpath='{range .items[?(@.spec.issuerRef.group=="infisical-issuer.infisical.com")]}{.metadata.namespace}/{.metadata.name}{" "}{end}' 2>/dev/null)
    if [[ -n "$stale" ]]; then
        hard_fail "certificates still name the retired Infisical issuer: $stale"
    else
        pass "no certificate names the retired Infisical issuer"
    fi
}
