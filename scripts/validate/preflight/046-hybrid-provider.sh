#!/usr/bin/env bash
# ADR-046 — Hybrid Provider / Home Worker: every STATIC check, in one file.
#
# One file per ADR. Before this, ADR-046's checks were spread across four
# preflight modules numbered by running order -- 15, 25, 26, 85 -- so "what
# validates ADR-046?" was a grep rather than a listing, and a decision added to
# the ADR had no obvious home. The number is now the ADR's, which makes the
# mapping mechanical in both directions:
#
#   ls scripts/validate/*/046-*        every check for this ADR
#   head -1 <file>                     the ADR a check belongs to
#
# Merging is safe and does not change behaviour: run.sh SOURCES every module
# into one shell and then runs each `validate_*` function it finds, so four
# sourced files and one concatenated file are the same program. Ordering is
# presentational -- nothing exits early, failures are collected and reported at
# the end.
#
# Live counterparts are in cluster/046-hybrid-provider.sh. The split is the
# runner's phase boundary (static vs. a real cluster), not a second taxonomy.
#
# Sections below carry the ADR section they enforce.


# ──────────────────────────────────────────────────────────────────────────
# §23 — Hetzner project capacity
# ──────────────────────────────────────────────────────────────────────────
# Hetzner project capacity, checked before anything is created (ADR-046 §23).
#
# A hub consumes one load balancer for its control plane, plus one more for every
# `type: LoadBalancer` Service still in the platform. Each spoke consumes another.
# The project ceiling is small and shared by every cluster in the account, so a run
# can be refused by resources that belong to a cluster nobody is looking at.
#
# When the ceiling is hit CAPH reports LoadBalancerCreateFailed on the HetznerCluster
# and the CAPI Cluster stays at Provisioning forever. That is indistinguishable from
# a slow provision unless you read the condition reason, which is why on 2026-08-23
# it was polled to timeout and diagnosed only from a live cluster.
#
# The zero-target detector below is the part that earns its place. A CCM-created
# load balancer is named a<service-uid> and carries exactly one label,
# hcloud-ccm/service-uid — nothing that ties it to a cluster. Teardown could not
# match it, and the CCM that alone could delete it dies with the cluster. Four such
# accumulated silently over four rebuilds and reported nothing wrong anywhere until
# they blocked the *next* provision.
#
# A CCM load balancer with no targets is one of two defects, and preflight runs
# without a cluster so it cannot tell them apart — deliberately, because both need
# action and neither is visible from inside Kubernetes:
#
#   leaked  — its Service is gone with a destroyed cluster. Delete it; it consumes
#             quota and is billed forever.
#   broken  — its Service is live but the CCM registered no backends, so the load
#             balancer accepts TCP and forwards nothing. Deleting it achieves
#             nothing; the CCM recreates it. Fix the cause (addendum 2: kubeadm's
#             exclude-from-external-load-balancers label on the control plane, and
#             home workers whose unmanaged:// providerID the CCM cannot target).
#
# Once §23 items 1–2 land, no platform Service creates a CCM load balancer at all
# and only the leaked case can occur.
validate_hetzner_capacity() {
    section "Hetzner project capacity (ADR-046 §23)"

    local token
    token=$(cat "${KAMARKAT_HOME:-$HOME/.kamarkat}/hetzner/token" 2>/dev/null | tr -d '[:space:]')
    if [[ -z "$token" ]]; then
        warn "no hetzner/token in this operator's store — skipping capacity check"
        return 0
    fi

    local body
    if ! body=$(curl -sf --max-time 20 -H "Authorization: Bearer $token" \
        'https://api.hetzner.cloud/v1/load_balancers' 2>/dev/null); then
        warn "hcloud API unreachable — skipping capacity check"
        return 0
    fi

    # Sole label hcloud-ccm/service-uid identifies a CCM-owned load balancer; zero
    # targets means it is serving nothing. Either way it consumes quota.
    local dead dead_count
    dead=$(jq -r '
        [ .load_balancers[]
          | select((.labels | keys | length) == 1)
          | select(.labels["hcloud-ccm/service-uid"] != null)
          | select((.targets | length) == 0)
          | "\(.name) (id \(.id), \(.location.name), created \(.created))"
        ] | .[]' <<<"$body" 2>/dev/null)
    dead_count=$(grep -c . <<<"$dead" || true)
    [[ -z "$dead" ]] && dead_count=0

    if (( dead_count > 0 )); then
        hard_fail "$dead_count CCM load balancer(s) with zero targets — leaked, or live with no backends"
        while IFS= read -r d; do [[ -n "$d" ]] && note "$d"; done <<<"$dead"
        note "check each against a live Service before deleting: a live one is recreated by the CCM"
        note "see ADR-046 §23 (leak) and addendum 2 (exclude-from-external-load-balancers)"
    else
        pass "no CCM load balancers with zero targets"
    fi

    local total
    total=$(jq '.load_balancers | length' <<<"$body")

    # HETZNER_LB_BUDGET is what this run will ask for: a hub bootstrap needs its own
    # control-plane LB, and provisioning a spoke needs one more. Callers that know
    # better override it.
    local budget="${HETZNER_LB_BUDGET:-2}"
    if (( total + budget > HETZNER_LB_QUOTA )); then
        hard_fail "load balancer quota: $total/${HETZNER_LB_QUOTA} in use, this run needs $budget more"
        note "CAPH will report LoadBalancerCreateFailed and the CAPI Cluster will never leave Provisioning"
    else
        pass "load balancer headroom: $total/${HETZNER_LB_QUOTA} used, $budget needed by this run"
    fi
}

# ──────────────────────────────────────────────────────────────────────────
# §24 — Cilium must reach the API server on a cold boot
# ──────────────────────────────────────────────────────────────────────────
# Cilium must be able to reach the API server on a cold boot (ADR-046 §24).
#
# The failure this encodes: the shared ClusterClass deletes kube-proxy right after
# `kubeadm init`, because its REJECT chains for endpoint-less services break Gateway
# API under Cilium's full kube-proxy-replacement. Cilium is the intended replacement
# — but with no `k8s-service-host` it discovers the API server through the in-cluster
# ClusterIP (10.96.0.1), the address kube-proxy used to route and that Cilium has not
# yet programmed. A fresh spoke deadlocks: the `config` init container times out, the
# CNI never initialises, the node stays NotReady, and every workload sits Pending.
#
# It stayed latent for weeks because the kube-proxy deletion was applied to a RUNNING
# spoke, where Cilium was already up and deleting kube-proxy is harmless. Only a cold
# boot reproduces it, and ADR-046 §17.3 made spokes reprovision-only — so the first
# cold boot after that change is where it fired. Nothing static caught it, which is
# why this check is static.
#
# The invariant, in one line: if kube-proxy is deleted and Cilium is the replacement,
# something must supply the API endpoint before the first node boots.
validate_cilium_apiserver_endpoint() {
    section "Cilium reaches the API server on a cold boot (ADR-046 §24)"

    local cc="$VALIDATE_ROOT/manifests/providers/hetzner/base/spokepool-clusterclass-v1.yaml"
    local kube_proxy_deleted=0
    if grep -q 'delete daemonset -n kube-system kube-proxy' "$cc" 2>/dev/null; then
        kube_proxy_deleted=1
    fi

    if (( ! kube_proxy_deleted )); then
        pass "spoke ClusterClass keeps kube-proxy — the ClusterIP path still works"
        return 0
    fi
    note "spoke ClusterClass deletes kube-proxy at init; Cilium must have a direct endpoint"

    # 1. Exactly one owner. An addon Secret that still ships a cilium-config document
    #    would race the rendered one, and which wins would depend on CRS apply order.
    local addon
    for addon in "manifests/providers/hybrid/k8s/cilium-addon-hybrid.yaml" \
                 "manifests/spoke/spoke-bootstrap/cilium-addon-template.yaml"; do
        [[ -f "$VALIDATE_ROOT/$addon" ]] || continue
        if (cd "$VALIDATE_ROOT" && python3 - "$addon" <<'PY'
import sys, yaml
doc = yaml.safe_load(open(sys.argv[1]))
payload = (doc.get("stringData") or doc.get("data") or {}).get("cilium.yaml", "")
found = any(d and d.get("kind") == "ConfigMap" and d["metadata"]["name"] == "cilium-config"
            for d in yaml.safe_load_all(payload))
sys.exit(1 if found else 0)
PY
        ); then
            pass "$(basename "$addon") ships no cilium-config — single owner preserved"
        else
            hard_fail "$addon still ships a cilium-config ConfigMap; hub-operator also renders one — two owners for one object"
        fi
    done

    # 1b. The ConfigMap value alone is NOT sufficient, and this arm exists because
    #     shipping it that way did not work.
    #
    #     The `config` init container's job is to READ cilium-config from the API.
    #     It cannot use a value inside that ConfigMap to find the API — circular —
    #     so it uses KUBERNETES_SERVICE_HOST, which kubelet defaults to the
    #     ClusterIP. Upstream's chart injects that env var on the containers when
    #     k8sServiceHost is set; a split that populates only the ConfigMap leaves
    #     the agent still dialling 10.96.0.1 and still deadlocked.
    #
    #     configMapKeyRef keeps one owner: the DaemonSet names a KEY, the rendered
    #     ConfigMap supplies the VALUE, and kubelet resolves it at pod creation over
    #     its own kubeconfig. optional:true stays after §24.6 gave the hub the same
    #     two keys: a rendered copy that is somehow missing one then leaves the agent
    #     dialling the ClusterIP and failing loudly on it, rather than refusing to
    #     start with CreateContainerConfigError and no diagnosis at all.
    for addon in "manifests/providers/hybrid/k8s/cilium-addon-hybrid.yaml" \
                 "manifests/spoke/spoke-bootstrap/cilium-addon-template.yaml"; do
        [[ -f "$VALIDATE_ROOT/$addon" ]] || continue
        if (cd "$VALIDATE_ROOT" && python3 - "$addon" <<'PY'
import sys, yaml
doc = yaml.safe_load(open(sys.argv[1]))
need = {("DaemonSet", "cilium"): {"config", "cilium-agent"},
        ("Deployment", "cilium-operator"): {"cilium-operator"}}
seen = set()
for d in yaml.safe_load_all((doc.get("stringData") or {}).get("cilium.yaml", "")):
    if not d:
        continue
    key = (d.get("kind"), d.get("metadata", {}).get("name"))
    if key not in need:
        continue
    spec = d["spec"]["template"]["spec"]
    for grp in ("initContainers", "containers"):
        for c in spec.get(grp, []):
            if c["name"] not in need[key]:
                continue
            envs = {e["name"]: e for e in c.get("env", [])}
            for var, k in (("KUBERNETES_SERVICE_HOST", "k8s-service-host"),
                           ("KUBERNETES_SERVICE_PORT", "k8s-service-port")):
                ref = (envs.get(var) or {}).get("valueFrom", {}).get("configMapKeyRef")
                if not ref or ref.get("name") != "cilium-config" \
                   or ref.get("key") != k or ref.get("optional") is not True:
                    print(f"{key[1]}/{c['name']} missing optional configMapKeyRef for {var}")
                    sys.exit(1)
            seen.add((key[1], c["name"]))
missing = {(k[1], c) for k, cs in need.items() for c in cs} - seen
if missing:
    print("containers not found: " + ", ".join(f"{a}/{b}" for a, b in sorted(missing)))
    sys.exit(1)
sys.exit(0)
PY
        ); then
            pass "$(basename "$addon") wires KUBERNETES_SERVICE_HOST/PORT from cilium-config"
        else
            hard_fail "$addon does not wire KUBERNETES_SERVICE_HOST/PORT via optional configMapKeyRef — the config init container will keep dialling the ClusterIP and deadlock, even with a correct ConfigMap"
        fi
    done

    # 1c. cilium-operator must fit on one node (ADR-046 §24.3).
    #
    #     It declares hostPort 9963 with hostNetwork, so two replicas can never share
    #     a node — and a hybrid spoke is single-node AT BOOT by construction: burst
    #     workers are replicas:0 and home workers join only after the control plane
    #     is Ready. Two replicas deadlock the cold boot in every environment:
    #     operator Unschedulable -> CRDs never registered -> agent never Ready ->
    #     Node never Ready -> the home-worker join never runs -> still one node.
    #
    #     Checked here rather than trusted to a comment: the manifest previously
    #     carried one asserting this was handled by a spoke-catalog overlay that did
    #     not exist and could not have worked (cilium is CRS-delivered; an ArgoCD
    #     overlay would be a second owner, which ADR-048 forbids).
    if (cd "$VALIDATE_ROOT" && python3 - "manifests/providers/hybrid/k8s/cilium-addon-hybrid.yaml" <<'PY'
import sys, yaml
doc = yaml.safe_load(open(sys.argv[1]))
for d in yaml.safe_load_all((doc.get("stringData") or {}).get("cilium.yaml", "")):
    if d and d.get("kind") == "Deployment" and d["metadata"]["name"] == "cilium-operator":
        ports = [p.get("hostPort") for c in d["spec"]["template"]["spec"]["containers"]
                 for p in c.get("ports", []) if p.get("hostPort")]
        replicas = d["spec"].get("replicas", 1)
        # Only a hostPort-bound operator is constrained; if upstream ever drops the
        # hostPort, more replicas become legitimate and this check should not block.
        if ports and replicas != 1:
            print("replicas>1 with a hostPort"); sys.exit(1)
        # Scaling to 1 is not enough on its own: a surge-based rollout keeps the old
        # pod (which holds the hostPort) until the new one is Ready, so the new one
        # can never schedule. With replicas:1 the percentage defaults resolve to
        # maxSurge=1 / maxUnavailable=0 — the deadlock. Recreate, or an explicit
        # zero surge, is required. Observed live before it was fixed.
        strat = d["spec"].get("strategy", {})
        if ports and strat.get("type") != "Recreate":
            surge = str(strat.get("rollingUpdate", {}).get("maxSurge", "25%"))
            if surge not in ("0", "0%"):
                print(f"hostPort workload uses surge rollout (type={strat.get('type')}, maxSurge={surge})")
                sys.exit(1)
        sys.exit(0)
sys.exit(1)
PY
    ); then
        pass "hybrid cilium-operator fits a single-node spoke and can roll without deadlocking"
    else
        hard_fail "hybrid cilium-operator cannot converge on a single-node spoke — either >1 replica with a hostPort, or a surge-based rollout whose old pod holds the port until the new one is Ready (which it never can be)"
    fi

    # 2. The renderer exists. If it is deleted while the split stays, no spoke gets a
    #    cilium-config at all — a worse failure than the one this replaced.
    local ctrl="$VALIDATE_ROOT/operators/hub-operator/internal/controller/spokepool_controller.go"
    if grep -q 'func (r \*SpokePoolReconciler) ensureCiliumConfigCRSWrapper' "$ctrl" 2>/dev/null &&
       grep -q 'r.ensureCiliumConfigCRSWrapper(ctx, spokePool)' "$ctrl" 2>/dev/null; then
        pass "hub-operator renders the cilium-config CRS wrapper and calls it from Reconcile"
    else
        hard_fail "hub-operator no longer renders cilium-config, but the addons were split on the assumption that it does — spokes would boot with no CNI config"
    fi

    # 3. The base carries the settings and NOT the endpoint. A pinned endpoint in Git
    #    cannot be right for more than one spoke, and a stale one is worse than none.
    local base provider found_base=0
    for base in "manifests/providers/hybrid/k8s/cilium-config-base.yaml" \
                "manifests/providers/hetzner/k8s/cilium-config-base.yaml"; do
        [[ -f "$VALIDATE_ROOT/$base" ]] || { hard_fail "missing cilium-config base: $base"; continue; }
        found_base=1
        provider=$(basename "$(dirname "$(dirname "$base")")")
        if (cd "$VALIDATE_ROOT" && python3 - "$base" <<'PY'
import sys, yaml
d = yaml.safe_load(open(sys.argv[1]))
data = d.get("data") or {}
bad = [k for k in ("k8s-service-host", "k8s-service-port") if k in data]
if bad:
    print(",".join(bad)); sys.exit(1)
if data.get("kube-proxy-replacement") != "true":
    print("kube-proxy-replacement not true"); sys.exit(2)
sys.exit(0)
PY
        ); then
            pass "$provider base defines the settings, leaves the endpoint to the renderer"
        else
            hard_fail "$base pins k8s-service-host/port, or does not set kube-proxy-replacement — the endpoint is per-spoke and belongs to hub-operator alone"
        fi
    done
    (( found_base )) || hard_fail "no cilium-config base found; the split is half-applied"

    # 4. Both compositions must deliver the wrapper, or that provider's spokes get a
    #    DaemonSet with no config.
    local comp
    for comp in "manifests/providers/hybrid/k8s/spokepool-hybrid-composition.yaml" \
                "manifests/providers/hetzner/k8s/spokepool-hetzner-composition.yaml"; do
        if grep -q '%s-cilium-config' "$VALIDATE_ROOT/$comp" 2>/dev/null; then
            pass "$(basename "$comp" | cut -d- -f2) composition registers the cilium-config wrapper"
        else
            hard_fail "$comp does not register {spoke}-cilium-config in its ClusterResourceSet"
        fi
    done

    # 5. The endpoint's only source. CAPH populates controlPlaneEndpoint from the
    #    load balancer it creates; with the LB disabled there is no endpoint, and the
    #    renderer would defer forever with no visible cause on the spoke.
    # Parsed, not grepped: the default sits under a nested openAPIV3Schema, so a
    # line-window grep reports a false failure (it did, on the first run of this
    # check — the manifest was correct).
    if (cd "$VALIDATE_ROOT" && python3 - "manifests/providers/hetzner/base/spokepool-clusterclass-v1.yaml" <<'PY'
import sys, yaml
for doc in yaml.safe_load_all(open(sys.argv[1])):
    if not doc or doc.get("kind") != "ClusterClass":
        continue
    for v in doc["spec"].get("variables", []):
        if v["name"] == "controlPlaneLoadBalancerEnabled":
            sys.exit(0 if v["schema"]["openAPIV3Schema"].get("default") is True else 1)
sys.exit(1)
PY
    ); then
        pass "controlPlaneLoadBalancerEnabled defaults true — the endpoint has a source"
    else
        hard_fail "spoke ClusterClass does not default controlPlaneLoadBalancerEnabled=true; without the LB there is no controlPlaneEndpoint and cilium-config can never render"
    fi

    # 6. The hub has no SpokePool and no renderer, so the CLI composes its copy at
    #    Day-0 from the same base. If that concatenation is dropped the hub boots with
    #    no cilium-config at all.
    local day0="$VALIDATE_ROOT/internal/kakt-cli/bootstrap/provider_cloud.go"
    if grep -q 'cilium-config-base.yaml' "$day0" 2>/dev/null; then
        pass "hub Day-0 install recomposes the addon with the cilium-config base"
    else
        hard_fail "hub Day-0 CNI install no longer reads cilium-config-base.yaml — the hub would boot with no cilium-config"
    fi

    # 7. The hub's endpoint is injected too (§24.6). §24.1 exempted the hub because it
    #    "keeps kube-proxy"; it does not — the base it installs sets kube-proxy-replacement
    #    true and the ClusterClass deletes kube-proxy — so the hub's own agents resolved
    #    the API server through 10.96.0.1 and deadlocked on 2026-10-28 when the API
    #    server was OOM-killed. The CLI is the single owner of the hub's two keys, and
    #    it must read the endpoint from the Cluster and render after the Cluster exists.
    local prov="$VALIDATE_ROOT/internal/kakt-cli/cluster/provisioner.go"
    if grep -q 'func injectAPIServerEndpoint' "$prov" 2>/dev/null &&
        grep -q 'func (p \*Provisioner) controlPlaneEndpoint' "$prov" 2>/dev/null; then
        pass "hub Day-0 CNI render injects the API server endpoint into cilium-config"
    else
        hard_fail "the hub Day-0 CNI render no longer injects k8s-service-host/port (ADR-046 §24.6) — the hub would boot reaching the API server only through 10.96.0.1, which cannot recover when that endpoint is withdrawn"
    fi

    # The render must FOLLOW the Cluster: the endpoint does not exist until the
    # infrastructure provider has created the control-plane load balancer. Applying the
    # addon first is how the exemption was able to exist at all, and an injection that
    # runs before applyCluster silently has nothing to inject.
    if python3 - "$prov" <<'PY'
import re, sys
src = open(sys.argv[1]).read()
body = re.search(r"func \(p \*Provisioner\) Provision\(ctx context\.Context\) error \{(.*?)\n\}", src, re.S)
if not body:
    sys.exit(1)
b = body.group(1)
cluster, crs = b.find("p.applyCluster(ctx)"), b.find("p.applyCRS(ctx")
sys.exit(0 if 0 <= cluster < crs else 1)
PY
    then
        pass "the CNI addon is applied after the Cluster, so the endpoint exists when it renders"
    else
        hard_fail "Provision applies the CNI addon before (or without) the Cluster; Cluster.spec.controlPlaneEndpoint is not set yet, so cilium-config would render with no API server address (ADR-046 §24.6)"
    fi

    # The endpoint is read from the Cluster, the same field hub-operator reads for a
    # spoke: one contract, whichever provider is underneath.
    if grep -q 'spec.controlPlaneEndpoint.host' "$prov" 2>/dev/null; then
        pass "the hub's endpoint is read from Cluster.spec.controlPlaneEndpoint"
    else
        hard_fail "the hub Day-0 render no longer reads Cluster.spec.controlPlaneEndpoint; the endpoint contract is that field, not an infrastructure object (ADR-046 §24.6)"
    fi
}

# ──────────────────────────────────────────────────────────────────────────
# §25.1 — apiserver LB port vs extraServices
# ──────────────────────────────────────────────────────────────────────────
# The kube-apiserver's LB listen port must not collide with a declared
# extraServices listenPort (ADR-046 §25.1).
#
# CAPH publishes the apiserver on the control-plane load balancer at
# `controlPlaneEndpoint.port`. A Hetzner LB cannot carry two services on one
# listen_port, so an extraServices entry claiming that same port is silently
# dropped: no error, no event, no condition — `LoadBalancerReady=True` and
# `HetznerCluster Ready=True` throughout.
#
# This is not hypothetical. The spoke ClusterClass declared 443->443 for tenant
# HTTPS while `controlPlaneEndpoint.port` was also 443. The LB served
# `listen 443 -> dest 6443` (the apiserver) and tenant HTTPS was dead from §17.4
# until §25 — `openssl s_client` against the LB returned CN=kube-apiserver.
# Every status object read healthy for the entire period, which is precisely why
# this needs a static check rather than a runtime one.
#
# Logic lives in the sibling .py: the repo has already hit bash quoting failures
# embedding YAML-parsing Python in heredocs (see 96-spoke-secret-authz.sh).
validate_lb_listen_port_collision() {
    section "Control-plane LB listen ports do not collide (ADR-046 §25.1)"

    local out
    out=$(cd "$VALIDATE_ROOT" && python3 "$VALIDATE_DIR/preflight/046-lb-listen-port-collision.py") || true

    if [[ -z "$out" ]]; then
        hard_fail "LB listen-port collision check produced no output"
        return 0
    fi

    local line
    while IFS= read -r line; do
        [[ -z "$line" ]] && continue
        case "$line" in
            OK*)  pass "$(printf '%s' "$line" | cut -f2-)" ;;
            BAD*) hard_fail "$(printf '%s' "$line" | cut -f2-)" ;;
            *)    note "$line" ;;
        esac
    done <<< "$out"
}

