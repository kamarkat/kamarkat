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
    local found=false
    for x in "${arr[@]}"; do [[ "$x" == "$c" ]] && { found=true; break; }; done
    $found || arr+=("$c")
    local json="["
    for i in "${!arr[@]}"; do
        [[ $i -gt 0 ]] && json+=","
        json+="\"${arr[$i]}\""
    done
    json+="]"
    printf '{"completedChecks":%s}' "$json" > "$STATE_FILE.tmp" && mv "$STATE_FILE.tmp" "$STATE_FILE"
}

auto_install() {
    local tool="$1" dir="$HOME/bin"
    mkdir -p "$dir"
    case "$tool" in
        kind) curl -fsSLo "$dir/kind.exe" "https://kind.sigs.k8s.io/dl/latest/kind-windows-amd64" && chmod +x "$dir/kind.exe" && export PATH="$PATH:$dir" ;;
        clusterctl) curl -fsSLo "$dir/clusterctl.exe" "https://github.com/kubernetes-sigs/cluster-api/releases/download/v1.13.7/clusterctl-windows-amd64.exe" && chmod +x "$dir/clusterctl.exe" && export PATH="$PATH:$dir" ;;
        helm) curl -fsSLo "$dir/helm.zip" "https://get.helm.sh/helm-v3.17.3-windows-amd64.zip" && unzip -jo "$dir/helm.zip" "windows-amd64/helm.exe" -d "$dir" 2>/dev/null && rm -f "$dir/helm.zip" && chmod +x "$dir/helm.exe" && export PATH="$PATH:$dir" ;;
        jq) curl -fsSLo "$dir/jq.exe" "https://github.com/jqlang/jq/releases/download/jq-1.7.1/jq-windows-amd64.exe" && chmod +x "$dir/jq.exe" && export PATH="$PATH:$dir" ;;
        aws)
            local aws_dir="/c/Program Files/Amazon/AWSCLIV2"
            [[ -f "$aws_dir/aws.exe" ]] && { case ":$PATH:" in *":$aws_dir:"*) ;; *) export PATH="$aws_dir:$PATH" ;; esac; return 0; }
            winget install -e --id Amazon.AWSCLI --silent --accept-package-agreements 2>/dev/null && return 0
            local msi="$dir/AWSCLIV2.msi"
            curl -fsSLo "$msi" "https://awscli.amazonaws.com/AWSCLIV2.msi" 2>/dev/null || return 1
            msiexec //i "$msi" //quiet //norestart 2>/dev/null || true; rm -f "$msi"
            [[ -f "$aws_dir/aws.exe" ]] && { export PATH="$aws_dir:$PATH"; return 0; }
            return 1
            ;;
        *) return 1 ;;
    esac
}

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

    check "kubectl" "kubectl CLI" bash -c "command -v kubectl >/dev/null 2>&1" || failed=1
    check "jq" "jq" bash -c "command -v jq >/dev/null 2>&1 || auto_install jq" || failed=1
    check "kind" "kind" bash -c "command -v kind >/dev/null 2>&1 || auto_install kind" || failed=1
    check "clusterctl" "clusterctl" bash -c "command -v clusterctl >/dev/null 2>&1 || auto_install clusterctl" || failed=1
    check "helm" "helm" bash -c "command -v helm >/dev/null 2>&1 || auto_install helm" || failed=1
    check "aws" "AWS CLI" bash -c "command -v aws >/dev/null 2>&1 || auto_install aws" || failed=1

    check "docker_cli" "docker CLI" bash -c "command -v docker >/dev/null 2>&1" || failed=1
    check "docker_daemon" "docker daemon" bash -c "docker info >/dev/null 2>&1" || failed=1
    check "kakt_binary" "kakt binary" test -f "$ZERO_OPS_DIR/bin/kakt" || failed=1

    # The file is the input to an enrolment, not a source the platform reads.
    # The operator's own store, not a checkout: these outlive every clone.
    KAMARKAT_STORE="${KAMARKAT_HOME:-$HOME/.kamarkat}"
    check "hetzner_token" "Hetzner token to enrol" test -f "$KAMARKAT_STORE/hetzner/token" || { log "  Fix: put the token in $KAMARKAT_STORE/hetzner/token, then enrol it: kakt escrow enroll-provider-credential --gitops-dir . --cluster <cluster> < $KAMARKAT_STORE/hetzner/token"; failed=1; }
    check "github_pat" "GitHub PAT" test -f "$KAMARKAT_STORE/github/github-pat-token" || { log "  Fix: create $KAMARKAT_STORE/github/github-pat-token"; failed=1; }
    check "aws_profile" "AWS profile" bash -c "aws configure list --profile zerotouch-platform-admin >/dev/null 2>&1" || { log "  Fix: aws configure --profile zerotouch-platform-admin"; failed=1; }

    [[ $failed -ne 0 ]] && error_exit "Missing prerequisites. Fix and re-run."
    log "" && log "✅ All prerequisites satisfied! Run: bash scripts/hub-bootstrap.sh --provider=hetzner"
}

main "$@"
