#!/usr/bin/env bash

# The Support Agent is one container, and holds no ServiceAccount token.
#
# ADR-077 addendum 4 decision 9. Every seam in this component is a place its
# trustworthiness has to be argued again: a sidecar has its own image, its own
# provenance and its own egress, and none of it appears in the allowlist a tenant
# reviews. One container means one thing to read.
#
# `automountServiceAccountToken: false` is the part most likely to be missed,
# and it is not decorative. Telemeter's ServiceAccount token IS its read
# credential -- assets/telemeter-client/deployment.yaml passes
# --from-token-file=/var/run/secrets/kubernetes.io/serviceaccount/token to reach
# the cluster's Prometheus. This agent has no ClusterRole (gate 80), so a mounted
# token grants it nothing today. It is one ClusterRole away from granting
# everything, and the mount is the half a review does not look at.
#
# Checked against the manifests rather than the cluster: the claim is about what
# the platform ships to every box, not about what one hub happens to run.

validate_support_agent_single_container() {
    section "The Support Agent is one container with no token (ADR-077)"

    platform_source_available "manifests/hub-core-services/support-agent" "manifests/spoke/spoke-catalog/infra/support-agent.yaml" || return 0

    local hub="$VALIDATE_ROOT/manifests/hub-core-services/support-agent"
    local spoke="$VALIDATE_ROOT/manifests/spoke/spoke-catalog/infra/support-agent.yaml"

    [ -d "$hub" ]   || { hard_fail "no support-agent component at $hub"; return 0; }
    [ -f "$spoke" ] || { hard_fail "no spoke support agent at $spoke"; return 0; }

    local checker
    checker=$(cat <<'PY'
import sys, os, yaml

def docs(path):
    with open(path) as fh:
        for d in yaml.safe_load_all(fh):
            if isinstance(d, dict):
                yield path, d

def walk(target):
    if os.path.isdir(target):
        for name in sorted(os.listdir(target)):
            if name.endswith((".yaml", ".yml")):
                yield from docs(os.path.join(target, name))
    else:
        yield from docs(target)

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

        containers = spec.get("containers") or []
        if len(containers) != 1:
            names = [c.get("name", "?") for c in containers]
            print(f"PROBLEM={where} declares {len(containers)} containers ({names}); "
                  f"ADR-077 allows one -- every extra is an image whose provenance "
                  f"and egress no allowlist describes")

        if spec.get("initContainers"):
            names = [c.get("name", "?") for c in spec["initContainers"]]
            print(f"PROBLEM={where} declares initContainers {names}; an init container "
                  f"is where 'wait for enrolment' gets added, which would make the pod's "
                  f"start depend on the commercial relationship")

        if spec.get("automountServiceAccountToken") is not False:
            print(f"PROBLEM={where} does not set automountServiceAccountToken: false; "
                  f"the agent reads metrics endpoints over HTTP and the Kubernetes API "
                  f"never, so a projected token is a credential with no consumer")

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

    local count
    count=$(sed -n 's/^DEPLOYMENTS=//p' <<<"$report")
    if [[ "$count" == "0" ]]; then
        hard_fail "no support-agent Deployment found in either manifest set; this check inspected nothing"
        return 0
    fi

    (( failed )) && return 0
    pass "$count support-agent Deployment(s): one container, no init containers, no token mounted"
}
