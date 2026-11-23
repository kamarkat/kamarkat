#!/usr/bin/env bash
# One Infisical project per workload cluster, agreed on by everything (ADR-031, ADR-037).
#
# The project is the only secret boundary an unlicensed Infisical enforces: a
# built-in role carries no environment or path condition, and conditioning one
# needs a paid feature. So the project slug is a contract between hub-operator,
# which creates the project and writes the credentials into it, and the
# ApplicationSet, which points each spoke's ClusterSecretStore at it. If the two
# disagree the store reads a project nobody wrote to while the identity is a
# viewer of a project nothing reads -- and each half looks correct alone.
validate_cluster_secret_project() {
    section "One secrets project per workload cluster (ADR-031, ADR-037)"

    local out
    out=$(cd "$VALIDATE_ROOT" && python3 "$VALIDATE_DIR/preflight/037-cluster-secret-project.py") || true

    if [[ -z "$out" ]]; then
        hard_fail "the cluster secrets project check produced no output"
        return 0
    fi

    local line
    while IFS= read -r line; do
        [[ -z "$line" ]] && continue
        case "$line" in
            OK*)  pass "$(printf '%s' "$line" | cut -f2-)" ;;
            BAD*) hard_fail "$(printf '%s' "$line" | cut -f2-)" ;;
            *)    note "$line" ;;
        esac
    done <<< "$out"
}
