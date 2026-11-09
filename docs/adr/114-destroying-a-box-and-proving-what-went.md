# ADR-114: Destroying a Box, and Proving What Went

**Date:** 2026-11-09

**Status:** Proposed

**Relates to:** ADR-076 (reaching a box you own; its 2026-11-09 amendment supplies the
credentials this uses), ADR-082 (cluster naming and the environment-driven deployment
amendment), ADR-111 (hub succession and reconstruction), ADR-014 (backups), ADR-051
(DNS ownership)

## Context

Every other ADR in this platform is about bringing something into being. Destruction had
no decision behind it, and the implementation showed it. Three properties of the
teardown that shipped, each found by reading it rather than by a failure report:

**Resources were selected by name.** The matcher that decided which cloud resources to
delete returned true on any of: a name beginning with the box's prefix, a name
*containing* a cluster's name, a label KEY containing it, or a label VALUE containing
it. On this platform a box's clusters share a prefix by construction (ADR-082:
`nutgraf.in` gives `nutgraf-hub` and `nutgraf-01`), so `--tenant nutgraf` made a
MANAGEMENT-cluster teardown delete the workload cluster's servers too — and nothing in
the output distinguished them. Substring matching on a label value is worse: any
resource whose metadata happened to mention the name was eligible.

**A missing credential downgraded the operation silently.** With no kubeconfig resolved,
every `kubectl` read returned empty and the run continued: load balancers were never
drained by the controller that owns them, finalizers were never stripped, DNS ownership
was never read — and the final line said the teardown completed. With no provider
credential it printed "skipping cloud resource cleanup", returned nil, and reported
success while every server kept running and kept billing.

**Absence was inferred from the wrong evidence.** A missing local kubeconfig was read as
"this box is already gone". It means only that this machine holds no file.

The common shape is that each failure produced a *confident* wrong answer. A teardown is
run to make things stop existing and stop costing; an operator reading "completed" has
no further step to take, so a false success is worse than an error.

## Decision

### 1. Ownership is the provider's own statement, and nothing else

A resource is eligible for deletion only when it carries the exact label
`caph-cluster-<cluster>` with the value `owned`, for a cluster in the operation's target
set. That is CAPH's ownership contract (`api/v1beta1/tags.go` at v1.0.7:
`NameHetznerProviderOwned` is the key prefix, `ResourceLifecycleOwned` is `owned`,
`ResourceLifecycleShared` is `shared`).

Four things are therefore never deleted, and each is reported with its reason:

- a resource marked `shared`, because other clusters use it too;
- a resource whose label names a cluster the operation does not target;
- a resource carrying an unexpected lifecycle value, because unknown is not permission;
- a resource with no CAPH label at all — the provider cannot attribute it, so neither
  can this.

An exact key, not a prefix: `caph-cluster-x` must not admit cluster `x-2`. No name, no
prefix, no substring and no account-wide credential is evidence of ownership. Two
narrow exceptions carry equivalent evidence from elsewhere and are named in the code: a
load balancer the in-cluster controller identified by IP, and a volume still attached to
a server this run has just deleted.

`--tenant` is withdrawn rather than deprecated, so a script that passed it stops instead
of quietly losing the scope it believed it had.

### 2. Two modes, and the caller chooses

**Normal.** The cluster's Kubernetes API is reachable. Its own cleanup runs first —
load balancers deprovisioned by their controller, finalizers stripped, DNS ownership
read — and then the provider resources go.

**Infrastructure-only.** The API cannot be reached, and the operator asks for the
recovery path by name, listing the clusters in scope. Only positively owned provider
resources are deleted, and every Kubernetes-dependent operation is reported as
outstanding.

Neither becomes the other by accident. Without access and without the flag the command
refuses and names what cannot run; asking for the flag on a box that *is* reachable is
also refused, because the full teardown is available and the recovery path leaves DNS
records and load balancers behind by construction.

### 3. The provider is asked, and the answer is shown before anything is destroyed

