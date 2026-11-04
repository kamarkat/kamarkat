#!/usr/bin/env bash
# =============================================================================
# scripts/hybrid/provision-flatcar-worker.sh
#
# THE single entry point to provision a Hyper-V Flatcar Container Linux worker
# node on Windows home-lab hardware into the hybrid spoke cluster (ADR-046 §13).
#
# Architecture:
#   - Windows Host: Hyper-V Gen2 VM with Dynamic Memory and AutoStart
#   - OS: Flatcar Container Linux (Immutable, read-only /usr, native cgroups v2)
#   - Storage: Zero host disk formatting; VM runs from isolated 20GB VHDX
#   - Provisioning: Declarative Ignition config + offline binaries delivered via DVD ISO
#   - Kubernetes: Kubeadm join v1.35.10 via one-shot systemd service
#   - CNI: Cilium v1.17.18 (clean native cgroup & eBPF, no cgroup hacks)
#   - Storage Addon: local-path-provisioner on persistent /var/local-path-provisioner
#   - Network Mesh: Native Tailscale daemon connecting to tailnet
#
# Phases per node:
#   [0/6] Offline binary cache verification on Mac
#   [1/6] SSH reachability gate
#   [2/6] Windows host & Hyper-V preparation (power/lid, Hyper-V feature, directories)
#   [3/6] Flatcar base VHDX & Ignition ISO bundle preparation
#   [4/6] Provision Generation 2 Hyper-V VM with attached Ignition DVD ISO
#   [5/6] Wait for Flatcar First-Boot, Tailscale mesh, and Kubeadm join
#   [6/6] Verify node Ready in Spoke cluster (success gate)
#
# Usage:
#   ./provision-flatcar-worker.sh                          # provision all nodes
#   ./provision-flatcar-worker.sh --node 1                 # node index 1 only
#   ./provision-flatcar-worker.sh --verify                 # verify Ready status
# =============================================================================
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ENV_FILE="${HERE}/home-lab.env"
ONLY_NODE=""
TS_AUTHKEY=""
# Rebuilding is destructive and is no longer the first thing tried; see
# phase_recover_existing_vm. --force-rebuild is how an operator asks for the old
# behaviour, and RECOVER_WAIT_SECONDS is how long the gate waits before giving up.
FORCE_REBUILD=0
RECOVER_WAIT_SECONDS="${RECOVER_WAIT_SECONDS:-300}"
VSWITCH_NAME="Hybrid-Switch"
# Host side of the guest network. Guests boot static ${HOST_SUBNET_PREFIX}.1<node-idx>
# addresses with $HOST_GATEWAY_IP as gateway (see the Ignition network unit), so these
# values and the Ignition template have to agree.
HOST_SUBNET_PREFIX="172.30.0"
HOST_GATEWAY_IP="172.30.0.1"
HOST_SUBNET_CIDR="172.30.0.0/24"
HOST_NAT_NAME="Hybrid-NAT"

# Sustained CPU ceiling applied to the Windows host (ADR-046 §24.5). These hosts are
# thin laptops running Kubernetes node VMs; at 100% they reach the ACPI critical
# thermal trip and the firmware shuts them down mid-provision. Override per host with
# --host-cpu-max if a box has healthier cooling.
HOST_CPU_MAX_PCT="${HOST_CPU_MAX_PCT:-70}"
TARGET_CLUSTER="all"    # Default: 'all' (auto-routes nodes to Hub/Spoke per home-lab.env)
MEMORY_BYTES="0"         # 0 = auto-detect (14GB or TotalHostRAM - 2GB)
MIN_MEMORY_BYTES="0"     # 0 = auto-detect (2GB)
MAX_MEMORY_BYTES="0"     # 0 = auto-detect (TotalHostRAM - 2GB)
CPU_COUNT="0"            # 0 = auto-detect (all host logical cores)
# Disk ceilings are chosen per target cluster; no flag should be needed for a
# normal provision. The VHDX is dynamically expanding and Resize-VHD only raises
# the ceiling, so a generous number costs nothing on the host until written.
#
# The old flat 20GB default was far too small once the hub moved its workloads to
# home-lab nodes (ADR-046 §19): a 20GB VHDX yields ~13GB usable after the Flatcar
# OS, and a hub node carries ArgoCD, cert-manager, Kyverno, Crossplane, ESO,
# Redis and Infisical AND platform-db's 10Gi local-path PVC. It hit
# DiskPressure=True, the kubelet tainted the node, and with the control plane
# tainted as well nothing could be scheduled anywhere.
HUB_DISK_GB_DEFAULT=100    # whole platform + platform-db PVC
SPOKE_DISK_GB_DEFAULT=60   # tenant workloads + shared-cnpg PVC
DISK_SIZE_BYTES=""         # set only by --disk-gb; empty means "use the target default"
CLI_DISK_SET=0
K8S_VERSION="v1.35.10"
TAILSCALE_VERSION="1.102.2"
FLATCAR_RELEASE_CHANNEL="stable"
UPGRADE_FLATCAR="false"
CACHE_DIR="${HOME}/.cache/kakt/flatcar-bin"
# Digest-pinned Gateway API dependency-gate image, pinned to the linux/amd64
# MANIFEST digest rather than the multi-arch index digest. That is deliberate and
# load-bearing: `docker save` of an index digest fails outright, because pulling
# --platform linux/amd64 fetches only that manifest while save insists on writing
# every sibling in the index —
#   Error: unable to create manifests file: NotFound: content digest sha256:3da345bb… not found
# (3da345bb is the arm64 manifest). Pinning the platform digest makes the archive
# self-contained at 17 MiB instead of ~4x that, and needs no crane/skopeo prerequisite.
# Every node here is linux/amd64: the binaries fetched above are linux/amd64 too.
# MUST match the initContainer in
# manifests/providers/hybrid/k8s/cilium-addon-hybrid.yaml and
# manifests/spoke/spoke-bootstrap/cilium-addon-template.yaml — a drift here means the
# node caches one image and the pod requests another, silently reintroducing a pull.
GATE_IMAGE_REF="registry.k8s.io/kubectl:v1.35.10@sha256:4e1d541a3d62f74637974e4c71f3cea91b9a36ffd059ab45e355648898783837"
MODE="provision"

usage() {
  local exit_code="${1:-0}"
  cat <<EOF
Usage: $0 [OPTIONS]

Options:
  --cluster <hub|spoke|all> Target cluster (default: 'all', auto-routes per home-lab.env).
  --spoke <name>       Shorthand to target a specific spoke cluster.
  --node N             Provision only node index N (1-based from home-lab.env).
  --env FILE           Path to environment file (default: scripts/hybrid/home-lab.env)
  --ts-authkey KEY     Tailscale auth-key (auto-loaded from k8-secrets/tailscale/authkey if omitted).
  --vswitch NAME       Hyper-V Virtual Switch name (default: 'Default Switch').
  --host-cpu-max N     Host PROCTHROTTLEMAX percent (default 70, ADR-046 §24.5):
                       the thermal cap that keeps a thin box from tripping ACPI
                       critical and powering off mid-provision.
  --upgrade-flatcar    Re-download fresh Flatcar base VHDX image from current release.
  --memory-gb N        OVERRIDE startup memory in GB (default: per-node from home-lab.env registry).
  --max-memory-gb N    OVERRIDE maximum memory ceiling in GB (default: per-node from registry).
  --min-memory-gb N    OVERRIDE minimum dynamic memory floor in GB (default: per-node from registry).
  --cpus N             OVERRIDE virtual CPU count (default: per-node from registry).
  --disk-gb N          OVERRIDE virtual disk ceiling in GB (default: 100 hub / 60 spoke).
  --verify             Only check Ready status of registered nodes; no changes.
  --force-rebuild      Skip the recovery gate and rebuild the VM from the base image.
  --recover-wait N     Seconds to let an existing VM rejoin before rebuilding (default 300).
  -h, --help           Show this help message

Phases per node:
  [0/6] Offline binary cache verification on Mac, then the recovery gate:
        an existing VM is started and given time to rejoin before it is replaced
  [1/6] SSH reachability gate
  [2/6] Windows host & Hyper-V preparation
  [3/6] Flatcar base VHDX & Ignition ISO bundle preparation
  [4/6] Provision Generation 2 Hyper-V VM with attached Ignition DVD ISO
  [5/6] Wait for Flatcar First-Boot, Tailscale mesh & Kubeadm join
  [6/6] Verify node Ready in target cluster (success gate)

Exit code 0 only if all targeted nodes are Ready.
EOF
  exit "$exit_code"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --cluster)          TARGET_CLUSTER="$2"; shift 2 ;;
    --spoke)            TARGET_CLUSTER="spoke"; HYBRID_SPOKE_NAME="$2"; shift 2 ;;
    --node)             ONLY_NODE="$2"; shift 2 ;;
    --env)              ENV_FILE="$2"; shift 2 ;;
    --ts-authkey)       TS_AUTHKEY="$2"; shift 2 ;;
    --vswitch)          VSWITCH_NAME="$2"; shift 2 ;;
    --host-cpu-max)     HOST_CPU_MAX_PCT="$2"; shift 2 ;;
    --upgrade-flatcar)  UPGRADE_FLATCAR="true"; shift ;;
    --memory-gb)        MEMORY_BYTES="$(($2 * 1024 * 1024 * 1024))"; shift 2 ;;
    --max-memory-gb) MAX_MEMORY_BYTES="$(($2 * 1024 * 1024 * 1024))"; shift 2 ;;
    --min-memory-gb) MIN_MEMORY_BYTES="$(($2 * 1024 * 1024 * 1024))"; shift 2 ;;
    --cpus)          CPU_COUNT="$2"; shift 2 ;;
    --disk-gb)       DISK_SIZE_BYTES="$(($2 * 1024 * 1024 * 1024))"; CLI_DISK_SET=1; shift 2 ;;
    --verify)        MODE="verify"; shift ;;
    --force-rebuild) FORCE_REBUILD=1; shift ;;
    --recover-wait)  RECOVER_WAIT_SECONDS="$2"; shift 2 ;;
    -h|--help)       usage 0 ;;
    *) echo "ERROR: unknown option $1" >&2; usage 1 ;;
  esac
done

# Snapshot explicit CLI capacity (if any) — per-node registry values become the
# default and are applied at each node iteration; CLI values always win.
CLI_MEMORY_BYTES="$MEMORY_BYTES"
CLI_MIN_MEMORY_BYTES="$MIN_MEMORY_BYTES"
CLI_MAX_MEMORY_BYTES="$MAX_MEMORY_BYTES"
CLI_CPU_COUNT="$CPU_COUNT"

REPO_ROOT="$(cd "${HERE}/../.." && pwd)"
export ZERO_OPS_DIR="${REPO_ROOT}"

if [[ ! -f "$ENV_FILE" ]]; then
  echo "ERROR: Environment file not found: $ENV_FILE" >&2
  echo "       Copy home-lab.env.example → home-lab.env and fill in real values." >&2
  exit 1
fi
# An explicit HUB_KUBECONFIG wins over the one the env file sets.
#
# home-lab.env assigns HUB_KUBECONFIG unconditionally, and it names ONE cluster's
# kubeconfig. That is fine while there is one hub; it is wrong the moment a second
# is built, because the caller's cluster is not the one the file names. The
# bootstrap passes the path of the cluster it just created and the assignment threw
# it away, so the join aborted with "HUB_KUBECONFIG not found" pointing at a
# kubeconfig for a different, possibly long-deleted cluster.
#
# The fix lives here rather than in the env file because that file is gitignored:
# it holds real values, so a correction there reaches nobody else.
_caller_hub_kubeconfig="${HUB_KUBECONFIG:-}"

# HYBRID_SPOKE_NAME, for exactly the same reason and from the same file.
#
# home-lab.env:119 assigns it unconditionally too, and the literal it carries --
# spoke-pool-hybrid-dev-01 -- is the naming ADR-082 replaced. A workload cluster
# is now named by the tenant (nutgraf-01), so the assignment silently swapped the
# caller's spoke for one that does not exist. Every lookup keyed on it then asked
# about the wrong cluster: the kubeconfig Secret, the join Secret, the node
# registration.
#
# Observed 2026-09-19: step 10e exported HYBRID_SPOKE_NAME=nutgraf-01, the source
# below replaced it, and the run failed on
#   secret=spoke-pool-hybrid-dev-01-home-worker-join ... token=MISSING
# while nutgraf-01-home-worker-join sat in platform-capi, correctly minted and
# untouched. The message blamed hub-operator for work it had already done.
#
# Here and not in the env file because that file is gitignored -- it holds real
# values, so a correction there reaches nobody else.
_caller_spoke_name="${HYBRID_SPOKE_NAME:-}"

# shellcheck source=/dev/null
source "$ENV_FILE"

if [[ -n "$_caller_hub_kubeconfig" ]]; then
  HUB_KUBECONFIG="$_caller_hub_kubeconfig"
fi
if [[ -n "$_caller_spoke_name" ]]; then
  HYBRID_SPOKE_NAME="$_caller_spoke_name"
fi
unset _caller_hub_kubeconfig _caller_spoke_name

# Auto-fetch Tailscale authkey from k8-secrets if not passed via CLI
DEFAULT_TS_AUTHKEY_FILE="${REPO_ROOT}/k8-secrets/tailscale/authkey"
if [[ -z "$TS_AUTHKEY" && -f "$DEFAULT_TS_AUTHKEY_FILE" ]]; then
  TS_AUTHKEY="$(tr -d '\r\n' < "$DEFAULT_TS_AUTHKEY_FILE")"
fi

if [[ ! -f "$HUB_KUBECONFIG" ]]; then
  echo "ERROR: HUB_KUBECONFIG not found: $HUB_KUBECONFIG" >&2
  echo "       The hub kubeconfig must exist on this machine." >&2
  echo "       Set HUB_KUBECONFIG for the cluster you mean, or correct the value in" >&2
  echo "       $ENV_FILE — note it names one cluster, so it is stale as soon as" >&2
  echo "       a differently named hub is built." >&2
  exit 1
fi

# Locate Mac SSH public keys for core user injection
SSH_PUBKEYS_JSON=""
for key_path in "$HOME/.ssh/"*.pub; do
  if [[ -f "$key_path" ]]; then
    k=$(tr -d '\r\n' < "$key_path")
    if [[ -n "$k" ]]; then
      if [[ -n "$SSH_PUBKEYS_JSON" ]]; then
        SSH_PUBKEYS_JSON="${SSH_PUBKEYS_JSON}, \"${k}\""
      else
        SSH_PUBKEYS_JSON="\"${k}\""
      fi
    fi
  fi
done
if [[ -z "$SSH_PUBKEYS_JSON" ]]; then
  echo "ERROR: No SSH public keys found in ~/.ssh/*.pub" >&2
  exit 1
fi

# ── Prerequisite check for required tools on Mac ─────────────────────────────
for tool in kubectl hdiutil curl ssh scp base64; do
  if ! command -v "$tool" &>/dev/null; then
    echo "ERROR: Required command '${tool}' not found on Mac." >&2
    echo "       Please install '${tool}' (e.g. brew install ${tool}) and re-run." >&2
    exit 1
  fi
done

win_ps() { # Execute PowerShell on the Windows host via SSH (buffered)
  local SSH_TARGET="$1" PS_SCRIPT="$2"
  printf '%s\n' "$PS_SCRIPT" | ssh -o BatchMode=yes -o ConnectTimeout=15 -o StrictHostKeyChecking=no \
    "$SSH_TARGET" "powershell -NoProfile -ExecutionPolicy Bypass -Command -" 2>&1 || true
}

win_ps_stream() { # Stream PowerShell output live from Windows host via SSH
  local SSH_TARGET="$1" PS_SCRIPT="$2" FILTER="${3:-✓|✗|→|===|⚠|Error|Failed|Exception|VM-CAPACITY|HOST-DISK}"
  printf '%s\n' "$PS_SCRIPT" | ssh -o BatchMode=yes -o ConnectTimeout=15 -o StrictHostKeyChecking=no \
    "$SSH_TARGET" "powershell -NoProfile -ExecutionPolicy Bypass -Command -" 2>&1 | \
    grep --line-buffered -E "$FILTER" | sed 's/^/      /' || true
}

# ── Registry naming: ONE definition of the conventions ───────────────────────
# These three facts were previously restated at four call sites between the
# verify loop, the cleanup phase and the main loop. Restating an implicit rule
# is how it drifts, and a drifted copy is exactly what let an index-derived
# cleanup name match a live node and delete it.

# The cluster an entry belongs to: its target-cluster column when set, else the
# legacy positional default (entry 1 = hub, the rest = spoke).
#
# $TARGET_CLUSTER deliberately does NOT participate. --cluster SELECTS which
# registered entries to act on; it never retargets them. The verify loop used to
# apply it here, so `--cluster hub` checked every spoke node against the hub.
cluster_for_entry() {
  local target_field="$1" idx="$2"
  if [[ -n "$target_field" ]]; then printf '%s\n' "$target_field"
  elif [[ "$idx" -gt 1 ]]; then printf '%s\n' "spoke"
  else printf '%s\n' "hub"; fi
}

# The VM / node name for an entry: its hostname column when set, else the
# conventional name. This is the only place the convention is spelled out.
vm_name_for() {
  local host_field="$1" cluster="$2" idx="$3"
  if [[ -n "$host_field" ]]; then printf '%s\n' "$host_field"
  elif [[ "$cluster" == "hub" ]]; then printf '%s\n' "flatcar-hub-node-${idx}"
  else printf '%s\n' "flatcar-spoke-node-${idx}"; fi
}

