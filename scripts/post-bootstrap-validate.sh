#!/bin/bash
# Zero-Ops Hub Post-Bootstrap Core Services Validation
# Run after hub-bootstrap.sh completes to validate all platform services are healthy.
# Reports a full pass/fail summary and exits non-zero if any critical check fails.

set -uo pipefail

# ─── Configuration ───────────────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"
# Honours a caller's ZERO_OPS_DIR, which is the workspace the box was
# bootstrapped from. Hardcoding PROJECT_ROOT wrote this script's logs into the
# platform checkout even when it was validating a tenant's box.
ZERO_OPS_DIR="${ZERO_OPS_DIR:-$PROJECT_ROOT}"
# .state, not .zero-ops. The latter was replaced, and a script still writing to
# it recreates the directory on every run.
LOG_DIR="$ZERO_OPS_DIR/.state/logs"
LOG_FILE="$LOG_DIR/post-bootstrap-validate.log"
# No default. It used to fall back to k8-secrets/kubeconfig/hub.kubeconfig, a
# cluster named "hub" -- so a run against a box named anything else silently
# validated the wrong cluster, or a stale kubeconfig belonging to a deleted one.
# Every caller passes KUBECONFIG; not being told which cluster to check is an
# error, not something to guess at.
if [[ -z "${KUBECONFIG:-}" ]]; then
    echo "post-bootstrap-validate: KUBECONFIG is not set." >&2
    echo "  Pass the kubeconfig of the box to validate; there is no default." >&2
    exit 2
fi
# Provider matrix: cloud providers use hetzner infra and production spoke.
CAPI_INFRA_PROVIDER="hetzner"
# No default. It used to fall back to spoke-pool-eu-prod-01, so a run that
# forgot to pass one validated a dev box against the PRODUCTION pool name, found
# nothing, and reported the spoke checks as failures of the box rather than of
# the invocation. Every caller passes it; not knowing is an error, not a guess.
if [[ -z "${SPOKEPOOL_NAME:-}" ]]; then
    echo "post-bootstrap-validate: SPOKEPOOL_NAME is not set." >&2
    echo "  Pass the SpokePool this box declares; there is no safe default." >&2
    exit 2
fi

# Timeout for individual checks (seconds)
DEPLOY_READY_TIMEOUT="${DEPLOY_READY_TIMEOUT:-120}"
ARGOCD_SYNC_TIMEOUT="${ARGOCD_SYNC_TIMEOUT:-60}"

mkdir -p "$LOG_DIR"
# Clear log on each run (do not append to stale output from previous runs)
> "$LOG_FILE"

# The environment this hub belongs to. Passed through by hub-bootstrap.sh;
# defaults to dev for manual runs.
ENVIRONMENT="${ENVIRONMENT:-dev}"

# ─── Counters ─────────────────────────────────────────────────────────────────
PASS=0
FAIL=0
WARN=0
declare -a FAILURES=()
declare -a WARNINGS=()

# ─── Logging ──────────────────────────────────────────────────────────────────
log() { echo "[$(date '+%Y-%m-%d %H:%M:%S')] $1" | tee -a "$LOG_FILE"; }
log_pass()  { log "  ✅ $1"; ((PASS++)); }
log_fail()  { log "  ❌ $1"; FAILURES+=("$1"); ((FAIL++)); }
log_warn()  { log "  ⚠️  $1"; WARNINGS+=("$1"); ((WARN++)); }
log_section() { log ""; log "══════════════════════════════════════════"; log "  $1"; log "══════════════════════════════════════════"; }

# ─── Waiting for converging state ────────────────────────────────────────────
#
# Every check below sampled the cluster exactly once. This script runs the
# instant bootstrap returns, so a component still converging -- a CNPG cluster
# electing its primary, an Application mid-sync -- was reported as a failure and
# ended the run on a box that was minutes from correct. On 2026-09-17 three of
# the five reported failures were healthy by the time anyone looked.
#
# The obvious repair is to sleep before validating. This is the better one. A
# sleep is a guess that does not generalise across a cold image cache or a slower
# node; it adds its full cost to runs that were going to fail anyway; and after
# it, a pass means "ready, or we waited long enough" with no way to tell which.
# settle() waits only as long as it must and reports the condition rather than a
# timestamp, so "converging" stays distinguishable from "broken".
#
# Use it ONLY where waiting can change the answer. A namespace no manifest
# declares and an image reference that 404s are answers now and in ten minutes;
# waiting on those turns a correct, instant failure into a slow one.
SETTLE_DEADLINE="${SETTLE_DEADLINE:-180}"
SETTLE_INTERVAL="${SETTLE_INTERVAL:-10}"

# settle <what> <command...> — poll until the command succeeds or the deadline
# passes. Returns 0 if it became true.
settle() {
    local what="$1"; shift
    local waited=0

    "$@" && return 0
    (( SETTLE_DEADLINE <= 0 )) && return 1

    log "     waiting up to ${SETTLE_DEADLINE}s for $what"
    while (( waited < SETTLE_DEADLINE )); do
        sleep "$SETTLE_INTERVAL"
        waited=$(( waited + SETTLE_INTERVAL ))
        if "$@"; then
            log "     $what after ${waited}s"
            return 0
        fi
    done
    return 1
}

# ─── kubectl wrapper ──────────────────────────────────────────────────────────
kc() { kubectl --kubeconfig="$KUBECONFIG" "$@" 2>/dev/null; }

