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


status, body = http("POST", f"{INF}/api/v1/auth/universal-auth/login", {
    "clientId": os.environ["SECRET_STORE_CLIENT_ID"].strip(),
    "clientSecret": os.environ["SECRET_STORE_CLIENT_SECRET"].strip(),
})
if status != 200 or "accessToken" not in body:
    print("SOFT|Infisical authentication failed, so no identity's memberships could be read")
    sys.exit(0)
hdr = {"Authorization": "Bearer " + body["accessToken"].strip()}

status, listed = http("GET", f"{INF}/api/v1/projects?limit=100", headers=hdr)
if status != 200:
    print("SOFT|the project list is unreadable, so a membership cannot be placed in a project")
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
            unknown = True
    return (None if unknown and not found else found)


def identity_id(name):
    st, b = http("GET", f"{INF}/api/v1/identities?limit=200", headers=hdr)
    if st != 200:
        return None
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
    eso = identity_id(f"spoke-{cluster}")
    if eso is None:
        print(f"SOFT|{cluster}: its workload identity could not be found, so its scope is unknown")
    else:
        held = memberships(eso)
        if held is None:
            print(f"SOFT|{cluster}: at least one project would not say whether its workload "
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
    pki = identity_id(f"spoke-{cluster}-pki")
    if pki is None:
        print(f"SOFT|{cluster}: its certificate issuer identity could not be found, so whether "
              f"issuance is separately authorised is unknown")
        continue
    held = memberships(pki)
    cert_slug = by_id.get(CERT_PROJECT, CERT_PROJECT)
    if held is None:
        print(f"SOFT|{cluster}: the issuer identity's memberships are partly unreadable")
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
