# ADR-115: A Cluster Is Accepted, or It Is Not

**Date:** 2026-11-19

**Status:** Proposed

**Relates to:** ADR-039 (ownership model), ADR-040 (Day-0 vs Day-1), ADR-021 (boundary-driven
GitOps), ADR-047 (the tenant deployment contract, which makes spoke provisioning
asynchronous), ADR-055 (boundary activation and per-boundary readiness), ADR-072 (declarative
cluster lifecycle), ADR-079 (phases declare their re-entrancy), ADR-045 (generated artifacts),
ADR-046/ADR-050/ADR-051 (the validation suites this governs)

## Context

The platform has two kinds of evidence about a cluster and treats them as one.

`kakt verify` asserts five invariants: nodes Ready, secret stores valid, ExternalSecrets
synced, Applications Synced and Healthy, pods Running or Completed. Its own definition
describes them as the invariants a hub must satisfy *before it is serving*. That is
convergence: the cluster's own reported state, read from its own API.

Separately, 35 live-cluster validation modules exist under the validation runner's cluster
phase. Each encodes a failure that previously reported healthy while being broken, which is
the only reason any of them exists. The released CLI carries all 35 as embedded content, and
no Go code invokes them.

The suite is reached through one wrapper, which marks its entire platform phase inapplicable
when the box declares no spoke, records that phase as skipped, and exits zero while printing
`result: incomplete`. The Day-0 command reads that zero as success. The distinction between
"passed" and "incomplete" is computed and then discarded by the exit status one line later.

Observed on a tenant's box, 2026-11-19: Day-0 reported success, 58 of 58 Applications were
Synced and Healthy, `kakt verify` passed all five checks in seven seconds, and two public
surfaces were broken. A DNS record for a platform hostname resolved to a management cluster
replaced weeks earlier, and a gateway redirect rule declared in the manifests was never
programmed, so browser traffic to the authentication host returned 404 while its machine
surfaces answered correctly. Zero of the 35 modules ran. Both faults are within the scope of
modules that exist today.

Two further facts bear on the contract. Day-0's completion is coupled to spoke readiness: the
command iterates every declared spoke pool and fails when one is not ready, which contradicts
ADR-047's statement that a spoke takes longer to build than Day-0 runs. And two final
validation runs exist, one at the end of the inner bootstrap and one in the outer command,
both writing the same report file, so the authoritative record depends on which ran last.

## Decision

**1. Convergence and acceptance are different claims, and only one of them means the cluster
does its job.**

Convergence is the cluster's own reported state. Acceptance is the behaviour asserted from
outside it. Provisioning completion is neither: it establishes that infrastructure exists.

A claim of success for a cluster means acceptance. Convergence remains a separate, useful
result and is never reported as acceptance.

**2. The unit of acceptance is one cluster, never a box.**

Each module declares the cluster its assertions are about:

| Scope | Subject | Assessed |
|---|---|---|
| hub | the management cluster alone | once |
| spoke | one workload cluster | once per workload cluster |
| every-cluster | the same assertion, for each cluster the box has | once per cluster, management included |
| path | one assertion spanning two clusters end to end | once per management-and-workload pair |

The four scopes exist because two different shapes were previously treated as one. A module
asserting that every cluster's alerting has its credentials, or that every database on the box
is backed up, is not asserting a relationship: it is one property repeated per cluster, and it
has subjects on a box with no workload cluster at all. A module asserting that a public
hostname reaches a workload on another cluster is a path, and it has no subject until both
clusters exist. Merging the two is what caused per-cluster assertions to be skipped on a
management-only box.

Scope is a property of the assertion, not of the cluster an assessment happens to point at. No
module substitutes one cluster for another to produce a result, and a module whose subject
changes with an environment variable is split rather than classified.

**3. A cluster occupies one of six states.**

| State | True when |
|---|---|
| declared | the tenant's repository holds the cluster's declaration |
| claimed | the management cluster holds the cluster's claim object |
| provisioned | the claim reports Ready and Synced |
| registered | the management cluster holds the cluster as a deployment destination |
| converged | everything declared for the cluster is Synced and Healthy on it |
| accepted | every applicable mandatory module passed in one complete assessment |

