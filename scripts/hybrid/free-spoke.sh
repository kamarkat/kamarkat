#!/usr/bin/env bash
# scripts/hybrid/free-spoke.sh
# ─────────────────────────────────────────────────────────────────────────────
# Reclaim disk on the home-lab hosts, and hold every guest log to 24 hours.
#
# Why this exists — the 2026-09-25 outage, in full, because the failure mode is
# invisible from inside the cluster:
#
#   C:\ on the Dell box (both spoke VMs live there on dynamic VHDXes) reached
#   0 bytes free. Hyper-V reported "Disk Full" on each VHDX and paused the VM
#   for a critical error; thirty minutes later it turned both off. The kubelet
#   froze with the VM, so the nodes went NotReady six minutes apart — and no
#   disk-pressure event was ever raised, because kubelet's eviction thresholds
#   read the GUEST's 60 GB disk (two thirds empty) while the HOST backing it
#   had none left. Nothing on the host watches its own free space either.
#
#   The host was holding 44.7 GB of VHDX files no VM was attached to: the hub's
#   disk from when it still ran on the Dell, an old flatcar-node-1, WSL2
#   leftovers, test images. Plus temp and update caches. None of it is
#   reclaimable from inside a guest.
#
# What this script must never do again — its own first run, 20:40 IST the same
# day, deleted BOTH spoke VMs' live disks (flatcar-spoke-node-1/2.vhdx, 33 GB)
# while they sat powered off. The guard was `Get-VHD .Attached`, which is FALSE
# for an Off VM: nothing holds the file open, so an Off VM's disk looked
# exactly like a stale one. The guard is now the VM CONFIGURATION — any path
# `Get-VMHardDiskDrive` reports for any VM on the host, and any VM name
# matching the file's base name, is untouchable regardless of VM state.
# flatcar-base.vhdx stays skipped by name. The base image and the ignition
# artefacts survived that run (their guards were already name-based); the two
# spoke VHDXes did not, and there was no shadow copy to recover them from.
#
# So this sweeps the side kubelet cannot see and bounds the side it can.
#
#   ./free-spoke.sh
#
# No options, no modes, one full pass every time. Run it whenever the host is
# short of space. It takes MINUTES, not seconds — the compaction below rewrites
# multi-GB files and restarts VMs, so do not run it when you need the cluster up
# thirty seconds from now.
#
# Phase 1, every guest that answers (SSH on port 2220 + registry index, the same
# numbering provision-flatcar-worker.sh uses):
#
#   1. writes /etc/systemd/journald.conf.d/24h.conf (MaxRetentionSec=1day,
#      SystemMaxUse=500M), restarts journald and vacuums the journal to 24h;
#   2. deletes rotated pod logs older than a day under /var/log/pods;
#   3. prunes unreferenced container images and exited sandboxes — normally the
#      LARGEST consumer on a k8s node, and the one nothing here used to touch:
#      kubelet's image GC does not fire until imagefs crosses 85%, which the
#      guest never reached because the HOST ran out first;
#   4. bounds kubelet disk usage via /etc/kubernetes/kubelet.conf.d/50-host-disk.conf
#      (image GC at 50%/40%, eviction at 40% nodefs/imagefs, 5% inodes, 100Mi memory);
#   5. runs fstrim.
#
# Phase 2, once per host:
#
#   6. deletes VHDX files that no VM on that host references (by declared disk
#      path or by name) and nothing has attached — never flatcar-base.vhdx, the
#      pristine image the provisioner copies from;
#   7. deletes *.config.ign / *-ignition.iso only for VMs that no longer exist
#      on that host (a live VM keeps its ISO: its firmware still boots that
#      DVD, and Hyper-V fails the start when the file is gone);
#   8. empties C:\Windows\Temp and C:\temp (files older than a day), the recycle
#      bin, the Windows Update download cache, and superseded WinSxS components;
#   9. REPORTS, without touching: hiberfil.sys, C:\Windows.old, shadow-copy
#      usage, every .vhdx/.avhdx over 1 GB anywhere on C:, and any VM holding a
#      checkpoint. Each is a decision, not a sweep;
#  10. COMPACTS every disk a VM declares: stop the VM gracefully, Optimize-VHD
#      -Mode Full, restart it if it had been running;
#  11. prints free space before and after per host, and EXITS 1 if any host ends
#      below 20 GB free. That non-zero is the alarm: something is growing faster
#      than this script is run.
#
# Why 10 exists, and why the sentence "it never starts or stops a VM" is gone
# from this header: fstrim alone reclaims NOTHING on the host. A dynamically
# expanding VHDX only ever grows; TRIM marks blocks reusable inside the file so
# it stops growing, but the file stays at its high-water mark forever and every
# byte of that stays charged to C:. Optimize-VHD is the only thing that gives it
# back, and it requires the disk offline — so the VM must be Off. Step 5 without
# step 10 is the reason a run can report success while the host is still full.
#
# The two guards are deliberate inverses. DELETION asks "does any VM claim this
# file?" and refuses when the answer is yes — the lesson of the 20:40 incident
# above. COMPACTION only ever touches a path a VM explicitly declares through
# Get-VMHardDiskDrive, and only while that VM is confirmed Off. A file nobody
# claims is never compacted; flatcar-base.vhdx is skipped by name in both.
#
# Shutdown is always graceful, never a power-off. A guest that will not stop
# within 180s keeps its disk uncompacted and is restarted as it was: reclaiming
# disk is not worth corrupting a filesystem to do.
# ─────────────────────────────────────────────────────────────────────────────
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENV_FILE="${HERE}/home-lab.env"
MIN_FREE_GB=20

