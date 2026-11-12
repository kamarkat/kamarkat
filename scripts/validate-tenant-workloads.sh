#!/bin/bash
# validate-tenant-workloads.sh
#
# Tenant Workload Validation Script (ADR-021 / ADR-022)
#
# Validates that all fleet-registry changes for a given tenant namespace have
# been fully applied and are healthy on the spoke cluster. Run this after
# pushing changes to fleet-registry and waiting for ArgoCD to sync.
#
# Usage:
#   ./scripts/validate-tenant-workloads.sh <tenant-id>
#
# Examples:
#   ./scripts/validate-tenant-workloads.sh app-creator
#   KUBECONFIG=~/.kamarkat/run/kubeconfig/hub.kubeconfig \
#     ./scripts/validate-tenant-workloads.sh app-creator
#
# Environment variables:
#   KUBECONFIG   Path to kubeconfig (default: ~/.kamarkat/run/kubeconfig/hub.kubeconfig)
#   WORKLOAD_CLUSTER  ArgoCD cluster name of the workload cluster (REQUIRED, no default)
#                     SPOKE_NAME is accepted as the older spelling.
#   ARGOCD_NS    Namespace where ArgoCD runs (default: platform-ops)

set -euo pipefail

# ─── Configuration ────────────────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"
KUBECONFIG="${KUBECONFIG:-${KAMARKAT_HOME:-$HOME/.kamarkat}/run/kubeconfig/hub.kubeconfig}"
# No default, for the reason post-bootstrap-validate.sh records beside the same
# decision: a fallback to spoke-pool-eu-prod-01 validated whatever box was in
# front of it against the PRODUCTION pool name, found nothing, and reported the
# absence as a failure of the box rather than of the invocation.
#
# That name is also pre-ADR-082 -- workload clusters are named by the tenant now
# (nutgraf-01), so the fallback cannot match any box built since. Not knowing
# which cluster to check is an error, not a guess.
WORKLOAD_CLUSTER="${WORKLOAD_CLUSTER:-${SPOKE_NAME:-}}"
if [[ -z "$WORKLOAD_CLUSTER" ]]; then
    echo "validate-tenant-workloads: WORKLOAD_CLUSTER is not set." >&2
    echo "  Pass the workload cluster to validate; there is no safe default." >&2
    echo "  e.g. WORKLOAD_CLUSTER=nutgraf-01 $0" >&2
    exit 2
fi
# SPOKE_NAME is still read above and still used below, so existing callers and
# the rest of this script keep working while the vocabulary moves to kubefirst's
# mgmt/workload (kubefirst-api: Type is one of mgmt|workload).
SPOKE_NAME="$WORKLOAD_CLUSTER"
ARGOCD_NS="${ARGOCD_NS:-platform-ops}"
LOG_DIR="$PROJECT_ROOT/.zero-ops"
LOG_FILE="$LOG_DIR/validate-tenant-workloads.log"
TRACE_FILE="$LOG_DIR/validate-tenant-workloads-traces.log"

mkdir -p "$LOG_DIR"
> "$LOG_FILE"
> "$TRACE_FILE"