# ─── Spoke kubeconfig ─────────────────────────────────────────────────────────
# The hub-side readiness signals (SpokePool Ready, CAPI Provisioned, bootstrap
# cert Ready) do NOT prove the spoke is functional — its ArgoCD agent, platform
# operators, and storage driver must be validated directly against the spoke
# cluster. Extract the spoke's admin kubeconfig from the CAPI secret.
SPOKE_KUBECONFIG=""
SPOKE_KC_TMP=""
get_spoke_kubeconfig() {
    local secret="${SPOKEPOOL_NAME}-kubeconfig"
    SPOKE_KC_TMP="$(mktemp)"
    if ! kc get secret "$secret" -n platform-capi -o jsonpath='{.data.value}' 2>/dev/null \
        | base64 -d > "$SPOKE_KC_TMP" 2>/dev/null; then
        log_warn "Spoke kubeconfig secret platform-capi/$secret not found — spoke platform checks skipped"
        SPOKE_KUBECONFIG=""
        return 0
    fi
    SPOKE_KUBECONFIG="$SPOKE_KC_TMP"
    if ! kubectl --kubeconfig="$SPOKE_KUBECONFIG" cluster-info >/dev/null 2>&1; then
        log_warn "Spoke kubeconfig unusable (agent/API unreachable) — spoke platform checks skipped"
        SPOKE_KUBECONFIG=""
    fi
}
kc_spoke() { kubectl --kubeconfig="$SPOKE_KUBECONFIG" "$@" 2>/dev/null; }

# ─── Helpers ──────────────────────────────────────────────────────────────────

# Check all pods in a namespace are Running/Completed (no CrashLoop, Pending, etc.)
check_namespace_pods() {
    local ns="$1"
    local label_selector="${2:-}"
    local label_arg=""
    [[ -n "$label_selector" ]] && label_arg="-l $label_selector"

    local not_ready
    # shellcheck disable=SC2086
    not_ready=$(kc get pods -n "$ns" $label_arg --no-headers 2>/dev/null \
        | grep -v -E '([0-9]+/[0-9]+\s+(Running|Completed))' \
        | grep -v "^$" || true)

    if [[ -z "$not_ready" ]]; then
        local count
        # shellcheck disable=SC2086
        count=$(kc get pods -n "$ns" $label_arg --no-headers 2>/dev/null | grep -c '' || echo 0)
        if [[ "$count" -eq 0 ]]; then
            log_warn "Namespace $ns: no pods found (nothing deployed yet?)"
        else
            log_pass "Namespace $ns: all $count pod(s) healthy"
        fi
    else
        local crashloop_count
        crashloop_count=$(echo "$not_ready" | grep -c "CrashLoopBackOff" || true)
        local pending_count
        pending_count=$(echo "$not_ready" | grep -c "Pending" || true)
        local error_count
        error_count=$(echo "$not_ready" | grep -c "Error" || true)
        log_fail "Namespace $ns: unhealthy pods (CrashLoop=$crashloop_count, Pending=$pending_count, Error=$error_count)"
        echo "$not_ready" | while IFS= read -r line; do
            [[ -n "$line" ]] && log "     → $line"
        done
    fi
}

# Check a specific deployment is available
check_deployment() {
    local ns="$1"
    local name="$2"
    local desired
    desired=$(kc get deployment "$name" -n "$ns" -o jsonpath='{.spec.replicas}' 2>/dev/null || echo "0")
    local available
    available=$(kc get deployment "$name" -n "$ns" -o jsonpath='{.status.availableReplicas}' 2>/dev/null || echo "0")
    desired="${desired:-0}"; available="${available:-0}"

    if [[ "$available" -ge "$desired" && "$desired" -gt 0 ]]; then
        log_pass "Deployment $ns/$name: $available/$desired available"
    else
        log_fail "Deployment $ns/$name: $available/$desired available"
    fi
}

# Check ArgoCD application sync + health
check_argocd_app() {
    local app="$1"
    local severity="${2:-FAIL}"   # FAIL or WARN
    # Synced+Healthy is a converging state, so it is waited for rather than
    # sampled. platform-database was reported OutOfSync on an ExternalSecret that
    # was Synced a minute later, and that one line failed the run.
    _app_converged() {
        [[ "$(kc get application "$app" -n platform-ops -o jsonpath='{.status.sync.status}' 2>/dev/null)" == "Synced" ]] &&
        [[ "$(kc get application "$app" -n platform-ops -o jsonpath='{.status.health.status}' 2>/dev/null)" == "Healthy" ]]
    }
    settle "ArgoCD app $app to reach Synced+Healthy" _app_converged || true

    local sync
    sync=$(kc get application "$app" -n platform-ops -o jsonpath='{.status.sync.status}' 2>/dev/null || echo "Unknown")
    local health
    health=$(kc get application "$app" -n platform-ops -o jsonpath='{.status.health.status}' 2>/dev/null || echo "Unknown")

    if [[ "$sync" == "Synced" && "$health" == "Healthy" ]]; then
        log_pass "ArgoCD app $app: Sync=$sync Health=$health"
    elif [[ "$sync" == "Synced" && "$health" == "Progressing" ]]; then
        # Progressing is transitional — warn regardless of severity so it is visible
        log_warn "ArgoCD app $app: Sync=$sync Health=$health (still converging)"
    else
        local out_of_sync_resources
        out_of_sync_resources=$(kc get application "$app" -n platform-ops \
            -o jsonpath='{range .status.resources[?(@.status=="OutOfSync")]}{.kind}/{.name} {end}' 2>/dev/null || echo "")
        local msg="ArgoCD app $app: Sync=$sync Health=$health"
        [[ -n "$out_of_sync_resources" ]] && msg="$msg (OutOfSync: $out_of_sync_resources)"
        if [[ "$severity" == "WARN" ]]; then
            log_warn "$msg"
        else
            log_fail "$msg"
        fi
    fi
}

# Check ExternalSecret is synced.
#
# A SecretSyncedError is NOT automatically a defect (ADR-046 §12, corrected). When a
# provider or store has just recovered, every dependent ExternalSecret carries the
# condition from its last failed attempt until the next retry. ESO retries on the
# rate limiter, not refreshInterval — baseDelay 1s, maxDelay 7m
# (external-secrets/pkg/controllers/common/common.go) — so the stale window is
# bounded at ~7 minutes and closes without intervention.
#
# Reporting that window as a critical failure is how a healthy platform reads as
# broken: on 2026-08-23 Infisical returned at 10:53, validation ran mid-backoff and
# flagged 21 ExternalSecrets, and all of them were SecretSynced by 11:04 untouched.
# So the age of the failing condition is the discriminator — a condition younger than
# the backoff cap is a wait, an older one is a real failure.
ESO_BACKOFF_CAP_SECONDS="${ESO_BACKOFF_CAP_SECONDS:-420}"   # ESO failureMaxDelay = 7m

