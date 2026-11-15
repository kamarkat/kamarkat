#!/usr/bin/env python3
"""Fail when a shipped script downloads a tool that only runs on one platform.

hub-bootstrap.sh installed its own prerequisites, and every branch of that
installer fetched a Windows artefact whatever the host was:

    kind-windows-amd64
    clusterctl-windows-amd64.exe
    helm-v3.17.3-windows-amd64.zip
    jq-windows-amd64.exe

It was written on a Windows workstation and no Linux path reached it until the
tenant launcher started running the bootstrap in CI. There it reported
"clusterctl installed to ~/bin/clusterctl.exe" and then "could not be
auto-installed": it had downloaded a PE binary, chmod'ed it, and found it
unrunnable. A success line followed by a failure line about the same tool, and
no gate between the two.

WHAT IS CHECKED, over the scripts a released box actually runs -- the closure
runtime-script-closure.py computes, plus the two entry points. A script this
repository keeps for a workstation but never ships cannot break a tenant's
bootstrap, and is not gated here.

  1. No download URL names one platform literally. A URL may name a platform
     only through a variable the script resolved from `uname` -- which is what
     tool_platform() exists for. A download whose target is a known machine
     rather than the host running the script -- a provisioned Linux node, say --
     declares that on the line above:

         # portable-tool-install: cross-target the Flatcar node is linux/amd64
  2. No `/latest/` download. A bootstrap that resolves a different tool version
     on each run is not the same bootstrap, and nothing downstream can state
     which version a box was built with.
  3. Every tool fetched by fetch_tool passes a checksum URL, or appears in
     UNVERIFIABLE below with the reason. The entry is the record that upstream
     publishes nothing to verify against, so the gap is declared rather than
     discovered.

Documentation and comments are not checked: naming the old artefacts is how the
defect stays explained.

Usage: portable-tool-install.py [repo-root]
"""
import os
import re
import subprocess
import sys

# Upstream publishes no per-artefact checksum for these. Keyed by the tool as
# auto_install_tool names it; the value is why, and it is read by a human.
UNVERIFIABLE = {
    "clusterctl": "the Cluster API project publishes no checksum file for its "
                  "clusterctl release assets; pinned by version and release URL",
}

PLATFORM_LITERAL = re.compile(
    r"(windows|darwin|linux)[-_/](amd64|arm64|x86_64|aarch64)|"
    r"(amd64|arm64)[-_/](windows|darwin|linux)",
    re.IGNORECASE,
)
DOWNLOAD = re.compile(r"\b(curl|wget)\b")
URL = re.compile(r"https?://[^\s\"'|)]+")
FETCH_TOOL = re.compile(r"^\s*fetch_tool\s+(.*)$")
MARKER = re.compile(r"portable-tool-install:\s*cross-target\s+(\S.*)$")
# Resolving the newest release of THIS product is what an installer is for; the
# rule is about third-party tools whose version a box is built with.
OWN_RELEASE_HOSTS = ("api.github.com",)


def shipped_scripts(root):
    """The scripts a released box runs, from the closure validator itself.

    Derived rather than listed for the same reason the closure is: a validator
    with its own copy of the list checks a different set than the release
    carries.
    """
    out = subprocess.run(
        [sys.executable, "-I", os.path.join(root, "scripts/validate/runtime-script-closure.py"),
         "--list", root],
        capture_output=True, text=True, check=True,
    )
    for rel in out.stdout.split():
        if rel.endswith(".sh"):
            yield rel


def code_lines(path):
    """Logical lines: comments dropped, `\\` continuations joined.

    A fetch_tool call wraps, and a validator reading physical lines saw its
    checksum argument as absent.
    """
    out = []
    pending_marker = None
    with open(path, encoding="utf-8") as fh:
        physical = fh.read().split("\n")
    i = 0
    while i < len(physical):
        raw = physical[i]
        n = i + 1
        stripped = raw.strip()
        if stripped.startswith("#"):
            m = MARKER.search(stripped)
            if m:
                pending_marker = m.group(1)
            i += 1
            continue
        line = raw
        while line.rstrip().endswith("\\") and i + 1 < len(physical):
            i += 1
            line = line.rstrip()[:-1] + " " + physical[i]
        out.append((n, line, pending_marker))
        pending_marker = None
        i += 1
    return out


def main():
    root = sys.argv[1] if len(sys.argv) > 1 else "."
    failures = []
    checked = 0
    fetches = 0

    for rel in shipped_scripts(root):
        path = os.path.join(root, rel)
        if not os.path.exists(path):
            continue
        checked += 1
        for n, line, marker in code_lines(path):
            for url in URL.findall(line):
                # A log line printing a documentation URL is not a download.
                if not DOWNLOAD.search(line):
                    continue
                if any(h in url for h in OWN_RELEASE_HOSTS):
                    continue
                if PLATFORM_LITERAL.search(url) and not marker:
                    failures.append(
                        f"{rel}:{n}: downloads a platform-specific artefact by "
                        f"literal name: {url}\n"
                        f"    resolve the platform from uname (tool_platform) "
                        f"and build the name from it"
                    )
                if ("/latest/" in url or url.rstrip("/").endswith("/latest")) and not marker:
                    failures.append(
                        f"{rel}:{n}: downloads /latest/: {url}\n"
                        f"    pin the version this release is tested with"
                    )

            m = FETCH_TOOL.match(line)
            if m:
                fetches += 1
                args = m.group(1)
                # fetch_tool <url> <dest> [checksum_url]; the third argument is
                # the verification. Continuations keep the call on one line.
                fields = args.split()
                declared = next((t for t in UNVERIFIABLE if t in args), None)
                if len(fields) < 3 and not declared:
                    failures.append(
                        f"{rel}:{n}: fetch_tool with no checksum URL: {args.strip()}\n"
                        f"    pass the checksum upstream publishes, or record the "
                        f"tool in UNVERIFIABLE in this validator with the reason"
                    )

    # Each declared exception must still be a real call site, so the list cannot
    # outlive the code it excuses.
    bootstrap = os.path.join(root, "scripts", "hub-bootstrap.sh")
    if os.path.exists(bootstrap):
        body = open(bootstrap, encoding="utf-8").read()
        for tool in UNVERIFIABLE:
            if f"        {tool})" not in body:
                failures.append(
                    f"scripts/validate/portable-tool-install.py: UNVERIFIABLE lists "
                    f"'{tool}', which auto_install_tool no longer installs; "
                    f"remove the entry"
                )

    if failures:
        for f in failures:
            print(f"FAIL {f}")
        print(f"\nportable-tool-install: {len(failures)} failure(s)")
        return 1
    print(
        f"portable-tool-install: {checked} script(s), {fetches} tool download(s), "
        f"{len(UNVERIFIABLE)} declared unverifiable, 0 failure(s)"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
