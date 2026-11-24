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

## Amendment (2026-11-18): the identity provider's retry is bounded, deliberately — **Accepted** (records behaviour shipped since `12d62a37`, 2026-06-12)

The 2026-10-29 amendment's Decision 3 records the retry every boundary carries:

```yaml
retry:
  limit: -1
  backoff: {duration: 5s, factor: 2, maxDuration: 3m}
```

Boundary 04's multi-source ApplicationSet, which generates the `zitadel` Application,
carries a different one:

```yaml
retry:
  limit: 15
  backoff: {duration: 30s, maxDuration: 10m}
```

These are not the same contract, and the difference is kept rather than reconciled.

**Decision: the bounded policy stands for this boundary, and is recorded here as a
scoped deviation from Decision 3.** Unlimited attempts are right where a failure is a
missing CRD that another Application will supply — the case Decision 3 argues from. They
are not right here: the identity provider's initialisation Jobs write to a database, a
failed attempt can leave state a later attempt must reason about (ADR-079), and an
attempt costs minutes rather than seconds.

What the deviation must preserve, and does:

* ArgoCD retries a failed synchronisation automatically, without a person.
* When the attempts are exhausted the failure stays visible: the Application reads
  Degraded or SyncFailed, and nothing reports success over it.
* Day-0 may request a further synchronisation when its own live-state checks justify one
  (`prepareZitadelForRetry`), which is a decision made from the cluster's state rather
  than from a retry counter.
* A required identity workload that never serves fails the bootstrap. Boundary 04's
  readiness gate is `awaitZitadelReady`; exhausted retries do not become a green run.

No retry count or backoff value is changed by this amendment. The numbers above are the
contract for boundary 04's multi-source set; Decision 3's remain the contract elsewhere.

## Addendum: a promotion converges without being driven (2026-11-24) — Accepted

**Status: Accepted (2026-11-24).** Approved by the architecture review of
2026-11-24, which corrected the first RCA on two points recorded below.

A promotion must converge on its own. Git declares the desired state, ArgoCD
reconciles it, and the CLI observes the result. The CLI is not a second
reconciliation engine, and forced synchronisation is administrative recovery, not
a release step.

### 1. What was reported, what is verified, and what is derived

Kept apart deliberately: the first RCA presented all three as one, and two of its
load-bearing claims were wrong.

**Reported** (release engineer, 0.3.45 promotions): `argocd-hard-sync.sh` was run
twice on `platform-spoke-catalog-nutgraf-stg-01` and waited on; a sync operation
would not terminate; `argocd app terminate-op` failed; `status.operationState` was
patched and then removed by hand. The cause was given as `grafana-alloy-cluster`
crashing "due to log bucket allocation failure on Hetzner S3".

**Verified from source, at the pinned versions:**

- The ApplicationSets already declare `automated: {prune: true, selfHeal: true}`
  with `ServerSideApply=true` and a retry policy. **GitOps was configured
  correctly; no policy was missing.**
- The spoke catalogue carried sync waves `-5` (87 CRDs), `0` (default, every
  workload) and `5` (Kyverno `ValidatingPolicy` objects). ArgoCD completes a wave
  only when its resources are Healthy.
- `platform-argocd.yaml` carries six health customizations -- ExternalSecret,
  Gateway, CNPG Cluster, Crossplane Provider and Function, webhook configuration --
  and **none for Deployment or StatefulSet**, so those use ArgoCD's built-in
  assessment, which reports `Progressing` and never `Degraded` while pods are not
  Ready.
- Chart pin `10.11.7` packages ArgoCD `v3.5.7` (`Chart.yaml`: `appVersion:
  v3.5.7`). That version's params contract declares
  `controller.sync.timeout.seconds`, **default `"0"`, meaning no timeout**
  (`docs/operator-manual/argocd-cmd-params-cm.yaml`). It was not configured here.
- `argocd_app_info` at v3.5.7 declares `namespace, name, project,
  autosync_enabled, repo, dest_server, dest_namespace, sync_status, health_status,
  operation` (`controller/metrics/metrics.go`, `descAppInfo`). **There is no
  `operation_phase` label**; `phase` belongs to `argocd_app_sync_total`.
