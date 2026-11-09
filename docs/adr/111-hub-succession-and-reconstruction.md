# ADR-111: A hub is replaced, not upgraded

**Date:** 2026-11-02
**Status:** Proposed
**Relates to:** ADR-014 (platform-owned stateful infrastructure), ADR-076 (reaching a box you own),
ADR-082 (tenant repository layout and cluster naming), ADR-092 (recovery is exceptional),
ADR-100 (deprecated; secretbox remains the at-rest mechanism), ADR-105 (encryption of Kubernetes
Secrets at rest, Accepted), ADR-104 (no key material in node bootstrap material — **Proposed**,
not in force), ADR-003 §6 (ESO/Infisical, at-rest encryption), ADR-112 (each platform application
owns its PostgreSQL cluster), ADR-037 (environment promotion; its clause 2 is superseded by
Revision 7 below), ADR-046 §11 (placement classes), ADR-051 (environment DNS naming; its
Amendment 2026-09-18 decided the DNS half of Revision 7), ADR-114 (destroying a box: the
inverse of this ADR, and where ownership, the two teardown modes and what outlives a
cluster are decided)

*Revised 2026-11-02 (Revision 5): Class B is per application (ADR-112); Velero is Class M's
mechanism. Revision 6: Class M splits into credentials, resources and activation streams, and
activation is a one-way GitOps transition — the ACM cluster-backup pattern applied with our own
mechanisms. Sequencing, rehearsals and their evidence live in
`docs/plans/k8s-cnpg-upgrade-and-restore-drill.md`; this ADR sets no acceptance or release
condition. Revision 7 (2026-11-06): the hub is environment-independent -- management identity,
management secret scope, management availability and workload environment are four separate
concerns, and the hub declares no `environmentSlug`.*

## Context

The management cluster is the one thing on a box that has no upgrade path. Spokes are
replaced by name (ADR-046: a CAPI template's spec is effectively immutable, so a change
is a new template, never an edit). The platform itself is replaced wholesale on every
release, because the bundle renders rather than migrates. Only the hub is expected to
survive in place, and two things follow from that expectation:

* the Kubernetes and CloudNativePG versions it runs are both EOL, and the jump is not
  attempted because nobody will attempt it in place on the cluster holding the tenant's
  identity provider, its secret store and its ledger;
* the two most recent promotions failed *in* the promotion machinery, on a box with live
  state, which is the only place that class of failure can occur.

So the hub needs the same property everything else on the box already has: it must be
replaceable. This ADR records how a hub is recreated as a **fresh cluster on a newer
Kubernetes version**, inheriting the box's data and resuming management of the existing
spokes, in two cases that differ in what is available rather than in what is wanted:

1. **Planned succession** — the predecessor is operational.
2. **Disaster reconstruction** — the predecessor is permanently unavailable.

### The reference model, and where it stops being one

kubefirst's management cluster is reconstructible for one reason: it originates almost
nothing. Its state is a function of the GitOps repository, Terraform state and Vault.
`registry/clusters/<mgmt>` declares the management cluster's own components exactly as a
workload is declared; every upstream chart version is pinned in Git; bootstrap is
re-runnable against a pre-existing external state store. That discipline is worth
adopting and we already hold most of it.

Their design stops there because it can. Their `registry/environments/{development,staging,
production}` are namespaces on the one management cluster, so they have no second hub to
reconcile against, and their documentation set contains no upgrade page at all: the model
is "create a new cluster", and the question this ADR answers is one their architecture never
had to ask.

Our hub **originates** state no repository can derive — identity records, a financial
ledger, and the certificate authorities of every spoke control plane. So the principle is
adopted with one clause added:

> The hub is a projection, not a system of record. Anything the hub originates must be
> recoverable from outside it — otherwise the hub *is* a system of record, and a system of
> record cannot be recreated.

## Decision

### 1. Identity: the box outlives the cluster

`clusterName` carries two meanings today, which is why its comment in every registry file
has to say *never change it*: it is both the box's permanent identity (escrow path, DNS
record ownership, public hostnames) and this cluster incarnation's name (WAL archive
prefix, object and node names). Succession requires the second to change and the first to
stay, and one field cannot do both.

* **`boxId`** — stable for the life of the box. Keys the escrow path and DNS record
  ownership. Takes the current `clusterName` value, so nothing migrates.
* **`clusterName`** — per incarnation. Keys the WAL archive prefix and the cluster's own
  object and node names.

This removes "never change it" as a consequence of the model rather than as an exception to
it. It also closes a defect found in review: under a single shared path a successor would
overwrite the predecessor's own recovery credentials, at exactly the moment both are needed.
The escrow therefore has three scopes:

| Scope | Artifacts | Lifetime |
|---|---|---|
| Logical hub (`boxId`) | `infisical-master-keys`, `zitadel-masterkey`, DNS ownership id | permanent |
| Hub incarnation (`clusterName`) | `admin-kubeconfig`, `secret-encryption-key` (+ `-previous`), that incarnation's archive prefix | as long as that incarnation's recovery point matters |
| Spoke identity (spoke `clusterName`) | spoke PKI, spoke kubeconfig, spoke encryption key | outlives every hub incarnation |

The third scope is the structural one: spoke recovery material is keyed to the **spoke**,
never to whichever hub currently manages it. That is what makes management ownership
transferable at all.

### 2. The recovery-state contract

Every piece of hub state is assigned exactly one class. State belonging to no class is a
latent disaster: it behaves identically until the day the hub is gone.

| Class | Content | Recovery authority |
|---|---|---|
| **R — Regenerated** | ClusterClass and machine templates, desired `Cluster` specs, Crossplane Compositions, all platform software, dashboards, policies, ACME certificates | GitOps at a pinned `environmentRevision` |
| **B — Restored data** | Each platform application's PostgreSQL cluster, physically and independently (ADR-112): `infisical-db`, `zitadel-db`, `credits-db`, `grafana-db`. `control_plane` and `hub` get no cluster and no archive — ADR-112 §1 records that nothing in the platform connects to either, so they are **retired state**: not newly created in the successor and not recovered into it, with their credentials retiring alongside `platform-db` after the last application has moved. Retiring them requires its own recorded decision about whatever historical data they hold, which ADR-112's transition owns and this ADR does not presume | that cluster's own object-store archive (Revision 5 §2) |
| **M — Management state** | CAPI core, bootstrap, control-plane and infrastructure-provider **objects**: their names, specs, labels and reference graph, plus the bootstrap-data Secrets they depend on. **Not the cluster PKI** — that is Class E, and duplicating it here would create a second recovery authority whose disagreement with the escrow nothing would notice | Versioned Kubernetes resource backup in external object storage (§10 governs its custody) |
| **E — Escrowed keys** | Keys that must return byte-identical | Tenant-controlled Infisical, scoped per §1 |
| **N — Newly created** | Hub etcd and cluster PKI, control-plane endpoint and node addresses, node identities, service-account tokens, the successor's archive prefix, Argo CD instance state, Tailscale registrations | created fresh |

Three notes that are load-bearing rather than descriptive.

**Class B is physical, per application.** Each cluster is restored whole from its own
archive; no logical per-database restore is used. Per ADR-092, restoring a CNPG cluster *is*
the creation of a new cluster — `spec.bootstrap` is read once — and the exit back to the
steady state is machine-enforced.

**Class B is a recovery copy, not the authority.** The live PostgreSQL instance, including
the credits ledger, remains the system of record. Externalising recovery material does not
move authority.

**Class E holds no KMS material.** ADR-100 is deprecated: accepted 2026-10-02, acceptance
withdrawn 2026-10-06, halted 2026-10-15 and not to be acted on, with its amendment to
ADR-003 §6 withdrawn alongside it. `secretbox` remains the platform's at-rest mechanism and
is active on every cluster. There is therefore no external KMS trust model in force and no
hub-held per-spoke KMS certificate authority to recover. What exists is a 32-byte secretbox
key per cluster, delivered to control-plane nodes by CAPI, incarnation-scoped above. Should
that model ever be revived — the ADR records that the first constraint to relax is
Infisical's exclusion from the identity path — per-spoke issuer material becomes a
**spoke-scoped** Class E artifact, which §1 already has a place for.

### 3. How a spoke is adopted: a documented pause contract, not a provider guarantee

Earlier drafts proposed restoring provider IDs and unpausing reconciliation. That was
rejected because no upstream contract makes provider IDs an adoption protocol, and the
rejection was correct. A later draft replaced it with the provider's own lookup behaviour and
claimed that behaviour was fail-safe. **That claim was wrong, and this section replaces it.**

