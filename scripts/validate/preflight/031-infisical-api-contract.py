#!/usr/bin/env python3
"""Every Infisical endpoint we call resolves to a real route in the deployed version.

Two halves, because the evidence for each lives somewhere different:

  A. ALWAYS. Each endpoint constant in our Go source equals the route recorded in
     scripts/validate/infisical-api-contract.yaml, and that file's tag equals the
     image tag the box deploys. Catches a changed constant, and an Infisical bump
     taken without re-deriving the routes.

  B. WHEN reference-projects/infiscial/infisical IS PRESENT. Each route is
     re-derived from `git show <tag>:<file>` -- the registration prefix from the
     index, the local url and method from the router -- and compared with the
     recorded value. Catches a wrong recording, which half A cannot.

WHY, AND WHAT THIS GUARDS AGAINST. On 2026-11-22 a grep for url strings containing
"/memberships" concluded that the identity-membership endpoint did not exist in
either pinned Infisical version, and therefore that no project role had ever been
granted. Both were false: the router that serves it is MOUNTED at
/projects/:projectId/memberships and declares only /identities/:identityId, so the
pattern could not have matched however the routes were registered. A router-local
path is not a public endpoint.

EVERY NEGATIVE RESULT HERE CARRIES A KNOWN-POSITIVE CONTROL. A search that cannot
find what is certainly there must fail this check rather than report that nothing
is wrong -- which is exactly how the original conclusion escaped.

Emits tab-separated lines the shell wrapper classifies:
    OK<TAB><message>    BAD<TAB><message>    NOTE<TAB><message>
"""
import re
import subprocess
from pathlib import Path

import yaml

CONTRACT = Path("scripts/validate/infisical-api-contract.yaml")

# The certificate issuer confines a cluster-scoped ClusterIssuer's credential to
# ONE namespace, and these two files have to agree about which.
ISSUER_MANIFEST = Path("manifests/spoke/spoke-catalog/infra/infisical-issuer.yaml")
PKI_SOURCE = Path("operators/spoke-identity-operator/internal/controller/pki_identity.go")

out = []


def ok(m):
    out.append(("OK", m))


def bad(m):
    out.append(("BAD", m))


def note(m):
    out.append(("NOTE", m))


def mounts(ep):
    """The registration prefixes, outermost first.

    A list when the router is registered inside another one: Infisical nests
    /cert-manager and then /applications, so the public path is the concatenation
    and NEITHER string is a prefix the index registers on its own. Modelling that
    is the whole point -- assuming one flat prefix is the shape of the mistake this
    check exists for.
    """
    m = ep["mount"]
    return m if isinstance(m, list) else [m]


def resolved(ep):
    """api + every mount prefix + local, as a public path with :params."""
    local = "" if ep["local"] == "/" else ep["local"]
    return ep["api"] + "".join(mounts(ep)) + local


def as_format(path):
    """The same path written the way a Go constant writes it."""
    return re.sub(r":[A-Za-z0-9_]+", "%s", path)


def go_constant(file, symbol):
    text = Path(file).read_text()
    m = re.search(rf'\b{re.escape(symbol)}\s*=\s*"([^"]+)"', text)
    return m.group(1) if m else None


def show(vendor, tag, path):
    r = subprocess.run(["git", "-C", str(vendor), "show", f"{tag}:{path}"],
                       capture_output=True, text=True, check=False)
    return r.stdout if r.returncode == 0 else None


def issuer_credential_namespace():
    """The issuer confines credentials to one namespace; assert both ends agree.

    pki-issuer v1.1.0 resolves a cluster-scoped issuer's secret references in
    --cluster-resource-namespace, defaulting to the controller's own namespace,
    and confineNamespace REFUSES a reference naming any other:
    "cross-namespace references are not allowed". So a ClusterIssuer pointing at
    the wrong namespace does not fall back -- it fails, and every
    CertificateRequest on that spoke stays pending.

    Caught here because the two halves live in different files and neither one is
    wrong on its own: the Deployment's namespace is in a manifest, the
    reference's is a Go constant.
    """
    docs = [d for d in yaml.safe_load_all(ISSUER_MANIFEST.read_text()) if d]
    deployment = next((d for d in docs if d.get("kind") == "Deployment"), None)
    if deployment is None:
        bad(f"{ISSUER_MANIFEST} declares no Deployment, so the namespace the issuer "
            f"resolves credentials in cannot be established")
        return

    runs_in = (deployment["metadata"] or {}).get("namespace")
    container = deployment["spec"]["template"]["spec"]["containers"][0]
    args = container.get("args") or []
    flagged = next((a.split("=", 1)[1] for a in args
                    if a.startswith("--cluster-resource-namespace=")), None)
    confined = flagged or runs_in
    if not confined:
        bad(f"{ISSUER_MANIFEST}'s Deployment names no namespace and passes no "
            f"--cluster-resource-namespace, so which namespace it confines credentials "
            f"to depends on where it happens to be applied")
        return

    src = PKI_SOURCE.read_text()
    m = re.search(r'pkiAuthNamespace\s*=\s*"([^"]+)"', src)
    if not m:
        bad(f"pkiAuthNamespace is not declared in {PKI_SOURCE}, so the namespace the "
            f"generated ClusterIssuer references cannot be established")
        return
    referenced = m.group(1)

    if referenced != confined:
        bad(f"the generated ClusterIssuer references its credential in {referenced!r} "
            f"while the issuer confines credentials to {confined!r} "
            f"({'--cluster-resource-namespace' if flagged else 'its own namespace'}). "
            f"confineNamespace refuses the reference outright, so certificate issuance "
            f"on every spoke stays pending")
        return
    ok(f"the issuer's credential namespace agrees on both sides: {confined!r}, from "
       f"{'--cluster-resource-namespace' if flagged else 'the Deployment namespace'}")


