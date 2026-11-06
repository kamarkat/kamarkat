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

# Run every validate_* function each sourced module defined, in module order.
for m in "${MODULES[@]}"; do
    while IFS= read -r fn; do
        [[ -n "$fn" ]] && "$fn"
    done < <(grep -oE '^validate_[a-z0-9_]+\(\)' "$m" | tr -d '()')
done

[[ "$PHASE" == "cluster" ]] && cleanup_spoke_kubeconfig

echo ""
echo "═══════════════════════════════════════════════════════════"
echo "  $PHASE: $PASS_COUNT passed, $FAIL_COUNT failed, $WARN_COUNT warned"
echo "═══════════════════════════════════════════════════════════"

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

if (( WARN_COUNT > 0 )); then
    echo "All checks passed ($WARN_COUNT warning(s))."
else
    echo "All checks passed."
fi
exit 0
