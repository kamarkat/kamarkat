#!/usr/bin/env bash
# ADR-092: a deployable steady-state provider manifest must not declare
# spec.bootstrap.recovery.
#
# CloudNativePG reads spec.bootstrap ONCE, at creation. Restoring a backup IS the
# creation of a new cluster, so a restore is a manifest transition plus a
# delete/recreate -- and between entering recovery and settling back, the
# repository holds a manifest that DESTROYS DATA if it is applied: a rebuild in
# that window restores the old incarnation and silently discards everything
# written since.
#
# On 2026-09-25 shared-cnpg was restored and never settled. The window stayed open
# for THREE DAYS and closed only because someone happened to look. A rebuild in it
# would have lost the ADR-090 role migration, a second tenant's entire database,
# and every workflow row the first tenant had written.
#
# The bump script warned about this already -- "settle is not optional
# bookkeeping" -- and opens with "A comment is not a mechanism", written about the
# serverName suffix, which got one. Settle did not. This is the mechanism.
#
# IT DOES NOT SETTLE THE MANIFEST. Whether a restore actually succeeded is a
# judgement about the data, not about the YAML, and auto-settling would make the
# dangerous state vanish without anyone deciding it was safe.
#
# IT PARSES, IT DOES NOT GREP. A search for "recovery:" is defeated by ordering,
# comments and unrelated keys of the same name, and it fires on documentation.
# Runbooks and ADRs must stay free to show a recovery stanza -- that is how the
# procedure is documented -- so only manifests a spoke actually renders are in
# scope.
validate_recovery_is_exceptional() {
    section "Recovery is exceptional: no bootstrap.recovery in steady-state manifests (ADR-092)"

    local out
    out=$(cd "$VALIDATE_ROOT" && python3 - <<'PY'
import glob
import sys

try:
    import yaml
except ImportError:
    print("SKIP\tPyYAML not available")
    sys.exit(0)

# Deployable provider manifests only. Not docs/, not runbooks, not examples:
# showing a recovery stanza is how the restore procedure is written down.
# The hub's clusters are matched by DIRECTORY, not by filename. Each platform
# application owns its own cluster (ADR-112) in <application>/database/, and the
# filename is the cluster's own name -- so a pattern spelling "cluster" matched
# none of them, and the hub went unscanned by the gate that exists to stop a
# data-destroying manifest sitting in the repository.
#
# database-recovery/ is deliberately NOT matched: an overlay declaring
# spec.bootstrap.recovery is what a restore IS, and it is not deployable
# steady-state. The two live in sibling directories so this distinction is a path
# and not a judgement.
PATTERNS = [
    "manifests/spoke/spoke-catalog/providers/*/cnpg-cluster.yaml",
    "manifests/providers/*/cnpg-cluster.yaml",
    "manifests/hub-core-services/*/database/*.yaml",
    "manifests/hub-core-services/*/*/database/*.yaml",
]

seen = set()
for pattern in PATTERNS:
    for path in sorted(glob.glob(pattern)):
        if path in seen:
            continue
        seen.add(path)
        try:
            docs = list(yaml.safe_load_all(open(path)))
        except Exception as exc:                      # noqa: BLE001
            print(f"UNPARSED\t{path}\t\t{exc.__class__.__name__}")
            continue
        for doc in docs:
            if not isinstance(doc, dict):
                continue
            # kind AND apiGroup: "Cluster" is also a CAPI kind, and a CAPI
            # Cluster has no spec.bootstrap.recovery to confuse us -- but being
            # explicit keeps the rule readable and survives a future kind clash.
            if doc.get("kind") != "Cluster":
                continue
            if "postgresql.cnpg.io" not in str(doc.get("apiVersion", "")):
                continue
            name = (doc.get("metadata") or {}).get("name", "<unnamed>")
            bootstrap = ((doc.get("spec") or {}).get("bootstrap") or {})
            if not isinstance(bootstrap, dict):
                continue
            if "recovery" in bootstrap:
                src = (bootstrap.get("recovery") or {}).get("source", "<none>")
                print(f"RECOVERY\t{path}\t{name}\t{src}")
            elif "initdb" in bootstrap:
                print(f"OK\t{path}\t{name}\tinitdb")
            else:
                print(f"UNKNOWN\t{path}\t{name}\t{','.join(bootstrap) or '<empty>'}")
PY
    )

    local bad=0 checked=0
    while IFS=$'\t' read -r tag file name detail; do
        [[ -n "$tag" ]] || continue
        case "$tag" in
            SKIP)
                warn "skipped: $file"
                return 0
                ;;
            OK)
                ((checked++))
                ;;
            RECOVERY)
                ((bad++))
                hard_fail "recovery bootstrap in a steady-state manifest

    file:      $file
    Cluster:   $name
    bootstrap: recovery <- $detail

  A rebuild from this manifest restores '$detail' and SILENTLY DISCARDS everything
  written since that restore. Recovery is permitted only while a restore is being
  performed, never as committed steady state.

  If the restore is complete and verified:
      make cnpg-settle-incarnation PROVIDER=<provider>

  If it is not complete, finish it before committing. See
  docs/runbooks/spoke-database-loss-and-recovery.md and ADR-092."
                ;;
            UNPARSED)
                ((bad++))
                hard_fail "$file could not be parsed as YAML ($detail) — a manifest this check cannot read is a manifest it cannot protect"
                ;;
            UNKNOWN)
                warn "$file: Cluster/$name declares bootstrap '$detail' — neither initdb nor recovery; check it is intended"
                ((checked++))
                ;;
        esac
    done <<< "$out"

    if (( ! bad )); then
        pass "$checked CNPG cluster manifest(s) in steady state (bootstrap.initdb)"
    fi
}
