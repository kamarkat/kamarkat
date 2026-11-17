#!/usr/bin/env python3
"""What this platform asks Cluster API for must exist in the Cluster API it pins.

TWO INCIDENTS, ONE SHAPE. Both cost a tenant's box a full provisioning cycle,
and both were a grep away:

  * A CoreProvider declared `featureGates: ClusterResourceSet: true`. The gate
    was GA in v1.10 and deleted in v1.11, so the v1.13.7 manager exited with
    `unrecognized feature gate: ClusterResourceSet` -- while the operator
    reported ProviderInstalled=True, because it had fetched and applied the
    components correctly. CoreProvider sat at Ready=False for the whole budget.

  * A health check waited for a Cluster's `Ready` condition. A Cluster has none
    at v1.13 -- its summary condition is `Available` -- so the check read ""
    every thirty seconds for thirty minutes against a cluster that was coming up
    perfectly, and then failed the phase.

Neither is detectable by rendering, building or testing this repository: the
contract is in the Cluster API source, at the tag versions.go pins. So that is
what this reads.

It needs the vendored reference checkout (reference-projects/cluster-api). Where
that is absent it says what it could not check and exits 0 -- a gate that
reports its own blind spot, rather than a gate that passes quietly.

    python3 scripts/validate/capi-contract-parity.py
"""

from __future__ import annotations

import pathlib
import re
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parents[2]
CAPI = ROOT / "reference-projects" / "cluster-api"
VERSIONS_GO = ROOT / "internal" / "kakt-cli" / "versions" / "versions.go"
PROVIDERS = ROOT / "internal" / "assets" / "manifests" / "core" / "capi-operator" / "providers"
HEALTH = ROOT / "internal" / "kakt-cli" / "health"

PIN_RE = re.compile(r'^\s*CAPIVersion\s*=\s*"([^"]+)"', re.M)
GATE_RE = re.compile(r'^\s*([A-Za-z][A-Za-z0-9]*)\s+featuregate\.Feature\s*=\s*"([^"]+)"', re.M)
CONDITION_RE = re.compile(r'^\s*[A-Za-z][A-Za-z0-9]*Condition\s*=\s*(?:"([^"]+)"|([A-Za-z][A-Za-z0-9]*Condition))', re.M)
# What our manifests ask for: `featureGates:` blocks, one key per line.
MANIFEST_GATE_RE = re.compile(r"^\s{4,}([A-Za-z][A-Za-z0-9]*):\s*(?:true|false)\s*$")
# What our health checkers wait for. The KIND matters as much as the name: a
# Machine has a Ready condition and a Cluster does not, so "does CAPI declare
# this name anywhere" would have passed the exact defect this exists to catch.
HEALTH_CONDITION_RE = re.compile(r'Condition:\s*"([^"]+)"')
CLUSTER_CHECKER_RE = re.compile(
    r"Kind:\s*CAPIClusterKind,.*?Condition:\s*\"([^\"]+)\"", re.S)

# The kinds whose conditions are declared in the vendored repository, and where.
# The CAPI OPERATOR's kinds (CoreProvider, BootstrapProvider, ...) are a
# different repository and are not vendored, so a condition on one of those is
# reported as unchecked rather than assumed wrong -- they do have Ready.
KIND_TYPES = {
    "Cluster": "api/core/v1beta2/cluster_types.go",
    "Machine": "api/core/v1beta2/machine_types.go",
}


def show(args: list[str]) -> str | None:
    out = subprocess.run(["git", "-C", str(CAPI), "show", *args],
                         capture_output=True, text=True)
    return out.stdout if out.returncode == 0 else None


def pinned() -> str:
    m = PIN_RE.search(VERSIONS_GO.read_text())
    if not m:
        sys.exit("capi-contract-parity: versions.go declares no CAPIVersion")
    return m.group(1)


def declared_gates() -> list[tuple[str, str]]:
    """(file, gate) for every feature gate the provider manifests ask for."""
    found: list[tuple[str, str]] = []
    for path in sorted(PROVIDERS.glob("*.yaml")):
        inside = False
        for line in path.read_text().split("\n"):
            if re.match(r"^\s*featureGates:\s*$", line):
                inside = True
                continue
            if inside:
                m = MANIFEST_GATE_RE.match(line)
                if m:
                    found.append((path.relative_to(ROOT).as_posix(), m.group(1)))
                    continue
                if line.strip() and not line.strip().startswith("#"):
                    inside = False
    return found