check_external_secret() {
    local ns="$1"
    local name="$2"
    local ready
    ready=$(kc get externalsecret "$name" -n "$ns" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || echo "Unknown")
    if [[ "$ready" == "True" ]]; then
        log_pass "ExternalSecret $ns/$name: Ready"
        return
    fi

    local msg since age
    msg=$(kc get externalsecret "$name" -n "$ns" -o jsonpath='{.status.conditions[?(@.type=="Ready")].message}' 2>/dev/null || echo "unknown error")
    since=$(kc get externalsecret "$name" -n "$ns" -o jsonpath='{.status.conditions[?(@.type=="Ready")].lastTransitionTime}' 2>/dev/null || echo "")

    age=""
    if [[ -n "$since" ]]; then
        # macOS date has no -d; -j -f is the portable-here equivalent (REMEMBER.md).
        local since_epoch
        since_epoch=$(date -j -u -f '%Y-%m-%dT%H:%M:%SZ' "$since" '+%s' 2>/dev/null \
                   || date -u -d "$since" '+%s' 2>/dev/null || echo "")
        [[ -n "$since_epoch" ]] && age=$(( $(date -u '+%s') - since_epoch ))
    fi

    # NB: no apostrophes and no ${var:+...} in these strings — inside a double-quoted
    # ${age:+...} a single quote opens a quote context and breaks parsing far away
    # from the actual line (it reported a syntax error 150 lines later).
    local detail=""
    [[ -n "$age" ]] && detail=" for ${age}s"

    if [[ -n "$age" && "$age" -lt "$ESO_BACKOFF_CAP_SECONDS" ]]; then
        log_warn "ExternalSecret $ns/$name: not ready${detail} ($msg) — within the ${ESO_BACKOFF_CAP_SECONDS}s ESO retry backoff, expected to clear without intervention"
    else
        log_fail "ExternalSecret $ns/$name: Not ready${detail} ($msg) — beyond the ${ESO_BACKOFF_CAP_SECONDS}s ESO backoff, this will not self-heal"
    fi
}

# Check a Crossplane object Ready+Synced
check_crossplane_object() {
    local ns="$1"
    local name="$2"
    local ready
    ready=$(kc get object "$name" -n "$ns" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || echo "Unknown")
    local synced
    synced=$(kc get object "$name" -n "$ns" -o jsonpath='{.status.conditions[?(@.type=="Synced")].status}' 2>/dev/null || echo "Unknown")
    if [[ "$ready" == "True" && "$synced" == "True" ]]; then
        log_pass "Crossplane object $ns/$name: Ready+Synced"
    else
        log_fail "Crossplane object $ns/$name: Ready=$ready Synced=$synced"
    fi
}

# ─── Prerequisite: kubeconfig ─────────────────────────────────────────────────
preflight_check() {
    log_section "PREFLIGHT"
    if [[ ! -f "$KUBECONFIG" ]]; then
        log "ERROR: kubeconfig not found at $KUBECONFIG"
        log "       Set KUBECONFIG env var or run hub bootstrap first."
        exit 1
    fi
    if ! kc cluster-info >/dev/null 2>&1; then
        log "ERROR: Cannot connect to cluster using $KUBECONFIG"
        exit 1
    fi
    local server
    server=$(kc cluster-info 2>/dev/null | head -1 | sed 's/.*at //' || echo "unknown")
    log_pass "Hub cluster reachable: $server"

    if ! kc get namespace platform-ops >/dev/null 2>&1; then
        log "ERROR: platform-ops namespace not found. Bootstrap may not have completed."
        exit 1
    fi
    log_pass "Core namespaces present"
}

# ─── 1. PLATFORM NAMESPACES ───────────────────────────────────────────────────
check_namespaces() {
    log_section "1. PLATFORM NAMESPACES"
    # platform-messaging is NOT here, and was.
    #
    # Nothing declares it: the platform ships no messaging component, and the
    # only mention of that namespace left in the tree is a .test.bak file. So
    # this required a namespace no manifest creates, and every box failed
    # post-bootstrap validation on it -- a check asserting a component that does
    # not exist, which reports the platform broken rather than itself.
    local required_ns=(
        platform-ops
        platform-capi
        platform-data
        platform-identity
        platform-billing
        platform-credits
        platform-observability
        platform-security
        platform-edge
        platform-controlplane
        cert-manager
        cnpg-system
        kube-system
    )
    for ns in "${required_ns[@]}"; do
        if kc get namespace "$ns" >/dev/null 2>&1; then
            log_pass "Namespace $ns exists"
        else
            log_fail "Namespace $ns missing"
        fi
    done
}

# ─── 2. ARGOCD ────────────────────────────────────────────────────────────────
check_argocd() {
    log_section "2. ARGOCD"
    check_namespace_pods "platform-ops" "app.kubernetes.io/name=argocd-server"
    check_deployment "platform-ops" "argocd-server"
    check_deployment "platform-ops" "argocd-repo-server"
    check_deployment "platform-ops" "argocd-applicationset-controller"

    # Kyverno (platform-ops per ADR-015)
    check_deployment "platform-ops" "kyverno-admission-controller"
    check_deployment "platform-ops" "kyverno-background-controller"
}

# ─── 3. CROSSPLANE ────────────────────────────────────────────────────────────
check_crossplane() {
    log_section "3. CROSSPLANE"
    check_namespace_pods "platform-ops" "app=crossplane"
    check_deployment "platform-ops" "crossplane"
    check_deployment "platform-ops" "crossplane-rbac-manager"

    # Crossplane providers
    log "  Checking Crossplane providers..."
    local providers
    providers=$(kc get providers.pkg.crossplane.io -o jsonpath='{range .items[*]}{.metadata.name}: {.status.conditions[?(@.type=="Healthy")].status}{"\n"}{end}' 2>/dev/null || echo "")
    if [[ -n "$providers" ]]; then
        while IFS= read -r line; do
            [[ -z "$line" ]] && continue
            if echo "$line" | grep -q ": True"; then
                log_pass "Provider $line"
            else
                log_fail "Provider $line (not Healthy)"
            fi
        done <<< "$providers"
    else
        log_warn "No Crossplane providers found"
    fi
}

