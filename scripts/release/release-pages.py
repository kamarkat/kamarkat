#!/usr/bin/env python3
"""Render the docs' release pages from each version's annotated inventory.

    kakt release docs [<version>...] --docs-dir <docs checkout>

`kakt release docs` first writes each named version's data with
release-data.py inventory into <docs>/data/releases/<version>.json, then runs
this, which renders whatever that directory holds:

    versions/lines/<minor>.mdx        every release on a minor line, with what changed
    versions/releases/<version>.mdx   one release: what changed, and every component
    snippets/releases/<version>.jsx   the data each page renders

and lists the line pages in the Versions tab of docs.json. Release pages are
reached from their line page, not listed one by one in the navigation.

Nothing is fetched while the docs build: the data is copied into the docs, so a
page never depends on the registry being reachable.
"""
from __future__ import annotations

import argparse
import json
import re
import shutil
import sys
from datetime import datetime
from pathlib import Path


def semver_key(v: str):
    m = re.match(r"(\d+)\.(\d+)\.(\d+)(?:-(.+))?$", v)
    if not m:
        sys.exit(f"release-pages: {v} is not a semantic version")
    major, minor, patch, pre = m.groups()
    # A release sorts after every prerelease of itself; rc.10 after rc.9.
    pre_key = (1,) if pre is None else (0,) + tuple(int(p) if p.isdigit() else p for p in re.split(r"[.-]", pre))
    return (int(major), int(minor), int(patch), pre_key)


def slug(v: str) -> str:
    return v.replace(".", "-")


def day(at: str) -> str:
    return datetime.fromisoformat(at.replace("Z", "+00:00")).strftime("%-d %b %Y")


def jsx(name: str, value) -> str:
    return f"export const {name} = {json.dumps(value, indent=2)};\n"


def release_page(r: dict, line: str) -> str:
    v = r["version"]
    lead = (f"Every component {'prerelease' if '-' in v else 'bundle version'} `{v}` installs, on which nodes, "
            f"at what version. Released {day(r['released'])}.")
    since = f" since `{r['previous']}`" if r.get("previous") else ""
    return f"""---
title: "{v}"
description: "Components in bundle version {v}."
keywords: ["{v}", "components", "inventory", "management cluster", "managed clusters"]
---

import {{ ReleaseComponents, ReleaseChanges, BootstrapTools }} from "/snippets/release-components.jsx";
import {{ release }} from "/snippets/releases/{slug(v)}.jsx";

> {lead}

[All {line} versions](/versions/lines/{slug(line)})

## What changed{since}

<ReleaseChanges release={{release}} />

## What's included

Your box has one management cluster and the managed clusters it provisions. Each has control plane nodes and worker nodes. Choose a cluster, then a node role, to see what runs there. A version marked **unpinned** is installed at whatever version its source serves when a node is created.

**Latest (as of release date)** is the newest release the component's project had published on {day(r['released'])}. A version in green is that release; a version in red is behind it, or cannot be shown to match it. For an image the platform builds, latest is the newest build, and green means the version shipped it. Hover or focus the ⓘ beside a red version for why it is behind.

<ReleaseComponents release={{release}} />

## Bootstrap tools

Run by the `kakt` CLI on your machine or in your CI during Day-0. Not installed in any cluster.

<BootstrapTools release={{release}} />

These components are for the `{r.get("provider", "hetzner")}` provider. Your applications, deployed from your GitOps repository, are yours and are not listed.
"""


def line_page(line: str, rows: list[dict]) -> str:
    return f"""---
title: "{line}"
description: "Every bundle version on the {line} line, and what each one changed."
keywords: ["{line}", "releases", "versions", "component changes"]
---

import {{ ReleaseLine }} from "/snippets/release-line.jsx";
import {{ line }} from "/snippets/releases/line-{slug(line)}.jsx";

> {len(rows)} versions on the {line} line, newest first. Open a version to see what changed, or follow its link for every component it installs.

<ReleaseLine line={{line}} />
"""


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("docs", help="the docs checkout to write the pages into")
    a = ap.parse_args()
    root = Path(a.docs).resolve()
    if not (root / "docs.json").is_file():
        sys.exit(f"release-pages: {root} is not a docs checkout (no docs.json)")
    DATA, PAGES, LINES, SNIPPETS = (root / "data/releases", root / "versions/releases",
                                    root / "versions/lines", root / "snippets/releases")

    releases = [json.loads(p.read_text()) for p in sorted(DATA.glob("*.json"))] if DATA.exists() else []
    for r in releases:
        if ("released" not in r or any("url" not in c for c in r["components"])
                or any(c["current"] is False and not c.get("behind") for c in r["components"])):
            sys.exit(f"release-pages: {r['version']} was fetched without its release date, upstream "
                     f"versions or the reasons components are behind. Fetch it again: kakt release docs {r['version']}")
    releases.sort(key=lambda r: semver_key(r["version"]), reverse=True)

    for d in (PAGES, LINES, SNIPPETS):
        if d.exists():
            shutil.rmtree(d)
        d.mkdir(parents=True)

    stable = [r["version"] for r in releases if "-" not in r["version"]]
    latest = stable[0] if stable else None
    lines: dict[str, list[dict]] = {}
    for r in releases:
        major, minor = r["version"].split(".")[:2]
        line = f"{major}.{minor}"
        (SNIPPETS / f"{slug(r['version'])}.jsx").write_text(jsx("release", r))
        (PAGES / f"{slug(r['version'])}.mdx").write_text(release_page(r, line))
        lines.setdefault(line, []).append({
            "version": r["version"],
            "released": day(r["released"]),
            "href": f"/versions/releases/{slug(r['version'])}",
            "latest": r["version"] == latest,
            "prerelease": "-" in r["version"],
            "components": len(r["components"]),
            "behind": sum(1 for c in r["components"] if c["current"] is False),
            "previous": r.get("previous", ""),
            "changes": r.get("changes"),
        })

    for line, rows in lines.items():
        (SNIPPETS / f"line-{slug(line)}.jsx").write_text(jsx("line", {"line": line, "releases": rows}))
        (LINES / f"{slug(line)}.mdx").write_text(line_page(line, rows))

    docs = json.loads((root / "docs.json").read_text())
    tab = next(t for t in docs["navigation"]["tabs"] if t.get("tab") == "Versions")
    tab["groups"] = [g for g in tab["groups"] if g["group"] != "Releases"]
    if lines:
        tab["groups"].append({
            "group": "Releases",
            "pages": [f"versions/lines/{slug(l)}" for l in sorted(lines, key=lambda l: tuple(map(int, l.split("."))), reverse=True)],
        })
    (root / "docs.json").write_text(json.dumps(docs, indent=2) + "\n")
    print(f"release-pages: {len(releases)} versions on {len(lines)} lines")


if __name__ == "__main__":
    main()
