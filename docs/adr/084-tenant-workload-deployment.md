# ADR-084: Tenant Workload Deployment

**Date:** 2026-09-21

**Status:** Proposed

*Constrained by: ADR-022 (Platform-Governed Workload Bases), ADR-039 (Platform Ownership Model), ADR-047 (Fleet Tenant Provisioning), ADR-050 (Tenant Gateway), ADR-051 (Public TLS and Hostname Ownership), ADR-053 (OAuth Client Provisioning), ADR-065 (The Control Plane Ships Into the Box), ADR-073 (A Fleet Chooses Versions, Not Locations), ADR-082 (Tenant Repository Layout and Cluster Naming)*

> An application's chart is published by the repository that builds it. The
> tenant's GitOps repository names a version of that chart and nothing else.

## Topology

The whole path, from a commit in an application repository to a request served
on a public hostname. Everything inside the boundary is the tenant's box.

```
   APPLICATION REPO                         the tenant's box — ADR-065
  ┌──────────────────┐   ┌────────────────────────────────────────────────────┐
  │ src/             │   │                                                    │
  │ charts/<app>/    │   │  MANAGEMENT CLUSTER          WORKLOAD CLUSTER      │
  │   Chart.yaml     │   │  ┌────────────────────┐     ┌───────────────────┐  │
  │   templates/     │   │  │ ApplicationSets    │     │ tenant-<fleet>    │  │
  └────────┬─────────┘   │  │  05 fleet xr       │     │  ┌─────────────┐  │  │
           │ CI          │  │  05 fleet spoke    │────▶│  │ AgentGateway│  │  │
           │             │  │  05 fleet workload │     │  └──────┬──────┘  │  │
      ┌────┴────┐        │  │  06 public tls     │     │         │ derived │  │
      ▼         ▼        │  └─────────┬──────────┘     │         ▼         │  │
  ┌────────┐ ┌────────┐  │            │ reads          │  ┌─────────────┐  │  │
  │ image  │ │ chart  │  │            │                │  │ <app>-      │  │  │
  │ @sha256│ │  OCI   │  │            │                │  │  workload   │  │  │
  └────────┘ └───┬────┘  │            │                │  │  Rollout    │  │  │
                 │       │            │                │  │  + Service  │  │  │
                 │ ver   │            │                │  └─────────────┘  │  │
                 ▼       │            │                └─────────▲─────────┘  │
  ┌──────────────────┐   │            │                          │            │
  │ GITOPS REPO      │◀──┘            │                  platform-ops         │
  │ environments/    │────────────────┘                  ┌───────────────┐    │
  │  <env>/          │                                   │ tenant-tls-   │    │
  │   values.yaml    │  fleet declaration                │  gateway :443 │    │
  │   <app>/         │                                   │ + Certificate │    │
  │     Chart.yaml   │  dependency + version only        │ + HTTPRoute   │    │
  └──────────────────┘                                   └───────▲───────┘    │
                                                                 │            │
  └──────────────────────────────────────────────────────────────┼────────────┘
                                                                 │
                                                        https://<host>
```

## Context

A tenant's workloads are reconciled from the tenant's own GitOps repository
(ADR-082), by ApplicationSets the platform renders into the management cluster
(ADR-047). What those ApplicationSets read, and who writes it, was decided
piecemeal across ADR-022, ADR-047, ADR-051, ADR-053 and ADR-073, and no single
document describes the path end to end. A reader wanting to know how an
application reaches a public hostname has had to assemble it from five ADRs and
the ApplicationSet templates.

Three facts already established constrain any answer.

ADR-073 withdrew `gitRepo`, `gitPath` and `gitRevision` from the fleet: a fleet
chooses neither repository, revision nor location. What remains for it to choose
is a version.

ADR-051 assigns public hostnames, their certificates and the routes carrying
them to the platform, and rejects tenant-authored routes by name — where routing
is a security control, ownership belongs with the control.

ADR-022 defines a platform-governed base for stateless web workloads whose
constraints a tenant cannot weaken. That base is expressed as a Kustomize base
and is not published to a chart registry, so a Helm chart cannot depend on it.

Two observations from the running platform, rather than from documents.

The constraints ADR-022 lists are also enforced at admission by a Kyverno
ClusterPolicy on each workload cluster: container images must match a digest
pattern, pods must run as non-root, and workloads must carry cost labels. A
manifest that weakens one is refused by the cluster.

An application build that must write to the tenant's GitOps repository needs a
credential to do it. Before this ADR no such credential existed for any
repository other than the GitOps repository itself: one application's build had
published a chart on every commit for several months and pinned a version not
once, with its publish step green each time.

## Decision

An application's deployment is expressed in two repositories, and the division
between them is by kind of fact rather than by convenience.