# ──────────────────────────────────────────────────────────────────────────
# §11 — placement classes and their storage
# ──────────────────────────────────────────────────────────────────────────
# ADR-046 §11 defines exactly two placement classes for stateful workloads, and
# each pairs a location with the only storage its nodes can attach:
#
#   on-prem  workload-location: on-prem     -> local-path      (nodes on the tenant's premises)
#   hetzner  workload-location: hetzner  -> hcloud-volumes  (Hetzner nodes)
#
# Mixing them is BANNED. hcloud-volumes cannot be attached by a home VM, so the PVC
# binds nowhere and the pod sits Pending; §11 calls that "the correct failure mode",
# which is precisely why it is invisible — nothing errors, the bootstrap just stops.
#
# This drifted once already: platform-db declared its placement while redis
# did not, and each failed in turn on separate days. The check exists so the third
# case is caught here rather than 40 minutes into a bootstrap.
validate_placement_class() {
    section "ADR-046 §11 placement classes (rendered per provider)"

    local out
    out=$(cd "$VALIDATE_ROOT" && python3 - <<'PY'
import glob, os, subprocess, sys, yaml

# The location a provider's workers live in, and the only class they can bind.
EXPECTED = {"hybrid": ("on-prem", "local-path"), "hetzner": ("hetzner", "hcloud-volumes")}

# Hub provider overlays AND spoke-catalog environment overlays. The spoke tree was
# missing here, and that is the whole reason this check passed while every hybrid
# spoke CNPG asked for hcloud-volumes with no nodeSelector at all — the exact
# condition §11 was written to ban, in the exact file it was written about.
#   manifests/hub-core-services/providers/<provider>/<component>
#   manifests/spoke/spoke-catalog/providers/<provider>
roots = sorted(glob.glob("manifests/hub-core-services/providers/*/*")) \
      + sorted(glob.glob("manifests/spoke/spoke-catalog/providers/*"))
if not roots:
    print("NONE")
    raise SystemExit

for root in roots:
    # provider is the parent dir for hub overlays and the LEAF for spoke overlays.
    parts = root.split(os.sep)
    provider = parts[-1] if "spoke-catalog" in root else parts[-2]
    want = EXPECTED.get(provider)
    if want is None:
        print(f"BAD\t{root}\tunknown provider directory {provider!r}")
        continue
    want_loc, want_sc = want

    r = subprocess.run(["kubectl", "kustomize", root], capture_output=True, text=True)
    if r.returncode != 0:
        print(f"BAD\t{root}\tdoes not build: {r.stderr.strip().splitlines()[0] if r.stderr.strip() else 'unknown'}")
        continue

    for doc in yaml.safe_load_all(r.stdout):
        if not doc:
            continue
        kind, name = doc.get("kind"), doc.get("metadata", {}).get("name", "?")
        spec = doc.get("spec", {})

        if kind == "StatefulSet":
            vcts = spec.get("volumeClaimTemplates") or []
            if not vcts:
                continue
            pod = spec.get("template", {}).get("spec", {}) or {}
            sel = pod.get("nodeSelector") or {}
            scs = [v.get("spec", {}).get("storageClassName") for v in vcts]
        elif kind == "Cluster" and "storage" in spec:   # CNPG
            sel = (spec.get("affinity", {}) or {}).get("nodeSelector") or {}
            scs = [spec.get("storage", {}).get("storageClass")]
        else:
            continue

        # ADR-014 applies to both classes and is not optional.
        if sel.get("node-role.kubernetes.io/worker") is None:
            print(f"BAD\t{root}\t{kind}/{name} has no node-role.kubernetes.io/worker selector (ADR-014)")

        # A value substituted per provider by templated-fields is a PLACEHOLDER here:
        # this builds the overlay, and the substitution happens at package time. The
        # DECLARATION is asserted instead, by validate_observability_placement below.
        # The worker key above is literal, so it is checked either way.
        loc = sel.get("workload-location")
        if loc != want_loc and not str(loc).startswith("PLACEHOLDER_"):
            print(f"BAD\t{root}\t{kind}/{name} workload-location={loc!r}, expected {want_loc!r}")

        for sc in scs:
            if sc != want_sc and not str(sc).startswith("PLACEHOLDER_"):
                print(f"BAD\t{root}\t{kind}/{name} storageClass={sc!r}, expected {want_sc!r} for a {want_loc} workload")
PY
)

    if [[ "$out" == "NONE" ]]; then
        hard_fail "no provider overlays found under manifests/hub-core-services/providers/"
        return 0
    fi

    if [[ -z "$out" ]]; then
        pass "every provider overlay renders a consistent §11 placement class"
    else
        local tag root msg
        while IFS=$'\t' read -r tag root msg; do
            [[ -z "$tag" ]] && continue
            hard_fail "$root — $msg"
        done <<< "$out"
    fi
}

