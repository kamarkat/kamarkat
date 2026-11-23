#!/usr/bin/env python3
"""Effective project memberships of each spoke identity, read live.

Authenticates as the platform's own machine identity -- the verifier
cluster/30-oauth-clients.sh already uses -- and reads back what each spoke
identity is a member of. Never authenticates AS a spoke identity: that needs a
spoke's client secret in a verifier's hands, and the denial it would demonstrate
is already implied by the membership set.

Prints permitted/denied conclusions, identity names, project slugs and role names.
Never a credential and never a secret value.
"""
import json
import os
import sys
import urllib.error
import urllib.request

INF = os.environ["SECRET_STORE_BASE"]
CERT_PROJECT = os.environ.get("CERT_PROJECT_ID", "")


def http(method, url, body=None, headers=None, timeout=20):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method)
    req.add_header("Content-Type", "application/json")
    for k, v in (headers or {}).items():
        req.add_header(k, v)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, json.loads(r.read() or b"{}")
    except urllib.error.HTTPError as e:
        return e.code, {}
    except Exception:
        return 0, {}


# The organization, which the identity listing REQUIRES. identity-router.ts
# declares `querystring: z.object({ orgId: z.string() })` -- not optional -- and
# declares no limit or offset at all. This asked for `?limit=200` and no orgId, so
# Infisical answered 422 Required: ["orgId"] every time, the lookup returned None,
# and the module reported that the identity "could not be found": a verifier that
# never once put its question, reporting the shape of an answer.
#
# No default. An org id guessed from anywhere would make this check assert about
# somebody else's organization, which is worse than not running.
ORG_ID = os.environ["SECRET_STORE_ORGANIZATION_ID"].strip()
if not ORG_ID:
    print("UNVERIFIED|the organization id is empty, so no identity can be looked up")
    sys.exit(0)

status, body = http("POST", f"{INF}/api/v1/auth/universal-auth/login", {
    "clientId": os.environ["SECRET_STORE_CLIENT_ID"].strip(),
    "clientSecret": os.environ["SECRET_STORE_CLIENT_SECRET"].strip(),
})
if status != 200 or "accessToken" not in body:
    # NOT soft. soft_fail means "not yet converged, expected to self-heal", and a
# refused credential or a malformed query never self-heals into a narrow scope.
    print("UNVERIFIED|Infisical refused the platform's own credential, so no identity's "
          "memberships could be read")
    sys.exit(0)
hdr = {"Authorization": "Bearer " + body["accessToken"].strip()}

status, listed = http("GET", f"{INF}/api/v1/projects?limit=100", headers=hdr)
if status != 200:
    print("UNVERIFIED|the project list is unreadable, so a membership cannot be placed in a project")
    sys.exit(0)
# `projects`, per the route's declared 200 body; `workspaces` belongs to the
# different /api/v1/workspace endpoint.
by_id = {p["id"]: p.get("slug", "") for p in listed.get("projects", [])}
by_slug = {v: k for k, v in by_id.items()}


def memberships(identity_id):
    """Projects this identity belongs to, as {slug: [roles]}, or None if unknown."""
    found, unknown = {}, False
    for pid, slug in by_id.items():
        st, b = http("GET", f"{INF}/api/v1/projects/{pid}/memberships/identities/{identity_id}",
                     headers=hdr)
        if st == 200:
            roles = (b.get("identityMembership") or {}).get("roles") or []
            found[slug or pid] = sorted(
                (r.get("customRole") or {}).get("slug") or r.get("role", "?")
                for r in roles)
        elif st != 404:
            # Only the specific 404 is an answer. Anything else means this project
            # would not say, and a project that would not say cannot be counted as
            # one this identity is absent from.
            unknown = True
    # ANY unanswered project makes the inventory incomplete, whatever the others
    # said. This read `unknown and not found`, so one project refusing to answer
    # while the cluster's own project answered produced a complete-looking
    # inventory -- and the caller asserted "a member of nothing else" on it. That
    # is a false green on the one property this check exists to establish, and it
    # passes most loudly in exactly the case worth catching: an identity still
    # holding hub-secrets while hub-secrets is the project that errored.
    return (None if unknown else found)


class Unobtainable(Exception):
    """The question could not be put. Distinct from an answer of "no"."""