# ─── 4. EXTERNAL SECRETS OPERATOR ─────────────────────────────────────────────
check_eso() {
    log_section "4. EXTERNAL SECRETS OPERATOR"
    check_namespace_pods "platform-ops" "app.kubernetes.io/name=external-secrets"

    # ClusterSecretStore
    local css_ready
    css_ready=$(kc get clustersecretstore infisical-backend -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || echo "Unknown")
    if [[ "$css_ready" == "True" ]]; then
        log_pass "ClusterSecretStore infisical-backend: Ready"
    else
        log_fail "ClusterSecretStore infisical-backend: Not ready (status=$css_ready)"
    fi
}

# ─── 5. INFISICAL ─────────────────────────────────────────────────────────────
check_infisical() {
    log_section "5. INFISICAL"
    check_namespace_pods "platform-security"

    # The role credentials ESO delivers, which the hub-operator blocks on before it
    # provisions any role (hubenvironment_controller.go, requiredSecrets). One per
    # declared role, derived from the same names RoleSecretName builds.
    local key_secrets=(
        "platform-data:hub-grafana-db-credentials"
        "platform-data:hub-credits-db-credentials"
        # Not a declared role: billing-db's initdb owner (ADR-113), which the
        # cluster cannot initialise without.
        "platform-data:hub-billing-db-credentials"
        "platform-identity:zitadel-db-credentials"
    )
    for entry in "${key_secrets[@]}"; do
        local ns="${entry%%:*}"
        local name="${entry##*:}"
        check_external_secret "$ns" "$name"
    done
}

# ─── 6. DATABASES (CNPG) ──────────────────────────────────────────────────────
check_databases() {
    log_section "6. DATABASES (CloudNative-PG)"
    # cnpg-system = upstream operator namespace (per ADR-015 Upstream Namespaces)
    # Operator health is checked here; database workloads live in platform-data
    # Deployment name from cloudnative-pg Helm chart is cloudnative-pg.
    # Fall back to legacy name cnpg-controller-manager if the primary is not found.
    local cnpg_deploy
    cnpg_deploy=$(kc get deployment -n cnpg-system --no-headers 2>/dev/null \
        | awk '{print $1}' | grep -E '^(cloudnative-pg|cnpg-controller-manager)$' | head -1 || echo "cloudnative-pg")
    check_deployment "cnpg-system" "$cnpg_deploy"

    # CNPG clusters (platform-data per ADR-015)
    local clusters
    clusters=$(kc get clusters.postgresql.cnpg.io -n platform-data \
        -o jsonpath='{range .items[*]}{.metadata.name}: {.status.phase}{"\n"}{end}' 2>/dev/null || echo "")
    if [[ -n "$clusters" ]]; then
        while IFS= read -r line; do
            [[ -z "$line" ]] && continue
            if echo "$line" | grep -q ": Cluster in healthy state"; then
                log_pass "CNPG Cluster $line"
            else
                log_fail "CNPG Cluster $line (not Ready)"
            fi
        done <<< "$clusters"
    else
        log_fail "No CNPG clusters found in platform-data"
    fi

    # Redis
    local redis_total redis_running
    redis_total=$(kc get pods -n platform-data -l app=platform-redis --no-headers 2>/dev/null | grep -c '' || true)
    redis_running=$(kc get pods -n platform-data -l app=platform-redis --no-headers 2>/dev/null | grep -c "Running" || echo "0")
    if [[ "$redis_total" -gt 0 && "$redis_running" -eq "$redis_total" ]]; then
        log_pass "Redis (platform-data): $redis_running/$redis_total Running"
    elif [[ "$redis_running" -gt 0 ]]; then
        log_warn "Redis (platform-data): $redis_running/$redis_total Running (partial)"
    else
        log_fail "Redis (platform-data): not found or not running"
    fi

    # All data-layer pods (postgres, pooler, redis)
    check_namespace_pods "platform-data"
}

# ─── 7. ORY IDENTITY STACK ────────────────────────────────────────────────────
check_ory() {
    log_section "7. ORY IDENTITY STACK"
    check_argocd_app "zitadel" "FAIL"

    local ory_deployments=(
        "platform-identity:zitadel"
    )
    for entry in "${ory_deployments[@]}"; do
        local ns="${entry%%:*}"
        local name="${entry##*:}"
        # Use word-boundary anchored match: first column (name) starts with $name
        local found
        found=$(kc get deployment -n "$ns" --no-headers 2>/dev/null | awk -v n="$name" '$1 ~ "^" n' | awk '{print $1}' | head -1 || true)
        if [[ -n "$found" ]]; then
            check_deployment "$ns" "$found"
        else
            log_warn "Ory deployment not found: $ns/$name (may use different name)"
        fi
    done
}

# ─── 8. HUB OPERATOR ─────────────────────────────────────────────────────────
check_hub_operator() {
    log_section "8. HUB-OPERATOR"
    check_argocd_app "hub-operator" "FAIL"
    check_deployment "platform-ops" "hub-operator"

    # HubEnvironment CR status
    local hub_env_name
    hub_env_name=$(kc get hubenvironment -A \
        -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}{"\n"}{end}' 2>/dev/null | head -1 || echo "")
    if [[ -n "$hub_env_name" ]]; then
        local he_ns="${hub_env_name%%/*}"
        local he_name="${hub_env_name##*/}"
        local he_ready
        he_ready=$(kc get hubenvironment "$he_name" -n "$he_ns" \
            -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || echo "Unknown")
        if [[ "$he_ready" == "True" ]]; then
            log_pass "HubEnvironment $he_name: Ready"
        else
            log_warn "HubEnvironment $he_name: Ready=$he_ready (may still reconciling)"
        fi
    else
        log_warn "No HubEnvironment CR found"
    fi
}