# A base that still carries location-specific settings defeats the overlays: it
# would apply to every provider, which is how the mixed class arises in the first
# place. The bases must declare the ADR-014 worker selector and nothing more.
validate_bases_are_provider_neutral() {
    section "Placement bases carry no provider-specific settings"

    local out
    out=$(cd "$VALIDATE_ROOT" && python3 - <<'PY'
import glob, yaml

for comp in ("database", "redis"):
    for f in glob.glob(f"manifests/hub-core-services/{comp}/*.yaml"):
        try:
            docs = [d for d in yaml.safe_load_all(open(f)) if d]
        except Exception:
            continue
        for d in docs:
            kind, spec = d.get("kind"), d.get("spec", {})
            name = d.get("metadata", {}).get("name", "?")
            if kind == "StatefulSet":
                sel = (spec.get("template", {}).get("spec", {}) or {}).get("nodeSelector") or {}
                scs = [v.get("spec", {}).get("storageClassName")
                       for v in (spec.get("volumeClaimTemplates") or [])]
            elif kind == "Cluster" and "storage" in spec:
                sel = (spec.get("affinity", {}) or {}).get("nodeSelector") or {}
                scs = [spec.get("storage", {}).get("storageClass")]
            else:
                continue
            if "workload-location" in sel:
                print(f"{f}: {kind}/{name} pins workload-location in the BASE")
            for sc in scs:
                if sc:
                    print(f"{f}: {kind}/{name} pins storageClass={sc!r} in the BASE")
PY
)

    if [[ -z "$out" ]]; then
        pass "bases declare only the ADR-014 worker selector"
    else
        while IFS= read -r line; do
            [[ -n "$line" ]] && hard_fail "$line"
        done <<< "$out"
    fi
}

