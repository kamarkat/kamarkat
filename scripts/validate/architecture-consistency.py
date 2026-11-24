#!/usr/bin/env python3
"""Fail a release whose ADRs name components the repository does not deploy.

Three decisions have now been recorded without the mechanism that realises them.
ADR-067 decided support telemetry and named no channel, so scaffolding adopted
the nearest one and turned a tenant's Grafana Cloud account into a licence check.
ADR-064 decided promotion and named no reconciler, so merged promotions did
nothing and looked normal doing it. ADR-013 named VictoriaMetrics, Loki, an OTel
gateway and MetricCollector sidecars; those
were never written, and the one that was -- 577 lines of operator, VMCluster,
ingress and a working 211-line alert rule -- is referenced by no component and
has never been applied to a cluster.

Each was found by a person reading, long after release. This is the machine that
reads instead.

Four checks, and the first is the one that catches the orphan class:

  A  every directory under manifests/hub-core-services/ is reachable from some
     component descriptor or ApplicationSet path, or is waived by name with a
     reason. Orphaning a directory silently becomes impossible.
  B  every component an ADR names is registered.
  C  a component declared by an ACCEPTED ADR must be shipped and reachable.
     A Proposed ADR may declare planned components -- which makes accepting an
     ADR the moment its claims have to be true, rather than a status edit.
  D  every registry entry points at a path that exists.
  E  the bundle's capability declaration is well formed: a lifecycle state that
     exists, a date wherever one is promised, selection expressed as a value,
     and named interactions that are mutual.
  F  every acceptance test an ADR names exists AND is wired into something that
     runs it. An ADR that claims behaviour has to point at the executable proof
     of that behaviour, and a proof nobody runs is prose in a different file.

An ADR declares capabilities the same way it declares components, under a
`capabilities:` key in the same block, and the executable proof of what it
claims under `acceptance:`:

    ```architecture
    acceptance:
      - scripts/validate/cluster/83-support-agent-no-ingress.sh
      - internal/support
    ```

An ADR declares its runtime components in a fenced `architecture` block:

    ```architecture
    components:
      - victoria-metrics-operator
      - vmalert
    ```

EVERY architecture block in an ADR is read, and their contents are unioned. A
decision recorded in an addendum is a decision, and this gate reading only the
first block meant an addendum could name a component, a capability or an
acceptance test and have none of A-F applied to it. Addenda are where most
long-lived ADRs do their real work, so the checks were absent exactly where the
design was still moving.

Usage: architecture-consistency.py [repo-root]
Exit 0 consistent, 1 inconsistent.
"""
import fnmatch
import os
import re
import sys

try:
    import yaml
except ImportError:
    sys.exit("PyYAML required")

REGISTRY = "manifests/architecture/components.yaml"
SERVICES = "manifests/hub-core-services"
DESCRIPTORS = "manifests/argocd/components"
APPSETS = "manifests/argocd/environment-manager/templates"

ADR_BLOCK = re.compile(r"^```architecture\n(.*?)^```", re.M | re.S)
ADR_STATUS = re.compile(r"^\*\*Status:\*\*\s*(.+?)\s*$", re.M)


def err(msg):
    """GitHub renders ::error:: as an annotation; a terminal renders it as noise."""
    if os.environ.get("GITHUB_ACTIONS"):
        print(f"::error::{msg}")
    else:
        print(f"error: {msg}", file=sys.stderr)


def literal_prefix(path):
    """The part of a templated path that is fixed.

    ApplicationSet paths interpolate the environment, provider and placement
    class. `manifests/hub-core-services/providers/{{ ... }}/database` reaches
    `providers/` whatever the class resolves to, so the fixed prefix is what a
    reachability question can honestly be asked about.
    """
    cut = path.find("{{")
    return path[:cut] if cut != -1 else path


PREFIXES = set()
# A recursive Application's directoryInclude: it applies only files matching it.
INCLUDE = {}
# Templated prefixes only: they resolve to the overlays beneath them at render time.
TEMPLATED = set()
ROOT = "."


