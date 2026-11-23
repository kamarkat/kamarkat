#!/usr/bin/env bash
# ADR-102: the platform's standard workload library chart (platform-workload).
#
# Apps' BFF and domain-service charts depend on it, so a change here reaches
# every app on the next release -- which is the point, and why its contract is
# pinned rather than reviewed. A library chart cannot be rendered alone, so each
# check renders a throwaway consumer chart against the working tree (file://):
#
#   W1  names and selector: <component>-workload, <component>-workload-sa, and
#       the immutable Rollout selector {app: <component>, workload-class}
#   W2  liveness and readiness are DIFFERENT paths by default (/health,
#       /health/ready), so readiness can carry a dependency and liveness never does
#   W3  the hardening admission requires is present: digest image, non-root,
#       read-only root, all capabilities dropped, ndots:2, no SA token by default
#   W4  presync-hook migration: egress policy and SQL hooks at wave -6, the Job at
#       -5, failed Jobs kept; hashed-job: the Job name changes with its SQL
#   W5  allowLifecycleCallbacks admits the EphemeralJob operator (ADR-052)
#   W6  refused: missing component / tenantId / image, an unknown migration
#       strategy, a migration with no command
validate_platform_workload() {
    section "ADR-102 platform-workload library chart contract"

    if ! command -v helm >/dev/null 2>&1; then
        soft_fail "helm not installed; cannot render the platform-workload library"
        return 0
    fi

    local out
    out=$(cd "$VALIDATE_ROOT" && python3 - <<'PY'
import copy, json, os, shutil, subprocess, tempfile, yaml

LIB = os.path.abspath("manifests/tenants/charts/platform-workload")
DIGEST = "sha256:" + "0" * 64
BASE = {
    "tenantId": "acme", "hubDomain": "box.example.test", "appId": "shop", "costCenter": "platform",
    "workload": {"component": "bff", "image": {"repository": "ghcr.io/example/shop-bff"},
                 "ports": {"http": 3001}},
}

def render(values, sql=None):
    d = tempfile.mkdtemp()
    try:
        os.makedirs(f"{d}/c/templates")
        with open(f"{d}/c/Chart.yaml", "w") as f:
            yaml.safe_dump({"apiVersion": "v2", "name": "shop-bff", "type": "application", "version": "0.1.0",
                            "appVersion": DIGEST,
                            "dependencies": [{"name": "platform-workload", "version": "0.1.0", "repository": f"file://{LIB}"}]}, f)
        open(f"{d}/c/templates/workload.yaml", "w").write('{{ include "platform-workload.all" . }}\n')
        if sql is not None:
            os.makedirs(f"{d}/c/files/migrations")
            open(f"{d}/c/files/migrations/0001.sql", "w").write(sql)
        with open(f"{d}/c/values.yaml", "w") as f:
            yaml.safe_dump(values, f)
        subprocess.run(["helm", "dependency", "build", f"{d}/c"], capture_output=True, check=True)
        r = subprocess.run(["helm", "template", "t", f"{d}/c"], capture_output=True, text=True)
        err = next((l.split("): ", 1)[-1] for l in r.stderr.splitlines() if l.startswith("Error:")), None)
        return [x for x in yaml.safe_load_all(r.stdout) if x] if not err else [], err
    finally:
        shutil.rmtree(d, ignore_errors=True)

def with_(**w):
    v = copy.deepcopy(BASE); v["workload"].update(w); return v

def get(objs, kind, name=None):
    return next((o for o in objs if o["kind"] == kind and (name is None or o["metadata"]["name"] == name)), None)

passes, fails = [], []
def check(ok, good, bad): (passes if ok else fails).append(good if ok else bad)

objs, err = render(BASE)
if err:
    fails.append(f"W1 base fixture failed to render: {err}")
else:
    ro, svc, sa = get(objs, "Rollout", "bff-workload"), get(objs, "Service", "bff-workload"), get(objs, "ServiceAccount", "bff-workload-sa")
    sel = ro and ro["spec"]["selector"]["matchLabels"]
    check(ro and svc and sa and sel == {"app": "bff", "workload-class": "stateless-web"}
          and svc["spec"]["selector"] == sel,
          "W1 bff-workload / bff-workload-sa, selector {app: bff, workload-class: stateless-web}",
          f"W1 names or selector changed: rollout={bool(ro)} service={bool(svc)} sa={bool(sa)} selector={sel}")
    c = ro["spec"]["template"]["spec"]["containers"][0] if ro else {}
    lp, rp = c.get("livenessProbe", {}).get("httpGet", {}).get("path"), c.get("readinessProbe", {}).get("httpGet", {}).get("path")
    check((lp, rp) == ("/health", "/health/ready"), "W2 liveness /health, readiness /health/ready by default",
          f"W2 default probe paths are liveness={lp} readiness={rp}")
    ps = ro["spec"]["template"]["spec"] if ro else {}
    sc = c.get("securityContext", {})
    ok = (c.get("image", "").endswith("@" + DIGEST) and ps.get("securityContext", {}).get("runAsNonRoot") is True
          and sc.get("readOnlyRootFilesystem") is True and sc.get("capabilities", {}).get("drop") == ["ALL"]
          and sc.get("allowPrivilegeEscalation") is False
          and {"name": "ndots", "value": "2"} in ps.get("dnsConfig", {}).get("options", [])
          and ps.get("automountServiceAccountToken") is False
          and all(ro["spec"]["template"]["metadata"]["labels"].get(k) for k in ("tenant-id", "app-id", "cost-center")))
    check(ok, "W3 digest image, non-root, read-only root, caps dropped, ndots:2, no SA token, ABI labels",
          f"W3 admission hardening missing from the Rollout: {json.dumps(ps)[:300]}")
    check(get(objs, "CiliumNetworkPolicy") is None, "W5 no network policy when none is declared",
          "W5 a network policy rendered with no rule declared (an empty ingress section default-denies)")

# ── migrations ───────────────────────────────────────────────────────────────
hook, err = render(with_(migration={"enabled": True, "command": ["migrate"], "sqlFiles": "files/migrations/*.sql",
                                   "extraVolumes": [{"name": "pg-ca", "secret": {"secretName": "shared-cnpg-ca"}}],
                                   "extraVolumeMounts": [{"name": "pg-ca", "mountPath": "/etc/ssl/postgres", "readOnly": True}]}),
                   sql="select 1;")
if err:
    fails.append(f"W4 presync fixture failed: {err}")
else:
    wave = lambda o: o and o["metadata"].get("annotations", {}).get("argocd.argoproj.io/sync-wave")
    job, pol = get(hook, "Job"), get(hook, "CiliumNetworkPolicy", "shop-bff-migration-egress")
    ann = (job or {}).get("metadata", {}).get("annotations", {})
    check(job and wave(pol) == "-6" and wave(job) == "-5" and wave(get(hook, "ConfigMap")) == "-6"
          and ann.get("argocd.argoproj.io/hook") == "PreSync"
          and "HookFailed" not in ann.get("argocd.argoproj.io/hook-delete-policy", "")
          and job["spec"]["template"]["spec"].get("automountServiceAccountToken") is False
          and {"name": "pg-ca", "secret": {"secretName": "shared-cnpg-ca"}} in job["spec"]["template"]["spec"]["volumes"]
          and any(m["name"] == "pg-ca" and m["mountPath"] == "/etc/ssl/postgres"
                  for m in job["spec"]["template"]["spec"]["containers"][0]["volumeMounts"]),
          "W4 presync-hook: policy and SQL at -6, Job at -5, failed Jobs kept, no SA token, extra volumes mounted",
          f"W4 presync ordering wrong: policy={wave(pol)} job={wave(job)} annotations={ann}")
names = []
for sql in ("select 1;", "select 2;"):
    o, err = render(with_(migration={"enabled": True, "strategy": "hashed-job", "command": ["migrate"],
                                     "sqlFiles": "files/migrations/*.sql"}), sql=sql)
    names.append((get(o, "Job") or {}).get("metadata", {}).get("name") if not err else err)
check(all(n and n.startswith("shop-bff-migration-") for n in names) and names[0] != names[1],
      "W4 hashed-job: the Job name follows its SQL", f"W4 hashed-job names did not change with the SQL: {names}")

# ── lifecycle callbacks ─────────────────────────────────────────────────────
o, err = render(with_(component="sdk", ports={"http": 3000}, networkPolicy={"allowLifecycleCallbacks": True}))
pol = get(o, "CiliumNetworkPolicy") if not err else None
rules = (pol or {}).get("spec", {}).get("ingress", [])
check(any(e.get("matchLabels") == {"app": "ephemeral-job-operator", "k8s:io.kubernetes.pod.namespace": "platform-ops"}
          for r in rules for e in r.get("fromEndpoints", [])),
      "W5 allowLifecycleCallbacks admits the EphemeralJob operator", f"W5 operator not admitted: {err or rules}")

# ── refusals ────────────────────────────────────────────────────────────────
bad = {
    "no component": with_(component=None),
    "no image": with_(image={"repository": ""}),
    "unknown strategy": with_(migration={"enabled": True, "strategy": "manual", "command": ["x"]}),
    "migration without command": with_(migration={"enabled": True}),
}
nt = copy.deepcopy(BASE); nt["tenantId"] = ""; bad["no tenantId"] = nt
missed = [k for k, v in bad.items() if render(v)[1] is None]
check(not missed, f"W6 all {len(bad)} invalid configurations refused", f"W6 rendered without error: {missed}")

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
