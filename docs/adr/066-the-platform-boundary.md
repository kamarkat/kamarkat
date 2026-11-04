# ADR-066: The Platform Boundary

**Date:** 2026-09-07
**Status:** Proposed

*Amended by: ADR-075 (On-Prem Nodes are a Capability, Not a Provider)*

*Amended by: ADR-098 (Metering Sources over Spoke Egress and Records in Lago) —
add.3's replacement sentence for `metering` ("the PostgreSQL implementation
already written at `internal/kube-sbt/providers/metering`") is replaced by
the ADR-098 design: Lago as the billing engine, sole usage record, with
the agent's write-ahead log shipped through a Vector sidecar into platform
object storage and consumed by the hub behind a durable checkpoint. The
capability stays `declared` / `planned` until that design ships; "what is
outstanding is an ADR for it" is answered.*

> On-prem nodes are named as a selectable capability. The boundary this ADR draws
> between the machinery every box runs and the capabilities a tenant selects is
> where that decision is made.

## Context

ADR-065 places a platform inside every tenant's box and makes maintaining it the service the platform sells. It does not say what "it" contains, and the platform currently draws no line: `manifests/hub-core-services/` holds thirty-one components reconciled uniformly, spanning cluster machinery, databases, an identity provider, a message bus, an analytics store, a mail server and a metering service.

Four facts bear on the boundary.

**The tenant has no platform engineers.** ADR-065's arrangement exists for organisations that want a golden path and cannot staff one. A boundary that hands back a component such an organisation cannot operate does not reduce the platform's obligation; it relocates the obligation to the party least able to meet it, and the component fails there.

**Two questions are being asked at once.** Whether every box runs a component, and who maintains it when a box does, are independent. A database is not run by every tenant and is unoperatable by any of them, so the answers differ; treating them as one question forces a component to be either universal or unsupported.

**A tenant's application is genuinely not the platform's.** Its code, its images, its schema and its data are authored, deployed and understood by the tenant. Nothing the platform knows would let it maintain them, and ADR-062 already places them in repositories the platform holds no access to.

**On-premises capacity is part of the topology.** ADR-046 provisions home workers on an immutable operating system and ADR-052 bursts to cloud capacity above them. Maintenance effort on those nodes scales with how much state they are asked to hold.

## Decision

**The platform is the set of capabilities it provides and maintains, on which a tenant's applications execute. The tenant owns the application logic and the use it makes of those capabilities.**

The definition is deliberately not "everything a tenant's application runs on". Taken literally that reaches the cloud provider, the region, the network, the firmware and the hardware, and it would make the platform answerable for things it neither ships nor can reach. What the platform maintains is what it provides.

### The test is what authored it, not what it does

A component belongs to the platform if the tenant's own logic is not inside it. A database engine, an identity provider, a message bus, a gateway, a certificate authority and a metrics store are all platform: their behaviour is determined by the platform that ships them and the configuration it defines. The schemas in that database, the organisations in that identity provider, the routes through that gateway and the images behind them are the tenant's.

The line falls between a capability and the use made of it. The platform maintains the capability; the tenant owns the use.

**Which side a component sits on has nothing to do with who wrote it.** A third-party database engine the platform ships and maintains is platform; an application the platform's own team once wrote for a tenant would not be.

### Required and selectable are a different question from platform and tenant

Every box runs the same cluster machinery: the GitOps engine, composition and cluster lifecycle, secret delivery, certificate issuance, DNS, gateway, admission policy, and node and capacity lifecycle. Without these a box cannot reconcile anything, so they are not selectable.

Above them sit capabilities a tenant selects: a database, an identity provider, object storage, mail, metrics. A tenant runs what it needs and does not carry the rest.

**Selecting a capability does not make it the tenant's to maintain.** Both sets are platform, and the platform maintains what a box actually runs. A tenant that selects a database receives a maintained database, which is the whole proposition: the alternative asks an organisation with no platform engineers to operate a Postgres cluster, and the component fails in their hands.

The two questions therefore have separate answers. Required against selectable decides what a box costs. Platform against tenant decides who is answerable when it breaks.

### The platform owns capability health; the tenant owns capability usage

Maintenance means the capability is available, current, recoverable and correct as shipped: the operator running, the version supported, backups taken and restorable, failover working, storage able to grow, the component patched. That is the platform's, on every box that runs it.

