#!/usr/bin/env bash
# Run every validation this platform has, and record what each one did.
#
# The validations existed; the account of them did not. They are spread across
# repo-level checkers, a preflight phase, per-module cluster checks and the
# post-bootstrap components pass, each invoked from somewhere different, and the
# only record of a run was whatever scrolled past. "Did the endpoint probes run
# on this box?" had no answer that did not depend on someone remembering.
#
# So this writes .state/validation.json: one entry per phase, with its status,
# its counts, its exit code and where its output went. A phase that did not run
# is recorded as skipped WITH THE REASON, never omitted and never folded into the
# pass count -- the distinction between "checked and fine" and "never checked" is
# the one that matters, and it is exactly the one a summary line loses.
#
#   scripts/validate-all.sh                      every phase
#   scripts/validate-all.sh --only=static        one, by name
#   BUNDLE_VERSION=0.1.16-rc.44 scripts/validate-all.sh --only=adr
#   scripts/validate-all.sh --list               what the phases are
#
# Exit is non-zero if any phase failed. A skipped phase does not fail a FULL run
# -- preflight needs cloud credentials and the cluster phases need a cluster, and
# refusing to report on a laptop with neither would make this unusable there.
#
# A phase the caller NAMED is different: `--only=platform` is a request to prove
# the platform, and skipping is not an answer to it. That call is the tenant
# bootstrap's final gate, and it skipped for a missing kubeconfig and exited 0 --
# so a box nothing had checked reported success. A named phase that cannot run
# fails.
#
# Inapplicability still skips, but it has to be read from what the box declares
# rather than inferred from an absent file or an unset variable.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PLATFORM_DIR="$(dirname "$SCRIPT_DIR")"
# Tracked, not just defaulted. The `static` phase validates THIS REPOSITORY, so
# a report beside it is right. The `platform` phase validates A BOX, and a box's
# record written into the platform checkout is one box's state in a tree shared
# by every run -- the same mistake as a per-box fact in a shipped chart, which
# this session found four times.
#
# The platform's OWN box legitimately has ZERO_OPS_DIR == PLATFORM_DIR, so the
# two cases cannot be told apart by comparing paths. What distinguishes them is
# whether the caller said so.
ZERO_OPS_DIR_EXPLICIT=1
if [[ -z "${ZERO_OPS_DIR:-}" ]]; then
    ZERO_OPS_DIR="$PLATFORM_DIR"
    ZERO_OPS_DIR_EXPLICIT=0
fi

LOG_DIR="$ZERO_OPS_DIR/.state/logs"
REPORT="$ZERO_OPS_DIR/.state/validation.json"
mkdir -p "$LOG_DIR"

# No ENVIRONMENT (ADR-111 Revision 7). It defaulted to "dev", so a validator that
# read it judged every box as a dev one. The checks read the box's own
# declarations instead.
CLUSTER_NAME="${CLUSTER_NAME:-${CLUSTER:-}}"
ONLY=""

for arg in "$@"; do
    case "$arg" in
        --only=*) ONLY="${arg#*=}" ;;
        --list)   printf '%s\n' static preflight platform adr; exit 0 ;;
        -h|--help)
            sed -n '2,20p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
            exit 0 ;;
        *) echo "validate-all: unknown argument $arg" >&2; exit 2 ;;
    esac
done

wants() { [[ -z "$ONLY" || ",$ONLY," == *",$1,"* ]]; }

# requested <phase> -- the caller NAMED it, rather than receiving it as part of
# a full run. wants() cannot tell those apart, and the difference decides
# whether a missing prerequisite is an excuse or a failure.
requested() { [[ -n "$ONLY" && ",$ONLY," == *",$1,"* ]]; }

# ─── Phase results, accumulated as JSON objects ──────────────────────────────
PHASES_JSON=()
ANY_FAILED=0