# Names an entry at index $1 could have carried under older schemes. Cleanup
# candidates only — never authoritative, and always filtered against the real
# registry by cleanup_names_for() before anything is destroyed.
legacy_names_for_idx() {
  local idx="$1"
  printf '%s\n' "flatcar-node-${idx}" "flatcar-hub-node-${idx}" "flatcar-spoke-node-${idx}"
}

# ── Phase 0: Offline binary cache verification on Mac ────────────────────────
phase_prep_binaries() {
  echo "    [0/6] Verifying offline worker binary cache on Mac..."
  mkdir -p "${CACHE_DIR}"

  # Also check /tmp/kakt-flatcar-cache
  if [[ -d "/tmp/kakt-flatcar-cache" ]]; then
    cp -n /tmp/kakt-flatcar-cache/* "${CACHE_DIR}/" 2>/dev/null || true
  fi

  if [[ ! -f "${CACHE_DIR}/kubelet" ]]; then
    echo "      → Downloading kubelet ${K8S_VERSION}..."
    curl -sSL -o "${CACHE_DIR}/kubelet" "https://dl.k8s.io/release/${K8S_VERSION}/bin/linux/amd64/kubelet"
    chmod +x "${CACHE_DIR}/kubelet"
  fi

  if [[ ! -f "${CACHE_DIR}/kubeadm" ]]; then
    echo "      → Downloading kubeadm ${K8S_VERSION}..."
    curl -sSL -o "${CACHE_DIR}/kubeadm" "https://dl.k8s.io/release/${K8S_VERSION}/bin/linux/amd64/kubeadm"
    chmod +x "${CACHE_DIR}/kubeadm"
  fi

  if [[ ! -f "${CACHE_DIR}/kubectl" ]]; then
    echo "      → Downloading kubectl ${K8S_VERSION}..."
    curl -sSL -o "${CACHE_DIR}/kubectl" "https://dl.k8s.io/release/${K8S_VERSION}/bin/linux/amd64/kubectl"
    chmod +x "${CACHE_DIR}/kubectl"
  fi

  local TS_VER_FILE="${CACHE_DIR}/.tailscale-version"
  if [[ ! -f "${CACHE_DIR}/tailscale" || ! -f "${CACHE_DIR}/tailscaled" || ! -f "$TS_VER_FILE" || "$(< "$TS_VER_FILE")" != "$TAILSCALE_VERSION" ]]; then
    echo "      → Downloading Tailscale ${TAILSCALE_VERSION}..."
    local TS_TMP
    TS_TMP=$(mktemp -d /tmp/tailscale-dl-XXXXXX)
    curl -sSL "https://pkgs.tailscale.com/stable/tailscale_${TAILSCALE_VERSION}_amd64.tgz" | tar -xz -C "$TS_TMP" --strip-components=1
    cp "${TS_TMP}/tailscale" "${TS_TMP}/tailscaled" "${CACHE_DIR}/"
    chmod +x "${CACHE_DIR}/tailscale" "${CACHE_DIR}/tailscaled"
    echo "$TAILSCALE_VERSION" > "$TS_VER_FILE"
    rm -rf "$TS_TMP"
  fi

  local CNI_VERSION="v1.6.0"
  if [[ ! -f "${CACHE_DIR}/loopback" ]]; then
    echo "      → Downloading CNI plugins ${CNI_VERSION}..."
    local CNI_TMP
    CNI_TMP=$(mktemp -d /tmp/cni-dl-XXXXXX)
    curl -sSL "https://github.com/containernetworking/plugins/releases/download/${CNI_VERSION}/cni-plugins-linux-amd64-${CNI_VERSION}.tgz" | tar -xz -C "$CNI_TMP"
    cp "$CNI_TMP"/* "${CACHE_DIR}/" 2>/dev/null || true
    chmod +x "${CACHE_DIR}"/* 2>/dev/null || true
    rm -rf "$CNI_TMP"
  fi
  # Gateway API dependency-gate image. cilium-operator schedules on this worker as
  # well as the control plane, and its initContainer (see cilium-addon-hybrid.yaml)
  # blocks until the five Gateway API CRDs are Established. Pulling that image from
  # an external registry during cold boot would make the gate fail HARDER than the
  # race it removes — no operator at all, rather than Gateway API disabled — and this
  # worker is the one on a domestic link (a 4m26s image pull is on record). Control
  # planes are not cached here and do not need to be: `kubeadm config images pull`
  # already makes registry.k8s.io a hard bootstrap dependency there, so the gate adds
  # no new failure domain on those nodes.
  local GATE_IMAGE_TAR="${CACHE_DIR}/gateway-api-gate-image.tar"
  if [[ ! -f "$GATE_IMAGE_TAR" ]]; then
    echo "      → Exporting Gateway API gate image (${GATE_IMAGE_REF})..."
    # --platform is load-bearing on Apple Silicon: the node is linux/amd64 and a
    # silently-pulled arm64 layer set would import cleanly and fail to run.
    docker pull --platform linux/amd64 "${GATE_IMAGE_REF}" >/dev/null
    docker save "${GATE_IMAGE_REF}" -o "$GATE_IMAGE_TAR"
  fi

  echo "      ✓ All worker binaries cached in ${CACHE_DIR}"
}

# ── Phase 2: Windows host & Hyper-V preparation ──────────────────────────────
phase_prep_hyperv() {
  local SSH_TARGET="$1" HOSTNAME="$2"
  echo "    [2/6] Preparing Windows host & Hyper-V..."
  local PS_PREP="
Write-Output '=== [2a/2d] Power & Lid Settings ==='
try {
  powercfg /setacvalueindex SCHEME_CURRENT 4f971e89-eebd-4455-a8de-9e59040e7347 5ca83367-6e45-459f-a27b-476b1d01c936 0
  powercfg /setdcvalueindex SCHEME_CURRENT 4f971e89-eebd-4455-a8de-9e59040e7347 5ca83367-6e45-459f-a27b-476b1d01c936 0
  powercfg /change standby-timeout-ac 0
  powercfg /change hibernate-timeout-ac 0
  powercfg /change monitor-timeout-ac 5
  # Critical-battery action -> Do nothing. These boxes are laptops-as-servers with
  # exhausted batteries reporting 0 mWh while on AC; Windows otherwise enacts the
  # critical action ON AC and hibernates a running node out from under the cluster.
  powercfg /setacvalueindex SCHEME_CURRENT SUB_BATTERY BATACTIONCRIT 0
  powercfg /setdcvalueindex SCHEME_CURRENT SUB_BATTERY BATACTIONCRIT 0
  powercfg /setactive SCHEME_CURRENT
  Write-Output '    ✓ Lid Close -> Do Nothing; Sleep/Hibernate -> Never; Critical battery -> None'
} catch { Write-Output ('    ⚠ Power: ' + \$_.Exception.Message) }

Write-Output '=== [2b/2d] Thermal Protection ==='
# Why this exists (ADR-046 §24.5): on 2026-08-23 a Dell Latitude E7470 (15W
# i5-6300U, 2 cores / 4 threads) hosting two Kubernetes node VMs shut itself down
# four times in one hour:
#     Kernel-Power 86: 'The system was shut down due to a critical thermal event.'
#     ACPI Thermal Zone = Intel(R) Dynamic Platform Thermal Framework, _CRT = 373K
# esifsvc enacts that trip via shutdown.exe as NT AUTHORITY\LOCAL SERVICE, which
# surfaces as event 1074 and looks like a clean administrative shutdown. It is not:
# it is the hardware protecting itself, and no amount of Kubernetes-side work
# survives it. Capping sustained CPU is the only software lever that helps.
try {
  \$g = ((powercfg /getactivescheme) -join ' ') -replace '.*GUID:\s*([0-9a-f-]{36}).*','\$1'
  \$SUBPROC = '54533251-82be-4824-96c1-47b60b740d00'
  \$BOOST   = 'be337238-0d82-4146-a960-4f3749d470c7'
  # Sustained ceiling. 70% keeps a 15W part under its trip point under container
  # load; slower bootstraps are strictly better than a shutdown mid-provision.
  powercfg /setacvalueindex \$g SUB_PROCESSOR PROCTHROTTLEMAX $HOST_CPU_MAX_PCT
  powercfg /setdcvalueindex \$g SUB_PROCESSOR PROCTHROTTLEMAX $HOST_CPU_MAX_PCT
  # Turbo is where the thermal spikes come from and buys little here. The setting
  # is hidden by default on most OEM images, hence the un-hide first.
  powercfg /attributes \$SUBPROC \$BOOST -ATTRIB_HIDE | Out-Null
  powercfg /setacvalueindex \$g \$SUBPROC \$BOOST 0
  powercfg /setdcvalueindex \$g \$SUBPROC \$BOOST 0
  # Active cooling ramps the fan BEFORE throttling. Passive throttles first and
  # lets heat accumulate, which is how the critical trip is reached.
  powercfg /setacvalueindex \$g SUB_PROCESSOR SYSCOOLPOL 1
  powercfg /setdcvalueindex \$g SUB_PROCESSOR SYSCOOLPOL 1
  powercfg /setactive \$g
  Write-Output ('    ✓ CPU max ' + $HOST_CPU_MAX_PCT + '%, turbo off, active cooling')
  \$therm = Get-WinEvent -FilterHashtable @{LogName='System'; Id=86; ProviderName='Microsoft-Windows-Kernel-Power'} -MaxEvents 3 -ErrorAction SilentlyContinue
  if (\$therm) {
    Write-Output '    ⚠ This host HAS shut down on thermal trips before:'
    # The '→' prefix is load-bearing: win_ps_stream filters host output through
    # grep -E '✓|→|===|⚠|Error|Failed|Exception', so an unprefixed line is silently
    # discarded and the warning would print with no evidence under it.
    \$therm | ForEach-Object { Write-Output ('      → ' + \$_.TimeCreated.ToString('yyyy-MM-dd HH:mm:ss')) }
    Write-Output '      → If these continue, the fix is physical: clean the fan, repaste, improve airflow.'
  }
} catch { Write-Output ('    ⚠ Thermal: ' + \$_.Exception.Message) }

Write-Output '=== [2c/2d] Hyper-V Feature Check ==='
try {
  \$os = Get-CimInstance Win32_OperatingSystem -ErrorAction SilentlyContinue
  \$feature = Get-WindowsOptionalFeature -Online -FeatureName Microsoft-Hyper-V -ErrorAction SilentlyContinue
  if (\$feature -and \$feature.State -eq 'Enabled') {
    \$hostInfo = if (\$os) { \$os.Caption.Trim() + ' (Build ' + \$os.BuildNumber + ')' } else { 'Enabled' }
    Write-Output ('    ✓ Hyper-V is enabled (' + \$hostInfo + ')')
  } else {
    Write-Output '    ⚠ Hyper-V not enabled. Enabling Microsoft-Hyper-V...'
    Enable-WindowsOptionalFeature -Online -FeatureName Microsoft-Hyper-V -All -NoRestart | Out-Null
    Write-Output '    ✓ Enabled Hyper-V'
  }
} catch { Write-Output ('    ⚠ Hyper-V feature: ' + \$_.Exception.Message) }

Write-Output '=== [2d/2d] Flatcar Directories & Virtual Switch ==='
try {
  \$kaktDir = 'C:\ProgramData\kakt\flatcar'
  if (!(Test-Path \$kaktDir)) { New-Item -ItemType Directory -Path \$kaktDir -Force | Out-Null }
  
  # Host networking is host prep, not a manual per-box chore. A box that arrived
  # without the switch used to get a warning here and then fail five phases later
  # inside New-VM, with the real cause long scrolled off the screen. Guests boot
  # static ${HOST_SUBNET_PREFIX}.1<idx>/24 with $HOST_GATEWAY_IP as gateway, so the host side of
  # the switch must own .1 and NAT that subnet outbound.
  \$switch = Get-VMSwitch -Name '$VSWITCH_NAME' -ErrorAction SilentlyContinue
  if (!\$switch) {
    Write-Output '    → Switch $VSWITCH_NAME missing - creating Internal switch'
    Write-Output '      NOTE: an Internal switch is host-local. It is correct for a cluster'
    Write-Output '      that lives on ONE box, and breaks silently across two — each box'
    Write-Output '      builds its own $HOST_SUBNET_CIDR island and the guests cannot reach'
    Write-Output '      each other. See scripts/hybrid/README.md and convert-to-external-switch.sh'
    New-VMSwitch -Name '$VSWITCH_NAME' -SwitchType Internal | Out-Null
    \$switch = Get-VMSwitch -Name '$VSWITCH_NAME' -ErrorAction SilentlyContinue
  }
  # The gateway address and NetNat below belong to the Internal layout ONLY.
  # Applying them to an External switch re-imposes the second NAT that
  # convert-to-external-switch.sh exists to remove, so a converted host would be
  # quietly un-converted by the next provisioning run.
  if (\$switch -and \$switch.SwitchType -ne 'Internal') {
    Write-Output ('    ✓ Virtual switch ($VSWITCH_NAME) is ' + \$switch.SwitchType + ' - skipping NAT setup')
    Write-Output 'SWITCH-READY=OK'
  }
  elseif (\$switch) {
    \$ifIdx = (Get-NetAdapter -Name ('vEthernet (' + '$VSWITCH_NAME' + ')') -ErrorAction SilentlyContinue).ifIndex
    if (\$ifIdx -and !(Get-NetIPAddress -InterfaceIndex \$ifIdx -IPAddress '$HOST_GATEWAY_IP' -ErrorAction SilentlyContinue)) {
      New-NetIPAddress -IPAddress '$HOST_GATEWAY_IP' -PrefixLength 24 -InterfaceIndex \$ifIdx -ErrorAction SilentlyContinue | Out-Null
      Write-Output '    ✓ Host IP $HOST_GATEWAY_IP/24 on vEthernet ($VSWITCH_NAME)'
    }
    if (!(Get-NetNat -Name '$HOST_NAT_NAME' -ErrorAction SilentlyContinue)) {
      New-NetNat -Name '$HOST_NAT_NAME' -InternalIPInterfaceAddressPrefix '$HOST_SUBNET_CIDR' -ErrorAction SilentlyContinue | Out-Null
      Write-Output '    ✓ NAT $HOST_NAT_NAME for $HOST_SUBNET_CIDR'
    }
    Write-Output '    ✓ Virtual switch ($VSWITCH_NAME) present'
    Write-Output 'SWITCH-READY=OK'
  } else {
    Write-Output '    ✗ Could not create switch $VSWITCH_NAME. Available switches:'
    Get-VMSwitch | ForEach-Object { Write-Output ('      - ' + \$_.Name) }
  }
  Write-Output 'HYPERV-PREP=OK'
} catch { Write-Output ('HYPERV-PREP=ERR: ' + \$_.Exception.Message) }
"
  win_ps_stream "$SSH_TARGET" "$PS_PREP"

  # Ask the host, do not trust the log line above. Without the switch, New-VM fails
  # five phases later and the run still ends in a five-minute Ready wait.
  local SWITCH_STATE
  SWITCH_STATE=$(win_ps "$SSH_TARGET" "if (Get-VMSwitch -Name '$VSWITCH_NAME' -ErrorAction SilentlyContinue) { 'OK' } else { 'MISSING' }" \
    | tr -d '\r' | grep -Ex 'OK|MISSING' | tail -1 || true)
  if [[ "$SWITCH_STATE" != "OK" ]]; then
    echo "    ✗ Hyper-V switch '${VSWITCH_NAME}' is absent on ${SSH_TARGET} and could not be created." >&2
    echo "      The SSH user must be a local administrator. To create it by hand:" >&2
    echo "      New-VMSwitch -Name '${VSWITCH_NAME}' -SwitchType Internal" >&2
    echo "      New-NetIPAddress -IPAddress ${HOST_GATEWAY_IP} -PrefixLength 24 -InterfaceIndex (Get-NetAdapter -Name 'vEthernet (${VSWITCH_NAME})').ifIndex" >&2
    echo "      New-NetNat -Name ${HOST_NAT_NAME} -InternalIPInterfaceAddressPrefix ${HOST_SUBNET_CIDR}" >&2
    return 1
  fi
}

# ── Phase 3: Flatcar base VHDX & Ignition ISO bundle preparation ─────────────
phase_prep_flatcar_and_ignition() {
  # NODE_IDX is the registry's GLOBAL index -- it sets the VM's IP and SSH port,
  # which must be unique across every VM on the host. JOIN_ORDINAL is the node's
  # position within its OWN cluster, which is how hub-operator keys the join
  # tokens (node-<i>-token, 1-based per spoke). They are different numbers for
  # every spoke node, and conflating them asked for a token that was never minted.
  # $4 is CLUSTER_TARGET, declared further down where it is first used.
  local SSH_TARGET="$1" VM_NAME="$2" NODE_IDX="$3" JOIN_ORDINAL="${5:-$3}"
  echo "    [3/6] Preparing Flatcar base VHDX & Ignition ISO bundle (${VM_NAME})..."

  # 1. Ensure Flatcar base VHDX exists on Windows Host
  local PS_VHDX="
\$kaktDir = 'C:\ProgramData\kakt\flatcar';
\$baseVhdx = Join-Path \$kaktDir 'flatcar-base.vhdx';
\$rawVhdx = Join-Path \$kaktDir 'flatcar_production_hyperv_vhdx_image.vhdx';
\$zipPath = Join-Path \$kaktDir 'flatcar-base.vhdx.zip';

if ('$UPGRADE_FLATCAR' -eq 'true') {
  Remove-Item \$baseVhdx -Force -ErrorAction SilentlyContinue
  Remove-Item \$rawVhdx -Force -ErrorAction SilentlyContinue
}

# Consolidate legacy named base image if present
if (!(Test-Path \$baseVhdx) -and (Test-Path \$rawVhdx)) {
  Move-Item -Path \$rawVhdx -Destination \$baseVhdx -Force
}

if (!(Test-Path \$baseVhdx)) {
  Write-Output '    → Downloading Flatcar ${FLATCAR_RELEASE_CHANNEL} Hyper-V base image...'
  [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
  # Flatcar publishes the Hyper-V images as .zip. The old .bz2 URL 404s, and the
  # bzip2 it piped into is on neither box anyway - which is how this phase came to
  # print a check mark over a base image it had never produced.
  \$url = 'https://${FLATCAR_RELEASE_CHANNEL}.release.flatcar-linux.net/amd64-usr/current/flatcar_production_hyperv_vhdx_image.vhdx.zip'
  # WebClient streams to disk; Invoke-WebRequest buffers the whole ~470 MB in RAM.
  (New-Object Net.WebClient).DownloadFile(\$url, \$zipPath)

  Write-Output '    → Expanding archive (Expand-Archive, no external tools needed)...'
  Expand-Archive -Path \$zipPath -DestinationPath \$kaktDir -Force
  if (Test-Path \$rawVhdx) {
    Move-Item -Path \$rawVhdx -Destination \$baseVhdx -Force
  }
  Remove-Item \$zipPath -Force -ErrorAction SilentlyContinue
  if (Test-Path \$baseVhdx) {
    Write-Output ('    ✓ Base Flatcar VHDX initialized (' + [math]::Round((Get-Item \$baseVhdx).Length/1MB) + ' MB)')
    Write-Output 'BASE-VHDX=OK'
  } else {
    Write-Output '    ✗ Base Flatcar VHDX was NOT produced - download or expand failed'
  }
} else {
  Write-Output '    ✓ Base Flatcar VHDX already cached'
  Write-Output 'BASE-VHDX=OK'
}
"
  win_ps_stream "$SSH_TARGET" "$PS_VHDX"

  # Same rule as the switch: verify, do not trust. Every later phase copies from
  # this file, and a missing one turns into a cascade of PathNotFound/ObjectNotFound
  # noise that buries its own cause.
  local BASE_STATE
  BASE_STATE=$(win_ps "$SSH_TARGET" "if (Test-Path 'C:\\ProgramData\\kakt\\flatcar\\flatcar-base.vhdx') { 'OK' } else { 'MISSING' }" \
    | tr -d '\r' | grep -Ex 'OK|MISSING' | tail -1 || true)
  if [[ "$BASE_STATE" != "OK" ]]; then
    echo "    ✗ Base Flatcar VHDX is missing on ${SSH_TARGET} — the download or the expand failed." >&2
    echo "      Check the host can reach ${FLATCAR_RELEASE_CHANNEL}.release.flatcar-linux.net and has ~2 GB free." >&2
    return 1
  fi

  # 2. Stop and remove any existing VM carrying this node's names, on EVERY
  #    registered host -- not just this node's current one. A hostname can move
  #    between boxes, and the box it left keeps running a VM with that hostname
  #    unless it is cleaned here. cleanup_names_for() guarantees we never touch
  #    a name that belongs to a different registered node.
  local CLEAN_NAMES
  CLEAN_NAMES="$(cleanup_names_for "$VM_NAME" "$NODE_IDX")"
  local -a CLEAN_ARR=()
  local _cn
  while IFS= read -r _cn; do
    [[ -n "$_cn" ]] && CLEAN_ARR+=("$_cn")
  done <<< "$CLEAN_NAMES"

  local PS_NAME_LIST=""
  for _cn in "${CLEAN_ARR[@]}"; do
    PS_NAME_LIST="${PS_NAME_LIST}'${_cn}',"
  done
  PS_NAME_LIST="${PS_NAME_LIST%,}"

  local PS_STOP="
\$vmNames = @(${PS_NAME_LIST}) | Select-Object -Unique;
foreach (\$name in \$vmNames) {
  \$existingVM = Get-VM -Name \$name -ErrorAction SilentlyContinue
  if (\$existingVM) {
    Stop-VM -Name \$name -Force -TurnOff -ErrorAction SilentlyContinue
    Remove-VM -Name \$name -Force -ErrorAction SilentlyContinue
  }
}
"
  local _clean_host
  while IFS= read -r _clean_host; do
    [[ -z "$_clean_host" ]] && continue
    if [[ "$_clean_host" != "$SSH_TARGET" ]]; then
      echo "    → Sweeping stale VMs named [${CLEAN_ARR[*]}] on ${_clean_host}..."
    fi
    win_ps "$_clean_host" "$PS_STOP" >/dev/null
  done <<< "$REGISTERED_SSH_TARGETS"

  # 2b. Clean up any stale Kubernetes node registration (idempotent across Hub & Spoke)
  echo "    → Cleaning up any stale Kubernetes node registration (${CLEAN_ARR[*]})..."
  kubectl --kubeconfig="${HUB_KUBECONFIG}" delete node "${CLEAN_ARR[@]}" --ignore-not-found=true >/dev/null 2>&1 || true

  local SPOKE_KC
  SPOKE_KC=$(mktemp /tmp/hybrid-spoke-XXXXXX)
  if kubectl --kubeconfig="${HUB_KUBECONFIG}" \
        get secret "${HYBRID_SPOKE_NAME}-kubeconfig" \
        -n platform-capi -o jsonpath='{.data.value}' 2>/dev/null \
        | base64 -d > "$SPOKE_KC" 2>/dev/null; then
    kubectl --kubeconfig="$SPOKE_KC" delete node "${CLEAN_ARR[@]}" --ignore-not-found=true >/dev/null 2>&1 || true
    rm -f "$SPOKE_KC"
  else
    rm -f "$SPOKE_KC"
  fi

  # 3. Fetch or mint join credentials
  local JOIN_TOKEN="" CA_CERT_HASH="" CONTROL_PLANE_ENDPOINT=""
  local CLUSTER_TARGET="${4:-$TARGET_CLUSTER}"

  if [[ "$CLUSTER_TARGET" == "hub" ]]; then
    echo "    → Minting Hub bootstrap token for node ${VM_NAME}..."
    local TOKEN_ID TOKEN_SECRET
    TOKEN_ID=$(python3 -c "import secrets, string; print(''.join(secrets.choice(string.ascii_lowercase + string.digits) for _ in range(6)))")
    TOKEN_SECRET=$(python3 -c "import secrets, string; print(''.join(secrets.choice(string.ascii_lowercase + string.digits) for _ in range(16)))")
    JOIN_TOKEN="${TOKEN_ID}.${TOKEN_SECRET}"

    cat <<EOF | kubectl --kubeconfig="${HUB_KUBECONFIG}" apply -f - >/dev/null
apiVersion: v1
kind: Secret
metadata:
  name: bootstrap-token-${TOKEN_ID}
  namespace: kube-system
type: bootstrap.kubernetes.io/token
stringData:
  token-id: "${TOKEN_ID}"
  token-secret: "${TOKEN_SECRET}"
  usage-bootstrap-authentication: "true"
  usage-bootstrap-signing: "true"
  auth-extra-groups: "system:bootstrappers:kubeadm:default-node-token"
EOF
    if [[ ${PIPESTATUS[1]:-1} -ne 0 ]]; then
      echo "    ✗ could not create bootstrap-token-${TOKEN_ID} on the hub" >&2
      echo "      HUB_KUBECONFIG=${HUB_KUBECONFIG}" >&2
      return 1
    fi

    # The token is only usable once the controller has signed cluster-info with
    # it. kubeadm join reads jws-kubeconfig-<id> from the kube-public
    # cluster-info ConfigMap and retries until it appears; if it never does, the
    # node loops for the full join timeout on
    #
    #   could not find a JWS signature in the cluster-info ConfigMap for token ID
    #
    # which is what a node gets when the token it was handed does not exist on
    # the cluster it is joining. That happened: the apply above was unchecked and
    # its output discarded, so a failed create still produced an ISO carrying the
    # token, the VM booted with it, and ten minutes of retries later the phase
    # failed pointing at the node. Meanwhile the only tokens on the hub were
    # kubeadm's own rotating ones, which look exactly like a token that had been
    # created and had expired.
    #
    # Waited for here, before the ISO is built, so a token that will never work
    # is caught while the cause is still on screen.
    echo "    → Waiting for the hub to sign cluster-info with ${TOKEN_ID}..."
    local _jws_ok=0 _jws_wait=0
    while (( _jws_wait < 60 )); do
      if kubectl --kubeconfig="${HUB_KUBECONFIG}" get cm -n kube-public cluster-info \
           -o "jsonpath={.data.jws-kubeconfig-${TOKEN_ID}}" 2>/dev/null | grep -q .; then
        _jws_ok=1
        break
      fi
      sleep 3
      _jws_wait=$(( _jws_wait + 3 ))
    done
    if [[ $_jws_ok -eq 0 ]]; then
      echo "    ✗ the hub never signed cluster-info with token ${TOKEN_ID} (${_jws_wait}s)." >&2
      echo "      The node would retry 'could not find a JWS signature' until it gave up." >&2
      echo "      Check that HUB_KUBECONFIG names the cluster the node joins:" >&2
      echo "        HUB_KUBECONFIG=${HUB_KUBECONFIG}" >&2
      echo "        endpoint the node is given: ${CONTROL_PLANE_ENDPOINT:-<resolved below>}" >&2
      return 1
    fi
    echo "    ✓ cluster-info signed for ${TOKEN_ID}"

    CA_CERT_HASH=$(kubectl --kubeconfig="${HUB_KUBECONFIG}" get cm -n kube-system kube-root-ca.crt -o jsonpath='{.data.ca\.crt}' 2>/dev/null \
      | openssl x509 -pubkey -noout 2>/dev/null \
      | openssl rsa -pubin -outform der 2>/dev/null \
      | openssl dgst -sha256 -hex 2>/dev/null \
      | sed 's/^.* //')

    local SERVER
    SERVER=$(kubectl --kubeconfig="${HUB_KUBECONFIG}" config view --minify -o jsonpath='{.clusters[0].cluster.server}')
    CONTROL_PLANE_ENDPOINT="${SERVER}"
  else
    # Fetch Spoke join credentials from Hub
    local JOIN_SECRET_NAME="${HYBRID_SPOKE_NAME}-home-worker-join"
    JOIN_TOKEN=$(kubectl --kubeconfig="${HUB_KUBECONFIG}" \
      get secret "${JOIN_SECRET_NAME}" -n platform-capi \
      -o jsonpath="{.data.node-${JOIN_ORDINAL}-token}" 2>/dev/null | base64 -d || true)
    CA_CERT_HASH=$(kubectl --kubeconfig="${HUB_KUBECONFIG}" \
      get secret "${JOIN_SECRET_NAME}" -n platform-capi \
      -o jsonpath='{.data.ca-cert-hash}' 2>/dev/null | base64 -d || true)
    CONTROL_PLANE_ENDPOINT=$(kubectl --kubeconfig="${HUB_KUBECONFIG}" \
      get secret "${JOIN_SECRET_NAME}" -n platform-capi \
      -o jsonpath="{.data.control-plane-endpoint}" 2>/dev/null | base64 -d || true)

    if [[ -z "$JOIN_TOKEN" || -z "$CA_CERT_HASH" || -z "$CONTROL_PLANE_ENDPOINT" ]]; then
      # "Wait for hub-operator" was the whole message, and it was usually wrong:
      # the operator had already written this Secret, just not the key being
      # asked for. Say which part is missing, because the remedies differ.
      if [[ -n "$CA_CERT_HASH" && -n "$CONTROL_PLANE_ENDPOINT" && -z "$JOIN_TOKEN" ]]; then
        local _have
        _have=$(kubectl --kubeconfig="${HUB_KUBECONFIG}" get secret "${JOIN_SECRET_NAME}" \
          -n platform-capi -o json 2>/dev/null \
          | tr ',' '\n' | grep -o '"node-[0-9]*-token"' | tr -d '"' | tr '\n' ' ')
        echo "    ✗ ${JOIN_SECRET_NAME} exists but has no node-${JOIN_ORDINAL}-token." >&2
        echo "      It holds: ${_have:-none}" >&2
        echo "      hub-operator mints one token per entry in the claim's home-workers" >&2
        echo "      annotation (1-based). This node is spoke worker #${JOIN_ORDINAL} by the" >&2
        echo "      home-lab.env registry, so the registry declares more spoke workers than" >&2
        echo "      the claim does." >&2
        echo "      Fix: list every spoke worker in home-workers in" >&2
        echo "      registry/clusters/${HYBRID_SPOKE_NAME}/infrastructure/spokepool.yaml," >&2
        echo "      commit and push, and let hub-operator re-mint." >&2
      else
        echo "    ✗ Spoke join credentials not ready on Hub. Wait for hub-operator." >&2
        echo "      secret=${JOIN_SECRET_NAME} ns=platform-capi" >&2
        echo "      token=$([[ -n "$JOIN_TOKEN" ]] && echo present || echo MISSING)" \
             "ca-hash=$([[ -n "$CA_CERT_HASH" ]] && echo present || echo MISSING)" \
             "endpoint=$([[ -n "$CONTROL_PLANE_ENDPOINT" ]] && echo present || echo MISSING)" >&2
      fi
      return 1
    fi
  fi

  # Normalize endpoint (host:port)
  CONTROL_PLANE_ENDPOINT="${CONTROL_PLANE_ENDPOINT#https://}"
  CONTROL_PLANE_ENDPOINT="${CONTROL_PLANE_ENDPOINT#http://}"
  CONTROL_PLANE_ENDPOINT="${CONTROL_PLANE_ENDPOINT%%/*}"

  # 4. Generate Ignition configuration (spec 3.4.0) with base64 data URIs
  echo "    → Generating declarative Ignition config..."
  local TMP_DIR
  TMP_DIR=$(mktemp -d /tmp/ignition-gen-XXXXXX)
  trap 'rm -rf "$TMP_DIR"' RETURN

  # ADR-107 §2: opt-in sandboxed runtime label
  local NODE_LABELS="workload-location=on-prem,topology.kubernetes.io/zone=on-prem,node.kubernetes.io/exclude-from-external-load-balancers=true$([[ "${SANDBOX_RUNTIME:-}" == "true" ]] && echo ",sandbox.zero-ops.io/runtime=sandboxed")"
  if [[ "$CLUSTER_TARGET" == "hub" ]]; then
    NODE_LABELS="workload-location=on-prem,hub-role=worker,topology.kubernetes.io/zone=on-prem,node.kubernetes.io/exclude-from-external-load-balancers=true$([[ "${SANDBOX_RUNTIME:-}" == "true" ]] && echo ",sandbox.zero-ops.io/runtime=sandboxed")"
  fi

  local HOSTNAME_B64 SYSCTL_B64 MODULES_B64 NETWORK_B64 TS_AUTHKEY_B64 JOURNAL_B64
  HOSTNAME_B64=$(printf '%s' "${VM_NAME}" | base64 | tr -d '\r\n')
  # route_localnet is DECLARED here, not left to the `sysctl -w` in the bootstrap
  # DNAT block below.
  #
  # That block DNATs 127.0.0.1:6443 to the control-plane endpoint so kubelet can
  # reach the API server before Cilium is up, and a DNAT to a loopback address is
  # only routed when route_localnet is set. It was being enabled by `sysctl -w`
  # inside the same `if`, which makes a kernel setting the node depends on both
  # undeclared and non-persistent: it is invisible to anyone reading the node's
  # sysctl configuration, it is skipped entirely when that branch does not run,
  # and it does not survive a reboot on its own.
  #
  # It also produced a difference between node classes that nothing recorded: home
  # workers ended up with route_localnet=1 as a side effect while the Hetzner
  # control plane, provisioned by CAPI and never by this script, has it at 0.
  SYSCTL_B64=$(printf 'net.ipv4.ip_forward = 1\nnet.bridge.bridge-nf-call-iptables = 1\nnet.bridge.bridge-nf-call-ip6tables = 1\nnet.ipv4.conf.all.route_localnet = 1\nfs.inotify.max_user_watches = 524288\nfs.inotify.max_user_instances = 8192\n' | base64 | tr -d '\r\n')
  MODULES_B64=$(printf 'overlay\nbr_netfilter\n' | base64 | tr -d '\r\n')
  local STATIC_NET="[Match]
Name=eth*
Name=!cilium_* !lxc* !tailscale*

[Network]
DHCP=no
Address=172.30.0.$((10 + NODE_IDX))/24
Gateway=172.30.0.1
DNS=8.8.8.8
DNS=1.1.1.1
"
  NETWORK_B64=$(printf '%s' "$STATIC_NET" | base64 | tr -d '\r\n')
  TS_AUTHKEY_B64=$(printf '%s' "${TS_AUTHKEY}" | base64 | tr -d '\r\n')
  # 24-hour log bound, declared rather than defaulted — same argument as the
  # route_localnet sysctl above: a default nobody set is a default nobody can
  # see failing. journald's own ceiling is a PERCENTAGE of the filesystem
  # (10%, i.e. ~6 GB on a 60 GB VHDX) with no age bound at all, and on
  # 2026-09-25 the Windows host behind these disks hit 0 bytes free and
  # Hyper-V paused both spoke VMs ("Disk Full") while kubelet — which measures
  # the guest's disk and can never see the host's — raised no pressure event.
  # Every byte the guest is free to grow therefore carries its own bound: 24
  # hours old, 500 MB total. free-spoke.sh applies the same file to nodes
  # provisioned before this line existed.
  JOURNAL_B64=$(printf '[Journal]\nMaxRetentionSec=1day\nSystemMaxUse=500M\n' | base64 | tr -d '\r\n')
  HOST_DISK_B64=$(base64 < "${HERE}/kubelet-host-disk.conf" | tr -d '\r\n')

  local ISO_ROOT="${TMP_DIR}/iso-root"
  mkdir -p "${ISO_ROOT}/ignition" "${ISO_ROOT}/bin"

  # Helper scripts on ISO
  cat <<'SH_EOF' > "${ISO_ROOT}/bin/setup-node.sh"
#!/bin/sh
mkdir -p /opt/bin /opt/cni/bin /etc/cni/net.d /var/lib/tailscale /var/lib/kubelet /etc/kubernetes/manifests /media/iso /etc/containerd
mount -t iso9660 /dev/disk/by-label/config-2 /media/iso 2>/dev/null || mount -t iso9660 /dev/disk/by-label/CONFIG-2 /media/iso 2>/dev/null || mount -t iso9660 /dev/sr0 /media/iso 2>/dev/null || true
if [ -d /media/iso/bin ]; then
  cp -n /media/iso/bin/* /opt/bin/
  chmod +x /opt/bin/*
  # Import the Gateway API gate image into containerd's k8s.io namespace so the
  # cilium-operator initContainer resolves it locally under IfNotPresent. Imported
  # after containerd is up; the join path below waits on containerd anyway, and a
  # failure here is logged rather than fatal — the pod can still pull if the network
  # happens to be available, which is strictly better than refusing to boot.
  if [ -f /media/iso/bin/gateway-api-gate-image.tar ]; then
    for _ in $(seq 1 30); do
      [ -S /run/containerd/containerd.sock ] && break
      sleep 2
    done
    # --base-name + --digests create a DIGEST-NAMED image, which is what the pod
    # reference resolves against under IfNotPresent. Importing without them leaves
    # the content addressable but unnamed, and the kubelet would pull anyway —
    # a silently hollow cache.
    ctr -n k8s.io images import --base-name registry.k8s.io/kubectl --digests \
      /media/iso/bin/gateway-api-gate-image.tar \
      || echo "[setup-node] WARN: gate image import failed; cilium-operator may need a registry pull"
  fi
  for cni_tool in loopback host-local portmap bridge bandwidth firewall tuning vlan macvlan ipvlan; do
    [ -f "/media/iso/bin/${cni_tool}" ] && cp -n "/media/iso/bin/${cni_tool}" /opt/cni/bin/
  done
  chmod +x /opt/cni/bin/* 2>/dev/null || true
fi
modprobe overlay || true
modprobe br_netfilter || true
sysctl --system || true

# Cilium BPF & cgroup2 prep
mkdir -p /sys/fs/bpf /run/cilium/cgroupv2 /sys/fs/cgroup/kubepods.slice /sys/fs/cgroup/kubepods-burstable.slice /sys/fs/cgroup/kubepods-besteffort.slice
mount -t bpf bpf /sys/fs/bpf 2>/dev/null || true
mount --make-shared /sys/fs/bpf 2>/dev/null || true
mount -t cgroup2 none /run/cilium/cgroupv2 2>/dev/null || true
mount --make-shared /run/cilium/cgroupv2 2>/dev/null || true

# Bootstrap DNAT for Cilium if kubelet.conf exists
if [ -f /etc/kubernetes/kubelet.conf ]; then
  EP=$(awk '/server:/{print $2}' /etc/kubernetes/kubelet.conf 2>/dev/null | sed 's#https://##')
  CP_HOST=${EP%%:*}; CP_PORT=${EP##*:}
  if [ -n "$CP_HOST" ] && [ -n "$CP_PORT" ]; then
    sysctl -w net.ipv4.conf.all.route_localnet=1 2>/dev/null || true
    iptables -t nat -I OUTPUT 1 -d 10.96.0.1 -p tcp --dport 443 -j DNAT --to-destination "${CP_HOST}:${CP_PORT}" 2>/dev/null || true
    iptables -t nat -I PREROUTING 1 -d 10.96.0.1 -p tcp --dport 443 -j DNAT --to-destination "${CP_HOST}:${CP_PORT}" 2>/dev/null || true
    iptables -t nat -I OUTPUT 1 -d 127.0.0.1 -p tcp --dport 6443 -j DNAT --to-destination "${CP_HOST}:${CP_PORT}" 2>/dev/null || true
    iptables -t nat -I POSTROUTING 1 -d "${CP_HOST}" -j MASQUERADE 2>/dev/null || true
  fi
fi

containerd config default > /etc/containerd/config.toml 2>/dev/null || true
sed -i 's/SystemdCgroup = false/SystemdCgroup = true/' /etc/containerd/config.toml 2>/dev/null || true

cat <<'CRI_EOF' > /etc/crictl.yaml
runtime-endpoint: unix:///run/containerd/containerd.sock
image-endpoint: unix:///run/containerd/containerd.sock
timeout: 10
debug: false
CRI_EOF

ip addr add 172.30.0.__NODE_IP__/24 dev eth0 2>/dev/null || true
ip link set eth0 up 2>/dev/null || true
ip route add default via 172.30.0.1 dev eth0 2>/dev/null || true
systemctl restart systemd-networkd 2>/dev/null || true
SH_EOF
  sed -i '' "s/__NODE_IP__/$((10 + NODE_IDX))/g" "${ISO_ROOT}/bin/setup-node.sh" 2>/dev/null || sed -i "s/__NODE_IP__/$((10 + NODE_IDX))/g" "${ISO_ROOT}/bin/setup-node.sh"
  chmod +x "${ISO_ROOT}/bin/setup-node.sh"

  cat <<'SH_EOF' > "${ISO_ROOT}/bin/join-cluster.sh"
#!/bin/sh
export PATH="/opt/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:$PATH"

for i in $(seq 1 30); do
  [ -S /run/containerd/containerd.sock ] && break
  sleep 1
done

for i in $(seq 1 30); do
  TS_IP=$(/opt/bin/tailscale ip -4 2>/dev/null || true)
  [ -n "$TS_IP" ] && break
  sleep 1
done

echo "[join] Starting kubeadm join against __ENDPOINT__ (node: __VM_NAME__, tailscale IP: ${TS_IP})..."
for i in $(seq 1 30); do
  if /opt/bin/kubeadm join __ENDPOINT__ \
      --token __TOKEN__ \
      --discovery-token-ca-cert-hash sha256:__HASH__ \
      --node-name __VM_NAME__ \
      --cri-socket unix:///run/containerd/containerd.sock \
      --ignore-preflight-errors=all \
      --v=2 2>&1; then
    # 1. IMMEDIATELY insert bootstrap DNAT: 10.96.0.1:443 & status.hostIP:6443 -> Control Plane Endpoint
    EP=$(awk '/server:/{print $2}' /etc/kubernetes/kubelet.conf 2>/dev/null | sed 's#https://##')
    CP_HOST=${EP%%:*}; CP_PORT=${EP##*:}
    if [ -n "$CP_HOST" ] && [ -n "$CP_PORT" ]; then
      sysctl -w net.ipv4.conf.all.route_localnet=1 2>/dev/null || true
      iptables -t nat -I OUTPUT 1 -d 10.96.0.1 -p tcp --dport 443 -j DNAT --to-destination "${CP_HOST}:${CP_PORT}" 2>/dev/null || true
      iptables -t nat -I PREROUTING 1 -d 10.96.0.1 -p tcp --dport 443 -j DNAT --to-destination "${CP_HOST}:${CP_PORT}" 2>/dev/null || true
      if [ -n "$TS_IP" ]; then
        iptables -t nat -I OUTPUT 1 -d "${TS_IP}" -p tcp --dport 6443 -j DNAT --to-destination "${CP_HOST}:${CP_PORT}" 2>/dev/null || true
        iptables -t nat -I PREROUTING 1 -d "${TS_IP}" -p tcp --dport 6443 -j DNAT --to-destination "${CP_HOST}:${CP_PORT}" 2>/dev/null || true
      fi
      iptables -t nat -I OUTPUT 1 -d 127.0.0.1 -p tcp --dport 6443 -j DNAT --to-destination "${CP_HOST}:${CP_PORT}" 2>/dev/null || true
      iptables -t nat -I POSTROUTING 1 -d "${CP_HOST}" -j MASQUERADE 2>/dev/null || true
      echo "[join] ✓ Inserted bootstrap DNAT: 10.96.0.1:443 & ${TS_IP}:6443 -> ${CP_HOST}:${CP_PORT}"
    fi

    # 2. Dynamic kubelet --node-ip assertion and restart
    TS_IP=$(/opt/bin/tailscale ip -4 2>/dev/null || true)
    if [ -n "$TS_IP" ] && [ -f /var/lib/kubelet/kubeadm-flags.env ]; then
      sed -i "s/KUBELET_KUBEADM_ARGS=\"/KUBELET_KUBEADM_ARGS=\"--node-ip=${TS_IP} /g" /var/lib/kubelet/kubeadm-flags.env 2>/dev/null || true
    fi
    systemctl restart kubelet
    echo "[join] ✓ kubeadm join succeeded"

    # 3. Tailscale carries NODE traffic only (ADR-046 addendum: Tailscale is a
    #    node-level underlay). Cilium runs routing-mode=tunnel, so cross-node pod
    #    traffic is VXLAN-encapsulated between tailnet IPs and pod IPs never appear
    #    on the wire. Advertising/accepting podCIDR subnet routes is therefore
    #    unnecessary AND harmful: tailscaled installs them into table 52, whose
    #    ip rule (5270) precedes main (32766), so they shadow Cilium's tunnel route
    #    for host-originated traffic to remote pods. Deliberately NOT advertised.
    echo "[join] ✓ Tailscale left as node-only underlay (no podCIDR advertisement)"

    exit 0
  fi
  echo "[join] Join attempt $i failed, retrying in 3s..."
  sleep 3
done
exit 1
SH_EOF
  sed -i '' "s|__ENDPOINT__|${CONTROL_PLANE_ENDPOINT}|g; s|__TOKEN__|${JOIN_TOKEN}|g; s|__HASH__|${CA_CERT_HASH}|g; s|__VM_NAME__|${VM_NAME}|g" "${ISO_ROOT}/bin/join-cluster.sh" 2>/dev/null || sed -i "s|__ENDPOINT__|${CONTROL_PLANE_ENDPOINT}|g; s|__TOKEN__|${JOIN_TOKEN}|g; s|__HASH__|${CA_CERT_HASH}|g; s|__VM_NAME__|${VM_NAME}|g" "${ISO_ROOT}/bin/join-cluster.sh"
  chmod +x "${ISO_ROOT}/bin/join-cluster.sh"

  # Cilium bootstrap-DNAT cleanup (ADR-046 §13). join-cluster.sh inserts
  # iptables rules (10.96.0.1:443 / <tailnet-ip>:6443 -> CP endpoint) so
  # kubelet/Cilium can reach the API server BEFORE Cilium's BPF kube-proxy
  # replacement is loaded. Once Cilium is Ready, those rules shadow the BPF
  # datapath (and go stale if the CP endpoint rotates) — this oneshot removes
  # them after the cilium-agent container is Running.
  cat <<'SH_EOF' > "${ISO_ROOT}/bin/cilium-bootstrap-cleanup.sh"
#!/bin/sh
export PATH="/opt/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:$PATH"

for i in $(seq 1 90); do
  if crictl ps --name cilium-agent 2>/dev/null | grep -q "Running"; then
    break
  fi
  sleep 2
done

sleep 10

[ -f /etc/kubernetes/kubelet.conf ] || exit 0
EP=$(awk '/server:/{print $2}' /etc/kubernetes/kubelet.conf 2>/dev/null | sed 's#https://##')
CP_HOST=${EP%%:*}; CP_PORT=${EP##*:}
TS_IP=$(/opt/bin/tailscale ip -4 2>/dev/null || true)
[ -n "$CP_HOST" ] && [ -n "$CP_PORT" ] || exit 0

# setup-node.sh bootstrap rules
iptables -t nat -D OUTPUT -d 10.96.0.1 -p tcp --dport 443 -j DNAT --to-destination "${CP_HOST}:${CP_PORT}" 2>/dev/null || true
iptables -t nat -D PREROUTING -d 10.96.0.1 -p tcp --dport 443 -j DNAT --to-destination "${CP_HOST}:${CP_PORT}" 2>/dev/null || true
iptables -t nat -D OUTPUT -d 127.0.0.1 -p tcp --dport 6443 -j DNAT --to-destination "${CP_HOST}:${CP_PORT}" 2>/dev/null || true
iptables -t nat -D POSTROUTING -d "${CP_HOST}" -j MASQUERADE 2>/dev/null || true
# join-cluster.sh bootstrap rules (node tailnet IP variants)
if [ -n "$TS_IP" ]; then
  iptables -t nat -D OUTPUT -d "${TS_IP}" -p tcp --dport 6443 -j DNAT --to-destination "${CP_HOST}:${CP_PORT}" 2>/dev/null || true
  iptables -t nat -D PREROUTING -d "${TS_IP}" -p tcp --dport 6443 -j DNAT --to-destination "${CP_HOST}:${CP_PORT}" 2>/dev/null || true
fi
echo "[cleanup] ✓ Removed bootstrap DNAT rules (Cilium BPF kube-proxy replacement active)"
exit 0
SH_EOF
  chmod +x "${ISO_ROOT}/bin/cilium-bootstrap-cleanup.sh"

  # Dynamic kubelet --node-ip re-assertion (ADR-046 §13). The kubelet.service
  # ExecStartPre runs this before EVERY kubelet start so the Node/CiliumNode
  # advertise the tailnet IPv4 (never the home-LAN 172.30.0.x), dynamically —
  # never hardcoded. Survives reboots and kubeadm upgrades. Idempotent.
  cat <<'SH_EOF' > "${ISO_ROOT}/bin/dynamic-node-ip.sh"
#!/bin/sh
TS_BIN="${TS_BIN:-$(command -v tailscale 2>/dev/null || echo /opt/bin/tailscale)}"
FLAGS=/var/lib/kubelet/kubeadm-flags.env
[ -f "$FLAGS" ] || exit 0
[ -x "$TS_BIN" ] || exit 0
TS_IP="$($TS_BIN ip -4 2>/dev/null)" || exit 0
[ -n "$TS_IP" ] || exit 0
sed -i "s/ --node-ip=[0-9.]*//g" "$FLAGS"
sed -i "s/KUBELET_KUBEADM_ARGS=\"/KUBELET_KUBEADM_ARGS=\"--node-ip=${TS_IP} /g" "$FLAGS"
exit 0
SH_EOF
  chmod +x "${ISO_ROOT}/bin/dynamic-node-ip.sh"

  local IGN_FILE="${ISO_ROOT}/config.ign"
  cat <<EOF > "$IGN_FILE"
{
  "ignition": {
    "version": "3.4.0"
  },
  "storage": {
    "files": [
      {
        "path": "/etc/hostname",
        "mode": 420,
        "overwrite": true,
        "contents": {
          "source": "data:text/plain;charset=utf-8;base64,${HOSTNAME_B64}"
        }
      },
      {
        "path": "/etc/sysctl.d/99-kubernetes.conf",
        "mode": 420,
        "overwrite": true,
        "contents": {
          "source": "data:text/plain;charset=utf-8;base64,${SYSCTL_B64}"
        }
      },
      {
        "path": "/etc/modules-load.d/kubernetes.conf",
        "mode": 420,
        "overwrite": true,
        "contents": {
          "source": "data:text/plain;charset=utf-8;base64,${MODULES_B64}"
        }
      },
      {
        "path": "/etc/systemd/network/10-static.network",
        "mode": 420,
        "overwrite": true,
        "contents": {
          "source": "data:text/plain;charset=utf-8;base64,${NETWORK_B64}"
        }
      },
      {
        "path": "/var/lib/tailscale/authkey",
        "mode": 384,
        "overwrite": true,
        "contents": {
          "source": "data:text/plain;charset=utf-8;base64,${TS_AUTHKEY_B64}"
        }
      },
      {
        "path": "/etc/systemd/journald.conf.d/24h.conf",
        "mode": 420,
        "overwrite": true,
        "contents": {
          "source": "data:text/plain;charset=utf-8;base64,${JOURNAL_B64}"
        }
      },
      {
        "path": "/etc/kubernetes/kubelet.conf.d/50-host-disk.conf",
        "mode": 420,
        "overwrite": true,
        "contents": {
          "source": "data:text/plain;charset=utf-8;base64,${HOST_DISK_B64}"
        }
      }
    ]
  },
  "passwd": {
    "users": [
      {
        "name": "core",
        "sshAuthorizedKeys": [
          ${SSH_PUBKEYS_JSON}
        ]
      }
    ]
  },
  "systemd": {
    "units": [
      {
        "name": "containerd.service",
        "enabled": true
      },
      {
        "name": "k8s-install.service",
        "enabled": true,
        "contents": "[Unit]\nDescription=Install Kubernetes & Tailscale Binaries from Local DVD\nAfter=local-fs.target\n\n[Service]\nType=oneshot\nRemainAfterExit=yes\nExecStart=/bin/sh -c 'mkdir -p /media/iso; mount -t iso9660 /dev/disk/by-label/config-2 /media/iso 2>/dev/null || mount -t iso9660 /dev/disk/by-label/CONFIG-2 /media/iso 2>/dev/null || mount -t iso9660 /dev/sr0 /media/iso 2>/dev/null || true; [ -f /media/iso/bin/setup-node.sh ] && /bin/sh /media/iso/bin/setup-node.sh'\n\n[Install]\nWantedBy=multi-user.target\n"
      },
      {
        "name": "tailscaled.service",
        "enabled": true,
        "contents": "[Unit]\nDescription=Tailscale Node Agent\nAfter=k8s-install.service\nWants=k8s-install.service\n\n[Service]\nExecStartPre=/usr/bin/mkdir -p /var/lib/tailscale /opt/bin\nExecStart=/opt/bin/tailscaled --state=/var/lib/tailscale/tailscaled.state --socket=/run/tailscale/tailscaled.sock\nExecStartPost=/bin/sh -c 'sleep 2; /opt/bin/tailscale up --authkey=${TS_AUTHKEY} --hostname=${VM_NAME}'\nRestart=always\nRestartSec=5\n\n[Install]\nWantedBy=multi-user.target\n"
      },
      {
        "name": "kubelet.service",
        "enabled": true,
        "contents": "[Unit]\nDescription=kubelet: The Kubernetes Node Agent\nDocumentation=https://kubernetes.io/docs/\nWants=containerd.service tailscaled.service\nAfter=containerd.service tailscaled.service\nConditionPathExists=/var/lib/kubelet/config.yaml\n\n[Service]\nEnvironment=\"KUBELET_EXTRA_ARGS=--node-labels=${NODE_LABELS} --provider-id=unmanaged://${VM_NAME}\"\nEnvironmentFile=-/var/lib/kubelet/kubeadm-flags.env\nExecStartPre=/opt/bin/dynamic-node-ip.sh\nExecStart=/opt/bin/kubelet --config=/var/lib/kubelet/config.yaml --config-dir=/etc/kubernetes/kubelet.conf.d --bootstrap-kubeconfig=/etc/kubernetes/bootstrap-kubelet.conf --kubeconfig=/etc/kubernetes/kubelet.conf \$KUBELET_EXTRA_ARGS \$KUBELET_KUBEADM_ARGS\nRestart=always\nStartLimitInterval=0\nRestartSec=10\n\n[Install]\nWantedBy=multi-user.target\n"
      },
      {
        "name": "kubeadm-join.service",
        "enabled": true,
        "contents": "[Unit]\nDescription=Join Kubernetes Cluster via Kubeadm\nAfter=tailscaled.service containerd.service\nWants=tailscaled.service containerd.service\nConditionPathExists=!/etc/kubernetes/kubelet.conf\n\n[Service]\nType=oneshot\nRemainAfterExit=yes\nExecStart=/opt/bin/join-cluster.sh\n\n[Install]\nWantedBy=multi-user.target\n"
      },
      {
        "name": "cilium-bootstrap-cleanup.service",
        "enabled": true,
        "contents": "[Unit]\nDescription=Remove Cilium bootstrap DNAT rules after Cilium is Ready\nAfter=kubeadm-join.service tailscaled.service\nWants=kubeadm-join.service\nConditionPathExists=/etc/kubernetes/kubelet.conf\n\n[Service]\nType=oneshot\nRemainAfterExit=yes\nExecStart=/opt/bin/cilium-bootstrap-cleanup.sh\n\n[Install]\nWantedBy=multi-user.target\n"
      }
    ]
  }
}
EOF

  # 5. Populate ISO bundle (supports both Ignition & official Flatcar config-2 config-drive)
  mkdir -p "${ISO_ROOT}/openstack/latest"
  cat <<EOF > "${ISO_ROOT}/openstack/latest/meta_data.json"
{
  "hostname": "${VM_NAME}",
  "name": "${VM_NAME}",
  "public_keys": {
    "core": "$(head -n 1 "$HOME/.ssh/"*.pub | tr -d '\r\n')"
  }
}
EOF

  cat <<EOF > "${ISO_ROOT}/openstack/latest/user_data"
#cloud-config

ssh_authorized_keys:
$(awk '{print "  - " $0}' "$HOME/.ssh/"*.pub)

write_files:
  - path: /etc/sysctl.d/99-kubernetes.conf
    permissions: '0644'
    content: |
      net.ipv4.ip_forward = 1
      net.bridge.bridge-nf-call-iptables = 1
      net.bridge.bridge-nf-call-ip6tables = 1
      fs.inotify.max_user_watches = 524288
      fs.inotify.max_user_instances = 8192

  - path: /etc/modules-load.d/kubernetes.conf
    permissions: '0644'
    content: |
      overlay
      br_netfilter

  - path: /etc/systemd/network/10-static.network
    permissions: '0644'
    content: |
      [Match]
      Name=eth*
      Name=!cilium_* !lxc* !tailscale*

      [Network]
      DHCP=no
      Address=172.30.0.$((10 + NODE_IDX))/24
      Gateway=172.30.0.1
      DNS=8.8.8.8
      DNS=1.1.1.1

  - path: /var/lib/tailscale/authkey
    permissions: '0600'
    content: |
      ${TS_AUTHKEY}

  - path: /etc/systemd/journald.conf.d/24h.conf
    permissions: '0644'
    content: |
      [Journal]
      MaxRetentionSec=1day
      SystemMaxUse=500M

  - path: /etc/kubernetes/kubelet.conf.d/50-host-disk.conf
    permissions: '0644'
    content: |
$(sed 's/^/      /' "${HERE}/kubelet-host-disk.conf")

coreos:
  units:
    - name: systemd-networkd.service
      command: restart

    - name: containerd.service
      command: start

    - name: k8s-install.service
      command: start
      content: |
        [Unit]
        Description=Install Kubernetes & Tailscale Binaries from Local DVD
        After=local-fs.target

        [Service]
        Type=oneshot
        RemainAfterExit=yes
        ExecStart=/bin/sh -c 'mkdir -p /media/iso; mount -t iso9660 /dev/disk/by-label/config-2 /media/iso 2>/dev/null || mount -t iso9660 /dev/disk/by-label/CONFIG-2 /media/iso 2>/dev/null || mount -t iso9660 /dev/sr0 /media/iso 2>/dev/null || true; [ -f /media/iso/bin/setup-node.sh ] && /bin/sh /media/iso/bin/setup-node.sh'

        [Install]
        WantedBy=multi-user.target

    - name: tailscaled.service
      command: start
      content: |
        [Unit]
        Description=Tailscale Node Agent
        After=k8s-install.service
        Wants=k8s-install.service

        [Service]
        ExecStartPre=/usr/bin/mkdir -p /var/lib/tailscale /opt/bin
        ExecStart=/opt/bin/tailscaled --state=/var/lib/tailscale/tailscaled.state --socket=/run/tailscale/tailscaled.sock
        ExecStartPost=/bin/sh -c 'sleep 2; /opt/bin/tailscale up --authkey=${TS_AUTHKEY} --hostname=${VM_NAME}'
        Restart=always
        RestartSec=5

        [Install]
        WantedBy=multi-user.target

    - name: kubelet.service
      command: start
      content: |
        [Unit]
        Description=kubelet: The Kubernetes Node Agent
        Documentation=https://kubernetes.io/docs/
        Wants=containerd.service tailscaled.service
        After=containerd.service tailscaled.service
        ConditionPathExists=/var/lib/kubelet/config.yaml

        [Service]
        Environment="KUBELET_EXTRA_ARGS=--node-labels=${NODE_LABELS} --provider-id=unmanaged://${VM_NAME}"
        EnvironmentFile=-/var/lib/kubelet/kubeadm-flags.env
        ExecStartPre=/opt/bin/dynamic-node-ip.sh
        ExecStart=/opt/bin/kubelet --config=/var/lib/kubelet/config.yaml --config-dir=/etc/kubernetes/kubelet.conf.d --bootstrap-kubeconfig=/etc/kubernetes/bootstrap-kubelet.conf --kubeconfig=/etc/kubernetes/kubelet.conf \$KUBELET_EXTRA_ARGS \$KUBELET_KUBEADM_ARGS
        Restart=always
        StartLimitInterval=0
        RestartSec=10

        [Install]
        WantedBy=multi-user.target

    - name: kubeadm-join.service
      command: start
      content: |
        [Unit]
        Description=Join Kubernetes Cluster via Kubeadm
        After=tailscaled.service containerd.service
        Wants=tailscaled.service containerd.service
        ConditionPathExists=!/etc/kubernetes/kubelet.conf

        [Service]
        Type=oneshot
        RemainAfterExit=yes
        ExecStart=/opt/bin/join-cluster.sh

        [Install]
        WantedBy=multi-user.target

    - name: cilium-bootstrap-cleanup.service
      command: start
      content: |
        [Unit]
        Description=Remove Cilium bootstrap DNAT rules after Cilium is Ready
        After=kubeadm-join.service tailscaled.service
        Wants=kubeadm-join.service
        ConditionPathExists=/etc/kubernetes/kubelet.conf

        [Service]
        Type=oneshot
        RemainAfterExit=yes
        ExecStart=/opt/bin/cilium-bootstrap-cleanup.sh

        [Install]
        WantedBy=multi-user.target

    - name: iso-cleanup.service
      command: start
      content: |
        [Unit]
        Description=Unmount Ignition ISO to prevent false DiskPressure condition
        After=local-fs.target
        ConditionPathIsMountPoint=/media/iso

        [Service]
        Type=oneshot
        RemainAfterExit=yes
        ExecStart=/bin/umount -f /media/iso

        [Install]
        WantedBy=multi-user.target
EOF

  cp "$IGN_FILE" "${ISO_ROOT}/ignition/config.ign"
  cp "${CACHE_DIR}/"* "${ISO_ROOT}/bin/"

  echo "    → Packaging standalone config-2 & binary ISO bundle..."
  local ISO_OUT="${TMP_DIR}/config-drive.iso"
  hdiutil makehybrid -iso -joliet -default-volume-name "config-2" -o "$ISO_OUT" "$ISO_ROOT" >/dev/null

  # 6. SCP Ignition config & ISO bundle to Windows host
  echo "    → Uploading Ignition config and binary ISO bundle to host..."
  local REMOTE_IGN="C:/ProgramData/kakt/flatcar/${VM_NAME}-config.ign"
  local REMOTE_ISO="C:/ProgramData/kakt/flatcar/${VM_NAME}-ignition.iso"
  # The previous VM must let go of the ISO first.
  #
  # Hyper-V holds an open handle on a mounted DVD image, so writing over
  # ${VM_NAME}-ignition.iso while the old VM still exists fails -- and both scps
  # below were unchecked, so it failed silently and left YESTERDAY'S ISO in place.
  # The VM was then destroyed and recreated around that stale image, booting with
  # the bootstrap token from an earlier run. The node retried
  #
  #   could not find a JWS signature in the cluster-info ConfigMap for token ID
  #
  # for the full join timeout, against a token the hub had never been given. The
  # giveaway was on the host: the .ign written this run, the .iso a day older.
  #
  # Removing the VM here rather than where it is recreated: the handle has to be
  # gone before the upload, and that is this side of the call.
  # One statement, not a multi-line block. win_ps feeds the script to
  # `powershell -Command -`, which reads stdin a line at a time; a multi-line
  # if/for is evaluated as it arrives and did not run, while `|| true` swallowed
  # the evidence. Semicolons keep it to one line, and the result is CHECKED below
  # rather than assumed.
  win_ps "$SSH_TARGET" \
    "Stop-VM -Name '${VM_NAME}' -Force -TurnOff -ErrorAction SilentlyContinue; Remove-VM -Name '${VM_NAME}' -Force -ErrorAction SilentlyContinue; Start-Sleep -Seconds 3" \
    >/dev/null 2>&1 || true

  # The precondition is not "the VM is gone", it is "the file can be written".
  # Ask that directly: an open handle from anything at all produces the same
  # silent scp refusal, and this is the question the upload actually depends on.
  local _iso_writable
  _iso_writable=$(win_ps "$SSH_TARGET" \
    "if (Test-Path '${REMOTE_ISO}') { try { \$f=[IO.File]::Open('${REMOTE_ISO}','Open','Write'); \$f.Close(); 'WRITABLE' } catch { 'LOCKED' } } else { 'ABSENT' }" \
    2>/dev/null | tr -d '\r' | grep -oE 'WRITABLE|LOCKED|ABSENT' | head -1)
  if [[ "$_iso_writable" == "LOCKED" ]]; then
    echo "    ✗ ${REMOTE_ISO} is held open on the host, so the new image cannot replace it." >&2
    echo "      A VM with it mounted as a DVD does this. Remove it and re-run:" >&2
    echo "        Remove-VM -Name '${VM_NAME}' -Force" >&2
    return 1
  fi

  if ! scp -o BatchMode=yes -o StrictHostKeyChecking=no "$IGN_FILE" "${SSH_TARGET}:${REMOTE_IGN}" >/dev/null </dev/null; then
    echo "    ✗ could not upload the Ignition config to ${SSH_TARGET}" >&2
    return 1
  fi
  if ! scp -o BatchMode=yes -o StrictHostKeyChecking=no "$ISO_OUT" "${SSH_TARGET}:${REMOTE_ISO}" >/dev/null </dev/null; then
    echo "    ✗ could not upload the config ISO to ${SSH_TARGET}" >&2
    echo "      Most often the VM still holds it as a mounted DVD. Remove it and re-run:" >&2
    echo "        Remove-VM -Name '${VM_NAME}' -Force" >&2
    return 1
  fi

  # Uploaded is not the same as landed. Compare the size the host reports with
  # the size just built, because a partial or refused write leaves a file that
  # still exists and is still bootable -- which is exactly how a day-old ISO went
  # on being used without anything reporting a failure.
  local _local_iso_size _remote_iso_size
  _local_iso_size=$(wc -c < "$ISO_OUT" | tr -d ' ')
  _remote_iso_size=$(win_ps "$SSH_TARGET" \
    "(Get-Item '${REMOTE_ISO}').Length" 2>/dev/null | tr -d '\r' | grep -oE '^[0-9]+$' | head -1)
  if [[ "$_remote_iso_size" != "$_local_iso_size" ]]; then
    echo "    ✗ ${REMOTE_ISO} is ${_remote_iso_size:-absent} bytes; the ISO just built is ${_local_iso_size}." >&2
    echo "      The host is not holding the image this run produced, so the VM would" >&2
    echo "      boot someone else's join token." >&2
    return 1
  fi
  echo "    ✓ Ignition config & ISO bundle uploaded"

  # kvpctl.exe injects the Ignition config over Hyper-V KVP. It ships with neither
  # Windows nor Flatcar, so a host that never had it hand-placed silently skips the
  # injection and falls back to the config-2 DVD alone.
  local KVP_STATE
  KVP_STATE=$(win_ps "$SSH_TARGET" "if (Test-Path 'C:\\ProgramData\\kakt\\flatcar\\kvpctl.exe') { 'OK' } else { 'MISSING' }" \
    | tr -d '\r' | grep -Ex 'OK|MISSING' | tail -1 || true)
  if [[ "$KVP_STATE" != "OK" ]]; then
    if [[ -f "${CACHE_DIR}/kvpctl.exe" ]]; then
      echo "    → Uploading kvpctl.exe (host is missing it)..."
      scp -o BatchMode=yes -o StrictHostKeyChecking=no "${CACHE_DIR}/kvpctl.exe" \
        "${SSH_TARGET}:C:/ProgramData/kakt/flatcar/kvpctl.exe" >/dev/null </dev/null
      echo "    ✓ kvpctl.exe uploaded"
    else
      echo "    ⚠ kvpctl.exe is on neither this host nor ${CACHE_DIR} — Ignition will rely on the config-2 DVD only" >&2
    fi
  fi
}

