#!/usr/bin/env python3
"""Fold the packaged component charts into the one published distribution.

ADR-063: a version names one artefact. Components are internal structure, and
the environment and provider a cluster runs are values supplied to it rather
than names it resolves.

This reads the charts the existing packagers produce -- each already verified to
render what the cluster applies today -- and folds them into a single umbrella
chart whose subcharts are those components. Reusing their output rather than
re-rendering keeps that verification meaningful: what ships is what was checked.

Components whose content varies by environment or provider become one subchart
carrying every variant, deduplicated by document content. The variation is tiny
and the shared part is not: the spoke catalogue's six combinations differed by
thirty lines out of two hundred and three thousand while each carried the same
12.8MB of vendored CRDs, so publishing them separately shipped seventy megabytes
to express a nodeSelector. Deduplicating by content means this does not depend on
knowing which part of a component varies, which changes whenever an overlay does.

A component that is already a Helm chart is copied with its templates intact.
Its content depends on values only a cluster can supply -- a gateway's
external-dns target, a tenant's issuer -- so pre-rendering it here would bake one
cluster's answer into every cluster's copy, which is what ADR-062 forbids and
what the first release of hub-environment-dev actually did.

Usage: distribution.py <version> <packaged-charts-dir> <out-dir>
"""
import hashlib
import os
import re
import shutil
import sys

# Components the existing packagers publish per combination. The suffix is the
# variant; what remains is the component a boundary asks for.
#
# Listed rather than inferred: platform-cert-manager would otherwise read as
# component "platform-cert" with variant "manager", and the result would be a
# component that renders nothing rather than an error.
# "platform-spoke-pools" is deliberately absent: the per-combination spoke-pool
# charts are no longer packaged (see inline-charts.sh), so a prefix for them would
# describe a variant no release contains.
VARIANT_PREFIXES = (
    "platform-spoke-catalog",
    "hub-environment",
    "infrastructure-provider",
)

SKIP = {"platform-bundle", "platform", "environment-manager"}


def sha(text):
    return hashlib.sha256(text.encode()).hexdigest()


def split_variant(name, provider_charts):
    for prefix in VARIANT_PREFIXES:
        if name == prefix:
            return name, None
        if name.startswith(prefix + "-"):
            return prefix, name[len(prefix) + 1:]
    if name in provider_charts:
        return provider_charts[name]
    return name, None


def documents(chart_dir):
    """Every YAML document a pre-rendered chart carries, in file order."""
    docs = []
    files_dir = os.path.join(chart_dir, "files")
    if not os.path.isdir(files_dir):
        return docs
    for name in sorted(os.listdir(files_dir)):
        if not name.endswith((".yaml", ".yml")):
            continue
        with open(os.path.join(files_dir, name)) as handle:
            for chunk in handle.read().split("\n---\n"):
                if chunk.strip():
                    docs.append(chunk.strip("\n"))
    return docs


def is_real_chart(directory):
    """A chart whose templates do more than emit files verbatim."""
    templates = os.path.join(directory, "templates")
    if not os.path.isdir(templates):
        return False
    return any(n != "content.yaml" for n in os.listdir(templates))


def write_docs(directory, docs):
    """Write documents under Helm's 5 MiB per-file limit, split on boundaries."""
    if not docs:
        return 0
    os.makedirs(directory, exist_ok=True)
    limit = 4 * 1024 * 1024
    part, size, index = [], 0, 0
    for doc in docs:
        if size + len(doc) + 5 > limit and part:
            _flush(directory, index, part)
            part, size, index = [], 0, index + 1
        part.append(doc)
        size += len(doc) + 5
    if part:
        _flush(directory, index, part)
    return len(docs)


def _flush(directory, index, part):
    with open(os.path.join(directory, f"{index:03d}.yaml"), "w") as handle:
        handle.write("\n---\n".join(part) + "\n")


def set_version(path, version):
    with open(path) as handle:
        text = handle.read()
    text = re.sub(r"(?m)^version:.*$", f"version: {version}", text)
    text = re.sub(r"(?m)^appVersion:.*$", f'appVersion: "{version}"', text)
    with open(path, "w") as handle:
        handle.write(text)


