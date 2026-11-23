#!/usr/bin/env python3
"""Every value universal-tenant requires has exactly one supplier (ADR-082).

The chart renders one application of one tenant onto one cluster, and it marks
six values `required`. Each must come from one of two places and nothing else:

  * the APPLICATION's own values.yaml, hydrated by `kakt tenant add-app` from
    manifests/tenants/gitops-template/templates/app/values.yaml -- what the
    tenant declares; or
  * a helm PARAMETER in 05-tenant-app-appset.yaml -- what the platform resolves.

WHY THIS EXISTS. clusterName used to come from the application's values.yaml,
which made the application's declaration its own placement decision. ADR-082 took
that away: an environment's applications go to every cluster serving that
environment, chosen by the ApplicationSet's cluster generator. So the field was
dropped from what the CLI writes -- and nothing replaced it on the platform side,
so the first application declared afterwards could not render at all:

    Error: execution error at (universal-tenant/templates/
    workload-platform-identity.yaml:5:49): clusterName must be supplied

Observed on nutgraf-waypoint-stg / nutgraf-stg-01, 2026-11-22. Every existing
application kept working, because each still carried the field its own
declaration was written with -- so the break was invisible until a NEW one was
declared, three weeks later.

NOT CAUGHT BY THE RENDER CHECK. 10-manifest-renders.sh renders this chart with a
fixture that passes `--set clusterName=spoke-01`, which is precisely the value the
ApplicationSet is supposed to supply. A fixture that supplies what the platform
owes cannot see the platform failing to owe it.

Emits tab-separated lines the shell wrapper classifies:
    OK<TAB><message>
    BAD<TAB><message>
"""
import re
import subprocess
import tempfile
from pathlib import Path

import yaml

CHART = Path("manifests/tenants/charts/universal-tenant")
APPSET = Path("manifests/argocd/environment-manager/templates/05-tenant-app-appset.yaml")
APP_VALUES = Path("manifests/tenants/gitops-template/templates/app/values.yaml")

out = []


def main():
    required = set()
    # .tpl as well as .yaml. Globbing only *.yaml missed _helpers.tpl, where
    # tenantId and appId are required -- so two of the six were silently outside
    # the check that exists to say who supplies them.
    paths = sorted((CHART / "templates").rglob("*.yaml")) + \
        sorted((CHART / "templates").rglob("*.tpl"))
    for path in paths:
        for name in re.findall(r'required\s+"[^"]*"\s+\.Values\.([A-Za-z0-9_]+)', path.read_text()):
            required.add(name)
    if not required:
        out.append(("BAD", f"no `required` values found under {CHART}/templates; this check "
                           f"would pass vacuously"))
        return

    # A NON-EMPTY default only. `required` rejects an empty string, so
    # `oidcIssuer: ""` in the chart's values declares that the key exists and
    # supplies nothing -- counting it as a supplier reported the normal
    # arrangement (key declared here, value passed by the platform) as a conflict.
    chart_values = yaml.safe_load((CHART / "values.yaml").read_text()) or {}
    chart_defaults = {k for k, v in chart_values.items() if v not in (None, "", [], {})}

    # What the tenant declares. The CLI hydrates this template into the
    # application's directory, so its keys are the application's vocabulary --
    # read from the template rather than from a list here, so a key added to one
    # and not the other cannot pass.
    declared = set(yaml.safe_load(APP_VALUES.read_text()) or {})

    appset = APPSET.read_text()
    # The parameter names the platform passes, in any of the appsets this file
    # declares. Helm parameters override a values file, which is what makes them
    # the right place for a value the application must not assert.
    # Comment lines between `name:` and `value:` are legal YAML, and requiring the
    # two to be adjacent made a correctly-supplied parameter invisible: hubDomain was
    # reported unsupplied because its rationale sat between them.
    supplied = set(re.findall(
        r"-\s+name:\s+([A-Za-z0-9_]+)\s*\n(?:\s*#[^\n]*\n)*\s+value:", appset))
    if not supplied:
        out.append(("BAD", f"no helm parameters parsed from {APPSET}; the check cannot tell "
                           f"what the platform supplies"))
        return

    for name in sorted(required):
        homes = []
        if name in declared:
            homes.append("the application's declaration")
        if name in supplied:
            homes.append("an ApplicationSet parameter")
        if name in chart_defaults:
            homes.append("the chart's own default")

        if not homes:
            out.append(("BAD", f"universal-tenant requires .Values.{name} and nothing supplies it: "
                               f"it is not a key {APP_VALUES} declares, not a parameter "
                               f"{APPSET.name} passes, and has no default in the chart. Every "
                               f"application declared from now on fails to render with "
                               f"\"{name} must be supplied\" -- and every application declared "
                               f"BEFORE the omission keeps working, so nothing says so until a "
                               f"tenant adds one"))
        elif len(homes) > 1:
            out.append(("BAD", f"universal-tenant requires .Values.{name} and {len(homes)} things "
                               f"supply it: {', '.join(homes)}. One of them wins silently -- a "
                               f"parameter beats a values file -- so the other is a declaration "
                               f"that looks honoured and is not"))
        else:
            out.append(("OK", f".Values.{name} is required and comes from {homes[0]}"))


def generated_destination_wins():
    """A stale clusterName in an application's values file loses to the generator.

    The appsets pass clusterName as a PARAMETER rather than through a values file
    precisely so that an application declared before ADR-082 -- which still
    carries the field its own declaration was written with -- is placed where the
    generator says, not where it says. That depends on Helm's precedence, which is
    a fact about Helm and not about us, so it is asserted rather than assumed:
    every pre-ADR-082 application on a box is relying on it.
    """
    stale, generated = "stale-cluster-01", "generated-cluster-02"
    with tempfile.NamedTemporaryFile("w", suffix=".yaml", delete=False) as f:
        # The shape an older application still has on disk.
        yaml.safe_dump({"tenantId": "example", "hubDomain": "box.example.test", "appId": "demo",
                        "clusterName": stale, "oidcIssuer": "https://id.example.test"}, f)
        values = f.name

    r = subprocess.run(["helm", "template", str(CHART), "-f", values,
                        "--set", f"clusterName={generated}"],
                       capture_output=True, text=True, check=False)
    Path(values).unlink(missing_ok=True)
    if r.returncode != 0:
        tail = (r.stderr.strip().splitlines() or ["(no output)"])[-1]
        out.append(("BAD", f"universal-tenant does not render with an application's values plus "
                           f"a clusterName parameter, which is exactly how the ApplicationSets "
                           f"render it: {tail}"))
        return

    if stale in r.stdout:
        out.append(("BAD", f"a stale clusterName in an application's values file reached the "
                           f"rendered output ({stale!r}). An application declared before ADR-082 "
                           f"carries that field, and it would place the application on the cluster "
                           f"IT names instead of the one the generator selected -- which is the "
                           f"placement decision ADR-082 took away from applications"))
        return
    if f"/spoke-pool/{generated}/" not in r.stdout:
        out.append(("BAD", f"the clusterName parameter did not reach the rendered Infisical path; "
                           f"expected /spoke-pool/{generated}/ in the output"))
        return
    out.append(("OK", f"a clusterName parameter overrides a stale one in an application's values "
                      f"file, so an application declared before ADR-082 is placed where the "
                      f"generator says"))


main()
generated_destination_wins()
for level, message in out:
    print(f"{level}\t{message}")