# ── Phase 4: Provision Generation 2 Hyper-V VM ───────────────────────────────
phase_provision_flatcar_vm() {
  local SSH_TARGET="$1" VM_NAME="$2" NODE_IDX="$3" CLUSTER_TARGET="${4:-hub}"
  echo "    [4/6] Provisioning Flatcar Gen2 VM via Hyper-V KVP + DVD (${VM_NAME} → ${CLUSTER_TARGET})..."

  # ── Two guards inside the PowerShell below, and why they are there ──────
  #
  # NOTE FOR ANYONE EDITING THE POWERSHELL: it is built as a bash DOUBLE-QUOTED
  # string, so a literal " ends the string early and a backtick runs a command on
  # the Mac. `bash -n` does not catch either -- both stay syntactically valid and
  # fail at run time, which is how a comment mentioning "Failed to reserve
  # resources" once executed local-e2e.sh mid-provision. Keep prose out here,
  # where it is a bash comment; inside, use no double quotes and no backticks.
  #
  # 1. HOST MEMORY CEILING.
  #    $totalRamBytes and $reserveBytes were computed at the top of the block and
  #    never read again: the "5 GB Host OS Reserve" in the capacity line was a
  #    string literal describing a reserve that nothing enforced. A registry row
  #    asking for more RAM than the host has went straight to New-VM, and failed
  #    later and elsewhere as Hyper-V refusing to start the VM with "Failed to
  #    finish reserving resources ... OperationFailed,...StartVM" -- an error
  #    naming neither memory nor the value that caused it.
  #
  #    That is what made it reproducible rather than flaky. The registry asks for
  #    13 GB startup (min = max = startup, so a hard 13 GB, not a range) on a
  #    16 GB host. It fits only when nothing else holds memory -- and on the
  #    FIRST run something always does, because `local-e2e.sh clean` tears down
  #    the cluster, the kind cluster and the tenant repo but touches no Hyper-V
  #    object at all, leaving the previous run's VM Running with its 13 GB
  #    committed. The second run competes with nothing. Fails first, works
  #    second, every time.
  #
  #    Clamped rather than warned about, unlike the vCPU guard: an oversubscribed
  #    CPU still runs, a VM whose startup memory exceeds the host cannot start at
  #    all. Min and max follow startup down -- Hyper-V requires
  #    min <= startup <= max and Set-VMMemory rejects an inconsistent set.
  #
  #    Every value is then aligned DOWN to a 2 MB boundary, which Hyper-V
  #    requires of any memory size. The registry values were whole GB and so
  #    aligned by accident; the clamped ceiling is host-total minus 5 GB, and
  #    TotalPhysicalMemory is usable RAM rather than a round number, so it
  #    lands on an arbitrary byte count. New-VM rejects that with
  #    Failed to modify device Memory / InvalidParameter -- a message that
  #    names the device and not the constraint. Integer modulo, not
  #    math::Floor on a division: these are int64 byte counts.
  #
  # 2. WAITING FOR THE MEMORY TO COME BACK.
  #    The Remove-VM poll waits for the VM OBJECT to disappear, which is
  #    necessary and not sufficient: the object goes almost at once while the
  #    worker process exits and releases the committed memory afterwards. The
  #    wait before Start-VM measures the quantity Start-VM is about to fail on.
  #
  #    AvailableBytes from Win32_PerfRawData_PerfOS_Memory, not
  #    FreePhysicalMemory: "available" counts the standby list, which Windows
  #    will hand to a starting VM, and "free" does not -- free under-reports and
  #    would wait out the whole window on a host that was ready. A raw perf
  #    counter rather than Get-Counter, whose names are localised and would not
  #    resolve on a non-English host.
  local PS_VM="
\$vmName = '${VM_NAME}';
\$kaktDir = 'C:\ProgramData\kakt\flatcar';
\$baseVhdx = Join-Path \$kaktDir 'flatcar-base.vhdx';
\$rawVhdx = Join-Path \$kaktDir 'flatcar_production_hyperv_vhdx_image.vhdx';
if (!(Test-Path \$baseVhdx) -and (Test-Path \$rawVhdx)) {
  Move-Item -Path \$rawVhdx -Destination \$baseVhdx -Force
}
\$vhdPath = Join-Path \$kaktDir '${VM_NAME}.vhdx';
\$ignPath = Join-Path \$kaktDir '${VM_NAME}-config.ign';
\$isoPath = Join-Path \$kaktDir '${VM_NAME}-ignition.iso';
\$kvpctl = Join-Path \$kaktDir 'kvpctl.exe';

\$existingVM = Get-VM -Name \$vmName -ErrorAction SilentlyContinue
if (\$existingVM) {
  Stop-VM -Name \$vmName -Force -TurnOff -ErrorAction SilentlyContinue
  Remove-VM -Name \$vmName -Force -ErrorAction SilentlyContinue
  # Waited for, not slept through -- but only for the OBJECT. The memory it held
  # comes back later and is waited for separately, just before Start-VM; see the
  # note there. The two seconds this used to sleep covered neither.
  for (\$i = 0; \$i -lt 30; \$i++) {
    if (-not (Get-VM -Name \$vmName -ErrorAction SilentlyContinue)) { break }
    Start-Sleep -Seconds 2
  }
  Start-Sleep -Seconds 3
}

# Clean any stale checkpoint avhdx files
Get-ChildItem \$kaktDir -Filter '${VM_NAME}*.avhdx' | Remove-Item -Force -ErrorAction SilentlyContinue

# Host disk headroom guard (ADR-046): ensure C: has room for bounded growth (60%) plus 20 GB reserve
\$cFree = [int64](Get-PSDrive C).Free
\$thisBounded = [int64]($NODE_DISK_BYTES * 0.60)
\$thisCurrent = if (Test-Path \$vhdPath) { (Get-Item \$vhdPath).Length } else { [int64]0 }
\$thisGrowth = [math]::Max([int64]0, \$thisBounded - \$thisCurrent)

\$otherGrowth = [int64]0
\$otherDisks = Get-VM | Where-Object { \$_.Name -ne \$vmName } | ForEach-Object { Get-VMHardDiskDrive -VMName \$_.Name }
foreach (\$d in \$otherDisks) {
  if (Test-Path \$d.Path) {
    \$cur = (Get-Item \$d.Path).Length
    \$maxBounded = [int64]((Get-VHD \$d.Path).Size * 0.60)
    \$growth = [math]::Max([int64]0, \$maxBounded - \$cur)
    \$otherGrowth += \$growth
  }
}
\$reserveBytes = [int64](20GB)
\$headroom = \$cFree - \$thisGrowth - \$otherGrowth
if (\$headroom -lt \$reserveBytes) {
  Write-Output ('    ✗ host disk headroom check failed: C: free ' + [math]::Round(\$cFree/1GB, 1) + ' GB, this VM growth ' + [math]::Round(\$thisGrowth/1GB, 1) + ' GB, other VMs growth ' + [math]::Round(\$otherGrowth/1GB, 1) + ' GB, projected headroom ' + [math]::Round(\$headroom/1GB, 1) + ' GB < 20 GB reserve')
  Write-Output 'HOST-DISK=FAIL'
  exit 1
}
Write-Output ('    ✓ host disk headroom: C: free ' + [math]::Round(\$cFree/1GB, 1) + ' GB, projected headroom ' + [math]::Round(\$headroom/1GB, 1) + ' GB (>= 20 GB reserve)')

Write-Output '    → Creating fresh VM disk from pristine base Flatcar VHDX...'
Copy-Item -Path \$baseVhdx -Destination \$vhdPath -Force
Resize-VHD -Path \$vhdPath -SizeBytes $NODE_DISK_BYTES

Write-Output ('    → Creating Generation 2 VM (' + \$vmName + ')...')
\$cs = Get-CimInstance Win32_ComputerSystem
\$totalRamBytes = [int64]\$cs.TotalPhysicalMemory
\$reserveBytes = [int64](5GB)

# Co-located Dual-VM memory layout (16 GB Dell):
# Windows + Hyper-V ~5 GB | Hub VM 6 GB | Spoke VM 4 GB | Safety margin ~1 GB
if ('$CLUSTER_TARGET' -eq 'spoke') {
  \$autoMaxRam = [int64](4GB)
  \$autoStartup = [int64](3GB)
  \$autoMin = [int64](2GB)
} else {
  \$autoMaxRam = [int64](6GB)
  \$autoStartup = [int64](3.5GB)
  \$autoMin = [int64](2GB)
}

\$proc = Get-CimInstance Win32_Processor
# Divide the host's threads between the VMs that will share it, rather than handing
# every VM the full count. The old behaviour gave each VM NumberOfLogicalProcessors,
# so two nodes on a 4-thread part ran 8 vCPUs — 2:1 oversubscription, and the direct
# cause of the thermal trips in ADR-046 §24.5. Counts VMs already defined plus this
# one, so it self-adjusts as a box gains or loses nodes. Floor of 2: kubelet plus a
# CNI on a single vCPU is not viable.
\$logical = [int]\$proc.NumberOfLogicalProcessors
\$peerVms = @(Get-VM -ErrorAction SilentlyContinue | Where-Object { \$_.Name -ne \$vmName }).Count
\$share   = [math]::Max(1, \$peerVms + 1)
\$autoCpus = [math]::Max(2, [math]::Floor(\$logical / \$share))

\$finalMaxRam = \$autoMaxRam
if ($MAX_MEMORY_BYTES -gt 0) { \$finalMaxRam = [int64]$MAX_MEMORY_BYTES }
\$finalStartup = \$autoStartup
if ($MEMORY_BYTES -gt 0) { \$finalStartup = [int64]$MEMORY_BYTES }
\$finalMin = \$autoMin
if ($MIN_MEMORY_BYTES -gt 0) { \$finalMin = [int64]$MIN_MEMORY_BYTES }
\$finalCpus = \$autoCpus
if ($CPU_COUNT -gt 0) { \$finalCpus = [int]$CPU_COUNT }

# Host memory ceiling -- see the note above this PowerShell block.
\$memAlign = [int64](2MB)
\$hostCeiling = \$totalRamBytes - \$reserveBytes
\$hostCeiling = \$hostCeiling - (\$hostCeiling % \$memAlign)
if (\$hostCeiling -lt [int64](2GB)) {
  Write-Output ('    ✗ host has ' + [math]::Round(\$totalRamBytes/1GB, 1) + ' GB total; after a ' + [math]::Round(\$reserveBytes/1GB, 1) + ' GB OS reserve there is not enough left to run a node')
  Write-Output 'VM-CAPACITY=FAIL'
  exit 1
}
if (\$finalStartup -gt \$hostCeiling) {
  Write-Output ('    ⚠ requested ' + [math]::Round(\$finalStartup/1GB, 1) + ' GB startup memory, but this host has ' + [math]::Round(\$totalRamBytes/1GB, 1) + ' GB total')
  Write-Output ('    → granting ' + [math]::Round(\$hostCeiling/1GB, 1) + ' GB (host total minus the ' + [math]::Round(\$reserveBytes/1GB, 1) + ' GB OS reserve)')
  Write-Output ('    → the request comes from the memory columns for this node in scripts/hybrid/home-lab.env')
  \$finalStartup = \$hostCeiling
}
# Hyper-V requires min <= startup <= max, and clamping startup can invalidate
# either. Both follow it rather than failing Set-VMMemory on an inconsistent set.
if (\$finalMin -gt \$finalStartup) { \$finalMin = \$finalStartup }
if (\$finalMaxRam -lt \$finalStartup) { \$finalMaxRam = \$finalStartup }
if (\$finalMaxRam -gt \$hostCeiling) { \$finalMaxRam = \$hostCeiling }
\$finalStartup = \$finalStartup - (\$finalStartup % \$memAlign)
\$finalMin = \$finalMin - (\$finalMin % \$memAlign)
\$finalMaxRam = \$finalMaxRam - (\$finalMaxRam % \$memAlign)

# Oversubscription guard (ADR-046 §24.5). home-lab.env is gitignored, so a stale
# cpus column survives every repo change and silently reinstates the condition that
# powered this class of host off mid-provision. An explicit value still wins — the
# operator may know better — but it does not get to be silent about it.
\$otherCpus = 0
Get-VM -ErrorAction SilentlyContinue | Where-Object { \$_.Name -ne \$vmName } | ForEach-Object { \$otherCpus += \$_.ProcessorCount }
\$totalCpus = \$otherCpus + \$finalCpus
if (\$totalCpus -gt \$logical) {
  Write-Output ('    ⚠ vCPU oversubscription: ' + \$totalCpus + ' vCPUs across ' + (\$peerVms + 1) + ' VM(s) on ' + \$logical + ' host threads')
  Write-Output ('    → this ratio drove ACPI critical-thermal shutdowns on a 15W host; see ADR-046 §24.5')
  Write-Output ('    → lower the cpus column in home-lab.env for the VMs on this box')
}

Write-Output ('    ✓ Capacity: ' + \$finalCpus + ' vCPUs, ' + [math]::Round(\$finalStartup/1GB, 1) + ' GB Startup (Dynamic ' + [math]::Round(\$finalMin/1GB, 1) + ' - ' + [math]::Round(\$finalMaxRam/1GB, 1) + ' GB Max, ' + [math]::Round(\$reserveBytes/1GB, 1) + ' GB Host OS Reserve)')

New-VM -Name \$vmName -Generation 2 -MemoryStartupBytes \$finalStartup -VHDPath \$vhdPath -SwitchName '$VSWITCH_NAME' | Out-Null
Add-VMDvdDrive -VMName \$vmName -Path \$isoPath | Out-Null
Set-VMFirmware -VMName \$vmName -EnableSecureBoot Off
\$hdd = Get-VMHardDiskDrive -VMName \$vmName
\$dvd = Get-VMDvdDrive -VMName \$vmName
Set-VMFirmware -VMName \$vmName -BootOrder \$hdd, \$dvd
Set-VM -VMName \$vmName -AutomaticCheckpointsEnabled:\$false
Set-VMMemory -VMName \$vmName -DynamicMemoryEnabled \$true -StartupBytes \$finalStartup -MinimumBytes \$finalMin -MaximumBytes \$finalMaxRam
Set-VMProcessor -VMName \$vmName -Count \$finalCpus
Set-VM -VMName \$vmName -AutomaticStartAction Start

Write-Output '    → Injecting Ignition config via kvpctl...'
if (Test-Path \$kvpctl) {
  & \$kvpctl \$vmName add-ign \$ignPath
}

# Wait for the host to return the memory -- see the note above this block.
\$needBytes = \$finalStartup
for (\$i = 0; \$i -lt 30; \$i++) {
  \$avail = 0
  try { \$avail = [int64](Get-CimInstance Win32_PerfRawData_PerfOS_Memory -ErrorAction Stop).AvailableBytes } catch { \$avail = 0 }
  if (\$avail -eq 0) {
    try { \$avail = [int64](Get-CimInstance Win32_OperatingSystem -ErrorAction Stop).FreePhysicalMemory * 1024 } catch { \$avail = 0 }
  }
  if (\$avail -ge \$needBytes) { break }
  if (\$i -eq 0) {
    Write-Output ('    → waiting for the host to return memory: need ' + [math]::Round(\$needBytes/1GB, 1) + ' GB, ' + [math]::Round(\$avail/1GB, 1) + ' GB available')
  }
  Start-Sleep -Seconds 2
}

# Retried as well as waited for. The wait makes the first attempt likely to
# succeed; the retry covers a host that is still settling, and neither invents
# memory -- which is why the ceiling above is the actual fix.
\$started = \$false
for (\$i = 0; \$i -lt 6; \$i++) {
  try {
    Start-VM -Name \$vmName -ErrorAction Stop
    \$started = \$true
    break
  } catch {
    Write-Output ('    … start attempt ' + (\$i + 1) + ' failed: ' + \$_.Exception.Message)
    Start-Sleep -Seconds 10
  }
}
if (\$started) {
  Write-Output '    ✓ Started VM'
} else {
  # Said here rather than inferred five minutes later from a node that never
  # joined. The caller checks Get-VM state after this block regardless.
  Write-Output '    ✗ Start-VM failed after 6 attempts over ~60s'
}

Write-Output '    → Setting up host portproxy for SSH...'
\$vmIp = \$null
for (\$i = 0; \$i -lt 15; \$i++) {
  Start-Sleep -Seconds 1
  \$adapter = Get-VMNetworkAdapter -VMName \$vmName -ErrorAction SilentlyContinue
  \$ip = \$adapter.IPAddresses | Where-Object { \$_ -match '^\d+\.\d+\.\d+\.\d+' } | Select-Object -First 1
  if (\$ip) { \$vmIp = \$ip; break }
}
if (!\$vmIp) { \$vmIp = '172.30.0.$((10 + NODE_IDX))' }
\$listenPort = $((2220 + NODE_IDX))
netsh interface portproxy delete v4tov4 listenport=\$listenPort listenaddress=0.0.0.0 | Out-Null
netsh interface portproxy add v4tov4 listenport=\$listenPort listenaddress=0.0.0.0 connectaddress=\$vmIp connectport=22 | Out-Null
# The portproxy only listens; Windows Firewall still has to let the Mac in, or
# the guest-side monitor sits on 'SSH: Booting' for the whole run while the node
# joins fine over Tailscale. box-a had this rule hand-made, box-b did not.
\$fwName = 'Flatcar SSH Portproxy ' + \$listenPort
if (!(Get-NetFirewallRule -DisplayName \$fwName -ErrorAction SilentlyContinue)) {
  New-NetFirewallRule -DisplayName \$fwName -Direction Inbound -Action Allow -Protocol TCP -LocalPort \$listenPort -Profile Any -ErrorAction SilentlyContinue | Out-Null
  Write-Output ('    ✓ Firewall: inbound TCP ' + \$listenPort + ' allowed')
}
Write-Output ('    ✓ Portproxy: host:' + \$listenPort + ' -> ' + \$vmIp + ':22')
Write-Output 'VM-READY=OK'
"
  win_ps_stream "$SSH_TARGET" "$PS_VM"

  # "✓ Started VM" is printed unconditionally by the block above; Hyper-V is the
  # authority. Without this check a failed New-VM still costs a five-minute Ready
  # wait and reports the failure as a join problem.
  local VM_STATE
  VM_STATE=$(win_ps "$SSH_TARGET" "(Get-VM -Name '$VM_NAME' -ErrorAction SilentlyContinue).State" \
    | tr -d '\r' | grep -Ex 'Running|Off|Saved|Paused|Starting|Stopping|Reset|Resuming|Other' | tail -1 || true)
  if [[ "$VM_STATE" != "Running" ]]; then
    echo "    ✗ ${VM_NAME}: Hyper-V reports state '${VM_STATE:-absent}' after provisioning — the VM was not created or not started." >&2
    echo "      Scroll up for the first PowerShell error; the ones after it are consequences." >&2
    return 1
  fi
}