**Deployed versions.** CAPI core `v1.10.0`, clusterctl `v1.10.0`, kubeadm bootstrap and
control-plane providers `v1.8.5`, Hetzner infrastructure provider (CAPH) `v1.0.7`
(`internal/kakt-cli/versions/versions.go`, asserted by its test). CAPH's `metadata.yaml`
maps release series 1.0 to Cluster API contract **`v1beta1`**, which is the contract CAPI
v1.10.0 serves. Only `HCloudMachineTemplate` is in use; no `HetznerBareMetalHost` appears in
the manifests, so the bare-metal host-inventory model is out of scope.

#### What the provider does, and what it does not promise

At CAPH v1.0.7, `Service.Reconcile` for an `HCloudMachine`
(`pkg/services/hcloud/server/server.go`) proceeds: refuse until `IsBootstrapDataReady()`;
`findServer` — resolve the provider ID and `GetServer`, and failing that list servers by a
label selector built from `createLabels()` — `caph-cluster-<HetznerCluster name>: owned`,
`machine.caph-name: <HCloudMachine name>` and `machine_type: control_plane|worker`, whose
literals are in `api/v1beta1/tags.go` at v1.0.7 (the prefix is `caph-`, not a
`cluster.x-k8s.io` form) —; **if no server is found,
`createServer`**; then `SetProviderID`, then `Status = statusFromHCloudServer(server)`, then
`validateLabels`.

Two properties of that sequence are load-bearing, and the second is the correction:

**The binding lives on the infrastructure.** CAPH writes the owning cluster and the machine
name onto every Hetzner server as labels. Those labels are tenant-owned, outside the hub, and
survive its destruction, so they are an **independent infrastructure-identity reference**.
Status is recomputed from the live server on every reconcile, so Class M restores identity and
relationships and controllers regenerate status.

**There is no fail-closed behaviour inside `Reconcile`.** `validateLabels` runs only *after* a
server has been found, so it cannot protect the path that matters: a lookup returning nil
leads unconditionally to `createServer`. A restored object carrying the wrong machine name, or
naming a different `HetznerCluster`, matches no labels, matches no provider ID, and therefore
**creates a duplicate server** while the real one keeps serving. `CreateMachineError` and
`MultipleInstances` are raised on other paths and are not evidence that an unsuccessful lookup
is non-destructive. They are not.

So this architecture does not rest on provider behaviour for safety. The provider's lookup is
what makes adoption *possible once identity is verified*; it is observed behaviour at a pinned
version, not a supported contract, and it is never the mechanism that prevents harm.

#### The mechanism that does prevent harm

Safety rests on a documented Cluster API contract. `PausedAnnotation`
(`cluster.x-k8s.io/paused`) is defined as applicable to **any** Cluster API object, and the
contract on controllers is explicit: *"Controllers working with Cluster API objects must check
the existence of this annotation on the reconciled object."* CAPH v1.0.7 honours it —
`controllers/hcloudmachine_controller.go` returns early on
`annotations.IsPaused(cluster, hcloudMachine)`, and its watches are filtered by
`ResourceNotPausedAndHasFilterLabel` and `ClusterUnpausedAndInfrastructureReady`.

That gives three tiers, all native, and the middle one was missing from an earlier revision:

* **cluster** — `Cluster.spec.paused`, which suppresses reconciliation for everything beneath
  it, because `annotations.IsPaused(cluster, obj)` consults the Cluster as well as the object;
* **creators** — the paused annotation on each `KubeadmControlPlane`, `MachineDeployment` and
  `MachineSet`. **Pausing leaves is not sufficient.** A paused `Machine` does not stop its
  parent from creating another one to satisfy a replica count, so the objects that can bring
  infrastructure into existence must be paused in their own right. `cluster.x-k8s.io/paused`
  applies to any Cluster API object, these included;
* **leaves** — the paused annotation on each `Machine` and `HCloudMachine`, so objects are
  released one verified match at a time rather than as a set.

**Release order follows from which tier creates.** The recovered topology graph is completed
first — every object the ClusterClass manages present and paused — because a ClusterClass-based
`Cluster` is reconciled by the topology controller, and pausing the objects that exist does not
stop it creating one that is missing. Classification follows. Only then is the Cluster unpaused,
which arms nothing: the topology controller finds its graph whole, and every object beneath is
still annotated. Matched leaves are released next, and the creators **last**, once the fleet
they would reconcile against is whole. `cluster.x-k8s.io/disable-machine-create` is a further
native signal to a `MachineSet`, though upstream documents it for the `OnDelete` strategy, so
it is secondary here.

The invariant, stated once: **no controller capable of creating, deleting or remediating
infrastructure becomes active before the relationships it would act on are classified.**

#### One owner during reconstruction

Class R and Class M can both produce a `Cluster` object — the bundle renders the desired spec,
and the backup restores the live graph — so an unowned race exists in which Argo CD creates an
unpaused `Cluster` before Velero restores the graph. The resolution is declarative rather than
sequential: **while a box is reconstructing, the rendered `Cluster` carries
`spec.paused: true`**, and Velero applies the same annotations to restored objects through
restore resource modifiers, which upstream applies before resources are restored. Whichever
path creates the object first, the object is paused, so the race has no unsafe outcome and
needs no ordering guarantee between the two mechanisms.

Authority is then unambiguous for the duration: **Class R owns the ClusterClass, the machine
templates and the desired `Cluster` spec; Class M owns the live object graph** — the
`Machine`, infrastructure, bootstrap and control-plane objects and their Secrets. Reconstruction
does not render what it restores.

Reconstruction therefore restores every CAPI object **paused at both levels**, and the only
act that can create or remediate infrastructure is the deliberate removal of an annotation
from an object whose match has already been established.

#### Classification before release, and fail-closed defaults

Every object and every labelled provider resource is placed in exactly one state, and only
the first is eligible for unpausing:

| State | Meaning | Disposition |
|---|---|---|
| **Matched** | restored object and exactly one labelled server agree on cluster and machine name | unpause this object |
| **Object without server** | the server was deleted after the Class M backup | **unresolved.** Unpausing creates a replacement — which may be wanted, but is a decision. The object is deleted before release if it is not. |
| **Server without object** | the machine was created after the Class M backup | **unresolved** (see §5) |
| **Ambiguous** | more than one labelled server matches, or labels conflict | **unresolved.** Never unpaused. |

`MachineHealthCheck` objects are absent until every state above is resolved: a restored graph
briefly has no Node correspondence, and health checking remediates on exactly that.
`cluster.x-k8s.io/disable-machine-create` exists as a further native signal to a `MachineSet`,
though upstream documents it for the `OnDelete` strategy, so it is a secondary guard here and
not the primary one.

**Unmatched and ambiguous states are terminal for automation.** They are reported as
unresolved and resolved by a person. The architecture deliberately provides no automatic
remedy, because every candidate remedy is an invented adoption protocol.

#### The limitation, stated plainly

CAPH v1.0.7 offers no mechanism that makes an *unpaused* machine with unverified identity
safe. The smallest supported alternative is the one adopted above: a documented CAPI pause
contract plus an external verification step, with no custom adoption controller. If a future
provider version removes the label lookup without supplying a supported adoption path, §9
records what applies instead.

### 4. Planned succession

The predecessor is operational, so management is **transferred**.

* **Database.** CNPG distributed topology with controlled switchover, coordinated by the
  demotion/promotion token, is preferred over an irreversible standalone-replica promotion:
  it keeps the predecessor demotable rather than discarded, so the cutover is reversible up
  to the switchover itself. This is same-major-version physical replication; a PostgreSQL
  major-version change is a separate supported logical migration and cannot ride a
  switchover.
* **Management.** `clusterctl move` is the supported transfer and belongs **here only** — it
  requires a reachable source, and upstream warns against it as a backup mechanism precisely
  because it does not restore status. It relocates the object graph together with the Secrets
  labelled for move, which is why the planned case needs no reconstruction of PKI. The
  controller then recomputes status from the provider as in §3.
* **Keys.** Both incarnations share the logical-hub escrow scope and write their own
  incarnation-scoped artifacts; neither overwrites the other.
* **DNS.** Record ownership belongs to `boxId`, so the successor adopts the zone's TXT
  records rather than re-keying them. The hazard was never re-keying — it is two boxes
  holding one owner id under `--policy=sync` at once, so the handover is sequenced:
  the predecessor withdraws before the successor publishes.
* **Verification before commitment.** The successor is validated against real restored data
  — identity counts, a decrypt of a known secret, the ledger's balance — while the
  predecessor still serves. In-place upgrade cannot offer this, and it is the strongest
  argument for the model.

### 5. Disaster reconstruction