if [[ ! -f "$ENV_FILE" ]]; then
  echo "ERROR: $ENV_FILE not found." >&2
  exit 1
fi

if [[ ! -f "${HERE}/kubelet-host-disk.conf" ]]; then
  echo "ERROR: ${HERE}/kubelet-host-disk.conf not found." >&2
  exit 1
fi
KUBELET_HOST_DISK_B64=$(base64 < "${HERE}/kubelet-host-disk.conf" | tr -d '\r\n')

# home-lab.env interpolates this when it is unset (its HUB_KUBECONFIG default
# otherwise shells out to `git rev-parse`, which fails from outside the repo and
# — under `set -e` — takes this script down while sourcing).
ZERO_OPS_DIR="${ZERO_OPS_DIR:-$(cd "${HERE}/../.." && pwd)}"
export ZERO_OPS_DIR
# shellcheck source=/dev/null
source "$ENV_FILE"

if [[ -z "${HOME_WORKER_NODES:-}" ]]; then
  echo "ERROR: $ENV_FILE does not define HOME_WORKER_NODES." >&2
  exit 1
fi

# ── Helpers (the provisioner's, trimmed to what this script needs) ───────────

# Execute PowerShell on a Windows host over SSH, buffered. Output on stdout,
# including anything the remote side wrote to stderr: failures are reported as
# text, never as an exit status, so one broken host cannot abort the sweep.
win_ps() {
  local SSH_TARGET="$1" PS_SCRIPT="$2"
  printf '%s\n' "$PS_SCRIPT" | ssh -o BatchMode=yes -o ConnectTimeout=15 -o StrictHostKeyChecking=no \
    "$SSH_TARGET" "powershell -NoProfile -ExecutionPolicy Bypass -Command -" 2>&1 || true
}

# First address of a comma-separated registry entry that accepts an SSH
# connection. The Lenovo is recorded at two addresses and which one answers
# changes; these hosts drop ICMP, so the PORT is the probe (ADR-046 §WS3).
# -n is mandatory here: without it this ssh would happily pump the caller's
# stdin — the registry heredoc — into the remote `echo ok`, silently eating the
# entries after the current one.
resolve_target() {
  local candidate IFS=','
  for candidate in $1; do
    candidate="${candidate#"${candidate%%[![:space:]]*}"}"
    if ssh -n -o BatchMode=yes -o ConnectTimeout=5 -o StrictHostKeyChecking=no \
      -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR "$candidate" "echo ok" &>/dev/null; then
      printf '%s\n' "$candidate"
      return 0
    fi
  done
  return 1
}

