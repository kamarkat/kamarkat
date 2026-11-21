#!/usr/bin/env bash
# scope: every-cluster
# ADR-078 addendum 5 §5: every cluster's Alertmanager has the credentials it needs,
# and the per-cell one is really there.
#
# Two of the three are copied into each cell by hub-operator
# (ALERT_SMTP_PASSWORD, GRAFANA_IRM_ALERTMANAGER_URL). The third is not and must
# not be: GRAFANA_IRM_HEARTBEAT_URL is one heartbeat integration PER CLUSTER, so
# copying the hub's into every cell would make every cluster's heartbeat land on
# one integration and a dead cluster would look alive.
#
# Nothing on the platform generates it, so the static producer check
# (preflight/95) can only report that it is externally provisioned per cell. That
# is a declaration, not evidence. This is the evidence: the credential is asserted
# on the live cluster, where a cell nobody seeded fails.
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
        hard_fail "$label: ExternalSecret $ns/$es is not Ready (${msg:-no message}) — a key missing from this cell's Infisical path is the usual cause, and GRAFANA_IRM_HEARTBEAT_URL is the one nothing produces for you"
        return 0
    fi

    # Key presence and non-emptiness, never the value: an empty string satisfies
    # ESO and satisfies nothing else. Sizes are of the base64 field, not content.
    for key in smtp_password irm_alertmanager_url irm_heartbeat_url; do
        size=$(kubectl --kubeconfig="$kubeconfig" -n "$ns" get secret "$es" \
            -o "jsonpath={.data.$key}" 2>/dev/null | wc -c | tr -d ' ')
        if [[ "${size:-0}" -lt 2 ]]; then
            hard_fail "$label: secret $ns/$es has no usable '$key' — this cluster's own value was never seeded into /spoke-pool/<cell>/shared/, and nothing on the platform will create it (ADR-078 add.5 §5)"
            missing=1
        fi
    done
    (( missing )) || pass "$label: Alertmanager credentials present, including this cluster's own heartbeat URL"
}

validate_adr078_alert_credentials() {
    section "ADR-078 add.5 §5: each cluster's Alertmanager credentials, per cell"

    _c078_alert_credentials hub "$HUB_KUBECONFIG"

    if command -v resolve_spoke_state >/dev/null; then
        resolve_spoke_state
    fi
    if [[ "${SPOKE_STATE:-}" == "undeclared" ]]; then
        note "this box declares no workload cluster; spoke Alertmanager check skipped"
    elif ensure_spoke_kubeconfig; then
        _c078_alert_credentials spoke "$SPOKE_KUBECONFIG"
    else
        hard_fail "no spoke kubeconfig — the per-cell heartbeat credential is exactly what a hub-only check cannot see, so this is not a pass"
    fi
}
