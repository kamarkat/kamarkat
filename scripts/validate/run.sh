#!/usr/bin/env bash
# Validation runner (ADR-046 / ADR-050 / ADR-051).
#
#   run.sh preflight                     every static check; needs no cluster
#   run.sh cluster                       every live-cluster check
#   run.sh cluster --only=NAME[,NAME]    a subset, for in-flight bootstrap gates
#   run.sh cluster --explain             what WOULD run, and why; runs nothing
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
EXPLAIN=""
for arg in "$@"; do
    case "$arg" in
        --only=*) ONLY="${arg#*=}" ;;
        --mode=*) VALIDATE_MODE="${arg#*=}" ;;
        --explain) EXPLAIN=1 ;;
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
    # THE BOX's repository, not the platform's. This globbed $VALIDATE_ROOT,
    # which in a released run is the temp directory the release was extracted
    # into -- it has no registry/ at all, so every box read as declaring no
    # workload cluster and its workload-cluster modules were marked not
    # applicable. The exact omission this change exists to remove, reintroduced
    # by reading the wrong root.
    #
    # Unset is not "undeclared". Without the box's repository the declarations
    # cannot be read, and a question that cannot be answered is not an answer of
    # no -- so it is reported as unverified rather than defaulted.
    if [[ -z "${ZERO_OPS_DIR:-}" ]]; then
        SPOKE_STATE="unreadable"
        SPOKE_REASON="the box's repository was not named (ZERO_OPS_DIR), so its declarations could not be read"
        return 0
    fi
    local -a declared=()
    local d
    for d in "$ZERO_OPS_DIR"/registry/clusters/*/infrastructure/spokepool.yaml; do
        [[ -e "$d" ]] || continue
        d="${d%/infrastructure/spokepool.yaml}"
        declared+=("$(basename "$d")")
    done

    if [[ -z "${SPOKEPOOL_NAME:-}" && ${#declared[@]} -eq 0 ]]; then
        SPOKE_STATE="undeclared"
        SPOKE_REASON="this box declares no workload cluster"
        return 0
    fi

    # ONE DECLARED CLUSTER HAS ONE ANSWER to "which one". The name was required
    # from the caller, so a box that declared a single workload cluster and did
    # not repeat its name reported every workload-cluster check as unassessable
    # -- including the bootstrap's own assessment of the management cluster,
    # which runs before that cluster is built and cannot name it.
    #
    # With SEVERAL declared it stays required. Picking one would assess a third
    # of a box and report it as the whole.
    if [[ -z "${SPOKEPOOL_NAME:-}" ]]; then
        if [[ ${#declared[@]} -eq 1 ]]; then
            SPOKEPOOL_NAME="${declared[0]}"
        else
            # SEVERAL DECLARED AND NONE NAMED, which the bootstrap does on
            # purpose: naming one of three would assess a third of a box and
            # report it as the whole. So the answer depends on whether any of
            # them exists yet.
            #
            # None built yet -> pending, exactly as for a single one. This is the
            # bootstrap's own first assessment, and failing it would fail every
            # multi-cluster box before it had built anything.
            #
            # Some built -> unverified. The run could have assessed one and did
            # not, and calling that pending would file an omission under "still
            # building" forever.
            local p built=0
            for p in "${declared[@]}"; do
                SPOKEPOOL_NAME="$p"
                if ensure_spoke_kubeconfig; then built=1; cleanup_spoke_kubeconfig; break; fi
            done
            SPOKEPOOL_NAME=""
            SPOKE_KUBECONFIG=""
            if (( built )); then
                SPOKE_STATE="unnamed"
                SPOKE_REASON="this box declares ${#declared[@]} workload clusters and none was named for this run; name one to assess it"
            else
                SPOKE_STATE="pending"
                SPOKE_REASON="all ${#declared[@]} declared workload clusters are not reachable yet"
            fi
            return 0
        fi
    fi

    if ensure_spoke_kubeconfig; then
        SPOKE_STATE="reachable"
        SPOKE_REASON=""
    else
        SPOKE_STATE="pending"
        SPOKE_REASON="$SPOKEPOOL_NAME is declared and is not reachable yet"
    fi
    return 0
}

# RESOLVED ONCE, IN THIS SHELL, and never in a command substitution.
#
# Everything this decides has to outlive the decision: the state the loop
# branches on, the pool name a single declaration implies, and the kubeconfig the
# modules then read. Resolving it in a subshell lost all three -- the state
# defaulted to the wrong branch, so an undeclared cluster reported `unverified`
# instead of `inapplicable`; the derived name never reached the modules; and a
# reachable cluster would have been asserted against with no kubeconfig.
SPOKE_STATE=""
SPOKE_REASON=""
resolve_spoke_state() { [[ -n "$SPOKE_STATE" ]] || spoke_state; return 0; }

# EXPLAIN: what would be assessed, and what would not, with the reason.
#
# Asserts nothing and reports no pass. It exists because applicability is derived
# from declarations, so "why did that check not run" has an answer the box itself
# can give -- and a reader who has to guess at it ends up trusting a skip.
if [[ -n "$EXPLAIN" ]]; then
    echo ""
    printf "  %-38s %-14s %s\n" "CHECK" "SCOPE" "WOULD RUN"
    for m in "${MODULES[@]}"; do
        name="$(basename "$m" .sh)"; name="${name#*-}"
        scope="$(module_scope "$m")"
        verdict="yes"
        if [[ "$scope" == "spoke" || "$scope" == "path" ]]; then
            resolve_spoke_state
            [[ -n "$SPOKE_REASON" ]] && verdict="no — $SPOKE_REASON"
        fi
        printf "  %-38s %-14s %s\n" "$name" "$scope" "$verdict"
    done
    echo ""
    echo "  Nothing was asserted. This run establishes nothing about the box."
    [[ "$PHASE" == "cluster" ]] && cleanup_spoke_kubeconfig
    exit 0
fi

# Run every validate_* function each applicable module defined, in module order.
for m in "${MODULES[@]}"; do
    MODULE_NAME="$(basename "$m" .sh)"; MODULE_NAME="${MODULE_NAME#*-}"
    scope="$(module_scope "$m")"

    if [[ "$scope" == "spoke" || "$scope" == "path" ]]; then
        resolve_spoke_state
        if [[ -n "$SPOKE_REASON" ]]; then
            section "$MODULE_NAME"
            case "$SPOKE_STATE" in
                undeclared) inapplicable "$SPOKE_REASON" || true ;;
                pending)    pending "$SPOKE_REASON" || true ;;
                *)          unverified "$SPOKE_REASON" || true ;;
            esac
            continue
        fi
    fi

    before_pass=$PASS_COUNT; before_fail=$FAIL_COUNT; before_warn=$WARN_COUNT
    # Every outcome a module can report about ITSELF, so the accounting below can
    # tell "it said something" from "it said nothing". Omitting pending here
    # counted a module that correctly reported a pending subject as having
    # asserted nothing, and reported it twice.
    before_skip=$((MODULE_INAPPLICABLE + MODULE_UNVERIFIED + MODULE_PENDING))

    while IFS= read -r fn; do
        [[ -n "$fn" ]] && "$fn"
    done < <(grep -oE '^validate_[a-z0-9_]+\(\)' "$m" | tr -d '()')

    # The module's own outcome, derived from what it did rather than declared.
    if (( MODULE_INAPPLICABLE + MODULE_UNVERIFIED + MODULE_PENDING > before_skip )); then
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
$MODULE_UNVERIFIED unverified, $MODULE_PENDING pending, $MODULE_INAPPLICABLE not applicable"
echo "═══════════════════════════════════════════════════════════"

# Every omission, with its reason. An omission nobody can read is the shape this
# whole file now exists to refuse.
if (( MODULE_UNVERIFIED + MODULE_PENDING + MODULE_INAPPLICABLE > 0 )); then
    echo ""
    echo "Not established:"
    for r in "${MODULE_RESULTS[@]}"; do
        case "$r" in
            unverified\|*|pending\|*|inapplicable\|*)
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

# INCOMPLETE: everything that could be assessed passed, and something the box
# declared does not exist yet. Exit 0, because there is no defect here -- the
# bootstrap that is still building that cluster is the thing to wait for, not a
# failure to fix (ADR-115).
#
# Said in full rather than folded into the success line. "Accepted" and "accepted
# so far" are different claims, and a reader who cannot tell them apart is the
# reader this whole change is for.
# NOTHING APPLIED IS NOT SUCCESS. It means the selection is wrong, not that the
# box is simple -- and it is precisely what exited 0 over an unasserted box.
#
# A pending module counts here: it did not assess anything, but it named a
# subject this box declared, so the selection was not empty. Excluding it would
# turn "your cluster is still building" into "your selection is broken".
if (( MODULE_PASSED + MODULE_FAILED + MODULE_PENDING == 0 )); then
    echo ""
    echo "No module applied to this box, so nothing was assessed. A selection that"
    echo "matches nothing is a defect in the selection, not a clean result."
    exit 1
fi

if (( MODULE_PENDING > 0 )); then
    echo ""
    echo "result: incomplete — $MODULE_PASSED module(s) passed and $MODULE_PENDING are waiting"
    echo "on something this box declares and has not built yet, named above. What was"
    echo "assessed is established; the rest is not assessed, and is not implied by it."
    exit 0
fi

if (( WARN_COUNT > 0 )); then
    echo "All applicable checks passed ($WARN_COUNT warning(s))."
else
    echo "All applicable checks passed."
fi
exit 0