# ─── §6, §11, §14 — the datapath invariants a chart upgrade must not reset ────
#
# Every value below is a DECISION with an incident behind it, and none of them
# is asserted anywhere else. That mattered the moment a Cilium upgrade was
# scoped: the settings live in cilium-config-base.yaml and a re-render from
# stock values silently restores the chart's defaults. Nothing fails, nodes go
# Ready, and the datapath breaks in ways that look like something else --
#
#   mtu 1200          ADR-046 §6/§14. 1200 inner + 50B VXLAN = 1250B, inside
#                     tailscale0's 1280. The stock overlay MTU produced ~1308B
#                     outer packets, which fragmented and were dropped by the
#                     BPF datapath as "First logical datagram fragment not
#                     found" -- traffic that works until a payload crosses the
#                     threshold.
#   routing-mode      §14. Native routing was tried and abandoned: it needed
#   tunnel/vxlan      Tailscale subnet-route advertisement, whose table-52
#                     entries shadowed Cilium's own routes (§17.1), and it put
#                     Envoy's return path into a cil_from_netdev/cilium_host
#                     loop that timed out as HTTP 503.
#   auto-direct-      §14, §17.1. These are NATIVE-routing settings. Re-enabling
#   node-routes       either one reintroduces the stale-route collision class
#   direct-routing-   that tunnel mode exists to remove, which is why the
#   device            assertion is that they stay off rather than that they
#                     hold some value.
#
# Asserted per provider, because the providers genuinely differ: the
# hostNetwork Gateway and its Envoy settings are hybrid-only (§8, §15), and
# flattening them into one expectation would either pass on hetzner for the
# wrong reason or fail on it for no reason.
_adr046_config_value() {
    local provider="$1" key="$2"
    python3 - "$provider" "$key" <<'PY' 2>/dev/null
import sys, yaml
provider, key = sys.argv[1], sys.argv[2]
with open(f"manifests/providers/{provider}/k8s/cilium-config-base.yaml") as f:
    print(yaml.safe_load(f)["data"].get(key, "<absent>"))
PY
}

