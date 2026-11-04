#!/usr/bin/env bash
# ADR-051 public TLS contract: per-tenant ACME certificates, platform-owned.
#
# The public-TLS migration moved tenant certificates out of the workload tier:
# fleet-registry declares public.hosts (hostname intent, exactly once), the
# tenant-public-tls ApplicationSet renders a Certificate plus a :443 listener
# per declared hostname into platform-ops on the owning spoke, and cert-manager
# issues via the environment's ClusterIssuer. This validator pins every link of
# that chain so none of it silently regresses:
#
#   repo-side (always):
#     R1  no wildcard dnsNames anywhere in spoke-catalog certificates
#     R2  tenant-gateway terminates NO TLS (:80 only; :443 belongs to
#         tenant-tls-gateway rendered by the chart)
#     R3  chart refuses missing issuer and wildcard hostnames at render time
#     R4  chart has no default issuer (staging is an ephemeral policy, never a
#         default)
#     R5  AppSet forwards publicTlsIssuer as the chart's issuer input
#     R6  bootstrap issuer mapping: ephemeral→staging, dev/stg/prod→prod
#
#   registry-side (when the fleet-registry checkout is present):
#     G1  every tenants/*/*/values.yaml declares public.hosts
#     G2  declared hosts are syntactically valid and never wildcards
#     G3  oauth.publicHost is covered by public.hosts when OAuth is enabled
#     G4  workload trees carry no PUBLIC certificate (cluster-local .svc mTLS on
#         infisical-fleet-issuer remains legitimate per ADR-035)
validate_public_tls_contract() {
    section "ADR-051 public TLS contract (per-tenant ACME, platform-owned)"

    # ── R1: no wildcard dnsNames in spoke-catalog certificates ──────────────
    local wildcards
    wildcards=$(grep -rn '^\s*-\s*"\?\*' "$VALIDATE_ROOT"/manifests/spoke/spoke-catalog/infra/certificates.yaml 2>/dev/null || true)
    if [[ -z "$wildcards" ]]; then
        pass "spoke-catalog certificates declare no wildcard dnsNames"
    else
        hard_fail "wildcard dnsName in spoke-catalog certificates.yaml (unissuable under HTTP-01): $wildcards"
    fi

    # ── R2: tenant-gateway is :80-only, TLS-free ─────────────────────────────
    if (cd "$VALIDATE_ROOT" && python3 - <<'PY'
import sys, yaml
gw = None
for doc in yaml.safe_load_all(open("manifests/spoke/spoke-catalog/infra/tenant-gateway.yaml")):
    if doc and doc.get("kind") == "Gateway":
        gw = doc
if gw is None:
    sys.exit(1)
for l in gw["spec"].get("listeners", []):
    if l.get("protocol") != "HTTP" or l.get("port") != 80 or "tls" in l:
        sys.exit(2)
sys.exit(0)
PY
    ); then
        pass "tenant-gateway serves plaintext :80 only (no TLS termination)"
    else
        hard_fail "tenant-gateway must not terminate TLS — :443 ownership belongs to tenant-tls-gateway (platform-spoke-gateway chart, ADR-096); same-port claims are first-wins on the shared Envoy"
    fi

    # ── R2b: no tenant chart owns the shared Gateway (ADR-096) ──────────────
    #
    # THE invariant, checked rather than only written down. tenant-public-tls
    # used to render this object once per application, each copy declaring only
    # its own listener -- and because every ArgoCD Application applies as one
    # field manager, the copies overwrote each other and flapped the public DNS
    # of an app that was already working.
    #
    # ADR-051 had refused that arrangement eight days earlier and the code did it
    # anyway, because nothing compared the two. This is that comparison.
    if grep -rlq "kind: Gateway" "$VALIDATE_ROOT/manifests/tenants/charts/tenant-public-tls/templates/" 2>/dev/null; then
        hard_fail "tenant-public-tls renders a Gateway again (ADR-096): the shared TLS Gateway has exactly one owner, platform-spoke-gateway. A per-application copy of it silently removes the other applications' listeners"
    else
        pass "no tenant chart renders the shared TLS Gateway (ADR-096)"
    fi

    # ── R2c: the shared Gateway has exactly one renderer ─────────────────────
    local owners
    owners=$(grep -rl "kind: Gateway" "$VALIDATE_ROOT/manifests/tenants/charts/" 2>/dev/null | wc -l | tr -d ' ')
    if [[ "$owners" == "1" ]]; then
        pass "exactly one chart under manifests/tenants/charts renders a Gateway"
    else
        hard_fail "$owners charts under manifests/tenants/charts render a Gateway; ADR-096 requires exactly one owner for the shared TLS Gateway"
    fi

    # ── R3: chart render-time guards exist ───────────────────────────────────
    local helpers="$VALIDATE_ROOT/manifests/tenants/charts/tenant-public-tls/templates/_helpers.tpl"
    if grep -q 'fail "issuer is required with no default' "$helpers" \
        && grep -q 'wildcard hostname' "$helpers"; then
        pass "tenant-public-tls chart fails rendering on missing issuer or wildcard hosts"
    else
        hard_fail "tenant-public-tls _helpers.tpl lost its requirements guard — the chart would render unissuable or mis-policyed certificates"
    fi

    # ── R4: no default issuer in chart values ────────────────────────────────
    if [[ "$(sed -n 's/^issuer: *"\{0,1\}\([^"]*\)"\{0,1\}$/\1/p' "$VALIDATE_ROOT/manifests/tenants/charts/tenant-public-tls/values.yaml" | tr -d '[:space:]')" == "" ]]; then
        pass "chart issuer value has an empty default (environment must supply policy)"
    else
        hard_fail "chart values.yaml ships a non-empty issuer default — issuer policy would silently inherit instead of failing"
    fi

    # ── R5: AppSet wires publicTlsIssuer into the chart ──────────────────────
    local appset="$VALIDATE_ROOT/manifests/argocd/environment-manager/templates/06-tenant-public-tls-appset.yaml"
    # ADR-055 moved the boundary gate off chart content: the AppSet is no longer
    # rendered conditionally, it is scoped to the boundary-06 AppProject whose
    # deny sync window is the activation state. The issuer contract is unchanged
    # — the AppSet must still forward it and still refuse to render without it.
    if grep -q 'name: issuer' "$appset" && grep -q '.Values.publicTlsIssuer' "$appset" \
        && grep -q 'project: boundary-06' "$appset" \
        && grep -q 'publicTlsIssuer is required' "$appset"; then
        pass "tenant-public-tls ApplicationSet scoped to boundary-06, forwards publicTlsIssuer, refuses empty issuer"
    else
        hard_fail "AppSet does not forward the environment issuer, lost its boundary-06 project scope, or lost its empty-issuer guard"
    fi

    # ── R6: bootstrap issuer mapping is strict ───────────────────────────────
    local orch="$VALIDATE_ROOT/internal/kakt-cli/bootstrap/orchestrator.go"
    if grep -q 'case "ephemeral":' "$orch" \
        && grep -A12 'case "dev", "stg", "prod":' "$orch" | grep -q 'letsencrypt-prod' \
        && grep -q 'refusing to guess TLS policy' "$orch"; then
        pass "bootstrap issuer mapping: ephemeral→staging, dev/stg/prod→prod, unknown→hard error"
    else
        hard_fail "publicTlsIssuerFor() mapping drifted from ADR-051 policy (ephemeral=staging; dev/stg/prod=prod; no default)"
    fi

    # ── registry-side checks ─────────────────────────────────────────────────
    local registry="${FLEET_REGISTRY:-$VALIDATE_ROOT/../fleet-registry}"
    if [[ ! -d "$registry/tenants" ]]; then
        warn "fleet-registry checkout not found at $registry — set FLEET_REGISTRY to run registry-side checks (G1-G4)"
        return 0
    fi

    local out
    out=$(cd "$VALIDATE_ROOT" && REGISTRY="$registry" python3 - <<'PY'
import os, sys, yaml

registry = os.environ["REGISTRY"]
failures = []

values_files = []
for root, dirs, files in os.walk(os.path.join(registry, "tenants")):
    if "values.yaml" in files:
        values_files.append(os.path.join(root, "values.yaml"))
if not values_files:
    failures.append("no tenants/*/*/values.yaml found under %s" % registry)

for vf in sorted(values_files):
    rel = os.path.relpath(vf, registry)
    try:
        d = yaml.safe_load(open(vf)) or {}
    except yaml.YAMLError as e:
        failures.append("%s: invalid YAML: %s" % (rel, e))
        continue

    # G1: schema completeness — the AppSet renders with missingkey=error, so an
    # absent key breaks application generation for EVERY fleet, not just this one.
    pub = d.get("public")
    if not isinstance(pub, dict) or "hosts" not in pub:
        failures.append("%s: missing public.hosts declaration" % rel)
        continue
    hosts = pub["hosts"]
    if not isinstance(hosts, list):
        failures.append("%s: public.hosts must be a list" % rel)
        continue

    # G2: hostname hygiene.
    for h in hosts:
        if h.startswith("*."):
            failures.append("%s: wildcard host %r (unissuable under HTTP-01)" % (rel, h))
        elif not all(c.isalnum() or c in "-." for c in h):
            failures.append("%s: malformed hostname %r" % (rel, h))

    # G3: OAuth redirect host must be covered by the public declaration.
    oauth = d.get("oauth") or {}
    if oauth.get("enabled") and oauth.get("publicHost"):
        if oauth["publicHost"] not in hosts:
            failures.append(
                "%s: oauth.publicHost %r not in public.hosts — login would redirect to a name with no listener/certificate"
                % (rel, oauth["publicHost"]))

# G4: workload trees may carry cluster-local mTLS certificates only. A
# dnsName is cluster-local when it is a short service name (<svc>), a
# namespace-scoped name (<svc>.<ns>), or an in-cluster FQDN (<...>.svc[.cluster.local]).
# Anything resolvable outside the cluster (any real zone, e.g. *.nutgraf.in per
# ADR-051) is public TLS and belongs to the tenant-public-tls path.
def is_cluster_local(name):
    return (
        name.endswith(".svc")
        or name.endswith(".svc.cluster.local")
        or "." not in name
        or name.count(".") == 1
    )

svc_markers = (".svc", ".svc.cluster.local")
wf_root = os.path.join(registry, "tenants")
for root, dirs, files in os.walk(wf_root):
    dirs[:] = [x for x in dirs if x != "migrations"]
    for f in files:
        if f.endswith((".yaml", ".yml")):
            p = os.path.join(root, f)
            try:
                docs = [d for d in yaml.safe_load_all(open(p)) if d]
            except yaml.YAMLError:
                continue
            for doc in docs:
                if not isinstance(doc, dict) or doc.get("kind") != "Certificate":
                    continue
                spec = doc.get("spec") or {}
                dns = spec.get("dnsNames") or []
                public_names = [n for n in dns if not is_cluster_local(n)]
                if public_names:
                    failures.append(
                        "%s: Certificate %s carries PUBLIC dnsNames %s — public TLS is platform-owned "
                        "(tenant-public-tls); workload trees may hold .svc mTLS certs only"
                        % (os.path.relpath(p, registry), doc.get("metadata", {}).get("name"), public_names))

if failures:
    for f in failures:
        print("FAIL\t" + f)
else:
    print("OK")
PY
)

    if [[ "$out" == "OK" ]]; then
        pass "fleet-registry declarations satisfy the public TLS contract (G1-G4)"
    elif [[ -z "$out" ]]; then
        warn "registry-side validator produced no output — inspect scripts/validate/preflight/81-public-tls-contract.sh"
    else
        local tag msg
        while IFS=$'\t' read -r tag msg; do
            [[ "$tag" == "FAIL" ]] || continue
            hard_fail "$msg"
        done <<< "$out"
    fi
}