def declared_paths(root):
    """Every path the repository can apply, from both places they are declared.

    Component descriptors carry an explicit `path:`. The boundary templates
    declare some Applications inline, because the environment and provider
    dimensions they need are only in scope there -- the same split that let six
    Applications in v0.1.1 name charts nothing packaged.
    """
    paths = set()
    # Paths that reach BELOW themselves: a templated path's fixed prefix, and a
    # directory an Application applies with directoryRecurse. Any other plain
    # directory applies only its own files -- Argo CD's directory source does not
    # descend, and kustomize applies only what its kustomization lists.
    PREFIXES.clear()
    INCLUDE.clear()
    TEMPLATED.clear()

    base = os.path.join(root, DESCRIPTORS)
    for boundary in sorted(os.listdir(base)) if os.path.isdir(base) else []:
        d = os.path.join(base, boundary)
        if not os.path.isdir(d):
            continue
        for name in sorted(os.listdir(d)):
            if not name.endswith(".yaml"):
                continue
            with open(os.path.join(d, name)) as fh:
                text = fh.read()
            recurse = re.search(r"^\s*directoryRecurse:\s*'?true'?\s*$", text, re.M) is not None
            inc = re.search(r"^\s*directoryInclude:\s*'?([^'\n]*)'?\s*$", text, re.M)
            include = inc.group(1).strip() if inc else ""
            for line in text.splitlines():
                m = re.match(r"\s*(?:path|extraManifestsPath):\s*(.+)", line)
                if m:
                    p = m.group(1).strip().strip("'\"")
                    if p:
                        lp = literal_prefix(p).rstrip("/")
                        paths.add(lp)
                        if lp != p.rstrip("/"):
                            TEMPLATED.add(lp)
                        if lp != p.rstrip("/") or recurse:
                            PREFIXES.add(lp)
                            if recurse and include:
                                INCLUDE[lp] = include

    tmpl = os.path.join(root, APPSETS)
    for name in sorted(os.listdir(tmpl)) if os.path.isdir(tmpl) else []:
        with open(os.path.join(tmpl, name)) as fh:
            for line in fh:
                m = re.search(r"path:\s*'([^']*)'", line)
                if m:
                    p = literal_prefix(m.group(1)).strip().rstrip("/")
                    if p:
                        paths.add(p)
                        if p != m.group(1).strip().rstrip("/"):
                            PREFIXES.add(p)
                            TEMPLATED.add(p)
    return paths


def expand_kustomize(root, paths):
    """Follow `resources:` so a base pulled in by an overlay counts as applied.

    manifests/hub-core-services/database is named by no Application. It is
    reached by providers/<class>/database, whose kustomization lists
    `../../../database` -- the ADR-046 placement-class split. Reachability that
    stopped at the Application would report every such base as an orphan and be
    switched off within a week, which is the usual fate of a check that cries
    wolf.
    """
    # A templated path contributes only its fixed prefix, so the seed is
    # `providers/` rather than `providers/hetzner/database`. Expand a prefix
    # that is not itself a kustomization into the overlays underneath it --
    # those are what the template resolves to at render time.
    seeds = set()
    for cur in paths:
        d = os.path.join(root, cur)
        if cur in TEMPLATED and os.path.isdir(d) and not os.path.exists(os.path.join(d, "kustomization.yaml")):
            for sub, _, files in os.walk(d):
                if "kustomization.yaml" in files:
                    seeds.add(os.path.relpath(sub, root))
        seeds.add(cur)

    seen, queue = set(seeds), list(seeds)
    while queue:
        cur = queue.pop()
        k = os.path.join(root, cur, "kustomization.yaml")
        if not os.path.exists(k):
            continue
        try:
            doc = yaml.safe_load(open(k)) or {}
        except yaml.YAMLError:
            continue
        refs = []
        for key in ("resources", "bases", "components"):
            for r in doc.get(key) or []:
                if isinstance(r, str):
                    refs.append(r)
        for r in refs:
            if re.match(r"^[a-z]+://", r) or r.startswith("git@"):
                continue
            nxt = os.path.normpath(os.path.join(cur, r))
            if nxt.startswith(".."):
                continue
            if os.path.isfile(os.path.join(root, nxt)):
                nxt = os.path.dirname(nxt)
            if nxt not in seen:
                seen.add(nxt)
                queue.append(nxt)
    return seen


def includes_any(target, pattern):
    """Whether target holds a file a directoryInclude pattern ({a,b} allowed) selects."""
    m = re.fullmatch(r"\{(.*)\}", pattern)
    globs = m.group(1).split(",") if m else [pattern]
    for _, _, files in os.walk(os.path.join(ROOT, target)):
        if any(fnmatch.fnmatch(f, g.strip()) for f in files for g in globs):
            return True
    return False


