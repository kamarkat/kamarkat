# Upgrade traps

Failure modes that have cost real time on this platform. They share three
properties: they are invisible early, several of them report green while being
broken, and a few look like findings when they are not.

Read this before the first edit. Each entry says what it looks like, why it
happens, and what to do instead.

## Contents

- [Taking the inventory's `latest` as the target](#taking-the-inventorys-latest-as-the-target)
- [Comparing a chart version to an application release](#comparing-a-chart-version-to-an-application-release)
- [Trusting a compatibility matrix read once](#trusting-a-compatibility-matrix-read-once)
- [Accepting a resolved version that is older than what you run](#accepting-a-resolved-version-that-is-older-than-what-you-run)
- [Pinning from a matrix without reading its column headings](#pinning-from-a-matrix-without-reading-its-column-headings)
- [Bumping a tag that carries a digest](#bumping-a-tag-that-carries-a-digest)
- [Bumping a checksummed binary](#bumping-a-checksummed-binary)
- [Re-rendering without reproducing first](#re-rendering-without-reproducing-first)
- [Round-tripping a file that carries comments](#round-tripping-a-file-that-carries-comments)
- [A script that succeeds and changes nothing](#a-script-that-succeeds-and-changes-nothing)
- [Adding a required value and leaving the fixtures behind](#adding-a-required-value-and-leaving-the-fixtures-behind)
- [Upgrading a component on one side only](#upgrading-a-component-on-one-side-only)
- [Sweeping only the components that have descriptors](#sweeping-only-the-components-that-have-descriptors)
- [Editing a constant nothing reads](#editing-a-constant-nothing-reads)
- [Changing a Kubernetes version by editing a ClusterClass](#changing-a-kubernetes-version-by-editing-a-clusterclass)
- [Raising a cluster's version by more than one minor](#raising-a-clusters-version-by-more-than-one-minor)
- [A template change that only one kind rejects](#a-template-change-that-only-one-kind-rejects)
- [A test that pins the version it should be guarding](#a-test-that-pins-the-version-it-should-be-guarding)
- [Breaking a parity pair "temporarily"](#breaking-a-parity-pair-temporarily)
- [Supplying a partial value for a replace-only key](#supplying-a-partial-value-for-a-replace-only-key)
- [Leaving a config key that upstream removed](#leaving-a-config-key-that-upstream-removed)
- [Inheriting a default that moved](#inheriting-a-default-that-moved)
- [Raising a breaking change that does not apply](#raising-a-breaking-change-that-does-not-apply)
- [Treating hand-written glue as a contract](#treating-hand-written-glue-as-a-contract)
- [Carrying migration mitigations into a fresh build](#carrying-migration-mitigations-into-a-fresh-build)
- [Editing before the ceiling is known](#editing-before-the-ceiling-is-known)
- [Un-vendoring to "modernise"](#un-vendoring-to-modernise)
- [Choosing one version for unrelated consumers](#choosing-one-version-for-unrelated-consumers)
- [Delegating a lookup you can run](#delegating-a-lookup-you-can-run)

## Taking the inventory's `latest` as the target

**Looks like:** a clean table of 50-odd components to bump.

**Why it fails:** `latest` is fetched per component, independently, with no
knowledge of the others. Several of those values are mutually exclusive, and a
few are unreachable by construction (the kubeadm-derived set). A change set
built from them cannot deploy, and the incompatibility surfaces only when
something refuses to start.

**Instead:** find the binding ceiling first, then take latest-compatible per
component and name each rejected `latest` with its constraint.

## Comparing a chart version to an application release

**Looks like:** "the chart says 7.9.1 but upstream is on 3.5.6, we're years
behind."

**Why it fails:** they are different number lines. A chart's major moves for
chart-structural reasons, independently of the application it packages. The
comparison is meaningless in both directions.

**Instead:** resolve the pair with `helm search repo --versions` and reason about
the application version only.

## Trusting a compatibility matrix read once

**Looks like:** a confident version range from a search result or a single page.

**Why it fails:** compatibility pages go stale, get mirrored with old content,
and a vendor's commercial distribution has its own numbering that looks like
upstream's. A single read has produced a range with the wrong major series
entirely, while the *conclusion* drawn from it happened to be right — which is
worse, because it survives review.

**Instead:** two independent sources, or a mechanical check against the artefact
(render the chart, read its `kubeVersion`, read the CRDs it ships). Prefer the
project's own documentation at the release you are pinning.

## Accepting a resolved version that is older than what you run

**Looks like:** a resolved "newest" chart or image that is at or below the
version already in the repo.

**Why it fails:** it cannot be true. The source is stale, empty, pointed at the
wrong repository, or fabricated. Reasoning onward from it produces a "bump" that
is a downgrade or a no-op.

**Instead:** treat it as proof the source is broken. Clear the cache, force the
repo refresh, show the index date, and re-resolve. This is the cheapest
inconsistency to detect — the repo is the control.

## Pinning from a matrix without reading its column headings

**Looks like:** picking the newest release from a project's compatibility table.

**Why it fails:** some matrices are keyed by the Kubernetes client library a
release was built against, not by the Kubernetes version it supports. The row
matching your target is then one or more releases *behind* the newest, and the
project treats anything outside the row as best-effort only.

**Instead:** read what the columns are, find the row for your target, and pin
that row's newest patch.

## Bumping a tag that carries a digest

**Looks like:** a one-line tag edit on `image: repo:tag@sha256:...`.

**Why it fails:** the digest wins. You either get the old content under a new
name, or a rejection — and the manifest now lies about what it runs.

**Instead:** resolve the digest for the new tag first (`crane digest`) and move
both halves together.

A batch find-and-replace over tags is how this happens at scale: the pattern
matches the tag, the digest sits after it and is left untouched, and every edited
line now claims a version it will not run. After any tag bump, audit the diff for
the pair:

```
git diff -U0 | grep '^+' | grep -oE 'image: [^ ]+@sha256:[a-f0-9]+' | sort -u
```

Every line that comes back needs its digest re-resolved, or confirmed as one you
resolved deliberately.

## Bumping a checksummed binary

**Looks like:** changing a version constant for a downloaded tool.

**Why it fails:** the download is verified against per-platform checksums
recorded at pin time. A version change without new checksums fails closed on
first use — which is the safe direction, but it fails at the worst moment,
during a bootstrap.

**Instead:** recompute every platform's checksum from the release assets in the
same change.

## Re-rendering without reproducing first

**Looks like:** the provenance header records the chart, the version and the
command, so you run it at the new version and commit the result.

**Why it fails:** headers record intent. They routinely omit the serialisation
pass that produced the committed text, hand-added containers and environment
variables, replica overrides, and the comments explaining the incidents those
patches prevent. Re-rendering from the header alone silently drops all of it,
and the loss is invisible in review because the file looks like a clean render —
which it is. It is just missing everything that was added to it.

**Instead:** reproduce the version you are **already on** first — fetch upstream
at the current pin, reverse any transformation you know about, and diff. Zero
means you have the whole pipeline. Anything else is the customisation, itemised
for you. `references/revendoring.md` has the method.

## Round-tripping a file that carries comments

**Looks like:** a clean way to patch a rendered manifest — parse it, change the
structure, write it back.

**Why it fails:** a YAML load/dump cycle deletes every comment. In a vendored
render those comments are frequently the only record of why a patch exists —
the outage it prevents, the race it closes, the alternative that was rejected.
The manifest still works, and the knowledge is gone, so the next person removes
the patch as unexplained.

**Instead:** patch textually, with explicit anchors, and assert each patch
applied exactly once. Reserve the round-trip for files that carry no comments,
and verify by reading the parsed result back rather than by trusting the write.

## A script that succeeds and changes nothing

**Looks like:** a re-vendor or codegen script that runs, prints nothing, and
leaves the tree untouched. Easy to read as "already up to date".

**Why it fails:** under `set -e` with `pipefail`, a failing command inside a
command substitution aborts the script at that line — before any guard written
below it can explain why. Piping the run through `tail` or `head` then masks the
exit code, so the failure looks like success. The underlying cause is often a
brittle assumption: a `grep -A1` that assumed two keys were adjacent, say.

**Instead:** check the exit code directly rather than through a pipe, and treat
"ran and changed nothing" as a result to verify, never to accept. When you find
one, fix the detection rather than working around it — and make the failure
print its reason.

## Adding a required value and leaving the fixtures behind

**Looks like:** a component bump or contract change that introduces a per-box
value with no safe default, so the template uses `required`. The real paths are
updated in the same commit — the box template declares it, Day-0 seeds it, the
operator reads it — and the change looks complete.

**Why it fails:** nothing renders the chart the way a box does. Every gate renders
it with a hand-written list of example values (`boxId=example-box`,
`environmentSlug=dev`, `hubDomain=dev.example.test` …), and those lists are
written out, not derived — so a newly required value is missing from all of them
at once. The chart's own `values.yaml` does not save you either: `required`
rejects an **empty string**, not just an absent key, so the usual `key: ""`
placeholder fails exactly like no key at all. There are six such renders, and the
failure arrives one at a time, minutes into packaging. Worse, a render whose
stderr is discarded and piped into a checker produces an *empty stream*: the
checker finds nothing wrong with nothing and the gate reports clean.

A nested value fails differently again and more quietly. A template reading
`.Values.global.management.infisicalEnvironmentSlug` against a chart that
declares no `management` map hits a nil pointer and renders **nothing**, which
the packager reports as "component produces no objects" and blames the component.
A flat key that is merely absent renders empty and passes. That difference is why
the nested case went unnoticed until an overlay templated one.

**Instead:** when you add a `required`, find every renderer before you commit —

```bash
grep -rn "helm template" scripts .github | grep -i "environment-manager\|platform-bundle"
```

— and add the value to each fixture, using what a box actually declares (for the
management scope, `management`, from gitops-template's `templates/mgmt/values.yaml`).
Never add a chart default to get a gate green: the default is the thing the
`required` exists to refuse. If a gate pipes a render into a checker, render to a
file and fail on the render's own exit code, so an unrenderable bundle is a
failure rather than a clean report.

## Upgrading a component on one side only

**Looks like:** the bump is done — you changed the chart version the component is
installed from, and the release shows the new version.

**Why it fails:** the same component usually reaches the management and managed
clusters by different routes. One pulls a chart; the other carries a vendored
render, a hand-assembled manifest, or an image literal in a bootstrap payload.
Changing the chart moves one side. The other keeps its old version, the release
reports the component twice, and the split looks deliberate to whoever reads it
next.

**Instead:** before bumping anything, list every cluster the component runs on and
every mechanism that delivers it there. The release inventory is the fastest way:
group its rows by component and source, and any component present on both sides
with different versions is a split. Fix them in one change, so the two sides are
never committed disagreeing.

## Sweeping only the components that have descriptors

**Looks like:** thorough coverage — you worked outward from the component
descriptors and chart pins and bumped everything they name.

**Why it fails:** descriptors and charts describe what the GitOps engine
reconciles. They do not describe what a cluster is *bootstrapped* with. Addon
payloads delivered by ClusterResourceSet, hand-assembled installs in a
bootstrap template, and anything applied before the engine exists carry their
versions as plain literals that no descriptor points at and no parity check
guards. Those are exactly the components that drift furthest, because nothing
reports them.

**Instead:** finish every sweep with a pass over raw image literals across the
whole tree, not just the descriptor set, and compare the result against the
inventory. A component delivered two different ways — one through a chart, one
through a bootstrap payload — will have two different versions, and the one
without a descriptor will be the older.

## Editing a constant nothing reads

**Looks like:** the version is in the file that has the header comments, the
parity notes and the explanations. You edit it and the bump is done.

**Why it fails:** a constant file accumulates anything version-shaped, and some
entries lose their consumer — or never had one. Editing those is a no-op that
looks complete, passes review, and ships nothing. Worse, if the live copy has an
attached artefact (a checksum, a digest), leaving it behind turns the no-op into
a failure at the moment the artefact is used.

**Instead:** before editing any version constant, grep for a consumer. No
caller means the real pin is elsewhere; find it, change that, and delete the
copy rather than wiring it up — a constant with an invented consumer is a second
authority waiting to drift. The inventory generator is a good tiebreaker: what
it reads is what is real.

## Changing a Kubernetes version by editing a ClusterClass

**Looks like:** the obvious edit. The class's cloud-init is where kubelet and
kubeadm are installed, so the version appears to live there.

**Why it fails:** it is not the authority and never was. CAPI reconciles to
`spec.topology.version`, and that is the version kubeadm is told to join as. A
literal in the class is a second declaration of the same fact, and the two drift
without anything noticing — on 2026-11-05 the templates said 1.35.10 while the
cluster was still v1.31.6, because the composition's matching bump was rejected
and nothing compared the two. The replacement control-plane node installed
kubelet 1.35.10, could not join a 1.31.6 control plane, and never registered.

The signature is quiet and misleading: `BootstrapReady=True`,
`InfrastructureReady=True`, `NodeHealthy=False` with reason `NodeProvisioning`
and "Waiting for a node with matching ProviderID to exist", no Node object on
the workload cluster at all, and a release script that reports a roll in
progress and advises waiting. It will never converge.

**Instead:** change the declaration — `versions.KubernetesVersion` for the
management cluster, the provider composition's `spec.topology.version` for a
workload cluster. The classes derive the package version from
`builtin.controlPlane.version` and `builtin.machineDeployment.version`. If you
find yourself typing a Kubernetes version into a ClusterClass, that is the
defect, not the fix.

## Raising a cluster's version by more than one minor

**Looks like:** one edit to `spec.topology.version`, straight to the target.

**Why it fails:** the Cluster webhook refuses it outright — `spec.topology.version:
Invalid value: "v1.35.10": version cannot be increased from "1.31.6" to
"1.35.10"`. Worse, the refusal lands on whatever applies it rather than on the
release: a composition carrying an impossible version applies cleanly, the
Application goes **Synced**, and Crossplane then fails to patch the Cluster. The
cluster simply never upgrades, while every status is green.

**Instead:** check the live `spec.topology.version` before choosing the target,
and dry-run the bump:

```bash
kubectl -n platform-capi patch cluster <name> --type merge \
  -p '{"spec":{"topology":{"version":"vX.Y.Z"}}}' --dry-run=server
```

Then either ladder one minor at a time — each step a full control-plane and
worker roll — or recreate the cluster, which has no skew constraint because
creation is not an increase. For an empty or disposable cluster, recreating is
much the smaller change; say so rather than designing a ladder nobody asked for.

## A template change that only one kind rejects

**Looks like:** a consistent edit across the control-plane and worker templates
of a class, so it will land consistently.

**Why it fails:** immutability is per kind. `KubeadmControlPlaneTemplate` and
`HetznerClusterTemplate` reject any `spec.template.spec` change once they exist;
`KubeadmConfigTemplate` accepts it. So the change lands *partially* — the config
templates update, the control-plane template is denied, and the Application
cannot be diffed at all. That is `ComparisonError`, not a failed sync: the
Application never reaches Synced, every other resource in it stops converging
too, and waiting does not help.

**Instead:** dry-run each object the way ArgoCD does before releasing, and plan
the one manual step a denial implies — the object has to be deleted so ArgoCD
recreates it, with anything referencing the class gone first.

```bash
kustomize build <dir> | kubectl -n platform-capi apply --server-side \
  --force-conflicts --field-manager=argocd-controller --dry-run=server -f -
```

## A test that pins the version it should be guarding

**Looks like:** a test asserting the currently deployed versions, which fails
the moment you bump anything.

**Why it fails:** it is not a guard, it is a copy. The version now lives in the
declaration, the constant, and the test, so a legitimate upgrade has to edit the
test to pass — which teaches people that editing tests is part of bumping a
version, and the next real failure gets the same treatment.

**Instead:** a parity test compares two things that must agree and names neither
value. If a test hardcodes the version, delete the hardcoded half rather than
updating it; assert shape and internal consistency (well-formedness, values that
are released together being equal) and leave the value to the parity check.

## Breaking a parity pair "temporarily"

**Looks like:** bumping the constant now and the manifest it describes "in the
next commit".

**Why it fails:** several versions exist in two places by design — a Go constant
and the provider CR it describes, a bootstrap seed version and the declaration
that adopts it. Each has a validator because each has already drifted. In the
window, the repo asserts something untrue, and if the pair is a seed/adopter
pair the consequence is a component upgrading itself mid-bootstrap.

**Instead:** move both halves in one commit. If one half cannot move yet (a
vendored artefact needing a render), move neither, and say why.

## Supplying a partial value for a replace-only key

**Looks like:** a short, targeted override — an exclusion list with the one entry
this platform needs.

**Why it fails:** when the value is a single opaque string, supplying it replaces
the upstream default wholesale. The override looks additive and is subtractive.
Every default you did not restate is now gone, silently, and the symptom is
unrelated churn appearing much later.

**Instead:** check whether the key merges. If it replaces, prefer inheriting the
upstream default and adding nothing. If an addition is genuinely required,
restate the entire upstream default verbatim plus the addition, with a comment
naming the chart version it was copied from — and expect that copy to go stale.

## Leaving a config key that upstream removed

**Looks like:** a settings block that still contains a key the new major deleted.

**Why it fails:** nothing errors. The key is ignored, and the behaviour it was
suppressing comes back. Access controls that were disabled become enforced, or a
guard that was relaxed tightens, and the first sign is a user who cannot do
something they could yesterday.

**Instead:** grep the descriptor for every key the upgrade notes list as
removed, delete them, and replace their effect explicitly where it was wanted.

## Inheriting a default that moved

**Looks like:** nothing. The field is absent, so there is nothing to review.

**Why it fails:** a default that changes between majors silently changes
behaviour, and the repo records no decision either way. This is hidden version
authority: the running configuration is set by whichever chart version happened
to be pinned.

**Instead:** for any default the upgrade notes say changed, either accept it
deliberately (and note that you did) or declare the previous value explicitly.
Prefer accepting the new default on a fresh environment — declaring the old one
only to avoid change is how a platform accumulates permanent divergence.

## Raising a breaking change that does not apply

**Looks like:** a serious finding from the upgrade notes that matches a field
name in the repo.

**Why it fails:** similarly-named fields coexist — one native to a resource or
generator, one at the framework level — and only one is affected by the change.
A false finding costs the same review time as a real one and spends credibility
that the real findings need.

**Instead:** verify mechanically which field you actually have, by its position
in the document rather than by its name. Then report it, or report that you
checked and it does not apply.

## Treating hand-written glue as a contract

**Looks like:** a manually authored piece of metadata that appears to make
something work, which you then design around preserving.

**Why it fails:** it may be broken and have always been broken. Hand-authored
ownership or tracking metadata in particular can name the wrong owner, conflict
with what the controller writes, and achieve nothing — while looking like a
deliberate pattern worth protecting. Designing to preserve it produces an
elaborate justification for keeping a bug.

**Instead:** before building on an unusual mechanism, verify it does what it
appears to do — which component actually manages the object, whether the normal
declarative path was available, and whether anything prevented it. Prefer the
ordinary mechanism and delete the glue.

## Carrying migration mitigations into a fresh build

**Looks like:** diligence — applying every mitigation the upgrade notes
recommend.

**Why it fails:** nearly every breaking change in an upgrade is a *migration*
hazard: orphaned resources, re-keyed ownership, state to convert. A newly
created environment has none of it. The mitigations become permanent complexity
paying down a cost never incurred, and some of them pin the platform to
behaviour upstream is moving away from.

**Instead:** separate the two cases explicitly. For a new environment, create at
the target and skip migration work. For a live one, respect the upgrade
contracts — consecutive-minor rules, immutable template fields, existing
ownership state — which do not apply to the new one.

## Editing before the ceiling is known

**Looks like:** starting with the obvious pins to make progress.

**Why it fails:** the ceiling changes the target for everything coupled to it.
Pins chosen before it is known get redone, and worse, some of them get reviewed
and approved in the meantime, so the rework is now visible churn.

**Instead:** resolve the destination first, state it with its constraint, and
then edit. If the ceiling needs a decision from someone, ask for that decision
before editing rather than after.

## Un-vendoring to "modernise"

**Looks like:** replacing committed renders with runtime chart references, so
components are "Helm-managed".

**Why it fails:** the bundle's whole promise is that a published version resolves
to the same artefact forever and a box reconciling it contacts nothing upstream.
Runtime chart references make every reconcile depend on an upstream, mutable
repository. The platform already uses Helm — at render time, which is the
authoring step, not the reconcile step.

**Instead:** keep charts as the authoring source and the bundle as what ships.
If packaging is the real concern, the useful direction is the opposite one:
reducing the number of components a box resolves from upstream at reconcile
time.

## Choosing one version for unrelated consumers

**Looks like:** tidiness — one database major, one runtime version, across the
platform.

**Why it fails:** unrelated applications have unrelated supported ranges. A
single version is either the lowest common denominator for everyone or
unsupported for someone, and when it is chosen by whoever did the upgrade rather
than by each consumer's documented range, it is hidden version authority with a
neat appearance.

**Instead:** check each consumer's own supported range, let the answers differ,
and record the constraint per consumer. Convergence is fine when it falls out of
the evidence; it is not a goal.

## Delegating a lookup you can run

**Looks like:** batching version lookups into a request for someone else, to
save your own time.

**Why it fails:** resolving a version is a read. Delegating it costs a
round-trip, and the result still has to be checked against the repo — which has
caught returned data that was stale enough to contradict the versions already
running. The checking is the expensive part and it does not get delegated.

**Instead:** run the reads. Hand over only what genuinely cannot be done from
here: renders and builds that need a toolchain, downloads of release artefacts,
and anything that writes to a cluster or a secret store.
