---
name: upgrade
description: Upgrade platform components — Kubernetes, CAPI and its providers, Crossplane, the GitOps engine, policy engine, CNI, operators, databases and every image in the release inventory. Use this whenever anyone mentions upgrading or bumping a component, moving to a newer Kubernetes, "is X compatible with Y", a version ceiling, chart versions, the component inventory or a release's components.json, Renovate proposals, or asks to take components "to latest" — and also when a version number appears in a request at all, because the first question is always which versions are *compatible*, not which are newest.
---

# Upgrade

An upgrade is a compatibility problem, not a version bump. The inventory in
`kamarkat/data/releases/<version>.json` records a `latest` for every component,
fetched from upstream — but `latest` means *newest released*, never *newest
compatible*. Treating those two as the same thing produces a change set that
cannot be deployed, and you only find out at the end.

So the order matters: **find the ceiling, resolve real versions, classify the
pins, triage the breaking changes, then edit.** Do not start editing because the
first few pins look obvious.

## The policy

**One version per component, everywhere it runs.** A component that exists on both
the management and the managed clusters runs the SAME version on both. There is no
such thing as upgrading it on one side: the pair moves together or neither moves.

This is not tidiness. The two sides are delivered differently -- the management
cluster usually pulls a chart, the managed clusters usually carry a vendored
render -- so a bump applied to one path leaves the other behind silently, and the
release reports the component twice at two versions with nobody having decided
that. Every such split found so far was an accident of delivery, not a decision.

Exceptions exist and are narrow: where two clusters run a component for different
applications with different supported ranges, the versions differ by design.
PostgreSQL is the standing case, and `references/databases.md` records how its
majors are chosen and why `TenantDatabase` exposes none. Record the reason in the
catalog; do not let the exception excuse a split nobody chose.

**Latest-compatible, not latest.** Every component is in scope. Each takes its
newest upstream release that works with the target Kubernetes version and the
platform's existing contracts. Where the newest does not fit, take the newest
that does *and name the specific upstream constraint* — a pin without a recorded
reason gets "helpfully" bumped by the next person.

Being behind `latest` is a normal, permanent state for several components. The
inventory will keep reporting them as not current. That is correct, not a gap.

## 1. Find the ceiling before you edit anything

One component usually caps the whole stack. Identify it first, from upstream's
own support matrix, and let it set the target for everything coupled to it.

Work outward from whatever gates admission or provisioning, because those block
everything: the policy engine's supported Kubernetes range, the infrastructure
provider's supported control-plane range, the CNI's supported range, the storage
operator's range. The lowest ceiling wins, and it is frequently *not* the
component anyone was thinking about.

Two ceilings exist in every upgrade and they are different questions:

- **Destination** — how far the stack can go. Set by the binding component.
- **Path** — whether you can get there in one step. Set by each component's own
  upgrade contract (some support consecutive minors only).

"Upgrade directly" answers the path question and says nothing about the
destination. Establish the destination first or the path is irrelevant.

## 2. Resolve real versions yourself

You have `crane`, `helm`, `curl` and network access. Use them. Resolving a
version is a read, not a deployment — do it rather than routing it elsewhere and
waiting a round-trip for data you then have to check anyway.

| Question | How |
| :- | :- |
| Newest patch of a Kubernetes minor | `curl -fsSL https://dl.k8s.io/release/stable-1.NN.txt` |
| Chart version → app version | `helm search repo <repo>/<chart> --versions` |
| Chart's Kubernetes constraint | `helm show chart <repo>/<chart> \| grep kubeVersion` |
| What a chart actually ships | `helm template ... --kube-version <target>` and read the output |
| Image tags that exist | `crane ls <repo> \| grep '^v1\.NN\.'` |
| Digest for a tag | `crane digest <repo>:<tag>` |
| A dependency's real behaviour | read it at its deployed tag (`reference-projects`, `git show <tag>:path`) |

**Cross-check anything that decides the target.** A compatibility matrix read
once can be stale, mis-numbered, or about a vendor's distribution rather than
upstream. Two independent sources, or a mechanical check against the artefact
itself, before a number becomes a pin.

**A source that reports a "newest" older than what the repo already runs is a
broken source, not a finding.** Stop and re-resolve rather than reasoning from
it. This is the cheapest lie to catch and the most expensive to act on.

**Chart version is never the app version.** Never compare a chart version to an
upstream application release — they are different number lines, and a chart's
newest release often ships an application version behind the newest app release,
or ahead of what the repo runs. Resolve the pair explicitly, every time.

**Check what a project's matrix is keyed by.** Some matrices map releases to a
client library version rather than to a Kubernetes version, so the row for your
target is not the newest release. Read the column headings.

## 3. Classify every pin before changing it

**There is no central version file.** `internal/kakt-cli/versions/versions.go`
looks like one and is not: it holds nine constants covering the Day-0 bootstrap
surface only — the CAPI operator, CAPI core, the two kubeadm providers, the
infrastructure provider, clusterctl, packer, cert-manager and the GitOps chart.
Those are the versions the CLI must know *before* GitOps exists. Every other
component's version lives somewhere else, and a typical upgrade touches
twenty-odd directories.

`references/delivery-map.md` is the map of which mechanism each component is
actually in, plus every file the Kubernetes version reaches. Start there rather
than grepping: it turns a sweep into a short list.

Do not look for one file, and do not try to create one. Several of these
mechanisms are the declaration a controller reconciles, so a central copy would
be a second source of truth that drifts. Read
`references/pin-locations.md` before editing — it lists every mechanism, where
it lives, and who may change it.

