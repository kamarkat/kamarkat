#!/usr/bin/env bash
# ADR-052 D4: the burst node's identity is defined in three places and all three
# must agree, exactly.
#
#   1. spokepool-burst-bootstrap-v1  register-with-taints / node-labels
#        — what the real node registers with
#   2. burst MachineDeployment       capacity.cluster-autoscaler.kubernetes.io/{taints,labels}
#        — what cluster-autoscaler simulates when the pool is at zero
#   3. ephemeral-job-operator        BurstTaintKey / BurstTaintValue in placement.go
#        — what the platform writes onto the workload
#
# Drift between (1) and (2) is a SILENT OUTAGE. With the pool at zero the
# autoscaler has no real node to inspect, so it builds a hypothetical one from
# the capacity annotations. If the real node carries a taint the annotation
# omits, the autoscaler simulates a node the pending pod cannot tolerate,
# concludes that scale-up would not help, declines to scale, and logs no error.
# The pod pends forever and every dashboard is green.
#
# Drift between (1) and (3) is the same failure with the polarity reversed: the
# workload tolerates a taint the node does not carry, or fails to tolerate one
# it does, and again nothing errors.
#
# Three hand-maintained strings for one taint is a silent-outage generator.
# This check is why it is not one.
validate_burst_node_identity() {
    section "ADR-052 D4 burst node identity (bootstrap / autoscaler / operator)"

    local out
    out=$(cd "$VALIDATE_ROOT" && python3 - <<'PY'
import io, re, sys, yaml

CLUSTERCLASS = "manifests/providers/hetzner/base/spokepool-clusterclass-v1.yaml"
COMPOSITION  = "manifests/providers/hybrid/k8s/spokepool-hybrid-composition.yaml"
PLACEMENT    = "operators/ephemeral-job-operator/internal/controller/placement.go"

errors = []

def norm_taints(s):
    return sorted(t.strip() for t in s.split(",") if t.strip())

def norm_labels(s):
    out = {}
    for kv in s.split(","):
        kv = kv.strip()
        if not kv:
            continue
        k, _, v = kv.partition("=")
        out[k] = v
    return out

# ── 1. the real node ──────────────────────────────────────────────────────
#
# The template name is RESOLVED from the ClusterClass, never hardcoded. A
# KubeadmConfigTemplate is immutable, so fixes ship as a new version and the
# ClusterClass is repointed; a check naming a version validates whatever that
# version happens to say long after nothing uses it. This check named
# spokepool-burst-bootstrap-v1 and would have kept passing against a template
# no node was built from.
docs = [d for d in yaml.safe_load_all(io.open(CLUSTERCLASS, encoding="utf-8")) if d]

burst_ref = None
for doc in docs:
    if doc.get("kind") != "ClusterClass":
        continue
    for m in doc["spec"]["workers"]["machineDeployments"]:
        if m.get("class") == "burst-worker":
            burst_ref = m["template"]["bootstrap"]["ref"]["name"]

if burst_ref is None:
    errors.append("no burst-worker class in the ClusterClass in %s" % CLUSTERCLASS)

node_taints = node_labels = None
burst_doc = None
for doc in docs:
    if doc.get("kind") != "KubeadmConfigTemplate":
        continue
    if doc["metadata"]["name"] != burst_ref:
        continue
    burst_doc = doc
    args = doc["spec"]["template"]["spec"]["joinConfiguration"]["nodeRegistration"]["kubeletExtraArgs"]
    node_taints = args.get("register-with-taints", "")
    node_labels = args.get("node-labels", "")

if node_taints is None:
    errors.append("%s (referenced by the burst-worker class) not found in %s"
                  % (burst_ref, CLUSTERCLASS))
elif not node_taints:
    errors.append(
        "burst bootstrap has no register-with-taints: the burst pool is "
        "unreserved, so any pod with a hetzner nodeSelector can consume "
        "capacity the cell pays for by the minute")

# ── 2. what the autoscaler simulates ──────────────────────────────────────
comp = yaml.safe_load(io.open(COMPOSITION, encoding="utf-8"))
md = None
for r in comp["spec"]["pipeline"][0]["input"]["resources"]:
    if r["name"] != "capi-cluster":
        continue
    mds = r["base"]["spec"]["forProvider"]["manifest"]["spec"]["topology"]["workers"]["machineDeployments"]
    md = next((m for m in mds if m.get("class") == "burst-worker"), None)

if md is None:
    errors.append("no burst-worker machineDeployment in %s" % COMPOSITION)
else:
    if "replicas" in md:
        errors.append(
            "burst machineDeployment declares replicas. CAPI rejects replicas "
            "together with the autoscaler bounds annotations, and "
            "provider-kubernetes applies the Cluster with a full Update — one "
            "invalid field wedges the whole Object at Synced=False and blocks "
            "every subsequent Crossplane update to that Cluster (bac465a9).")

    ann = (md.get("metadata") or {}).get("annotations") or {}
    ca_taints = ann.get("capacity.cluster-autoscaler.kubernetes.io/taints", "")
    ca_labels = ann.get("capacity.cluster-autoscaler.kubernetes.io/labels", "")

    for req in ("cluster.x-k8s.io/cluster-api-autoscaler-node-group-min-size",
                "cluster.x-k8s.io/cluster-api-autoscaler-node-group-max-size"):
        if req not in ann:
            errors.append("burst machineDeployment missing %s: the autoscaler "
                          "will not discover this node group at all" % req)

    if node_taints is not None and norm_taints(ca_taints) != norm_taints(node_taints):
        errors.append(
            "TAINT DRIFT — the autoscaler would simulate a node the real one is not.\n"
            "        node registers with : %s\n"
            "        autoscaler simulates: %s\n"
            "        Effect: pending burst pods never trigger scale-up, and nothing errors."
            % (node_taints or "(none)", ca_taints or "(none)"))

    if node_labels is not None:
        nl, cl = norm_labels(node_labels), norm_labels(ca_labels)
        missing = {k: v for k, v in nl.items() if k not in cl}
        if missing:
            errors.append(
                "LABEL DRIFT — labels on the real node are absent from the "
                "autoscaler's simulated node: %s\n"
                "        Effect: a pod selecting them is judged unschedulable on "
                "this group and scale-up is declined." % sorted(missing))

# ── 3. what the platform writes onto the workload ─────────────────────────
src = io.open(PLACEMENT, encoding="utf-8").read()
def const(name):
    m = re.search(r'\b%s\s*=\s*"([^"]*)"' % name, src)
    return m.group(1) if m else None

op_key, op_val = const("BurstTaintKey"), const("BurstTaintValue")
if op_key is None or op_val is None:
    errors.append("could not read BurstTaintKey/BurstTaintValue from %s" % PLACEMENT)
elif node_taints:
    want = "%s=%s:NoSchedule" % (op_key, op_val)
    if want not in norm_taints(node_taints):
        errors.append(
            "OPERATOR/NODE DRIFT — the toleration the operator writes does not "
            "match the taint the node carries.\n"
            "        operator tolerates: %s\n"
            "        node registers    : %s\n"
            "        Effect: every burst pod is rejected by the taint it was "
            "supposed to tolerate." % (want, node_taints))

# ── 4. the tailnet node IP (ADR-046 invariant 6) ──────────────────────────
#
# Invariant 6 requires a routable Tailscale IP on every node that carries pod
# traffic, and Cilium derives the VXLAN tunnel endpoint from the node's
# InternalIP DIRECTLY. With cloud-provider=external the hcloud CCM sets
# InternalIP to the node's private address, so without something re-asserting
# kubelet --node-ip the burst node registers 10.0.0.x, Cilium builds its tunnel
# there, and home-lab nodes have no route to it.
#
# The failure is partial, which is what makes it hard to see: the burst node
# still reaches pods on the Hetzner control plane over the private network, so
# cluster DNS resolves and the node looks healthy. Only traffic to home-lab
# nodes times out. On 2026-09-02 that presented as a sandbox harness dying with
# psycopg ConnectionTimeout against waypoint-pooler while every dashboard,
# including the node's own Ready condition, was green.
#
# Only the control-plane template carried dynamic-node-ip.sh; both worker
# templates lacked it, and burst was simply the first CAPI-provisioned worker to
# carry pod traffic.
NODE_IP_SCRIPT = "/usr/local/bin/dynamic-node-ip.sh"
NODE_IP_DROPIN = "/etc/systemd/system/kubelet.service.d/10-dynamic-node-ip.conf"

if burst_doc is not None:
    spec = burst_doc["spec"]["template"]["spec"]
    paths = [f.get("path") for f in (spec.get("files") or [])]
    pre = spec.get("preKubeadmCommands") or []

    if NODE_IP_SCRIPT not in paths:
        errors.append(
            "NO TAILNET NODE IP — %s does not write %s.\n"
            "        Effect: the node registers its PRIVATE address as InternalIP, "
            "Cilium builds the VXLAN tunnel endpoint on it, and pods on home-lab "
            "nodes are unreachable. Cluster DNS still resolves, so the node looks "
            "healthy (ADR-046 invariant 6)." % (burst_ref, NODE_IP_SCRIPT))

    # The kubelet drop-in is FORBIDDEN, which reverses the earlier rule.
    #
    # It carried After=tailscaled.service, which makes kubelet wait for
    # tailscaled. If tailscaled is slow or unhealthy kubelet never starts,
    # kubeadm join times out, and no Node registers — the server boots,
    # BootstrapReady goes true, and CAPI sits on "Waiting for a node with
    # matching ProviderID to exist". It was the only structural difference
    # between the template that joins and the four that did not.
    if NODE_IP_DROPIN in paths:
        errors.append(
            "KUBELET DROP-IN PRESENT — %s writes %s.\n        Effect: its "
            "After=tailscaled.service gates kubelet on tailscaled, and a slow or "
            "unhealthy tailscaled stops the node registering at all. Apply the "
            "node ip from postKubeadmCommands instead, where a failure costs the "
            "address rather than the node." % (burst_ref, NODE_IP_DROPIN))

    # Content of the node-ip script, needed by the providerID assertion below.
    # Bound here beside `paths`, which carries only the path strings.
    script = next((f.get("content") or ""
                   for f in (spec.get("files") or [])
                   if f.get("path") == NODE_IP_SCRIPT), "")

    post = spec.get("postKubeadmCommands") or []
    if not any("dynamic-node-ip.sh" in c for c in post):
        errors.append(
            "NODE IP NOT APPLIED AFTER JOIN — %s does not run %s in "
            "postKubeadmCommands.\n        Effect: it must run where "
            "kubeadm-flags.env already exists and tailscaled is up; anywhere "
            "earlier either does nothing or risks the join itself."
            % (burst_ref, NODE_IP_SCRIPT))

    # A preKubeadmCommand must not restart kubelet.
    #
    # It runs BEFORE kubeadm join, when kubeadm-flags.env does not exist: the
    # script exits 0 and the restart then acts on an unconfigured kubelet. A
    # non-zero exit fails the && chain, and a failed preKubeadmCommand aborts
    # cloud-init before kubeadm join — the server boots, BootstrapReady goes
    # true, and no Node ever registers. Diagnosed only from the CAPI Machine
    # condition "Waiting for a node with matching ProviderID to exist" (v4).
    #
    # It is also unnecessary: the kubelet drop-in's ExecStartPre applies the
    # flags before kubelet's first start, so the first registration already
    # carries them.
    for c in pre:
        if "restart kubelet" in c:
            errors.append(
                "KUBELET RESTART BEFORE JOIN — %s restarts kubelet in a "
                "preKubeadmCommand.\n        Effect: it runs against an "
                "unconfigured kubelet before kubeadm join; a non-zero exit "
                "aborts cloud-init and the node never registers. The drop-in's "
                "ExecStartPre already applies the flags at first start." % burst_ref)
            break

    if script and "169.254.169.254" not in script:
        errors.append(
            "providerID NOT FROM METADATA — %s does not read the instance id from "
            "the Hetzner metadata service.\n        Effect: the id must come from "
            "somewhere the node can reach without the cloud provider, or this "
            "reintroduces the dependency it exists to remove." % burst_ref)

# ── 4. the priority class value, in the THREE places it is written ───────
#
# Kyverno must set spec.priority alongside priorityClassName on sandbox pods.
# The in-tree Priority admission plugin runs BEFORE MutatingAdmissionPolicy, so
# it resolves the class present in the submitted object and never sees the one
# the policy adds; nothing re-checks the pair afterwards. Writing only the class
# name would persist burst-tenant beside priority 0 -- admitted, and scheduled at
# the wrong preemption order with nothing reporting it.
#
# Three places now, not two: the mutation writes the values, the companion
# ValidatingPolicy requires them on the final pod, and the PriorityClass defines
# them. A disagreement between the mutation and the validation is worse than
# either being wrong alone -- the platform would place a sandbox and then deny
# it, and the sandbox controller would retry forever.
#
# The policies are CEL, so the values are asserted by their presence in the
# expressions rather than by walking a patch structure. That is the drift this
# check exists to catch: the same literals in more than one file.
PRIORITYCLASS = "manifests/spoke/spoke-catalog/infra/burst-priorityclass.yaml"
POLICY        = "manifests/spoke/spoke-catalog/infra/kyverno-burst-placement.yaml"

pc = yaml.safe_load(io.open(PRIORITYCLASS, encoding="utf-8"))
pc_name, pc_value = pc["metadata"]["name"], pc["value"]

pol = [d for d in yaml.safe_load_all(io.open(POLICY, encoding="utf-8")) if d]
mut = next((d for d in pol if d.get("kind") == "MutatingPolicy"
            and d["metadata"]["name"] == "burst-placement-sandbox"), None)
val = next((d for d in pol if d.get("kind") == "ValidatingPolicy"
            and d["metadata"]["name"] == "burst-placement-authorization"), None)

# DELIVERED, not merely authored. Both policies lived in this file for months
# while no kustomization included them, so nothing applied them to a spoke and
# every assertion below passed against a file with no effect.
KUSTOMIZATION = "manifests/spoke/spoke-catalog/infra/kustomization.yaml"
kz = io.open(KUSTOMIZATION, encoding="utf-8").read()
if "kyverno-burst-placement.yaml" not in kz:
    errors.append(
        "BURST POLICIES NOT DELIVERED — %s does not list kyverno-burst-placement.yaml.\n"
        "        Effect: the mutation and its companion validation exist in source and "
        "are never applied, so burst placement is neither written nor enforced."
        % KUSTOMIZATION)

if mut is None:
    errors.append("burst-placement-sandbox MutatingPolicy not found in %s" % POLICY)
else:
    patch = " ".join(m.get("jsonPatch", {}).get("expression", "")
                     for m in mut["spec"].get("mutations") or [])
    tol = next((v["expression"] for v in mut["spec"].get("variables") or []
                if v["name"] == "burstToleration"), "")
    if pc_name not in patch:
        errors.append("mutation does not set priorityClassName=%r (the PriorityClass name)" % pc_name)
    if str(pc_value) not in patch:
        errors.append(
            "mutation does not write priority %r. The Priority admission plugin runs "
            "before the policy, so an unwritten integer persists as 0 beside class %r "
            "and the pod is scheduled at the wrong preemption order."
            % (pc_value, pc_name))
    if node_taints:
        key, rest = node_taints.split("=", 1)
        value, effect = rest.split(":", 1)
        if not (key in tol and value in tol and effect in tol):
            errors.append("KYVERNO/NODE DRIFT — mutation tolerates %r, node registers %r"
                          % (tol or "(none)", node_taints))

if val is None:
    errors.append("burst-placement-authorization ValidatingPolicy not found in %s" % POLICY)
else:
    placed = next((v["expression"] for v in val["spec"].get("variables") or []
                   if v["name"] == "placedOnBurst"), "")
    if not placed:
        errors.append("burst-placement-authorization declares no placedOnBurst variable, so "
                      "it cannot require the placement the mutation writes")
    else:
        if pc_name not in placed:
            errors.append("MUTATION/VALIDATION DRIFT — the validation does not require "
                          "priorityClassName %r" % pc_name)
        if str(pc_value) not in placed:
            errors.append("MUTATION/VALIDATION DRIFT — the validation does not require "
                          "priority %r" % pc_value)
        if node_taints and node_taints.split("=", 1)[0] not in placed:
            errors.append("MUTATION/VALIDATION DRIFT — the validation does not require the "
                          "%s toleration the mutation adds" % node_taints.split("=", 1)[0])
    # CREATE only: request.userInfo names the current caller, not the creator, so an
    # update by another controller would fail the identity test on a legitimate pod.
    ops = [o for r in val["spec"]["matchConstraints"]["resourceRules"]
           for o in r.get("operations") or []]
    if sorted(set(ops)) != ["CREATE"]:
        errors.append(
            "burst-placement-authorization matches %s. It must match CREATE only: "
            "request.userInfo identifies the current request, so on UPDATE a legitimate "
            "sandbox pod would enter the ordinary-caller branch and be refused for "
            "carrying the placement the platform gave it." % sorted(set(ops)))

if errors:
    for e in errors:
        print("  FAIL: %s" % e)
    sys.exit(1)

print("  burst identity agrees across bootstrap, autoscaler, operator, Kyverno and node IP")
print("    bootstrap: %s" % burst_ref)
print("    taint : %s" % node_taints)
print("    labels: %s" % node_labels)
PY
    ) || { echo "$out"; hard_fail "burst identity drift (ADR-052 D4) — see FAIL lines above"; return 0; }

    echo "$out"
    pass "burst node identity consistent"
}
