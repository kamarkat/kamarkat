# ADR 021: Boundary-Driven GitOps and Day-0 Choreography

## Status
Accepted

## Context
Our initial GitOps deployment strategy relied on a monolithic "App-of-Apps" pattern, using ArgoCD `sync-wave` annotations (ranging from -2 to 11) to artificially sequence the deployment of infrastructure, databases, and services. 

This approach proved brittle:
1. **Global Deadlocks:** A transient failure in a high-level application could halt the synchronization of core infrastructure.
2. **Secret Zero Paradox:** The CLI attempted to inject fundamental trust roots (CA certificates, master keys) into a cluster that was simultaneously attempting to boot workloads that required those secrets, creating a race condition.
3. **Abuse of Sync-Waves:** Using sync-waves as a runtime orchestration engine violates the principles of Kubernetes Eventual Consistency.

## Decision
We will separate our deployment architecture into **Day-0 Imperative Choreography** and **Day-1+ Declarative Continuous Reconciliation**.

1. **Independent Boundaries:** We will split the monolithic App-of-Apps into mathematically independent ArgoCD applications:
   - `01-platform-infra`: Core controllers, Operators (Crossplane, CNPG, Atlas), and CRDs.
   - `02-platform-data`: Stateful workloads (CNPG Clusters, NATS, Redis).
   - `03-platform-services`: Secret Providers and Core Platform Services (Infisical, HubEnvironment, API Gateway).
   - `04-tenant-services`: Secret Consumers, Identity, and Billing Services (Ory, OpenMeter, Auth Proxy).
2. **Day-0 CLI Choreography:** The `hub` CLI will imperatively gate these boundaries during initial bootstrap:
   - Apply `01-platform-infra` and wait for webhooks.
   - Apply `02-platform-data` and wait for CNPG readiness.
   - Bootstap Secret Infrastructure and apply `03-platform-services`.
   - Bootstap Infisical API (establishing secret hierarchy).
   - Apply `04-tenant-services` (secret consumers).
   - The CLI will then exit permanently.
3. **Ban Application Sync-Waves:** We will purge `argocd.argoproj.io/sync-wave` annotations from all application workloads. Sync-waves are restricted exclusively to ordering CRDs before the operators that own them in the `01-platform-infra` boundary.

## Ownership

| Resource Class | System of Record | Lifecycle Owner | Reconciler | Consumer | Phase |
|---|---|---|---|---|---|
| Kubernetes Resources | Git | ArgoCD | ArgoCD | Platform, Tenants | Day-1+ |

See ADR-039 for the complete ownership matrix.

## Consequences
* **Positive:** Complete blast-radius isolation. An application failure will no longer block infrastructure reconciliation.
* **Positive:** Secret Zero injection is deterministic and race-condition-free.
* **Positive:** Clear architectural boundary between Secret Providers (`03`) and Secret Consumers (`04`), eliminating circular dependencies during bootstrap.
* **Negative:** Applications in `04-tenant-services` will attempt to start before their databases in `02-platform-data` are fully provisioned, requiring robust internal retry logic (See ADR-022).

## Addendum (2026-08-22): health coupling inside a boundary

This ADR states as a positive consequence that *"an application failure will no
longer block infrastructure reconciliation."* Between boundaries that holds. It does
not hold **within** one, and the difference produced an undeliverable sync.

`platform-database` carries both:

- infrastructure — the CNPG `Cluster`, its `Pooler`, and the `Database` CR;
- credentials — ExternalSecrets that resolve out of Infisical.

ArgoCD treats an Application's sync as one unit, so the operation reported
*"waiting for healthy state of ExternalSecret/control-plane-db-credentials and 4
more"* and applied nothing. Those ExternalSecrets cannot go healthy until Infisical
runs; Infisical could not run until the `infisical` role existed; and the resource
that creates that role was in the same, blocked sync. GitOps could not deliver its
own fix, and the manifests had to be applied out of band to break the cycle.

The failure is not sync-waves — this ADR already bans those for workloads — it is
**health coupling between resource classes inside a single Application**. Anything
whose readiness depends on a running platform service must not share an Application
with the resource that brings that service up.

Recorded rather than fixed: separating credential delivery from database
infrastructure changes boundary composition, which is this ADR's subject and
deserves a deliberate decision rather than an incidental split. Until then, a
bootstrap that needs a database-layer change while Infisical is down requires the
manifests to be applied directly.

**Closed 2026-11-03 by ADR-112.** One Application per platform application cluster
is the deliberate change in boundary composition this asked for, and the cycle is
gone by construction rather than by care:

- `platform-infisical-db` carries the Cluster that lets Infisical start and **no
  ExternalSecret**. Infisical's role and database come from CNPG — `managed.roles`
  and an `initdb.secret` the CLI writes at Day-0 — so nothing in that Application
  waits on Infisical running.
- The other three carry their own ExternalSecret beside their own Cluster, which is
  still health coupling. It is no longer a cycle: none of those Clusters brings up
  the service their ExternalSecrets resolve from, so the sync waits where waiting
  is correct and does not block its own precondition.

The rule stated above is what makes this checkable: anything whose readiness depends
on a running platform service must not share an Application with the resource that
brings that service up. Boundary 02 now satisfies it per Application rather than by
not containing the pair.


## Amendment (2026-10-29): what orders what — **Proposed**

**Status:** Proposed. It amends Decision 1 and Decision 3 of this ADR, and is the
governing document for the composition contract that ADR-029, ADR-061 and ADR-066
are amended to agree with.

### Why

rc.177 delivered the self-hosted observability backend of ADR-078 addendum 5 and
disagreed with four accepted ADRs at once. Nothing caught it, because none of the
disagreements is visible in a green sync. The architecture review that followed
established these facts about this repository:

1. **An Application-level sync wave orders nothing here.** ArgoCD orders resources
   within one synchronisation operation. An `Application` is ordered by a wave only
   when a parent Application synchronises it, and Decision 1 of this ADR removed the
   App-of-Apps that would have been that parent. Every `Application` in the platform
   is generated by an ApplicationSet controller, so the annotation this ADR's
   boundary 01 and 04 templates rendered had no effect. Boundary 03's template never
   rendered it at all, so the `syncWave` its descriptors declared was a dead field.
2. **The historical ladder never worked either.** The removed VictoriaMetrics stack
   declared waves 1, 2 and 3 in boundary 03 in October 2026, four months after the
   annotation was stripped from that template (`60ae1341`, 2026-06-13). The ordering
   those descriptors described was never delivered by anything.
3. **Boundary sequencing is boundary activation**, the AppProject deny sync windows
   opened in order by Day-0 (ADR-055, `scripts/lib/boundary-gating.sh`) — not waves.

### Decision

**1. Waves order resources inside one Application, and nowhere else.** Resource-level
`argocd.argoproj.io/sync-wave` is permitted for ordering the layers of ADR-029 §4
inside a single Application — a CRD before the operator that owns it, chiefly —
**in any Application that ships both**, and is not restricted to boundary 01: the
mechanism is a property of a synchronisation, not of a boundary. This replaces
Decision 3's clause *"Sync-waves are restricted exclusively to ordering CRDs before
the operators that own them in the `01-platform-infra` boundary"*. The spoke
catalogue's vendored CRD sets at wave `-5` are this case and are conformant.

**2. No Application carries a sync wave.** The annotation is prohibited on generated
Applications, and `syncWave` is removed from every descriptor and boundary template.
A declaration that orders nothing is worse than no declaration: it is read as a
guarantee. Intended dependencies are recorded in prose where they matter.

**3. There is no ordering between independently reconciled Applications, and none is
invented.** No App-of-Apps, no progressive syncs, no dependency orchestrator. An
Application whose CRDs are owned by another Application fails while they are absent
and recovers by retry: boundaries 01, 03 and 04 now carry `retry: {limit: -1,
backoff: {duration: 5s, factor: 2, maxDuration: 3m}}`. Unlimited attempts under a
capped backoff converge without a Git revision change or a manual sync; a permanent
failure stays visible on the Application, because a retry is not health.
`SkipDryRunOnMissingResource` remains where a CR is declared ahead of its CRD, and is
not a readiness or ordering mechanism — it skips a dry-run, nothing more.

**4. Operators are classified on seven dimensions, not on whether they install CRDs.**
Decision 1 lists "Core controllers, Operators (Crossplane, CNPG, Atlas), and CRDs" in
boundary 01. That is the set Day-0 choreography blocks on, which is the test —
bootstrap dependency, not CRD count. A capability-specific operator that no bootstrap
step waits for belongs in its capability's boundary; the VictoriaMetrics and Grafana
operators are boundary 03. Installation authority and Day-1 ownership are separate
dimensions: cert-manager is installed once at pivot from a vendored asset while a
boundary-03 Application owns its hub-specific patches, and a classification that
conflates the two cannot describe it.

**5. Resource classes separate when their readiness, ownership or upgrade cadence
differ — not by universal rule.** ArgoCD supports a CRD and its custom resources in
one synchronisation, so no rule forbids it. What this ADR's 2026-08-22 addendum
forbids is health coupling: a resource that cannot become healthy until a service in
the same Application is running blocks the sync that would have started it. Where
intra-Application waves resolve that ordering, one Application is correct. Where the
lifecycles genuinely differ — the Grafana operator's upgrade cadence against the
dashboards it reconciles — they are separate Applications, and the dependency is
carried by retry per Decision 3.

**6. An ownership transfer preserves identity.** Moving a resource between
Applications is governed by: no CRD changes Application owner; every moved object
keeps its `metadata.uid`; no window exists in which a prune-enabled Application
still tracks an object that has left its desired state; and the procedure is
rehearsed on an isolated cluster before a live one.
`preserveResourcesOnDeletion` governs an ApplicationSet deleting Applications, and
`Prune=false` governs one resource — neither makes a transfer safe by itself.

### Consequences

- Cold convergence is retried rather than ordered, and reports `ComparisonError`
  until the CRDs it needs exist. That is the accepted cost of having no
  cross-Application ordering mechanism, and it is visible rather than hidden.
- A capability's blast radius is its own Applications, unchanged.
- Dead declarations cannot return silently: `scripts/validate/preflight/021-boundary-composition.sh`
  asserts the invariants above.

### Codified

- `manifests/argocd/environment-manager/templates/{01,04}-*-appset.yaml` — the
  Application sync-wave annotation and the inline `syncWave` literals removed.
- `manifests/argocd/components/{01,03}/*.yaml` — 14 descriptors lose a dead field.
- `manifests/argocd/environment-manager/templates/{01,03,04}-*-appset.yaml` — the
  retry policy of Decision 3.
- `manifests/hub-core-services/{victoriametrics-operator,platform-dashboards}` —
  vendored CRDs at wave `-5`, operators at `1`.
- `manifests/hub-core-services/grafana` — the Grafana instance, datasources, folder,
  route and dashboards as their own Application; `platform-dashboards` keeps the
  operator and its CRDs as the ownership anchor.
- `manifests/argocd/components/03/grafana-alloy.yaml` — required machinery, ungated.
- `manifests/provenance/` — external digests (ADR-029 §1).
- `scripts/validate/preflight/021-boundary-composition.sh` — five invariants.
- `docs/Administer/Runbooks/grafana-application-ownership-transfer.md` — the transfer
  of Decision 6, with its evidence requirements.