What a tenant does with the capability is the tenant's: a query that will not terminate, a schema that will not migrate, an OAuth client pointed at the wrong callback, a topic nobody consumes. The platform does not tune them, and an incident caused by one is not a platform incident.

The distinction is what makes the obligation answerable at three in the morning. A Postgres operator that has stopped reconciling is the platform's. A slow query against a healthy Postgres is the tenant's. A Zitadel that will not start is the platform's; a misconfigured client registered in a healthy Zitadel is the tenant's.

It is also the boundary most likely to be argued, because a tenant's usage can make a healthy capability behave as an unhealthy one. Where the capability is functioning as shipped, the platform's obligation is to say so and to help — not to own the outcome.

### A capability is offered once and consumed many times

A selected capability is provisioned once per box and used as often as the tenant needs. A tenant with a database creates the databases its applications require; a tenant with an identity provider registers the applications it runs. The platform maintains the capability and does not count or approve the uses.

This is what lets the box adapt without the platform being consulted, and it is the difference between a golden path and a menu.

### The bundle's capability declaration is how a capability is added

Capabilities are declared in the bundle itself, and a tenant selects from that declaration in the values its cluster supplies (ADR-063). The declaration travels inside the artefact it describes; there is no second body of curated content beside the platform, which is what ADR-071 refuses. Selection is a value and never a version, so adopting a capability and upgrading a bundle are independent acts and neither blocks the other.

A capability enters the declaration by being packaged and versioned with the bundle, and certified against it: tested with the machinery at that bundle version, not against every other capability. The machinery is one tested substrate; each capability is certified against that substrate and declares what it requires of it.

Testing every combination is not possible and is not what a bundle version claims. Ten independently selectable capabilities are a thousand combinations, and a claim to have exercised them all would be false in a way nobody could check. What is claimed is narrower and true: this substrate was tested, and each capability was tested on it.

Where two capabilities genuinely interact -- a database and the metering that reads it -- the interaction is itself declared and certified, so a pair that must be tested together is named rather than assumed.

### A capability has a lifecycle, and leaving the declaration is part of it

A capability moves through declared states: **declared** and installable, **supported** and maintained on every box running it, **deprecated** and still maintained but closed to new selection, **end-of-life** with maintenance ending on a stated date, and **removed** from the declaration.

Every state but the last carries maintenance. Deprecation is an announcement with a date, not a withdrawal, and a tenant running a deprecated capability keeps being maintained until that date passes.

Without this, "nothing reaches a tenant's box that the platform is not prepared to maintain there" is a promise with no end, and every capability ever declared would have to be maintained forever. The economics of this platform are decided by what enters the declaration and what is allowed to leave it, so both are decisions rather than events.

What the platform owes a tenant whose capability reaches end-of-life -- a migration path, a supported successor, or notice alone -- is a support-contract question this ADR does not settle.

### On-premises capacity carries workloads, not cluster state

Cluster-machinery state is not placed on on-premises nodes. Those nodes carry capabilities and burst capacity, so a node can be replaced without the ceremony reconciling cluster state would require, which is the property ADR-046's immutable operating system already assumes.

### Alternatives considered

**Draw the line at the control plane: cluster machinery is maintained, everything above it is the tenant's.** Rejected, and it was this ADR's first form. It produces a defensible boundary for a platform sold to platform engineers and the wrong one here: ADR-065's tenant has none, so a database or an identity provider handed back is a component nobody maintains. It also contradicts what the platform is sold as — a box a startup runs its business on, not a cluster it must then furnish.

**Maintain the tenant's applications too.** Rejected. Their logic, images and data are the tenant's, the platform holds no access to the repositories carrying them (ADR-062), and nothing the platform knows would let it maintain them.

**Let each tenant draw its own boundary.** Rejected. A support obligation that varies per tenant cannot be stated once, and the uniformity of the platform is what makes a single tested bundle meaningful under ADR-063.

## Capabilities

The declaration this ADR requires, checked by
`scripts/validate/architecture-consistency.py`. This ADR stays **Proposed** while
any of them is `planned` — and six of the seven selectable ones are, because only
`onPrem.enabled` is a real toggle today.

