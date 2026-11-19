#!/usr/bin/env bash
# scope: spoke
# ADR-087 — Workload Configuration and Secrets: every LIVE-CLUSTER check.
#
# One file per ADR; the number is the ADR's. Static counterparts are in
# preflight/087-workload-configuration-and-secrets.sh.


# ──────────────────────────────────────────────────────────────────────────
# Every declared secret is actually present
# ──────────────────────────────────────────────────────────────────────────
# ADR-087 requires a declared secret to be verified BEFORE a workload consumes
# it. This is that check, and its value is entirely in WHEN it reports and WHAT
# it says.
#
# The failure it replaces: a key absent from the provider fails the whole
# ExternalSecret, so no Secret is created, so every workload reading it stays in
# CreateContainerConfigError, its Service has no endpoints, and the gateway in
# front answers 503. Nothing in that chain names a secret. It happened on
# 2026-09-02 and again on 2026-09-22, both times surfacing as
# `/api/v1/auth/me` returning `503 ... backends required DNS resolution which
# failed` -- an ingress error for a missing credential.
#
# Read from ESO's own status rather than by querying Infisical directly. ESO has
# already performed the lookup with the credential and scoping the workload will
# actually use, so its verdict is the delivery path itself; a separate query
# would be a second path that can agree while the real one fails.
#
# The report names the CAPABILITY, because that is what an operator can act on.
# "AI_GATEWAY_API_KEY is missing" asks the reader to already know what that is.
validate_adr087_declared_secrets_are_present() {
    section "fleet secrets present (ADR-087)"

    local es_json
    es_json="$(kc_spoke get externalsecrets -A -o json 2>/dev/null)"
    if [[ -z "$es_json" ]]; then
        hard_fail "could not list ExternalSecrets on the spoke — cannot tell whether any declared secret was delivered"
        return
    fi

    # Fleet declarations, so an absent value can be named by its capability.
    local decls
    decls="$(find . \
        \( -name node_modules -o -name reference-projects -o -name inspirations -o -name archived \) -prune \
        -o -path '*/environments/*/values.yaml' -print 2>/dev/null | tr '\n' ' ')"

    local script findings rc
    script="$(mktemp)"
    cat > "$script" <<'PYEOF'
import json, sys, yaml

es_doc = json.load(open(sys.argv[1]))

# key -> capability, from every fleet declaration in the repository.
capability = {}
for path in sys.argv[2:]:
    try:
        d = yaml.safe_load(open(path)) or {}
    except Exception:
        continue
    if not isinstance(d, dict):
        continue
    for s in (d.get("secrets") or []):
        if isinstance(s, dict) and s.get("name"):
            capability[s["name"]] = s.get("capability", "")

problems = []
checked = 0
for es in es_doc.get("items", []):
    meta = es["metadata"]
    ns = meta["namespace"]
    if not ns.startswith("tenant-"):
        continue
    checked += 1
    conds = {c["type"]: c for c in (es.get("status") or {}).get("conditions", [])}
    ready = conds.get("Ready")
    if ready and ready.get("status") == "True":
        continue

    keys = [d.get("secretKey") for d in (es.get("spec") or {}).get("data", [])]
    reason = (ready or {}).get("message", "no Ready condition")
    lost = []
    for k in keys:
        cap = capability.get(k)
        lost.append(f"{k} ({cap})" if cap else k)
    problems.append({
        "name": f"{ns}/{meta['name']}",
        "reason": reason,
        "keys": lost,
    })

print(json.dumps({"checked": checked, "problems": problems}))
PYEOF

    local tmp_es
    tmp_es="$(mktemp)"
    printf '%s' "$es_json" > "$tmp_es"
    findings="$(python3 "$script" "$tmp_es" $decls)"
    rc=$?
    rm -f "$script" "$tmp_es"

    # A checker that died is a failure, not a pass. Three preflight modules once
    # reported green because their python exited before printing anything.
    if [[ $rc -ne 0 || -z "$findings" ]]; then
        hard_fail "the secret-presence checker exited $rc without completing — ${findings:-no output}"
        return
    fi

    local checked count
    checked="$(python3 -c 'import json,sys; print(json.loads(sys.argv[1])["checked"])' "$findings")"
    count="$(python3 -c 'import json,sys; print(len(json.loads(sys.argv[1])["problems"]))' "$findings")"

    if (( checked == 0 )); then
        pass "no tenant ExternalSecrets on this spoke (no fleet declares secrets yet)"
        return
    fi
    if (( count == 0 )); then
        pass "all $checked tenant ExternalSecret(s) resolved — every declared secret is present"
        return
    fi

    local report
    report="$(python3 - "$findings" <<'PYEOF'
import json, sys
d = json.loads(sys.argv[1])
for p in d["problems"]:
    print(f"\n  {p['name']}: {p['reason']}")
    for k in p["keys"]:
        print(f"      withholds {k}")
PYEOF
)"
    hard_fail "$count of $checked tenant ExternalSecret(s) did not resolve. An ExternalSecret is
atomic, so EVERY key below is withheld from its workload and every container reading one
stays in CreateContainerConfigError — which surfaces as a 503 from whatever fronts it,
naming nothing:${report}"
}
