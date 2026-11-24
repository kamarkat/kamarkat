#!/usr/bin/env python3
"""A promotion converges without being driven (ADR-021 addendum, 2026-11-24).

Four facts, each verified against the pinned upstream when it was written, each
able to drift silently:

  1. The sync operation is BOUNDED. argo-cd's controller.sync.timeout.seconds
     defaults to "0" -- no timeout -- so an unbounded operation is the default
     state, not an accident. Unset, one resource that can never become Ready keeps
     an operation Running for ever, and ArgoCD will not begin a new one while one
     is in flight: a promotion is then seen and cannot be applied.
  2. NO SYNC WAVE FOLLOWS THE WORKLOADS. A wave completes only when its resources
     are Healthy, so a late wave turns every workload's health into a gate on the
     operation finishing. CRD and controller waves are ordering; a wave after the
     bulk is staging, and staging is what wedges.
  3. The stuck-sync alert selects a label that EXISTS. It selected
     operation_phase, which argocd_app_info does not declare, so it matched no
     series and could not fire -- which is why a sync stuck far past its threshold
     paged nobody.
  4. The release path OBSERVES. Forced synchronisation is break-glass, not step 5
     of a release, and the CLI is not a second reconciliation engine.
"""
import re
import sys
from pathlib import Path

ARGOCD = Path("manifests/argocd/components/01/platform-argocd.yaml")
CATALOGUE = Path("manifests/spoke/spoke-catalog")
ALERTS = Path("manifests/hub-core-services/platform-alerts/alerts.yaml")
PROMOTE = Path("cmd/kakt/promote.go")
SKILL = Path(".agents/skills/release/SKILL.md")

# descAppInfo at argo-cd v3.5.7 (controller/metrics/metrics.go). operation_phase is
# NOT among them; `phase` belongs to argocd_app_sync_total.
APP_INFO_LABELS = {
    "namespace", "name", "project", "autosync_enabled", "repo",
    "dest_server", "dest_namespace", "sync_status", "health_status", "operation",
}

out = []
def ok(m): out.append(("OK", m))
def bad(m): out.append(("BAD", m))


def bounded_operation():
    text = ARGOCD.read_text()
    m = re.search(r"controller\.sync\.timeout\.seconds:\s*\"(\d+)\"", text)
    if not m:
        bad("controller.sync.timeout.seconds is not set on the ArgoCD descriptor. Its "
            "default is \"0\" -- no timeout -- so a sync operation that cannot finish "
            "runs for ever and blocks every later revision of that Application")
        return
    seconds = int(m.group(1))
    if seconds == 0:
        bad("controller.sync.timeout.seconds is \"0\", which disables the bound; that is "
            "the default and the state this check exists to prevent")
    elif seconds > 3600:
        bad(f"controller.sync.timeout.seconds is {seconds}s: longer than an hour is not a "
            f"bound anyone waits for, and a promotion blocked that long is an outage")
    else:
        ok(f"the sync operation is bounded at {seconds}s, so an operation that cannot "
           f"finish fails and syncPolicy.retry takes over")


def no_wave_after_the_workloads():
    waves = {}
    for f in CATALOGUE.rglob("*.yaml"):
        for w in re.findall(r"argocd\.argoproj\.io/sync-wave:\s*[\"']?(-?\d+)[\"']?",
                            f.read_text()):
            waves.setdefault(int(w), set()).add(str(f.relative_to(CATALOGUE)))
    if not waves:
        ok("the spoke catalogue declares no sync waves, so no wave can gate on a "
           "workload's health")
        return
    late = {w: files for w, files in waves.items() if w > 0}
    if late:
        detail = "; ".join(f"wave {w}: {', '.join(sorted(files))}" for w, files in sorted(late.items()))
        bad(f"the spoke catalogue declares a sync wave AFTER the default wave ({detail}). "
            f"Every workload at wave 0 must then be Healthy before the operation can "
            f"finish, so one resource that cannot become Ready blocks the promotion. "
            f"Order by dependency (CRDs, then controllers), never as staging")
    else:
        ok(f"no sync wave follows the workloads (declared: "
           f"{', '.join(str(w) for w in sorted(waves))}), so no workload's health gates "
           f"the sync operation")


def alert_selects_a_real_label():
    text = ALERTS.read_text()
    block = re.search(r"- alert: ArgoCDSyncStuck(.*?)(?=\n    - alert:|\Z)", text, re.S)
    if not block:
        bad("ArgoCDSyncStuck is gone: nothing reports an operation that outlives its bound")
        return
    body = block.group(1)
    used = set(re.findall(r"argocd_app_info\{(\w+)=", body))
    unknown = used - APP_INFO_LABELS
    if unknown:
        bad(f"ArgoCDSyncStuck selects {sorted(unknown)} on argocd_app_info, which declares "
            f"no such label at the pinned version. A selector on a label that does not "
            f"exist matches no series, so the alert cannot fire -- it reports healthy by "
            f"being wrong")
    elif not used:
        bad("ArgoCDSyncStuck no longer selects any label on argocd_app_info; this check "
            "cannot tell whether it matches anything")
    else:
        ok(f"ArgoCDSyncStuck selects {sorted(used)}, which argocd_app_info declares, so "
           f"the expression can match")


def the_release_path_observes():
    promote = PROMOTE.read_text()
    forcing = re.findall(r'"scripts/argocd-hard-sync\.sh",\s*"(--?[a-z-]+)"', promote)
    if any(f == "--promote" for f in forcing):
        bad("cmd/kakt/promote.go still runs argocd-hard-sync.sh --promote: that refreshes "
            "and submits a sync per Application, which makes the CLI a second "
            "reconciliation engine and forced synchronisation part of a release")
    elif not forcing:
        bad("cmd/kakt/promote.go invokes no convergence step at all; a promotion that is "
            "never observed reports success it has not established")
    else:
        ok(f"the promotion observes convergence ({', '.join(sorted(set(forcing)))}) rather "
           f"than driving it")

    skill = SKILL.read_text()
    if re.search(r"argocd-hard-sync\.sh --promote", skill):
        bad("the release skill still documents argocd-hard-sync.sh --promote as a release "
            "step; forced synchronisation is administrative recovery, not normal operation")
    else:
        ok("the release skill documents no forced synchronisation in the release path")


bounded_operation()
no_wave_after_the_workloads()
alert_selects_a_real_label()
the_release_path_observes()
for level, message in out:
    print(f"{level}\t{message}")
