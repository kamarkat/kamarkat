#!/usr/bin/env bash
# ADR-113: the billing engine's isolation, as rendered. The same shape as
# 109-credit-authority-isolation.sh, for the second platform capability with
# one caller.
#
# The billing engine is a selectable capability run on the hub
# (hub-core-services/billing-engine), reached only through the hub Gateway at
# billing.<hubDomain>.
#
#   B1  the pod: the queue is a native sidecar bound to 127.0.0.1; the migration
#       is a one-shot init container after it; the API binds 127.0.0.1; the
#       gateway is the only container with a port, and the pod's and Service's
#       only port is 8080
#   B2  the API route: strict JWT on the box's issuer and the capability's
#       project; every rule is `require` or `allow`, never `deny`
#   B3  the caller binding: no ID token, exactly the one client the platform
#       publishes from the declaration, and that client's organisation; the
#       engine's key is presented by the gateway, never by the caller
#   B4  the document route admits only GET of the engine's signed storage paths,
#       anchored at both ends
#   B5  the config holds no dollar sign but its environment references
#   B6  the chart refuses to render without the issuer, the hub domain, the
#       provider or a pinned image
#   B7  exposure: ingress to the engine is the hub Gateway's identity only, on
#       8080; the renderer admits only the engine; the route and its listener
#       exist only while the capability is enabled, admit only the capability's
#       namespace, and reach only the gateway; the caller's Secret is revocable
#   B8  the prerequisites are on every box: the engine's own cluster (ADR-112)
#       running the pg_partman image, the lago database on it (owned by
#       hub_billing, retained), the owner made superuser once at initialisation
#       from the generated credential, the namespace, and billing-db admitting
#       only the engine
#   B9  enablement: the service, the gateway's flag and the fleets' URL follow
#       capabilities.billing.enabled, and a fleet declaring the capability on a
#       box without it does not render
validate_billing_engine_isolation() {
    section "ADR-113 billing engine isolation (rendered)"

    if ! command -v helm >/dev/null 2>&1; then
        soft_fail "helm not installed; cannot render the billing engine chart"
        return 0
    fi

    local out
    out=$(cd "$VALIDATE_ROOT" && python3 - <<'PY'
import re, subprocess, tempfile, yaml

BE = "manifests/hub-core-services/billing-engine"
GW = "manifests/hub-core-services/gateway"
EM = "manifests/argocd/environment-manager"
UT = "manifests/tenants/charts/universal-tenant"
DB = f"{BE}/database"
FIXTURE = {"oidcIssuer": "https://id.example.test", "oidcJwksUrl": "https://id.example.test/keys",
           "global": {"hubDomain": "example.test", "provider": "hybrid"}}
EM_ARGS = ["--set", "environmentSlug=dev", "--set", "provider=hybrid", "--set", "boxId=example-box",
           "--set", "instanceRepoURL=https://github.com/example-org/example-gitops",
           "--set", "hubDomain=dev.example.test", "--set", "previewDomain=example-preview.test", "--set", "alerting.emailTo[0]=alerts@example.test",
           "--set", "topology=single", "--set", "publicTlsIssuer=letsencrypt-prod",
           "--set", "oidcIssuer=https://auth.example.invalid"]

fails, passes = [], []
def check(ok, good, bad): (passes if ok else fails).append(good if ok else bad)
def docs(text): return [d for d in yaml.safe_load_all(text) if d]

def render(chart, values):
    with tempfile.NamedTemporaryFile("w", suffix=".yaml") as f:
        yaml.safe_dump(values, f); f.flush()
        r = subprocess.run(["helm", "template", "t", chart, "-f", f.name], capture_output=True, text=True)
    err = next((l for l in r.stderr.splitlines() if l.startswith("Error:")), None)
    return r.stdout, err

out, err = render(BE, FIXTURE)
if err:
    fails.append(f"B0 the chart did not render: {err}")
else:
    ds = docs(out)
    dep = next(d for d in ds if d["kind"] == "Deployment" and d["metadata"]["name"] == "billing-engine")
    pod = dep["spec"]["template"]["spec"]
    inits, containers = pod.get("initContainers", []), pod["containers"]
    names = [c["name"] for c in inits]
    queue = inits[0] if inits else {}
    qargs = queue.get("args", [])
    qbind = qargs[qargs.index("--bind") + 1] if "--bind" in qargs else ""
    api = next((c for c in containers if c["name"] == "api"), {})
    api_cmd = " ".join(api.get("command", []))
    ported = [c["name"] for c in inits + containers if c.get("ports")]
    ports = [p["containerPort"] for c in inits + containers for p in c.get("ports", [])]
    svc_ports = [p["port"] for d in ds if d["kind"] == "Service" and d["metadata"]["name"] == "billing-engine" for p in d["spec"]["ports"]]
    check(names == ["queue", "migrate"] and queue.get("restartPolicy") == "Always" and qbind == "127.0.0.1"
          and "restartPolicy" not in inits[1] and "-b 127.0.0.1 -p 3000" in api_cmd
          and ported == ["gateway"] and ports == [8080] and svc_ports == [8080] and not pod.get("hostNetwork"),
          "B1 queue is a loopback native sidecar, migration is one-shot, the API binds loopback, 8080 is the only port",
          f"B1 pod shape wrong: init={names} queue bind={qbind!r} api={api_cmd!r} ported={ported} ports={ports} service={svc_ports}")

    cm = next(d for d in ds if d["kind"] == "ConfigMap" and "config.yaml" in d.get("data", {}))
    raw = cm["data"]["config.yaml"]
    routes = {r["name"]: r for r in yaml.safe_load(raw)["binds"][0]["listeners"][0]["routes"]}
    pol = routes.get("api", {}).get("policies", {})
    jwt = pol.get("jwtAuth", {})
    rules = pol.get("authorization", {}).get("rules", [])
    modes = [next(iter(r)) for r in rules]
    check(jwt.get("mode") == "strict" and jwt.get("issuer") == "${OIDC_ISSUER_URL}"
          and jwt.get("audiences") == ["${OIDC_PROJECT_ID}"] and jwt.get("jwks", {}).get("url") == "${OIDC_JWKS_URL}"
          and rules and set(modes) == {"require", "allow"},
          "B2 the API route requires a strict JWT for the capability's project, and uses only require and allow",
          f"B2 API route policy wrong: jwtAuth={jwt} modes={modes}")

    requires = [r["require"] for r in rules if "require" in r]
    check("!has(jwt.at_hash)" in requires
          and 'jwt.client_id == "${BILLING_CALLER_CLIENT_ID}"' in requires
          and 'jwt["urn:zitadel:iam:user:resourceowner:id"] == "${BILLING_CALLER_ORG_ID}"' in requires
          and pol.get("backendAuth") == {"key": "${LAGO_ORG_API_KEY}"},
          "B3 the caller is the one declared client, of its own organisation, with an access token; the gateway presents the engine's key",
          f"B3 caller binding incomplete: requires={requires} backendAuth={pol.get('backendAuth')}")

    doc_rules = routes.get("documents", {}).get("policies", {}).get("authorization", {}).get("rules", [])
    check(len(doc_rules) == 1 and doc_rules[0].get("allow", "").startswith('request.method == "GET" && request.path.matches(r"^/rails/active_storage/')
          and doc_rules[0]["allow"].endswith('\\z")') and set(routes) == {"health", "ready", "documents", "api"},
          "B4 documents are GET of the signed storage paths only, and no other route reaches the engine",
          f"B4 routes wrong: {sorted(routes)} documents={doc_rules}")

    check("$" not in re.sub(r"\$\{[A-Z_]+\}", "", raw), "B5 the config's only dollar signs are its environment references",
          "B5 a bare dollar sign in the gateway config would be shell-expanded before it is parsed")

    def without(path, key):
        v = yaml.safe_load(yaml.safe_dump(FIXTURE)); node = v
        for p in path: node = node.setdefault(p, {})
        node[key] = ""
        return bool(render(BE, v)[1])
    refused = {
        "no issuer": without([], "oidcIssuer"),
        "no hub domain": without(["global"], "hubDomain"),
        "no provider": without(["global"], "provider"),
        "no engine digest": without(["images", "engine"], "digest"),
        "no gateway digest": without(["images", "gateway"], "digest"),
        "no queue digest": without(["images", "queue"], "digest"),
    }
    check(all(refused.values()), "B6 the chart refuses a missing issuer, hub domain, provider and unpinned images",
          f"B6 rendered what it must refuse: {[k for k, v in refused.items() if not v]}")

    cnps = {d["metadata"]["name"]: d for d in ds if d["kind"] == "CiliumNetworkPolicy"}
    ingress = cnps.get("billing-engine-ingress", {}).get("spec", {}).get("ingress", [])
    sources = [(tuple(r.get("fromEntities", [])), tuple(map(str, r.get("fromEndpoints", [])))) for r in ingress]
    cnp_ports = {p["port"] for r in ingress for tp in r.get("toPorts", []) for p in tp.get("ports", [])}
    pdf_from = [e.get("matchLabels") for r in cnps.get("billing-pdf-ingress", {}).get("spec", {}).get("ingress", []) for e in r.get("fromEndpoints", [])]
    caller = next((d for d in ds if d["kind"] == "ExternalSecret" and d["metadata"]["name"] == "billing-engine-caller"), {})
    gw_off, e1 = render(GW, {"global": {"hubDomain": "example.test"}})
    gw_on, e2 = render(GW, {"global": {"hubDomain": "example.test"}, "billingEngine": {"enabled": True}})
    if e1 or e2:
        fails.append(f"B7 the hub gateway did not render: {e1 or e2}")
    else:
        on = docs(gw_on)
        route = next((d for d in on if d["kind"] == "HTTPRoute" and d["metadata"]["name"] == "billing-engine"), None)
        gwy = next(d for d in on if d["kind"] == "Gateway")
        lst = next((l for l in gwy["spec"]["listeners"] if l["name"] == "billing"), None)
        admitted = lst and lst["allowedRoutes"]["namespaces"]["selector"]["matchExpressions"][0]["values"]
        backends = [(b["name"], b["port"]) for r in (route or {}).get("spec", {}).get("rules", []) for b in r["backendRefs"]]
        check(sources == [(("ingress",), ())] and cnp_ports == {"8080"}
              and pdf_from == [{"app.kubernetes.io/name": "billing-engine"}]
              and "billing." not in gw_off and route and route["metadata"]["namespace"] == "platform-billing"
              and admitted == ["platform-billing"] and backends == [("billing-engine", 8080)]
              and caller.get("spec", {}).get("target", {}).get("deletionPolicy") == "Delete",
              "B7 only the hub Gateway reaches the engine, on 8080; only the engine reaches the renderer; the route exists only while enabled; the caller is revocable",
              f"B7 exposure wrong: ingress={sources} ports={cnp_ports} renderer from={pdf_from} listener admits={admitted} backends={backends}")

# The prerequisites, on every box, in the engine's OWN component (ADR-112).
objs = docs(open(f"{DB}/billing-db.yaml").read())
cluster = next((d for d in objs if d["kind"] == "Cluster"), {})
database = next((d for d in objs if d["kind"] == "Database"), {})
initdb = cluster.get("spec", {}).get("bootstrap", {}).get("initdb", {})
es = yaml.safe_load(open(f"{DB}/hub-billing-db-credentials-es.yaml"))
kust = yaml.safe_load(open(f"{DB}/kustomization.yaml"))
netpol = next((d for d in objs if d["kind"] == "NetworkPolicy"), {})
app_from = netpol.get("spec", {}).get("ingress", [{}])[0].get("from", [])
mappings = open("operators/hub-operator/internal/infisical/secret_mappings.go").read()
namespaces = [d["metadata"]["name"] for d in docs(open("manifests/hub-core-services/platform-namespaces/namespaces.yaml").read())]
check(cluster.get("spec", {}).get("imageName", "").startswith("ghcr.io/kamarkat/kamarkat/postgres-partman")
      and initdb.get("owner") == "hub_billing" and initdb.get("secret", {}).get("name") == es["metadata"]["name"] == "hub-billing-db-credentials"
      and initdb.get("postInitSQL") == ["ALTER ROLE hub_billing SUPERUSER"]
      and database.get("spec", {}).get("owner") == "hub_billing" and database["spec"].get("databaseReclaimPolicy") == "retain"
      and es["spec"]["target"]["template"]["type"] == "kubernetes.io/basic-auth"
      and set(kust["resources"]) == {"billing-db.yaml", "hub-billing-db-credentials-es.yaml"}
      and '"hub-billing-db-password"' in mappings and "platform-billing" in namespaces
      and app_from == [{"namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": "platform-billing"}},
                        "podSelector": {"matchLabels": {"app.kubernetes.io/name": "billing-engine"}}}],
      "B8 the engine's own pg_partman cluster, its retained database, its superuser owner from the generated credential, the namespace and engine-only access are on every box",
      f"B8 prerequisite wrong: image={cluster.get('spec', {}).get('imageName')} initdb={initdb} database={database.get('spec')} db access={app_from}")