# ─── 9. KUBE-SBT API ─────────────────────────────────────────────────────────
check_kube_sbt_api() {
    log_section "9. KUBE-SBT API"
    check_argocd_app "kube-sbt" "FAIL"
    check_namespace_pods "platform-ops" "app=kube-sbt"

    local svc_ip
    svc_ip=$(kc get svc kube-sbt -n platform-ops -o jsonpath='{.spec.clusterIP}' 2>/dev/null || echo "")
    if [[ -n "$svc_ip" && "$svc_ip" != "None" ]]; then
        log_pass "kube-sbt Service: ClusterIP=$svc_ip"
    else
        log_warn "kube-sbt Service: not found or no ClusterIP"
    fi
}

# ─── 10. INGRESS + CERT-MANAGER ───────────────────────────────────────────────
check_ingress() {
    log_section "10. INGRESS & CERT-MANAGER"
    # The hub serves its hostnames from the Gateway API; ingress-nginx was
    # retired, so checking for its Application fails every bootstrap.
    check_argocd_app "hub-gateway" "FAIL"
    check_namespace_pods "cert-manager"
    check_deployment "cert-manager" "cert-manager"
    check_deployment "cert-manager" "cert-manager-webhook"

    local ingress_ready
    ingress_ready=$(kc get pods -n platform-edge --no-headers 2>/dev/null \
        | grep -c "Running" || echo "0")
    if [[ "$ingress_ready" -gt 0 ]]; then
        log_pass "Ingress pods (platform-edge): $ingress_ready running"
    else
        log_warn "No ingress pods running in platform-edge"
    fi
}

# ─── 11. SPOKE POOL + CAPI ────────────────────────────────────────────────────
check_spoke() {
    log_section "11. SPOKE POOL & CAPI"

    # Extract the spoke admin kubeconfig for direct spoke-cluster validation.
    get_spoke_kubeconfig

    # CAPI operator deployment (migrated to platform-capi per ADR-015)
    check_deployment "platform-capi" "capi-operator-controller-manager"

    # CAPI Provider CRs (all in platform-capi per ADR-015)
    local providers=("CoreProvider/cluster-api" "BootstrapProvider/kubeadm" "ControlPlaneProvider/kubeadm" "InfrastructureProvider/${CAPI_INFRA_PROVIDER}")
    for p in "${providers[@]}"; do
        local kind="${p%%/*}"
        local name="${p##*/}"
        local ready
        ready=$(kc get "$kind" "$name" -n platform-capi \
            -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || echo "Unknown")
        if [[ "$ready" == "True" ]]; then
            log_pass "CAPI $kind/$name: Ready"
        else
            log_fail "CAPI $kind/$name: Ready=$ready"
        fi
    done

    local spokepool_ready
    spokepool_ready=$(kc get spokepool "$SPOKEPOOL_NAME" -n platform-ops \
        -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || echo "Unknown")
    local spokepool_synced
    spokepool_synced=$(kc get spokepool "$SPOKEPOOL_NAME" -n platform-ops \
        -o jsonpath='{.status.conditions[?(@.type=="Synced")].status}' 2>/dev/null || echo "Unknown")
    if [[ "$spokepool_ready" == "True" && "$spokepool_synced" == "True" ]]; then
        log_pass "SpokePool $SPOKEPOOL_NAME: Ready+Synced"
    else
        log_fail "SpokePool $SPOKEPOOL_NAME: Ready=$spokepool_ready Synced=$spokepool_synced"
    fi

    local capi_cluster_phase
    capi_cluster_phase=$(kc get clusters.cluster.x-k8s.io "$SPOKEPOOL_NAME" -n platform-capi \
        -o jsonpath='{.status.phase}' 2>/dev/null || echo "Unknown")
    if [[ "$capi_cluster_phase" == "Provisioned" ]]; then
        log_pass "CAPI Cluster $SPOKEPOOL_NAME: Provisioned"
    else
        log_fail "CAPI Cluster $SPOKEPOOL_NAME: phase=$capi_cluster_phase"
    fi

    # Certificate Ready condition (cert-manager issued, hub-operator cert wrapper)
    if kc wait --for=condition=Ready certificate "argocd-agent-${SPOKEPOOL_NAME}" \
        -n platform-capi --timeout=5s >/dev/null 2>&1; then
        log_pass "Bootstrap certificate: Ready"
    else
        log_fail "Bootstrap certificate: Not Ready — check cert-manager and infisical-issuer"
    fi

    # SpokeMachineIdentity Ready condition (provisioned by spoke-identity-operator)
    if kc wait --for=condition=Ready spokemachineidentity "${SPOKEPOOL_NAME}" \
        -n platform-capi --timeout=5s >/dev/null 2>&1; then
        log_pass "SpokeMachineIdentity: Ready"
    else
        log_fail "SpokeMachineIdentity: Not Ready — check spoke-identity-operator logs"
    fi

    check_spoke_platform
}

# ─── 14c. SPOKE PLATFORM (direct cluster validation) ─────────────────────────
# Validates the spoke cluster directly (not hub-side signals). Catches:
#   - spoke ArgoCD pods stuck Pending (control-plane taint / bootstrap gaps)
#   - missing spoke platform operators (crossplane/ESO/kyverno/CNPG/cert-manager/
#     agent-sandbox) and their CRDs
#   - storage driver down (blocks CNPG PVs → tenant databases)
#   - shared-cnpg not healthy (tenant DB prerequisite)