# The host sweep. Runs as the local PowerShell via win_ps. Emits lines this
# script parses:
#   REPORT FREE_BEFORE_GB=<n> / VM <name> <state>
#   NOTE   <something large this script will not touch on its own>
#   DEL VHDX|ARTEFACT|TEMP <what> <size>
HOST_SWEEP_PS=$(cat <<'PS_EOF'
$ErrorActionPreference = 'SilentlyContinue'
$dir = 'C:\ProgramData\kakt\flatcar'

function Get-CFree { (Get-CimInstance Win32_LogicalDisk -Filter "DeviceID='C:'").FreeSpace }

$before = Get-CFree
'REPORT FREE_BEFORE_GB={0:N2}' -f ($before / 1GB)

$vms = @(Get-VM -ErrorAction SilentlyContinue)
$vmNames = @($vms | ForEach-Object { $_.Name })
foreach ($v in $vms) { 'REPORT VM {0} {1}' -f $v.Name, $v.State }

# Every disk path any VM on this host declares — Off or Running. This is the
# authoritative "do not touch" set: the first run of this script trusted
# Get-VHD .Attached instead, which is FALSE for a VM that is merely Off
# (nothing holds the file open), and it therefore deleted both spoke VMs'
# live disks at 20:40 IST while they were powered down. A VM's configuration
# knows its disks whatever state the VM is in; ask that, not the file handle.
$vmDiskPaths = @()
foreach ($vm in $vms) {
    $vmDiskPaths += @(Get-VMHardDiskDrive -VMName $vm.Name -ErrorAction SilentlyContinue |
        ForEach-Object { $_.Path })
}

# 1. VHDX files no VM references, nothing has attached, and no VM is named
#    after. flatcar-base.vhdx is the pristine image the provisioner copies
#    from and is skipped by name before anything else is considered.
Get-ChildItem $dir -Filter '*.vhdx' -ErrorAction SilentlyContinue | ForEach-Object {
    if ($_.Name -eq 'flatcar-base.vhdx') { return }
    $base = [IO.Path]::GetFileNameWithoutExtension($_.Name)
    if ($vmNames -contains $base) {
        'SKIP VHDX {0} (VM of that name exists here)' -f $_.Name
        return
    }
    if ($vmDiskPaths -contains $_.FullName) {
        'SKIP VHDX {0} (a VM on this host declares that path)' -f $_.Name
        return
    }
    $vhd = Get-VHD -Path $_.FullName -ErrorAction SilentlyContinue
    if ($null -eq $vhd) {
        'SKIP VHDX {0} (Get-VHD could not read it)' -f $_.Name
        return
    }
    if ($vhd.Attached) {
        'SKIP VHDX {0} (attached)' -f $_.Name
        return
    }
    $mb = $_.Length / 1MB
    Remove-Item -LiteralPath $_.FullName -Force -ErrorAction SilentlyContinue
    if (-not (Test-Path -LiteralPath $_.FullName)) {
        'DEL VHDX {0} {1:N0}MB' -f $_.Name, $mb
    }
}

# 2. Ignition artefacts whose VM does not exist on this host any more. A live
#    VM keeps its ISO: Add-VMDvdDrive left it as the boot DVD.
foreach ($suffix in '-config.ign', '-ignition.iso') {
    Get-ChildItem $dir -Filter ("*" + $suffix) -ErrorAction SilentlyContinue | ForEach-Object {
        $base = $_.Name.Substring(0, $_.Name.Length - $suffix.Length)
        if ($vmNames -notcontains $base) {
            $mb = $_.Length / 1MB
            Remove-Item -LiteralPath $_.FullName -Force -ErrorAction SilentlyContinue
            if (-not (Test-Path -LiteralPath $_.FullName)) {
                'DEL ARTEFACT {0} {1:N0}MB' -f $_.Name, $mb
            }
        }
    }
}

# 3. Caches. One day old, so a file something is writing right now survives.
foreach ($t in 'C:\Windows\Temp', 'C:\temp') {
    if (Test-Path -LiteralPath $t) {
        Get-ChildItem $t -Recurse -Force -ErrorAction SilentlyContinue |
            Where-Object { -not $_.PSIsContainer -and $_.LastWriteTime -lt (Get-Date).AddDays(-1) } |
            ForEach-Object { Remove-Item -LiteralPath $_.FullName -Force -Recurse -ErrorAction SilentlyContinue }
    }
}
'DEL TEMP C:\Windows\Temp and C:\temp (files older than 1 day)'
Clear-RecycleBin -Force -ErrorAction SilentlyContinue
'DEL TEMP Recycle Bin'
Get-ChildItem 'C:\Windows\SoftwareDistribution\Download' -Recurse -Force -ErrorAction SilentlyContinue |
    Remove-Item -Force -Recurse -ErrorAction SilentlyContinue
'DEL TEMP SoftwareDistribution\Download'

# 4. The component store. Superseded update payloads, which on a box that has
#    been patched for a year or two is routinely several GB. No /ResetBase:
#    that reclaims more but permanently forecloses uninstalling any installed
#    update, which is not a trade a disk-cleanup script should make silently.
#    The line is reported below for the operator to run deliberately.
Dism.exe /Online /Cleanup-Image /StartComponentCleanup /Quiet 2>&1 | Out-Null
'DEL TEMP WinSxS superseded components (DISM StartComponentCleanup)'

# 5. Anything large this script deliberately does NOT delete, reported so the
#    number is visible instead of merely absent. Each of these is either a
#    system setting, a backup, or someone else's data: reclaiming them is a
#    decision, not a sweep.
$hib = 'C:\hiberfil.sys'
if (Test-Path -LiteralPath $hib) {
    $g = (Get-Item -LiteralPath $hib -Force).Length / 1GB
    'NOTE hiberfil.sys {0:N2}GB -- reclaim with: powercfg /h off' -f $g
}
if (Test-Path -LiteralPath 'C:\Windows.old') {
    'NOTE C:\Windows.old present -- reclaim via Disk Cleanup (Previous Windows installations)'
}
$vss = (vssadmin list shadowstorage 2>&1 | Select-String 'Used Shadow Copy Storage') -join '; '
if ($vss) { 'NOTE shadow copies: {0}' -f $vss }

# Virtual disks in the provisioner directory, declared VM disk paths, and Hyper-V locations.
# Only inspect real virtual disk paths rather than whole-drive recursion to avoid NTFS junction loops.
$reportPaths = @()
Get-ChildItem $dir -Filter '*.vhdx' -ErrorAction SilentlyContinue | ForEach-Object { $reportPaths += $_.FullName }
Get-ChildItem $dir -Filter '*.avhdx' -ErrorAction SilentlyContinue | ForEach-Object { $reportPaths += $_.FullName }
$hvPath = (Get-VMHost -ErrorAction SilentlyContinue).VirtualHardDiskPath
if ($hvPath -and (Test-Path $hvPath)) {
    Get-ChildItem $hvPath -Filter '*.vhdx' -ErrorAction SilentlyContinue | ForEach-Object { $reportPaths += $_.FullName }
    Get-ChildItem $hvPath -Filter '*.avhdx' -ErrorAction SilentlyContinue | ForEach-Object { $reportPaths += $_.FullName }
}
foreach ($p in $vmDiskPaths) { if ($p) { $reportPaths += $p } }
$reportPaths | Select-Object -Unique | ForEach-Object {
    if (Test-Path -LiteralPath $_) {
        $item = Get-Item -LiteralPath $_ -Force -ErrorAction SilentlyContinue
        if ($item.Length -gt 1GB) {
            'NOTE DISK {0} {1:N1}GB' -f $item.FullName, ($item.Length / 1GB)
        }
    }
}

foreach ($vm in $vms) {
    $snaps = @(Get-VMSnapshot -VMName $vm.Name -ErrorAction SilentlyContinue)
    if ($snaps.Count -gt 0) {
        'NOTE CHECKPOINT {0} has {1} checkpoint(s) -- compaction is skipped for it; merge with: Get-VMSnapshot -VMName {0} | Remove-VMSnapshot' -f $vm.Name, $snaps.Count
    }
}
PS_EOF
)