def main():
    c = yaml.safe_load(CONTRACT.read_text())

    # ── A1. the recorded tag is the tag the box deploys ──────────────────────
    values = yaml.safe_load(Path(c["deployed_tag_file"]).read_text()) or {}
    node = values
    for key in c["deployed_tag_path"]:
        node = (node or {}).get(key)
    if node != c["tag"]:
        bad(f"{CONTRACT} records routes for Infisical {c['tag']} and "
            f"{c['deployed_tag_file']} deploys {node!r}. The routes below were read "
            f"from a version this box does not run -- re-derive them against {node!r} "
            f"before trusting any endpoint here")
        return
    ok(f"the recorded routes are for Infisical {c['tag']}, the tag "
       f"{c['deployed_tag_file']} deploys")

    # ── A2. every constant we ship matches its recorded route ───────────────
    for ep in c["endpoints"]:
        want = as_format(resolved(ep))
        for ref in ep["constants"]:
            got = go_constant(ref["file"], ref["symbol"])
            if got is None:
                bad(f"{ref['symbol']} is not declared in {ref['file']}; the contract "
                    f"records a route for an endpoint constant that no longer exists")
            elif got != want:
                bad(f"{ref['symbol']} is {got!r} and {ep['name']} resolves to {want!r} "
                    f"({ep['api']} + mount {ep['mount']} + local {ep['local']}). One of "
                    f"the two is wrong, and the call fails at runtime, not here")
            else:
                ok(f"{ref['symbol']} is the {ep['name']} endpoint ({want})")

    # ── A3. nothing we ship calls a route recorded as deprecated ────────────
    for dep in c.get("deprecated", []):
        path = as_format(dep["api"] + dep["mount"] + dep["local"])
        for ep in c["endpoints"]:
            for ref in ep["constants"]:
                if go_constant(ref["file"], ref["symbol"]) == path:
                    bad(f"{ref['symbol']} calls {path}, which {dep['router']} serves for "
                        f"compatibility only: {dep['why']}")
    ok(f"no endpoint constant calls one of the {len(c.get('deprecated', []))} route(s) "
       f"recorded as deprecated")

    issuer_credential_namespace()

    # ── B. re-derive from Infisical's own source, where it is present ───────
    vendor = Path(c["vendor"])
    if not (vendor / ".git").exists():
        note(f"{vendor} is absent, so the recorded routes were not re-derived from "
             f"Infisical's source -- it is gitignored and exists only on a "
             f"workstation. The halves above still ran")
        return

    checked = 0
    for ep in c["endpoints"]:
        index = show(vendor, c["tag"], ep["index"])
        router = show(vendor, c["tag"], ep["router"])
        if index is None or router is None:
            bad(f"{ep['name']}: {ep['index']} or {ep['router']} is not in "
                f"{vendor} at {c['tag']}, so its route cannot be re-derived. The file "
                f"moved, or the tag is not fetched")
            continue

        # THE CONTROL. If reading the router cannot find a method and a url at all,
        # the derivation is broken and every "not found" below would be a false
        # negative -- which is the failure this check exists for.
        if not re.search(r'method:\s*"[A-Z]+"', router) or not re.search(r'url:\s*"', router):
            bad(f"{ep['name']}: no method/url pair was found anywhere in {ep['router']}, "
                f"so this check cannot read that file and its findings would be "
                f"meaningless. Fix the derivation, do not trust the result")
            continue

        unregistered = [m for m in mounts(ep) if f'prefix: "{m}"' not in index]
        if unregistered:
            bad(f"{ep['name']}: {ep['index']} registers no router at prefix(es) "
                f"{', '.join(repr(m) for m in unregistered)} in {c['tag']}. The public "
                f"endpoint is every mount prefix plus the router's own url, so a moved "
                f"prefix moves the endpoint without touching the router")
            continue

        missing = [m for m in ep["methods"]
                   if not re.search(rf'method:\s*"{m}",\s*\n\s*url:\s*"{re.escape(ep["local"])}"',
                                    router)]
        if missing:
            bad(f"{ep['name']}: {ep['router']} at {c['tag']} does not declare "
                f"{', '.join(missing)} on {ep['local']!r}. Recorded methods that the "
                f"version we deploy does not serve")
            continue

        checked += 1
        ok(f"{ep['name']}: re-derived from {vendor.name} at {c['tag']} -- prefix "
           f"{ep['mount']} + {ep['local']}, methods {', '.join(ep['methods'])}")

    for dep in c.get("deprecated", []):
        router = show(vendor, c["tag"], dep["router"])
        if router is None:
            note(f"the route recorded as deprecated no longer has {dep['router']} at "
                 f"{c['tag']}; it may have been removed upstream, which is worth "
                 f"knowing before the next bump")
        elif f'url: "{dep["local"]}"' in router:
            ok(f"the deprecated {dep['local']} route is still served at {c['tag']}, so "
               f"keeping it distinguished from the current one still matters")

    if checked:
        ok(f"{checked} of {len(c['endpoints'])} recorded endpoint(s) re-derived from "
           f"Infisical's source")


main()
for level, message in out:
    print(f"{level}\t{message}")