# record <name> <status> <started> <ended> <exit> <passed> <failed> <warned> <log> <note>
record() {
    local name="$1" status="$2" started="$3" ended="$4" code="$5"
    local p="$6" f="$7" w="$8" logfile="$9" note="${10}"
    PHASES_JSON+=("$(python3 - "$name" "$status" "$started" "$ended" "$code" "$p" "$f" "$w" "$logfile" "$note" <<'PY'
import json, sys
n, st, a, b, code, p, f, w, log, note = sys.argv[1:11]
out = {"name": n, "status": st, "startedAt": a, "endedAt": b}
# Counts are omitted rather than zeroed when they could not be read. A phase that
# ran and whose summary could not be parsed is not a phase that checked nothing.
if p != "-":
    out.update({"passed": int(p), "failed": int(f), "warned": int(w)})
if code != "-":
    out["exitCode"] = int(code)
if log:
    out["log"] = log
if note:
    out["note"] = note
print(json.dumps(out))
PY
)")
    [[ "$status" == "failed" ]] && ANY_FAILED=1
    return 0
}

now() { date -u +%Y-%m-%dT%H:%M:%SZ; }

# skip <phase> <why>
#
# Records a failure instead when the caller asked for this phase by name. See
# the header: a skip is an answer only when nobody asked the question.
skip() {
    if requested "$1"; then
        record "$1" failed "$(now)" "$(now)" - - - - "" \
            "$2 (asked for by name with --only=$ONLY, so it cannot be skipped)"
        echo "  ❌  $1 — $2"
        echo "      --only=$ONLY asks for this phase. It cannot be answered by skipping," >&2
        echo "      so this run fails rather than reporting a box nothing checked." >&2
        return 0
    fi
    record "$1" skipped "$(now)" "$(now)" - - - - "" "$2"
    echo "  ⏭️  $1 — $2"
}

# inapplicable <phase> <why>
#
# A genuine skip, even for a phase the caller named, because the question has an
# answer and the answer is "nothing to assert". Read from what the box DECLARES
# -- not from an absent file or an unset variable, which is skip()'s case and
# fails.
#
# The distinction is the whole of it. "This box declares no workload cluster" is
# a topology; "the pool name was not passed" is a check that could not run. The
# first must not fail a management-only box, which is built on its own and is the
# first bootstrap of every box; the second must not pass one.
inapplicable() {
    record "$1" skipped "$(now)" "$(now)" - - - - "" "$2 (not applicable to this box)"
    echo "  ⏭️  $1 — $2"
}

# run_phase <name> <logfile> <command...>
#
# Counts come from the summary line the validators already print
# ("<phase>: N passed, N failed, N warned"). Parsed from the LOG rather than
# from a pipeline, so the exit code is the command's own and not a tee's.
run_phase() {
    local name="$1" logfile="$2"; shift 2
    local started; started="$(now)"
    echo "── $name ─────────────────────────────────────────────"
    "$@" >"$logfile" 2>&1
    local code=$?
    local ended; ended="$(now)"

    local p="-" f="-" w="-"
    local line
    line="$(grep -oE '[0-9]+ passed, [0-9]+ failed, [0-9]+ warned' "$logfile" | tail -1 || true)"
    if [[ -n "$line" ]]; then
        p="$(awk '{print $1}' <<<"$line")"
        f="$(awk '{print $3}' <<<"$line")"
        w="$(awk '{print $5}' <<<"$line")"
    fi

    local status=passed
    [[ "$code" -ne 0 ]] && status=failed
    record "$name" "$status" "$started" "$ended" "$code" "$p" "$f" "$w" "${logfile#$ZERO_OPS_DIR/}" ""

    if [[ "$status" == passed ]]; then
        echo "  ✅ $name${line:+ ($line)}"
    else
        echo "  ❌ $name (exit $code)${line:+ — $line}"
        tail -12 "$logfile" | sed 's/^/     /'
    fi
}

# ─── static: the repo's own checkers, no cluster required ────────────────────
if wants static; then
    STATIC=(
        "$PLATFORM_DIR/scripts/validate-shipped-artifacts.sh"
        "$PLATFORM_DIR/scripts/validate-argocd-seed-parity.sh"
        "$PLATFORM_DIR/scripts/validate-component-descriptors.sh"
        "$PLATFORM_DIR/scripts/validate-cluster-name-contract.sh"
        "$PLATFORM_DIR/scripts/validate-spokepool-compositions.sh"
        "$PLATFORM_DIR/scripts/lint-helm-values-yaml.sh"
    )
    static_log="$LOG_DIR/validation-static.log"
    started="$(now)"; : >"$static_log"
    echo "── static ─────────────────────────────────────────────"
    sp=0; sf=0
    for checker in "${STATIC[@]}"; do
        [[ -x "$checker" ]] || { echo "  (absent: ${checker##*/})" >>"$static_log"; continue; }
        echo "### ${checker##*/}" >>"$static_log"
        if ( cd "$PLATFORM_DIR" && "$checker" ) >>"$static_log" 2>&1; then
            ((sp++)); echo "  ✅ ${checker##*/}"
        else
            ((sf++)); echo "  ❌ ${checker##*/}"
        fi
    done
    st=passed; [[ "$sf" -gt 0 ]] && st=failed
    record static "$st" "$started" "$(now)" "$sf" "$sp" "$sf" 0 "${static_log#$ZERO_OPS_DIR/}" ""
