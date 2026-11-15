#!/usr/bin/env bash

# Failing to report must not affect anything else.
#
# ADR-077 states this as the most important runtime invariant after the RBAC one,
# and names the ways it erodes: "a readiness probe wired to upload success, a
# sidecar that blocks a pod's start, an init container that waits for enrolment,
# a controller that treats a transmission error as a reconcile failure."
#
# Each of those is a small change that makes the platform's OPERATION depend on
# the platform's COMMERCIAL RELATIONSHIP, which is the thing this architecture
# exists not to do. None of them would be described that way in the commit that
# added it.
#
# The agent therefore declares no probe at all. That is stronger than "no probe
# wired to upload success" and it is checkable without reading the probe's
# meaning: an agent with no probe cannot have one that reports the support
# endpoint's health as the pod's health. It costs nothing -- the agent serves no
# traffic, so nothing needs to know when it is ready.

validate_support_agent_degrades_alone() {
    section "The Support Agent degrades alone (ADR-077)"

    platform_source_available "manifests/hub-core-services/support-agent" "manifests/spoke/spoke-catalog/infra/support-agent.yaml" || return 0

    local hub="$VALIDATE_ROOT/manifests/hub-core-services/support-agent"
    local spoke="$VALIDATE_ROOT/manifests/spoke/spoke-catalog/infra/support-agent.yaml"

    [ -d "$hub" ]   || { hard_fail "no support-agent component at $hub"; return 0; }
    [ -f "$spoke" ] || { hard_fail "no spoke support agent at $spoke"; return 0; }

    local checker
    checker=$(cat <<'PY'
import sys, os, yaml

PROBES = ("readinessProbe", "livenessProbe", "startupProbe")

def walk(target):
    targets = ([os.path.join(target, n) for n in sorted(os.listdir(target))
                if n.endswith((".yaml", ".yml"))] if os.path.isdir(target) else [target])
    for path in targets:
        with open(path) as fh:
            for d in yaml.safe_load_all(fh):
                if isinstance(d, dict):
                    yield path, d

checked = 0
for target in sys.argv[1:]:
    for path, d in walk(target):
        if d.get("kind") != "Deployment":
            continue
        name = (d.get("metadata") or {}).get("name", "")
        if "support-agent" not in name:
            continue
        checked += 1
        spec = ((d.get("spec") or {}).get("template") or {}).get("spec") or {}
        where = f"{os.path.basename(path)}:{name}"
        for c in spec.get("containers") or []:
            for probe in PROBES:
                if c.get(probe):
                    print(f"PROBLEM={where} container {c.get('name','?')!r} declares a "
                          f"{probe}; the agent serves no traffic, so the only thing a probe "
                          f"here can report is whether reporting is working -- which is "
                          f"exactly what must not become the pod's health")

print(f"DEPLOYMENTS={checked}")
PY
)

    local report
    report=$(python3 -c "$checker" "$hub" "$spoke") \
        || { hard_fail "the support-agent manifests could not be inspected"; return 0; }

    local failed=0 line
    while IFS= read -r line; do
        [[ -z "$line" ]] && continue
        hard_fail "${line#PROBLEM=}"
        failed=1
    done < <(grep '^PROBLEM=' <<<"$report")

    # No other WORKLOAD may reference the agent.
    #
    # Narrowed deliberately from "no file mentions support-agent", which was the
    # first form and was wrong: the ApplicationSet that deploys it, the values
    # template carrying its capability toggle, the kustomization listing it and
    # the architecture registry describing it all name it legitimately. A file
    # mentioning the agent is not a dependency on it.
    #
    # What would be a dependency is another POD referencing it -- an init
    # container waiting for it, a probe reaching it, a sidecar injected beside
    # it. The agent has no Service and no endpoint (gate 83), so any pod naming
    # it is either coupling that must not exist or a reference that cannot
    # resolve.
    local coupling
    coupling=$(python3 - "$VALIDATE_ROOT" <<'PY2'
import os, sys, yaml

root = sys.argv[1]
WORKLOADS = {"Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob", "Pod"}
OWN = ("/support-agent/", "spoke-catalog/infra/support-agent.yaml")

for dirpath, _, names in os.walk(os.path.join(root, "manifests")):
    for name in names:
        if not name.endswith((".yaml", ".yml")):
            continue
        path = os.path.join(dirpath, name)
        rel = os.path.relpath(path, root)
        if any(o.strip("/") in rel for o in OWN):
            continue
        try:
            with open(path) as fh:
                docs = list(yaml.safe_load_all(fh))
        except Exception:
            continue  # templated or non-YAML; not this check's business
        for d in docs:
            if not isinstance(d, dict) or d.get("kind") not in WORKLOADS:
                continue
            spec = ((d.get("spec") or {}).get("template") or {}).get("spec") or {}
            if "support-agent" in yaml.safe_dump(spec):
                print(f"{rel}:{(d.get('metadata') or {}).get('name','?')}")
PY2
) || true

    if [[ -n "$coupling" ]]; then
        while IFS= read -r line; do
            [[ -z "$line" ]] && continue
            hard_fail "$line references support-agent in a pod spec; nothing may depend on a component whose failure must change nothing"
            failed=1
        done <<<"$coupling"
    fi

    local count
    count=$(sed -n 's/^DEPLOYMENTS=//p' <<<"$report")
    if [[ "$count" == "0" ]]; then
        hard_fail "no support-agent Deployment found; this check inspected nothing"
        return 0
    fi

    (( failed )) && return 0
    pass "$count support-agent Deployment(s): no probes, and nothing else depends on them"
}
