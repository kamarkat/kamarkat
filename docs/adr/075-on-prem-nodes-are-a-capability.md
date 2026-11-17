# ADR-075: On-Prem Nodes are a Capability, Not a Provider

**Date:** 2026-09-10

**Status:** Proposed

## Context

ADR-046 introduced the hybrid provider: a box whose default workers are on-prem machines joining over Tailscale, with both control planes on the tailnet because a on-prem node's only `InternalIP` is its tailnet address and Cilium derives its VXLAN endpoint from it (§21, invariant 6).

"Hybrid" became the name for that arrangement, and `--provider` became the switch. Five facts bear on whether it should have.

**`provider` names four decisions at once.** It selects the spoke-pool directory, the Crossplane composition, the Cilium addon, and whether on-prem nodes exist. Those have different lifetimes: the first three are infrastructure the platform maintains, the fourth is something a tenant chooses and may change.

**The choice is currently made once and cannot be revised.** A box bootstrapped as `hetzner` has no supported path to gaining a on-prem node. Nothing about the arrangement is inherently permanent -- it is permanent because the decision was encoded as an identity rather than as a setting.

**The joining machinery is already installed on every box, switched off.** `driver_hetzner.go` writes the `tailscale-hybrid-psk` Secret with empty values on every hetzner cluster, because an unresolvable `contentFrom.secret` blocks KubeadmConfig rendering entirely. Every Tailscale step in the ClusterClass is guarded on `[ -s /etc/tailscale-hostname ]` and is a no-op until that Secret holds something. The capability is present and inert.

**Two keys gate on-prem nodes; twelve others differ for unrelated reasons.** Both providers now run `routing-mode: tunnel`, `tunnel-protocol: vxlan` and `auto-direct-node-routes: false` -- the hybrid addon's header still calls the routing mode its first deliberate delta, which has not been true for some time.

What gates an on-prem node is `mtu` (1230 against 1200) and `devices` (runtime detection against `eth+ enp+ tailscale0`). Those two decide whether a node reachable only over the tailnet can carry pod traffic.

The remaining twelve differences are a second axis that was decided at the same time and written into the same file: `gateway-api-hostnetwork-enabled`, `external-envoy-proxy`, `envoy-xds-mode`, `envoy-keep-cap-netbindservice`, `disable-envoy-version-check`, `node-port-mode`, `node-port-bind-protection`, `enable-auto-protect-node-port-range`, `bpf-lb-sock`, `tunnel-port`, `proxy-use-original-source-address`, `direct-routing-device` and `enable-source-ip-verification`. Most belong to hostNetwork Gateway mode with decoupled Envoy (ADR-046 addenda 10 and 15), which is about how a hub binds `:80/:443` on its control-plane host. None of them is required by an on-prem node.

**Neither differing value is a tuned one.** 1230 is Cilium's stock VXLAN MTU, and ADR-046 §6 records what it produces: a 1280-byte outer packet, exactly `tailscale0`'s MTU, with zero headroom. 1200 was chosen to give 1250 and fit strictly inside. A box at 1230 is not optimised for Hetzner; it is on a default that happens not to have met a tailnet yet. `devices` listing `tailscale0` on a machine that has no such interface matches nothing and costs nothing.

## Decision

**On-prem nodes are a capability a tenant may enable at any point in a cluster's life. The platform runs one datapath, sized for the smallest path any node may ever take.**

### Two settings are platform-wide; the rest of the difference is not unified

Every box runs `mtu: 1200` and `devices: eth+ enp+ tailscale0`, whether or not it has an on-prem node today.

Those two are the whole of what makes the capability available later. They are the only settings that cannot be changed on a live cluster without restarting every Cilium agent and interrupting pod traffic; fixing them at the value that works everywhere removes the one irreversible part of the decision. Everything else the capability needs -- the Secret, the ClusterClass hook, the join -- is already dynamic.

`devices` listing `tailscale0` on a machine with no such interface matches nothing and costs nothing. The MTU costs thirty bytes of payload per packet, about 2.4%, on a box that never gains an on-prem node. It is accepted, and it is the price of the decision being revisable: a platform that saved those thirty bytes would be one where choosing Hetzner on day one silently forecloses on-prem nodes on day four hundred, and the tenant would not learn that until they asked.

