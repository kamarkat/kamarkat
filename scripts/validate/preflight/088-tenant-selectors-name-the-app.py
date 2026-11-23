#!/usr/bin/env python3
"""Every pod selector a tenant composition renders names the application.

A tenant namespace holds one application (ADR-088), but it can still hold pods
the composition no longer renders: renaming a composed object makes
provider-kubernetes create the new one and leave the old one running. A selector
that matches on the component alone picks up that leftover.

On 2026-10-15 that is what lost wake-ups. The ADR-088 rename left
nutgraf-waypoint-cache beside waypoint-cache in tenant-nutgraf-waypoint. Both
Services selected `app.kubernetes.io/component: cache`, so each fronted two
independent Redis servers and every connection landed on either. The SDK
published to one; browsers subscribed on the other; nothing errored.

So each Service selector, workload selector and pod template a tenant
composition renders must carry `app-id`, patched from spec.appId.

And each workload must carry what Kyverno's require-cost-labels admits
(spoke-catalog/infra/kyverno-tenant-abi.yaml): tenant-id, app-id and
cost-center, each a literal or patched. A workload created before that rule
keeps running without them and is refused only when it is recreated -- the
cache Deployment was, on 2026-10-15, and the tenant was left with no cache.

Emits tab-separated lines the shell wrapper classifies:
    OK<TAB><message>
    BAD<TAB><message>
"""
from pathlib import Path

import yaml

COMPOSITIONS = Path("manifests/hub-core-services/crossplane/tenant-platform/compositions")

# kind -> the label maps that must carry app-id, as patch target paths.
SELECTORS = {
    "Service": ["spec.selector"],
    "Deployment": ["spec.selector.matchLabels", "spec.template.metadata.labels"],
    "StatefulSet": ["spec.selector.matchLabels", "spec.template.metadata.labels"],
}


# Kyverno require-cost-labels: the kinds it matches, and the labels it requires.
ADMITTED_KINDS = {"Deployment", "Job", "Rollout"}
COST_LABELS = ("tenant-id", "app-id", "cost-center")


def dig(doc, dotted):
    for key in dotted.split("."):
        if not isinstance(doc, dict):
            return None
        doc = doc.get(key)
    return doc


def main():
    files = sorted(COMPOSITIONS.glob("*.yaml"))
    if not files:
        print(f"BAD\tno tenant compositions found under {COMPOSITIONS}")
        return
    examined = 0
    for path in files:
        for doc in yaml.safe_load_all(path.read_text()):
            if not doc or doc.get("kind") != "Composition":
                continue
            # BOTH FORMS. This read spec.resources only, and these compositions are
            # written in the pipeline form -- spec.pipeline[].input.resources, with
            # function-patch-and-transform -- so the loop ran zero times, the script
            # printed nothing at all, and the wrapper reported "the tenant selector
            # check produced no output" rather than any finding. It had stopped
            # checking, silently, whenever the migration happened.
            resources = list((doc.get("spec") or {}).get("resources") or [])
            for step in (doc.get("spec") or {}).get("pipeline") or []:
                resources.extend(((step.get("input") or {}).get("resources")) or [])
            if not resources:
                print(f"BAD\t{path.name}: a Composition declaring no resources in "
                      f"spec.resources or spec.pipeline[].input.resources — this check "
                      f"cannot see what it composes")
                continue
            for res in resources:
                examined += 1
                manifest = dig(res, "base.spec.forProvider.manifest") or {}
                kind = manifest.get("kind")
                name = f"{path.name}: {res.get('name')} ({kind})"
                targets = {p.get("toFieldPath") for p in res.get("patches") or []}
                if kind in ADMITTED_KINDS:
                    labels = dig(manifest, "metadata.labels") or {}
                    unset = [
                        k for k in COST_LABELS
                        if not labels.get(k) and f"spec.forProvider.manifest.metadata.labels.{k}" not in targets
                    ]
                    if unset:
                        print(f"BAD\t{name}: metadata.labels lacks {', '.join(unset)} (Kyverno require-cost-labels refuses it)")
                    else:
                        print(f"OK\t{name}: carries tenant-id, app-id and cost-center")
                paths = SELECTORS.get(kind)
                if not paths:
                    continue
                patched = {
                    p.get("toFieldPath")
                    for p in res.get("patches") or []
                    if p.get("fromFieldPath") == "spec.appId"
                }
                missing = []
                for sel in paths:
                    labels = dig(manifest, sel)
                    target = f"spec.forProvider.manifest.{sel}.app-id"
                    if not isinstance(labels, dict) or "app-id" not in labels or target not in patched:
                        missing.append(sel)
                if missing:
                    print(f"BAD\t{name}: {', '.join(missing)} must carry app-id patched from spec.appId")
                else:
                    print(f"OK\t{name}: selects by app-id")

    # Printing nothing is how this failed for as long as it did. Say so here, where
    # the reason is known, instead of leaving the wrapper to infer it from silence.
    if examined == 0:
        print("BAD\tno composed resources were examined, so no selector was checked")


if __name__ == "__main__":
    main()