The provider inventory is enumerated for every resource type the deletion path handles —
servers, load balancers, volumes, placement groups, firewalls, floating and primary IPs,
networks — following pagination, so a box larger than one page cannot appear partly
absent. The two lists are the same list on purpose: a kind the deletion path handles and
the inventory omits would be destroyed without having been shown, which is the one thing
this report exists to prevent. That
produces four states, and only one of them deletes:

| Provider inventory | Kubernetes API | Behaviour |
|---|---|---|
| read, resources owned | reachable | normal teardown |
| read, resources owned | unavailable | infrastructure-only, if asked for |
| read, nothing owned | — | **no owned resources found**; nothing deleted |
| not read | — | refuse; nothing deleted |

The third state is deliberately worded as a statement about ownership rather than about
existence. "Already absent" claims more than the query supports: unlabelled or
ambiguous resources may be sitting there, and they are reported separately rather than
described as gone.

The resolved target set is printed before the confirmation gate: the management cluster,
the workload clusters in scope, and every resource by kind, name and immutable id with
the ownership label that admits it — plus every exclusion with its reason, and, in
recovery mode, the work that will not be done. The point is that an operator can see
that another cluster's servers are NOT in the list before deleting, instead of reading
it afterwards in the list of what was destroyed.

### 4. An incomplete teardown says so, in the exit status

Work that could not be performed is recorded and reported, and the command exits
non-zero. A provider deletion that failed is recorded the same way: it is the step whose
failure means the servers are still running.

### 5. A cluster's removal never edits an environment's definitions

Removing a workload cluster removes its own declaration and its deployment instances.
The applications of the environment it served are declared independently of it
(ADR-082, amendment 2026-11-07) and keep running on every other cluster serving that
environment. `kakt tenant remove-cluster` reports what stops running, and refuses only
on a reference that would otherwise dangle — the box's own `burstSpokePool`.

### 6. The management cluster goes last, and does not cascade

**A management cluster is not destroyed while a cluster it manages is still there.** The
run refuses, names what it found and the source that still says so, and deletes nothing.

Three things make this an ordering rather than a preference:

- the workload cluster's CAPI objects live on the management cluster, so once it is gone
  nothing on this platform can delete that cluster — there is no controller to reconcile
  the deletion and no provider credential mounted anywhere;
- its load balancers can only be deprovisioned by the in-cluster controller, whose
  credential arrives by ExternalSecret from the management cluster's secret store, and a
  load balancer orphaned that way is never collected (ADR-046 §23: four of them once
  exhausted a five-load-balancer project quota);
- its backups, secret paths and DNS records are reclaimed through the management cluster,
  and its name is never reused (§7 below), so what is stranded stays stranded.

**The cascade is withdrawn.** A hub teardown read the hub's own CAPI cluster list and
swept those clusters' resources into the same run. That destroys production clusters
nobody named on the command line, by a list the operator first sees in the inventory
report, and when half the sweep fails the management cluster is gone anyway and the
remainder is unreachable. **The clusters a run may destroy are the clusters named on
it**, and a management-cluster teardown names none.

The check is made against two independent sources, because each is blind to what the
other sees:

| Source | Asked | Blind to |
|---|---|---|
| the management cluster's own CAPI inventory | before anything is destroyed, while it answers | a cluster whose CAPI objects are gone and whose servers are not |
| the provider's inventory, for every cluster the box declares in `registry/clusters/` or names with `--managed-cluster` | after the inventory read, before any deletion | a cluster declared and never provisioned |

The declaration is consulted only against the provider, never on its own: a stale
directory for a cluster already destroyed must not block a teardown that is correct. A
cluster the provider says owns nothing is gone for this purpose, stated as what was
established — no resource in the project carries its ownership label.

A run may name the workload clusters it is to destroy, and then it destroys them
first. With the management cluster reachable that is `--destroy-managed`, and the
PLATFORM does the deleting: the box's root Application is removed so nothing
recreates what follows, then each cluster's own Applications are deleted and waited
on, which is what destroys the cluster and lets the in-cluster controllers
deprovision the load balancers only they can. kubefirst sequences its own teardown
the same way, deleting the registry Application with cascade before any cloud
resource (`providers/civo/delete.go`).

