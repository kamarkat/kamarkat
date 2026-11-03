#!/usr/bin/env bash
# Backups for every CNPG cluster in the fleet — every cluster on the hub AND the
# spokes'.
#
# On 2026-09-02 the hub's and the spoke's had both run their entire lives without a
# single successful backup while reporting "Cluster in healthy state". The hub's
# bootstrap cluster holds Infisical: the PKI root and every platform secret, on
# local-path, one node's disk.
#
# Enumerated on both sides rather than named. A box runs one cluster per platform
# application (ADR-112), so a check that named the cluster it knew about would
# assert one and silently ignore the others — and a new application's cluster would
# be unwatched from the day it shipped, which is the day it matters.
#
# A lastSuccessfulBackup-only check misses two things:
#
#   1. Timing. Backups are scheduled daily, so a cluster validated minutes after
#      creation legitimately has no backup yet. The field is empty whether backups
#      are healthy-but-pending or permanently broken, so it cannot fail a fresh
#      cluster — which is exactly when this must be caught.
#   2. Reach. Every cluster, on every reachable spoke, not just the one the caller
#      happened to name.
#
# ContinuousArchiving is the LEADING indicator. It goes False within seconds of
# the first WAL and stays there. Both real failures were visible in it hours
# before any backup was due, and NEITHER self-healed:
#
#   failed to get envs: cache miss
#       The instance manager caches barman credentials ONCE, at instance
#       reconcile. If the S3 secret is not there at that moment it never retries,
#       so the database runs healthy and archives nothing, forever.
#
#   WAL archive check failed for server <name>: Expected empty archive
#       serverName still points at a previous incarnation's WALs after a rebuild
#       from initdb. barman refuses to interleave two databases under one server
#       name, and CNPG blocks every base backup behind the failing archive.
#
# Because neither converges, these are hard failures in gate mode too.

# Assert one CNPG cluster's backup posture.
#   $1 label   $2 kubectl fn   $3 namespace   $4 cluster name
_assert_cnpg_backup() {
    local label="$1" kcf="$2" ns="$3" name="$4"
    local json archiving reason msg last sc sched backups

    json=$($kcf get cluster.postgresql.cnpg.io "$name" -n "$ns" -o json 2>/dev/null)
    if [[ -z "$json" ]]; then
        warn "$label: no CNPG cluster $ns/$name found — nothing to assert"
        return 0
    fi

    archiving=$(printf '%s' "$json" | jq -r '.status.conditions[]? | select(.type=="ContinuousArchiving") | .status')
    reason=$(printf '%s' "$json" | jq -r '.status.conditions[]? | select(.type=="ContinuousArchiving") | .reason // ""')
    msg=$(printf '%s' "$json" | jq -r '.status.conditions[]? | select(.type=="ContinuousArchiving") | .message // ""')
    last=$(printf '%s' "$json" | jq -r '.status.lastSuccessfulBackup // ""')
    sc=$(printf '%s' "$json" | jq -r '.spec.storage.storageClass // "unknown"')

    # ── 1. Continuous archiving: the leading indicator ───────────────────────
    if [[ "$archiving" == "True" ]]; then
        pass "$label: continuous WAL archiving is working"
    elif [[ -z "$archiving" ]]; then
        soft_fail "$label: no ContinuousArchiving condition yet — barman has not reported; re-check once the first WAL is written"
    else
        hard_fail "$label: WAL archiving is FAILING (${reason:-unknown}) — no base backup can complete while it is, and this does not self-heal"
        [[ -n "$msg" ]] && note "barman: ${msg:0:180}"
        case "$msg" in
            *"cache miss"*)
                note "CAUSE: the instance manager cached backup credentials before the S3 secret existed." ;;
            *"Expected empty archive"*)
                note "CAUSE: serverName points at a previous incarnation's WALs. Bump the suffix — as part of a recreation from initdb, never on a running cluster." ;;
        esac
    fi

    # ── 2. Something must be scheduled, or nothing ever backs up ─────────────
    sched=$($kcf get scheduledbackup -n "$ns" -o json 2>/dev/null \
        | jq -r --arg c "$name" '[.items[]? | select(.spec.cluster.name==$c)] | length')
    if [[ "${sched:-0}" -gt 0 ]]; then
        pass "$label: $sched ScheduledBackup(s) target this cluster"
    else
        hard_fail "$label: NO ScheduledBackup targets $name — archiving may work, but no base backup will ever be taken"
    fi

    # ── 3. A Backup wedged in a failing phase blocks its own schedule ────────
    backups=$($kcf get backup -n "$ns" -o json 2>/dev/null \
        | jq -r --arg c "$name" '[.items[]? | select(.spec.cluster.name==$c) | select(.status.phase|tostring|test("failed|walArchivingFailing";"i")) | .metadata.name + "=" + (.status.phase|tostring)] | join(", ")')
    if [[ -n "$backups" ]]; then
        soft_fail "$label: Backup(s) stuck in a failing phase: $backups — CNPG reports \"already taking a backup\" and the schedule stops producing new ones until they are removed"
    fi

    # ── 4. Recoverability, graded by what the storage class implies ──────────
    if [[ -n "$last" ]]; then
        pass "$label: lastSuccessfulBackup=$last"
    elif [[ "$archiving" == "True" ]]; then
        note "$label: no base backup yet, but archiving is healthy — expected on a cluster younger than its schedule"
    elif [[ "$sc" == "local-path" ]]; then
        soft_fail "$label: has NEVER backed up successfully AND is on local-path — its data exists only on one node's disk"
    else
        soft_fail "$label: has never backed up successfully (storageClass=$sc)"
    fi
}

