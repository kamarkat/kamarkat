#!/usr/bin/env bash
# Shared harness for the validation modules in preflight/ and cluster/.
#
# Sourced by every module. Modules only call the reporting helpers below and
# never manage counters or exit codes themselves — run.sh owns the aggregate
# result so that a module can be executed standalone during debugging without
# behaving differently from how it behaves inside a bootstrap.

# ─── Severity model ──────────────────────────────────────────────────────────
# Validation runs at two moments with different tolerances:
#
#   VALIDATE_MODE=gate   mid-bootstrap. The platform is still converging, so a
#                        resource that has not appeared yet is not a defect.
#   VALIDATE_MODE=final  after bootstrap. Everything must have converged; the
#                        same condition is now a defect.
#
# `soft_fail` encodes exactly that difference, so one module implementation
# serves both moments instead of two copies drifting apart. `hard_fail` is for
# conditions that are wrong regardless of timing (a wrong environment slug is
# not going to converge into the right one).
VALIDATE_MODE="${VALIDATE_MODE:-final}"

PASS_COUNT=0
FAIL_COUNT=0
WARN_COUNT=0
declare -a FAIL_MESSAGES=()
declare -a WARN_MESSAGES=()

pass() { echo "  ✅ $1"; ((PASS_COUNT++)); return 0; }

hard_fail() {
    echo "  ❌ $1"
    FAIL_MESSAGES+=("$1")
    ((FAIL_COUNT++))
    return 0
}

warn() {
    echo "  ⚠️  $1"
    WARN_MESSAGES+=("$1")
    ((WARN_COUNT++))
    return 0
}

soft_fail() {
    if [[ "$VALIDATE_MODE" == "gate" ]]; then
        warn "$1 (not yet converged — expected to self-heal; enforced after bootstrap)"
    else
        hard_fail "$1"
    fi
}

note() { echo "     $1"; }

section() {
    echo ""
    echo "── $1 $(printf '─%.0s' $(seq 1 $((60 - ${#1} > 0 ? 60 - ${#1} : 0))))"
}

# ─── Context ─────────────────────────────────────────────────────────────────
VALIDATE_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# No ENVIRONMENT. The hub has none (ADR-111 Revision 7), and this defaulted to
# "dev" -- so a check that needed it silently judged every box as a dev one. What
# the checks actually need is the box's own declaration: HUB_DOMAIN for hostnames,
# and hub-bootstrap-config's INFISICAL_ENVIRONMENT_SLUG for the management scope.

# The public DNS zone for this environment. prod is un-prefixed (ADR-051); every
# other environment is prefixed. Must stay in lockstep with the --domain-filter
# expression in 03-platform-services-appset.yaml, which is what actually scopes
# what a spoke may write.
# HUB_DOMAIN first: a tenant's box publishes on the tenant's domain, and probing
# the platform's hostnames from it proves nothing about either. The literals
# below are the platform's OWN box, kept as the fallback so running this in this
# checkout needs no arguments -- the same reason the manifests carried them, and
# they were wrong there for the same reason.
#
# ADR-051 makes the base domain the authority every public host derives from, and
# hubDomain is that value: environment-prefixed except in prod, which uses the
# apex. So it is used as-is rather than re-prefixed here.
# ENV_ZONE_SOURCE is carried so the banner can say WHERE the zone came from.
# The literal below is the platform's own, and a check that silently falls back
# to it judges a tenant's box by hostnames it does not own -- which failed a
# correct box with "external-dns --domain-filter is not dev.nutgraf.in" while the
# line above it confirmed the filter was correctly dev.acme.example. The value
# was wrong and nothing said so, because the banner printed a zone either way.
# HUB_DOMAIN, or nothing. There is no default.
#
# It used to fall back to the PLATFORM's own zone, so every check on a tenant box
# was made against a domain that box does not own. That failed a correct box --
# "external-dns --domain-filter is not dev.nutgraf.in" on a box whose filter was
# correctly dev.acme.example -- and it would equally have PASSED a box misconfigured
# to publish on the platform's zone, which is the worse direction.
#
# Only the cluster modules read ENV_ZONE, and every caller with a cluster can read
# HubEnvironment.spec.domain, which ADR-051 makes the sole authority. A caller that
# cannot is a caller with nothing to check hostnames against, and it is told so
# rather than handed the platform's.
if [[ -z "${HUB_DOMAIN:-}" ]]; then
    ENV_ZONE=""
    ENV_ZONE_SOURCE="UNSET"
else
    ENV_ZONE="$HUB_DOMAIN"
    ENV_ZONE_SOURCE="the box's HubEnvironment"
fi