What *is* worth knowing up front: one logical value is often spelled in many
files. The Kubernetes version is the extreme case, appearing in the
ClusterClass, the management-class asset, the provider compositions, node
provisioning scripts, CLI constants, a Kind pin, re-vendor script arguments and
the header of every vendored render. Find all of its spellings before changing
any of them, and re-grep afterwards — a missed copy produces a node pool that
installs from the wrong package repository, which fails late and looks unrelated.

The short form:

| Kind | Changed by |
| :- | :- |
| Remote chart descriptor | editing `targetRevision` |
| Vendored chart render | re-rendering only, never by hand |
| kubeadm-derived image | not a choice; follows the Kubernetes version |
| Node-level package | the ClusterClass cloud-init |
| CAPI provider | its provider CR **and** the matching Go constant |
| Crossplane package | its `Provider`/`Function` package reference |
| Operator CR field | the CR (database image, instance image) |
| Our own image | CI, from a commit |
| Utility image in a hand-written manifest | editing it, with a digest if one is pinned |

Classifying first is what stops you hand-editing a generated file or bumping a
constant whose vendored counterpart then disagrees with it.

## 4. Triage breaking changes against the actual configuration

Read the upstream upgrade notes for every major, then check each item against
what this repo really does. Most will not apply, and the ones that do are rarely
the headline items.

- **Render the new chart and read its shipped defaults.** It is the only
  reliable way to learn what you are now inheriting or overriding.
- **Find the keys that replace rather than merge.** A single opaque string value
  (an exclusion list, a policy block) is replace-only: supplying a partial value
  silently deletes the upstream default. These are the dangerous ones.
- **A config key removed upstream goes inert, not loud.** Nothing errors; the
  behaviour it suppressed simply returns. Grep the descriptor for every key the
  notes say was removed.
- **A default that changed is now your decision.** Either accept it deliberately
  or declare the old value explicitly. Inheriting a default that moved is how
  version authority goes hidden.
- **Check whether a change applies at all before raising it.** Two
  similarly-named fields often exist — one native to a generator or resource,
  one at the framework level — and only one is affected. Verify mechanically;
  a false alarm costs as much review time as a real finding.

## 5. A new environment and an existing one are different problems

Almost every breaking change in an upgrade is a *migration* hazard: orphaned
resources, re-keyed ownership, data that must be converted, state that must be
preserved. A newly created environment has none of that.

So decide which you are doing, and do not carry migration mitigations into a
fresh build — they add permanent complexity to pay down a cost you never
incurred. Conversely, do not assume a fresh-build plan works on a live box:
consecutive-minor upgrade contracts, immutable template fields and existing
tracking state all bind there and not here.

When a component's upgrade path is blocked but a fresh install is not,
recreating is often smaller than upgrading. Say so rather than designing a
migration nobody asked for.

## 6. Apply, in source only

- Edit the source tree, never a generated mirror. Generated copies are rebuilt
  by their own script and verified by a hook; editing the copy produces a
  conflict that looks like someone else's mistake.
- **Move paired values together, in one commit.** Several versions exist twice
  by design — a constant and the manifest it describes, a seed and the
  declaration that adopts it. A parity check exists for each pair because it has
  drifted before. Breaking a pair "temporarily" ships a lie.
- Never hand-edit a vendored render to match a new version. Its provenance
  header carries a content digest and the exact command that produced it;
  editing it makes the file disagree with both. Re-render it instead —
  `references/revendoring.md` has the method, and the first step is proving you
  can reproduce the version you are already on.
- A digest-pinned image needs its new digest resolved before the tag moves. A
  checksummed binary needs its checksums recomputed, or it fails closed on first
  use.
- Build what you touched. A version constant that does not compile is a fast
  failure; a vendored manifest that no longer matches its digest is a slow one.
- Refresh the generated mirror and update the catalog's `behind` items in the
  same change as the pins. `references/release-gates.md` lists the gates a
  version change trips, what each means, and the order to fix them — read it
  before the first rehearsal, not after it refuses.

## 7. Record the decisions as you make them

Every ceiling, every rejected `latest`, and every per-service choice goes into
the governing plan or ADR **as it is decided**, with the constraint that forced
it. An upgrade is re-derived by the next person otherwise, and they will not
find the constraint — they will find a pin that looks stale and bump it.

Correct stale records when you find them. A comment naming a validator that does
not exist, or pointing at the wrong file for a pin, is worse than no comment: it
is believed.

## Traps

## References

Each covers one thing; read the one the step you are on needs.

| File | When |
| :- | :- |
| `delivery-map.md` | which mechanism a component is in, and the Kubernetes version chain |
| `pin-locations.md` | what each mechanism is and who may change it |
| `revendoring.md` | replacing a vendored render, and proving it first |
| `databases.md` | PostgreSQL, which is chosen per application rather than once |
| `release-gates.md` | the gates a version change trips, and the order to fix them |
| `traps.md` | the failure modes that have cost time here |

`references/traps.md` lists the failure modes that have actually cost time here
— the ones that are invisible until late, produce green status while being
broken, or look like a finding and are not. Read it before the first edit, not
after the first surprise.

## Standards

- Compatibility claims carry their source. "It should be fine" is not a pin.
- One brief covers all pending changes in deploy order; never split by component.
- Report what is not takeable as plainly as what is. A silent omission reads as
  an oversight later.
- No new release gates or verification phases as part of an upgrade. Use the
  checks that exist.
- Secrets, digests and counts only in reports — never key material, even from a
  file that already contains it.
