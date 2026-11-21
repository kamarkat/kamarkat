#!/usr/bin/env bash
# ADR-046 — Home-lab Windows host disk headroom check.
#
# WHY THIS EXISTS
#
# On 2026-09-25 and 2026-10-11, C: on the Dell home-lab host reached 0 bytes free.
# Hyper-V reported "Disk Full" on the dynamically expanding VHDX files and paused
# the spoke VMs, turning them off 30 minutes later. The kubelet raised no disk-pressure
# event because its eviction thresholds only measure the guest filesystem (which was
# not full), while the host backing the virtual disk was exhausted.
#
# This check verifies that every Windows host running home-lab Flatcar worker VMs
# maintains at least 20 GB free on C:. Any host with less than 20 GB free fails
# cluster validation before Hyper-V pauses the guest.
#
# Static counterpart is in preflight/046-home-node-disk-bound.py.

if ! declare -f pass >/dev/null 2>&1; then
    _COMMON="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/common.sh"
    # shellcheck source=../common.sh
    [[ -f "$_COMMON" ]] && source "$_COMMON"
fi

_resolve_target() {
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

validate_home_host_disk_headroom() {
    section "Home-lab Windows host disk headroom (ADR-046)"

    # On a pure-Hetzner hub the worker MachineDeployment provides nodes and no
    # on-prem home worker exists — nothing to assert, so skip cleanly.
    local labelled
    labelled=$(kc get nodes -l 'workload-location=on-prem' -o name 2>/dev/null | grep -c . || true)
    if [[ "${labelled:-0}" -eq 0 ]]; then
        pass "no workload-location=on-prem nodes — not a hybrid hub, no home-lab hosts to check"
        return 0
    fi

    local env_file="${VALIDATE_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)}/scripts/hybrid/home-lab.env"
    if [[ ! -f "$env_file" ]]; then
        hard_fail "home-lab.env not found at $env_file"
        return 0
    fi

    # shellcheck source=/dev/null
    source "$env_file"

    if [[ -z "${HOME_WORKER_NODES:-}" ]]; then
        hard_fail "HOME_WORKER_NODES is not defined in $env_file"
        return 0
    fi

    local host_targets=()
    local line hostname ssh_target _rest
    while IFS='|' read -r hostname ssh_target _rest; do
        [[ -z "${ssh_target:-}" ]] && continue
        local resolved
        if ! resolved="$(_resolve_target "$ssh_target")"; then
            hard_fail "$hostname: host unreachable at '$ssh_target'"
            continue
        fi
        local already=0
        for t in "${host_targets[@]}"; do
            if [[ "$t" == "$resolved" ]]; then
                already=1
                break
            fi
        done
        if [[ "$already" -eq 0 ]]; then
            host_targets+=("$resolved")
        fi
    done <<< "$HOME_WORKER_NODES"

    if (( ${#host_targets[@]} == 0 )); then
        hard_fail "no home-lab Windows host could be reached"
        return 0
    fi

    local ps_script
    ps_script='
$free = (Get-PSDrive C).Free
"REPORT FREE_GB={0:N2}" -f ($free / 1GB)
$vms = @(Get-VM -ErrorAction SilentlyContinue)
foreach ($v in $vms) {
    "REPORT VM={0} {1}" -f $v.Name, $v.State
    $disks = @(Get-VMHardDiskDrive -VMName $v.Name -ErrorAction SilentlyContinue)
    foreach ($d in $disks) {
        $len = (Get-Item -LiteralPath $d.Path -ErrorAction SilentlyContinue).Length
        "REPORT DISK={0}|{1}|{2:N2}" -f $v.Name, $d.Path, ($len / 1GB)
    }
}
'

    local target
    for target in "${host_targets[@]}"; do
        local out
        out=$(printf '%s\n' "$ps_script" | ssh -o BatchMode=yes -o ConnectTimeout=15 -o StrictHostKeyChecking=no \
            -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR "$target" \
            "powershell -NoProfile -ExecutionPolicy Bypass -Command -" 2>&1 | tr -d '\r' | grep -v '^\*\*' || true)

        local free_gb
        free_gb=$(printf '%s\n' "$out" | awk -F= '/^REPORT FREE_GB=/{print $2}' | tr -d ' ' | tr ',' '.')
        if [[ -z "$free_gb" ]]; then
            hard_fail "host $target: failed to read C: free space"
            continue
        fi

        while IFS= read -r vm_line; do
            [[ -z "$vm_line" ]] && continue
            local disk_info="${vm_line#REPORT DISK=}"
            local d_vm d_path d_size
            IFS='|' read -r d_vm d_path d_size <<< "$disk_info"
            local d_file
            d_file=$(basename "$d_path")
            note "$target: $d_vm disk $d_file is ${d_size} GB"
        done < <(printf '%s\n' "$out" | grep '^REPORT DISK=')

        if awk -v f="$free_gb" -v m="20.0" 'BEGIN { exit !(f + 0 < m + 0) }'; then
            hard_fail "host $target: C: has only ${free_gb} GB free (< 20 GB reserve)"
        else
            pass "host $target: C: has ${free_gb} GB free (>= 20 GB reserve)"
        fi
    done
}
