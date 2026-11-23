#!/usr/bin/env bash
# scope: every-cluster
# ADR-078 addendum 10 §4: every cluster's Alertmanager has the credentials it needs,
# and which credentials those are now depends on the cluster.
#
# A SPOKE NEEDS ONE: ALERT_SMTP_PASSWORD. Mail is not self-hosted, so every cluster
# delivers its own alerts by email with the box's credential. It pages nobody through
# Grafana IRM and holds no credential for one.
#
# THE HUB NEEDS THREE: the SMTP password, GRAFANA_IRM_ALERTMANAGER_URL for critical
# and warning paging, and GRAFANA_IRM_HEARTBEAT_URL for its dead-man's switch. A
# third-party stack serves the hub's monitoring only (add.10 §1), the same carve-out
# the escrow has.
#
# This check asserted three keys on EVERY cluster, from add.5 §5's per-cell
# heartbeat. Two of those were the box owner's own integration copied into every
# cluster, and the third was a per-cell value nothing ever created -- so on a spoke
# this check could only ever fail, and did, while the architecture it was defending
# had already been decided against.
#
# The failure it catches is silent by construction. A missing key leaves the
# ExternalSecret not Ready, the Secret absent, and Alertmanager running normally --
# it just never sends a heartbeat. The monitoring system then reports nothing
# wrong, which is indistinguishable from healthy until an incident is missed.

# _c078_alert_credentials <label> <kubeconfig>
_c078_alert_credentials() {
    local label="$1" kubeconfig="$2"
    local es="alertmanager-credentials" ns="platform-observability"
    local ready msg key size missing=0

    ready=$(kubectl --kubeconfig="$kubeconfig" -n "$ns" get externalsecret "$es" \
        -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)
    if [[ -z "$ready" ]]; then
        hard_fail "$label: no ExternalSecret $ns/$es — Alertmanager has no credentials at all, so mail and incident alerts are not delivered (ADR-078 add.5 §5)"
        return 0
    fi
    if [[ "$ready" != "True" ]]; then
        msg=$(kubectl --kubeconfig="$kubeconfig" -n "$ns" get externalsecret "$es" \
            -o jsonpath='{.status.conditions[?(@.type=="Ready")].message}' 2>/dev/null)
        hard_fail "$label: ExternalSecret $ns/$es is not Ready (${msg:-no message}) — a key missing from this cluster's own secrets project is the usual cause, and ESO is atomic across spec.data, so one absent key keeps the whole Secret from existing"
        return 0
    fi

    # Key presence and non-emptiness, never the value: an empty string satisfies
    # ESO and satisfies nothing else. Sizes are of the base64 field, not content.
    # Per cluster, not one list. A spoke carrying an IRM key would mean the hub's
    # incident integration had been copied into it again.
    # label is the caller's literal "hub" or "spoke" (see the two call sites), and
    # nothing else is in scope here. An earlier draft of this also tested
    # "$cluster" == "$HUB_CLUSTER"; both are unset in this file, so that comparison
    # was "" == "" -- true -- and every spoke would have taken the hub's branch and
    # demanded two credentials it must not have.
    if [[ "$label" == "hub" ]]; then
        expected=(smtp_password irm_alertmanager_url irm_heartbeat_url)
    else
        expected=(smtp_password)
    fi
    for key in "${expected[@]}"; do
        size=$(kubectl --kubeconfig="$kubeconfig" -n "$ns" get secret "$es" \
            -o "jsonpath={.data.$key}" 2>/dev/null | wc -c | tr -d ' ')
        if [[ "${size:-0}" -lt 2 ]]; then
            hard_fail "$label: secret $ns/$es has no usable '$key' — hub-operator materialises it into this cluster's own secrets project at /spoke-pool/<cluster>/shared/, so an absent one means that copy did not happen (ADR-078 add.10 §4)"
            missing=1
        fi
    done
    (( missing )) || pass "$label: Alertmanager credentials present, including this cluster's own heartbeat URL"
}

validate_adr078_alert_credentials() {
    section "ADR-078 add.10 §4: each cluster's Alertmanager credentials, by cluster"

    _c078_alert_credentials hub "$HUB_KUBECONFIG"

    if command -v resolve_spoke_state >/dev/null; then
        resolve_spoke_state
    fi
    if [[ "${SPOKE_STATE:-}" == "undeclared" ]]; then
        note "this box declares no workload cluster; spoke Alertmanager check skipped"
    elif ensure_spoke_kubeconfig; then
        _c078_alert_credentials spoke "$SPOKE_KUBECONFIG"
    else
        hard_fail "no spoke kubeconfig — whether a spoke holds only its SMTP credential, and no copy of the hub's incident integration, is exactly what a hub-only check cannot see, so this is not a pass"
    fi
}
