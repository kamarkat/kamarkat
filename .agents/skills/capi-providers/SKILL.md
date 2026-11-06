---
name: provider-environment-matrix
description: >
  Reference for how the management hub and the clusters it manages are
  addressed (ADR 036/037/038, ADR-111 Revision 7) — the hub is
  environment-independent and selects only a provider, while each workload
  cluster declares its own environment. Use this skill whenever the task
  involves AppSet paths, provider isolation, the management secret scope,
  spoke pool placement, the environment-manager Helm chart, bootstrap
  wiring, or any question about why a provider-specific component exists
  or does not exist on a given hub. Also trigger when someone asks about
  the boundary ApplicationSets, the `provider` or
  `management.infisicalEnvironmentSlug` Helm values, the infra-provider
  AppSet element, or where a cluster's `environmentSlug` comes from.
---

# Provider Topology and the Management Hub

One shared management hub manages every environment (ADR-111 Revision 7). A hub
is identified by its own `boxId`, not by a matrix cell: it declares no workload
environment, and the clusters it manages each declare theirs.

## The Dimensions

Four concerns, and none is derived from another:

| Concern | ADR | Declared as | Where |
|---------|-----|-------------|-------|
| Management identity | 111 §1 | `boxId` | the box's `values.yaml`, pinned for life |
| Management secret scope | 111 R7 §2 | `management.infisicalEnvironmentSlug` | the box's `values.yaml` |
| Management availability | 111 R7 §3 | the hub's own sizing, backups, certificate policy | the box's `values.yaml` and the hub components |
| Workload environment | 037 | `environmentSlug` | each managed cluster's own declaration |

What a bootstrap passes to the `environment-manager` chart is in
`renderSeedApplication` (`internal/kakt-cli/bootstrap/seed.go`). The two values
that select content are:

| Value | ADR | Source | Purpose |
|-------|-----|--------|---------|
| `environmentRevision` | 038 | git revision | what ArgoCD tracks |
| `provider` | 036 | `--provider` | which infra provider (`local`/`hetzner`) |

There is no `environmentSlug` for a hub and no `--environment` flag on any hub
command. `provider=docker` is mapped to `local` to match the directory name.

## Directory Structure

```
manifests/
├── argocd/environment-manager/    # Helm chart (ADR 038)
│   ├── values.yaml                # provider/topology (CLI-only), management scope
│   └── templates/
│       ├── 01-platform-infra-appset.yaml    # infra boundary
│       ├── 02-platform-data-appset.yaml     # data boundary
│       └── 03-platform-services-appset.yaml # services boundary
├── environments/                  # the hub's own overlay (ADR 037)
│   ├── base/                      # the HubEnvironment and hub-bootstrap-config
│   └── management/                # the one hub overlay: kustomization + patches
└── providers/                     # Provider resources (ADR 036)
    ├── local/k8s/                 # CAPD ClusterClass, Composition, storage-class
    └── hetzner/k8s/               # Hetzner ClusterClass, Composition, CCM, external-dns
```

One overlay, not one per environment. A hub that belonged to one environment had
one overlay per environment and a zone per environment; the leftmost label of
`spec.domain` is a declared subdomain (ADR-051 Amendment 2026-09-18), not a stage.

Workload clusters are not here at all. A tenant declares its own (ADR-082):
`kakt tenant add-cluster` hydrates `registry/clusters/<name>/infrastructure/`
from the repository's own template, and the claim carries the name the tenant
chose — which is why the platform ships no `spoke-pools/<env>/<provider>`
directory and packages no per-combination spoke-pool chart.

## How Paths Resolve at Runtime

```yaml
# The hub's own overlay (ADR 037) -- a constant, because there is one hub overlay
path: 'manifests/environments/management'

# Provider-specific infrastructure (ADR 036)
path: 'manifests/providers/{{ .Values.provider }}/k8s'
```

A managed cluster's secret scope comes from that cluster's own `environment`
label, never from the hub's: reading the hub's would give a cluster the
environment of whatever provisioned it.

## Provider Isolation Rules

1. **No provider-specific code in core platform services.** All Hetzner or
   CAPD specifics live under `manifests/providers/<name>/`.
2. **The AppSet resolves the provider directory**, so adding one needs no
   AppSet change.
3. **Compositions are labelled by provider** so a claim routes to the right
   Crossplane Composition dynamically.

## The Management Secret Scope

1. `manifests/environments/management/` carries the hub's ConfigMap patch
   (`CLUSTER_ID`, `INFISICAL_ENVIRONMENT_SLUG`) and its HubEnvironment patch.
   No provider references.
2. Both are rendered from the one declared input,
   `management.infisicalEnvironmentSlug`, so the hub-operator (which reads
   `INFISICAL_ENVIRONMENT_SLUG`) and every `ClusterSecretStore` resolve one
   scope.
3. The scope is `management`: not a workload environment, nothing is promoted
   into or out of it, and no managed cluster reads it. Infisical creates a
   project with its own defaults, so Day-0 creates this environment before
   writing the hub's secrets into it (`ensureManagementScope`,
   `internal/kakt-cli/infisical/bootstrap.go`).
4. **CODEOWNERS** gates the hub overlay to Platform Engineering.

## Bootstrap Wiring (kakt CLI)

`internal/kakt-cli/bootstrap/seed.go` renders the seed Application that points
ArgoCD at the `environment-manager` chart and passes the values above as Helm
parameters. The orchestrator carries `ManagementScope`, read from the box's own
declaration (`ReadBoxValues`) rather than from a flag.

## Adding a New Provider

1. Create `manifests/providers/<name>/k8s/` with:
   - ClusterClass (CAPI templates)
   - Crossplane Composition (labelled `provider: <name>`)
   - Any provider-specific CRDs, CCMs, webhooks, or secrets
2. Add a claim template to the tenant scaffolding's
   `templates/workload-cluster/infrastructure/spokepool-<name>.yaml`, which
   `add-cluster` selects by provider.
3. No AppSet changes needed — the `infrastructure-provider` element
   dynamically resolves `manifests/providers/{{ .Values.provider }}/k8s`.
4. Add a provider driver in `internal/kakt-cli/bootstrap/` implementing the
   `Provider` interface.
5. Add the combination to `supportedMatrix` in both the chart's `values.yaml`
   and `internal/kakt-cli/tenant/scaffold.go`; a test asserts they agree, and
   `add-cluster` refuses a combination the platform ships no source for.
