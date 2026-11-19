#!/usr/bin/env bash
# scope: hub
# A provisioned workload cluster that is not REGISTERED receives nothing.
#
# Registration is a fact on the management cluster: capi2argo writes an ArgoCD
# cluster Secret for the cluster, labelled so the platform's ApplicationSet
# generator selects it. Until that Secret exists the generator matches no
# cluster, so not one platform Application is ever created for it -- and nothing
# reports this, because an ApplicationSet that generates zero Applications is
# Healthy, and a cluster nothing was deployed to is Ready.
#
# No check asserted it. The gap was found while answering what the bootstrap does
# when a cluster is declared but not registered (2026-11-19): the answer was that
# the workload-cluster checks would fail one by one, each naming a missing
# workload, none naming the reason.
#
# PROVISIONED FIRST. A cluster still being built is not expected to be
# registered, so this reports pending for it rather than failing -- registration
# is a consequence of provisioning, and failing on the interval between them
# would fail every bootstrap while it was still running.
validate_managed_cluster_registered() {
    section "Declared workload clusters are registered on the management cluster"

    [[ -z "${ZERO_OPS_DIR:-}" ]] && {
        unverified "the box's repository was not named, so its declarations could not be read"
        return 0
    }

    local -a declared=()
    local d
    for d in "$ZERO_OPS_DIR"/registry/clusters/*/infrastructure/spokepool.yaml; do
        [[ -e "$d" ]] || continue
        d="${d%/infrastructure/spokepool.yaml}"
        declared+=("$(basename "$d")")
    done
    [[ ${#declared[@]} -eq 0 ]] && {
        inapplicable "this box declares no workload cluster, so there is nothing to register"
        return 0
    }

    local pool ready secret
    for pool in "${declared[@]}"; do
        # The claim's own report of whether the cluster exists yet. Read from the
        # SpokePool rather than inferred from the Secret's absence, so "still
        # building" and "built and not registered" are different answers.
        ready=$(kc get spokepool "$pool" -n platform-ops \
            -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)
        if [[ "$ready" != "True" ]]; then
            pending "$pool is declared and not provisioned yet, so registration is not due"
            continue
        fi

        # The generator selects on this label. A Secret without it is invisible
        # to the generator, which is the same outcome as no Secret at all, so the
        # label is part of what is asserted rather than a detail of it.
        secret=$(kc get secrets -n platform-argocd \
            -l "argocd.argoproj.io/secret-type=cluster,spoke-type=pool" \
            -o jsonpath="{.items[?(@.metadata.name==\"$pool\")].metadata.name}" 2>/dev/null)
        if [[ -z "$secret" ]]; then
            hard_fail "$pool is provisioned and NOT REGISTERED: no ArgoCD cluster Secret labelled spoke-type=pool exists for it in platform-argocd, so the platform's generator selects it for nothing and no Application will ever be created for this cluster"
            continue
        fi
        pass "$pool is registered, so the platform's generator can target it"
    done
}
