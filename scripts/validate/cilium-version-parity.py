#!/usr/bin/env python3
"""Every Cilium image a box installs is the one version this platform pins, and
every cell's cilium-config is spelled for the Cilium it is delivered with.

WHY BOTH HALVES, and why neither alone would have caught what happened.

The version used to live only in image strings inside three manifests. Commit
4a086204 -- one omnibus bump -- raised the shared spoke-bootstrap addon from
1.17.18 to 1.20.4. The hetzner hub installs that same artifact, so the hub's CNI
moved with it and nothing said so. A pin alone catches that.

The config delivered beside it kept `unmanaged-pod-watcher-interval: '15'`, which
is exactly what v1.17's chart renders. v1.20's renders `'15s'`, because the field
became a duration -- and a bare integer is fatal: cilium-operator exits on boot
with `time: missing unit in duration "15"`. The agent still starts, so the nodes
still report Ready and the cluster looks built; with no operator there is no
IPAM, every pod that is not hostNetwork sits in ContainerCreating, and the box
dies much later somewhere else entirely. On nutgraf-mgmt-1794768541 it died 25
minutes into ArgoCD's pre-install hook, naming neither Cilium nor the field. A
pin would NOT have caught that: the images were self-consistent. The spelling has
to be checked against the version it is paired with.

Run from the repository root:

    python3 scripts/validate/cilium-version-parity.py
"""

from __future__ import annotations

import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parents[2]

# THE PIN, read from the one place that declares it. Parsed rather than copied:
# a second literal here is a second thing to forget.
VERSIONS_GO = ROOT / "internal" / "kakt-cli" / "versions" / "versions.go"
PIN_RE = re.compile(r'^\s*CiliumVersion\s*=\s*"([^"]+)"', re.M)

# Trees that ship. internal/platform/embedded is generated from these at release
# time, so checking it would report every finding twice and let a fix to the
# generated copy look like a fix.
TREES = ("internal/assets/manifests", "manifests")

IMAGE_RE = re.compile(r"quay\.io/cilium/(?P<repo>[a-z0-9-]+):(?P<version>v[0-9]+\.[0-9]+\.[0-9]+)")

# Images whose version is their own, not Cilium's: cilium-envoy follows Envoy's
# numbering and certgen is released separately.
NOT_CILIUM = {"cilium-envoy", "certgen"}

# EXCEPTIONS, each with the condition that retires it.
#
# An exception is not a pass: it is a recorded disagreement with the pin, and it
# exists only where the fix is a chart re-render rather than an edit.
EXCEPTIONS = {
    "manifests/providers/hybrid/k8s/cilium-addon-hybrid.yaml":
        "the hybrid cell is pinned to v1.17.18 until its addon is re-rendered from "
        "the pinned chart with hybrid's own values (standalone cilium-envoy, the "
        "mangle guard). ADR-075's exit condition retires this cell; this entry goes "
        "with it.",
    "manifests/providers/hybrid/cilium-values.yaml":
        "the values the hybrid addon above is rendered from; it moves when that "
        "addon is re-rendered, not before.",
}

# Each cell's config base and the addon whose Cilium it is delivered with. This
# pairing is the thing that broke, so it is written down rather than inferred.
PAIRINGS = (
    (
        "manifests/providers/hetzner/k8s/cilium-config-base.yaml",
        "manifests/spoke/spoke-bootstrap/cilium-addon-template.yaml",
    ),
    (
        "manifests/providers/hybrid/k8s/cilium-config-base.yaml",
        "manifests/providers/hybrid/k8s/cilium-addon-hybrid.yaml",
    ),
)

# unmanaged-pod-watcher-interval: a duration from v1.18 on, an integer of seconds
# before it. v1.20's chart renders `printf "%ds"`; v1.17's renders the bare value.
DURATION_FROM_MINOR = 18
INTERVAL_RE = re.compile(r"^\s*unmanaged-pod-watcher-interval:\s*['\"]?([^'\"\s#]+)", re.M)