The states are ordered and nothing skips one. Registration is load-bearing and is the state
most easily mistaken for completion: until a cluster is registered, the generators that
produce its workloads match nothing, so the cluster has nodes and no platform services while
no condition anywhere describes the gap.

A declared cluster that is not yet assessable is **pending**, and the assessment records which
of three reasons applies: not claimed, provisioning, or not registered. Pending is neither a
pass nor a failure. The tenant asked for the cluster, so it cannot be inapplicable; nothing
about it is established, so it cannot be accepted.

**4. Applicability derives from declarations. Observed resources never establish it.**

A module applies when the tenant's declarations give it a subject: the cluster declarations,
the capability selections, and the provider the cluster declares. A capability that is off
makes its modules inapplicable. A capability that is on and unhealthy makes them fail.

A missing runtime resource never makes a module inapplicable. A missing resource is frequently
the defect the module exists to detect, and inapplicability derived from absence is how a
broken box reports success.

**5. A module reports exactly one of four results, and two of them are not passes.**

`passed`, `failed`, `inapplicable`, `unverified`.

`unverified` is a module that applies and could not run: missing credentials, an unreachable
subject, an absent prerequisite. A cluster with an unverified mandatory module is not accepted,
because nothing established what that module asserts.

Two cases are defined as `unverified` rather than left to interpretation: a module that
applies and executes zero assertions, and a module whose subject could not be identified. An
assessment in which no module applies **fails** as an invalid assessment.

Advisory findings remain distinct from mandatory assertions and never block acceptance.

**6. A check's name is a user interface, and three of them must change.**

Modules are selected by name, derived from the file name. Those names appear in the
documented command surface, so the terminology rules that govern customer documentation
govern them: `fleet-pki-issuers` becomes `certificate-issuers`, `hub-placement-capacity`
becomes `management-cluster-capacity`, and `class-m-coverage` becomes
`management-state-backup-coverage`, which is the vocabulary the customer documentation
already uses for that backup. No caller selects any of the three by name today, so the
rename breaks nothing.

The selectable names are discoverable from the executable. An assessment that explains
itself lists every check with the declaration that makes it apply, which is what makes the
documented list a reference rather than the authority.

**7. A subset never certifies the whole.**

An assessment restricted to named modules establishes the behaviours it ran. It does not
produce acceptance, does not publish an acceptance result, and does not advance any completion
state. The same applies to an assessment that only explains what would run: explanation
asserts nothing and mutates nothing.

**8. One authoritative assessment, at the end of Day-0.**

Acceptance runs after the generated artifacts are committed, after every management boundary,
after artifact reconciliation is verified, and after convergence. The commit ordering of
ADR-045 is unchanged: boundary 04 consumes those artifacts through GitOps, so they are
committed before it, and acceptance evaluates the reconciled platform rather than uncommitted
desired state.

A failed assessment fails the Day-0 run. It preserves provisioning checkpoints, preserves
committed artifacts, records the failing module and what it observed, and permits a later
assessment of the same cluster without rebuilding it.

Exactly one final assessment exists per run. The duplicate final validation in the inner
bootstrap path is removed once the outer path is proven in the released executable. Preflight
checks and in-flight boundary gates are unaffected, as are phase postconditions.

**9. Hub acceptance does not wait on spokes, and pending spokes stay visible.**

The management cluster is accepted on its own evidence. Day-0 does not block on asynchronous
spoke provisioning, which ADR-047 establishes is slower than the run.

The box's aggregate verdict is reported as follows:

| Verdict | Condition | Exit |
|---|---|---|
| accepted | every declared cluster is accepted | zero |
| incomplete | the management cluster is accepted and a declared spoke is pending | zero |
| failed | an applicable mandatory module failed or was unverified, on any cluster | non-zero |

A spoke that reaches **registered** and then fails, or cannot be assessed, fails the verdict.
A spoke that never reached registration is pending, with its reason, and does not. A spoke
named explicitly for assessment while not registered fails rather than being skipped: naming
it made the inability to assess it the answer.

A failing spoke never retroactively converts an accepted management cluster into a failure,
and an aggregate verdict never reports every cluster accepted while one is pending.

**10. One acceptance implementation, and it is the one that exists.**

