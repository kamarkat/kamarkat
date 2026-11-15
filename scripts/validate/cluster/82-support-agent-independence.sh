#!/usr/bin/env bash
# The Support Agent must not read the observability backend.
#
# ADR-078 §8: shared platform evidence sources, independent collection and
# export paths. The agent's collectors name component metrics endpoints
# directly -- the same /metrics surfaces Alloy scrapes -- and never the store
# Alloy writes to.
#
# Gated rather than reviewed, because the coupling is the convenient thing to
# introduce. Once a metrics store exists in the box, pointing the agent at it is
# one line and looks like a simplification. It is not:
#
#   * support telemetry would then depend on a component the tenant may scale
#     down, so a box with observability disabled becomes an unsupported box --
#     the licence check ADR-077 exists to prevent, arriving by a different door;
#   * the store holds whatever the tenant opted in under ADR-078 §7, so the
#     allowlist would be filtering an unbounded payload instead of bounding a
#     known one, which is a preference and not a boundary.
#
# Checked against the shipped allowlist, which is the contract, by a positive
# test: a collector's query must be a bare series name and no collector may name
# the namespace that holds the query surface. See the checker below for why that
# replaced a list of product names.
#
# A module, not a standalone script. run.sh SOURCES each file and then calls the
# validate_* functions it defined, so top-level code here ran in the runner's own
# shell. Three things followed from that, and all three were live:
#
#   * `$1` read the RUNNER's first argument, which is --mode=final. This looked
#     for its allowlist at "--mode=final/manifests/.../allowlist.yaml" and
#     reported it missing -- while the file sat in the repository, present and
#     correct.
#   * a top-level `exit 1` ended the whole validation run, not one check, so a
#     single failure here took every check after it with it.
#   * `set -euo pipefail` changed the runner's shell options for everything that
#     sourced afterwards.
validate_support_agent_independence() {
    section "The Support Agent collects from components, not the observability store (ADR-078 §8)"

    # ADR-077 puts one agent on every cluster, so there are two allowlists: the
    # hub's component and the spoke catalogue's. Both are checked, and they are
    # checked against each other -- a spoke reporting under a different contract
    # would make "the allowlist is what leaves" true of only half the fleet, and
    # nothing else in the repository compares them.
    platform_source_available "manifests/hub-core-services/support-agent/allowlist.yaml" \
        "manifests/spoke/spoke-catalog/infra/support-agent.yaml" || return 0

    local hub="$VALIDATE_ROOT/manifests/hub-core-services/support-agent/allowlist.yaml"
    local spoke="$VALIDATE_ROOT/manifests/spoke/spoke-catalog/infra/support-agent.yaml"

    [ -f "$hub" ]   || { hard_fail "no hub allowlist at $hub"; return 0; }
    [ -f "$spoke" ] || { hard_fail "no spoke agent at $spoke"; return 0; }

    local extract
    extract=$(cat <<'PY'
import sys, yaml
for doc in yaml.safe_load_all(open(sys.argv[1])):
    if isinstance(doc, dict) and doc.get("kind") == "ConfigMap" \
            and "allowlist.yaml" in (doc.get("data") or {}):
        sys.stdout.write(doc["data"]["allowlist.yaml"])
        sys.exit(0)
sys.exit(f"no allowlist ConfigMap in {sys.argv[1]}")
PY
)

    local hub_raw spoke_raw
    hub_raw=$(python3 -c "$extract" "$hub") \
        || { hard_fail "the hub allowlist could not be read from $hub"; return 0; }
    spoke_raw=$(python3 -c "$extract" "$spoke") \
        || { hard_fail "the spoke agent's allowlist could not be read from $spoke"; return 0; }

    # The spoke's allowlist may be a SUBSET of the hub's -- a spoke has no
    # database to back up -- but never a superset, and never a different emit set
    # for a collector both carry. Either would mean a field leaves a spoke that a
    # review of the hub's contract would not have shown.
    local contract
    contract=$(python3 - "$hub_raw" "$spoke_raw" <<'PY'
import sys, yaml
hub = {c["collector"]: c for c in yaml.safe_load(sys.argv[1]) or []}
spoke = {c["collector"]: c for c in yaml.safe_load(sys.argv[2]) or []}
for name, c in spoke.items():
    h = hub.get(name)
    if h is None:
        print(f"PROBLEM=spoke collector {name!r} is in no hub allowlist")
        continue
    extra = set(c.get("emit") or []) - set(h.get("emit") or [])
    if extra:
        print(f"PROBLEM=spoke collector {name!r} emits {sorted(extra)}, which the hub's does not")
    if (c.get("source") or {}).get("query") != (h.get("source") or {}).get("query"):
        print(f"PROBLEM=spoke collector {name!r} queries something different from the hub's")
PY
) || { hard_fail "the hub and spoke allowlists could not be compared"; return 0; }

    local failed=0 line
    while IFS= read -r line; do
        [[ -z "$line" ]] && continue
        hard_fail "${line#PROBLEM=}"
        failed=1
    done < <(grep '^PROBLEM=' <<<"$contract")

    local raw
    raw=$(printf '%s\n%s\n' "$hub_raw" "$spoke_raw")

    # python3 -c with the YAML on stdin. `python3 - <<'PY' <<<"$raw"` gives stdin
    # to the LAST redirection, so python would read the YAML as its own program --
    # the way check 81 silently could never run.
    local checker
    checker=$(cat <<'PY'
import sys, re, yaml

# A POSITIVE test, replacing a list of vendor product names.
#
# The old form matched "victoriametrics", "vmalert", "loki", "thanos" and so on
# as substrings. Two things were wrong with it: it had to anticipate every
# product a store might ever be, and it silently permitted the one nobody
# thought of. Neither is a property you want in the check that holds a boundary.
#
# What ADR-078 §8 actually forbids is reaching a QUERY SURFACE. Two rules say
# that without naming anyone's product:
#
#   1. a collector's query is a bare series name. ADR-077 already requires this
#      -- "collectors run defined queries; they never forward what a response
#      happened to contain" -- and a query API call cannot be written as a bare
#      series name, so the rule enforces the decision rather than restating it.
#   2. no collector names platform-observability, the one namespace in this
#      platform that holds a store and its query surface.
#
# Rule 1 is the load-bearing one. VMSingle serves /metrics and /api/v1/query on
# the same port, so no network policy can separate them and no namespace check
# can either once a collector is inside. What distinguishes reading a component
# from querying a store is the SHAPE OF THE REQUEST, which is what this reads.

SERIES = re.compile(r"^[A-Za-z_:][A-Za-z0-9_:]*$")
QUERY_SURFACE_NS = "platform-observability"

count = 0
for c in yaml.safe_load(sys.stdin) or []:
    count += 1
    name = c.get("collector", "?")
    src = c.get("source") or {}
    kind = src.get("kind", "")
    ns = str(src.get("namespace", ""))
    query = str(src.get("query", ""))

    if ns == QUERY_SURFACE_NS:
        print(f"PROBLEM=collector {name!r} names the {QUERY_SURFACE_NS!r} namespace; "
              f"ADR-078 §8 keeps the paths independent -- collect from the component's "
              f"own /metrics, which is the source the store also reads")

    if kind == "metrics":
        if not query:
            print(f"PROBLEM=collector {name!r} is kind: metrics with no query; a "
                  f"collector with no defined query has no known result schema, so "
                  f"its emit list bounds nothing")
        elif not SERIES.match(query):
            print(f"PROBLEM=collector {name!r} queries {query!r}, which is not a bare "
                  f"series name. ADR-077 requires a predefined query whose result "
                  f"schema is known in advance; an expression, path or selector here "
                  f"means the payload's shape is whatever the cluster happens to "
                  f"contain, and the allowlist becomes a preference rather than a bound")

print(f"COLLECTORS={count}")
PY
)

    local report
    report=$(python3 -c "$checker" <<<"$raw") \
        || { hard_fail "the support-agent allowlists could not be inspected"; return 0; }

    while IFS= read -r line; do
        [[ -z "$line" ]] && continue
        hard_fail "${line#PROBLEM=}"
        failed=1
    done < <(grep '^PROBLEM=' <<<"$report")

    (( failed )) && return 0
    pass "$(sed -n 's/^COLLECTORS=//p' <<<"$report") collector(s): every query is a bare series name, none names the query surface, and the spoke's contract matches the hub's"
}
