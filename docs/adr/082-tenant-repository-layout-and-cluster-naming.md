# ADR-082: Tenant Repository Layout and Cluster Naming

**Date:** 2026-09-17
**Status:** Proposed

## Context

ADR-062 establishes that a tenant's repository holds one directory per cluster and
retains the templates from which the tenant renders its next cluster. It does not say
where the declaration that *creates* a cluster lives, nor what clusters are called.

Both were answered by accident, and the answers were wrong.

The declaration that creates a workload cluster was shipped in the published bundle,
as a claim under a path selected by environment and provider. ADR-063 makes the bundle
one artefact delivered identically to every tenant, so the name in that claim was the
same on every box that pulled it. Every identity derived from the name was therefore
also the same. ADR-014 derives the WAL archive prefix from it: barman refuses to
archive into a prefix already holding another database's write-ahead logs, so the first
box to use a prefix worked and every later box had continuous archiving permanently
false, no base backup behind it, and the management cluster's root-of-trust database
running on node-local storage with no copy anywhere. This was observed on 2026-09-17
with an archive holding backups from boxes that no longer existed, while the running
box had never archived a single segment.

The name cannot be made per-box by templating the packaged chart. The packaging
mechanism deliberately refuses to rewrite an object's name, because renaming an object
by a rendered value makes the resource ArgoCD prunes depend on that value — and for a
cluster claim, prune-and-recreate is destruction and reprovisioning.

Separately, the platform's vocabulary drifted. The CLI states that naming follows
kubefirst, where a management cluster runs the platform and workload clusters run
tenant workloads. The manifests instead named clusters by a topology role and encoded
environment and provider into the name, which is how a name ended up describing a
category rather than an instance. ADR-047 already establishes the cell as the unit the
fleet ApplicationSets select on, and the cell id is derived from the claim's name, so
the claim's name and the cell id are one identity that was being written in two
vocabularies.

## Decision

**A tenant repository holds exactly one management cluster and any number of workload
clusters.** Each has a directory named for it, as ADR-062 requires. The management
cluster's directory additionally holds one Application per workload cluster, which is
what brings that cluster into being.

**The declaration that creates a workload cluster is the tenant's, not the bundle's.**
It is hydrated from a retained template into the workload cluster's own directory and
committed by the tenant. The bundle ships the template, never an instance of it. This
follows ADR-071's boundary: the bundle carries capability, the repository carries the
tenant's facts.

**Addressing follows what a resource acts on.** The Application that creates a workload
cluster is addressed to the management cluster, because the workload cluster does not
yet exist to reconcile its own creation. Every Application describing what runs *on* a
workload cluster is addressed to that cluster by name. An Application addressed to the
cluster holding it, when it describes another cluster, deploys the wrong platform to
the wrong place.

**A workload cluster's name is chosen by the tenant, free-form, and is the cell id.**
It is written once, at declaration, and never computed. Names derived from environment
and provider cannot distinguish two workload clusters a tenant runs in one environment
on one provider, and names rendered from values cannot be changed without destroying
what they name. The management cluster's name derives from the tenant's domain, which
is already a per-box fact under ADR-051.

**Every per-cluster identity derives from the cluster's name.** The WAL archive prefix
(ADR-014), the cell id label (ADR-047), and the autoscaler's target are one name
resolved in several places rather than several literals that agree by coincidence.

Alternatives considered. Templating the name in the packaged chart was attempted and is
refused by the packaging mechanism for the reason stated above; the refusal is correct
and settles the question rather than blocking it. Keying the name on environment and
provider was the prior arrangement and cannot express the multiple-cells case.

## Amendment 2026-11-07: an application is deployed to every cluster serving its environment

**The contract, in full, including both of its negatives:**

> A workload cluster declares its `environmentSlug`. Every application defined in that
> environment is deployed to every workload cluster serving that environment.
> **Applications do not select clusters, and clusters do not select individual
> workloads.**

```
cluster.environmentSlug == application's environment directory
```

An application is defined once in its environment and deployed to every workload cluster
serving it. There is no separate placement decision, and there are therefore two shapes
that do not belong in this model and are not coming back:

| Withdrawn shape | Where it was | Why it is excluded |
|---|---|---|
| Application-side placement — `clusterName`, `cellId`, or a `clusters[]` list | `environments/<env>/<app>/values.yaml` | it made the application's own file the placement decision, so an environment's definitions had to be edited whenever its clusters changed |
| Cluster-side workload selection — `workloads[]`, or any per-cluster list of what runs on it | `registry/clusters/<cluster>/values.yaml` | it would make a cluster's declaration a second authority over the same fact, and two authorities over placement disagree silently: an application present in one and absent from the other generates nothing and reports healthy |

Both negatives matter for the same reason the positive rule does. Placement is derived
from one equality, so there is no second place it can be stated, no pair of statements to
reconcile, and nothing to edit when a cluster is added or removed. A cluster's
declaration says what the cluster IS; an application's says what the application IS;
neither names the other.

**Four things this separates, which were previously conflated:**

| Concept | Means | Declared by |
|---|---|---|
| Multiple clusters serving one environment | `dev` is served by `nutgraf-01` and `nutgraf-02` | each cluster's `environmentSlug` |
| Per-application placement | one application on one cluster, another on a different one | **no longer exists** |
| Simultaneous multi-cluster deployment | one definition, one deployment instance per cluster | the equality above |
| Stateful relocation | an application's *data* ends up elsewhere | backup and restore (ADR-014); never a declaration edit |

