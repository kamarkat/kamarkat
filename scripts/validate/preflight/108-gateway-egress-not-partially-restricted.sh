#!/usr/bin/env bash
# ADR-108: tenant gateway egress must not be restricted by network policy.
#
# Tenant gateway pods carry no tenant-id and rely on unrestricted egress for DNS
# resolution (node-local-dns:53) and fetching OIDC discovery/keys. Any policy with
# an egress section selecting the gateway pod (app: agentgateway-<app>) activates
# Cilium endpoint default-deny egress, silently dropping DNS and causing crash loops.
#
#   108-1  no CiliumNetworkPolicy or NetworkPolicy selecting agentgateway-<app>
#          has an egress section.

validate_gateway_egress_not_partially_restricted() {
    section "ADR-108: tenant gateway egress is not restricted"

    if ! command -v helm >/dev/null 2>&1; then
        soft_fail "helm is needed to render universal-tenant chart"
        return 0
    fi

    local out
    out=$(cd "$VALIDATE_ROOT" && python3 - <<'PY'
import subprocess, tempfile, yaml

FIXTURE = {
    "tenantId": "test-tenant",
    "appId": "test-app",
    "clusterName": "spoke-1",
    "oidcIssuer": "https://id.example.test",
    "gateway": {
        "enabled": True,
        "hostnames": ["app.example.com"],
    },
    "previewAccess": {
        "enabled": True,
    },
}

with tempfile.NamedTemporaryFile("w", suffix=".yaml") as f:
    yaml.safe_dump(FIXTURE, f)
    f.flush()
    r = subprocess.run(
        ["helm", "template", "t", "manifests/tenants/charts/universal-tenant", "-f", f.name],
        capture_output=True,
        text=True,
    )

if r.returncode != 0:
    print("FAIL universal-tenant fixture failed to render: " + r.stderr.strip()[:300])
    raise SystemExit

docs = [d for d in yaml.safe_load_all(r.stdout) if d]
gateway_app = "agentgateway-" + FIXTURE["appId"]

bad_policies = []
for d in docs:
    kind = d.get("kind")
    if kind not in ("CiliumNetworkPolicy", "NetworkPolicy"):
        continue
    spec = d.get("spec", {})
    selector = spec.get("endpointSelector", spec.get("podSelector", {}))
    match_labels = selector.get("matchLabels", {})
    app_label = match_labels.get("app", "")

    if app_label == gateway_app or (app_label and app_label.startswith("agentgateway-")):
        if "egress" in spec:
            name = d.get("metadata", {}).get("name", "unknown")
            bad_policies.append(name)

if bad_policies:
    print(f"FAIL {', '.join(bad_policies)} has an egress section selecting {gateway_app}; "
          f"gateway pods must not have egress restricted (ADR-108)")
else:
    print(f"PASS no rendered network policy restricts egress for {gateway_app}")
PY
    )

    local line
    while IFS= read -r line; do
        [[ -z "$line" ]] && continue
        case "$line" in
            PASS\ *) pass "${line#PASS }" ;;
            FAIL\ *) hard_fail "${line#FAIL }" ;;
            *)       note "$line" ;;
        esac
    done <<< "$out"
}