# The compaction pass. THIS is what returns space to C:, and the reason this
# script now stops and starts VMs where it previously promised never to.
#
# A dynamically expanding VHDX only ever grows. fstrim in the guest marks blocks
# reusable INSIDE the disk, so the file stops growing -- but the file never
# shrinks on its own, and every byte it reached at its high-water mark stays
# charged to the host. Optimize-VHD is the only thing that gives it back, and it
# requires the disk to be offline, which means the VM must be Off.
#
# The guard is deliberately the INVERSE of the deletion guard above. Deletion
# asks "does any VM claim this file?" and refuses if the answer is yes.
# Compaction only ever touches a path a VM explicitly declares through
# Get-VMHardDiskDrive, and only while that VM is confirmed Off. A file nobody
# claims is never compacted, and flatcar-base.vhdx is skipped by name in both.
#
# Stop-VM here is a graceful shutdown, never a power-off: a hung guest that will
# not shut down within the timeout keeps its disk uncompacted and is restarted
# as it was. Reclaiming disk is not worth corrupting a filesystem to do.
HOST_COMPACT_PS=$(cat <<'PS_EOF'
$ErrorActionPreference = 'SilentlyContinue'

function Get-CFree { (Get-CimInstance Win32_LogicalDisk -Filter "DeviceID='C:'").FreeSpace }

foreach ($vm in @(Get-VM -ErrorAction SilentlyContinue)) {
    $name = $vm.Name

    if (@(Get-VMSnapshot -VMName $name -ErrorAction SilentlyContinue).Count -gt 0) {
        'COMPACT SKIP {0} (has checkpoints -- compacting under one reclaims nothing)' -f $name
        continue
    }

    $disks = @(Get-VMHardDiskDrive -VMName $name -ErrorAction SilentlyContinue |
        ForEach-Object { $_.Path } |
        Where-Object { $_ -and ([IO.Path]::GetFileName($_) -ne 'flatcar-base.vhdx') })
    if ($disks.Count -eq 0) { continue }

    $wasRunning = ($vm.State -eq 'Running')
    if ($wasRunning) {
        'COMPACT STOP {0} (graceful)' -f $name
        Stop-VM -Name $name -Confirm:$false -ErrorAction SilentlyContinue
        $deadline = (Get-Date).AddSeconds(180)
        while ((Get-VM -Name $name).State -ne 'Off' -and (Get-Date) -lt $deadline) {
            Start-Sleep -Seconds 3
        }
    }

    $state = (Get-VM -Name $name).State
    if ($state -ne 'Off') {
        'COMPACT SKIP {0} (did not reach Off within 180s; state={1}) -- left running' -f $name, $state
        if ($wasRunning) { Start-VM -Name $name -ErrorAction SilentlyContinue }
        continue
    }

    foreach ($d in $disks) {
        if (-not (Test-Path -LiteralPath $d)) { continue }
        $b = (Get-Item -LiteralPath $d -Force).Length
        Optimize-VHD -Path $d -Mode Full -ErrorAction SilentlyContinue
        $a = (Get-Item -LiteralPath $d -Force).Length
        # One line on purpose. PowerShell's line-continuation is a backtick, and
        # a lone backtick inside $( ... ) opens a command substitution as far as
        # bash is concerned, whatever the heredoc quoting says -- it parses to
        # find the closing paren before the heredoc body is ever set aside.
        $n = [IO.Path]::GetFileName($d)
        'COMPACT VHDX {0} {1:N0}MB -> {2:N0}MB (reclaimed {3:N0}MB)' -f $n, ($b / 1MB), ($a / 1MB), (($b - $a) / 1MB)
    }

    if ($wasRunning) {
        'COMPACT START {0}' -f $name
        Start-VM -Name $name -ErrorAction SilentlyContinue
    } else {
        'COMPACT {0} was already Off -- left Off (start it yourself when ready)' -f $name
    }
}

$after = Get-CFree
'REPORT FREE_AFTER_GB={0:N2}' -f ($after / 1GB)
PS_EOF
)