The predecessor is permanently unavailable, so `clusterctl move` does not apply and
management is **re-established** from independently retained resources: the bundle, the
tenant repository, the CNPG archive, the escrow, and the provider's own inventory.

The spokes are unaffected by the hub's loss: each spoke's control plane and etcd are its
own, its workloads keep serving, and its database archives under its own per-spoke prefix
(ADR-014, ADR-082). What is lost is control — provisioning, scaling, rolls, delivery and
observability aggregation. **Hub loss is a control-plane outage; hub *mis*-reconstruction is
a tenant outage.** Remediation is the mechanism that converts one into the other, which is
why the ordering below is a correctness property and not an operational nicety.

Reconstruction restores Class M's objects and Secrets with reconciliation **paused at both
levels** (§3), strips `ownerReferences`, classifies every object and every labelled provider
resource against the provider's live inventory, and releases only the objects whose match §3
calls *matched*.

**`ownerReferences` must be stripped, not restored.** UIDs do not survive into a new API
server. An ownerReference naming a UID that does not resolve makes its object eligible for
garbage collection, so a *faithful* restore of the graph invites Kubernetes to delete the
fleet. CAPI's controllers re-establish owner references as part of normal reconciliation;
the restore must not attempt to.

**Provider inventory is an independent identity reference, not a substitute for the
backup.** Because CAPH's labels carry the owning cluster and the machine name, the provider's
server list is enumerable and authoritative *about correlation*. It says which server belongs
to which cluster and which machine name. It says nothing else: not the `Machine` spec, not the
bootstrap-data Secret, not the `KubeadmConfig` linkage, not the cluster PKI, not the
`HetznerCluster` spec or its load-balancer state. Those have no source but Class M and
Class E.

So the two authorities divide cleanly, and a previous draft of this ADR conflated them:

| Question | Answered by |
|---|---|
| which server is which machine | provider labels — an independent reference |
| what that machine's object must contain | Class M backup |
| what credential or authority it needs | Class E escrow |

**Class M therefore carries a backup-based RPO, and this ADR claims no better.** The earlier
claim of effectively zero RPO for machine identity was too broad: labels let divergence be
*detected* and a missing object be *named*, which is not the same as reconstructing it.

Divergence after the last Class M backup resolves as follows, and two of the three cases are
terminal for automation:

* **A server created after the backup.** Detected as *server without object*. Its
  bootstrap-data Secret and `KubeadmConfig` were never captured, and `IsBootstrapDataReady()`
  gates the lookup, so a reconstructed object for it can never reach adoption. There is no
  supported mechanism to adopt it, and manufacturing bootstrap material to satisfy the gate
  would be an invented protocol. It is reported **unresolved**; the supported resolution is to
  replace the machine, which the fleet's design already accommodates for workers and burst
  nodes and which is a deliberate decision for a control-plane node.
* **A server deleted after the backup.** Detected as *object without server*. Releasing the
  object creates a replacement; that may be correct, so it is a decision rather than a default.
* **A server modified after the backup.** Labels still correlate and status is recomputed from
  the live server, so spec drift reconciles through ordinary CAPI behaviour.

**Incomplete or ambiguous recovery is classified as unresolved, never as automatically
recoverable.** That is the architecture's position wherever a supported mechanism is absent.

### 6. Exclusive ownership

A replacement hub must be the only authority over the fleet, including against a predecessor
that returns unexpectedly. The fence is **credential revocation and rotation**, because every
authority a hub holds is a credential the tenant controls — no distributed lock or custom
leader election is introduced. Each authority that can survive hub replacement is enumerated,
because an unenumerated one is the whole failure:

| Authority | Fence | Native mechanism |
|---|---|---|
| Hetzner infrastructure API | rotate the token | CAPH cannot act on any server without it; v1.0.7 marks the token unavailable rather than acting |
| Object storage (archives, Class M) | rotate the credential | the predecessor's archive prefix is also read-only by construction once the successor derives its own from `clusterName` |
| PostgreSQL | revoke the predecessor's roles | CNPG topology additionally admits one primary at a time |
| Financial ledger write authority | revoke the Credit Authority's database credential | single-writer follows from the role revocation above, not from an application lock |
| Infisical | revoke the predecessor's machine identities | its master keys stay escrowed; revocation removes use, not recoverability |
| Git repository | revoke the deploy credential | the predecessor's Argo CD holds no authority the repository does not grant |
| DNS publishing | revoke the external-dns credential, then hand over ownership | `boxId`-keyed owner id plus the sequenced withdraw-then-publish of §4 |
| Tailnet identity | revoke the auth key and remove the predecessor's devices | the tailnet is tenant-owned and administered outside the box |
| **Spoke Kubernetes API** | **see below — no revocation exists** | — |

**A predecessor that is still writing backups is a fencing dimension of its own.** Revoking
the object-store credential (above) stops it, but the ordering matters and the reference
implementation makes it explicit: once a successor activates, the predecessor's backup
schedules must no longer run. Two hubs writing management-state backups to one location
produce a set in which no backup says which hub produced it, so a later reconstruction cannot
tell a predecessor's fleet from a successor's. Activation therefore stops the predecessor's
schedules, and because activation is a commit (Revision 6 §2) that is a declarative act rather
than a remembered one.

**The spoke API credential is the one authority that cannot be revoked, and it is a real
limitation.** A hub holds each spoke's administrative kubeconfig, whose client certificate is
signed by that spoke's certificate authority. Kubernetes has no certificate revocation list,
so a certificate already extracted stays valid until it expires — deleting the Secret in the
hub withdraws nothing. The same exposure covers Argo CD's cluster access, because those
registrations are derived from the CAPI kubeconfig Secrets rather than from a separate
credential.

An earlier revision of this ADR claimed a `KubeadmControlPlane` rollout rotates the spoke's
certificate authority and so revokes the predecessor's access. **That was wrong, and it
conflated two different operations.** Leaf renewal replaces certificates signed by the
existing authority; authority replacement replaces the trust root so certificates the old
authority issued stop being accepted. kubeadm does not support rotating or replacing a
cluster's certificate authority out of the box — its renewal function retains the existing
one — and Cluster API's control-plane certificate rotation triggers a Machine rollout based on
certificate *expiry*, which renews leaves under the same root. Neither invalidates an
administrative kubeconfig that has already been extracted.

**So there is no supported mechanism that revokes a predecessor's spoke API access, and this
ADR classifies the gap rather than claiming one.** It is a **residual access risk with a
bounded lifetime**:

* **The bound** is the `notAfter` of the client certificate the predecessor holds. Cluster API
  renews the kubeconfig Secret it manages, but a copy taken before replacement remains valid
  to its own expiry and no in-cluster action shortens that.
* **What the predecessor can still do** within the window: reach each spoke's API server with
  administrative rights.
* **What it cannot do**, because §6's other fences are credentials and therefore revocable:
  provision or delete infrastructure, archive or read backups, publish DNS, write to the
  ledger, read or write the secret store, or pull from the repository.
* **Network containment, where the endpoint allows it.** Where a spoke's control-plane
  endpoint is reachable only through a controlled network, access controls on that network can
  keep a retired hub away from it. Whether that is available is a per-box property of
  `controlPlaneLoadBalancerEnabled` and `controlPlaneEndpointHost`: a spoke whose endpoint is
  served by a public load balancer cannot be contained this way. This bounds reachability; it
  does not revoke a credential, and it is recorded as mitigation rather than as a fence.
* **Replacing the spoke is the supported platform-level remedy**, because it replaces the trust
  root along with everything else, and because replacement is already this platform's model for
  a spoke — a template's spec is effectively immutable, so a change is a new name rather than an
  edit (ADR-046). Kubernetes documents manual certificate-authority replacement procedures, so
  replacement is the *supported* approach here rather than the only technically possible one;
  the manual path is not adopted because it is not automated by kubeadm and would become
  platform-owned custom procedure over every spoke's trust root.

**A documented tenant acceptance of this residual risk is not equivalent to revocation, and
this ADR does not present it as one.** The tenant accepts the window with its length and its
full scope as stated above, contains it at the network where the endpoint permits, or replaces
the spokes.

### 7. Recovery points

There are three independent recovery authorities — a GitOps revision, a CNPG archive point,
and a Class M backup version — with separate writers and separate failure modes. **They
cannot be made mutually atomic, and this ADR does not claim they are.** Nor does it claim zero
RPO anywhere: no state in the table below has a mechanism that would guarantee it.

| State | RPO | Reconciliation |
|---|---|---|
| Class R | none — version-pinned in Git | authoritative for desired state; drift self-heals |
| Class B | last archived WAL, **per cluster** | bounded, non-zero, and different for each application; the ledger's exposure is its own cluster's window (Revision 5 §4) |
| Class M | last management-state backup, for objects **and** Secrets alike | divergence is *detected* against provider inventory and classified per §5; detection is not recovery |
| Class E | last escrow write | rotation windows retain both generations, as `secret-encryption-key-previous` already models |

