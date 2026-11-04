#!/usr/bin/env bash
# Every tenant's OAuth clients exist IN THE ISSUER (ADR-060).
#
# This replaces the hydra-maester gate. That one read a CR's status, because a
# controller reconciled clients into Hydra and could fail silently. Zitadel has
# no such controller: the Tenant Identity Service creates clients through the
# issuer's API. So the failure mode moved rather than disappearing — the service
# can report success and publish a client id whose application was later deleted,
# renamed, or created in the wrong organisation, and nothing in the cluster goes
# unhealthy. The only symptom is a login that fails in the browser.
#
# So the assertion is deliberately end-to-end and against the issuer itself:
#
#   Infisical holds a client id for the tenant   (the platform published it)
#     ∧ Zitadel holds an application with that exact client id
#     ∧ it lives in the tenant's own organisation
#
# Credentials come from the Infisical API rather than from the projected Secret,
# and that is the point: reading the Secret would only prove ESO copied
# something. Reading the source of truth proves the value the platform published
# is the value the issuer knows, which is what a login actually depends on.
#
# Infisical connection details come from the ADR-045 bootstrap-generated
# artifacts committed in Git — the same values the platform itself runs on, so
# this cannot drift from what it is validating.

# Local port for a port-forward, echoed on stdout. Ports are chosen by the
# kernel (":0") because a fixed one collides with whatever else the operator is
# running, and the failure then looks like an unreachable service.
_oauth_pf_start() {
    local ns="$1" svc="$2" port="$3" logf pf_pid local_port i
    logf="$(mktemp)"
    kubectl --kubeconfig="$HUB_KUBECONFIG" port-forward -n "$ns" "svc/$svc" ":$port" \
        --address 127.0.0.1 >"$logf" 2>&1 &
    pf_pid=$!
    for i in $(seq 1 40); do
        local_port="$(sed -nE 's|^Forwarding from 127\.0\.0\.1:([0-9]+).*|\1|p' "$logf" | head -1)"
        [[ -n "$local_port" ]] && break
        kill -0 "$pf_pid" 2>/dev/null || break
        sleep 0.25
    done
    rm -f "$logf"
    if [[ -z "$local_port" ]]; then
        kill "$pf_pid" 2>/dev/null || true
        return 1
    fi
    echo "$pf_pid $local_port"
}