# require_zone is called by any check that derives a hostname. Declared here so a
# module cannot quietly proceed with an empty zone and probe "https://api." --
# which resolves to nothing and reads as a platform failure.
require_zone() {
    if [[ -z "$ENV_ZONE" ]]; then
        hard_fail "HUB_DOMAIN is not set, so there is no zone to check hostnames against. \
The box declares it as HubEnvironment.spec.domain (ADR-051); the caller must pass it."
        return 1
    fi
    return 0
}

# Hetzner exposes no quota endpoint, so the project's load balancer ceiling is a
# declared constant rather than something that can be read. Raise it here if the
# account limit is raised — the check that consumes it (preflight/15) can only be
# as right as this number.
HETZNER_LB_QUOTA="${HETZNER_LB_QUOTA:-5}"

# ─── Waiting for converging state ────────────────────────────────────────────
#
# VALIDATE_SETTLE bounds how long a check will wait for a condition that is
# expected to become true. 0 disables waiting entirely, which is what a run
# wants when it is asking "is this box ready RIGHT NOW" rather than "will it
# become ready".
VALIDATE_SETTLE="${VALIDATE_SETTLE:-180}"
VALIDATE_SETTLE_INTERVAL="${VALIDATE_SETTLE_INTERVAL:-5}"

# wait_until <seconds> <what> <command...>
#
# Polls until the command succeeds or the deadline passes. Returns 0 if it
# became true, 1 if it never did.
#
# This exists instead of a sleep between bootstrap and validation, which is the
# obvious thing to reach for and the wrong one. Every check here sampled the
# cluster once, so a component still converging -- a CNPG cluster electing its
# primary, an Application mid-sync -- was reported as a failure, and the run
# died on a box that was seconds from correct.
#
# A blanket sleep would have hidden that, and hidden more besides. It is a guess
# that does not generalise across a cold image cache or a slower node; it adds
# its full cost to runs that were going to fail anyway; and after it, a pass
# means "ready, or we waited long enough" with no way to tell which. This waits
# only as long as it must, fails with the condition named rather than a
# timestamp, and leaves "converging" distinguishable from "broken" -- which is
# the distinction the whole validation surface exists to make.
#
# Use it ONLY for state that converges. A namespace no manifest declares, an
# image reference that 404s, an allowlist that does not match: waiting cannot
# change those answers, and a check that waits on one turns an instant, correct
# failure into a slow one.
wait_until() {
    local deadline="$1" what="$2"; shift 2
    local waited=0

    if "$@"; then
        return 0
    fi
    if (( deadline <= 0 )); then
        return 1
    fi

    note "waiting up to ${deadline}s for $what"
    while (( waited < deadline )); do
        sleep "$VALIDATE_SETTLE_INTERVAL"
        waited=$(( waited + VALIDATE_SETTLE_INTERVAL ))
        if "$@"; then
            note "$what after ${waited}s"
            return 0
        fi
    done
    return 1
}

# ─── Cluster access (cluster/ modules only) ──────────────────────────────────
HUB_KUBECONFIG="${HUB_KUBECONFIG:-${KUBECONFIG_PATH:-${KUBECONFIG:-}}}"
SPOKE_KUBECONFIG="${SPOKE_KUBECONFIG:-}"
SPOKEPOOL_NAME="${SPOKEPOOL_NAME:-}"

kc() { kubectl --kubeconfig="$HUB_KUBECONFIG" "$@" 2>/dev/null; }
kc_spoke() { kubectl --kubeconfig="$SPOKE_KUBECONFIG" "$@" 2>/dev/null; }

# Hub-side readiness (SpokePool Ready, CAPI Provisioned) does not prove the spoke
# is functional, so spoke checks talk to the spoke directly. Returns non-zero
# when the spoke is unreachable; callers skip rather than fail, because an
# unreachable spoke is already reported by the spoke readiness checks.
_SPOKE_KC_TMP=""
ensure_spoke_kubeconfig() {
    [[ -n "$SPOKE_KUBECONFIG" && -f "$SPOKE_KUBECONFIG" ]] && return 0
    [[ -z "$SPOKEPOOL_NAME" ]] && return 1
    _SPOKE_KC_TMP="$(mktemp)"
    if ! kc get secret "${SPOKEPOOL_NAME}-kubeconfig" -n platform-capi \
        -o jsonpath='{.data.value}' | base64 -d > "$_SPOKE_KC_TMP" 2>/dev/null; then
        rm -f "$_SPOKE_KC_TMP"; _SPOKE_KC_TMP=""
        return 1
    fi
    SPOKE_KUBECONFIG="$_SPOKE_KC_TMP"
    kubectl --kubeconfig="$SPOKE_KUBECONFIG" cluster-info >/dev/null 2>&1 || return 1
    return 0
}

cleanup_spoke_kubeconfig() { [[ -n "$_SPOKE_KC_TMP" ]] && rm -f "$_SPOKE_KC_TMP"; }
