#!/usr/bin/env bash
# A wrong Infisical slug makes every ExternalSecret on this hub resolve against
# another scope — while reporting Healthy. There is no behavioural signal that
# distinguishes it from correct operation, which is why it is asserted explicitly
# and why a mismatch is a hard failure in both modes: a wrong slug does not
# converge into the right one.
#
# WHAT IS COMPARED CHANGED (ADR-111 Revision 7). This checked both objects
# against $ENVIRONMENT, the environment the hub was being bootstrapped as. That
# was the coupling the HubEnvironment CRD also enforced in CEL --
# secrets.infisical.environmentSlug == spec.environment -- and it made the hub's
# own credentials live in a workload environment by construction. The hub declares
# no environment now; it declares a management scope.
#
# So the comparison is against the DECLARED scope, read from hub-bootstrap-config,
# which is where the hub-operator reads it too (managementEnvironmentSlug). Three
# places carry one value -- the ConfigMap, the HubEnvironment and the
# ClusterSecretStore -- and all three are rendered from
# management.infisicalEnvironmentSlug, so disagreement means a partial render or a
# hand edit, and that is exactly what this catches.
validate_environment_isolation() {
    section "Management secret scope (Infisical slug)"

    # The declared scope. Read first: without it there is nothing to compare
    # against, and comparing the other two to each other would pass a box where
    # both are wrong in the same way.
    local declared
    declared=$(kc -n platform-ops get configmap hub-bootstrap-config \
        -o jsonpath='{.data.INFISICAL_ENVIRONMENT_SLUG}')
    if [[ -z "$declared" ]]; then
        soft_fail "hub-bootstrap-config carries no INFISICAL_ENVIRONMENT_SLUG, so the management scope is undeclared and nothing can be checked against it"
        return 0
    fi
    pass "declared management scope: $declared"

    local he
    he=$(kc get hubenvironment -A -o jsonpath='{.items[0].spec.secrets.infisical.environmentSlug}')
    if [[ -z "$he" ]]; then
        soft_fail "HubEnvironment not present, so its Infisical slug cannot be verified"
    elif [[ "$he" == "$declared" ]]; then
        pass "HubEnvironment environmentSlug=$he"
    else
        hard_fail "HubEnvironment environmentSlug=$he but this box declares $declared — the hub-operator would read another scope's secrets"
    fi

    # The store resource is named infisical-backend. "platform-cluster-secret-store"
    # is the ArgoCD Application that delivers it — querying that name finds nothing
    # and the check silently passes as "not created yet".
    local css
    css=$(kc get clustersecretstore infisical-backend \
        -o jsonpath='{.spec.provider.infisical.secretsScope.environmentSlug}')
    if [[ -z "$css" ]]; then
        soft_fail "ClusterSecretStore infisical-backend not created, so its Infisical slug cannot be verified"
    elif [[ "$css" == "$declared" ]]; then
        pass "ClusterSecretStore infisical-backend environmentSlug=$css"
    else
        hard_fail "ClusterSecretStore infisical-backend environmentSlug=$css but this box declares $declared — every ExternalSecret on this hub resolves against the wrong scope"
    fi

    # The SECOND management-scope store on this hub. It reads /<tenant>-mgmt, the
    # folder hub-operator writes the owner's own credentials to, and it carried a
    # literal `dev` of its own -- correct only while the hub's scope happened to be
    # dev, and silent when it was not: the secret exists, in another scope, and the
    # ExternalSecret reports only that it could not get secret data.
    local mgmt
    mgmt=$(kc get clustersecretstore infisical-tenant-mgmt \
        -o jsonpath='{.spec.provider.infisical.secretsScope.environmentSlug}')
    if [[ -z "$mgmt" ]]; then
        soft_fail "ClusterSecretStore infisical-tenant-mgmt not created, so its Infisical slug cannot be verified"
    elif [[ "$mgmt" == "$declared" ]]; then
        pass "ClusterSecretStore infisical-tenant-mgmt environmentSlug=$mgmt"
    else
        hard_fail "ClusterSecretStore infisical-tenant-mgmt environmentSlug=$mgmt but this box declares $declared — the owner's credentials are written to one scope and read from another"
    fi
}