def write_subchart(sub, component, version, has_variants):
    with open(os.path.join(sub, "Chart.yaml"), "w") as handle:
        handle.write(
            "apiVersion: v2\n"
            f"name: {component}\n"
            f"description: {component}, a component of the platform distribution\n"
            "type: application\n"
            f"version: {version}\n"
            f'appVersion: "{version}"\n'
        )
    os.makedirs(os.path.join(sub, "templates"), exist_ok=True)
    # content.yaml emits the verbatim half. Any other template in this directory
    # came from the component's own chart and is left alone.
    with open(os.path.join(sub, "templates", "content.yaml"), "w") as handle:
        handle.write(SUBCHART_TEMPLATE if has_variants else PLAIN_TEMPLATE)


def write_umbrella(chart, version, components):
    deps = "".join(
        f"  - name: {c}\n    version: {version}\n    condition: {c}.enabled\n"
        for c in components)
    with open(os.path.join(chart, "Chart.yaml"), "w") as handle:
        handle.write(
            "apiVersion: v2\n"
            "name: platform\n"
            "description: The SOLOZ platform distribution -- every component, "
            "with supported topology profiles\n"
            "type: application\n"
            f"version: {version}\n"
            f'appVersion: "{version}"\n'
            "dependencies:\n" + deps
        )
    enabled = "".join(f"{c}:\n  enabled: false\n" for c in components)
    with open(os.path.join(chart, "values.yaml"), "w") as handle:
        handle.write(VALUES_HEADER + enabled)


def main() -> int:
    if len(sys.argv) != 4:
        print(__doc__)
        return 2
    version, src, out = sys.argv[1], sys.argv[2], sys.argv[3]

    names = [n for n in sorted(os.listdir(src))
             if os.path.exists(os.path.join(src, n, "Chart.yaml"))]

    # Provider-suffixed charts from boundary 02, discovered rather than listed:
    # their component names are arbitrary, so a suffix is a variant only when the
    # same component appears under more than one provider.
    # A provider suffix is a variant whether or not a sibling exists. Requiring
    # two meant a component built for one provider kept the suffix in its
    # subchart name, while the boundary enables it by appName -- so the
    # Application enabled a component the distribution does not carry, rendered
    # nothing, and reported healthy. platform-redis is built for hetzner only.
    provider_charts = {}
    for name in names:
        for provider in ("hetzner", "hybrid"):
            if name.endswith("-" + provider):
                provider_charts[name] = (name[: -(len(provider) + 1)], provider)

    chart = os.path.join(out, "platform")
    shutil.rmtree(chart, ignore_errors=True)
    os.makedirs(os.path.join(chart, "charts"))
    os.makedirs(os.path.join(chart, "templates"))

    grouped = {}
    for name in names:
        if name in SKIP:
            continue
        component, variant = split_variant(name, provider_charts)
        grouped.setdefault(component, {})[variant] = os.path.join(src, name)

    total = written = 0
    for component, variants in sorted(grouped.items()):
        sub = os.path.join(chart, "charts", component)
        real = [d for d in variants.values() if is_real_chart(d)]
        if real and len(variants) == 1:
            shutil.copytree(real[0], sub)
            set_version(os.path.join(sub, "Chart.yaml"), version)
            continue

        if real:
            # Both templated and varying: the spoke catalogue is reconciled by
            # every spoke, so a few fields come from values, and its content
            # still differs by environment and provider. The templates are the
            # same across variants -- they are what the packager derived from
            # one declaration -- so they are taken once and the verbatim content
            # is deduplicated as usual. A variant whose templates differed would
            # mean the declaration is not shared, which is a packaging fault
            # rather than something to merge silently.
            # A templated object can also vary by topology -- the catalogue's
            # CNPG Cluster carries both a per-spoke backup path and the
            # placement pair -- so taking the templates from one variant would
            # give every spoke that variant's placement. Each variant keeps its
            # own template, guarded so only the matching one emits.
            os.makedirs(os.path.join(sub, "templates"))
            for variant, directory in sorted(variants.items()):
                for name in sorted(os.listdir(os.path.join(directory, "templates"))):
                    if name == "content.yaml":
                        continue
                    with open(os.path.join(directory, "templates", name)) as handle:
                        body = handle.read()
                    # A variant is not always environment-and-provider. The
                    # spoke catalogue varies by both and its variants read
                    # "dev-hetzner"; a provider overlay reads "hetzner". (hub-environment
                    # used to vary by environment and read "dev"; since ADR-111
                    # Revision 7 the hub has no environment, so it ships as one
                    # unsuffixed component and reaches split_variant with no
                    # variant at all.) Tested
                    # against the env-provider pair only, the second and third
                    # never matched, so the object was carried into the
                    # distribution and emitted by nothing -- the component
                    # rendered one object where standalone it rendered two, which
                    # is what verify-distribution.py caught.
                    guard = ('{{- $v := printf "%s-%s" '
                             '(.Values.global.environmentSlug | default "") '
                             '(.Values.global.provider | default "") }}\n'
                             '{{- if or '
                             f'(eq $v "{variant}") '
                             f'(eq (.Values.global.environmentSlug | default "") "{variant}") '
                             f'(eq (.Values.global.provider | default "") "{variant}") }}}}\n')
                    target = os.path.join(sub, "templates", f"{variant}-{name}")
                    with open(target, "w") as handle:
                        handle.write(guard + body + "\n{{- end }}\n")

        os.makedirs(sub, exist_ok=True)
        by_variant = {v: documents(d) for v, d in variants.items()}
        total += sum(len(d) for d in by_variant.values())

        if list(by_variant) == [None]:
            written += write_docs(os.path.join(sub, "files"), by_variant[None])
            write_subchart(sub, component, version, False)
            continue

        counts = {}
        for docs in by_variant.values():
            for digest in {sha(d) for d in docs}:
                counts[digest] = counts.get(digest, 0) + 1
        shared = {d for d, c in counts.items() if c == len(by_variant)}
        first = True
        for variant, docs in sorted(by_variant.items(), key=lambda kv: kv[0] or ""):
            if first:
                written += write_docs(os.path.join(sub, "files", "_shared"),
                                      [d for d in docs if sha(d) in shared])
                first = False
            written += write_docs(os.path.join(sub, "files", variant),
                                  [d for d in docs if sha(d) not in shared])
        write_subchart(sub, component, version, True)

    write_umbrella(chart, version, sorted(grouped))
    print(f"packaged platform -> {chart}")
    print(f"  {len(grouped)} component(s), {written} document(s) written, "
          f"{total - written} duplicate(s) shared")
    return 0