def reachable(target, paths):
    """A directory is reachable if something applies it, something inside it, or
    a templated path whose fixed prefix lands on it."""
    for p in paths:
        if p == target or p.startswith(target + "/"):
            return True
        if p in PREFIXES and target.startswith(p + "/"):
            if p not in INCLUDE or includes_any(target, INCLUDE[p]):
                return True
    return False


def unwired(root, rel):
    """Why nothing would run this, or None if something would.

    Three shapes are recognised, because three exist:

      scripts/validate/{preflight,cluster}/*.sh  run.sh globs the directory, so
          being in it IS the wiring and nothing further is needed.
      a Go package directory                     `go test ./...` finds it, but
          only if it actually contains tests.
      anything else                              has to be invoked by name from
          a workflow or a release script, or nothing reaches it.
    """
    full = os.path.join(root, rel)

    parts = rel.replace("\\", "/").split("/")
    if len(parts) >= 3 and parts[:2] == ["scripts", "validate"] \
            and parts[2] in ("preflight", "cluster") and rel.endswith(".sh"):
        if not os.access(full, os.X_OK):
            return "is not executable, so run.sh will skip it"
        return None

    if os.path.isdir(full):
        has_test = any(n.endswith("_test.go") for n in os.listdir(full))
        if has_test:
            return None
        return ("is a directory with no _test.go in it, so `go test` on it "
                "reports success having run nothing")

    # Grep the places that invoke things. A path nobody names is a path nobody
    # runs, whatever it contains.
    callers = []
    for d in (".github/workflows", "scripts", "Makefile"):
        base = os.path.join(root, d)
        if os.path.isfile(base):
            callers.append(base)
        elif os.path.isdir(base):
            for sub, _, files in os.walk(base):
                callers.extend(os.path.join(sub, f) for f in files)
    for c in callers:
        if os.path.abspath(c) == os.path.abspath(full):
            continue
        try:
            with open(c, errors="ignore") as fh:
                if rel in fh.read():
                    return None
        except OSError:
            continue
    return "is invoked by no workflow, script or Makefile target"


def load_adrs(root):
    """ADR name -> (status, [components], [capabilities], [acceptance]).

    EVERY architecture block in the file is read, not the first.

    This used `.search()`, which stops at the first match, and the difference is
    not cosmetic: a decision recorded in an addendum was invisible to every check
    below. ADR-078 carries four blocks and ADR-083 two, and the components named
    in the later ones -- kube-state-metrics, node-exporter,
    victoriametrics-operator, spoke-query-endpoint -- were never resolved against
    the registry, never required to be registered, and never held their ADR to
    the shipped-before-accepted rule.

    That is the whole discipline of this gate switched off for anything decided
    after an ADR was first written, which is where most of the observability
    design now lives. Union of every block, so a later block can only add.
    """
    out = {}
    d = os.path.join(root, "docs", "adr")
    for name in sorted(os.listdir(d)):
        if not name.endswith(".md"):
            continue
        text = open(os.path.join(d, name)).read()
        blocks = ADR_BLOCK.findall(text)
        if not blocks:
            continue

        comps, caps, accept, invalid = [], [], [], False
        for raw in blocks:
            try:
                data = yaml.safe_load(raw) or {}
            except yaml.YAMLError as e:
                err(f"{name}: architecture block is not valid YAML: {e}")
                invalid = True
                break
            # Order-preserving union: an addendum adds to the decision, and a
            # name repeated across blocks is one component, not two.
            for key, dest in (("components", comps),
                              ("capabilities", caps),
                              ("acceptance", accept)):
                for v in (data.get(key) or []):
                    if v not in dest:
                        dest.append(v)

        if invalid:
            out[name] = ("invalid", [], [], [])
            continue

        status = ADR_STATUS.search(text)
        out[name] = (status.group(1) if status else "unknown",
                     comps, caps, accept)
    return out


