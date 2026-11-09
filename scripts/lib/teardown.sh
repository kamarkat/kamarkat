#!/usr/bin/env bash
# Full-environment teardown (hub + spoke).
#
# Sourced by hub-bootstrap.sh; invoked by `hub-bootstrap.sh <env> --teardown`.
#
# The order is the contract, not a convenience:
#
#   1. The spoke goes first. The hub holds the spoke's CAPI objects and the only
#      controller that can deprovision its load balancers, so a hub destroyed
#      first leaves the spoke running with nothing able to delete it. `kakt
#      teardown` now refuses on exactly that, naming what it found, so step 2
#      below is what makes step 3 possible.
#
#   2. The claim goes before the infrastructure. ArgoCD re-creates the SpokePool
#      while it is being deleted, so the Application and the claim are removed
#      first -- which the Go orchestrator does not do for the hub's own root
#      Application.
#
#   3. Deletion is by ownership label. `caph-cluster-<cluster>=owned`, an exact
#      key, so naming one cluster cannot reach another's resources and a spoke
#      belonging to a different box in the same project is reported and left.
#
# A verification sweep runs afterwards and deletes anything the CLI missed, scoped
# to this environment's names, because "teardown succeeded" with servers still
# billing is the failure mode being fixed.

# ─── Hetzner inventory ───────────────────────────────────────────────────────
# Everything is scoped by exact name prefix (hub name, spoke name), never by a
# loose substring, so a teardown of dev can never reach stg or prod.
_hcloud_api() {
    local path="$1"
    curl -sS -m 30 -H "Authorization: Bearer ${HCLOUD_TOKEN}" \
        "https://api.hetzner.cloud/v1/${path}" 2>/dev/null
}

# Prints: <type>\t<id>\t<name>\t<scope>   (scope = hub | spoke | other)
_hcloud_inventory() {
    local hub="$1" spoke="$2"
    local kind
    for kind in servers load_balancers volumes firewalls networks placement_groups; do
        _hcloud_api "${kind}?per_page=100" | python3 -c "
import json, sys
kind = sys.argv[1]; hub = sys.argv[2]; spoke = sys.argv[3]
try:
    payload = json.load(sys.stdin)
except Exception:
    sys.exit(0)
for item in payload.get(kind, []) or []:
    name = item.get('name') or ''
    labels = item.get('labels') or {}
    blob = name + ' ' + ' '.join('%s=%s' % kv for kv in labels.items())
    if hub and (name.startswith(hub) or ('caph-cluster-' + hub) in blob):
        scope = 'kakt'
    elif spoke and (name.startswith(spoke) or ('caph-cluster-' + spoke) in blob):
        scope = 'spoke'
    elif name.startswith('spoke-pool-') or 'caph-cluster-spoke-pool-' in blob:
        scope = 'other'      # a spoke, but not this environment's
    else:
        continue
    print('\t'.join([kind, str(item.get('id')), name, scope]))
" "$kind" "$hub" "$spoke"
    done
}

_print_inventory() {
    local inv="$1" label="$2"
    if [[ -z "$inv" ]]; then
        log "  ($label: nothing found)"
        return
    fi
    local kind id name scope
    while IFS=$'\t' read -r kind id name scope; do
        [[ -z "$kind" ]] && continue
        log "  [$scope] ${kind%s}: $name (id $id)"
    done <<< "$inv"
}