fi

# ─── preflight: before a cluster exists ──────────────────────────────────────
if wants preflight; then
    if [[ -z "${HCLOUD_TOKEN:-}" ]]; then
        # A platform-developer phase, not a box command: these checks query a cloud
        # account directly, before any box exists to hold a credential, so the one
        # they use is supplied here. No box lifecycle command reads this variable.
        skip preflight "HCLOUD_TOKEN is not set; these pre-cluster checks query a cloud account directly"
    else
        run_phase preflight "$LOG_DIR/validation-preflight.log" \
            bash "$PLATFORM_DIR/scripts/validate/run.sh" preflight
    fi
fi

# ─── platform: the components, then every cluster module at --mode=final ─────
if wants platform; then
    if [[ "$ZERO_OPS_DIR_EXPLICIT" -eq 0 ]]; then
        echo "validate-all: refusing to validate a box without ZERO_OPS_DIR." >&2
        echo "  The report and its logs belong with the box's own state, and this" >&2
        echo "  would write them into the platform checkout instead." >&2
        echo "  Set ZERO_OPS_DIR to the workspace the box was bootstrapped from." >&2
        exit 2
    fi
    kubeconfig="${KUBECONFIG:-}"
    if [[ -z "$kubeconfig" && -n "$CLUSTER_NAME" ]]; then
        kubeconfig="${KAMARKAT_HOME:-$HOME/.kamarkat}/run/kubeconfig/${CLUSTER_NAME}.kubeconfig"
    fi
    if [[ -z "$kubeconfig" || ! -r "$kubeconfig" ]]; then
        skip platform "no readable kubeconfig (set KUBECONFIG, or CLUSTER_NAME for the conventional path)"
    else
        # NO WHOLE-PHASE SKIP. This phase used to be marked inapplicable when the
        # box declared no workload cluster, which skipped all 35 modules because
        # ONE of their subjects was absent -- and then exited 0. A box with two
        # broken public surfaces passed its bootstrap that way (2026-11-19).
        #
        # Applicability is the MODULE's now (ADR-115): each declares what it is
        # about, the runner resolves whether this box gives it a subject, and a
        # workload cluster that is declared and unreachable is reported pending
        # rather than quietly dropped. A phase is only skipped when it cannot be
        # reached at all, which is the kubeconfig case above.
        #
        # SPOKEPOOL_NAME remains how a run names WHICH workload cluster to assess
        # when a box declares several. Unset, the workload-cluster modules report
        # that; it no longer silences the rest.
        # NO DOMAIN READ HERE, and no skip for the lack of one. The zone comes
        # from HubEnvironment.spec.domain (ADR-051) and scripts/validate/common.sh
        # reads it from the cluster when a caller does not pass it -- so every
        # caller gets the same value, including ones that reach the modules
        # directly. Reading it here as well made this script the only caller that
        # worked.
        #
        # A box that declares no domain no longer skips this phase either. It has
        # no zone, which the hostname checks say per check, where it is visible.
        # Skipping the phase said it about all 36 at once, in a line that read
        # like a box with nothing to check.
        #
        # SPOKEPOOL_NAME is optional and may be unset: with one workload cluster
        # declared the runner derives it, with none the workload-cluster checks
        # report that, and with several it is required. Expanded with a default
        # because this runs under `set -u`, where a bare "$SPOKEPOOL_NAME" aborted
        # the whole assessment with "unbound variable".
        run_phase platform "$LOG_DIR/validation-platform.log" \
            env KUBECONFIG="$kubeconfig" ZERO_OPS_DIR="$ZERO_OPS_DIR" \
                SPOKEPOOL_NAME="${SPOKEPOOL_NAME:-}" \
                bash "$PLATFORM_DIR/scripts/post-bootstrap-validate.sh"
    fi
