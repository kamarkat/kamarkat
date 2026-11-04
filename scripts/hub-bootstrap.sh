#!/bin/bash

# Zero-Ops Hub Bootstrap Script
# This script performs the hub-bootstrap steps one by one with proper intervals
# Based on zero-ops/README.md hub bootstrap process
#
# Provider-agnostic after step 1. The bootstrap produces a result contract
# (kubeconfig path) consumed by all downstream steps. No step knows whether
# the cluster came from Hetzner, hybrid, or any other provider.

set -euo pipefail

# Configuration
#
# Two roots, not one, because they stop being the same directory the moment this
# script is not run from a checkout.
#
#   SCRIPT_DIR   where THIS script and its siblings live (lib/, validate/).
#                Derived from BASH_SOURCE, so it is correct whether the script
#                was run from a checkout or materialised from the binary that
#                embeds it.
#   ZERO_OPS_DIR the WORKSPACE the run operates on: state, kubeconfigs, secrets,
#                the tenant's gitops repository. In a checkout these coincide;
#                on a released box the scripts come from the binary and the
#                workspace is the tenant's own directory.
#
# They were one variable (ZERO_OPS_DIR=$(dirname $SCRIPT_DIR)), which silently
# made every workspace path relative to wherever the script happened to sit. Run
# from a temp directory that is true and useless: k8-secrets, .zero-ops and
# bin/kakt would all resolve under the temp copy.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# The workspace. An explicit ZERO_OPS_DIR wins; otherwise fall back to the
# checkout layout, which is what a developer running ./scripts/hub-bootstrap.sh
# expects and what every existing invocation relies on.
if [[ -n "${ZERO_OPS_DIR:-}" ]]; then
    ZERO_OPS_DIR="$(cd "$ZERO_OPS_DIR" && pwd)"
else
    ZERO_OPS_DIR="$(cd "$(dirname "$SCRIPT_DIR")" && pwd)"
fi
PROJECT_ROOT="$ZERO_OPS_DIR"

# Where the platform's own files are: the CLI binary, its source, and the
# developer's provider credentials. Derived from the script's location, so it is
# the checkout this script came from regardless of which workspace it operates on.
#
# Separate from ZERO_OPS_DIR because they are not the same thing once the script
# is pointed at a tenant's repository: state, logs and kubeconfigs belong to the
# box, while the binary and the credentials that create it do not.
PLATFORM_DIR="$(cd "$(dirname "$SCRIPT_DIR")" && pwd)"

# resolve_owned <relative-path> -> the first of workspace, platform that has it.
#
# Workspace first, platform second: the same order the CLI uses for the same files
# (orchestrator.go: "ZERO_OPS_DIR first, then the executable's directory"). A real
# tenant box carries its own credentials in its own repository and finds them
# there; a developer's checkout carries them once and every workspace finds them.
# Falls back to the workspace path so a missing file is reported against the place
# it is expected to be.
resolve_owned() {
    local rel="$1"
    if [[ -e "$ZERO_OPS_DIR/$rel" ]]; then
        printf '%s' "$ZERO_OPS_DIR/$rel"
    elif [[ -e "$PLATFORM_DIR/$rel" ]]; then
        printf '%s' "$PLATFORM_DIR/$rel"
    else
        printf '%s' "$ZERO_OPS_DIR/$rel"
    fi
}

# Logs live under .state too, because .zero-ops is retired. One hidden directory
# holds what a box has recorded about itself; .zero-ops held a durable state file
# beside run artefacts, so "clear the logs" could clear the record of what had
# already been done.
LOG_DIR="$ZERO_OPS_DIR/.state/logs"
# Step state lives with the other state, not with the logs.
#
# .state/ is what the box's state is: the CLI already writes its phase record to
# .state/<cluster>.json there, so one directory answers "what has been
# done to this box". Keeping the step record beside the logs made a durable fact
# look like a run artefact.
# Two records, one per cluster, because a run builds two.
#
# One file held both, so "has this step run" was answered for the box rather than
# for the cluster the step belongs to — and tearing down a workload cluster left
# its steps marked complete in the same file the management cluster reads. The
# legacy script kept them apart and this restores that.
#
# Which file a step belongs to is declared once, below, rather than decided at
# each call site: a step recorded in one file and read from the other is a step
# that silently re-runs forever, or never runs again.
MGMT_STATE_FILE="$ZERO_OPS_DIR/.state/bootstrap-mgmt.json"
WORKLOAD_STATE_FILE="$ZERO_OPS_DIR/.state/bootstrap-workload.json"

# Steps that belong to the WORKLOAD cluster. Everything else is the management
# cluster's.
WORKLOAD_STEPS=(wait_spokepool spoke_home_worker)

state_file_for() {
    local step="$1" w
    for w in "${WORKLOAD_STEPS[@]}"; do
        if [[ "$step" == "$w" ]]; then
            printf '%s' "$WORKLOAD_STATE_FILE"
            return
        fi
    done
    printf '%s' "$MGMT_STATE_FILE"
}

# The CLI this script drives.
#
# KAKT_BINARY wins when set: a released run already downloaded a pinned binary,
# and rebuilding or re-resolving it would discard the version the caller chose.
# Otherwise the checkout's bin/kakt, which is what build_binary produces.
KAKT_BINARY="${KAKT_BINARY:-}"   # resolved in check_prerequisites, once resolve_owned exists

# Defaults (overridable via flags)
# The cluster name must be stable across runs: the Go state file is the source
# of truth for bootstrap completion.
CLUSTER_NAME="${CLUSTER_NAME:-hub}"
PROVIDER="${PROVIDER:-hetzner}"
REGION="${REGION:-hel1}"

# Cluster creation mode (ADR-055). sequenced = boundaries activated in phase
# order (the default, and the flow this script has always run); converged = all
# boundaries reconcile concurrently. Mode and environment are independent: any
# environment may be created in either mode.
GATING="${GATING:-sequenced}"
# Which bundle this run reconciles. Empty builds an unreleased binary, which
# sources platform content from the working tree (ADR-068) -- the right default
# for a cluster being built. A version here injects it at build time, so the run
# resolves the published distribution instead and exercises the path a tenant
# actually gets. Nothing else changes: the same binary, the same phases.
BUNDLE_VERSION="${BUNDLE_VERSION:-}"
# A checkout of the tenant's own repository. Set, the cluster is seeded with the
# declaration that repository holds and its Day-0 artifacts are committed there
# (ADR-072). Empty, the CLI renders its own seed, which is this repository's
# development path.
GITOPS_DIR="${GITOPS_DIR:-}"

# Teardown existing cluster before bootstrap
TEARDOWN="${TEARDOWN:-false}"
SEED_ONLY="${SEED_ONLY:-false}"

# SpokePool Configuration (provider-agnostic)
SPOKEPOOL_NAME="${SPOKEPOOL_NAME:-}"
SPOKEPOOL_NAMESPACE="${SPOKEPOOL_NAMESPACE:-platform-ops}"
SPOKEPOOL_TIMEOUT="${SPOKEPOOL_TIMEOUT:-1800}"  # 30 minutes in seconds

# Environment slug for matrix topology (ADR 037). MUST be explicitly set
# via --environment flag. The Go bootstrap CLI defaults to prod for hetzner,
# hybrid if omitted.
ENVIRONMENT="${ENVIRONMENT:-}"
# Hybrid provider cell (ADR-046): passed through to `hub bootstrap --provider=hybrid`.
# HOME_WORKER_ENABLED=1 activates the home-worker join flow; TAILNET_NAME sets
# the Tailscale MagicDNS tailnet for the spoke control-plane endpoint.
HOME_WORKER_ENABLED="${HOME_WORKER_ENABLED:-}"
HOME_WORKER_TTL="${HOME_WORKER_TTL:-24h}"
TAILNET_NAME="${TAILNET_NAME:-}"
CERT_TIMEOUT="${CERT_TIMEOUT:-1200}"  # 20 minutes in seconds
CLUSTER_TIMEOUT="${CLUSTER_TIMEOUT:-900}"  # 15 minutes in seconds

# Load .env file if present (local development overrides for admin credentials)
if [[ -f "$PROJECT_ROOT/.env" ]]; then
    set -a; source "$PROJECT_ROOT/.env"; set +a
fi

# Kubeconfig path — set by step1 from the Go bootstrap result contract
KUBECONFIG_PATH=""

# Full-environment teardown (hub + spoke). Kept in its own module because the
# ordering and the CLI work-arounds it encodes are the whole point of it.
# shellcheck source=lib/teardown.sh
source "$SCRIPT_DIR/lib/teardown.sh"

# Boundary activation gating (ADR-055). Content is Git-owned; only the
# activation state is Day-0, and only that is reconciled here.
# shellcheck source=lib/boundary-gating.sh
source "$SCRIPT_DIR/lib/boundary-gating.sh"

# Create log directory
mkdir -p "$LOG_DIR"
# Start fresh log on every run (not just on teardown) so old entries
# from a previous interrupted run don't pollute the current session.
: > "$LOG_DIR/bootstrap.log" 2>/dev/null || true

# Logging function
log() {
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] $1" | tee -a "$LOG_DIR/bootstrap.log"
}

# End-to-end wall clock. The Go bootstrap reports its own per-phase timings, but
# creating a usable cluster also includes everything this script does afterwards
# (tailscale, AWS/GitHub wiring, waiting for Infisical, the database and the
# spoke). This is the number to quote for "how long does a cluster take".
RUN_START_EPOCH=$(date +%s)

format_elapsed() {
    local secs="$1"
    if (( secs < 60 )); then
        printf '%ds' "$secs"
    else
        printf '%dm%02ds' $(( secs / 60 )) $(( secs % 60 ))
    fi
}

report_elapsed() {
    local outcome="$1"
    local secs=$(( $(date +%s) - RUN_START_EPOCH ))
    log ""
    log "⏱  Total elapsed ($outcome): $(format_elapsed "$secs")"
    log "   started $(date -r "$RUN_START_EPOCH" '+%Y-%m-%d %H:%M:%S' 2>/dev/null || date '+%Y-%m-%d %H:%M:%S')  →  ended $(date '+%Y-%m-%d %H:%M:%S')"
}

# Error handling
error_exit() {
    log "ERROR: $1"
    report_elapsed "failed"
    exit 1
}

# Dump CAPI/infrastructure state before failing out of a cluster wait.
#
# Without this a failed run leaves a log full of identical "Infrastructure=False"
# lines and nothing else, so diagnosing it requires a live cluster — which by then
# may have been torn down. The infrastructure object carries the provider's actual
# error (e.g. LoadBalancerCreateFailed with the hcloud API message); the CAPI Cluster
# only reflects it. Best-effort throughout: this runs on a path that is already
# failing and must never itself abort.
dump_cluster_diagnostics() {
    local cluster_name="$1"
    log "── diagnostics: cluster/$cluster_name ─────────────────────────────"
    kubectl get clusters.cluster.x-k8s.io "$cluster_name" -n platform-capi \
        --kubeconfig="$KUBECONFIG_PATH" \
        -o jsonpath='{range .status.conditions[*]}{.type}={.status}  {.reason}  {.message}{"\n"}{end}' 2>&1 | tee -a "$LOG_DIR/bootstrap.log" || true
    log "── diagnostics: infrastructure objects ────────────────────────────"
    kubectl get hetznercluster,kubeadmcontrolplane,machine -n platform-capi \
        --kubeconfig="$KUBECONFIG_PATH" 2>&1 | tee -a "$LOG_DIR/bootstrap.log" || true
    log "───────────────────────────────────────────────────────────────────"
}

# Generic function to check if a step is completed based on bootstrap state file
is_step_completed() {
    local step="$1"
    local BOOTSTRAP_STATE_FILE; BOOTSTRAP_STATE_FILE="$(state_file_for "$step")"

    # Create empty state file if it doesn't exist
    if [[ ! -f "$BOOTSTRAP_STATE_FILE" ]]; then
        mkdir -p "$(dirname "$BOOTSTRAP_STATE_FILE")"
        printf '{"completedSteps": []}' > "$BOOTSTRAP_STATE_FILE"
        return 1
    fi

    if command -v jq >/dev/null 2>&1; then
        # Use any() to check if any step matches, returns true/false with proper exit codes
        jq -e --arg step "$step" '.completedSteps | any(. == $step)' "$BOOTSTRAP_STATE_FILE" >/dev/null
    else
        # Fallback: simple grep check
        grep -q "\"$step\"" "$BOOTSTRAP_STATE_FILE"
    fi
}

