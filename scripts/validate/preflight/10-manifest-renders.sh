#!/usr/bin/env bash
# A kustomization or chart that does not build takes ArgoCD down a
# ComparisonError path which reports as "Unknown" rather than "Degraded" — easy
# to miss in a wall of sync output, and it needs no cluster to detect.
validate_manifest_renders() {
    section "Manifest renders"

    # THE ENVIRONMENTS THAT EXIST. This listed dev, stg and prod, which ADR-111
    # removed when the management hub became environment-independent (b2f98739) --
    # so this check hard-failed on three directories the platform deliberately
    # deleted, and a gate that is always red is a gate nobody reads. `ephemeral`
    # is deliberately absent too: it is a single ApplicationSet, with no
    # kustomization to build.
    local targets=(
        "manifests/environments/base"
        "manifests/environments/management"
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
    #
    # THE DECLARATION A BOX ACTUALLY GETS, not a list of --set flags.
    #
    # This was nine hand-written --set values, and one of them was
    # `alerting.emailTo[0]=alerts@example.test`. The chart requires that key
    # while observability is on and refuses to render without it; the tenant
    # declaration template did not carry it. So the chart was only ever rendered
    # against a fixture that supplied what the real values file lacked, every
    # gate passed, and the first tenant declaration ArgoCD rendered failed --
    # taking every ApplicationSet with it and stalling boundary 01 for its whole
    # budget on "no ApplicationSet targets project boundary-01", which names
    # neither the chart nor the key.
    #
    # Rendering the template itself closes that: a required value missing from
    # the file a box is created with now fails here instead of on a box. The
    # tokens are substituted the way `kakt box init` substitutes them, with
    # example facts, and a token left over is itself a failure -- a new one added
    # to the template without being given a value here would otherwise reach the
    # chart as the literal string "<SOMETHING>".
    # ONE FIXTURE for every gate that renders this chart; see
    # scripts/validate/management-declaration-fixture.sh for why there is only
    # one, and what the three separate ones cost.
    local rendered
    rendered=$(mktemp)
    if ! "$VALIDATE_ROOT/scripts/validate/management-declaration-fixture.sh" > "$rendered"; then
        hard_fail "the management declaration fixture could not be rendered (see above)"
    fi
    local em_values=(-f "$rendered")

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
