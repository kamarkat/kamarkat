#!/usr/bin/env bash
# ADR-111 §2: what the management-state backup covers, and what it must not.
#
# Class M's correctness rests on two labels that nothing in the cluster reconciles
# against each other, and both fail silently.
#
# The credentials stream selects bootstrap-data Secrets BY LABEL and takes nothing
# else. A Machine whose bootstrap Secret is unlabelled is therefore absent from every
# backup -- and because `IsBootstrapDataReady` gates adoption, that Machine can never
# be adopted by a replacement hub. Nothing reports this: Velero backs up what it
# matched and reports success, the Machine is healthy, and the gap appears only when
# the box is being rebuilt and it is too late to fix.
#
# The certificate authorities carry the opposite label, keeping them OUT. They are
# Class E, recoverable only from escrow, and exactly one writer may restore them. A
# missing exclusion puts a private trust root inside a Class M archive, where a
# restore would make Velero a second writer of the fleet's trust.
#
# hub-operator maintains both on every reconcile, so a box that has reconciled since
# this shipped is covered. This check exists because the failure of that loop is
# invisible from anything else: the labels are the only evidence, and a backup
# succeeding is not evidence.
#
# LABEL PRECEDENCE, verified in Velero at v1.18.4 rather than assumed. The exclusion
# label is checked per item in pkg/backup/item_backupper.go, AFTER selection has
# matched it ("Excluding item because it has label velero.io/exclude-from-backup=true").
# So exclusion beats inclusion, and the consequences are not symmetrical:
#
#   inclusion only            backed up -- the wanted state for bootstrap data
#   exclusion only            absent    -- the wanted state for a trust root
#   both                      ABSENT    -- exclusion wins
#   neither                   absent, because the stream selects on the label
#
# Which makes the dangerous case the opposite of what it first appears. A trust root
# carrying BOTH labels is still excluded, so it is a contradiction rather than a leak.
# The leak is a trust root carrying the INCLUSION label and no exclusion -- that one is
# selected and backed up. And a bootstrap-data Secret carrying the exclusion label is
# silently dropped from the stream that exists to carry it.
#
# NON-BLOCKING BY DEFAULT. These labels are applied by hub-operator's reconcile, so a
# box that has just been provisioned legitimately has none of them yet: failing a
# bootstrap or a release for that would reject a box that is converging normally. The
# findings are therefore warnings, and the safety contract stays where it is enforced --
# in hub-operator's reconcile and in the single-writer rules of ADR-111 §2.
#
# Set CLASS_M_COVERAGE_STRICT=1 to make them failures, when the coverage contract is
# what is being assessed rather than passed on the way to something else.
#
# WHAT THIS DOES NOT ESTABLISH, because the name of a check is where overclaiming
# starts. Passing says the Kubernetes objects observed right now satisfy the label
# contract: every machine's bootstrap data is selected, no trust root is. It says
# nothing about whether
#
#   * Velero actually archived those objects,
#   * the archive is decryptable and restorable,
#   * the Class E artefacts a reconstruction needs are all escrowed,
#   * or the restored CAPI graph can resume management safely.
#
# Those are four separate questions with four separate answers -- `kakt escrow verify`,
# a real restore, `kakt cluster match-inventory`. This is a coverage check, and a box
# that passes it is not thereby recoverable.
#
# Checked here rather than against Velero: these are facts about coverage, true or
# false before Velero is installed at all. It reports live ELIGIBILITY, never the
# contents of a completed archive -- a backup that succeeded is a different claim.