def awaited_conditions() -> list[tuple[str, str]]:
    """(file, condition) for every CAPI condition a health checker waits on."""
    found: list[tuple[str, str]] = []
    for path in sorted(HEALTH.glob("*.go")):
        if path.name.endswith("_test.go"):
            continue
        for m in HEALTH_CONDITION_RE.finditer(path.read_text()):
            found.append((path.relative_to(ROOT).as_posix(), m.group(1)))
    return found


def cluster_conditions() -> list[tuple[str, str]]:
    """(file, condition) for every condition a checker waits on for a CAPI Cluster."""
    found: list[tuple[str, str]] = []
    for path in sorted(HEALTH.glob("*.go")):
        if path.name.endswith("_test.go"):
            continue
        for cond in CLUSTER_CHECKER_RE.findall(path.read_text()):
            found.append((path.relative_to(ROOT).as_posix(), cond))
    return found


def conditions_of(pin: str, kind: str) -> set[str]:
    """The condition names the vendored types declare for one kind."""
    rel = KIND_TYPES.get(kind)
    if rel is None:
        return set()
    src = show([f"{pin}:{rel}"]) or ""
    names: set[str] = set()
    for literal, alias in CONDITION_RE.findall(src):
        names.add(literal or alias.removesuffix("Condition"))
    # The shared aliases (`ClusterAvailableCondition = AvailableCondition`)
    # resolve through api/core/v1beta2/common_types.go.
    common = show([f"{pin}:api/core/v1beta2/common_types.go"]) or ""
    aliases = dict(re.findall(r'^\s*([A-Za-z]+Condition)\s*=\s*"([^"]+)"', common, re.M))
    return {aliases.get(n + "Condition", n) for n in names}


def main() -> int:
    pin = pinned()
    if not (CAPI / ".git").exists():
        print(f"capi-contract-parity: no checkout at {CAPI.relative_to(ROOT)}, so the "
              f"feature gates and conditions this platform asks CAPI {pin} for were NOT "
              f"checked against it")
        return 0

    gates_src = show([f"{pin}:feature/feature.go"])
    if gates_src is None:
        print(f"capi-contract-parity: {CAPI.relative_to(ROOT)} has no {pin}, so nothing was "
              f"checked; fetch it (git -C {CAPI.relative_to(ROOT)} fetch --tags)")
        return 0
    known_gates = {name for _, name in GATE_RE.findall(gates_src)}

    known_conditions: set[str] = set()
    listing = subprocess.run(["git", "-C", str(CAPI), "ls-tree", "-r", "--name-only", pin],
                             capture_output=True, text=True)
    for rel in listing.stdout.split("\n"):
        if re.match(r"api/core/v1beta2/.*_types\.go$", rel or ""):
            src = show([f"{pin}:{rel}"]) or ""
            for literal, alias in CONDITION_RE.findall(src):
                known_conditions.add(literal or alias.removesuffix("Condition"))

    failures: list[str] = []

    for path, gate in declared_gates():
        if gate not in known_gates:
            failures.append(
                f"{path} declares the feature gate {gate}, and Cluster API {pin} has no such "
                f"gate.\n    The manager exits on an unrecognised gate -- `unrecognized "
                f"feature gate: {gate}` -- while the operator still reports "
                f"ProviderInstalled=True, so the provider sits at Ready=False for the whole "
                f"timeout. {pin} knows: {', '.join(sorted(known_gates))}")

    # The Cluster's own conditions first, because that is the one that bit us.
    cluster_known = conditions_of(pin, "Cluster")
    for path, cond in cluster_conditions():
        if cluster_known and cond not in cluster_known:
            failures.append(
                f"{path} waits for a CAPI Cluster's {cond} condition, and a Cluster has no "
                f"such condition at {pin}.\n    It reads as the empty string for the whole "
                f"timeout, which is indistinguishable from a condition that is not true yet -- "
                f"so a cluster coming up perfectly fails its phase. A Cluster declares: "
                f"{', '.join(sorted(cluster_known))}")

    for path, cond in awaited_conditions():
        if cond not in known_conditions:
            failures.append(
                f"{path} waits for the condition {cond}, and no Cluster API {pin} type "
                f"declares it.\n    A condition that does not exist reads as the empty string "
                f"for the whole timeout, which is indistinguishable from one that is not true "
                f"yet. {pin} declares: {', '.join(sorted(known_conditions))}")

    for line in failures:
        print(f"capi-contract-parity: {line}", file=sys.stderr)

    print(f"capi-contract-parity: CAPI {pin}, {len(cluster_conditions())} Cluster "
          f"condition(s) against {len(cluster_known)} a Cluster has, "
          f"{len(declared_gates())} declared gate(s) against "
          f"{len(known_gates)} known, {len(awaited_conditions())} awaited condition(s) against "
          f"{len(known_conditions)} known, {len(failures)} failure(s)")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
