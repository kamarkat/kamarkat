#!/bin/bash

# Zero-Ops Cloud Provider Prerequisite Checker
# Standalone script. Validates tools needed for Hetzner cloud bootstrap.
# Idempotent: tracks checks in .zero-ops/pre-req-cloud-state.json.
# Delete that file to re-run all checks from scratch.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ZERO_OPS_DIR="$(dirname "$SCRIPT_DIR")"
LOG_DIR="$ZERO_OPS_DIR/.zero-ops"
STATE_FILE="$LOG_DIR/pre-req-cloud-state.json"

mkdir -p "$LOG_DIR"

log() { echo "[$(date '+%Y-%m-%d %H:%M:%S')] $1" | tee -a "$LOG_DIR/pre-req-cloud.log"; }
error_exit() { log "ERROR: $1"; exit 1; }

is_completed() {
    local c="$1"
    [[ ! -f "$STATE_FILE" ]] && { printf '{"completedChecks":[]}' > "$STATE_FILE"; return 1; }
    jq -e --arg c "$c" '.completedChecks | any(. == $c)' "$STATE_FILE" >/dev/null 2>&1
}

mark_completed() {
    local c="$1" existing arr=()
    existing=$(jq -r '.completedChecks[]?' "$STATE_FILE" 2>/dev/null | tr -d '\r' || echo "")
    for x in $existing; do arr+=("$x"); done
    # ${arr[@]} on an EMPTY array is an unbound variable under `set -u` in bash
    # 3.2, which is what macOS ships -- so the first check of a fresh run died
    # here, before any check had been recorded. Guarded by length rather than by
    # relaxing set -u.
    local found=false
    if (( ${#arr[@]} )); then
        for x in "${arr[@]}"; do [[ "$x" == "$c" ]] && { found=true; break; }; done
    fi
    $found || arr+=("$c")
    local json="["
    for i in "${!arr[@]}"; do
        [[ $i -gt 0 ]] && json+=","
        json+="\"${arr[$i]}\""
    done
    json+="]"
    printf '{"completedChecks":%s}' "$json" > "$STATE_FILE.tmp" && mv "$STATE_FILE.tmp" "$STATE_FILE"
}

# Installing a missing tool is lib/tool-install.sh's job, not a second copy of
# it. This script had its own: four branches that each downloaded a Windows
# artefact whatever the host was, one of them from /latest/, plus an AWS CLI
# branch that shelled out to winget and msiexec. None of it had run in a long
# time -- see the note in main() about the subshell.
# shellcheck source=lib/tool-install.sh
source "$SCRIPT_DIR/lib/tool-install.sh"

check() {
    local name="$1" desc="$2"
    is_completed "$name" && { log "  ✓ $desc (previously checked)"; return 0; }
    log "  Checking $desc..."
    shift 2 && "$@" && { mark_completed "$name"; log "  ✓ $desc"; return 0; } || return 1
}

main() {
    local failed=0
    log "Zero-Ops Cloud Provider Prerequisite Check"
    [[ -f "$STATE_FILE" ]] && log "Resuming. Delete $STATE_FILE to re-check."

    # NOT `bash -c "... || auto_install x"`, which is how these read. A shell
    # function does not exist in a child process, so every auto-install branch
    # here failed with "auto_install: command not found" -- reported as the TOOL
    # being missing, which is why nobody chased it.
    #
    # No AWS. Nothing on this path invokes the AWS CLI; the entry existed because
    # object storage is S3-compatible, and the backup credential is escrowed and
    # delivered to the cluster instead.
    have() { command -v "$1" >/dev/null 2>&1; }
    check "kubectl" "kubectl CLI" have kubectl || failed=1
    for t in jq kind clusterctl helm; do
        if is_completed "$t"; then
            log "  ✓ $t (previously checked)"
        elif have "$t" || auto_install_tool "$t"; then
            mark_completed "$t"
            log "  ✓ $t"
        else
            failed=1
        fi
    done

    check "docker_cli" "docker CLI" bash -c "command -v docker >/dev/null 2>&1" || failed=1
    check "docker_daemon" "docker daemon" bash -c "docker info >/dev/null 2>&1" || failed=1
    check "kakt_binary" "kakt binary" test -f "$ZERO_OPS_DIR/bin/kakt" || failed=1

    # The file is the input to an enrolment, not a source the platform reads.
    # The operator's own store, not a checkout: these outlive every clone.
    KAMARKAT_STORE="${KAMARKAT_HOME:-$HOME/.kamarkat}"
    check "hetzner_token" "Hetzner token to enrol" test -f "$KAMARKAT_STORE/hetzner/token" || { log "  Fix: put the token in $KAMARKAT_STORE/hetzner/token, then enrol it: kakt escrow enroll-provider-credential --gitops-dir . --cluster <cluster> < $KAMARKAT_STORE/hetzner/token"; failed=1; }
    # ENVIRONMENT FIRST, matching require_credential in hub-bootstrap.sh: a CI run
    # holds this in the environment and has no file, and demanding the file
    # refused every runner. No AWS profile check either -- there is no AWS
    # consumer to have one for.
    if [[ -n "${GITOPS_REPO_WRITE_TOKEN:-}" ]] || [[ -f "$KAMARKAT_STORE/github/github-pat-token" ]]; then
        mark_completed "github_pat"; log "  ✓ GitHub credential with write access to the GitOps repository"
    else
        log "  Fix: export GITOPS_REPO_WRITE_TOKEN, or create $KAMARKAT_STORE/github/github-pat-token"
        failed=1
    fi

    [[ $failed -ne 0 ]] && error_exit "Missing prerequisites. Fix and re-run."
    log "" && log "✅ All prerequisites satisfied! Run: bash scripts/hub-bootstrap.sh --provider=hetzner"
}

main "$@"
