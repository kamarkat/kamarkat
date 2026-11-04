#!/usr/bin/env python3
"""Fail a release whose bundle generates Applications that cannot be deployed.

Rendering successfully is not the same as being deployable, and three defects
reached published releases through exactly that gap: Applications naming charts
no release published, a chart carrying one cluster's Infisical project IDs, and
an Application naming a Helm chart and a Kustomize block together -- which
ArgoCD refuses with "multiple application sources defined" and which no gate
noticed because the chart itself rendered.

Every ApplicationSet a bundle generates is expanded into the Applications it
would create, and each is checked as a spec rather than as text:

  1. source types are mutually exclusive     -- ArgoCD permits one per source
  2. the chart it names is published         -- a reference to nothing
  3. it resolves to at least one object      -- Healthy while managing nothing
  4. no runtime dependency on platform git   -- ADR-063 custody
  5. it renders for the topology given       -- variants must resolve
  6. the spec is structurally valid          -- required fields, shapes

Reads the released render on stdin.

Usage: helm template ... | application-specs.py <chart-dir> [<chart-dir> ...]
Exit 0 deployable, 1 not.
"""
import os
import sys

try:
    import yaml
except ImportError:
    sys.exit("PyYAML required")

# ArgoCD keys the source type off which of these fields is present, and refuses
# a source carrying more than one (application/v1alpha1/types.go).
SOURCE_TYPES = ("chart", "kustomize", "helm", "directory", "plugin")


# Where this platform publishes. A chart from anywhere else is third-party: its
# name and version are pinned by its descriptor and it is not ours to publish,
# so checking it against what this release produced would report every upstream
# chart as missing.
PLATFORM_REGISTRY = "ghcr.io/soloz-io/charts"

# The one published artefact (ADR-063). Every platform-owned Application names
# it and enables its own component through values.
DISTRIBUTION_CHART = "platform"

# The platform's own repository. A released Application resolving it is the
# runtime dependency ADR-063 forbids.
PLATFORM_GIT = "github.com/kamarkat/kamarkat"

# The organisation the platform controls. ADR-063 forbids a released bundle from
# resolving "the platform's repository" at runtime, and the check below read that
# as one repository -- zero-ops. It is the organisation.
#
# soloz-io/fleet-registry is the instance repository for the platform's OWN box
# (ADR-062), and the environment-manager chart carried it as the default for
# instanceRepoURL (then named fleetRegistryRepoURL). Every released bundle
# naming a repository the platform controls and no tenant can read, through
# several versions, past this gate -- because the string "zero-ops" was not in it.
#
# A tenant's own repository is not a violation: it is the box reading its own
# state, which is what ADR-062 requires. What is forbidden is a repository on the
# platform's side of the boundary.
PLATFORM_ORG = "github.com/soloz-io/"


def is_registry(url):
    """Whether a repoURL names a chart registry rather than a git repository.

    ArgoCD decides how to fetch a Helm source from the shape of the URL, and so
    does this: a scheme-less host is pulled as an OCI artefact. Keying on an
    "oci://" prefix instead would have made every check below silently stop
    applying the moment the scheme was dropped -- which is what the platform had
    to do to make ArgoCD pull these charts at all.
    """
    return bool(url) and not url.startswith(("http://", "https://", "git@"))


def published(dirs):
    """Chart names this release publishes, by what Chart.yaml declares."""
    names = {}
    for directory in dirs:
        if not os.path.isdir(directory):
            continue
        for entry in sorted(os.listdir(directory)):
            meta = os.path.join(directory, entry, "Chart.yaml")
            if not os.path.exists(meta):
                continue
            with open(meta) as handle:
                name = (yaml.safe_load(handle) or {}).get("name")
            if name:
                names[name] = entry
    return names


def sources_of(spec):
    single = spec.get("source")
    return ([single] if single else []) + list(spec.get("sources") or [])