check_spoke_namespace_pods() {
    local ns="$1"
    local label_selector="${2:-}"
    local label_arg=""
    [[ -n "$label_selector" ]] && label_arg="-l $label_selector"

    local not_ready
    # shellcheck disable=SC2086
    not_ready=$(kc_spoke get pods -n "$ns" $label_arg --no-headers 2>/dev/null \
        | grep -v -E '([0-9]+/[0-9]+\s+(Running|Completed))' \
        | grep -v "^$" || true)

    if [[ -z "$not_ready" ]]; then
        local count
        # shellcheck disable=SC2086
        count=$(kc_spoke get pods -n "$ns" $label_arg --no-headers 2>/dev/null | grep -c '' || echo 0)
        if [[ "$count" -eq 0 ]]; then
            log_warn "Spoke ns $ns: no pods found (nothing deployed yet?)"
        else
            log_pass "Spoke ns $ns: all $count pod(s) healthy"
        fi
    else
        local crashloop_count pending_count error_count
        crashloop_count=$(echo "$not_ready" | grep -c "CrashLoopBackOff" || true)
        pending_count=$(echo "$not_ready" | grep -c "Pending" || true)
        error_count=$(echo "$not_ready" | grep -c "Error" || true)
        log_fail "Spoke ns $ns: unhealthy pods (CrashLoop=$crashloop_count, Pending=$pending_count, Error=$error_count)"
        echo "$not_ready" | while IFS= read -r line; do
            [[ -n "$line" ]] && log "     → $line"
        done
    fi
}

check_spoke_deployment() {
    local ns="$1"
    local name="$2"
    local desired available
    desired=$(kc_spoke get deployment "$name" -n "$ns" -o jsonpath='{.spec.replicas}' 2>/dev/null || echo "0")
    available=$(kc_spoke get deployment "$name" -n "$ns" -o jsonpath='{.status.availableReplicas}' 2>/dev/null || echo "0")
    desired="${desired:-0}"; available="${available:-0}"

    if [[ "$available" -ge "$desired" && "$desired" -gt 0 ]]; then
        log_pass "Spoke Deployment $ns/$name: $available/$desired available"
    else
        log_fail "Spoke Deployment $ns/$name: $available/$desired available"
    fi
}

check_spoke_crd() {
    local name="$1"
    if kc_spoke get crd "$name" >/dev/null 2>&1; then
        log_pass "Spoke CRD $name: present"
    else
        log_fail "Spoke CRD $name: MISSING (operator not installed?)"
    fi
}

# Verify that Kyverno CRDs have the Established=True condition.
# A CRD can be present (APIServed) but not yet Established — the API server
# has not yet accepted the schema. Controllers that depend on the CRD will
# fail with "no matches for kind" if they start before Established=True.
# See: kyverno-tenant-abi.yaml ("applying before the CRD is Established
# causes an irrecoverable rejection") and cilium-addon-hybrid.yaml
# (--for=condition=Established).
check_kyverno_crd_established() {
    log "  Checking Kyverno CRD Established conditions..."
    local kyverno_crds=(
        "clusterpolicies.kyverno.io"
        # Policy reports live in the Kubernetes Policy WG group, NOT kyverno.io.
        # Asserted as policyreports.kyverno.io until 2026-09-02 and reported
        # "not found" on every run against a spoke where Kyverno was healthy and
        # the reports existed the whole time.
        "policyreports.wgpolicyk8s.io"
        "clusterpolicyreports.wgpolicyk8s.io"
        "admissionreports.kyverno.io"
        "backgroundscanreports.kyverno.io"
        "clusterbackgroundscanreports.kyverno.io"
    )
    for crd in "${kyverno_crds[@]}"; do
        local established
        established=$(kc_spoke get crd "$crd" \
            -o jsonpath='{.status.conditions[?(@.type=="Established")].status}' 2>/dev/null || echo "NotFound")
        if [[ "$established" == "True" ]]; then
            log_pass "Spoke CRD $crd: Established=True"
        elif [[ "$established" == "NotFound" ]]; then
            log_fail "Spoke CRD $crd: not found"
        else
            log_fail "Spoke CRD $crd: Established=$established"
        fi
    done
}

check_spoke_cnpg() {
    # readyInstances, not the phase string. The phase is human-facing text that
    # changes between CNPG releases; a ready instance count is the fact, and it is
    # what "the database works" actually means.
    _spoke_cnpg_ready() {
        local ready
        ready=$(kc_spoke get clusters.postgresql.cnpg.io shared-cnpg -n platform-data \
            -o jsonpath='{.status.readyInstances}' 2>/dev/null || echo "")
        [[ -n "$ready" && "$ready" -ge 1 ]]
    }

    local phase
    if settle "the spoke's database to report a ready instance" _spoke_cnpg_ready; then
        log_pass "Spoke CNPG shared-cnpg: healthy"
    else
        phase=$(kc_spoke get clusters.postgresql.cnpg.io shared-cnpg -n platform-data \
            -o jsonpath='{.status.phase}' 2>/dev/null || echo "Unknown")
        log_fail "Spoke CNPG shared-cnpg: no ready instance after ${SETTLE_DEADLINE}s (phase='$phase'; needs healthy storage driver + operator)"
    fi
}

check_spoke_platform() {
    log_section "14c. SPOKE PLATFORM (direct cluster validation)"

    [[ -z "$SPOKE_KUBECONFIG" ]] && { log_warn "Skipping spoke platform checks"; return 0; }

    # Spoke ArgoCD — catches the control-plane taint / bootstrap gaps that keep
    # every spoke-targeted app from syncing.
    check_spoke_namespace_pods "argocd"
    check_spoke_deployment "argocd" "argocd-agent"
    check_spoke_deployment "argocd" "argocd-repo-server"

    # Required CRDs (operators must be deployed to the spoke)
    local crds=(
        "rollouts.argoproj.io"
        "certificates.cert-manager.io"
        "externalsecrets.external-secrets.io"
        "providers.pkg.crossplane.io"
        "clusters.postgresql.cnpg.io"
        "sandboxes.agents.x-k8s.io"
        "clusterpolicies.kyverno.io"
    )
    for c in "${crds[@]}"; do check_spoke_crd "$c"; done

    # Verify Kyverno CRDs are Established (not just present).
    # A CRD that is present but not Established means the API server has
    # not yet accepted the schema — controllers depending on it will fail.
    check_kyverno_crd_established

    # Platform operators
    check_spoke_namespace_pods "crossplane-system"
    check_spoke_deployment "crossplane-system" "crossplane"
    check_spoke_deployment "crossplane-system" "crossplane-rbac-manager"
    check_spoke_namespace_pods "external-secrets"
    check_spoke_deployment "external-secrets" "eso-external-secrets"
    check_spoke_namespace_pods "kyverno"
    check_spoke_deployment "kyverno" "kyverno-admission-controller"
    check_spoke_deployment "kyverno" "kyverno-background-controller"
    check_spoke_deployment "cnpg-system" "cnpg-cloudnative-pg"
    check_spoke_namespace_pods "cert-manager"
    # cm- prefix: the spoke installs cert-manager as a Helm release named "cm",
    # so its deployments are cm-cert-manager{,-cainjector,-webhook}. Asserting
    # the unprefixed name reported "0/0 available" against a spoke whose
    # cert-manager had been 1/1 for hours.
    check_spoke_deployment "cert-manager" "cm-cert-manager"
    check_spoke_deployment "cert-manager" "cm-cert-manager-webhook"
    check_spoke_deployment "cert-manager" "infisical-issuer"
    check_spoke_deployment "agent-sandbox-system" "agent-sandbox-controller"

    # Storage driver — required for CNPG PVs (tenant databases)
    check_spoke_deployment "kube-system" "hcloud-csi-controller"

    # Tenant database prerequisite
    check_spoke_cnpg

    # Cross-node CNI datapath & Cilium health gate (ADR-046 Addendum 14)
    check_spoke_datapath
}

