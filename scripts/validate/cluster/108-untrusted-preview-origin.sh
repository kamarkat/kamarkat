#!/usr/bin/env bash
# scope: spoke
# ADR-108: session previews are served from a separate registrable domain,
# admitted by a platform handoff. LIVE checks, read-only.
#
#   P1  the PreviewAccess API is served
#   P2  the preview-access service is Ready (one replica, by design)
#   P3  its admission webhook has a CA bundle, so record creates are checked
#       rather than refused with a TLS error
#   P4  every PreviewAccess is Ready, or says exactly why not
#   P5  with a preview domain set: the :443 Gateway has the preview listener
#       Programmed and the wildcard certificate is Ready
validate_adr108_preview_origin() {
    section "ADR-108 preview origin"
    ensure_spoke_kubeconfig || { soft_fail "no spoke kubeconfig; ADR-108 is a spoke property"; return 0; }

    if kc_spoke get crd previewaccesses.compute.nutgraf.in >/dev/null; then
        pass "P1 PreviewAccess API is served"
    else
        hard_fail "P1 the PreviewAccess CRD is absent: products cannot declare previews"
        return 0
    fi

    local ready
    ready=$(kc_spoke -n platform-ops get deploy preview-access -o jsonpath='{.status.readyReplicas}')
    if [[ "$ready" == "1" ]]; then
        pass "P2 preview-access is Ready"
    else
        hard_fail "P2 preview-access has ${ready:-0} ready replicas: no preview can be opened, and PreviewAccess creates are refused (failurePolicy Fail)"
    fi

    local ca
    ca=$(kc_spoke get validatingwebhookconfiguration preview-access -o jsonpath='{.webhooks[0].clientConfig.caBundle}')
    if [[ -n "$ca" ]]; then
        pass "P3 the PreviewAccess webhook has its CA bundle"
    else
        hard_fail "P3 the PreviewAccess webhook has no CA bundle: cert-manager has not injected platform-ops/preview-access-webhook, so every create fails TLS"
    fi

    local out line
    out=$(kc_spoke get previewaccesses -A -o json | python3 -c '
import json, sys
for r in json.load(sys.stdin)["items"]:
    c = next((c for c in r.get("status", {}).get("conditions", []) if c["type"] == "Ready"), None)
    where = "%s/%s" % (r["metadata"]["namespace"], r["metadata"]["name"])
    if c is None:
        print("FAIL P4 %s has no Ready condition: the controller has not reconciled it" % where)
    elif c["status"] == "True":
        print("PASS P4 %s serves at %s" % (where, r["status"].get("host")))
    else:
        print("FAIL P4 %s is not Ready: %s -- %s" % (where, c.get("reason"), c.get("message")))
')
    while IFS= read -r line; do
        case "$line" in
            PASS\ *) pass "${line#PASS }" ;;
            FAIL\ *) hard_fail "${line#FAIL }" ;;
        esac
    done <<< "$out"

    local domain
    domain=$(kc_spoke -n platform-ops get deploy preview-access -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="PREVIEW_DOMAIN")].value}')
    if [[ -z "$domain" ]]; then
        hard_fail "P5 no preview domain is configured for this environment (previewDomain): preview-access cannot start and no preview can be served"
        return 0
    fi
    local prog cert
    prog=$(kc_spoke -n platform-ops get gateway tenant-tls-gateway -o jsonpath='{.status.listeners[?(@.name=="preview")].conditions[?(@.type=="Programmed")].status}')
    cert=$(kc_spoke -n platform-ops get certificate preview-wildcard -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')
    [[ "$prog" == "True" ]] && pass "P5 the preview listener *.$domain is Programmed" \
        || hard_fail "P5 the preview listener for *.$domain is not Programmed (${prog:-absent}): no preview host resolves to anything"
    [[ "$cert" == "True" ]] && pass "P5 the *.$domain certificate is Ready" \
        || hard_fail "P5 certificate preview-wildcard is not Ready (${cert:-absent}): is $domain in the dns01 solver's dnsZones?"
}