The validation runner and its modules remain the acceptance mechanism. The Go health waiter
remains the convergence mechanism. The CLI invokes the embedded runner; the modules are not
reimplemented in Go, and no second validation engine is introduced. The released executable
carries the modules, so no platform source checkout is required on a tenant's runner.

**11. The assessment is a point-in-time record, and it does not acquire a second writer.**

An assessment is evidence about a revision at a moment, not continuously observed health. It
records the target cluster and its role, the owning management cluster where applicable, the
bundle version and the committed revision assessed, the convergence result, every module's
applicability and result, the counts, the reason for every omission, and the verdict.

`HubEnvironment` status remains owned by its controller (ADR-039). The verification command is
never a second writer to it. Where an acceptance condition is surfaced through the platform's
status API, the controller consumes the recorded assessment under a single-writer contract, and
the condition identifies the revision and time assessed so that a historical assessment is
never read as current health.

## Ownership

| Resource Class | System of Record | Lifecycle Owner | Reconciler | Consumer | Phase |
|---|---|---|---|---|---|
| Acceptance assessment, per cluster | the assessment report produced by the run | `kakt verify` | none; a point-in-time record is not reconciled | operators, CI, release decisions | Day-0 and Day-1+ |
| Module applicability | the tenant's repository: cluster declarations, capability selections, provider | the tenant | the GitOps engine | the validation runner | Day-1+ |
| Convergence invariants | each cluster's own API | the platform's controllers | each owning controller | `kakt verify` | Day-0 and Day-1+ |
| Acceptance condition, where surfaced | `HubEnvironment.status` | hub-operator | hub-operator | operators | Day-1+ |

## Consequences

### Positive

- A success claim means the cluster's declared behaviours were asserted, so a green run and a
  working box stop being independent facts.
- The 35 modules the released CLI already carries become reachable on every box, including a
  box with no workload cluster, where today none of them runs.
- Pending, inapplicable and unverified become distinguishable from passed, which removes the
  reporting shape that let a broken box read as complete.
- Acceptance is re-runnable against an unchanged cluster, so correcting a defect is verified
  without rebuilding infrastructure.
- Spoke acceptance is reportable per cluster, which the current coupling prevents.

### Negative

- Day-0 takes longer and fails more often, both by design. A run that previously completed in
  four minutes over an unasserted box now spends minutes probing real surfaces.
- Acceptance probes behaviour, so it creates and removes short-lived canary resources. Those
  must be uniquely named, non-sensitive, and cleaned up; a probe that mutates production
  material is prohibited.
- Modules whose assertions mix scopes require splitting before they can report per cluster,
  and until a module is classified its result cannot be attributed to one cluster. Four of the
  modules that read two clusters today are per-cluster assertions rather than paths, and
  reclassifying them makes them run where they previously did not.
- A tenant's first run on a box that declares a spoke now reports `incomplete` rather than
  success, which is accurate and is a change in what a green pipeline looks like.

## Impact

Amends the Day-0 completion contract: hub success no longer depends on declared spoke
readiness, and the duplicate final validation is removed in favour of one authoritative
assessment.

Amends the validation suites' invocation (ADR-046, ADR-050, ADR-051): applicability moves from
the suite to the module, and an assessment that ran nothing no longer exits zero.

Leaves unchanged: ADR-045's requirement that generated artifacts are committed before their
GitOps consumers, ADR-047's asynchronous spoke contract, ADR-055's per-boundary readiness,
ADR-079's phase postconditions, and ADR-039's single System of Record for every resource.

Customer-facing documentation renders these states and results in the vocabulary that
documentation requires, where a management cluster is never called a hub and a workload
cluster is never called a spoke. The contract is this ADR; the rendering is not a second
source of truth for it.

## References

- ADR-039: Platform Ownership Model
- ADR-040: Day-0 vs Day-1 Lifecycle Boundary
- ADR-045: Generated artifacts and their reconciliation
- ADR-047: The tenant deployment contract, and asynchronous workload-cluster provisioning
- ADR-055: Boundary activation
- ADR-072: Tenant-controlled Day-0 and declarative cluster lifecycle
- ADR-079: Day-0 phases declare their re-entrancy
