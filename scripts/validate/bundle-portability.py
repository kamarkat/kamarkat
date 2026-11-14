#!/usr/bin/env python3
"""Fail a release whose bundle still fetches from the platform's repository.

Reads a rendered bundle on stdin and the platform's repository URL as argv[1].

The URL is NOT hardcoded here. It was, as the substring "zero-ops", and the
platform's repository had since been renamed -- so no reference could ever match
it and this gate reported the one thing it exists to disprove, on every release,
for as long as it had been running. The URL now comes from the single place that
declares it, the bundle chart's own platformRepoURL value, so renaming the
repository cannot silently disarm the check.

ADR-063 requires every artefact a tenant's runtime needs to be mirrored into
infrastructure the tenant controls before that runtime is supported, and ADR-065
that revoking the platform's access stops proposals arriving and stops nothing
running. Both are false for anything a released cluster still reaches into the
platform's repository for: the bundle is then portable only in appearance, and
the gap stays invisible until the day that repository is not reachable.

Exits non-zero when any such reference remains, so a release cannot ship one.
"""
import sys
import yaml


def repo_identity(url: str) -> tuple[str, ...] | None:
    """The host and first two path segments of an http(s) repository URL.

    Compared as segments rather than as a substring or a prefix: a prefix match
    reports github.com/<org>/<name>-docs as the platform repository, and a
    substring match reports any URL that happens to contain the org name.
    """
    if not url.startswith(("http://", "https://")):
        return None
    rest = url.split("://", 1)[1]
    host, _, path = rest.partition("/")
    parts = [seg for seg in path.split("/") if seg]
    if len(parts) < 2:
        return None
    parts[1] = parts[1].removesuffix(".git")
    return (host.lower(), parts[0].lower(), parts[1].lower())


def main() -> int:
    if len(sys.argv) != 2 or not sys.argv[1].strip():
        print(
            "bundle-portability: the platform repository URL is required as the "
            "only argument; without it there is nothing to compare against and a "
            "pass would mean nothing",
            file=sys.stderr,
        )
        return 2
    platform = repo_identity(sys.argv[1].strip())
    if platform is None:
        print(
            f"bundle-portability: {sys.argv[1]!r} is not an http(s) repository URL "
            "with an owner and a name, so no reference could be matched against it",
            file=sys.stderr,
        )
        return 2

    refs = set()
    for doc in yaml.safe_load_all(sys.stdin):
        if not doc or doc.get("kind") != "ApplicationSet":
            continue
        name = doc["metadata"]["name"]

        def note(kind: str, url: str) -> None:
            # A registry URL is scheme-less; anything naming the platform's
            # repository over http(s) is a runtime dependency on it.
            if url and repo_identity(url) == platform:
                refs.add((name, kind))

        for gen in doc["spec"].get("generators", []):
            for key, value in gen.items():
                if key == "git":
                    note("generator", value.get("repoURL", ""))
                elif key == "matrix":
                    for sub in value.get("generators", []):
                        if "git" in sub:
                            note("generator", sub["git"].get("repoURL", ""))
        spec = doc["spec"]["template"]["spec"]
        for src in [spec["source"]] if "source" in spec else spec.get("sources", []):
            note("source", src.get("repoURL", ""))

    if not refs:
        print("bundle portability: nothing reaches the platform repository")
        return 0
    print(f"bundle portability: {len(refs)} reference(s) still reach the platform repository")
    for name, kind in sorted(refs):
        print(f"  {name} ({kind})")
    print()
    print("Each is content a released cluster cannot obtain from the bundle alone,")
    print("so this bundle cannot be mirrored into a tenant's own infrastructure and")
    print("the custody ADR-063 states would be asserted and untrue for every tenant")
    print("that received it. A release carrying one of these is refused.")
    return 1


if __name__ == "__main__":
    sys.exit(main())
