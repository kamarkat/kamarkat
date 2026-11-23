#!/usr/bin/env python3
"""One Infisical project per workload cluster, agreed on by everything (ADR-031, ADR-037).

The project is the only secret boundary an unlicensed Infisical enforces. A
built-in role carries no environment or path condition -- `viewer` grants
DescribeSecret and ReadValue on Secrets across its whole project -- and
conditioning one needs PlanFeature.Rbac, false on the default plan along with
secretsFolderRbac. So `environmentSlug` and /spoke-pool/<cluster>/ express
ownership, and which project an identity belongs to is what enforces it.

That makes the project SLUG a contract between three places that cannot see each
other:

  * hub-operator creates the project and materialises the credentials into it,
  * the ApplicationSet points each spoke's ClusterSecretStore at it,
  * and the identity operator scopes the spoke's identity to it.

The third reads the slug from the SpokeMachineIdentity rather than recomputing
it, so only the first two can disagree -- and if they do, the store reads a
project nobody wrote to while the identity is a viewer of a project nothing
reads. Both halves look correct in isolation.

Emits tab-separated lines the shell wrapper classifies:
    OK<TAB><message>    BAD<TAB><message>
"""
import re
from pathlib import Path

HUB_SOURCE = Path("operators/hub-operator/internal/secrets/cluster_project.go")
APPSET = Path("manifests/argocd/environment-manager/templates/03-platform-services-appset.yaml")
# THE RELEASED PATH. A per-spoke field on the spoke catalogue is delivered twice:
# the ApplicationSet's kustomize patches when the box runs unreleased manifests,
# and these field rules when it runs a packaged chart -- because an ArgoCD source
# naming a Helm chart cannot also carry patches (ADR-063).
#
# This file was not read here, and the omission cost a release: the store's
# projectSlug was patched on the ApplicationSet alone, so every check agreed while
# the packaged chart -- what a released box actually runs -- still named
# hub-secrets. On nutgraf-stg-01 the identity was narrowed correctly and then the
# store could not validate, because it was pointed at a project the identity is
# deliberately not a member of.
TEMPLATED_FIELDS = Path("manifests/spoke/spoke-catalog/templated-fields.yaml")
IDENTITY_SCOPE = Path("operators/spoke-identity-operator/internal/controller/workload_identity_scope.go")
PULL_SECRET = Path("manifests/tenants/charts/universal-tenant/templates/pull-secret-es.yaml")

out = []


def ok(m):
    out.append(("OK", m))


def bad(m):
    out.append(("BAD", m))


