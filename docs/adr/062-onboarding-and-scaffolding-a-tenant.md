# ADR-062: Onboarding and Scaffolding a Tenant

**Date:** 2026-09-06
**Status:** Accepted — implemented as `kakt box init`; `nutgraf-gitops-1794508917`
is a repository this produced.

**Scope: a TENANT, not an application.** This ADR is how an organisation becomes a
running box: repositories, scaffolding, who may write where. It does not describe
how a product team puts a workload on that box — what the platform delivers
unasked, what a fleet declares, which labels a chart must carry. That is
`docs/onboarding-an-application.md`, and a reader who arrived here looking for it
should go there instead.

*Amended by: ADR-065 (The Control Plane Ships Into the Box), ADR-073 (Workload Delivery is Version Pinning), ADR-088 (The Tenant and the Application Are Different Axes)*

> A tenant's control plane runs in that tenant's own box (ADR-065). Workload state
> is published from a tenant-owned repository and the gitops repository holds a
> version and values (ADR-073), which resolves the tension between the workload
> separation asserted here and ADR-047's rejection restated here.

*Amended by: ADR-076 (Reaching a Box You Own)*

> Onboarding now produces a box the tenant can reach. Day-0 generated an admin
> kubeconfig and discarded it with the runner, so a tenant finished onboarding
> owning a cluster they held no credential for. Access is OIDC through the
> tenant's own identity provider; the admin credential is escrowed outside the
> box, in an account the tenant controls.

*Amended by: ADR-082 (Tenant Repository Layout and Cluster Naming)*

> The layout asserted here stands and is extended. A repository holds exactly one
> management cluster and any number of workload clusters, and the directory of a
> workload cluster also holds the declaration that creates it; the management
> cluster's directory holds the Application that applies it. That declaration was
> shipped in the bundle, so its name was identical on every box and every identity
> derived from it collided — including the WAL archive prefix, which left the
> management cluster's own database with no completed backup on any box. A
> workload cluster's name is now chosen by the tenant, written once, and is the
> cell id.

## Context

ADR-004 establishes a dual-repository contract: platform code in `zero-ops`, tenant runtime state in `fleet-registry`. ADR-007 defines the fleet-registry spoke flow. ADR-047 defines the tenant deployment contract and rejects fleets authoring external workload repositories.

Nothing in the decision record says how an organisation becomes a running box. ADR-065 has the platform propose changes to a tenant's infrastructure repository, which presumes that repository exists, is structured in a way the platform can propose against, and is reconciled by a control plane. None of those are true of an organisation that has just arrived.

Five facts bear on this decision.

**The word "fleet" names two different things in the decision record.** ADR-033 uses it for the cluster estate. ADR-047 uses it for a single tenant's deployment boundary. The implementation follows neither consistently — the tenant provisioning ApplicationSets are written in terms of `tenantId`, while ADR-047 specifies `fleetId`, and both appear in the same file.

**`fleet-registry` holds two kinds of content with different owners and change rates.** Per-tenant, per-environment values, which change when a tenant is onboarded or reassigned; and rendered workload state, whose image digests change on every build of every tenant. Those writes share a branch and contend, and the contention scales with tenants multiplied by build frequency.

**Cluster instances and cluster definitions share commit rights.** SpokePool declarations are stored in `zero-ops` beside the Compositions and XRDs that define what a spoke pool is, so declaring a cluster and changing the meaning of a cluster are the same privilege.

**Tenants are connected through a GitHub App.** The App installation is the platform's record of which repositories belong to which tenant, and it is the access under which ADR-065 permits the platform to propose.

**A comparable platform scaffolds and does not return.** kubefirst creates repositories in the customer's own organisation and renders a template into them, substituting some thirty tokens whose reference documentation warns explicitly that none may carry a secret. Its placement logic prunes every provider but the one selected, flattens that provider's directory to the repository root, discards the upstream history, hydrates a cluster directory from a template and then deletes the template that produced it. What remains in the customer's repository is a version pin and values per component; chart bodies are referenced from upstream registries and never copied. Nothing propagates to that repository afterwards.

## Decision

**A tenant is onboarded by creating and scaffolding the repositories its box reconciles.**

### The tenant supplies a git organisation and a cloud account

Onboarding begins with an App installation on the tenant's git organisation and a cloud account the tenant controls. The App installation is the platform's only credential, and it reaches repositories and nothing else.

