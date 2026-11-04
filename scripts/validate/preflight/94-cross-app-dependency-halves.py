#!/usr/bin/env python3
"""Every cross-application dependency must be declared by BOTH sides.

ADR-088's amendment introduced the declaration and named this gap explicitly:

    "It does not discover dependencies, and it does not verify that the callee
     agreed: a dependsOn naming an app whose chart declares no matching ingress
     renders a valid policy and a call that hangs."

This is that verification. A caller declaring `identity.backendDependencies:
[waypoint]` gets egress and an audience scope; the target declaring
`identity.allowedConsumers: [oranger]` gets ingress and admits the caller's azp.
Either alone is a half-built path, and the two failure shapes are both quiet:

  caller declares, target does not
      egress opens, ingress does not. Cilium DROPS denied ingress without an RST
      (ADR-046 section 30), so the call HANGS to its timeout rather than being
      refused. The token is minted correctly, the receiver is healthy, and
      nothing in either application reports a policy.

  target declares, caller does not
      the target admits a caller that never arrives, and the platform renders no
      audience scope for it -- so the day someone adds the calling code, the
      exchange fails with invalid_target naming an audience rather than a missing
      declaration.

Checked against the tenant GitOps repository, where both halves are declared.

Emits tab-separated lines the shell wrapper classifies:
    OK<TAB><message>
    BAD<TAB><message>
    NONE
"""
import os
import sys
from pathlib import Path

import yaml

REPO = os.environ.get("GITOPS_DIR", ".local-e2e/nutgraf-gitops")


def declarations(root: Path):
    """Every app's two dependency declarations, keyed (environment, appId)."""
    out = {}
    envs = root / "environments"
    if not envs.is_dir():
        return out
    for env_dir in sorted(p for p in envs.iterdir() if p.is_dir()):
        for app_dir in sorted(p for p in env_dir.iterdir() if p.is_dir()):
            values = app_dir / "values.yaml"
            if not values.is_file():
                continue
            try:
                doc = yaml.safe_load(values.read_text()) or {}
            except yaml.YAMLError as exc:
                print(f"BAD\t{values}: not parseable as YAML ({exc})")
                continue
            identity = doc.get("identity") or {}
            # A CAPABILITY an application declares is NOT a sibling dependency, and
            # must not be checked as one. ADR-109: the Credit Authority "is not an
            # application" -- it is a platform capability that runs on the hub in
            # platform-credits, with no tenant application, no allowedConsumers and
            # nothing for a sibling lookup to find. The two halves ADR-088 pairs do
            # not exist for it; the capability rule below applies instead.
            #
            # Keeping them in one list is what produced the opposite false failure:
            # the caller was reported as depending on an application of the tenant
            # that does not and should not exist.
            caps = sorted(name for name, cfg in (doc.get("capabilities") or {}).items()
                          if isinstance(cfg, dict) and cfg.get("enabled"))
            out[(env_dir.name, app_dir.name)] = (
                list(identity.get("backendDependencies") or []),
                list(identity.get("allowedConsumers") or []),
                caps,
            )
    return out



def key_grammar_agrees() -> None:
    """The chart and the operator must derive the same key name from one appId.

    TWO IMPLEMENTATIONS OF ONE RULE, in two languages:

        Helm  {{ . | upper | replace "-" "_" }}
        Go    strings.ToUpper(strings.ReplaceAll(name, "-", "_"))   OAuthKeySegment

    The operator WRITES the Infisical key with the Go rule; the chart READS it
    with the Helm rule. Nothing connects them, so a change to either produces an
    ExternalSecret asking for a key nothing wrote -- reported as
    SecretSyncedError naming a key that looks entirely plausible.

    Checked on an appId that actually exercises the rule. A single-word id agrees
    under any transform, so it would prove nothing.
    """
    import re
    import subprocess

    chart = Path("manifests/tenants/charts/universal-tenant")
    if not chart.is_dir():
        return
    out = subprocess.run(
        [
            "helm", "template", "t", str(chart),
            "--set", "oidcIssuer=https://issuer.example",
            "--set", "tenantId=t", "--set", "appId=a", "--set", "clusterName=c",
            "--set", "scopeId=t-a", "--set", "deployXR=false",
            "--set-json", 'gateway={"enabled":true,"hostnames":["h.example"]}',
            "--set-json", 'identity={"backendDependencies":["code-builder"]}',
        ],
        capture_output=True, text=True,
    )
    if out.returncode != 0:
        print("BAD\tthe tenant chart does not render, so key-name agreement cannot be checked")
        return

    expected = "CODE_BUILDER"  # what Go's OAuthKeySegment produces for code-builder
    for key in ("OIDC_BACKEND_AUDIENCE_SCOPE_", "OIDC_BACKEND_PROJECT_ID_"):
        found = set(re.findall(rf"{key}([A-Z0-9_]+)", out.stdout))
        if found == {expected}:
            print(f"OK\t{key}<appId> agrees with the operator's key grammar ({expected})")
        else:
            print(
                f"BAD\tthe chart derives {key}{found or '<nothing>'} where the operator writes "
                f"{key}{expected}. An ExternalSecret would request a key nothing publishes."
            )