# Generic function to mark a step as completed in bootstrap state file
mark_step_completed() {
    local step="$1"
    local BOOTSTRAP_STATE_FILE; BOOTSTRAP_STATE_FILE="$(state_file_for "$step")"

    mkdir -p "$(dirname "$BOOTSTRAP_STATE_FILE")"

    # Read existing completed steps or create new array
    local existing_steps=""
    if [[ -f "$BOOTSTRAP_STATE_FILE" ]]; then
        if command -v jq >/dev/null 2>&1; then
            existing_steps=$(jq -r '.completedSteps[]?' "$BOOTSTRAP_STATE_FILE" 2>/dev/null | tr -d '\r' || echo "")
        else
            # Fallback: parse simple JSON array
            existing_steps=$(grep '"completedSteps":' "$BOOTSTRAP_STATE_FILE" | sed 's/.*"completedSteps": \[\(.*\)\]/\1/' | sed 's/[","]//g' | tr ',' '\n')
        fi
    fi

    # Remove quotes and convert to array
    local steps_array=()
    if [[ -n "$existing_steps" ]]; then
        for existing_step in $existing_steps; do
            if [[ -n "$existing_step" ]]; then
                steps_array+=("$existing_step")
            fi
        done
    fi

    # Add new step if not already present
    local step_already_exists=false
    if [[ ${#steps_array[@]} -gt 0 ]]; then
        for existing_step in "${steps_array[@]}"; do
            if [[ "$existing_step" == "$step" ]]; then
                step_already_exists=true
                break
            fi
        done
    fi

    if [[ "$step_already_exists" == "false" ]]; then
        steps_array+=("$step")
    fi

    # Create JSON array string
    local completed_steps_json="["
    for i in "${!steps_array[@]}"; do
        if [[ $i -gt 0 ]]; then
            completed_steps_json+=","
        fi
        completed_steps_json+="\"${steps_array[$i]}\""
    done
    completed_steps_json+="]"

    # Create or update bootstrap state file (use printf to avoid CRLF on Windows/Git Bash)
    printf '{"completedSteps": %s}' "$completed_steps_json" > "$BOOTSTRAP_STATE_FILE.tmp"
    mv "$BOOTSTRAP_STATE_FILE.tmp" "$BOOTSTRAP_STATE_FILE"

    log "Step '$step' marked as completed in bootstrap state file"
}

# Returns 0 (true) iff Go checkpoint shows preflight completed.
# State-only gate per user request: no hash, no repo drift check.
is_static_preflight_checkpointed() {
    local go_state="$ZERO_OPS_DIR/.state/${CLUSTER_NAME}.json"
    [[ -f "$go_state" ]] || return 1
    if command -v jq >/dev/null 2>&1; then
        jq -e '.completedPhases | index("preflight")' "$go_state" >/dev/null 2>&1
    else
        grep -q '"preflight"' "$go_state" 2>/dev/null
    fi
}

# Auto-install kind and clusterctl into ~/bin on Windows/Git Bash if missing
auto_install_tool() {
    local tool="$1"
    local install_dir="$HOME/bin"
    mkdir -p "$install_dir"

    case "$tool" in
        kind)
            log "  Auto-installing kind..."
            if curl -fsSLo "$install_dir/kind.exe" "https://kind.sigs.k8s.io/dl/latest/kind-windows-amd64" 2>/dev/null; then
                chmod +x "$install_dir/kind.exe"
                export PATH="$PATH:$install_dir"
                log "  ✓ kind installed to $install_dir/kind.exe"
            else
                return 1
            fi
            ;;
        clusterctl)
            log "  Auto-installing clusterctl..."
            if curl -fsSLo "$install_dir/clusterctl.exe" "https://github.com/kubernetes-sigs/cluster-api/releases/download/v1.13.7/clusterctl-windows-amd64.exe" 2>/dev/null; then
                chmod +x "$install_dir/clusterctl.exe"
                export PATH="$PATH:$install_dir"
                log "  ✓ clusterctl installed to $install_dir/clusterctl.exe"
            else
                return 1
            fi
            ;;
        helm)
            log "  Auto-installing helm..."
            local helm_version="v3.17.3"
            local helm_zip="$install_dir/helm.zip"
            if curl -fsSLo "$helm_zip" "https://get.helm.sh/helm-${helm_version}-windows-amd64.zip" 2>/dev/null; then
                unzip -jo "$helm_zip" "windows-amd64/helm.exe" -d "$install_dir" 2>/dev/null
                rm -f "$helm_zip"
                chmod +x "$install_dir/helm.exe"
                export PATH="$PATH:$install_dir"
                log "  ✓ helm installed to $install_dir/helm.exe"
            else
                return 1
            fi
            ;;
        aws)
            log "  Auto-installing aws CLI..."
            # Use winget if aws not already installed via MSI
            local aws_dir="/c/Program Files/Amazon/AWSCLIV2"
            if [[ ! -f "$aws_dir/aws.exe" ]]; then
                if winget install -e --id Amazon.AWSCLI --silent --accept-package-agreements 2>/dev/null; then
                    log "  ✓ aws installed via winget"
                else
                    log "  Attempting AWS CLI MSI install..."
                    local msi="$install_dir/AWSCLIV2.msi"
                    if curl -fsSLo "$msi" "https://awscli.amazonaws.com/AWSCLIV2.msi" 2>/dev/null; then
                        msiexec //i "$msi" //quiet //norestart 2>/dev/null || true
                        rm -f "$msi"
                    fi
                fi
            fi
            # Add the real AWSCLIV2 dir to PATH (aws.exe needs sibling DLLs)
            if [[ -f "$aws_dir/aws.exe" ]]; then
                case ":$PATH:" in
                    *":$aws_dir:"*) ;;
                    *) export PATH="$aws_dir:$PATH" ;;
                esac
                log "  ✓ aws found: $aws_dir/aws.exe"
            else
                return 1
            fi
            ;;
        jq)
            log "  Auto-installing jq..."
            if curl -fsSLo "$install_dir/jq.exe" "https://github.com/jqlang/jq/releases/download/jq-1.7.1/jq-windows-amd64.exe" 2>/dev/null; then
                chmod +x "$install_dir/jq.exe"
                export PATH="$PATH:$install_dir"
                log "  ✓ jq installed to $install_dir/jq.exe"
            else
                return 1
            fi
            ;;
        *)
            return 1
            ;;
    esac

    # Persist to ~/.bashrc if not already there
    local bashrc="$HOME/.bashrc"
    # For aws, use the Program Files dir instead of ~/bin
    local persist_dir="$install_dir"
    if [[ "$tool" == "aws" ]]; then
        persist_dir="/c/Program Files/Amazon/AWSCLIV2"
    fi
    local path_line="export PATH=\"\$PATH:$persist_dir\""
    if ! grep -qF "$persist_dir" "$bashrc" 2>/dev/null; then
        echo -e "\n# auto-installed tools\n$path_line" >> "$bashrc"
    fi
}

# Check prerequisites
check_prerequisites() {
    log "Checking prerequisites..."
    local failed=0

    # --- Required CLI tools ---
    for tool in kubectl aws jq kind clusterctl helm; do
        # Also check ~/bin since auto-installed tools live there but
        # non-interactive shells don't source ~/.bashrc (where PATH is persisted)
        if [[ -x "$HOME/bin/$tool" ]]; then
            export PATH="$HOME/bin:$PATH"
        fi
        if ! command -v "$tool" >/dev/null 2>&1; then
            case "$tool" in
                kind|clusterctl|helm|aws|jq)
                    log "  '$tool' not found — attempting auto-install..."
                    if auto_install_tool "$tool" && command -v "$tool" >/dev/null 2>&1; then
                        log "  ✓ $tool found: $(command -v "$tool")"
                    else
                        log "ERROR: '$tool' could not be auto-installed"
                        case "$tool" in
                            kind)
                                log "  Install (Windows): winget install -e --id Kubernetes.kind"
                                log "  Or download: https://kind.sigs.k8s.io/dl/latest/kind-windows-amd64"
                                log "  Docs: https://kind.sigs.k8s.io/docs/user/quick-start/#installation"
                                ;;
                            clusterctl)
                                log "  Install (Windows): curl -Lo clusterctl.exe https://github.com/kubernetes-sigs/cluster-api/releases/download/v1.13.7/clusterctl-windows-amd64.exe"
                                log "  Then add to PATH"
                                log "  Docs: https://cluster-api.sigs.k8s.io/user/quick-start#install-clusterctl"
                                ;;
                            helm)
                                log "  Install (Windows): winget install -e --id Helm.Helm"
                                log "  Or download: https://get.helm.sh/helm-v3.17.3-windows-amd64.zip"
                                log "  Docs: https://helm.sh/docs/intro/install/"
                                ;;
                            aws)
                                log "  Install (Windows): https://awscli.amazonaws.com/AWSCLIV2.msi"
                                log "  Or: winget install -e --id Amazon.AWSCLI"
                                log "  Docs: https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html"
                                ;;
                            jq)
                                log "  Install (Git Bash): curl -L -o ~/jq.exe https://github.com/jqlang/jq/releases/download/jq-1.7.1/jq-windows-amd64.exe && chmod +x ~/jq.exe && export PATH=\$PATH:~"
                                log "  Or (Windows): winget install -e --id jqlang.jq"
                                log "  Docs: https://jqlang.github.io/jq/download/"
                                ;;
                        esac
                        failed=1
                    fi
                    ;;
                *)
                    log "ERROR: '$tool' is not installed or not in PATH"
                    case "$tool" in
                        kubectl)
                            log "  Install (Windows): winget install -e --id Kubernetes.kubectl"
                            log "  Or download: https://dl.k8s.io/release/v1.30.0/bin/windows/amd64/kubectl.exe"
                            log "  Docs: https://kubernetes.io/docs/tasks/tools/install-kubectl-windows/"
                            ;;
                    esac
                    failed=1
                    ;;
            esac
        else
            log "  ✓ $tool found: $(command -v "$tool")"
        fi
    done

    # --- Docker (required for kind) ---
    local docker_dirs=(
        "/c/Program Files/Docker/Docker/resources/bin"
        "/c/Program Files/Docker/Docker"
        "$HOME/bin"
    )
    if ! command -v docker >/dev/null 2>&1; then
        for d in "${docker_dirs[@]}"; do
            if [[ -f "$d/docker.exe" ]]; then
                export PATH="$d:$PATH"
                log "  ✓ docker found: $d/docker.exe"
                break
            fi
        done
    fi
    if ! command -v docker >/dev/null 2>&1; then
        log "ERROR: 'docker' not found"
        log "  Install Docker Desktop: https://docs.docker.com/desktop/setup/install/windows-install/"
        log "  Or: winget install -e --id Docker.DockerDesktop"
        log "  Then start Docker Desktop and re-run."
        failed=1
    else
        # The kind bootstrap cluster may run on a REMOTE docker context
        # (ssh:// or tcp://), e.g. a home-worker Windows laptop acting as the
        # docker host. In that case the local daemon is irrelevant — only the
        # active context's endpoint needs to be reachable. Do NOT try to launch
        # local Docker Desktop for a remote context.
        local active_ctx docker_endpoint is_remote
        active_ctx="$(docker context show 2>/dev/null || echo default)"
        docker_endpoint="$(docker context inspect "$active_ctx" --format '{{.Endpoints.docker.Host}}' 2>/dev/null || echo '')"
        is_remote=0
        if [[ -n "$docker_endpoint" && "$docker_endpoint" != unix://* ]]; then
            is_remote=1
        fi

        if (( is_remote )); then
            if docker info >/dev/null 2>&1; then
                log "  ✓ docker context '$active_ctx' reachable ($docker_endpoint)"
            else
                log "ERROR: docker context '$active_ctx' ($docker_endpoint) is unreachable."
                log "  The kind bootstrap cluster is created on this remote docker host."
                log "  Power the host on / fix the ssh connection, then re-run."
                failed=1
            fi
        elif ! docker info >/dev/null 2>&1; then
            log "WARNING: Docker is installed but no daemon is reachable."
            # kind runs the CAPI bootstrap cluster locally before the pivot, so a
            # container runtime is required here — this is unrelated to image
            # builds, which happen in GitHub workflows.
            #
            # Launching is platform-specific. The previous code only knew how to
            # start "Docker Desktop.exe", so on macOS and Linux it announced that
            # it was starting Docker and then did nothing.
            local started=0
            case "$(uname -s)" in
                Darwin)
                    if [[ -d "/Applications/Docker.app" ]]; then
                        log "  Starting Docker Desktop (macOS)..."
                        open -a Docker >/dev/null 2>&1 && started=1
                    elif command -v colima >/dev/null 2>&1; then
                        log "  Starting Colima..."
                        colima start >/dev/null 2>&1 && started=1
                    elif [[ -d "/Applications/OrbStack.app" ]]; then
                        log "  Starting OrbStack..."
                        open -a OrbStack >/dev/null 2>&1 && started=1
                    fi
                    ;;
                Linux)
                    if command -v systemctl >/dev/null 2>&1; then
                        log "  Starting the docker service..."
                        sudo systemctl start docker >/dev/null 2>&1 && started=1
                    fi
                    ;;
                *)
                    local docker_dir desktop_exe
                    docker_dir="$(dirname "$(command -v docker)")"
                    desktop_exe="${docker_dir}/../Docker Desktop.exe"
                    if [[ -f "$desktop_exe" ]]; then
                        log "  Starting Docker Desktop (Windows)..."
                        "$desktop_exe" &>/dev/null & started=1
                    fi
                    ;;
            esac

            if (( started )); then
                log "  Waiting up to 90s for the daemon to accept connections..."
                local waited=0
                while (( waited < 90 )); do
                    if docker info >/dev/null 2>&1; then break; fi
                    sleep 5; waited=$(( waited + 5 ))
                done
                if docker info >/dev/null 2>&1; then
                    log "  ✓ Docker daemon is now running (after ${waited}s)"
                else
                    log "ERROR: the runtime was launched but the daemon is still unreachable after ${waited}s."
                    failed=1
                fi
            else
                log "ERROR: no container runtime is available on this machine."
                log "  'docker' here is only the CLI — there is no daemon behind it."
                log "  The bootstrap needs one because kind hosts the CAPI bootstrap"
                log "  cluster locally before pivoting to the Hetzner hub."
                log "  Install any one of these, then re-run:"
                log "    brew install --cask docker        # Docker Desktop"
                log "    brew install colima && colima start --cpu 4 --memory 8"
                log "    brew install --cask orbstack"
                log "  Or point DOCKER_HOST / 'docker context use' at a remote daemon;"
                log "  a remote context is detected and used as-is."
                failed=1
            fi
        else
            log "  ✓ docker daemon running"
        fi
    fi

    # --- Hub binary ---
    #
    # Resolved here rather than at the top of the file: resolve_owned needs both
    # roots settled, and ZERO_OPS_DIR is not final until the flags are parsed.
    if [[ -z "$KAKT_BINARY" ]]; then
        KAKT_BINARY="$(resolve_owned bin/kakt)"
    fi
    #
    # A binary that already exists and reports a version is used as-is.
    #
    # This is the difference between a developer checkout and a released run. A
    # released run downloaded a PINNED binary for the bundle version it was asked
    # for, and there is no source tree beside it; rebuilding would either fail
    # for want of a toolchain or -- worse, where a toolchain exists -- silently
    # replace the pinned artefact with a build of whatever source happened to be
    # present. The version the caller chose is the one thing a release must not
    # discard.
    #
    # KAKT_BUILD=always forces a rebuild for the case a developer wants one
    # despite an existing binary.
    # Skip the build, NOT the rest of this function. check_prerequisites keeps
    # checking tokens and the AWS profile after this block, and returning early
    # would silently drop those -- the same class of bug as a gate that exists
    # and is never called.
    local skip_build=""
    if [[ "${KAKT_BUILD:-}" != "always" && -x "$KAKT_BINARY" ]]; then
        local existing_version
        existing_version="$("$KAKT_BINARY" bundle-version 2>/dev/null || true)"
        if [[ -n "$existing_version" ]]; then
            log "Using existing binary $KAKT_BINARY (bundle ${existing_version})"
            # A binary reporting a version other than the one requested is the
            # drift this check exists for.
            if [[ -n "$BUNDLE_VERSION" && "$existing_version" != "$BUNDLE_VERSION" ]]; then
                log "ERROR: binary at $KAKT_BINARY reports bundle '${existing_version}', expected '${BUNDLE_VERSION}'"
                log "       Set KAKT_BUILD=always to rebuild, or point KAKT_BINARY at the right one."
                failed=1
            fi
            skip_build=1
        else
            log "Existing binary $KAKT_BINARY reports no version; rebuilding"
        fi
    fi

    if [[ -z "$skip_build" && ! -d "$(resolve_owned cmd/kakt)" ]]; then
        log "ERROR: no usable kakt binary, and no source to build one"
        log "       (no cmd/kakt under $ZERO_OPS_DIR or $PLATFORM_DIR). Set KAKT_BINARY to a prebuilt CLI."
        failed=1
        skip_build=1
    fi

    if [[ -z "$skip_build" ]]; then
    # The version is injected the same way the release workflow injects it, so
    # a run against a published bundle is not a different build path -- it is
    # this build path with the version set.
    local ldflags=""
    if [[ -n "$BUNDLE_VERSION" ]]; then
        ldflags="-X github.com/kamarkat/kamarkat/internal/kakt-cli/versions.BundleVersion=${BUNDLE_VERSION}"
        log "Building hub binary for bundle ${BUNDLE_VERSION}..."
    else
        log "Building hub binary..."
    fi
    if (cd "$(dirname "$(resolve_owned cmd/kakt)")" && go build -mod=mod -ldflags "$ldflags" -o "$KAKT_BINARY" ./cmd/kakt 2>&1); then
        log "  ✓ hub binary built"
        if [[ -n "$BUNDLE_VERSION" ]]; then
            # The binary must request what was asked for. A build that still
            # reported "development" would silently source from the working tree
            # on every cluster it bootstrapped, which is the failure this flag
            # exists to avoid rather than introduce.
            local got
            got=$("$KAKT_BINARY" bundle-version 2>/dev/null || true)
            if [[ "$got" != "$BUNDLE_VERSION" ]]; then
                log "ERROR: built CLI reports '${got}', expected '${BUNDLE_VERSION}'"
                failed=1
            else
                log "  ✓ CLI reports bundle ${got}"
            fi
        fi
    else
        log "ERROR: Hub binary build failed"
        failed=1
    fi
    fi

    # --- Secret files ---
    if [[ "$PROVIDER" == "hetzner" || "$PROVIDER" == "hybrid" ]]; then
        if [[ ! -f "$(resolve_owned k8-secrets/hetzner/token)" ]]; then
            log "ERROR: Hetzner token file not found at k8-secrets/hetzner/token"
            failed=1
        else
            log "  ✓ k8-secrets/hetzner/token found"
        fi
    fi

    if [[ ! -f "$(resolve_owned k8-secrets/github/github-pat-token)" ]]; then
        log "ERROR: GitHub token file not found at k8-secrets/github/github-pat-token"
        failed=1
    else
        log "  ✓ k8-secrets/github/github-pat-token found"
    fi

    # --- AWS profile ---
    if ! command -v aws >/dev/null 2>&1; then
        : # already reported above
    elif ! aws configure list --profile zerotouch-platform-admin >/dev/null 2>&1; then
        log "ERROR: AWS profile 'zerotouch-platform-admin' not configured"
        log "  Fix: aws configure --profile zerotouch-platform-admin"
        failed=1
    else
        log "  ✓ AWS profile 'zerotouch-platform-admin' configured"
    fi

    if [[ $failed -ne 0 ]]; then
        error_exit "Prerequisites check failed. Fix the errors above and re-run."
    fi

    log "Prerequisites check passed"
}