Desired state self-heals because Class R is authoritative for it. Identity correlation is
independently *detectable* from provider labels, which narrows how much of a Class M gap is
silent — but every gap still resolves through §5's classification, and some resolutions are
machine replacement rather than recovery. The obligation this ADR accepts is **detection and
explicit classification**, not atomicity and not completeness.

### 8. Minimal changes to the platform

1. Split `boxId` from `clusterName` in the registry schema and the scaffold spec.
2. Scope the escrow path to logical hub, hub incarnation and spoke identity.
3. Extend the escrow contract to per-spoke CAPI PKI, keyed to the spoke.
4. Derive the hub's WAL archive `destinationPath` from `clusterName`. ADR-014 addendum 2
   decided this on 2026-09-17 and it was implemented for spokes only; the hub manifest still
   carries a literal path with a hand-bumped server name. Concurrent boxes are impossible
   until this is done, succession or otherwise.
5. Adopt Velero for Class M (Revision 5 §5) — native OSS, no custom controller — covering CAPI core, bootstrap, control-plane and
   infrastructure-provider resources, the bootstrap data Secrets, and the cluster PKI
   Secrets, under the custody requirements of §10.
6. Record a predecessor pointer carrying only the archive prefix to restore from.
7. Make escrow verification assert sufficiency rather than presence: today it proves the
   artifacts exist, never that a box can be rebuilt from them.

No Terraform, Atlantis or Vault is introduced. CAPI, CNPG, Infisical and the bundle model
are retained.

### 9. Version compatibility contract

The adoption property in §3 is read from CAPH v1.0.7's source. It is **version-bound**, and
treating it as durable across upgrades would be exactly the error of taking observed
implementation behaviour for a supported enterprise pattern. This section fixes what the
architecture may assume.

**The supported relationship.** CAPH's `metadata.yaml` maps release series to a Cluster API
*contract* version — series 1.0 declares contract `v1beta1` — and the contract may change
between minor or major versions but never between patches. So the compatibility statement is
a chain, and every link is pinned: the Kubernetes version a spoke runs, the CAPI core serving
a contract (`v1.10.0`), the kubeadm bootstrap and control-plane providers implementing it
(`v1.8.5`), the infrastructure provider declaring it (`v1.0.7`, contract `v1beta1`), and the
API versions of the objects a Class M backup contains (`v1beta1` for CAPI and CAPH today).
**A Class M backup is only restorable into a successor serving the same contract version.**
Restoring `v1beta1` objects into a hub whose providers have moved to a later contract is a
conversion problem, not a restore, and is outside this ADR.

**Which recovery properties are provider-version dependent.** Three, and they are the three
§3 depends on:

| Property | Status at v1.0.7 | Risk on upgrade |
|---|---|---|
| Label-based server lookup | present, undeprecated | carries an explicit deprecation notice at v1.1.5 and is stated as planned for removal |
| Status derived from the live server | `Status = statusFromHCloudServer(server)` each reconcile | v1.1.5 introduces a `Status.BootState` machine, so status becomes load-bearing |
| Create-if-not-found | unconditional after a nil lookup | unchanged; this is why §3 rests on pausing, not on the provider |

The `cluster.x-k8s.io/paused` contract is the one property that is **not** provider-specific:
it is defined by Cluster API for any of its objects, with an explicit obligation on
controllers to honour it. The architecture therefore depends on a CAPI contract for safety and
on provider behaviour only for convenience — which is what keeps it valid across versions even
when §3's lookup changes.

**What applies when a newer CAPH changes the behaviour.** The fresh-hub requirement is
unaffected; only the adoption step is. In order of preference:

1. **Planned succession** becomes the only supported route for that version, since
   `clusterctl move` relocates objects together with their Secrets and the provider then
   reconstructs state itself. Disaster reconstruction is the case that degrades, not succession.
2. **Class M restores status** for the infrastructure objects whose controllers have made
   status load-bearing, which raises a requirement on the backup tool rather than on the
   provider, and must be verified against that tool before the provider is upgraded.
3. If neither holds, the limitation is recorded and disaster reconstruction of **machine
   management** is declared unsupported at that provider version while data recovery and fresh
   provisioning remain supported. That is a reduction in capability, stated, rather than an
   assumed adoption path.

**A provider upgrade is therefore a change to the recovery contract** and re-opens §3 against
the new version's source before it is deployed. The EOL Kubernetes and CloudNativePG track
will raise this, and it is the reason this section exists.

### 10. Custody and protection of Class M

A Class M backup contains the bootstrap-data Secrets and the cluster PKI. It is therefore the
most sensitive artefact this architecture creates, and an earlier draft of this ADR was wrong
about why that is tolerable: it cited ADR-104 as having made node bootstrap material safe to
carry. **ADR-104 is Proposed, not Accepted.** Bootstrap-material exposure is an open problem,
not a resolved one — ADR-100's halt record describes the live condition plainly, that the
at-rest encryption key reaches nodes through user-data and the metadata endpoint re-serves
user-data to any unprivileged pod.

So the correct premise is the opposite of the one withdrawn here: **Class M backups must be
treated as containing key material**, and will continue to be until ADR-104 is accepted and
in force. The requirements follow from that premise and reuse mechanisms the platform already
has:

* **Encryption, by the object store rather than by Velero.** An earlier revision named a
  "Velero backup encryption key" as the key that reads a Class M backup. **Velero's
  Kubernetes API-object backup has no such key**: it uploads an archive of API objects to
  object storage. Its repository encryption belongs to the separate file-system backup path
  (Kopia), which this design does not use, and conflating the two invented an encryption
  authority that is not in the data path.

  So the encryption authority is **the object store's**, and it is named: **server-side
  encryption with a customer-provided key (SSE-C)** on Hetzner Object Storage, configured
  through the Velero S3 plugin's customer-key setting. The tenant is therefore the encryption
  authority, and the consequence is the part that belongs in this ADR:

  **The SSE-C key is a Class E artifact, escrowed under the logical-hub scope (§1).** A key held
  only in the hub would make every Class M backup unreadable the moment the hub is gone — the
  precise circularity ADR-076 exists to prevent — so it is escrowed outside the box with the
  master keys, and it is one of the authorities §6 revokes when a hub is replaced. Provider-managed
  server-side encryption is rejected for this artefact: it would place the key to the fleet's PKI
  with the infrastructure provider rather than with the tenant.

  Within the successor, restored Secrets are protected by the accepted at-rest mechanism
  (ADR-105, `secretbox`, active on every cluster). That protects them *after* restoration and
  does nothing for the archive, which is the reason the row above matters: Class M holds
  bootstrap Secrets as **plaintext API objects**, carrying none of their original etcd
  protection. (The PKI is not among them — it is Class E only, per §2.)
* **Tenant-owned custody.** The bucket is the tenant's, in the tenant's account, consistent
  with ADR-065 — the platform holds no credential of theirs — and with ADR-076's placement of
  recovery material outside the box.
* **Two authorities, both escrowed.** Encryption and authentication are different things, and a
  successor needs both: the SSE-C customer key *and* the object-store credential that reaches the
  bucket at all. Keeping only the key outside the box would leave the credential inside the
  Infisical that has to be recovered from that bucket. Both are box-scoped Class E
  (`backup-ssec-key`, `backup-store-credential`), delivered straight into the cluster by
  `kakt escrow deliver-backup-credentials` and never through Infisical or ESO -- routing the
  delivery of a value whose purpose is to work without Infisical through Infisical is the
  circularity ADR-076 exists to break.
* **Credential isolation.** A dedicated credential for the Class M bucket, distinct from the
  CNPG archive credential and from any workload credential, so compromise of one recovery
  authority does not yield the others. This also matters for §6: the Class M credential is one
  of the authorities a replaced hub must lose.
* **Four key roles, not one.** An earlier revision said an older Class M backup needs the hub
  at-rest key generation that was in force when it was taken. **That is incorrect for a
  Kubernetes API-object backup.** At-rest encryption applies to what etcd stores; the API
  server returns Secret data decrypted, so Velero captures plaintext and protects it with its
  own encryption. No hub at-rest generation is needed to read a Class M backup — which makes
  this bullet's other requirements more important, not less, because the artefact is plaintext
  before Velero encrypts it. The roles separate as:

  | Key | Needed for | Retention |
  |---|---|---|
  | the object store's SSE-C customer key | reading any Class M backup | **Class E, logical-hub scope** — escrowed outside the box, and revoked from a replaced hub (§6) |
  | the successor's hub at-rest key | the successor's own etcd | newly created — Class N, never inherited |
  | each spoke's encryption key | managing and recovering that spoke | retained, spoke-scoped Class E (§1) |
  | historical hub generations | **only** artefacts that actually hold ciphertext under them, such as an etcd-level copy if one is ever taken | retained only while such an artefact exists; not retained for Class M |

  `secret-encryption-key-previous` continues to cover a rotation window, which is a different
  requirement from this one. Historical generations are not retained on a dependency that does
  not exist.
