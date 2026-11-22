#!/usr/bin/env python3
"""A declared provider resolves to one existing composition (ADR-008, ADR-046).

The tenant declares `spec.provider` and the platform decides which Crossplane
Composition builds the cluster. Four things have to hold together for that to be
true, and on 2026-11-22 none of them did.

WHAT HAPPENED. Commit 4a086204 took Crossplane from 1.20.5 to 2.4.3 and the
SpokePool XRD from apiextensions.crossplane.io/v1 to /v2 with `scope: Cluster`.
A v1 XRD defaults to `scope: LegacyCluster`, whose XR carries its Crossplane
machinery directly under `spec`; a modern XR carries it under `spec.crossplane`
(crossplane-runtime v2.4.3 pkg/xcrd/schemas.go: "Modern XRs nest their Crossplane
machinery fields under spec.crossplane"). Two claim templates and hub-operator's
mutating webhook kept writing `spec.compositionSelector`, which the generated CRD
no longer declares.

WHY NOTHING CAUGHT IT FOR THREE WEEKS. The API server PRUNES an undeclared field
when it decodes the request, before admission (Kubernetes v1.36
apiextensions-apiserver customresource_handler.go wraps the request decoder in
schemaCoercingDecoder). So nothing errored: the field vanished, Crossplane saw no
selection, and its resolver listed every Composition for the type and picked one
at random. A box carries only its own provider's composition, so that single
candidate won whatever the cluster declared. The only reason it surfaced at all
is that ArgoCD builds a typed patch against the live schema before it syncs, and
refused the file -- which it did on the first workload cluster declared since the
migration.

A VALIDATING WEBHOOK CANNOT CATCH THIS. The field is gone before admission runs.
It is caught here, in the rendered source, or not at all.

Emits tab-separated lines the shell wrapper classifies:
    OK<TAB><message>
    BAD<TAB><message>
"""
import re
import subprocess
from pathlib import Path

import yaml

XRD = Path("manifests/hub-core-services/crossplane/tenant-platform/xrds/spokepool-v1.yaml")
TEMPLATES = Path("manifests/tenants/gitops-template/templates/workload-cluster/infrastructure")
REGISTRY = Path("operators/hub-operator/config/manager/provider-registry.yaml")
CHART_VALUES = Path("manifests/argocd/environment-manager/values.yaml")
RULE_SOURCE = Path("internal/kakt-cli/tenant/render_box.go")
OPERATOR_MAIN = Path("operators/hub-operator/cmd/main.go")
OPERATOR_CONFIG = Path("operators/hub-operator/config/default")
PROVIDER_CELL = "manifests/providers"

# What Crossplane adds under spec.crossplane for a Namespaced or Cluster scoped
# XR (crossplane-runtime v2.4.3 pkg/xcrd/schemas.go CompositeResourceSpecProps),
# confirmed against the generated CRD on a live box. The check needs this because
# the XRD's own schema is not what the API server serves: Crossplane augments it,
# and a CEL rule or a claim field is judged against the augmented result.
INJECTED = {
    "compositionRef",
    "compositionRevisionRef",
    "compositionRevisionSelector",
    "compositionSelector",
    "compositionUpdatePolicy",
    "resourceRefs",
}

# The same names directly under spec, which is where they lived before the
# migration, plus the two a LegacyCluster XR also carried. Any of these in a
# shipped XR is the 2026-11-22 defect returning.
LEGACY_SPEC_FIELDS = sorted(INJECTED | {"claimRef", "writeConnectionSecretToRef"})

out = []


def ok(msg):
    out.append(("OK", msg))


def bad(msg):
    out.append(("BAD", msg))


def render(path):
    """kustomize output for a directory, or None with the reason reported."""
    try:
        r = subprocess.run(["kubectl", "kustomize", str(path)],
                           capture_output=True, text=True, check=False)
    except FileNotFoundError:
        bad("kubectl is not on PATH, so the delivered composition and webhook "
            "configurations cannot be rendered. This check asserts what a box "
            "receives, not what the files say, so it cannot run without it")
        return None
    if r.returncode != 0:
        bad(f"kustomize build of {path} failed, so what a box receives there cannot "
            f"be established: {r.stderr.strip().splitlines()[-1] if r.stderr.strip() else r.returncode}")
        return None
    return [d for d in yaml.safe_load_all(r.stdout) if d]


def cel_paths(rule):
    """The self.<a>.<b> paths a CEL rule reads."""
    return set(re.findall(r"self\.([A-Za-z0-9_.]+)", rule))