# The guest sweep, as root (Flatcar's core user has passwordless sudo). Sent
# over stdin to `sudo sh -s`, so nothing here is interpolated locally.
GUEST_SWEEP_SH=$(cat <<'SH_EOF'
set -e
echo "node: $(hostname)"
echo "journal before: $(journalctl --disk-usage 2>/dev/null)"

mkdir -p /etc/systemd/journald.conf.d
printf '[Journal]\nMaxRetentionSec=1day\nSystemMaxUse=500M\n' \
    > /etc/systemd/journald.conf.d/24h.conf
systemctl restart systemd-journald 2>/dev/null
journalctl --vacuum-time=24h --vacuum-size=500M 2>&1 | tail -n 3
echo "journal after:  $(journalctl --disk-usage 2>/dev/null)"

# Rotated container logs only (pod_*.log.<stamps>); the active *.log kubelet
# holds open and rotates itself.
n=$(find /var/log/pods -type f -name '*.log.*' -mtime +1 -print -delete 2>/dev/null | wc -l)
echo "rotated pod logs older than 1 day removed: $n files"

# Container storage. On a k8s node this is normally the LARGEST consumer and
# the one nothing here used to touch: kubelet's own image GC does not run until
# imagefs crosses 85%, and the guest disk never got near that -- the host
# underneath it ran out first. So the images accumulated indefinitely inside a
# VHDX that only ever grows, which is the growth this script exists to stop.
#
# --prune removes images no container references; rmp -a removes sandboxes that
# have already exited. Neither can touch a running workload.
echo "containerd before: $(du -sh /var/lib/containerd 2>/dev/null | cut -f1)"
if command -v crictl >/dev/null 2>&1; then
    crictl rmp -a 2>/dev/null | tail -n 1 | sed 's/^/  sandboxes: /' || true
    crictl rmi --prune 2>/dev/null | tail -n 1 | sed 's/^/  images: /' || true
    echo "containerd after:  $(du -sh /var/lib/containerd 2>/dev/null | cut -f1)"
else
    echo "  crictl not on PATH -- images NOT pruned"
fi

# Bound kubelet disk usage (ADR-046): early image GC (50%/40%) and eviction
# (40% nodefs/imagefs, 5% inodes, 100Mi memory) keep guest disk growth under ~60%.
mkdir -p /etc/kubernetes/kubelet.conf.d
KUBELET_CHANGED=0

TMP_HOST_DISK=$(mktemp)
printf '%s' "__KUBELET_HOST_DISK_B64__" | base64 -d > "$TMP_HOST_DISK"
if [ ! -f /etc/kubernetes/kubelet.conf.d/50-host-disk.conf ] || ! cmp -s "$TMP_HOST_DISK" /etc/kubernetes/kubelet.conf.d/50-host-disk.conf; then
    cp "$TMP_HOST_DISK" /etc/kubernetes/kubelet.conf.d/50-host-disk.conf
    chmod 0644 /etc/kubernetes/kubelet.conf.d/50-host-disk.conf
    KUBELET_CHANGED=1
fi
rm -f "$TMP_HOST_DISK"

if [ -f /etc/systemd/system/kubelet.service ] && ! grep -q -- '--config-dir=/etc/kubernetes/kubelet.conf.d' /etc/systemd/system/kubelet.service; then
    sed -i 's|--config=/var/lib/kubelet/config.yaml|--config=/var/lib/kubelet/config.yaml --config-dir=/etc/kubernetes/kubelet.conf.d|' /etc/systemd/system/kubelet.service
    KUBELET_CHANGED=1
fi

if [ "$KUBELET_CHANGED" -eq 1 ]; then
    systemctl daemon-reload 2>/dev/null || true
    systemctl restart kubelet 2>/dev/null || true
    echo "kubelet disk bound: applied"
else
    echo "kubelet disk bound: already present"
fi

# fstrim LAST, so it returns every block the steps above freed. On its own this
# still does not shrink the VHDX file: it only marks blocks reusable inside it.
# The host-side Optimize-VHD pass is what actually returns the space to C:, and
# it is worthless unless this ran first.
if command -v fstrim >/dev/null 2>&1; then
    fstrim -av 2>/dev/null | sed 's/^/trim: /' || echo "trim: not supported here (guest frees will not shrink the VHDX)"
fi

df -h / | awk 'NR==2 {print "root fs: " $3 " used of " $2 " (" $5 ")"}'
SH_EOF
)
GUEST_SWEEP_SH="${GUEST_SWEEP_SH/__KUBELET_HOST_DISK_B64__/${KUBELET_HOST_DISK_B64}}"