# ── Phase 6: Verify node Ready ───────────────────────────────────────────────
# node_state answers ONE question about a node, in a way the caller can act on:
# is it serving, is it registered-but-not-serving, is it not in this cluster at
# all, or could the cluster not be asked. Echoes exactly one word on stdout —
# ready | notready | absent | unknown — with any diagnostics on stderr.
#
# The distinction between `notready` and `absent` is load-bearing and used to be
# collapsed. node_ready returns non-zero for both, and the recovery gate read
# that as "wait for it to come back". A node that is ABSENT from this cluster is
# not coming back: on a fresh bootstrap the cluster has a new CA and new tokens,
# so the VM left over from the previous run holds a kubelet certificate for a
# cluster that no longer exists. Waiting for it is waiting for something that
# cannot happen -- observed as a 5-minute pause on every `make e2e-fresh` before
# the rebuild that was always going to be required.
node_state() {
  local HOSTNAME="$1"
  local CLUSTER_TARGET="${2:-$TARGET_CLUSTER}"
  local TARGET_KC
  local TMP_KC=""

  if [[ "$CLUSTER_TARGET" == "hub" ]]; then
    TARGET_KC="${HUB_KUBECONFIG}"
  else
    TMP_KC=$(mktemp /tmp/hybrid-spoke-XXXXXX)
    if ! kubectl --kubeconfig="${HUB_KUBECONFIG}" \
          get secret "${HYBRID_SPOKE_NAME}-kubeconfig" \
          -n platform-capi -o jsonpath='{.data.value}' 2>/dev/null \
          | base64 -d > "$TMP_KC"; then
      rm -f "$TMP_KC"
      echo "    ✗ could not fetch spoke kubeconfig" >&2
      echo unknown
      return 0
    fi
    TARGET_KC="$TMP_KC"
  fi

  # Whether the API answered at all, separately from what it said. `get node`
  # failing because the cluster is unreachable is not the same claim as the node
  # not being in it, and treating the first as the second is how an unreachable
  # hub came to look like a node that needed rebuilding.
  local ALL
  if ! ALL=$(kubectl --kubeconfig="$TARGET_KC" get nodes \
        -o jsonpath='{range .items[*]}{.metadata.name}{"="}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' 2>/dev/null); then
    [[ -n "$TMP_KC" ]] && rm -f "$TMP_KC"
    echo unknown
    return 0
  fi
  [[ -n "$TMP_KC" ]] && rm -f "$TMP_KC"

  local line name status
  while IFS= read -r line; do
    name="${line%%=*}"; status="${line#*=}"
    if [[ "$name" == "$HOSTNAME" ]]; then
      [[ "$status" == "True" ]] && echo ready || echo notready
      return 0
    fi
  done <<< "$ALL"
  echo absent
}