# ─── Argument parsing ─────────────────────────────────────────────────────────
if [[ $# -lt 1 ]]; then
    echo "Usage: $0 <tenant-id>"
    echo "  Example: $0 app-creator"
    exit 1
fi

TENANT_ID="$1"
TENANT_NS="tenant-${TENANT_ID}"

# ─── Counters ─────────────────────────────────────────────────────────────────
PASS=0
FAIL=0
WARN=0
declare -a FAILURES=()
declare -a WARNINGS=()

# ─── Logging ──────────────────────────────────────────────────────────────────
log()         { echo "[$(date '+%Y-%m-%d %H:%M:%S')] $1" | tee -a "$LOG_FILE"; }
log_pass()    { log "  ✅ $1"; ((PASS+=1)); }
log_fail()    { log "  ❌ $1"; FAILURES+=("$1"); ((FAIL+=1)); }
log_warn()    { log "  ⚠️  $1"; WARNINGS+=("$1"); ((WARN+=1)); }
log_section() { log ""; log "══════════════════════════════════════════"; log "  $1"; log "══════════════════════════════════════════"; }

# Capture verbose error traces (describe/log outputs) for failed checks.
capture_trace() {
    local title="$1"
    shift
    {
        echo "### $title"
        "$@" 2>&1 || true
        echo ""
    } >> "$TRACE_FILE"
}

capture_ns_events() {
    local ns="$1"
    capture_trace "Namespace Events: $ns (last 50)" kc_spoke get events -n "$ns" --sort-by='.lastTimestamp'
}

capture_rollout_deep_trace() {
    local ns="$1"
    local rollout="$2"

    capture_trace "Rollout YAML: $ns/$rollout" kc_spoke get rollout "$rollout" -n "$ns" -o yaml
    capture_trace "Rollout Describe: $ns/$rollout" kc_spoke describe rollout "$rollout" -n "$ns"

    local rs_names
    rs_names=$(kc_spoke get rs -n "$ns" -l "rollouts.kubernetes.io/rollout-name=$rollout" -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' || true)
    if [[ -n "${rs_names:-}" ]]; then
        while IFS= read -r rs; do
            [[ -z "$rs" ]] && continue
            capture_trace "ReplicaSet Describe: $ns/$rs" kc_spoke describe rs "$rs" -n "$ns"
        done <<< "$rs_names"
    fi

    local pod_names
    pod_names=$(kc_spoke get pods -n "$ns" -l "rollouts.kubernetes.io/rollout-name=$rollout" -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' || true)
    if [[ -n "${pod_names:-}" ]]; then
        while IFS= read -r pod; do
            [[ -z "$pod" ]] && continue
            capture_trace "Pod Describe: $ns/$pod" kc_spoke describe pod "$pod" -n "$ns"

            # Capture logs for all containers in the pod (best effort).
            local containers
            containers=$(kc_spoke get pod "$pod" -n "$ns" -o jsonpath='{range .spec.containers[*]}{.name}{"\n"}{end}' || true)
            while IFS= read -r c; do
                [[ -z "$c" ]] && continue
                capture_trace "Pod Logs: $ns/$pod container=$c" kc_spoke logs "$pod" -n "$ns" -c "$c" --tail=120
                capture_trace "Pod Previous Logs: $ns/$pod container=$c" kc_spoke logs "$pod" -n "$ns" -c "$c" --previous --tail=120
            done <<< "$containers"
        done <<< "$pod_names"
    fi
}

# ─── kubectl wrappers ─────────────────────────────────────────────────────────
# kc_hub: runs against the hub cluster (where ArgoCD and Crossplane live)
kc_hub() { kubectl --kubeconfig="$KUBECONFIG" "$@"; }

# kc_spoke: runs against the spoke cluster (where tenant workloads live)
# Uses the spoke kubeconfig path derived from SPOKE_NAME.
# No fallback is allowed: missing spoke kubeconfig is a hard failure.
_SPOKE_KUBECONFIG=""
_resolve_spoke_kubeconfig() {
    local spoke_kc="${KAMARKAT_HOME:-$HOME/.kamarkat}/run/kubeconfig/${SPOKE_NAME}.kubeconfig"
    if [[ -f "$spoke_kc" ]]; then
        _SPOKE_KUBECONFIG="$spoke_kc"
    else
        log "ERROR: Spoke kubeconfig not found: $spoke_kc"
        log "       Set SPOKE_NAME correctly or create the spoke kubeconfig file."
        exit 1
    fi
}
kc_spoke() { kubectl --kubeconfig="$_SPOKE_KUBECONFIG" "$@"; }

# ─── Helpers ──────────────────────────────────────────────────────────────────

# Check an ArgoCD Application is Synced + Healthy on the hub
check_argocd_app() {
    local app="$1"
    local severity="${2:-FAIL}"

    local sync health
    sync=$(kc_hub get application "$app" -n "$ARGOCD_NS" \
        -o jsonpath='{.status.sync.status}' 2>/dev/null || echo "NotFound")
    health=$(kc_hub get application "$app" -n "$ARGOCD_NS" \
        -o jsonpath='{.status.health.status}' 2>/dev/null || echo "NotFound")

    if [[ "$sync" == "NotFound" ]]; then
        if [[ "$severity" == "WARN" ]]; then
            log_warn "ArgoCD app '$app': not found (may not exist yet)"
        else
            log_fail "ArgoCD app '$app': not found"
            capture_trace "ArgoCD Application Describe: $app" kc_hub describe application "$app" -n "$ARGOCD_NS"
            capture_trace "ArgoCD Application YAML: $app" kc_hub get application "$app" -n "$ARGOCD_NS" -o yaml
        fi
        return
    fi

    if [[ "$sync" == "Synced" && "$health" == "Healthy" ]]; then
        log_pass "ArgoCD app '$app': Synced + Healthy"
    elif [[ "$sync" == "Synced" && "$health" == "Progressing" ]]; then
        log_warn "ArgoCD app '$app': Synced but still Progressing (workloads converging)"
    else
        # Collect out-of-sync resource names for context
        local oosync
        oosync=$(kc_hub get application "$app" -n "$ARGOCD_NS" \
            -o jsonpath='{range .status.resources[?(@.status=="OutOfSync")]}{.kind}/{.name} {end}' \
            2>/dev/null || echo "")
        local msg="ArgoCD app '$app': Sync=$sync Health=$health"
        [[ -n "$oosync" ]] && msg="$msg  [OutOfSync: $oosync]"
        if [[ "$severity" == "WARN" ]]; then
            log_warn "$msg"
        else
            log_fail "$msg"
            capture_trace "ArgoCD Application Describe: $app" kc_hub describe application "$app" -n "$ARGOCD_NS"
            capture_trace "ArgoCD Application YAML: $app" kc_hub get application "$app" -n "$ARGOCD_NS" -o yaml
        fi
    fi
}

# Check all pods in a namespace are Running or Completed
check_namespace_pods() {
    local ns="$1"
    local label="${2:-}"
    local label_arg=""
    [[ -n "$label" ]] && label_arg="-l $label"

    local total not_ready
    # shellcheck disable=SC2086
    total=$(kc_spoke get pods -n "$ns" $label_arg --no-headers | wc -l | tr -d '[:space:]')
    # shellcheck disable=SC2086
    not_ready=$(kc_spoke get pods -n "$ns" $label_arg --no-headers \
        | grep -v -E '\s+(Running|Completed)\s+' | grep -v "^$" || true)

    if [[ "$total" -eq 0 ]]; then
        log_warn "Namespace $ns: no pods found${label:+ (label: $label)}"
        return
    fi

    if [[ -z "$not_ready" ]]; then
        log_pass "Namespace $ns: all $total pod(s) healthy${label:+ (label: $label)}"
    else
        local crash pending err
        crash=$(echo "$not_ready" | grep -c "CrashLoopBackOff" || true)
        pending=$(echo "$not_ready" | grep -c "Pending" || true)
        err=$(echo "$not_ready" | grep -c "Error" || true)
        log_fail "Namespace $ns: unhealthy pods (CrashLoop=$crash Pending=$pending Error=$err)"
        echo "$not_ready" | while IFS= read -r line; do
            [[ -n "$line" ]] && log "     → $line"
        done
    fi
}

# Check a Deployment is fully available
check_deployment() {
    local ns="$1"
    local name="$2"
    local desired available
    desired=$(kc_spoke get deployment "$name" -n "$ns" \
        -o jsonpath='{.spec.replicas}' 2>/dev/null || echo "0")
    available=$(kc_spoke get deployment "$name" -n "$ns" \
        -o jsonpath='{.status.availableReplicas}' 2>/dev/null || echo "0")
    desired="${desired:-0}"; available="${available:-0}"

    if [[ "$desired" -gt 0 && "$available" -ge "$desired" ]]; then
        log_pass "Deployment $ns/$name: $available/$desired available"
    else
        log_fail "Deployment $ns/$name: $available/$desired available"
        capture_trace "Deployment Describe: $ns/$name" kc_spoke describe deployment "$name" -n "$ns"
        capture_trace "Deployment YAML: $ns/$name" kc_spoke get deployment "$name" -n "$ns" -o yaml
        capture_ns_events "$ns"
    fi
}

# Check an Argo Rollout is healthy (phase=Healthy, all replicas available)
check_rollout() {
    local ns="$1"
    local name="$2"
    local phase available desired
    phase=$(kc_spoke get rollout "$name" -n "$ns" \
        -o jsonpath='{.status.phase}' 2>/dev/null || echo "NotFound")
    available=$(kc_spoke get rollout "$name" -n "$ns" \
        -o jsonpath='{.status.availableReplicas}' 2>/dev/null || echo "0")
    desired=$(kc_spoke get rollout "$name" -n "$ns" \
        -o jsonpath='{.spec.replicas}' 2>/dev/null || echo "0")
    available="${available:-0}"; desired="${desired:-0}"

    if [[ "$phase" == "NotFound" ]]; then
        log_fail "Rollout $ns/$name: not found"
        capture_rollout_deep_trace "$ns" "$name"
        capture_ns_events "$ns"
    elif [[ "$phase" == "Healthy" && "$available" -ge "$desired" ]]; then
        log_pass "Rollout $ns/$name: phase=$phase available=$available/$desired"
    elif [[ "$phase" == "Progressing" ]]; then
        log_warn "Rollout $ns/$name: phase=$phase available=$available/$desired (canary in progress)"
        capture_rollout_deep_trace "$ns" "$name"
    else
        log_fail "Rollout $ns/$name: phase=$phase available=$available/$desired"
        capture_rollout_deep_trace "$ns" "$name"
        capture_ns_events "$ns"
    fi
}

# Check a Job completed successfully (or is still running)
check_job() {
    local ns="$1"
    local name="$2"
    local succeeded failed active
    succeeded=$(kc_spoke get job "$name" -n "$ns" \
        -o jsonpath='{.status.succeeded}' 2>/dev/null || echo "0")
    failed=$(kc_spoke get job "$name" -n "$ns" \
        -o jsonpath='{.status.failed}' 2>/dev/null || echo "0")
    active=$(kc_spoke get job "$name" -n "$ns" \
        -o jsonpath='{.status.active}' 2>/dev/null || echo "0")
    succeeded="${succeeded:-0}"; failed="${failed:-0}"; active="${active:-0}"

    if [[ "$succeeded" -ge 1 ]]; then
        log_pass "Job $ns/$name: completed (succeeded=$succeeded)"
    elif [[ "$active" -ge 1 ]]; then
        log_warn "Job $ns/$name: still running (active=$active)"
    elif [[ "$failed" -ge 1 ]]; then
        log_fail "Job $ns/$name: failed (failed=$failed succeeded=$succeeded)"
    else
        # Job may have been cleaned up by ttlSecondsAfterFinished — that is expected
        log_warn "Job $ns/$name: not found (may have been cleaned up by TTL — check events)"
    fi
}

# Check a Service exists and has endpoints
check_service() {
    local ns="$1"
    local name="$2"
    local port="${3:-}"

    local cluster_ip
    cluster_ip=$(kc_spoke get svc "$name" -n "$ns" \
        -o jsonpath='{.spec.clusterIP}' 2>/dev/null || echo "")

    if [[ -z "$cluster_ip" ]]; then
        log_fail "Service $ns/$name: not found"
        capture_trace "Service Describe: $ns/$name" kc_spoke describe svc "$name" -n "$ns"
        capture_ns_events "$ns"
        return
    fi

    local endpoint_count
    endpoint_count=$(kc_spoke get endpoints "$name" -n "$ns" \
        -o jsonpath='{range .subsets[*].addresses[*]}{.ip}{"\n"}{end}' \
        | wc -l | tr -d '[:space:]')

    if [[ "$endpoint_count" -gt 0 ]]; then
        log_pass "Service $ns/$name: ClusterIP=$cluster_ip endpoints=$endpoint_count${port:+ port=$port}"
    else
        log_warn "Service $ns/$name: ClusterIP=$cluster_ip but no ready endpoints (pods may still be starting)"
        capture_trace "Endpoints YAML: $ns/$name" kc_spoke get endpoints "$name" -n "$ns" -o yaml
        capture_ns_events "$ns"
    fi
}

# Check an Ingress has an address assigned
check_ingress() {
    local ns="$1"
    local name="$2"
    local address
    address=$(kc_spoke get ingress "$name" -n "$ns" \
        -o jsonpath='{.status.loadBalancer.ingress[0].ip}{.status.loadBalancer.ingress[0].hostname}' \
        2>/dev/null || echo "")

    local hosts
    hosts=$(kc_spoke get ingress "$name" -n "$ns" \
        -o jsonpath='{range .spec.rules[*]}{.host}{"\n"}{end}' 2>/dev/null | tr '\n' ' ' || echo "")

    if [[ -n "$address" ]]; then
        log_pass "Ingress $ns/$name: address=$address hosts=[$hosts]"
    else
        log_warn "Ingress $ns/$name: no address assigned yet (cert-manager may still be issuing TLS)"
    fi
}

# Check a CiliumNetworkPolicy exists
check_cilium_policy() {
    local ns="$1"
    local name="$2"
    if kc_spoke get ciliumnetworkpolicy "$name" -n "$ns" >/dev/null 2>&1; then
        log_pass "CiliumNetworkPolicy $ns/$name: present"
    else
        log_fail "CiliumNetworkPolicy $ns/$name: not found"
    fi
}

# Check a ConfigMap exists
check_configmap() {
    local ns="$1"
    local name="$2"
    if kc_spoke get configmap "$name" -n "$ns" >/dev/null 2>&1; then
        log_pass "ConfigMap $ns/$name: present"
    else
        log_fail "ConfigMap $ns/$name: not found"
        capture_trace "ConfigMap Get: $ns/$name" kc_spoke get configmap "$name" -n "$ns" -o yaml
        capture_ns_events "$ns"
    fi
}

# Check a ServiceAccount exists
check_serviceaccount() {
    local ns="$1"
    local name="$2"
    if kc_spoke get serviceaccount "$name" -n "$ns" >/dev/null 2>&1; then
        log_pass "ServiceAccount $ns/$name: present"
    else
        log_fail "ServiceAccount $ns/$name: not found"
        capture_trace "ServiceAccount Get: $ns/$name" kc_spoke get serviceaccount "$name" -n "$ns" -o yaml
        capture_ns_events "$ns"
    fi
}

# Check a Secret exists (does not print value)
check_secret() {
    local ns="$1"
    local name="$2"
    if kc_spoke get secret "$name" -n "$ns" >/dev/null 2>&1; then
        log_pass "Secret $ns/$name: present"
    else
        log_fail "Secret $ns/$name: not found (database provisioning may not have completed)"
        capture_trace "Secret Get: $ns/$name" kc_spoke get secret "$name" -n "$ns" -o yaml
        capture_ns_events "$ns"

        # Deep traces for known producer chain of DB credentials.
        if [[ "$name" == *"pooler-app" ]]; then
            capture_trace "TenantDatabase list: $ns" kc_spoke get tenantdatabase -n "$ns" -o yaml
            capture_trace "Pooler list: $ns" kc_spoke get pooler -n "$ns" -o yaml
            capture_trace "Spoke infrastructure app describe" kc_hub describe application "${SPOKE_NAME}-infrastructure" -n "$ARGOCD_NS"
        fi
    fi
}

# Check namespace PSA labels are set correctly
check_namespace_psa() {
    local ns="$1"
    local enforce
    enforce=$(kc_spoke get namespace "$ns" \
        -o jsonpath='{.metadata.labels.pod-security\.kubernetes\.io/enforce}' \
        2>/dev/null || echo "")
    if [[ "$enforce" == "restricted" ]]; then
        log_pass "Namespace $ns: PSA enforce=restricted"
    else
        log_fail "Namespace $ns: PSA enforce label missing or not 'restricted' (got: '${enforce:-<empty>}')"
    fi
}

# Check AINativeSaaS XR is Ready+Synced on the hub
check_ainativesaas_xr() {
    local name="$1"
    local ready synced
    ready=$(kc_hub get ainativesaas "$name" \
        -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || echo "Unknown")
    synced=$(kc_hub get ainativesaas "$name" \
        -o jsonpath='{.status.conditions[?(@.type=="Synced")].status}' 2>/dev/null || echo "Unknown")

    if [[ "$ready" == "True" && "$synced" == "True" ]]; then
        log_pass "AINativeSaaS XR '$name': Ready + Synced"
    else
        local msg
        msg=$(kc_hub get ainativesaas "$name" \
            -o jsonpath='{.status.conditions[?(@.type=="Synced")].message}' 2>/dev/null || echo "")
        log_fail "AINativeSaaS XR '$name': Ready=$ready Synced=$synced${msg:+  [$msg]}"
    fi
}

# ─── 0. PREFLIGHT ─────────────────────────────────────────────────────────────
preflight() {
    log_section "0. PREFLIGHT"

    if [[ ! -f "$KUBECONFIG" ]]; then
        log "ERROR: kubeconfig not found: $KUBECONFIG"
        log "       Set KUBECONFIG env var or run from the zero-ops repo root."
        exit 1
    fi

    if ! kc_hub cluster-info >/dev/null 2>&1; then
        log "ERROR: Cannot connect to hub cluster using $KUBECONFIG"
        exit 1
    fi
    local server
    server=$( (kc_hub cluster-info | sed -n 's/.*control plane.* at //p' | tr -d '\r') || true )
    server="${server:-unknown}"
    log_pass "Hub cluster reachable: $server"

    _resolve_spoke_kubeconfig

    if ! kc_spoke cluster-info >/dev/null 2>&1; then
        log "ERROR: Cannot connect to spoke cluster using $_SPOKE_KUBECONFIG"
        exit 1
    fi
    log_pass "Spoke cluster reachable (kubeconfig: $_SPOKE_KUBECONFIG)"

    log_pass "Tenant ID: $TENANT_ID"
    log_pass "Tenant namespace: $TENANT_NS"
}

# ─── 1. ARGOCD APPLICATIONS ───────────────────────────────────────────────────
check_argocd_apps() {
    log_section "1. ARGOCD APPLICATIONS"

    # XR application (hub-side — provisions data layer via Crossplane)
    check_argocd_app "${TENANT_ID}-xr" "FAIL"

    # Spoke application (spoke-side — provisions ConfigMap + migrations)
    check_argocd_app "${TENANT_ID}-spoke" "FAIL"

    # Workload application (spoke-side — provisions BFF, frontend, ingress)
    check_argocd_app "tenant-${TENANT_ID}-workloads" "FAIL"
}

# ─── 2. NAMESPACE ─────────────────────────────────────────────────────────────
check_namespace() {
    log_section "2. NAMESPACE"

    if kc_spoke get namespace "$TENANT_NS" >/dev/null 2>&1; then
        log_pass "Namespace $TENANT_NS: exists"
    else
        log_fail "Namespace $TENANT_NS: not found (XR provisioning may not have completed)"
        return
    fi

    # ADR-021 Blocker 4: PSA restricted must be enforced
    check_namespace_psa "$TENANT_NS"
}

# ─── 3. DATA LAYER (XR + DATABASE) ───────────────────────────────────────────
check_data_layer() {
    log_section "3. DATA LAYER (XR + DATABASE)"

    # AINativeSaaS XR on hub
    check_ainativesaas_xr "$TENANT_ID"

    # Database credentials secret (created by TenantDatabase XR on spoke)
    check_secret "$TENANT_NS" "${TENANT_ID}-pooler-app"


    # Migration ConfigMap (created by spoke ApplicationSet from universal-tenant chart)
    check_configmap "$TENANT_NS" "tenant-${TENANT_ID}-migrations"
}

# ─── 4. SERVICE ACCOUNTS ──────────────────────────────────────────────────────
check_service_accounts() {
    log_section "4. SERVICE ACCOUNTS (ADR-021 Blocker 6)"

    # ADR-022: namePrefix bff- and frontend- are applied by Kustomize overlays.
    check_serviceaccount "$TENANT_NS" "bff-workload-sa"
    check_serviceaccount "$TENANT_NS" "frontend-workload-sa"
    check_serviceaccount "$TENANT_NS" "migration-sa"
}

# ─── 5. NETWORK POLICY ────────────────────────────────────────────────────────
check_network_policy() {
    log_section "5. NETWORK POLICY (ADR-021 Blocker 3)"

    # CiliumNetworkPolicy — namePrefix bff- applied by Kustomize
    check_cilium_policy "$TENANT_NS" "bff-strict-egress-contract"
    check_cilium_policy "$TENANT_NS" "frontend-strict-egress-contract"
}

# ─── 6. MIGRATION JOB ─────────────────────────────────────────────────────────
check_migration_job() {
    log_section "6. MIGRATION JOB (ADR-021 Blockers 5, 8, 11, 12)"

    # ConfigMap with tenant SQL
    check_configmap "$TENANT_NS" "${TENANT_ID}-custom-migrations"

    # Job (PreSync hook — may have been cleaned up by TTL after success)
    check_job "$TENANT_NS" "custom-schema-migration"
}

# ─── 7. BFF WORKLOAD ──────────────────────────────────────────────────────────
check_bff() {
    log_section "7. BFF WORKLOAD"

    # Argo Rollout (namePrefix bff- applied by Kustomize)
    check_rollout "$TENANT_NS" "bff-workload"

    # Service
    check_service "$TENANT_NS" "bff-workload" "4000"

    # Pod health
    check_namespace_pods "$TENANT_NS" "app=bff"
}

# ─── 8. FRONTEND WORKLOAD ─────────────────────────────────────────────────────
check_frontend() {
    log_section "8. FRONTEND WORKLOAD"

    # Argo Rollout (namePrefix frontend- applied by Kustomize)
    check_rollout "$TENANT_NS" "frontend-workload"

    # Service
    check_service "$TENANT_NS" "frontend-workload" "3000"

    # Pod health
    check_namespace_pods "$TENANT_NS" "app=frontend"
}

# ─── 9. INGRESS ───────────────────────────────────────────────────────────────
check_ingress_resources() {
    log_section "9. INGRESS (ADR-021 Blocker 7)"

    check_ingress "$TENANT_NS" "${TENANT_ID}-ingress"

    # TLS secret (created by cert-manager after Ingress is applied)
    check_secret "$TENANT_NS" "${TENANT_ID}-tls"
}

# ─── 10. KYVERNO ABI ENFORCEMENT ─────────────────────────────────────────────
check_kyverno_abi() {
    log_section "10. KYVERNO ABI ENFORCEMENT (ADR-021)"

    # ClusterPolicy must be present on the spoke
    if kc_spoke get clusterpolicy enforce-tenant-abi >/dev/null 2>&1; then
        local ready
        ready=$(kc_spoke get clusterpolicy enforce-tenant-abi \
            -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || echo "Unknown")
        if [[ "$ready" == "True" ]]; then
            log_pass "Kyverno ClusterPolicy 'enforce-tenant-abi': Ready"
        else
            log_warn "Kyverno ClusterPolicy 'enforce-tenant-abi': Ready=$ready (may still be loading)"
        fi
    else
        log_fail "Kyverno ClusterPolicy 'enforce-tenant-abi': not found on spoke"
        capture_trace "ClusterPolicy Get/Describe: enforce-tenant-abi" kc_spoke get clusterpolicy enforce-tenant-abi -o yaml
        capture_trace "Spoke Infrastructure App Describe" kc_hub describe application "${SPOKE_NAME}-infrastructure" -n "$ARGOCD_NS"
        capture_trace "Spoke Kyverno App Describe" kc_hub describe application "${SPOKE_NAME}-kyverno" -n "$ARGOCD_NS"
    fi

    # Verify at least one pod in the namespace has the required labels
    local labeled_pods
    labeled_pods=$(kc_spoke get pods -n "$TENANT_NS" \
        -l "tenant-id=${TENANT_ID}" --no-headers | wc -l | tr -d '[:space:]')
    if [[ "$labeled_pods" -gt 0 ]]; then
        log_pass "Namespace $TENANT_NS: $labeled_pods pod(s) carry tenant-id=$TENANT_ID label"
    else
        log_warn "Namespace $TENANT_NS: no pods with tenant-id=$TENANT_ID label found yet"
    fi
}

# ─── 11. RECENT EVENTS ────────────────────────────────────────────────────────
check_events() {
    log_section "11. RECENT WARNING EVENTS"

    local warnings
    warnings=$(kc_spoke get events -n "$TENANT_NS" \
        --field-selector type=Warning \
        --sort-by='.lastTimestamp' 2>/dev/null \
        | tail -10 || true)

    if [[ -z "$warnings" || "$warnings" == "No resources found"* ]]; then
        log_pass "Namespace $TENANT_NS: no Warning events"
    else
        log_warn "Namespace $TENANT_NS: recent Warning events (last 10):"
        echo "$warnings" | while IFS= read -r line; do
            [[ -n "$line" ]] && log "     $line"
        done
    fi
}

# ─── SUMMARY ──────────────────────────────────────────────────────────────────
print_summary() {
    log ""
    log "╔══════════════════════════════════════════════╗"
    log "║   TENANT WORKLOAD VALIDATION SUMMARY         ║"
    log "╠══════════════════════════════════════════════╣"
    log "║  Tenant:   $TENANT_ID"
    log "║  Namespace: $TENANT_NS"
    log "╠══════════════════════════════════════════════╣"
    log "║  ✅ PASSED : $PASS"
    log "║  ❌ FAILED : $FAIL"
    log "║  ⚠️  WARNED : $WARN"
    log "╠══════════════════════════════════════════════╣"

    if [[ ${#FAILURES[@]} -gt 0 ]]; then
        log "║  FAILURES:"
        for f in "${FAILURES[@]}"; do
            log "║    ✗ $f"
        done
        log "╠══════════════════════════════════════════════╣"
    fi

    if [[ ${#WARNINGS[@]} -gt 0 ]]; then
        log "║  WARNINGS:"
        for w in "${WARNINGS[@]}"; do
            log "║    ! $w"
        done
        log "╠══════════════════════════════════════════════╣"
    fi

    if [[ -s "$TRACE_FILE" ]]; then
        log "║  ERROR TRACE EXTRACTS (describe/get):"
        while IFS= read -r line; do
            log "║    $line"
        done < <(sed -n '1,120p' "$TRACE_FILE")
        log "╠══════════════════════════════════════════════╣"
    fi

    if [[ "$FAIL" -eq 0 ]]; then
        log "║  🎉 ALL CRITICAL CHECKS PASSED               ║"
    else
        log "║  💥 $FAIL CRITICAL CHECK(S) FAILED            ║"
        log "║  Review failures above before proceeding      ║"
    fi
    log "╚══════════════════════════════════════════════╝"
    log "  Full log: $LOG_FILE"
    log "  Trace log: $TRACE_FILE"
}

# ─── MAIN ─────────────────────────────────────────────────────────────────────
main() {
    log "═══════════════════════════════════════════════════════════"
    log " Tenant Workload Validation — ADR-021 / ADR-022"
    log " Tenant:     $TENANT_ID"
    log " Namespace:  $TENANT_NS"
    log " Hub config: $KUBECONFIG"
    log " Spoke:      $SPOKE_NAME"
    log "═══════════════════════════════════════════════════════════"

    preflight
    check_argocd_apps
    check_namespace
    check_data_layer
    check_service_accounts
    check_network_policy
    check_migration_job
    check_bff
    check_frontend
    check_ingress_resources
    check_kyverno_abi
    check_events

    print_summary

    [[ "$FAIL" -gt 0 ]] && exit 1
    exit 0
}

trap 'log "Script interrupted"; print_summary; exit 1' INT TERM

main "$@"