fi

# ─── adr: what ADR-062..070 claim, asserted against the live box ─────────────
#
# Omitted when this orchestrator was first written, which is exactly the gap it
# exists to close: the ADR acceptance suite ran only from local-e2e's `adr`
# phase, so a run that stopped earlier -- at a gate, say -- left no record that
# the platform's own architectural claims had never been checked, and nothing
# distinguished that from their having passed.
if wants adr; then
    adr_script="$PLATFORM_DIR/scripts/dev/adr-acceptance.sh"
    kubeconfig="${KUBECONFIG:-}"
    if [[ -z "$kubeconfig" && -n "$CLUSTER_NAME" ]]; then
        kubeconfig="${KAMARKAT_HOME:-$HOME/.kamarkat}/run/kubeconfig/${CLUSTER_NAME}.kubeconfig"
    fi
    if [[ ! -x "$adr_script" ]]; then
        skip adr "adr-acceptance.sh is not present at $adr_script"
    elif [[ -z "$kubeconfig" || ! -r "$kubeconfig" ]]; then
        skip adr "no readable kubeconfig; the ADR claims are asserted against a live box"
    elif [[ -z "${BUNDLE_VERSION:-}" ]]; then
        # Required, not guessed. ADR-063's assertion is that every chart the
        # cluster names is published AT THIS VERSION, so a wrong one either fails
        # a correct box or passes an unpublished bundle.
        skip adr "BUNDLE_VERSION is not set; ADR-063 asserts the charts are published at a specific version"
    else
        run_phase adr "$LOG_DIR/validation-adr.log" \
            env PATH="$PLATFORM_DIR/bin:$PATH" \
                bash "$adr_script" "$BUNDLE_VERSION" "$ZERO_OPS_DIR" "$kubeconfig" \
                    "${BUNDLE_REGISTRY:-ghcr.io/kamarkat/charts}"
    fi
fi

# ─── the report ──────────────────────────────────────────────────────────────
# Written whatever happened, including when nothing ran: a run that produced no
# report is indistinguishable from one that was never started.
python3 - "$REPORT" "$CLUSTER_NAME" "$ANY_FAILED" "${PHASES_JSON[@]:-}" <<'PY'
import json, os, sys, datetime
report, cluster, any_failed = sys.argv[1:4]
phases = [json.loads(p) for p in sys.argv[4:] if p.strip()]
doc = {
    "schemaVersion": 1,
    "generatedAt": datetime.datetime.now(datetime.timezone.utc)
                   .replace(microsecond=0).isoformat().replace("+00:00", "Z"),
    "cluster": cluster or None,
    # "passed" only when something actually ran and nothing failed. A run whose
    # every phase was skipped is "incomplete", and incomplete DOES NOT EXIT 0 --
    # see below. This file computed the distinction correctly and then threw it
    # away one line later, which is how `result: incomplete` printed inside a
    # green job (2026-11-19).
    "result": ("failed" if any_failed == "1"
               else "passed" if any(p["status"] == "passed" for p in phases)
               else "incomplete"),
    "phases": phases,
}
os.makedirs(os.path.dirname(report), exist_ok=True)
tmp = report + ".tmp"
with open(tmp, "w") as fh:
    json.dump(doc, fh, indent=2)
    fh.write("\n")
os.replace(tmp, report)
print(f"\n  report: {report}")
print(f"  result: {doc['result']}  "
      + "  ".join(f"{p['name']}={p['status']}" for p in phases))
PY

# INCOMPLETE IS NOT SUCCESS. `exit "$ANY_FAILED"` alone reported zero for a run
# that asserted nothing, so the Day-0 command read it as success and printed
# "built and validated" over a box whose checks had all been skipped.
#
# A phase that could not run is now a non-zero exit, with the report naming which
# and why. A phase that is genuinely inapplicable to this box -- there is nothing
# of its subject here -- is recorded as such by the phase itself and does not
# reach this.
if [[ "$ANY_FAILED" != "0" ]]; then
    exit "$ANY_FAILED"
fi
RESULT="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["result"])' "$REPORT" 2>/dev/null || echo unknown)"
if [[ "$RESULT" != "passed" ]]; then
    echo ""
    echo "  result: $RESULT — nothing was established. A run that asserted nothing is"
    echo "  not a run that passed; the report above names every phase and why."
    exit 1
fi
exit 0
