#!/usr/bin/env bash
# The hub's secret store (ADR-117 §4): the one ClusterSecretStore on the hub reads
# the box's OpenBao, verifies it against the listener CA, and logs in as eso-hub,
# whose policy reads the hub scope and nothing else. A second store, or one that
# skips verification, is a path to secrets that the policy does not describe.
#
# Run as an in-flight gate once the HubEnvironment and the store exist.

validate_secret_store_isolation() {
    section "Hub secret store (OpenBao)"
    local ready
    ready=$(kc get clustersecretstore openbao-hub \
        -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)
    if [[ -z "$ready" ]]; then
        hard_fail "ClusterSecretStore openbao-hub is absent or reports no Ready condition"
        return 0
    elif [[ "$ready" != "True" ]]; then
        hard_fail "ClusterSecretStore openbao-hub is not Ready: $(kc get clustersecretstore openbao-hub \
            -o jsonpath='{.status.conditions[?(@.type=="Ready")].message}')"
    else
        pass "ClusterSecretStore openbao-hub Ready"
    fi

    local ca role
    ca=$(kc get clustersecretstore openbao-hub -o jsonpath='{.spec.provider.vault.caProvider.name}')
    role=$(kc get clustersecretstore openbao-hub -o jsonpath='{.spec.provider.vault.auth.kubernetes.role}')
    if [[ "$ca" == "openbao-listener-trust" ]]; then
        pass "openbao-hub verifies the server against the listener CA"
    else
        hard_fail "openbao-hub does not verify against openbao-listener-trust (caProvider=${ca:-none})"
    fi
    if [[ "$role" == "eso-hub" ]]; then
        pass "openbao-hub logs in as eso-hub"
    else
        hard_fail "openbao-hub logs in as '${role:-nothing}', not the restricted eso-hub role"
    fi

    # No store may still point at the retired self-hosted secret store.
    local legacy
    legacy=$(kc get clustersecretstores,secretstores -A \
        -o jsonpath='{range .items[?(@.spec.provider.infisical)]}{.metadata.namespace}/{.metadata.name}{" "}{end}' 2>/dev/null)
    if [[ -n "$legacy" ]]; then
        hard_fail "secret stores still use the infisical provider: $legacy"
    else
        pass "no secret store uses the retired infisical provider"
    fi
}
