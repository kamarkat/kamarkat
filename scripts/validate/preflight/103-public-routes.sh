#!/usr/bin/env bash
# ADR-103: declared login-free dynamic routes (gateway.publicRoutes).
#
# An application may serve one prefix plus one fixed-shape token segment without
# a login -- a shared session at /share/<token> -- answered by its BFF. Two
# charts render it from one values file: universal-tenant binds an OIDC-free
# :3004 on the gateway, tenant-public-tls routes the prefix to it. Pinned here,
# from a synthetic fixture:
#
#   R1  declared: :3004 bind 'public-routes', one route per prefix, to
#       bff-workload only, regex ^<prefix>/[A-Za-z0-9]{N}$, GET/HEAD only, with
#       the rate limit, cookie/authorization stripping and noindex/no-referrer/
#       no-store headers; the regex holds no dollar-brace (shell-expanded config)
#   R2  the OIDC policy still targets the 'app' listener only
#   R3  the public route sends that prefix, GET/HEAD, to :3004 with the header
#       filters, and the gateway Service exposes 3004
#   R4  undeclared: neither chart renders 3004
#   R5  both charts refuse the same inputs with the same message
validate_public_routes() {
    section "ADR-103 declared public routes (one prefix + one token segment, GET/HEAD, BFF, no OIDC)"

    if ! command -v helm >/dev/null 2>&1; then
        soft_fail "helm not installed; cannot render the gateway charts"
        return 0
    fi

    local out
    out=$(cd "$VALIDATE_ROOT" && python3 - <<'PY'
import json, subprocess, tempfile, yaml

UT = "manifests/tenants/charts/universal-tenant"
TLS = "manifests/tenants/charts/tenant-public-tls"
FIXTURE = {
    "tenantId": "acme", "appId": "shop", "clusterName": "spoke-1",
    "oidcIssuer": "https://id.example.test",
    "issuer": "letsencrypt-staging", "externalDnsTarget": "192.0.2.10",
    "public": {"hosts": ["shop.example.test"]},
    "gateway": {"enabled": True, "hostnames": ["shop.example.test"]},
}
ROUTES = [{"prefix": "/share"}, {"prefix": "/demo", "tokenLength": 32}]
WANT = {"/share": r"^/share/[A-Za-z0-9]{22}$", "/demo": r"^/demo/[A-Za-z0-9]{32}$"}

def render(chart, routes, extra=None):
    v = json.loads(json.dumps(FIXTURE))
    v["gateway"]["publicRoutes"] = routes
    if extra:
        v["gateway"].update(extra)
    with tempfile.NamedTemporaryFile("w", suffix=".yaml") as f:
        yaml.safe_dump(v, f); f.flush()
        r = subprocess.run(["helm", "template", "t", chart, "-f", f.name], capture_output=True, text=True)
    err = next((l.split("): ", 1)[-1] for l in r.stderr.splitlines() if l.startswith("Error:")), None)
    return r.stdout, err

def docs(text):
    return [d for d in yaml.safe_load_all(text) if d]

fails, passes = [], []
def check(ok, good, bad): (passes if ok else fails).append(good if ok else bad)

ut, e1 = render(UT, ROUTES)
tls, e2 = render(TLS, ROUTES)
if e1 or e2:
    fails.append(f"fixture failed to render: {e1 or e2}")
else:
    cfg = next(yaml.safe_load(d["data"]["config.yaml"]) for d in docs(ut)
               if d["kind"] == "ConfigMap" and "config.yaml" in d.get("data", {}))
    b = {x["port"]: x for x in cfg["binds"]}.get(3004)
    ok, got = bool(b), {}
    if b:
        ls = b["listeners"]
        ok = len(ls) == 1 and ls[0].get("name") == "public-routes"
        for r in ls[0].get("routes", []) if ok else []:
            regexes = {m["path"].get("regex") for m in r["matches"]}
            methods = sorted(m.get("method") for m in r["matches"])
            pol = r.get("policies", {})
            tr = pol.get("transformations", {})
            ok = ok and len(regexes) == 1 and methods == ["GET", "HEAD"] \
                and all(set(m["path"]) == {"regex"} for m in r["matches"]) \
                and [x["host"].split(".")[0] for x in r["backends"]] == ["bff-workload"] \
                and pol.get("localRateLimit") \
                and {"cookie", "authorization"} <= set(tr.get("request", {}).get("remove", [])) \
                and "set-cookie" in tr.get("response", {}).get("remove", []) \
                and {"x-robots-tag", "referrer-policy", "cache-control"} <= set(tr.get("response", {}).get("set", {}))
            rx = next(iter(regexes)) if regexes else ""
            ok = ok and ("$" + "{") not in rx  # spelled apart: bash 3.2 scans $(...) heredocs
            got[rx.split("/")[1] if rx else "?"] = rx
        ok = ok and got == {k.strip("/"): v for k, v in WANT.items()}
    check(ok, "R1 :3004 'public-routes': exact token regex, GET/HEAD, bff-workload, rate limit, cookies stripped, safe headers",
          f"R1 :3004 bind wrong or missing: {json.dumps(b)[:400]}")
    targets = [p["target"]["gateway"].get("listenerName") for p in cfg.get("policies", []) if "oidc" in p.get("policy", {})]
    check(targets == ["app"], "R2 the OIDC policy targets the 'app' listener only", f"R2 OIDC targets {targets}")

    route = next(d for d in docs(tls) if d["kind"] == "HTTPRoute")
    seen = {}
    for rule in route["spec"]["rules"]:
        if any(br.get("port") == 3004 for br in rule["backendRefs"]):
            prefixes = {m["path"]["value"] for m in rule["matches"] if m["path"]["type"] == "PathPrefix"}
            methods = sorted(m.get("method") for m in rule["matches"])
            ftypes = {f["type"] for f in rule.get("filters", [])}
            rq = next((f["requestHeaderModifier"] for f in rule["filters"] if f["type"] == "RequestHeaderModifier"), {})
            rs = next((f["responseHeaderModifier"] for f in rule["filters"] if f["type"] == "ResponseHeaderModifier"), {})
            good = len(prefixes) == 1 and methods == ["GET", "HEAD"] \
                and {"Cookie", "Authorization"} <= set(rq.get("remove", [])) and "Set-Cookie" in rs.get("remove", []) \
                and {"X-Robots-Tag", "Referrer-Policy"} <= {h["name"] for h in rs.get("set", [])}
            seen[next(iter(prefixes)) if prefixes else "?"] = good
    check(seen == {"/share/": True, "/demo/": True},
          "R3 the public route sends each prefix, GET/HEAD, to :3004 with cookie and header filters",
          f"R3 public route rules to :3004: {seen}")
    ports = [p["port"] for d in docs(ut) if d["kind"] == "Service" and d["metadata"]["name"].startswith("agentgateway-")
             for p in d["spec"]["ports"]]
    check(3004 in ports, "R3 the gateway Service exposes 3004", f"R3 gateway Service ports {ports} lack 3004")

ut0, a = render(UT, [])
tls0, b0 = render(TLS, [])
check(not a and not b0 and "3004" not in ut0 and "3004" not in tls0,
      "R4 undeclared: neither chart renders 3004", f"R4 an empty publicRoutes still renders 3004 (or failed: {a or b0})")

BAD = [
    ("not a list", {"prefix": "/share"}, None),
    ("two segments", [{"prefix": "/share/x"}], None),
    ("upper case", [{"prefix": "/Share"}], None),
    ("no slash", [{"prefix": "share"}], None),
    ("reserved api", [{"prefix": "/api"}], None),
    ("reserved oauth", [{"prefix": "/oauth"}], None),
    ("reserved ui", [{"prefix": "/ui"}], None),
    ("short token", [{"prefix": "/share", "tokenLength": 8}], None),
    ("long token", [{"prefix": "/share", "tokenLength": 65}], None),
    ("fractional token", [{"prefix": "/share", "tokenLength": 22.5}], None),
    ("string token", [{"prefix": "/share", "tokenLength": "22"}], None),
    ("duplicate", [{"prefix": "/share"}, {"prefix": "/share"}], None),
    ("too many", [{"prefix": f"/p{i}"} for i in range(5)], None),
    ("collides with publicAssets", [{"prefix": "/share"}], {"publicAssets": {"paths": ["/share/logo.png"]}}),
]
bad_ok = True
for name, routes, extra in BAD:
    _, x = render(UT, routes, extra)
    _, y = render(TLS, routes, extra)
    if not x or x != y:
        bad_ok = False
        fails.append(f"R5 {name}: universal-tenant={x!r} tenant-public-tls={y!r}")
check(bad_ok, f"R5 both charts refuse all {len(BAD)} unsafe inputs with the same message", "R5 refusal mismatch (above)")

# ── answeredBy (amendment 2026-10-12) ───────────────────────────────────────
ut6, e6 = render(UT, [{"prefix": "/share", "answeredBy": "frontend"}, {"prefix": "/shares"}])
backends = {}
if not e6:
    cfg6 = next(yaml.safe_load(d["data"]["config.yaml"]) for d in docs(ut6)
                if d["kind"] == "ConfigMap" and "config.yaml" in d.get("data", {}))
    b6 = {x["port"]: x for x in cfg6["binds"]}.get(3004) or {"listeners": [{}]}
    for r in b6["listeners"][0].get("routes", []):
        rx = r["matches"][0]["path"].get("regex", "")
        backends[rx.split("/")[1]] = [x["host"].split(".")[0] for x in r["backends"]]
check(backends == {"share": ["frontend-workload"], "shares": ["bff-workload"]},
      "R6 answeredBy: frontend reaches frontend-workload, the default reaches bff-workload",
      f"R6 answeredBy backends wrong (or failed: {e6}): {backends}")
_, x6 = render(UT, [{"prefix": "/share", "answeredBy": "sdk"}])
_, y6 = render(TLS, [{"prefix": "/share", "answeredBy": "sdk"}])
check(bool(x6) and x6 == y6, "R6 both charts refuse an answeredBy other than bff or frontend",
      f"R6 answeredBy refusal mismatch: universal-tenant={x6!r} tenant-public-tls={y6!r}")

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
