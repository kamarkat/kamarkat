#!/usr/bin/env bash
# scope: hub
# Each spoke identity is a viewer of its own cluster's project and nothing else
# (ADR-031, ADR-037).
#
# WHAT THIS CAN AND CANNOT ESTABLISH. It reads each identity's effective project
# memberships from Infisical, as the platform's own machine identity -- the
# verifier the platform already uses in cluster/30-oauth-clients.sh. It does NOT
# authenticate as a spoke identity to attempt a forbidden read: that needs a
# spoke's client secret in the verifier's hands, and extracting one to prove a
# denial is a worse trade than the proof is worth.
#
# The denial follows from the membership set plus the role's definition, which is
# checkable in the pinned source: a built-in project role grants nothing outside
# its own project (Infisical v0.167.12 default-roles.ts), so an identity that is a
# member of exactly one project cannot read another's. That is why "exactly one"
# is the assertion, and why an extra membership is a failure rather than a note.
#
# Values are never printed. Identity names, project slugs and role names only.
_iso_pf_start() {
    local ns="$1" svc="$2" port="$3" logf pf_pid local_port i
    logf="$(mktemp)"
    kubectl --kubeconfig="$HUB_KUBECONFIG" port-forward -n "$ns" "svc/$svc" ":$port" \
        --address 127.0.0.1 >"$logf" 2>&1 &
    pf_pid=$!
    for i in $(seq 1 40); do
        local_port="$(sed -nE 's|^Forwarding from 127\.0\.0\.1:([0-9]+).*|\1|p' "$logf" | head -1)"
        [[ -n "$local_port" ]] && break
        kill -0 "$pf_pid" 2>/dev/null || break
        sleep 0.25
    done
    rm -f "$logf"
    if [[ -z "$local_port" ]]; then
        kill "$pf_pid" 2>/dev/null || true
        return 1
    fi
    echo "$pf_pid $local_port"
}

validate_cluster_secret_isolation() {
    section "Each spoke identity is scoped to its own cluster's project (ADR-031)"

    local declared
    declared=$(kc get spokemachineidentity -A -o json 2>/dev/null | python3 -c "
import json, sys
try:
    items = json.load(sys.stdin).get('items', [])
except Exception:
    items = []
for i in items:
    spec = i.get('spec', {})
    print(spec.get('spokeRef', {}).get('name', ''), (spec.get('infisical') or {}).get('secretsProjectSlug', ''))
" 2>/dev/null)

    if [[ -z "$declared" ]]; then
        inapplicable "this box declares no workload cluster identity, so there is no spoke scope to assess"
        return 0
    fi

    local client_id client_secret cert_project
    client_id=$(kc get secret infisical-auth -n platform-ops -o jsonpath='{.data.client-id}' | base64 -d 2>/dev/null)
    client_secret=$(kc get secret infisical-auth -n platform-ops -o jsonpath='{.data.client-secret}' | base64 -d 2>/dev/null)
    cert_project=$(kc get secret infisical-auth -n platform-ops -o jsonpath='{.data.projectId}' | base64 -d 2>/dev/null)
    # The organization, from the ConfigMap the operators themselves read it from
    # (hub-bootstrap-config/SECRET_STORE_ORGANIZATION_ID). The identity listing
    # requires it; without it every lookup returned 422 and the module reported an
    # identity it had never managed to ask about.
    local org_id
    org_id=$(kc get configmap hub-bootstrap-config -n platform-ops \
        -o jsonpath='{.data.SECRET_STORE_ORGANIZATION_ID}' 2>/dev/null)
    if [[ -z "$org_id" ]]; then
        unverified "the organization id is not readable from hub-bootstrap-config, so no identity's memberships could be looked up"
        return 1
    fi
    if [[ -z "$client_id" || -z "$client_secret" ]]; then
        unverified "the platform's own Infisical credential is not readable, so no identity's effective memberships could be established"
        return 0
    fi

    local pf pid port
    if ! pf=$(_iso_pf_start platform-security infisical-standalone-infisical 8080); then
        unverified "Infisical is not reachable, so no identity's memberships could be read"
        return 0
    fi
    read -r pid port <<< "$pf"

    local out
    out=$(SECRET_STORE_BASE="http://127.0.0.1:$port" \
          SECRET_STORE_ORGANIZATION_ID="$org_id" \
        SECRET_STORE_CLIENT_ID="$client_id" \
          SECRET_STORE_CLIENT_SECRET="$client_secret" \
          CERT_PROJECT_ID="$cert_project" \
          DECLARED="$declared" \
          python3 "$VALIDATE_DIR/cluster/031-cluster-secret-isolation.py" 2>&1)
    kill "$pid" 2>/dev/null || true

    local line
    while IFS= read -r line; do
        [[ -z "$line" ]] && continue
        case "$line" in
            PASS\|*) pass "${line#PASS|}" ;;
            FAIL\|*) hard_fail "${line#FAIL|}" ;;
            SOFT\|*) soft_fail "${line#SOFT|}" ;;
            # A verifier that could not put its question is not a converging
            # system. soft_fail says "expected to self-heal"; this says the
            # property was not assessed, which is the honest result and does not
            # read as a pass in the summary.
            UNVERIFIED\|*) unverified "${line#UNVERIFIED|}" ;;
            NOTE\|*) note "${line#NOTE|}" ;;
            *)       note "$line" ;;
        esac
    done <<< "$out"
}