**The other twelve differences are deliberately left alone.** Unifying them would give every box hostNetwork Gateway mode, external Envoy, NodePort protections disabled and `enable-source-ip-verification: false` -- a security posture change arriving as a side effect of a capability toggle. That axis is a separate decision, and this one does not make it.

### Enabling is a declaration, and it is reversible

A tenant enables the capability by editing the cluster's values file:

```yaml
onPrem:
  enabled: true
  tailnet: acme.ts.net
```

Enabling populates the Tailscale Secret and puts the control plane on the tailnet. Disabling empties it. Neither changes the datapath, because the datapath does not depend on the setting.

Selection is a value and never a version (ADR-063), so adopting this capability and upgrading a bundle stay independent acts.

### `provider` returns to naming one thing

`provider` selects infrastructure: where control-plane nodes are created, which composition provisions them, which credentials reach the cloud API. It stops implying anything about on-prem nodes.

`hybrid` is therefore withdrawn as a provider. A box that ran as `hybrid` is a box whose provider is `hetzner` and whose `onPrem` capability is enabled, which is what it always was.

### The capability is answered by the box, not by the provider

Whether a box accepts on-prem nodes is asked of the provider in two places: the
pre-flight capacity check, and the phase that joins a node. Both were answered only
by the hybrid driver, so `--provider hetzner --on-prem` set a flag nothing read.

The consequences were quiet and compounding. A hetzner box reported the capability
enabled, staged its tailnet credentials **empty**, joined no nodes, and -- once
development boxes stopped provisioning cloud workers -- was refused for having no
capacity while the flag that would have given it some was set. Every part behaved
correctly on its own; the box was simply never asked a question it could answer.

So the capability, the tailnet credential staging and the control plane's tailnet
name are properties of any box, implemented once and shared. The hybrid driver
delegates to them rather than carrying its own copy, which is what let the two
paths differ in the first place.

This is the last thing that made the exit condition below unreachable rather than
merely untested.

### `hybrid` is retained in code until the arrangement it names is proven elsewhere

The withdrawal above is the decision. It is **not** yet the state of the tree, and the gap is deliberate rather than pending cleanup.

`hybrid` is the only provider on which on-prem nodes have ever run. Deleting it before `hetzner` with `onPrem.enabled` has provisioned a cluster and carried pod traffic to a node on someone's premises would remove the only working configuration in favour of one that has never been exercised -- and would do so at the moment there is nothing left to compare against when it fails.

So both exist for now. `hetzner` gains the capability; `hybrid` keeps working and unchanged.

**The exit condition is a hetzner box with `onPrem.enabled` that has provisioned a control plane, admitted a node on the tenant's own premises, and passed pod traffic across that boundary in both directions.** When that has happened, `hybrid` is removed: the provider entry, `manifests/providers/hybrid/`, its Crossplane composition, its spoke-pool directories, and its entries in the support matrix.

Naming the condition is the point. A transitional state with no stated end is indistinguishable from a decision nobody made, and this repository already carries one of those -- a chart default that outlived the ADR justifying it by four released versions, because the comment beside it still cited the superseded decision as live.

### Joining a node is a separate act from enabling the capability

Enabling says the cluster will accept on-prem nodes. It does not create one. A
machine joins by a plain `kubeadm join` against a bootstrap token, outside CAPI --
no Machine, no MachineDeployment, and a `providerID` of `unmanaged://<name>`.

That it is unmanaged is what makes joining *later* work at all. The hub's token is
minted from the hub's own kubeconfig and carries no expiry, so a node may join
minutes or months after Day-0 and depends on nothing the bootstrap installed. It is
also why the platform cannot roll, upgrade or remediate these nodes: CAPI did not
create them, so readiness is measured on Nodes and never on Machines.

Two consequences follow and are accepted:

**The cloud controller cannot see them.** An `unmanaged://` providerID is one the
Hetzner CCM refuses -- `failed to convert provider id to server id` -- so an
on-prem node can never be a backend for a CCM-managed LoadBalancer. Public entry
stays on cloud nodes regardless of where the workload behind it runs.

**A joined node carries capacity, not workloads.** It arrives labelled
`workload-location=home` and `node-role.kubernetes.io/worker=`, untainted and
schedulable, and nothing moves onto it. What a box runs where is the placement
class below, which is a second decision the tenant makes deliberately.

### Placement is a separate declaration, and moving a stateful service is a restore

The capability says a cluster may have on-prem nodes. `placement.class` says
whether its platform data runs on them:

```yaml
placement:
  class: hybrid        # the on-prem class; empty means this box's provider
```

A placement class is a pair -- a `workload-location` nodeSelector and the storage
class provisionable on nodes carrying it (ADR-046 §11) -- and it is selected as one
name because §11 forbids the halves drifting apart. Two values, one picking the
location and one the storage, would be that drift.

**Changing it is not a reschedule.** The on-prem class stores on `local-path`,
which is node-pinned to the exact machine, has no snapshot support, and is
destroyed when that machine is re-provisioned. A stateful service moving between
classes is therefore recreated and restored from object storage, not moved. That is
an operation with a runbook, not an edit that takes effect on the next reconcile,
and this ADR does not pretend otherwise.

Stateless workloads have no such constraint and may follow placement freely.

### Cloud worker capacity is a file in the tenant's repository, changed by hand

The machines a box runs on are recorded at `clusters/<name>/generated/hub-cluster.yaml`
-- the CAPI Cluster, written by Day-0 and reconciled by the box's own control plane
(ADR-045, ADR-072). Changing `replicas` there and committing is how cloud workers
are added and removed.

Before this, the topology existed only in the API server: rendered from Go at
Day-0, skipped entirely on any later run once the cluster reported `Provisioned`,
and reconciled by nothing. Changing capacity meant an out-of-band `kubectl edit`
that no file recorded and nothing would restore.

**Nothing scales it automatically, and that is the decision rather than a gap.**
The obvious alternative is cluster-autoscaler owning the field, which the spoke
already does. It is rejected here for two reasons. CAPI refuses a topology carrying
both `replicas` and the autoscaler's bounds annotations, and the refusal wedges the
object so that every later change to that cluster blocks behind it -- which is not
hypothetical, it is what commit `bac465a9` did to a spoke until it was reverted.
And a node that is unreachable for ninety seconds is not a node that has left: a
platform that provisioned cloud machines on that evidence would answer a flapping
tailnet with a bill.

So the response to losing on-prem capacity is a person deciding to replace it. The
sync policy makes that decision authoritative: `selfHeal` is on, so a `kubectl edit`
of the topology is reverted to what Git says and capacity has exactly one source.

`prune` is off, and only for this. The directory holds a CAPI Cluster, and pruning
one destroys every node it describes including the control plane -- so a deleted
file, a bad rebase or a renamed directory would take out the estate. What is given
up is that a generated artifact removed from Git lingers in the cluster, which is a
stale ConfigMap rather than an estate.

### Every box provisions cloud workers by default; using on-prem instead is asked for

A box provisions cloud workers in every environment, development included. Running
without them is available -- `--workers 0`, or the count in the cluster's values
file -- and it is what a tenant with their own hardware will want.

Development briefly defaulted to zero, on the reasoning that non-production
capacity is the clearest case for hardware someone already owns and that ADR-070
bounds what a box costs. That reasoning still holds; the default did not. It made
on-prem hardware a **precondition** for the simplest box the platform can build:
scaffolding with no arguments produced a repository whose bootstrap was refused for
having nowhere to schedule, and the first thing a new tenant would meet was a
refusal rather than a cluster.

So the preference is expressed by making the arrangement easy to ask for, not by
making its absence a failure. A tenant who wants it says so once, in a flag or a
values file, and the platform provisions nothing they did not ask for.

What survives from that attempt is the distinction it forced: an explicit zero and
an absent setting are not the same answer, and the code keeps them apart. Without
that, "use my own hardware" and "I have not said" would be one value, and a tenant
who had not decided would get the arrangement that needs hardware they may not own.

### Losing an on-prem node is a restore, not a failover

An on-prem node can leave at any time -- someone's power, someone's network, a
machine repurposed. What that costs depends on what was placed there.

**Stateless workloads reschedule.** A hetzner box keeps its cloud workers whether
or not the capability is enabled, so capacity to reschedule onto already exists.
Nothing is provisioned in response, because nothing needs to be.

**Stateful workloads must be restored.** The on-prem class stores on `local-path`,
which is node-local and destroyed with its node, and the hub database runs a single
instance. So losing that node loses the primary and its volume together, and the
object-store archive is the only remaining copy. The sequence is: return
`placement.class` to the cloud class, rebuild the database from the archive
(`manifests/hub-core-services/database-recovery`), and let it come up on the cloud
workers that were there all along.