_adr046_expect() {
    local provider="$1" key="$2" want="$3" why="$4" got
    got="$(_adr046_config_value "$provider" "$key")"
    if [[ "$got" == "$want" ]]; then
        pass "cilium-config-base($provider): $key=$want"
    else
        hard_fail "cilium-config-base($provider): $key is '${got}', must be '${want}' — $why"
    fi
}

validate_adr046_datapath_invariants() {
    local p
    for p in hybrid hetzner; do
        [[ -f "manifests/providers/$p/k8s/cilium-config-base.yaml" ]] || {
            hard_fail "manifests/providers/$p/k8s/cilium-config-base.yaml is missing — the datapath settings have no home and a rendered chart would supply its own defaults"
            continue
        }

        # Shared by both providers. A spoke of either kind carries VXLAN over a
        # 1280-MTU underlay.
        _adr046_expect "$p" mtu 1200 \
            "ADR-046 §6/§14: 1200 + 50B VXLAN = 1250B, inside tailscale0's 1280. Larger fragments and the BPF datapath drops it"
        _adr046_expect "$p" routing-mode tunnel \
            "ADR-046 §14: native routing shadowed Cilium's routes via Tailscale table-52 entries and looped Envoy's return path"
        _adr046_expect "$p" tunnel-protocol vxlan \
            "ADR-046 §14: the tunnel is VXLAN between node tailnet IPs"
        _adr046_expect "$p" enable-pmtu-discovery true \
            "ADR-046 §14: path-MTU discovery is what keeps the 1250B ceiling honest for traffic that ignores it"
        _adr046_expect "$p" kube-proxy-replacement true \
            "ADR-046 §24: the ClusterClass deletes kube-proxy, so Cilium IS the replacement"
        _adr046_expect "$p" enable-local-redirect-policy true \
            "ADR-046: NodeLocal DNSCache reaches pods ONLY through a CiliumLocalRedirectPolicy, because kube-proxy-replacement rewrites the kube-dns ClusterIP inside connect() before any host-bound listener can see it. False here and the policy is accepted, inert, and every pod's DNS crosses the overlay while the cache reports Ready"

        # Native-routing settings that must STAY off (§14, §17.1).
        local adnr drd
        adnr="$(_adr046_config_value "$p" auto-direct-node-routes)"
        if [[ "$adnr" == "false" || "$adnr" == "<absent>" ]]; then
            pass "cilium-config-base($p): auto-direct-node-routes is off"
        else
            hard_fail "cilium-config-base($p): auto-direct-node-routes is '${adnr}' — ADR-046 §17.1 forbids re-enabling native routing; its table-52 routes shadow Cilium's own"
        fi
        drd="$(_adr046_config_value "$p" direct-routing-device)"
        if [[ -z "$drd" || "$drd" == "<absent>" ]]; then
            pass "cilium-config-base($p): direct-routing-device is unset"
        else
            hard_fail "cilium-config-base($p): direct-routing-device is '${drd}' — ADR-046 §17.1 requires it unset in tunnel mode"
        fi
    done

    # Hybrid only: the hostNetwork Gateway and its decoupled Envoy (§8, §15).
    # hetzner serves ingress through a cloud load balancer and sets none of
    # these, so asserting them there would be asserting someone else's design.
    _adr046_expect hybrid tunnel-port 8472 \
        "ADR-046 §14: the VXLAN port carried between node tailnet IPs"
    _adr046_expect hybrid gateway-api-hostnetwork-enabled true \
        "ADR-046 §8: the spoke Gateway binds host ports on the control plane; without this no listener is reachable from the load balancer"
    _adr046_expect hybrid disable-envoy-version-check true \
        "ADR-046 §15: the decoupled standalone Envoy is pinned to the agent build, and the version check rejects it on Kubernetes 1.31+"
    _adr046_expect hybrid envoy-keep-cap-netbindservice true \
        "ADR-046 §15: Envoy binds privileged host ports 80/443 and needs the capability retained"
}