# Enablement.
def em(template, enabled):
    r = subprocess.run(["helm", "template", "x", EM, "--show-only", template, *EM_ARGS,
                        "--set", f"capabilities.billing.enabled={str(enabled).lower()}"], capture_output=True, text=True)
    return r.stdout, next((l for l in r.stderr.splitlines() if l.startswith("Error:")), None)
def elements(text):
    return {el["appName"]: el for d in docs(text) for g in d["spec"]["generators"] for el in g.get("list", {}).get("elements", [])}
s_off, e3 = em("templates/03-platform-services-appset.yaml", False)
s_on, e4 = em("templates/03-platform-services-appset.yaml", True)
f_off, e5 = em("templates/05-tenant-fleet-appset.yaml", False)
f_on, e6 = em("templates/05-tenant-fleet-appset.yaml", True)
if e3 or e4 or e5 or e6:
    fails.append(f"B9 environment-manager did not render: {e3 or e4 or e5 or e6}")
else:
    off, on = elements(s_off), elements(s_on)
    gw_flag = lambda els: yaml.safe_load(els["hub-gateway"]["helmValues"]).get("billingEngine", {}).get("enabled")
    declared = {"tenantId": "acme", "appId": "waypoint", "cellId": "cell-1", "oidcIssuer": "https://id.example.test",
                "capabilities": {"billing": {"enabled": True}}}
    _, refused = render(UT, declared)
    admitted, e7 = render(UT, {**declared, "billingEngineUrl": "https://billing.example.test"})
    check("billing-engine" not in off and on.get("billing-engine", {}).get("destinationNamespace") == "platform-billing"
          and gw_flag(off) is False and gw_flag(on) is True
          and "billingEngineUrl" not in f_off and "billingEngineUrl" in f_on
          and refused and not e7 and "PROVIDER_BILLING_BASE_URL" in admitted,
          "B9 the service, its route and the fleets' URL follow capabilities.billing.enabled; a declaration without it does not render",
          f"B9 enablement wrong: service off={'billing-engine' in off} on={'billing-engine' in on} "
          f"gateway off={gw_flag(off)} on={gw_flag(on)} declaration refused without it={bool(refused)}")

for p in passes: print("PASS " + p)
for f in fails: print("FAIL " + f)
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