```architecture
capabilities:
  - gitops-engine
  - cluster-lifecycle
  - secret-delivery
  - certificate-issuance
  - dns
  - gateway
  - admission-policy
  - capacity-lifecycle
  - on-prem-capacity
  - database
  - identity
  - metering
```

## Ownership

| Resource Class | System of Record | Lifecycle Owner | Reconciler | Consumer | Phase |
|---|---|---|---|---|---|
| Cluster machinery | `zero-ops`, published as charts | Platform | ArgoCD | Tenant control plane | Day-1+ |
| Selectable capabilities | `zero-ops`, published as charts | Platform | ArgoCD | Tenant control plane | Day-1+ |
| Capability selection | `<tenant>-gitops` values | Tenant | ArgoCD | Bundle chart | Day-1+ |
| Use made of a capability | tenant | Tenant | — | Tenant | Day-1+ |
| Application logic and images | tenant workload repositories | Tenant | ArgoCD | Tenant | Day-1+ |

See ADR-039 for the complete ownership matrix.

## Consequences

### Positive

A tenant with no platform engineers receives a database, an identity provider and a gateway that someone maintains, which is what ADR-065's arrangement is for.

What maintenance covers is derivable from the boundary rather than negotiated per component and per tenant.

A tenant pays for the capabilities it runs and not for the ones it does not, while still being maintained on everything it does run.

A capability is provisioned once and used freely, so a box adapts to what a tenant builds without the platform being asked.

On-premises nodes hold no cluster-machinery state, so replacing one is a capacity operation.

### Negative

The platform is answerable for a large surface: a database engine, an identity provider, a message bus and a mail server are each an operational speciality, and maintaining them across every tenant's box is the cost of the proposition rather than an incidental one.

The floor price is not bounded by cluster machinery. A box costs its machinery plus every capability the tenant selected, and the selectable half is where the weight sits — a Postgres cluster and an identity provider are not free. Whether that floor is low enough for the organisations ADR-065 describes is an open question this ADR does not answer, and it is the one most likely to invalidate the arrangement.

The boundary runs through some components rather than between them. An identity provider serves both the platform's own access and the tenant's end users; a metrics store holds both. Each such component needs its two sides named, and the second side is the tenant's data with the retention and disclosure obligations that follow.

Every capability added to the declaration is a permanent maintenance obligation across every box that selects it. Adding one is a decision about the platform's cost base, not a feature.

## Impact

- **Amends ADR-065.** What ships into a box is the cluster machinery plus the capabilities a tenant selects, and the platform maintains both.
- **Amends ADR-063.** A bundle is the tested set of the machinery and every declared capability; a box runs the machinery and its selections.
- **Confirms ADR-062.** Application logic stays in tenant-owned repositories the platform holds no access to, which is the same line drawn from the other side.
- **Confirms ADR-046 and ADR-052.** On-premises nodes carry capabilities and burst capacity.
- **Constrains ADR-013.** Observability spans the boundary, so its components are classified explicitly rather than by location.

## Addendum 1: "catalogue" was the wrong word, and the declaration is a file (2026-09-12)

This ADR said *"the catalogue is how a capability is added"*. ADR-071 says
*"This platform publishes no catalog"*, rejecting kubefirst's separately curated
`gitops-catalog` repository. Read together the two ADRs appeared to contradict
each other, and neither could be implemented while they did.

They do not disagree about anything substantive. What ADR-071 refuses is a
**second body of maintained content beside the platform** — content the platform
maintains but only some tenants run, curated and versioned apart from the bundle.
What this ADR requires is that the bundle **say what it carries**. The word
"catalogue" was doing both jobs, and it is withdrawn: the thing is the bundle's
**capability declaration**, it travels inside the artefact it describes, and
nothing is curated beside the platform.

The declaration is now a file: `manifests/architecture/components.yaml`, under
`capabilities:`. It records for each capability whether it is machinery or
selectable, the values path that selects it, its lifecycle state and end-of-life
date, the components that realise it, and the pairs certified together.
`scripts/validate/architecture-consistency.py` checks it, and this ADR cannot
reach Accepted while anything it declares is still `planned` — which is what
stops the declaration being a list of intentions.

## Addendum 2: selection is wired for four of seven (2026-09-12)

