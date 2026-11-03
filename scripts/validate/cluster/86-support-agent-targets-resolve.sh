#!/usr/bin/env bash

# Every collector's target exists.
#
# ADR-077 addendum 4 decision 13 exists because of a live defect this would have
# caught on the day it was written: the `node-pressure` collector declared
#
#     source: {kind: metrics, namespace: kube-system, service: metrics-server,
#              query: kube_node_status_condition}
#
# and kube_node_status_condition is a kube-state-metrics series. metrics-server's
# own /metrics describes metrics-server. So the one collector ADR-067 names for
# an upgrade pre-flight verdict returned nothing on every box, and said so only
# in the payload's `Skipped` map, which nobody reads until a support case needs
# the evidence that is not there.
#
# A collector that silently collects nothing is worse than one that fails: the
# payload still arrives, still looks complete, and the gap appears as "this box
# has no node pressure" rather than as an error.
#
# This runs against the live cluster because the targets are Services from
# upstream charts, not manifests in this repository -- there is nothing static to
# resolve them against. A non-empty Skipped map at runtime is an ALERT, never a
# release failure (a collector may be legitimately absent on a spoke); this gate
# is the release-time half, and it asks only whether the target exists at all.

validate_support_agent_targets_resolve() {
    section "Every Support Agent collector names a target that exists (ADR-077)"

    local hub="$VALIDATE_ROOT/manifests/hub-core-services/support-agent/allowlist.yaml"
    [ -f "$hub" ] || { hard_fail "no hub allowlist at $hub"; return 0; }

    if ! kc get serviceaccount support-agent -n platform-ops >/dev/null 2>&1; then
        note "support-agent not installed on this hub; targets cannot be resolved"
        return 0
    fi

    local extract
    extract=$(cat <<'PY'
import sys, yaml
for doc in yaml.safe_load_all(open(sys.argv[1])):
    if isinstance(doc, dict) and doc.get("kind") == "ConfigMap" \
            and "allowlist.yaml" in (doc.get("data") or {}):
        for c in yaml.safe_load(doc["data"]["allowlist.yaml"]) or []:
            src = c.get("source") or {}
            print("\t".join([
                c.get("collector", "?"),
                src.get("kind", "?"),
                src.get("namespace", ""),
                src.get("service", ""),
                str(src.get("port", "")),
                src.get("path", ""),
            ]))
        sys.exit(0)
sys.exit(f"no allowlist ConfigMap in {sys.argv[1]}")
PY
)

    local rows
    rows=$(python3 -c "$extract" "$hub") \
        || { hard_fail "the hub allowlist could not be read from $hub"; return 0; }

    local failed=0 total=0 name kind ns svc port path
    while IFS=$'\t' read -r name kind ns svc port path; do
        [[ -z "$name" ]] && continue
        (( total++ ))
        case "$kind" in
            metrics)
                if [[ -z "$ns" || -z "$svc" ]]; then
                    hard_fail "collector '$name' is kind: metrics with no namespace or service"
                    failed=1
                elif ! kc get service "$svc" -n "$ns" >/dev/null 2>&1; then
                    hard_fail "collector '$name' names Service $ns/$svc, which does not exist on this hub"
                    note "ADR-077: a collector whose target is absent reports nothing and says so"
                    note "only in the payload's Skipped map. Either the Service moved, or the"
                    note "collector was written against the wrong component."
                    failed=1
                elif [[ -z "$port" || "$port" == "0" ]]; then
                    hard_fail "collector '$name' is kind: metrics with no port"
                    failed=1
                elif ! kc get service "$svc" -n "$ns" \
                        -o jsonpath='{.spec.ports[*].port}' 2>/dev/null \
                        | tr ' ' '\n' | grep -qx "$port"; then
                    hard_fail "collector '$name' scrapes $ns/$svc:$port, which that Service does not serve"
                    note "A Service that exists on a different port fails exactly like an absent"
                    note "one -- a refused scrape, recorded only in the payload's Skipped map."
                    note "Exporters do not agree on a port: kube-state-metrics serves 8080 and a"
                    note "CloudNativePG instance serves 9187."
                    failed=1
                fi
                ;;
            artefact)
                # shellcheck disable=SC2086
                if [[ -z "$path" ]]; then
                    hard_fail "collector '$name' is kind: artefact with no path"
                    failed=1
                elif ! compgen -G "$VALIDATE_ROOT/$path" >/dev/null; then
                    hard_fail "collector '$name' reads '$path', which matches no file in this repository"
                    failed=1
                fi
                ;;
            *)
                hard_fail "collector '$name' has source kind '$kind', which is neither metrics nor artefact"
                failed=1
                ;;
        esac
    done <<<"$rows"

    (( failed )) && return 0
    pass "$total collector target(s) resolve"
}
