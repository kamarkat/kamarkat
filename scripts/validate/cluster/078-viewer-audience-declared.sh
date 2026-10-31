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

    local live_aud
    live_aud=$(kc_spoke get vmuser viewer -n platform-observability \
        -o jsonpath='{.spec.jwt.matchClaims.aud}' || true)
    if [[ -z "$live_aud" ]]; then
        hard_fail "VMUser/viewer on the spoke matches no audience, so a token from any client of this issuer satisfies it"
    elif [[ "$live_aud" != "$audience" ]]; then
        hard_fail "VMUser/viewer on the spoke matches audience '$live_aud' while the box declares '$audience'; one is stale and the endpoint enforces whichever is live"
    else
        pass "VMUser/viewer matches the declared audience"
    fi
}
