#!/usr/bin/env python3
"""Resolve every spoke ExternalSecret's effective remoteRef.key and assert it lands
inside the cell's ADR-031 prefix.

Kept as a real file rather than a heredoc: the logic parses YAML that itself contains
quotes and Go-template braces, and embedding it in `$(... <<'PY' ...)` has already
produced two separate bash parsing failures.

Emits tab-separated lines the shell wrapper classifies:
    OK<TAB><message>
    BAD<TAB><name><TAB><message>
    NONE                     (nothing rendered)
"""
import subprocess
import sys

import yaml

CELL = "test-cell"  # stand-in for the ArgoCD {{.name}} substitution
ALLOWED = (f"/spoke-pool/{CELL}/shared/", f"/spoke-pool/{CELL}/tenants/")
CATALOG = "manifests/spoke/spoke-catalog/infra"

# Known root-path consumers, recorded so the rule is enforceable TODAY while the
# debt stays visible — the same pattern 99-tenant-identifiers.sh uses.
#
# These are real ADR-031 violations: the cell Machine Identity cannot read a root
# path, so they will fail even after the spoke store is healthy. They are NOT fixed
# here because the fix is a producer change — something must write the value to
# /spoke-pool/<cell>/shared/ — and the ownership/generation contract for that is a
# deliberate architecture decision, not a path rewrite.
#
#   crossplane-admin-credentials  <cell>-crossplane-admin-password
#       already per-cell BY NAME, at the root. hub-operator generates it
#       (EnsureCrossplaneAdminPassword); moving it under the cell prefix is a
#       producer-side change.
#   hetzner-dns-credentials       hcloud-token
#       fleet-constant. Per the Grafana Cloud precedent it should be materialised
#       into each cell path rather than read from a shared root.
#
# Remove an entry the moment its producer writes the cell-scoped path. The list
# only shrinks.
BASELINE = {
    "crossplane-admin-credentials",
    "hetzner-dns-credentials",
}
CHART = "manifests/argocd/environment-manager"
APPSET = "platform-spoke-catalog"


def die(msg):
    print(f"BAD\t-\t{msg}")
    sys.exit(0)


def run(cmd, what):
    r = subprocess.run(cmd, capture_output=True, text=True)
    if r.returncode != 0:
        first = (r.stderr.strip().splitlines() or ["unknown error"])[0]
        die(f"{what}: {first}")
    return r.stdout


# ── 1. the ExternalSecrets the spoke actually receives ───────────────────────
secrets = {}
for doc in yaml.safe_load_all(run(["kubectl", "kustomize", CATALOG],
                                  "spoke-catalog does not build")):
    if not doc or doc.get("kind") != "ExternalSecret":
        continue
    name = doc["metadata"]["name"]
    if doc["spec"].get("dataFrom"):
        # dataFrom has no /spec/data/N index for a JSON6902 patch to target, so it
        # cannot be re-scoped per cell at all.
        print(f"BAD\t{name}\tuses dataFrom — no index for a per-cell patch to target")
    # An entry sourced from a GENERATOR reads nothing from the secret store, so the
    # cell prefix does not apply to it: ADR-031 constrains what a spoke's SecretStore
    # is authorised to read, and a generatorRef never consults the store. Its
    # remoteRef.key names a field of the generated secret (the Password generator's
    # `password`), not an Infisical path -- reported as a root-path consumer, it would
    # push the manifest back to `dataFrom`, which has no index for a per-cell patch
    # and is the shape this check exists to forbid.
    secrets[name] = [d.get("remoteRef", {}).get("key", "")
                     for d in (doc["spec"].get("data") or [])
                     if not (d.get("sourceRef") or {}).get("generatorRef")]

if not secrets:
    print("NONE")
    sys.exit(0)