# ──────────────────────────────────────────────────────────────────────────
# NodeLocal DNSCache must be in the LOCAL-REDIRECT shape, not the host-bound one
# ──────────────────────────────────────────────────────────────────────────
# enable-local-redirect-policy above makes the policy possible; this makes it
# apply.
#
# NodeLocal DNSCache ships in two mutually exclusive arrangements that look
# almost identical in a diff:
#
#   host-bound       hostNetwork: true, binds 169.254.20.10 and the kube-dns
#   (upstream)       ClusterIP on the node, creates a dummy interface and NOTRACK
#                    rules itself. Depends on the service proxy leaving a locally
#                    bound ClusterIP alone -- true under kube-proxy, FALSE under
#                    Cilium's socket load balancer, which rewrites the
#                    destination in the pod's own namespace before a packet
#                    exists.
#
#   local-redirect   hostNetwork: false, binds 0.0.0.0 in the pod namespace, and
#   (Cilium)         is put in the path by a CiliumLocalRedirectPolicy.
#
# Picking the first on this platform yields a component that is Running, Ready
# and Synced/Healthy while doing nothing at all. Measured on the hub: 40/40
# lookups succeeded against the cache directly, 38/40 against the kube-dns
# ClusterIP from the same pod -- the failures being queries the socket LB had
# sent to the CoreDNS replica on the far side of the Tailscale overlay. What
# that surfaced as was EAI_AGAIN inside the Zitadel login app and a sign-in
# reporting "Could not create session for user": an identity error for a DNS
# fault, three layers from the cause.
#
# Asserted here because no status anywhere reports the difference, and because
# reverting hostNetwork "back" to the upstream default would look entirely
# reasonable in review.
validate_adr046_nodelocal_dns_local_redirect() {
    local f="manifests/hub-core-services/dns/coredns-nodelocal.yaml"
    if [[ ! -f "$f" ]]; then
        hard_fail "$f is missing -- NodeLocal DNSCache is what keeps pod DNS off the Tailscale overlay (ADR-046)"
        return
    fi

    # One invocation, output captured: running the checker twice to re-read its
    # findings is how a validator and its subject drift apart.
    local findings rc
    findings="$(python3 - "$f" <<'PYEOF'
import sys, yaml

docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
ds = next((d for d in docs if d.get("kind") == "DaemonSet"), None)
lrp = next((d for d in docs if d.get("kind") == "CiliumLocalRedirectPolicy"), None)

if ds is None:
    print("no DaemonSet document -- the cache is not declared here at all")
else:
    spec = ds["spec"]["template"]["spec"]
    if spec.get("hostNetwork", False):
        print("the DaemonSet sets hostNetwork: true -- a CiliumLocalRedirectPolicy "
              "redirects to Cilium endpoints and a host-networked pod is not one, so "
              "the policy matches no backend and silently does nothing")
    container = spec["containers"][0]
    args = " ".join(container.get("args", []))
    for flag in ("-setupinterface=false", "-setupiptables=false", "-skipteardown=true"):
        if flag not in args:
            print(f"node-cache is missing {flag} -- without it the cache provisions a "
                  "link-local interface and NOTRACK rules for an address nothing is sent "
                  "to, and tears that interface down on exit")
    names = {p.get("name") for p in container.get("ports", [])}
    for want in ("dns", "dns-tcp"):
        if want not in names:
            print(f"container port '{want}' is not named -- the policy's toPorts "
                  "references it by name and cannot bind to an unnamed port")

if lrp is None:
    print("no CiliumLocalRedirectPolicy document -- under kube-proxy-replacement the "
          "DaemonSet alone is not in the DNS path")
else:
    fe = lrp["spec"]["redirectFrontend"].get("serviceMatcher", {})
    if (fe.get("serviceName"), fe.get("namespace")) != ("kube-dns", "kube-system"):
        print(f"the policy's frontend is {fe}, not kube-dns/kube-system -- pods resolve "
              "against that Service and no other")
    sel = lrp["spec"]["redirectBackend"]["localEndpointSelector"]["matchLabels"]
    if sel.get("k8s-app") != "node-local-dns":
        print(f"the policy's backend selector is {sel}, which does not select the cache")
PYEOF
)"
    rc=$?

    # A crashed checker is a failure, not a pass. Three preflight modules once
    # reported green because their python died before printing anything.
    if [[ $rc -ne 0 ]]; then
        hard_fail "coredns-nodelocal.yaml: the shape checker exited $rc without completing -- ${findings:-no output}"
        return
    fi

    if [[ -z "$findings" ]]; then
        pass "NodeLocal DNSCache is in the local-redirect shape and the policy selects it"
        return
    fi

    local line
    while IFS= read -r line; do
        [[ -n "$line" ]] && hard_fail "coredns-nodelocal.yaml: $line"
    done <<< "$findings"
}

