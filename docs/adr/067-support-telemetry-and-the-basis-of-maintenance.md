# ADR-067: Support Telemetry and the Basis of Maintenance

**Date:** 2026-09-07
**Status:** Proposed

*Constrained by: ADR-065 (The Control Plane Ships Into the Box)*

> Evidence crosses outward under a tenant's grant. No platform component initiates
> a connection into a box.

## Context

Maintenance of the control plane is what the platform sells. ADR-065 places that control plane inside the tenant's box and gives the platform no access to it, recording as a consequence that support, incident response and defect reproduction all depend on what a tenant reports or chooses to share.

Three facts make that consequence load-bearing rather than incidental.

**A maintenance proposal is presently unverified against its target.** ADR-064 has the platform raise a pull request advancing a cluster's bundle version. Whether that version is safe for a given cluster depends on that cluster's state — the versions it runs, the resources it holds, whether its reconciliation is currently healthy — and none of it is legible to the platform. The proposal is reasoned from declarations alone.

**The dependency-update analogy stops here.** A dependency bump is checkable in the repository that receives it. A control-plane upgrade is not: correctness is a property of the cluster, not of the declaration, so the mechanism ADR-064 borrows does not bring its verification with it.

**The export mechanism already exists.** ADR-013 establishes Grafana Alloy forwarding metrics and logs by `remote_write` to an endpoint supplied by configuration, with credentials delivered through External Secrets. It runs egress-only against a destination outside the cluster today. What is missing is not a mechanism but a decision about destination, scope and obligation.

## Decision

**A maintenance claim extends exactly as far as the telemetry a tenant exports.**

### Telemetry is exported by the box, never collected from it

Observability for support is egress-only and originates inside the tenant's box. The box sends; the platform receives. No platform component initiates a connection into a tenant's cluster, holds a credential for one, or queries it.

The tenant holds the destination and the credential. Withdrawing them stops the stream and stops nothing running, which is the same property ADR-065 gives to the repository access behind maintenance proposals: what the platform may do is granted by the tenant and revocable by the tenant, and revocation degrades service rather than operation.

### The scope is the control plane, not the tenant's business

What is exported is what a maintenance obligation requires: the health and reconciliation state of the control plane, the ADR-063 bundle version and the component versions it resolved to, drift and policy violations, and the outcome of upgrades. It is bounded and its volume is knowable in advance, because a tenant pays the egress that carries it. That is the evidence a proposal is reasoned from and the evidence that it worked.

Workload data, application logs, database contents, secret material and end-user identifiers are not exported for support. A tenant's business is not observable to the platform, and a support obligation is not a reason to make it so. Where a tenant wants workload observability, that is a workload under ADR-066 and is the tenant's to run.

### A proposal is gated on evidence from its target

An upgrade proposal is accompanied by a pre-flight verdict produced inside the box, against the cluster the proposal targets, and reported through the same export. A proposal that cannot be verified against its target is raised as unverified and says so.

This closes the gap the dependency-update analogy leaves open. The judgement about whether a bundle is safe for a cluster is made where the cluster is, and only the verdict crosses the boundary.

### What is not observed is not supported

A tenant that exports nothing receives the bundle and the proposals and no undertaking about the outcome on its clusters. This is stated rather than implied, and it is the honest form of the alternative — a maintenance claim over a cluster whose state is unknown is a claim that cannot be met.

Support obligations therefore follow the export, and are graded by it rather than by a tier bought independently of it.

### Alternatives considered

**Grant the platform read access into tenant clusters.** Rejected. It reintroduces standing access into every box, which is the property ADR-065 exists to remove, and it makes the platform a holder of credentials for every tenant's cluster — the concentration that ADR's cross-tenant reasoning rules out.

**Support on tenant-reported information alone.** Rejected as the basis of a paid obligation. It makes every incident begin with a request for evidence, defect reproduction depend on a tenant's diagnostic skill, and an upgrade's outcome unknown to the party that proposed it.

**Export everything the box observes.** Rejected. It places a tenant's application and business data in the platform's custody for no maintenance purpose, and creates a retention and disclosure obligation disproportionate to what support requires.

## Ownership

| Resource Class | System of Record | Lifecycle Owner | Reconciler | Consumer | Phase |
|---|---|---|---|---|---|
| Control-plane telemetry | tenant's box | Tenant | Collection agent | Platform | Day-1+ |
| Export destination and credential | tenant's box | Tenant | ESO | Collection agent | Day-1+ |
| Pre-flight verdicts | tenant's box | Tenant | Collection agent | Platform | Day-1+ |
| Received telemetry | Platform | Platform | — | Platform | Day-1+ |

See ADR-039 for the complete ownership matrix.

## Consequences

### Positive

The maintenance claim becomes verifiable. The platform proposes an upgrade knowing the state of the cluster receiving it and learns whether it succeeded.

A proposal is gated on evidence from the cluster it targets, so the failure mode where a correct bundle meets an unhealthy cluster is caught before merge rather than after.

The boundary ADR-065 draws survives. Everything crosses outward, under a grant the tenant makes and can withdraw, and no credential into a tenant's cluster exists to be compromised.

What support covers is derivable from what is exported, so the obligation can be stated precisely instead of promised broadly.

### Negative

The platform holds tenant operational data and acquires the retention, access and disclosure obligations that follow, including for tenants in regulated jurisdictions.

Telemetry becomes a dependency of the upgrade path. A broken export presents as an estate that cannot be safely advanced, and the pre-flight runs in the box, so a defect in it is present across the field.