# ── 2. apply the ApplicationSet patches the way ArgoCD does ──────────────────
rendered = run(["helm", "template", CHART,
                "--set", "environmentSlug=dev",
                "--set", "provider=hybrid",
                "--set", "environmentRevision=main",
                # ADR-111 keys the escrow on it and the chart refuses to assume one.
                "--set", "boxId=example-box",
                # ADR-055: every boundary renders now, so boundary 06's issuer
                # guard applies here too. ADR-051 gives it no default on purpose.
                "--set", "publicTlsIssuer=letsencrypt-prod",
                # A placeholder, but a required one. The tenant-workloads
                # AppProject asks for this and the chart refuses to guess it
                # (ADR-062: it names the repository holding the box's own
                # declarations, and a wrong value grants a box trust over
                # someone else's repository). Without it the render failed on a
                # missing value and this check reported that instead of what it
                # tests -- so the spoke ExternalSecret prefixes it exists to
                # assert were never actually checked.
                #
                # The value only has to parse as https://host/org/repo; nothing
                # here resolves it.
                "--set", "instanceRepoURL=https://github.com/example-org/example-gitops",
                # Same reasoning: the chart refuses to default this, because a
                # box falling back to the platform's own domain would publish
                # the platform's hostnames (ADR-051, ADR-065).
                "--set", "hubDomain=example.test",
                "--set", "global.hubDomain=example.test",
                "--set", "previewDomain=example-preview.test", "--set", "alerting.emailTo[0]=alerts@example.test",
                "--set", "global.previewDomain=example-preview.test"],
               "environment-manager chart does not render")

appset = next((d for d in yaml.safe_load_all(rendered)
               if d and d.get("kind") == "ApplicationSet"
               and d.get("metadata", {}).get("name") == APPSET), None)
if appset is None:
    die(f"ApplicationSet {APPSET} not found in the rendered chart")

source = ((appset["spec"].get("template") or {}).get("spec") or {}).get("source") or {}
patches = (source.get("kustomize") or {}).get("patches") or []

for p in patches:
    target = p.get("target") or {}
    if target.get("kind") != "ExternalSecret":
        continue
    name = target.get("name")
    if name not in secrets:
        print(f"BAD\t{name}\tApplicationSet patches an ExternalSecret the spoke-catalog does not render")
        continue
    try:
        ops = yaml.safe_load(p.get("patch") or "") or []
    except yaml.YAMLError as exc:
        print(f"BAD\t{name}\tpatch is not parseable YAML: {exc}")
        continue
    for op in ops:
        if not isinstance(op, dict) or op.get("op") != "replace":
            continue
        path = str(op.get("path", ""))
        if not (path.startswith("/spec/data/") and path.endswith("/remoteRef/key")):
            continue
        try:
            idx = int(path.split("/")[3])
        except (IndexError, ValueError):
            print(f"BAD\t{name}\tunparseable patch path {path!r}")
            continue
        if idx >= len(secrets[name]):
            print(f"BAD\t{name}\tpatch targets /spec/data/{idx} but the manifest has "
                  f"{len(secrets[name])} entr(ies) — index drift")
            continue
        secrets[name][idx] = str(op.get("value", "")).replace("{{.name}}", CELL)

# ── 3. every resolved key must land inside the cell prefix ───────────────────
clean = True
baselined = []
for name, keys in sorted(secrets.items()):
    for i, key in enumerate(keys):
        if not key or key.startswith(ALLOWED):
            continue
        if name in BASELINE:
            baselined.append(f"{name} data[{i}] -> {key}")
            continue
        clean = False
        if "PLACEHOLDER" in key:
            print(f"BAD\t{name}\tdata[{i}] still resolves to {key!r} — "
                  f"no ApplicationSet patch substitutes the cell id")
        else:
            print(f"BAD\t{name}\tdata[{i}] resolves to {key!r}, outside "
                  f"/spoke-pool/<cell>/{{shared,tenants}}/ (ADR-031)")

if clean:
    n = len(secrets) - len(BASELINE)
    print(f"OK\t{n} spoke ExternalSecret(s) resolve inside the cell prefix; "
          f"no new root-path consumers")
for b in baselined:
    print(f"NOTE\t  root-path (ADR-031 debt, pending producer decision): {b}")
