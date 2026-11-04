# ADR-068: The Build Declares the Bundle Version

**Date:** 2026-09-08
**Status:** Accepted

## Context

ADR-063 makes a published version the thing a cluster runs, and allows a working branch as a bundle for a cluster being built rather than run. It does not say how a build knows which of the two it is, and the implementation left that to whoever ran the command.

Three facts bear on the decision.

**A version computed twice is a version that can disagree.** ADR-063 records two version pairs that drifted because each side computed its own, and generalises the parity validator that caught them. A bundle version derived independently by the release pipeline and by the Day-0 CLI would be a third: the pipeline publishes one string, the cluster requests another, and the failure is a chart that does not exist rather than a version mismatch anyone can read.

**Requiring a tag to test makes tagging routine.** ADR-063 states that a tag names a set that was tested together. A workflow in which a tag must exist before a change can be exercised inverts that: tags are created in order to test, so most of them name sets that were not tested, and the property the tag was supposed to carry is gone.

**A comparable platform resolves this at build time.** kubefirst injects its version through linker flags at release (`-X ...configs.K1Version=v{{.Version}}`), so the binary carries it as a constant and nothing recomputes it. A build without that injection carries the literal `development`, and the code branches on it: a development build sources its template from the working branch, a released build sources it from the tag matching its own version. Its release trigger is a GitHub Release rather than a raw tag push, and the same workflow releases the template repository at the same tag, so the coupling between the two is automated rather than remembered. Overriding the template's location without also naming its version is refused rather than guessed.

## Decision

**The build declares the bundle version, and the version declares where content comes from.**

### The version is injected, never derived

A release build carries its version as a constant, injected by the release pipeline from the release being made. Nothing recomputes it — not the CLI, not a script, not a second workflow. There is one derivation, in the pipeline that publishes, and every other component is told.

A build with no version injected carries a sentinel meaning *unreleased*. That is what an ordinary local build is, and it is not an error.

### An unreleased build sources from the working tree

A build carrying the sentinel resolves platform content from the repository at the revision being worked on, which is what ADR-063 already permits for a cluster being built rather than run.

This is what makes testing possible without tagging. A change is exercised by building and running it; a tag is then a statement that what was exercised is being released, which is the meaning ADR-063 gives it.

### The build seeds the version; the cluster owns it

Day-0 writes the build's version onto the cluster once. From then on the cluster's own declaration is what it runs, and no later CLI operation rewrites it.

This is ADR-040's boundary applied to a single field, and stating it is not pedantry. ADR-064 promotes a cluster by changing the version it declares, and the seed can be re-applied after Day-0 — to supply a value discovered later, to resume an interrupted run, or to bring an existing cluster to the current renderer. A re-apply that re-asserted the build's version would silently return a promoted cluster to whatever the operator's binary happened to carry: a downgrade with no diff, no error, and nothing recording what moved it.

### A released build sources from the published bundle

A build carrying a version resolves platform content from the bundle published under that version. The version a cluster requests and the version the pipeline published are the same string because they are the same constant.

### The source is resolved centrally, not declared per component

A component declares what it is. Where its content comes from is resolved once, from the build's version, for every component alike.

The alternative is each component naming its own source, which fixes it at whichever the author chose: a component pinned to a published chart cannot be exercised from a working tree, and one pinned to a path is never exercised as published. The distinction is a property of the build, so it belongs where the build is known.

### Releasing is one act

A release publishes the bundle and produces the build that requests it, from the same version, in the same run. Nothing else creates a published version, and no step is left to be remembered afterwards.

The build it produces is distributable, because under ADR-065 a tenant runs Day-0 itself and can request a published bundle only if it holds a build carrying that version. A release that published charts and no binary would leave the released path reachable only by whoever could produce one, which is the platform — the arrangement ADR-065 exists to remove.

### Overriding a source requires naming its version

A build may be pointed at a different registry or repository. Doing so without also stating the version to request is refused rather than defaulted, because the default would be a version that the overridden source is not known to hold.

### Alternatives considered

**Derive the version from git state in the CLI.** Rejected. It is a second derivation of a value the pipeline already computes, and the two agreeing depends on both implementing the same rules for tags, branches, detached heads and slugging. The mode of failure is a cluster requesting a chart that was never published.

**Publish a prerelease from every branch push.** Rejected. It makes every push a publication, so the registry accumulates a version per commit and the question of which of them was tested is exactly the question a tag exists to answer. It also leaves a cluster's source dependent on CI having run, where the working tree is present already.

**Require a tag before testing.** Rejected, and it is the arrangement this ADR replaces. It makes tags routine and therefore meaningless, and it puts a push to a shared repository between a change and the first chance to observe it.

## Ownership

| Resource Class | System of Record | Lifecycle Owner | Reconciler | Consumer | Phase |
|---|---|---|---|---|---|
| Bundle version constant | release pipeline | Platform | — | Day-0 CLI | Day-0 |
| Published bundle | OCI registry | Platform | Release pipeline | Tenant control planes | Day-1+ |
| Source resolution | `zero-ops` boundary charts | Platform | ArgoCD | Boundary ApplicationSets | Day-1+ |

See ADR-039 for the complete ownership matrix.

## Consequences

### Positive

A change is exercised by building and running it, so a tag is only ever created to release something that was already exercised. The property ADR-063 asks of a tag becomes true by construction rather than by discipline.