# ── Sweep ────────────────────────────────────────────────────────────────────

echo "══ home-lab disk sweep · $(date '+%Y-%m-%d %H:%M:%S %Z') ══"
echo "   registry: $ENV_FILE"
echo ""

# Bash 3.2 (macOS default) has no associative arrays, so the swept hosts and
# their before/after free space ride along as delimited strings.
SWEPT_HOSTS=""
SWEPT_ROWS=""
FAILED=0
NODE_IDX=0

# Two passes, and the ORDER is the substance of this script rather than a tidy
# arrangement of it.
#
# Compaction can only reclaim blocks the guest has already released. So every
# guest has to vacuum its journal, prune its images and fstrim BEFORE the host
# is asked to shrink the file underneath it. This script used to sweep each
# host first and then its guests, which compacted disks whose freed blocks had
# not been marked yet and therefore reclaimed almost nothing -- the "I ran it
# and the space did not come back" this ordering exists to fix.
#
# Read the registry on fd 3, not fd 0: a command inside the loop that reads
# stdin (an ssh without -n) would otherwise consume the entries that follow.
echo "══ phase 1 · guests — release blocks inside each VHDX ══"
echo ""
while IFS='|' read -r HOSTNAME SSH_TARGET WSL_DISTRO TAILNET_HOST BOX_TAG NODE_TARGET STARTUP_GB MIN_GB MAX_GB CPUS DISK_GB <&3; do
  [[ -z "${SSH_TARGET:-}" ]] && continue
  NODE_IDX=$((NODE_IDX + 1))

  if ! TARGET="$(resolve_target "$SSH_TARGET")"; then
    echo "✗ $HOSTNAME: no host answered SSH at '$SSH_TARGET' — skipped"
    FAILED=1
    continue
  fi
  HOST_IP="${TARGET#*@}"
  GUEST_PORT=$((2220 + NODE_IDX))

  if [[ " ${SWEPT_HOSTS} " != *" ${TARGET} "* ]]; then
    SWEPT_HOSTS="${SWEPT_HOSTS} ${TARGET}"
  fi

  # Guest phase: the provisioner's own numbering — 2220 + registry index.
  echo "── guest $HOSTNAME ($HOST_IP:$GUEST_PORT) ──"
  GUEST_SSH_OPT=(-o BatchMode=yes -o ConnectTimeout=6 -o StrictHostKeyChecking=no
    -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -p "$GUEST_PORT")
  # -n on the probe only: the sweep itself reads the script from stdin.
  if ! ssh -n "${GUEST_SSH_OPT[@]}" "core@$HOST_IP" "echo ok" &>/dev/null; then
    # Not a failure. A VM that is Off cannot trim, but its disk is still the
    # one most worth compacting -- an outage like 2026-09-25 leaves every guest
    # unreachable and the host desperate for exactly this pass.
    echo "  → unreachable (VM off, or not yet joined) — no trim; its disk is still compacted in phase 2"
  elif ! printf '%s\n' "$GUEST_SWEEP_SH" | ssh "${GUEST_SSH_OPT[@]}" "core@$HOST_IP" "sudo -n sh -s" | sed 's/^/  /'; then
    echo "  ✗ guest sweep failed"
    FAILED=1
  fi
  echo ""