VALUES_HEADER = """# The platform distribution.
#
# A chart reference names a chart and not a subchart within one (ADR-063), so an
# Application resolving any part of the platform resolves all of it and enables
# the component it owns. Every component is disabled here: an Application that
# forgets to enable one renders nothing, which ArgoCD reports Healthy while
# managing nothing, so each boundary sets its own.
#
# Topology reaches components as globals, because a component that varies by
# environment or provider needs them and the Application enabling it should not
# have to know which components those are.
global:
  environmentSlug: ""
  provider: ""

"""

PLAIN_TEMPLATE = """{{- /*
Emit this component verbatim.

Content lives under files/ rather than templates/ because Helm templates
everything in templates/, and platform manifests legitimately contain Go
template delimiters -- an ExternalSecret's {{ .apikey }} rendered once to an
empty string, blanking a credential silently.
*/ -}}
{{- range $path, $_ := .Files.Glob "files/*.yaml" }}
---
{{ $.Files.Get $path }}
{{- end }}
"""

SUBCHART_TEMPLATE = """{{- /*
Emit this component for the topology the cluster runs.

Shared content is what every variant carries; a variant directory holds only
what differs. The split is by document content rather than by knowing which part
varies, so an overlay changing what it patches needs no change here.

A topology with no variant directory emits the shared content alone. That is a
real state -- a component may be identical everywhere the platform supports it --
and distinct from a component that is absent, which the umbrella refuses.
*/ -}}
{{- $env := .Values.global.environmentSlug | default "" -}}
{{- $provider := .Values.global.provider | default "" -}}
{{- range $path, $_ := .Files.Glob "files/_shared/*.yaml" }}
---
{{ $.Files.Get $path }}
{{- end }}
{{- $candidates := list -}}
{{- if and $env $provider }}{{ $candidates = append $candidates (printf "%s-%s" $env $provider) }}{{ end }}
{{- if $env }}{{ $candidates = append $candidates $env }}{{ end }}
{{- if $provider }}{{ $candidates = append $candidates $provider }}{{ end }}
{{- $done := false -}}
{{- range $variant := $candidates }}
{{- if not $done }}
{{- range $path, $_ := $.Files.Glob (printf "files/%s/*.yaml" $variant) }}
{{- $done = true }}
---
{{ $.Files.Get $path }}
{{- end }}
{{- end }}
{{- end }}
{{- range $path, $_ := .Files.Glob "files/*.yaml" }}
---
{{ $.Files.Get $path }}
{{- end }}
"""


if __name__ == "__main__":
    sys.exit(main())