# node_ready keeps its original contract -- exit 0 when the node is serving --
# and now reads node_state so there is one place that asks the cluster.
node_ready() {
  local HOSTNAME="$1"
  local CLUSTER_TARGET="${2:-$TARGET_CLUSTER}"
  case "$(node_state "$HOSTNAME" "$CLUSTER_TARGET")" in
    ready)
      echo "    ✓ ${HOSTNAME}: Ready in ${CLUSTER_TARGET} cluster"
      return 0 ;;
    notready)
      echo "    ✗ ${HOSTNAME}: exists in ${CLUSTER_TARGET} but is not Ready" >&2
      return 1 ;;
    absent)
      echo "    ✗ ${HOSTNAME}: not a member of ${CLUSTER_TARGET} cluster" >&2
      return 1 ;;
    *)
      echo "    ✗ ${HOSTNAME}: could not ask the ${CLUSTER_TARGET} cluster" >&2
      return 1 ;;
  esac
}

# ── Recovery gate: start what exists before replacing it ─────────────────────
#
# Rebuilding is destructive and used to be the only thing this script could do.
# phase_prep_flatcar_and_ignition sweeps the VM and deletes the Node object;
# phase_provision_flatcar_vm then replaces the disk from the pristine base image.
# Between them they cost ~10 minutes and take with them the evidence for why the
# node dropped out in the first place.
#
# A node that has lost its heartbeat usually needs none of that. On a workstation
# the VM is Off because the host slept, rebooted, or ran out of memory; the guest
# still holds a valid kubelet client certificate, so starting it is enough — it
# re-registers itself, which is exactly what was observed on 2026-09-15 after a
# rebuild had already been triggered to fix it.
#
# Returns 0 when the node reached Ready without a rebuild, and the caller then
# skips every remaining phase for this node. Returns 1 to mean "rebuild", which
# is also what an absent VM and an unreachable Hyper-V both mean — this gate only
# ever declines to destroy something, it never decides to.
phase_recover_existing_vm() {
  local SSH_TARGET="$1" HOSTNAME="$2" NODE_IDX="$3" CLUSTER_TARGET="${4:-$TARGET_CLUSTER}"

  if [[ "$FORCE_REBUILD" == "1" ]]; then
    echo "    [0/6] Recovery gate skipped (--force-rebuild)"
    return 1
  fi

  local VM_STATE
  VM_STATE=$(win_ps "$SSH_TARGET" "(Get-VM -Name '${HOSTNAME}' -ErrorAction SilentlyContinue).State" \
    | tr -d '\r' | grep -Ex 'Running|Off|Saved|Paused|Starting|Stopping|Reset|Resuming|Other' | tail -1 || true)

  if [[ -z "$VM_STATE" ]]; then
    echo "    [0/6] Recovery gate: no VM named ${HOSTNAME} on the host — provisioning from scratch"
    return 1
  fi

  echo "    [0/6] Recovery gate: ${HOSTNAME} already exists on the host (Hyper-V state: ${VM_STATE})"

  # What the CLUSTER says decides whether recovery is possible at all, and it is
  # asked before the VM's power state is acted on.
  case "$(node_state "$HOSTNAME" "$CLUSTER_TARGET")" in
    ready)
      # Already serving. The caller's own gate usually catches this, but the
      # script is also run by hand -- the CLI's failure message tells operators
      # to do exactly that -- so it must not destroy a healthy node.
      echo "    ✓ ${HOSTNAME}: already Ready in ${CLUSTER_TARGET} — nothing to do"
      return 0 ;;
    absent)
      # Not a member of this cluster, so there is nothing to wait for. This is
      # every fresh bootstrap: `local-e2e.sh clean` tears down the cluster, the
      # kind cluster and the tenant repo but touches no Hyper-V object, so the
      # previous run's VM is still on the host holding a kubelet certificate for
      # a cluster that no longer exists. It cannot rejoin one with a new CA --
      # waiting the full window for it only delays the rebuild it always needed.
      echo "    → ${HOSTNAME} is not a member of the ${CLUSTER_TARGET} cluster and cannot"
      echo "      rejoin one it holds no credentials for — rebuilding."
      return 1 ;;
  esac

  case "$VM_STATE" in
    Off|Saved|Paused)
      echo "    → VM is ${VM_STATE}; starting it rather than replacing it..."
      # Retried, and for the same reason the rebuild path is: Hyper-V does not
      # release a VM's memory reservation the instant it stops, and a Start-VM
      # issued into that window fails with "Failed to finish reserving resources".
      if ! win_ps "$SSH_TARGET" "
