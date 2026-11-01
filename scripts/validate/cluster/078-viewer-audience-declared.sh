#!/usr/bin/env bash
# ADR-078 add.5 §7: a box that provisions a viewer identity declares its audience.
#
# The spoke's viewer query endpoint is GATED on global.observability.viewer.audience
# (spoke/spoke-catalog/templated-fields.yaml). Without it the VMAuth and VMUser are
# not rendered at all, which is the right failure -- a VMUser whose matchClaims
# carried an empty `aud` would match every token this issuer signed, and every
# client of the box's own identity provider could read its telemetry.
#
# But a gate that closes quietly is its own hazard. The audience arrives from the
# box registry through environment-manager's globalValues, and a key written at the
# wrong path, or a registry never updated after the client was registered, produces
# exactly the same cluster as a box that deliberately has no viewer: no objects, no
# error, no event. The release looks clean because nothing it declared is missing.
#
# So the two halves are asserted together. The marker ConfigMap the capability
# renders says a viewer identity IS provisioned on this box; from there, an
# undeclared audience is a failure rather than a choice, and a declared one must
# have actually opened the gate on the spoke.
#
# The audience is the id the box's own issuer minted, published as
# hub-observability-project-id. It cannot exist before the release that registers
# the client, which is why the value is declared one release after the capability
# ships -- and why this check exists to make that step visible instead of implicit.

