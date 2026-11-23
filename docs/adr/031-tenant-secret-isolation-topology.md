# ADR 031: Tenant Secret Isolation and Identity Topology

*Amended by: ADR-062 (Onboarding and Scaffolding a Tenant), ADR-064 (Bundle Promotion and Cell-Scoped Policy)*

> Secret isolation is bounded by a box: a spoke cluster belongs to exactly one
> tenant, and no secret store serves more than one. The cross-tenant reasoning
> below describes a topology the platform no longer has.

## Status

Accepted

## Context

As the Zero-Ops platform scales, managing secret distribution across the Hub and Spoke architecture presents two competing challenges:
1. **API Limits & Management Overhead**: Generating a unique Infisical Machine Identity for every single SaaS tenant (which could be tens of thousands) exhausts Infisical API limits and creates an unmanageable explosion of IAM policies.
2. **Tenant Isolation (Blast Radius)**: Relying solely on a single global `ClusterSecretStore` on each Spoke cluster violates the principle of least privilege. If the global store is used, any misconfigured `ExternalSecret` could accidentally pull credentials belonging to another tenant.

We need a topology that minimizes the number of Machine Identities while maximizing Kubernetes namespace isolation, aligning with our existing GitOps governance (ADR-021).

## Decision

We will implement a **Cell-Based Identity Topology with Namespace-Scoped Secret Stores**:

1. **Spoke-Scoped Machine Identities**: The Hub Operator will generate exactly *one* Infisical Machine Identity per Spoke Pool (Cell). 
2. **Path-Based IAM**: This identity will be granted read access exclusively to its specific cell paths:
   - `/spoke-pool/<cell-id>/shared/*` (Platform/Shared credentials)
   - `/spoke-pool/<cell-id>/tenants/*` (Tenant-specific credentials)
3. **Identity Projection**: The Hub will project this Machine Identity's credentials into the Spoke's `tenant-<id>` namespaces using the platform's bootstrap `ClusterSecretStore`.
4. **Namespace-Scoped SecretStores**: Inside each tenant namespace, the platform will provision a local `SecretStore` that consumes the projected Machine Identity. All tenant-specific `ExternalSecret` resources (e.g., database credentials, JWT keys) will strictly reference this local `SecretStore`.

### Security Boundary Enforcement

Because the Spoke-scoped Machine Identity has access to *all* tenants within that cell, isolation relies on Kubernetes RBAC and GitOps boundaries:
- **Platform Control**: Only the AINativeSaaS Crossplane XR (governed by the platform team) can create `ExternalSecret` and `SecretStore` resources.
- **Tenant Restriction**: Per **ADR-021** (Tenant ABI), the `tenant-workloads` ArgoCD AppProject explicitly **whitelists** the resources tenants can deploy (Deployments, Rollouts, Services, etc.). Tenants are **denied** the ability to create `ExternalSecret` objects, making it impossible for a malicious tenant workload to query Infisical for another tenant's paths.

## Ownership

| Resource Class | System of Record | Lifecycle Owner | Reconciler | Consumer | Phase |
|---|---|---|---|---|---|
| Tenant Passwords | Infisical | Tenant Identity Service | ESO | Tenant Apps, provider-sql | Day-1+ |
| Machine Identities | Infisical | Hub Operator | Infisical API | Spokes, Operators | Day-0 |

See ADR-039 for the complete ownership matrix.

## Consequences

### Positive
* **Scalability**: Reduces Infisical Machine Identities from $O(Tenants)$ to $O(Spokes)$. Dramatically reduces API pressure on the secret backend.
* **Blast Radius Reduction**: A compromised Spoke cluster's Machine Identity can only read secrets for tenants residing on that specific Spoke, rather than the entire global fleet.
* **Explicit Boundaries**: Using `SecretStore` instead of `ClusterSecretStore` for tenant resources prevents platform engineers from accidentally cross-wiring secrets between namespaces during composition development.

### Negative
* **Shared Cell Risk**: A complete compromise of the Kubernetes API on a Spoke cluster would expose all tenant secrets on that Spoke. (Mitigated by the fact that a K8s API compromise already implies full cluster compromise).
* **Two-Hop Resolution**: Bootstrapping a tenant requires a two-hop secret resolution: ESO must first use the `ClusterSecretStore` to fetch the Machine Identity, and then use the local `SecretStore` to fetch the actual application secrets.

## Amendment (2026-05-28): Tenant Classification and Spoke Silo Model

### Context
The shared blast radius of Spoke Pools—where one Machine Identity accesses all tenant secrets within a cell—is acceptable for standard workloads but violates compliance requirements for highly regulated data.

### Decision
Tenant workloads are strictly categorized and placed into distinct Spoke topologies based on compliance requirements.

**Spoke Pool Model:**
Utilized exclusively for non-regulated, standard commercial tenants. A single Spoke-scoped Machine Identity manages secrets for all tenants within the shared cluster.

**Spoke Silo Model:**
Mandatory for tenants requiring PCI, HIPAA, or SOC2 strict compliance. These tenants are provisioned into dedicated, single-tenant Spoke clusters. The Machine Identity is scoped exclusively to the single tenant, ensuring total physical and cryptographic isolation. Platform operators are forbidden from scheduling regulated workloads onto Spoke Pools.

