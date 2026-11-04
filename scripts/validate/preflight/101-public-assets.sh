#!/usr/bin/env bash
# ADR-101: declared public static assets on the tenant gateway.
#
# An application may serve a declared list of static files without a login
# (gateway.publicAssets.paths): a web app manifest, its icons, a service worker,
# which browsers fetch outside the user's session. Two charts render it from the
# one per-app values file -- universal-tenant binds an OIDC-free :3003 on the
# gateway, tenant-public-tls routes exactly those paths to it -- and every bound
# on it is structural. This validator pins each one, from a synthetic fixture:
#
#   A1  declared: the gateway has a :3003 bind named public-assets, whose one
#       route reaches only frontend-workload, by EXACT path, GET/HEAD only
#   A2  the OIDC policy still targets the `app` listener alone, so :3003 is
#       outside it and :3000 is not
#   A3  the public HTTPRoute sends exactly the same (path, method) set to :3003,
#       and the gateway Service exposes 3003
#   A4  undeclared: neither chart renders 3003 anywhere
#   A5  both charts refuse the same inputs with the same message: the app shell,
#       /api /internal /v1 /oauth /health /healthz, anything that is not a plain
#       exact path, "." and "..", duplicates, more than 32, non-strings
validate_public_assets() {
    section "ADR-101 declared public static assets (exact paths, GET/HEAD, no OIDC)"

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
PATHS = ["/manifest.json", "/sw.js", "/.well-known/assetlinks.json"]

def render(chart, paths, dirs=None, routes=None):
    v = json.loads(json.dumps(FIXTURE))
    v["gateway"]["publicAssets"] = {"paths": paths}
    if dirs is not None:
        v["gateway"]["publicAssets"]["directories"] = dirs
    if routes is not None:
        v["gateway"]["publicRoutes"] = routes
    with tempfile.NamedTemporaryFile("w", suffix=".yaml") as f:
        yaml.safe_dump(v, f); f.flush()
        r = subprocess.run(["helm", "template", "t", chart, "-f", f.name],
                           capture_output=True, text=True)
    err = next((l.split("): ", 1)[-1] for l in r.stderr.splitlines() if l.startswith("Error:")), None)
    return r.stdout, err

def docs(text):
    return [d for d in yaml.safe_load_all(text) if d]

fails, passes = [], []
def check(ok, good, bad):
    (passes if ok else fails).append(good if ok else bad)

# ── declared ────────────────────────────────────────────────────────────────
ut, err = render(UT, PATHS)
tls, err2 = render(TLS, PATHS)
if err or err2:
    fails.append(f"fixture failed to render: {err or err2}")
else:
    cfg = next(yaml.safe_load(d["data"]["config.yaml"]) for d in docs(ut)
               if d["kind"] == "ConfigMap" and "config.yaml" in d.get("data", {}))
    binds = {b["port"]: b for b in cfg["binds"]}
    b = binds.get(3003)
    gw_matches = set()
    if not b:
        fails.append("A1 no :3003 bind rendered for a declared publicAssets list")
    else:
        ls = b["listeners"]
        routes = [r for l in ls for r in l.get("routes", [])]
        ok = (len(ls) == 1 and ls[0].get("name") == "public-assets" and len(routes) == 1
              and [x["host"].split(".")[0] for x in routes[0]["backends"]] == ["frontend-workload"])
        for m in routes[0].get("matches", []) if routes else []:
            p = m.get("path", {})
            if set(p) != {"exact"} or m.get("method") not in ("GET", "HEAD") or set(m) - {"path", "method"}:
                ok = False
            gw_matches.add((p.get("exact"), m.get("method")))
        ok = ok and gw_matches == {(p, m) for p in PATHS for m in ("GET", "HEAD")}
        check(ok, "A1 :3003 bind 'public-assets' reaches only frontend-workload, exact paths, GET/HEAD",
              f"A1 :3003 bind is not exactly one exact-path GET/HEAD route to frontend-workload: {b}")
    targets = [p["target"]["gateway"].get("listenerName") for p in cfg.get("policies", []) if "oidc" in p.get("policy", {})]
    check(targets == ["app"], "A2 the OIDC policy targets the 'app' listener only",
          f"A2 OIDC policy targets {targets}; it must target exactly the 'app' listener")

    route = next(d for d in docs(tls) if d["kind"] == "HTTPRoute")
    rt_matches = set()
    for rule in route["spec"]["rules"]:
        if any(br.get("port") == 3003 for br in rule["backendRefs"]):
            for m in rule.get("matches", []):
                if m["path"]["type"] != "Exact":
                    fails.append(f"A3 route match to :3003 is {m['path']['type']}, not Exact")
                rt_matches.add((m["path"]["value"], m.get("method")))
    check(rt_matches == gw_matches and rt_matches,
          "A3 the public route sends exactly the gateway's (path, method) set to :3003",
          f"A3 route {sorted(rt_matches)} != gateway {sorted(gw_matches)}")
    svc_ports = [p["port"] for d in docs(ut) if d["kind"] == "Service" and d["metadata"]["name"].startswith("agentgateway-")
                 for p in d["spec"]["ports"]]
    check(3003 in svc_ports, "A3 the gateway Service exposes 3003", f"A3 gateway Service ports {svc_ports} lack 3003")

# ── undeclared ──────────────────────────────────────────────────────────────
ut0, e0 = render(UT, [])
tls0, e1 = render(TLS, [])
check(not e0 and not e1 and "3003" not in ut0 and "3003" not in tls0,
      "A4 undeclared: neither chart renders 3003",
      f"A4 an empty publicAssets list still renders 3003 (or failed: {e0 or e1})")

# ── refused, identically ────────────────────────────────────────────────────
BAD = ["/manifest.json", ["/"], ["/index.html"], ["/INDEX.HTML"], ["/api/x"], ["/API"],
       ["/internal/user/me"], ["/v1/webhooks/x"], ["/oauth/callback"], ["/health"], ["/healthz"],
       ["/assets/*"], ["/assets/"], ["manifest.json"], ["/m.json?x=1"], ["/a/../api"], ["/a/./b"],
       ["/m.json", "/m.json"], [42], ["/f%d.png" % i for i in range(33)]]
bad_ok = True
for case in BAD:
    _, a = render(UT, case)
    _, b = render(TLS, case)
    if not a or a != b:
        bad_ok = False
        fails.append(f"A5 {json.dumps(case)[:60]}: universal-tenant={a!r} tenant-public-tls={b!r}")
check(bad_ok, f"A5 both charts refuse all {len(BAD)} unsafe inputs with the same message", "A5 refusal mismatch (above)")

# ── directories (amendment 2026-10-12) ──────────────────────────────────────
ut6, e6 = render(UT, [], ["/assets/"])
tls6, f6 = render(TLS, [], ["/assets/"])
if e6 or f6:
    fails.append(f"A6 a directory fixture failed to render: {e6 or f6}")
else:
    cfg6 = next(yaml.safe_load(d["data"]["config.yaml"]) for d in docs(ut6)
                if d["kind"] == "ConfigMap" and "config.yaml" in d.get("data", {}))
    b6 = {x["port"]: x for x in cfg6["binds"]}.get(3003)
    r6 = [r for l in (b6 or {}).get("listeners", []) for r in l.get("routes", [])]
    gw6 = {(m["path"].get("pathPrefix"), m.get("method")) for r in r6 for m in r.get("matches", [])}
    ok6 = len(r6) == 1 and [x["host"].split(".")[0] for x in r6[0]["backends"]] == ["frontend-workload"] \
        and gw6 == {("/assets/", "GET"), ("/assets/", "HEAD")}
    route6 = next(d for d in docs(tls6) if d["kind"] == "HTTPRoute")
    rt6 = {(m["path"]["type"], m["path"]["value"], m.get("method")) for rule in route6["spec"]["rules"]
           if any(br.get("port") == 3003 for br in rule["backendRefs"]) for m in rule.get("matches", [])}
    ok6 = ok6 and rt6 == {("PathPrefix", "/assets/", "GET"), ("PathPrefix", "/assets/", "HEAD")}
    check(ok6, "A6 a declared directory is a GET/HEAD path prefix to frontend-workload on the bind and the route",
          f"A6 directory rendering wrong: bind {sorted(gw6)} route {sorted(rt6)}")
BAD_DIRS = [("not a list", [], "/assets/", None), ("no trailing slash", [], ["/assets"], None),
            ("nested", [], ["/a/b/"], None), ("wildcard", [], ["/assets/*"], None), ("upper case", [], ["/Assets/"], None),
            ("reserved api", [], ["/api/"], None), ("dot-dot", [], ["/../"], None), ("duplicate", [], ["/assets/", "/assets/"], None),
            ("too many", [], [f"/d{i}/" for i in range(5)], None),
            ("holds a declared path", ["/assets/logo.png"], ["/assets/"], None),
            ("is a public route prefix", [], ["/share/"], [{"prefix": "/share"}]),
            ("paths and directories over 32", ["/f%d.png" % i for i in range(30)], ["/a/", "/b/", "/c/"], None)]
dirs_ok = True
for name, paths, dirs, routes in BAD_DIRS:
    _, a6 = render(UT, paths, dirs, routes)
    _, b6_ = render(TLS, paths, dirs, routes)
    if not a6 or a6 != b6_:
        dirs_ok = False
        fails.append(f"A6 {name}: universal-tenant={a6!r} tenant-public-tls={b6_!r}")
check(dirs_ok, f"A6 both charts refuse all {len(BAD_DIRS)} unsafe directory inputs with the same message",
      "A6 directory refusal mismatch (above)")

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
