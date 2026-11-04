#!/usr/bin/env python3
"""The product-team onboarding guide names things that still exist.

docs/onboarding-an-application.md tells a product team what to declare, what
labels to carry, which commands to run and which library functions to call. It is
the one document a team reads before touching the platform, and it is the one
document nothing verifies -- an ADR at least gets read when its subject changes,
while a guide simply ages until someone follows it and fails.

The failure mode is specific and expensive: a guide that names a field the chart
no longer reads, or a label the policy no longer requires, sends a team to debug
their own work. Every check here is therefore "does the thing the guide names
still exist", never "is the guide complete" -- completeness is a judgement and
this is a gate.

Checked:
    A  every label the tenant ABI policy REQUIRES is named in the guide
    B  every values key the guide shows is a real key somewhere
    C  every ADR the guide cites exists
    D  every `kakt` command the guide shows is registered
    E  every zero-ops-auth function the guide shows is exported

Emits tab-separated lines the shell wrapper classifies:
    OK<TAB><message>
    BAD<TAB><message>
    NONE
"""
import re
import sys
from pathlib import Path

import yaml

GUIDE = Path("docs/onboarding-an-application.md")
ABI = Path("manifests/spoke/spoke-catalog/infra/kyverno-tenant-abi.yaml")
CHART_VALUES = Path("manifests/tenants/charts/universal-tenant/values.yaml")
XRD = Path("manifests/hub-core-services/crossplane/tenant-platform/xrds/ainativesaas-v1.yaml")
AUTH_INDEX = Path("packages/auth/src/index.ts")
KAKT = Path("cmd/kakt")
ADR_DIR = Path("docs/adr")

bad = False


def fail(msg: str) -> None:
    global bad
    bad = True
    print(f"BAD\t{msg}")


def ok(msg: str) -> None:
    print(f"OK\t{msg}")


def declaration_keys(text: str):
    """Top-level keys of the guide's FLEET DECLARATION example.

    That block only, identified by it carrying `tenantId`. The guide has other
    YAML blocks -- workload labels, a secrets entry -- whose keys are not fleet
    values and are checked by their own sections. An earlier version read every
    block and reported `tenant-id` (a LABEL) as an invented values key.
    """
    keys = set()
    for block in re.findall(r"```yaml\n(.*?)```", text, re.S):
        try:
            doc = yaml.safe_load(block)
        except yaml.YAMLError:
            continue
        if isinstance(doc, dict) and "tenantId" in doc:
            keys |= set(doc.keys())
    return keys


def required_labels() -> set:
    out = set()
    for d in yaml.safe_load_all(ABI.read_text()):
        if not d or d.get("kind") != "ClusterPolicy":
            continue
        for rule in d.get("spec", {}).get("rules", []):
            pattern = rule.get("validate", {}).get("pattern", {}) or {}
            labels = (pattern.get("metadata", {}) or {}).get("labels") or {}
            out |= set(labels.keys())
    return out


def real_value_keys() -> set:
    """Every key a fleet may legitimately set.

    THREE SOURCES, unioned, because no single one is complete:

      the chart's values.yaml   what has a default
      the XRD                   what is REQUIRED and therefore has none
                                (appId, cellId, scopeId)
      real fleet declarations   what a tenant repository actually writes, which
                                is a different schema from the chart's -- `public`
                                lives only here, and checking the chart alone
                                rejected the guide for naming it

    A key present in none of the three is one a team would copy into a file
    nothing reads.
    """
    keys = set()
    chart = yaml.safe_load(CHART_VALUES.read_text()) or {}
    keys |= set(chart.keys())
    for decl in Path(".").glob(".local-e2e/*/environments/*/*/values.yaml"):
        try:
            doc = yaml.safe_load(decl.read_text()) or {}
        except yaml.YAMLError:
            continue
        if isinstance(doc, dict):
            keys |= set(doc.keys())
    try:
        for d in yaml.safe_load_all(XRD.read_text()):
            if not d or d.get("kind") != "CompositeResourceDefinition":
                continue
            for ver in d["spec"]["versions"]:
                props = ver["schema"]["openAPIV3Schema"]["properties"]["spec"]["properties"]
                keys |= set(props.keys())
    except (KeyError, TypeError, yaml.YAMLError):
        pass
    return keys


def main() -> int:
    if not GUIDE.is_file():
        print("NONE")
        return 0
    text = GUIDE.read_text()

    # A — labels the policy requires must be named in the guide, AS A LABEL.
    #
    # Searched inside fenced blocks and as `label:`, not as a bare substring
    # anywhere in the prose. A substring match passes on a label that survives
    # only in a sentence explaining why something else matters, which is exactly
    # the state this check exists to catch -- the guide mentioning a requirement
    # it no longer shows a team how to satisfy.
    blocks = "\n".join(re.findall(r"```[a-z]*\n(.*?)```", text, re.S))
    for label in sorted(required_labels()):
        if re.search(rf"^\s*{re.escape(label)}:", blocks, re.M):
            ok(f"guide names the required label {label!r}")
        else:
            fail(
                f"the tenant ABI policy requires the label {label!r} and the guide does not "
                f"name it. A workload without it is refused by admission, and the team has "
                f"no way to know from this document."
            )

    # B — every values key the guide shows must exist somewhere real.
    real = real_value_keys()
    shown = declaration_keys(text)
    ignore: set = set()
    for key in sorted(shown - ignore):
        if key in real:
            continue
        fail(
            f"the guide's example declares {key!r}, which is not a value the chart or the "
            f"XRD defines. A team copying it gets a field nothing reads."
        )
    if shown - ignore:
        ok(f"every values key the guide shows is real ({len(shown - ignore)} checked)")

    # C — cited ADRs exist.
    cited = sorted({int(n) for n in re.findall(r"ADR-(\d{3})", text)})
    missing = [n for n in cited if not list(ADR_DIR.glob(f"{n:03d}-*.md"))]
    for n in missing:
        # waypoint's own ADRs are cited by number and live in another repository.
        if re.search(rf"waypoint ADR-{n:03d}", text):
            continue
        fail(f"the guide cites ADR-{n:03d}, which does not exist in {ADR_DIR}/")
    if cited:
        ok(f"every ADR the guide cites exists ({len(cited)} checked)")

    # D — kakt commands the guide shows are registered.
    kakt_src = "\n".join(p.read_text() for p in KAKT.glob("*.go"))
    for cmd in sorted(set(re.findall(r"kakt ([a-z]+(?: [a-z-]+)*)", text))):
        leaf = cmd.split()[-1]
        if re.search(rf'Use:\s+"{re.escape(leaf)}[ "]', kakt_src):
            continue
        fail(f"the guide shows `kakt {cmd}`, and no command named {leaf!r} is registered")
    ok("every kakt command the guide shows is registered")

    # E — zero-ops-auth functions the guide shows are exported.
    if AUTH_INDEX.is_file():
        exports = AUTH_INDEX.read_text()
        named = set(re.findall(r"\b([a-z][A-Za-z]+(?:Validator|Headers|Caller|Principal))\b", text))
        for fn in sorted(named):
            if fn in exports:
                continue
            fail(f"the guide tells a team to import {fn!r}, which zero-ops-auth does not export")
        if named:
            ok(f"every zero-ops-auth function the guide shows is exported ({len(named)} checked)")

    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
