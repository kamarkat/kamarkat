# ADR-063: The Platform Bundle and its Version

**Date:** 2026-09-07
**Status:** Proposed

*Amended by: ADR-064 (Bundle Promotion and Cell-Scoped Policy), ADR-066 (The Platform Boundary)*

> How a version travels to a cluster is ADR-064. What a version certifies -- cluster
> machinery every box runs, against capabilities a tenant selects -- is ADR-066.

## Context

The platform is delivered as a set of third-party components — Crossplane, ArgoCD, Kyverno, External Secrets, CloudNativePG, cert-manager, Cilium, two Crossplane providers and several charts besides. Nothing in the decision record says how their versions are chosen, where they are recorded, or what it means to upgrade them.

In practice the versions are scattered across five kinds of location: component descriptors under `manifests/argocd/components/`, inline elements in the boundary ApplicationSets, a standalone `application.yaml`, Crossplane `Provider` packages, and a Go constant in `internal/hub-cli/versions/versions.go`. Nothing states that they are related, so nothing prevents them being changed one at a time.

Three facts about the current arrangement bear on this decision.

**A version pair already exists that nothing enforces.** `versions.go` pins the ArgoCD chart the Day-0 seed installs, and a descriptor pins the chart GitOps reconciles. They must be equal, and they drifted — 7.7.12 against 7.8.0 — until a validator was written to compare them. That validator exists because the coupling was discovered by failure, not because the coupling was recorded.

**A second such pair was found the same way.** `provider-kubernetes` v1.x runs only on Crossplane v2: v1.0.0 moved it to `crossplane-runtime/v2` and added a namespaced API tree. The package declares no core-version constraint, so pairing it with a v1 core installs cleanly and then misbehaves. The absence of a declared constraint was briefly taken as evidence of compatibility, which it is not.

**A bundle selector already exists.** `environmentRevision` is set once during Day-0 and flows into every boundary ApplicationSet — all nine templates, eighteen references — supplying `targetRevision` for every platform-owned Application and the revision for every descriptor-reading git generator. One value already selects the revision of the entire platform. It resolves to `"main"`.

### What a comparable platform does

kubefirst was examined because it builds the same kind of thing by the same means, for a different customer: one with a platform engineering team that will operate and customise what it is handed. The mechanisms are comparable; the customer is not, which is why some of what follows is adopted and some is rejected (ADR-071).

Its CLI version is injected at build time (`-X configs.K1Version=v{{.Version}}`) and used directly as the tag of a separate `gitops-template` repository (`GitopsTemplateBranch = configs.K1Version`). Inside that template, every component pins an explicit chart version at the leaf, while the intermediate Applications point at the consuming repository at `HEAD`. The CLI version therefore names a complete, fixed component set.

Its current release ships Crossplane 1.17.0, External Secrets 0.8.1, cert-manager v1.14.4 and external-dns 1.14.4 — every one substantially behind the latest of its line, at a time when Crossplane 2.4.0, External Secrets 2.10.0 and cert-manager v1.21.1 are all available. That is not neglect. It is the observable consequence of shipping a set that was tested together rather than the newest of each component.

The codebase contains no upgrade machinery of any kind. The only occurrence of "upgrade" is a message telling the operator to update the CLI. The unit of change is the bundle.

**Two separable ideas are bundled in their design.** `gitops-template` carries 52 distinct placeholder tokens — `<CLUSTER_NAME>`, `<GITOPS_REPO_URL>` and others — because the CLI clones it, substitutes them, and pushes the result into each customer's own git organisation. That copy exists because each customer ends up **operating** the cluster it describes, and so owns the copy; it solves a hand-over problem. Separately, and independently of the hand-over, each cluster ends up with its own pinned version that can be advanced on its own. The second idea does not require the first. ADR-064 adopts the per-cluster pin and does not adopt the copy, for the reason recorded there: here the platform operates the infrastructure.

## Decision

**The platform is a bundle, and its version is a tag.**

A bundle is the complete set of platform content at one revision of `zero-ops`: every boundary, every component descriptor, every chart version, and the Day-0 constants that must agree with them.

What a version certifies is one substrate and each capability on it, not every combination of capabilities. The machinery every box runs is tested as a unit, because it is only ever deployed as one; a selectable capability is tested against that substrate at that version and declares what it requires of it. Ten independently selectable capabilities are a thousand combinations, and a version claiming to have exercised them all would assert something nobody could check. Where two capabilities genuinely interact, that pair is declared and certified as a pair (ADR-066).

### The bundle is published as a versioned chart

