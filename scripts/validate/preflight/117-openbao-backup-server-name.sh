#!/usr/bin/env bash
# The pre-upgrade Backup names the openbao-db incarnation it protects (ADR-117 §7).
#
# serverName is declared twice -- on openbao-db's Cluster and in the OpenBao
# component's Backup name -- because the Backup must be synced before OpenBao. A
# restore bumps the Cluster's; if the Backup's does not follow, a completed backup
# from before the restore satisfies the next upgrade.
validate_openbao_backup_server_name() {
    section "Pre-upgrade Backup follows openbao-db's serverName (ADR-117 §7)"

    local out
    out=$(cd "$VALIDATE_ROOT" && python3 "$VALIDATE_DIR/preflight/117-openbao-backup-server-name.py") || true

    if [[ -z "$out" ]]; then
        hard_fail "the openbao backup serverName check produced no output"
        return 0
    fi

    local line
    while IFS= read -r line; do
        [[ -z "$line" ]] && continue
        case "$line" in
            OK*)  pass "$(printf '%s' "$line" | cut -f2-)" ;;
            BAD*) hard_fail "$(printf '%s' "$line" | cut -f2-)" ;;
            *)    note "$line" ;;
        esac
    done <<< "$out"
}
