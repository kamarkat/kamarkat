#!/bin/bash
# Boundary activation gating (ADR-055).
#
# Boundary CONTENT is reconciled from Git by the seed Application and needs no
# help from this script — that is the whole point of ADR-055, and the reason the
# old render-and-apply step that lived here is gone. What remains is the one
# thing Git deliberately does not own: boundary ACTIVATION.
#
# Activation is a deny sync window on each boundary's AppProject. It is excluded
# from Git reconciliation on purpose (it is the ADR-040 Day-0/Day-1 boundary),
# which means it is also the one piece of state that does NOT self-correct and is
# NOT reported as drift. A bootstrap interrupted between two boundary phases
# leaves a boundary closed forever, and nothing anywhere says so.
#
# This module is that missing check. It reports activation state, and closes the
# gap an interrupted run leaves behind.

# Resolve the management kubeconfig for the current cluster.
_gating_kubeconfig() {
    local kc="${KUBECONFIG_PATH:-}"
    if [[ -z "$kc" ]]; then
        kc="${KAMARKAT_HOME:-$HOME/.kamarkat}/run/kubeconfig/${CLUSTER_NAME}.kubeconfig"
    fi
    [[ -f "$kc" ]] || return 1
    echo "$kc"
}

# Is boundary N inactive (deny window present)?
_boundary_inactive() {
    local kc="$1" n="$2" windows
    windows=$(kubectl --kubeconfig="$kc" get appproject "$(printf 'boundary-%02d' "$n")" \
        -n platform-ops -o jsonpath='{.spec.syncWindows}' 2>/dev/null || echo "")
    [[ -n "$windows" && "$windows" != "null" ]]
}

# Open boundary N by removing its deny window. Idempotent: a merge patch setting
# the field to null removes it if present and does nothing if absent.
_open_boundary() {
    local kc="$1" n="$2"
    kubectl --kubeconfig="$kc" patch appproject "$(printf 'boundary-%02d' "$n")" \
        -n platform-ops --type merge -p '{"spec":{"syncWindows":null}}' >/dev/null 2>&1
}

# Step 1b: reconcile boundary activation state.
#
# Runs after the Go bootstrap, which owns the sequence itself. This does not
# re-drive that sequence — it reports the resulting state, and in sequenced mode
# opens any boundary the Go orchestrator did not reach, which is the signature of
# an interrupted run rather than of a healthy one.
step1b_reconcile_boundary_gates() {
    local kc
    if ! kc=$(_gating_kubeconfig); then
        log "  ⚠️  Management kubeconfig not found — skipping boundary gate reconciliation"
        return 0
    fi

    if ! kubectl --kubeconfig="$kc" get appproject boundary-01 -n platform-ops >/dev/null 2>&1; then
        log "  ⚠️  Boundary AppProjects not found — the seed has not been applied yet"
        return 0
    fi

    log "Reconciling boundary activation (gating=${GATING})..."

    if [[ "$GATING" == "converged" ]]; then
        # Converged creation seeds no windows at all. One present here means a
        # sequenced run's state was left behind on this cluster.
        local n stuck=0
        for n in 1 2 3 4 5 6; do
            if _boundary_inactive "$kc" "$n"; then
                log "  ⚠️  boundary-$(printf '%02d' "$n") is inactive under converged creation — opening it"
                _open_boundary "$kc" "$n"
                stuck=$((stuck + 1))
            fi
        done
        [[ $stuck -eq 0 ]] && log "  ✓ all boundaries active (converged)"
        return 0
    fi

    local n inactive=()
    for n in 1 2 3 4 5 6; do
        _boundary_inactive "$kc" "$n" && inactive+=("$n")
    done

    if [[ ${#inactive[@]} -eq 0 ]]; then
        log "  ✓ all six boundaries activated"
        return 0
    fi

    # Reaching here means the phase sequence did not complete. Say so plainly:
    # this state is invisible to ArgoCD, which reports no drift for a boundary
    # that is merely not permitted to sync.
    log "  ⚠️  ${#inactive[@]} boundary/boundaries still inactive after bootstrap: ${inactive[*]}"
    log "      This is not reported as drift — a boundary held closed looks idle, not failed."
    for n in "${inactive[@]}"; do
        log "      opening boundary-$(printf '%02d' "$n")"
        _open_boundary "$kc" "$n"
    done
    log "  ✓ boundary activation reconciled"
}
