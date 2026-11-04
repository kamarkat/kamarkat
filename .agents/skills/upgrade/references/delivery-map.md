# What delivers each component

The map this platform actually has, so an upgrade can start from "which mechanism
is this component in" instead of grepping for a version string. Regenerate the
listings when they look stale — each section says how.

A component often reaches the management and managed clusters by **different**
mechanisms, and that is where one-sided bumps come from. Check both columns
before changing anything.

## Contents

- [Remote charts, pulled at sync time](#remote-charts-pulled-at-sync-time)
- [Vendored chart renders](#vendored-chart-renders)
- [Upstream release manifests, applied whole](#upstream-release-manifests-applied-whole)
- [The Kubernetes version chain](#the-kubernetes-version-chain)
- [kubeadm-derived images](#kubeadm-derived-images)
- [Cluster API providers](#cluster-api-providers)
- [Crossplane packages](#crossplane-packages)
- [Operator CR fields](#operator-cr-fields)
- [Our own images](#our-own-images)
- [Where the inventory reads each kind](#where-the-inventory-reads-each-kind)

## Remote charts, pulled at sync time

The only components a box resolves from an upstream repository while
reconciling. Everything else travels inside the bundle.

```bash
grep -H targetRevision manifests/argocd/components/*/*.yaml | grep -v "''"
awk "/repoURL: 'https/{u=\$0} /targetRevision: '[0-9]/{print FILENAME\": \"u\" -> \"\$0}" \
  manifests/argocd/environment-manager/templates/0*.yaml
```

Eight at the time of writing: argo-cd, crossplane, reloader, kube-state-metrics
and metrics-server as component descriptors under `manifests/argocd/components/`;
kyverno, external-secrets and zitadel as appset elements.

`targetRevision` is the **chart** version. Resolve the chart→app pair before
pinning, and remember a chart's newest release often ships an application version
behind the newest app release — or, for zitadel, *behind what an image override
was pushed to*.

## Vendored chart renders

Rendered at a pinned version and committed, each carrying a provenance header.

```bash
grep -rl "Vendored from" manifests internal/assets | grep -v embedded
```

Roughly two dozen, and the split matters: `spoke/spoke-catalog/infra/*` is the
managed-cluster copy of a component the management cluster usually gets from a
chart. cert-manager, external-secrets, kyverno, crossplane, kube-state-metrics,
reloader, loki, victoriametrics, cloudnative-pg and agent-sandbox all exist on
both sides by different routes.

Replacing one of these is a re-vendor, not an edit: see
`references/revendoring.md`, and reproduce the current version before you
replace it.

## Upstream release manifests, applied whole

Not charts — a release artefact downloaded and committed, or applied from a URL:

| What | Where | Applied by |
| :- | :- | :- |
| cert-manager (prerequisite) | `internal/assets/manifests/core/cert-manager/install.yaml` | pivot (`internal/kakt-cli/pivot/orchestrator.go`) |
| cert-manager (from a URL) | no file — `versions.CertManagerVersion` builds the URL | capi-init (`internal/kakt-cli/capi/operator.go`) |
| Cluster API operator | `internal/assets/manifests/core/capi-operator/install.yaml` | the CLI |
| cert-manager CRDs (spoke) | `spoke/spoke-catalog/infra/crds/cert-manager-crds.yaml` | GitOps |
| Argo CD core (spoke) | `spoke/spoke-bootstrap/argocd-core-template.yaml` | ClusterResourceSet |

These are usually upstream verbatim plus one transformation (a namespace rename,
a filter to the objects this platform runs). Prove which before replacing.

cert-manager is the sharp one: **three** places hold its version — the URL
constant, the vendored pivot asset, and the server-side-apply patches under
`hub-core-services/cert-manager/`. A release gate compares the first against the
last.

## The Kubernetes version chain

One logical value, no owning declaration. Find every spelling before changing
any, and re-grep afterwards:

```bash
grep -rln "<the version>" manifests internal scripts | grep -v embedded
```

It currently reaches: the managed ClusterClass and the management class asset
(`KUBERNETES_VERSION` plus a trimmed `major.minor` companion, many occurrences
each), both provider compositions, the hybrid and spoke Cilium addon payloads
(the `kubectl` image), `node-labeler.yaml`, the flatcar provisioning script, the
Kyverno re-vendor script's arguments, `versions.KubernetesVersion`, the Kind node
image, a topology test, the catalog, and the `--kube-version` recorded in every
vendored render's header.

A missed copy of the trimmed value sends nodes to the wrong package repository,
which fails late and reads as unrelated. `TestKubernetesVersionParity` guards the
constant against the class; nothing guards the rest.

## kubeadm-derived images

The four control-plane images, etcd, CoreDNS and pause are **not pinned
anywhere**. `inventory.py:kubeadm_images` fetches kubeadm's `constants.go` at the
chosen version and reads them out. They move only when Kubernetes does, and they
will never reach their own upstream latest.

Upstream restructures that file: through 1.34 the etcd version was keyed by
literal minor in `SupportedEtcdVersion`; from 1.35 the keys are computed, so the
scraper reads `DefaultEtcdVersion` instead. If the gate reports "constants name
no CoreDNS, pause or etcd version", that file changed shape again — fix the
scraper, do not pin the images.

## Cluster API providers

Each provider has a CR under
`internal/assets/manifests/core/capi-operator/providers/` whose `spec.version` is
authoritative, **and** a matching constant in
`internal/kakt-cli/versions/versions.go`. `TestProviderManifestVersionParity`
compares them; move both in one commit.

What caps the set is usually the *infrastructure* provider's published
compatibility table, which can lag the core project by a release line.

## Crossplane packages

`Provider` and `Function` objects naming an OCI `xpkg` reference:

```bash
grep -rl "package: xpkg" manifests | grep -v embedded
```

Both clusters carry copies (`hub-core-services/crossplane/providers/*` and
`spoke/spoke-catalog/infra/crossplane-*`). Installed by Crossplane's own package
manager, not Helm. Check the provider line against the Crossplane major: a
pre-v2 provider keeps cluster-scoped managed resources, which has to agree with
the scope its composite definitions declare.

## Operator CR fields

An instance's version is a field on its custom resource, not a chart value:

- the CloudNativePG clusters under `hub-core-services/*/database/*-db.yaml` and
  `spoke/spoke-catalog/providers/*/cnpg-cluster.yaml` (`imageName`) — chosen per
  application, with its own rules in `references/databases.md`
- the Grafana instance (`hub-core-services/grafana/grafana.yaml`), `spec.version`

**Always pin these.** An operator's default moves with the operator, so omitting
the field means an operator bump changes the application underneath a workload
that never asked for it.

Pin them where the operator *and* the inventory both read: for Grafana that is
`spec.version`, not a pod-template image override. An override sets the running
image without the inventory seeing it, so the release keeps reporting the
operator's default — a change that looks done and is not.

## Our own images

Built by CI from a commit, with the resolved digest committed back, and recorded
in `kustomization.yaml` `images:` entries as a name plus digest. A placeholder
tag such as `:latest` or `:unbuilt` there is not a mutable tag — kustomize
replaces it.

Never bump these by hand. Two consequences for an upgrade:

- CI commits the digest **after** a push, so a tree that has not pulled still
  pins the previous one. The first release gate exists for exactly this.
- A new image whose build workflow has not reached CI has no published package
  at all, so upstream lookups for it 404.

## Where the inventory reads each kind

`scripts/package/inventory.py` is the map in code. When a component appears in a
release and you cannot find its pin, read the scanner that emitted it rather than
grepping — grep finds the vendored copies and the build output, the scanner names
the source:

| Function | Reads |
| :- | :- |
| `upstream_charts` / `scan_upstream_charts` | component descriptors and appset elements with an `http` repoURL |
| `scan_bundle` | every image the platform's own charts render |
| `scan_operated` / `operated_images` | instances an operator turns into pods, against the `OPERATED` table |
| `scan_capi_providers` | the provider CRs' `spec.version` |
| `kubernetes_version` / `kubeadm_images` / `scan_nodes` | the ClusterClass and kubeadm's constants |
| `scan_bootstrap` | `internal/*/binaries/<tool>.go` for downloaded binaries |
| `check_seed_parity` | the Day-0 constants against what the bundle reconciles |