In recovery mode the operator names them with `--infrastructure-only` instead,
alongside the management cluster. They are then destroyed **in order**: each named workload cluster first, the
provider re-read, and the management cluster only once nothing carries the others'
ownership labels. Verified, not assumed — a sweep reports its failures and carries on, so
"the sweep ran" is not "the cluster is gone". If anything survives, the management
cluster is left standing deliberately, because it is the only thing that can still
finish the job, and the run exits non-zero naming what remains.

`--managed-cluster` names a cluster for this gate and does not widen what the run
deletes. It was `--spoke`; the platform's vocabulary is management cluster and
workload cluster, and the CLI now has no command, flag or message carrying the
other word.

**`kakt spoke teardown` is withdrawn.** It deleted a workload cluster's resources
straight through the provider, with no way to tell the management cluster it had
done so -- so used while the box was alive the claim was still reconciled and the
platform rebuilt the cluster on new servers, under a name whose secret path and
backup prefix are never reused (§7). Its two sibling commands, which are
reconstruction tools rather than deletions, moved to `kakt cluster restore-pki` and
`kakt cluster match-inventory`.

### 7. What a teardown deliberately does not take

Secrets under the box's and cluster's paths, and database backups under their own
prefixes, survive the infrastructure. A name is never reused, because those two outlive
the cluster that wrote them (ADR-111 §1). A teardown that also destroyed the recovery
material would make "destroy and rebuild" indistinguishable from "lose the box".

## Consequences

**Positive.** A box can be destroyed from any authorized workstation, because the
credentials come from the escrow rather than from the machine that built it (ADR-076
amendment). A management-cluster teardown cannot take a workload cluster's
infrastructure with it. A teardown that could not finish is visible to a script, not
just to a person reading output.

**Negative.** A resource CAPH did not label is never deleted by this command, so a box
whose history includes hand-created infrastructure leaves residue that must be removed
deliberately. That is the intended trade: the alternative is a command that deletes
things it cannot attribute.

**Negative.** Emptying a box is now at least two commands, and a scripted
"destroy everything" has to walk its managed clusters itself. That is the intended
trade: the one-command form destroyed clusters nobody named, and its failure mode left
them undeletable.

**Negative.** Enumerating the provider inventory before deleting costs API calls and
makes a teardown slower to start. It buys the only honest answer to "is it still there",
which a local file cannot give.

## Impact

- `kakt teardown` gains `--infrastructure-only` and `--destroy-managed`, and loses
  `--tenant`. It refuses while a cluster it manages is still there, and the clusters it
  destroys are the ones named on the run -- `--managed-cluster` (formerly `--spoke`) is
  evidence for the gate, not authorization.
- `kakt spoke` is gone: `spoke teardown` withdrawn, `restore-pki` and
  `match-inventory` now under `kakt cluster`. No kakt command, flag or message uses
  the word. The identifiers that remain are stored or published names -- the
  `SpokePool` claim kind, `/spoke-pool/<cluster>/` secret paths, `spoke-pki-*` escrow
  artefacts, `manifests/spoke/` and `spokepool-clusterclass-v1.yaml` -- where a rename
  is a migration of live boxes rather than a word change, and is not done here.
- `scripts/lib/teardown.sh` already sequenced the workload cluster before the hub; it now names
  the declared pool on `--destroy-managed`, so the platform destroys it before the
  management cluster, and no longer releases the claim itself: stripping finalizers
  stopped the reconciler and the platform's own deletion with it.
  `scripts/dev/local-e2e.sh clean` and the `TEARDOWN=true` rebuild path do the same.
- Ownership attribution, inventory, state classification and the target report are
  the one rule, stated once in `inventory.go`.
- The provider credential is read from the escrow (ADR-076 amendment), so
  `HCLOUD_TOKEN` is an override rather than a convention.

## References

- CAPH v1.0.7, `api/v1beta1/tags.go` — the ownership label and its lifecycle values
- ADR-076 amendment 2026-11-09 — where the credentials come from
- ADR-082 amendment 2026-11-07 — why removing a cluster edits no application
- ADR-111 §1 — box identity, and what outlives a cluster