**This bounds data loss; it does not eliminate it.** Recovery restores to the last
archived WAL, and whatever was written after that is gone. A claim of no data loss
would require a second instance on a second node so a replica can be promoted --
that is a topology decision under ADR-070, and this decision does not make it. What
this decision guarantees is that the loss is bounded and recoverable rather than
silent: the alternative, which is what the platform did before, was a replacement
database initialising EMPTY and reporting Healthy.

Neither step is automatic, and that is deliberate. A node that has gone missing for
ninety seconds is not a node that has left, and a platform that rebuilt a database
on that evidence would destroy more data than it saved.

### What this does not make instantaneous

**Nodes already joined do not gain a tailnet address.** `preKubeadmCommands` runs once, at join. Enabling the capability enrols every node that joins afterwards; existing nodes keep the addresses they registered with. Where that matters is the control plane: ADR-046 §21 requires the hub CP on the tailnet for cross-node pod traffic to on-prem nodes, so a box enabling this after its control plane exists must replace those nodes to complete the arrangement.

This is stated rather than solved. The capability is available at any time; on an existing cluster, making it *effective* costs a control-plane roll. That is a smaller and more predictable operation than a datapath change, and it is the residue this decision does not remove.

## Alternatives considered

**Reserve the datapath at scaffold time, behind a flag.** Rejected. It is this decision with a switch in front of it, and the switch is the problem: a tenant would have to predict at scaffold time whether they will ever want a on-prem node, which is precisely the prediction they cannot make. It also leaves two datapaths to test rather than one.

**Keep the per-provider datapath and change MTU when the capability is enabled.** Rejected. Lowering MTU on a live cluster restarts every agent and interrupts pod traffic, so the capability would be available at any time in name and be a maintenance window in practice. A capability whose use requires an outage is not one a tenant reaches for.

**Keep `hybrid` as a provider and add on-prem nodes to `hetzner` as well.** Rejected. It doubles the matrix -- four combinations where two decisions exist -- and leaves `provider` naming two things. The support matrix would grow entries nobody exercises, which ADR-063 rejects for the same reason it rejects publishing an artefact per environment.

**Unify the whole of the two provider bases, not just the two keys.** Rejected, having read the diff. Twelve further keys differ, and they carry hostNetwork Gateway mode, decoupled Envoy, NodePort protection settings and source-IP verification. Adopting them platform-wide to deliver an on-prem capability would change the security posture of every box as a side effect, which is not a trade this decision is entitled to make on a tenant's behalf.

**Author the missing `cilium-addon-hetzner.yaml`.** Deferred, not rejected. A released hetzner box cannot bootstrap today because that artefact has never existed, and this ADR does not conjure it: with the two keys unified the two addons are closer, but the remaining twelve differences mean they are still two. Producing it is a real piece of work and is tracked separately.

## Ownership

| Resource Class | System of Record | Lifecycle Owner | Reconciler | Consumer | Phase |
|---|---|---|---|---|---|
| Cilium datapath settings | `zero-ops` | Platform | hub-operator | Every node | Day-1+ |
| `onPrem` selection | `<tenant>-gitops` | Tenant | ArgoCD | Tailscale Secret | Day-1+ |
| `placement.class` selection | `<tenant>-gitops` | Tenant | ArgoCD | provider overlay | Day-1+ |
| Cloud worker count | `<tenant>-gitops` | Tenant | ArgoCD | CAPI | Day-1+ |
| Tailnet auth key | tenant's secret store | Tenant | ESO | ClusterClass pre-kubeadm hook | Day-0 and after |
| On-prem node membership | the node itself | Tenant | — | kubeadm join | any |
| On-prem node lifecycle | — | Tenant | — | — | any |

The empty reconciler on the last row is the decision, not an omission: an on-prem
node is joined outside CAPI, so nothing on the platform's side rolls, upgrades or
remediates it. See ADR-039 for the complete ownership matrix.

## Consequences

### Positive

A tenant can add on-prem capacity to a box that was built without it, by editing a file. That was previously impossible and the impossibility was undocumented.

One datapath is tested rather than two, and the combination matrix loses a dimension.

The two provider addons move closer together, which makes authoring the missing `cilium-addon-hetzner.yaml` a smaller job than it was. It does not remove the need for it.

The Tailscale machinery that was already installed on every box becomes something a tenant can reach, rather than dead weight carried for a provider they did not choose.