An application's declaration carried `clusterName` (and `cellId` before that), so the
application's own file was the placement decision. The costs were structural rather than
cosmetic: a second cluster serving an environment received nothing until every
application was edited; removing a cluster meant editing the applications that named it,
which made deleting an environment's definitions look like the way to remove a cluster;
and one application could not be deployed to two clusters at all, because the generated
object carried no cluster in its name and the second instance would have collided with
the first.

**What the generated identity now carries.** One Application per
(environment, application, cluster), named with all three. The boundary ApplicationSets
select `spoke-type: pool` — which is what excludes the management hub, since it has no
spoke-pool claim — plus `environment`, which reaches the Argo CD cluster Secret through
`take-along-label.capi-to-argocd.environment` on the SpokePool composition.

**The generated name is load-bearing and that is why this is written down.** The tenant
ApplicationSets set `preserveResourcesOnDeletion: false`, and the object they generate is
an XR owning a CNPG Database, the application's namespace, its stores and its
credentials. Changing a generated name therefore deletes the previous instance and
everything it owns. Introducing the cluster into the name recreated the existing
development instances deliberately, under the platform's current no-user contract; the
same fact means a future change to that name is a data event, not a rename.

**Two things stay exclusive, and neither is derivable from the inclusive rule above.**

*Secrets* are per deployment. A managed cluster's store reads only its own
`/spoke-pool/<cluster>/`, so a value is supplied once per cluster serving the
environment; `kakt app secrets status` reports per deployment, because a secret present
on one cluster and absent on another is a state a single summary hides.

*Public hostnames* have one owner. Two Gateways claiming one host each order a
certificate for it and each publish the same DNS record, which external-dns then
ignores. So an environment's listeners and certificates aggregate onto one cluster: the
only one serving it, or the one declaring `publicIngress: true`. Two claimants, or none
among several, is refused rather than resolved by choosing.

**Cluster removal never edits an environment's definitions.** `kakt tenant
remove-cluster` reports the environment and its applications — what stops running — and
removes only the cluster's own declaration (ADR-114 decides what destruction then takes). The definitions stay and keep deploying to
every other cluster serving that environment.

## Ownership

| Resource Class | System of Record | Lifecycle Owner | Reconciler | Consumer | Phase |
|---|---|---|---|---|---|
| Workload cluster claim | Tenant repository | Tenant | Management cluster ArgoCD, Crossplane | CAPI | Day-1+ |
| Workload cluster creating Application | Tenant repository | Tenant | Management cluster ArgoCD | Management cluster | Day-1+ |
| Workload cluster name and cell id | Tenant repository | Tenant | — | Fleet ApplicationSets, backup prefix, autoscaler | Day-0 at declaration, immutable after |
| Application placement | Tenant repository | Tenant | — | Boundary ApplicationSets | Derived: every cluster whose `environmentSlug` matches the application's environment (amendment 2026-11-07) |
| Environment public-hostname ownership | Tenant repository | Tenant | — | Shared TLS Gateway aggregate | One cluster per environment: the only one, or `publicIngress: true` |
| Workload cluster template | Platform bundle | Platform | — | Tenant scaffolding | Day-0 |

## Consequences

### Positive

Two boxes cannot collide. Every per-cluster identity is derived from a name the tenant
chose, so an archive prefix, a cell id and a cluster secret are unique by construction
rather than by a suffix someone remembered to bump.

A tenant can run several workload clusters in one environment on one provider, which
the previous naming could not express.

The bundle carries no instance data of this class, which is the property ADR-063 exists
to hold and which this was the last violation of.

A cluster's name is stable for its life, because nothing renders it.

### Negative

Declaring a workload cluster is now an explicit act rather than a consequence of
choosing an environment and a provider. A tenant that expects one to appear will find
none.

The name is unvalidated beyond being a DNS label. A tenant may choose a name that is
accurate today and misleading later, and nothing will correct it, because correcting it
would mean destroying the cluster.

Upgrading a box built before this change is destructive: the Application that shipped
the claim is pruning, so a box must declare its workload cluster in its own repository
before taking a bundle that no longer ships one.

## Impact

ADR-062 is amended. Its statement that the repository holds one directory per cluster
stands and is extended: the directory of a workload cluster also holds the claim that
creates it, and the management cluster's directory holds the Application that applies
that claim.

ADR-014's backup contract is amended. The archive prefix for the management cluster's
database derives from the cluster name rather than a literal path and a manually
incremented server name. The manual increment remains necessary only when one named
cluster is rebuilt from an empty database, which the derivation cannot distinguish.

ADR-047 is unchanged and now has a single vocabulary: the cell id is the workload
cluster's name because the claim's name is the only place either comes from.

ADR-071 is unchanged. This brings the manifests into the parity the CLI already
claimed.

ADR-063 is unchanged and better satisfied: the bundle ships a template for a workload
cluster and no instance of one.

## References

- ADR-014: Platform-Owned Stateful Infrastructure
- ADR-039: Platform Ownership Model
- ADR-040: Day-0 vs Day-1 Lifecycle Boundary
- ADR-047: Fleet Tenant Deployment Contract
- ADR-051: Environment DNS Naming and Public Gateway TLS
- ADR-062: Onboarding and Scaffolding a Tenant
- ADR-063: One Artefact
- ADR-071: How This Platform Differs From kubefirst
- ADR-072: Tenant-Controlled Day-0 and Declarative Cluster Lifecycle