def main():
    hub = HUB_SOURCE.read_text()

    # The formula, read from the one place that owns it.
    m = re.search(r'func ClusterProjectSlug\([^)]*\)\s*string\s*\{\s*return fmt\.Sprintf\("([^"]+)"', hub)
    if not m:
        bad(f"ClusterProjectSlug is not a single Sprintf in {HUB_SOURCE}, so the slug the "
            f"ApplicationSet must match cannot be read")
        return
    formula = m.group(1)                       # e.g. spoke-%s-secrets
    rendered = formula.replace("%s", "{{ .name }}")
    ok(f"hub-operator derives a cluster's project slug as {formula!r}")

    # The ApplicationSet has to point the store at exactly that.
    appset = APPSET.read_text()
    patch = re.search(r"path: /spec/provider/infisical/secretsScope/projectSlug\s*\n\s*value: '([^']+)'",
                      appset)
    if not patch:
        bad(f"{APPSET.name} does not patch the store's projectSlug, so every spoke reads "
            f"whatever project the catalogue manifest hardcodes -- one project for the whole "
            f"fleet, which is the boundary this design replaces")
        return

    # The ApplicationSet writes ArgoCD's go-template escaped through Helm; compare
    # on the resolved shape rather than the escaping.
    declared = re.sub(r'\{\{"\{\{"\}\}\s*', "{{ ", patch.group(1))
    declared = re.sub(r'\s*\{\{"\}\}"\}\}', " }}", declared)
    if declared != rendered:
        bad(f"{APPSET.name} points each spoke's store at {declared!r} and hub-operator creates "
            f"{rendered!r}. The store would read a project nobody writes to while the identity "
            f"is scoped to a project nothing reads, and each half looks right on its own")
        return
    ok(f"the ApplicationSet points each spoke's store at the project hub-operator creates "
       f"({declared})")

    # The same field, on the path a released box runs.
    tf = TEMPLATED_FIELDS.read_text()
    tf_rule = re.search(r"path:\s*\[spec,\s*provider,\s*infisical,\s*secretsScope,\s*projectSlug\]"
                        r"\s*\n\s*value:\s*'([^']+)'", tf)
    if not tf_rule:
        bad(f"{TEMPLATED_FIELDS.name} carries no rule for the store's projectSlug, so the "
            f"PACKAGED chart keeps whatever the catalogue hardcodes. The ApplicationSet's "
            f"patches do not apply to a released box -- a Helm source cannot carry them -- so "
            f"the spoke's store names a project its identity is not a member of, and every "
            f"ExternalSecret on it fails at the store")
    else:
        released = tf_rule.group(1).replace("{{ .Values.spokeName }}", "%s").strip()
        if released != formula:
            bad(f"the released path points a spoke's store at {released!r} while hub-operator "
                f"creates {formula!r}. The two delivery paths must name the same project, or a "
                f"box reads one and the publisher writes the other depending only on whether it "
                f"runs a packaged chart")
        else:
            ok(f"the released path ({TEMPLATED_FIELDS.name}) names the same project as the "
               f"ApplicationSet and hub-operator, so a packaged box and an unreleased one "
               f"resolve the store identically")

    # The DNS credential travels the same two paths, and was migrated on one.
    dns = re.search(r"name:\s*hetzner-dns-credentials(.*?)(?=\n- kind:|\Z)", tf, re.S)
    if not dns:
        bad(f"{TEMPLATED_FIELDS.name} carries no rule for hetzner-dns-credentials, so the "
            f"packaged chart reads hcloud-token at the project ROOT. Under one project per "
            f"cluster the root holds nothing and the identity may not read it")
    elif "/spoke-pool/{{ .Values.spokeName }}/shared/hcloud-token" not in dns.group(1):
        bad(f"{TEMPLATED_FIELDS.name} templates hetzner-dns-credentials but not to this "
            f"cluster's own prefix")
    else:
        ok("the DNS credential is read from this cluster's own prefix on the released path too")

    # The identity operator must NOT carry a second formula.
    scope = IDENTITY_SCOPE.read_text()
    if "Sprintf" in scope and "secrets" in scope and "spoke-%s" in scope:
        bad(f"{IDENTITY_SCOPE.name} computes a project slug of its own. It must read "
            f"spec.infisical.secretsProjectSlug instead: that field is published by the "
            f"component that created the project, and is also the signal that the project "
            f"holds this cluster's credentials")
    elif "SecretsProjectSlug" not in scope:
        bad(f"{IDENTITY_SCOPE.name} does not read spec.infisical.secretsProjectSlug, so the "
            f"narrowing has no destination and no readiness signal")
    else:
        ok("the identity operator reads the published slug rather than recomputing it, so the "
           "formula has one home")

    # Consumers must read within the cluster prefix, not the project root. With one
    # project per cluster the root holds nothing a cluster may read.
    pull = PULL_SECRET.read_text()
    key = re.search(r"remoteRef:(?:.|\n)*?key: ([^\n]+)", pull)
    if not key:
        bad(f"{PULL_SECRET.name} declares no remoteRef key")
    elif "/spoke-pool/" not in key.group(1):
        bad(f"{PULL_SECRET.name} reads {key.group(1).strip()!r}, which is the project root. "
            f"A cluster's identity is a viewer of its own project and what it may read is what "
            f"hub-operator materialised under /spoke-pool/<cluster>/shared/")
    else:
        ok("the image-pull credential is read from this cluster's own prefix")


main()
for level, message in out:
    print(f"{level}\t{message}")
