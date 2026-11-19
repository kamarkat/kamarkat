#!/usr/bin/env bash
# Validation runner (ADR-046 / ADR-050 / ADR-051).
#
#   run.sh preflight                     every static check; needs no cluster
#   run.sh cluster                       every live-cluster check
#   run.sh cluster --only=NAME[,NAME]    a subset, for in-flight bootstrap gates
#
# Modules live in preflight/ and cluster/, one concern per file, numbered for
# deterministic order. Each defines validate_<name>() and documents WHY the check
# exists — every one of them encodes a failure that previously reported Healthy
# while being broken, which is the only reason it is worth a check at all.
#
# Adding a check means adding a file. Nothing here needs editing.
#
# Exit status: non-zero if any check failed. Warnings never fail the run.
set -uo pipefail

VALIDATE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=common.sh
source "$VALIDATE_DIR/common.sh"

PHASE="${1:-}"
shift || true

ONLY=""
for arg in "$@"; do
    case "$arg" in
        --only=*) ONLY="${arg#*=}" ;;
        --mode=*) VALIDATE_MODE="${arg#*=}" ;;
        *) echo "unknown argument: $arg" >&2; exit 2 ;;
    esac
done

case "$PHASE" in
    preflight) MODULE_DIR="$VALIDATE_DIR/preflight"; VALIDATE_MODE="final" ;;
    cluster)   MODULE_DIR="$VALIDATE_DIR/cluster" ;;
    *) echo "usage: run.sh {preflight|cluster} [--only=name,...] [--mode=gate|final]" >&2; exit 2 ;;
esac

# Selecting a subset by module name (the filename minus its numeric prefix and
# .sh) is what lets hub-bootstrap.sh run one check at the step that introduces
# the condition, rather than all of them at the end.
selected_modules() {
    local f base
    for f in "$MODULE_DIR"/*.sh; do
        base="$(basename "$f" .sh)"; base="${base#*-}"
        if [[ -n "$ONLY" && ",$ONLY," != *",$base,"* ]]; then
            continue
        fi
        echo "$f"
    done
}

MODULES=()
while IFS= read -r m; do [[ -n "$m" ]] && MODULES+=("$m"); done < <(selected_modules)

if (( ${#MODULES[@]} == 0 )); then
    echo "no modules matched${ONLY:+ --only=$ONLY}" >&2
    exit 2
fi

echo "═══════════════════════════════════════════════════════════"
echo "  Zero-Ops validation — phase=$PHASE mode=$VALIDATE_MODE"
echo "  zone=$ENV_ZONE (${ENV_ZONE_SOURCE:-unknown})"
echo "═══════════════════════════════════════════════════════════"

for m in "${MODULES[@]}"; do
    # shellcheck disable=SC1090
    source "$m"
done

# What a module is ABOUT, declared in the module and read from here (ADR-115).
#
#   # scope: hub            the management cluster alone
#   # scope: spoke          one workload cluster
#   # scope: every-cluster  the same assertion, per cluster the box has
#   # scope: path           one assertion spanning two clusters
#
# A module that declares nothing is hub-scoped. That default is deliberate: a
# module about the management cluster must run on every box, and the failure this
# replaces was 35 modules skipped together because ONE of their subjects, a
# workload cluster, was not declared.
module_scope() {
    local s
    s=$(grep -m1 -oE '^# scope: *[a-z-]+' "$1" | awk '{print $3}')
    echo "${s:-hub}"
}

# Whether a workload cluster can be asserted against, and why not when it cannot.
#
# Read from the DECLARATION first and the cluster second: a box that declares no
# workload cluster makes those modules inapplicable, while a box that declares
# one and cannot reach it is PENDING, which is neither a pass nor an omission to
# be forgotten. An unreachable spoke never silently becomes "not applicable".
spoke_state() {
    if [[ -z "${SPOKEPOOL_NAME:-}" ]] &&
       ! compgen -G "$VALIDATE_ROOT/registry/clusters/*/infrastructure/spokepool.yaml" >/dev/null 2>&1
    then
        echo "undeclared"; return
    fi
    if [[ -z "${SPOKEPOOL_NAME:-}" ]]; then echo "unnamed"; return; fi
    if ensure_spoke_kubeconfig; then echo "reachable"; else echo "pending"; fi
}

# Resolved ONCE, in this shell. It was resolved inside spoke_reason, which the
# loop below calls in a command substitution -- so the assignment happened in a
# subshell, the parent's copy stayed empty, and an undeclared workload cluster
# was reported `unverified` instead of `inapplicable`. The two mean opposite
# things to a verdict: one is an omission with a reason, the other blocks
# acceptance.
SPOKE_STATE=""
resolve_spoke_state() { [[ -z "$SPOKE_STATE" ]] && SPOKE_STATE="$(spoke_state)"; return 0; }

