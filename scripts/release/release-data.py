#!/usr/bin/env python3
"""What a bundle version ships, against each component's newest upstream release.

    soloz release inventory --version <v> --out <file>     (release-data.py inventory)
    soloz release check                                    (release-data.py check)

`inventory` pulls a published version's `components.json` from its bundle chart
and records, as of the day the version was published to the registry:

    released            that day
    url                 where each component's title links (the catalog's repo)
    latest              the newest release its upstream repository had published
    current             whether the version shipped is that release
    behind              for one that is not, the catalog's reasons for the versions
                        it ships; a version with none fails

The docs' release pages are generated from exactly this, so they show what was
latest on each release date.

`check` runs the same lookups on an inventory not yet published (a rehearsal's
components.json), as of today, and names every version behind its latest release
that the catalog does not explain. It writes nothing.

Both read manifests/architecture/component-catalog.yaml: where each component's
versions are published (`track`, `tag`, `repos`) and why one is behind (`behind`)
are the catalog's, so the lookups live beside it.
"""
from __future__ import annotations

import argparse
import json
import re
import subprocess
import sys
import tempfile
import urllib.request
from datetime import datetime, timedelta, timezone
from pathlib import Path

import yaml

CATALOG = Path(__file__).resolve().parents[2] / "manifests/architecture/component-catalog.yaml"

# A release tag that names a version, unless the catalog gives the repo its own pattern.
RELEASE_TAG = r"^v?(\d+)\.(\d+)\.(\d+)$"


def gh(path: str, paginate: bool = True, missing_ok: bool = False) -> list[dict] | None:
    """The API's rows, or None when it has no such resource and that is expected.

    `check=True` raised a CalledProcessError whose message was the command line,
    so a 404 on one package surfaced as a traceback that named no component and
    discarded the reason gh printed. Every failure now says what gh said, and a
    caller that can carry on without the resource asks for `missing_ok`.
    """
    r = subprocess.run(["gh", "api", *(["--paginate"] if paginate else []), path, "--jq", ".[] | @json"],
                       capture_output=True, text=True)
    if r.returncode:
        err = " ".join((r.stderr or "").split()) or "no output on stderr"
        if "HTTP 404" in err:
            if missing_ok:
                return None
            sys.exit(f"release-data: {path} does not exist\n  {err}")
        sys.exit(f"release-data: gh api {path} failed\n  {err}")
    return [json.loads(line) for line in r.stdout.splitlines()]


def published(owner: str) -> dict[str, str]:
    """The day each bundle version was published to the registry."""
    days = {}
    for pv in gh(f"/orgs/{owner}/packages/container/charts%2Fenvironment-manager/versions?per_page=100"):
        for tag in pv["metadata"]["container"]["tags"]:
            days[tag] = pv["created_at"]
    return days


