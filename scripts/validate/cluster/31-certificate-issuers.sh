#!/usr/bin/env bash
# The box's own PKI issuers exist and are Ready.
#
# infisical-fleet-issuer signs every internal identity on this box: the three
# argocd-agent mTLS identities, the spoke's cluster issuer delivered by
# ClusterResourceSet (ADR-048), and the Support Agent's client certificate --
# which IS its enrolment (ADR-077). infisical-signing-issuer is the second half
# of the ADR-035 audience split.
#
# Nothing checked for them, and the way they go missing is silent. The security
# chart emits them only when infisical.fleet.projectId and .clientId are both
# supplied, and those arrive from a Day-0 artifact that has to be generated,
# committed and pushed before ArgoCD can render anything. Every step in that
# chain has failed at least once. When it does, the chart renders no issuer,
# every Certificate naming one stays Pending forever, and each consumer reports
# its own symptom -- eleven pending certificates on the box that found it,
# reported as eleven unrelated problems.
#
# Absent is therefore checked separately from not-Ready: they have different
# causes and different fixes, and a single "issuer unhealthy" would send someone
# to the issuer when the fault is an uncommitted values file.
#
# NOT cert-manager's ClusterIssuer. These are
# infisical-issuer.infisical.com/v1alpha1, a different CRD that happens to share
# the kind name -- `kubectl get clusterissuer` resolves to cert-manager's and
# reports NotFound for both of these on a perfectly healthy box.
validate_fleet_pki_issuers() {
    section "The box's Infisical PKI issuers (ADR-035, ADR-048, ADR-077)"

    local kind="clusterissuers.infisical-issuer.infisical.com"

    if ! kc get crd "$kind" >/dev/null 2>&1; then
        hard_fail "the $kind CRD is absent — the Infisical issuer controller is not installed, so no internal certificate on this box can ever issue"
        return 0
    fi

    local issuer ready reason
    for issuer in infisical-fleet-issuer infisical-signing-issuer; do
        if ! kc get "$kind" "$issuer" >/dev/null 2>&1; then
            hard_fail "$issuer does not exist — the security chart emits it only when infisical.fleet.projectId and .clientId are both supplied, so this box's generated PKI values are missing, uncommitted, or unpushed"
            continue
        fi

        ready=$(kc get "$kind" "$issuer" \
            -o jsonpath='{range .status.conditions[?(@.type=="Ready")]}{.status}{end}' 2>/dev/null)
        if [[ "$ready" == "True" ]]; then
            pass "$issuer: Ready"
            continue
        fi

        reason=$(kc get "$kind" "$issuer" \
            -o jsonpath='{range .status.conditions[?(@.type=="Ready")]}{.reason}: {.message}{end}' 2>/dev/null)
        hard_fail "$issuer exists but is not Ready (${reason:-no Ready condition reported}) — it is configured but cannot authenticate to Infisical, so certificates naming it will stay Pending"
    done
}