def check(appset, element, spec, charts, components, problems):
    name = element.get("appName") or appset
    sources = sources_of(spec)
    if not sources:
        problems.append((name, "the Application declares no source"))
        return

    for source in sources:
        # 1. Mutually exclusive source types. `helm` alongside `chart` is the
        # normal way to pass values and is not a conflict; the others are.
        present = [t for t in SOURCE_TYPES if source.get(t)]
        conflicting = [t for t in present if t != "helm"]
        if len(conflicting) > 1:
            problems.append((name, "multiple application sources defined: "
                                   + ",".join(conflicting)))

        repo = source.get("repoURL", "")
        chart = source.get("chart")

        # 4. Runtime dependency on a repository the platform controls.
        if repo and not is_registry(repo) and PLATFORM_ORG in repo:
            problems.append((name, f"resolves platform git at runtime: {repo}. "
                                   f"A released bundle carries what it needs "
                                   f"(ADR-063); a box reads its own repository "
                                   f"and never the platform's (ADR-062)"))

        # 2. A chart this release does not publish.
        if chart and repo == PLATFORM_REGISTRY and chart not in charts:
            problems.append((name, f"names chart {chart!r}, which this release "
                                   f"does not publish"))

        # 6. A chart source carrying a scheme. ArgoCD decides how to fetch a
        # Helm source from the shape of this URL: a scheme-less host is pulled
        # as an OCI artefact, and anything else goes to `helm pull --repo`,
        # which is the classic chart repository protocol and does not speak OCI.
        # An "oci://" prefix therefore reads as a classic repository whose host
        # happens to be a registry, and every Application fails with "not a
        # valid chart repository or cannot be reached: object required" -- a
        # message about the registry, for a registry that is reachable and
        # correct. Every platform-owned Application failed this way once.
        if chart and repo.startswith("oci://"):
            problems.append((name, f"chart source repoURL carries an oci:// "
                                   f"scheme ({repo}); ArgoCD would fetch it as a "
                                   f"classic Helm repository and fail"))

        # 6. Structural: an OCI source needs a version to resolve.
        if chart and is_registry(repo) and not source.get("targetRevision"):
            problems.append((name, "registry source has no targetRevision"))

        # 6. A chart source carrying a path is rejected by ArgoCD.
        if chart and source.get("path"):
            problems.append((name, "source declares both chart and path"))

    # An Application resolving the distribution must enable the component it
    # owns, and that component must exist in the distribution. Neither is
    # checked by anything else: the chart name is "platform" for all of them, so
    # a component that is not enabled, or enabled under a name the distribution
    # does not carry, renders nothing -- and ArgoCD reports the Application
    # Synced and Healthy while it manages zero resources. platform-database
    # shipped exactly that way.
    for source in sources:
        if source.get("chart") != DISTRIBUTION_CHART:
            continue
        values = (source.get("helm") or {}).get("values") or ""
        # A COMMENT is never a component.
        #
        # This read any unindented line ending in ":" as an enabled component,
        # and a values block's comments sit at that same level. A comment whose
        # sentence happened to end in a colon was therefore reported as a
        # component the distribution does not carry, and the release failed
        # naming a fragment of English prose. Observed 2026-09-20.
        enabled = [line.split(":")[0].strip()
                   for line in values.splitlines()
                   if line and not line[0].isspace()
                   and not line.lstrip().startswith("#")
                   and line.rstrip().endswith(":")]
        enabled = [e for e in enabled if e and e != "global"]
        if not enabled:
            problems.append((name, "resolves the distribution but enables no "
                                   "component; it would render nothing and "
                                   "report healthy"))
        for component in enabled:
            if component not in components:
                problems.append((name, f"enables component {component!r}, which "
                                       f"the distribution does not carry"))

    dest = spec.get("destination") or {}
    if not (dest.get("server") or dest.get("name")):
        problems.append((name, "destination names neither server nor name"))
    if not spec.get("project"):
        problems.append((name, "Application declares no project"))