class Upstream:
    """The newest version of each tracked component, as of a given moment.

    A component's catalog entry says where its versions are published (`track`):

      releases   GitHub releases of `repo` (the default for an owner/name repo)
      tags       Git tags of `repo`, for a project that tags but publishes no releases
      dockerhub  the artefact's own Docker Hub tags. Docker Hub records no publish
                 date per tag, so this is as of the fetch, not the release date
      ghcr       the artefact's own GHCR package: the newest image built, compared
                 by digest, for images the platform builds itself
      ubuntu     Ubuntu's LTS releases, from Launchpad
      file       a version written in `file` of repository `from`, read at that
                 repository's newest release

    GitHub releases are listed newest first. Reading stops at the first page that
    reaches WINDOW before the moment with a matching release already found: the
    newest version is among the releases of the half-year before it, and a
    long-lived project has more releases than the API will page through."""

    WINDOW = timedelta(days=183)

    def __init__(self):
        # repo -> (pages read, every release on them, whether the last page was read)
        self.read: dict[str, tuple[int, list[tuple[str, str]], bool]] = {}
        self.cache: dict[tuple, list] = {}

    @staticmethod
    def newest(items, pattern: str, before: str | None) -> tuple[str, str]:
        """(version, tag) of the highest version among (tag, published) items."""
        found = []
        for tag, at in items:
            m = re.match(pattern, tag)
            if m and (before is None or (at and at <= before)):
                found.append((tuple(int(g) for g in m.groups() if g is not None), tag))
        if not found:
            return "", ""
        version, tag = max(found)
        return ".".join(map(str, version)), tag

    def releases(self, repo: str, pattern: str, before: str) -> tuple[str, str]:
        oldest = (datetime.fromisoformat(before.replace("Z", "+00:00")) - self.WINDOW).strftime("%Y-%m-%dT%H:%M:%SZ")
        pages, seen, end = self.read.get(repo, (0, [], False))
        while True:
            found = self.newest(seen, pattern, before)
            if end or (found[0] and seen[-1][1] < oldest):
                break
            batch = gh(f"repos/{repo}/releases?per_page=100&page={pages + 1}", paginate=False)
            pages += 1
            seen = seen + [(r["tag_name"], r["published_at"] or "") for r in batch
                           if not r["draft"] and not r["prerelease"]]
            end = len(batch) < 100
        self.read[repo] = (pages, seen, end)
        return found

    def tags(self, repo: str, pattern: str, before: str) -> tuple[str, str]:
        if ("tags", repo) not in self.cache:
            owner, name = repo.split("/")
            query = ('query($o:String!,$n:String!){repository(owner:$o,name:$n){refs(refPrefix:"refs/tags/",'
                     'first:100,orderBy:{field:TAG_COMMIT_DATE,direction:DESC}){nodes{name target{'
                     '... on Commit{committedDate} ... on Tag{target{... on Commit{committedDate}}}}}}}}')
            out = subprocess.run(["gh", "api", "graphql", "-f", f"query={query}", "-F", f"o={owner}", "-F", f"n={name}"],
                                 check=True, capture_output=True, text=True).stdout
            nodes = json.loads(out)["data"]["repository"]["refs"]["nodes"]
            self.cache[("tags", repo)] = [
                (n["name"], n["target"].get("committedDate") or (n["target"].get("target") or {}).get("committedDate") or "")
                for n in nodes]
        return self.newest(self.cache[("tags", repo)], pattern, before)

    def dockerhub(self, image: str, pattern: str) -> tuple[str, str]:
        if ("dockerhub", image) not in self.cache:
            tags, url = [], f"https://hub.docker.com/v2/repositories/{image}/tags?page_size=100&ordering=last_updated"
            for _ in range(3):
                page = http_json(url)
                tags += [(t["name"], "") for t in page["results"]]
                url = page.get("next")
                if not url:
                    break
            self.cache[("dockerhub", image)] = tags
        return self.newest(self.cache[("dockerhub", image)], pattern, None)

    def ghcr(self, package: str, before: str) -> tuple[str, str]:
        """(short commit tag, digest) of the newest image built before the moment."""
        owner, name = package.split("/", 1)
        path = f"/orgs/{owner}/packages/container/{name.replace('/', '%2F')}/versions?per_page=100"
        if ("ghcr", package) not in self.cache:
            rows = gh(path, missing_ok=True)
            if rows is None:
                # The platform builds this image, but nothing has been published
                # under that name: a component whose build workflow has not run
                # yet, usually because the commit that adds it is not pushed.
                # There is no latest for it to be behind, so it is reported as
                # neither current nor behind rather than failing the run.
                print(f"release-data: {package} is not published in the registry; "
                      f"reporting no latest version for it", file=sys.stderr)
                rows = []
            self.cache[("ghcr", package)] = rows
        built = [v for v in self.cache[("ghcr", package)] if v["created_at"] <= before]
        if not built:
            return "", ""
        v = max(built, key=lambda v: v["created_at"])
        tags = [t for t in v["metadata"]["container"]["tags"] if t != "latest"]
        return (tags[0][:8] if tags else v["name"][7:19]), v["name"]

    def ubuntu(self, before: str) -> tuple[str, str]:
        if ("ubuntu",) not in self.cache:
            self.cache[("ubuntu",)] = http_json("https://api.launchpad.net/devel/ubuntu/series")["entries"]
        lts = [(e["version"], e["datereleased"].replace("+00:00", "Z")) for e in self.cache[("ubuntu",)]
               if e.get("datereleased") and re.match(r"^\d*[02468]\.04$", e["version"])]
        # Ubuntu's version is a date, YY.MM: shown as written, not as numbers.
        _, version = self.newest([(v, at) for v, at in lts], r"^(\d+)\.(\d+)$", before)
        return version, version

    def file(self, repo: str, path: str, pattern: str, before: str) -> tuple[str, str]:
        _, ref = self.releases(repo, RELEASE_TAG, before)
        if not ref:
            return "", ""
        key = ("file", repo, path, ref)
        if key not in self.cache:
            self.cache[key] = subprocess.run(
                ["gh", "api", f"repos/{repo}/contents/{path}?ref={ref}", "-H", "Accept: application/vnd.github.raw"],
                check=True, capture_output=True, text=True).stdout
        m = re.search(pattern, self.cache[key], re.M)
        return (".".join(g for g in m.groups() if g is not None), ref) if m else ("", "")


