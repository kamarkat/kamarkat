# Where versions live

Every mechanism the platform uses to pin a version, what changes it, and what
breaks if you change it the wrong way. Check which row a component is in before
editing anything — most of these do not behave the way the file looks.

## Contents

- [What `versions.go` is, and is not](#what-versionsgo-is-and-is-not)
- [One value, many spellings](#one-value-many-spellings)
- [Remote chart descriptors](#remote-chart-descriptors)
- [Vendored chart renders](#vendored-chart-renders)
- [Kubernetes itself](#kubernetes-itself)
- [kubeadm-derived images](#kubeadm-derived-images)
- [Node-level packages and the node image](#node-level-packages-and-the-node-image)
- [CAPI providers and bootstrap binaries](#capi-providers-and-bootstrap-binaries)
- [Crossplane packages](#crossplane-packages)
- [Operator CR fields](#operator-cr-fields)
- [Our own images](#our-own-images)
- [Utility images in hand-written manifests](#utility-images-in-hand-written-manifests)
- [Generated mirrors](#generated-mirrors)
- [Finding a pin you cannot place](#finding-a-pin-you-cannot-place)

## What `versions.go` is, and is not

`internal/soloz-cli/versions/versions.go` is the **Day-0 bootstrap** version
authority, not the platform's. It holds nine constants:

| Constant | Covers |
| :- | :- |
| `CAPIOperatorVersion` | the operator that installs CAPI providers |
| `CAPIVersion` | CAPI core |
| `KubeadmBootstrapProviderVersion` | kubeadm bootstrap provider |
| `KubeadmControlPlaneProviderVersion` | kubeadm control-plane provider |
| `HetznerInfraProviderVersion` | the infrastructure provider |
| `ClusterctlVersion` | the clusterctl binary the CLI downloads |
| `PackerVersion` | the node-image build tool |
| `CertManagerVersion` | the CAPI prerequisite, installed from a release URL |
| `ArgoCDChartVersion` | the chart the Day-0 seed installs |

What they have in common is *when* they are needed: before GitOps exists, so the
CLI has to carry them. Nothing else belongs here, and adding a component's
version to this file because it would be tidier moves it away from the
declaration that actually reconciles it.

Five of the nine are one half of a pair — four provider constants against their
provider CRs, and the chart constant against the GitOps descriptor. Each pair
has a validator. Move both halves together.

`ClusterctlVersion` is also duplicated by a constant inside the binary manager
that downloads it. Both must move, and the checksums with them.

## One value, many spellings

Some logical values are spelled in many files, with no declaration that owns
them. These are where upgrades leak.

The **Kubernetes version** is the worst: the ClusterClass and the
management-class asset (as `KUBERNETES_VERSION` and a trimmed `major.minor`
companion), the provider compositions, the node provisioning scripts, a CLI
bootstrap constant, a topology test, the Kind node image, the re-vendor script
arguments, and the `--kube-version` recorded in the header of every vendored
render.

**Utility and sidecar images** are duplicated per provider and per side —
a hub copy, a spoke copy, and a hybrid variant of the same addon.

Two habits handle this:

- Enumerate every spelling before changing any of them, and re-grep afterwards.
  A missed copy of the Kubernetes version produces a node pool installing from
  the wrong package repository, which fails late and looks unrelated to the
  upgrade.
- Count the occurrences before and after. A sed that reports fewer
  substitutions than there were matches has silently skipped a file.

## Remote chart descriptors

A component descriptor under `manifests/argocd/components/<boundary>/`, or an
element in a boundary appset under
`manifests/argocd/environment-manager/templates/`, that names a `repoURL`
beginning `http` and a `chart`. Its `targetRevision` is the **chart** version.

Change by editing `targetRevision`. Resolve the chart→app pair first; the
application version is not what goes here.

A descriptor with `targetRevision: ''` and an empty `repoURL` is **not** a remote
chart — it is platform content, and its version is the bundle version. Nothing
to bump.

These are also the only components a box resolves from an upstream repository at
reconcile time. Everything else travels inside the bundle.

## Vendored chart renders

An upstream chart rendered at a pinned version and committed, with a provenance
header naming the chart, the chart version, the exact `helm template` command
and a content digest of the rendered output.

**Change by re-rendering only.** The digest is verified, so a hand edit is
caught; and a hand edit that happened to pass would make the file disagree with
the command in its own header, which is what the next person reads to reproduce
it.

Re-rendering is ordinary work, not someone else's: `helm`, `crane` and `curl`
with network access are enough for all of it. `references/revendoring.md` has
the method — classify the file, reproduce the version you are on, then replace
it. Reproduce first; the header does not record post-processing or hand patches,
and the diff against upstream at the current version is what reveals them.

Two things to carry into every re-render:

- the target chart version **and** the Kubernetes version to render at, because
  the header records `--kube-version` and chart output varies by it
- every value the current render was produced with, from its header — a
  re-render that drops one silently changes behaviour

Watch for a chart whose newest release ships an application version **older**
than what the repo currently runs. Re-rendering from it is a downgrade. Resolve
where the running version actually came from before proceeding.

## Kubernetes itself

Set in the ClusterClass and management-class assets as shell exports in
cloud-init (`KUBERNETES_VERSION` and a trimmed `major.minor` companion), and in
the provider composition that names the control-plane version.

Both halves move together. The trimmed value feeds package repository paths, so
leaving it behind produces a node that installs from the wrong repository.

`HetznerClusterTemplate.spec` and ClusterClass templates are **immutable** once
a cluster exists. For a new environment, editing in place is correct — it is
rendered fresh. For an existing one, an in-place edit wedges reconciliation and
the change has to arrive as a new-named class.

## kubeadm-derived images

The control-plane images, etcd, CoreDNS and the pause image are **not pinned
anywhere**. The inventory derives them by reading kubeadm's own constants for
the chosen Kubernetes version.

They are not a choice. They will land wherever the target Kubernetes minor puts
them, and the inventory will keep reporting them behind their own upstream
latest. Do not try to pin them, and do say so when reporting what is not
takeable.

## Node-level packages and the node image

The Kubernetes node packages, the operating system and the container runtime are
installed by cloud-init in the ClusterClass, not deployed as workloads. The OS
image is a provider image plus whatever the node build installs.

An OS major move is a node-image rebuild with changed package sources, not a
version bump. Scope it separately from a component upgrade.

## CAPI providers and bootstrap binaries

Each provider has a CR under `internal/assets/manifests/core/capi-operator/providers/`
whose `spec.version` is authoritative for what the operator installs, **and** a
matching constant in `internal/soloz-cli/versions/versions.go`. A parity test
compares them.

Change both, in one commit. The constants file also pins the bootstrap binary
and prerequisite versions; the binary carries per-platform checksums that must
be recomputed from the release assets, or installation fails closed.

The operator's own version is a separate pin with a large vendored install
manifest behind it. Move the constant and the vendored manifest together, or the
constant describes something that is not installed.

Two distinct contracts constrain this set and they are easy to conflate:

- the **operator's** own dependency version
- the **provider** versions it installs

An operator can install provider versions other than its own dependency. What
caps the provider set is usually the *infrastructure* provider's published
compatibility table, which may lag the core project by a release line.

## Crossplane packages

`Provider` and `Function` objects naming an OCI `xpkg` reference with a tag.
Installed by Crossplane's own package manager, not Helm, by design.

Change the package reference. Check the provider line against the Crossplane
major: a pre-v2 provider keeps cluster-scoped managed resources, which has to
agree with the scope its consuming definitions declare.

## Operator CR fields

A database cluster's image, or an operator-managed application instance's image,
is a field on the custom resource — not a chart value and not an image in a
Deployment. The operator comes from a chart; the instance version lives on the
CR.

This is the correct place for it. Two rules:

- **Always pin it.** An operator's default image moves with the operator, so
  omitting the field means an operator upgrade silently changes the application
  or database version underneath a workload that never asked for it.
- **Choose per consumer.** Unrelated applications sharing a version is a
  coincidence, not a design. Check each consumer's own supported range and let
  the answers differ, recording the constraint for each.

## Our own images

Built by CI from a commit, with the digest committed back. The inventory's
`latest` for these is a commit identifier, not a release.

Never bump these by hand. The release gate refuses when operator source is
uncommitted, because the image is built from a commit — otherwise new manifests
ship with an older image.

## Utility images in hand-written manifests

Helper and init-container images inside manifests the platform authors itself.
No chart governs them, and none should: there is no upstream chart for "the
image an init container runs".

Change the tag, and if the reference carries a digest, resolve the new digest
first. A tag-only edit to a digest-pinned reference is either rejected or pulls
the old content, depending on where it is used.

## Generated mirrors

Some trees are verbatim copies produced by a packaging script and verified by a
pre-commit hook — an embedded copy the CLI carries, and the build output of a
release.

Edit the source. Run the generator (or hand it over) to refresh the copy. Never
edit a mirror directly: the hook reports it as drift, and the drift looks like
someone else's uncommitted work.

## Finding a pin you cannot place

The inventory generator is the map. It discovers every component by scanning,
so reading how it scans tells you exactly where each kind of version lives —
which globs it walks for descriptors and appsets, which file it reads the
Kubernetes version from, which provider CRs it loads, and which values it
derives rather than reads.

When a component appears in the inventory and you cannot find its pin, read the
scanner function that emitted it rather than grepping the tree for the version
string. Grep finds the vendored copies and the build output; the scanner names
the source.
