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
servers, load balancers, volumes, placement groups, firewalls, floating and primary IPs
— following pagination, so a box larger than one page cannot appear partly absent. That
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

### 6. What a teardown deliberately does not take

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

**Negative.** Enumerating the provider inventory before deleting costs API calls and
makes a teardown slower to start. It buys the only honest answer to "is it still there",
which a local file cannot give.

## Impact

- `kakt teardown` gains `--infrastructure-only` and loses `--tenant`.
- Ownership attribution, inventory, state classification and the target report are
  shared with `kakt spoke teardown`, which carried the same unsafe matcher and now
  imports the rule rather than restating it.
- The provider credential is read from the escrow (ADR-076 amendment), so
  `HCLOUD_TOKEN` is an override rather than a convention.

## References

- CAPH v1.0.7, `api/v1beta1/tags.go` — the ownership label and its lifecycle values
- ADR-076 amendment 2026-11-09 — where the credentials come from
- ADR-082 amendment 2026-11-07 — why removing a cluster edits no application
- ADR-111 §1 — box identity, and what outlives a cluster