A bundle is built from `zero-ops` at a tag and published as a Helm chart in an OCI registry, versioned with the same string. One semantic version therefore names three things that must not diverge: the revision the bundle was built from, the chart published from it, and what a cluster runs.

The version is a tag rather than a branch because a tag names a set that was tested together, where a branch names whatever was merged most recently and cannot be reproduced. It is a published chart rather than a repository reference because what a cluster reconciles should not depend on the availability of the platform's source control, and because a registry is the mechanism built for one artefact to be resolved by many consumers.

A cluster consumes the bundle by naming a chart version and supplying values, both held in the `<tenant>-gitops` repository ADR-062 establishes. Nothing of the bundle's content is copied there: the version and the values are the tenant's to hold, and the content behind the version is the platform's to publish.

Default values belong to the chart and are published with it. What a cluster supplies is only what deviates from them. A default can therefore be corrected in a new version without a proposal into any tenant's values file, and a tenant that has overridden a field keeps its override across the change.

A working branch is a legitimate bundle for a cluster being built rather than run, and a published version is what a cluster that is run requests. Which of the two a given build is, and how it knows, is ADR-068: the build carries its version, and an unreleased build carries none.

Where that version is recorded, and how a cluster moves from one to the next, is ADR-064. This ADR settles what a version names; that one settles how it travels.

### The distribution is one artefact; topology is configuration

A version names one published artefact. The components it contains are internal structure, not separately published products, and the environment and provider a cluster runs are values supplied to it rather than names it resolves.

The first implementation published a chart per component and then a chart per environment and provider combination, reaching fifty-five artefacts. Measuring two of them settled it: the spoke catalogue published for `dev+hybrid` and for `prod+hetzner` differed by thirty lines out of two hundred and three thousand, and those thirty lines were the placement pair ADR-046 §11 defines -- a `workload-location` nodeSelector and a `storageClassName`. Six twelve-megabyte artefacts existed to express a nodeSelector.

**An artefact is published separately only when it is independently installable, independently versioned, independently supported, and able to change without the platform bundle moving. All four, or it belongs inside the distribution.**

An environment and provider combination meets none of them. It is not installed on its own, it carries no version of its own, no support commitment attaches to it, and it cannot change without the bundle changing. Publishing it as an artefact makes a deployment dimension into a product identity, and the registry then describes the platform's internal topology rather than what the platform sells.

The same test explains why a capability could one day qualify and a combination never will. If a capability is genuinely installed, versioned and supported on its own, it is a product and may be published as one; until that is true of it, asserting it through packaging claims a lifecycle boundary that does not exist.

Deduplication follows rather than motivates. Those six artefacts held the same vendored CRDs six times, seventy megabytes of the seventy-seven the release published; carrying the variants as content inside one artefact leaves about eighteen. That the count and the size fall together is a consequence of the boundary being drawn correctly, not the reason for drawing it.

The cost is that a cluster resolving any part of the platform resolves the whole distribution, because a chart reference names a chart and not a subchart within one. Every Application therefore renders the full chart and uses its own part. That is real work repeated per Application, and it is accepted: the alternative buys a smaller render by asserting product boundaries the platform cannot yet support, and a boundary asserted early is harder to withdraw than a render is to make cheaper.

### A published version remains resolvable

A version names a bundle a tenant is running, so it is withdrawn from neither the tenant nor the record. Published versions are immutable: a version is never re-published, re-pointed or deleted, and a defective bundle is superseded by a new version rather than corrected in place.

A published bundle carries everything reconciling it requires, including the component declarations that name each third-party chart and its version. A runtime reference from a published bundle back to the platform's repository fails the release rather than being reported: a bundle carrying one is not mirrorable, and shipping it would make the custody claim above false for every tenant that received it. Those declarations are discovered from this repository today, so a cluster reconciling a published bundle would still depend on the platform's source control being reachable at the revision they were written at — and the bundle would be portable only in appearance. Travelling with the bundle is what makes one artefact enough.

Immutability alone is insufficient. A tenant's repository holds a version and its values, not the content behind them, so what its clusters run is reachable only while that artefact resolves.

**Every artefact required to operate a tenant's subscribed platform is mirrored into infrastructure the tenant controls before that artefact becomes part of a supported runtime.** Mirroring is a condition of the runtime being supported, not an option the tenant may take. An arrangement in which a tenant *could* mirror and has not is one where custody is asserted and untrue, and the moment it is discovered is the moment the platform has become unreachable.

Two properties follow, and both are release invariants rather than aspirations. A published bundle contains everything reconciling it requires, so nothing a cluster needs is left behind in the platform's own repository. And a release that reaches a tenant's runtime carries a build that tenant can run, because a bundle nobody can install is not custody either.