* **Restoration has a plaintext intermediate, and the earlier claim that it does not was
  wrong.** Velero extracts a resource-backup archive into a temporary directory inside its own
  container and writes every object in it to a file before restoring them
  (`pkg/archive/extractor.go`, `readBackup` → `fs.TempDir("", "")` → `writeFile`, read at
  v1.18.2), so the bootstrap-data Secrets in a Class M restore exist as
  plaintext files in that container's filesystem for the duration of the restore. No
  configuration removes that step, and building a restoration engine to avoid it would be a
  custom replacement for the mechanism this design chose precisely because it is standard.

  What follows is that the Velero workload itself is part of the custody boundary, not merely
  the bucket: its node, its container filesystem and anything with access to them are in scope
  for as long as a restore runs. The protections that remain available are the ones to state --
  the archive is encrypted at rest under SSE-C (above), the extraction is transient and local to
  a pod on the successor, and the restore credential is held only for the duration of the
  reconstruction. The PKI is not exposed this way at all, because it is Class E and is never in
  a Class M archive (§2).
* **Retention bounded by purpose.** A Class M backup is retained while its recovery point is
  meaningful and destroyed thereafter, because an indefinitely retained copy of the fleet's
  PKI is a liability rather than an asset.

Until ADR-104 is in force, the honest statement of residual risk is that Class M backups
inherit whatever exposure node bootstrap material has, and the mitigations above bound where
copies exist and who can read them rather than removing the underlying exposure.

## Consequences

### Alignment with the reference model

| Property | kubefirst | This design |
|---|---|---|
| Reproducible management-cluster creation | management cluster declared as a directory of components, upstream versions pinned | same shape, via `registry/clusters/<name>` and a version-pinned bundle |
| External desired-state authority | GitOps repository | GitOps repository plus pinned `environmentRevision` |
| External infrastructure-state authority | Terraform state | CAPI objects in Class M **plus the provider's own labels**, which Terraform state has no analogue for |
| Reuse of existing external configuration and credentials | Vault | tenant-controlled Infisical escrow, scoped per incarnation |
| Rebuilding platform software independently of the original cluster | yes | yes |
| Recovering originated state | not required — originates almost nothing | required: identity, ledger, spoke PKI |
| Documented upgrade path for the management cluster | none | this ADR |

The remaining differences are necessary, not avoidable: they exist because our hub
originates state theirs does not, and because our fleet is CAPI-managed rather than
Terraform-managed.

### What this costs

* A quiesce window in the planned case, shortened but not removed by the switchover
  mechanism.
* Double infrastructure for the duration of a succession.
* A cadence ceiling: ACME duplicate-certificate limits bound how often the same name set can
  be re-issued. Succession is the mechanism for changes in-place cannot make — the EOL
  Kubernetes and CloudNativePG jump, and the PostgreSQL major-version move behind it — not
  for ordinary releases.
* The predecessor's archive is retained, not migrated, for as long as its recovery point
  matters.
* On-prem nodes re-register, which the registry already records as a consequence of
  replacing a control plane.

### Unresolved limitations

1. **No fail-closed behaviour exists inside the provider's reconcile.** Once an object is
   unpaused, a nil lookup creates a server unconditionally; `validateLabels` runs only after a
   server is found. Safety is supplied entirely by the pause-and-verify contract of §3, which
   is a CAPI contract, not a provider guarantee.
2. **The adoption property is pinned to CAPH v1.0.7**, read from source rather than from a
   documented provider contract. §9 governs what applies when it changes; any CAPH upgrade
   re-opens §3.
3. **A machine created after the last Class M backup cannot be adopted.** Its bootstrap-data
   Secret was never captured and `IsBootstrapDataReady()` gates the lookup. The supported
   resolution is replacement, which is a deliberate decision for a control-plane node.
4. **Spoke administrator access cannot be revoked, and the residual risk is full control of
   that spoke.** Kubernetes has no certificate revocation list, so a predecessor keeps a valid
   administrative client certificate — and with it Argo CD's derived cluster access — until the
   certificate expires. The exposure is not merely reaching an API server: it is reading and
   writing workloads, Secrets and any other resource on that spoke. §6 records the bound, the
   containment options and the fact that a documented tenant acceptance of this risk is **not**
   equivalent to revocation.
5. **SSE-C support is a compatibility dependency, not an architectural unknown.** §10 decides the
   mechanism; whether the pinned Velero S3 plugin and Hetzner Object Storage accept the
   customer-key configuration together is established during implementation. If they do not, the
   requirement that the tenant hold the encryption authority stands and the mechanism is
   re-chosen under it.
6. **Class M custody assumes an exposure that is not yet closed.** ADR-104 is Proposed, so
   §10's requirements bound where copies of bootstrap material exist rather than removing the
   exposure itself.
7. **CNPG distributed topology is unverified at the deployed 1.29.0.** If unavailable there,
   planned succession falls back to physical recovery with a longer quiesce.
8. **Velero is pinned and verified from source, but not yet installed.** The schema this design
   depends on was read at v1.18.2 and re-checked at v1.18.4 — restore modifiers
   (`internal/resourcemodifiers`), the Backup and BackupStorageLocation specs, and the
   archive's temp-file extraction — and the SSE-C configuration was read in the AWS plugin at
   v1.14.4 (`velero-plugin-for-aws/object_store.go`). Pinned: chart **12.2.2**, server
   **1.18.4** by image override, AWS plugin **1.14.4**.

   The config-key uncertainty recorded here earlier is **closed, and its failure model was
   wrong**: the plugin validates its configuration against an allow-list, so a misspelled key
   is rejected rather than ignored, and there is no silent path to unencrypted backups through
   a typo. Reading that source also added two requirements this design now depends on — the
   plugin resolves the Secret from `VELERO_NAMESPACE`, so the chart setting that variable is
   load-bearing, and it reads the Secret with its own in-cluster client, so Velero's service
   account needs `get` on Secrets in its namespace.

   What remains open is narrower: the chart and plugin are not vendored, so image digests are
   unresolved placeholders that fail loudly; **Hetzner-specific S3 interoperability at these
   versions is unverified**; and that the credentials stream's selector captures every
   bootstrap-data Secret in practice needs a live cluster, because the label is applied by our
   own reconcile and its coverage is only as current as the last pass.
9. **Class M is restorable only into a successor serving the same CAPI contract version.**
   Cross-contract conversion is out of scope.
10. **Spoke etcd is not backed up by this design.** Spokes survive hub loss, but a spoke's own
   loss remains governed by its per-spoke archive and ADR-092.
11. **Failback is not designed.** This ADR covers succeeding from a predecessor to a successor.
    It does not cover going back -- succeed, test, and return to the original -- which the
    reference implementation treats as a first-class scenario and guards against explicitly,
    because stale management-state backups from the other hub otherwise collide. Nothing here
    forbids failback; nothing here makes it safe either, and the Class M location is where it
    would go wrong first.
12. **A run that does nothing must not read as a run that succeeded.** The reference encodes
    this as a skip-all check. Provider-inventory matching refuses an empty object set for the
    same reason, but the principle is not yet asserted across every recovery step, and each new
    one has to be checked for the shape of it.
13. **Bare-metal hosts are out of scope.** CAPH's bare-metal host-inventory consumption model
    differs and §3 does not transfer to it.

## Revision 5 (2026-11-02): recovery is proven first, and Class B is per application

The sections above define what must be recoverable. Nothing in them has yet been recovered:
no archive has been restored, Velero is adopted nowhere, and CNPG's distributed topology is
unverified at the deployed version. ADR-112 has also replaced the single `platform-db` with
one cluster per platform application, so Class B is no longer one physical backup. This
revision records the consequences, and the order the work is done in.

### 1. What this revision changes

ADR-112 replaces the single `platform-db` with one cluster per platform application, so
Class B is no longer one physical backup: it is an inventory of independent recovery
authorities (§2), recovered in a dependency order (§3) to a declared target point (§4).
Velero becomes Class M's mechanism (§5), and the provider compatibility rules are tabulated
(§7).