validate_adr111_class_m_coverage() {
    section "ADR-111 §2: Class M SELECTION covers every machine's bootstrap data and no trust root"

    if ! kc get machines.cluster.x-k8s.io -n platform-capi >/dev/null 2>&1; then
        note "this box has no CAPI machines, so there is no management state to cover"
        return 0
    fi

    local out
    out=$(HUB_KUBECONFIG="$HUB_KUBECONFIG" python3 - <<'PYCHECK'
import json, os, subprocess

KC = ["kubectl", "--kubeconfig", os.environ["HUB_KUBECONFIG"], "-n", "platform-capi"]
EXCLUDE = "velero.io/exclude-from-backup"
INCLUDE = "recovery.platform.io/class-m"
PURPOSES = ("ca", "etcd", "sa", "proxy")


def get(kind):
    r = subprocess.run(KC + ["get", kind, "-o", "json"], capture_output=True, text=True)
    if r.returncode != 0:
        return None
    return json.loads(r.stdout).get("items", [])


machines = get("machines.cluster.x-k8s.io")
secrets = get("secrets")
if machines is None or secrets is None:
    print("FAIL platform-capi cannot be read, so Class M's coverage cannot be established")
    raise SystemExit

by_name = {s["metadata"]["name"]: (s["metadata"].get("labels") or {}) for s in secrets}
clusters = sorted({m["metadata"].get("labels", {}).get("cluster.x-k8s.io/cluster-name", "")
                   for m in machines} - {""})

# 1. Every machine that names bootstrap data must have it selected into the backup.
#    A machine with none is Pending by Cluster API's own definition, not a gap.
pending = covered = 0
for m in machines:
    name = (((m.get("spec") or {}).get("bootstrap") or {}).get("dataSecretName") or "")
    if not name:
        pending += 1
        continue
    labels = by_name.get(name)
    if labels is None:
        print("FAIL %s names bootstrap data %s, which does not exist: that machine cannot "
              "be adopted by any replacement hub" % (m["metadata"]["name"], name))
        continue
    if labels.get(INCLUDE) != "bootstrap-data":
        print("FAIL %s's bootstrap data %s carries no %s=bootstrap-data, so it is in NO "
              "Class M backup and that machine is unrecoverable -- while every backup of "
              "the rest reports success" % (m["metadata"]["name"], name, INCLUDE))
        continue
    covered += 1
if covered:
    print("PASS %d machine(s) have their bootstrap data selected into Class M" % covered)
if pending:
    print("NOTE %d machine(s) name no bootstrap data yet and are Pending, which is not a gap"
          % pending)

# 2. Every trust root must be excluded from the backup.
roots = marked = 0
for cluster in clusters:
    for purpose in PURPOSES:
        sname = "%s-%s" % (cluster, purpose)
        labels = by_name.get(sname)
        if labels is None:
            continue
        roots += 1
        if labels.get(EXCLUDE) != "true":
            print("FAIL %s is a certificate authority carrying no %s=true, so a Class M "
                  "backup can capture a private trust root -- and a restore of it would "
                  "make Velero a second writer of this cluster's trust" % (sname, EXCLUDE))
        else:
            marked += 1
if roots and roots == marked:
    print("PASS %d certificate authorit(ies) are excluded from Class M" % marked)
elif not roots:
    print("NOTE no certificate authority secrets found, so none can be captured")

# 3. Contradictions, read in the direction the precedence actually runs.
for cluster in clusters:
    for purpose in PURPOSES:
        sname = "%s-%s" % (cluster, purpose)
        labels = by_name.get(sname)
        if labels is None:
            continue
        if labels.get(INCLUDE) == "bootstrap-data":
            if labels.get(EXCLUDE) == "true":
                print("FAIL %s is a certificate authority labelled BOTH excluded and "
                      "selected. Exclusion wins, so it is not in the archive -- but the "
                      "contradiction means something is classifying trust roots as "
                      "bootstrap data, and without the exclusion label it would be "
                      "backed up" % sname)
            else:
                print("FAIL %s is a certificate authority carrying %s=bootstrap-data and "
                      "no exclusion, so it IS selected into Class M. That puts a private "
                      "trust root in the archive and gives it a second recovery authority"
                      % (sname, INCLUDE))

for m in machines:
    name = (((m.get("spec") or {}).get("bootstrap") or {}).get("dataSecretName") or "")
    labels = by_name.get(name) if name else None
    if labels and labels.get(EXCLUDE) == "true":
        print("FAIL %s's bootstrap data %s carries %s=true, and exclusion beats the "
              "stream's selector -- so it is absent from every backup and that machine "
              "cannot be adopted, whatever the inclusion label says"
              % (m["metadata"]["name"], name, EXCLUDE))
PYCHECK
) || true

    # `report` is warn unless a readiness assessment was explicitly asked for. NOT
    # soft_fail: that one still fails in final mode, which is where releases are
    # evaluated, and coverage must not gate a release.
    local report=warn
    if [[ "${CLASS_M_COVERAGE_STRICT:-}" == "1" ]]; then
        report=hard_fail
    fi

    if [[ -z "$out" ]]; then
        "$report" "ADR-111's coverage check produced no result, so whether Class M covers this box's machines is unknown -- which is the same position as not having checked"
        return 0
    fi

    local line
    while IFS= read -r line; do
        case "$line" in
            PASS\ *) pass "${line#PASS }" ;;
            NOTE\ *) note "${line#NOTE }" ;;
            FAIL\ *) "$report" "${line#FAIL }" ;;
        esac
    done <<< "$out"

    if [[ "$report" == "warn" ]]; then
        note "reported as warnings: these labels come from hub-operator's reconcile, so a box that has not converged yet is not a failure. Run with CLASS_M_COVERAGE_STRICT=1 to assess the coverage contract itself, where they are failures"
    fi
}