**The application repository holds the chart.** The chart that renders an
application's Kubernetes resources lives beside the source that produces its
image, is versioned with it, and is published by its build to an OCI registry in
the tenant's own organisation. Not the platform's registry: an application must
not stop deploying because the platform's registry is unavailable. Not a
registry inside the cluster: that is a stateful component requiring backup,
bought for a failure domain the management cluster already shares.

**The GitOps repository holds a dependency on it.** One directory per
application per environment, each containing a chart whose only content is a
dependency on the published chart at a version. No image reference, no overlay,
no rendered manifest. A rendered manifest in a GitOps repository is a copy, and
a copy has no version — ADR-073's distinction between identifying content and
identifying where a copy was put.

One directory per application, and not one file listing every application: a
shared file makes every build of every application contend on it, and collapses
a tenant's workloads into a single Application sharing one sync, one health
state and one blast radius.

**The version is the only field a build writes.** An application's CI publishes
its chart, then commits that version to the one line describing that application
in that environment. Promotion between environments is the same act with the
same version and a different target, so what was tested is what ships.

**The image is pinned by digest and travels inside the chart.** It is carried as
the chart's `appVersion`, set at package time, so no field exists in the GitOps
repository in which an image could be written. A digest rather than a tag: the
workload cluster's admission policy matches the rendered image reference against
a digest pattern and refuses anything else, so a tag does not deploy and later
drift — it is rejected before it runs.

**A chart may carry the ADR-022 shape directly.** ADR-022's base cannot be
depended upon from a chart registry, and vendoring it into each application
would create copies that drift. The constraints are enforced at admission
regardless of how the manifest was produced, so the guarantee does not rest on
inheritance. ADR-022's base remains the reference expression of the shape and
the starting point for a new application; admission is what makes it binding.

**Backend names are derived by the platform, not declared by the fleet.** The
tenant's gateway resolves an application's service by a name derived from the
application and the tenant namespace. A fleet able to name its own backend could
route a public hostname at another fleet's service and inherit its session. The
consequence is that the service name is part of the contract: an application
that renames its service is not routed, and the gateway continues to answer.

**One credential, published once, inherited by every application repository.**
An application build needs write access to the tenant's GitOps repository. The
credential collected when the tenant's repository is created is published at the
organisation level, so every application repository the tenant owns inherits it.
Scaffolding is not told which application repositories exist: applications are
created, renamed and retired by the tenant long after scaffolding has run, and a
platform tracking that set would fall behind it silently.

**The organisation-wide form requires a paid plan.** On GitHub's free plan an
organisation secret is delivered only to PUBLIC repositories. The write
succeeds, the API reports the secret with visibility `all`, it appears in each
repository's list of available organisation secrets -- and every private
repository's workflow receives an empty string, with nothing at any layer
reporting that it was withheld. The build then fails on *"Invalid username or
token"*, which names the credential rather than the delivery, and sends the
operator to re-mint a token that was read correctly.

So on a free plan the credential is published PER REPOSITORY. That is strictly
weaker and the weakness is the one this decision was taken to avoid: a
repository created tomorrow is NOT covered, and must be given the credential
when it is created. The platform cannot close that gap from outside -- it is the
plan's boundary, not a design choice -- so it is stated rather than worked
around. `kakt tenant set-gitops-token --discover --gitops-repo <repo>` covers the set
that exists; `--repo owner/name` covers one.

The discovered set is DERIVED, never enumerated, for the same reason this
decision refuses to track application repositories: a repository needs the
credential exactly when one of its workflows reads `secrets.GITOPS_REPO_WRITE_TOKEN` and
names this box's GitOps repository, and that is readable from the workflows
themselves. Publishing to every repository in the organisation would be the
easier implementation and the wrong one -- most have nothing to do with this
box, and a credential that can write a tenant's GitOps repository should not sit
in a repository that never asked for it.

The CLI detects the plan and refuses the organisation form where it would be
silently ineffective, because a credential that looks present everywhere except
where it is read is worse than one that was never set.

Published by the Day-0 CLI at scaffold, and republishable afterwards
(`kakt tenant set-gitops-token`). The publication is deliberately non-fatal --
a box is complete without it and aborting a scaffold would destroy a working
repository over a credential no cluster depends on -- which means a scaffold
whose token lacked the organisation's "Secrets: write" permission leaves the
credential unset. Without a retry the only recovery was an operator pasting a
`gh secret set` line out of a warning printed days earlier, which is the tenant
performing a step this ADR assigns to the platform. The retry is a command for
that reason.

This is weaker than a credential held only inside the cluster, where an
application's build holds nothing and the platform's automation supplies the
write. That arrangement requires an in-cluster workflow engine and runners
inside the cluster network, neither of which the platform has. It is adopted as
an interim position, and the revisit trigger is the platform acquiring either.

