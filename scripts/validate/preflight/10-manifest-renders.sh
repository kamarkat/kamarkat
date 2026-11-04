#!/usr/bin/env bash
# A kustomization or chart that does not build takes ArgoCD down a
# ComparisonError path which reports as "Unknown" rather than "Degraded" — easy
# to miss in a wall of sync output, and it needs no cluster to detect.
validate_manifest_renders() {
    section "Manifest renders"

    local targets=(
        "manifests/environments/dev"
        "manifests/environments/stg"
        "manifests/environments/prod"
        "manifests/providers/hybrid"
        "manifests/providers/hetzner"
        "manifests/providers/hetzner/base"
        "manifests/spoke/spoke-catalog/infra"
        "manifests/hub-core-services/cluster-secret-store"
    )
    local t err
    for t in "${targets[@]}"; do
        if [[ ! -d "$VALIDATE_ROOT/$t" ]]; then
            hard_fail "kustomize target missing: $t"
            continue
        fi
        if err=$(kubectl kustomize "$VALIDATE_ROOT/$t" 2>&1 >/dev/null); then
            pass "kustomize builds: $t"
        else
            hard_fail "kustomize fails: $t — $(head -1 <<< "$err")"
        fi
    done

    # ADR-055: environment-manager renders every boundary on every render, so it
    # must be given the environment values the seed Application carries.
    # publicTlsIssuer deliberately has no default (ADR-051) and must be named.
    local em_values=(--set environmentSlug=prod --set boxId=example-box --set provider=hetzner
                     --set topology=single --set publicTlsIssuer=letsencrypt-prod
                     --set environmentRevision=main \
                     --set instanceRepoURL=https://github.com/example-org/example-gitops --set hubDomain=dev.example.test --set previewDomain=example-preview.test --set 'alerting.emailTo[0]=alerts@example.test')

    # ADR-088: the tenant chart renders one APP of one TENANT, and neither
    # identifier defaults to the other. A fixture short of either reports the
    # chart broken when it is the fixture that is incomplete.
    local ut_values=(--set tenantId=example --set appId=demo --set clusterName=spoke-01
                     --set oidcIssuer=https://id.example.test)

    local chart
    for chart in manifests/argocd/environment-manager manifests/tenants/charts/universal-tenant internal/kube-sbt/providers/gitops/helm-chart; do
        local values=()
        if [[ "$chart" == "manifests/argocd/environment-manager" ]]; then
            values=("${em_values[@]}")
        fi
        if [[ "$chart" == "manifests/tenants/charts/universal-tenant" ]]; then
            values=("${ut_values[@]}")
        fi
        if err=$(helm template "$VALIDATE_ROOT/$chart" "${values[@]+"${values[@]}"}" 2>&1 >/dev/null); then
            pass "helm renders: $chart"
        else
            hard_fail "helm fails: $chart — $(head -1 <<< "$err")"
        fi
    done
}