### Negative

Every box pays about 2.4% of payload for a capability most will not use. The saving is available only by making the decision irreversible, which is the trade this ADR refuses.

Enabling the capability on an existing cluster requires replacing control-plane nodes before on-prem nodes can exchange pod traffic with them. The capability is available immediately; the arrangement is not.

Two providers describe one arrangement until the exit condition above is met, so the tree carries a redundant cell and the support matrix carries entries for it. That is a real cost of the staging, paid to keep a working configuration while its replacement is unproven.

A tenant can now enable a capability whose other half -- an actual machine on their own premises, running the join -- the platform cannot see or verify. A box can report the capability enabled and have nothing joined.

Replacing lost capacity is manual, so a box whose on-prem node dies at night stays degraded until someone edits a file. That is the cost of refusing to let a flapping network provision servers, and it is paid in recovery time.

Capacity and use are two declarations, so a box can also have on-prem nodes joined and idle: enabling the capability places nothing, and `placement.class` is a second, deliberate edit. That is the correct shape -- moving stateful data is a restore and must not happen because a node appeared -- but it means "I joined a machine and nothing changed" is a supported state rather than a fault.

A capability implemented once for every box is a capability whose failures are
shared. The tailnet staging that a hetzner box now uses is the one a hybrid box
depends on, so a defect in it reaches both -- which is the trade for the two paths
being unable to drift apart again.

Nodes the platform cannot roll are nodes the platform cannot patch. An unmanaged join buys the ability to add capacity at any time and gives up CAPI's remediation for it, so keeping those machines current is the tenant's, on hardware the platform never sees.

## Impact

- **Amends ADR-046.** `hybrid` is withdrawn as a provider in principle and retained in code until the exit condition above is met; the arrangement it named becomes `provider: hetzner` with the `onPrem` capability enabled. Two of the datapath settings it specifies per-provider become platform-wide -- `mtu` and `devices` -- and the rest do not. Its invariants are unchanged: invariant 6 still requires a tailnet address on every node carrying pod traffic, §21 still puts the control plane on the tailnet, and §11's placement classes are still pairs.
- **Amends ADR-066.** On-prem nodes are named as a selectable capability, and the boundary between cluster machinery and tenant selection is where this decision is drawn.
- **Confirms ADR-063.** Selection is a value, never a version.
- **Amends the support matrix.** `dev+hetzner` becomes supported, with a spoke-pool source created for it: a dev environment could not previously run on the provider this decision expects to survive, and the CLI's own scaffold defaults produced exactly that combination. `stg` remains hybrid-only until a source exists for it. The `hybrid` entries stay until the exit condition is met, and are withdrawn with the provider.
- **Confirms ADR-014.** Stateful workloads run on worker nodes and never on the control plane, in every class. An on-prem node is worker capacity by construction, and enabling this capability does not untaint a control plane. It is also why a box with no worker capacity stalls rather than degrading: a control plane carries no `node-role.kubernetes.io/worker` label for those workloads to select.
- **Confirms ADR-070.** Running a box on the tenant's own hardware is a cost decision available where the topology allows it, asked for rather than defaulted. No supported topology is weakened to reach a number.
- No change to ADR-072: the capability is declared in the tenant's repository and reconciled by the tenant's control plane, like everything else there.

## Amendment, 2026-11-17: the capability belongs to a managed cluster, in dev

**This ADR made on-prem a property of a BOX, and that is withdrawn. On-premises
nodes are capacity a MANAGED cluster may declare, in the dev environment only.
A management cluster takes none.**

The architecture this platform publishes has said so throughout: "On-premises
worker nodes · dev only -- your own machines, joining over your tailnet", drawn
inside the managed-clusters block. The decision above put the capability on the
hub instead, and nothing reconciled the two. The published shape wins, because it
is the one a tenant reads before they decide.

What the decision above got right stands: on-prem is a setting rather than an
identity, `mtu: 1200` and `devices: eth+ enp+ tailscale0` stay platform-wide so
the capability remains available without restarting agents, and `provider` is not
the switch for it. What changes is WHICH cluster may use it.

What this withdraws:

- `--on-prem` on a management cluster is refused (`cmd/kakt/bootstrap.go`), and a
  box whose declaration carries `onPrem.enabled` is refused where that
  declaration is read (`cmd/kakt/bootstrap_day0.go`), naming the file to edit.