Mirroring is a first-class operation of the distribution mechanism rather than something the platform must build, which is a further reason to publish rather than to be referenced.

This is what makes the independence ADR-065 claims survive the platform. Revoking an App installation stops proposals arriving and stops nothing running, but only if the content those clusters reconcile is still fetchable. A tenant whose running estate is reachable solely through a service it no longer buys has custody of its clusters and not of its platform.

### A version is consumed by any release that begins publishing it

Publishing is not atomic. Charts are pushed one at a time, and a run can fail after some have been pushed and before the release is complete -- the first release attempt here pushed forty-four charts at 0.1.0 and then failed attaching its assets. The artefacts that reached the registry are real and pullable, so the version already names something.

**A release that publishes any artefact consumes its version, whether or not it completed. The next attempt takes the next version.**

The alternative is to re-run the same version, which re-points tags that already resolve. That is the one thing immutability forbids, and it would be done at the moment attention is on a failure rather than on the invariant, which is when a rule is least likely to be defended.

Deleting the partial artefacts to reclaim the version is also rejected. It makes deleting a published version a routine step, which is the habit immutability exists to prevent, and it cannot be verified after the fact: anything pulled during the window is content the platform would then believe had never existed.

Version numbers are free and gaps in them carry no meaning. A gap is not evidence of a defect, a withdrawal, or a version anyone ran -- it is evidence only that a release was attempted, which is why nothing is served by reusing one.

### The published bundle is authoritative, not the build that made it

Two properties are easily conflated, and the platform guarantees only one of them absolutely.

Runtime reproducibility says that a bundle digest resolves to the same artefact, forever. It is mandatory, and it is what every claim above rests on. A published chart carries its rendered objects with exact image references and declares no dependencies, so a tenant reconciling it contacts no upstream chart repository. What the tenant depends on is the immutable published artefact.

Build reproducibility says that a source revision and the release inputs of the day produce the same bundle again. It is desirable, and it is not absolute. Third-party charts are fetched at package time from their upstream repositories, pinned to exact versions but not vendored into this repository, so reconstructing a bundle years later depends on those repositories still serving those versions. That dependency is on the platform's ability to rebuild, not on a tenant's ability to run.

**The platform guarantees continued resolvability of published bundles, not indefinite ability to reconstruct them from their upstream sources. Release pipelines SHOULD retain the immutable inputs required to reconstruct supported bundle versions.**

Stating it this way is not a weakening. It is what makes the custody claim precise: the artefact a tenant runs is immutable and mirrorable, and the platform's own build inputs are a separate concern that cannot reach into a running cluster. A platform that claimed indefinite rebuildability would be claiming something it does not control, and the claim would fail silently at the moment someone depended on it.

### Component versions are declared once

Every third-party version the platform installs is declared in one place. The existing parity validators assert that the scattered pins — descriptors, inline elements, provider packages, Go constants — agree with that declaration.

This is the mechanism that already exists, generalised. `scripts/validate-argocd-seed-parity.sh` compares one Go constant against one descriptor and would have caught the drift that produced it. The same comparison applied to the whole matrix turns every coupling from something discovered by failure into something a pre-commit hook refuses.

A coupling that cannot be expressed as equal version strings — `provider-kubernetes` v1.x requiring a Crossplane v2 core — is recorded as a constraint beside the versions it constrains, and asserted the same way.

### Upgrades move the bundle, not a component

A component is not upgraded on its own. The bundle is advanced to a new set, that set is exercised, and the result is tagged. What reaches a cluster is a version that existed as a whole before it was deployed.

Publishing a tag is where a release begins rather than where it ends. Under ADR-065 the platform then proposes the new version to each tenant, and under ADR-064 that proposal is a pull request against the tenant's own repository.

This is a statement about what is *released*, not about how work is done. Bumping one component while integrating it is ordinary; shipping that bump alone, to a cluster, as a change in its own right, is what this forbids.

### The bundle includes what a tenant's workloads rest on

A bundle version selects more than the platform's own components. The chart
renders the platform-owned infrastructure a tenant's workloads depend on —
namespaces, RBAC, secret bindings, gateways and TLS — from the same version, so
advancing a cluster's bundle advances that infrastructure with it on the next
reconcile, without touching any tenant's repository.

Which of those a given cluster runs is a value, not a version. ADR-066 separates
the cluster machinery every box runs from the capabilities a tenant selects, and
the platform maintains both; selection is expressed in the tenant's values file
and never in the version, so adopting a capability and upgrading a bundle remain
independent acts.