\$ok = \$false
for (\$i = 0; \$i -lt 6; \$i++) {
  try {
    if ((Get-VM -Name '${HOSTNAME}' -ErrorAction Stop).State -eq 'Paused') {
      Resume-VM -Name '${HOSTNAME}' -ErrorAction Stop
    } else {
      Start-VM -Name '${HOSTNAME}' -ErrorAction Stop
    }
    \$ok = \$true
    break
  } catch {
    Write-Output ('    … start attempt ' + (\$i + 1) + ' failed: ' + \$_.Exception.Message)
    Start-Sleep -Seconds 10
  }
}
if (\$ok) { Write-Output 'START=OK' } else { Write-Output 'START=FAIL' }
" | tr -d '\r' | grep -q 'START=OK'; then
        echo "    ✗ could not start ${HOSTNAME} after 6 attempts — falling through to a rebuild" >&2
        return 1
      fi
      echo "    ✓ ${HOSTNAME} started"
      ;;
    Running)
      echo "    → VM is Running but the node is not Ready; giving the kubelet time to rejoin..."
      ;;
    *)
      # Starting, Stopping, Reset, Resuming, Other: transitional or wedged. Not a
      # state to start from and not one to wait on either.
      echo "    → VM is ${VM_STATE}, which is neither running nor startable — rebuilding"
      return 1
      ;;
  esac

  # Poll proportionally: a long window wants a quiet loop, and a caller who asked
  # for a short one (a test, or --recover-wait 30) wants an answer inside it
  # rather than one fixed ten-second tick later.
  local POLL=10
  if (( RECOVER_WAIT_SECONDS < 50 )); then
    POLL=$(( RECOVER_WAIT_SECONDS / 5 ))
    (( POLL < 1 )) && POLL=1
  fi

  local DEADLINE=$(( $(date +%s) + RECOVER_WAIT_SECONDS ))
  local WAITED=0
  while true; do
    if node_ready "$HOSTNAME" "$CLUSTER_TARGET" >/dev/null 2>&1; then
      echo "    ✓ ${HOSTNAME}: Ready in ${CLUSTER_TARGET} after ${WAITED}s — no rebuild needed"
      return 0
    fi
    if [[ $(date +%s) -ge $DEADLINE ]]; then
      break
    fi
    sleep "$POLL"
    WAITED=$((WAITED + POLL))
    if (( WAITED % 60 == 0 )); then
      echo "        still waiting for ${HOSTNAME} to rejoin (${WAITED}s of ${RECOVER_WAIT_SECONDS}s)"
    fi
  done

  echo "    ${HOSTNAME} did not rejoin within ${RECOVER_WAIT_SECONDS}s — rebuilding it" >&2
  return 1
}