def http_json(url: str) -> dict:
    with urllib.request.urlopen(url, timeout=30) as r:
        return json.load(r)


def current(version: str, latest: str) -> bool:
    """Whether every version shipped is the latest release. A shipped version names
    as many parts as it pins: 1.27-alpine is current while 1.27 is the newest line."""
    want = tuple(int(p) for p in latest.split("."))
    for token in version.split(","):
        m = re.search(r"\d+(?:\.\d+)*", token)
        if not m:
            return False
        have = tuple(int(p) for p in m.group().split("."))
        if want[:len(have)] != have:
            return False
    return True


def tracking(catalog: list[dict], key: str) -> dict:
    """How an artefact is linked and tracked: its catalog entry's repo, track, tag,
    file and from, or the `repos` override for it, which replaces them whole. Matched
    as the inventory matches: an exact key, or the longest prefix ending in *."""
    def best(patterns):
        hits = [p for p in patterns if key == p or (p.endswith("*") and key.startswith(p[:-1]))]
        return max(hits, key=len) if hits else None
    scored = [(best(e["match"]), e) for e in catalog]
    scored = [(p, e) for p, e in scored if p]
    if not scored:
        sys.exit(f"release-data: {key} has no entry in {CATALOG.name}")
    entry = max(scored, key=lambda pe: len(pe[0]))[1]
    override = best(entry.get("repos", {}))
    spec = entry["repos"][override] if override else entry
    spec = {"repo": spec} if isinstance(spec, str) else spec
    if "repo" not in spec:
        sys.exit(f"release-data: catalog entry {entry['name']} has no repo for {key}")
    github = "://" not in spec["repo"]
    return {
        "url": f"https://github.com/{spec['repo']}" if github else spec["repo"],
        "repo": spec["repo"],
        "track": spec.get("track", "releases" if github else ""),
        "tag": spec.get("tag", RELEASE_TAG),
        "file": spec.get("file", ""),
        "from": spec.get("from", ""),
    }


def latest(t: dict, c: dict, released: str, upstream: Upstream) -> tuple[str, bool | None]:
    """The newest version of a component as of `released`, and whether it shipped."""
    image = c["source"].split("/", 1)[1] if c["source"].startswith(("docker.io/", "ghcr.io/")) else ""
    match t["track"]:
        case "":
            return "", None
        case "releases":
            version, _ = upstream.releases(t["repo"], t["tag"], released)
        case "tags":
            version, _ = upstream.tags(t["repo"], t["tag"], released)
        case "dockerhub":
            version, _ = upstream.dockerhub(image, t["tag"])
        case "ubuntu":
            version, _ = upstream.ubuntu(released)
        case "file":
            version, _ = upstream.file(t["from"], t["file"], t["tag"], released)
        case "ghcr":
            version, digest = upstream.ghcr(image, released)
            return version, (digest in c["digests"]) if version else None
        case other:
            sys.exit(f"release-data: {c['source']} has unknown track '{other}'")
    if not version:
        sys.exit(f"release-data: no {t['track']} version found for {c['source']} ({t['repo']}) "
                 f"before {released}; check its catalog entry")
    return version, current(c["version"], version)


def annotate(doc: dict, released: str, catalog: list[dict], upstream: Upstream) -> None:
    doc["released"] = released
    for c in doc["components"]:
        t = tracking(catalog, c["source"])
        c["url"] = t["url"]
        c["latest"], c["current"] = latest(t, c, released, upstream)