Cloud credentials are not supplied to the platform. They are held in the tenant's box under ADR-065, and the Day-0 sequence that establishes the control plane is run by the tenant against its own account. The platform scaffolds what Day-0 reconciles; it does not provision the tenant's infrastructure.

### The platform creates and scaffolds the box's repository

The platform creates one repository per box, named as below, in the tenant's organisation, and renders it from a template held in `zero-ops`, substituting the identifiers and locations that make it that tenant's: organisation, repository locations, domain, cloud provider and region, cluster names.

The repository holds one directory per cluster — the tenant's control plane and each of its spoke clusters alike — and each such directory holds two things: the bundle version that cluster runs, and the values it runs with. It also retains the templates from which the tenant renders its next cluster.

Nothing of the bundle's content is placed there. Under ADR-063 the bundle is published as a versioned chart and consumed by naming a version, so the platform writes the version file and the tenant writes the values file. That the two write to separate files is what allows a fleet of these repositories to be maintained without arbitration, and it is the reason the content is published rather than copied.

**The values file carries deviations, not defaults.** Scaffolding writes the facts that are true of this tenant and no other — organisation, domain, region, cluster names, selected workloads — and nothing that merely restates what the chart already specifies. A tenant customises by adding the field it wants to change. Copying the chart's defaults into the repository at scaffold time would make every later change to a default a proposal into every tenant's repository, and a conflict with any tenant that had edited the same file.

Separation is by file rather than by permission. A repository in the tenant's organisation is administered by that organisation, and no arrangement of access control changes that; a repository the tenant could not write would also be one it could not customise, and would contradict the ownership ADR-065 asserts. Review rules on the version file express the intended division and are the tenant's to keep or remove.

Substituted values carry no secret material. They are identifiers and locations. Secrets are generated into and delivered from the tenant's own store, which the standing secret lifecycle already requires and which onboarding does not vary.

### A box mints its own names, once, from one timestamp

A box outlives every cluster it ever runs (ADR-111 §1), and a tenant may have more than one. So a name derived from the tenant alone is not an identity: a second box, or a box rebuilt after the first was torn down, would collide with the first in the escrow paths, the DNS owner record and the repository name alike.

`kakt box init` therefore mints the box's identity at the moment the box is created — one Unix-seconds timestamp, three names derived from it:

| Name | Shape | What it keys |
|---|---|---|
| Box id | `<tenant>-<timestamp>` | the escrow paths and `dns.ownerId` |
| GitOps repository | `<tenant>-gitops-<timestamp>` | where the box's desired state lives |
| First management cluster | `<tenant>-mgmt-<timestamp>` | the CAPI objects and the WAL archive prefix (ADR-082) |

**Minted once and then read, never re-derived.** The timestamp is taken immediately before the identity is first needed and written into the box's own declaration, `registry/box.yaml`, before any remote call. A run that stops part-way is continued with `kakt box init --resume <checkout>`, which reads those names back rather than minting a second set — so the same box cannot acquire two identities however many times the command runs.

**`-mgmt-`, not `-hub-`.** The cluster is named for what it is. "Hub" survives only in the names of components that were built under it, such as `hub-operator`, where it is a component identifier rather than a description of a cluster.

**The box id is not the cluster name.** They are different lifetimes: `clusterName` keys the CAPI objects of one cluster and changes when that cluster is replaced, while `boxId` is the one fact that must outlive every cluster. Nothing may derive one from the other.

### Scaffolding happens once; maintenance is continuous

The rendered repository is a starting state, not a fork. Once it exists the platform proposes changes to it under ADR-064 and never renders it again.

This is the difference from the platform examined above, and it is the whole of the service: scaffolding without maintenance leaves a tenant holding a copy that ages, and maintenance without scaffolding has nothing to propose against.

### The platform writes a branch, never a default branch

Every write the platform makes into a tenant's repository is a branch and a pull request. That is true of scaffolding's own follow-ups, of ADR-064's promotions, and of anything a future mechanism adds on a tenant's behalf. The platform holds write access because a pull request requires it; holding it is not permission to use it against `main`.

The platform examined above ends its equivalent operations differently. Adding a component to a customer's cluster clones the customer's repository, renders the component into the cluster's registry directory, commits with a message naming the requesting user, and pushes to the default branch. The cluster reconciles moments later, because there is nothing in between.

