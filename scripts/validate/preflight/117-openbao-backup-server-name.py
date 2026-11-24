#!/usr/bin/env python3
"""The pre-upgrade Backup names the openbao-db incarnation it protects (ADR-117 §7).

The Backup's name carries openbao-db's serverName, which a restore bumps. The
serverName is declared in two components -- openbao-db's Cluster (boundary 02) and
the OpenBao component's Backup (boundary 03) -- because the Backup must be ordered
before OpenBao's StatefulSet. If the two disagree, a restore that bumps the
Cluster's serverName leaves the Backup's name unchanged, and a completed Backup from
before the restore would satisfy the next upgrade. One line per finding: OK or BAD.
"""
import sys
import yaml

CLUSTER = "manifests/hub-core-services/openbao/database/openbao-db.yaml"
COMPONENT = "manifests/hub-core-services/openbao/openbao-platform.yaml"
FIELDS = "manifests/hub-core-services/openbao/templated-fields.yaml"


def docs(path):
    with open(path) as f:
        return [d for d in yaml.safe_load_all(f) if d]


def main():
    try:
        cluster = next(d for d in docs(CLUSTER)
                       if d.get("kind") == "Cluster" and d["metadata"]["name"] == "openbao-db")
        backup = next(d for d in docs(COMPONENT) if d.get("kind") == "Backup")
        fields = docs(FIELDS)
    except (OSError, StopIteration, KeyError, yaml.YAMLError) as e:
        print(f"BAD\tcould not read the openbao-db Cluster or the pre-upgrade Backup: {e}")
        return 0

    server = cluster["spec"]["backup"]["barmanObjectStore"]["serverName"]
    name = backup["metadata"]["name"]
    label = backup["metadata"].get("labels", {}).get("kamarkat.io/openbao-db-server-name")

    ok = True
    if not name.endswith("-" + server):
        print(f"BAD\tpre-upgrade Backup {name} does not end in openbao-db's serverName {server}")
        ok = False
    if label != server:
        print(f"BAD\tpre-upgrade Backup label openbao-db-server-name is {label!r}, openbao-db's serverName is {server!r}")
        ok = False

    released = [f["value"] for entry in (fields[0] if fields and isinstance(fields[0], list) else fields)
                if isinstance(entry, dict) and entry.get("kind") == "Backup"
                for f in entry.get("fields", []) if f.get("path") == ["metadata", "name"]]
    if not released:
        print("BAD\tthe released Backup name is not templated (templated-fields.yaml)")
        ok = False
    elif not released[0].rstrip("'\"").endswith("-" + server):
        print(f"BAD\treleased Backup name {released[0]} does not end in openbao-db's serverName {server}")
        ok = False

    if ok:
        print(f"OK\tpre-upgrade Backup is keyed to openbao-db serverName {server}, unreleased and released")
    return 0


if __name__ == "__main__":
    sys.exit(main())
