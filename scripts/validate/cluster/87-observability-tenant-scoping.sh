#!/usr/bin/env bash

# Collection is scoped to platform namespaces, and carries a real cluster identity.
#
# ADR-078 records both of these as live defects in its own Context: Alloy scrapes
# "discovery.kubernetes.pods and discovery.kubernetes.services with no namespace
# filter -- every tenant workload's metrics, forwarded off-cluster -- and stamps
# external_labels = { cluster = "hub" }, a constant, so two clusters arriving at
# one destination are indistinguishable."
#
# The first is a boundary violation under ADR-066 and does not become less true
# because the destination is changing. The second is worse than it looks:
# mislabelled data is being written now and cannot be relabelled later, which is
# why ADR-078 addendum 1 decision 14 sequences both fixes AHEAD of the storage
# build rather than with it.
#
# ADR-078 is Proposed, so this reports as a warning rather than failing the run.
# It becomes a hard failure when ADR-078 is Accepted -- which is the same rule
# architecture-consistency.py applies to a Proposed ADR's acceptance tests, and
# for the same reason: mid-build is allowed, silent mid-build is not.

validate_observability_tenant_scoping() {
    section "Collection is scoped and identified (ADR-078 §3)"

    platform_source_available "manifests/hub-core-services/grafana-alloy/deployment.yaml" \
        "manifests/spoke/spoke-catalog/infra/grafana-alloy.yaml" || return 0

    local -a configs=(
        "$VALIDATE_ROOT/manifests/hub-core-services/grafana-alloy/deployment.yaml"
        "$VALIDATE_ROOT/manifests/spoke/spoke-catalog/infra/grafana-alloy.yaml"
    )

    local found=0 issues=0 f
    for f in "${configs[@]}"; do
        [ -f "$f" ] || continue
        (( found++ ))
        local rel="${f#$VALIDATE_ROOT/}"

        # 1. Platform-owned namespaces are selected by label. ADR-078 §3 names
        #    the mechanism, and the support agent's NetworkPolicy already relies
        #    on the same key, so a config without it is collecting unscoped.
        # The META-LABEL form, not the Kubernetes form.
        #
        # An Alloy config can only spell this one way: Prometheus meta-labels
        # replace dots and slashes with underscores, so the rule reads
        # __meta_kubernetes_namespace_label_topology_platform_io_role. Grepping
        # for "topology.platform.io/role" was wrong in both directions -- it
        # missed the spoke, which scopes correctly, and passed the hub on the
        # strength of the string appearing in a COMMENT. Verified 2026-09-19:
        # with every real scoping rule stripped out, the hub still passed.
        if ! grep -q "namespace_label_topology_platform_io_role" "$f"; then
            warn "$rel does not scope collection by topology.platform.io/role (ADR-078 §3)"
            note "unscoped discovery collects every tenant namespace, which ADR-066"
            note "puts on the wrong side of the platform boundary"
            (( issues++ ))
        fi

        # 2. The cluster label must not be a literal. A constant means two boxes
        #    at one destination cannot be told apart, and the data written under
        #    it cannot be repaired afterwards.
        if grep -qE 'cluster[[:space:]]*=[[:space:]]*"(hub|spoke)"' "$f"; then
            warn "$rel stamps a constant cluster label (ADR-078 §3: 'external_labels carries the cluster's real identity, not a constant')"
            (( issues++ ))
        fi

        # 3. kube-state-metrics is cluster-scoped and cannot be bounded by target,
        #    so its series must be bounded at the scrape. ADR-078 addendum 1 §6.
        # The keep must be on the kube-state-metrics pipeline, not merely the
        # word "namespace" somewhere in the file -- which every Alloy config
        # contains many times over, so the original form could never fail.
        if grep -q "kube-state-metrics" "$f" \
           && ! grep -q "prometheus.relabel \"platform_namespaces_only\"" "$f"; then
            warn "$rel scrapes kube-state-metrics with no namespace keep rule (ADR-078 addendum 1 §6)"
            (( issues++ ))
        fi
    done

    if (( found == 0 )); then
        hard_fail "no Alloy configuration found; this check inspected nothing"
        return 0
    fi

    if (( issues )); then
        note "ADR-078 is Proposed; these are warnings until it is Accepted, at which"
        note "point the claims in §3 have to be true rather than intended."
        return 0
    fi

    pass "$found Alloy configuration(s): scoped by platform label, real cluster identity"
}