A bundle version is therefore also a statement about what a tenant's workloads
rest on, and publishing it makes that reproducible on the same terms as the
platform's own components. Because the version is pinned per cluster under
ADR-064, the statement is made one cluster at a time.

### Latest is not a target

The bundle pins versions that work together. That a newer release of a component exists is not a reason to move, and a platform whose components are each individually newest is a platform whose combination nobody has run — including its own upstreams.

The worked example is this repository on 2026-09-07: nine components moved to their respective latest releases in a single change, producing a combination that had no upstream precedent and no local test history. That is the state this ADR exists to make visible, not to permit.

### Alternatives considered

**Per-component upgrades, as now.** Rejected as the release model. It is how the current version matrix was reached, and it produces combinations that were never tested together. Its failures are not confined to the component moved: a stale observation in one provider presented as a spoke that would not become ready, and a diff strategy enabled in one component presented as unrelated Applications unable to render.

**Pinning nothing and tracking latest.** Rejected. It makes every reconcile a potential upgrade and removes the ability to reproduce a cluster, which ADR-042's bootstrap state machine and ADR-045's generated artifacts both assume.

**A separate repository holding the component versions.** Rejected on ADR-062's test. Such a repository would hold the same content, at the same revision, as `zero-ops`, and so is not a distinct type, instance or workload. Per-cluster independence is obtained instead from the version each cluster pins in the tenant's own repository.

**One artefact per environment and provider combination.** Rejected, having been built and measured. It makes the registry grow with the Cartesian product of deployment dimensions rather than with what the platform independently supports, so adding a provider multiplies published artefacts instead of adding one. It also makes every such artefact a thing a tenant can pull and pin on its own, which invites exactly the per-component drift the bundle exists to prevent.

**Vendoring third-party charts into this repository.** Rejected. It would make a bundle rebuildable without reaching upstream, which is a real gain, but it blurs platform source with third-party release inputs and puts megabytes of upstream content under a review nobody performs. It also improves nothing a tenant depends on, because the published chart already carries the rendered objects. Retaining the fetched inputs alongside a release strengthens rebuildability without moving them into the source of record.

**Copying the bundle's content into each tenant's repository.** Rejected. It would make a tenant's repository self-describing, which is a real benefit, at the cost of the property that makes a fleet maintainable: the platform and the tenant would write to the same files, so every upgrade would carry a merge for the platform to arbitrate, and the cost of an upgrade would grow with the number of tenants rather than staying constant. Publishing the content and pinning a version keeps the two writers on disjoint fields.

## Ownership

| Resource Class | System of Record | Lifecycle Owner | Reconciler | Consumer | Phase |
|---|---|---|---|---|---|
| Bundle definition (tag on `zero-ops`) | `zero-ops` | Platform | Release workflow | Cluster instances | Day-1+ |
| Component version matrix | `zero-ops` | Platform | pre-commit validators | Boundary ApplicationSets | Day-1+ |
| Cross-component constraints | `zero-ops` | Platform | pre-commit validators | Platform | Day-1+ |

The version each cluster runs is a separate resource class, owned by the cluster instance and recorded in ADR-064. See ADR-039 for the complete ownership matrix.

## Consequences

### Positive

A cluster can be rebuilt at the version it was built at, because that version names a fixed set rather than a moving branch, and because that version cannot be moved or withdrawn afterwards.

A tenant can mirror every bundle its estate runs, so the independence the platform claims does not depend on the platform continuing to exist.

A tenant's repository holds a version and values and nothing else of the platform's, so an upgrade is a change to one field. The platform and the tenant write to disjoint parts of that repository, which is what allows a fleet of them to be maintained without arbitration.

A version coupling is stated once and asserted mechanically. The two that have been found so far were both discovered by a cluster failing; further ones are refused at commit.

The set that reaches a cluster is one that existed as a whole beforehand. A combination that nobody has run is visible as such before it is deployed rather than after.

Upgrading becomes a bounded, reviewable act with one artefact — a tag — rather than a diff spread across five kinds of file.

### Negative

Component versions will lag their upstreams, sometimes considerably, and that lag is deliberate rather than a defect to be corrected. A security fix in a single component still moves the whole bundle, which is more work than moving the component.

Declaring versions once means a second place to change when adding a component, and a validator that will refuse the addition until both agree. That cost is the point, but it is a cost.

Publishing is a release step the platform does not have today: `manifests/` is applied directly and would have to be packaged as a chart, with a pipeline and a registry behind it.