done 3<<< "$HOME_WORKER_NODES"

# Phase 2, once per host however many VMs it carries: delete what no VM claims,
# then compact what one does.
echo "══ phase 2 · hosts — delete unclaimed files, then compact claimed disks ══"
echo ""
for TARGET in $SWEPT_HOSTS; do
  echo "── host $TARGET ──"
  BEFORE=""
  AFTER=""

  SWEEP_OUT=$(win_ps "$TARGET" "$HOST_SWEEP_PS" | tr -d '\r' | grep -v '^\*\*' || true)
  if [[ -z "$SWEEP_OUT" ]]; then
    echo "  ✗ no output from the host sweep — is SSH up?"
    FAILED=1
  else
    printf '%s\n' "$SWEEP_OUT" | sed 's/^/  /'
    # N2 renders with the host's decimal separator; these boxes have been
    # seen with both, and a comma read as "no separator" is 100x the value.
    BEFORE=$(printf '%s\n' "$SWEEP_OUT" | awk -F= '/^REPORT FREE_BEFORE_GB=/{print $2}' | tr -d ' ' | tr ',' '.')
  fi

  # Minutes, not seconds: Optimize-VHD rewrites a multi-GB file, and each VM is
  # shut down and brought back in turn. No ssh timeout bounds this deliberately
  # -- killing a compaction midway is how a VHDX gets left inconsistent.
  echo "  ── compaction · stops each VM, compacts, restarts what was running ──"
  COMPACT_OUT=$(win_ps "$TARGET" "$HOST_COMPACT_PS" | tr -d '\r' | grep -v '^\*\*' || true)
  if [[ -z "$COMPACT_OUT" ]]; then
    echo "  ✗ no output from the compaction pass"
    FAILED=1
  else
    printf '%s\n' "$COMPACT_OUT" | sed 's/^/  /'
    AFTER=$(printf '%s\n' "$COMPACT_OUT" | awk -F= '/^REPORT FREE_AFTER_GB=/{print $2}' | tr -d ' ' | tr ',' '.')
  fi

  SWEPT_ROWS="${SWEPT_ROWS}${TARGET}|${BEFORE:-?}|${AFTER:-?};"
  echo ""
