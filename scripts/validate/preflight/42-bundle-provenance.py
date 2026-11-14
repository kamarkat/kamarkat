#!/usr/bin/env python3
"""A published bundle must say which source produced it.

0.1.16-rc.29 was published, pulled by a cluster, and carried exactly one fact
about itself:

    version: 0.1.16-rc.29

No commit, no branch, no indication that it was built from a working tree with
uncommitted changes -- which it was. "What is in rc.29?" had no answer that did
not depend on someone remembering, and none of it was written down.

Both callers of publish.sh build from a working tree: the release workflow from
a checkout, a developer from a laptop (that is the local release path, and it is
deliberate). So the dirty case cannot be refused without breaking the workflow
this supports. It is recorded instead, and this asserts the recording exists.

Checks the generator, not a built artefact, because the artefact only exists
during a publish and this has to fail before one happens.
"""
import ast
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[3]
SCRIPT = ROOT / "scripts/package/bundle-chart.sh"

REQUIRED = [
    "kamarkat.io/source-commit",
    "kamarkat.io/source-dirty",
    "kamarkat.io/source-branch",
    "kamarkat.io/built-at",
]


def main():
    if not SCRIPT.exists():
        print(f"BAD\tbundle provenance: {SCRIPT} not found")
        return 1
    body = SCRIPT.read_text()
    problems = []

    for key in REQUIRED:
        if key not in body:
            problems.append(f"the chart is not stamped with {key}")

    if "git rev-parse HEAD" not in body:
        problems.append("the source commit is never read")
    if "git status --porcelain" not in body:
        problems.append("a dirty working tree is never detected, so it cannot be recorded")

    # The generator must survive being run twice: publish is re-run constantly
    # during development, and a stamp that appends would stack annotation blocks
    # until the Chart.yaml stopped parsing.
    if "annotations:" in body and not re.search(r'annotations:\\\\n\(\?:', body) \
            and "re.sub(r\"(?ms)^annotations:" not in body:
        problems.append("the stamp does not strip an existing annotations block, "
                        "so re-running publish would stack them")

    # And it must compile: a syntax error here breaks every publish, and the
    # first sign would be a failed release.
    try:
        i = body.index('python3 - "$CHART/Chart.yaml" "$VERSION" "$GIT_SHA"')
        gen = body[body.index("<<'PY'", i) + len("<<'PY'"):]
        gen = gen[:gen.index("\nPY\n")]
        ast.parse(gen)
    except ValueError:
        problems.append("the provenance generator could not be located")
    except SyntaxError as e:
        problems.append(f"the provenance generator does not compile: {e}")

    if problems:
        print("BAD\tbundle provenance")
        for p in problems:
            print(f"BAD\t  {p}")
        return 1
    print("OK\tpublished bundles record commit, branch, dirty state and build time")
    return 0


if __name__ == "__main__":
    sys.exit(main())