### Alternatives considered

**A chart in the GitOps repository.** Rejected: it makes the GitOps repository
the system of record for how an application runs, which belongs with the code
that produces it, and it splits one change across two repositories that can
disagree.

**An Application sourcing the chart from the application repository by path.**
Rejected by ADR-073, which withdrew the fields that would express it. A path
identifies a location rather than content, so a deployment could not state what
it runs without fetching it, and rollback would have no expression.

**A per-repository credential for each application.** Rejected: it requires the
platform to track a set of repositories that changes without it, and the failure
when it falls behind is silent.

## Ownership

Reference ADR-039 (Platform Ownership Model).

| Resource Class | System of Record | Lifecycle Owner | Reconciler | Consumer | Phase |
|---|---|---|---|---|---|
| Application chart (templates, values) | Application repository | Application team | Application CI | Chart registry | Day-1+ |
| Published chart artefact | OCI registry in the tenant's organisation | Application CI | Application CI | ArgoCD | Day-1+ |
| Container image | OCI registry in the tenant's organisation | Application CI | Application CI | Workload cluster | Day-1+ |
| Deployed chart version | Tenant GitOps repository (`environments/<env>/<app>/Chart.yaml`) | Application CI | ArgoCD ApplicationSet | Workload cluster | Day-1+ |
| Fleet declaration (`environments/<env>/values.yaml`) | Tenant GitOps repository | Tenant | ArgoCD ApplicationSet | Platform charts | Day-1+ |
| Tenant namespace, gateway, OAuth clients, pull secret | Platform chart (universal-tenant) | Platform | ArgoCD ApplicationSet | Workload cluster | Day-1+ |
| Public hostname certificate + hostname route | Platform chart (tenant-public-tls) | Platform | ArgoCD ApplicationSet | Workload cluster | Day-1+ |
| Shared TLS gateway and its listener set | Platform chart (platform-spoke-gateway), from the generated per-spoke aggregate | Platform | ArgoCD ApplicationSet, one per spoke | Workload cluster | Day-1+ |

<!-- ADR-096: the TLS gateway was split out of tenant-public-tls on 2026-09-29.
     It had been rendered once per application, each copy declaring only its own
     listener, and because every ArgoCD Application applies as one field manager
     the copies overwrote each other and flapped public DNS. One object, one
     owner. -->
| GitOps write credential | Tenant's Git organisation | Tenant | Day-0 CLI | Application CI | Day-0 |
| Workload admission constraints | Workload cluster policy | Platform | Kyverno | Workload cluster | Day-1+ |

## Consequences

### Positive

What runs is legible from one line in the GitOps repository without fetching
anything, and rollback is expressible because a version identifies content.

Each application has its own file, so builds do not contend and each workload
keeps its own sync, health state and blast radius.

An application that renames its service, weakens its security context or
references a mutable tag is refused by the cluster rather than deployed and
later found wrong.

Adding an application requires nothing from the platform: no credential, no
registration, no scaffolding change.

### Negative

An application team must own a chart, which is work that did not exist when a
platform base was inherited by overlay.

The ADR-022 shape is now expressed in each application's chart rather than in
one place, so a change to the reference shape does not reach existing
applications. Admission bounds the divergence; it does not remove it.

The GitOps write credential is readable by every repository in the tenant's
organisation, including repositories the tenant adds for unrelated purposes.

An application cannot be deployed from a working tree: it must be published
first. This is intended — the released path is the only path — but it lengthens
the loop for a change being tested for the first time.

## Impact

Amends ADR-022: its base remains the reference expression of the stateless-web
shape and is no longer the only sanctioned way to obtain it. An application
chart carrying the shape is sanctioned, because admission rather than
inheritance is what makes the constraints binding.

Amends ADR-047: the fleet values file remains the fleet's declaration, and this
ADR states what accompanies it in the same repository and who writes it.

Completes ADR-073: that ADR withdrew the fields by which a fleet named a
location. This states what replaces them.

Does not alter ADR-051. Public hostnames, certificates and the routes carrying
them remain platform-owned and are rendered from the fleet's declaration.

## References
- ADR-022: Platform-Governed Workload Bases
- ADR-039: Platform Ownership Model
- ADR-040: Day-0 vs Day-1 Lifecycle Boundary
- ADR-041: Controller Responsibility Matrix
- ADR-043: Control Plane Authority Model
- ADR-047: Fleet Tenant Provisioning
- ADR-050: Tenant Gateway
- ADR-051: Public TLS and Hostname Ownership
- ADR-053: OAuth Client Provisioning
- ADR-065: The Control Plane Ships Into the Box
- ADR-073: A Fleet Chooses Versions, Not Locations
- ADR-082: Tenant Repository Layout and Cluster Naming