check_spoke_datapath() {
    log "  Checking Spoke cross-node CNI datapath & Cilium health..."
    local cilium_pod
    cilium_pod=$(kc_spoke get pods -n kube-system -l k8s-app=cilium --no-headers 2>/dev/null | grep "Running" | head -1 | awk '{print $1}' || echo "")
    if [[ -z "$cilium_pod" ]]; then
        log_fail "Spoke Cilium: no running agent pods in kube-system"
        return 0
    fi

    local c_status
    c_status=$(kc_spoke exec -n kube-system "$cilium_pod" -c cilium-agent -- cilium-dbg status 2>/dev/null || echo "")
    if echo "$c_status" | grep -q "Cilium:                  Ok"; then
        log_pass "Spoke Cilium Agent: Ok (healthy)"
    else
        log_fail "Spoke Cilium Agent: Degraded or not Ok"
    fi

    # ── WS1 / ADR-046 addendum 17: datapath-ownership invariants ────────────
    # Tailscale is a NODE-LEVEL UNDERLAY ONLY. Cilium must own pod routing via
    # VXLAN. These two checks catch the exact regressions behind the 2026-08-20
    # outage: native routing re-enabled, and podCIDR subnet routes reappearing
    # in table 52 where they shadow the tunnel route (ip rule 5270 < 32766).
    local routing_mode
    routing_mode=$(kc_spoke get cm -n kube-system cilium-config -o jsonpath='{.data.routing-mode}' 2>/dev/null || echo "")
    if [[ "$routing_mode" == "tunnel" ]]; then
        log_pass "Spoke Cilium routing-mode: tunnel (VXLAN)"
    else
        log_fail "Spoke Cilium routing-mode: '$routing_mode' (expected 'tunnel' — ADR-046 addendum 14)"
    fi

    local adnr
    adnr=$(kc_spoke get cm -n kube-system cilium-config -o jsonpath='{.data.auto-direct-node-routes}' 2>/dev/null || echo "")
    if [[ "$adnr" == "true" ]]; then
        log_fail "Spoke Cilium auto-direct-node-routes: true (native-routing setting; must be false under tunnel mode)"
    else
        log_pass "Spoke Cilium auto-direct-node-routes: disabled"
    fi

    # No node may advertise a podCIDR over the tailnet. Checked from inside each
    # Cilium agent's host netns view of table 52 (tailscaled's routing table).
    local ts_shadow=0 n
    for n in $(kc_spoke get pods -n kube-system -l k8s-app=cilium --no-headers 2>/dev/null | grep Running | awk '{print $1}'); do
        if kc_spoke exec -n kube-system "$n" -c cilium-agent -- ip route show table 52 2>/dev/null | grep -qE '10\.244\.'; then
            log_fail "Tailscale table 52 on $n carries a podCIDR route — shadows the VXLAN path (ADR-046 addendum 17)"
            ts_shadow=1
        fi
    done
    [[ "$ts_shadow" -eq 0 ]] && log_pass "Tailscale table 52: no podCIDR shadow routes on any node"

    # Check for multi-node / home-worker presence
    local nodes_count
    nodes_count=$(kc_spoke get nodes --no-headers 2>/dev/null | grep -c '' || true)
    if [[ "$nodes_count" -ge 2 ]]; then
        local worker_node
        worker_node=$(kc_spoke get nodes -l '!node-role.kubernetes.io/control-plane' -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || echo "")
        if [[ -n "$worker_node" ]]; then
            log "  Validating worker $worker_node cross-node control-plane & ClusterIP connectivity..."
            local worker_cilium
            worker_cilium=$(kc_spoke get pods -n kube-system -l k8s-app=cilium --field-selector spec.nodeName="$worker_node" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || echo "")
            if [[ -n "$worker_cilium" ]]; then
                local bpf_lb
                bpf_lb=$(kc_spoke exec -n kube-system "$worker_cilium" -c cilium-agent -- cilium-dbg bpf lb list 2>/dev/null || echo "")
                if echo "$bpf_lb" | grep -q "10.96.0.1:443"; then
                    log_pass "Worker $worker_node Cilium eBPF Service Map: 10.96.0.1:443 active"
                else
                    log_fail "Worker $worker_node Cilium eBPF Service Map: 10.96.0.1:443 missing"
                fi
            fi

            local worker_crashloops
            worker_crashloops=$(kc_spoke get pods -A --field-selector spec.nodeName="$worker_node" --no-headers 2>/dev/null | grep "CrashLoopBackOff" | grep -c '' || true)
            if [[ "$worker_crashloops" -eq 0 ]]; then
                log_pass "Worker $worker_node Workloads: 0 CrashLoopBackOff pods"
            else
                log_fail "Worker $worker_node Workloads: $worker_crashloops CrashLoopBackOff pod(s)"
            fi
        fi
    fi
}

