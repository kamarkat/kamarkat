#!/usr/bin/env bash
# ADR-052: an EphemeralJob's lifecycle callback must be able to reach the SDK.
#
# The operator (platform-ops) POSTs to spec.callbackUrl, the SDK that created the
# job, when the job is Running and when it finishes. A CiliumNetworkPolicy with an
# `ingress` section flips the SDK to default-deny, and a denied packet is dropped
# without an RST, so a callback that is not admitted does not fail: it times out,
# is retried forever, and the client is never told the sandbox is ready. That is
# what happened on 2026-10-03 to every Running callback to waypoint's SDK.
#
#   C1  every policy the universal-tenant chart renders that restricts the SDK's
#       ingress admits the operator, on 3000
#   C2  the operator it admits is the one the platform deploys: the namespace and
#       pod labels in that rule match operators/ephemeral-job-operator/config,
#       so renaming either side fails here instead of in a sandbox
validate_callback_reaches_sdk() {
    section "ADR-052 EphemeralJob callbacks reach the SDK through its ingress policy"

    if ! command -v helm >/dev/null 2>&1 || ! command -v kubectl >/dev/null 2>&1; then
        soft_fail "helm and kubectl are needed to render the chart and the operator config"
        return 0
    fi

    local out
    out=$(cd "$VALIDATE_ROOT" && python3 - <<'PY'
import json, subprocess, tempfile, yaml

# A fixture that renders every SDK ingress policy the chart has: allowedConsumers
# is what makes the cross-app ingress policy exist.
FIXTURE = {
    "tenantId": "acme", "appId": "shop", "clusterName": "spoke-1",
    "oidcIssuer": "https://id.example.test",
    "identity": {"allowedConsumers": ["store"], "backendDependencies": ["ledger"]},
    "sandbox": {"agentVault": {"enabled": True}},
    "burstCompute": {"enabled": True},
}

def docs(text):
    return [d for d in yaml.safe_load_all(text) if d]

with tempfile.NamedTemporaryFile("w", suffix=".yaml") as f:
    yaml.safe_dump(FIXTURE, f); f.flush()
    r = subprocess.run(["helm", "template", "t", "manifests/tenants/charts/universal-tenant", "-f", f.name],
                       capture_output=True, text=True)
if r.returncode:
    print("FAIL C1 universal-tenant fixture failed to render: " + r.stderr.strip()[:300]); raise SystemExit

op = subprocess.run(["kubectl", "kustomize", "operators/ephemeral-job-operator/config/default"],
                    capture_output=True, text=True)
dep = next((d for d in docs(op.stdout) if d["kind"] == "Deployment"), None)
if not dep:
    print("FAIL C2 could not render the operator Deployment: " + op.stderr.strip()[:300]); raise SystemExit
op_ns = dep["metadata"]["namespace"]
op_labels = dep["spec"]["template"]["metadata"]["labels"]

def admits_operator(rule):
    ports = {p["port"] for t in rule.get("toPorts", []) for p in t.get("ports", [])}
    for ep in rule.get("fromEndpoints", []):
        m = ep.get("matchLabels", {})
        ns = m.get("k8s:io.kubernetes.pod.namespace")
        labels = {k: v for k, v in m.items() if not k.startswith("k8s:")}
        if ns == op_ns and labels and all(op_labels.get(k) == v for k, v in labels.items()) and "3000" in ports:
            return True
    return False

policies = [d for d in docs(r.stdout) if d["kind"] == "CiliumNetworkPolicy"
            and d["spec"].get("endpointSelector", {}).get("matchLabels", {}).get("app") == "sdk"
            and "ingress" in d["spec"]]
if not policies:
    print("FAIL C1 the fixture rendered no SDK ingress policy; the check is no longer exercising anything")
for p in policies:
    name = p["metadata"]["name"]
    if any(admits_operator(rule) for rule in p["spec"]["ingress"]):
        print(f"PASS C1+C2 {name} admits {op_ns}/{op_labels} on 3000")
    else:
        print(f"FAIL C1/C2 {name} restricts the SDK's ingress and does not admit the EphemeralJob operator "
              f"({op_ns}, labels {op_labels}) on 3000: lifecycle callbacks will time out")
PY
    )
    local line
    while IFS= read -r line; do
        case "$line" in
            PASS\ *) pass "${line#PASS }" ;;
            FAIL\ *) hard_fail "${line#FAIL }" ;;
        esac
    done <<< "$out"
}
