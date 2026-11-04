#!/usr/bin/env bash
# ADR-087 — Workload Configuration and Secrets: every STATIC check, in one file.
#
# One file per ADR; the number is the ADR's.
#
#   ls scripts/validate/*/087-*        every check for this ADR
#   head -1 <file>                     the ADR a check belongs to
#
# The live counterpart -- "is every declared secret actually present in the
# provider" -- is in cluster/087-workload-configuration-and-secrets.sh, because
# it needs a credential and a reachable Infisical. What can be decided from the
# repository alone is decided here.


# ──────────────────────────────────────────────────────────────────────────
# The declaration itself must be well-formed
# ──────────────────────────────────────────────────────────────────────────
# ADR-087 makes the fleet's values.yaml the single statement of what its
# workloads read. That only holds if the statement is complete, so the three
# things the ADR requires of every entry are asserted here rather than left to
# the Helm template -- which catches them too, but only when something renders,
# and by then the fleet is mid-sync.
#
# What is NOT checked here is presence of the value. A declared secret whose
# value is absent is a live-cluster fact and belongs in the cluster module; a
# declaration that could never work is a repository fact and belongs here.
validate_adr087_fleet_declarations() {
    # Scoped to trees this platform OWNS. reference-projects/ and
    # docs/inspirations/ hold other projects' gitops templates whose values.yaml
    # carry Go placeholders and are not valid YAML by design -- reporting them
    # is 90 failures about files no one here can fix, which buries the one
    # finding that matters.
    local f found=0
    for f in $(find . \
            \( -name node_modules -o -name reference-projects -o -name inspirations -o -name archived \) -prune \
            -o -path '*/environments/*/values.yaml' -print 2>/dev/null); do
        found=1
        local findings rc
        findings="$(python3 - "$f" <<'PYEOF'
import sys, yaml

path = sys.argv[1]
try:
    d = yaml.safe_load(open(path)) or {}
except Exception as e:
    print(f"is not parseable YAML: {e}")
    sys.exit(0)

# A fleet that declares no workloads has nothing to check.
if not isinstance(d, dict):
    sys.exit(0)

if "externalSecrets" in d:
    print("declares `externalSecrets`, which ADR-087 removed. It delivered one "
          "ExternalSecret per FLEET, so any one absent key withheld every other "
          "key from every workload. Use `config:` for non-secret values and "
          "`secrets:` for secret ones")

cfg = d.get("config")
if cfg is not None and not isinstance(cfg, dict):
    print(f"`config` is {type(cfg).__name__}, not a mapping of KEY to value")

secrets = d.get("secrets")
if secrets is None:
    sys.exit(0)
if not isinstance(secrets, list):
    print(f"`secrets` is {type(secrets).__name__}, not a list")
    sys.exit(0)

seen = {}
for i, s in enumerate(secrets):
    if not isinstance(s, dict):
        print(f"secrets[{i}] is not a mapping")
        continue
    name = s.get("name")
    if not name:
        print(f"secrets[{i}] declares no name")
        continue
    if not s.get("capability"):
        print(f"secret {name!r} declares no capability -- the key name alone is "
              "not something an operator can act on, and the capability is what "
              "the live check reports when the value is absent")
    wl = s.get("workloads")
    if not wl:
        print(f"secret {name!r} declares no workloads -- it would be delivered "
              "nowhere, and an undelivered secret is indistinguishable from an "
              "absent one")
    elif not isinstance(wl, list):
        print(f"secret {name!r} has workloads that are not a list")
    if name in seen:
        print(f"secret {name!r} is declared twice (entries {seen[name]} and {i})")
    seen[name] = i

    # A fleet cannot supply what the issuer mints. Declaring it here asserts
    # otherwise, and then fails the live check for a key the tenant was never
    # able to provide.
    if isinstance(name, str) and name.startswith("OAUTH_") and name.endswith(("_CLIENT_ID", "_CLIENT_SECRET")):
        print(f"secret {name!r} is platform-generated, not tenant-supplied. "
              "Declaring the confidential OAuth client under `oauth.clients` is "
              "what creates AND delivers it; listing it here claims the fleet "
              "supplies a value the issuer mints")
    if name in ("CACHE_PASSWORD", "OWNER_INITIAL_PASSWORD"):
        print(f"secret {name!r} is platform-generated. It is created by declaring "
              "the capability that needs it, not by listing it here")
PYEOF
)"
        rc=$?
        if [[ $rc -ne 0 ]]; then
            hard_fail "$f: the declaration checker exited $rc without completing -- ${findings:-no output}"
            continue
        fi
        if [[ -z "$findings" ]]; then
            pass "$f: fleet config/secret declarations are well-formed"
        else
            local line
            while IFS= read -r line; do
                [[ -n "$line" ]] && hard_fail "$f: $line"
            done <<< "$findings"
        fi
    done

    if (( found == 0 )); then
        pass "no fleet values.yaml in this tree (nothing declares workloads here)"
    fi
}