def main() -> int:
    if len(sys.argv) < 2:
        print(__doc__)
        return 2
    charts = published(sys.argv[1:])
    components = set()
    for directory in sys.argv[1:]:
        sub = os.path.join(directory, DISTRIBUTION_CHART, "charts")
        if os.path.isdir(sub):
            components |= {n for n in os.listdir(sub)
                           if os.path.exists(os.path.join(sub, n, "Chart.yaml"))}

    problems = []
    checked = 0
    for doc in yaml.safe_load_all(sys.stdin):
        if not doc or doc.get("kind") not in ("ApplicationSet", "Application"):
            continue
        if doc["kind"] == "Application":
            checked += 1
            check(doc["metadata"]["name"], {}, doc["spec"], charts, components, problems)
            continue

        appset = doc["metadata"]["name"]
        template = (doc["spec"].get("template") or {}).get("spec") or {}

        # templatePatch is applied by ArgoCD after the template and can add a
        # source type the template does not have. That is how every
        # directoryRecurse component ended up with a directory block beside a
        # chart and was refused with "multiple application sources defined:
        # Helm,Directory" -- invisible to a check that reads only the template.
        patch = doc["spec"].get("templatePatch") or ""

        # A patch adding `sources` to a chart-sourced template is the worst of
        # these, because ArgoCD does not report it. GetSources returns
        # spec.sources whenever it is non-empty and never consults spec.source,
        # so the Application resolves the array and silently ignores the chart it
        # was given -- Synced, Healthy, and running what the bundle stopped
        # shipping. Both boundaries did this, and the cluster resolved the
        # platform's own repository for three published versions before a
        # cluster-side check found it.
        if "sources:" in patch and template.get("source", {}).get("chart"):
            problems.append((appset, "templatePatch adds `sources` to an "
                                     "Application whose source is a chart; ArgoCD "
                                     "resolves the array and ignores the chart"))
        if PLATFORM_GIT in patch:
            problems.append((appset, f"templatePatch names the platform "
                                     f"repository ({PLATFORM_GIT}); a released "
                                     f"cluster would resolve it at runtime"))

        for kind in ("directory", "kustomize", "plugin"):
            if f"{kind}:" in patch and template.get("source", {}).get("chart"):
                problems.append((appset, f"templatePatch adds a {kind} source to "
                                         f"an Application whose source is a chart; "
                                         f"ArgoCD refuses both"))
        elements = [e for g in doc["spec"].get("generators", [])
                    for e in (g.get("list") or {}).get("elements", []) or []]
        if not elements:
            # A generator this cannot expand -- cluster, git, pullRequest. Its
            # template is still checked, because a conflict there applies to
            # every Application it will ever generate.
            checked += 1
            check(appset, {}, template, charts, components, problems)
            continue
        for element in elements:
            checked += 1
            resolved = resolve(template, element)
            check(appset, element, resolved, charts, components, problems)

    if problems:
        print(f"application specs: {len(problems)} problem(s) in {checked} "
              f"Application(s)")
        for name, detail in problems:
            print(f"  {name}: {detail}")
        print("Rendering is not deploying: ArgoCD would reject or silently "
              "manage nothing.")
        return 1
    print(f"application specs: {checked} Application(s), all deployable")
    return 0


def resolve(template, element):
    """Expand an ApplicationSet template the way ArgoCD's generator does.

    The templates carry conditionals, not just references -- `path` is emitted
    only when the element names its own repoURL, and a checker that ignored the
    condition would report every platform-owned Application as declaring both a
    chart and a path. Go text/template is what ArgoCD runs, so this runs it too
    rather than approximating it.
    """
    import copy
    import json
    import subprocess

    def walk(node):
        if isinstance(node, dict):
            return {k: walk(v) for k, v in node.items()}
        if isinstance(node, list):
            return [walk(v) for v in node]
        if isinstance(node, str) and "{{" in node:
            return _render(node, element)
        return node

    return walk(copy.deepcopy(template))


_GO = None


def _render(text, element):
    """Render one field through Go's template engine, as ArgoCD does."""
    global _GO
    if _GO is None:
        _GO = _start_helper()
    if _GO is False:
        return text
    import json
    try:
        _GO.stdin.write(json.dumps({"t": text, "d": element}) + "\n")
        _GO.stdin.flush()
        line = _GO.stdout.readline()
        if not line:
            return text
        result = json.loads(line)
        return result.get("out", text)
    except Exception:
        return text


def _start_helper():
    """A tiny Go program that renders a template against an element.

    Falls back to leaving fields untouched when Go is unavailable, because a
    checker that cannot expand a template should say less rather than say
    something wrong -- an unexpanded field carries braces and matches none of
    the checks that would otherwise fire on it.
    """
    import shutil
    import subprocess
    import tempfile

    if not shutil.which("go"):
        return False
    source = """package main
import ("bufio";"encoding/json";"os";"strings";"text/template")
type in struct{ T string; D map[string]interface{} }
func main(){
  r:=bufio.NewScanner(os.Stdin); r.Buffer(make([]byte,1<<20),1<<20)
  w:=bufio.NewWriter(os.Stdout); defer w.Flush()
  for r.Scan(){
    var q in
    if json.Unmarshal(r.Bytes(),&q)!=nil { continue }
    out:=q.T
    if t,err:=template.New("x").Parse(q.T); err==nil {
      var b strings.Builder
      if t.Execute(&b,q.D)==nil { out=b.String() }
    }
    b,_:=json.Marshal(map[string]string{"out":out})
    w.Write(b); w.WriteByte('\\n'); w.Flush()
  }
}
"""
    directory = tempfile.mkdtemp()
    path = os.path.join(directory, "render.go")
    with open(path, "w") as handle:
        handle.write(source)
    try:
        return subprocess.Popen(["go", "run", path], stdin=subprocess.PIPE,
                                stdout=subprocess.PIPE, text=True)
    except Exception:
        return False


if __name__ == "__main__":
    sys.exit(main())