validate_oauth_clients() {
    section "OAuth client registration (Zitadel)"
    # Every hostname below derives from the zone; without it there is nothing
    # to check against, and the platform's own zone is not a substitute.
    require_zone || return 0

    # Infisical's coordinates, read from the ConfigMap the platform itself reads.
    #
    # This read two Kustomize patch files --
    # hub-core-services/security/generated/infisical-fleet-issuer-patch.yaml and
    # environments/base/generated/hub-bootstrap-config-patch.yaml -- and neither
    # has existed for some time. manifests/generated/artifacts.yaml records why:
    # the PKI coordinates "were generated here as a Kustomize patch ... they now
    # reach the security component as chart values read from the infisical-auth
    # Secret at seed time. Nothing is written to the tree, so there is no artifact
    # to register or validate." So this gated on artifacts the design had removed,
    # reported "bootstrap has not published Infisical's coordinates yet" on a box
    # where it had, and could never once have passed.
    #
    # It is the argument the comment below already makes about the machine
    # identity, applied to the coordinates as well: a validator that reads a
    # different source than the platform can pass while the platform is broken, or
    # fail while it works. hub-bootstrap-config is what hub-operator is wired to,
    # so it is what this asks.
    local secrets_project_id env_slug
    secrets_project_id=$(kc get configmap hub-bootstrap-config -n platform-ops \
        -o jsonpath='{.data.INFISICAL_SECRETS_PROJECT_ID}' 2>/dev/null)
    env_slug=$(kc get configmap hub-bootstrap-config -n platform-ops \
        -o jsonpath='{.data.INFISICAL_ENVIRONMENT_SLUG}' 2>/dev/null)

    if [[ -z "$secrets_project_id" ]]; then
        soft_fail "hub-bootstrap-config carries no INFISICAL_SECRETS_PROJECT_ID — bootstrap has not reached the Infisical API phase, or its generated values are not committed"
        return 0
    fi

    # The machine identity, read from where the platform itself reads it.
    #
    # platform-ops/infisical-auth, keys client-id and client-secret, is what the
    # infisical-backend ClusterSecretStore authenticates with
    # (spec.provider.infisical.auth.universalAuthCredentials) and what the CLI
    # creates (constants.NamespaceOps in components/secrets_database_impl.go).
    # Both id and secret come from the Secret rather than the id from the
    # ADR-045 artifact: the artifact is the issuer's copy, and a validator that
    # reads a different source than the platform can pass while the platform
    # cannot authenticate, or fail while it can.
    local infisical_client_id infisical_client_secret
    infisical_client_id=$(kc get secret infisical-auth -n platform-ops \
        -o jsonpath='{.data.client-id}' 2>/dev/null | base64 -d 2>/dev/null)
    infisical_client_secret=$(kc get secret infisical-auth -n platform-ops \
        -o jsonpath='{.data.client-secret}' 2>/dev/null | base64 -d 2>/dev/null)
    if [[ -z "$infisical_client_secret" || -z "$infisical_client_id" ]]; then
        soft_fail "Secret platform-ops/infisical-auth absent or incomplete — the machine identity that reads Infisical does not exist yet"
        return 0
    fi

    # Tenants, and the cell whose Infisical folder holds their secrets.
    local tenants
    tenants=$(kc get ainativesaas -A -o json 2>/dev/null | python3 -c "
import json,sys
try: d=json.load(sys.stdin)
except Exception: sys.exit(0)
for x in d.get('items',[]):
    spec=x.get('spec',{})
    t=spec.get('tenantId') or x['metadata']['name']
    c=spec.get('clusterName') or ''
    if t and c: print(t, c)
" 2>/dev/null)

    if [[ -z "$tenants" ]]; then
        note "no tenants provisioned yet — nothing to verify"
        pass "OAuth client registration: no tenants declared"
        return 0
    fi

    # Both services are in-cluster only; the public names are not up until DNS
    # and ACME have completed, which is later than this gate runs.
    local pf_infisical pf_zitadel infisical_pid infisical_port zitadel_pid zitadel_port
    if ! pf_infisical=$(_oauth_pf_start platform-security infisical-standalone-infisical 8080); then
        soft_fail "cannot reach Infisical (port-forward failed) — the service is not serving yet"
        return 0
    fi
    read -r infisical_pid infisical_port <<< "$pf_infisical"

    if ! pf_zitadel=$(_oauth_pf_start platform-identity zitadel 8080); then
        kill "$infisical_pid" 2>/dev/null || true
        soft_fail "cannot reach Zitadel (port-forward failed) — the issuer is not serving yet"
        return 0
    fi
    read -r zitadel_pid zitadel_port <<< "$pf_zitadel"

    local out
    out=$(INFISICAL_BASE="http://127.0.0.1:$infisical_port" \
          ZITADEL_BASE="http://127.0.0.1:$zitadel_port" \
          ZITADEL_HOST="id.${ENV_ZONE}" \
          INFISICAL_CLIENT_ID="$infisical_client_id" \
          INFISICAL_CLIENT_SECRET="$infisical_client_secret" \
          INFISICAL_PROJECT_ID="$secrets_project_id" \
          INFISICAL_ENV="$env_slug" \
          TENANTS="$tenants" \
          python3 - <<'PY'
import json, os, sys, urllib.error, urllib.parse, urllib.request

INF   = os.environ["INFISICAL_BASE"]
ZIT   = os.environ["ZITADEL_BASE"]
# Zitadel is MULTI-INSTANCE: it resolves which instance serves a request from
# the request's origin. Reached through a port-forward the origin is
# 127.0.0.1:<port>, which matches no instance, and every management call answers
# 404 -- not "unauthorized", not "not found in this org", just 404, which reads
# as a wrong endpoint rather than a wrong host. Sending the public issuer as the
# Host header is what auth-proxy does for the same reason.
ZIT_HOST = os.environ["ZITADEL_HOST"]
PROJ  = os.environ["INFISICAL_PROJECT_ID"]
ENVS  = os.environ["INFISICAL_ENV"]

def http(method, url, body=None, headers=None, timeout=15):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method)
    req.add_header("Content-Type", "application/json")
    for k, v in (headers or {}).items():
        req.add_header(k, v)
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read() or b"{}")

# 1. Authenticate to Infisical as the platform's own machine identity.
try:
    tok = http("POST", f"{INF}/api/v1/auth/universal-auth/login", {
        "clientId": os.environ["INFISICAL_CLIENT_ID"].strip(),
        "clientSecret": os.environ["INFISICAL_CLIENT_SECRET"].strip(),
    })["accessToken"].strip()
except Exception as e:
    print(f"SOFT|Infisical authentication failed ({e}) — the machine identity is not usable yet")
    sys.exit(0)

ihdr = {"Authorization": f"Bearer {tok}"}

def secret(name, path):
    """One secret value, WHITESPACE-STRIPPED.

    The strip is not cosmetic. A credential stored through a file or a UI
    commonly carries a trailing newline, and Python refuses to build a request
    with one:

      Invalid header value b'Bearer eyJ...\n'

    which names neither the credential nor its source and reads as a malformed
    token rather than one extra byte. internal/kube-sbt/providers/zitadel/
    config.go trims for exactly this reason; this is the same guard on the
    validation path.
    """
    url = (f"{INF}/api/v3/secrets/raw/{name}"
           f"?workspaceId={PROJ}&environment={ENVS}&secretPath={urllib.parse.quote(path, safe='')}")
    try:
        return (http("GET", url, headers=ihdr)["secret"]["secretValue"] or "").strip()
    except Exception:
        return ""

# 2. The service token the platform manages Zitadel with.
svc = secret("hub-identity-service-token", "/")
if not svc:
    print("SOFT|Infisical holds no hub-identity-service-token — the identity service has not been provisioned yet")
    sys.exit(0)

zhdr = {"Authorization": f"Bearer {svc}", "Host": ZIT_HOST}

for line in os.environ["TENANTS"].strip().splitlines():
    tenant, cell = line.split()
    tpath = f"/spoke-pool/{cell}/tenants/{tenant}"

    org = secret("OIDC_ORG_ID", tpath)
    pub = secret("OIDC_CLIENT_ID", tpath)
    bff = secret("OAUTH_BFF_CLIENT_ID", tpath)

    if not org or not pub:
        print(f"SOFT|{tenant}: Infisical holds no OIDC_ORG_ID/OIDC_CLIENT_ID yet — identity provisioning has not completed")
        continue

    # 3. Resolve the project inside the tenant's OWN organisation. The org header
    #    is not optional: omitted, Zitadel answers from the default organisation
    #    and the apps of a DIFFERENT tenant would be compared against.
    ohdr = dict(zhdr); ohdr["x-zitadel-orgid"] = org
    try:
        projects = http("POST", f"{ZIT}/management/v1/projects/_search",
                        {"query": {"limit": 100}}, ohdr).get("result", []) or []
    except Exception as e:
        print(f"HARD|{tenant}: Zitadel project search failed in org {org} ({e})")
        continue

    if not projects:
        print(f"HARD|{tenant}: organisation {org} holds no project — a login cannot resolve any role")
        continue

    apps = []
    for p in projects:
        try:
            apps += http("POST", f"{ZIT}/management/v1/projects/{p['id']}/apps/_search",
                         {"query": {"limit": 100}}, ohdr).get("result", []) or []
        except Exception as e:
            print(f"HARD|{tenant}: Zitadel app search failed for project {p.get('id')} ({e})")

    by_client = {}
    for a in apps:
        cid = (a.get("oidcConfig") or {}).get("clientId")
        if cid:
            by_client[cid] = a.get("name", "?")

    # 4. The published client id must be one the issuer actually holds.
    # On failure, say what the issuer DOES hold. "not an application in org X"
    # cannot be acted on: it does not distinguish a stale published id from an
    # app in a project the search missed, from an issuer rebuilt underneath the
    # published value.
    seen = ", ".join(f"{n}={c}" for c, n in sorted(by_client.items(), key=lambda kv: kv[1])) or "none"

    if pub in by_client:
        print(f"PASS|{tenant}: browser client registered in Zitadel as {by_client[pub]!r} (org {org})")
    else:
        print(f"HARD|{tenant}: OIDC_CLIENT_ID {pub} is not among the {len(by_client)} application(s) "
              f"in org {org} [{seen}] — the gateway authenticates with a client the issuer does not "
              f"know, which fails in the browser only")

    if not bff:
        print(f"SOFT|{tenant}: Infisical holds no OAUTH_BFF_CLIENT_ID yet — the server-side client has not been provisioned")
    elif bff in by_client:
        print(f"PASS|{tenant}: server-side client registered in Zitadel as {by_client[bff]!r}")
    else:
        print(f"HARD|{tenant}: OAUTH_BFF_CLIENT_ID {bff} is not among the {len(by_client)} "
              f"application(s) in org {org} [{seen}] — the delegated token exchange fails on the "
              f"first API call after a successful login")
PY
    ) || true

    kill "$infisical_pid" "$zitadel_pid" 2>/dev/null || true
    wait "$infisical_pid" "$zitadel_pid" 2>/dev/null || true

    if [[ -z "$out" ]]; then
        soft_fail "OAuth client verification produced no result — Infisical or Zitadel did not answer"
        return 0
    fi

    local line kind msg
    while IFS= read -r line; do
        [[ -z "$line" ]] && continue
        kind="${line%%|*}"; msg="${line#*|}"
        case "$kind" in
            PASS) pass "$msg" ;;
            SOFT) soft_fail "$msg" ;;
            HARD) hard_fail "$msg" ;;
            *)    note "$line" ;;
        esac
    done <<< "$out"
}