### Consequences
- **Positive:** Provides clear regulatory isolation for compliance-bound tenants.
- **Negative:** Increases operational cost for siloed Spoke clusters.
- **Negative:** Requires tenant classification at provisioning time.

## References
* **ADR-003**: Infisical as Single Source of Truth
* **ADR-019**: Runtime Plugin Credential Resolution
* **ADR-021**: GitOps Governance and Tenant ABI Enforcement


## Correction (2026-11-23): the path was never the boundary

This ADR describes a spoke's Machine Identity as authorised for
`/spoke-pool/<clusterName>/` only. It was not, and could not be, on the Infisical
this platform ships.

A built-in project role carries no conditions. Infisical v0.167.12's
`buildViewerPermissionRules` grants `DescribeSecret` and `ReadValue` on `Secrets`
with no `environment` and no `secretPath` qualifier, so a member of a project reads
every path of it. Conditioning a role on either requires a custom role, and
`createRole` refuses one without `PlanFeature.Rbac` -- `false` in the default
entitlement set, along with `secretsFolderRbac`. The grants in effect were broader
still: `spoke-identity-operator` requested `admin` on two projects and
`hub-operator` requested project `viewer` for a second identity.

So the prefix expressed **ownership** and enforced nothing. The live evidence was
in plain sight: the tenant image-pull `ExternalSecret` read `ghcr-pull-secret` at
the project ROOT, outside any cluster prefix, and resolved.

**What enforces it now.** One Infisical project per workload cluster, slug
`spoke-<cluster>-secrets`, created by `hub-operator` -- which owns it because it
writes the credentials into it -- with only the environment the cluster declares.
The spoke's identity is a `viewer` of that project and a member of nothing else;
`hub-secrets` and the Certificate Manager project are removed from it. The prefix
stays, because it still says who owns a secret, and because a project is a
container rather than a naming scheme.

**The identity is narrowed only once the project holds its credentials.**
`hub-operator` publishes the slug on the `SpokeMachineIdentity` after
materialising them, and `spoke-identity-operator` waits for that field. Narrowing
first would turn an over-privilege defect into failed image pulls and a database
with no backup credential.

**PKI is a separate authority.** Certificate issuance authorises through
Infisical's application surface, not a project: the issuer's identity holds
`operator` on the PKI application, plus `member` on the Certificate Manager
project, which is the minimum built-in role that can read a certificate profile by
slug (`viewer` cannot). It is a different identity from ESO's, with its own
credential, so neither consumer can exercise the other's authority. `member` is
broader than the one read it exists for and OSS cannot narrow it; what bounds it is
that project's content -- PKI objects, never this platform's secrets.

Gated by `scripts/validate/preflight/037-cluster-secret-project.py` and
`031-infisical-api-contract.py`.

## Delivering a per-spoke field (learned releasing 0.3.45)

A per-spoke field on the spoke catalogue is delivered twice, and changing one path
is the failure that cost two release candidates:

- **Unreleased manifests** — the `kustomize.patches` on the ApplicationSet
  (`03-platform-services-appset.yaml`, the `{{- else }}` branch).
- **A released box** — the field rules in
  `manifests/spoke/spoke-catalog/templated-fields.yaml`, because an ArgoCD source
  naming a Helm chart cannot also carry patches (ADR-063).

The store's `projectSlug` was patched on the ApplicationSet alone. Every check
agreed, because the only check that compared the slug read the same path that had
been edited. The packaged chart — what a released box actually runs — still named
`hub-secrets`, so on `nutgraf-stg-01` the identity was narrowed correctly and the
store then could not validate: it was pointed at a project the identity is
deliberately not a member of. `hetzner-dns-credentials` had the same omission one
release later.

`037-cluster-secret-project.py` now reads both paths and asserts they name the
same project. A check that knows one of two delivery paths cannot see a
divergence between them.

## What a verifier may conclude (learned releasing 0.3.45)

`031-cluster-secret-isolation` could not run for its first three candidates: it
called the identity listing without the `orgId` that route requires, took the 422
as "no such identity", and reported that through `soft_fail` — which means *not
yet converged, expected to self-heal*. A malformed query never self-heals, and the
property had to be established by hand instead.

Three rules follow, and they apply to any check that asserts an authorization
property:

1. **"Could not put the question" is not "the answer is no."** One return value
   for both makes a broken verifier indistinguishable from a finding.
2. **Unobtainable is `unverified`, never `soft_fail`.** Soft means converging.
   Nothing about a refused credential or a rejected query converges.
3. **A partial inventory asserts nothing.** Only the specific "membership not
   found" 404 is an answer; any other response means that project would not say,
   and the identity cannot be counted absent from it. This check returned its
   partial result whenever anything had been found, so one project erroring while
   the cluster's own project answered produced a complete-looking inventory and a
   PASS on "a member of nothing else" — loudest in exactly the case worth
   catching, an identity still holding `hub-secrets` when `hub-secrets` is the
   project that errored.