That is coherent there and would not be here. Their customer operates the console that issues the request, so the push executes an instruction its own operator just gave: the commit records who asked, and the answer is always someone on the customer's side. Here the platform is a party outside the tenant, and ADR-065 fixes what that party may do — it may propose changes to a tenant's platform state, and may never be the authority that causes those changes to take effect. A push to `main` on a repository whose reconciler auto-syncs *is* that authority, whatever the commit message says.

The difference is not a preference about review. It is that a pull request leaves a decision point owned by the tenant, and a push does not. A tenant that wants the fast path can enable auto-merge, and the authority is then delegated deliberately by the tenant rather than assumed by the platform — which is the same end state reached from the correct direction.

The cost is real and is accepted: every change costs a round trip, and a tenant that ignores its pull requests drifts while believing it is maintained. ADR-067 is what makes that drift visible, and ADR-069 is what bounds how long it may continue before the platform stops promising maintenance for the version left running.

### Repositories are separated by what their content is

Repositories are separated by what their content **is** — a type, an instance, or a workload — rather than by who authored it, because that division is also the maintenance boundary.

The platform maintains types, proposes changes to instances, and never touches workloads. Those are three different relationships, and a repository is the unit at which access is granted and revoked. Separating by content puts each relationship in its own repository, so what the platform may do to a thing follows from where the thing lives rather than from a rule applied on top of it.

**`zero-ops` holds types.** XRDs, Compositions, ClusterClasses, boundary charts, the scaffolding template, and the catalogue of selectable workloads. It defines what a spoke cluster is, what a tenant is, and what a boundary is. It holds no instance of any of them.

**`<tenant>-gitops` holds cluster instances.** One per tenant, in the tenant's own organisation, created by scaffolding, declaring each of that tenant's clusters — control plane and spokes alike — with the bundle version each runs and the values each runs with, together with the cell definitions that group them for policy and the workloads the tenant has selected. The tenant's own control plane reconciles it.

`fleet-registry` is this repository for the platform's own box, and is the name retained for it.

**Tenant-owned repositories hold workloads.** In the tenant's organisation, holding overlays and image pins, separate from `<tenant>-gitops` because the platform is granted access to one and never to the others. The platform does not fix their number; a tenant has as many as it has applications.

### The App installation is the record

Which repositories belong to which tenant is established by the App installation and is not restated elsewhere. No separate registry repository records tenant existence, because nothing reconciles such a record: under ADR-065 no platform-run component reconciles anything on a tenant's behalf.

What remains — that an organisation is a customer, what it has bought, what it is owed — is commercial and is not held as reconciled state.

### A credential is named for the access it grants

Every credential this platform asks for is named for what it is permitted to do and who owns it, never for the mechanism that implements it. An operator is asked for the access; whether they satisfy it with a GitHub App installation token, a classic PAT or a fine-grained one is theirs to choose and is not a question the CLI asks.

| Variable | The access | Owner |
|---|---|---|
| `GITHUB_ORG_SETUP_TOKEN` | create this tenant's GitOps repository in their organisation | the platform operator, on their own machine, across every box |
| `GITOPS_REPO_WRITE_TOKEN` | write the box's GitOps repository, and read it from ArgoCD; inherited by the tenant's application builds so they can pin a chart version | the tenant, in their own organisation's secrets |

No credential substitutes for another. Each is read by its own name and a missing one is reported by that name: the bootstrap that gives ArgoCD write access to the box's own repository fails when `GITOPS_REPO_WRITE_TOKEN` is absent rather than reaching for the operator's credential or the Actions token, because either would be stored in the cluster's repo-creds Secret and outlive the run that borrowed it (ADR-065).

`GITHUB_TOKEN` names neither. It is reserved for GitHub Actions' own job token and for third-party interfaces that require that exact variable name — a credential of ours parked under it is read by whatever else happens to be in the environment, and inside a workflow it is silently the wrong one: Actions' token reaches only the repository running the job, never its siblings.

This replaces two earlier names that described implementations rather than access. `GITHUB_APP_TOKEN` and `GITHUB_TOKEN` were two variables for one permission, and which one an operator had chosen changed nothing but a line of output; they are now one variable, and the kind of token is read from the token itself. The boundary is unchanged: two credentials, two owners, each revocable without the other (ADR-065).

### Vocabulary

One word per concept, used unchanged by ADR-063 through ADR-067.

**Platform** is the golden path: the types, the bundle, and the engineering that maintains them.