# ──────────────────────────────────────────────────────────────────────────
# The chart must still render one ExternalSecret PER WORKLOAD
# ──────────────────────────────────────────────────────────────────────────
# The grouping is the whole point of ADR-087, and it is the property most easily
# lost by a well-meaning simplification: merging the per-workload objects back
# into one is fewer resources, reads more tidily, and silently restores the
# failure coupling the ADR removed. Nothing about the merged form looks wrong.
validate_adr087_secrets_are_grouped_per_workload() {
    local chart="manifests/tenants/charts/universal-tenant"
    [[ -d "$chart" ]] || { pass "universal-tenant not in this tree"; return; }

    local out
    out="$(helm template ut "$chart" \
        --set deployXR=false --set tenantId=t --set appId=a --set clusterName=c \
        --set oidcIssuer=https://x --set oidcJwksUrl=https://x/k \
        --set 'secrets[0].name=A' --set 'secrets[0].capability=cap-a' --set 'secrets[0].workloads[0]=one' \
        --set 'secrets[1].name=B' --set 'secrets[1].capability=cap-b' --set 'secrets[1].workloads[0]=two' \
        2>&1)" || { hard_fail "universal-tenant failed to render with two declared secrets: ${out}"; return; }

    # The rendered manifest reaches python on STDIN and the script comes from a
    # file. Passing both as heredocs gives python the manifest as its source
    # text, which fails on the first YAML line and reports the grouping as wrong
    # without ever having looked at it.
    local script names
    script="$(mktemp)"
    cat > "$script" <<'PYEOF'
import sys, yaml
got = []
for d in yaml.safe_load_all(sys.stdin):
    if d and d.get("kind") == "ExternalSecret" and d["metadata"]["name"].endswith("-secrets"):
        got.append(d["metadata"]["name"])
print(" ".join(sorted(got)))
PYEOF
    names="$(printf '%s' "$out" | python3 "$script")"
    rm -f "$script"
    # <app>-<workload>-secrets. The app segment is not decoration: two products
    # of one customer each have a `bff`, and a name built from the tenant alone
    # would be one object claimed by two workloads. The tenant segment was dropped
    # in b6ee1dba (ADR-088, "only cluster-scoped names carry the scope"): the
    # namespace, tenant-<tenant>-<app>, already carries it.
    # COMPARED AFTER NORMALISING WHITESPACE, and the failure prints both sides
    # delimited.
    #
    # On 2026-10-04 this failed with "rendered as: a-one-secrets a-two-secrets".
    # The rendered names were RIGHT; the comparison still expected the pre-b6ee1dba
    # "t-a-one-secrets t-a-two-secrets", and the old message printed only one side.
    # Correcting the expected value fixed it; nothing invisible was involved. Both
    # sides are now printed, delimited, so a stale expectation reads as one.
    local want="a-one-secrets a-two-secrets"
    local got_n want_n
    got_n="$(printf '%s' "$names" | tr -d '\r' | tr -s '[:space:]' ' ' | sed 's/^ *//; s/ *$//')"
    want_n="$want"
    if [[ "$got_n" == "$want_n" ]]; then
        pass "two workloads' secrets render as two ExternalSecrets, not one"
    else
        hard_fail "secrets for two workloads rendered as [${names:-<none>}] but ADR-087 expects [${want}] — one ExternalSecret per workload, because the object is atomic and its key set is the blast radius of any one key being wrong"
        note "normalised: got [${got_n:-<none>}] want [${want_n}]"
        note "bytes got:  $(printf '%s' "$names" | od -c | head -3 | tr '\n' ' ')"
    fi
}