# ─── 12. CRITICAL ARGOCD APPS (PLATFORM INFRA) ───────────────────────────────
check_platform_argocd_apps() {
    log_section "12. CRITICAL ARGOCD APPS"

    # Critical — failure blocks operations
    local critical_apps=(
        "platform-namespaces"
        "platform-external-secrets"
        "platform-crossplane"
        "platform-crossplane-providers"
        "platform-cluster-secret-store"
        "platform-cloudnative-pg"
        "platform-database"
        "hub-environment"
        "hub-operator"
        "platform-redis"
    )
    for app in "${critical_apps[@]}"; do
        check_argocd_app "$app" "FAIL"
    done

    # Warning only — degraded but not blocking
    local warn_apps=(
        "platform-kyverno"
        "zitadel"
        "platform-infisical"
    )
    for app in "${warn_apps[@]}"; do
        check_argocd_app "$app" "WARN"
    done
}

# ─── 13. OBSERVABILITY ────────────────────────────────────────────────────────
check_observability() {
    log_section "13. OBSERVABILITY"
    check_argocd_app "victoria-metrics-cluster" "WARN"
    check_argocd_app "victoria-metrics-alerts" "WARN"
    check_argocd_app "grafana-alloy" "WARN"

    local obs_ready
    obs_ready=$(kc get pods -n platform-observability --no-headers 2>/dev/null \
        | grep -c "Running" || echo "0")
    if [[ "$obs_ready" -gt 0 ]]; then
        log_pass "Observability pods (platform-observability): $obs_ready running"
    else
        log_warn "No observability pods running in platform-observability"
    fi
}

# ─── 15b. FLEET PROVISIONING APPS (ADR-047 boundary05) ─────────────────────
# The tenant-app ApplicationSets (xr/spoke/workloads) + platform-spoke-catalog.
check_fleet_argocd_apps() {
    log_section "15b. FLEET PROVISIONING APPS (ADR-047)"

    # platform-spoke-catalog is WARN — it deploys the spoke platform and converges
    # over time (operator installs + CRD ordering).
    check_argocd_app "platform-spoke-catalog-${SPOKEPOOL_NAME}" "WARN"

    # Fleet tenant apps are discovered from the appset (name = <tenant>-<env>-xr);
    # validate the ones that exist rather than failing on envs not yet deployed.
    local fleet_apps
    fleet_apps=$(kc get applications -n platform-ops --no-headers 2>/dev/null \
        | awk '{print $1}' | grep -E '^tenant-.*-workloads$|-xr$|-spoke$' | grep -v '^tenant-app-' || true)
    if [[ -z "$fleet_apps" ]]; then
        log_warn "No fleet tenant apps found yet (boundary05 appsets may not have generated any)"
        return 0
    fi
    for app in $fleet_apps; do
        check_argocd_app "$app" "FAIL"
    done
}

# ─── SUMMARY ──────────────────────────────────────────────────────────────────
print_summary() {
    log ""
    log "╔══════════════════════════════════════════╗"
    log "║    POST-BOOTSTRAP VALIDATION SUMMARY     ║"
    log "╠══════════════════════════════════════════╣"
    log "║  ✅ PASSED : $PASS"
    log "║  ❌ FAILED : $FAIL"
    log "║  ⚠️  WARNED : $WARN"
    log "╠══════════════════════════════════════════╣"

    if [[ ${#FAILURES[@]} -gt 0 ]]; then
        log "║  CRITICAL FAILURES:"
        for f in "${FAILURES[@]}"; do
            log "║    ✗ $f"
        done
        log "╠══════════════════════════════════════════╣"
    fi

    if [[ ${#WARNINGS[@]} -gt 0 ]]; then
        log "║  WARNINGS:"
        for w in "${WARNINGS[@]}"; do
            log "║    ! $w"
        done
        log "╠══════════════════════════════════════════╣"
    fi

    if [[ "$FAIL" -eq 0 ]]; then
        log "║  🎉 ALL CRITICAL CHECKS PASSED           ║"
    else
        log "║  💥 $FAIL CRITICAL CHECK(S) FAILED        ║"
        log "║  Review failures above before proceeding  ║"
    fi
    log "╚══════════════════════════════════════════╝"
    log "  Full log: $LOG_FILE"
}

# ─── MAIN ─────────────────────────────────────────────────────────────────────
main() {
    log "═══════════════════════════════════════════════════════"
    log " Zero-Ops Post-Bootstrap Core Services Validation"
    log " Kubeconfig: $KUBECONFIG"
    log " SpokePool:  $SPOKEPOOL_NAME"
    log "═══════════════════════════════════════════════════════"

    preflight_check
    check_namespaces
    check_argocd
    check_crossplane
    check_eso
    check_infisical
    check_databases
    check_ory
    check_hub_operator
    check_kube_sbt_api
    check_ingress
    check_spoke
    check_platform_argocd_apps
    check_fleet_argocd_apps
    check_observability

    print_summary

    # ADR-050/051 checks live as modules under scripts/validate/cluster/ so the
    # same implementations run as in-flight bootstrap gates and here, in strict
    # mode, instead of two copies drifting apart. See scripts/validate/run.sh.
    log ""
    log "Running ADR-050/051 validation modules (strict)..."
    local adr_rc=0
    HUB_KUBECONFIG="$KUBECONFIG" ENVIRONMENT="$ENVIRONMENT" SPOKEPOOL_NAME="$SPOKEPOOL_NAME" \
        HUB_DOMAIN="${HUB_DOMAIN:-}" \
        bash "$(dirname "${BASH_SOURCE[0]}")/validate/run.sh" cluster --mode=final \
        2>&1 | tee -a "$LOG_FILE" || adr_rc=1

    if [[ "$FAIL" -gt 0 || "$adr_rc" -ne 0 ]]; then
        exit 1
    fi
    exit 0
}

trap 'log "Script interrupted"; print_summary; exit 1' INT TERM
trap '[[ -n "$SPOKE_KC_TMP" ]] && rm -f "$SPOKE_KC_TMP"' EXIT

main "$@"