# Read the kubeconfig path from the Go bootstrap state file (the result contract).
# Called after hub bootstrap completes to set KUBECONFIG_PATH for all downstream steps.
read_kubeconfig_from_state() {
    # ONE source: the state file the CLI writes, at .state/<cluster>.json
    # in the workspace (ADR-072 puts it in the TENANT's repository, which is what
    # ZERO_OPS_DIR names).
    #
    # There were three sources before -- this path, a legacy .zero-ops/state one,
    # and a convention-based guess at k8-secrets/kubeconfig/<cluster>.kubeconfig
    # if neither answered. The guess is what made the rest untestable: it is
    # usually right, so a state file that was missing or held a different path
    # produced a working run against a kubeconfig nobody had chosen, and the two
    # could not be told apart. A box whose state does not record its kubeconfig
    # is a box this cannot act on, and it now says so.
    local go_state_file="$ZERO_OPS_DIR/.state/${CLUSTER_NAME}.json"
    if [[ ! -f "$go_state_file" ]]; then
        error_exit "no bootstrap state at $go_state_file — the management cluster has not been bootstrapped from this workspace."
    fi

    KUBECONFIG_PATH=$(jq -r '.mgmtKubeconfig // ""' "$go_state_file" 2>/dev/null || echo "")
    if [[ -z "$KUBECONFIG_PATH" ]]; then
        error_exit "$go_state_file records no mgmtKubeconfig — bootstrap did not finish writing it."
    fi

    # The state file records the path as the CLI saw it, which is relative when
    # the CLI ran with --gitops-dir . from inside the tenant's repository. Resolve
    # it against the workspace rather than the caller's cwd, which is arbitrary.
    if [[ "$KUBECONFIG_PATH" != /* ]]; then
        KUBECONFIG_PATH="$ZERO_OPS_DIR/$KUBECONFIG_PATH"
    fi
    if [[ ! -r "$KUBECONFIG_PATH" ]]; then
        error_exit "state names $KUBECONFIG_PATH as the kubeconfig, and it is not readable."
    fi

    export KUBECONFIG_PATH
    log "Kubeconfig path: $KUBECONFIG_PATH"
}

# Resolve a credential the operator owns.
#
# The ENVIRONMENT WINS. Both callers that matter already set these: the tenant
# workflow passes them to the bootstrap step as env, and local-e2e exports them
# from the platform checkout. The file is for someone running this script by hand.
#
# Each of these used to be `export X=$(cat "$ZERO_OPS_DIR/...")`, which is the
# opposite precedence and silently destructive: `cat` on a missing file prints
# nothing, `export` masks its exit code, and a correctly supplied credential was
# replaced with an empty string. The tenant's repository has no k8-secrets/ --
# it is the OPERATOR's directory, and gitignored in a tenant repo precisely so it
# never appears there -- so the file was always absent on the tenant path and the
# clobber always fired. It surfaced as "--ghcr-pat is required" from a command
# invoked with a credential the caller had provided correctly.
#
# Refuses rather than proceeding empty: every step below treats the value as
# present, and an empty credential fails several commands later with an error
# naming none of this.
require_credential() {
    local var="$1" rel="$2" what="$3"
    if [[ -n "${!var:-}" ]]; then
        return 0
    fi
    local path; path="$(resolve_owned "$rel")"
    if [[ -r "$path" ]]; then
        printf -v "$var" '%s' "$(tr -d '\r\n' < "$path")"
        export "${var?}"
        [[ -n "${!var:-}" ]] && return 0
        error_exit "$path is empty; it must hold $what."
    fi
    error_exit "$var is not set and $path does not exist. Export $var, or write $what there."
}

# Step 1: Bootstrap Hub Cluster
step1_bootstrap_hub() {
    # Check if step is already completed AND the Go bootstrap state confirms postboot finished
    local go_state_file="$ZERO_OPS_DIR/.state/${CLUSTER_NAME}.json"
    # Handle teardown-on-bootstrap: clean up existing cluster before starting
    # State files are removed unconditionally to prevent stale state from
    # skipping the bootstrap on re-run. If teardown fails (cluster gone etc.),
    # the fresh bootstrap still proceeds from scratch.
    if [[ "$TEARDOWN" == "true" ]]; then
        log "TEARDOWN=true — tearing down existing cluster '${CLUSTER_NAME}' (forceful)..."
        "$KAKT_BINARY" teardown --name="${CLUSTER_NAME}" --confirm --force 2>&1 || log "WARNING: Teardown returned non-zero — cluster may not exist, continuing..."

        log "Pre-flight: cleaning up any leftover kind clusters (bootstrap kind, prior runs)..."
        # hub teardown only knows about the named hub cluster. The kind
        # bootstrap cluster (and any stale pre-pivot failures) can leave
        # kind clusters with bound host ports behind. The fix: delete
        # every kind cluster on the host, not just the named one.
        if command -v kind >/dev/null 2>&1; then
            for kc in $(kind get clusters 2>/dev/null); do
                log "  Deleting stale kind cluster: $kc"
                kind delete cluster --name "$kc" 2>&1 | tail -1 || true
            done
        fi

        log "Pre-flight: removing stale 'kind' Docker network (if any)..."
        # The kind network can outlive its containers on abnormal exits
        # and then refuse to re-bind the same IPAM range, blocking the
        # next `kind create`. Removing it forces a clean re-create.
        if docker network ls --format '{{.Name}}' 2>/dev/null | grep -qx 'kind'; then
            docker network rm kind 2>&1 | tail -1 || log "  (could not remove kind network — likely still in use)"
        fi

        log "Cleanup: removing stale bootstrap state and logs..."
        rm -f "$MGMT_STATE_FILE" "$WORKLOAD_STATE_FILE"
        rm -f "$go_state_file"
        rm -f "$LOG_DIR/bootstrap.log"
        rm -f "$LOG_DIR/bootstrap-hub.log"
        rm -f "$LOG_DIR/init-secrets.log"
        rm -f "$LOG_DIR/infisical-bootstrap.json"
        rm -f "$ZERO_OPS_DIR/.state/kind/kind-config-generated.yaml"
        log "✓ Bootstrap state, logs, and stale kind resources reset for fresh start"
        sleep 10  # let the smoke clear
    fi

    if is_step_completed "bootstrap_hub"; then
        # Verify the Go bootstrap actually completed all phases (incl. postboot)
		if [[ -f "$go_state_file" ]] && grep -q '"complete"' "$go_state_file"; then
            log "Step 1: Hub bootstrap already completed (postboot confirmed), skipping"
            read_kubeconfig_from_state
            return
        fi
        log "Step 1: Bash state says done but Go bootstrap postboot not found — re-running hub bootstrap to resume"
    fi

    # Source of truth check: the Go state file at state/<name>.json is the
    # single authoritative record of the bootstrap phase. If it shows the
    # bootstrap is complete, skip — even when the bash state cache is empty
    # (e.g. after a direct `hub bootstrap` CLI run, or a stale cache).
    if [[ -f "$go_state_file" ]] && grep -q '"complete"' "$go_state_file"; then
        log "Step 1: Hub bootstrap already completed (state file: $go_state_file), skipping"
        read_kubeconfig_from_state
        # Mirror to bash state so downstream steps see consistent step tracking
        mark_step_completed "bootstrap_hub"
        return
    fi

    log "Step 1: Bootstrapping Hub Cluster..."

    # Pre-flight: detect and recover from a stale kind API-server port
    # bind. With hostPort 6443 pinned in kind-config.yaml, this should
    # not happen, but stale containers from aborted runs (e.g. a
    # previous `hub bootstrap` killed mid-kind-create) can still hold
    # 6443. Detect early and fail with a clear message + fix instead
    # of letting kind spew "address already in use".
    if command -v docker >/dev/null 2>&1; then
        if docker ps -a --format '{{.Ports}}' 2>/dev/null | grep -q '0.0.0.0:6443\|127.0.0.1:6443'; then
            log "⚠️  Detected stale container bound to host port 6443. Cleaning up..."
            # Find and remove the offending container (not the running
            # hub-local one — only the stale ones, by exclusion of name).
            for cid in $(docker ps -a --filter publish=6443 --format '{{.ID}}' 2>/dev/null); do
                cname=$(docker inspect --format '{{.Name}}' "$cid" 2>/dev/null | tr -d '/')
                if [[ "$cname" != "${CLUSTER_NAME}-control-plane" ]]; then
                    log "  Removing stale container: $cname ($cid)"
                    docker rm -f "$cid" >/dev/null 2>&1 || true
                fi
            done
        fi
    fi

    require_credential HCLOUD_TOKEN "k8-secrets/hetzner/token" "the Hetzner API token"
    local env_flag=""
    if [[ -n "${ENVIRONMENT:-}" ]]; then
        env_flag="--environment=${ENVIRONMENT}"
    else
        env_flag="--environment=prod"
    fi
    local gating_flag="--gating=${GATING}"
    local gitops_flag=""
    [[ -n "$GITOPS_DIR" ]] && gitops_flag="--gitops-dir=${GITOPS_DIR}"
    local topo_flag=""
    # Topology is a matrix dimension (ADR-037) but hybrid claims live flat under
    # spoke-pools/{env}/{provider}. Pass an explicit empty topology so the Go CLI
    # default of "single" does not append a /single segment to the claim path.
    if [[ "$PROVIDER" == "hybrid" ]]; then
        topo_flag="--topology="
    elif [[ -n "${TOPOLOGY:-}" ]]; then
        topo_flag="--topology=$TOPOLOGY"
    fi

    if [[ "$PROVIDER" == "hybrid" ]]; then
        # --on-prem, not --home-worker-enabled. The CLI names this flag
        # --on-prem (ADR-075: nodes on the tenant's own premises), and passing a
        # flag it does not define makes it exit on the argument rather than
        # bootstrap. HOME_WORKER_TTL has no CLI flag at all and is read from the
        # SpokePool annotation instead, so it is not forwarded here.
        local hybrid_flags=""
        if [[ -n "${HOME_WORKER_ENABLED:-}" ]]; then
            hybrid_flags="$hybrid_flags --on-prem"
        fi
        if [[ -n "${TAILNET_NAME:-}" ]]; then
            hybrid_flags="$hybrid_flags --tailnet-name=$TAILNET_NAME"
        fi
        log "Running: $KAKT_BINARY bootstrap-mgmt --name=${CLUSTER_NAME} --provider=hybrid --region=${REGION} $env_flag $topo_flag $gating_flag $hybrid_flags --debug"
        (cd "$ZERO_OPS_DIR" && "$KAKT_BINARY" bootstrap-mgmt \
            --name="${CLUSTER_NAME}" \
            --provider=hybrid \
            --region="${REGION}" \
            $env_flag \
            $topo_flag \
            $gating_flag $gitops_flag \
            $hybrid_flags \
            --debug 2>&1 | tee "$LOG_DIR/bootstrap-hub.log")
    else
        log "Running: $KAKT_BINARY bootstrap-mgmt --name=${CLUSTER_NAME} --region=${REGION} $env_flag $gating_flag --debug"
        (cd "$ZERO_OPS_DIR" && "$KAKT_BINARY" bootstrap-mgmt \
            --name="${CLUSTER_NAME}" \
            --region="${REGION}" \
            $env_flag \
            $topo_flag \
            $gating_flag $gitops_flag \
            --debug 2>&1 | tee "$LOG_DIR/bootstrap-hub.log")
    fi

    # Read the result contract produced by the Go bootstrap
    read_kubeconfig_from_state

    mark_step_completed "bootstrap_hub"
    log "Hub cluster bootstrap completed"
}

# Step 1c: Configure Tailscale credentials (hybrid only)
# Creates platform-capi/tailscale-hybrid-psk from k8-secrets/tailscale/*
# (gitignored), mirroring the hetzner/github Secret Zero pattern. hub-operator
# uploads it to Infisical via CLISecretMappings and ESO syncs it back, so
# ArgoCD never resets it to empty. Required before a hybrid spoke provisions
# (the ClusterClass pre-kubeadm hook reads this Secret).
step1c_configure_tailscale() {
    # Only relevant for the hybrid provider cell.
    if [[ "$PROVIDER" != "hybrid" ]]; then
        return
    fi

    if is_step_completed "configure_tailscale"; then
        if kubectl get secret -n platform-capi tailscale-hybrid-psk \
            --kubeconfig="$KUBECONFIG_PATH" >/dev/null 2>&1; then
            log "Step 1c: Tailscale credentials already configured, skipping"
            return
        fi
        log "Step 1c: State says completed but tailscale-hybrid-psk missing — re-running"
    fi

    log "Step 1c: Configuring Tailscale credentials..."

    # Mandatory on hybrid, not optional. On-prem nodes reach the control plane
    # over the tailnet (ADR-046 §21, invariant 6), so a hybrid box without this
    # is not a box with a missing extra -- it is one whose pod traffic to its own
    # workers dies in one direction.
    #
    # This looked only in ZERO_OPS_DIR and WARNED. ZERO_OPS_DIR is the TENANT's
    # checkout, and k8-secrets/ is the platform operator's own gitignored
    # directory, so the file was never there on the tenant path and the step
    # always skipped. readTailscaleAuthkey in driver_hybrid.go carries the same
    # fix and records what the warning cost: a control plane that never joined
    # the tailnet, discovered when traffic to the home workers died.
    #
    # Same precedence as the Go driver -- environment first, the operator's file
    # second -- so both halves of a hybrid bootstrap enrol from one credential.
    require_credential TS_AUTHKEY "k8-secrets/tailscale/authkey" "a Tailscale auth key"
    # DERIVED from the SpokePool, not a credential. It is the control plane's
    # MagicDNS name, which is definitionally "<pool>-cp" -- the operator's
    # k8-secrets/tailscale/hostname held exactly that. Deriving it means a tenant
    # needs no secret for a value that is already determined by the pool they
    # declared, and it cannot drift from the pool the way a second copy can.
    TS_HOSTNAME="${TS_HOSTNAME:-${SPOKEPOOL_NAME}-cp}"
    if [[ -z "${SPOKEPOOL_NAME:-}" ]]; then
        error_exit "cannot derive the control plane's tailnet hostname: no SpokePool is named."
    fi
    export TS_HOSTNAME

    # Passed explicitly. The CLI's own defaults read k8-secrets/ relative to its
    # WORKING DIRECTORY, which here is the tenant's checkout -- so resolving the
    # paths above and then letting it look them up again would find nothing.
    "$KAKT_BINARY" configure-tailscale \
        --authkey="$TS_AUTHKEY" \
        --hostname="$TS_HOSTNAME" \
        --kubeconfig="$KUBECONFIG_PATH" || error_exit "configure-tailscale failed"

    # No tailscale-node-authkey Secret is created here any more. It fed a
    # tailscale-node DaemonSet that has been removed: the hub control plane runs
    # tailscaled NATIVELY from the ClusterClass (ADR-046 §21), and a second daemon
    # sharing the host netns stripped the tailnet addresses off tailscale0 — see
    # ADR-046 §22. The authkey the ClusterClass needs travels in
    # platform-capi/tailscale-hybrid-psk, created by configure-tailscale above.

    mark_step_completed "configure_tailscale"
    log "Tailscale credentials configuration completed"
}

# Step 3: Configure GitHub Access
# The organisation this box's credential is scoped to.
#
# Read from the repository Day-0 runs inside (ADR-072), which is the same source
# and the same normalisation boxOrgURL uses in the orchestrator -- an SSH remote
# becomes the https shape, because that is what ArgoCD matches a source against.
#
# Not defaulted. configure-github-access requires it for the reason recorded on
# the flag: a default would be the PLATFORM's organisation, which scopes a
# tenant's credential to repositories they do not own and away from the one they
# do (ADR-062).
box_org_url() {
    local remote
    remote="$(git -C "$ZERO_OPS_DIR" remote get-url origin 2>/dev/null || true)"
    [[ -n "$remote" ]] || error_exit "$ZERO_OPS_DIR has no origin remote, so this box cannot name the organisation its credential is scoped to."

    remote="${remote%.git}"
    if [[ "$remote" == git@* ]]; then
        remote="https://${remote#git@}"
        remote="${remote/:/\/}"
    fi

    local org; org="$(printf '%s' "$remote" | cut -d/ -f1-4)"
    [[ "$(printf '%s' "$org" | tr -cd '/' | wc -c)" -eq 3 ]] \
        || error_exit "cannot read an organisation out of '$remote': expected https://host/org/repo."
    printf '%s' "$org"
}

step3_configure_github() {
    if is_step_completed "configure_github"; then
        # Verify the artifact actually exists — the cluster may have been
        # recreated since the state was saved (e.g. teardown + re-bootstrap).
        if kubectl get secret -n platform-ops ghcr-pull-secret \
            --kubeconfig="$KUBECONFIG_PATH" >/dev/null 2>&1; then
            log "Step 3: GitHub access already configured, skipping"
            return
        fi
        log "Step 3: State says completed but ghcr-pull-secret missing — re-running"
    fi

    log "Step 3: Configuring GitHub Access..."

    require_credential GITHUB_TOKEN "k8-secrets/github/github-pat-token" "a GitHub PAT"

    local org_url; org_url="$(box_org_url)"

    log "Running: $KAKT_BINARY configure-github-access --ghcr-pat=\$GITHUB_TOKEN --org-url=$org_url"
    "$KAKT_BINARY" configure-github-access \
        --ghcr-pat="$GITHUB_TOKEN" \
        --org-url="$org_url" \
        --kubeconfig="$KUBECONFIG_PATH" || error_exit "configure-github-access failed"

    mark_step_completed "configure_github"
    log "GitHub access configuration completed"

    # Restart hub-operator so it picks up the new ghcr-pull-secret.
    # The pod was created before the secret existed and won't retry on its own.
    log "Restarting hub-operator to pick up ghcr-pull-secret..."
    kubectl rollout restart deployment/hub-operator -n platform-ops \
        --kubeconfig="$KUBECONFIG_PATH" 2>/dev/null || \
        log "WARNING: Could not restart hub-operator — may need manual restart"
}

# Step 4: Wait for ArgoCD to sync and create namespaces
step4_wait_namespaces() {
    if is_step_completed "wait_namespaces"; then
        # Verify the namespace actually exists — stale state from a previous
        # cluster may have marked this done prematurely.
        if kubectl get namespace platform-data \
            --kubeconfig="$KUBECONFIG_PATH" >/dev/null 2>&1; then
            log "Step 4: Namespaces already created, skipping"
            return
        fi
        log "Step 4: State says completed but platform-data namespace missing — re-running"
    fi

    log "Step 4: Waiting for ArgoCD to sync and create namespaces (timeout: 600s)..."

    # Correct polling logic for a resource that doesn't exist yet
    # kubectl wait fails if namespace doesn't exist, so we must poll first
    timeout=600
    elapsed=0
    while ! kubectl get namespace platform-data --kubeconfig="$KUBECONFIG_PATH" >/dev/null 2>&1; do
        if [ $elapsed -ge $timeout ]; then
            log "❌ Timeout waiting for platform-data namespace to be created by ArgoCD"
            error_exit "ArgoCD failed to create platform-data namespace within ${timeout}s"
        fi
        sleep 5
        elapsed=$((elapsed+5))
    done

    # Now wait for it to be active since we know it exists
    log "Running: kubectl wait --for=jsonpath='{.status.phase}'=Active namespace/platform-data --timeout=60s"
    kubectl wait --for=jsonpath='{.status.phase}'=Active namespace/platform-data \
        --timeout=60s \
        --kubeconfig="$KUBECONFIG_PATH"

    mark_step_completed "wait_namespaces"
    log "✅ platform-data namespace created by ArgoCD"
}

# Step 5: Verify bootstrap secrets (handled internally by hub bootstrap orchestrator)
step5_verify_secrets() {
    if is_step_completed "verify_secrets"; then
        log "Step 5: Bootstrap secrets already verified, skipping"
        return
    fi

    log "Step 5: Verifying bootstrap secrets (initialized by hub bootstrap orchestrator)..."

    # The init-secrets pipeline now runs as an internal Go function inside the
    # hub bootstrap orchestrator (between Boundary 02 and Boundary 03). This
    # step is a lightweight verification that the infisical-auth Secret was
    # created and Infisical is operational.

    local max_attempts=60
    local attempt=1

    while [[ $attempt -le $max_attempts ]]; do
        local infisical_auth_data
        infisical_auth_data=$(kubectl get secret infisical-auth -n platform-ops \
            -o jsonpath='{.data.client-id}{" "}{.data.client-secret}' \
            --kubeconfig="$KUBECONFIG_PATH" 2>/dev/null || echo "")

        if [[ -n "$infisical_auth_data" ]]; then
            log "✓ infisical-auth Secret verified"
            break
        fi

        log "Waiting for infisical-auth secret (attempt $attempt/$max_attempts)..."
        sleep 10
        ((attempt++))
    done

    if [[ $attempt -gt $max_attempts ]]; then
        log "❌ infisical-auth Secret not found after bootstrap"
        log "   The orchestrator's init-secrets phase may have failed."
        log "   Check bootstrap logs and re-run: hub bootstrap"
        return 1
    fi

    mark_step_completed "verify_secrets"
    log "Bootstrap secrets verification completed"
}

# Step 6: Wait for Infisical to be ready
step6_wait_infisical() {
    if is_step_completed "wait_infisical"; then
        log "Step 6: Infisical already ready, skipping"
        return
    fi

    log "Step 6: Waiting for Infisical to be ready..."

    # Wait for Infisical pods to be ready (using same logic as Step 5)
    log "Checking Infisical pods status..."
    local max_attempts=180  # 30 minutes to match Step 5
    local attempt=1

    while [[ $attempt -le $max_attempts ]]; do
        local pod_ready
        pod_ready=$(kubectl get pods -n platform-security \
            --kubeconfig="$KUBECONFIG_PATH" \
            -l app=infisical-standalone \
            -o jsonpath='{.items[0].status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || echo "NotFound")

        if [[ "$pod_ready" == "True" ]]; then
            log "Infisical is ready"
            break
        fi

        log "Waiting for Infisical to be ready (attempt $attempt/$max_attempts, ready: $pod_ready)"
        sleep 10
        ((attempt++))
    done

    if [[ $attempt -gt $max_attempts ]]; then
        error_exit "Infisical did not become ready within expected time"
    fi

    mark_step_completed "wait_infisical"
    log "Infisical pods are running"
}

# Step 6b: Seed externally-issued credentials into Infisical
#
# Some credentials are issued by systems OUTSIDE this platform — object storage
# keys, IRM alert URLs — so nothing in the cluster can generate them.
# They used to be seeded by hand into the Infisical console, which meant a rebuilt
# Infisical silently came up without them: the ExternalSecret said only "could not
# get secret data from provider", the consuming pod said only
# CreateContainerConfigError, and neither named the missing key or its owner.
#
# This step turns that folklore into a declared, repeatable phase. Each entry below
# maps a directory under k8-secrets/ (gitignored) to one Kubernetes Secret; the
# hub-operator's CLISecretMappings then uploads it to Infisical and ESO syncs it
# back out — the same Secret Zero path already used for hetzner, github and
# tailscale. Nothing new writes to Infisical directly.
#
# Placeholder files are created for anything missing so the required set is
# discoverable from the filesystem rather than from documentation. A key that is
# still empty is NOT turned into a Secret: the uploader rejects empty values, and a
# half-populated Secret is worse than an absent one.
#
# Format: <k8-secrets subdir>|<target namespace>/<secret name>|<file>=<secret key>,...
# subdir | namespace/secret | file=key=ENV_VAR,...
#
# The environment variable is declared beside the file because both are real
# sources and they must not drift: these are the same credentials, with the same
# names, that secrets_platform.go reads env-first. This step read the FILE only,
# under the tenant's checkout -- where k8-secrets/ does not exist -- so on the
# tenant path it wrote empty placeholders into the tenant's repository and seeded
# nothing, while the values sat in the environment the whole time.
SEED_SPECS=(
    "s3|platform-ops/s3-object-storage|access-key-id=access-key-id=S3_ACCESS_KEY_ID,secret-access-key=secret-access-key=S3_SECRET_ACCESS_KEY"
    "grafana-cloud|platform-ops/grafana-cloud|irm-alertmanager-url=irm-alertmanager-url=GRAFANA_IRM_ALERTMANAGER_URL,irm-heartbeat-url=irm-heartbeat-url=GRAFANA_IRM_HEARTBEAT_URL"
    # The issuer's outbound mail: the owner's Resend SMTP URL,
    # smtps://resend:<api-key>@smtp.resend.com:465. hub-operator uploads only its
    # password, to /<tenant>-mgmt, where zitadel-smtp-config reads it.
    "smtp|platform-ops/zitadel-smtp|resend=resend=ZITADEL_SMTP_URL"
)

step6b_seed_external_credentials() {
    log "Step 6b: Seeding externally-issued credentials..."

    local spec subdir target files_spec ns name
    local seeded=0 skipped=0

    for spec in "${SEED_SPECS[@]}"; do
        IFS='|' read -r subdir target files_spec <<< "$spec"
        ns="${target%%/*}"
        name="${target##*/}"

        # Environment first, then the operator's own file -- the precedence the
        # Go path uses, and the reason is the same: k8-secrets/ belongs to an
        # operator's laptop, while a tenant's Day-0 runs in CI under the tenant's
        # secrets (ADR-072), where that directory does not exist and never will.
        #
        # Values are passed on stdin rather than as --from-file or --from-literal:
        # a literal would put the credential in this process's argument list,
        # which is world-readable, and writing it to a file to satisfy --from-file
        # would put it on disk in the tenant's repository.
        local -a args=()
        local pair file key env_name value missing=0
        local IFS_SAVE="$IFS"
        IFS=','
        for pair in $files_spec; do
            IFS="$IFS_SAVE"
            file="${pair%%=*}"
            env_name="${pair##*=}"
            key="${pair#*=}"; key="${key%=*}"

            value="${!env_name:-}"
            if [[ -z "$value" ]]; then
                local path; path="$(resolve_owned "k8-secrets/$subdir/$file")"
                [[ -s "$path" ]] && value="$(tr -d '\r\n' < "$path")"
            fi

            if [[ -z "$value" ]]; then
                log "  $target: no $env_name in the environment and no value in k8-secrets/$subdir/$file"
                missing=1
            else
                args+=("--from-literal=$key=$value")
            fi
            IFS=','
        done
        IFS="$IFS_SAVE"

        if [[ "$missing" -eq 1 ]]; then
            log "  ⏭️  $target not seeded — set the variables above, or fill the files, and re-run (idempotent)"
            ((skipped++))
            continue
        fi

        kubectl --kubeconfig="$KUBECONFIG_PATH" -n "$ns" \
            create secret generic "$name" "${args[@]}" \
            --dry-run=client -o yaml \
            | kubectl --kubeconfig="$KUBECONFIG_PATH" apply -f - >/dev/null \
            || error_exit "failed to create $target"

        log "  ✓ $target seeded"
        ((seeded++))
    done

    log "Step 6b: $seeded seeded, $skipped awaiting values"
    if [[ "$skipped" -gt 0 ]]; then
        log "  Note: hub-operator uploads these to Infisical on its next reconcile,"
        log "        so re-running this step once the values are present is enough."
    fi
}

# Step 8b: Re-trigger ESO after Day-0 has populated Infisical
#
# Ordering makes this necessary, and without it a first bootstrap looks broken for
# a full hour:
#
#   ESO reconciles first  -> Infisical is empty, Day-0 has not run yet
#                         -> every ExternalSecret records SecretSyncedError
#   refreshInterval: 1h   -> ESO caches that failure and will not look again
#   Day-0 completes       -> hub-operator uploads the secrets
#                         -> nothing tells ESO they have arrived
#
# The data is present and the store is valid, yet every ExternalSecret stays red
# until an hour has passed — long after the gates below have judged them. Observed
# as 2/22 ready with 39 successful uploads logged by hub-operator.
#
# ESO reconciles an ExternalSecret whenever its metadata changes, so stamping an
# annotation is the supported nudge. It is idempotent and harmless when the secrets
# already resolve.
step8b_refresh_external_secrets() {
    log "Step 8b: Re-syncing ExternalSecrets now that Infisical is populated..."

    local stamp total=0 ready=0
    stamp="$(date +%s)"

    local pairs
    pairs=$(kubectl --kubeconfig="$KUBECONFIG_PATH" get externalsecrets -A \
        --no-headers -o custom-columns=NS:.metadata.namespace,NAME:.metadata.name 2>/dev/null || true)

    if [[ -z "$pairs" ]]; then
        log "  No ExternalSecrets found — nothing to re-sync"
        return
    fi

    while read -r ns name; do
        [[ -z "$ns" || -z "$name" ]] && continue
        ((total++))
        kubectl --kubeconfig="$KUBECONFIG_PATH" -n "$ns" annotate externalsecret "$name" \
            force-sync="$stamp" --overwrite >/dev/null 2>&1 || true
    done <<< "$pairs"

    log "  Triggered re-sync on $total ExternalSecret(s); waiting for convergence..."

    # Bounded wait: this is a nudge, not a gate. The secrets-resolve gate that runs
    # next is what decides whether anything is genuinely unseeded.
    local attempt
    for attempt in $(seq 1 12); do
        ready=$(kubectl --kubeconfig="$KUBECONFIG_PATH" get externalsecrets -A \
            -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' 2>/dev/null \
            | grep -c "^True$" || true)
        if [[ "${ready:-0}" -ge "$total" ]]; then
            break
        fi
        sleep 10
    done

    log "  ExternalSecrets ready: ${ready:-0}/$total"
    if [[ "${ready:-0}" -lt "$total" ]]; then
        log "  Remaining ones are reported individually by the secret resolution gate below."
    fi
}

# Step 7-8: Create Machine Identity and configure ESO (automated in hub-operator)
step7_8_configure_eso() {
    if is_step_completed "configure_eso"; then
        log "Step 7-8: ESO configuration already completed, skipping"
        return
    fi

    log "Step 7-8: Machine Identity creation and ESO configuration (automated in hub-operator)..."
    log "These steps are automated in hub-operator. Skipping manual configuration."

    mark_step_completed "configure_eso"
}

# Step 9: Wait for database deployment
step9_wait_database() {
    if is_step_completed "wait_database"; then
        log "Step 9: Database already ready, skipping"
        return
    fi

    log "Step 9: Waiting for database deployment (timeout: 1800s)..."

    # The BOOTSTRAP cluster only. Each platform application owns its own (ADR-112)
    # and the rest arrive with boundary 02, after this step; Day-0 cannot proceed
    # without this one, because Infisical's database is on it.
    log "Running: kubectl wait --for=condition=ready pod -l cnpg.io/cluster=infisical-db --timeout=1800s"
    kubectl wait --for=condition=ready pod -l cnpg.io/cluster=infisical-db \
        -n platform-data --timeout=1800s \
        --kubeconfig="$KUBECONFIG_PATH"

    mark_step_completed "wait_database"
    log "Database deployment completed"
}

# Step 10: Wait for SpokePool readiness and certificate distribution
step10_wait_spokepool() {
    if is_step_completed "wait_spokepool"; then
        log "Step 10: SpokePool already ready, skipping"
        return
    fi

    log "Step 10: Waiting for SpokePool readiness and certificate distribution..."
    log "SpokePool: $SPOKEPOOL_NAME in namespace: $SPOKEPOOL_NAMESPACE"

    local max_attempts=$((SPOKEPOOL_TIMEOUT / 10))  # Convert seconds to attempts (10s intervals)
    local attempt=1

    log "Checking SpokePool $SPOKEPOOL_NAME status..."

    while [[ $attempt -le $max_attempts ]]; do
        # Check SpokePool status
        local spokepool_status
        spokepool_status=$(kubectl get spokepool "$SPOKEPOOL_NAME" -n "$SPOKEPOOL_NAMESPACE" \
            --kubeconfig="$KUBECONFIG_PATH" \
            -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || echo "NotFound")

        local spokepool_synced
        spokepool_synced=$(kubectl get spokepool "$SPOKEPOOL_NAME" -n "$SPOKEPOOL_NAMESPACE" \
            --kubeconfig="$KUBECONFIG_PATH" \
            -o jsonpath='{.status.conditions[?(@.type=="Synced")].status}' 2>/dev/null || echo "NotFound")

        if [[ "$spokepool_status" == "True" && "$spokepool_synced" == "True" ]]; then
            log "✅ SpokePool is Ready and Synced"
            break
        fi

        log "Waiting for SpokePool readiness (attempt $attempt/$max_attempts, Ready: $spokepool_status, Synced: $spokepool_synced). It mostly get ready by 55"
        sleep 10
        ((attempt++))
    done

    if [[ $attempt -gt $max_attempts ]]; then
        log "❌ SpokePool did not become ready within expected time"
        log "Current status: Ready=$spokepool_status, Synced=$spokepool_synced"
        error_exit "SpokePool readiness timeout"
    fi

    # Step 10a: Wait for CAPI Cluster to be Ready
    log "Step 10a: Checking CAPI Cluster readiness..."

    local cluster_name="$SPOKEPOOL_NAME"
    local cluster_attempt=1
    local cluster_max_attempts=$((CLUSTER_TIMEOUT / 10))

    # Infrastructure reasons that will never converge. CAPH reports a refused cloud
    # API call as a condition reason and then stops trying to make progress; polling
    # it to timeout burns CLUSTER_TIMEOUT and throws away the only useful fact.
    #
    # This is not theoretical: on 2026-08-23 a leaked-load-balancer quota exhaustion
    # (ADR-046 §23) surfaced here as LoadBalancerCreateFailed and was polled for the
    # full 15 minutes while the log printed nothing but "Infrastructure=False".
    local terminal_infra_reasons='LoadBalancerCreateFailed|LoadBalancerAttachFailed|NetworkCreateFailed|PlacementGroupCreateFailed|SSHKeyNotFound|CredentialsNotFound|FatalError'

    while [[ $cluster_attempt -le $cluster_max_attempts ]]; do
        # Get cluster phase and conditions
        local cluster_phase
        cluster_phase=$(kubectl get clusters.cluster.x-k8s.io "$cluster_name" -n platform-capi \
            --kubeconfig="$KUBECONFIG_PATH" \
            -o jsonpath='{.status.phase}' 2>/dev/null || echo "Unknown")

        # Read status, reason and message together. Reading only the status is what
        # made this loop undiagnosable from its own log: every terminal CAPH failure
        # and every genuinely-slow provision print the identical line.
        local infra_line infra_ready infra_rest infra_reason infra_msg
        infra_line=$(kubectl get clusters.cluster.x-k8s.io "$cluster_name" -n platform-capi \
            --kubeconfig="$KUBECONFIG_PATH" \
            -o jsonpath='{range .status.conditions[?(@.type=="InfrastructureReady")]}{.status}|{.reason}|{.message}{end}' 2>/dev/null || echo "")
        [[ -z "$infra_line" ]] && infra_line="False||"
        infra_ready="${infra_line%%|*}"
        infra_rest="${infra_line#*|}"
        infra_reason="${infra_rest%%|*}"
        infra_msg="${infra_rest#*|}"

        local cp_ready
        cp_ready=$(kubectl get clusters.cluster.x-k8s.io "$cluster_name" -n platform-capi \
            --kubeconfig="$KUBECONFIG_PATH" \
            -o jsonpath='{.status.conditions[?(@.type=="ControlPlaneReady")].status}' 2>/dev/null || echo "False")

        local workers_ready
        workers_ready=$(kubectl get clusters.cluster.x-k8s.io "$cluster_name" -n platform-capi \
            --kubeconfig="$KUBECONFIG_PATH" \
            -o jsonpath='{.status.v1beta2.conditions[?(@.type=="WorkerMachinesReady")].status}' 2>/dev/null || echo "False")

        if [[ "$infra_ready" == "True" && "$cp_ready" == "True" && "$workers_ready" == "True" ]]; then
            log "✅ CAPI Cluster is ready: Phase=$cluster_phase, InfrastructureReady=True, ControlPlaneReady=True, WorkersReady=True"
            break
        fi

        if [[ -n "$infra_reason" && "$infra_reason" =~ $terminal_infra_reasons ]]; then
            log "❌ CAPI Cluster infrastructure failed terminally: $infra_reason"
            log "   $infra_msg"
            dump_cluster_diagnostics "$cluster_name"
            error_exit "CAPI Cluster infrastructure error ($infra_reason) — terminal, not waiting for timeout"
        fi

        log "Waiting for CAPI Cluster (attempt $cluster_attempt/$cluster_max_attempts): Phase=$cluster_phase, Infrastructure=$infra_ready${infra_reason:+ ($infra_reason)}, ControlPlane=$cp_ready, Workers=$workers_ready"
        sleep 10
        ((cluster_attempt++))
    done

    if [[ $cluster_attempt -gt $cluster_max_attempts ]]; then
        log "❌ CAPI Cluster did not become ready within expected time"
        dump_cluster_diagnostics "$cluster_name"
        error_exit "CAPI Cluster readiness timeout"
    fi

    # Step 10b: Verify ProviderConfig for spoke cluster exists
    log "Step 10b: Checking ProviderConfig for spoke cluster..."

    local providerconfig_attempt=1
    local providerconfig_max_attempts=60

    while [[ $providerconfig_attempt -le $providerconfig_max_attempts ]]; do
        if kubectl get providerconfig "$cluster_name" -n "$SPOKEPOOL_NAMESPACE" \
            --kubeconfig="$KUBECONFIG_PATH" >/dev/null 2>&1; then
            log "✅ ProviderConfig for spoke cluster exists: $cluster_name"
            break
        fi

        log "Waiting for ProviderConfig (attempt $providerconfig_attempt/$providerconfig_max_attempts): $cluster_name"
        sleep 10
        ((providerconfig_attempt++))
    done

    if [[ $providerconfig_attempt -gt $providerconfig_max_attempts ]]; then
        log "⚠️ ProviderConfig not found (may be created by external controller, continuing...)"
    fi

    # Step 10c: Wait for Identity Infrastructure Readiness
    # The PKI artifacts depend on the HubEnvironment controller finishing Phase 3 (OAuth clients).
    # That phase is gated by Infisical -> ExternalSecrets -> Hydra. We wait for the condition here
    # to avoid race conditions with ArgoCD's retry backoffs.
    # resolve_owned, not ZERO_OPS_DIR: this is a PLATFORM script, and the tenant's
    # checkout has no scripts/ at all (ADR-072). Looked for there it was never
    # found, so the wait it performs was skipped on every tenant-path run -- and
    # what it waits for is the precondition of the step immediately below, which
    # then spun against artifacts nothing had been allowed to create yet.
    local identity_wait; identity_wait="$(resolve_owned scripts/k8-setup/wait-for-identity.sh)"
    if [[ -x "$identity_wait" ]]; then
        "$identity_wait" "$KUBECONFIG_PATH" "$CERT_TIMEOUT"
    else
        error_exit "wait-for-identity.sh not found at $identity_wait. The PKI wait below depends on it; skipping it turns a missing precondition into a twenty-minute timeout."
    fi

    # Step 10d: Wait for bootstrap PKI artifacts (machine-identity + bootstrap-cert CRS wrappers)
    # Created by hub-operator (bootstrap-cert) and spoke-identity-operator (machine-identity)
    # in platform-capi namespace for ClusterResourceSet consumption.
    log "Step 10d: Waiting for bootstrap PKI artifacts..."
    local pki_attempt=1
    local pki_max_attempts=$((CERT_TIMEOUT / 10))

    while [[ $pki_attempt -le $pki_max_attempts ]]; do
        local identity_ready=false
        local cert_ready=false

        # Wait for Certificate CR to be Ready (cert-manager issued)
        if kubectl wait --for=condition=Ready certificate "argocd-agent-${SPOKEPOOL_NAME}" \
            -n platform-capi --kubeconfig="$KUBECONFIG_PATH" --timeout=5s >/dev/null 2>&1; then
            cert_ready=true
        fi

        # Wait for SpokeMachineIdentity CR to be Ready (identity provisioned)
        if kubectl wait --for=condition=Ready spokemachineidentity "${SPOKEPOOL_NAME}" \
            -n platform-capi --kubeconfig="$KUBECONFIG_PATH" --timeout=5s >/dev/null 2>&1; then
            identity_ready=true
        fi

        if [[ "$identity_ready" == "true" && "$cert_ready" == "true" ]]; then
            log "✅ Bootstrap PKI artifacts ready"
            break
        fi

        log "Waiting for bootstrap PKI. Usually becomes ready at 5th attempt. (attempt $pki_attempt/$pki_max_attempts): certificate=$cert_ready identity=$identity_ready"
        sleep 10
        ((pki_attempt++))
    done

    if [[ $pki_attempt -gt $pki_max_attempts ]]; then
        log "❌ Bootstrap PKI artifacts not ready within timeout"
        log "Check hub-operator and spoke-identity-operator logs"
        log "This may mean Infisical is unhealthy or the infisical-auth secret is missing"
        error_exit "bootstrap PKI timeout. Resume the run 5min sometime later."
    fi

    # Step 10d: Verify certificates exist in target namespaces
    log "Step 10d: Verifying certificates in target namespaces..."

    # platform-messaging dropped: no manifest declares it and the platform ships
    # no messaging component, so this looked for a namespace nothing creates and
    # logged "not found yet (may be created later)" on every box, forever.
    local target_namespaces=("platform-observability" "platform-ops")
    for ns in "${target_namespaces[@]}"; do
        # Check if namespace exists
        if kubectl get namespace "$ns" \
            --kubeconfig="$KUBECONFIG_PATH" >/dev/null 2>&1; then
            log "✅ Namespace exists: $ns"
        else
            log "⚠️ Namespace not found yet (may be created later): $ns"
        fi
    done

    # Step 10f -- the spoke's database -- deliberately does NOT run here.
    #
    # It used to, and that made a fresh hybrid spoke impossible to bootstrap:
    # this function runs at main():2200 and step10e_spoke_home_worker, which
    # brings up the spoke's ONLY general capacity, runs at 2213. So the database
    # wait sat ahead of the thing that creates the nodes the database needs, and
    # spent 900s proving it. Not a race -- an ordering inversion that failed the
    # same way every time, reporting "shared-cnpg has no ready instance" about a
    # cluster whose one node was a tainted control plane.
    #
    # It is now step10f_spoke_database, called from main() after 10e.

    # Step 10e is NOT called here: it is invoked from main(), outside this
    # function's completed-step skip, because worker convergence is live state and
    # must be re-evaluated on every run (ADR-046 §24.2).
    mark_step_completed "wait_spokepool"
    log "✅ Spoke provisioning gates passed"
}

# Step 10f: the spoke's own database, gated AFTER its workers exist.
#
# Ordering is the whole point of this being a separate function. It ran inside
# step10_wait_spokepool, which main() calls before step10e_spoke_home_worker --
# so it waited for a database on a cluster that had no schedulable node yet, and
# the step that would give it one came afterwards. Every fresh hybrid spoke
# failed here, and the message named CNPG rather than the empty node pool.
step10f_spoke_database() {
    #
    # Step 9 waits up to 1800s for the HUB's CNPG cluster and nothing waited for
    # the spoke's, so bootstrap reported complete while shared-cnpg was still
    # electing a primary -- and post-bootstrap validation, which samples once,
    # read that as a failure and ended the run on a box that was minutes from
    # correct.
    #
    # The instinct there is to sleep between bootstrap and validation. This is
    # the gate that makes the sleep unnecessary: validation was not too eager,
    # bootstrap was declaring done on a component it never waited for.
    log "Step 10f: Waiting for the spoke's database to report healthy..."
    local spoke_kc; spoke_kc="$(mktemp)"
    if ! kubectl get secret "${SPOKEPOOL_NAME}-kubeconfig" -n platform-capi \
        --kubeconfig="$KUBECONFIG_PATH" -o jsonpath='{.data.value}' 2>/dev/null \
        | base64 -d > "$spoke_kc" 2>/dev/null || [[ ! -s "$spoke_kc" ]]; then
        rm -f "$spoke_kc"
        error_exit "Step 10f: cannot read ${SPOKEPOOL_NAME}-kubeconfig — the spoke's database cannot be verified"
    fi

    # Capacity BEFORE the database, because a spoke with nowhere to schedule
    # cannot produce one and the wait below would spend 900s proving it.
    #
    # On 2026-09-18 nutgraf-01 had a single node -- its control plane, tainted
    # NoSchedule -- because the hybrid claim carried 0 cloud workers and an empty
    # home-workers list. Thirteen pods sat Pending, ArgoCD's sync waves never
    # turned Healthy, and the wave carrying shared-cnpg was therefore never
    # applied. The run then failed on "shared-cnpg has no ready instance", which
    # sends the reader to CNPG, to storage, to the operator -- everywhere except
    # the empty node pool an hour upstream.
    #
    # A node that tolerates nothing is not capacity. Count only nodes that are
    # Ready AND carry no NoSchedule taint.
    local schedulable
    schedulable=$(kubectl --kubeconfig="$spoke_kc" get nodes \
        -o jsonpath='{range .items[*]}{range .status.conditions[?(@.type=="Ready")]}{.status}{end}{" "}{range .spec.taints[?(@.effect=="NoSchedule")]}{.key}{end}{"\n"}{end}' 2>/dev/null \
        | awk '$1=="True" && NF==1' | wc -l | tr -d ' ')

    if [[ "${schedulable:-0}" -lt 1 ]]; then
        kubectl --kubeconfig="$spoke_kc" get nodes -o wide 2>&1 | sed 's/^/    /' >&2
        rm -f "$spoke_kc"
        error_exit "Step 10f: the spoke has no schedulable node -- every node is either NotReady or carries a NoSchedule taint.
    Nothing can run there, so shared-cnpg will never be created and waiting for it would tell you nothing.
    A hybrid pool gets its default capacity from home workers (ADR-075); check that
    registry/clusters/${SPOKEPOOL_NAME}/infrastructure/spokepool-hybrid.yaml names real hardware
    in home-workers, or set nodePool.count above 0 for Hetzner burst capacity."
    fi
    log "✅ Spoke has ${schedulable} schedulable node(s)"

    local cnpg_deadline=900 cnpg_waited=0 cnpg_ready="" cnpg_phase=""
    while (( cnpg_waited < cnpg_deadline )); do
        # readyInstances, not phase text. The phase is a human-facing string that
        # changes between CNPG releases; the instance count is the fact.
        cnpg_ready=$(kubectl --kubeconfig="$spoke_kc" get clusters.postgresql.cnpg.io \
            shared-cnpg -n platform-data -o jsonpath='{.status.readyInstances}' 2>/dev/null || echo "")
        [[ -n "$cnpg_ready" && "$cnpg_ready" -ge 1 ]] && break
        sleep 10
        cnpg_waited=$(( cnpg_waited + 10 ))
    done

    cnpg_phase=$(kubectl --kubeconfig="$spoke_kc" get clusters.postgresql.cnpg.io \
        shared-cnpg -n platform-data -o jsonpath='{.status.phase}' 2>/dev/null || echo "unknown")
    rm -f "$spoke_kc"

    if [[ -z "$cnpg_ready" || "$cnpg_ready" -lt 1 ]]; then
        error_exit "Step 10f: the spoke's shared-cnpg has no ready instance after ${cnpg_deadline}s (phase='${cnpg_phase}')"
    fi
    log "✅ Spoke database ready: ${cnpg_ready} instance(s), phase='${cnpg_phase}' (after ${cnpg_waited}s)"

}

# Bring the spoke's home-lab worker into service, mirroring the hub's
# home-worker-join phase (ADR-046 §21).
#
# Why the bootstrap script and not an operator: CAPI cannot provision these nodes —
# they are Hyper-V VMs on a workstation, reached over SSH — so the join is a script
# the operator's machine runs. hub-operator's part (minting the bootstrap token into
# {spoke}-home-worker-join) is already done by the time we get here; nothing consumed
# it until now, which is why a spoke came up with zero workers and every workload
# stayed Pending behind ADR-014 placement.
#
# What this replaces: the previous Step 10e counted CAPI Machine objects, never
# looked at a Node, warned either way, and then printed four hardcoded success lines
# — "Ready=True", "WorkersReady=True", "All 3 certificates ready" — regardless of
# state. On 2026-08-23 it printed all four while the spoke had no CNI, no worker, no
# CRDs and zero certificates. A bootstrap must never claim a condition it did not
# observe, so every assertion below is measured.
step10e_spoke_home_worker() {
    local cluster_name="$1"
    log "Step 10e: Converging the spoke's home-lab worker..."

    # Home workers are a hybrid-cell concept (ADR-046). Other providers use CAPI
    # MachineDeployments and have nothing to do here.
    if [[ "$PROVIDER" != "hybrid" ]]; then
        log "Step 10e: provider=$PROVIDER has no home workers — skipping"
        return 0
    fi

    local spoke_kc
    spoke_kc="$(mktemp)"
    # shellcheck disable=SC2064
    trap "rm -f '$spoke_kc'" RETURN

    if ! kubectl get secret "${cluster_name}-kubeconfig" -n platform-capi \
        --kubeconfig="$KUBECONFIG_PATH" -o jsonpath='{.data.value}' 2>/dev/null \
        | base64 -d > "$spoke_kc" 2>/dev/null || [[ ! -s "$spoke_kc" ]]; then
        error_exit "Step 10e: cannot read ${cluster_name}-kubeconfig — the spoke's worker cannot be joined or verified"
    fi

    # The join needs a functioning control plane: kubeadm join talks to the API, and
    # the new node cannot leave NotReady without a CNI. Cilium's config now carries
    # the control-plane endpoint (ADR-046 §24), so this should already hold.
    local cp_ready=""
    local waited=0
    while (( waited < CLUSTER_TIMEOUT )); do
        cp_ready=$(kubectl --kubeconfig="$spoke_kc" get nodes \
            -l node-role.kubernetes.io/control-plane \
            -o jsonpath='{.items[*].status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || echo "")
        [[ "$cp_ready" == *"True"* ]] && break
        log "Step 10e: waiting for the spoke control plane to be Ready (CNI must be up first) [${waited}s/${CLUSTER_TIMEOUT}s]"
        sleep 15
        waited=$((waited + 15))
    done
    if [[ "$cp_ready" != *"True"* ]]; then
        log "❌ Spoke control plane never became Ready"
        kubectl --kubeconfig="$spoke_kc" get nodes -o wide 2>&1 | tee -a "$LOG_DIR/bootstrap.log" || true
        kubectl --kubeconfig="$spoke_kc" get pods -n kube-system 2>&1 | tee -a "$LOG_DIR/bootstrap.log" || true
        error_exit "Step 10e: spoke control plane not Ready — check the Cilium agent (ADR-046 §24)"
    fi
    log "Step 10e: ✓ spoke control plane Ready"
    SPOKE_HOME_WORKERS_PENDING=""

    # Idempotent: provisioning a Flatcar VM is ~10 minutes of Hyper-V work, and a
    # resumed bootstrap must not pay it again for a node that is already serving.
    if spoke_home_worker_ready "$spoke_kc"; then
        log "Step 10e: ✓ all registry home workers already Ready — skipping provisioning"
        return 0
    fi

    # resolve_owned: this is a PLATFORM script, and ZERO_OPS_DIR is the TENANT's
    # checkout, which has no scripts/ at all (ADR-072). Looked for there it is
    # never found -- the same defect that silently skipped the tailscale step and
    # the identity wait before it.
    local script; script="$(resolve_owned scripts/hybrid/provision-flatcar-worker.sh)"
    if [[ ! -x "$script" && ! -f "$script" ]]; then
        error_exit "Step 10e: $script is missing; the spoke has no worker and platform workloads cannot schedule (ADR-014)"
    fi

    # Provision only the workers that are actually missing. Re-running the whole
    # --cluster spoke set would destroy and rebuild VMs that are already serving,
    # which is ~10 minutes of Hyper-V work per node and a needless outage.
    local registry pending_idx="" entry idx name node_state
    registry="$(spoke_home_worker_registry)"

    if [[ -z "$registry" ]]; then
        log "Step 10e: no home-lab.env registry readable — provisioning the whole spoke set"
        log "Step 10e: Running provision-flatcar-worker.sh --cluster spoke"
        log "Step 10e: (Hyper-V VM creation over SSH — this takes several minutes)"
        if ! HUB_KUBECONFIG="$KUBECONFIG_PATH" HYBRID_SPOKE_NAME="$cluster_name" \
             bash "$script" --cluster spoke 2>&1 | tee -a "$LOG_DIR/bootstrap.log"; then
            error_exit "Step 10e: home-worker provisioning failed. Fix the cause and re-run — this step is idempotent and skips an already-Ready node."
        fi
    else
        while IFS='|' read -r idx name <&3; do
            [[ -z "$idx" ]] && continue
            node_state=$(kubectl --kubeconfig="$spoke_kc" get node "$name" \
                -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || echo "")
            if [[ "$node_state" == "True" ]]; then
                log "Step 10e: ✓ ${name} already Ready — not rebuilding it"
                continue
            fi
            pending_idx="${pending_idx}${idx} "
        done 3<<< "$registry"

        log "Step 10e: home workers to provision (registry index): ${pending_idx:-none}"
        for idx in $pending_idx; do
            log "Step 10e: Running provision-flatcar-worker.sh --cluster spoke --node ${idx}"
            log "Step 10e: (Hyper-V VM creation over SSH — this takes several minutes)"
            if ! HUB_KUBECONFIG="$KUBECONFIG_PATH" HYBRID_SPOKE_NAME="$cluster_name" \
                 bash "$script" --cluster spoke --node "$idx" 2>&1 | tee -a "$LOG_DIR/bootstrap.log"; then
                error_exit "Step 10e: home-worker provisioning failed for registry node ${idx}. Fix the cause and re-run — this step is idempotent and skips an already-Ready node."
            fi
        done
    fi

    # The script has its own gate, but the cluster's view is what the next steps
    # depend on, so it is measured here too.
    local join_waited=0
    while (( join_waited < CLUSTER_TIMEOUT )); do
        if spoke_home_worker_ready "$spoke_kc"; then
            log "Step 10e: ✅ every registry home worker Ready and correctly labelled"
            return 0
        fi
        log "Step 10e: waiting for home workers to become Ready [${join_waited}s/${CLUSTER_TIMEOUT}s]: ${SPOKE_HOME_WORKERS_PENDING:-?}"
        sleep 15
        join_waited=$((join_waited + 15))
    done

    log "❌ Home workers still outstanding on $cluster_name: ${SPOKE_HOME_WORKERS_PENDING:-?}"
    kubectl --kubeconfig="$spoke_kc" get nodes --show-labels 2>&1 | tee -a "$LOG_DIR/bootstrap.log" || true
    error_exit "Step 10e: not every registry home worker is Ready on the spoke — workloads pinned to the missing box would sit Pending against the ADR-014 placement rule"
}

# The home-worker registry (scripts/hybrid/home-lab.env) is the source of truth for
# which nodes belong to the spoke — the same file provision-flatcar-worker.sh walks.
# Emits one "<index>|<hostname>" line per spoke entry, where <index> is the 1-based
# registry position the provisioner's --node flag takes. Emits nothing when the file
# is absent: it is gitignored, so a checkout without it must not hard-fail here.
#
# The file is sourced in a subshell on purpose. It sets HUB_KUBECONFIG,
# HYBRID_SPOKE_NAME and TAILNET_NAME, and none of those may leak into the
# bootstrap's own environment.
spoke_home_worker_registry() {
    local env_file; env_file="$(resolve_owned scripts/hybrid/home-lab.env)"
    [[ -f "$env_file" ]] || return 0

    local nodes
    nodes="$( . "$env_file" >/dev/null 2>&1; printf '%s' "${HOME_WORKER_NODES:-}" )"
    [[ -n "$nodes" ]] || return 0

    local host ssh_target node_target curr idx=0
    while IFS='|' read -r host ssh_target _os _tailnet _tag node_target _rest <&3; do
        [[ -z "${ssh_target:-}" ]] && continue
        idx=$((idx + 1))
        # Same routing rule as the provisioner: column 6 is authoritative, the
        # index heuristic is only a fallback for registries predating it.
        curr="spoke"
        [[ "$idx" -eq 1 ]] && curr="hub"
        [[ -n "${node_target:-}" ]] && curr="$node_target"
        [[ "$curr" == "spoke" ]] || continue
        [[ -z "${host:-}" ]] && host="flatcar-spoke-node-${idx}"
        printf '%s|%s\n' "$idx" "$host"
    done 3<<< "$nodes"
}

# Ready only when EVERY home worker the registry assigns to the spoke is a real
# Kubernetes Node that is Ready AND carries the ADR-046 §11 placement contract.
# Node conditions, never CAPI Machine objects: a Machine is a provisioning record,
# and a home worker has no Machine at all because CAPI did not create it. Counting
# Machines is what made the old gate pass on an empty cluster.
#
# It used to return on the FIRST Ready node, so a two-box spoke reported ✅ with
# box-b's worker missing entirely. Sets SPOKE_HOME_WORKERS_PENDING to the nodes
# still outstanding ("name (state)"), for the caller to log.
spoke_home_worker_ready() {
    local spoke_kc="$1"
    local expected status_lines entry name state line all_ready=1
    SPOKE_HOME_WORKERS_PENDING=""

    status_lines=$(kubectl --kubeconfig="$spoke_kc" get nodes \
        -l 'workload-location=on-prem,node-role.kubernetes.io/worker' \
        -o jsonpath='{range .items[*]}{.metadata.name}{"="}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' 2>/dev/null || true)

    expected="$(spoke_home_worker_registry)"
    if [[ -z "$expected" ]]; then
        # No readable registry: fall back to the weaker "at least one" contract
        # rather than blocking a bootstrap that has no home-lab.env to check.
        while IFS= read -r line; do
            [[ -z "$line" ]] && continue
            [[ "${line#*=}" == "True" ]] && return 0
        done <<< "$status_lines"
        SPOKE_HOME_WORKERS_PENDING="(no home-lab.env registry; no Ready home worker found)"
        return 1
    fi

    while IFS= read -r entry <&3; do
        [[ -z "$entry" ]] && continue
        name="${entry#*|}"
        state=""
        while IFS= read -r line; do
            [[ -z "$line" ]] && continue
            if [[ "${line%%=*}" == "$name" ]]; then
                state="${line#*=}"
                break
            fi
        done <<< "$status_lines"
        if [[ "$state" != "True" ]]; then
            all_ready=0
            SPOKE_HOME_WORKERS_PENDING="${SPOKE_HOME_WORKERS_PENDING}${name} (${state:-absent}) "
        fi
    done 3<<< "$expected"

    [[ "$all_ready" == "1" ]]
}

# Main execution
# Run one validation module at the bootstrap step where the condition it checks
# first becomes true, so a violation halts the run at its cause instead of
# surfacing later as an unrelated symptom in the post-bootstrap summary.
# mode=gate tolerates resources that have not converged yet; anything that cannot
# self-heal (a wrong environment slug, an unsubstituted DNS filter) still fails.
# The zone this box publishes on, read from what it declares.
#
# ADR-051 makes HubEnvironment.spec.domain the sole authority, and the gates need
# it: without it common.sh falls back to the PLATFORM's own zone, so a tenant box
# is checked against a domain it does not own. That produced a gate failing on a
# correct box -- "external-dns --domain-filter is not dev.nutgraf.in" on a box
# whose filter was correctly dev.acme.example, with the line above it confirming
# the substitution was right.
box_hub_domain() {
    kubectl --kubeconfig="$KUBECONFIG_PATH" get hubenvironment hub-environment \
        -n platform-ops -o jsonpath='{.spec.domain}' 2>/dev/null || true
}

run_gate() {
    local modules="$1" label="$2"
    log "Gate: $label"
    local hub_domain; hub_domain="$(box_hub_domain)"
    if [[ -z "$hub_domain" ]]; then
        # Refused rather than defaulted. The fallback is the platform's zone, and
        # checking a tenant's box against it either fails a correct box or passes
        # a wrong one -- both worse than saying the authority is unreadable.
        error_exit "Gate '$label': the box declares no domain (HubEnvironment.spec.domain is empty), so there is nothing to validate its hostnames against."
    fi
    if ! HUB_KUBECONFIG="$KUBECONFIG_PATH" ENVIRONMENT="$ENVIRONMENT" \
         SPOKEPOOL_NAME="$SPOKEPOOL_NAME" HUB_DOMAIN="$hub_domain" \
         bash "$SCRIPT_DIR/validate/run.sh" cluster --only="$modules" --mode=gate; then
        error_exit "Gate '$label' failed — see above. Continuing would build the rest of the platform on a broken foundation."
    fi
}

main() {
    # Parse arguments
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --name=*)
                CLUSTER_NAME="${1#*=}"
                shift
                ;;
            --name)
                CLUSTER_NAME="$2"
                shift 2
                ;;
            --provider=*)
                PROVIDER="${1#*=}"
                shift
                ;;
            --provider)
                PROVIDER="$2"
                shift 2
                ;;
            --environment=*)
                ENVIRONMENT="${1#*=}"
                shift
                ;;
            --environment)
                ENVIRONMENT="$2"
                shift 2
                ;;
            --spoke=*)
                SPOKEPOOL_NAME="${1#*=}"
                shift
                ;;
            --spoke)
                SPOKEPOOL_NAME="$2"
                shift 2
                ;;
            --topology=*)
                TOPOLOGY="${1#*=}"
                shift
                ;;
            --topology)
                TOPOLOGY="$2"
                shift 2
                ;;
            --gitops-dir=*)
                GITOPS_DIR="${1#*=}"
                shift
                ;;
            --gitops-dir)
                GITOPS_DIR="$2"
                shift 2
                ;;
            --bundle-version=*)
                BUNDLE_VERSION="${1#*=}"
                shift
                ;;
            --bundle-version)
                BUNDLE_VERSION="$2"
                shift 2
                ;;
            --gating=*)
                GATING="${1#*=}"
                shift
                ;;
            --gating)
                GATING="$2"
                shift 2
                ;;
            --teardown)
                TEARDOWN="true"
                shift
                ;;
            --seed-only)
                SEED_ONLY="true"
                shift
                ;;
            --region=*)
                REGION="${1#*=}"
                shift
                ;;
            --region)
                REGION="$2"
                shift 2
                ;;
            --on-prem|--home-worker-enabled)
                # --on-prem is the name the CLI uses and the one to pass.
                # --home-worker-enabled is kept because existing invocations use
                # it; both set the same thing.
                HOME_WORKER_ENABLED="1"
                shift
                ;;
            --home-worker-ttl=*)
                HOME_WORKER_TTL="${1#*=}"
                shift
                ;;
            --home-worker-ttl)
                HOME_WORKER_TTL="$2"
                shift 2
                ;;
            --tailnet-name=*)
                TAILNET_NAME="${1#*=}"
                shift
                ;;
            --tailnet-name)
                TAILNET_NAME="$2"
                shift 2
                ;;
            --yes|-y)
                TEARDOWN_YES="1"
                shift
                ;;
            dev|stg|prod|ephemeral)
                # Positional environment: `hub-bootstrap.sh dev --teardown`.
                ENVIRONMENT="$1"
                shift
                ;;
            *)
                # A typo in a flag used to be logged and ignored, which meant the
                # run continued with a silently wrong configuration.
                error_exit "Unknown argument: $1"
                ;;
        esac
    done

    if [[ "$PROVIDER" != "hetzner" && "$PROVIDER" != "hybrid" ]]; then
        error_exit "Invalid provider: $PROVIDER (must be 'hetzner' or 'hybrid')"
    fi

    # An empty --environment is not a no-op: cmd/kakt/bootstrap.go coerces "" to
    # "prod", which makes the spoke AppSet path spoke-pools/prod/... — a path that
    # does not exist — so the spoke silently never provisions. Refuse it here,
    # where the message can say so, rather than debugging a missing spoke later.
    if [[ -z "$ENVIRONMENT" ]]; then
        error_exit "--environment is required (the CLI coerces an empty value to 'prod', and the spoke then never provisions)"
    fi
    case "$ENVIRONMENT" in
        dev|stg|prod|ephemeral) ;;
        *) error_exit "Invalid environment: $ENVIRONMENT (expected dev, stg, prod or ephemeral)" ;;
    esac

    case "$GATING" in
        sequenced|converged) ;;
        *) error_exit "Invalid gating mode: $GATING (expected sequenced or converged)" ;;
    esac

    # Export KUBECONFIG so the Go bootstrap binary's internal bare-kubectl calls
    # (e.g. infisical bootstrap pod discovery) resolve the same cluster instead
    # of a stale default context. The path is deterministic per cluster name.
    if [[ -f "$ZERO_OPS_DIR/k8-secrets/kubeconfig/${CLUSTER_NAME}.kubeconfig" ]]; then
        export KUBECONFIG="$ZERO_OPS_DIR/k8-secrets/kubeconfig/${CLUSTER_NAME}.kubeconfig"
    fi

    # --teardown destroys the environment and exits. It deliberately does NOT fall
    # through into a bootstrap: a rebuild is two explicit commands, so an
    # interrupted teardown can never half-build a replacement on top of the wreck.
    if [[ "$TEARDOWN" == "true" ]]; then
        run_full_teardown
        exit $?
    fi

    # --seed-only runs step 6b alone, for the normal case: the placeholders under
    # k8-secrets/ were empty on the first run, they have since been filled, and the
    # values need to reach Infisical without rebuilding anything. The operator
    # uploads them on its next reconcile.
    if [[ "$SEED_ONLY" == "true" ]]; then
        if [[ -z "${KUBECONFIG_PATH:-}" || ! -f "$KUBECONFIG_PATH" ]]; then
            KUBECONFIG_PATH="$ZERO_OPS_DIR/k8-secrets/kubeconfig/${CLUSTER_NAME}.kubeconfig"
        fi
        [[ -f "$KUBECONFIG_PATH" ]] || error_exit "kubeconfig not found at $KUBECONFIG_PATH — pass --name for the cluster to seed"
        log "Seeding externally-issued credentials only (--seed-only)"
        step6b_seed_external_credentials
        exit 0
    fi

    log "Starting Zero-Ops Hub Bootstrap Process"
    log "Project root: $ZERO_OPS_DIR"
    log "Log directory: $LOG_DIR"
    log "Provider: $PROVIDER"
    log "Region: $REGION"
    log "Cluster name: $CLUSTER_NAME"

    # Check prerequisites
    check_prerequisites

    # Pre-bootstrap validation. Every check is statically decidable from the repo
    # and needs no cluster, so config faults are found before the first Hetzner
    # server is billed rather than after a 40-minute provision. Fatal by design:
    # what it reports cannot be fixed forward from a half-built platform.
    #
    # Checkpoint-aware: Go infra-preflight is checkpointed via
    # .state/<cluster>.json (orchestrator.go:99, phaseDone).
    # Shell static validation (59 checks) is gated only on that state —
    # no hash, per user request. If Go says preflight completed, skip.
    if [[ "${SKIP_PREFLIGHT:-0}" == "1" ]]; then
        log "⚠️  SKIP_PREFLIGHT=1 — static repo validation bypassed (Go infra-preflight still checkpointed via .state/${CLUSTER_NAME}.json)"
    elif is_static_preflight_checkpointed; then
        log "Static repo validation skipped (checkpointed — Go state shows preflight completed via .state/${CLUSTER_NAME}.json)"
    else
        log "Running static repo validation (59 checks) — Go infra-preflight is checkpointed via .state/${CLUSTER_NAME}.json..."
        if ! ENVIRONMENT="$ENVIRONMENT" bash "$SCRIPT_DIR/validate/run.sh" preflight; then
            error_exit "Pre-bootstrap validation failed — nothing was created. Fix the reported invariants and re-run (SKIP_PREFLIGHT=1 overrides)."
        fi
    fi

    # Execute steps in order (each step is independently idempotent)
    # The hub bootstrap orchestrator internally manages sequential boundary
    # gating (B01 → B02 → init-secrets → B03). No fixed sleeps needed
    # between steps — each step polls until its prerequisites converge.
    step1_bootstrap_hub

    step1b_reconcile_boundary_gates


    # Workers have joined, so every node's InternalIP is now claimed. On hybrid
    # that address is a tailnet IP, and it underpins both kubelet access and
    # Cilium's VXLAN endpoint — if it is advertised but not actually configured,
    # cross-node pod traffic (including CoreDNS) fails while every node still
    # reports Ready. Checked here because everything below this line depends on it.
    run_gate "kubelet-reachability" "API server → kubelet reachability"

    # The HubEnvironment and ClusterSecretStore now exist, so the Infisical slug is
    # decidable. A wrong slug is invisible afterwards: every ExternalSecret
    # resolves against another environment and reports Healthy doing it.
    run_gate "environment-isolation" "environment isolation"

    step1c_configure_tailscale

    # No step 2. AWS Secrets Manager was replaced by the Infisical-based escrow
    # (37f76d84), which the Go orchestrator installs before hub-operator starts
    # reconciling. The step numbering is left alone rather than renumbered: the
    # names are recorded in bootstrap-mgmt.json on every existing box.

    step3_configure_github

    # Clean up stale ReplicaSets left behind by rolling updates during
    # the bootstrap phase. These accumulate quickly and consume etcd
    # space in resource-constrained clusters (e.g. local kind).
    log "Cleanup: removing stale ReplicaSets..."
    kubectl delete replicasets --all-namespaces --field-selector=status.replicas=0 \
        --kubeconfig="$KUBECONFIG_PATH" 2>/dev/null || true

    step4_wait_namespaces

    step5_verify_secrets

    step6_wait_infisical

    step6b_seed_external_credentials

    step7_8_configure_eso

    step8b_refresh_external_secrets

    # Infisical is up and ESO has had a reconcile window, so an ExternalSecret that
    # still cannot resolve is a seeding fault rather than a race.
    run_gate "secrets-resolve" "secret resolution"

    step9_wait_database

    # The identity stack is up; prove the tenants' OAuth clients exist IN Zitadel
    # (ADR-060). The service creating them reports its own failures, but a client
    # deleted, renamed, or created in the wrong organisation afterwards leaves
    # nothing unhealthy in the cluster — it fails in the browser only.
    run_gate "oauth-clients" "OAuth client registration"

    if [[ -z "$SPOKEPOOL_NAME" ]]; then
        error_exit "SPOKEPOOL_NAME must be set via --spoke flag or SPOKEPOOL_NAME env var for provider '$PROVIDER'"
    fi
    step10_wait_spokepool

    # Deliberately OUTSIDE the wait_spokepool checkpoint (ADR-046 §24.2).
    #
    # Worker convergence is live state, not a one-shot fact. "We once waited for the
    # SpokePool" says nothing about whether a node is serving now — a home worker is
    # an unmanaged Hyper-V VM that can stop, and its host can sleep. Checkpointing it
    # produced exactly that: a resumed run logged "SpokePool already ready, skipping",
    # never provisioned the spoke's worker, and still declared success.
    #
    # This mirrors the hub's home-worker-join phase, which re-checks readyHubWorker
    # on every run rather than trusting a completed-phase marker. The SpokePool wait
    # above stays checkpointed; the worker gate must not be.
    step10e_spoke_home_worker "$SPOKEPOOL_NAME" || return 1

    # AFTER 10e, because the spoke's database needs a node to run on and 10e is
    # what puts one there. Ordered the other way -- which it was, inside
    # step10_wait_spokepool above -- this waited 900s for a Postgres instance on
    # a cluster whose only node was a tainted control plane, then failed naming
    # the database instead of the missing capacity.
    step10f_spoke_database || return 1

    # The spoke is Ready, so its ingress path is decidable — and under ADR-051 it
    # is the path that actually serves tenant traffic.
    run_gate "tenant-ingress" "spoke tenant ingress"

    # NOT a gate. The public endpoints are checked at the END, by the
    # post-bootstrap validation below, which runs every cluster module at
    # --mode=final -- module 35 among them.
    #
    # It was a gate here, and that was wrong twice over. It duplicated a check the
    # final sweep already makes, and it made the LAST thing to converge block
    # everything after it: DNS propagation and ACME issuance take minutes that the
    # steps behind this gate do not need to wait for, so a box whose endpoints were
    # merely not up YET failed here and never reached the validation that would
    # have said so precisely. A box on a domain that cannot resolve at all never
    # got past this line.
    #
    # The check has not been weakened. --mode=final is the STRICTER of the two
    # modes, so what was a gate failure is still a failure -- it just happens once,
    # at the end, after everything that could make it pass has had its chance.

    # Assert, do not announce. The previous banner claimed "SpokePool: Provisioned
    # and ready for tenant workloads", "Certificate distribution: Complete" and
    # "Spoke cluster: Ready for tenant database provisioning" unconditionally — and
    # printed all three while the spoke had no worker and the hub was shedding 53
    # pods. The run is only allowed to say what the gates above actually observed.
    log "Zero-Ops Hub Bootstrap Process completed"
    log "  Gates passed: secret resolution, OAuth clients, spoke readiness,"
    log "                spoke home worker, tenant ingress"
    log "  Post-bootstrap validation runs next and is fatal — the platform is not"
    log "  proven until it passes."
    log "You can now access your hub cluster using: kubectl --kubeconfig=$KUBECONFIG_PATH"

    # Run post-bootstrap core services validation
    log ""
    log "Running post-bootstrap core services validation..."
    # validate-all.sh, which runs post-bootstrap-validate and records the phase
    # in .state/validation.json. One entry point: bootstrap, the verify phase and
    # CI all reach the validators the same way, so "was this box validated" has
    # one answer wherever it was asked.
    local validate_script="$SCRIPT_DIR/validate-all.sh"
    if [[ ! -f "$validate_script" ]]; then
        # Not a warning. This is the step that decides whether the platform
        # works, and a bootstrap that cannot run it has not proven anything --
        # which is exactly what the log line here used to say while continuing.
        error_exit "validate-all.sh not found at $validate_script — the platform cannot be proven."
    fi
    # Fatal: swallowing this into a warning is how a bootstrap "succeeds"
    # while leaving a platform that does not work.
    #
    # KUBECONFIG is passed explicitly. The script used to default it to
    # k8-secrets/kubeconfig/hub.kubeconfig -- a cluster named "hub" -- so a box
    # named anything else was validated against another cluster's kubeconfig, or
    # a deleted one.
    if ! KUBECONFIG="$KUBECONFIG_PATH" \
         ZERO_OPS_DIR="$ZERO_OPS_DIR" \
         SPOKEPOOL_NAME="$SPOKEPOOL_NAME" \
         ENVIRONMENT="$ENVIRONMENT" \
         CLUSTER_NAME="$CLUSTER_NAME" \
         bash "$validate_script" --only=platform; then
        error_exit "Post-bootstrap validation FAILED — see the summary above."
    fi

    report_elapsed "success"
}

# Handle script interruption
trap 'log "Script interrupted"; report_elapsed "interrupted"; exit 1' INT TERM

# Run main function, unless this file was sourced.
#
# Sourcing is how a single step gets exercised against a box whose other steps
# already passed:
#
#   CLUSTER_NAME=... PROVIDER=... ENVIRONMENT=... SPOKEPOOL_NAME=... \
#   ZERO_OPS_DIR=... bash -c 'source scripts/hub-bootstrap.sh
#                             read_kubeconfig_from_state
#                             export KUBECONFIG="$KUBECONFIG_PATH"
#                             step10_wait_spokepool'
#
# Without the guard that runs the whole sequence instead, which is the opposite
# of what a caller sourcing the file is asking for.
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
    main "$@"
fi