# ─── Resolve which cluster we are tearing down ───────────────────────────────
# NAMED, never derived (ADR-111 Revision 7).
#
# This resolved the target from the environment: a .state file matching
# *-<env>.json, then a Hetzner server matching ^hub-<provider>-<env>-, then the
# literal guess `hub-<provider>-<env>`. All three encode a naming scheme the
# platform no longer uses -- a cluster is named by the tenant that declares it
# (boxId and clusterName, §1) -- and the hub has no environment to key any of them
# on.
#
# The guess is the part that mattered. This function decides what gets DELETED, and
# its last resort was a plausible-looking name nothing had confirmed: on a box whose
# cluster is named anything else, teardown would either find nothing or find
# somebody else's hub. Refusing is the only safe end to that chain.
_resolve_teardown_targets() {
    if [[ -z "$CLUSTER_NAME" || "$CLUSTER_NAME" == "hub" ]]; then
        error_exit "teardown needs the cluster to tear down, by name: pass --name <cluster>.

It used to work this out from the environment -- a .state file, then a Hetzner
server matching hub-<provider>-<env>, then that name as a literal guess. A cluster
is named by the tenant that declares it now, so there is nothing to derive it from,
and this function decides what gets deleted: a guess here is somebody else's hub."
    fi

    log "  provider: $PROVIDER"

    # The workload cluster, read from the box's own repository.
    #
    # It was read from manifests/spoke/spoke-pools/<env>/<provider> in the
    # platform tree, and fell back to a constructed name
    # ("spoke-pool-<provider>-<env>-01") when that found nothing. Both are gone:
    # the claim is the tenant's, at clusters/<name>/infrastructure/spokepool.yaml,
    # because a name shipped in the bundle was the same on every box.
    #
    # No fallback replaces the guess. A name this script invents is one the
    # platform never created, and a TEARDOWN acting on an invented name is worse
    # than one that stops: it would look for another box's cluster. Unknown is
    # reported, and the caller names it with --spoke.
    if [[ -z "$SPOKEPOOL_NAME" && -d "$ZERO_OPS_DIR/clusters" ]]; then
        local claim
        for claim in "$ZERO_OPS_DIR"/registry/clusters/*/infrastructure/spokepool.yaml; do
            [[ -f "$claim" ]] || continue
            SPOKEPOOL_NAME=$(python3 -c "
import yaml, sys
for d in yaml.safe_load_all(open(sys.argv[1])):
    if d and d.get('kind') == 'SpokePool':
        print(d['metadata']['name']); break
" "$claim" 2>/dev/null)
            [[ -n "$SPOKEPOOL_NAME" ]] && break
        done
    fi
    if [[ -z "$SPOKEPOOL_NAME" ]]; then
        log "  no workload cluster declared in $ZERO_OPS_DIR/registry/clusters/ — pass --spoke to name one"
    else
        log "  workload cluster: $SPOKEPOOL_NAME"
    fi

}

# ArgoCD IS STOPPED BY THE CLI, NOT HERE.
#
# This stripped the Application's finalizers and force-deleted it, then did the
# same to the SpokePool and every CAPI object -- which stops the reconciler and
# also stops the PLATFORM from deleting the cluster, leaving the servers to the
# sweep below. `kakt teardown --destroy-managed` removes the box's root
# Application and then deletes each cluster's Applications with ArgoCD's cascade,
# so the controllers that own the cluster deprovision it, including the load
# balancers only the in-cluster controller can.

# ─── Delete whatever the CLI left behind ─────────────────────────────────────
# Dependency order matters: a network cannot be deleted while servers or load
# balancers still attach to it, and a volume cannot be deleted while attached.
_sweep_leftovers() {
    local hub="$1" spoke="$2"
    local inv
    inv=$(_hcloud_inventory "$hub" "$spoke" | grep -v $'\tother$' || true)
    [[ -z "$inv" ]] && { log "  nothing left in Hetzner for this environment"; return 0; }

    log "  the CLI left these behind — deleting directly:"
    _print_inventory "$inv" "leftovers"

    local kind id name scope
    for pass in servers load_balancers volumes firewalls placement_groups networks; do
        while IFS=$'\t' read -r kind id name scope; do
            [[ "$kind" == "$pass" ]] || continue
            # A volume still attached to a (now deleted) server needs detaching first.
            if [[ "$kind" == "volumes" ]]; then
                curl -sS -m 30 -X POST -H "Authorization: Bearer ${HCLOUD_TOKEN}" \
                    "https://api.hetzner.cloud/v1/volumes/${id}/actions/detach" >/dev/null 2>&1 || true
                sleep 2
            fi
            local code
            code=$(curl -sS -m 60 -o /dev/null -w '%{http_code}' -X DELETE \
                -H "Authorization: Bearer ${HCLOUD_TOKEN}" \
                "https://api.hetzner.cloud/v1/${kind}/${id}")
            if [[ "$code" =~ ^(200|204)$ ]]; then
                log "    ✓ deleted ${kind%s} $name"
            else
                log "    ⚠️  ${kind%s} $name — HTTP $code (may have dependents; re-run to retry)"
            fi
        done <<< "$inv"
    done
}


# ─── Home-lab worker VMs (ADR-046 hybrid provider) ───────────────────────────
# The Hyper-V workers are provisioned out-of-band by provision-flatcar-worker.sh
# and have no CAPI owner, so nothing in the Hetzner or Kubernetes teardown path
# touches them: they keep running, holding their Tailscale identities and RAM on
# the Windows box, and rejoin nothing.
#
# Node registry: scripts/hybrid/home-lab.env (gitignored, real values only)
#   <hostname>|<ssh-target>|<os-info>|<tailnet-host>|<box>|<cluster>|<gb...>|<cpus>
#
# Deletion mirrors provisioning exactly — Stop-VM -Force -TurnOff, Remove-VM
# -Force, then the VM's own disk and Ignition config. The shared base image
# (flatcar-base.vhdx) is deliberately kept: it is a large download shared by every
# node and re-fetching it costs minutes on the next provision.
_teardown_home_workers() {
    local envfile; envfile="$(resolve_owned scripts/hybrid/home-lab.env)"
    if [[ ! -f "$envfile" ]]; then
        log "  no scripts/hybrid/home-lab.env — no home-lab workers registered"
        return 0
    fi

    # Read in a subshell so the registry cannot clobber this script's variables.
    local nodes registry_spoke
    nodes=$(set +u; . "$envfile" >/dev/null 2>&1; printf '%s\n' "${HOME_WORKER_NODES:-}")
    registry_spoke=$(set +u; . "$envfile" >/dev/null 2>&1; printf '%s\n' "${HYBRID_SPOKE_NAME:-}")

    # The registry carries no cluster field, so its spoke name is what ties it to
    # this box. If it names a different spoke, these are not our VMs.
    #
    # Matched against SPOKEPOOL_NAME, the spoke being torn down. It was matched
    # against the environment as an infix -- "-<env>-" -- which is the
    # hub-<provider>-<env> naming scheme (ADR-111 Revision 7), and it both passed
    # any spoke that merely had the word in it and failed every correctly named
    # one that did not.
    if [[ -n "$registry_spoke" && -n "$SPOKEPOOL_NAME" && "$registry_spoke" != "$SPOKEPOOL_NAME" ]]; then
        log "  ⚠️  home-lab.env targets '$registry_spoke', not '$SPOKEPOOL_NAME'"
        log "      — leaving its VMs alone"
        return 0
    fi
    [[ -n "$registry_spoke" ]] && log "  registry '$registry_spoke' is this box's spoke — proceeding"

    local any=0 line host ssh_target rest
    while IFS= read -r line; do
        line="${line%%#*}"
        [[ -z "${line// }" ]] && continue
        IFS='|' read -r host ssh_target rest <<< "$line"
        [[ -z "$host" || -z "$ssh_target" ]] && continue
        any=1
        _delete_one_home_vm "$host" "$ssh_target"
    done <<< "$nodes"

    [[ "$any" == "0" ]] && log "  home-lab.env lists no worker nodes"
    return 0
}


# macOS has no coreutils `timeout`. Use gtimeout when present, otherwise run a
# watchdog that kills the child, so a wedged remote call can never stall a teardown.
_with_timeout() {
    local secs="$1"; shift
    if command -v timeout >/dev/null 2>&1; then
        timeout "$secs" "$@"
    elif command -v gtimeout >/dev/null 2>&1; then
        gtimeout "$secs" "$@"
    else
        "$@" &
        local pid=$!
        ( sleep "$secs"; kill -9 "$pid" 2>/dev/null ) >/dev/null 2>&1 &
        local watcher=$!
        wait "$pid"; local rc=$?
        kill "$watcher" 2>/dev/null || true
        return $rc
    fi
}

_delete_one_home_vm() {
    local vm="$1" ssh_target="$2"

    # -n keeps ssh from draining the caller's `while read` loop; without it only
    # the first VM in the registry is ever processed.
    if ! ssh -n -o BatchMode=yes -o ConnectTimeout=15 -o StrictHostKeyChecking=no \
             "$ssh_target" "echo ok" >/dev/null 2>&1; then
        log "  ⚠️  $vm — cannot reach $ssh_target over SSH; VM left running"
        log "       delete manually:  Stop-VM -Name $vm -Force -TurnOff; Remove-VM -Name $vm -Force"
        return 0
    fi

    log "  deleting VM $vm on $ssh_target"

    # Sent as -EncodedCommand, NOT piped into `powershell -Command -`.
    # `-Command -` reads stdin line-by-line and silently produces NO output for a
    # brace block spanning multiple lines: the VM is never stopped and the exit
    # status is still 0. Base64 UTF-16LE is the only form that reliably carries a
    # multi-line script, and it sidesteps shell/PowerShell quoting entirely.
    local ps
    ps="\$ErrorActionPreference = 'SilentlyContinue'
\$kaktDir = 'C:\\ProgramData\\kakt\\flatcar'
\$target = Get-VM -Name '${vm}' -ErrorAction SilentlyContinue
if (\$target) {
  Stop-VM -Name '${vm}' -Force -TurnOff -ErrorAction SilentlyContinue
  Remove-VM -Name '${vm}' -Force -ErrorAction SilentlyContinue
  Write-Output 'removed VM ${vm}'
} else {
  Write-Output 'VM ${vm} not present'
}
foreach (\$f in @('${vm}.vhdx','${vm}-config.ign','${vm}-ignition.iso','${vm}-ign.vhdx')) {
  \$fp = Join-Path \$kaktDir \$f
  if (Test-Path \$fp) {
    Remove-Item \$fp -Force -ErrorAction SilentlyContinue
    if (Test-Path \$fp) { Write-Output ('KEPT ' + \$f) } else { Write-Output ('removed ' + \$f) }
  }
}
if (Get-VM -Name '${vm}' -ErrorAction SilentlyContinue) {
  Write-Output 'STILL-PRESENT ${vm}'
} else {
  Write-Output 'verified gone: ${vm}'
}"

    local encoded
    encoded=$(printf '%s' "$ps" | iconv -f UTF-8 -t UTF-16LE | base64 | tr -d '\n')

    local out rc=0
    out=$(_with_timeout 240 ssh -n -o BatchMode=yes -o ConnectTimeout=20 \
            -o StrictHostKeyChecking=no "$ssh_target" \
            "powershell -NoProfile -ExecutionPolicy Bypass -EncodedCommand $encoded" 2>&1) || rc=$?

    local shown
    shown=$(printf '%s\n' "$out" | sed 's/\r$//' \
            | grep -E "removed |not present|verified gone|STILL-PRESENT|KEPT " || true)

    if [[ -z "$shown" ]]; then
        log "  ⚠️  $vm — no confirmation from the host (rc=$rc); verify manually with Get-VM"
        return 0
    fi
    printf '%s\n' "$shown" | sed 's/^/    /'

    if grep -q "STILL-PRESENT" <<< "$shown"; then
        log "  ⚠️  $vm still exists after Remove-VM — check Hyper-V on $ssh_target"
    fi
    return 0
}

# ─── Local artifacts ─────────────────────────────────────────────────────────
_local_cleanup() {
    if command -v kind >/dev/null 2>&1; then
        local kc
        for kc in $(kind get clusters 2>/dev/null); do
            log "  deleting kind cluster: $kc"
            kind delete cluster --name "$kc" 2>&1 | tail -1 | sed 's/^/    /' || true
        done
    fi
    if docker network ls --format '{{.Name}}' 2>/dev/null | grep -qx 'kind'; then
        log "  removing stale 'kind' docker network"
        docker network rm kind >/dev/null 2>&1 || log "    (still in use)"
    fi

    local state="$ZERO_OPS_DIR/.state/${CLUSTER_NAME}.json"
    if [[ -f "$state" ]]; then
        rm -f "$state"
        log "  removed bootstrap state: ${CLUSTER_NAME}.json"
    fi

    # Kubeconfigs are archived rather than deleted: they are the only record of
    # what was running if the teardown has to be investigated afterwards.
    local archive="$ZERO_OPS_DIR/k8-secrets/kubeconfig/archived"
    mkdir -p "$archive"
    local f stamp
    stamp="$(date +%Y%m%d-%H%M%S)"
    for f in "$CLUSTER_NAME" "$SPOKEPOOL_NAME"; do
        local kc="$ZERO_OPS_DIR/k8-secrets/kubeconfig/${f}.kubeconfig"
        if [[ -f "$kc" ]]; then
            mv "$kc" "${archive}/${f}.kubeconfig.${stamp}"
            log "  archived ${f}.kubeconfig"
        fi
    done
    return 0
}

# ─── Entry point ─────────────────────────────────────────────────────────────
run_full_teardown() {
    # The teardown is driven by the hub CLI; without it only the sweep would run,
    # which would leave the Kubernetes-side cleanup (finalizers, CAPI objects) undone.
    if [[ ! -x "$KAKT_BINARY" ]]; then
        log "bin/kakt not found — building it (teardown drives the CLI, not just the API)"
        (cd "$ZERO_OPS_DIR" && go build -o bin/kakt ./cmd/kakt) \
            || error_exit "could not build bin/kakt; teardown needs it"
    fi

    export HCLOUD_TOKEN="${HCLOUD_TOKEN:-$(cat "$ZERO_OPS_DIR/k8-secrets/hetzner/token" 2>/dev/null | tr -d '\n')}"
    [[ -z "$HCLOUD_TOKEN" ]] && error_exit "HCLOUD_TOKEN not set and k8-secrets/hetzner/token is unreadable — teardown cannot delete cloud resources"

    # After the token, because resolving the hub name may have to ask Hetzner.
    _resolve_teardown_targets


    log "═══════════════════════════════════════════════════════════"
    log " FULL TEARDOWN"
    log "   hub:   $CLUSTER_NAME"
    log "   spoke: $SPOKEPOOL_NAME"
    log "═══════════════════════════════════════════════════════════"

    log ""
    log "Taking a Hetzner inventory before deleting anything..."
    local inv
    inv=$(_hcloud_inventory "$CLUSTER_NAME" "$SPOKEPOOL_NAME" || true)
    _print_inventory "$inv" "inventory"

    # Resources belonging to a workload cluster that is not this box's. Reported,
    # and nothing more: deletion is by this cluster's CAPH ownership label, so a
    # workload cluster belonging to another box is never reached.
    #
    local foreign
    foreign=$(grep $'\tother$' <<< "$inv" | awk -F'\t' '{print $3}' | sort -u || true)
    if [[ -n "$foreign" ]]; then
        log ""
        log "ℹ️  Resources of other workload clusters are present in this Hetzner project:"
        while IFS= read -r n; do log "     $n"; done <<< "$foreign"
        log "   They are not touched: deletion is by this cluster's ownership label."
    fi

    if [[ "${TEARDOWN_YES:-0}" != "1" ]]; then
        log ""
        log "This permanently deletes the resources listed above, plus local state."
        # The CLUSTER's name, not an environment's. Typing "dev" confirmed the
        # deletion of whichever box the resolution above happened to pick; typing
        # the cluster name confirms the one actually being deleted.
        read -r -p "Type the cluster name ('$CLUSTER_NAME') to confirm: " reply
        [[ "$reply" == "$CLUSTER_NAME" ]] || error_exit "Teardown aborted (got '$reply')"
    fi

    log ""
    log "── 1/4 destroying the workload cluster, then the hub ──"
    #
    # ONE ORDERED RUN, and the order is the CLI's to enforce: --destroy-managed
    # deletes the workload cluster's Applications on the hub, which is what
    # destroys the cluster, waits for each, re-reads the provider, and only then
    # takes the hub. `kakt teardown` refuses outright if a cluster it manages is
    # still there, so there is no sequence of these commands that strands one.
    #
    # Deleting a cluster's servers at the provider is not a substitute while the
    # hub is alive: the claim is still reconciled, so the platform rebuilds the
    # cluster on new servers -- under a name whose secret path and backup prefix
    # are never reused.
    # --gitops-dir, because that is where the box's declaration is: it names the
    # escrow holding the kubeconfig and the provider credential, which are read
    # with the operator's own identity rather than from this machine.
    "$KAKT_BINARY" teardown --name="$CLUSTER_NAME" --confirm --force --debug \
        --gitops-dir "$ZERO_OPS_DIR" \
        ${SPOKEPOOL_NAME:+--destroy-managed="$SPOKEPOOL_NAME"} 2>&1 | sed 's/^/  /' \
        || log "  ⚠️  teardown returned non-zero — the sweep below will catch leftovers"

    log ""
    log "── 2/4 home-lab worker VMs (Hyper-V) ──"
    _teardown_home_workers

    log ""
    log "── 3/4 verification sweep ──"
    _sweep_leftovers "$CLUSTER_NAME" "$SPOKEPOOL_NAME"

    log ""
    log "── 4/4 local artifacts ──"
    _local_cleanup

    log ""
    log "── final verification ──"
    local remaining
    remaining=$(_hcloud_inventory "$CLUSTER_NAME" "$SPOKEPOOL_NAME" | grep -v $'\tother$' || true)
    if [[ -z "$remaining" ]]; then
        log "✅ No Hetzner resources remain for '$CLUSTER_NAME'."
    else
        log "❌ Resources still present after teardown:"
        _print_inventory "$remaining" "remaining"
        log ""
        log "Re-run the teardown — deletions that fail on dependencies usually succeed"
        log "on a second pass once their dependents are gone."
        return 1
    fi

    log ""
    log "Out-of-band cleanup this script deliberately does not touch:"
    log "  • Tailscale devices (flatcar-hub-node-1 on box-b, flatcar-spoke-node-{1,2} on box-a) — remove in the admin console"
    log "  • AWS Secrets Manager: delete the old ENCRYPTION_KEY / AUTH_SECRET entries."
    log "    Restoring a stale key against a fresh Infisical database is the failure to avoid."
    log "  • The shared Flatcar base image (C:\\ProgramData\\kakt\\flatcar\\flatcar-base.vhdx)"
    log "    is kept on purpose — it is reused by the next provision."
    return 0
}
