#!/usr/bin/env bash
# scope: path
# ADR-046 — Hybrid Provider / Home Worker: every LIVE-CLUSTER check, in one file.
#
# One file per ADR; the number is the ADR's. These were cluster modules 60, 61
# and 70, numbered by running order, so the set of checks belonging to this ADR
# could only be found by grep.
#
# Merging changes no behaviour: run.sh sources every module into one shell and
# runs each `validate_*` function it finds, so three sourced files and one
# concatenated file are the same program.
#
# Static counterparts are in preflight/046-hybrid-provider.sh.


# ──────────────────────────────────────────────────────────────────────────
# kubelet reachability
# ──────────────────────────────────────────────────────────────────────────
# The API server must be able to reach every node's kubelet on :10250.
#
# On a hybrid cell each node's InternalIP is its TAILNET address (ADR-046
# invariant 6), so this one reachability property sits underneath three things
# that otherwise fail with unrelated-looking symptoms:
#
#   - kubectl logs/exec/port-forward against the node (i/o timeout);
#   - Cilium's VXLAN tunnel, whose endpoint is derived from that same
#     InternalIP — break it and cross-node pod traffic silently stops;
#   - consequently CoreDNS, when a pod and the DNS replicas sit on different
#     nodes. That surfaces as EAI_AGAIN inside application containers.
#
# This has happened: a tailscale DaemonSet ran alongside the tailscaled that the
# ClusterClass installs natively on the control plane. Both share the host netns
# and therefore the same tailscale0, so the DaemonSet's daemon stripped the
# tailnet addresses off the interface. The node kept ADVERTISING its tailnet
# InternalIP while that address was no longer configured anywhere, so every
# connection to it timed out. Nothing in the failing components named tailscale:
# what was visible was Infisical CrashLoopBackOff on "Boot up migration failed",
# with a healthy database and a healthy pooler.
#
# Checking node Ready is not a substitute — kubelet reports Ready over its own
# OUTBOUND connection to the API server, which keeps working the whole time.
validate_kubelet_reachability() {
    section "API server → kubelet reachability (ADR-046 invariant 6)"

    local nodes node ip unreachable=0
    nodes=$(kc get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')

    if [[ -z "$nodes" ]]; then
        hard_fail "no nodes returned — cannot validate kubelet reachability"
        return 0
    fi

    while IFS= read -r node; do
        [[ -z "$node" ]] && continue
        ip=$(kc get node "$node" -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')

        # proxy/healthz goes API server -> kubelet, the exact path that breaks.
        if kc get --raw "/api/v1/nodes/${node}/proxy/healthz" >/dev/null; then
            pass "kubelet reachable on $node (InternalIP ${ip:-unknown})"
        else
            unreachable=1
            soft_fail "API server cannot reach kubelet on $node (InternalIP ${ip:-unknown})"
        fi
    done <<< "$nodes"

    if (( unreachable )); then
        note "the node still reports Ready — that is its outbound path and proves nothing here"
        note "on hybrid, confirm the InternalIP is actually configured: ip -4 addr show tailscale0"
        note "check for a second tailscaled (a DaemonSet) competing with the ClusterClass one"
    fi
}

# ──────────────────────────────────────────────────────────────────────────
# Gateway API startup contract
# ──────────────────────────────────────────────────────────────────────────
# Gateway API startup dependency contract (ADR-046 §24 class).
#
# cilium-operator checks for the Gateway API CRDs EXACTLY ONCE at start and
# disables Gateway API for the process lifetime if they are absent. Observed on a
# cold boot: operator up at 11:08:01, CRDs landed 11:20:53 via ArgoCD, and
# GatewayClass/cilium stayed Accepted=Unknown for hours — no listener served
# traffic, HTTPRoutes could not attach, AgentGateway never became ready. Nothing
# self-heals, because the operator never re-checks.
#
# The fix is a dependency gate on the operator (initContainer), not a delivery
# ordering trick. These checks assert the CONTRACT, not the mechanism, so they stay
# valid if delivery is recomposed later.
#
# This is a COLD-BOOT check. Run against a freshly bootstrapped cluster: on an
# already-running cluster criterion 2 passes trivially, because the CRDs have long
# since been established and the ordering it exists to prove is unobservable.

validate_gateway_api_startup_contract() {
    section "Gateway API startup dependency contract"

    if ! ensure_spoke_kubeconfig; then
        soft_fail "spoke unreachable — Gateway API startup contract not verified"
        return 0
    fi

    local required=(
        gatewayclasses gateways httproutes grpcroutes referencegrants
    )

    # 1. All five required CRDs Established=True.
    local missing=0
    for crd in "${required[@]}"; do
        local est
        est=$(kc_spoke get crd "${crd}.gateway.networking.k8s.io" \
                -o jsonpath='{.status.conditions[?(@.type=="Established")].status}' 2>/dev/null)
        if [[ "$est" != "True" ]]; then
            hard_fail "Gateway API CRD ${crd}.gateway.networking.k8s.io is not Established (got '${est:-absent}') — cilium-operator disables Gateway API when these are missing at start"
            missing=1
        fi
    done
    [[ $missing -eq 0 ]] && pass "all five required Gateway API CRDs are Established"

    # 2. The gate exists on the operator. Asserted on the workload rather than by
    #    inferring from timestamps: a cluster that has been up for hours cannot
    #    demonstrate the ordering, but it can demonstrate that the dependency is
    #    still encoded and would hold on the next cold boot.
    local gate
    gate=$(kc_spoke get deployment cilium-operator -n kube-system \
              -o jsonpath='{.spec.template.spec.initContainers[?(@.name=="wait-for-gateway-api-crds")].name}' 2>/dev/null)
    if [[ "$gate" == "wait-for-gateway-api-crds" ]]; then
        pass "cilium-operator carries the Gateway API dependency gate"
    else
        hard_fail "cilium-operator has no wait-for-gateway-api-crds initContainer — the startup race is unguarded and will recur on the next cold boot"
    fi

    # 2b. Ordering, when it is still observable. The operator container's start time
    #     must be at or after CRD establishment. On a long-running cluster the CRD
    #     timestamps predate everything and this is vacuous, so it is reported as a
    #     note rather than a pass to avoid manufacturing false confidence.
    local crd_est op_start
    crd_est=$(kc_spoke get crd gatewayclasses.gateway.networking.k8s.io \
                -o jsonpath='{.metadata.creationTimestamp}' 2>/dev/null)
    op_start=$(kc_spoke get pods -n kube-system -l io.cilium/app=operator \
                -o jsonpath='{.items[0].status.startTime}' 2>/dev/null)
    if [[ -n "$crd_est" && -n "$op_start" ]]; then
        if [[ "$op_start" < "$crd_est" ]]; then
            hard_fail "cilium-operator started ($op_start) BEFORE the Gateway API CRDs existed ($crd_est) — this is the exact race the gate exists to prevent"
        else
            note "operator start $op_start is at/after CRD creation $crd_est (only meaningful on a cold boot)"
        fi
    fi

    # 3 + 4. GatewayClass exists, is Accepted, and is owned by Cilium's controller.
    #        Accepted=Unknown is the signature of the disabled-at-startup state and
    #        is treated as failure, not as "still settling".
    local gc_accepted gc_controller
    gc_accepted=$(kc_spoke get gatewayclass cilium \
                     -o jsonpath='{.status.conditions[?(@.type=="Accepted")].status}' 2>/dev/null)
    gc_controller=$(kc_spoke get gatewayclass cilium \
                     -o jsonpath='{.spec.controllerName}' 2>/dev/null)
    if [[ -z "$gc_controller" ]]; then
        hard_fail "GatewayClass/cilium does not exist — no Gateway API implementation is registered"
    elif [[ "$gc_controller" != "io.cilium/gateway-controller" ]]; then
        hard_fail "GatewayClass/cilium controllerName is '$gc_controller', expected io.cilium/gateway-controller"
    elif [[ "$gc_accepted" != "True" ]]; then
        hard_fail "GatewayClass/cilium Accepted=${gc_accepted:-Unknown} — the controller is not reconciling it, which is what a missed CRD check looks like"
    else
        pass "GatewayClass/cilium Accepted=True, controller io.cilium/gateway-controller"
    fi

    # 5. A representative route actually attaches. The preceding checks can all pass
    #    while nothing routes, so this closes the loop on the user-visible outcome.
    local routes
    routes=$(kc_spoke get httproute -A -o json 2>/dev/null)
    if [[ -z "$routes" ]] || [[ "$(echo "$routes" | jq -r '.items | length' 2>/dev/null)" == "0" ]]; then
        note "no HTTPRoute present to verify attachment; skipping the route half of the contract"
        return
    fi
    local unaccepted
    unaccepted=$(echo "$routes" | jq -r '
        .items[]
        | select(([.status.parents[]?.conditions[]? | select(.type=="Accepted") | .status] | index("True")) | not)
        | "\(.metadata.namespace)/\(.metadata.name)"' 2>/dev/null)
    if [[ -n "$unaccepted" ]]; then
        hard_fail "HTTPRoute(s) not Accepted by any parent: $(echo "$unaccepted" | tr '\n' ' ')— the Gateway exists but routes nothing"
    else
        pass "every HTTPRoute is Accepted by a parent Gateway"
    fi
}

# ──────────────────────────────────────────────────────────────────────────
# §24 — spoke cold-boot causal chain
# ──────────────────────────────────────────────────────────────────────────
# The spoke cold-boot causal chain (ADR-046 §24).
#
#   controlPlaneEndpoint populated
#        -> {spoke}-cilium-config CRS wrapper exists
#        -> wrapper carries THAT endpoint
#        -> kube-system/cilium-config on the spoke carries it too
#        -> Cilium agent Ready
#        -> control-plane Node Ready
#        -> home worker Node Ready with the placement contract
#        -> argocd-agent Ready
#        -> spoke catalog reconciled (CRDs present)
#        -> CNPG installed
#        -> CNPG has an eligible placement target
#
# Every link is asserted against live state. None is inferred from the one before,
# because the failure this encodes broke a link in the middle while both ends looked
# fine: the SpokePool XR reported Ready=True/Available and the CAPI Cluster reported
# Provisioned for five hours while the spoke had no CNI, no worker, no CRDs and no
# workloads. Anything that reports health without observing it is how that happened.
#
# A link that cannot be checked FAILS. It does not pass, and it does not skip.
validate_spoke_cilium_chain() {
    section "Spoke cold-boot chain (ADR-046 §24)"

    if [[ -z "$SPOKEPOOL_NAME" ]]; then
        soft_fail "SPOKEPOOL_NAME unset — the spoke cold-boot chain was not verified"
        return 0
    fi

    # ── Link 1: controlPlaneEndpoint — the sole source for the endpoint ──────
    local host port
    host=$(kc get clusters.cluster.x-k8s.io "$SPOKEPOOL_NAME" -n platform-capi -o jsonpath='{.spec.controlPlaneEndpoint.host}')
    port=$(kc get clusters.cluster.x-k8s.io "$SPOKEPOOL_NAME" -n platform-capi -o jsonpath='{.spec.controlPlaneEndpoint.port}')
    if [[ -n "$host" && -n "$port" && "$port" != "0" ]]; then
        pass "controlPlaneEndpoint populated: ${host}:${port}"
    else
        hard_fail "controlPlaneEndpoint is empty — CAPH has not created the load balancer, so cilium-config can never render (check HetznerCluster for LoadBalancerCreateFailed)"
        return 0
    fi

    # ── Link 2: the CRS wrapper exists and is the rendered one ───────────────
    local wrapper="${SPOKEPOOL_NAME}-cilium-config" wtype
    wtype=$(kc get secret "$wrapper" -n platform-capi -o jsonpath='{.type}')
    if [[ "$wtype" == "addons.cluster.x-k8s.io/resource-set" ]]; then
        pass "CRS wrapper $wrapper exists"
    else
        hard_fail "CRS wrapper $wrapper missing — hub-operator has not rendered it (old operator image, or it is deferring; check its logs)"
        return 0
    fi

    # ── Link 3: the wrapper carries THAT endpoint, not a stale one ───────────
    local wrapped_host wrapped_port payload
    payload=$(kc get secret "$wrapper" -n platform-capi -o jsonpath='{.data.cilium-config\.yaml}' | base64 -d 2>/dev/null)
    wrapped_host=$(sed -n 's/^ *k8s-service-host: *"\{0,1\}\([^"]*\)"\{0,1\} *$/\1/p' <<<"$payload" | head -1)
    wrapped_port=$(sed -n 's/^ *k8s-service-port: *"\{0,1\}\([^"]*\)"\{0,1\} *$/\1/p' <<<"$payload" | head -1)
    if [[ "$wrapped_host" == "$host" && "$wrapped_port" == "$port" ]]; then
        pass "wrapper carries the live endpoint (${wrapped_host}:${wrapped_port})"
    else
        hard_fail "wrapper endpoint ${wrapped_host:-<empty>}:${wrapped_port:-<empty>} does not match the cluster's ${host}:${port} — the spoke would look for the API server at the wrong address"
    fi

    if ! ensure_spoke_kubeconfig; then
        hard_fail "spoke API unreachable — the rest of the cold-boot chain cannot be verified, and an unverified chain is exactly what ADR-046 §24 forbids claiming"
        return 0
    fi

    # ── Link 4: the delivered ConfigMap carries it too ───────────────────────
    # CRS guarantees creation, not steady state (ADR-048 amendment): a payload
    # deleted after a successful apply stays deleted, and the binding still reads
    # applied: true. So the live object is checked, never the binding.
    local live_host live_port kpr
    live_host=$(kc_spoke get cm cilium-config -n kube-system -o jsonpath='{.data.k8s-service-host}')
    live_port=$(kc_spoke get cm cilium-config -n kube-system -o jsonpath='{.data.k8s-service-port}')
    kpr=$(kc_spoke get cm cilium-config -n kube-system -o jsonpath='{.data.kube-proxy-replacement}')
    if [[ "$live_host" == "$host" && "$live_port" == "$port" ]]; then
        pass "spoke cilium-config carries the endpoint (${live_host}:${live_port})"
    else
        hard_fail "spoke cilium-config endpoint is ${live_host:-<empty>}:${live_port:-<empty>}, expected ${host}:${port} — this is the exact state that deadlocked the cold boot"
    fi

    # kube-proxy is deleted by the ClusterClass; with KPR on and no endpoint there is
    # no route to the API at all. Asserted together because either alone is survivable.
    if [[ "$kpr" == "true" ]] && kc_spoke get ds kube-proxy -n kube-system >/dev/null 2>&1; then
        warn "both kube-proxy and Cilium kube-proxy-replacement are active — addendum 1 removes kube-proxy for a reason"
    elif [[ "$kpr" == "true" && -z "$live_host" ]]; then
        hard_fail "kube-proxy-replacement=true with no k8s-service-host and no kube-proxy — nothing can route the API ClusterIP"
    fi

    # ── Link 5: Cilium actually Ready (not merely scheduled) ─────────────────
    local cil_ready cil_desired
    cil_ready=$(kc_spoke get ds cilium -n kube-system -o jsonpath='{.status.numberReady}')
    cil_desired=$(kc_spoke get ds cilium -n kube-system -o jsonpath='{.status.desiredNumberScheduled}')
    if [[ -n "$cil_ready" && "${cil_ready:-0}" -ge 1 && "$cil_ready" == "$cil_desired" ]]; then
        pass "Cilium agent Ready on all $cil_ready node(s)"
    else
        hard_fail "Cilium agent ${cil_ready:-0}/${cil_desired:-0} Ready — check the 'config' init container; a timeout on 10.96.0.1 means the endpoint never reached the agent"
    fi

    # ── Link 6: control-plane Node Ready ─────────────────────────────────────
    _chain_nodes_ready "control plane" 'node-role.kubernetes.io/control-plane' \
        "control-plane Node not Ready — 'cni plugin not initialized' means link 5 has not taken effect"

    # ── Link 7: home worker Ready AND carrying the placement contract ────────
    # Both labels, per ADR-014 + §11. A Ready node without workload-location=on-prem
    # satisfies nothing that is scheduled against it.
    _chain_nodes_ready "home worker" 'workload-location=on-prem,node-role.kubernetes.io/worker' \
        "no Ready node carries workload-location=on-prem + node-role.kubernetes.io/worker — Step 10e did not converge, and every stateful workload will sit Pending"

    # ── Link 8: argocd-agent Ready — the spoke's only GitOps path ────────────
    local agent
    agent=$(kc_spoke get deployment argocd-agent -n argocd -o jsonpath='{.status.readyReplicas}')
    if [[ "${agent:-0}" -ge 1 ]]; then
        pass "argocd-agent Ready ($agent replica(s))"
    else
        hard_fail "argocd-agent not Ready — nothing delivers the spoke catalog, so every CRD and controller below is unreachable"
    fi

    # ── Link 9: the catalog actually reconciled ──────────────────────────────
    local crd missing=()
    for crd in clusters.postgresql.cnpg.io externalsecrets.external-secrets.io \
               certificates.cert-manager.io clusterpolicies.kyverno.io; do
        kc_spoke get crd "$crd" >/dev/null 2>&1 || missing+=("$crd")
    done
    if (( ${#missing[@]} == 0 )); then
        pass "spoke catalog reconciled (platform CRDs established)"
    else
        hard_fail "spoke catalog has not reconciled — missing CRDs: ${missing[*]}"
    fi

    # ── Link 10 + 11: CNPG installed, and its placement is satisfiable ───────
    local cnpg
    cnpg=$(kc_spoke get deployment cnpg-cloudnative-pg -n cnpg-system -o jsonpath='{.status.readyReplicas}')
    if [[ "${cnpg:-0}" -ge 1 ]]; then
        pass "CNPG operator Ready"
    else
        hard_fail "CNPG operator not Ready — no database can be provisioned on this spoke"
    fi

    # The placement rule is only satisfiable if a schedulable node matches BOTH
    # selectors. A Pending CNPG PVC is the correct failure mode for a missing home
    # worker (REMEMBER.md), so the eligibility is asserted rather than the PVC.
    # Filter in the shell, not in jsonpath. `?(@.spec.unschedulable!=true)` does NOT
    # match objects where the field is absent — and it is absent on every schedulable
    # node — so that filter reports zero eligible nodes while a healthy one is sitting
    # right there. This check produced exactly that false alarm on 2026-08-23.
    local eligible=0 line
    while IFS= read -r line; do
        [[ -z "$line" ]] && continue
        [[ "${line#*=}" == "true" ]] && continue    # cordoned
        eligible=$((eligible + 1))
    done < <(kc_spoke get nodes -l 'workload-location=on-prem,node-role.kubernetes.io/worker' \
        -o jsonpath='{range .items[*]}{.metadata.name}{"="}{.spec.unschedulable}{"\n"}{end}')
    if [[ "${eligible:-0}" -ge 1 ]]; then
        pass "CNPG placement satisfiable ($eligible schedulable home worker(s))"
    else
        hard_fail "no schedulable node satisfies the ADR-014 + §11 placement class — CNPG will stay Pending with a healthy-looking cluster around it"
    fi
}

# Ready is asserted on the Node condition. Never on a CAPI Machine: a home worker
# has no Machine at all, so a Machine count is structurally blind to it — which is
# how the previous Step 10e passed on a cluster with zero workers.
_chain_nodes_ready() {
    local what="$1" selector="$2" failure="$3"
    local line ready=0 total=0
    while IFS= read -r line; do
        [[ -z "$line" ]] && continue
        total=$((total + 1))
        [[ "${line#*=}" == "True" ]] && ready=$((ready + 1))
    done < <(kc_spoke get nodes -l "$selector" \
        -o jsonpath='{range .items[*]}{.metadata.name}{"="}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}')

    if (( ready >= 1 )); then
        pass "$what Node Ready ($ready/$total)"
    else
        hard_fail "$failure (matched $total node(s), $ready Ready)"
    fi
}

# ──────────────────────────────────────────────────────────────────────────
# Cluster DNS is answered on the node that asked
# ──────────────────────────────────────────────────────────────────────────
# The static counterpart asserts the manifests are in the local-redirect shape.
# This asserts the datapath actually took it, which is a different claim and the
# only one that matters: every failure this guards against left the manifests
# correct, the pods Running and the Application Synced/Healthy.
#
# The evidence is the agent's own service table. A kube-dns ClusterIP that has
# not been claimed looks like this --
#
#   3  10.96.0.10:53/UDP  ClusterIP  1 => 10.244.0.26:53/UDP (active)
#                                    2 => 10.244.1.72:53/UDP (active)
#
# -- two cluster-wide CoreDNS backends, one of them on the far side of the
# Tailscale overlay, chosen per connection by the socket load balancer. Roughly
# half of every pod's DNS then crosses a link with ~207ms RTT and multi-second
# stalls, and the queries that stall return EAI_AGAIN to the application. On
# 2026-09-22 that was a Zitadel sign-in reporting "Could not create session for
# user": a gRPC connect that never resolved its target, rendered as an identity
# error.
#
# Claimed, the same row reads `LocalRedirect` with exactly the node's own cache
# behind it. Nothing else distinguishes the two states -- not pod status, not
# the DaemonSet, not the policy object's existence, which is why this check
# reads the table rather than the objects.
#
# Asserted on EVERY agent: the redirect is programmed per node, and a node whose
# agent missed it is a node whose pods are silently back on the overlay.
validate_nodelocal_dns_redirect_programmed() {
    section "cluster DNS answered node-locally (ADR-046)"

    local pods pod table claimed=0 total=0 unclaimed=""
    pods=$(kc -n kube-system get pods -l k8s-app=cilium \
        -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')

    if [[ -z "$pods" ]]; then
        hard_fail "no cilium agent pods found — cannot tell whether cluster DNS is answered locally"
        return
    fi

    while IFS= read -r pod; do
        [[ -z "$pod" ]] && continue
        total=$((total + 1))
        table=$(kc -n kube-system exec "$pod" -c cilium-agent -- \
            cilium-dbg service list 2>/dev/null | grep '10\.96\.0\.10:53')

        if [[ -z "$table" ]]; then
            unclaimed+=$'\n'"  $pod: no 10.96.0.10:53 entry in the service table at all"
            continue
        fi
        if grep -q 'LocalRedirect' <<< "$table"; then
            claimed=$((claimed + 1))
        else
            unclaimed+=$'\n'"  $pod: $(awk '{$1=""; print}' <<< "$table" | head -1 | xargs)"
        fi
    done <<< "$pods"

    if (( total > 0 && claimed == total )); then
        pass "kube-dns ClusterIP is LocalRedirect on all $total node(s) — DNS never leaves the node"
    else
        hard_fail "kube-dns ClusterIP is NOT redirected to the node-local cache on $((total - claimed)) of $total node(s).
Those nodes' pods resolve through cluster-wide CoreDNS endpoints, so roughly half of every
lookup crosses the Tailscale overlay and the ones that stall surface as EAI_AGAIN.
Check enable-local-redirect-policy in cilium-config and the CiliumLocalRedirectPolicy
in kube-system; with the flag off the policy is accepted and inert.${unclaimed}"
    fi
}

# ──────────────────────────────────────────────────────────────────────────
# every PodDisruptionBudget permits an eviction
# ──────────────────────────────────────────────────────────────────────────
# A PDB whose allowedDisruptions is 0 and stays 0 makes its pods UN-EVICTABLE: the
# eviction API refuses with 429, and `kubectl drain` does not fail on that -- it
# retries, indefinitely. The symptom is not an error but a node that never finishes
# draining, which on a control plane is a roll that cannot complete.
#
# It has now cost two rolls. The upstream ccm-hetzner chart ships `minAvailable: 1`
# beside `replicas: 1`, which can never permit an eviction, and it was rendered
# verbatim into the CCM ClusterResourceSet addon. On nutgraf-01 (2026-10-05) a
# control-plane roll replaced the node correctly and the OLD Machine then sat in
# Deleting, draining kube-system/ccm-ccm-hetzner with nothing to time out. The hub hit
# the identical wall hours later, and on the hub the stall left TWO control-plane nodes
# live with different encryption providers -- a Secret written through one unreadable
# through the other.
#
# WHY THIS EXISTS AS WELL AS preflight/046-pdb-permits-eviction. The static check reads
# the manifests and is now clean, but a ClusterResourceSet has no `strategy` and so
# defaults to ApplyOnce: addon content is applied when a cluster is CREATED and never
# again. Every cluster built before the manifest was fixed still carries the old PDB,
# and no amount of fixing the repository changes that. Only a live check sees it -- and
# it sees it BEFORE a roll is attempted rather than from inside a drain that will not
# end.
#
# allowedDisruptions is read from status rather than computed from the spec, because
# the API server already does that arithmetic against the pods that actually exist.
validate_pdb_permits_eviction() {
    section "Every PodDisruptionBudget permits an eviction (ADR-046)"

    local rows
    rows=$(kc get poddisruptionbudgets -A -o jsonpath='{range .items[*]}{.metadata.namespace}{"\t"}{.metadata.name}{"\t"}{.status.disruptionsAllowed}{"\t"}{.status.currentHealthy}{"\t"}{.status.desiredHealthy}{"\t"}{.status.expectedPods}{"\n"}{end}' 2>/dev/null) || {
        hard_fail "could not list PodDisruptionBudgets"
        return 0
    }

    if [[ -z "$rows" ]]; then
        pass "no PodDisruptionBudget on this cluster"
        return 0
    fi

    local ns name allowed healthy desired expected total=0 blocked=0
    while IFS=$'\t' read -r ns name allowed healthy desired expected; do
        [[ -z "$name" ]] && continue
        total=$((total + 1))
        : "${allowed:=0}" "${healthy:=0}" "${desired:=0}" "${expected:=0}"

        [[ "${allowed}" -ne 0 ]] && continue

        # A PDB selecting NO pods, or one the controller has not evaluated yet, has an
        # all-zero status -- and reads as "0 allowed, 0 healthy, 0 expected", which the
        # test below would call un-evictable. It is neither: there is nothing to evict.
        # A freshly created PDB is in this state for a moment, and a false hard failure
        # on an ordinary object is how a check earns a reputation for noise and stops
        # being read.
        if [[ "${expected}" -eq 0 ]]; then
            note "PDB ${ns}/${name} selects no pods or has not been evaluated yet (empty status) -- nothing to evict, nothing to judge"
            continue
        fi

        # 0 allowed with everything healthy is the un-evictable case: the budget is
        # already satisfied and still refuses, so waiting changes nothing. 0 allowed
        # while a pod is genuinely unhealthy is the PDB doing its job, and it clears
        # on its own -- reporting that as a failure would make this check noise during
        # every ordinary rollout.
        if [[ "${healthy}" -ge "${desired}" && "${expected}" -le "${desired}" ]]; then
            blocked=$((blocked + 1))
            hard_fail "PDB ${ns}/${name} allows 0 disruptions with ${healthy}/${expected} healthy and desiredHealthy=${desired} -- no pod can ever be evicted, so draining a node that carries one never finishes and a control-plane roll cannot complete. Use maxUnavailable instead of minAvailable, or raise replicas above desiredHealthy"
        else
            warn "PDB ${ns}/${name} allows 0 disruptions, but only ${healthy}/${expected} pods are healthy (desiredHealthy=${desired}) -- that is the budget working, and it should clear once the pods recover"
        fi
    done <<< "$rows"

    if [[ "${blocked}" -eq 0 ]]; then
        pass "${total} PodDisruptionBudget(s) permit an eviction or are transiently constrained"
    fi
}

# ──────────────────────────────────────────────────────────────────────────
# §24.6 — the HUB's agents are given the API server's address too
# ──────────────────────────────────────────────────────────────────────────
# The rule §24 encodes holds for every cluster running kube-proxy replacement, and
# the hub is one. §24.1 exempted it on the grounds that it "keeps kube-proxy": it
# does not, so its own agents resolved the API server through 10.96.0.1 and, when
# the API server was OOM-killed on 2026-10-28, Cilium synced the empty backend set
# and could not reach the API server to learn the endpoint was back. Every
# controller on the hub failed with `connect: operation not permitted` until an
# operator patched cilium-config by hand. The spoke chain above checks this on the
# spoke; this checks the cluster the chain itself runs on.
validate_hub_cilium_apiserver_endpoint() {
    section "ADR-046 §24.6: the hub's Cilium agents know where the API server is"

    local kpr host port
    kpr=$(kc get cm cilium-config -n kube-system -o jsonpath='{.data.kube-proxy-replacement}' 2>/dev/null)
    host=$(kc get cm cilium-config -n kube-system -o jsonpath='{.data.k8s-service-host}' 2>/dev/null)
    port=$(kc get cm cilium-config -n kube-system -o jsonpath='{.data.k8s-service-port}' 2>/dev/null)

    if [[ "$kpr" != "true" ]]; then
        pass "hub cilium-config does not claim kube-proxy-replacement (${kpr:-<unset>}) — the endpoint keys are not required"
        return 0
    fi
    if [[ -z "$host" || -z "$port" ]]; then
        hard_fail "hub cilium-config has kube-proxy-replacement=true and no k8s-service-host/port (${host:-<empty>}:${port:-<empty>}) — its agents reach the API server only through 10.96.0.1, the state that deadlocked the hub on 2026-10-28 (ADR-046 §24.6)"
        return 0
    fi

    # The address has to be the one this cluster's control plane actually answers on.
    # A populated but wrong value is the same outage with a healthier-looking
    # ConfigMap, so it is compared against the Cluster object, the field hub-operator
    # reads for a spoke and the CLI reads for the hub. The hub's own Cluster is the
    # one in platform-capi that is not the spoke pool: CAPI self-manages after the
    # pivot, so it is here to be read.
    local hub_cluster expected_host expected_port name
    while IFS= read -r name; do
        [[ -z "$name" || "$name" == "$SPOKEPOOL_NAME" ]] && continue
        hub_cluster="$name"
    done < <(kc get clusters.cluster.x-k8s.io -n platform-capi -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null)

    if [[ -z "$hub_cluster" ]]; then
        hard_fail "no hub Cluster object in platform-capi besides ${SPOKEPOOL_NAME:-the spoke pool} — the endpoint the hub's agents use cannot be checked against its source (ADR-046 §24.6)"
        return 0
    fi
    expected_host=$(kc get clusters.cluster.x-k8s.io "$hub_cluster" -n platform-capi -o jsonpath='{.spec.controlPlaneEndpoint.host}' 2>/dev/null)
    expected_port=$(kc get clusters.cluster.x-k8s.io "$hub_cluster" -n platform-capi -o jsonpath='{.spec.controlPlaneEndpoint.port}' 2>/dev/null)

    if [[ "$host" == "$expected_host" && "$port" == "$expected_port" ]]; then
        pass "hub cilium-config carries its own controlPlaneEndpoint (${host}:${port})"
    else
        hard_fail "hub cilium-config points at ${host}:${port} but Cluster/${hub_cluster} publishes ${expected_host:-<empty>}:${expected_port:-<empty>} — the agents are looking for the API server at the wrong address (ADR-046 §24.6)"
    fi
}