A published values schema is a public interface, though a narrower one than it first appears: only fields a tenant has actually overridden are load-bearing for compatibility, and the rest can be restructured freely. Renaming an overridden field is still a breaking change requiring migration across every tenant that set it, so the schema needs validation and a compatibility policy from the first release rather than the twentieth.

Every Application resolves the whole distribution to use one part of it, so a cluster with sixty-four platform-owned Applications renders the same chart sixty-four times per reconcile sweep. The transfer is small and cached; the render is not free.

Version numbers will have gaps wherever a release failed part-way, and someone reading the sequence will eventually ask what happened to a missing one. The answer is that nothing did, and that has to be explained each time rather than being visible from the record.

Rebuilding a supported bundle from source depends on upstream chart repositories still serving the pinned versions, so the platform's ability to reconstruct an old bundle is weaker than its ability to keep serving one. No tenant runtime depends on this, and no release gate can detect it in advance.

Immutable tags accumulate, and every tag any tenant still runs is one the platform continues to answer for. A version cannot be retired by deleting it, only by promoting every tenant off it.

Because the tenant template moves with the bundle, a defective bundle reaches the platform-rendered infrastructure of every workload on a cluster at once. The blast radius of a bundle is therefore a whole cluster, which is what makes per-cluster promotion under ADR-064 the mechanism that bounds it, and what argues for exercising a bundle before tagging it rather than for decoupling the template.

## Impact

- **Amends ADR-040.** Day-0's selection of a bundle version is named as such. Its CLI-only authority is unchanged.
- **Amends ADR-045.** Generated artifacts are scoped to the bundle that produced them; a bundle version and the artifacts committed under it are read together.
- **Confirms ADR-062.** A fifth repository was considered for this purpose and rejected on that ADR's own test.
- **Extends the mechanism of `scripts/validate-argocd-seed-parity.sh`** from one pair to the whole matrix.
- **Amends ADR-061.** Component descriptors are discovered from within the published bundle rather than from this repository, so a cluster running a bundle needs nothing the bundle does not contain. How they are declared and what they carry is unchanged.
- **Deferred to ADR-064.** Where a bundle version is recorded, how it advances, who approves it, and how it is withdrawn.
- No change to ADR-021, ADR-042, ADR-055 or ADR-061. Boundaries, bootstrap phases, activation gating and descriptor composition are all *within* a bundle and are unaffected by how it is versioned.

## References

- ADR-021: Boundary-Driven GitOps and Day-0 Choreography
- ADR-039: Platform Ownership Model
- ADR-040: Day-0 vs Day-1 Lifecycle Boundary
- ADR-042: Bootstrap State Machine
- ADR-045: Bootstrap-Generated GitOps Artifacts
- ADR-046: Hybrid Provider Cell (Hetzner Control Plane + Home-Lab Workers)
- ADR-055: Boundary Activation as the Day-0 Gating Mechanism
- ADR-061: Component Descriptors for Boundary Composition
- ADR-062: Onboarding and Scaffolding a Tenant
- ADR-065: The Control Plane Ships Into the Box
- ADR-066: The Platform Boundary
- ADR-068: The Build Declares the Bundle Version
- ADR-064: Bundle Promotion and Tenant Placement
- ADR-071: How This Platform Differs From kubefirst

## Amendment 2026-11-15: a release carries its own orchestration

The binary carries the platform content Day-0 reads. That now includes the
scripts Day-0 *runs*, not only the manifests it reads: the computed runtime
script closure is embedded alongside them.

It had to be fetched before. A tenant's workflow took a second checkout of this
repository at the release tag to get `hub-bootstrap.sh` and the validators, so
"the scripts belong to this release" was a property of a matching ref rather
than of the artefact — and it needed the tenant's CI to be able to read this
repository at all. Now a release is self-contained: the binary, the manifests,
and the orchestration are one immutable thing.

Two consequences worth naming:

- **The embedded tree is generated and not tracked.** It is copied from the one
  authoritative source by `scripts/package/embed-platform-assets.sh`, so a clean
  checkout has no copy and `go build` alone cannot satisfy the directive. The
  entry points regenerate first (`make build`, `make test`,
  `scripts/package/build-cli.sh`). Committing the copy made it a second place a
  manifest could be edited and carried every script twice.

- **The closure is computed, not listed.** `scripts/validate/runtime-script-closure.py`
  derives it from the entry points Day-0 invokes and takes the validator
  directories whole, because `scripts/validate/run.sh` discovers modules by
  glob. A hand-written manifest goes stale on the next validator, and the
  failure is silent in the worst way: `validate-all.sh` *skips* a suite whose
  script is absent, so a release missing one reports success with less coverage
  than it claims. The release gate refuses an incomplete package.