*"A tenant runs what it needs and does not carry the rest"* was true of nothing.
Every capability was unconditionally on, so selection existed in this ADR and in
no cluster. Four now select:

| capability | value | |
|---|---|---|
| on-prem capacity | `onPrem.enabled` | was already wired |
| database | `capabilities.database.enabled` | new |
| observability | `capabilities.observability.enabled` | new |

**Every default is `true`, and that is load-bearing rather than tidy.** These
capabilities run on every existing box, so a default of `false` would remove them
on the first sync of the version that introduced the flag. A promotion that
silently deletes a tenant's database is the worst thing this mechanism could do,
and ADR-064 makes promotions routine. A tenant turns one off deliberately and
never by accepting an upgrade.

Wiring observability surfaced a constraint worth recording. A capability gate
cannot live on a component **descriptor**: the released path globs descriptors
out of the chart and the development path reads them from git, and only the
first can filter — so the gate would take effect in a released box and not in a
development one. ADR-068 is explicit that those two paths must not diverge, and
two defects have already reached clusters through exactly that gap. So a
capability whose presence is selectable is declared inline in its boundary,
which is the same test ADR-061 already applies to a component whose *source* is
parameterised. `grafana-alloy` moved accordingly, and into `inline-charts.sh`
with it — leaving that out would have published a bundle naming a chart no
release produced.

Three remain unselectable, each for a stated reason rather than for want of
effort:

- **identity** is entangled with the OIDC wiring every other component
  authenticates against; a flag that switched it off would leave the gateway,
  the auth proxy and ArgoCD's GitHub auth resolving nothing.
- **object storage** is selected by supplying a destination, not by deploying
  anything, so there is no Application to gate. Its selection mechanism is the
  credential tiering, which is not yet a cluster value. (`platform-storage` is
  not this — that is the on-prem local-path provisioner.)
- **metering** is applied by no component at all today.

This ADR stays **Proposed** until all three select, which is what
`scripts/validate/architecture-consistency.py` enforces.

## Addendum 3: two reclassifications, and what is left selectable (2026-09-12)

Addendum 2 left three capabilities unselectable and read as three pieces of
missing work. Two were not: they were misclassifications, and building toggles
for them would have produced states nobody wants.

**`identity` is machinery.** This ADR's own test for machinery is that a box
cannot function without it. The gateway is machinery and resolves an OIDC issuer
from identity; the auth proxy, ArgoCD's login and every relying party follow the
same value. A box with identity switched off has a gateway that authenticates
nobody — a supported-looking state with nothing behind it. Reclassified rather
than given a toggle.

**`object storage` is not a capability in this sense at all.** It is a
*destination*: selected by supplying an S3 endpoint and credentials, with no
Application to gate and nothing deployed either way. It sat in a table of things
that have Applications, which is why its `enable` field read as a fiction. Its
selection mechanism is the credential tiering in
`internal/kakt-cli/tenant/platform_credentials.go`, which already distinguishes
a capability the platform refuses to run without from a destination it reports
the absence of. Removed from the declaration; it is not lost, it is recorded
where it belongs.

**`metering` stays declared and `planned`**, because it is neither of the above
— it is a decision not yet made. OpenMeter has been withdrawn (it wanted
ClickHouse, Kafka and Svix for a capability no box ran), and the replacement is
the PostgreSQL implementation already written at
`internal/kube-sbt/providers/metering`, on the CNPG cluster every box runs. What
is outstanding is an ADR for it, not a toggle.

**What is selectable now:** `on-prem-capacity`, `database`, `observability`,
`support`, `metering`. The first three work today. `support` defaults false until
its image and Support Plane exist; `metering` awaits its decision. `messaging`
was selectable and has been withdrawn — see ADR-080, which removes NATS. Both are recorded in the declaration with their reasons rather than
left as gaps someone has to rediscover.

## Addendum 4: telemetry collection is required machinery (2026-09-19)

ADR-078 addendum 1 needed a place to put Grafana Alloy, `kube-state-metrics` and
`node-exporter`, and the answer turned out to need no new concept — only this ADR's
existing distinction, applied.

**Collection is required machinery. The observability backend is the selectable
capability.**