done

# ── Report ───────────────────────────────────────────────────────────────────

echo "══ summary ══"
if [[ -z "$SWEPT_ROWS" ]]; then
  echo "  ✗ no host was reachable — nothing was swept"
  exit 1
fi

UNDER=0
IFS=';' read -r -a ROWS <<< "$SWEPT_ROWS"
for ROW in "${ROWS[@]}"; do
  [[ -z "$ROW" ]] && continue
  IFS='|' read -r TARGET BEFORE AFTER <<< "$ROW"
  printf '  %-24s %8s GB → %8s GB free\n' "$TARGET" "$BEFORE" "$AFTER"
  if [[ "$AFTER" != "?" ]] && awk -v a="$AFTER" -v m="$MIN_FREE_GB" 'BEGIN { exit !(a + 0 < m + 0) }'; then
    echo "      ⚠ below ${MIN_FREE_GB} GB free — run this again soon, or move data off C:"
    UNDER=1
  fi
done

if [[ "$FAILED" -ne 0 || "$UNDER" -ne 0 ]]; then
  echo ""
  echo "✗ sweep incomplete or a host is low on space (threshold ${MIN_FREE_GB} GB)." >&2
  exit 1
fi
echo ""
echo "✓ sweep complete — every host above ${MIN_FREE_GB} GB free."