validate_root_of_trust_backup() {
    section "Backups for every CNPG cluster (hub + spokes)"

    # Every cluster the hub runs, enumerated. The bootstrap cluster is the root of
    # trust, and the rest hold an application's data each; none of them may be
    # backing up to nothing.
    local hub_names
    hub_names=$(kc get cluster.postgresql.cnpg.io -A -o json 2>/dev/null \
        | jq -r '.items[]? | .metadata.namespace + "/" + .metadata.name')
    if [[ -z "$hub_names" ]]; then
        soft_fail "the hub reports NO CNPG clusters — Infisical's database is one of them, so this is either unreachable or broken"
    else
        local hub_entry
        while IFS= read -r hub_entry; do
            [[ -z "$hub_entry" ]] && continue
            _assert_cnpg_backup "${hub_entry##*/} (hub)" kc "${hub_entry%%/*}" "${hub_entry##*/}"
        done <<< "$hub_names"
    fi

    # Every spoke, not just $SPOKEPOOL_NAME. That variable is supplied by the
    # bootstrap caller and is empty on a standalone run, which silently skipped
    # the spoke databases — and a fleet with several spokes would only ever have
    # had one of them looked at. Each spoke's kubeconfig is on the hub, so
    # enumerate them there.
    local secrets kcfile spoke names entry checked=0 hub_server
    hub_server=$(kubectl --kubeconfig="$HUB_KUBECONFIG" config view \
        -o jsonpath='{.clusters[0].cluster.server}' 2>/dev/null)
    secrets=$(kc get secret -n platform-capi -o json 2>/dev/null \
        | jq -r '.items[]? | select(.metadata.name|endswith("-kubeconfig")) | .metadata.name')

    while IFS= read -r sec; do
        [[ -z "$sec" ]] && continue
        spoke="${sec%-kubeconfig}"
        kcfile="$(mktemp)"
        if ! kc get secret "$sec" -n platform-capi -o jsonpath='{.data.value}' \
             | base64 -d > "$kcfile" 2>/dev/null; then
            rm -f "$kcfile"
            soft_fail "$spoke: could not read its kubeconfig — database backups NOT verified"
            continue
        fi
        if ! kubectl --kubeconfig="$kcfile" cluster-info >/dev/null 2>&1; then
            rm -f "$kcfile"
            soft_fail "$spoke: unreachable — database backups NOT verified, and they have failed silently before"
            continue
        fi

        # The hub's own kubeconfig secret lives in this namespace too, and the
        # hub is asserted above. Identify it by the API endpoint the kubeconfig
        # points at rather than by name, so this holds whatever the hub is
        # called and does not report the hub's clusters twice.
        if [[ "$(kubectl --kubeconfig="$kcfile" config view -o jsonpath='{.clusters[0].cluster.server}' 2>/dev/null)" \
              == "$hub_server" ]]; then
            rm -f "$kcfile"
            continue
        fi

        _kc_this() { kubectl --kubeconfig="$kcfile" "$@" 2>/dev/null; }
        names=$(_kc_this get cluster.postgresql.cnpg.io -A -o json 2>/dev/null \
            | jq -r '.items[]? | .metadata.namespace + "/" + .metadata.name')
        if [[ -z "$names" ]]; then
            note "$spoke: no CNPG clusters"
        else
            while IFS= read -r entry; do
                [[ -z "$entry" ]] && continue
                _assert_cnpg_backup "${entry##*/} ($spoke)" _kc_this "${entry%%/*}" "${entry##*/}"
                checked=$((checked+1))
            done <<< "$names"
        fi
        rm -f "$kcfile"
    done <<< "$secrets"

    if [[ "$checked" -eq 0 && -n "$secrets" ]]; then
        note "no spoke databases were reachable to assert"
    fi
}