# ── Phase 5 & 6: Monitor guest boot & verify node Ready ─────────────────────
phase_monitor_and_verify() {
  local SSH_TARGET="$1" HOSTNAME="$2" NODE_IDX="$3" CLUSTER_TARGET="${4:-$TARGET_CLUSTER}"
  echo "    [5/6] Flatcar VM booted — monitoring guest startup & join progress..."
  echo "    [6/6] Waiting for node Ready in ${CLUSTER_TARGET} cluster (up to 5 min)..."
  local HOST_IP="${SSH_TARGET#*@}"
  local GUEST_PORT=$((2220 + NODE_IDX))
  for attempt in $(seq 1 30); do
    local ELAPSED=$((attempt * 10))
    local VM_STATUS SSH_STATUS INST_STATUS TS_STATUS CRI_STATUS JOIN_STATUS KUBE_STATUS LAST_LOG

    # Query VM status from Hyper-V. The script must be piped in, not passed as an
    # argument: with OpenSSH DefaultShell=cmd.exe the single quotes survive into
    # powershell, which then echoes the command back as a literal string, and the
    # monitor reports that text as the VM state for the whole five minutes.
    VM_STATUS=$(win_ps "$SSH_TARGET" "(Get-VM -Name '${HOSTNAME}' -ErrorAction SilentlyContinue).State" \
      | tr -d '\r' | grep -Ex 'Running|Off|Saved|Paused|Starting|Stopping|Reset|Resuming|Other' | tail -1 || true)
    [[ -z "$VM_STATUS" ]] && VM_STATUS="Unknown"

    # Query Guest SSH & service status
    local GUEST_SSH_OPT=(-o BatchMode=yes -o ConnectTimeout=2 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -p "$GUEST_PORT")
    if ssh -n "${GUEST_SSH_OPT[@]}" core@"${HOST_IP}" "echo ok" &>/dev/null; then
      SSH_STATUS="Connected"
      local RAW_STATUS
      RAW_STATUS=$(ssh -n "${GUEST_SSH_OPT[@]}" core@"${HOST_IP}" \
        'for u in k8s-install tailscaled containerd kubeadm-join kubelet; do printf "%s " "$(systemctl is-active $u.service 2>/dev/null || echo unknown)"; done' 2>/dev/null || echo "unknown unknown unknown unknown unknown")

      read -r INST_STATUS TS_STATUS CRI_STATUS JOIN_STATUS KUBE_STATUS <<< "$RAW_STATUS"
      [[ -z "$INST_STATUS" ]] && INST_STATUS="unknown"
      [[ -z "$TS_STATUS" ]] && TS_STATUS="unknown"
      [[ -z "$CRI_STATUS" ]] && CRI_STATUS="unknown"
      [[ -z "$JOIN_STATUS" ]] && JOIN_STATUS="unknown"
      [[ -z "$KUBE_STATUS" ]] && KUBE_STATUS="unknown"

      CRI_PODS=$(ssh -n "${GUEST_SSH_OPT[@]}" core@"${HOST_IP}" \
        "sudo crictl pods -o json 2>/dev/null | jq -r '[.items[]? | \"\(.metadata.name) (\(.state))\"] | join(\", \")' 2>/dev/null" || true)

      LAST_LOG=$(ssh -n "${GUEST_SSH_OPT[@]}" core@"${HOST_IP}" \
        "journalctl -u k8s-install.service -u tailscaled.service -u containerd.service -u kubeadm-join.service -u kubelet.service -n 1 --no-pager -q 2>/dev/null | tr -d '\r\n'" 2>/dev/null || true)
    else
      SSH_STATUS="Booting"
      INST_STATUS="pending"
      TS_STATUS="pending"
      CRI_STATUS="pending"
      JOIN_STATUS="pending"
      KUBE_STATUS="pending"
      CRI_PODS=""
      LAST_LOG=""
    fi

    # Check cluster membership
    if node_ready "$HOSTNAME" "$CLUSTER_TARGET" 2>/dev/null | grep -q "Ready"; then
      echo "      [${attempt}/30] (${ELAPSED}s) VM: ${VM_STATUS} | SSH: ${SSH_STATUS} | Install: ${INST_STATUS} | Tailscale: ${TS_STATUS} | CRI: ${CRI_STATUS} | Join: ${JOIN_STATUS} | Kubelet: ${KUBE_STATUS} | ${CLUSTER_TARGET}: Ready ✓"
      echo "    ✓ ${HOSTNAME}: Ready in ${CLUSTER_TARGET} cluster"
      # Apply node-role labels from cluster side (idempotent with kubelet --node-labels)
      if [[ "$CLUSTER_TARGET" == "hub" ]]; then
        kubectl --kubeconfig="${HUB_KUBECONFIG}" label node "${HOSTNAME}" \
          node-role.kubernetes.io/on-prem= node-role.kubernetes.io/worker= hub-role=worker workload-location=on-prem --overwrite >/dev/null 2>&1 || true
      else
        local SPOKE_KC
        SPOKE_KC=$(mktemp /tmp/hybrid-spoke-XXXXXX)
        if kubectl --kubeconfig="${HUB_KUBECONFIG}" \
              get secret "${HYBRID_SPOKE_NAME}-kubeconfig" \
              -n platform-capi -o jsonpath='{.data.value}' 2>/dev/null \
              | base64 -d > "$SPOKE_KC"; then
          kubectl --kubeconfig="$SPOKE_KC" label node "${HOSTNAME}" \
            node-role.kubernetes.io/on-prem= node-role.kubernetes.io/worker= workload-location=on-prem --overwrite >/dev/null 2>&1 || true
          rm -f "$SPOKE_KC"
        fi
      fi
      return 0
    else
      echo "      [${attempt}/30] (${ELAPSED}s) VM: ${VM_STATUS} | SSH: ${SSH_STATUS} | Install: ${INST_STATUS} | Tailscale: ${TS_STATUS} | CRI: ${CRI_STATUS} | Join: ${JOIN_STATUS} | Kubelet: ${KUBE_STATUS} | ${CLUSTER_TARGET}: Joining..."
      if [[ -n "$CRI_PODS" ]]; then
        echo "        ↳ [CRI Pods]: ${CRI_PODS}"
      fi
      if [[ -n "$LAST_LOG" ]]; then
        echo "        ↳ [Guest Log]: ${LAST_LOG}"
      fi
    fi
    sleep 10
  done

  echo "    ✗ ${HOSTNAME} did not become Ready within 5 min." >&2
  echo "    → Diagnostic: Dumping guest journal logs (port ${GUEST_PORT})..." >&2
  ssh -n -o BatchMode=yes -o ConnectTimeout=5 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -p "$GUEST_PORT" core@"${HOST_IP}" \
    "journalctl -u containerd.service -u k8s-install.service -u tailscaled.service -u kubeadm-join.service -u kubelet.service --no-pager -n 120" 2>&1 | sed 's/^/        /' || true
  return 1
}

# ── Main ─────────────────────────────────────────────────────────────────────

if [[ "$MODE" == "verify" ]]; then
  echo "=== Verify mode — checking Flatcar node Ready status ==="
  local_ok=1
  idx=0
  while IFS='|' read -r _HOST _SSH _OS _TAILNET _TAG NODE_TARGET _STARTUP_GB _MIN_GB _MAX_GB _CPUS <&3; do
    [[ -z "$_HOST" && -z "$_SSH" ]] && continue
    idx=$((idx + 1))
    CURR_TARGET="$(cluster_for_entry "${NODE_TARGET:-}" "$idx")"
    VM_NAME="$(vm_name_for "${_HOST}" "$CURR_TARGET" "$idx")"
    if node_ready "$VM_NAME" "$CURR_TARGET"; then :; else local_ok=0; fi
  done 3<<< "$HOME_WORKER_NODES"
  [[ "$local_ok" == "1" ]] && echo "=== ALL REGISTERED NODES READY ===" || echo "=== SOME NODES NOT READY ==="
  exit $((1 - local_ok))
