#!/usr/bin/env bash
# ADR-082 amendment 2026-11-09: a workload cluster exists because its claim does.
#
# The cluster's two Applications are GENERATED from
# registry/clusters/*/infrastructure/spokepool.yaml by two ApplicationSets in the
# management cluster's directory, so the commit that removes a claim is what
# deletes the cluster. Three things have to be true on the box for that to hold,
# and each one fails silently.
#
# THE SETS HAVE TO BE THERE. Without them a claim declares nothing: the cluster
# keeps running on Applications nobody generates, and removing the claim does
# nothing at all. The repository would say a cluster was gone while it ran.
#
# EVERY GENERATED APPLICATION HAS TO BE OWNED BY ITS SET. An Application with no
# controller owner is one the sets are not managing -- a leftover from the layout
# where these were flat files, or one created by hand. The set does adopt an
# unowned Application of the same name: Argo CD's ApplicationSet controller calls
# SetControllerReference on the object it found (argo-cd v3.5.7,
# applicationset/controllers/applicationset_controller.go, createOrUpdateInCluster),
# which succeeds when nothing else owns it and errors when something does. So an
# unowned one here means the set has not reconciled it yet, or cannot -- and until
# it does, removing the claim leaves that Application, and the cluster, running.
#
# THE FINALIZERS HAVE TO BE ASYMMETRIC, because that asymmetry IS the deletion
# contract. preserveResourcesOnDeletion decides it: false gives the generated
# Application Argo CD's resources-finalizer, so deleting it cascades to the claim
# and the cluster is unwound; true withholds the finalizer, so the Application can
# go without a cascade. The infrastructure set must be the former and the
# generated set the latter -- its contents are addressed to the cluster being
# destroyed, and a cascade there blocks on an API server that is already gone.
# Inverted, the two failures are opposite and both quiet: an infrastructure
# Application with no finalizer is deleted leaving the claim, the cluster and its
# servers behind, and a generated Application with one makes every removal wait
# out a cascade against a dying cluster.
#
# WHAT THIS DOES NOT ESTABLISH. It says the generation and ownership are in place
# right now. It does not say a removal was tried, that Crossplane and CAPI
# completed one, or that the provider has nothing left -- `kakt tenant
# cluster-status` answers that, per cluster, by observation.
#
# Reported as soft failures: they fail a final assessment, not an in-flight
# bootstrap gate on a box whose ApplicationSet controller has not reconciled yet.

validate_adr082_workload_cluster_lifecycle() {
    section "ADR-082: a workload cluster's Applications are generated from its claim"

    local mgmt sets_missing=0 set
    mgmt="$(kc get application -n platform-ops -o name | sed -n 's|^application.argoproj.io/\(.*\)-root$|\1|p' | head -1)"
    if [[ -z "$mgmt" ]]; then
        note "no <cluster>-root Application, so this box's management cluster cannot be named from the cluster; skipping"
        return 0
    fi

    for set in "${mgmt}-workload-clusters" "${mgmt}-workload-generated"; do
        if kc get applicationset "$set" -n platform-ops >/dev/null 2>&1; then
            pass "$set exists"
        else
            soft_fail "$set is missing: a claim file declares nothing without it, and removing a claim would delete no cluster"
            sets_missing=1
        fi
    done
    [[ "$sets_missing" -eq 1 ]] && return 0

    local out
    out=$(HUB_KUBECONFIG="$HUB_KUBECONFIG" MGMT="$mgmt" python3 - <<'PYCHECK'
import json, os, subprocess

KC = ["kubectl", "--kubeconfig", os.environ["HUB_KUBECONFIG"], "-n", "platform-ops"]
MGMT = os.environ["MGMT"]
FINALIZER = "resources-finalizer.argocd.argoproj.io"
OWNER = {"infrastructure": MGMT + "-workload-clusters", "generated": MGMT + "-workload-generated"}

r = subprocess.run(KC + ["get", "application", "-o", "json"], capture_output=True, text=True)
if r.returncode != 0:
    print("FAIL platform-ops cannot be read, so generation and ownership are unknown")
    raise SystemExit

apps = json.loads(r.stdout).get("items", [])
seen = 0
for app in apps:
    meta = app["metadata"]
    name = meta["name"]
    kind = next((k for k in OWNER if name.endswith("-" + k)), None)
    if kind is None or name == MGMT + "-generated":
        continue  # the management cluster's own, and anything else in platform-ops
    cluster = name[: -len(kind) - 1]
    if cluster == MGMT:
        continue
    seen += 1

    owners = [o for o in meta.get("ownerReferences", []) if o.get("kind") == "ApplicationSet"]
    if not owners:
        print("FAIL %s has no ApplicationSet owner: it is not generated from %s's claim, "
              "so removing that claim would leave it -- and the cluster -- running"
              % (name, cluster))
    elif owners[0].get("name") != OWNER[kind]:
        print("FAIL %s is owned by %s, not %s" % (name, owners[0].get("name"), OWNER[kind]))
    else:
        print("PASS %s is generated and owned by %s" % (name, OWNER[kind]))

    has = FINALIZER in (meta.get("finalizers") or [])
    if kind == "infrastructure" and not has:
        print("FAIL %s carries no resources-finalizer: deleting it would leave the claim, "
              "the cluster and its servers behind" % name)
    if kind == "generated" and has:
        print("FAIL %s carries the resources-finalizer: every removal would wait out a "
              "cascade against the cluster being destroyed" % name)

if seen == 0:
    print("NOTE this box declares no workload cluster, so there is nothing to generate")
PYCHECK
) || true

    if [[ -z "$out" ]]; then
        soft_fail "ADR-082's lifecycle check produced no result, so whether a claim still declares its cluster is unknown"
        return 0
    fi

    local line
    while IFS= read -r line; do
        case "$line" in
            PASS\ *) pass "${line#PASS }" ;;
            NOTE\ *) note "${line#NOTE }" ;;
            FAIL\ *) soft_fail "${line#FAIL }" ;;
        esac
    done <<< "$out"
}