- `on-prem-join` is no longer a phase of a hub pipeline that cannot have one. It
  used to run under every provider, return in 46 milliseconds having done
  nothing, and record ITSELF COMPLETE -- a record of this run's flags rather than
  of the cluster, which is what left a hybrid box resumed once as hetzner with a
  tainted control plane and no worker at all.
- The exit condition above -- "a hetzner box ... admitted a node on the tenant's
  own premises" -- is withdrawn with it. It was never met. The replacement
  condition is a dev managed cluster that has admitted one.

The hybrid cell is the one exception and keeps its hub-side join, because ADR-046
made home-lab nodes its hub's only worker capacity (`WorkerReplicas = 0`) and
refusing it would leave those boxes with nowhere to schedule. The cell is still
retired by this ADR's exit condition, now restated against managed clusters, and
the exception is withdrawn with the cell.

### Nothing is asked, and nothing is defaulted

**The machines a tenant lists are the declaration. There is no enable flag, on
either kind of cluster.**

A toggle beside a list is two declarations of one fact, and they drift: a pool
carrying `home-worker-enabled: true` and an empty list had ONE machine invented
for it, named `<spoke>-home-worker`, and `nutgraf-01` held a join token for that
imaginary machine while the second of the two it actually owns had none -- with
the Secret looking correctly minted throughout. A malformed list did the same
thing. Both are gone: an empty or unreadable list is no capacity, and an
unreadable one is an error rather than a default.

So the question is never put to anyone:

- A management cluster's declaration no longer carries an `onPrem` block. It
  admits no nodes on the tenant's own premises, so there was nothing to set.
- `--on-prem`, `--tailnet-name` and `--on-prem-join-ttl` are no longer offered by
  `bootstrap-mgmt`; they remain, hidden, for the hybrid cell that still joins
  home-lab nodes to its own hub.
- A managed cluster's SpokePool carries `home-workers` and `home-worker-ttl`, and
  no `home-worker-enabled`. The tailnet is read from the machines' own
  `tailnetHost` names rather than declared a second time.
- With no machines listed, a pool runs on `spec.nodePool.count` cloud workers --
  which is the ordinary case and reported as nothing at all, not as a failure.

### Where the capability lives now

A managed cluster declares it on its own SpokePool, in the tenant's repository:
`home-worker-enabled`, `home-workers` (the machines, by hostname and tailnet
host), `home-worker-ttl` and `tailnet-name`. hub-operator's
`reconcileHomeWorkerJoin` mints one kubeadm bootstrap token per declared machine,
and `scripts/hybrid/provision-flatcar-worker.sh --cluster spoke <cluster>` joins
them. That machinery was already there for the hybrid cell; what this amendment
changes is who may use it and where it is checked.

**It is gated on the environment, and no longer on the provider.** The join flow
asked `provider == "hybrid"` and the annotation, and never the environment -- so
"dev only" was drawn in the architecture and enforced nowhere, while a dev pool
on hetzner could not have a node at all. Both are now the other way round:
hub-operator mints tokens only for a pool whose `environment` label is `dev`, and
reports a pool that asks and is not, and the provider is not consulted.

Dropping the provider gate costs nothing this decision had not already paid for.
`mtu: 1200` and `devices: eth+ enp+ tailscale0` are platform-wide by the decision
above, every spoke's ClusterClass already carries the Tailscale pre-kubeadm hook
inert, and `manifests/providers/hetzner/k8s/tailscale-psk-es.yaml` now syncs the
tailnet credential into a hetzner cell as the hybrid cell's copy always did for
that one. The hetzner spoke template ships the four annotations, disabled.

Still outstanding, and an operation rather than a change: `nutgraf-hub` is
declared `provider: hybrid`, so its hub's only worker capacity is a home-lab
node. Moving that box to cloud workers -- and its hardware to `nutgraf-01`, which
already declares two home workers of its own -- is what retires the cell, the
hub-side join, `OnPremEnabled`, and the second Cilium version with it.

## References

- ADR-014: Platform-Owned Stateful Infrastructure
- ADR-039: Platform Ownership Model
- ADR-046: Hybrid Provider Cell (Hetzner Control Plane + On-Prem Workers)
- ADR-063: The Platform Bundle and its Version
- ADR-066: The Platform Boundary
- ADR-072: Tenant-Controlled Day-0 and Declarative Cluster Lifecycle