**Sequencing, rehearsals and their evidence are not architecture and are not recorded here.**
They live in `docs/plans/k8s-cnpg-upgrade-and-restore-drill.md`, together with the restore-proof
and drill decisions of 2026-10-15. This ADR states recovery contracts, recovery points,
security requirements and version compatibility; it sets no acceptance condition on
demonstration, and it gates no release.

One architectural dependency does belong here, because it constrains the design rather than
its rollout: the recovery mechanisms this ADR relies on — the Barman Cloud plugin, point-in-time
recovery and controlled switchover — are properties of CloudNativePG releases later than the
deployed one, so §7 records them as unverified at the deployed version rather than as
available.

### 2. Class B, per application

Each cluster ADR-112 creates is a separate recovery authority, recorded in this inventory and
kept current with it:

| Cluster | Owning application | Recovery credentials needed | Restore prerequisites | Recovery-point exposure |
|---|---|---|---|---|
| `infisical-db` | secret store | archive read credential **from escrow**; Infisical master keys (Class E) | CNPG and the plugin | every credential the platform delivers through ESO |
| `zitadel-db` | identity provider | archive read credential (Infisical); `zitadel-masterkey` (Class E) | `infisical-db` restored and Infisical running | users, projects, grants issued after the point |
| `credits-db` | credit authority | archive read credential (Infisical) | as above | ledger entries after the point |
| `grafana-db` | Grafana | archive read credential (Infisical) | as above | sessions and preferences only; no telemetry |

**The archive namespace is a composite, and deriving it from the hub alone would collide.**
With several independent CNPG clusters in one hub, a prefix keyed only on the hub's
`clusterName` would place four archives on one path, and barman refuses to interleave two
databases under one server name (ADR-014, ADR-082). The contract is therefore that an archive
is unique in **both** dimensions: `destinationPath` is keyed by the hub incarnation's
`clusterName`, and `serverName` is keyed by the application cluster's own name and its
incarnation. Neither alone is sufficient — the first collides between applications, the second
between hub incarnations.

For each, the transition plan records the archive destination and prefix, the archive's owner
across hub incarnations, the recovery method and RPO, the verification procedure, and how its
writer is fenced (§6 of the Decision). The
`infisical-db` row is the one that cannot borrow from Infisical: its archive read credential
is escrowed (ADR-112 §4), because a credential stored in the service being recovered cannot
recover it.

### 3. Reconstruction order

1. The successor's Kubernetes control plane, network and storage; cert-manager; the
   CloudNativePG operator and the Barman Cloud plugin with their prerequisites; Velero.
2. The escrowed material, retrieved outside the box (ADR-076): Infisical's master keys and
   `infisical-db`'s archive read credential.
3. `infisical-db` restored and Infisical started; its machine identities re-established, so
   ESO can deliver again.
4. The other application clusters restored, each from its own archive. They depend on
   Infisical for their credentials and not on each other.
5. The applications started, and identity reconciliation run.
6. Class M restored with reconciliation paused at both levels (§3, §5).
7. Every object classified against the provider's inventory, and matched objects released one
   at a time.

This order is derived from the dependency graph above and not from an observed run. Any
dependency found that it does not satisfy is recorded here, not worked around.

### 4. Recovery points across applications

Each cluster recovers to its own last archived WAL. Restoring all of them to their latest
points does **not** produce a consistent platform snapshot, and this ADR does not claim one.
The known cross-application assumptions are recorded so a reconstruction checks them rather
than discovering them:

* an identity issued in `zitadel-db` after the secret store's point may reference a client
  credential Infisical no longer holds;
* a ledger entry in `credits-db` names identities, and may name one `zitadel-db` no longer
  has.

**The integrity policy.** Reporting an inconsistency is not a policy for identity and
financial data, so each cross-application relationship is placed in one of three categories,
and the mechanisms are point-in-time recovery and idempotent reconciliation — not a
distributed transaction, and no attempt to make independent backups appear atomic.

*Never the newest by default.* Each Class M stream is restored from a **named** backup, never
from `latest`. If the newest backup for a stream is missing or invalid while an older one
exists, the alternatives are reported and an explicit choice is required: silently falling back
to an older backup would move the recovery point without anyone deciding to, and the streams
would then disagree about when the fleet was. This is the same rule as the database's below,
applied to management state.

*One declared target.* A reconstruction chooses **one explicit target timestamp that every
archive can actually reach**, and recovers each cluster to that timestamp by point-in-time
recovery. The target is chosen from the archives' common reachable range rather than declared
first and approximated afterwards, and independent recoveries to one timestamp are still
independent recoveries — they do not compose into an atomic snapshot, and this ADR does not
claim they do.

| Category | Relationship | Resolution |
|---|---|---|
| **Must hold — by schema, not by ordering** | a ledger entry in `credits-db` refers to an identity | **A ledger entry carries an immutable subject reference and whatever identity detail the record itself needs, captured at transaction time.** An earlier revision tried to guarantee this by recovering `zitadel-db` to a point at or after `credits-db`'s; that is insufficient, because an identity may have been deleted, deactivated or altered *after* the transaction, so no recovery ordering makes a live lookup valid. The contract is therefore that **a historical ledger record is valid independently of the continued existence or current form of any identity record** — the dependency is removed rather than sequenced. Ledger rows are never discarded. |
| **Reconstructible through existing authority** | an OIDC client exists in `zitadel-db` whose secret Infisical no longer holds | the client is **re-created**, which is already the platform's only route: the issuer discloses a client secret once, at creation. Re-creation is *not* identity-preserving — the new client has a new identifier and a new secret — so **a client identifier is not a durable reference** and nothing outside the registration may store one as though it were. `hub-operator` owns the registration and re-resolves every dependent reference; that is what makes the reconciliation idempotent, not the re-creation itself. |
| **Requires intervention** | grants and roles issued after the target point; any reference that is neither immutable by schema nor re-resolvable by its owning authority | reported unresolved. Grants are re-issued by the authority that issued them. A reference in neither of the first two categories is a design defect in the referring application, recorded as such rather than patched during a recovery. |

Platform-generated state (§2, Class R) re-creates itself through reconciliation. Nothing in
the first or third category is ever silently resolved.

### 5. Velero is Class M's mechanism

Velero is adopted for management state only. It does not back up application databases or
their volumes, which are Class B.

* **Scope.** CAPI core, kubeadm bootstrap and control-plane, and CAPH resources; the
  bootstrap-data and cluster-PKI Secrets they reference; the metadata that relates them.
  `MachineHealthCheck` objects are excluded, so they are absent until classification ends
  (§3). Application workloads, CNPG resources and persistent volumes are excluded.
* **Custody.** §10, unchanged: a tenant-owned bucket, encryption independent of the cluster,
  and a credential distinct from every CNPG archive credential.
* **Pause at creation, across all three tiers.** Velero's restore resource modifiers set
  `Cluster.spec.paused`, remove `ownerReferences`, and add the paused annotation to **every
  object in the creator tier as well as the leaf tier** — `KubeadmControlPlane`,
  `MachineDeployment` and `MachineSet` alongside `Machine` and `HCloudMachine`. An earlier
  revision listed only the leaves, which contradicted §3 and left a restored
  `MachineDeployment` able to reconcile before its children were reconstructed. An object must
  never exist unpaused, not even between its creation and a later patch, so a mechanism that
  pauses after creation does not meet this. Whether the modifiers apply before creation, and in
  what order Velero restores the kinds, is verified at the pinned Velero version against the
  deployed CAPI version.
* **The managed topology is why the Cluster is released last.** Our spokes are ClusterClass-based,
  so the topology controller reconciles the Cluster into its topology-managed objects, and
  pausing the objects that exist does not stop it creating one that is **missing entirely**.
  `Cluster.spec.paused` is the only thing that holds that controller, so it is removed **only
  once the recovered topology graph is complete** — every object the ClusterClass manages
  present, and paused. An earlier revision released the Cluster first; that order was wrong and
  §3's release order supersedes it: complete the graph, classify, then unpause the Cluster
  (which now finds nothing missing), then release matched leaves, then release the creators.
* **Finalizers are kept, and the consequence is accepted deliberately.** Cluster API puts
  finalizers on `Cluster` and `Machine` so that deleting one cannot skip tearing down the
  infrastructure behind it. A restored graph therefore arrives with them, and every controller
  that would remove them is paused -- so an abandoned restore cannot simply be deleted: its
  objects would sit in Terminating until something is unpaused or a finalizer is removed by
  hand. Stripping them on restore would make that tidier and is rejected, because a `Cluster`
  without its finalizer can be deleted without its servers being released, and a recovery that
  quietly enables silent infrastructure orphaning is worse than one that is awkward to abandon.
  The reference implementation's own tests are full of finalizer-ordering failures, which is
  what prompted making this choice explicit rather than inheriting it.