One derivation means the version a cluster requests cannot disagree with the version published, which removes the third instance of the failure ADR-063 was written about.

A component is exercised through both paths without being edited, because the path is not one of its properties.

Releasing publishes the bundle and produces the build that requests it together, so a released build cannot reference a bundle that was never published.

### Negative

An unreleased build does not exercise the published path, and the two paths are not the same mechanism: content reconciled from the repository is a directory rendered by Kustomize, and content reconciled from the bundle is a chart rendered by Helm. The platform this decision follows has no such asymmetry — its two modes differ only in which revision the same template is taken from — so the risk here is larger than its precedent suggests and is not confined to packaging.

What bounds it is the assertion that a packaged component renders exactly the objects its repository path renders, run before anything is published. That check is load-bearing rather than a formality: writing it surfaced a component that packaged to an empty chart and reported success, and another whose credential rendered to an empty string.

The sentinel is a value with behaviour attached, and a build that was meant to be a release but was produced without injection is a build that silently sources from a working tree. The release pipeline is the only thing that injects, which is what keeps that from happening quietly.

Two sources of platform content exist and must stay equivalent: the repository at a revision, and the chart published from it. That equivalence is asserted by the packaging checks, and an unasserted difference between them presents as a cluster behaving differently once released.

## Impact

- **Extends ADR-063.** Its allowance for a working branch becomes a defined build mode rather than an operator's choice, and the published version becomes the constant a release build carries.
- **Amends ADR-064.** The bundle version a cluster runs is written by Day-0 from the build's own version, so a promotion changes the version a cluster requests, not the source each component names.
- **Confirms ADR-040.** Day-0 continues to select the bundle version exactly once and carries no further authority.

## Addendum 1: A prerelease version is how the released path is exercised (2026-09-11)

The Negative section above names the cost of the sentinel plainly: *"An unreleased
build does not exercise the published path, and the two paths are not the same
mechanism."* It then rests on the packaging checks to hold the two equivalent. That
was not enough, and this records what closes the gap.

Two defects reached clusters through it. The `bundleVersion` parameter that a
released chart needs and a repository path does not was absent, and every boundary
rendered `0 of 3 Applications` on a released box while a development box was healthy.
Separately, the content a released binary carries is an allowlist
(`scripts/package/embed-platform-assets.sh`), and a development build never consults
it — so a Day-0 read of a manifest outside that set works everywhere it is tested and
fails at a tenant's first bootstrap, naming a file the repository visibly contains.

Neither is an equivalence the packaging checks can assert. They compare a packaged
component against the repository path it came from; these are properties of the
*delivery*, and only running the delivered thing shows them.

**A prerelease version is a third build mode, and it is how the platform tests
itself.** `0.1.16-rc.1` is injected exactly as a release version is, so the binary
reads its embedded content and renders the published-chart shape — the tenant's path
in every respect. It is published by the same workflow through `workflow_dispatch`.
And it does not consume the release: ADR-063 consumes a version by publishing it, and
`0.1.16-rc.1` is a distinct version from `0.1.16`, so the release number stays free
for the set that is actually released.

This does not weaken the reason the sentinel exists. Requiring a tag to test still
makes tagging routine, and the development build remains the default for ordinary
work. What changes is that the released path stops being first exercised by a tenant.

Two guards make the older failure mode visible earlier rather than relying on anyone
running the loop:

- The embedded tree is asserted to carry every path Day-0 reads, per provider,
  environment and boundary, enumerated from the working tree so a new one is covered
  by the commit that adds it (`internal/kakt-cli/bootstrap/embedded_assets_test.go`).
- The check that the embedded tree matches its source runs in CI, not only in a
  pre-commit hook on one machine, and triggers on any manifest rather than on a
  restatement of the embed set — the narrower pattern it replaces omitted the Cilium
  artifact carrying ADR-075's `mtu` and `devices` values.

See `docs/runbooks/local-release-path-testing.md`.

## Addendum 2: accepted (2026-09-12)

Every clause above is now implemented and guarded. The last one outstanding was
*"Overriding a source requires naming its version"*, which guarded nothing:
`--bundle-registry` and `--platform-repo` both existed, and `--bundle-version`
defaulted to whatever the binary carried, so a scaffold against a fork pinned a
version that fork had never published and said so only when the first
Application failed to load its source. `refuseUndeclaredSourceOverride` in
`cmd/kakt/tenant.go` refuses it, and
`cmd/kakt/tenant_source_override_test.go` asserts the refusal, the permitted
form, and that the default path is untouched.

## Acceptance

```architecture
acceptance:
  - internal/kakt-cli/bootstrap
  - cmd/kakt
  - scripts/validate/released-cli-standalone.sh
```

`embedded_assets_test.go` asserts a released binary carries every path Day-0
reads, per provider, environment and boundary. `released-cli-standalone.sh`
refuses a release whose CLI needs a checkout. `cmd/kakt` covers the source
override that must name its version.

## References

- ADR-039: Platform Ownership Model
- ADR-040: Day-0 vs Day-1 Lifecycle Boundary
- ADR-063: The Platform Bundle and its Version
- ADR-064: Bundle Promotion and Cell-Scoped Policy
- ADR-065: The Control Plane Ships Into the Box
