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

# ─── No provider API in this script ──────────────────────────────────────────
#
# This library used to take its own Hetzner inventory and delete its own
# leftovers, with curl and a token read from k8-secrets. That made it a SECOND
# implementation of the two things that have to be exact -- which resources belong
# to this cluster, and in what order they may be deleted -- and it ran on a
# credential from this workstation, which authenticates perfectly against whichever
# provider project issued it and then reports that the box owns nothing.
#
# Both now come from `kakt`, with the box's own escrowed credential:
#
#   kakt teardown --destroy-managed       the ordered deletion, through the platform
#   kakt teardown --infrastructure-only   the ownership-scoped sweep, and the verdict
#
# The sweep deletes strictly by CAPH's exact ownership label
# (caph-cluster-<name>=owned), reports resources it did not claim, and exits
# non-zero while anything it was asked to destroy is still there. That is the
# verification this script used to do by name prefix, now done by the authority
# that owns the rule.

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
    local archive="${KAMARKAT_HOME:-$HOME/.kamarkat}/run/kubeconfig/archived"
    mkdir -p "$archive"
    local f stamp
    stamp="$(date +%Y%m%d-%H%M%S)"
    for f in "$CLUSTER_NAME" "$SPOKEPOOL_NAME"; do
        local kc="${KAMARKAT_HOME:-$HOME/.kamarkat}/run/kubeconfig/${f}.kubeconfig"
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

    # No provider credential is resolved here. `kakt` reads the box's own from its
    # escrow, as the operator; a token on this workstation is not consulted and
    # cannot point the deletion at another provider project.
    #
    # The targets come from the box's declarations in Git, not from the provider.
    _resolve_teardown_targets


    log "═══════════════════════════════════════════════════════════"
    log " FULL TEARDOWN"
    log "   hub:   $CLUSTER_NAME"
    log "   spoke: $SPOKEPOOL_NAME"
    log "═══════════════════════════════════════════════════════════"

    log ""
    log "The inventory, the ownership check and the report of resources belonging to"
    log "other clusters all come from the CLI runs below, which read the provider with"
    log "this box's own escrowed credential."

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
    #
    # The recovery form of the same command: it re-reads the provider and deletes
    # strictly what carries this cluster's exact ownership label, workload
    # clusters first and the management cluster last. Anything it did not claim is
    # reported rather than deleted, and it exits non-zero while anything it was
    # asked to destroy is still there -- which is this script's verdict too.
    local sweep_scope="$CLUSTER_NAME"
    [[ -n "$SPOKEPOOL_NAME" ]] && sweep_scope="$CLUSTER_NAME,$SPOKEPOOL_NAME"
    if ! "$KAKT_BINARY" teardown --name="$CLUSTER_NAME" --confirm --force \
        --gitops-dir "$ZERO_OPS_DIR" \
        --infrastructure-only "$sweep_scope" 2>&1 | sed 's/^/  /'; then
        SWEEP_INCOMPLETE=1
    fi

    log ""
    log "── 4/4 local artifacts ──"
    _local_cleanup

    log ""
    log "── final verification ──"
    #
    # The sweep above IS the verification: it re-read the provider after deleting
    # and failed if anything it was asked to destroy survived. Re-checking here
    # with a second inventory would be a second implementation of the ownership
    # rule, and the two could disagree.
    if [[ "${SWEEP_INCOMPLETE:-0}" == "1" ]]; then
        log "❌ The sweep reported resources still present for '$CLUSTER_NAME'."
        log ""
        log "Re-run the teardown — deletions that fail on dependencies usually succeed"
        log "on a second pass once their dependents are gone."
        return 1
    fi
    log "✅ The sweep reported nothing remaining for '$CLUSTER_NAME'."

    log "Out-of-band cleanup this script deliberately does not touch:"
    log "  • Tailscale devices (flatcar-hub-node-1 on box-b, flatcar-spoke-node-{1,2} on box-a) — remove in the admin console"
    log "  • AWS Secrets Manager: delete the old ENCRYPTION_KEY / AUTH_SECRET entries."
    log "    Restoring a stale key against a fresh Infisical database is the failure to avoid."
    log "  • The shared Flatcar base image (C:\\ProgramData\\kakt\\flatcar\\flatcar-base.vhdx)"
    log "    is kept on purpose — it is reused by the next provision."
    return 0
}
