#!/usr/bin/env python3
"""Assert the distribution renders each component exactly as its own chart did.

The per-component charts were each verified against what the cluster applies
today. Folding them into one artefact is only safe if that verification carries
over, so this compares the umbrella's output for a component against the
standalone chart it was built from -- for every topology the component varies
across, not just one.

Usage: verify-distribution.py <umbrella-chart> <packaged-charts-dir>
Exit 0 identical, 1 differing.
"""
import os
import subprocess
import sys

try:
    import yaml
except ImportError:
    sys.exit("PyYAML required")

# Topologies to compare a variant component across. Every combination the
# platform supports, because a component can be right in one and wrong in
# another and comparing only the first would not show it.
PROFILES = (("dev", "hybrid"), ("stg", "hybrid"), ("prod", "hetzner"))


def normalise(text):
    docs = [d for d in yaml.safe_load_all(text) if d]
    docs.sort(key=lambda d: (d.get("kind", ""), (d.get("metadata") or {}).get("name", "")))
    return yaml.safe_dump_all(docs, sort_keys=True), len(docs)


def render(args):
    done = subprocess.run(args, capture_output=True, text=True)
    return (None, done.stderr.strip()[:160]) if done.returncode else (done.stdout, None)


def main() -> int:
    if len(sys.argv) != 3:
        print(__doc__)
        return 2
    umbrella, src = sys.argv[1], sys.argv[2]
    charts = os.path.join(umbrella, "charts")
    available = {n for n in os.listdir(src)
                 if os.path.exists(os.path.join(src, n, "Chart.yaml"))}

    same = differ = skipped = 0
    for component in sorted(os.listdir(charts)):
        # A component that renders from values a cluster supplies is compared
        # by the release's own equivalence gate against its source, not here:
        # rendering it needs those values, and inventing them would compare two
        # guesses rather than two renders.
        if not os.path.isdir(os.path.join(charts, component, "files")):
            skipped += 1
            continue

        for env, provider in PROFILES:
            for candidate in (f"{component}-{env}-{provider}", f"{component}-{env}",
                              f"{component}-{provider}", component):
                if candidate in available:
                    origin = candidate
                    break
            else:
                continue

            # Values both sides need identically. A component that takes
            # per-cluster values renders nothing without them, and comparing two
            # empty renders would pass while proving nothing.
            shared = ["--set", "spokeName=probe",
                      "--set", f"global.environmentSlug={env}",
                      "--set", f"global.provider={provider}",
                      # Required since ADR-108 (an empty one would widen
                      # external-dns to every zone); a probe value, like the rest.
                      "--set", "global.previewDomain=example-preview.test", "--set", "global.alerting.emailTo[0]=alerts@example.test"]
            # A component that renders one TENANT'S one APP needs both named
            # (ADR-088), the same way spokeName=probe is named above: they are
            # required rather than defaulted, because an appId falling back to
            # tenantId is the single-axis model wearing the new field's name.
            # Probe values, not real ones -- this gate compares two renders of
            # the same chart, so what matters is that both sides get the same
            # identity, not which.
            shared += ["--set", "tenantId=probe",
                       "--set", "appId=probeapp",
                       "--set", "clusterName=probe-01"]
            expected, err = render(
                ["helm", "template", "x", os.path.join(src, origin)] + shared)
            if err:
                print(f"  ERROR   {component} [{env}+{provider}]: {err}")
                differ += 1
                continue
            actual, err = render([
                "helm", "template", "x", umbrella,
                "--set", f"{component}.enabled=true",
                "--set", f"{component}.spokeName=probe",
                "--set", f"{component}.tenantId=probe",
                "--set", f"{component}.appId=probeapp",
                "--set", f"{component}.clusterName=probe-01",
                "--set", f"global.environmentSlug={env}",
                "--set", f"global.provider={provider}",
                "--set", "global.previewDomain=example-preview.test", "--set", "global.alerting.emailTo[0]=alerts@example.test",
            ])
            if err:
                print(f"  ERROR   {component} [{env}+{provider}]: {err}")
                differ += 1
                continue

            exp, n_exp = normalise(expected)
            act, n_act = normalise(actual)
            if exp == act:
                same += 1
            else:
                differ += 1
                print(f"  DIFFERS {component} [{env}+{provider}]: "
                      f"standalone={n_exp} distribution={n_act}")

    print(f"distribution equivalence: {same} identical, {differ} differing, "
          f"{skipped} templated component(s) checked by the release gate instead")
    return 1 if differ else 0


if __name__ == "__main__":
    sys.exit(main())