fi

NODE_IDX=0
FAILED_NODES=()

# Registry size, counted before any node work. The loop below must walk exactly
# this many entries; anything less means entries were silently never read (see
# the post-loop assertion). Counting rule matches the loop's own: an entry needs
# an ssh target to count.
REGISTRY_TOTAL=0
while IFS='|' read -r _RC_HOST _RC_SSH _RC_REST <&3; do
  [[ -z "$_RC_SSH" ]] && continue
  REGISTRY_TOTAL=$((REGISTRY_TOTAL + 1))
done 3<<< "$HOME_WORKER_NODES"

# Every registered hostname, and every distinct Windows host, read once.
#
# Both exist to make node cleanup safe under two things the original cleanup
# assumed away (ADR-046, home-lab placement addendum 2026-08-30):
#
#   1. That the index-derived name variants below could never collide with a
#      REAL node. They can. The variants are flatcar-{,hub-,spoke-}node-$IDX,
#      which was safe only while hub and spoke occupied disjoint index ranges.
#      Once the registry became hub 1-3 / spoke 4-5, provisioning hub index 1
#      swept the name "flatcar-spoke-node-1" -- a live node -- deleting its
#      Node object and, on a shared host, destroying its VM.
#
#   2. That a hostname never moves between boxes. It does: the hub moved from
#      box-a to box-b. Cleaning only the node's CURRENT ssh-target leaves the
#      old box running a VM with the same hostname, whose kubelet still holds a
#      valid client cert and re-registers the Node object the new VM just took.
REGISTERED_HOSTNAMES=""
REGISTERED_SSH_TARGETS=""
_RN_IDX=0
while IFS='|' read -r _RN_HOST _RN_SSH _RN_OS _RN_TAILNET _RN_TAG _RN_TARGET _RN_REST <&3; do
  [[ -z "$_RN_SSH" ]] && continue
  _RN_IDX=$((_RN_IDX + 1))
  # Store the EFFECTIVE name, resolved the same way the main loop resolves it.
  # Storing the raw column would miss entries that omit a hostname and fall back
  # to the conventional name, leaving them unprotected from another entry's sweep.
  REGISTERED_HOSTNAMES="${REGISTERED_HOSTNAMES}$(vm_name_for "$_RN_HOST" "$(cluster_for_entry "${_RN_TARGET:-}" "$_RN_IDX")" "$_RN_IDX")
"
  REGISTERED_SSH_TARGETS="${REGISTERED_SSH_TARGETS}${_RN_SSH}
"
done 3<<< "$HOME_WORKER_NODES"
REGISTERED_SSH_TARGETS="$(printf '%s' "$REGISTERED_SSH_TARGETS" | awk 'NF' | sort -u)"

# Names that are safe to destroy when provisioning $1 (hostname) at index $2.
# The node's own name, plus the legacy index-derived variants MINUS any variant
# that is a different registered node's hostname. Never returns a name that
# belongs to another node in the registry.
cleanup_names_for() {
  local vm_name="$1" idx="$2" n
  {
    printf '%s\n' "$vm_name"
    while IFS= read -r n; do
      [[ "$n" == "$vm_name" ]] && continue
      printf '%s\n' "$REGISTERED_HOSTNAMES" | grep -qxF -- "$n" && continue
      printf '%s\n' "$n"
    done <<< "$(legacy_names_for_idx "$idx")"
  } | awk 'NF' | sort -u
}

echo "=== Hybrid Hyper-V + Flatcar Container Linux worker provisioner ==="
echo "    Target Mode:    ${TARGET_CLUSTER} (Hub + Spoke auto-routing)"
echo "    Spoke:          ${HYBRID_SPOKE_NAME}"
echo "    Tailnet:        ${TAILNET_NAME}"
echo "    Hub kc:         ${HUB_KUBECONFIG}"
echo "    K8s ver:        ${K8S_VERSION}"
echo "    Tailscale ver:  ${TAILSCALE_VERSION}"
echo "    Flatcar ch/ver: ${FLATCAR_RELEASE_CHANNEL} (stable 4593.2.4)"
echo "    Hyper-V Switch: ${VSWITCH_NAME}"
echo ""

phase_prep_binaries

while IFS='|' read -r _HOST SSH_TARGET WSL_DISTRO _TAILNET BOX_TAG NODE_TARGET STARTUP_GB MIN_GB MAX_GB CPUS DISK_GB <&3; do
  [[ -z "$SSH_TARGET" ]] && continue
  NODE_IDX=$((NODE_IDX + 1))

  # A host may be reachable at more than one address, and which one answers is
  # not ours to decide.
  #
  # The registry field takes a comma-separated list -- user@a,user@b -- and the
  # first that accepts a TCP connection on its ssh port wins. Everything below
  # then uses that one address, so no other code has to know a list was possible.
  #
  # A workstation on DHCP changes address, and it changed: the Lenovo was
  # recorded at .12 on 2026-09-14 with a note that .11 "does not respond", and by
  # 2026-09-18 .12 refused connections while .11 answered. The provisioner tried
  # the one address it had been given, six times over thirty seconds, and failed
  # the whole bootstrap phase on a machine that was up the entire time.
  #
  # Probing the PORT rather than pinging: these hosts drop ICMP, so both
  # addresses look dead to ping while one of them is serving ssh.
  if [[ "$SSH_TARGET" == *,* ]]; then
    _resolved=""
    IFS=',' read -r -a _cands <<< "$SSH_TARGET"
    for _c in "${_cands[@]}"; do
      _c="${_c#"${_c%%[![:space:]]*}"}"       # trim leading space
      [[ -z "$_c" ]] && continue
      _chost="${_c#*@}"
      if nc -z -G 3 "$_chost" 22 >/dev/null 2>&1; then
        _resolved="$_c"
        break
      fi
    done
    if [[ -n "$_resolved" ]]; then
      [[ "$_resolved" != "${_cands[0]}" ]] &&
        echo "    note: ${_cands[0]} is not answering; using ${_resolved}"
      SSH_TARGET="$_resolved"
    else
      # None answered. Keep the first so the reachability gate below reports a
      # real ssh error against a real address rather than a list.
      SSH_TARGET="${_cands[0]}"
    fi
  fi
  [[ -n "$ONLY_NODE" && "$NODE_IDX" != "$ONLY_NODE" ]] && continue

  # Per-node capacity: registry fields are the DEFAULT; CLI flags override.
  MEMORY_BYTES="0"; MIN_MEMORY_BYTES="0"; MAX_MEMORY_BYTES="0"; CPU_COUNT="0"
  if [[ "${STARTUP_GB:-}" =~ ^[0-9]+$ ]]; then
    MEMORY_BYTES="$((${STARTUP_GB} * 1024 * 1024 * 1024))"
    [[ "${MIN_GB:-}" =~ ^[0-9]+$ ]] && MIN_MEMORY_BYTES="$((${MIN_GB} * 1024 * 1024 * 1024))"
    [[ "${MAX_GB:-}" =~ ^[0-9]+$ ]] && MAX_MEMORY_BYTES="$((${MAX_GB} * 1024 * 1024 * 1024))"
    [[ "${CPUS:-}" =~ ^[0-9]+$ ]] && CPU_COUNT="${CPUS}"
  fi
  [[ "$CLI_MEMORY_BYTES" -gt 0 ]] && MEMORY_BYTES="$CLI_MEMORY_BYTES"
  [[ "$CLI_MIN_MEMORY_BYTES" -gt 0 ]] && MIN_MEMORY_BYTES="$CLI_MIN_MEMORY_BYTES"
  [[ "$CLI_MAX_MEMORY_BYTES" -gt 0 ]] && MAX_MEMORY_BYTES="$CLI_MAX_MEMORY_BYTES"
  [[ "$CLI_CPU_COUNT" -gt 0 ]] && CPU_COUNT="$CLI_CPU_COUNT"

  # Disk ceiling, in precedence order: --disk-gb > registry field 11 > per-target
  # default. Resolved after CURR_TARGET is known (just below), so a hub node gets
  # the hub default without anyone passing a flag.

  # Where this node belongs. The registry's target field (column 6) is
  # authoritative; the index heuristic is only a fallback for older registries
  # that predate that column.
  CURR_TARGET="$(cluster_for_entry "${NODE_TARGET:-}" "$NODE_IDX")"

  # The node's ordinal WITHIN its own cluster, counted before the --cluster
  # filter below so it does not depend on which subset this invocation walks.
  #
  # NODE_IDX is the registry's global position and must stay that way: it sets
  # the VM's IP (172.30.0.$((10 + NODE_IDX))) and its SSH port
  # ($((2220 + NODE_IDX))), which have to be unique across every VM on the host,
  # hub and spoke alike. But hub-operator mints join tokens per SPOKE, 1-based --
  # node-<i>-token for i in 1..len(home-workers) -- so it knows nothing of that
  # global ordering.
  #
  # Using NODE_IDX for both asked for the wrong key every time. Registry entry 1
  # is always the hub (cluster_for_entry's fallback), so a spoke's first worker
  # is entry 2 and the script looked up node-2-token while the operator had
  # minted node-1-token. Verified on nutgraf-01, 2026-09-18: the secret held
  # exactly [ca-cert-hash, control-plane-endpoint, node-1-token] and the run
  # failed with "Spoke join credentials not ready on Hub. Wait for hub-operator."
  # -- a message blaming a controller that had done its work 155 minutes earlier.
  if [[ "$CURR_TARGET" == "hub" ]]; then
    HUB_ORDINAL=$((${HUB_ORDINAL:-0} + 1))
    CLUSTER_ORDINAL="$HUB_ORDINAL"
  else
    SPOKE_ORDINAL=$((${SPOKE_ORDINAL:-0} + 1))
    CLUSTER_ORDINAL="$SPOKE_ORDINAL"
  fi

  # --cluster SELECTS which registered nodes to act on; it does not retarget them.
  #
  # It used to overwrite CURR_TARGET, so `--cluster hub` walked every entry in
  # home-lab.env and joined them all to the hub — flatcar-spoke-node-1 included,
  # built under its spoke name but wired into the hub. That silently contradicts
  # the registry, whose whole purpose is to say where each node belongs, and the
  # documented behaviour ("auto-routes per home-lab.env").
  if [[ "$TARGET_CLUSTER" == "hub" || "$TARGET_CLUSTER" == "spoke" ]]; then
    if [[ "$CURR_TARGET" != "$TARGET_CLUSTER" ]]; then
      echo "── node ${NODE_IDX}: ${_HOST:-node-${NODE_IDX}} → ${CURR_TARGET}, skipped (--cluster ${TARGET_CLUSTER})"
      continue
    fi
  fi

  DEFAULT_DISK_GB="$SPOKE_DISK_GB_DEFAULT"
  [[ "$CURR_TARGET" == "hub" ]] && DEFAULT_DISK_GB="$HUB_DISK_GB_DEFAULT"
  NODE_DISK_BYTES="$((DEFAULT_DISK_GB * 1024 * 1024 * 1024))"
  [[ "${DISK_GB:-}" =~ ^[0-9]+$ ]] && NODE_DISK_BYTES="$((DISK_GB * 1024 * 1024 * 1024))"
  [[ "$CLI_DISK_SET" == "1" ]] && NODE_DISK_BYTES="$DISK_SIZE_BYTES"

  HOSTNAME="$(vm_name_for "${_HOST}" "$CURR_TARGET" "$NODE_IDX")"
  TAILNET_HOST="${HOSTNAME}.${TAILNET_NAME}"

  echo "── node ${NODE_IDX}: ${HOSTNAME} (${BOX_TAG} → ${CURR_TARGET}) ─────────────────"
  echo "    SSH: ${SSH_TARGET}  VM: ${HOSTNAME}  Tailnet: ${TAILNET_HOST}  Target: ${CURR_TARGET}"
  if [[ "$MEMORY_BYTES" -gt 0 || "$CPU_COUNT" -gt 0 ]]; then
    MCAP="$((MEMORY_BYTES / 1024 / 1024 / 1024))" MMIN="$((MIN_MEMORY_BYTES / 1024 / 1024 / 1024))" MMAX="$((MAX_MEMORY_BYTES / 1024 / 1024 / 1024))"
    echo "    Capacity: ${CPU_COUNT:-auto} vCPUs, ${MCAP:-auto} GB startup (${MMIN:-auto}-${MMAX:-auto} GB dynamic range)"
  fi

  # [1/6] SSH reachability gate
  #
  # The error is captured and printed rather than discarded. It used to go to
  # /dev/null and every cause was reported as "Windows host unreachable" -- a
  # rejected key, a wrong username, a refused connection and a sleeping machine all
  # produced that one line, and the message that would tell them apart was the one
  # being thrown away. Diagnosing it meant re-running ssh by hand to see what the
  # script had already been told.
  echo "    [1/6] SSH reachability gate..."
  #
  # Retried, because these are workstations rather than servers. A machine on a
  # desk sleeps, and its wireless adapter powers down before it does: the first
  # connection after an idle period gets "No route to host" and the one after it
  # succeeds, because the attempt itself woke the host. A single attempt therefore
  # failed the whole phase on a machine that was two seconds from being reachable.
  #
  # The retry does not paper over a host that is off -- that still fails, with the
  # last error, after the window below.
  _ssh_err=""
  _ssh_ok=0
  for _attempt in 1 2 3 4 5 6; do
    if _ssh_err=$(ssh -n -o BatchMode=yes -o ConnectTimeout=8 -o StrictHostKeyChecking=no \
          "${SSH_TARGET}" "echo ok" 2>&1 >/dev/null); then
      _ssh_ok=1
      break
    fi
    if [[ $_attempt -lt 6 ]]; then
      echo "        attempt ${_attempt}/6 failed, retrying in 5s (${_ssh_err##*: })"
      sleep 5
    fi
  done
  if [[ $_ssh_ok -eq 0 ]]; then
    echo "    ✗ SSH to ${SSH_TARGET} failed after 6 attempts over ~30s. Skipping." >&2
    if [[ -n "$_ssh_err" ]]; then
      # Indented so it reads as the cause of the line above rather than as a new
      # failure of its own.
      printf '      ssh: %s\n' "$_ssh_err" >&2
    else
      echo "      ssh exited non-zero with no message — most often a host that is" >&2
      echo "      powered off or asleep, or a firewall dropping the connection." >&2
    fi
    echo "      A workstation that sleeps will do this. On the host: disable sleep" >&2
    echo "      (powercfg /change standby-timeout-ac 0) and turn off power saving on" >&2
    echo "      its network adapter, or the same window will reopen on the next run." >&2
    FAILED_NODES+=("${HOSTNAME}")
    continue
  fi
  echo "    ✓ SSH connected"

  # Before anything destructive: if this node already exists, try to bring it
  # back rather than replace it. Returns 0 only when it is Ready, in which case
  # every phase below would be undoing work that is already done.
  if phase_recover_existing_vm "$SSH_TARGET" "$HOSTNAME" "$NODE_IDX" "$CURR_TARGET"; then
    echo ""
    continue
  fi

  # Each phase is a precondition for the next, so a failure stops this node here
  # rather than cascading into unrelated-looking errors further down.
  if ! phase_prep_hyperv "$SSH_TARGET" "$HOSTNAME"; then
    FAILED_NODES+=("${HOSTNAME}")
    echo ""
    continue
  fi
  if ! phase_prep_flatcar_and_ignition "$SSH_TARGET" "$HOSTNAME" "$NODE_IDX" "$CURR_TARGET" "$CLUSTER_ORDINAL"; then
    FAILED_NODES+=("${HOSTNAME}")
    echo ""
    continue
  fi
  if ! phase_provision_flatcar_vm "$SSH_TARGET" "$HOSTNAME" "$NODE_IDX" "$CURR_TARGET"; then
    FAILED_NODES+=("${HOSTNAME}")
    echo ""
    continue
  fi

  if ! phase_monitor_and_verify "$SSH_TARGET" "$HOSTNAME" "$NODE_IDX" "$CURR_TARGET"; then
    FAILED_NODES+=("${HOSTNAME}")
  fi
  echo ""
done 3<<< "$HOME_WORKER_NODES"

echo ""

# Every registry entry must have been read. This catches the class of bug where a
# child process inside the loop consumes the registry stream and the loop ends
# early on what looks like a clean EOF — a successful `ssh` without -n did exactly
# that, and box-b's flatcar-spoke-node-2 was silently never provisioned while the
# run still reported success. Reading on fd 3 plus `ssh -n` makes this
# unreachable; the assertion is here so a regression fails loudly instead.
if [[ "$NODE_IDX" -ne "$REGISTRY_TOTAL" ]]; then
  echo "=== PROVISION INCOMPLETE — registry truncated ===" >&2
  echo "    home-lab.env has ${REGISTRY_TOTAL} node entries but the loop only walked ${NODE_IDX}." >&2
  echo "    Entries after #${NODE_IDX} were never evaluated. A child process in the loop" >&2
  echo "    body is consuming the registry stream (an ssh/scp missing -n or </dev/null)." >&2
  exit 1
fi

if [[ ${#FAILED_NODES[@]} -gt 0 ]]; then
  echo "=== PROVISION INCOMPLETE — failed nodes: ${FAILED_NODES[*]} ==="
  exit 1
fi
echo "=== ✓ ALL TARGETED FLATCAR NODES PROVISIONED AND READY ==="