def pinned_version() -> str:
    m = PIN_RE.search(VERSIONS_GO.read_text())
    if not m:
        sys.exit(f"cilium-version-parity: {VERSIONS_GO} declares no CiliumVersion")
    return m.group(1)


def minor_of(version: str) -> int:
    return int(version.lstrip("v").split(".")[1])


def shipped_yaml() -> list[pathlib.Path]:
    out: list[pathlib.Path] = []
    for tree in TREES:
        base = ROOT / tree
        if not base.is_dir():
            continue
        out.extend(sorted(p for p in base.rglob("*.yaml") if p.is_file()))
        out.extend(sorted(p for p in base.rglob("*.yml") if p.is_file()))
    return out


def addon_version(rel: str) -> str | None:
    """The Cilium version an addon installs, read from its own images."""
    text = (ROOT / rel).read_text()
    for m in IMAGE_RE.finditer(text):
        if m.group("repo") not in NOT_CILIUM:
            return m.group("version")
    return None


def main() -> int:
    pin = pinned_version()
    failures: list[str] = []
    excused: list[str] = []
    checked = 0

    for path in shipped_yaml():
        rel = path.relative_to(ROOT).as_posix()
        versions = {
            m.group("version")
            for m in IMAGE_RE.finditer(path.read_text())
            if m.group("repo") not in NOT_CILIUM
        }
        if not versions:
            continue
        checked += 1
        off = sorted(v for v in versions if v != pin)
        if not off:
            continue
        if rel in EXCEPTIONS:
            excused.append(f"{rel}: {', '.join(off)} -- {EXCEPTIONS[rel]}")
            continue
        failures.append(
            f"{rel} installs Cilium {', '.join(off)}, and this platform pins {pin}.\n"
            f"    versions.go is the declaration; a manifest that disagrees with it "
            f"installs a CNI nobody chose."
        )

    # The spelling, against the version each config base is actually paired with.
    pairs = 0
    for config_rel, addon_rel in PAIRINGS:
        config, addon = ROOT / config_rel, ROOT / addon_rel
        if not config.is_file() or not addon.is_file():
            failures.append(f"{config_rel} or {addon_rel} is missing, so their pairing cannot be checked")
            continue
        installed = addon_version(addon_rel)
        if installed is None:
            failures.append(f"{addon_rel} names no Cilium image, so {config_rel} cannot be checked against it")
            continue
        m = INTERVAL_RE.search(config.read_text())
        if m is None:
            # Absent is fine: the operator's own default applies, and it is
            # spelled correctly for whatever version reads it.
            continue
        pairs += 1
        value = m.group(1)
        has_unit = not value.isdigit()
        wants_unit = minor_of(installed) >= DURATION_FROM_MINOR
        if wants_unit and not has_unit:
            failures.append(
                f"{config_rel} sets unmanaged-pod-watcher-interval: '{value}' and is delivered with "
                f"Cilium {installed}, which parses it as a duration.\n"
                f"    cilium-operator will exit on boot with `time: missing unit in duration \"{value}\"`, "
                f"there will be no IPAM, and every pod that is not hostNetwork will sit in "
                f"ContainerCreating. Write it with a unit, as that version's chart does: '{value}s'."
            )
        elif not wants_unit and has_unit:
            failures.append(
                f"{config_rel} sets unmanaged-pod-watcher-interval: '{value}' and is delivered with "
                f"Cilium {installed}, whose operator reads this field as an integer number of seconds.\n"
                f"    A unit is rejected there. Write the bare number: '{value.rstrip('smh')}'."
            )

    for line in excused:
        print(f"cilium-version-parity: excused {line}")
    for line in failures:
        print(f"cilium-version-parity: {line}", file=sys.stderr)

    print(
        f"cilium-version-parity: pin {pin}, {checked} manifest(s) carrying a Cilium image, "
        f"{pairs} config pairing(s), {len(excused)} excused, {len(failures)} failure(s)"
    )
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