**Tenant** is a customer organisation, and holds exactly one **box** — control plane access together with that tenant's own spoke clusters. It is `tenantId`.

**Application** is a product running on that box, and is `appId`. It is the isolation unit: it gets the namespace, the database, the identity project, the secret prefix and the policy. One tenant holds any number of applications, and they share nothing but the box.

The tenant and the application are **different axes** (ADR-088), and neither defaults to the other. This section said `fleetId` in ADR-047 is `tenantId` — which answered the vocabulary problem in this ADR's own Context by deleting a level rather than naming one. The level it deleted was the tenant, which this ADR defines as the owner of a box; what the single identifier then held was the product. So a customer running two products had two "tenants" and no way to say they were the same customer. Both axes are now named, and every namespace, role, secret path and policy selector is built from the pair.

**Spoke cluster** is the unit of everything measurable. It declares its region, provider and node pool, pins a bundle version, and is where compute is metered. It belongs to exactly one tenant, and is a **cluster instance** when referred to as a record in the tenant's infrastructure repository.

**Cell** is a dynamic grouping of one tenant's spoke clusters, by label, for the purpose of attaching policy. A cluster may belong to several cells and receives the union of their policy. A cell is a selector: it holds no compute, pins no version, and owns nothing.

**Fleet** is the cluster estate, as ADR-033 already uses it.

**Bundle** is the complete set of platform content at one revision of `zero-ops`; a **bundle version** is the tag naming it. **Promotion** is advancing a cluster instance from one bundle version to the next.

**Workload** is what a tenant runs on its own clusters.

### A tenant authors its own instances

A tenant declares its clusters, the cells that group them, and the workloads it selects. All are held in the tenant's own infrastructure repository and reconciled by the tenant's own control plane.

The reasoning that once made cluster identity platform-held was cross-tenant: a tenant able to author a region or a secret path could name something outside its own box. With a single-tenant cluster and a control plane in the box there is nothing outside the box to name. What remains is the platform's interest in a declaration being well-formed, which is asserted by the validators ADR-064 records rather than by withholding the field.

### Alternatives considered

**Hold types and instances in one repository, separated by directory, as kubefirst does.** Rejected here, and correct there. Its template and its rendered instances live in one repository because the customer owns both after the render: no boundary runs through that repository, so no repository boundary is needed. Here the platform maintains the types and proposes to the instances, and a directory is not a unit at which access is granted or revoked. Placing both in one repository would require the platform to hold write access to a tenant's types in order to propose against its instances.

**Push directly to the tenant's default branch, as the platform examined above does.** Rejected, and correct there. It is the fastest path from a decision to a running cluster, and it costs nothing where the party pushing and the party owning the repository are the same. They are not the same here, and an arrangement in which the platform's write reaches a tenant's cluster without the tenant acting is the one ADR-065 exists to prevent. Rejecting it is what makes the authority claim testable rather than a statement of intent.

**Retain one repository for all tenants, with directory-level ownership.** Rejected. It does not remove the write contention, because the contention is on the branch rather than on the paths. It also leaves history shared, so a tenant cannot be granted write access to its own state without being granted every other tenant's history.

**Retain cluster instances in `zero-ops` under CODEOWNERS.** Rejected. Declaring a cluster should not require commit rights over the Compositions that define what a cluster is, and under ADR-065 the instances are reconciled by the tenant's control plane, which cannot read a repository the tenant does not hold.

**Record tenant existence in a dedicated registry repository.** Considered and not taken. It was the natural shape while tenant discovery was a reconciliation input — the set of tenants was whatever a directory glob returned. With no platform-run component reconciling on a tenant's behalf, such a repository would have no reconciler, and the App installation already records the fact it would hold. Should a platform-operated control plane serving several tenants be adopted, tenant discovery becomes a reconciliation input again and this decision is revisited with it.

## Ownership

| Resource Class | System of Record | Lifecycle Owner | Reconciler | Consumer | Phase |
|---|---|---|---|---|---|
| Platform type definitions | `zero-ops` | Platform | ArgoCD | Platform | Day-1+ |
| Scaffolding template | `zero-ops` | Platform | — | Onboarding | Day-0 |
| Repository ownership record | App installation | Tenant | — | Platform | Day-0 |
| Cluster instance declarations | `<tenant>-gitops` | Tenant | Crossplane / CAPI | Tenant control plane | Day-1+ |
| Bundle version per cluster | `<tenant>-gitops` | Platform | ArgoCD | Tenant control plane | Day-1+ |
| Cluster values (deviations) | `<tenant>-gitops` | Tenant | ArgoCD | Bundle chart | Day-1+ |
| Default values | published bundle chart | Platform | Release pipeline | Tenant control plane | Day-1+ |
| Cell definitions and policy binding | `<tenant>-gitops` | Tenant | ArgoCD | Spoke clusters | Day-1+ |
| Published bundle charts | OCI registry | Platform | Release pipeline | Tenant control planes | Day-1+ |
| Tenant workload state | tenant workload repositories | Tenant | ArgoCD | Tenant | Day-1+ |

