# ADR-046: Hybrid Provider Cell (Hetzner Control Plane + Home-Lab Workers)

**Date:** 2026-08-12
**Status:** Accepted (corrected 2026-09-25 — the home class selects
`workload-location: on-prem`; see Placement classes)

*Amended by: ADR-075 (On-Prem Nodes are a Capability, Not a Provider)*

> `hybrid` is withdrawn as a provider in principle and **retained in code** until a
> `hetzner` box has run on-prem nodes end to end; ADR-075 names that exit
> condition. Everything below therefore still describes a working cell.
>
> The arrangement this ADR names is, in the new vocabulary, `provider: hetzner`
> with the `onPrem` capability enabled -- selectable at any point in a cluster's
> life. Two of the datapath settings specified here per-provider are now
> platform-wide: every box runs `mtu: 1200` with `tailscale0` named in `devices`,
> so enabling on-prem nodes never requires a datapath change. The rest of this
> cell's Cilium configuration is NOT unified; it carries hostNetwork Gateway mode
> and decoupled Envoy, which ADR-075 leaves as a separate question.
>
> Every invariant below stands, invariant 6 and §21 included. "Home worker" and
> "home node" throughout this ADR mean an on-prem node.

## Context

The platform historically used CAPD (Cluster API Provider Docker) for local
development — a Windows-hosted Docker engine acting as the control plane with a
macOS node as worker. This model was architecturally flawed (the Mac was the
worker by design, the Windows box ran the ingestion engine) and never worked.
Production runs entirely on Hetzner.

We need a local development and staging environment that:
- Reuses the tested Hetzner CAPI control-plane path as-is.
- Provides real, useful worker capacity from home-lab hardware (two home
  Windows boxes running Flatcar VMs, ~16GB RAM each, scalable to more).
- Costs nothing to keep running (no idle Hetzner worker nodes).
- Reaches home-lab workers over Tailscale while keeping the control plane on
  Hetzner's public network (CP advertised via a CAPH-managed Hetzner Load
  Balancer, exactly like the Hub and the pure-hetzner spokes).

The CAPD/local provider is removed entirely; home-lab is the new local/staging
ground.

## Decision

Introduce a new **hybrid** provider cell in the provider matrix
(ADR-036/037/038). It is a platform/provider cell — a directory of GitOps
manifests and a Hub CLI driver — NOT a new CAPI infrastructure provider.
Hetzner CAPI remains the infrastructure provider.

### Topology

| Layer | Placement |
|-------|-----------|
| Hub (management) cluster | Hetzner control plane; workers are home-lab Flatcar nodes (§19). The Hetzner worker `MachineDeployment` runs at `replicas: 0` under `--provider=hybrid` |
| Spoke control plane | Hetzner (1 replica dev, 3 stg/prod) |
| Spoke burst worker pool | Hetzner CAPI `MachineDeployment` at `replicas: 0` (escape hatch) |
| Spoke default workers | Home-lab Flatcar nodes, unmanaged kubeadm join over Tailscale |
| Spoke API access | Public Hetzner Load Balancer (CAPH-managed, `controlPlaneLoadBalancer.enabled=true`) |
| Tailscale | Home-lab workers, run via native tailscaled (Ignition/DVD boot, §13). **Both** control planes also join the tailnet via native tailscaled bootstrapped by their ClusterClass `preKubeadmCommands` — invariant 6 requires a routable Tailscale IP on every node that carries pod traffic, and that now includes the hub CP (§21) |

### Provider Cell Layout

- `manifests/providers/_shared/` — ClusterClass (`spokepool-v1`), CCM/CSI addon
  templates. Shared by hetzner and hybrid cells (kustomize `../` works within
  the repo).
- `manifests/providers/hybrid/` — hybrid Composition, home-worker integration,
  credentials.
- `manifests/providers/hetzner/` — unchanged hetzner composition + credentials,
  referencing `../_shared`.

### ClusterClass Variables

The shared ClusterClass exposes two variables:
- `controlPlaneLoadBalancerEnabled` (default `true`) — both hetzner and hybrid
  keep the Hetzner LB; the LB IP becomes the spoke API endpoint. (The ADR
  previously named this `controlPlaneLoadBalancer.enabled`, which never matched
  the manifest; the flat name is authoritative.)
- `controlPlaneEndpointHost` (default `""`) — left empty for both providers;
  CAPH auto-fills `controlPlaneEndpoint.host` from the LB's IPv4
  (`ControlPlaneEndpointSet` condition).

Burst workers carry kubelet label `workload-location=hetzner`; home workers are
labeled `workload-location=on-prem` by the join script (`provision-flatcar-worker.sh`).
See the correction under Placement classes: the CLASS is named `home`, the label
VALUE is `on-prem`, and this sentence said `home` for both until 2026-09-25.

### Home-Worker Lifecycle (Unmanaged)

Home workers are convenience nodes, not CAPI Machines:

1. hub-operator `reconcileHomeWorkerJoin` (WS4) gates on
   `spec.provider == hybrid` + `home-worker-enabled: "true"`.
2. It reads the CAPI-generated spoke kubeconfig (`<spoke>-kubeconfig`,
   `platform-capi`), connects to the spoke, and mints a pure-API kubeadm
   bootstrap token Secret (`bootstrap.kubernetes.io/token`) in the spoke's
   `kube-system`.
3. It writes the join payload Secret (`<spoke>-home-worker-join`,
   `platform-capi`) with per-node tokens, the discovery CA hash, and the
   control-plane endpoint.
4. The join payload Secret (`<spoke>-home-worker-join`,
   `platform-capi`) carries per-node tokens, the discovery CA hash, and the
   control-plane endpoint; consumed by
   `scripts/hybrid/provision-flatcar-worker.sh` (ADR-046 §13), which runs an
   idempotent `kubeadm join` from the home node.
5. Tokens rotate before expiry (rotation window = TTL/2); expired tokens are
   pruned.

Home workers never appear in the ClusterClass topology.

### Spoke API Endpoint

- CAPH creates a Hetzner Load Balancer per spoke (`controlPlaneLoadBalancer`
  `port: 6443`, `type: lb11`) and publishes the kube-apiserver on the LB IPv4.
  **AMENDED by §25.1 (2026-08-23): the listen port is 6443, not 443.** The LB
  `ListenPort` is `controlPlaneEndpoint.port`; while that was 443 it consumed the
  one port tenant HTTPS ingress needs, and CAPH silently dropped the conflicting
  `extraServices` entry. 443 is now reserved for ingress. The endpoint is derived
  automatically; no tailnet name is involved.
- The segment is identical to the pure-hetzner provider; hybrid adds nothing
  for the API path. The endpoint is stable across control-plane node rotation
  because the LB IP does not change.
- Home workers reach the spoke API through this same public endpoint (the
  kubeconfig server used by the join flow); they do not depend on the CP being
  on the tailnet.

### Scheduling Contract

- Default workloads: `nodeSelector: workload-location: on-prem`.
- Burst workloads: explicit `nodeSelector: workload-location: hetzner`.

### Cilium CNI — Flatcar-Native Datapath

Home workers run native Linux cgroup v2 + eBPF (Flatcar Hyper-V VMs, ADR-046
§13), so Cilium deploys with its **standard** stock manifest — no cgroup
workarounds are required. See §13 for the migration that eliminated the
legacy cgroup-mount hacks referenced by earlier revisions of this ADR.

The hybrid provider uses a separate `cilium-addon-hybrid` Secret (registered
in `manifests/providers/hybrid/k8s/cilium-addon-hybrid.yaml`, referenced only
from `spokepool-hybrid-composition.yaml`) for the deltas that remain:

1. **routing-mode: tunnel (VXLAN)** — *superseded addendum 14; this item
   previously read "routing-mode: native (no VXLAN tunnel)".* Cross-node pod
   traffic is VXLAN-encapsulated between node tailnet IPs, so pod IPs never
   appear on the wire and Tailscale needs no knowledge of pod CIDRs.
2. **devices `eth+ enp+ tailscale0`** — `direct-routing-device` and
   `auto-direct-node-routes` are **unset/false**; they are native-routing
   settings and must not be re-enabled (addendum 14, and addendum 17 for the
   table-52 shadowing they leave behind).
3. **mtu: 1200** — Tailscale's WireGuard underlay runs at MTU 1280; the stock
   VXLAN overlay MTU caused IP fragmentation on `tailscale0` and BPF datapath
   drops.
4. **`cilium-netns` volumeMount with `mountPropagation: HostToContainer`** —
   *superseded; this item previously specified `None`, a legacy-era workaround.*
   Flatcar mounts `/run` as tmpfs with shared propagation (verified live), so
   `HostToContainer` is accepted and `cilium-dbg` pod-netns exec works.

The shared `cilium-addon-template` used by Hetzner spokes is left unchanged.

**Control-plane durability**: the Hetzner CP host must still establish the
shared cgroup2 mount itself (the stock mount-cgroup init does this, but the
cgroup2 mount is lost on `/run` tmpfs reboots, so cilium-host-prep
re-creates it at boot). Codified in the shared
ClusterClass (`_shared/spokepool-clusterclass-v1.yaml`): a
`cilium-host-prep.service` systemd unit is written via `files[]` and
enabled/started in `preKubeadmCommands`, re-creating the shared cgroup2/BPF
mounts at every boot (`/run` is tmpfs), with a fail-closed `findmnt` gate.
Idempotent and harmless on pure-hetzner spokes (it pre-creates what stock
`mount-cgroup` would establish anyway).

### Provider Registry

`operators/hub-operator/config/manager/provider-registry.yaml` maps exactly:
`hetzner → spokepool-hetzner`, `hybrid → spokepool-hybrid`. `capd` and `aws`
entries were removed (no compositions exist; sole-developer clean cut).

## Ownership

Defined by this ADR: hybrid cell manifests and the home-worker join flow, and
the provider-registry map. ClusterClass and addon templates are owned jointly
with the hetzner cell (shared base, ADR-036).

## Consequences

### Positive

- Zero idle Hetzner worker cost (burst pool at `replicas: 0`).
- Reuses the tested Hetzner CAPI control-plane path verbatim, including the
  CAPH-managed Load Balancer (same as the Hub and pure-hetzner spokes).
- Real hardware capacity (~32GB across two boxes, scalable to 3+ nodes).
- Stable spoke API endpoint (LB IP survives control-plane rotation); home
  workers reach the API over the public LB, Tailscale is used only for the
  worker nodes themselves.

### Negative

- Home workers are unmanaged — no CAPI-driven rollouts, upgrades, or
  remediation for them.
- kubeadm join depends on the spoke API being reachable (public LB) and on the
  hub-operator token being current (24h TTL, rotated by the controller).
- The spoke API is publicly exposed on a Hetzner Load Balancer (TCP 443) rather
  than kept tailnet-only; exposure is scoped by Hetzner firewall rules.

## Addenda (2026-08-17)

Operational corrections applied live on `spoke-pool-hybrid-dev-01` (Aug 2026)
and codified so re-provisioned spokes work out of the box:

1. **kube-proxy addon removal (Cilium full KPR).** kubeadm's stock
   `addon/kube-proxy` conflicts with Cilium full kube-proxy-replacement
   (`kube-proxy-replacement: "true"`): its iptables REJECT chains for
   endpoint-less services (incl. the Cilium Gateway LB VIP) break Gateway API
   TLS and in-cluster service networking. Codified as
   `spokepool-control-plane-v3` = v2 + `postKubeadmCommands` that delete the
   kube-proxy DaemonSet + ConfigMap post-init (this CAPI version has no
   `skipPhases` on `KubeadmControlPlaneTemplate`; `postKubeadmCommands` run
   after kubeadm's addon phase, so deletion is effective)
   (`manifests/providers/_shared/spokepool-clusterclass-v1.yaml`). The
   ClusterClass ref rotation to v3 is a deliberate, separate step: it
   propagates to existing clusters and rolls the control-plane machine.
2. **LB exclude-label removal.** kubeadm sets
   `node.kubernetes.io/exclude-from-external-load-balancers` on control-plane
   nodes by default; the Hetzner CCM honors it and registers ZERO backends for
   Cilium Gateway-API LoadBalancer services, so the external VIP accepts TCP
   but forwards nothing and TLS handshakes fail externally (they work
   in-cluster). Codified in v3 `postKubeadmCommands`:
   `kubectl ... label nodes --all node.kubernetes.io/exclude-from-external-load-balancers- || true`.
3. **clean-cilium-state=false.** Must stay `false` in steady state: with
   `true`, an agent restart wipes all endpoint/BPF state while existing pods
   keep their netns/veth but lose their Cilium endpoint (kubelet does not
   re-run CNI for existing sandboxes) → probes fail → CrashLoopBackOff. The
   flag was a KPR-deadlock workaround while the conflicting kube-proxy addon
   ran; that conflict is gone (kube-proxy removal in v3).
4. **Firewall gap (not codified).** LB→node traffic for Cilium Gateway-API
   service nodePorts (e.g. 30657/31888) is NOT codified anywhere in this repo —
   no hcloud `Firewall` manifests, no `HetznerCluster.spec.firewall` /
   `HCloudMachine.spec.firewall` config (firewalls are only deleted by the
   hub-cli teardown). Hetzner-side rules for spoke load balancers are
   provisioned out-of-band; re-provisioning a spoke requires re-adding the
   LB→node port rules manually.
5. **spoke-api-front removed (stale pre-ADR artifact).** `spoke-api-front.yaml`
   was a Tailscale-enrolled HAProxy TCP front for the spoke control-plane API —
   a design that predates and contradicts this ADR: the Hub and spoke control
   planes do NOT run Tailscale (topology table above), and the spoke API path
   is the CAPH-managed Hetzner LB ("hybrid adds nothing for the API path"). It
   referenced nonexistent ADR sections (§WS2/§WS5) and its Deployment was
   permanently Pending (nodeSelector `workload-location: home` matches no hub
   node). Removed from `manifests/providers/hybrid/kustomization.yaml` and the
   stg claim comments corrected; home workers join via the spoke kubeconfig
   server (LB endpoint) per the join flow (hub-operator WS4), which also
   supports the `control-plane-endpoint-host` claim annotation when a
   tailnet-resolvable endpoint is ever needed.
6. **Hybrid Cilium VXLAN MTU (1200) & Tailscale Overlay Invariant.**
   Tailscale's WireGuard underlay operates at MTU 1280 (`tailscale0`). The stock
   Cilium VXLAN overlay MTU (1230) produces outer encapsulated UDP packets of
   ~1308 bytes, causing IP fragmentation on `tailscale0` and packet drops in the
   Cilium BPF datapath (`First logical datagram fragment not found`).
   **Codified**: `mtu: "1200"` in `manifests/providers/hybrid/cilium-values.yaml`
   and `manifests/providers/hybrid/k8s/cilium-addon-hybrid.yaml`.
   **Correction (2026-08-25).** The MTU decision above stands and is codified
   (`mtu: '1200'` in `manifests/providers/hybrid/k8s/cilium-config-base.yaml`,
   confirmed live). The node-addressing half of this addendum did not survive contact
   with the platform and is withdrawn.

   It claimed a `cilium-node-ip-reconciler` DaemonSet was codified in
   `manifests/providers/hybrid/k8s/cilium-addon-hybrid.yaml`, maintaining the CP
   node's Tailscale IP in `CiliumNode.spec.addresses`. No such DaemonSet exists — not
   in that file, not anywhere in the repository. An ADR reporting a component as
   codified when it was never committed is worse than one that omits it: it sends the
   next reader looking for a control that is not there, which is exactly what happened
   while diagnosing addendum 28.

   Its stated purpose is met without it. §21 has the hub control plane join the
   tailnet, so the node's `InternalIP` IS its Tailscale address — on `hub-hybrid-dev`
   the CP node registers `InternalIP=100.105.102.41` — and Cilium derives the VXLAN
   tunnel endpoint from `InternalIP` directly. The reconciler addressed a shape that
   §21 removed: `InternalIP` on a private Hetzner address with the tailnet address
   held separately. Nothing needs to reconcile what the node already reports.

   The cross-node Envoy failure this addendum reached for is real, but its cause is
   not node addressing. Addendum 28 has it: the to-proxy mark escapes onto the VXLAN
   outer packet and the encapsulated frame is routed to loopback. Node addresses,
   ipcache entries and tunnel maps were each verified correct while that failure was
   active.
7. **Standalone `cilium-envoy` DaemonSet removed (embedded Envoy only).** The
   rendered addon had BOTH `external-envoy-proxy: "false"` (agent runs Envoy
   embedded, serving Gateway-API L7 on 127.0.0.1:10515) AND the standalone
   `cilium-envoy` DaemonSet (`--base-id 0`). Both bind the same
   `/var/run/cilium/envoy/sockets` abstract domain sockets; a stale host
   `cilium-envoy` process (orphaned after pod restarts) left
   `@envoy_domain_socket_parent_0` behind, and every new standalone Envoy
   instance crashed in ~35ms with `errno=98 (EADDRINUSE)`, taking Gateway
   traffic down. **Codified**: `envoy.enabled: false` in
   `manifests/providers/hybrid/cilium-values.yaml` and the rendered Secret
   `manifests/providers/hybrid/k8s/cilium-addon-hybrid.yaml` no longer contains
   the cilium-envoy ServiceAccount / ConfigMap / Service / DaemonSet. Gateway-API
   proxying is unaffected — the agent serves it in-process. Pure-hetzner spokes
   keep the standalone DS until re-rendered consistently.
8. **Gateway API hostNetwork mode + Hetzner LB (external provision).**
   NodePort+TPROXY (L7LB) silently drops genuinely-external SYNs (no SYN-ACK,
   no RST, no BPF trace) on ALL interfaces — a known Cilium class of bugs
   (#8971/PR#12434, #35012, #43819) — while loopback/self-originated tests
   pass (false positives). **Codified**: `gatewayAPI.hostNetwork.enabled: true`
   with `nodes.matchLabels: node-role.kubernetes.io/control-plane=` — the
   embedded Envoy binds `0.0.0.0:80/443` directly on the CP node; the gateway
   Service becomes ClusterIP so the Hetzner CCM no longer provisions an LB.
   Two prerequisites (both codified):
   - `envoy.securityContext.capabilities.keepCapNetBindService: true` +
     `NET_BIND_SERVICE` capability on the cilium-agent container, otherwise
     Envoy fails `cannot bind '0.0.0.0:80': Permission denied`.
   - Cilium's stock `CILIUM_PRE_mangle` rule
     `-m socket --transparent ... -j MARK --set-xmark 0x200` matches
     post-handshake packets destined to the transparent host-bound listener
     and reroutes them via table 2004 (local lo), breaking the TCP handshake
     for external traffic (loopback exempt via Cilium's `! -o lo` exclusion).
     Since Cilium wipes foreign rules on every chain re-sync, a guard
     DaemonSet (`cilium-hostnetwork-mangle-guard`, codified in the rendered
     addon) continuously re-asserts
     `iptables -t mangle -I CILIUM_PRE_mangle 1 -p tcp -m multiport --dports 80,443 -j RETURN`.
   The public-facing Hetzner LB (`waypoint-gateway-lb`, lb11/hel1, public
   `77.42.12.176`, private `10.0.0.5` on network 12543641) is an EXTERNAL
   resource — out of scope for in-cluster GitOps and unmanageable by the CCM
   in hostNetwork mode (ClusterIP Service, no NodePorts). **Codified**:
   `scripts/hybrid/ensure-waypoint-lb.sh` (idempotent hcloud CLI: services
   80/443 with PROXY protocol, HTTP health checks with relaxed timeouts, CP
   server target over the private network `use_private_ip=true` — the
   embedded Envoy binds IPv4-only, so public-IPv4 health probes must be
   avoided).    DNS `waypoint.nutgraf.in`/`api.waypoint.nutgraf.in` → LB public
   IPv4 (Hetzner Cloud DNS).
9. **Known limitation: Envoy stuck-drain on Gateway changes.** Modifying
   `Gateway` annotations or listeners triggers an Envoy hot-restart in
   Cilium. In hostNetwork mode this frequently results in a stuck drain
   state (parent shuts down after drain, child fails to bind, traffic drops
   to `000` on every port while the sockets remain bound by the draining
   parent — observed twice). **Workaround**: after applying Gateway changes,
   manually restart the Cilium agent on the control-plane node:
   `kubectl delete pod -n kube-system -l k8s-app=cilium --field-selector spec.nodeName=<cp-node>`.
   Deliberately NOT mitigated with a watchdog: Gateway topology is a Day-1
   operation; tracked as upstream technical debt (embedded-Envoy hot-restart
   in hostNetwork mode). **Superseded by addendum 10** (the same failure
   class reproduces on plain agent restarts, without any Gateway change).

10. **Confirmed restart-safety defect in the Cilium v1.17.18 hostNetwork
    embedded-Envoy lifecycle under overlapping agent replacement — decision:
    decouple Envoy from the agent (draft for sign-off, NOT yet applied).**

    **Observed failure.** A plain Cilium agent restart on the control-plane
    node — no Gateway configuration change, no CNP change, no certificate
    change — repeatedly produces an HTTPS availability loss on
    `waypoint.nutgraf.in`. `kubectl delete` of the agent pod triggers an
    immediate DaemonSet replacement that starts **before** the old
    container's grace-period termination releases the host abstract sockets
    (`@envoy_domain_socket_parent_0` / `@envoy_domain_socket_child_0`) and
    the `:80`/`:443` listeners. The new Envoy finds the previous hot-restart
    participant still present, engages the hot-restart drain handoff at
    startup, and the parent stays in drain until the 900s parent-shutdown
    boundary. Gateway HTTPS goes `000` during/after that transition. Severity
    is variable: one run stayed down until the agent pod was deleted; another
    recovered by itself after ~4 minutes.

    **Evidence (two controlled replays, 2026-08-17, all times UTC).**

    | Generation | Pod start | Envoy start | Parent-shutdown (+900s) | HTTPS |
    |---|---|---|---|---|
    | A (wedged) | 12:19:18Z | 12:19:27Z | 12:34:27Z (+900.2s) | `000` observed 12:48:05Z–12:53:45Z (probe logs), no self-recovery; restored ~12:59:31Z only after agent pod deletion |
    | B (self-healing) | 12:51:51Z | 12:51:52Z | 13:06:52Z (+900.0s) | `200` 12:59:31Z–13:13:45Z, `000` 13:14:01Z–13:17:44Z, self-recovered 13:18:56Z, no intervention |

    Both runs share the same conditions: clean socket ownership at start
    (single Envoy generation, 4× SO_REUSEPORT `:80` + 1× `:443`, no stale
    external process); LDS/SDS fully converged before the failure
    (`tenant-waypoint/cilium-gateway-waypoint-gateway/listener` added,
    TLS cert from the `cilium-secrets` SDS sync served, continuous `200`);
    no Gateway object mutation (Gateway 42h old, PROGRAMMED); agent log
    silent at the boundary except the `[shutting down parent after drain`
    line — no listener reprogramming. At the boundary the Envoy re-bound
    its `:443` listener set 1→4 internally (no agent LDS involved) and the
    process never exited; socket ownership stayed within the expected
    generation throughout. The overlap window between the new pod start
    (12:51:51Z) and the old container's termination (~12:52:30Z) explains
    why the handoff engages on every restart, and why severity varies with
    the race.

    **Root-cause classification.** Embedded Envoy + hot-restart handoff
    engaging during the overlapping agent replacement window, in Cilium
    v1.17.18 hostNetwork Gateway mode. We do **not** claim a deeper upstream
    Envoy bug: the mechanism is fully consistent with the observed data, but
    the upstream defect boundary is not established (no source-level
    evidence). Conclusion wording: **confirmed restart-safety defect in the
    Cilium 1.17.18 hostNetwork embedded-Envoy lifecycle under overlapping
    agent replacement.**

    **Decision.** Move to **decoupled Envoy**; do **not** add a watchdog.
    A watchdog that deletes agent pods on HTTPS `000` turns a deterministic
    lifecycle defect into an availability-control loop (delete → restart →
    re-wedge) and was rejected. Decoupling gives Envoy an independent
    lifecycle: agent restarts (including the overlap window) never restart
    Envoy, and Gateway `:80`/`:443` stays served. Mechanics: `envoy.enabled:
    true` in `manifests/providers/hybrid/cilium-values.yaml` renders the
    standalone `cilium-envoy` DaemonSet; the chart flips
    `external-envoy-proxy: "true"` atomically with `envoy.enabled=true`
    (templates/cilium-configmap.yaml:1508), so no dual-mode intermediate
    state exists (the addendum-7 EADDRINUSE collision class). The DS
    supports hostNetwork, `nodeSelector`, and
    `keepCapNetBindService`/`NET_BIND_SERVICE` (envoy.enabled DS +
    `gatewayAPI.hostNetwork` semantics preserved).

    **Migration / rollback.** Status: draft for sign-off — nothing applied
    yet; the addon is still the addendum-7 embedded configuration.
    Migration sequence:
    1. Pre-flight: verify current ingress stays serving (LB health check
       continuity gate); identify and record current Envoy socket holders.
    2. Render the addon with `envoy.enabled: true` (single rendered
       manifest, `external-envoy-proxy` flips in the same sync — no
       dual-mode window by construction).
    3. Stale-holder handling: the flipped agents restart and release the
       embedded Envoy's sockets **before** the DS pods bind; additionally,
       a fail-closed init container on the DS (via the chart's
       `envoy.initContainers` hook) verifies no foreign process holds
       `@envoy_domain_socket_parent_0`/`:80`/`:443` and exits non-zero with
       a clear message otherwise — the DS pod crash-loops loudly instead of
       silently wedging.
    4. Verify: `:80`/`:443` bound by the DS pod only (not the agent);
       LDS listener `tenant-waypoint/cilium-gateway-waypoint-gateway/
       listener` present; TLS cert served (SDS via `cilium-secrets`);
       LB backend healthy; external HTTPS `200`.
    5. Rollback boundary: git revert of the values flip + re-render
       (`envoy.enabled: false`); the DS pods are removed and the agent
       flip back happens **after** DS termination (the init-gate ordering
       below also applies in reverse), then the same verification gate.
       Rollback is exercised only as a deliberate operation, never as the
       recovery mechanism.

    **DS restart-overlap mitigation (concrete, enforced by the controller
    + the init gate, not by operational care).** The embedded-mode failure
    is fundamentally the overlap of two Envoy generations competing for
    host sockets. In decoupled mode this is prevented structurally:
    - Socket safety is guaranteed by the `wait-for-envoy-release` init
      container: a new DS Envoy never binds until the host sockets
      (`:80`/`:443`, `@envoy_domain_socket_parent_0`) are free AND the
      agent's xDS socket is up — regardless of how the new pod was created
      (rollout, replacement, out-of-band). 600s fail-closed deadline.
    - `envoy.updateStrategy.type: RollingUpdate` with
      `rollingUpdate.maxUnavailable: 1` (default is `2`). NOTE: the
      DaemonSet API rejects `maxUnavailable: 0` ("cannot be 0 when
      maxSurge is 0"), and surge (`maxSurge: 1`) would deadlock the
      release gate (the old pod holds the sockets until the new pod is
      ready — the init gate would wait forever). With `maxUnavailable: 1`
      the per-node update is delete-old-then-create-new (grace 1s), and
      the init gate then serializes socket ownership: no old/new Envoy
      generation can coexist on the host sockets at any rollout.
    - Agent restarts are decoupled entirely: the agent DaemonSet rollout no
      longer restarts Envoy, so the failure trigger (overlapping agent
      replacement) cannot engage the Envoy drain state machine.
    - The DS pod's own drain on kubelet termination is safe by construction:
      the replacement's init gate waits for the sockets the terminating pod
      releases, and kubelet enforces the final kill if the drain stalls.

    **Out of scope** (explicitly unchanged by this amendment): BFF
    cross-node packet-loss investigation remains a separate Phase 2 effort;
    no CiliumNetworkPolicy weakening; no certificate/issuer changes; no
    watchdog that deletes Cilium agents when HTTPS becomes unhealthy.

    **Codified**: executed 2026-08-17 on `spoke-pool-hybrid-dev-01`.
    `manifests/providers/hybrid/cilium-values.yaml` now sets `envoy.enabled:
    true`, `envoy.updateStrategy.rollingUpdate.maxUnavailable: 1` (see the
    mitigation note: `0` is invalid for DaemonSets without surge), and
    `envoy.securityContext.capabilities.envoy: [NET_ADMIN, SYS_ADMIN,
    NET_BIND_SERVICE]` (the chart default envoy cap list lacks
    NET_BIND_SERVICE; without it the DS cannot bind `:80`/`:443`).
    `manifests/providers/hybrid/k8s/cilium-addon-hybrid.yaml` carries the
    chart-rendered cilium-envoy ServiceAccount / ConfigMap / Service /
    DaemonSet (exactly four resources), `external-envoy-proxy: "true"` in
    the cilium-config data, and the DS post-render additions:
    `wait-for-envoy-release` init container (busybox; polls for the agent
    xDS socket on the shared hostPath AND absence of LISTEN `:80`/`:443`
    and `@envoy_domain_socket_parent_0` in the host netns; 600s fail-closed
    deadline) and `updateStrategy.maxUnavailable: 1`. The section above
    ("Migration / rollback") is the operator reference for this rollout.

## Addenda (2026-08-18)

### 11. Hybrid Topology Database Model (placement + storage per location)

**Foundation.** ADR-014's Platform-Wide Placement Rule applies to hybrid cells
unchanged: every stateful workload runs on **worker nodes only**
(`nodeSelector: node-role.kubernetes.io/worker: ""` — key-only label, never
`"true"`), control-plane nodes keep `control-plane:NoSchedule`, and placement is
declared, never inferred from storage binding. Hybrid adds one orthogonal axis:
`workload-location` (`on-prem` vs `hetzner`), which determines the storage class a
stateful workload may use.

**Failure that motivated this model.** On `spoke-pool-hybrid-dev-01`,
`shared-cnpg` (then at `manifests/spoke/spoke-catalog/infra/cnpg-cluster.yaml`;
deployed from per-environment overlays since Wave 1, see Codified) was
created with `storage.storageClass: hcloud-volumes` and **no nodeSelector**.
With placement undeclared, CNPG scheduled the sole instance onto the
control-plane node. Storage binding did not save it: `hcloud-volumes` is
attachable on the Hetzner CP host, so the PVC bound and the cluster ran "fine"
on the CP — placement by accident of storage topology, exactly the failure
class ADR-014 now bans.

#### Placement classes (one per location)

> **Correction 2026-09-25 — the label value is `on-prem`, not `home`.** The
> CLASS is called `home`; the label it selects on is `workload-location=on-prem`.
> This document used `home` for both in most places and `on-prem` in one, and the
> implementation has always written `on-prem`: `provision-flatcar-worker.sh` at
> join time, the hybrid composition's default nodeSelector, both CSI addons'
> node affinity, `hub-bootstrap.sh`'s worker selector and the bootstrap
> postconditions. No live object anywhere selects `workload-location=home`.
>
> Every PRESCRIPTIVE mention is corrected: this table, the definition above, the
> `workload-location` axis, and the CSI/local-path addon affinities that §Codified
> specifies. What is left as written is narrative — incident accounts and
> postmortems that describe what a selector said at the time, where changing it
> would falsify the record.
>
> The cost of the ambiguity, so it is not repeated: `ephemeral-job-operator`
> implemented the class from this table and selected `workload-location=home`.
> Every sandbox pod it built was well-formed and admissible and matched no node,
> so it sat Pending while its EphemeralJob reported `WaitingForCapacity` — a
> message about capacity for what was a label mismatch, which sends the reader
> looking for nodes rather than for a typo.

| Class | nodeSelector (BOTH required) | StorageClass | Nodes |
|---|---|---|---|
| home | `node-role.kubernetes.io/worker: ""` + `workload-location: on-prem` | `local-path` | Flatcar home workers |
| hetzner | `node-role.kubernetes.io/worker: ""` + `workload-location: hetzner` | `hcloud-volumes` | burst pool (`replicas: 0` default in dev) |

- A stateful workload is **bound to exactly one location at creation**; the
  location never changes afterwards (storage is node-local in the home class).
- `workload-location` is **never sufficient on its own** — the platform-wide
  worker selector is mandatory in both classes (ADR-014).
- The home-located dev cluster keeps the name `shared-cnpg`; a hetzner-located
  twin is named `shared-cnpg-hetzner`. In dev/stg only the home cluster is
  provisioned; the hetzner twin is provisioned only when burst capacity is
  actually scaled.

#### Storage class contract

| StorageClass | Provisioner | Binds on | Snapshots |
|---|---|---|---|
| `hcloud-volumes` | csi.hetzner.cloud | Hetzner nodes only (WaitForFirstConsumer) | yes |
| `local-path` | rancher.io/local-path | home workers only (node-local) | **no** |

- `hcloud-volumes` remains the **default** class in hybrid cells; `local-path`
  is referenced explicitly by name. A PVC with no class on a home worker gets
  `hcloud-volumes`, cannot bind (no Hetzner topology), and stays Pending — this
  is the correct failure mode, not an invitation to remove the class.
- `local-path` PVCs are **node-pinned**: data lives on the exact home worker
  that bound the claim. Node affinity on the PVC is a *consequence* of declared
  placement, never the placement mechanism itself (ADR-014).
- **BANNED**: `hcloud-volumes` on a home worker (hcloud CSI cannot provision
  for a home VM) and `local-path` on a Hetzner worker (provisioner runs on
  home nodes only via nodeAffinity).
- Durability for home-located clusters: recovery is **barman restore only**
  (ADR-014 §Backup and restore contract — Hetzner Object Storage,
  `spoke-pool-backups` @ `https://hel1.your-objectstorage.com`) — local-path
  has no volume-snapshot support, so CNPG WAL archiving + S3 restore is the
  sole restore mechanism; see "Backup and restore isolation" below.
  Restoration requires a base backup **plus** WALs. Retention stays `30d`.

#### Backup and restore isolation

- Each spoke's backup data lives in a **per-spoke prefix**,
  `s3://spoke-pool-backups/<spoke-name>/` (bucket on Hetzner Object Storage per
  ADR-014 §Backup and restore contract), set via the
  `platform-spoke-catalog` ApplicationSet kustomize patch on
  `Cluster.spec.backup.barmanObjectStore.destinationPath` (keyed on the ArgoCD
  cluster name `{{ .name }}`). Previously every spoke shared the single
  `shared-cnpg` folder — a dev/stg collision that would cross-contaminate
  recoveries. The isolated prefix is established and proven **before** any
  destructive cutover.
- Base backups are declared per spoke via a static `ScheduledBackup`
  (`spec.cluster.name: shared-cnpg`, `backupOwnerReference: cluster`, daily
  schedule); none existed before Wave 1. `destinationPath` is **not** set on
  the ScheduledBackup — its CRD (CNPG 1.29) carries no destination fields; the
  backups inherit the Cluster's barman configuration.
- `spec.immediate: true` fires the first base backup at resource creation —
  the declarative **G1 gate**: the backup must reach `completed` (Backup CR)
  and appear in the new prefix before Wave A of any environment.

#### CSI daemonset boundary

The shared `csi-addon-template.yaml` runs `hcloud-csi-node` on **every** node
(its affinity excludes only Hetzner robot/root servers; a label-less home
worker matches and gets a driver that cannot reach block devices). Codified
in a new hybrid-cell addon `manifests/providers/hybrid/k8s/csi-addon-hybrid.yaml`
(referenced from `spokepool-hybrid-composition.yaml`, analogous to
`cilium-addon-hybrid`):

1. `hcloud-csi-node` DaemonSet: add `nodeAffinity required NotIn
   workload-location: on-prem` (exclude home workers).
2. `local-path-provisioner` DaemonSet: `nodeAffinity required In
   workload-location: on-prem`, hostPath `/opt/local-path-provisioner`.
3. `local-path` StorageClass (`WaitForFirstConsumer`, `reclaimPolicy: Delete`,
   **not** default).

**Scope correction (see §22).** This boundary was written while only *spokes* had
home workers, so it was codified in the spoke addon alone. §19 gave the **hub**
home workers too, at which point the hub's day-0 CSI asset needed the identical
guard and did not have it. The boundary has two enforcement points, not one.

#### CNPG pattern for hybrid spokes

The CNPG Cluster manifest for the home class (`shared-cnpg`, per-environment
overlay `manifests/spoke/spoke-catalog/environments/{dev,stg,prod}/hybrid/cnpg-cluster.yaml`)
declares:

```yaml
spec:
  storage:
    size: 100Gi
    storageClass: local-path
  affinity:
    enablePodAntiAffinity: true
    topologyKey: kubernetes.io/hostname
    nodeSelector:
      node-role.kubernetes.io/worker: ""
      workload-location: on-prem
```

- Instances, the Pooler, barman WAL-archiver jobs, and Atlas migration jobs all
  inherit this placement. The Pooler's own `affinity` must **mirror** the
  cluster selector — the CP-pinning bug reproduces in poolers.
- With `instances: 1` on local-path, pod anti-affinity is moot but retained so
  a later `instances: 2` spreads replicas across two home workers (each replica
  node-pinned via its PVC).
- Crossplane `tenant-db` flow unchanged: provider-sql over
  `shared-cnpg-rw.platform-data.svc.cluster.local`, credentials via ESO from
  Infisical (ADR-003); DB/user creation is storage-agnostic.

#### Migration of existing spokes (codified procedure)

> **Superseded for the hybrid overlays (2026-08-23).** The A→B→C cutover below
> exists to carry *data* through a storage-class change, via barman restore. It does
> not apply to a spoke that never initialised, and it is no longer what the manifests
> describe. See "Home class applied directly" immediately after this procedure.


Storage class cannot change in place and the recovery source requires an
isolated, proven prefix. The cutover is therefore a destructive-first GitOps
state machine (per-environment overlay state; dev and stg progress
independently because their authoritative state is directory-scoped):

1. **Wave A — prune.** Remove both `cnpg-cluster.yaml` and
   `scheduled-backup.yaml` from the environment overlay; ArgoCD prunes the
   Cluster **and its ScheduledBackup**. No resource references the pruned
   cluster during the WAL-only window — the `ScheduledBackup → Cluster`
   dependency is pruned along with its root. The barman store is untouched.
2. **Wave B — recover.** Re-add `cnpg-cluster.yaml` with
   `bootstrap.recovery` (method `object_store`, source = per-spoke prefix),
   the home-class placement + `local-path` storage, and re-add
   `scheduled-backup.yaml` with `immediate: true`. The name `shared-cnpg` is
   kept, so tenant connections (`shared-cnpg-rw.platform-data...`) are
   unchanged. Recovery is proven by: restored data + ADR-014/§11 placement
   checks + a completed post-recovery base backup (CNPG fires no backup while
   the cluster is still restoring).
3. **Wave C — steady state.** Return `bootstrap` to `initdb` in the manifest.
   `.spec.bootstrap` is creation-time state (CNPG does not re-reconcile it);
   the spoke-catalog ApplicationSet permanently ignores `/spec/bootstrap` —
   the declarative steady-state manifest cannot represent the historical
   bootstrap mechanism of an already-initialized cluster.
4. **Hetzner-only spokes skip A→B→C:** `hcloud-volumes` block devices attach
   anywhere in the zone, so changing `nodeSelector` rolls instances onto
   workers with their existing PVCs (placement-only rotation).
5. barman `retentionPolicy: "30d"` remains the safety net; WALs under the old
   shared prefix become orphaned garbage once the per-spoke prefix is live.

#### Home class applied directly (2026-08-23)

The three `environments/*/hybrid/cnpg-cluster.yaml` overlays carried a header saying
storage and placement were *"deliberately UNCHANGED … the live cluster's PVC is
hcloud-volumes (CP-attached); a worker/home selector would strand it until the A→B→C
cutover"*. That premise expired when the spoke was reprovisioned, and the manifests
were left describing a cluster that no longer existed:

```
phase                     Setting up primary   (never completed)
readyInstances            (none)
PVC shared-cnpg-1         Pending, 85m         (never bound)
PersistentVolumes         0
firstRecoverabilityPoint  []
lastSuccessfulBackup      []
```

With no bound PVC, no PV and no backup, Waves A and B are no-ops — there is nothing
to prune and nothing to restore — so the end state is reachable directly. The hybrid
overlays now declare the home class outright: `local-path`, both selectors, barman to
Hetzner Object Storage unchanged. Durability for the home class is S3 only;
local-path has no snapshot support.

The A→B→C procedure remains correct and remains the required path for any spoke that
*does* hold data.

Three defects surfaced while applying this, each invisible in the way §11 warns
about:

1. **A duplicate `affinity:` key.** Each hybrid overlay already ended with an
   `affinity` block carrying `enablePodAntiAffinity` and `topologyKey` but no
   `nodeSelector`. YAML is silently last-wins (REMEMBER.md), so that trailing block
   defeated the placement — and would have defeated any nodeSelector added earlier in
   the file. Deduplicated.

2. **`prod/hetzner` had no nodeSelector at all**, while dev and stg hetzner both did.
   A plain ADR-014 violation, unrelated to the hybrid work and unnoticed because
   nothing checked that tree.

3. **The NATS leaf node had no placement in any of the six overlays**, and inherited
   `hcloud-volumes` from the provider-neutral base — so on a hybrid spoke its PVC
   could bind nowhere and `nats-0` sat Pending (observed: 105 minutes). Fixed per the
   §22 convention: the base declares only the ADR-014 worker selector, and each
   overlay supplies the location and storage class as a strategic-merge patch.

**Why none of this was caught.** `preflight/85-placement-class.sh` globbed only
`manifests/hub-core-services/providers/*/*`. It never looked at
`manifests/spoke/spoke-catalog/environments/*/*` — the tree holding every one of
these files. The check passed throughout. It now scans both, and immediately failed
on all six NATS overlays before they were fixed.

#### Enforcement / verification (hybrid-specific)

Beyond the ADR-014 post-bootstrap validation, in every hybrid spoke:

- every stateful workload's nodeSelector contains **both**
  `node-role.kubernetes.io/worker: ""` and exactly one `workload-location`;
- storageClass and location agree (`local-path` ↔ home, `hcloud-volumes` ↔
  hetzner);
- zero `hcloud-csi-node` pods on home workers; `local-path-provisioner` pods
  on all home workers;
- every `ScheduledBackup` references an existing Cluster; per-spoke barman
  prefixes `s3://spoke-pool-backups/<spoke-name>/` are unique across spokes.

**Codified:** the model above is the spec. Manifest changes required by the
Wave-1 merge (per §11 review):

1. CNPG moves out of `manifests/spoke/spoke-catalog/infra` (the base stays
   provider-neutral): `cnpg-cluster.yaml` + `scheduled-backup.yaml` land in
   `manifests/spoke/spoke-catalog/environments/{dev,stg,prod}/{hybrid,hetzner}/`.
2. `take-along-label.capi-to-argocd.provider` added to both provider
   compositions (`hybrid` / `hetzner`); the `platform-spoke-catalog`
   ApplicationSet path resolves `{{ .Values.environmentSlug }}/{{ provider }}`
   and hard-fails with `{{ fail }}` on any other provider value
   (`missingkey=error` on the label).
3. ApplicationSet kustomize patch targets **`Cluster/shared-cnpg` only**:
   `/spec/backup/barmanObjectStore/destinationPath` →
   `s3://spoke-pool-backups/{{ .name }}/`; every overlay manifest declares
   `endpointURL: https://hel1.your-objectstorage.com` per the ADR-014 backup
   contract (the pre-contract manifest shipped without it).
4. `scheduled-backup.yaml` per overlay — static (no per-spoke fields):
   `spec.immediate: true` at creation (fires the G1 base backup), daily
   schedule in steady state, `backupOwnerReference: cluster`.
5. spoke-catalog syncOptions gain `RespectIgnoreDifferences=true`, together
   with `ignoreDifferences: Cluster/shared-cnpg → /spec/bootstrap`.
6. `csi-addon-hybrid.yaml` (new, wired into `spokepool-hybrid-composition.yaml`):
   hcloud-csi-node excludes home workers, `local-path-provisioner` DaemonSet
   on home workers, `local-path` StorageClass (`WaitForFirstConsumer`,
   `reclaimPolicy: Delete`, **not** default).
7. post-bootstrap-validate.sh gains the §11 enforcement checks (Wave-8
   enforcement pass).



### 12. Backup-credential chain incident (2026-08-18) — codified lessons

**Incident (facts).** On `spoke-pool-hybrid-dev-01` the CRS-delivered
`infisical-auth` Secrets (platform-ops + cert-manager) vanished, taking
down the ESO `infisical-backend` store, all ExternalSecrets, and — via the
CNPG barman envs — WAL archiving (the G1 gate). The root cause was an
ArgoCD Application deletion performed with forced finalizer removal while
the ApplicationSet still declared
`syncPolicy.preserveResourcesOnDeletion: false`. The resulting prune
deleted the ArgoCD-tracked platform Namespaces (`namespaces.yaml` is part
of the catalog), and the namespace cascade deleted the untracked
CRS-delivered Secrets inside them. The CRS
(`spoke-pool-hybrid-dev-01-bootstrap`, strategy `Reconcile`) did not
self-heal, and no store/externalsecret revalidation occurred. Recovery
sequence: SMI rotation, wrapper re-render, binding hash bump + re-apply,
ESO revalidation nudges, CNPG instance restart, and the ES credential
correction. The operational procedure lives in
`docs/runbooks/backup-credential-chain-recovery.md`.

**Hardening (root cause prevention, codified).** Both ApplicationSets in
`03-platform-services-appset.yaml` now declare
`syncPolicy.preserveResourcesOnDeletion: true` (commit `3d7c65fd`).
Deleting an Application must never prune platform state; spoke removal is
a rotation concern, never a deletion. Deleting or regenerating
appset-managed Applications is banned — recreate/regenerate does not
restore pruned untracked resources either: a CRS re-applies payloads only
when the payload/binding hash changes.

**CRS `Reconcile` semantics (ADR-048 amendment).** A CRS with
`strategy: Reconcile` re-applies payloads only when the payload or binding
hash changes — it performs no live-state drift repair. A payload deleted
after a successful apply stays deleted until the payload changes;
`ApplyOnce` (the hub's CRS) is weaker still (apply strictly at provision
time). CRS guarantees creation, not steady state. The sanctioned way to
force a re-render (hash bump) is the SMI rotation below.

**Rotation (the sanctioned re-render).** `rotationPolicy`
(`{enabled: true, interval: 60d, overlapPeriod: 24h}`) is codified in
hub-operator Go on the `SpokeMachineIdentity` CR — there is no YAML to
edit. Immediate rotation is triggered by clearing `Status.NextRotation`;
the operator then rotates `smi-<spoke>-auth`, re-renders the CRS wrapper,
bumps the binding hash, and re-applies — re-delivering both
`infisical-auth` Secrets. The verification chain (secret resourceVersions,
binding generation/hash/applied) and the exact trigger command are in the
runbook. A plain status patch that omits the subresource is a no-op.

**ESO revalidation lag** *(corrected 2026-08-23 — this paragraph previously
said "stuck … indefinitely" and called for a controller; both were wrong,
and acting on them cost time twice)*. The ExternalSecret controller does not
watch `SecretStore`/`ClusterSecretStore` — verified against vendored
upstream, `pkg/controllers/externalsecret/externalsecret_controller.go`
`SetupWithManager`, which watches only ExternalSecret, Secret metadata and
optional generic targets. It does not need to:

- A failed fetch takes the error path at
  `externalsecret_controller.go:419` (`msgErrorGetSecretData`, our exact
  message) and returns `ctrl.Result{}, err`. A non-nil error requeues via
  the rate limiter, **not** `refreshInterval` — which governs only the
  success path (`getRequeueResult`).
- ESO's own limiter (`pkg/controllers/common/common.go:113`) is
  `baseDelay 1s, maxDelay 7m`. So a failing ExternalSecret retries within
  **7 minutes at worst**, regardless of a `refreshInterval` of `1h`.
- The store controller is `For(&ClusterSecretStore{})` with
  `--store-requeue-interval` defaulting to **5 minutes**, so it revalidates
  on its own too.

Measured on 2026-08-23: Infisical returned at ~10:53 and all 21
ExternalSecrets went `SecretSynced` by 11:04 with no intervention.

The annotation nudge in the runbook is therefore a **time-saver, not a
repair** — use it to skip a backoff window, never as recovery. No controller
should be built for this; the earlier "future work" note is withdrawn.

What remains true is that a recovering store produces a window in which
every dependent ExternalSecret reports `SecretSyncedError` while being
perfectly healthy. That is a *validation* concern, not a platform defect —
see `post-bootstrap-validate.sh`, which treats the window as critical.

If a future incident genuinely does require manual intervention here, record
what was actually observed: the reasoning above says the 2026-08-18 recovery
should not have needed the nudge, so either something else was broken or the
nudge merely shortened the wait.

**CNPG barman env cache.** The instance manager resolves
`barmanObjectStore.s3Credentials` once at instance boot; changes to the
referenced Secret after boot are not picked up (observed as a stale
`InvalidAccessKeyId` after the credential fix). Restoration requires an
instance restart — remediation commands are in the runbook.

**`hcloud-token` is not an S3 key.** The previous `s3-credentials`
ExternalSecret pointed both `accessKeyId` and `secretAccessKey` at the
Infisical `hcloud-token` (an HCloud API token). barman proved it live with
`InvalidAccessKeyId ... does not exist in our records` (exit status 4),
distinct from `AccessDenied` (key known but unauthorized) and
`NoSuchBucket`. Fix codified in commit `60aa3c66`: the ES reads
`S3_ACCESS_KEY_ID` / `S3_SECRET_ACCESS_KEY` from
`/spoke-pool/<spoke>/shared/` in the hub-secrets project, injected
per-spoke by the spoke-catalog ApplicationSet kustomize patch
(`PLACEHOLDER` default, mirroring the `crossplane-admin-credentials` and
`destinationPath` patches). These are raw-value secrets: plain
`remoteRef.key` with the full path, no `property`. Hetzner Object Storage
access keys start `0A…` (24 chars), never `HT…`.

**Immediate base backup (operator convention).** `ScheduledBackup` fires
only on its schedule; a manual base backup is a one-shot `Backup` CR
(`spec.cluster.name`, `spec.method: barmanObjectStore`) — operational,
not steady-state, and must not be added to the catalog. Verification is
Backup `phase: completed` plus the Cluster's `lastSuccessfulBackup`.

**CRS-delivered ClusterIssuer restoration.** The `infisical-fleet-issuer`
ClusterIssuer (delivered by the `{spoke}-cluster-issuer` wrapper) was
found missing on the spoke although the binding recorded `applied: true` —
the same no-drift-repair trap as the Secrets (a namespace cascade cannot
remove a cluster-scoped object; the issuer is untracked, so nothing
re-created it and the alloy / argocd-agent / nats certificates blocked on
it). Restoration re-applies the authoritative wrapper payload — always
operator-rendered, never hand-edited; per-spoke `clientId` must not be
committed to shared Git (ADR-048). After re-applying, a stale
exponential-backoff retry queue on the operator's CertificateRequest
controller may keep failing with `issuer not found`; a restart of the
infisical-issuer operator flushes the queue and surfaces the next layer
(auth or permission errors from Infisical itself). A durable fix
(spoke-identity-operator monitoring CRS delivery completeness) is
referenced in the ADR-048 amendment.

**Infisical Universal-Auth lockout.** Repeated failed logins with a
machine identity lock its auth method, surfaced as
`401 "This identity auth method is temporarily locked, please try again
later"` (and, on earlier attempts, `"... not allowed for the current
project"`). Every subsequent failed login extends the lock; credentials
must not be probed repeatedly. After the lock window (or an Infisical-side
unlock), a single login probe distinguishes a valid pair (202) from an
invalid one (401) — see the runbook. G1 gate: no cluster-change work may
proceed as long as the store/archiver chain is red or locked.

**Adhoc-change rule (meta-lesson).** Every change made during recovery has
a manifest/commit counterpart, a runbook procedure, or an ADR decision —
nothing may live only in shell history. Anything applied to a cluster that
is not reproducible from Git is debt that will recur as an incident.

### 13. Hyper-V Worker OS Evolution: Rejection of Talos and Adoption of Flatcar Container Linux (2026-08-19)

**Context & Motivation.**
The initial home worker design relied on Ubuntu distributions hosted on Windows hardware. While functional, this introduced severe operational friction:
- Modified Microsoft kernel lacking standard modules.
- Broken cgroup v2 namespace propagation requiring fragile `fix-cgroup-mount` init containers and `cilium-host-prep.service` workarounds.
- Inability to safely run `clean-cilium-state=true` on restart.
- Windows host sleep/standby lifecycle coupling.

To eliminate these constraints, the platform transitions home-lab workers to dedicated Generation 2 Hyper-V virtual machines running an immutable, container-optimized Linux OS orchestrated 100% remotely from macOS via SSH.

**Evaluation & Rejection of Talos Linux.**
Talos Linux was evaluated and live-tested on Hyper-V Gen2:
1. **Control Plane Incompatibility**: Talos Linux is an immutable appliance governed by its own `machined` daemon. Talos workers expect to connect to a Talos Control Plane (`apid` on port 50000) to receive cluster PKI and certificates. Because our hybrid Spoke control planes are standard CAPI Hetzner VMs running Ubuntu + Kubeadm (`v1.31.6`), a standalone Talos worker cannot complete the join without replacing the entire Hetzner control plane with Talos.
2. **Non-Standard Bootstrap**: Talos does not run `kubeadm join` or consume standard Kubeadm bootstrap tokens (`bootstrap.kubernetes.io/token`) minted by `hub-operator`.
3. **Hyper-V Boot Overhead**: Talos does not publish pre-formatted Hyper-V VHDX images with embedded extensions, requiring ISO transfer, maintenance-mode portproxies (`50000`), and UEFI boot-order manipulation.
*Conclusion*: Talos Linux is rejected for hybrid home workers because it cannot join a standard Kubeadm/CAPI control plane.

**Decision: Adoption of Flatcar Container Linux on Hyper-V.**
Flatcar Container Linux is adopted as the definitive OS for Hyper-V home-lab worker nodes:
1. **100% Native Kubeadm & CAPI Compatibility**: Flatcar runs standard systemd, `containerd`, and standard `kubeadm join`, seamlessly consuming the `<spoke>-home-worker-join` tokens minted by `hub-operator`.
2. **Immutable & Minimal**: Read-only `/usr` partition with atomic updates, zero package-manager drift, and purpose-built container isolation.
3. **Clean cgroup v2 & eBPF**: Standard Linux kernel with native cgroups v2 and BPF filesystem support, completely eliminating the legacy `fix-cgroup-mount` init container and allowing standard Cilium CNI deployment.
4. **Declarative Ignition Provisioning**: Node configuration (hostname, Tailscale auth key, SSH keys, kubelet, and `kubeadm join` oneshot systemd unit) is declaratively defined via Ignition (`config.ign`) attached as a virtual CD-ROM (`ignition.iso`) on first boot.
5. **Official Hyper-V Gen2 Artifacts**: Pre-built Generation 2 VHDX images (`flatcar_production_hyperv.vhdx.bz2`) downloaded and resized dynamically on the host.

**Addendum (2026-08-20): pod-based Tailscale DaemonSet retired.**
The composition-mounted `tailscale-node-addon` DaemonSet (and its in-composition
PSK ExternalSecret step) were removed from `spokepool-hybrid-composition.yaml`.
Live verification showed the DS pod crash-looping (119 restarts) while tailnet
connectivity was already provided natively on all node classes: control plane
via ClusterClass `preKubeadmCommands` `files[]`/tailscale up, home workers via
Flatcar Ignition §13, and burst (Hetzner) workers via the same ClusterClass
`preKubeadmCommands` block added to `spokepool-worker-bootstrap-v1`. The hub-side
`tailscale-hybrid-psk` ExternalSecret
(`manifests/providers/hybrid/k8s/tailscale-psk-es.yaml`, authkey + hostname,
consumed by the ClusterClass CP bootstrap) is unaffected and remains.

### 14. Cross-Node Datapath Architecture: Adoption of VXLAN Tunneling over Tailscale (`routingMode: tunnel`) and Removal of Native Direct Node Routes (2026-08-20)

**Context & Failure Mode in Native Mode.**
In the hybrid spoke topology, node `InternalIP`s are Tailscale IPs (`100.x.x.x`). Running Cilium in `routing-mode: native` required Tailscale subnet-route advertisement (`--advertise-routes`) and route acceptance (`--accept-routes`), coupling Linux kernel routing table 52 to Tailscale's cryptokey routing and exposing nodes to stale-route collisions when orphaned tailnet devices persisted.

More critically, in native routing mode with Gateway API `hostNetwork` listeners (Addendum 8), Envoy's upstream egress sockets sourced from the local Cilium ingress endpoint (`10.244.0.125`, endpoint id 1017, `identity 8 / reserved:ingress`, with `ifindex=0` and no veth). When remote home-worker backends answered with SYN-ACK, the inbound packets on `tailscale0` were intercepted by Cilium's TCX ingress hook (`cil_from_netdev`). Because endpoint 1017 has `ifindex=0`, Cilium's BPF redirected the packet into `cilium_host`, where it looped indefinitely between `cil_from_netdev` and `cilium_host` (`ICMP time exceeded in-transit`), timing out after 5s and returning `HTTP 503 Service Unavailable`. Host-side iptables masquerade and kernel routing table additions (`table local` / `lo /32`) were completely bypassed because Cilium's TCX BPF program captured the packet at the device layer before kernel stack processing.

**Decision: Definitive Adoption of VXLAN Tunneling (`routingMode: tunnel`).**
1. **VXLAN over Tailscale Transport**: Cilium hybrid clusters configure `routingMode: tunnel`, `tunnelProtocol: vxlan`, and `tunnelPort: 8472`.
2. **Removal of Native Direct Routes**: `auto-direct-node-routes` and `direct-routing-device` are disabled and removed. Stale native routes (`10.244.1.0/24 dev tailscale0` and `10.244.0.0/24 dev tailscale0`) are superseded by `cilium_host mtu 1150`.
3. **Structural Loop Prevention**: Under VXLAN encapsulation, cross-node pod-to-pod and ingress return traffic is encapsulated in UDP 8472 datagrams with outer node Tailscale IPs (`100.118.202.60:8472` ↔ `100.85.175.14:8472`). Upon arrival on the node's Tailscale endpoint, Cilium's VXLAN decapsulation handler directly delivers the inner packet to the local ingress endpoint without any unencapsulated device-layer routing loops.
4. **Tailscale Subnet Decoupling**: Because only UDP 8472 packets between node tailnet IPs traverse `tailscale0`, Tailscale subnet route advertisement and acceptance (`--advertise-routes` / `--accept-routes`) are no longer load-bearing for the datapath, permanently eliminating the v5 stale-route collision class.
5. **MTU Invariant**: `MTU: 1200` inner + 50B VXLAN header = 1250B outer, fitting strictly inside `tailscale0`'s MTU of 1280B with zero fragmentation (reconciling the VXLAN MTU invariant in Addendum 6).

### 15. Decoupled Standalone Envoy xDS Protocol Architecture and Gateway API HostNetwork Invariants (2026-08-20)

**Context & Discovery.**
Decoupled standalone Envoy (`envoy.enabled: true`, `external-envoy-proxy: "true"`, Addendum 10) connects to the Cilium agent's xDS server over the host Unix Domain Socket at `/var/run/cilium/envoy/sockets/xds.sock`.

Inspection of official Cilium source (`pkg/envoy/grpc.go`) revealed that Cilium v1.17 implements individual discovery services (`CDS`, `LDS`, `RDS`, `SDS`, `EDS`) and explicitly disables ADS (`AggregatedDiscoveryServiceServer` is commented out). When Envoy boots with `ads: {}` under `dynamicResources.cdsConfig` / `ldsConfig`, Envoy requests `StreamAggregatedResources`, causing gRPC stream resets (`unknown service envoy.service.discovery.v3.AggregatedDiscoveryService`).

**Decision & Codified Configuration:**
1. **ConfigMap Bootstrap xDS Mode**: In `cilium-envoy-config` (`bootstrap-config.json`), dynamic resources must use `apiConfigSource` (pointing to static cluster `xds-grpc-cilium` with `transportApiVersion: V3`, `apiType: GRPC`) for both `cdsConfig` and `ldsConfig`, rather than `adsConfig`.
2. **Version Check Bypass & Pinned Image**: When running decoupled standalone Envoy on Kubernetes 1.31+, the image must be pinned to the matching agent binary build (`quay.io/cilium/cilium-envoy:v1.36.9-1782267392-edeb3f2af56c37c407efa1f63f0b32f595399bbc`), `disable-envoy-version-check: "true"` must be set in `cilium-config`, and container arguments in the DaemonSet must be cleanly delimited strings.
3. **Capabilities & Host Binding**: The Envoy container requires `NET_BIND_SERVICE`, `NET_ADMIN`, `SYS_ADMIN` and `envoy-keep-cap-netbindservice: "true"` to bind privileged host ports 80/443 on the control-plane node (`node-role.kubernetes.io/control-plane: ""`).
4. **PROXY Protocol Alignment**: Gateway API PROXY protocol parsing (`enable-gateway-api-proxy-protocol: "true"`) must be consistently configured across `cilium-values.yaml`, `cilium-config`, and operator flags, matching the Hetzner Load Balancer's `--proxy-protocol=true` setting.
5. **Mangle Guard DaemonSet**: The `cilium-hostnetwork-mangle-guard` DaemonSet on the control plane maintains `iptables -t mangle -I CILIUM_PRE_mangle 1 -p tcp -m multiport --dports 80,443 -j RETURN`, preventing Cilium's transparent socket filter from diverting incoming LB SYN/ACK packets into table 2004 loopback.

### 16. systemd-networkd Link Matching Invariant on CNI Nodes (2026-08-20)

**Context & Defect.**
Flatcar worker provisioning scripts initially wrote `/etc/systemd/network/10-static.network` with `[Match] Type=ether`. On Kubernetes nodes running Cilium CNI, `Type=ether` matches every dynamically created ethernet-class interface, including `cilium_net`, `cilium_host`, and every container veth (`lxc*`). Consequently, `systemd-networkd` attached static IP addresses (`172.30.0.11/24`) and default gateway routes to all pod network interfaces, corrupting the host routing table with duplicate default routes.

**Decision & Invariant:**
1. **Strict Link Name Pattern**: `10-static.network` must match only the physical uplink interface and explicitly exclude CNI and overlay links:
   ```ini
   [Match]
   Name=eth*
   Name=!cilium_* !lxc* !tailscale*
   ```
2. **Explicit Static Addressing**: Network configurations must set `DHCP=no` with deterministic static `Address=` and `Gateway=` assignments bound exclusively to `eth0`.

### 17. Post-outage hardening: Tailscale underlay invariant, ClusterClass collapse, and ingress LB into GitOps (2026-08-20)

Follow-on to addenda 14–16, closing the architectural fault lines the
`waypoint.nutgraf.in` outage exposed. Dev environment; changes are deliberately
not backward-compatible.

**17.1 — Tailscale is a node-level underlay ONLY (invariant).**
Under `routingMode: tunnel` (addendum 14) cross-node pod traffic is VXLAN-
encapsulated between node tailnet IPs, so pod IPs never reach the wire.
Advertising or accepting podCIDR subnet routes is therefore unnecessary **and
actively harmful**: `tailscaled` installs accepted routes into table 52, whose
`ip rule` priority (5270) precedes `main` (32766), so they **shadow Cilium's
tunnel route** for host-originated traffic to remote pods:

```
ip route get 10.244.1.57  →  dev tailscale0 table 52          # stale, unencapsulated
table main:                  10.244.1.0/24 via 10.244.0.86 dev cilium_host mtu 1150
```

Envoy was unaffected only because its proxy mark (`0x80b00`) matches rule 5210
(`fwmark 0x80000 → main`); unmarked host traffic — kubelet exec/logs/probes,
hostNetwork pods — took the stale path. **Codified**: `--advertise-routes` and
`--accept-routes` removed from both node classes
(`manifests/providers/_shared/spokepool-clusterclass-v1.yaml`,
`scripts/hybrid/provision-flatcar-worker.sh`). This retroactively vindicates the
instinct behind `347dfe53` — the route collisions were real — while removing the
dependency that made removing the flag fatal under native routing.

**17.2 — `spokepool-control-plane-v7` regression (fixed).**
v7 was the live-referenced template and had silently dropped, relative to v6,
both `preKubeadmCommands` lines that enable and verify `cilium-host-prep`:
`systemctl enable/start cilium-host-prep.service` and the fail-closed
`findmnt -n -t cgroup2 /run/cilium/cgroupv2` gate. The unit was still written by
`files[]`, but `WantedBy=multi-user.target` does not start an un-enabled unit, so
the shared cgroup2 mount kube-proxy-replacement requires would have been absent
on the next CP roll or reboot (`/run` is tmpfs). Also restored: the
`node.cloudprovider.kubernetes.io/uninitialized-` taint removal. Deliberately
**not** restored: control-plane taint removal (reverted by `d74cbeaa`) and the
bootstrap DNAT rules + `--advertise-address` patch (obsolete under tunnel mode).

**17.3 — ClusterClass collapsed to one control-plane template.**
The v1…v7 chain (7 near-identical `KubeadmControlPlaneTemplate`s, ~1000 lines of
duplication — the containerd/runc install block appeared 8×) is **deleted**.
There is now a single `spokepool-control-plane-v1`. That copy-paste chain is the
mechanism behind 17.2 and behind the worker bootstrap template drifting from the
CP templates. CAPI treats `KubeadmControlPlaneTemplate.spec.template.spec` as
immutable once referenced, so editing it now requires **reprovisioning the
spoke** instead of rotating a version suffix — the accepted dev-environment
tradeoff that replaces the version chain.

**17.4 — Ingress LB moved into GitOps; `ensure-waypoint-lb.sh` deleted.**
Supersedes addendum 8's "EXTERNAL resource — out of scope for in-cluster GitOps".

> **AMENDED by §25.1 (2026-08-23).** The 443 entry declared below was never
> created. `controlPlaneEndpoint.port` was also 443, and a Hetzner LB cannot
> carry two services on one listen port, so CAPH kept the apiserver there and
> discarded the ingress entry without error. Spoke tenant HTTPS was therefore
> dead from this change until §25 moved the apiserver to 6443. Only the 80
> entry ever worked.
Ports 80/443 are now declared as
`HetznerCluster.spec.controlPlaneLoadBalancer.extraServices` on
`spokepool-cluster-v1`, so CAPH owns the public entry point and **retargets it
automatically when the CP machine rolls** — eliminating the failure mode where
the LB kept pointing at a deleted server ID after every spoke reprovision.
`scripts/hybrid/ensure-waypoint-lb.sh` (which had zero call sites and was manual-
only) is removed.

*Consequence — PROXY protocol is OFF.* CAPH's `extraServices` API exposes only
`{protocol, listenPort, destinationPort}`; there is no `proxyProtocol` field, so
the LB cannot prepend a PROXY preamble. `enable-gateway-api-proxy-protocol` is
therefore `"false"` in `cilium-addon-hybrid.yaml` (ConfigMap key *and* operator
arg) and `proxyProtocol: false` in `cilium-values.yaml`. **These must move in
lockstep** — a mismatch makes Envoy read the preamble as malformed HTTP and reset
the connection. Client source IP no longer reaches Envoy; accepted for dev.

*Known gap — firewall rules remain uncodified.* CAPH's `HetznerCluster` v1beta1
has no `firewall` field (verified against the live CRD schema), so addendum 4's
manual LB→node rule step still stands. Intended fix is to extend the existing
hcloud SDK usage in `internal/hub-cli/` (currently delete-only) to create the
rules at spoke provision.

*Known tradeoff — shared LB blast radius.* Tenant ingress now shares an LB with
kube-apiserver. A dedicated ingress node pool is the production answer and would
break this ADR's "zero idle Hetzner worker cost" premise; deferred deliberately.

**17.5 — Corrections.** `driver_hybrid.go` set `base.LoadBalancer = false`
("Tailscale-only"), contradicting the composition
(`controlPlaneLoadBalancerEnabled: true`) and this ADR — reconciled. Spoke
`function-patch-and-transform` realigned v0.2.1 → v0.8.0 (hub was already v0.8.0).
The pre-commit kustomize gate now covers `hetzner/` and `_shared/`, not just
`hybrid/` — `_shared` feeds both cells and was previously ungated.

### 18. Public TLS: ACME issuer for browser-facing Gateway hostnames (2026-08-20)

**Fault.** `https://waypoint.nutgraf.in` was never browser-trusted. The served
leaf was signed by the PRIVATE fleet CA, and the intermediate was not included
in the chain:

```
subject= /CN=waypoint.nutgraf.in
issuer=  /O=Zero-Ops/CN=Fleet Intermediate CA
Verify return code: 20 (unable to get local issuer certificate)   # chain depth 0
```

Two independent defects: a private CA on a public hostname (fatal for browsers
regardless of chain), and an incomplete chain (fatal even for a client that
trusts the Zero-Ops root). HTTP worked, so the outage runbook's "resolved"
state masked this.

**Decision.** Public, internet-facing Gateway hostnames use ACME
(Let's Encrypt). `infisical-fleet-issuer` remains the issuer for internal fleet
and mTLS certificates (ADR-035, ADR-048) — the private CA is correct there.
These are complementary issuers, not a replacement.

**Codified.** `manifests/spoke/spoke-catalog/infra/acme-cluster-issuer.yaml`
defines `letsencrypt-staging` and `letsencrypt-prod`, wired into the
spoke-catalog kustomization.

Solver is **HTTP-01 via `gatewayHTTPRoute`**, not the ingress solver: the spoke
runs no Ingress controller (ingress is Cilium Gateway API), so the default
solver cannot complete a challenge. That solver requires
`--feature-gates=ExperimentalGatewayAPISupport=true` on the cert-manager
controller, added in `manifests/spoke/spoke-catalog/infra/cert-manager.yaml`.
The RBAC it needs (`gateway.networking.k8s.io/httproutes`) was already present
in `cm-cert-manager-controller-challenges`.

**Operational note.** Issue against `letsencrypt-staging` first. Let's Encrypt
production enforces 5 duplicate certificates per week, which is easily exhausted
while iterating on a Gateway/solver configuration. The `Certificate` for
`waypoint-tls` lives in the fleet-registry repo (`tenants/waypoint/workloads`),
so the `issuerRef` switch lands there, not in this repo.

### 19. Extension of Hybrid Home-Lab Pattern to Hub Cluster and S3 Barman Backup Invariant (2026-08-20)

**Context & Scope Expansion.**
The hybrid home-lab pattern established in ADR-046 initially targeted Spoke tenant clusters. To eliminate cloud compute costs on the Hub management plane, the pattern is extended to the Hub cluster:
1. Home-lab Flatcar VMs (`flatcar-hub-node-1`) join the Hub cluster over Tailscale as primary workload nodes with `hub-role=worker` and `workload-location=on-prem`.
2. Cloud Hetzner worker MachineDeployment (`hub-hybrid-dev-md-0`) scales to `replicas: 0`.

**Durability Invariant: Mandatory S3 Barman Backups for Home-Located Stateful Workloads.**
Because home-lab Flatcar nodes utilize ephemeral local disk (`local-path-provisioner` on `/opt/local-path-provisioner`) without volume snapshot or block replication capabilities, **data durability relies entirely on S3 Barman object storage backups**:
1. **S3 Backup Destination**: All CNPG PostgreSQL clusters (`platform-db` on Hub, `shared-cnpg` on Spokes) MUST configure `spec.backup.barmanObjectStore` targeting S3-compatible Hetzner Object Storage (`https://hel1.your-objectstorage.com`) with isolated bucket/prefix paths (e.g. `s3://hub-db-backups/<hub-name>/` for Hub, `s3://spoke-pool-backups/<spoke-name>/` for Spokes).
2. **Continuous WAL Archiving**: CNPG streams write-ahead logs (WAL) continuously to S3 with compression (`gzip`), enabling Point-In-Time Recovery (PITR).
3. **Scheduled & Immediate Base Backups**: `ScheduledBackup` resources with `immediate: true` ensure a baseline physical backup is taken immediately upon cluster creation and retained for `30d`.
4. **Worker-Only Placement**: All CNPG database pods MUST declare worker node affinity (`nodeSelector: node-role.kubernetes.io/worker: ""` / `workload-location: on-prem`) to guarantee separation from the control plane.

### 20. Per-environment hostname scheme and DNS automation (2026-08-20)

**Decision — environment is a DNS zone; production is bare.**

```
dev    waypoint.dev.nutgraf.in    api.waypoint.dev.nutgraf.in    oranger.dev.nutgraf.in
stg    waypoint.stg.nutgraf.in    api.waypoint.stg.nutgraf.in    oranger.stg.nutgraf.in
prod   waypoint.nutgraf.in        api.waypoint.nutgraf.in        oranger.nutgraf.in
```

Env-as-zone (rather than `dev.waypoint.…`) means **one wildcard per environment
covers every tenant**, instead of one per tenant. With two tenants already live
(`waypoint`, `oranger`) that difference compounds with each onboarding. It also
matches intent already in the repo: `manifests/environments/stg/patch-config.yaml`
had `DOMAIN: stg.nutgraf.in` before this change.

**Codified:** `DOMAIN` per overlay in `manifests/environments/{dev,stg,prod}/patch-config.yaml`
(dev was `nutgraf.local`, a local-dev artifact that never matched the real dev
endpoints). `manifests/environments/stg/patch-hubenvironment.yaml` is **new** — stg
previously had no HubEnvironment patch and silently inherited `nutgraf.in` from
base, i.e. staging claimed the production domain. prod deliberately has no patch:
inheriting bare `nutgraf.in` is correct under this scheme.

*Caveat:* `HubEnvironment.spec.domain` and `spec.tls` have **no Go consumers** —
they are declarative only. Making them load-bearing, and removing the ~15 hostname
literals (plus two compiled into `internal/auth-proxy/hydra.go`), is tracked
separately as the base-domain centralisation work.

**20.1 — ACME solver de-coupled from a single tenant.** Addendum 18's issuer
hard-pinned `parentRefs` to `waypoint-gateway`/`tenant-waypoint`, which could never
issue for a second tenant. Replaced with **one solver per tenant Gateway**,
discriminated by `selector.dnsZones`. cert-manager matches a dnsName against
dnsZones by suffix and takes the most specific, so `waypoint.dev.nutgraf.in` also
covers `api.waypoint.dev.nutgraf.in` without a separate entry. There is
deliberately **no catch-all solver**: an unmatched hostname must fail loudly rather
than attach a challenge to another tenant's Gateway.

**20.2 — Cross-namespace challenge attach (prerequisite, external).** These are
*Cluster*Issuers and cert-manager runs with
`--cluster-resource-namespace=cert-manager`, so challenge HTTPRoutes are created in
`cert-manager` while `parentRefs` targets the tenant namespace. The tenant Gateway's
**:80 listener must set `allowedRoutes.namespaces.from: All`** (or a selector
matching `cert-manager`) or every challenge fails to attach. That Gateway spec lives
in the private `fleet-registry` repo — it is a prerequisite this repo cannot enforce.

**20.3 — external-dns deployed on the spoke.** `manifests/spoke/spoke-catalog/infra/external-dns.yaml`
runs external-dns with the Hetzner **webhook** provider (Hetzner is not an in-tree
external-dns provider) and `--source=gateway-httproute`. It runs on the spoke, not
the hub, because the Gateways it publishes records for live there — the hub copy at
`manifests/hub-core-services/external-dns/` was never referenced by any
ApplicationSet and has never run, which is why the
`external-dns.alpha.kubernetes.io/hostname` annotation on the waypoint Gateway has
been inert.

`--domain-filter` and `--txt-owner-id` are `args[0]`/`args[1]` **by contract**,
pinned first so appended flags can never shift the patched index, and rewritten
per-spoke by the spoke-catalog ApplicationSet. `--domain-filter` is the
blast-radius control: a dev spoke cannot write prod records.

*Note:* spoke-catalog is **kustomize-rendered, not Helm** — `{{ .Values.x }}` does
not resolve there. `manifests/spoke/spoke-catalog/infra/certificates.yaml`
demonstrates the trap: its `commonName` reaches the live cluster as the literal
`argocd-agent:{{ .Values.spokeName }}`. Index patching via the ApplicationSet is
therefore the only injection mechanism available in that tree.

**20.4 — `hetzner-dns-credentials` pointed at the wrong credential.**

> **SUPERSEDED IN FULL by §26.1 (2026-08-23). The claim below is false.** Hetzner
> merged DNS into the Cloud API; `hcloud-token` authenticates against
> `api.hetzner.cloud/v1/zones` (verified, HTTP 200) and the standalone
> `dns.hetzner.com` API now 301s to the console. `hcloud-token` was correct;
> `hetzner-dns-token` does not exist and cannot be created. Do not apply this edit.

It mapped
`remoteRef.key: hcloud-token`. A Hetzner **Cloud** API token (console.hetzner.cloud —
CAPH/CCM/CSI) is **not** a Hetzner **DNS** API token (dns.hetzner.com): different
product, different console, different credential. This is the same failure class
addendum 12 recorded for `hcloud-token` being mistaken for an S3 access key.
Repointed to a distinct `hetzner-dns-token` key, which must be created in the
hub-secrets Infisical project before external-dns can authenticate.

**20.5 — Wildcard follow-up.** *(AMENDED by §26.1: the "real DNS token" this
awaits does not exist. The blocker is webhook API compatibility, not a credential —
see §26.2.)* `manifests/providers/hetzner/k8s/cert-manager-webhook-hetzner.yaml`
already installs the Hetzner DNS-01 webhook (v1.4.2) and is **installed and unused** —
no `dns01` solver exists anywhere. Once 20.4 lands a real DNS token, switching to a
per-env wildcard (`*.dev.nutgraf.in`) via DNS-01 becomes cheap and removes the
per-tenant Gateway coupling of 20.1 and the prerequisite of 20.2 entirely.

### 21. Hub control plane joins the tailnet; hub bootstrap ordering (2026-08-22)

**Correction.** The Topology table previously read *"the hub does not run
Tailscale"*. That was true only while the hub had exclusively Hetzner nodes. §19
moved hub workloads onto home-lab Flatcar nodes, and from that point the statement
was wrong: **invariant 6 applies to the hub control plane exactly as it does to the
spoke control plane.** The table has been corrected.

**Why.** Cilium derives its VXLAN tunnel endpoint from a node's `InternalIP`. A
home-lab worker's only `InternalIP` is its tailnet address, and a home node cannot
route the hub CP's Hetzner private address — from a home worker,
`ip route get 10.0.0.3` leaves via the house gateway. Cross-node pod traffic is
then dead in one direction while both nodes report `Ready`. Observed as CNPG
`Instance Status Extraction Error: HTTP communication issue`, with 100% packet loss
between a CP pod and a home-worker pod.

**Codified** in `internal/assets/manifests/classes/hetzner-mgmt-ubuntu-v1.yaml`,
mirroring `spokepool-clusterclass-v1.yaml`:

1. `/etc/tailscale-{authkey,hostname}` via `contentFrom.secret`, tailscaled brought
   up in `preKubeadmCommands` **before** kubeadm registers the node.
2. `dynamic-node-ip.sh` re-asserts `--node-ip=<tailnet IP>`, wired as kubelet
   `ExecStartPre` and re-run in `postKubeadmCommands`.
3. Every tailscale step is guarded on a non-empty `/etc/tailscale-hostname`. A
   pure-Hetzner hub is written an **empty** Secret and installs nothing — the same
   guard pattern the spoke uses. The Secret is always created, because an
   unresolvable `contentFrom.secret` blocks KubeadmConfig rendering entirely.
4. The Secret is staged into the **bootstrap (kind) cluster** at `capi-init`, since
   the ClusterClass reads it while the hub Cluster is being created.

**Consequence — the CCM can no longer initialise the node, and that is expected.**
Kubernetes' cloud-node-controller validates `alpha.kubernetes.io/provided-node-ip`
against the addresses the cloud reports. Hetzner reports only its own public and
private IPs, so a tailnet `--node-ip` fails validation:

```
provided node ip for node "…" is not valid:
failed to get node address from cloud provider that matches ip: 100.x.x.x
```

The CCM therefore never writes `.spec.providerID`. CAPI matches Machines to Nodes
**by providerID**, so the Machine keeps an empty `NODENAME` and `pivot-move` times
out on *"all machines joined"*. Two consequences follow, both codified:

- `dynamic-node-ip.sh` **self-assigns** `--provider-id=hcloud://<instance-id>` from
  the Hetzner metadata service. The instance id is authoritative on the node itself,
  which takes the CCM off the critical path for nodeRef.
- `postKubeadmCommands` clears `node.cloudprovider.kubernetes.io/uninitialized`,
  which the CCM would otherwise leave in place forever. This is what the spoke
  ClusterClass already does.

**Bootstrap ordering — home workers join before anything is installed on the hub.**
With the hub at `WorkerReplicas: 0` (§19) and the control plane keeping its taint,
a hub whose home worker has not joined has no node that satisfies the §11
worker-only placement rule, so every platform workload sits `Pending`.

CAPI cannot provision these nodes, so the orchestrator gained a `home-worker-join`
phase that invokes `scripts/hybrid/provision-flatcar-worker.sh --cluster hub` and
waits for a Ready node labelled `hub-role=worker`. It is idempotent: an
already-Ready node is skipped, so a resumed bootstrap does not repeat ~10 minutes of
Hyper-V work.

It runs between **`cluster-provision` and `pivot-move`**, which is earlier than the
workload boundaries would suggest. `pivot-move` installs cert-manager and the CAPI
operators onto the hub immediately, so a worker that only appears at boundary time
is too late — the phase fails first with *"timed out waiting for the condition on
deployments/cert-manager"*. At this point the hub API is up but nothing has been
deployed to it, and the admin kubeconfig is read from the CAPI-generated Secret in
the bootstrap cluster, because `pivot-move` is what normally persists it to disk.

The control plane keeps its `control-plane:NoSchedule` taint whenever home workers
are enabled — untainting it would re-create precisely the placement-by-accident
failure §11 bans. Only a hybrid hub with **no** home workers registers the control
plane as schedulable, because then no other node exists.

**`local-path` delivery to the hub.** §11 defines the home storage class, but
`manifests/hub-core-services/storage/` was referenced by no ApplicationSet, so the
class never existed on the hub and `platform-db`'s PVC stayed `Pending` forever. It
is now delivered at sync-wave 1 under `{{ if eq .Values.provider "hybrid" }}`, and
three defects in it were fixed: it declared itself `is-default-class` (§11 reserves
that for `hcloud-volumes`), its DaemonSet had no `workload-location: home` affinity
despite `tolerations: [operator: Exists]`, and its ServiceAccount name did not match
the one the provisioner gives its helper pods
(`local-path-provisioner-service-account`), nor did its ClusterRole allow
`pods: create/delete` for them.

**Two ordering traps in the shared addons**, both of which deadlock a single-node
cluster and are only visible in PVC/pod events:

- The Hetzner CCM must tolerate `node.cilium.io/agent-not-ready` and must carry
  **no** `instance.hetzner.cloud/provided-by` nodeSelector — that label is applied
  *by* the CCM, so the selector can never be satisfied on a fresh cluster. Without
  both, HCCM stays `Pending` → the node never gets addresses → cilium-agent aborts
  with *"unable to determine direct routing device"* → the agent-not-ready taint is
  never cleared → HCCM stays `Pending`.
- The Hetzner CSI must **not** be installed on the ephemeral kind bootstrap cluster.
  Its controller requires the Hetzner metadata service and CrashLoops on kind; the
  bootstrap cluster has no PVCs and needs no CSI. Gated on node `providerID` rather
  than on the provider name, so a Hetzner-hosted bootstrap cluster still gets it.

### 22. One tailscaled per node; CSI boundary reaches the hub; hybrid hub component set (2026-08-22)

Three corrections found while bootstrapping a hybrid hub end-to-end for the first
time. Each was invisible in the component that failed.

**Exactly one tailscaled per node, owned by the ClusterClass.** §21 established that
the hub control plane runs tailscaled natively from `preKubeadmCommands`. It did not
say what happens if something else also runs one, and something did: a
`tailscale-node` DaemonSet, added before §21 and never removed once the ClusterClass
superseded it. A DaemonSet with `hostNetwork: true` shares the host network namespace
and therefore the **same `tailscale0` interface** as the native daemon, so its
tailscaled stripped the tailnet addresses off the interface the native one was using,
leaving only the link-local address:

```
diff: ips tailscale0: [100.87.212.49/32 fd7a:…/128 fe80::…/64] -> [fe80::…/64]
rebind-reason=[ips-changed]
```

The node then **advertises an `InternalIP` that is configured nowhere**. Because
invariant 6 makes that address load-bearing twice over, the consequences land far
from the cause and name nothing that would lead an operator back to it:

- the API server cannot reach *any* kubelet on `:10250`, including the one on its own
  node — `kubectl logs/exec/port-forward` fail with `i/o timeout`;
- Cilium's VXLAN tunnel endpoint is derived from that same `InternalIP`, so cross-node
  pod traffic stops;
- a pod therefore loses CoreDNS whenever the DNS replicas sit on the other node,
  surfacing inside the container as `EAI_AGAIN`. (§34: `EAI_AGAIN` is not
  diagnostic of this. It also occurs with the tunnel entirely healthy, every time
  a query crosses it — which was the normal case until the local redirect landed.
  Reaching for invariant 6 on seeing it sends you to a tunnel that is fine.)

What was actually visible was Infisical in `CrashLoopBackOff` on *"Boot up migration
failed"*, with `platform-db` reporting *"Cluster in healthy state"* and its pooler
`1/1 Running`. ArgoCD self-heal re-created the DaemonSet after each manual repair,
so the fault also appeared to recur spontaneously.

**Node `Ready` does not cover this.** `Ready` is asserted over kubelet's *outbound*
connection to the API server, which keeps working throughout. The inbound path is now
gated explicitly by `scripts/validate/cluster/60-kubelet-reachability.sh`, which probes
`/api/v1/nodes/<node>/proxy/healthz` for every node and runs during the bootstrap
immediately after the workers join.

**The §11 CSI daemonset boundary applies to the hub, not only the spoke.** §11
diagnosed this precisely — *"a label-less home worker matches and gets a driver that
cannot reach block devices"* — but was written on 2026-08-18, before §19 (2026-08-20)
put home workers on the hub, so it codified the guard only in the spoke addon. The
hub's day-0 asset `internal/assets/catalog/cloud-providers/hetzner/csi/install.yaml`
kept the upstream affinity, which excludes Hetzner robot and root servers but matches
a home worker. Both `hcloud-csi-node` and `hcloud-csi-controller` scheduled onto the
Flatcar node, where there is no Hetzner metadata service, and crash-looped
indefinitely (151 restarts observed). The controller reached it by a second route:
its Hetzner `nodeAffinity` was only `preferred`, which does not exclude anything.

Both now carry the same `workload-location NotIn on-prem` requirement as the spoke addon.
A consequence appears here that cannot arise on a spoke: **the controller must
tolerate `control-plane:NoSchedule`**, because on a hybrid hub the only Hetzner node
*is* the control plane, and without the toleration the guard merely converts a crash
loop into `Pending` forever. A spoke has Hetzner workers and never meets this. The CSI
controller is cluster infrastructure, not a workload, so ADR-014 does not apply to it.
Both changes are no-ops on a pure-Hetzner hub, where no node carries
`workload-location` at all.

**A hybrid hub does not run NATS.** This was decided and implemented but recorded only
in an ApplicationSet comment, which is not where an architectural boundary belongs.
NATS has no consumer inside `hub-core-services` — it is the messaging layer that spoke
leaf-nodes connect to — and a hybrid hub runs its workloads on a single home-lab node
where a 10Gi JetStream volume is not worth the disk. It is therefore gated out of
`02-platform-data` for `provider: hybrid`; the `hetzner` overlay is retained for hubs
that do run it. Left ungated it does not fail cleanly: the StatefulSet requests
`hcloud-volumes`, which by §11 can never bind on a home worker, so the pod sits
`Pending` and its ArgoCD Application cannot finalize — one was found stuck deleting for
over two hours behind an unbindable PVC.

Per-provider component selection is expressed as overlays under
`manifests/hub-core-services/providers/<provider>/`, matching the established
`spoke-catalog/environments/<env>/<provider>/` convention: the base stays
provider-neutral and declares only the ADR-014 worker selector, and the overlay
supplies the §11 placement class. `scripts/validate/preflight/85-placement-class.sh`
asserts every overlay renders a consistent class.

**No change to the default StorageClass.** A hybrid hub cannot use `hcloud-volumes`
for anything — home workers cannot attach them and the control plane is tainted — which
invites the conclusion that the class should be removed or `local-path` made default on
hybrid. §11 already rejects this: a PVC that omits `storageClassName` getting
`hcloud-volumes` and staying `Pending` **is the intended failure mode**, and it is what
makes a missing placement declaration loud instead of silently landing data on
node-local ephemeral disk. The position stands unchanged.

### 23. Cloud load balancers are cluster-lifecycle resources, not Service side effects (2026-08-23)

§17.4 moved spoke ingress onto `HetznerCluster.spec.controlPlaneLoadBalancer.extraServices`
so CAPH owns the public entry point and retargets it when the CP machine rolls. §19
extended the home-lab pattern to the hub but left hub ingress on a CCM-managed
`type: LoadBalancer` Service. Two consequences followed, both realised.

**The leak.** The Hetzner CCM names a load balancer `a<service-uid>` and gives it one
label, `hcloud-ccm/service-uid`. Neither carries the cluster name nor a
`caph-cluster-*` label, so `hub teardown`'s name/label matcher could not see it — and
the CCM, the only component that can deprovision it, dies with the cluster. Each
rebuild therefore stranded one billed load balancer that nothing would ever collect.

Four rebuilds on 2026-08-22 filled a five-load-balancer project quota. The fifth
request was the spoke's own control-plane LB:

```
HetznerCluster spoke-pool-hybrid-dev-01-ps847
  READY=false  LoadBalancerCreateFailed
  failed to create load balancer: load balancer limit exceeded (resource_limit_exceeded)
```

`spoke-pool-hybrid-dev-01` sat at `InfrastructureReady=False` for five hours. The
bootstrap polled it for the full `CLUSTER_TIMEOUT` and logged only
`Infrastructure=False`, never the reason — a terminal condition presented as a slow
provision. `Workers=True` throughout, because the burst pool is `replicas: 0` and is
trivially satisfied.

**The load balancer never worked anyway.** It carried zero targets. kubeadm sets
`node.kubernetes.io/exclude-from-external-load-balancers` on control-plane nodes and
the CCM honours it by registering no backends — addendum 2 exactly, codified in
`spokepool-clusterclass-v1.yaml` and never mirrored into
`internal/assets/manifests/classes/hetzner-mgmt-ubuntu-v1.yaml`. On a hybrid hub the
control plane is the *only* Hetzner node, because home workers carry
`unmanaged://` providerIDs the CCM cannot target at all and logs as
`failed to convert provider id to server id`. So the CCM path cannot serve a hybrid
cluster even when the quota is free.

This is the third instance of one shape: a fix codified for the spoke, then the hub
grows the same property and does not inherit it (§21 tailscale, §22 CSI, now this).

**Decision.** A cloud load balancer is owned by the cluster lifecycle, not created as
a side effect of a Service.

1. Public ingress on the hub joins the CAPH-managed control-plane LB via
   `extraServices`, as spokes already do; ingress-nginx becomes a hostNetwork
   DaemonSet on the control plane with a `ClusterIP` Service. One LB per cluster,
   deleted by CAPH with the cluster.
2. The hub ClusterClass clears
   `node.kubernetes.io/exclude-from-external-load-balancers`, matching the spoke.
3. Any remaining `type: LoadBalancer` Service **must** set
   `load-balancer.hetzner.cloud/name` containing the cluster name, or teardown
   cannot reap it by name.
4. Teardown deletes LoadBalancer Services through the cluster API **first** and
   waits for the CCM to deprovision — the Service's `load-balancer-cleanup`
   finalizer makes the Service disappearing the signal that the cloud resource is
   gone. Deleting the cloud resource while its Service still exists only makes the
   CCM recreate it.
5. Whatever the CCM does not remove within the bounded wait is reaped by the public
   IP recorded during the drain. That match relies on no naming convention, so it
   also covers a `type: LoadBalancer` Service authored by a fleet, which ADR-047
   Tier 3 permits and which would otherwise leak identically.

**Codified (2026-08-23):** items 4 and 5 in
`internal/hub-cli/teardown/orchestrator.go` — `drainLoadBalancerServices` runs before
finalizer stripping and feeds drained IPs to the sweep in `deleteHetznerResources`;
kubeconfig resolution is shared via `resolveKubectlBaseArgs` so the two steps cannot
target different clusters.

> **§25 (2026-08-23) supersedes item 1 and narrows item 2.** Item 1 is
> unsatisfiable as written: `controlPlaneEndpoint.port: 443` already occupies the
> LB listen port that ingress needs, and on a hybrid hub nothing the CAPH LB can
> target is listening on 80/443. Item 2 only affects CCM-managed load balancers.
> See §25.3 for the replacement decision.

**Pending:** items 1–3, plus `preflight/15-hetzner-capacity.sh` (fails on leaked
zero-target CCM load balancers and on insufficient quota headroom before a run
starts) and the ADR-005 readiness gap below. Items 1 and 2 edit
`KubeadmControlPlaneTemplate.spec.template.spec`, which CAPI treats as immutable once
referenced, so per §17.3 they land with a hub reprovision rather than a version
rotation.

**Related — the XR reported Ready throughout.** `SpokePool` held
`Ready=True / Available` for the whole five hours. The `capi-cluster` `Object` in both
SpokePool compositions declares no `spec.readiness`, so provider-kubernetes applies
its `SuccessfulCreate` default: ready means *the manifest applied*. ADR-005 lists this
as a known negative and ADR-008 §6 requires the composition to close it.
`readiness.policy: DeriveFromObject` makes the XR reflect the Cluster's own `Ready`
condition. Until that lands, no gate may treat SpokePool readiness as evidence that a
spoke exists.

### 24. Cilium's API endpoint is lifecycle-injected; the spoke's home worker is a bootstrap gate (2026-08-23)

Two defects, found together on the first cold boot of a spoke since §17.3, and
sharing one consequence: `spoke-pool-hybrid-dev-01` reached `Provisioned` with no
CNI, no worker, no CRDs and no workloads.

#### 24.1 The kube-proxy-free deadlock

Addendum 1 added, to the shared ClusterClass `postKubeadmCommands`, deletion of the
kube-proxy DaemonSet and ConfigMap immediately after `kubeadm init` — correctly, since
kube-proxy's REJECT chains for endpoint-less services break Gateway API under
Cilium's full kube-proxy-replacement. It shipped without its required companion.

Cilium was configured `kube-proxy-replacement: "true"` with **no `k8s-service-host`**,
so it discovered the API server through the in-cluster ClusterIP — the address
kube-proxy used to route and that Cilium itself had not yet programmed:

```
cilium-<pod>  Init:CrashLoopBackOff  (init container "config")
  Unable to contact k8s api-server
  Get "https://10.96.0.1:443/api/v1/namespaces/kube-system": dial tcp 10.96.0.1:443: i/o timeout
```

Deadlock: Cilium cannot start until it reaches the API, and the API's ClusterIP is
not routable until Cilium starts. The node stayed `NotReady` with
`cni plugin not initialized`, so argocd-agent and every other workload sat Pending,
the spoke-catalog never synced, and all seven "CRD MISSING" and every `0/0 available`
finding in post-bootstrap validation were downstream of this one cause. The Hetzner
CCM crash-looped on the same address.

**Why it stayed latent.** `k8sServiceHost` has never existed in the live `hybrid`,
`hetzner`, `_shared` or `spoke-bootstrap` cilium configs (`git log -S` over all
history); it existed only in the abandoned `hybrid-flatcar` scaffold deleted in
`67df5a2b`, which had it set correctly. The hub never hit the deadlock because its
ClusterClass does not delete kube-proxy — it still runs kube-proxy today with the
same empty `k8s-service-host`, and works *because* of it. And the spoke's kube-proxy
deletion was introduced by rotating the control-plane template on a **running** spoke,
where Cilium was already up and deleting kube-proxy is harmless. §17.3 then collapsed
the version chain and made spokes reprovision-only. The first cold boot after that is
where it fired. The fix was validated in the one state in which it could not fail.

**Decision — the endpoint is cluster-lifecycle-injected configuration (ADR-048).**
This is the same shape as ADR-048's ClusterIssuer: a value that must be *inline* in a
config object, is per-spoke, and is unknown at composition time. ADR-048's rejected
candidate 2 — "imperative live patches … a controller mutating GitOps-owned
resources" — also rejects the obvious workaround of patching `cilium-config` in place
with a systemd unit, and it is rejected here for the same reason.

**Exactly one owner of the delivered object**, achieved as ADR-048 achieved it: by
deletion, not coordination.

| Half | Owner | Where |
|---|---|---|
| Settings (164 keys) | Git / ArgoCD | `manifests/providers/<provider>/k8s/cilium-config-base.yaml` |
| `k8s-service-host` / `k8s-service-port` | hub-operator | rendered from `Cluster.spec.controlPlaneEndpoint` |
| The `cilium-config` a spoke receives | **hub-operator, alone** | `{spoke}-cilium-config` CRS wrapper |

The addon Secrets no longer carry a `cilium-config` document at all, so nothing else
can write the object on a spoke. hub-operator is the renderer because ADR-043 assigns
**Spoke Lifecycle → Hub Operator**, and it already implements this exact wrapper
pattern twice (`ensureBootstrapCertCRSWrapper`, `ensureBootstrapCACRSWrapper`).
ADR-048 rejected extending the identity operator into a general configuration
controller, and the control-plane endpoint is not identity material.

**Fail closed.** While `controlPlaneEndpoint` is unset, or the base is missing, no
payload is published. A wrapper carrying an empty host would be applied by CRS and
reproduce the deadlock it exists to prevent.

**The ordering invariant the whole cold-boot fix rests on.** CAPH populates
`controlPlaneEndpoint` when it creates the load balancer, which happens **before any
machine is provisioned** — observed directly during the §23 incident, where the
HetznerCluster sat at `LoadBalancerCreateFailed` with an empty endpoint and no Machine
existed until the LB was created. The renderer therefore depends on nothing that
requires a booted node. This is not merely documented: it is asserted by
`TestCiliumConfigWrapper_EndpointPrecedesNodeBoot`, and
`preflight/25-cilium-apiserver-endpoint.sh` fails if the load balancer — the
endpoint's only source — is ever defaulted off.

**The ConfigMap alone is not sufficient — the env var is the other half.** The first
implementation of this decision populated only `cilium-config`, and the deadlock did
not lift: with the correct endpoint delivered to the spoke, the agent was still
dialling `10.96.0.1`. The reason is circular by construction — the `config` init
container's *job* is to read `cilium-config` from the API, so it cannot use a value
inside that ConfigMap to find the API. It uses `KUBERNETES_SERVICE_HOST`, which
kubelet defaults to the ClusterIP. Upstream's chart injects that env var onto the
containers when `k8sServiceHost` is set; populating only the ConfigMap reproduces the
original failure with a correct-looking config in place, which is strictly harder to
diagnose.

The endpoint therefore reaches the API-facing containers — `config`, `cilium-agent`,
`cilium-operator` — as `KUBERNETES_SERVICE_HOST`/`_PORT` sourced by
`configMapKeyRef` from `cilium-config`. That preserves single ownership rather than
weakening it: the static DaemonSet names a **key**, the rendered ConfigMap supplies
the **value**, and kubelet resolves the reference at pod creation over its own
kubeconfig — no circularity, because kubelet already knows the real endpoint.

`optional: true` is load-bearing. The hub's base carries no such key, so the
reference resolves to nothing and the hub keeps using the ClusterIP — correct there,
because the hub retains kube-proxy. The other init containers (`mount-cgroup`,
`mount-bpf-fs`, `clean-cilium-state`, `install-cni-binaries`) only touch the host and
are deliberately left alone.

**The hub is not a spoke.** It has no SpokePool and no renderer, so its Day-0 CNI
install rejoins the two halves by path (`provider_cloud.go`) rather than keeping a
second copy of 160 settings that would drift. ~~It keeps kube-proxy, so it needs no
injection.~~ **Withdrawn by §24.6** (2026-10-29): the hub runs kube-proxy replacement
like every other box, and the hub deadlocked exactly as this section describes.

#### 24.3 cilium-operator cannot fit on a single-node spoke

Found while watching 24.1 converge: with the endpoint fix in place the agent got
past its init containers and then stalled on

```
Still waiting for Cilium Operator to register the following CRDs: [ciliumnodes.cilium.io ...]
```

`cilium-operator` declares `hostPort: 9963` with `hostNetwork: true` and ran
`replicas: 2`. The spoke has one node, so:

```
Unschedulable: 0/1 nodes are available: 1 node(s) didn't have free ports
```

A hybrid spoke is single-node **at boot by construction** — burst workers are
`replicas: 0` and home workers join only after the control plane is Ready — so this
closes a circle that cannot open on its own:

```
operator Unschedulable -> Cilium CRDs never registered
  -> cilium-agent never Ready -> Node never Ready
  -> the home-worker join never runs -> still one node
```

It is the same shape as 24.1 and as §21 and §22: `CiliumOperatorReplicas = 1` exists
and is applied in `driver_hybrid.go`, but only on the **hub** provisioning path — its
own comment says "on a single-node *hub*". The spoke takes cilium from the CRS addon
Secret, which carried `replicas: 2` verbatim.

The manifest additionally carried a comment asserting the single-node case was
"scoped per environment in the spoke-catalog overlay". No such overlay existed, and
one could not have worked: cilium is CRS-delivered, so an ArgoCD overlay patching it
would be a second owner of a CRS-delivered object, which the ADR-048 ownership
boundary forbids. The claim survived review because it reads like a decision.

**Decision.** The hybrid addon sets `replicas: 1`. This applies to every hybrid
environment, not only dev, because every hybrid spoke is single-node during early
boot. The operator is not in the datapath — agents keep forwarding while it restarts
— so the cost is operator-restart latency, not connectivity. The hetzner spoke addon
keeps 2 and is unaffected: it boots with a worker MachineDeployment at `replicas: 1`,
so it has two nodes.

**Scaling to 1 is not sufficient on its own.** Applying it live left the operator
still Pending: the Deployment's percentage rollout defaults resolve, at `replicas: 1`,
to `maxSurge=1` (25% of 1 rounds UP) and `maxUnavailable=0` (50% of 1 rounds DOWN),
so Kubernetes keeps the old pod until the new one is Ready — and the old pod holds
hostPort 9963, so the new one can never schedule. The rollout deadlocks.

That is addendum 10's finding on a Deployment rather than a DaemonSet: two
generations competing for one host socket. There it was resolved by forbidding surge
(`maxSurge` would "deadlock the release gate — the old pod holds the sockets until
the new pod is ready"); the Deployment equivalent is `strategy.type: Recreate`.
Operator downtime during the swap is acceptable — it is not in the datapath.

`preflight/25` asserts both the replica bound and the rollout strategy, and only when
a hostPort is actually declared — if upstream drops the hostPort, more replicas
become legitimate and the check must not block that.

#### 24.2 The spoke's home worker was never provisioned

The hub joins its home-lab worker in the orchestrator's `home-worker-join` phase
(§21). The spoke had no equivalent: hub-operator minted the join token into
`{spoke}-home-worker-join` and nothing ever consumed it. The spoke therefore ran one
node — the tainted control plane — and could not satisfy ADR-014 placement for any
stateful workload even once the CNI worked.

It cannot live in the Go orchestrator: that owns hub creation and exits before the
spoke exists. The spoke is provisioned by Crossplane after boundary sync, so the only
stage that observes a provisioned spoke is Step 10 of `hub-bootstrap.sh`.

**Decision.** Step 10e becomes the spoke's mirror of the hub's phase: wait for the
spoke control plane to be Ready (which now requires 24.1), skip if a Ready node
already carries the placement contract, otherwise run
`provision-flatcar-worker.sh --cluster spoke`, then gate on the Node actually
becoming Ready.

**Readiness is measured on Nodes, never on CAPI Machines.** A home worker has no
Machine — CAPI did not create it — so a Machine count is structurally blind to the
thing being gated. The predicate requires a Node that is `Ready` **and** carries both
`node-role.kubernetes.io/worker` and `workload-location=on-prem`, which is the ADR-014 +
§11 contract the workloads are scheduled against.

**Fabricated success removed.** The previous Step 10e counted Machines, warned either
way, and then printed unconditionally:

```
📊 SpokePool Status: Ready=True, Synced=True
🔐 Certificate Distribution: All 3 certificates ready and synced
🏗️  Spoke Cluster: InfrastructureReady=True, ControlPlaneReady=True, WorkersReady=True
```

All four lines were hardcoded and printed while none of it was true. A bootstrap must
never assert a condition it did not observe; this is the same class as the §23
readiness lie and as the 34-minute poll of a terminal error.

#### 24.4 Worker convergence is live state, not a checkpoint (2026-08-23)

The first implementation of 24.2 placed the Step 10e gate *inside*
`step10_wait_spokepool()`, which begins:

```bash
if is_step_completed "wait_spokepool"; then
    log "Step 10: SpokePool already ready, skipping"
    return
fi
```

On the resumed run that skip fired, so the worker gate never executed, the spoke's
home worker was never provisioned, and the bootstrap declared success anyway. The
fix reintroduced the failure mode it was written to remove.

The error is treating worker convergence as a fact that can be *completed*. "We once
waited for the SpokePool" says nothing about whether a node is serving now. A home
worker is an unmanaged Hyper-V VM on a workstation: it can stop, and its host can
sleep. The hub's own `home-worker-join` phase already models this correctly — it
re-checks `readyHubWorker` on every run rather than trusting a phase marker.

**Rule.** A checkpoint may record that an *irreversible* step was performed. It must
not stand in for a *condition that can regress*. The SpokePool wait stays
checkpointed; the worker gate is invoked from `main()`, outside the skip, and
evaluates live Node state on every run.

The same run also demonstrated why: the **hub's** home worker stopped posting node
status 34 seconds before the bootstrap started. 53 pods went Terminating and 48
Pending across argocd, crossplane, kyverno, cnpg, ory, cert-manager and hub-operator
— and the run still printed "Hub cluster: Ready and operational", because the
terminal banner asserted four conditions it never measured. The banner now reports
only which gates ran, and `cluster/65-hub-placement-capacity.sh` asserts the hub has
a Ready, schedulable node to place its platform on.

#### 24.5 Home-lab hosts are thermally limited, and that is a platform concern (2026-08-23)

Four host shutdowns in one hour took the hub's only worker down mid-bootstrap and,
separately, killed a spoke provisioning run 23 seconds after it started. Every
Kubernetes-side symptom — 53 pods Terminating, 48 Pending, every hub Deployment at
`0/N` — was downstream of the host powering off.

**The cause was misread twice before it was found.** The event log showed `1074`
("initiated by NT AUTHORITY\LOCAL SERVICE"), which reads as a clean administrative
shutdown, and the box is a laptop whose battery reports 0 mWh while on AC — so the
first conclusion was Windows enacting its critical-battery action, and
`BATACTIONCRIT` was set to None. The shutdowns continued. The actual cause was one
layer down, in a correlated event at the same second:

```
Kernel-Power 86 (Error): "The system was shut down due to a critical thermal event."
  ACPI Thermal Zone = Intel(R) Dynamic Platform Thermal Framework
  _CRT = 373K                                    (99.85 °C)
```

`esifsvc` — Intel's thermal framework — enacts the trip *via* `shutdown.exe`, which
is why it appears as an administrative shutdown by a service account. The battery was
a real fault but an unrelated one.

**Why it overheats.** A Dell Latitude E7470 (15 W i5-6300U, 2 cores / 4 threads) was
running two Kubernetes node VMs, and the provisioner sized each VM's vCPUs as
`NumberOfLogicalProcessors` with no awareness that another VM would claim the same
threads — 8 vCPUs on 4 threads, uncapped. The trips cluster exactly around bootstrap
and provisioning activity.

**Decision — host thermal state is provisioner-owned, not operator-owned.** Anything
applied by hand to a home-lab host is lost on the next rebuild, so the settings live
in `phase_prep_hyperv`, beside the lid/sleep settings that were already there:

- `PROCTHROTTLEMAX` = `HOST_CPU_MAX_PCT` (default 70, `--host-cpu-max` to override).
  Slower bootstraps are strictly better than a shutdown mid-provision.
- turbo (`PERFBOOSTMODE`) disabled — the spikes come from there and it buys little
  under container load. Hidden by default on OEM images, so it is un-hidden first.
- `SYSCOOLPOL` = Active: ramp the fan *before* throttling. Passive throttles first
  and lets heat accumulate, which is how the critical trip is reached.
- `BATACTIONCRIT` = None, carried forward. It was not the cause, but a dead battery
  on a laptop-as-server is a real hazard and the setting is correct regardless.
- The phase reports any prior Kernel-Power 86 events on the host, so a thermally
  marginal box announces itself at provision time instead of during a bootstrap.

**vCPU allocation now divides the host rather than replicating it:** threads are
split across the VMs that will share the box (existing VMs + this one, floor of 2).
Two nodes on a 4-thread part get 2 vCPUs each — 1:1 instead of 2:1.

**Limits of the software fix, stated plainly.** Capping CPU reduces the trip rate; it
does not repair cooling. A nine-year-old ultrabook running two Kubernetes nodes is
thermally marginal by construction, and the durable remedies are physical — clean the
fan, repaste, improve airflow — or a host with real thermal headroom. The provisioner
now surfaces the evidence rather than leaving it to be rediscovered from a cluster
that merely looks broken.

#### Codified

- `operators/hub-operator/internal/controller/spokepool_controller.go` —
  `ensureCiliumConfigCRSWrapper`, called from `Reconcile`; RBAC for
  `configmaps` and `cluster.x-k8s.io/clusters`.
- `manifests/providers/{hybrid,hetzner}/k8s/cilium-config-base.yaml` — the static
  half, wired into each provider kustomization.
- `manifests/providers/hybrid/k8s/cilium-addon-hybrid.yaml`,
  `manifests/spoke/spoke-bootstrap/cilium-addon-template.yaml` — `cilium-config`
  document removed; header records why.
- Both SpokePool compositions register `{spoke}-cilium-config` as CRS resource
  index 18; `validate-spokepool-compositions.sh` asserts the slot. (The index
  comment table in the hybrid composition was stale for indices 14–17 and is
  corrected — an off-by-one there mis-delivers a payload silently.)
- `internal/hub-cli/bootstrap/provider_cloud.go` — hub Day-0 recomposition.
- `scripts/validate/preflight/25-cilium-apiserver-endpoint.sh` — twelve invariants,
  including single ownership, the configMapKeyRef env wiring on both addons,
  the single-node operator replica bound, renderer presence, and the LB source.
- `scripts/hub-bootstrap.sh` — Step 10e rewritten; `dump_cluster_diagnostics`.
- `operators/hub-operator/internal/controller/cilium_config_wrapper_test.go` —
  12 regression tests.

#### 24.6 The hub's API endpoint is injected too (2026-10-29)

**Status:** Accepted, implemented. It amends §24.1 and the Day-0 recomposition it
codifies.

##### What §24.1 got wrong

§24.1 says that the hub "keeps kube-proxy, so it needs no injection". That premise is
false, and the rest of this ADR already says so. The provider base that the hub
installs verbatim sets `kube-proxy-replacement: 'true'`. The NodeLocal DNS section
treats KPR as an invariant on every box: the socket load balancer acts before a
packet exists, and kube-proxy is never consulted. The hub runs no kube-proxy
DaemonSet. So the hub's agents reach the API server through `10.96.0.1`, which is
the state §24.1 calls a deadlock on spokes.

Evidence, 2026-10-28, the 0.1.16-rc.177 sync (full timeline in
`docs/Administer/Troubleshooting/Cilium/Repair/01-API-server-service-has-no-backend.md`):

1. The sync overloaded the 7.6 GiB control-plane node.
2. The kernel's OOM killer took Argo CD's application controller, and the API
   server exited 137.
3. Cilium synced the empty backend set for `10.96.0.1` and could not learn that the
   backend was back.
4. Every controller using the Service failed with `connect: operation not
   permitted` until an operator patched `cilium-config` with `k8s-service-host` /
   `k8s-service-port` and restarted the agents.

The cluster did not recover by itself, which is the failure §24.1 exists to prevent.

The checks did not catch it because they encode the false premise. Check 6 of
`scripts/validate/preflight/046-hybrid-provider.sh` passes the hub because "it keeps
kube-proxy". The live check in `scripts/validate/cluster/046-hybrid-provider.sh` asserts
the endpoint on the spoke only.

##### Decision

1. **Every box running KPR has its endpoint injected, the hub included.** The
   exemption in §24.1 is withdrawn.
2. **The owner of the hub's copy is the kakt CLI, at Day-0, and only the CLI.** It
   renders the hub's `cilium-config` from the same base with the hub
   `Cluster.spec.controlPlaneEndpoint` as `k8s-service-host` / `k8s-service-port`.
   This follows §24.1's ordering invariant: CAPH sets the endpoint before any
   machine boots, and the first agent on the hub starts with it. The base stays
   endpoint-free (check 5 still holds). The endpoint is per-cluster, and each
   cluster has exactly one renderer: hub-operator for spokes, the CLI for the hub.
3. **Fail closed.** If the endpoint is not set when the CLI renders, provisioning
   stops with that reason. The CLI never renders a hub `cilium-config` without it,
   never falls back to the Service path, and never patches afterwards. A post-boot
   patch is the imperative path that ADR-048 rejected (candidate 2).
4. **hub-operator does not own the hub's copy.** It runs on the hub, so it would
   start only after the agents it would configure. It would also be a second owner
   over the cluster's life, beside the Day-0 render. This option is rejected.
5. **The existing hub converges on the owner, not on the patch.** The live
   `k8s-service-host: 65.109.41.89`, `k8s-service-port: 6443` was set by an
   operator during the repair, and nothing owns it. The implementation must:
   - bring the hub's delivered `cilium-config` (the Day-0 resource that installs
     it) to the value the owner renders for this hub;
   - show that the rendered value equals the live one, so convergence changes
     nothing on the datapath.

   The live value already equals what the renderer writes, so this is a change to
   the Day-0 path, not to a running datapath.
6. **The checks state the rule, not the exemption.**
   - A preflight check asserts that the hub Day-0 render injects the endpoint, that
     it runs after the Cluster, and that it reads the Cluster's own field.
   - The live check asserts, on the hub as on the spoke, that `cilium-config`
     carries `Cluster.spec.controlPlaneEndpoint` whenever
     `kube-proxy-replacement` is `true`.

##### Consequences

- The `KUBERNETES_SERVICE_HOST` / `KUBERNETES_SERVICE_PORT` env wiring stays
  `optional: true`. §24.1 called that "load-bearing" because the hub's base had no
  such keys. Under this amendment, every cluster's rendered copy has them. The flag
  is kept so that an agent with a missing key fails loudly on the Service path,
  instead of failing to start with `CreateContainerConfigError`. Whether to make
  the keys required is for review.
- The comment in `internal/kakt-cli/bootstrap/provider_cloud.go` that repeats the
  "keeps kube-proxy" premise is corrected with the code.
- Separately, Argo CD's application controller now has a memory request and limit
  (`manifests/argocd/components/01/platform-argocd.yaml`). A sync-time spike is
  then contained in that container rather than taking the API server beside it.
  That change reduces how often the trigger occurs; this amendment removes the
  deadlock that made it permanent.

##### Resolved on implementation

- **The render point.** The CNI addon is now applied *after* the Cluster, not before
  it, and the endpoint is read from `Cluster.spec.controlPlaneEndpoint` in between.
  The ClusterResourceSet delivers its payload later still — once the workload cluster
  answers — so the Secret is in place before the first agent reads it. The CLI waits
  up to five minutes for the field and stops with that reason if it never appears.
- **The hybrid hub.** No separate source. A hybrid hub's control plane is Hetzner CAPI
  infrastructure like a cloud hub's, and the endpoint is read from the Cluster, which
  every provider populates. Only the spoke side of hybrid uses a MagicDNS name, and
  that path is hub-operator's.
- **`optional: true` stays.** A rendered copy that is somehow missing a key then
  leaves the agent dialling the ClusterIP and failing loudly, instead of refusing to
  start with `CreateContainerConfigError` and no diagnosis.

##### Codified

- `internal/kakt-cli/cluster/provisioner.go` — `Provision` applies the CNI addon
  after the Cluster; `controlPlaneEndpoint` reads and waits for the field, with no
  fallback; `injectAPIServerEndpoint` writes the two keys into the cilium-config
  document, refusing a stream with no such ConfigMap, with two of them, or with the
  keys already pinned.
- `internal/kakt-cli/bootstrap/provider_cloud.go` — the "keeps kube-proxy" premise
  is removed from the Day-0 comment and the owner recorded.
- `scripts/validate/preflight/046-hybrid-provider.sh` — check 7: the render injects
  the endpoint, it runs after the Cluster, and it reads the Cluster's field.
- `scripts/validate/cluster/046-hybrid-provider.sh` —
  `validate_hub_cilium_apiserver_endpoint`: on the hub, `kube-proxy-replacement: true`
  requires the two keys, and they must equal the hub Cluster's own endpoint.
- `docs/Administer/Troubleshooting/Cilium/Repair/01-API-server-service-has-no-backend.md`
  — prevention updated to what ships.

##### Still open

- **The live hub.** Its `k8s-service-host` was patched by an operator during the
  repair and matches what this renderer would write (`65.109.41.89:6443`), so the
  live datapath is already correct and the live check above passes. The Day-0 path
  is what changed; the existing hub keeps the patched value until it is rebuilt.
- **The sync spike** that triggered the outage is a separate change: an earlier CRD
  wave, or only the CRDs in use.