STATUSES = ("held", "scheduled", "not assessed", "declined")


def unexplained(doc: dict, catalog: list[dict]) -> list[str]:
    """Copy onto each component behind its latest the catalog's `behind` items for
    the versions it ships, and name every version no item explains. A reason is for
    the version it names, so a changed pin needs a new one."""
    entries = {e["name"]: e for e in catalog}
    missing = []
    for c in doc["components"]:
        c.pop("behind", None)
        if c["current"] is not False:
            continue
        items = [i for i in entries[c["component"]].get("behind") or []
                 if i.get("reason") and i.get("status") in STATUSES]
        found = []
        # A row lists every version of the artefact it ships; only those behind need
        # a reason. A build the platform makes has a commit for latest, not a version.
        numbered = re.fullmatch(r"\d+(?:\.\d+)*", c["latest"] or "")
        for v in (t.strip() for t in c["version"].split(",")):
            if numbered and current(v, c["latest"]):
                continue
            item = next((i for i in items if v in i.get("versions", [])), None)
            if not item:
                missing.append(f"{c['component']} [{v}]")
            elif item not in found:
                found.append(item)
        c["behind"] = [{k: i[k] for k in ("status", "reason", "blocked_by", "target", "ref") if i.get(k)}
                       for i in found]
    return sorted(set(missing))


def explain(doc: dict, catalog: list[dict]) -> None:
    missing = unexplained(doc, catalog)
    if missing:
        sys.exit(f"release-data: {doc['version']} ships versions behind their latest release that the "
                 f"catalog does not explain. Add a `behind` item for each to its entry in {CATALOG.name}:\n  "
                 + "\n  ".join(missing))


def inventory(v: str, owner: str, catalog: list[dict], out: Path) -> None:
    """A published version's inventory, annotated as of the day it was published."""
    days = published(owner)
    if v not in days:
        sys.exit(f"release-data: {v} is not published in the registry")
    with tempfile.TemporaryDirectory() as tmp:
        subprocess.run(
            ["helm", "pull", f"oci://ghcr.io/{owner}/charts/environment-manager", "--version", v,
             "--untar", "-d", tmp],
            check=True, stdout=subprocess.DEVNULL,
        )
        src = Path(tmp) / "environment-manager/components.json"
        if not src.exists():
            sys.exit(f"release-data: {v} carries no inventory. Build one with "
                     f"scripts/package/backfill-inventory.sh.")
        doc = json.loads(src.read_text())
    annotate(doc, days[v], catalog, Upstream())
    explain(doc, catalog)
    out.write_text(json.dumps(doc, indent=2) + "\n")
    behind = sum(1 for c in doc["components"] if c["current"] is False)
    print(f"fetched {v}: {behind} of {len(doc['components'])} components behind their latest release")


def check(path: str, catalog: list[dict]) -> None:
    """Before a version is published: which of its components are behind their latest
    release today with no reason in the catalog. Writes nothing."""
    doc = json.loads(Path(path).read_text())
    annotate(doc, datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"), catalog, Upstream())
    missing = unexplained(doc, catalog)
    behind = sum(1 for c in doc["components"] if c["current"] is False)
    print(f"check: {behind} of {len(doc['components'])} components behind their latest release, "
          f"{len(missing)} without a reason")
    if missing:
        sys.exit("  " + "\n  ".join(missing))


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--catalog", default=str(CATALOG))
    sub = ap.add_subparsers(dest="cmd", required=True)
    inv = sub.add_parser("inventory", help="a published version's inventory, annotated as of its release date")
    inv.add_argument("version")
    inv.add_argument("--owner", default="soloz-io")
    inv.add_argument("--out", required=True)
    chk = sub.add_parser("check", help="which versions of an unpublished inventory are behind with no reason")
    chk.add_argument("components_json")
    a = ap.parse_args()
    catalog_file = Path(a.catalog)
    if not catalog_file.exists():
        sys.exit(f"release-data: no component catalog at {catalog_file}")
    catalog = yaml.safe_load(catalog_file.read_text())["components"]
    if a.cmd == "inventory":
        inventory(a.version, a.owner, catalog, Path(a.out))
    else:
        check(a.components_json, catalog)


if __name__ == "__main__":
    main()