def main():
    # ── 1. The XRD's scope decides where the machinery lives ──────────────────
    xrd = yaml.safe_load(XRD.read_text())
    api_version = xrd.get("apiVersion", "")
    scope = xrd["spec"].get("scope")
    if api_version != "apiextensions.crossplane.io/v2" or scope != "Cluster":
        bad(f"{XRD} is {api_version} scope={scope}. This check, the claim templates "
            f"and hub-operator's defaulter all place the composition reference at "
            f"spec.crossplane, which is where a Namespaced or Cluster scoped XR "
            f"carries it. A LegacyCluster XR carries it directly under spec, so "
            f"changing the scope moves the field and every writer of it")
        return
    ok(f"the SpokePool XRD is {api_version} scope={scope}, so its machinery is at "
       f"spec.crossplane")

    version = xrd["spec"]["versions"][0]
    spec_schema = version["schema"]["openAPIV3Schema"]["properties"]["spec"]
    declared = set(spec_schema.get("properties") or {})

    # ── 2. Unknown fields are pruned, so this check is the only guard ─────────
    for killer, why in (("additionalProperties",
                         "it is not copied to the generated CRD at all "
                         "(xcrd.genCrdVersion copies required, "
                         "x-kubernetes-preserve-unknown-fields, "
                         "x-kubernetes-validations, oneOf, description and "
                         "properties), so it describes a permissiveness the XR "
                         "does not have"),
                        ("x-kubernetes-preserve-unknown-fields",
                         "it IS copied, and preserving unknown fields is exactly "
                         "what would have hidden spec.compositionSelector for "
                         "another three weeks")):
        if killer in spec_schema:
            bad(f"{XRD} sets spec.{killer}. Remove it: {why}")
    if "additionalProperties" not in spec_schema and \
            "x-kubernetes-preserve-unknown-fields" not in spec_schema:
        ok("the XRD's spec preserves no unknown fields, so an undeclared claim field "
           "is pruned and this check is what catches it")

    # ── 3. CEL rules survive Crossplane's CRD generation ─────────────────────
    addressable = declared | {"crossplane"}
    rules = list(spec_schema.get("x-kubernetes-validations") or [])
    for prop, schema in (spec_schema.get("properties") or {}).items():
        for rule in ((schema or {}).get("x-kubernetes-validations") or []):
            # A rule attached to a property is evaluated with `self` bound to that
            # property's own value, so a path under self reads a field of a scalar.
            if "self." in rule.get("rule", ""):
                bad(f"the CEL rule on spec.{prop} reads a path under self, and self "
                    f"is bound to that property's own value here, not to spec")
    bad_paths = []
    for rule in rules:
        for path in cel_paths(rule.get("rule", "")):
            head = path.split(".")[0]
            if head not in addressable:
                bad_paths.append((head, path))
            elif head == "crossplane":
                parts = path.split(".")
                if len(parts) > 1 and parts[1] not in INJECTED:
                    bad_paths.append((parts[1], path))
    if bad_paths:
        for head, path in bad_paths:
            bad(f"a CEL rule on the XRD's spec reads self.{path}, and {head!r} is in "
                f"neither the XRD's own properties nor the set Crossplane injects. "
                f"The rule is compiled when Crossplane applies the generated CRD, so "
                f"this would leave the XRD unestablished and every SpokePool "
                f"unadmittable")
    else:
        ok(f"every CEL rule on the XRD's spec reads a path the generated CRD declares "
           f"({len(rules)} rule(s))")

    # ── 4. The two invariants that hold without the webhook ──────────────────
    provider_rules = " ".join(
        r.get("rule", "") for r in
        (spec_schema.get("properties") or {}).get("provider", {}).get("x-kubernetes-validations") or [])
    if "oldSelf" not in provider_rules:
        bad("spec.provider carries no transition rule. The provider selects the "
            "composition that built the cluster, so editing it on a provisioned "
            "SpokePool asks the platform to replace a running cluster's "
            "infrastructure in place -- the XRD's description says so and nothing "
            "enforced it")
    else:
        ok("spec.provider is immutable by a CEL transition rule, enforced by the API "
           "server rather than by a webhook being reachable")

    joined = " ".join(r.get("rule", "") for r in rules)
    if "crossplane.compositionRef" not in joined:
        bad("no CEL rule requires spec.crossplane.compositionRef. Without one, a "
            "SpokePool that reaches the API server WITHOUT passing through "
            "hub-operator's mutating webhook -- unregistered, unreachable, or a "
            "provider the registry cannot resolve -- is admitted, and Crossplane "
            "picks a composition for it")
    else:
        ok("a CEL rule requires an exact composition reference, so a SpokePool that "
           "missed the platform's injection is refused by the API server")

    # ── 5 & 6. The claim templates declare only what the XRD declares ────────
    templates = sorted(TEMPLATES.glob("spokepool-*.yaml"))
    if not templates:
        bad(f"no SpokePool claim templates under {TEMPLATES}; this check would pass "
            f"vacuously")
        return
    for path in templates:
        doc = yaml.safe_load(path.read_text())
        spec = doc.get("spec") or {}
        legacy = sorted(set(spec) & set(LEGACY_SPEC_FIELDS))
        if legacy:
            bad(f"{path.name} declares {', '.join('spec.' + f for f in legacy)}, which "
                f"a Cluster scoped XR carries under spec.crossplane. The API server "
                f"prunes it before any webhook sees it, and ArgoCD refuses the whole "
                f"file with 'field not declared in schema'")
        undeclared = sorted(set(spec) - declared)
        if undeclared:
            bad(f"{path.name} declares {', '.join('spec.' + f for f in undeclared)}, "
                f"which the XRD does not declare. This is the question ArgoCD's typed "
                f"patch asks before it syncs")
        if "crossplane" in spec:
            bad(f"{path.name} declares spec.crossplane. Which composition builds a "
                f"cluster is the platform's to resolve from spec.provider; a tenant "
                f"file naming it is a second declaration of one fact")
        if "provider" not in spec:
            bad(f"{path.name} declares no spec.provider, so nothing identifies which "
                f"composition should build it")
        if doc.get("metadata", {}).get("namespace"):
            bad(f"{path.name} sets metadata.namespace on a cluster-scoped XR, which "
                f"the API server ignores -- so it reads as a placement that is not one")
    if not any(o[0] == "BAD" for o in out[-len(templates):]):
        ok(f"{len(templates)} claim template(s) declare a provider, no Crossplane "
           f"machinery, and only fields the XRD declares")

    # ── 7. Every registry mapping resolves to a composition that exists ──────
    registry = yaml.safe_load(REGISTRY.read_text()).get("data") or {}
    if not registry:
        bad(f"{REGISTRY} declares no provider mapping, so no workload cluster could "
            f"be admitted on any box")
        return
    xr_api_version = f"{xrd['spec']['group']}/{version['name']}"
    xr_kind = xrd["spec"]["names"]["kind"]

    cells = {}
    for provider in sorted(set(registry) | {p.name for p in Path(PROVIDER_CELL).iterdir() if p.is_dir()}):
        cell = Path(PROVIDER_CELL) / provider
        if not cell.is_dir():
            continue
        docs = render(cell)
        if docs is None:
            return
        cells[provider] = {
            d["metadata"]["name"]: d for d in docs if d.get("kind") == "Composition"
        }

    for provider, composition in sorted(registry.items()):
        home = cells.get(provider)
        if home is None:
            bad(f"the registry maps provider {provider!r} to {composition!r} and there "
                f"is no {PROVIDER_CELL}/{provider} cell to deliver it")
            continue
        comp = home.get(composition)
        if comp is None:
            bad(f"the registry maps provider {provider!r} to composition "
                f"{composition!r}, which {PROVIDER_CELL}/{provider} does not deliver. "
                f"hub-operator would inject a reference to a composition the box does "
                f"not carry and the cluster would never be built")
            continue
        ref = comp["spec"].get("compositeTypeRef", {})
        if ref.get("apiVersion") != xr_api_version or ref.get("kind") != xr_kind:
            bad(f"composition {composition!r} composes "
                f"{ref.get('apiVersion')} {ref.get('kind')}, not {xr_api_version} "
                f"{xr_kind}, so it cannot build a workload cluster")
            continue
        ok(f"provider {provider!r} resolves to {composition!r}, which this cell "
           f"delivers and which composes {xr_kind}")

    # ── 8. The CLI accepts exactly the providers the registry can resolve ────
    values = yaml.safe_load(CHART_VALUES.read_text()) or {}
    supported = set(values.get("supportedProviders") or [])
    if not supported:
        bad(f"{CHART_VALUES} declares no supportedProviders, so the set of providers "
            f"a cluster may declare is unknown and this check would pass vacuously")
        return
    for p in sorted(supported - set(registry)):
        bad(f"a cluster may declare provider {p!r} and the provider registry cannot "
            f"resolve it to a composition, so hub-operator would refuse every such "
            f"cluster at admission")
    for p in sorted(set(registry) - supported):
        bad(f"the provider registry resolves {p!r} and no cluster may declare it, so "
            f"the mapping names a composition nothing can select")
    if supported == set(registry):
        ok(f"the providers a cluster may declare and the providers the registry "
           f"resolves are the same set: {', '.join(sorted(supported))}")

    # ── 9. A box carries the composition for every provider it may declare ───
    #
    # The box's provider and a workload cluster's provider are different facts.
    # A cell is synced by the BOX's provider (03-platform-services-appset.yaml),
    # and `kakt tenant add-cluster` permits any supported provider except a
    # premises-bound one on a box that is not itself that provider.
    rule_src = RULE_SOURCE.read_text()
    m = re.search(r"var developmentOnlyProviders = map\[string\]bool\{([^}]*)\}", rule_src)
    if not m:
        bad(f"could not read developmentOnlyProviders from {RULE_SOURCE}, so which "
            f"provider a box may declare cannot be established here")
        return
    premises_bound = set(re.findall(r'"([^"]+)":\s*true', m.group(1)))

    for box in sorted(supported):
        if box not in cells:
            continue
        for workload in sorted(supported):
            if premises_bound and workload in premises_bound and workload != box:
                continue  # add-cluster refuses it: the box has no premises
            composition = registry.get(workload)
            if composition and composition not in cells[box]:
                bad(f"a {box} box may declare a {workload} workload cluster and does "
                    f"not carry {composition!r}: {PROVIDER_CELL}/{box} delivers "
                    f"{', '.join(sorted(cells[box])) or 'no composition'}. The "
                    f"reference hub-operator injects would name a composition the box "
                    f"has never seen")
            elif composition:
                ok(f"a {box} box may declare a {workload} workload cluster and carries "
                   f"{composition!r}")

    # ── 10. The admission path is registered AND delivered ───────────────────
    main_src = OPERATOR_MAIN.read_text()
    for symbol, why in (
        ("LoadProviderRegistry", "the provider registry is never read, so no provider "
                                 "resolves to anything"),
        ("SpokePoolDefaulter", "nothing injects the composition reference"),
        ("NewSpokePoolValidator", "nothing refuses a wrong one"),
    ):
        if symbol not in main_src:
            bad(f"{OPERATOR_MAIN} does not reference {symbol}, so {why}. The webhook "
                f"implementations existed unregistered from a56502c4 until "
                f"2026-11-22, and a source file without a registered endpoint is not "
                f"an enforced contract")
    if all(s in main_src for s in ("LoadProviderRegistry", "SpokePoolDefaulter", "NewSpokePoolValidator")):
        ok("hub-operator loads the provider registry and registers both SpokePool "
           "admission webhooks")

    docs = render(OPERATOR_CONFIG)
    if docs is None:
        return
    services = {d["metadata"]["name"] for d in docs if d.get("kind") == "Service"}
    configured = [d for d in docs
                  if d.get("kind") in ("MutatingWebhookConfiguration",
                                       "ValidatingWebhookConfiguration")]
    wanted = {"MutatingWebhookConfiguration", "ValidatingWebhookConfiguration"}
    found = {d["kind"] for d in configured}
    if found != wanted:
        delivered = ", ".join(sorted(found)) or "no admission configuration"
        bad(f"{OPERATOR_CONFIG} delivers {delivered}. A box received the serving "
            f"certificate and neither configuration until 2026-11-22: cert-manager "
            f"renewed a certificate for a Service that did not exist, and nothing "
            f"enforced the provider mapping")
    for d in configured:
        for w in d["webhooks"]:
            svc = w["clientConfig"]["service"]
            if svc["name"] not in services:
                bad(f"{d['kind']} {w['name']} points at Service {svc['name']!r}, which "
                    f"{OPERATOR_CONFIG} does not deliver, so the API server has "
                    f"nothing to call")
            if w.get("failurePolicy") != "Fail":
                bad(f"{d['kind']} {w['name']} has failurePolicy={w.get('failurePolicy')}. "
                    f"Ignore means a SpokePool created while hub-operator is down is "
                    f"admitted without a composition reference")
            if not d["metadata"].get("annotations", {}).get("cert-manager.io/inject-ca-from"):
                bad(f"{d['kind']} carries no cert-manager.io/inject-ca-from annotation, "
                    f"so its caBundle is never filled and the API server cannot verify "
                    f"the endpoint it is told to call")
    if found == wanted:
        ok("both SpokePool admission configurations are delivered, fail closed, and "
           "name a Service and a CA this operator's tree provides")


main()
for level, message in out:
    print(f"{level}\t{message}")
