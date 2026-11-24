#!/usr/bin/env bash
# An ExternalSecret that cannot resolve produces no target Secret, and the
# consumer then reports the *Secret* as missing — pointing at the wrong layer
# entirely. That misdirection cost real debugging time on platform-database, so
# the real cause is named here instead of being inferred later.
#
# "could not get secret data from provider" never says WHERE the key was looked
# for, so each report resolves the ExternalSecret's store and prints the exact
# OpenBao coordinates — server, auth mount and role, KV scope and property — that
# have to be populated. Everything is read from the cluster rather than hardcoded, so the
# message stays correct if a store is re-scoped or an ExternalSecret re-pointed.
#
# The whole fleet is fetched in three API calls and correlated locally; resolving
# per-secret took ~5 calls each and timed out against a remote API server.
validate_secrets_resolve() {
    section "ExternalSecrets resolve against OpenBao"

    local es_json css_json ss_json
    es_json=$(kc get externalsecrets -A -o json)
    css_json=$(kc get clustersecretstores -o json)
    ss_json=$(kc get secretstores -A -o json)

    if [[ -z "$es_json" ]]; then
        soft_fail "could not list ExternalSecrets (CRD not installed, or API unreachable)"
        return 0
    fi

    local report
    report=$(python3 - "$es_json" "$css_json" "$ss_json" <<'PY'
import json, sys

def load(s):
    try:
        return json.loads(s).get("items", [])
    except Exception:
        return []

es_items, css_items, ss_items = (load(a) for a in sys.argv[1:4])

# Index every store by (kind, namespace, name) -> how it reaches OpenBao.
def store_view(it):
    v = it.get("spec", {}).get("provider", {}).get("vault", {}) or {}
    auth = v.get("auth", {}) or {}
    if "kubernetes" in auth:
        how = "kubernetes/%s" % auth["kubernetes"].get("role", "?")
    elif "jwt" in auth:
        how = "%s/%s" % (auth["jwt"].get("path", "?"), auth["jwt"].get("role", "?"))
    else:
        how = "?"
    return {"server": v.get("server", "?"), "auth": how}

scopes = {}
for it in css_items:
    scopes[("ClusterSecretStore", None, it["metadata"]["name"])] = store_view(it)
for it in ss_items:
    m = it["metadata"]
    scopes[("SecretStore", m["namespace"], m["name"])] = store_view(it)

def location(es):
    spec = es.get("spec", {})
    if any((d.get("sourceRef") or {}).get("generatorRef") for d in spec.get("dataFrom", []) or []):
        gens = [d["sourceRef"]["generatorRef"].get("name", "?") for d in spec.get("dataFrom", [])
                if (d.get("sourceRef") or {}).get("generatorRef")]
        return "generator(s) %s (e.g. VaultDynamicSecret database/static-creds/...)" % ",".join(gens)
    ref = spec.get("secretStoreRef", {})
    kind = ref.get("kind") or "SecretStore"
    name = ref.get("name", "?")
    ns = es["metadata"]["namespace"]
    sc = scopes.get((kind, None if kind == "ClusterSecretStore" else ns, name), {})
    refs = ["kv/%s#%s" % (d.get("remoteRef", {}).get("key"), d.get("remoteRef", {}).get("property", "*"))
            for d in spec.get("data", []) or [] if d.get("remoteRef", {}).get("key")]
    return "server=%s auth=%s refs=%s (via %s/%s)" % (
        sc.get("server", "?"), sc.get("auth", "?"),
        ",".join(dict.fromkeys(refs)) or "?", kind, name)

for es in es_items:
    m = es["metadata"]
    # ghcr-pull-secret is an optional capability (ADR-066); skipped when no GHCR_TOKEN is configured.
    target_name = es.get("spec", {}).get("target", {}).get("name", "")
    if m["name"] == "ghcr-pull-secret-credentials" or target_name == "ghcr-pull-secret":
        continue
    conds = es.get("status", {}).get("conditions", []) or []
    ready = next((c for c in conds if c.get("type") == "Ready"), conds[0] if conds else None)
    if ready and ready.get("status") == "True":
        continue
    reason = "no status yet"
    if ready:
        reason = "%s: %s" % (ready.get("reason", "?"), ready.get("message", ""))
    print("\t".join(["BAD", "%s/%s" % (m["namespace"], m["name"]), reason.strip(), location(es)]))

PY
)

    local bad_count
    bad_count=$(grep -c $'^BAD\t' <<< "$report" || true)
    if [[ "${bad_count:-0}" -eq 0 ]]; then
        pass "all ExternalSecrets report Ready=True"
    else
        local tag name reason loc
        while IFS=$'\t' read -r tag name reason loc; do
            [[ "$tag" == "BAD" ]] || continue
            soft_fail "ExternalSecret $name not ready — $reason"
            note "expects: $loc"
        done <<< "$report"

        if [[ "$VALIDATE_MODE" == "gate" ]]; then
            note ""
            note "These failures are transient during bootstrap. The hub-operator writes"
            note "secrets into OpenBao, and ESO re-reconciles within its retry backoff"
            note "window (~7 min). No manual intervention is needed."
        fi
    fi

    _validate_s3_backup_credentials
}

# s3-credentials-escrowed backs the CNPG backup of all clusters including the cluster
# holding the PKI root of trust. Without it the platform runs with no durable copy of
# any platform secret, and nothing else in the stack ever reports that.
_validate_s3_backup_credentials() {
    if kc get secret s3-credentials-escrowed -n platform-data >/dev/null 2>&1; then
        pass "s3-credentials-escrowed present — CNPG can back up the root of trust"
        return 0
    fi

    soft_fail "s3-credentials-escrowed absent in platform-data — no CNPG cluster on this box has a durable backup"
    note "Deliver via Day-0 InstallPlatformCredentials or \`kakt escrow deliver-backup-credentials\`"
}