* **Leaving the paused state is a GitOps change, not a patch.** Because the rendered `Cluster`
  carries `spec.paused: true` while a box is reconstructing (§3), removing the pause in the
  cluster would simply be re-applied on the next sync. The registry's reconstruction
  declaration is what renders it, and clearing that declaration — a commit — is what renders the
  Cluster unpaused. Nothing exits recovery by an imperative edit.
* **Status.** Where §9 makes status load-bearing, Velero's status restoration is what restores
  it, and it is verified before any provider upgrade relies on it.
* **Where Velero falls short.** The exact upstream limitation is recorded here, with the
  smallest supported alternative. No controller is written to replace it.

### 6. Restore proofs and drills

Recorded in `docs/plans/k8s-cnpg-upgrade-and-restore-drill.md`, not here. The restore-proof and
drill decisions of 2026-10-15 — read-only restore access, capacity, scheduling, validation,
cleanup, alerting and rights — are operational practice, and ADR-112's per-application clusters
mean each is drilled from its own archive. Nothing in them is a condition of this ADR.

### 7. Version compatibility matrix

§9's chain is extended to everything a recovery depends on. A change to any row is a change to
the recovery contract and re-opens the section that depends on it.

| Component | Deployed | Recovery dependency |
|---|---|---|
| Kubernetes | v1.31.6 (EOL; target open in the upgrade plan) | every class |
| CAPI core / clusterctl | v1.10.0, contract `v1beta1` | Class M (§3, §9) |
| kubeadm bootstrap and control-plane providers | v1.8.5 | Class M |
| CAPH | v1.0.7, contract `v1beta1` | Class M adoption (§3) |
| CloudNativePG | 1.29.0 (EOL) | Class B; in-tree Barman is removed in 1.31.0 |
| PostgreSQL | the operator's default image, not pinned | Class B; a major-version change is a logical migration, not a switchover (§4) |
| Barman Cloud plugin | not deployed | Class B after ADR-014 addendum 3 |
| Velero | not deployed | Class M (§5) |
| Backup API and CRD versions | CNPG `postgresql.cnpg.io/v1`; CAPI and CAPH `v1beta1` | restorability into a successor (§9) |

### 8. State of each area

The design above depends on mechanisms that are not yet exercised on this platform. They are
listed so the architecture is read with its dependencies visible — this is a statement of what
is unverified, not a condition on approving the design.

| Area | State |
|---|---|
| The current `platform-db` archive restores | NOT YET VERIFIED |
| Velero restores Class M paused, with no infrastructure mutated | NOT YET VERIFIED — Velero is not deployed |
| CNPG distributed topology and switchover at the deployed version | NOT YET VERIFIED |
| Barman Cloud plugin: backup, WAL continuity, point-in-time recovery | NOT YET VERIFIED — waits on the upgrade track |
| `infisical-db` recovers without Infisical | NOT YET VERIFIED — its archive read credential is not escrowed yet |
| Per-application clusters (ADR-112) | NOT YET VERIFIED — none exists |
| Kubernetes upgrade target | BLOCKED — on the target-version decision in the upgrade plan |

## Revision 6 (2026-11-02): a passive standby, and activation as a one-way door

The reference for externally recoverable management authority is Red Hat ACM's cluster backup
operator (`reference-projects/hub-recreation/cluster-backup-operator`, v2.5.0). Its principles
are adopted; none of its components are. No operator is written, and Velero, CAPI, CNPG and the
escrow remain the mechanisms.

### 1. Class M splits into three backup classes

The reference does not take one management backup. It takes three, on separate schedules —
credentials, resources, and the **activation** resources that connect a hub to the clusters it
manages — because only the third makes a hub the fleet's manager. Class M adopts the same split:

| Class M stream | Contents | Confers ownership |
|---|---|---|
| **credentials** | bootstrap-data Secrets. **PKI is excluded** — see below | no |
| **resources** | CAPI metadata and relationships that do not themselves command infrastructure | no |
| **activation** | `Cluster`, `KubeadmControlPlane`, `MachineDeployment`, `MachineSet`, `Machine`, `HCloudMachine` | **yes** |

**Exactly one recovery authority per artefact, and the PKI is where that rule bites.** An
earlier revision put the cluster PKI in both Class E and Class M's credentials stream. Velero's
default policy leaves an existing resource alone, and an earlier draft treated that as
sufficient — but skipping is not verifying: a Secret already restored from escrow would be
silently passed over, and a Secret restored from the backup first would become the live trust
root with nothing ever comparing the two. Restore order would decide which authority won.

So the PKI is excluded from the Class M backup outright. The escrow is its only source, and
`kakt cluster restore-pki` its only writer.

**Separating it needs more than exclusion, and an earlier revision got this wrong.** A
bootstrap-data Secret and a certificate authority carry the same type
(`cluster.x-k8s.io/secret`) and the same `cluster.x-k8s.io/cluster-name` label, so no selector
over type or those labels tells them apart. Cluster API's own discriminator is the name suffix
— which `util/secret.HasPurposeSuffix` formalises — and Velero filters by resource, namespace
and label, not by name suffix.

The first answer here was to label the authorities `velero.io/exclude-from-backup` and argue the
reconciliation window was harmless. **It is not.** An authority created between two reconciles
can be captured by a backup taken in that gap, which puts a private trust root inside a Class M
archive, outside the Class E boundary it exists to stay within. And if such a Secret were later
restored, **Velero would create it** — the earlier argument reasoned from "`restore-pki` reads
only the escrow" to "there is no second writer", and Velero is the second writer.

So the credentials stream uses **positive selection**: `hub-operator` marks each
bootstrap-data Secret `recovery.platform.io/class-m: bootstrap-data`, the stream selects on that
label and takes nothing else, and every Secret that has not been identified — an unlabelled
authority included — is excluded **by construction** rather than by a rule that has to keep up
with it. The `velero.io/exclude-from-backup` label on the authorities is retained as well, so
two independent reasons keep each of them out. The resources and activation streams exclude
Secrets outright, so neither can pick one up incidentally.

**Which Secrets, from the bootstrap contract rather than a name convention.** A Machine names
its bootstrap data at `spec.bootstrap.dataSecretName`, and a nil value means the Machine is
still Pending. So a Machine without one is mid-provision and not a gap; a Machine **with** one
whose Secret cannot be read or marked is a coverage gap, and `hub-operator` returns it as an
error rather than counting it as protected. A backup that succeeded is not evidence that every
machine is recoverable — only that what was marked was captured.

**The inclusion side is not yet verified.** That bootstrap-data Secrets carry the labels a
Class M selector would need is unconfirmed here: the kubeadm bootstrap provider is not vendored
in this repository, so its Secret's labels have not been read at the deployed version. The
schedules must be checked against a live cluster before they are trusted to capture every
bootstrap-data Secret. Bootstrap data is the mirror case: no escrow copy
exists, so Class M is its only source. Neither artefact has two authorities to reconcile.

**This is stronger than pausing, and it is why the split is worth adopting.** §3's three pause
tiers keep a restored object inert; withholding the activation stream means the object **is not
there at all**. An object that does not exist cannot be unpaused by mistake, cannot be adopted
by a controller, and cannot contend with a live hub. Pausing is the gate *inside* an activation
restore; class separation is the gate *before* one.

So a standby successor restores credentials and resources continuously, on an interval, with the
activation stream **skipped** — the reference's own requirement for its passive mode. It holds
every credential and every relationship it will need, and none of the objects that would make it
a second manager of the same fleet.

### 2. Activation is terminal, and it is a commit

In the reference, patching the managed-clusters backup from `skip` to `latest` creates the
activation restores and the passive sync **stops permanently**. That one-way property is the
safety: a hub cannot drift in and out of being the fleet's manager.

Ours is the same transition expressed in GitOps, which also answers how the rendered desired
state leaves `spec.paused: true` (Revision 5 §5): **clearing the registry's reconstruction
declaration is the activation.** One commit restores the activation stream, renders the `Cluster`
unpaused, and ends passive sync. It is recorded, reviewable and attributable, and there is no
imperative path to it — which matters more here than anywhere else in this ADR, because it is the
act that takes a fleet over.

Split-brain therefore requires two deliberate activations rather than one accident, and §6's
credential revocation remains in force underneath it. The residual exposure is unchanged and
still the spoke administrator certificate (§6).

### 3. Never back up what another authority recreates

The reference excludes its own backup namespace, its own `Backup` and `Restore` resources, and
CRDs "being recreated by owner resources, which are also backed up". The same rule removes a
whole class of conflict here, because our second authority is Argo CD:

* **Class R is excluded from Class M.** ClusterClass, machine templates, Compositions and
  Applications are rendered from the bundle. Backing them up would give one object two writers
  and reintroduce the ownership race Revision 5 §3 closed.
* **The successor's own Velero objects and namespace are excluded**, so a restore cannot rewrite
  the mechanism performing it.
* **`MachineHealthCheck` is excluded** (Revision 5 §5), and remains absent until classification
  ends.
* **CNPG objects and persistent volumes are excluded.** They are Class B, and Barman owns their
  archival and recovery (ADR-014, ADR-092, ADR-112). Velero never backs up a platform database.

### 4. Stale restored state

A successor that restores more than once accumulates objects the current backup no longer
contains. The reference's three cleanup modes are adopted as stated:

| Mode | When |
|---|---|
| none | a fresh successor — disaster reconstruction, where there is nothing to clean |
| restored-only | a repeated or resumed restore: remove what a prior restore left that this backup does not contain |
| broad | removes platform-scoped objects absent from the backup, **including ones a person created** |

The reference gates its broad mode behind an explicit per-operation acknowledgement, separate
from the field that requests it, precisely because that mode can delete what nobody backed up.
**That idiom is adopted verbatim**: destructive breadth is never reachable from a declaration
alone. It is the same reasoning as §3's refusal to let an unverified object be released.

### 5. What this revision does not change

Planned succession and disaster reconstruction remain separate paths (§4, §5). The supported
version combination and the bounds of CAPH disaster adoption remain as §9 records them —
Kubernetes v1.31.6, CAPI v1.10.0, kubeadm providers v1.8.5, CAPH v1.0.7 at contract `v1beta1`,
with the target combination recorded there when the upgrade track selects it. ADR-112 continues
to own per-application clusters, their sizing and their Barman archives. No release workflow
changes, and no gate is added.


## Revision 7 (2026-11-06): the hub is environment-independent

A hub that declares an environment is a hub that belongs to one. ADR-037 clause 2 said so
explicitly -- each environment maps to a physically isolated hub -- and the configuration
followed: the hub's registry entry carries `environmentSlug`, and nine separate things read it.

That model costs a production-grade management plane per environment, and it buys an isolation
guarantee the repository gates were already providing. One shared management hub replaces it.

**This completes a decision already taken.** ADR-051's Amendment 2026-09-18 removed the
environment from the box's DNS, on exactly this reasoning: a domain of the form
`<environment>.<domain>` made the box's hostnames a function of its environment "and therefore
made the MANAGEMENT cluster have an environment, because it has hostnames". It named what
followed from that -- the box's Infisical slug, nine packaged chart variants, and a spoke
inheriting its hub's environment rather than declaring its own -- and fixed the first of them by
declaring a `subdomain` instead. Revision 7 finishes the list. The configuration half outlived
the DNS half by seven weeks because nothing forced them to move together; that is the finding,
not the decision.

What follows records what environment-independence means for the hub's own configuration,
because removing the field is the smallest part of it.

### 1. Four concerns, four declarations

`environmentSlug` on the hub was answering four questions at once. Separating them is the whole
decision; everything below follows from it.

| Concern | Declared by | Authority |
|---|---|---|
| Management identity | `boxId` | §1 of this ADR, unchanged |
| Management secret scope | `management.infisicalEnvironmentSlug` | this revision |
| Management availability and capacity | the hub's own management-plane configuration | this revision, §3 |
| Workload environment | the managed cluster's own `environmentSlug` | ADR-037 clauses 1, 3-8 |

**No new identity field.** `boxId` already is the hub's stable identity and already keys the
escrow path and DNS record ownership (§1). A management-plane concern that needs to name the hub
names `boxId`.

**The hub does not declare `environmentSlug`.** Not as an empty value, and not as
`environmentSlug: management`. Reusing the key with a management value would leave one field
answering two questions -- a secret scope and a workload environment -- which is the defect this
revision exists to remove, and it would be re-conflated by the next reader who sees a slug and
assumes it names an environment. The field is absent from the hub's configuration, so a reader
that needs an environment has to say whose.

### 2. The management secret scope

The hub reads its own credentials from a dedicated Infisical environment, declared as
`management.infisicalEnvironmentSlug` and set to `management`. It is the hub's scope and nothing
else's: it is not a fourth workload environment, nothing is promoted into or out of it, and no
managed cluster reads it.

Every site that resolved it from the hub's `environmentSlug`, or from a literal of its own, now
resolves it from this field: the `ClusterSecretStore` patch in the environment-manager
boundaries, the `cluster-secret-store` component's templated field, the `infisical-tenant-mgmt`
store that reads the folder the hub-operator writes the owner's credentials to, and the tenant
`SecretStore` the tenant-platform Composition plants on a cell, which reads
`/spoke-pool/<cell>/tenants/<tenant>/...` -- also hub-operator's writes. The last two carried
`dev` as a literal, which was correct only while the hub's scope happened to be `dev`.

A managed cluster's store continues to resolve from that cluster's own `environment` label, which
is already correct and already carries the recorded reason: a cluster reading the hub's slug
would read the environment of whatever provisioned it.

**Day-0 creates the scope; there is no migration.** Infisical supplies a project with its own
default environments -- `dev`, `staging`, `prod` -- and `management` is not among them, so a box
that declares it would otherwise declare a destination that does not exist. That failure is
silent in the direction that matters: the write succeeds against nothing an `ExternalSecret` can
read, every store resolves empty, and the box reports Healthy while holding no credentials.

So the bootstrap creates the environment before it writes a secret into it
(`ensureManagementScope`, `internal/kakt-cli/infisical/bootstrap.go`), reading the scope from
`hub-bootstrap-config` -- the same key the writers read, so the creator and the writers cannot
disagree. It asks before it writes, so a re-run against a box that already has the environment
performs no write at all, and it runs on the cached path too: a box bootstrapped before the scope
was declared reaches that path with the environment still absent.

That is what makes the declaration self-sufficient: **the scope a box declares is the scope that
exists.** A new box bootstraps directly into `management`; no earlier scope is read, aliased or
kept.

Nothing is copied. The hub's credentials are not data the platform holds for someone else -- they
are written by the hub, and changing the declaration changes the HubEnvironment's spec, which
bumps its generation, which makes `SecretsBackedUp` stale and re-runs the upload phase against
the declared scope. The secrets a box needs are therefore re-written where its readers look, by
the same writer that put them in the old scope, with no compatibility path and no rotation
performed inside a migration.

### 3. Management availability is its own policy

The hub's footprint does **not** become the `prod` branch of the old environment conditionals.
Those branches encode workload-environment assumptions -- replica counts, pod spread and resource
reservations chosen for what a dev or prod *workload* cluster needs -- and inheriting one of them
is the same mistake as declaring an environment, arrived at by a different route.

The hub's sizing is declared explicitly, as management-plane policy:

* **Replicas** follow the management-plane availability design, not a workload environment's.
* **Scheduling constraints** follow this box's measured node capacity and failure domains. A
  constraint that cannot be satisfied does not fail loudly; it pends, and the service it governs
  silently never comes back.
* **Backup configuration** follows this ADR's backup classes (Revision 6 §1), which are already
  independent of any workload environment.
* **TLS issuer** follows the hub's own domain and certificate policy.
* **Resource reservations** follow measured capacity.

This is stated because the obvious reading of "one shared production-grade hub" is "set
everything to three", and on the current box that is unsafe: its worker node sits at 98% of
memory requests with the control-plane node tainted, so moving five CNPG clusters and the
observability components to three replicas yields pending pods, not availability. A hub intended
to manage production must reach suitable availability and capacity -- by a capacity decision made
on measurements, not as a side effect of deleting a field.

### 4. Workload topology is validated per cluster

The supported-topology check stops being keyed by the hub. `supportedMatrix` and
`assertSupported` validate each managed cluster against that cluster's own declared environment
and provider; neither is inferred from the hub. The duplicated matrix in the CLI and the chart
(and the parity test asserting they agree) is unaffected -- what changes is whose environment is
looked up in it.

`kakt tenant add-cluster --environment` remains the entry point for a workload environment.
Hub scaffolding no longer takes one.

### 5. What this revision does not change

Planned succession and disaster reconstruction remain separate paths (§4, §5), and both continue
to key on `boxId` and `clusterName` exactly as §1 defines them. The hub's domain and identity
configuration are unchanged and are not derived from any environment. The supported version
combination stands as §9 records it. ADR-112 continues to own per-application clusters and their
archives. No new operator, control plane, reconciler or release gate is introduced, and no
release workflow changes.
