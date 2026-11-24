#!/usr/bin/env python3
"""Every hub database role has exactly one password authority.

ADR-023/ADR-112, as amended by ADR-117 §7.3-7.4. Two models exist, and every
CNPG Cluster under hub-core-services must be one of them:

  OpenBao model (zitadel, credits, grafana, openbao):
    1. The initdb owner <app>_owner cannot log in: pg_hba's FIRST rule rejects it
       outright, so the password CNPG generates for it authenticates nothing.
    2. Each login role is a CNPG DatabaseRole with NO passwordSecret: CNPG owns its
       existence and membership, and nothing in Kubernetes holds its password.
    3. Its password authority is then either OpenBao (a static role, declared in
       HubEnvironment.spec.database.roles) or nobody (a certificate-only identity,
       which pg_hba admits by `cert` and nothing else).

  CNPG model (billing, whose role is a superuser and so cannot self-rotate):
    The initdb owner logs in with the Secret CNPG reconciles it from; that Secret
    satisfies CNPG's contract (basic-auth, username = owner, cnpg.io/reload).

And in both: hub-operator issues no password-changing SQL, and readiness asks
OpenBao whether each static role has actually rotated -- existence is not
agreement, which is the false green the 2026 outage ran under.
"""
import re
from pathlib import Path

import yaml

HUB = Path("manifests/hub-core-services")
OPERATOR = Path("operators/hub-operator/internal")
CONTROLLER = OPERATOR / "controller/hubenvironment_controller.go"
HUBENV = Path("manifests/environments/base/hubenvironment.yaml")

out = []


def ok(m): out.append(("OK", m))
def bad(m): out.append(("BAD", m))


def hba_rules(cluster):
    return [r.split() for r in
            (cluster.get("spec", {}).get("postgresql", {}).get("pg_hba") or [])]


def check_openbao_model(f, cluster, owner, logins, declared):
    rules = hba_rules(cluster)
    first = rules[0] if rules else []
    if first[:2] != ["host", "all"] or len(first) < 5 or first[2] != owner or first[-1] != "reject":
        bad(f"{f.name}: pg_hba's first rule must be `host all {owner} all reject`, so the "
            f"owner CNPG generates a password for can never authenticate (got {' '.join(first) or 'none'})")
        return
    for role in logins:
        spec = role["spec"]
        name = spec.get("name")
        if "passwordSecret" in spec:
            bad(f"{f.name}: DatabaseRole {name} names a passwordSecret, a second password "
                f"authority beside OpenBao")
            continue
        methods = {r[-1] for r in rules if len(r) >= 4 and r[2] == name}
        if name in declared:
            ok(f"{f.name}: {name}'s password is OpenBao's alone (static role, ADR-117 §7.3); "
               f"{owner} is rejected")
        elif methods and methods <= {"cert", "reject"}:
            ok(f"{f.name}: {name} is a certificate-only identity; no password exists to own")
        else:
            bad(f"{f.name}: DatabaseRole {name} has no password authority -- not a declared "
                f"HubEnvironment role (so no OpenBao static role), and pg_hba admits it by "
                f"{sorted(methods) or 'nothing'} rather than certificate only")


def check_cnpg_model(f, owner, secret):
    if secret is None:
        bad(f"{f.name}: owner {owner} logs in but no Secret is named for it, so CNPG "
            f"generates and reconciles its own password and anything else that sets it competes")
        return
    es = next((p for p in HUB.rglob("*.yaml")
               if "kind: ExternalSecret" in p.read_text()
               and re.search(rf"\n\s+name:\s*{re.escape(secret)}\s*\n", p.read_text())), None)
    if es is None:
        bad(f"{f.name} names {secret} as its owner Secret and nothing delivers it")
        return
    text = es.read_text()
    problems = []
    if not re.search(r"\n\s+type:\s*kubernetes\.io/basic-auth\s*\n", text):
        problems.append("not type kubernetes.io/basic-auth, which CNPG ignores")
    if 'cnpg.io/reload: "true"' not in text:
        problems.append("no cnpg.io/reload, so a rotation waits for the next resync")
    if not re.search(rf'username:\s*"{re.escape(owner)}"', text):
        problems.append(f"username is not the literal {owner!r}")
    if problems:
        bad(f"{es.name} does not satisfy CNPG's password contract: {'; '.join(problems)}")
    else:
        ok(f"{f.name}: CNPG reconciles {owner} from {secret}, which satisfies its contract")


def main():
    declared = {r["name"] for r in
                (yaml.safe_load(HUBENV.read_text())["spec"]["database"].get("roles") or [])}
    seen = 0
    for f in sorted(HUB.rglob("*-db.yaml")):
        docs = [d for d in yaml.safe_load_all(f.read_text()) if isinstance(d, dict)]
        cluster = next((d for d in docs if d.get("kind") == "Cluster"), None)
        if cluster is None:
            continue
        seen += 1
        initdb = cluster.get("spec", {}).get("bootstrap", {}).get("initdb", {})
        owner = initdb.get("owner")
        if not owner:
            bad(f"{f.name} declares a Cluster with no initdb owner")
            continue
        logins = [d for d in docs if d.get("kind") == "DatabaseRole"
                  and owner in (d["spec"].get("inRoles") or [])]
        if logins:
            check_openbao_model(f, cluster, owner, logins, declared)
        else:
            check_cnpg_model(f, owner, (initdb.get("secret") or {}).get("name"))
    if not seen:
        bad(f"no CNPG Clusters found under {HUB}; this check cannot see what it guards")

    writers = [f"{g.name}: {m.group(1)[:40]}"
               for g in OPERATOR.rglob("*.go") if not g.name.endswith("_test.go")
               for m in re.finditer(r'"(ALTER ROLE[^"]*PASSWORD[^"]*)"', g.read_text(), re.I)]
    if writers:
        bad(f"hub-operator issues password-changing SQL ({'; '.join(writers)}): a second "
            f"authority beside OpenBao and CloudNativePG")
    else:
        ok("hub-operator issues no password-changing SQL")

    if "StaticRoleRotated" not in CONTROLLER.read_text():
        bad("DatabaseRolesProvisioned does not ask OpenBao whether each static role has "
            "rotated, so a role that merely exists reports as provisioned")
    else:
        ok("readiness requires each static role to have rotated, not merely to exist")


main()
for level, message in out:
    print(f"{level}\t{message}")