def identity_id(name):
    """The identity's id, or None if the organization genuinely has no such identity.

    These two were one value before: any HTTP failure returned None, which read as
    "no such identity" and was then reported as an unconverged scope. The lookup
    failing and the identity being absent are different findings, and only one of
    them is about this platform's authorization.
    """
    st, b = http("GET", f"{INF}/api/v1/identities?orgId={ORG_ID}", headers=hdr)
    if st != 200:
        raise Unobtainable(f"the identity listing returned HTTP {st}")
    # `identities`, per the route's declared 200 body.
    for entry in b.get("identities", []):
        ident = entry.get("identity", entry)
        if ident.get("name") == name:
            return ident.get("id")
    return None


for raw in os.environ["DECLARED"].strip().splitlines():
    parts = raw.split()
    cluster = parts[0] if parts else ""
    published = parts[1] if len(parts) > 1 else ""
    if not cluster:
        continue

    want_slug = f"spoke-{cluster}-secrets"

    if not published:
        print(f"NOTE|{cluster}: no secrets project is published for it yet, so its identity "
              f"keeps its existing access by design -- hub-operator publishes the slug once "
              f"the project holds this cluster's credentials")
        continue
    if published != want_slug:
        print(f"FAIL|{cluster}: the published secrets project is {published!r} and the slug "
              f"derived from the cluster's name is {want_slug!r}; the store and the identity "
              f"would be pointed at different projects")
        continue
    if want_slug not in by_slug:
        print(f"FAIL|{cluster}: project {want_slug!r} is published for it and does not exist, "
              f"so its workloads have no project to read from")
        continue

    # ── the ESO identity ────────────────────────────────────────────────────
    try:
        eso = identity_id(f"spoke-{cluster}")
        pki_lookup = identity_id(f"spoke-{cluster}-pki")
    except Unobtainable as e:
        # The listing itself failed: say so once for this cluster and assess
        # nothing, rather than reporting an absent identity we never asked about.
        print(f"UNVERIFIED|{cluster}: {e}, so no identity's scope could be assessed")
        continue
    if eso is None:
        print(f"FAIL|{cluster}: the organization has no identity named spoke-{cluster}, so "
              f"nothing holds this cluster's secret access and the narrowing this check exists "
              f"to confirm has not happened")
    else:
        held = memberships(eso)
        if held is None:
            print(f"UNVERIFIED|{cluster}: at least one project would not say whether its workload "
                  f"identity is a member, so 'a member of nothing else' is unestablished")
        elif list(held) == [want_slug] and held[want_slug] == ["viewer"]:
            print(f"PASS|{cluster}: its workload identity is viewer of {want_slug} and a member "
                  f"of no other project, so it can read this cluster's secrets and -- by what a "
                  f"built-in role grants -- no other cluster's and none of the box's")
        else:
            extra = {k: v for k, v in held.items() if k != want_slug}
            print(f"FAIL|{cluster}: its workload identity holds {json.dumps(held, sort_keys=True)}; "
                  f"expected viewer of {want_slug} alone. "
                  + (f"Still a member of {', '.join(sorted(extra))}" if extra else
                     "The role in its own project is not viewer"))

    # ── the PKI identity ────────────────────────────────────────────────────
    pki = pki_lookup
    if pki is None:
        print(f"FAIL|{cluster}: the organization has no identity named spoke-{cluster}-pki, so whether "
              f"issuance is separately authorised is unknown")
        continue
    held = memberships(pki)
    cert_slug = by_id.get(CERT_PROJECT, CERT_PROJECT)
    if held is None:
        print(f"UNVERIFIED|{cluster}: the issuer identity's memberships are partly unreadable")
    elif want_slug in held:
        print(f"FAIL|{cluster}: its certificate issuer identity is a member of the cluster's "
              f"secrets project, so a compromised issuer credential reads workload secrets")
    elif cert_slug and list(held) == [cert_slug] and held[cert_slug] == ["member"]:
        print(f"PASS|{cluster}: its certificate issuer identity is member of {cert_slug} alone -- "
              f"enough to read its certificate profile, and not a member of any secrets project")
    else:
        print(f"FAIL|{cluster}: its certificate issuer identity holds "
              f"{json.dumps(held, sort_keys=True)}; expected member of the Certificate Manager "
              f"project alone")