A tenant that exports nothing is supportable only on a reduced basis, which is a commercial conversation at the point of sale rather than a technical setting.

Pre-flight verdicts are produced by the tenant's own control plane, so the platform is acting on evidence it did not gather and cannot independently confirm.

## Impact

- **Extends ADR-013.** Its collection pattern gains a destination outside the box, carrying control-plane telemetry only.
- **Confirms ADR-065.** Telemetry is egress-only under a revocable grant, so the platform gains evidence without gaining access.
- **Constrains ADR-064.** A promotion proposal carries a pre-flight verdict from its target, or is marked unverified.
- **Confirms ADR-066.** Control-plane observability is control plane; workload observability is a workload.

## Addendum 1: the mechanism is the Support Agent, not the tenant's own export (2026-09-12)

This decided what support telemetry is for and what it may carry, and left open how it travels. The Context above names ADR-013's Grafana Alloy `remote_write` as an export mechanism that already exists, and that sentence was read as settling the question. It does not.

The cost of the ambiguity was real. Scaffolding was built to collect a tenant's own Grafana Cloud credentials and, finding none, to report that the tenant's maintenance was unsupported — so a tenant's private monitoring account had become a condition of being supported, in a product whose position is that the software is free and only the accountability is sold. Nothing in this ADR said that; nothing in it said otherwise either, and the nearest available channel was assumed to be the intended one.

**ADR-077 names the mechanism: a proprietary agent, shipped inert to every box, enrolled by the tenant, reporting a bounded set of control-plane facts outbound to the platform.** ADR-013's export is the tenant's own observability and carries no support relationship.

Three things this ADR touches are distinct, and collapsing any two produces the defect above:

| | flows | requires |
|---|---|---|
| Platform telemetry | the platform's components to the tenant's own store | nothing; it is theirs |
| Support telemetry | the Support Agent to the platform | enrolment |
| Licensing | — | does not exist in the runtime |

Two clauses above are refined by ADR-077 rather than replaced:

**"The tenant holds the destination and the credential"** remains true and becomes specific. The destination is the platform's ingress; the credential is a one-time enrolment secret the tenant supplies, exchanged for a short-lived credential the agent renews. Revocation is at the platform's end, takes effect within one renewal, and stops the stream without stopping anything running.

**"Bounded and its volume knowable in advance"** is achieved by an explicit field allowlist, not by anonymising an unbounded payload. Hashing a cluster name leaves identifying material in labels, image references, node names, annotations and error strings; the allowlist is the boundary and anonymisation is a control layered on top of it.

Nothing in the original decision is withdrawn. Support obligations still follow the export and are still graded by it; what is now stated is which export.

## Addendum 2: the pre-flight verdict has a mechanism (2026-09-12)

*"A proposal is gated on evidence from its target"* was the clause with nothing
behind it. A proposal arrived as a version bump with release notes, and whether
that version was safe for **this** cluster was a judgement the tenant had no way
to make and the platform had no standing to make for them — it holds no access
to the cluster and under ADR-065 never will.

`kakt proposal preflight` is that judgement, made where the cluster is. The
tenant's own CI runs it on the pull request, in the tenant's runners, under the
tenant's secrets; only the verdict is ever seen elsewhere. It checks that the
release's declared minimum permits the step this cluster is taking, that the
candidate is actually published where this box pulls from, and that the cluster
is reconciling now.

Three outcomes, and the third is the one this ADR named: **pass**, **fail**, and
**unverified**. A check that cannot run returns unverified rather than passing,
because a verdict that silently degrades to "fine" when it could see nothing is
worse than no verdict — it is indistinguishable, to whoever merges it, from one
that looked. A tenant with no kubeconfig in CI gets an unverified proposal that
says what that means, which is the honest answer and not a failure.

Failing exits non-zero so the tenant's own branch protection can act on it.
Unverified does not, unless the tenant asks: blocking every merge on a check
that could not reach the cluster is how a check gets deleted rather than fixed,
and whether to require it is the tenant's rule to set (ADR-065).

`internal/kakt-cli/proposal/`, `cmd/kakt/proposal.go`, and
`.github/workflows/bundle-preflight.yml` in the tenant template.


## Acceptance

What proves the decisions above, rather than restating them. Checked by
`scripts/validate/architecture-consistency.py`, which refuses an ADR naming
proof that does not exist or that nothing runs.

```architecture
acceptance:
  - scripts/validate/cluster/80-support-agent-rbac.sh
  - scripts/validate/cluster/81-support-agent-allowlist.sh
  - scripts/validate/cluster/82-support-agent-independence.sh
  - scripts/validate/cluster/83-support-agent-no-ingress.sh
  - internal/support
  - internal/kakt-cli/proposal
```

Three claims and their instruments. *"No platform component queries a box"* is a
ClusterRole with no read verb (80) and a component with no inbound path (83) —
both properties of the manifests, true whatever the agent's code does. *"Bounded
and knowable in advance"* is the allowlist (81) enforced against the shipped
contract with a fixture cluster full of tenant identifiers (`internal/support`).
*"A proposal is gated on evidence from its target"* is the verdict, including
that a check which could not run never reads as a pass
(`internal/kakt-cli/proposal`).

## References

- ADR-013: Hub-Spoke Observability Architecture with Dual Collection Patterns
- ADR-039: Platform Ownership Model
- ADR-063: The Platform Bundle and its Version
- ADR-064: Bundle Promotion and Cell-Scoped Policy
- ADR-065: The Control Plane Ships Into the Box
- ADR-066: The Platform Boundary
- ADR-077: The Support Agent