- Alloy writes logs to `http://loki.platform-observability.svc:3100/loki/api/v1/push`
  and references **no bucket**. The S3 bucket belongs to Loki (ADR-078 add.5 §4).

**Derived** -- the mechanism, which holds independently of which resource triggers
it:

> A wave placed after the workloads makes every workload's health a precondition
> for the sync operation finishing. A workload that cannot become Ready reports
> Progressing indefinitely. With no operation bound, the operation stays `Running`
> for ever. ArgoCD will not begin a new operation while one is in flight, so a
> promotion that moves `targetRevision` is seen and cannot be acted on: the
> Application reports OutOfSync at the new version and converges never.

**Corrections to the first RCA, both material:**

1. It claimed ArgoCD has no sync-operation timeout. **Wrong.** The capability
   exists from v2.14 and is disabled by default; the defect was that this platform
   had never configured it.
2. It proposed custom Deployment and StatefulSet health checks treating
   `observedGeneration == generation && replicas > 0 && readyReplicas == 0` as
   terminal failure. **Rejected, correctly.** A healthy workload satisfies that
   during scheduling, image pull or volume attachment. Kubernetes already
   expresses stalled rollout through `ProgressDeadlineExceeded`; native health
   semantics are preserved.

**Unestablished, and not asserted:** why `grafana-alloy-cluster` was not Ready.
Alloy has no bucket dependency, so the reported chain does not hold as given; an
unavailable Loki makes Alloy *drop* entries (`loki_write_dropped_entries_total`,
and add.5 §5's `LogsDroppedUnsent`), not crash. Also unestablished is why
`terminate-op` failed -- the error output was not captured. Neither matters to the
mechanism, and neither licenses hand-editing `status.operationState`, which this
addendum forbids as routine practice.

### 2. The detection was broken, which is why nobody was told

`ArgoCDSyncStuck` selected `argocd_app_info{operation_phase="Running"}`. That label
does not exist, a selector on a non-existent label matches no series, and the
expression was therefore always empty. **The alert could not fire.** A sync stuck
far beyond its thirty-minute threshold paged no one, and the state was found only
because a person went looking -- which is how forcing became routine.

It now selects `operation="sync"`, which the controller sets while a sync is
pending or in flight and clears when it completes.

### 3. What changed

- **`controller.sync.timeout.seconds: "900"`**, on the existing chart descriptor.
  At the bound the operation fails and `syncPolicy.retry` takes over -- unlimited
  attempts under a 3m backoff cap for platform services. A new revision is not
  synced while an operation runs, but once the bound expires the controller sees
  OutOfSync and syncs the current revision, so a promotion landing on a stuck sync
  converges by itself.
- **The wave-5 annotations are gone** from the spoke catalogue's Kyverno policies.
  Nothing is lost: Kyverno generates its `ValidatingWebhookConfiguration` at
  runtime, none is shipped, so a fail-closed policy denies nothing until Kyverno is
  serving -- and admission is fail-closed from that moment whatever order the
  objects arrived in. It is also stricter than before, since governance now lands
  with the workloads instead of after them.
- **The release path observes rather than forces.** `kakt promote sync` and the
  release skill's step 5 ran `argocd-hard-sync.sh --promote`; they now use
  `--wait-promote`, the same traversal with no refresh and no submitted operation,
  on one code path so the two cannot drift. The forcing modes remain, marked
  break-glass, and reaching for one is a signal to read the Application.
- **`ArgoCDSyncStuck`** fires on the real label at 20m, against the 900s bound. Its
  meaning changed with it: not "a slow sync", which the bound now ends, but "an
  operation outlived its bound" -- a wedged controller, or a timeout not in effect.
  ArgoCD monitoring stays on the hub alone (ADR-078 add.10 §6), where ArgoCD runs.

### 4. What is not in scope

The Hetzner capacity fault is an infrastructure problem and is unchanged. These
corrections stop it from blocking unrelated Applications; they do not fix it. No
custom health framework, promotion lock, release gate, status patching or new
controller is introduced.