# ──────────────────────────────────────────────────────────────────────────
# §11 + ADR-014 — observability is stateful, so it is worker-pinned too
# ──────────────────────────────────────────────────────────────────────────
# ADR-014's Platform-Wide Placement Rule is unconditional: "every stateful workload
# pod" runs on worker nodes only, "in every cluster, every provider cell, and every
# environment", with "no exceptions and no per-cluster opt-outs", and a stateful
# workload "SHALL NOT tolerate" the control-plane taint.
#
# ADR-078 add.5 shipped four PVC-backed observability workloads per cluster -- the
# Alloy cluster agent, Loki and two VMSingles -- with no placement at all, and the
# spoke catalogue patched the control-plane toleration onto every StatefulSet. On
# nutgraf-01 (one control-plane node, TWO idle workers) that put loki-0,
# grafana-alloy-cluster-0 and vmsingle-platform-b on the control-plane node: the
# exact co-location ADR-014's rationale names, on the node whose memory exhaustion
# caused the 2026-10-28 outage. Observed 2026-10-30, corrected by that date.
#
# Two mechanisms own the contract, so each is asserted where it is readable:
#
#   the worker key   literal in the manifest, asserted at build time by
#                    validate_placement_class above, which no longer exempts these
#   location + class per provider, substituted by templated-fields, so a kustomize
#                    build shows a placeholder and the DECLARATION is asserted here
validate_observability_placement() {
    section "ADR-014: stateful observability declares worker placement in every cluster"

    local out line
    out=$(cd "$VALIDATE_ROOT" && python3 - <<'PYCHECK'
import glob, subprocess, yaml

WORKER = "node-role.kubernetes.io/worker"
CP_TAINT = "node-role.kubernetes.io/control-plane"
# Component -> the declaration that must carry its provider-specific half.
DECLS = {
    "manifests/spoke/spoke-catalog/templated-fields.yaml":
        [("StatefulSet", "loki"), ("StatefulSet", "grafana-alloy-cluster"),
         ("VMSingle", "platform-a"), ("VMSingle", "platform-b")],
    "manifests/hub-core-services/loki/templated-fields.yaml": [("StatefulSet", "loki")],
    "manifests/hub-core-services/grafana-alloy/templated-fields.yaml":
        [("StatefulSet", "grafana-alloy-cluster")],
    "manifests/hub-core-services/victoriametrics/templated-fields.yaml":
        [("VMSingle", "platform-a"), ("VMSingle", "platform-b")],
}

for decl, wanted in sorted(DECLS.items()):
    specs = yaml.safe_load(open(decl))
    by_key = {(s.get("kind"), s.get("name")): s for s in specs}
    for kind, name in wanted:
        spec = by_key.get((kind, name))
        where = decl.split("/")[-2] + "/" + kind + "/" + name
        if spec is None:
            print("FAIL %s declares no templated fields, so its location and storage "
                  "class ship as placeholders" % where)
            continue
        fields = {tuple(f["path"]): f.get("value", "") for f in spec["fields"] if "path" in f}
        loc = next((v for k, v in fields.items() if k[-1] == "workload-location"), None)
        sc = next((v for k, v in fields.items() if k[-1] in ("storageClassName", "storageClass")), None)
        if loc is None:
            print("FAIL %s declares no workload-location; a PVC can then bind on a node "
                  "whose class cannot serve it, which §11 calls a placement violation" % where)
        elif "hybrid" not in loc:
            print("FAIL %s workload-location does not vary by provider: %r" % (where, loc))
        else:
            print("PASS %s: location and storage follow the provider" % where)
        if sc is None:
            print("FAIL %s declares no storage class, so the placeholder ships" % where)

# Nothing may put a stateful workload back on the control plane. Asserted on the
# rendered overlay, where the catalogue's own patches are visible.
for root in sorted(glob.glob("manifests/spoke/spoke-catalog/providers/*")):
    r = subprocess.run(["kubectl", "kustomize", root], capture_output=True, text=True)
    if r.returncode != 0:
        print("FAIL %s does not build, so its placement cannot be read" % root)
        continue
    bad = []
    for doc in yaml.safe_load_all(r.stdout):
        if not isinstance(doc, dict):
            continue
        kind, meta = doc.get("kind"), doc.get("metadata") or {}
        spec = doc.get("spec") or {}
        if kind == "StatefulSet":
            if not (spec.get("volumeClaimTemplates") or []):
                continue
            pod = (spec.get("template") or {}).get("spec") or {}
        elif kind == "VMSingle":
            pod = spec
        else:
            continue
        name = "%s/%s" % (kind, meta.get("name", "?"))
        if (pod.get("nodeSelector") or {}).get(WORKER) != "":
            bad.append("%s has no key-only %s selector" % (name, WORKER))
        if any(t.get("key") == CP_TAINT for t in (pod.get("tolerations") or [])):
            bad.append("%s tolerates the control-plane taint, which ADR-014 bans for a "
                       "stateful workload" % name)
    if bad:
        for b in bad:
            print("FAIL %s: %s" % (root, b))
    else:
        print("PASS %s: every PVC-backed workload is worker-pinned and tolerates no "
              "control-plane taint" % root)
PYCHECK
    )
    while IFS= read -r line; do
        [[ -z "$line" ]] && continue
        case "$line" in
            PASS\ *) pass "${line#PASS }" ;;
            FAIL\ *) hard_fail "${line#FAIL }" ;;
        esac
    done <<< "$out"
}