spoke_reason() {
    case "$SPOKE_STATE" in
        undeclared) echo "this box declares no workload cluster" ;;
        unnamed)    echo "this box declares a workload cluster and none was named for this run" ;;
        pending)    echo "the declared workload cluster is not yet reachable" ;;
        *)          echo "" ;;
    esac
}

# Run every validate_* function each applicable module defined, in module order.
for m in "${MODULES[@]}"; do
    MODULE_NAME="$(basename "$m" .sh)"; MODULE_NAME="${MODULE_NAME#*-}"
    scope="$(module_scope "$m")"

    if [[ "$scope" == "spoke" || "$scope" == "path" ]]; then
        resolve_spoke_state
        reason="$(spoke_reason)"
        if [[ -n "$reason" ]]; then
            section "$MODULE_NAME"
            if [[ "$SPOKE_STATE" == "undeclared" ]]; then
                inapplicable "$reason" || true
            else
                # Declared and not assessable is PENDING, and pending does not
                # pass. The box asked for the cluster.
                unverified "$reason" || true
            fi
            continue
        fi
    fi

    before_pass=$PASS_COUNT; before_fail=$FAIL_COUNT; before_warn=$WARN_COUNT
    before_skip=$((MODULE_INAPPLICABLE + MODULE_UNVERIFIED))

    while IFS= read -r fn; do
        [[ -n "$fn" ]] && "$fn"
    done < <(grep -oE '^validate_[a-z0-9_]+\(\)' "$m" | tr -d '()')

    # The module's own outcome, derived from what it did rather than declared.
    if (( MODULE_INAPPLICABLE + MODULE_UNVERIFIED > before_skip )); then
        :  # it reported its own outcome
    elif (( FAIL_COUNT > before_fail )); then
        MODULE_RESULTS+=("failed|$MODULE_NAME|$((FAIL_COUNT - before_fail)) assertion(s) failed")
        ((MODULE_FAILED++))
    elif (( PASS_COUNT > before_pass || WARN_COUNT > before_warn )); then
        MODULE_RESULTS+=("passed|$MODULE_NAME|$((PASS_COUNT - before_pass)) assertion(s)")
        ((MODULE_PASSED++))
    else
        # ZERO ASSERTIONS IS NOT A PASS. An empty module reads exactly like a
        # working one, which is the whole failure mode this file now refuses.
        section "$MODULE_NAME"
        unverified "the module applied and asserted nothing" || true
    fi
done

[[ "$PHASE" == "cluster" ]] && cleanup_spoke_kubeconfig

echo ""
echo "═══════════════════════════════════════════════════════════"
echo "  $PHASE: $PASS_COUNT passed, $FAIL_COUNT failed, $WARN_COUNT warned"
echo "  modules: $MODULE_PASSED passed, $MODULE_FAILED failed, \
$MODULE_UNVERIFIED unverified, $MODULE_INAPPLICABLE not applicable"
echo "═══════════════════════════════════════════════════════════"

# Every omission, with its reason. An omission nobody can read is the shape this
# whole file now exists to refuse.
if (( MODULE_UNVERIFIED + MODULE_INAPPLICABLE > 0 )); then
    echo ""
    echo "Not established:"
    for r in "${MODULE_RESULTS[@]}"; do
        case "$r" in
            unverified\|*|inapplicable\|*)
                echo "  • ${r#*|}" | sed 's/|/ — /' ;;
        esac
    done
fi

if (( FAIL_COUNT > 0 )); then
    echo ""
    if [[ "$PHASE" == "preflight" ]]; then
        echo "Nothing was created. Every failure below would otherwise have"
        echo "surfaced only after the cluster was built:"
    else
        echo "Failures:"
    fi
    for f in "${FAIL_MESSAGES[@]}"; do echo "  • $f"; done
    exit 1
fi

# A module that applies and could not run blocks the verdict, because nothing
# established what it asserts (ADR-115). Reported apart from a failure, counted
# the same by the exit status.
if (( MODULE_UNVERIFIED > 0 )); then
    echo ""
    echo "$MODULE_UNVERIFIED module(s) applied and established nothing. That is not a pass:"
    echo "either the subject was unreachable, or the module asserted nothing at all."
    exit 1
fi

# NOTHING APPLIED IS NOT SUCCESS. It means the selection is wrong, not that the
# box is simple -- and it is precisely what exited 0 over an unasserted box.
if (( MODULE_PASSED + MODULE_FAILED == 0 )); then
    echo ""
    echo "No module applied to this box, so nothing was assessed. A selection that"
    echo "matches nothing is a defect in the selection, not a clean result."
    exit 1
fi

if (( WARN_COUNT > 0 )); then
    echo "All applicable checks passed ($WARN_COUNT warning(s))."
else
    echo "All applicable checks passed."
fi
exit 0
