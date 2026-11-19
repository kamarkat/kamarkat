#!/usr/bin/env bash
# The hub must have somewhere to run its own platform (ADR-014, ADR-046 §11/§19).
#
# §19 moved the hub's workloads onto home-lab Flatcar nodes and scaled the Hetzner
# worker MachineDeployment to 0, and §11 keeps the control plane tainted. So a hybrid
# hub has exactly one place its platform can run, and that place is an unmanaged
# Hyper-V VM on a workstation — a machine that can sleep, reboot, or lose its tailnet
# address with nothing in Kubernetes to prevent it.
#
# When it goes, the failure is loud but anonymous: every Deployment reports
# `0/N available` at once and nothing names the cause. On 2026-08-23 that produced
# 53 pods Terminating and 48 Pending across argocd, crossplane, kyverno, cnpg, ory,
# cert-manager and hub-operator — and the bootstrap still printed
# "Hub cluster: Ready and operational" because nothing asserted the node.
#
# Node Ready is checked here rather than pod health because pod health is the
# symptom: by the time Deployments report 0/N the useful fact is already three
# layers down. This is also NOT covered by 60-kubelet-reachability: that probes
# nodes the API server can still see, and a node whose kubelet has stopped posting
# is a different failure.
validate_hub_placement_capacity() {
    section "Hub has capacity for its own platform (ADR-014 / §19)"

    # The control plane is tainted on a hybrid hub, so it is not capacity. On a
    # pure-Hetzner hub the worker MachineDeployment provides nodes and no
    # workload-location label exists at all — nothing to assert, so skip cleanly.
    local labelled
    labelled=$(kc get nodes -l 'workload-location' -o name | grep -c . || true)
    if [[ "${labelled:-0}" -eq 0 ]]; then
        pass "no workload-location nodes — not a hybrid hub, placement is CAPI-managed"
        return 0
    fi

    local line ready=0 total=0 notready=()
    while IFS= read -r line; do
        [[ -z "$line" ]] && continue
        total=$((total + 1))
        if [[ "${line#*=}" == "True" ]]; then
            ready=$((ready + 1))
        else
            notready+=("${line%%=*}")
        fi
    done < <(kc get nodes -l 'workload-location=on-prem,node-role.kubernetes.io/worker' \
        -o jsonpath='{range .items[*]}{.metadata.name}{"="}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}')

    if (( ready >= 1 )); then
        pass "hub home worker Ready ($ready/$total)"
    else
        soft_fail "hub has no Ready home worker (${total} matched) — the control plane is tainted, so every platform workload has nowhere to schedule"
        (( ${#notready[@]} )) && note "not Ready: ${notready[*]}"
        note "check the Hyper-V VM and its host; 'Kubelet stopped posting node status' means the node, not the cluster"
        return 0
    fi

    # A Ready node that cannot be scheduled onto is the same outage with a friendlier
    # status, so cordon is checked separately rather than folded into Ready.
    local schedulable
    schedulable=$(kc get nodes -l 'workload-location=on-prem,node-role.kubernetes.io/worker' \
        -o jsonpath='{range .items[?(@.spec.unschedulable==true)]}{.metadata.name}{"\n"}{end}' | grep -c . || true)
    if [[ "${schedulable:-0}" -eq 0 ]]; then
        pass "hub home worker is schedulable"
    else
        soft_fail "$schedulable hub home worker(s) cordoned — Ready but unusable for placement"
    fi
}