| | |
|---|---|
| required — not selectable | Grafana Alloy, `kube-state-metrics`, `node-exporter` |
| selectable — `capabilities.observability.enabled` | `VMSingle`, `VictoriaLogs`, `VMAlert`, Grafana |

The required-machinery sentence above — *"the GitOps engine, composition and cluster
lifecycle, secret delivery, certificate issuance, DNS, gateway, admission policy,
and node and capacity lifecycle"* — gains telemetry collection.

Two reasons, and the second is the one that forces it.

**A tenant may choose where their telemetry goes, including nowhere.** ADR-077's
Context defends exactly this — *"ships wherever the tenant chooses — Grafana Cloud,
a self-hosted backend, or nowhere"* — and ADR-078 §7 says additional destinations
are *"additive, never instead"*. If collection went with the capability, disabling
the in-box backend would also destroy the tenant's own external forwarding, which
turns an additive extension point into a mutually exclusive one.

**The Support Agent depends on `kube-state-metrics` for node capacity**, which
ADR-067 names as the basis for an upgrade pre-flight verdict. If the exporters were
part of a selectable capability, a tenant disabling observability would degrade
their own support — which is the licence check ADR-077 exists to prevent, arriving
through a configuration setting instead of a subscription check. ADR-078 §8 forbids
the observability capability from being a dependency of support, and this
classification is what makes that true rather than asserted.

**A tenant loses nothing they could previously turn off.** Alloy has never been
selectable; it has been machinery on every box since ADR-013. This records what was
already the case, before ADR-078's toggle could have quietly changed it.


## References

- ADR-013: Hub-Spoke Observability Architecture with Dual Collection Patterns
- ADR-039: Platform Ownership Model
- ADR-046: Hybrid Provider Home Worker
- ADR-052: Sandbox Compute and S3-Backed Workspaces
- ADR-062: Onboarding and Scaffolding a Tenant
- ADR-063: The Platform Bundle and its Version
- ADR-065: The Control Plane Ships Into the Box

## Addendum 5: required machinery is declared outside the gate, and what selection means today (2026-10-29)

Addendum 4 classified Grafana Alloy, `kube-state-metrics` and `node-exporter` as
required machinery and the observability backend as the selectable capability. The
declaration did not follow: Alloy had moved inline under
`capabilities.observability.enabled` a week earlier, by addendum 2's rule, and stayed
there. A box that deselected the backend would therefore have lost collection too —
the opposite of what addendum 4 decided, and it would have taken the agent that
addendum 4 defends with *"a tenant may choose where their telemetry goes, including
nowhere"*.

**A capability gate expresses selectable presence. Required machinery has none, so it
is never declared under one.** Alloy is a descriptor again
(`manifests/argocd/components/03/grafana-alloy.yaml`), `kube-state-metrics` already
was, and `node-exporter` ships inside the Alloy DaemonSet. Addendum 2's rule is
untouched for what it governs: the backend — stores, query gateway, evaluators, Loki,
the Grafana operator and the Grafana instance — stays inline under the gate.

A declaration route is also a packaging route: inline components are packaged by
`scripts/package/inline-charts.sh` and descriptors by `scripts/package/component-chart.sh`,
so Alloy moved between them in the same change. A component in neither publishes a
bundle naming a chart no release produced, which is the defect addendum 2 recorded
when Alloy moved the other way.

### What selection means in this release

`capabilities.observability.enabled` is an **initial-provisioning selection**. A box
provisioned with it false does not get the backend. On a box that already runs the
backend, setting it false stops the Applications being generated and **does not stop
the backend**: the ApplicationSets declare `applicationsSync: create-update`, which
keeps Applications the generator no longer emits, and that policy is deliberate —
*"deletion must never be the mechanism that removes platform state"*.

So **runtime disablement is not a supported operation and is not documented as one**.
Nothing here claims a tenant can turn a running backend off by flipping this value.
Permanent decommissioning remains a separate, explicitly approved act, and every
capability default stays `true` for the reason addendum 2 gives: a promotion must not
delete a tenant's data.

One consequence must be verified rather than assumed: the effective ApplicationSet
controller policy. A controller-level `--policy` can override the per-ApplicationSet
field, and this repository sets no `applicationsetcontroller.*` parameter, so the
retention this addendum relies on is only as true as the deployed controller's flags.
It is checked on the live cluster, not inferred from the manifests.
