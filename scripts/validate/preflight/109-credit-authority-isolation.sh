#!/usr/bin/env bash
# ADR-109: the Credit Authority's isolation, as rendered. The live half is
# cluster/109-credit-authority-isolation.sh; both gate the capability.
#
# The Credit Authority is a selectable capability run on the hub
# (hub-core-services/credit-authority), reached only through the hub Gateway at
# credits.<hubDomain>.
#
#   I1  the pod: the ledger is a native sidecar bound to 127.0.0.1; the
#       provisioner is a one-shot init container after it; the gateway is the
#       only container with a port, and the pod's and Service's only port is 3000
#   I2  the gateway's ledger route: strict JWT on the box's issuer and the
#       capability's project; every rule is `require` or `allow`, never `deny`
#   I3  the caller binding: no ID token, exactly the one client the platform
#       publishes from the declaration, and that client's organisation
#   I4  no opt-out of the balance check: creating a transaction requires no query
#       string and a body whose fields are exactly postings, metadata, reference
#   I5  every `allow` is anchored at both ends on the box's ledger, with the path
#       characters ADR-109 §4 states, and nothing else is allowed
#   I6  the config holds no dollar sign but its environment references (it is
#       shell-expanded before it is parsed)
#   I7  the chart refuses to render without the issuer, a pinned image, or with a
#       ledger name its rules cannot be built from
#   I8  exposure: ingress to the pod is the hub Gateway's identity only, on 3000;
#       the route and its listener exist only while the capability is enabled,
#       admit only the capability's namespace, and reach only the gateway; the
#       caller's Secret is revocable
#   I9  the prerequisites are on every box: the ledger's own cluster (ADR-112), the
#       credits database on it (owned by hub_credits, retained), the role bound to
#       that cluster, its generated credential delivered with them, the namespace,
#       and credits-db admitting it
#   I10 enablement: the service, the gateway's flag and the fleets' URL follow
#       capabilities.credits.enabled, and a fleet declaring the capability on a
#       box without it does not render
validate_credit_authority_isolation() {
    section "ADR-109 Credit Authority isolation (rendered)"

    if ! command -v helm >/dev/null 2>&1; then
        soft_fail "helm not installed; cannot render the Credit Authority chart"
        return 0
    fi

    local out
    out=$(cd "$VALIDATE_ROOT" && python3 - <<'PY'
import re, subprocess, tempfile, yaml

CA = "manifests/hub-core-services/credit-authority"
GW = "manifests/hub-core-services/gateway"
EM = "manifests/argocd/environment-manager"
UT = "manifests/tenants/charts/universal-tenant"
DB = f"{CA}/database"
FIXTURE = {"oidcIssuer": "https://id.example.test", "oidcJwksUrl": "https://id.example.test/keys",
           "global": {"hubDomain": "example.test"}}
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

out, err = render(CA, FIXTURE)
if err:
    fails.append(f"I0 the chart did not render: {err}")
else:
    ds = docs(out)
    dep = next(d for d in ds if d["kind"] == "Deployment")
    pod = dep["spec"]["template"]["spec"]
    inits = pod.get("initContainers", [])
    names = [c["name"] for c in inits]
    ledger = inits[0] if inits else {}
    args = ledger.get("args", [])
    bind = args[args.index("--bind") + 1] if "--bind" in args else ""
    ported = [c["name"] for c in inits + pod["containers"] if c.get("ports")]
    ports = [p["containerPort"] for c in inits + pod["containers"] for p in c.get("ports", [])]
    svc_ports = [p["port"] for d in ds if d["kind"] == "Service" for p in d["spec"]["ports"]]
    check(names == ["ledger", "provision"] and ledger.get("restartPolicy") == "Always"
          and bind == "127.0.0.1:3068" and "restartPolicy" not in inits[1]
          and len(pod["containers"]) == 1 and ported == [pod["containers"][0]["name"]] and ports == [3000]
          and svc_ports == [3000] and not pod.get("hostNetwork"),
          "I1 ledger is a loopback native sidecar, provisioning is one-shot before the gateway, 3000 is the only port",
          f"I1 pod shape wrong: init={names} bind={bind!r} ported={ported} ports={ports} service={svc_ports}")

    cm = next(d for d in ds if d["kind"] == "ConfigMap" and "config.yaml" in d.get("data", {}))
    raw = cm["data"]["config.yaml"]
    cfg = yaml.safe_load(raw)
    routes = {r["name"]: r for r in cfg["binds"][0]["listeners"][0]["routes"]}
    pol = routes.get("ledger", {}).get("policies", {})
    jwt = pol.get("jwtAuth", {})
    rules = pol.get("authorization", {}).get("rules", [])
    modes = [next(iter(r)) if isinstance(r, dict) else "allow" for r in rules]
    check(jwt.get("mode") == "strict" and jwt.get("issuer") == "${OIDC_ISSUER_URL}"
          and jwt.get("audiences") == ["${OIDC_PROJECT_ID}"] and jwt.get("jwks", {}).get("url") == "${OIDC_JWKS_URL}"
          and rules and "deny" not in modes and set(modes) == {"require", "allow"},
          "I2 the ledger route requires a strict JWT for the capability's project, and uses only require and allow",
          f"I2 ledger route policy wrong: jwtAuth={jwt} modes={modes}")

    requires = [r["require"] for r in rules if isinstance(r, dict) and "require" in r]
    check("!has(jwt.at_hash)" in requires
          and 'jwt.client_id == "${CREDITS_CALLER_CLIENT_ID}"' in requires
          and 'jwt["urn:zitadel:iam:user:resourceowner:id"] == "${CREDITS_CALLER_ORG_ID}"' in requires,
          "I3 the caller is the one declared client, of its own organisation, with an access token",
          f"I3 caller binding incomplete: {requires}")

    body = [r for r in requires if "json(request.body)" in r]
    check(len(body) == 1 and "request.pathAndQuery == request.path" in body[0]
          and 'k in ["postings", "metadata", "reference"]' in body[0] and body[0].startswith('request.method != "POST" ||'),
          "I4 creating a transaction admits no query string and only postings, metadata and reference",
          f"I4 transaction body rule missing or wrong: {body}")

    allows = [r["allow"] if isinstance(r, dict) else r for r in rules if not (isinstance(r, dict) and "require" in r)]
    pat = re.compile(r'^request\.method == "(GET|POST)" && request\.path\.matches\(r"\^/v2/credits/([^"]*)\\z"\)$')
    ok = len(allows) == 4 and all(pat.match(a) for a in allows)
    tails = sorted(pat.match(a).group(2) for a in allows) if ok else allows
    check(ok and tails == sorted(["transactions", "transactions(/[0-9]+)?", "accounts(/[A-Za-z0-9_:-]+)?",
                                  "(aggregate/balances|volumes)"]),
          "I5 only the four operations are allowed, each anchored on the box's ledger",
          f"I5 allow rules wrong: {tails}")

    check("$" not in re.sub(r"\$\{[A-Z_]+\}", "", raw), "I6 the config's only dollar signs are its environment references",
          "I6 a bare dollar sign in the gateway config would be shell-expanded before it is parsed")

    refused = {
        "no issuer": bool(render(CA, {**FIXTURE, "oidcIssuer": ""})[1]),
        "no ledger digest": bool(render(CA, {**FIXTURE, "images": {"ledger": {"digest": ""}}})[1]),
        "no gateway digest": bool(render(CA, {**FIXTURE, "images": {"gateway": {"digest": ""}}})[1]),
        "a ledger name with a dot": bool(render(CA, {**FIXTURE, "ledger": {"name": "a.b", "bucket": "a"}})[1]),
    }
    check(all(refused.values()), "I7 the chart refuses a missing issuer, unpinned images and an unsafe ledger name",
          f"I7 rendered what it must refuse: {[k for k, v in refused.items() if not v]}")

    cnp = next((d for d in ds if d["kind"] == "CiliumNetworkPolicy"), {})
    ingress = cnp.get("spec", {}).get("ingress", [])
    sources = [(tuple(r.get("fromEntities", [])), tuple(r.get("fromEndpoints", []))) for r in ingress]
    cnp_ports = {p["port"] for r in ingress for tp in r.get("toPorts", []) for p in tp.get("ports", [])}
    caller = next((d for d in ds if d["kind"] == "ExternalSecret" and d["metadata"]["name"] == "credit-authority-caller"), {})
    gw_off, e1 = render(GW, {"global": {"hubDomain": "example.test"}})
    gw_on, e2 = render(GW, {"global": {"hubDomain": "example.test"}, "creditAuthority": {"enabled": True}})
    if e1 or e2:
        fails.append(f"I8 the hub gateway did not render: {e1 or e2}")
    else:
        on = docs(gw_on)
        route = next((d for d in on if d["kind"] == "HTTPRoute" and d["metadata"]["name"] == "credit-authority"), None)
        gwy = next(d for d in on if d["kind"] == "Gateway")
        lst = next((l for l in gwy["spec"]["listeners"] if l["name"] == "credits"), None)
        admitted = lst and lst["allowedRoutes"]["namespaces"]["selector"]["matchExpressions"][0]["values"]
        backends = [(b["name"], b["port"]) for r in (route or {}).get("spec", {}).get("rules", []) for b in r["backendRefs"]]
        check(sources == [(("ingress",), ())] and cnp_ports == {"3000"}
              and "credits." not in gw_off and route and route["metadata"]["namespace"] == "platform-credits"
              and admitted == ["platform-credits"] and backends == [("credit-authority", 3000)]
              and caller.get("spec", {}).get("target", {}).get("deletionPolicy") == "Delete",
              "I8 only the hub Gateway reaches the pod, on 3000; the route exists only while enabled and reaches only the gateway; the caller is revocable",
              f"I8 exposure wrong: ingress={sources} ports={cnp_ports} listener admits={admitted} backends={backends}")

# The prerequisites, on every box.
#
# All of them in the ledger's OWN component (ADR-112): the cluster, its Database, the
# role credential and the policy that admits the ledger are one directory, so a box that
# deploys the Credit Authority cannot deploy it without them.
objs = docs(open(f"{DB}/credits-db.yaml").read())
credits_db = next((d for d in objs if d["kind"] == "Database" and d["spec"].get("name") == "credits"), {})
cluster = next((d for d in objs if d["kind"] == "Cluster"), {})
es = yaml.safe_load(open(f"{DB}/hub-credits-db-credentials-es.yaml"))
kust = yaml.safe_load(open(f"{DB}/kustomization.yaml"))
pg_peers = [f["namespaceSelector"]["matchLabels"].get("kubernetes.io/metadata.name")
            for d in objs if d["kind"] == "NetworkPolicy" and d["metadata"]["name"] == "credits-db-access"
            for r in d["spec"]["ingress"] for f in r.get("from", []) if "namespaceSelector" in f]
hubenv = yaml.safe_load(open("manifests/environments/base/hubenvironment.yaml"))
roles = {r["name"]: r for r in hubenv["spec"]["database"]["roles"]}
mappings = open("operators/hub-operator/internal/infisical/secret_mappings.go").read()
namespaces = [d["metadata"]["name"] for d in docs(open("manifests/hub-core-services/platform-namespaces/namespaces.yaml").read())]
check(credits_db.get("spec", {}).get("owner") == "hub_credits" and credits_db["spec"].get("databaseReclaimPolicy") == "retain"
      and credits_db["spec"]["cluster"]["name"] == cluster["metadata"]["name"] == "credits-db"
      and es["metadata"]["namespace"] == "platform-data" and es["metadata"]["name"] == "hub-credits-db-credentials"
      and "hub-credits-db-credentials-es.yaml" in kust["resources"]
      and roles.get("hub_credits", {}).get("database") == "credits"
      and roles.get("hub_credits", {}).get("clusterRef") == "credits-db"
      and '"hub-credits-db-password"' in mappings and "platform-credits" in namespaces and "platform-credits" in pg_peers,
      "I9 the ledger's own cluster, its database (owned, retained), the role bound to that cluster, the credential, the namespace and database access are on every box",
      f"I9 prerequisite missing: cluster={cluster.get('metadata', {}).get('name')} database={credits_db.get('spec')} "
      f"role={roles.get('hub_credits')} namespace={'platform-credits' in namespaces} db access={pg_peers}")

# Enablement.
def em(template, enabled):
    r = subprocess.run(["helm", "template", "x", EM, "--show-only", template, *EM_ARGS,
                        "--set", f"capabilities.credits.enabled={str(enabled).lower()}"], capture_output=True, text=True)
    return r.stdout, next((l for l in r.stderr.splitlines() if l.startswith("Error:")), None)
def elements(text):
    return {el["appName"]: el for d in docs(text) for g in d["spec"]["generators"] for el in g.get("list", {}).get("elements", [])}
s_off, e3 = em("templates/03-platform-services-appset.yaml", False)
s_on, e4 = em("templates/03-platform-services-appset.yaml", True)
f_off, e5 = em("templates/05-tenant-fleet-appset.yaml", False)
f_on, e6 = em("templates/05-tenant-fleet-appset.yaml", True)
if e3 or e4 or e5 or e6:
    fails.append(f"I10 environment-manager did not render: {e3 or e4 or e5 or e6}")
else:
    off, on = elements(s_off), elements(s_on)
    gw_flag = lambda els: yaml.safe_load(els["hub-gateway"]["helmValues"]).get("creditAuthority", {}).get("enabled")
    declared = {"tenantId": "acme", "appId": "waypoint", "cellId": "cell-1", "oidcIssuer": "https://id.example.test",
                "capabilities": {"credits": {"enabled": True}}}
    _, refused = render(UT, declared)
    admitted, e7 = render(UT, {**declared, "creditAuthorityUrl": "https://credits.example.test"})
    check("credit-authority" not in off and on.get("credit-authority", {}).get("destinationNamespace") == "platform-credits"
          and gw_flag(off) is False and gw_flag(on) is True
          and "creditAuthorityUrl" not in f_off and "creditAuthorityUrl" in f_on
          and refused and not e7 and "CREDITS_URL" in admitted,
          "I10 the service, its route and the fleets' URL follow capabilities.credits.enabled; a declaration without it does not render",
          f"I10 enablement wrong: service off={'credit-authority' in off} on={'credit-authority' in on} "
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