In every row Git is the System of Record and the corresponding in-cluster resource is its reconciled projection. See ADR-039 for the complete ownership matrix.

## Consequences

### Positive

An organisation becomes a running box by a defined sequence rather than by manual assembly, and the repository the platform later proposes against exists because onboarding created it.

Write contention disappears rather than being mitigated, because per-tenant repositories share no branch.

A tenant can be granted write access to its own desired state without being granted any other tenant's history.

Declaring a cluster and defining what a cluster is carry different commit rights.

The platform holds one credential per tenant, reaching repositories only, and revoking it ends the relationship without stopping anything running.

### Negative

A tenant that does not act on its pull requests stays where it is. Proposing rather than pushing means the platform cannot correct a running cluster on its own, including when the correction is one it considers urgent, and the tenant that most needs the fix is the one least likely to be watching for it.


Onboarding becomes software the platform must maintain, and a defect in it produces a malformed repository that a tenant owns and the platform can only propose to fix.

Two or more repositories per tenant, each requiring a credential, ownership rules and continuous integration. They are the tenant's to carry, but the failure in which one repository is named two ways now occurs inside a tenant's box where the platform cannot see it.

A change spanning a type and its instances spans repositories and cannot be atomic. Ordering that a single commit once guaranteed has to be reasoned about.

The scaffolding template is a second expression of what a box contains, alongside the bundle chart, and the two can disagree. What a tenant's repository states is a version and values, so reading it does not say what runs without resolving the chart behind it.

## Impact

Supersedes ADR-004. The dual-repository contract is replaced by the separation above.

Amends ADR-047. The rejection of fleets authoring external workload repositories stands on its original ground. Its `fleetId` is resolved into the two axes named in Vocabulary above — `tenantId` for the organisation, `appId` for the product — not into `tenantId` alone, which is what this ADR first said and ADR-088 corrected.

Amends ADR-007. The spoke flow is unchanged in mechanism, but the repository it names now holds the platform's own cluster instances.

Amends ADR-031. A spoke cluster belongs to exactly one tenant, so the shared-cell blast radius that ADR's topology was written to contain does not arise between tenants. What remains within a cluster is isolation between one tenant's own workloads.

Confirms ADR-033. Its use of "fleet" for the cluster estate is adopted as the platform's definition.

Amended by ADR-065. A tenant's control plane runs in that tenant's box, which places the infrastructure repository with the control plane that reconciles it.

No change to ADR-021, ADR-037, ADR-039, ADR-043 or ADR-055. No change to ADR-061: a component declares its own source, so a cluster-instance component naming a different repository is already expressible.

## References

- ADR-004: Dual Repository GitOps Pattern (superseded by this ADR)
- ADR-007: Fleet Registry Spoke Flow
- ADR-021: Boundary-Driven GitOps and Day-0 Choreography
- ADR-031: Tenant Secret Isolation and Identity Topology
- ADR-033: Fleet Scale Targets & SLOs
- ADR-037: Directory-Based Environment Promotion and Gating
- ADR-039: Platform Ownership Model
- ADR-043: Control Plane Authority Model
- ADR-047: Fleet Tenant Deployment Contract
- ADR-055: Boundary Activation as the Day-0 Gating Mechanism
- ADR-061: Component Descriptors for Boundary Composition
- ADR-063: The Platform Bundle and its Version
- ADR-064: Bundle Promotion and Cell-Scoped Policy
- ADR-065: The Control Plane Ships Into the Box
- ADR-067: Support Telemetry and the Basis of Maintenance
- ADR-069: The Maintenance Promise
- ADR-082: Tenant Repository Layout and Cluster Naming
- ADR-088: The Tenant and the Application Are Different Axes — the second axis this ADR's Vocabulary now names
- `docs/onboarding-an-application.md` — putting a workload on a box this ADR created