def main() -> int:
    root = Path(REPO)
    if not root.is_dir():
        print("NONE")
        return 0

    decls = declarations(root)
    if not decls:
        print("NONE")
        return 0

    pairs = 0
    bad = False
    for (env, app), (deps, _, _) in sorted(decls.items()):
        for target in deps:
            pairs += 1
            key = (env, target)
            if key not in decls:
                print(
                    f"BAD\t{env}/{app} declares a dependency on {target!r}, which is not an "
                    f"application of this tenant in {env}. A dependency names a SIBLING "
                    f"application (ADR-088); cross-tenant gains no mechanism."
                )
                bad = True
                continue
            _, consumers, _ = decls[key]
            if app not in consumers:
                print(
                    f"BAD\t{env}/{app} -> {target}: {target} does not list {app!r} in "
                    f"identity.allowedConsumers. Egress would open and ingress would not, "
                    f"and Cilium drops denied ingress WITHOUT an RST -- so the call hangs "
                    f"to its timeout instead of being refused."
                )
                bad = True
            else:
                print(f"OK\t{env}/{app} -> {target}: both halves declared")

    # The reverse direction: an admission nobody asked for.
    for (env, app), (_, consumers, _) in sorted(decls.items()):
        for caller in consumers:
            key = (env, caller)
            deps = decls.get(key, ([], [], []))[0]
            if app not in deps:
                print(
                    f"BAD\t{env}/{app} admits {caller!r}, but {caller} declares no dependency "
                    f"on {app}. The platform renders no audience scope for a call nobody "
                    f"declared, so the exchange would fail with invalid_target."
                )
                bad = True

    # ── capabilities, which are not sibling applications (ADR-109) ───────────
    #
    # A capability has no second half to pair: it runs on the box, not in the
    # tenant, so there is no application to admit a caller and no allowedConsumers
    # to read. What must hold instead is that the box grants it and that its one
    # caller is unambiguous -- ADR-109 names "the single source of its one caller",
    # and two applications declaring it would make the platform mint two callers for
    # a ledger that admits one.
    #
    # The box's own declaration is read from the registry rather than assumed: an
    # application declaring a capability the box has switched off renders nothing,
    # and the chart fails with "this box does not run the Credit Authority" at sync
    # time instead of here.
    box_caps = {}
    for values in sorted((root / "registry" / "clusters").glob("*/values.yaml")):
        try:
            doc = yaml.safe_load(values.read_text()) or {}
        except yaml.YAMLError as exc:
            print(f"BAD\t{values}: not parseable as YAML ({exc})")
            bad = True
            continue
        for name, cfg in (doc.get("capabilities") or {}).items():
            if isinstance(cfg, dict) and cfg.get("enabled"):
                box_caps.setdefault(name, []).append(values.parent.name)

    declared_by = {}
    for (env, app), (_, _, caps) in sorted(decls.items()):
        for cap in caps:
            declared_by.setdefault((env, cap), []).append(app)

    for (env, cap), apps in sorted(declared_by.items()):
        if cap not in box_caps:
            print(
                f"BAD\t{env}: {', '.join(apps)} declares capability {cap!r}, which no cluster "
                f"in registry/clusters enables. The capability's resources do not render, so "
                f"the declaration buys an audience scope for something that is not running."
            )
            bad = True
            continue
        if len(apps) > 1:
            print(
                f"BAD\t{env}: capability {cap!r} is declared by {', '.join(apps)}. ADR-109 "
                f"gives it one caller, and a second declaration mints a second service "
                f"identity for a ledger that admits one."
            )
            bad = True
            continue
        print(f"OK\t{env}/{apps[0]} declares capability {cap!r}, enabled on "
              f"{', '.join(box_caps[cap])}; no sibling application is required (ADR-109)")

    if pairs == 0 and not declared_by and not bad:
        print("OK\tno cross-application dependencies declared")

    key_grammar_agrees()
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