def main():
    global ROOT
    root = sys.argv[1] if len(sys.argv) > 1 else "."
    ROOT = root
    failures = []

    reg_path = os.path.join(root, REGISTRY)
    if not os.path.exists(reg_path):
        err(f"{REGISTRY} is missing; there is nothing to check against")
        return 1
    registry = yaml.safe_load(open(reg_path)) or {}
    components = {c["name"]: c for c in registry.get("components") or []}
    # A directory an ADR has decided on but nothing has built yet is pending,
    # not orphaned. Check C is what stops it staying that way: the declaring
    # ADR cannot be accepted while it does.
    planned = {c.get("path") for c in components.values()
               if c.get("status") == "planned" and c.get("path")}
    waived = {w["path"]: w.get("reason", "") for w in registry.get("unreferenced") or []}

    paths = expand_kustomize(root, declared_paths(root))

    # ── A. No directory is orphaned without saying so ───────────────────────
    svc = os.path.join(root, SERVICES)
    for name in sorted(os.listdir(svc)):
        d = os.path.join(SERVICES, name)
        if not os.path.isdir(os.path.join(root, d)):
            continue
        if reachable(d, paths) or d in waived or d in planned:
            continue
        failures.append(
            f"{d} is applied by nothing: no component descriptor and no "
            f"ApplicationSet names it, so its content has never reached a "
            f"cluster. Add a descriptor, or record it under `unreferenced:` in "
            f"{REGISTRY} with the reason.")

    for p, reason in sorted(waived.items()):
        if not os.path.isdir(os.path.join(root, p)):
            failures.append(f"{REGISTRY} waives {p}, which does not exist")
        elif not reason:
            failures.append(f"{REGISTRY} waives {p} with no reason; a waiver "
                            f"nobody justified is an orphan with paperwork")
        elif reachable(p, paths):
            failures.append(f"{REGISTRY} waives {p} as unreferenced, but "
                            f"something applies it; remove the waiver")

    # ── B/C. What an ADR claims, the repository has to contain ──────────────
    adrs = load_adrs(root)
    for adr, (status, named, _, _) in sorted(adrs.items()):
        accepted = status.lower().startswith("accepted")
        for cname in named:
            c = components.get(cname)
            if c is None:
                failures.append(
                    f"{adr} names the component '{cname}', which is registered "
                    f"nowhere. An ADR may not name a runtime component the "
                    f"repository cannot resolve.")
                continue
            if adr not in (c.get("declared_by") or []):
                failures.append(
                    f"{adr} names '{cname}' but the registry does not list "
                    f"{adr} in its declared_by; the link has to hold both ways "
                    f"or removing the ADR leaves the entry unattributed.")
            if not accepted:
                continue
            if c.get("status") != "shipped":
                failures.append(
                    f"{adr} is {status} and names '{cname}', which is "
                    f"'{c.get('status')}'. An accepted decision may not rest on "
                    f"a component nothing deploys -- ship it, or return the ADR "
                    f"to Proposed.")
            elif not reachable(c.get("path", ""), paths):
                failures.append(
                    f"{adr} is {status} and names '{cname}', registered as "
                    f"shipped at {c.get('path')}, which nothing applies.")

    # ── E. The capability declaration (ADR-066) ─────────────────────────────
    #
    # ADR-066 said "catalogue" and ADR-071 refuses one; its addendum 1 settles
    # the word and makes this file the declaration. What it has to be true of:
    # a state that exists, a date wherever maintenance is promised to end, and
    # selection expressed as a value rather than a version.
    LIFECYCLE = ("declared", "supported", "deprecated", "end-of-life", "removed")
    caps = {c["name"]: c for c in registry.get("capabilities") or []}

    for name, c in sorted(caps.items()):
        life = c.get("lifecycle")
        if life not in LIFECYCLE:
            failures.append(f"capability '{name}' has lifecycle "
                            f"'{life}', which is not one of {', '.join(LIFECYCLE)}")
        # ADR-069: what a tenant is owed at end-of-life is the date. A state
        # that promises an ending without naming one is the announcement
        # without the thing announced.
        if life in ("deprecated", "end-of-life") and not c.get("eol"):
            failures.append(f"capability '{name}' is '{life}' and names no "
                            f"`eol` date; ADR-069 makes the date what the "
                            f"tenant is owed")

        kind = c.get("kind")
        if kind not in ("machinery", "selectable"):
            failures.append(f"capability '{name}' has kind '{kind}'; ADR-066 "
                            f"knows machinery and selectable")
        elif kind == "selectable" and not c.get("enable"):
            failures.append(f"capability '{name}' is selectable and names no "
                            f"`enable` values path, so nothing selects it")
        elif kind == "machinery" and c.get("enable"):
            failures.append(f"capability '{name}' is machinery and names an "
                            f"`enable` path; a box that can switch it off "
                            f"cannot reconcile anything")

        # Selection is a value and never a version (ADR-066), so adopting a
        # capability and upgrading a bundle stay independent acts.
        enable = str(c.get("enable") or "")
        if re.search(r"\bversion\b|targetRevision", enable, re.I):
            failures.append(f"capability '{name}' selects on '{enable}', which "
                            f"reads as a version; ADR-066 requires selection to "
                            f"be a value so selecting and upgrading stay "
                            f"independent")

        if c.get("status") not in ("shipped", "planned"):
            failures.append(f"capability '{name}' has status "
                            f"'{c.get('status')}'")
        if not (c.get("declared_by") or []):
            failures.append(f"capability '{name}' is declared by no ADR")

        for comp in c.get("components") or []:
            if comp not in components:
                failures.append(f"capability '{name}' is realised by component "
                                f"'{comp}', which is registered nowhere")

    # ADR-066: "a pair that must be tested together is named rather than
    # assumed". A one-way naming is an assumption wearing a declaration.
    for name, c in sorted(caps.items()):
        for other in c.get("interacts_with") or []:
            if other not in caps:
                failures.append(f"capability '{name}' names an interaction with "
                                f"'{other}', which is not a capability")
            elif name not in (caps[other].get("interacts_with") or []):
                failures.append(f"'{name}' names an interaction with '{other}' "
                                f"and '{other}' does not name it back; a pair "
                                f"certified together has to be declared by both")

    # An ADR may name capabilities exactly as it names components, and the same
    # rule binds: accepting it is the moment its claims have to be true.
    for adr, (status, _, named_caps, _) in sorted(adrs.items()):
        accepted = status.lower().startswith("accepted")
        for cname in named_caps:
            c = caps.get(cname)
            if c is None:
                failures.append(f"{adr} names the capability '{cname}', which "
                                f"is declared nowhere")
                continue
            if adr not in (c.get("declared_by") or []):
                failures.append(f"{adr} names capability '{cname}' but the "
                                f"declaration does not list {adr} in its "
                                f"declared_by")
            if accepted and c.get("status") != "shipped":
                failures.append(
                    f"{adr} is {status} and names capability '{cname}', which "
                    f"is '{c.get('status')}'. A tenant cannot select a "
                    f"capability nothing lets them select.")

    # ── F. A claimed behaviour has an executable proof, and it is wired ─────
    #
    # Checks A-E answer "does the thing exist". Most of what an ADR decides is
    # not a thing but a BEHAVIOUR -- declining a proposal breaks nothing, an
    # unverified check does not read as a pass, a capability switched off leaves
    # the machinery alone. Nothing in the repository connected a decision like
    # that to the test that proves it, which is how ADR-064 came to decide
    # promotion with no reconciler behind it.
    #
    # Existence is the easy half. The half that matters is WIRED: a gate nobody
    # runs and a test file in a package with no test runner are both green by
    # never executing, and both look exactly like coverage from the ADR.
    for adr, (status, _, _, accept) in sorted(adrs.items()):
        accepted = status.lower().startswith("accepted")
        for rel in accept:
            full = os.path.join(root, rel)
            if not os.path.exists(full):
                failures.append(
                    f"{adr} names '{rel}' as its acceptance test, and it does "
                    f"not exist. An ADR may not cite proof the repository does "
                    f"not contain.")
                continue

            reason = unwired(root, rel)
            if reason and accepted:
                failures.append(f"{adr} is {status} and its acceptance test "
                                f"'{rel}' {reason}")
            elif reason:
                # Proposed is allowed to be mid-build, but it is still said out
                # loud -- silence here is how "we will wire it up" becomes the
                # permanent state.
                print(f"note: {adr} acceptance '{rel}' {reason}")

    # ── D. The registry describes the repository that exists ────────────────
    for cname, c in sorted(components.items()):
        p = c.get("path")
        if not p:
            if c.get("status") != "planned":
                failures.append(f"registry entry '{cname}' is "
                                f"'{c.get('status')}' and has no path")
        elif not os.path.exists(os.path.join(root, p)):
            failures.append(f"registry entry '{cname}' points at {p}, which "
                            f"does not exist")
        if not (c.get("declared_by") or []):
            failures.append(f"registry entry '{cname}' is declared by no ADR; "
                            f"a component nobody decided on is not architecture")

    for f in failures:
        err(f)
    print(f"architecture-consistency: components={len(components)} "
          f"capabilities={len(caps)} waived={len(waived)} "
          f"failures={len(failures)}")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
