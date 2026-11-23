#!/usr/bin/env python3
"""A tenant's public HTTPRoute reaches a backend some chart actually renders.

ADR-088: a gateway, its Service and the grant that lets a platform-ops route reach
it belong to one APP. Two charts build that path and neither could see the other:

  tenant-public-tls  renders the HTTPRoute, in platform-ops, with backendRefs
                     naming tenant-<tenant>-<app>
  universal-tenant   renders Service agentgateway-<app> AND the ReferenceGrant
                     permitting the cross-namespace reference, both under
                     gateway.enabled

An app with public.hosts and no gateway.enabled therefore published a route to a
Service nobody had asked for. Gateway API reports that as RefNotPermitted, which
reads as a missing grant -- and sent two people looking for a grant template that
was never the problem. Observed on nutgraf-stg-01, 2026-11-24.

This asserts the three facts that make that impossible to reintroduce: the route's
backend name is the one universal-tenant renders, the grant is issued by the target
namespace for that same name, and tenant-public-tls refuses to render a route when
the flag that produces both is unset.
"""
import re
import sys
from pathlib import Path

PUBLIC_TLS = Path("manifests/tenants/charts/tenant-public-tls")
UNIVERSAL = Path("manifests/tenants/charts/universal-tenant")

out = []
def ok(m): out.append(("OK", m))
def bad(m): out.append(("BAD", m))


def main():
    route = (PUBLIC_TLS / "templates/httproute.yaml").read_text()
    gateway = (UNIVERSAL / "templates/agentgateway.yaml").read_text()
    helpers = (PUBLIC_TLS / "templates/_helpers.tpl").read_text()

    # 1. Every backendRef names the same Service universal-tenant renders.
    backends = set(re.findall(r"backendRefs:\s*\n\s*- name:\s*(\S+)", route))
    if not backends:
        bad("tenant-public-tls renders no backendRefs; this check cannot confirm the "
            "route reaches anything")
    else:
        unexpected = {b for b in backends if "agentgateway-" not in b}
        if unexpected:
            bad(f"a public route targets {sorted(unexpected)}, which is not the app's "
                f"gateway Service. Only universal-tenant's agentgateway-<app> carries a "
                f"ReferenceGrant, so any other backend is refused at the namespace edge")
        else:
            ok(f"every public backendRef names the app's gateway Service "
               f"({len(backends)} distinct reference(s))")

    # 2. The route crosses a namespace, so the grant has to exist and be issued by
    #    the namespace being referenced -- never by the one doing the referencing.
    if "namespace: platform-ops" not in route:
        bad("the public HTTPRoute is not rendered into platform-ops; the grant below is "
            "scoped to that source namespace and would no longer apply")
    elif 'include "tenant-public-tls.appNamespace"' not in route:
        bad("no backendRef names the app's namespace, so this check cannot tell whether "
            "the reference crosses one")
    else:
        # \b, because "kind: ReferenceGrant" also matches the start of
        # "ReferenceGrantX" -- renaming the kind left this check finding it and
        # reporting the grant present. Caught by mutation, not by reading.
        grant = re.search(r"kind: ReferenceGrant\b(.*?)(?=\n---|\Z)", gateway, re.S)
        if not grant:
            bad("universal-tenant renders no ReferenceGrant, so a platform-ops route "
                "cannot reach the app's Service: Gateway API refuses it as "
                "RefNotPermitted")
        else:
            body = grant.group(1)
            issued_by_target = 'include "universal-tenant.namespace"' in body
            from_platform_ops = "namespace: platform-ops" in body
            to_service = "kind: Service" in body
            if not (issued_by_target and from_platform_ops and to_service):
                bad(f"the ReferenceGrant does not permit exactly this reference "
                    f"(issued by the app's namespace: {issued_by_target}, from "
                    f"platform-ops: {from_platform_ops}, to a Service: {to_service})")
            else:
                ok("the grant is issued BY the referenced namespace, for HTTPRoute from "
                   "platform-ops to the app's Service -- so no tenant can make another "
                   "tenant's services routable")

    # 3. The two charts are gated together, and the render says so rather than
    #    producing a route to nothing.
    if ".Values.gateway.enabled" not in gateway:
        bad("universal-tenant no longer gates the gateway on gateway.enabled; the "
            "refusal in tenant-public-tls names a flag that does not decide anything")
    elif not re.search(r"public\.hosts.*?not\s+\.Values\.gateway\.enabled", helpers, re.S):
        bad("tenant-public-tls does not refuse public.hosts without gateway.enabled, so "
            "an app can still publish a route whose backend and grant are never "
            "rendered -- the exact state that looked like a missing grant")
    else:
        ok("tenant-public-tls refuses to render a public route unless the flag that "
           "produces its backend and grant is set, naming that flag")


def secret_store_has_one_owner():
    """The application's SecretStore is rendered by exactly one thing.

    It used to be built by the tenant-platform composition, which gates its whole
    XR on `database.enabled` -- so an application with no database never received a
    store, while the templates that read it render regardless. Two consequences,
    and this asserts against both: a store nobody creates (the waypoint case), and
    a store two things create (what moving it would cause if the composition kept
    its copy).
    """
    chart = UNIVERSAL / "templates/tenant-secret-store.yaml"
    composition = Path("manifests/hub-core-services/crossplane/tenant-platform/"
                       "compositions/ainativesaas-starter-hetzner.yaml")

    if not chart.is_file():
        bad("universal-tenant renders no tenant-secret-store: every application's "
            "platform ExternalSecrets reference it, so without it they all fail with "
            "'SecretStore not found' and nothing says why")
        return

    text = chart.read_text()
    if "kind: SecretStore" not in text:
        bad(f"{chart.name} exists but declares no SecretStore")
    elif ".Values.deployXR" not in text:
        bad(f"{chart.name} is not confined to the workload path, so the XR Application "
            f"would render a second copy of the same object")
    # The CONSTRUCT, not the two words. Testing for "required" and "hubDomain"
    # separately passed when the guard was replaced outright, because both words
    # still occur elsewhere in the file -- the clusterName guard uses one and the
    # hostAPI uses the other. Caught by mutation.
    elif not re.search(r'required\s+"[^"]*"\s+\.Values\.hubDomain', text):
        bad(f"{chart.name} does not require hubDomain: a guessed hostname points an "
            f"application's secrets at another box")
    else:
        ok("the application's SecretStore is rendered once, in the workload path, "
           "beside the ExternalSecrets that read it, and refuses a guessed hub name")

    comp = composition.read_text()
    # Only inside a `base:` does this name an object the composition CREATES; a
    # secretStoreRef naming it is a consumer, which is fine and expected.
    creates = re.search(r"kind: SecretStore\s*\n\s*metadata:", comp)
    if creates:
        bad(f"{composition.name} still creates a SecretStore. Two owners of one object: "
            f"Crossplane and the chart would fight over it, and the composition's copy "
            f"only exists when the application declares a database")
    else:
        ok("the tenant-platform composition creates no SecretStore, so there is one "
           "owner and its lifecycle no longer depends on database.enabled")


main()
secret_store_has_one_owner()
for level, message in out:
    print(f"{level}\t{message}")