validate_adr078_viewer_audience_declared() {
    section "ADR-078 add.5 §7: the viewer audience is declared where a viewer identity exists"

    if ! kc get configmap observability-viewer -n platform-observability >/dev/null 2>&1; then
        note "no observability-viewer marker on this box, so no viewer identity is"
        note "provisioned and there is no audience to declare"
        return 0
    fi

    local out line audience=""
    out=$(HUB_KUBECONFIG="$HUB_KUBECONFIG" python3 - <<'PYCHECK'
import json, os, subprocess, yaml

kubeconfig = os.environ["HUB_KUBECONFIG"]
r = subprocess.run(["kubectl", "--kubeconfig", kubeconfig, "get",
                    "applications.argoproj.io", "-n", "platform-ops", "-o", "json"],
                   capture_output=True, text=True)
if r.returncode != 0:
    print("FAIL the hub's Applications cannot be read, so what this box declares "
          "cannot be checked")
    raise SystemExit

apps = [a for a in json.loads(r.stdout).get("items", [])
        if a["metadata"]["name"].startswith("platform-spoke-catalog-")]
if not apps:
    print("NOTE this box runs no spoke catalogue Application, and the viewer endpoint "
          "is a spoke object")
    raise SystemExit

for app in apps:
    name = app["metadata"]["name"]
    raw = (((app.get("spec") or {}).get("source") or {}).get("helm") or {}).get("values") or ""
    try:
        values = yaml.safe_load(raw) or {}
    except yaml.YAMLError as exc:
        print("FAIL %s: its helm values do not parse (%s), so the audience it passes "
              "cannot be read" % (name, exc))
        continue
    viewer = (((values.get("global") or {}).get("observability") or {}).get("viewer") or {})
    audience = str(viewer.get("audience") or "").strip()
    if audience:
        print("PASS %s passes the viewer audience %s" % (name, audience))
        print("AUD %s" % audience)
    else:
        print("FAIL %s passes no global.observability.viewer.audience, so its viewer "
              "query endpoint is not rendered and nothing on that spoke answers a "
              "viewer's query -- while this box has an observability client "
              "registered. Read hub-observability-project-id from Infisical and set "
              "observability.viewer.audience in the box registry" % name)
PYCHECK
    )
    while IFS= read -r line; do
        [[ -z "$line" ]] && continue
        case "$line" in
            PASS\ *) pass "${line#PASS }" ;;
            FAIL\ *) hard_fail "${line#FAIL }" ;;
            NOTE\ *) note "${line#NOTE }" ;;
            AUD\ *)  audience="${line#AUD }" ;;
        esac
    done <<< "$out"

    # The gate actually opened. Asserted on the spoke, because the declaration
    # reaching the Application and the objects existing are different facts: a
    # value delivered at a path the catalogue does not read passes the half above
    # and renders nothing here.
    [[ -n "$audience" ]] || return 0
    if ! ensure_spoke_kubeconfig; then
        note "spoke unreachable, so the rendered viewer endpoint was not checked"
        return 0
    fi

    local live
    live=$(kc_spoke get vmauth viewer -n platform-observability -o name || true)
    if [[ -z "$live" ]]; then
        hard_fail "the box declares the viewer audience $audience but the spoke has no VMAuth/viewer: the catalogue's gate did not open, so the value is reaching the Application at a path the catalogue does not read"
        return 0
    fi
    pass "the spoke renders VMAuth/viewer"

    # NOT the audience against itself. Comparing what the Application passes
    # with what the object rendered from it is a value compared against its own
    # source: it agrees whenever the pipeline worked and says nothing about
    # whether the result authorizes anybody. What matters is INDEPENDENT -- the
    # claims the object actually matches, and the issuer it trusts, against the
    # hub that sends people there.
    local claims role_claim live_issuer hub_issuer
    claims=$(kc_spoke get vmuser viewer -n platform-observability \
        -o jsonpath='{.spec.jwt.matchClaims}' || true)
    role_claim=$(printf '%s' "$claims" | python3 -c '
import json, sys
raw = sys.stdin.read().strip()
try:
    claims = json.loads(raw) if raw else {}
except ValueError:
    claims = {}
print("\n".join(k for k in claims if ":roles." in k))' || true)
    if [[ -z "$role_claim" ]]; then
        hard_fail "VMUser/viewer matches no project role claim, only an audience: a client of this issuer may add any project to its own token's audience with the urn:zitadel:iam:org:project:id:<id>:aud scope and the issuer does not check for a grant, so a tenant application's token would be accepted here"
    elif [[ "$role_claim" != *":${audience}:roles."* ]]; then
        hard_fail "VMUser/viewer matches the role claim '$role_claim', which is not scoped to the declared project $audience: role keys repeat across projects -- every tenant project defines a 'viewer' -- so an unscoped or mis-scoped claim admits their users"
    elif [[ "$role_claim" == *"%!"* ]]; then
        hard_fail "VMUser/viewer's role claim '$role_claim' contains a Go format error, so the claim name matches nothing and every viewer query is denied; an id that reached the template as a number rather than a string does this"
    else
        pass "VMUser/viewer matches the viewer role in project $audience"
    fi

    live_issuer=$(kc_spoke get vmuser viewer -n platform-observability \
        -o jsonpath='{.spec.jwt.oidc.issuer}' || true)
    hub_issuer=$(kc get grafana platform -n platform-observability \
        -o jsonpath='{.spec.config.auth\.generic_oauth.auth_url}' || true)
    hub_issuer="${hub_issuer%/oauth/v2/authorize}"
    if [[ -z "$live_issuer" || -z "$hub_issuer" ]]; then
        note "issuer not declared on both ends yet, so the pair was not compared"
    elif [[ "$live_issuer" != "$hub_issuer" ]]; then
        hard_fail "the spoke validates tokens from '$live_issuer' while the hub signs people in at '$hub_issuer': a token from one is refused by the other as signed by somebody else, and neither end's status says which is wrong"
    else
        pass "the hub and the spoke name the same issuer, $live_issuer"
    fi

    # RENDERED IS NOT REACHABLE. The stores answer on a ClusterIP; the box's
    # Grafana runs on the hub, in another cluster, and reaches them over the
    # box's own public address. So the endpoint is only finished when its
    # listener is programmed, its certificate is issued and its route resolves
    # to the VMAuth -- three objects in two namespaces, each of which fails
    # quietly on its own: an unprogrammed listener answers nothing, an unissued
    # certificate answers with a TLS error no Grafana message explains, and an
    # unresolved backendRef answers 503.
    local gw listener route cert ready
    gw=$(kc_spoke get gateway tenant-tls-gateway -n platform-ops \
        -o jsonpath='{.status.listeners[?(@.name=="observability-viewer")].conditions[?(@.type=="Programmed")].status}' || true)
    if [[ "$gw" != "True" ]]; then
        hard_fail "the spoke's Gateway has no programmed observability-viewer listener (${gw:-absent}), so nothing terminates TLS for this endpoint"
    else
        pass "the observability-viewer listener is programmed on the spoke's Gateway"
    fi

    route=$(kc_spoke get httproute observability-viewer -n platform-ops \
        -o jsonpath='{.status.parents[0].conditions[?(@.type=="ResolvedRefs")].status}' || true)
    if [[ "$route" != "True" ]]; then
        hard_fail "HTTPRoute platform-ops/observability-viewer does not resolve its backend (${route:-absent}) — the ReferenceGrant in platform-observability is what permits it to name vmauth-viewer across namespaces, and without it every query answers 503"
    else
        pass "the viewer route resolves to the VMAuth across namespaces"
    fi

    cert=$(kc_spoke get certificate observability-viewer -n platform-ops \
        -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' || true)
    if [[ "$cert" != "True" ]]; then
        ready=$(kc_spoke get certificate observability-viewer -n platform-ops \
            -o jsonpath='{.status.conditions[?(@.type=="Ready")].message}' || true)
        hard_fail "the viewer endpoint's certificate is not issued (${cert:-absent}: ${ready:-no message}); the hostname resolves and then fails TLS, which Grafana reports only as a datasource error"
    else
        pass "the viewer endpoint's certificate is issued"
    fi
}

# ADR-078 add.5 §7: the hub half of the query path.
#
# The endpoint being reachable is not the same as the box being able to use it.
# Three more things have to hold, each of which fails in a way that reads as the
# SPOKE being broken:
#
#   * the viewer client's credentials exist, or Grafana starts with generic OAuth
#     configured and no client, and the sign-in button returns an issuer error.
#   * generic OAuth is actually on. Without it the only way in is the admin
#     credential, and an admin session carries no OIDC token -- so every spoke
#     datasource answers 401 while the hub's own pair keep working, which looks
#     exactly like a broken spoke.
#   * the hub names the host the spoke terminates. Four objects carry that
#     string, three on the spoke and one here, and nothing reconciles them
#     against each other. They are compared rather than recomputed: a check that
#     re-derives the hostname from the same rule the producer used agrees with
#     itself and says nothing about whether the two ends agree.
validate_adr078_viewer_queryable_from_grafana() {
    section "ADR-078 add.5 §7: the box's Grafana can query what its spokes publish"

    if ! kc get configmap observability-viewer -n platform-observability >/dev/null 2>&1; then
        note "no observability-viewer marker on this box, so there is no viewer"
        note "identity and nothing for Grafana to sign a viewer in with"
        return 0
    fi

    # PRESENCE ONLY. The values are a client credential; this reports that both
    # keys resolved, never what they hold.
    local cid csec
    cid=$(kc get secret grafana-oidc -n platform-observability -o jsonpath='{.data.clientId}' 2>/dev/null || true)
    csec=$(kc get secret grafana-oidc -n platform-observability -o jsonpath='{.data.clientSecret}' 2>/dev/null || true)
    if [[ -z "$cid" || -z "$csec" ]]; then
        hard_fail "platform-observability/grafana-oidc is missing its clientId or clientSecret, so Grafana has no viewer client: the issuer discloses a client secret only when it mints one, and hub-operator does not regenerate on a reconcile, so the key is absent rather than wrong and the client has to be re-created for a secret to exist at all"
    else
        pass "the viewer client's id and secret both resolved into grafana-oidc"
    fi

    local enabled authurl
    enabled=$(kc get grafana platform -n platform-observability \
        -o jsonpath='{.spec.config.auth\.generic_oauth.enabled}' 2>/dev/null || true)
    authurl=$(kc get grafana platform -n platform-observability \
        -o jsonpath='{.spec.config.auth\.generic_oauth.auth_url}' 2>/dev/null || true)
    if [[ "$enabled" != "true" ]]; then
        hard_fail "the hub Grafana has generic OAuth ${enabled:-unset}, so the only way in is the admin credential -- and an admin session carries no OIDC token, so every spoke datasource answers 401 while the hub's own pair keep working"
    elif [[ -z "$authurl" ]]; then
        hard_fail "the hub Grafana has generic OAuth enabled with no auth_url, so the sign-in button leads nowhere"
    else
        pass "the hub Grafana signs viewers in against $authurl"
    fi

    local spoke url logurl pass_thru
    while read -r spoke; do
        [[ -z "$spoke" ]] && continue
        url=$(kc get grafanadatasource "${spoke}-metrics" -n platform-observability \
            -o jsonpath='{.spec.datasource.url}' 2>/dev/null || true)
        logurl=$(kc get grafanadatasource "${spoke}-logs" -n platform-observability \
            -o jsonpath='{.spec.datasource.url}' 2>/dev/null || true)
        if [[ -z "$url" || -z "$logurl" ]]; then
            hard_fail "the hub has no datasource pair for spoke $spoke, so its telemetry is published and nothing lists it: the platform-spoke-datasources Application is gated on the same audience as the endpoint, so a declared audience with no datasource means that ApplicationSet is not reconciling"
            continue
        fi
        pass_thru=$(kc get grafanadatasource "${spoke}-metrics" -n platform-observability \
            -o jsonpath='{.spec.datasource.jsonData.oauthPassThru}' 2>/dev/null || true)
        if [[ "$pass_thru" != "true" ]]; then
            hard_fail "datasource ${spoke}-metrics does not forward the viewer's token (oauthPassThru=${pass_thru:-unset}), so it reaches the spoke's VMAuth with no token at all and every query answers 401"
        else
            pass "the hub lists $spoke and forwards each viewer's own token to it"
        fi
    done < <(kc get applications.argoproj.io -n platform-ops -o name 2>/dev/null \
        | sed -n 's|.*/platform-spoke-catalog-||p')

    # THE TWO ENDS, COMPARED. Only for the spoke this run can reach.
    if ! ensure_spoke_kubeconfig; then
        note "spoke unreachable, so the host the hub queries was not compared with"
        note "the host the spoke terminates"
        return 0
    fi
    local listener_host hub_host
    listener_host=$(kc_spoke get gateway tenant-tls-gateway -n platform-ops \
        -o jsonpath='{.spec.listeners[?(@.name=="observability-viewer")].hostname}' 2>/dev/null || true)
    [[ -n "$listener_host" ]] || return 0
    hub_host=$(kc get grafanadatasource -n platform-observability -o json 2>/dev/null \
        | python3 -c '
import json, sys
from urllib.parse import urlparse
hosts = set()
for d in json.load(sys.stdin).get("items", []):
    ds = (d.get("spec") or {}).get("datasource") or {}
    if (ds.get("jsonData") or {}).get("oauthPassThru"):
        hosts.add(urlparse(ds.get("url") or "").hostname or "")
print("\n".join(sorted(h for h in hosts if h)))' || true)
    if grep -qxF "$listener_host" <<< "$hub_host"; then
        pass "the hub queries $listener_host, which is the host that spoke terminates"
    else
        hard_fail "the spoke terminates $listener_host and the hub's viewer datasources name ${hub_host:-nothing}; the listener, the certificate, the route and the datasource all carry that string and nothing reconciles them, so the one that disagrees fails TLS or resolves nowhere while every object reports Ready"
    fi
}
