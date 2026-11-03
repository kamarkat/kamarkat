# ADR-098: Metering Sources over Spoke Egress and Records in Lago

**Date:** 2026-10-01
**Status:** Proposed

**Relates to:** ADR-012 (declarative billing catalog), ADR-014
(platform-owned stateful infrastructure — the object-store, credential and
backup contracts this design reuses), ADR-021 (tenant ABI), ADR-039
(platform ownership model), ADR-041 (controller responsibility matrix),
ADR-043 (control-plane authority), ADR-046 (hybrid provider placement),
ADR-047 (fleet tenant deployment contract), ADR-052 (sandbox compute and S3
workspaces), ADR-066 (the platform boundary), ADR-070 (the minimum
supported box), ADR-078 (the observability capability), ADR-080 (NATS is
removed), ADR-083 (cross-cluster observability federation), ADR-088 (the
tenant and the application are different axes)

**Amends:** ADR-012, ADR-039, ADR-043, ADR-052, ADR-066, ADR-078 — status
lines added with this ADR.

**Amended:** 2026-10-27 — see "Amendment 2026-10-27: records in the
usage-analytics database".

## Amendment 2026-10-27: records in the usage-analytics database

Lago is not part of the architecture. Credits are the platform's Credit
Authority (ADR-109); monetary billing is the coordinating product layer's,
through external providers. Infrastructure usage therefore has no billing
engine to be recorded in.

*Amended by ADR-113 (2026-11-03):* Lago is now the platform's selectable
billing capability, recording plans, subscriptions and invoices for the one
application that declares it. Infrastructure usage is still recorded in the
usage-analytics database below, never in the billing engine.

**The usage record is the usage-analytics database.**
- Infrastructure usage and internal execution costs are stored in a
  usage-analytics database operated by the platform, provisioned by the
  `metering` capability (ADR-099). It is separate from the Credit Authority
  and from every billing provider.
- The record semantics of Decision 3 carry over unchanged — deterministic
  record identity, resource UID, tenant identity, interval, quantity and
  unit, source, the four timestamps, backfill flag, gap markers — as fields
  of the record instead of Lago properties. A record is unique by its
  identity, so a replayed object inserts nothing new.
- `metering-ingest` writes records to this database. Decision 6's ordering
  becomes: insert the object's records idempotently, then commit the
  checkpoint. Its lease, fencing, outage envelope and poison isolation are
  unchanged.
- Internal execution-cost records (tokens, models, tools, compute per run)
  are accepted from a tenant's coordinating product service, deduplicated by
  their identity, and joined to infrastructure usage by pod identity.
- The agent, its write-ahead log, the transport, coverage and lifecycle
  recovery (Decisions 1, 4, 5, 7–11) are unchanged.

**The usage record is not billable usage.** No invoice, charge or price is
computed from it, and nothing reads it at billing time. Where a pricing
model bills for usage, the coordinating product layer decides what is
billable and sends it to its billing provider, identified by its billable
fact. The commercial interpretation of usage is the product layer's.

**Withdrawn:**
- Decision 2 — Lago as the billing engine, its light profile, and box-local
  invoicing.
- From Decision 3 — Lago's events as the record, and the financial chain
  from events to invoices.
- From Decision 12 — the Lago backend of the catalog: plans, customers,
  subscription intent and the `metering-boundaries` effective-date contract.
  Meters remain versioned, fail-closed definitions in Git; a semantic change
  is a new meter version.
- From Decision 13 — every prerequisite about Lago's deployment, schema,
  keys and invoices.

**Ownership, replacing the Lago rows:**

| Resource Class | System of Record | Lifecycle Owner | Reconciler | Consumer | Phase |
|---|---|---|---|---|---|
| Usage record (infrastructure usage, internal execution cost) | Usage-analytics database | Platform | `metering-ingest`; the coordinating product service for execution cost | Finance, SRE, the product layer's commercial interpretation | Day-1+ |
| Meter definitions | Git | Platform | `metering-ingest` (validation) | `metering-agent`, `metering-ingest` | Day-1+ |
| Billable usage | The product layer's billing provider | Product layer | Product layer | Finance | Day-1+ |

## Context

### The hole this decides

`metering` is a declared capability with `status: planned`
(`manifests/architecture/components.yaml`), and the platform owes it a
decision. The history is a sequence of withdrawals:

- **OpenMeter was withdrawn** because "it wanted ClickHouse, Kafka and Svix
  for a capability no box ran" (ADR-066 add.3).
- Its announced replacement — "the PostgreSQL implementation already written
  at `internal/kube-sbt/providers/metering`" (ADR-066 add.3) — **does not
  compile and is wired to nothing**: `models.UsageEvent` is undefined,
  `IngestUsageEvent` has no callers, no migration creates a usage table, and
  `cmd/kube-sbt` builds only because it never imports the broken packages.
- The legacy ports it would have implemented are shaped by the withdrawn
  vendor: `interfaces/metering.go` and `interfaces/billing.go` still carry
  OpenMeter's concepts, and the catalog's declared consumer (`GetTenantCost`)
  is a stub.
- **No metering write path exists anywhere.** There is no event a workload
  emits, no window a process closes, and no record a database keeps.

Meanwhile the one capacity the platform does have — a metrics store — was
claimed by ADR-052 as sufficient ("derives from pod resource-seconds already
collected by the observability stack"), and that claim does not survive
inspection: spokes run no kubelet/cAdvisor scrape, tenant kube-state-metrics
series are dropped before remote-write, and the corrected sentence and
ownership rows carry this ADR's amendment.

### The bar the design carries forward

Any replacement has to hold the principles this capability is accountable
to, and this ADR states them as decisions rather than hopes:

1. **Two lanes.** Telemetry and the usage record are separate architectures.
   Telemetry may be *evidence* the metering lane reads; a metrics store is
   never the system of record for money, and no invoice is ever computed at
   read time from a live query.
2. **Platform-authoritative identity.** Quantity may come from measurement;
   identity — tenant, application, resource, and every price dimension —
   comes from the Kubernetes API and platform-written fields, never from a
   workload's own labels or environment, and never from a series' labels.
3. **Billing-grade record semantics.** Every usage record carries: a stable
   idempotency identity, a resource identity stable across renames (UID),
   the tenant identity, a measurement interval, a quantity with a unit, a
   source, an observed-at and an effective-at time; corrections are
   append-only when they exist at all; backfills are flagged; missing data
   produces explicit records — never estimates; and aggregates are computed
   only from records.
4. **Coverage is proven, not assumed.** A closed window means the whole
   interval was observable, not merely that a sample exists near its end.
5. **No undefined financial write paths.** Nothing can move a number on an
   invoice except a governed, attributed, append-only mechanism.
6. **The catalog stays declarative** (ADR-012, **amended**): plans and
   metrics are declared in git with the fleet's values and reconciled
   into the billing engine; subscription *intent* (including effective
   dates) is declared while subscription *state* remains runtime
   (Decision 12); runtime usage is the only imperative path.

### The reference project

`reference-projects/lago/` is a vendored clone of `github.com/getlago/lago`
(AGPLv3, images `getlago/api:v1.53.0` / `getlago/front:v1.53.0`). What it
offers this decision:

- **An events API with the right contract**: `transaction_id` (idempotency),
  `external_subscription_id`, `code` (billable metric), Unix `timestamp`,
  `properties`, optional precise amount (`connectors/README.md`). Quantity
  is **not** a top-level field — it travels in `properties`, and the
  billable metric's field mapping decides which property aggregates
  (Decision 3).
- **Billable metrics with aggregations** (`sum`, `weighted_sum`, `max`,
  `count`, `distinct`) and **group-by properties** — the hook for grouping
  charges by cell or burst placement.
- **A light profile** (`deploy/docker-compose.light.yml`): Postgres, Redis,
  api, api-worker (Sidekiq), api-clock, pdf, front — **no Kafka, no
  Redpanda, no ClickHouse, no events-processor.** The high-volume path
  (`events-processor/README.md`) requires ClickHouse and a Kafka log; it is
  the exact footprint ADR-066 used to withdraw OpenMeter.
- **Connector precedent**: `connectors/*.yml` are trivial normalisers onto
  the events format, so producing into Lago is a known-small piece of work.

Our volume decides the profile: a few hundred containers closing windows on
a 30-second cadence is well under one event per second at the box. HTTP
ingestion into the light profile is the whole path; the Kafka path is
excluded (Decision 2).

A second vendored project settles the terminal hop empirically:
`reference-projects/litellm/litellm/integrations/lago.py` posts every usage
event to Lago's events API as **one plain HTTP POST with a bearer key, no
queue and no bus** — because producer and engine share fate and loss is
tolerable there. This box does not share fate across clusters, so it needs
the same terminal hop *preceded by* a durable local write-ahead log and a
durable shared-storage spool; it does not need a message bus.

### Sourcing is the missing half

The store can be queried from the hub — ADR-083 already gives every cluster
a query API reachable through its own ingress — so a hub-side poller is
*possible*. It is not good enough for three verified reasons:

1. **Lifecycle truth decays.** A pod created, run and deleted while the
   observer is down leaves no trace in the live API, and Kubernetes watch
   semantics (finite history, relist) are not a historical event store.
2. **Retention bounds reconstruction.** Every store holds 15 days
   (`retentionPeriod: "15d"`, spoke and `victoriametrics/storage.yaml`),
   so any design must recover within that horizon or admit a residual.
3. **Cross-cluster polling makes billing depend on hub-initiated ingress
   reachability** for correctness, not just freshness.

Publishing from the source inverts all three: the process that watches the
API and measures the store commits identity-bearing events to a local
durable write-ahead log as they happen, and a shipper carries them into the
object store this platform already runs on both sides — the Hetzner Object
Storage and Infisical/ESO credential pattern of ADR-014 — from which the
hub pulls. What the transport must add on top of the WAL — backpressure,
retry with backoff, and purge-after-acknowledgement — is a
solved problem shipped as an off-the-shelf log shipper, not code the
platform should write (Alternatives considered).

### Measured reality (verified in this repository)

| Fact | Value | Evidence |
|---|---|---|
| Metrics backend, per box | `VMSingle platform`, **15d retention, raw samples, no downsampling** (OSS VictoriaMetrics does not downsample); `VLogs` 7d for logs | spoke `victoriametrics/storage.yaml:17-28`, hub `:41` |
| CPU quantity | `container_cpu_usage_seconds_total`, kubelet `/metrics/cadvisor` via apiserver proxy, **30s**, no label drops; cAdvisor `id` label embeds the pod UID (evidence only, never identity — Decision 8) | hub `grafana-alloy.yaml` `prometheus.scrape "cadvisor"` |
| Memory quantity | `container_memory_working_set_bytes`, same scrape, **30s** | same |
| Restart sources | API `restartCount` (live only); KSM restart series (dropped for tenants today — `grafana-alloy.yaml:35-41`); counter resets in the store (historical, 15d) | as cited |
| Series identity stamps | `cell_id` and `cluster` from platform-owned Alloy env `CELL_ID`; `tenant` likewise — none writable by workloads | `grafana-alloy.yaml:66-90` |
| kube-sbt topology | hub-only `Deployment`, **replicas: 2**, no leader election anywhere in the codebase | `hub-core-services/kube-sbt/manager.yaml:21` |
| Spoke collection floor | **absent** — no kubelet/cAdvisor scrape on spokes; prerequisite of Decision 13 | spoke `grafana-alloy.yaml` (no cadvisor job) |
| Existing shipper on spokes | Alloy `v1.5.1`, egress **cluster-local only** (VMSingle/VLogs); no Vector or Fluent Bit anywhere | spoke `grafana-alloy.yaml:92-100` |
| Storage classes | templated per provider: **`hcloud-volumes`** (Hetzner — detach/attach, survives node loss) vs **`local-path`** (hybrid — node-pinned) | `spoke-catalog/templated-fields.yaml:224-231`, `providers/hetzner/cnpg-cluster.yaml:92`, `providers/hybrid/cnpg-cluster.yaml:101` |
| Namespace-label admission | ABI policy is `Enforce` but matches **workload kinds only** — no `kind: Namespace` rule; namespace labels are not fail-closed today | `spoke-catalog/infra/kyverno-tenant-abi.yaml:24-44` |
| platform-db backup | CNPG `barmanObjectStore` backup configured (physical backup/PITR at cluster level) | `hub-core-services/database/platform-db.yaml:115` |
| Lago submodule | `reference-projects/lago/api/` is an **empty submodule** — engine verification requires initialising it | directory listing |

## Decision

### 1. The two lanes, and the shape of the metering lane

```text
SPOKE (per cluster)                          SHARED OBJECT STORE          HUB (per box)
┌─────────────────────────────────────┐      ┌───────────────────┐       ┌──────────────────────────────┐
│ metering-agent pod                  │ PUT  │ metering-events   │ list  │ metering-ingest              │
│ (StatefulSet, replicas: 1,         ├─────►│  bucket           │◄──────┤ (Deployment, replicas: 2,    │
│  Retain(×2), hcloud-volumes)       │ sigv4│  <box>/inbox/…    │ /get  │  behind a Service):          │
│ ┌─────────────────────────────────┐ │      │  retention = the  │       │  • durable checkpoint,       │
│ │ metering-agent                 │ │      │  SIOE + margin   │       │    lease-serialized drain    │
│ │  • API watch: identity,        │ │      │  (Decision 6)     │       │  • validate vs catalog,      │
│ │    lifecycle, dimensions (10)  │ │      └───────────────────┘       │    isolate poison (12)       │
│ │  • query local VMSingle:       │ │                                  │  • Lago 2xx → advance        │
│ │    coverage-gated (7)          │ │                                  │    checkpoint (6)            │
│ │  • commit WAL records (4):     │ │                                  │        │                     │
│ │    observation facts + events  │ │                                  │        ▼                     │
│ │  • rotation + hard capacity    │ │                                  │ LAGO (light profile on       │
│ │    limit (Vector removes acked)│ │                                  │  platform CNPG + Redis):     │
│ ├─────────────────────────────────┤ │                                  │  sole financial SoR          │
│ │ vector (sidecar)               │ │                                  └──────────────────────────────┘
│ │  • file source: WAL* sealed   │ │
│ │    (rotation-aware checkpoints)│ │
│ │  • in-memory buffer (e2e acks) │ │
│ │  • s3 sink (2xx-acked, e2e)    │ │
│ └─────────────────────────────────┘ │
│   one PVC: WAL + vector state       │      local VMSingle (observability
└─────────────────────────────────────┘      lane, unchanged; evidence only)
```

The observability lane is untouched by this ADR: Alloy, the stores, Grafana
and ADR-078's opt-in rules continue to mean *support and visibility*. What
changes is that the metering lane is now a separate architecture with its
own transport, its own record and its own failure semantics.

### 2. Lago is the billing engine: light profile, on the platform database, box-local

- Lago runs **on the hub of every box**, storing its state as an
  **isolated schema boundary inside the existing `platform-db` CNPG
  cluster** (`hub-core-services/database/platform-db.yaml`), against the
  Redis the hub already runs — no new stateful component is introduced.
- Deployment is the **light profile**: api, api-worker (Sidekiq), api-clock,
  pdf, front — with the parts of a production deployment that are easy to
  omit and impossible to skip: a **migration step** runs to completion
  before first traffic (the upstream container's job, adapted to our
  database), **RSA signing material and application encryption keys** are
  provisioned from Infisical (never generated ad hoc, never in a values
  file), and the **`pg_partman` question is a prerequisite**, not a
  follow-up — it is part of deciding how this schema lives on a managed
  CNPG cluster rather than Lago's bundled database (Decision 13).
- **Kafka, Redpanda, ClickHouse and `events-processor` are excluded by
  decision**, not by omission: they are the footprint that withdrew OpenMeter
  (ADR-066 add.3), and our volume (sub-1 event/s at the box) does not
  approach their reason for existing. Reintroducing them is a new decision.
- The clone is **AGPLv3**; the platform runs unmodified upstream images and
  writes its own integration code, which is the licence's expected shape.
- Identity for ingestion (API keys) and for every downstream consumer comes
  from Infisical, never from a value file (scope requirements in Decision 6,
  security posture in Decision 13).
- **Backup/PITR is inherited at the cluster level, not assumed at the
  schema level**: `platform-db` already carries a `barmanObjectStore`
  backup contract, and Decision 13 requires proving that contract restores
  the Lago schema specifically, plus a tested restore procedure — because
  once Lago holds invoices, that database is financial state (Decision 13).

**Financial scope — box-local invoicing.** One Lago instance per hub means
**billing is box-scoped**, exactly as observability is (ADR-083: "the
datasource set is generated per box, not per cluster", "no query spans
clusters"):

```text
Tenant A
 ├─ Box 1 → Invoice 1   (subscription <tenant>:<app>, unique within Box 1)
 └─ Box 2 → Invoice 2   (subscription <tenant>:<app>, unique within Box 2)
```

A tenant deployed on two boxes receives two invoices. This is a
**financial-domain invariant of the product**, not an incidental
consequence of topology; cross-box rollup invoicing would require a billing
authority above the boxes and is explicitly out of scope for v1.

### 3. The usage record is Lago's events — the sole system of record

**Lago's usage events are the usage record, the input to a financial
chain that Lago models in separate stages** — events → billable-metric
evaluation → fees/charges → billing periods → invoices → adjustments/credit
notes. There is no second platform ledger: one resource, one system of
record (ADR-039). The transport's stages — WAL file, shipper in-memory
hop,
S3 objects — are **bounded transport artifacts and never the record**
(Decision 4/5/6). The record semantics map as follows, and every row must
be verified against the engine before the capability ships (Decision 13):

| Required semantics | Where it lives |
|---|---|
| Record identity / idempotency | `transaction_id`, **deterministic from the business observation** — `hash(meter_code, meter_version, resource_uid, window_start, window_end, dimension_snapshot_version)` (Decision 4). The **meter code is part of the hash**, so `container-cpu-seconds` and `container-memory-byte-seconds` can never collide on the same resource and window, and codes are themselves globally unique and versioned (`container-cpu-seconds.v1`, Decision 12). `dimension_snapshot_version` identifies the **complete authoritative dimension snapshot** (every price-affecting dimension value plus subscription/plan version for the window), not an unspecified bare counter — see the invariant below. Never random, never derived from transport position: every replay of the same observation converges on the same identity |
| Resource identity across renames | Kubernetes **UID** carried in `properties.resource_uid`, sourced from the API — never the name, never a telemetry label |
| Price snapshot carried in the record | The event itself carries the immutable price-affecting snapshot as properties — `properties.cost_center`, `properties.cell_id`, `properties.burst_class`, `properties.plan_version`, `properties.subscription_version`, `properties.dimension_snapshot_version`. The version remains the identity/audit key (row above), but the financial record is **interpretable on its own**: historical pricing never depends on an old `metering-boundaries` revision still existing |
| Tenant identity | `external_subscription_id`, derived from namespace ABI labels (`tenant-id`, `app-id`) and created by the catalog reconciler; unique per Lago instance, which is per box (Decision 2); never from telemetry |
| Measurement interval | `properties.window_start` / `properties.window_end` (effective interval); event `timestamp` = window end (effective-at) |
| Quantity + unit | `properties.quantity` + `properties.unit` — **properties, not a top-level field**; the billable metric's field mapping names exactly these properties as the aggregate input, and that mapping is verified against v1.53 (Decision 13) |
| Source | `properties.source` ∈ {`metric-window`, `api-lifecycle`, `gap-marker`} (Decision 11) |
| Four timestamps, never conflated | **`observed_at`** = when the source measurement was observed/closed — stamped by the agent; for windowed usage this is the closure/observation timestamp, **not the window end**; travels in `properties.observed_at`. **`effective_at`** = the interval/measurement time the event represents, maps to Lago's native `event.timestamp` (= window end, per the interval row above). **`received_at`** = `metering-ingest` receipt time (transport-ingest, stamped at receipt), travels in `properties.received_at` — it is never presented as observation time. **`created_at`** = Lago persistence time, engine-owned. The first three are **logical accounting/platform timestamps owned by the platform** in platform-chosen fields; Lago's persistence time substitutes for none of them |
| Backfill flag | `properties.backfill = true` on any event published from store reconstruction |
| Missing data | Explicit **gap-marker events** (Decision 7), on a metric that no plan can charge for (invariant enforced by the reconciler, Decision 12) |
| Aggregates from records only | Lago computes charges from its events; no PromQL path to an invoice exists by construction |

**One window, one immutable snapshot.** A usage event's interval may
contain exactly **one** immutable snapshot — meter version +
subscription/plan version + the complete price-dimension snapshot. Any
effective semantic, subscription or dimension change *inside* a window
**closes the current window at the boundary and starts a new
deterministic event** carrying the new `dimension_snapshot_version`:

```text
10:00–10:30 → cost-center=A, snapshot v7   (two events, two identities)
10:30–11:00 → cost-center=B, snapshot v8   ← window split at 10:30
```

A single 10:00–11:00 event can never straddle two price dimensions.
The same rule covers an effective-dated plan/subscription change landing
mid-window: the effective date *is* the boundary. Enforcement is
concrete, not implied: price-affecting **namespace** dimensions are
**immutable in v1** (Decision 10), so labels can never create a
mid-window boundary; subscription/plan effective dates reach the agent
through the named **`metering-boundaries` ConfigMap** contract (Decision
12), and the agent splits any window straddling a declared boundary at
event-build time. This invariant is the event-side
counterpart of Decision 12: effective-dating *defines* the boundaries;
this rule *enforces them on the wire*. Splits are ordinary events, never
estimates.

**Corrections do not ship in v1.** v1 publishes exactly the three sources
of Decision 11; a correcting-event protocol (negative quantities,
`corrects` references, post-invoice restatement) is **an accounting
protocol that must be demonstrated against the engine before it is
claimed** — it is out of scope for this ADR and gated on its own
verification suite (Decision 13) as a future capability.

**Ownership of the financial chain.** The stages are distinct resources
even though Lago is their common system:

```text
Git catalog ──► Lago catalog ──► usage event ──► billable charge
                                                    │
                                    invoice ◄───────┘
                                      │
                              adjustment / credit note
```

### 4. The agent and its single write-ahead log

One `metering-agent` per spoke — a `StatefulSet`, **replicas: 1,
`volumeClaimTemplate` with `whenDeleted: Retain` / `whenScaled: Retain`**
(Decision 9) — in the platform's own namespace. It
watches the cluster's API for identity and lifecycle (UID, ABI labels,
create/delete/`restartCount`, dimension authorities), queries the cluster's
local VMSingle for quantities through Decision 7's coverage gate, stamps
dimensions at origin (`cell_id` from platform-owned configuration, burst
from the platform-written `priorityClassName`), and commits everything it
learns to **one append-only write-ahead log** on the shared PVC.

**The WAL is a domain-event log, not a state file plus an outbox.** It
contains two record types — `observation` facts (pod created, restarted,
deleted, with evidence timestamps) and `usage-event` records (closed
windows, gap-markers) — and **observation state is derived by replaying
it, never separately persisted**. There is no second state store whose
semantics could drift from the log.

**Commit semantics.** Each record is self-describing (newline-framed,
length-checkable, CRC-protected). A record is *committed* exactly when

```text
0. on active-file creation (first start, and after each seal):
   create wal.active → fsync(parent directory)
   — the directory entry itself must be durable before any record
   can be committed; write_all + fsync(file) alone does not make
   the file's existence crash-safe in every crash scenario
1. write_all(record)      — the full frame, in a loop until fully written;
                            a short write is an error, never a silent
                            partial record
2. fsync(active file)     — returns success          ⇒ committed
```

at sub-1 record/s this costs nothing — giving the invariant:

> A record is observable after recovery iff it passed the configured
> durability boundary.

**Rotation is a two-step durable operation, and the seal *is* the
publish event**: seal the active file, then
`rename()` it into the `wal.*` glob **and `fsync()` the parent directory**,
so a crash between the two steps cannot leave a renamed file that the
directory does not know about (rename persists after `fsync(dir)`; without
it, recovery may not see the sealed file at all). **Only after this
sequence does the file enter Vector's glob — the active file is never in
the source pattern** (Decision 5), so a record can never be shipped
before it crossed the durability boundary.

Recovery rules: a torn tail (partial record, CRC failure, short write
interrupted by crash) is truncated to
the last committed record; an `fsync` failure, disk-full or persistent
`EIO` **stops publication and raises an alert** — the agent will not
advance state it cannot commit, and the resulting uncovered windows become
gaps under Decision 7 rather than silently accepted losses; a process or
node crash loses at most records that never passed the boundary — and those
events, if still derivable from API + store evidence, are re-derived on
replay into the **same deterministic id** (Decision 3), or become gaps.
A crash after rename but before `fsync(dir)` leaves the sealed file either
way (old or new name) — content intact, discovered by the same glob scan.
PVC detach/reattach onto a new node is a normal path on `hcloud-volumes`
and recovers by the same rules.

**The deterministic identity.** `transaction_id` is a pure function of the
business observation — **meter code**, meter version, resource UID, window
bounds, dimension snapshot version (Decision 3) — never of a random mint
and never
of WAL position. Replay, retry, duplicate delivery and restart all converge
on one identity; Lago's idempotency (Decision 6) is terminal.

**Rotation, capacity and reaping — one owner each.**

- **The agent owns** the active WAL, rotation by size/time to sealed files
  (`wal.*`), and a **hard capacity limit** across the WAL directory —
  crossing it takes the disk-full path above (stop committing, alert) so
  an outage can never fill the PVC. The agent **never reads Vector's
  internal checkpoint state** — that is an implementation boundary, not a
  contract.
- **Vector owns** read checkpoints and the removal of acknowledged files
  through its own file-source lifecycle (`remove_after_secs`, set to the
  7-day outer bound). **There is no Vector disk buffer in this design** —
  the WAL is the one and only local durability boundary — so the removal
  invariant has exactly one form:

  > A WAL file may be deleted only after the file-source checkpoint has
  > passed its end, and the checkpoint advances only for events marked
  > `Delivered`, which — with E2E acknowledgements enabled and only
  > in-memory sink buffering — requires the remote S3 2xx. Crash before
  > 2xx ⇒ no advance ⇒ WAL replay. 2xx ⇒ advance ⇒ then removable.

  The blocking suite proves that chain on the pinned version (Decision
  13): crash-before-S3 → WAL replay; S3 2xx → checkpoint advances; file
  removable only after the advanced checkpoint — not merely `remove_after_secs`
  elapsed.
- **Replay cannot be stranded**: recovery is anchored on API relist
  (Decision 8), and retained files cover the transition horizon the
  journal needs; depth and age alerting fires within minutes, so both
  bounds are backstops, never detectors.

**Storage and rollout.** The agent is a **`StatefulSet` with a
`volumeClaimTemplate`** on **`hcloud-volumes`** (the templated
Hetzner class — detached on node failure, reattached elsewhere), with
`persistentVolumeClaimRetentionPolicy: {whenDeleted: Retain,
whenScaled: Retain}` — the Kubernetes-native retention contract for
claim-template PVCs. This is deliberately **not a `Deployment`**: that
retention field belongs to `StatefulSetSpec`, and a single persistent
writer is the case StatefulSets exist for; the alternative
Deployment + `Recreate` + separately-managed PVC story was considered and
rejected. And
**metering v1 runs on Hetzner spokes only**; hybrid spokes use node-pinned
`local-path`, which cannot meet these semantics, and stay unmetered — the
same boundary ADR-046 already draws. StatefulSet ordered recreation of the
single ordinal plus the replicas: 1 rule of Decision 9 guarantee one
writer across rollouts (RWO detach/reattach is the normal
`hcloud-volumes` path); there is no
singleton-RWO precedent in the repository today, so this is a new, fully
specified pattern rather than an inherited one.

### 5. Transport: a Vector sidecar into object storage

Egress is an off-the-shelf shipper — **Vector** (Datadog, MPL-2.0, Rust) —
running as a **second container in the agent pod**, sharing the PVC.
Exactly what Vector provides, stated precisely:

- **Source**: the `file` source reads **sealed WAL files only** — the
  active file is **excluded from the source glob** and enters it only
  after the agent's seal + `rename` + parent-directory `fsync`
  (Decision 4) — with **rotation-aware read checkpoints** persisted
  under the Vector data directory on the PVC. This is what makes the
  agent's commit invariant *compose* with the shipper:

  ```text
  anything Vector can ship  ⟹  it already crossed the WAL
                               durability boundary
  ```

  Without sealed-only visibility the sequence *write_all → Vector reads →
  S3 2xx → checkpoint advance → agent fsync fails → file reaped* would
  externally acknowledge a record the agent never committed. A checkpoint
  is a *read
  position*, nothing more: a lost checkpoint means re-reads, which are
  safe because every re-read carries the same deterministic
  `transaction_id`s. Non-event records (observation facts) are filtered
  after read; only `usage-event` records continue.
- **Buffer: in-memory only — no disk buffer.** The WAL is already the
  durable local source; adding a Vector disk buffer would create a
  *second* local durability boundary and make WAL-deletion safety depend
  on another persistence mechanism, for no gain. The flow and its
  invariant:

  ```text
  WAL (durable) → file source → in-memory buffer → S3
  checkpoint advances only after S3 2xx
  WAL file removal only after that advanced checkpoint
  crash before 2xx → checkpoint frozen → WAL replay
  ```

  With E2E acknowledgements enabled and only in-memory buffering,
  `Delivered` requires the remote 2xx — there is no disk-buffer path that
  could satisfy it. The blocking suite proves the chain (Decision 13).
  Backpressure (bounded memory, `when_full = "block"`) stalls Vector's
  *reads*, not the agent's *appends* — appends go to the WAL regardless.
- **Sink**: the `s3` sink PUTs batches (ndjson, gzip, flushed within ~60s
  or a size cap) into `s3://metering-events/<box>/inbox/` with keys
  templated **at request-build time**, `…/%Y/%m/%d/%H/<batch-uuid>.ndjson.gz`,
  and an **explicit `retry_attempts` cap** bounding how long a built
  request may keep retrying — Decision 6's `T`. Retries reuse the built
  key, so a batch built at 10:59:59 that finally succeeds at 11:20 still
  lands under the `10/` prefix; the watermark's request-lifetime bound
  covers exactly that case, and the `batch-uuid` keeps even a rebuilt
  request collision-free. Authentication is SigV4 over TLS with the box's
  metering keys (Decision
  6). **The composed metering pipeline provides at-least-once delivery
  *effect* — not Vector alone.** Vector's formal at-least-once guarantee
  across restarts requires a disk buffer, which this design deliberately
  does not use; what Vector contributes here is acknowledgement/checkpoint
  semantics. The composition is:

  ```text
  WAL durability (Decision 4)
  + file checkpoint advancing only after successful S3 delivery (Decision 4)
  + S3 2xx
  + deterministic Lago idempotency (Decisions 4/6)
  = at-least-once delivery effect for the metering pipeline
  ```

  This composition is **verified by the blocking Vector suite**
  (Decision 13), not assumed from a single component's documentation.
  Nothing here claims exactly-once from any single stage — exactly-once
  *effect* remains the composition.
- **Honesty about the custom part**: this buys the transport retry loop,
  backoff, in-memory buffering and acknowledgement from a mature shipper. The
  local durability protocol — one WAL, its framing, its fsync boundary, its
  rotation + sealing rules — is small and fully specified in Decision 4, and
  is owned by us. Nothing in the chain is bespoke *transport* code.

The bus stays home: `interfaces.IEventBus` remains exactly as ADR-080 left
it — an unused port. No NATS is added, hub or spoke (Alternatives).

### 6. The hub consumer: durable checkpoint, outage envelope, scoped credentials

`metering-ingest` (hub, `Deployment`, **replicas: 2**, behind a Service) is
the sole Lago writer. It polls `inbox/` every 30–60 seconds and hands
validated events to Lago. Five things make this safe for money:

**A durable consumer checkpoint — never a derived, in-memory cursor.**
The consumer persists its position:

```text
metering_ingest_checkpoint (schema: metering_consumer, in platform-db)
    box · hour-prefix · object key · position · updated_at

metering_processed_objects (same schema — late-object detection)
    box · object_key · etag · processed_at        (retained for the
                                                   object's bucket lifetime)
```

Ordering is honest, not pseudo-distributed-transaction:

```text
POST object's events to Lago
→ Lago returns 2xx
→ commit the checkpoint + processed-object index transaction

crash before the commit → replay the object
→ Lago deduplicates every event by transaction_id (Decision 4)
```

There is **no atomic transaction across Lago and PostgreSQL**; the
guarantee is 2xx-then-commit plus idempotent replay, which is stronger
and more honest than pretending otherwise. A crash at any point re-reads
from the last checkpoint; re-reads are free under deterministic
idempotency. The checkpoint may lag arbitrarily — an outage of any length
inside the envelope (below) is recovered from the bucket, because the
checkpoint is durable and object retention outlives the outage. **Both
tables are consumer state, not financial records**: they hold no usage,
quantities or charges, so the single-system-of-record principle (ADR-039)
is preserved — Lago remains the only financial system of record.

**Concurrency protocol — `replicas: 2` is active/standby, not two
free-running pollers.** Deterministic ids make *duplicate* delivery safe;
they do not make the checkpoint *algorithm* safe — two consumers racing
objects 10 and 11 can advance to 11 while 10 never completes, skipping it
forever. Safety comes from two rules:

1. **A fenced, database-backed lease** — a lease row in `metering_consumer`
   carrying a **monotonically increasing fencing epoch** (holder, expiry,
   epoch, acquired under a row lock). The standby takes over only on lease
   expiry and always resumes from the last durable checkpoint; every
   acquisition increments the epoch. **Every checkpoint or index mutation
   must prove it holds the current epoch**, so a paused-then-resumed stale
   holder can observe but never write:

   ```sql
   UPDATE metering_ingest_checkpoint ...
     AND lease_holder = :me
     AND lease_epoch  = :my_epoch      -- = current epoch at read time
   ```

   If the epoch moved on, zero rows update, the stale writer learns it is
   fenced, and the write is discarded. A lease without fencing is not
   fail-safe — expiry only *usually* prevents two writers; the epoch makes
   it impossible.
2. **Contiguous advancement**: the checkpoint may move only over a
   completed sequence — advance to the highest position K such that
   everything ≤ K is confirmed processed (under the fenced lease this is
   single-writer by construction; it is stated as the invariant
   regardless, so no future change can weaken it).

The alternative serializations — leader election, or partitioned
checkpoints with independently durable ownership — are equivalent; the
fenced lease is chosen because there is exactly one inbox per box and one
row keeps it simple.

**Retention is a financial-RPO decision, not an aesthetic one.** While an
object sits in `inbox/` it is the only durable copy of those events — the
bucket is temporarily load-bearing for billing. Therefore:

```text
Declared Supported Ingestion Outage Envelope (SIOE) = 90 days
S3 lifecycle retention = SIOE + safety margin = 120 days
retention ≥ SIOE + margin — an outage at the envelope boundary must
 never race lifecycle expiration
financial ingestion RPO = 0 within the SIOE — explicitly scoped:
 for hub-consumer / Lago outages, while the metering-events object
 store remains available and retains objects per the declared policy
```

The lifecycle value is *derived from* the envelope plus its margin, not
chosen for volume: a puller broken for up to 90 days loses nothing, and
the 30-day margin means an outage landing exactly at the boundary cannot
race the expiry job. Beyond SIOE + margin, objects expire and the loss
becomes real gaps — past the declared support contract. Alerting on
`oldest-unprocessed-object-age` fires at 7 days (critical) and 30 days
(page) — orders of magnitude inside the envelope — so expiry is a
declared boundary, never an accident. **The store itself is a declared
failure domain**: Hetzner Object Storage is Ceph-backed *within the
selected location* — not cross-region. RPO = 0 says nothing about losing
the location; **cross-location replication / a secondary object-store
copy** for the metering store is a declared future option with its own
cost and a future provider/storage decision (Hetzner lists replication
as *unsupported* today, so this is not a toggle but a new choice),
out of v1 scope, and
the failure-domain boundary is stated here so RPO = 0 is never read as a
cross-region guarantee.

**The polling protocol is specified, and verified against the actual
provider.** Hetzner Object Storage is only *S3-compatible* (a documented
subset), and **does not support notifications** — polling is the design,
not a fallback. The correctness boundary is a **deterministic watermark**,
not a "no new keys for N minutes" heuristic:

```text
prefix H is finalizable only after:
    end(H) + S + T
where
    S = declared maximum producer clock skew           (platform SLO: ≤ 2 min)
    T = maximum request lifetime — batch-build time of the    (platform
        S3 request → successful PUT, bounded by an explicit   SLO: ≤ 5 min)
        Vector `retry_attempts` cap and retry timing
```

The S3 key's prefix hour is fixed **when the request is built**, and
retries reuse the built key — a batch built at 10:59:59 whose PUT finally
succeeds at 11:20 still lands under `…/10/`. The watermark must therefore
bound the *whole request lifetime* `T` (capped retry budget), not a single
attempt; `T` is a **configured bound with a hard retry cap**, not an
observation. Skew `S` is the only remaining way a key lands outside its
eventual prefix, and it is an NTP-guarded SLO (alerting under
`metering-*`, Decision 13).

- The walk is **oldest-first and monotonic** for first-time processing,
  with the durable checkpoint recording position.
- **Late-object handling is a durable rule, not a checkpoint heuristic.**
  A late object's key can sit *lexically before* the checkpoint while
  never having been processed — the checkpoint alone cannot distinguish
  "already processed" from "newly arrived in an old prefix". So each
  processed object is recorded in the `metering_processed_objects` index
  (above), and **every poll cycle re-lists the look-behind window — the
  most recent 24 h, which exceeds `S + T` by an ample margin (≈ 200×,
  ~2.3 orders of magnitude) —
  and processes any key absent from the index**, regardless of checkpoint
  position. The index is written in the same fenced transaction as the
  checkpoint, so late-object detection survives crashes and lease
  handover.
- **Object mutation fails closed.** Hetzner overwrites an object when the
  same key is uploaded with versioning disabled, so the index carries the
  object's ETag and the consumer enforces integrity, not just presence:

  ```text
  same object_key + same ETag   → already processed (duplicate, skip)
  same object_key + different ETag → integrity violation:
      quarantine + alert — never treated as another legitimate
      late object under the same key
  ```

  UUID-based naming makes collisions negligible, but the invariant is
  enforced regardless; proven in the Hetzner suite (Decision 13).
- The three layers, stated together:

  ```text
  monotonic checkpoint    → ordered first-time drain
  processed-object index  → deterministic late-object detection
  transaction_id          → event idempotency (replay, duplicates)
  ```
- Duplicate objects and re-listed keys are tolerated (idempotency makes
  them free); the checkpoint advances only per the fenced lease +
  contiguous rules above.

Before this ships, the protocol is exercised **on Hetzner Object Storage
itself** — PUT, LIST, repeated-LIST stability, GET, late PUT arriving
after its prefix was finalized (index-driven recovery), bounded retry
exhaustion, consumer restart mid-prefix, duplicate delivery, prefix
rollover, and observed skew and request lifetimes against the declared
`S`/`T` bounds (Decision 13).

**Credentials are scoped per box, and the scoping is verified, not
assumed.** Hetzner access keys are project-wide **by default**;
fine-grained restriction requires bucket access policies. Multiple boxes
share `metering-events`, so **this is an isolation requirement, not
least-privilege cleanup**: a Box 1 hub must be structurally unable to read
Box 2's billing records. The required posture:

| Principal | Required scope |
|---|---|
| spoke Box N metering key | `PutObject` → `<box-N>/inbox/*` only |
| hub Box N metering key | `GetObject` → `<box-N>/inbox/*`; `ListBucket` restricted to prefix `<box-N>/inbox/*` (`s3:prefix` condition); `PutObject` → `<box-N>/failed/*`; `GetObject` → `<box-N>/failed/*` |
| cross-box access | **explicitly denied** — no principal may `GetObject` or `ListBucket` any other `<box-M>/…` prefix; denial is asserted in the policy, not inferred from absence of an allow |
| `failed/` (dead letters) | written only by Box N's own hub consumer |

Spoke and hub write to disjoint prefixes (the hub never writes `inbox/`;
dead letters flow Box N hub → Box N `failed/` → Box N hub requeue path).

Verification that the provider can express **prefix-scoped List and Get
reliably** is a blocking item (Decision 13); the fallback if it cannot is
the already-declared **per-box bucket or isolated project** — chosen
before implementation, not discovered during it.

### 7. Coverage and completeness (the closure rule)

No quantity is published for an interval unless the interval is proven
observable end to end. For window `[t₀, t₁)` against the local store at 30s
cadence:

- **Segments, not wholes.** A delta > 90s between consecutive samples (or a
  counter reset) splits the window. Each segment bills its own boundary
  samples; the **union of covered segments is the billable quantity**, and
  every uncovered remainder becomes a gap-marker. A window with 9 minutes
  observed and 1 minute missing bills 9 minutes and records 1 minute of
  gap — presence percentages are diagnostics, never a licence to bill a
  partially observed interval.
- **Restarts are not gaps.** A counter reset in
  `container_cpu_usage_seconds_total` is a lifecycle fact (Decision 8): the
  window bills the sum of unbroken segments.
- **CPU sufficiency** = boundary samples present per segment, no unexplained
  resets; quantity = sum of per-segment increases.
- **Memory sufficiency** = samples present across the segment's stretches;
  quantity = byte-seconds by **step-function integration over observed
  samples only** — for each consecutive sample pair in a stretch:

  ```text
  (tᵢ, vᵢ), (tᵢ₊₁, vᵢ₊₁):
      byte_seconds += vᵢ × (tᵢ₊₁ − tᵢ)      (rectangle/step rule)
  ```

  the earlier sample's value is held until the next. Only intervals
  bounded by actual samples are covered: **no interpolation** between
  samples, **no extrapolation** to `t₀`/`t₁`; the uncovered prefix and
  suffix of a stretch (window start → first sample, last sample → window
  end) are not billed and fall to the gap rule above. An average is
  derivable from the billed primitive but never the billed primitive.
- **Extraction is from raw samples.** `rate()` and `increase()` **banned**:
  Prometheus extrapolates both to range boundaries, which is estimation by
  another name. Quantities are accumulated from actual sample points with
  explicit boundary handling (delta summation per segment, integration
  per observed stretch).
- **The gap invariant**: *a gap marker never contributes quantity to any
  billable metric and never substitutes for a usage record* — enforced in
  the reconciler contract (Decision 12), not merely by nobody having
  configured it otherwise.
- **Closure cadence** is rolling and minutes-scale, orders of magnitude
  inside every retention horizon.

### 8. Lifecycle recovery after agent downtime

- **While the agent is alive**, create/delete/restart are committed to the
  WAL as observed; delivery — not observation — tolerates outages: events
  sit in the WAL until the PUT succeeds, then in the bucket until the hub consumes.
- **On restart**, the agent relists (list-watch semantics) and folds the
  API's present state over WAL replay: *present now, absent from the log* →
  missed create (the API still has it — capture identity then); *open in
  the log, absent now* → missed delete, closed at the log's last
  observation, **timestamped by store evidence**
  (`container_last_seen` / series presence) where it exists — evidence
  supplies *when*, the log supplies *what*, and identity never comes from
  telemetry.
- **Born and dead entirely inside the downtime**: identity comes from the
  Kubernetes API if the object still exists; timing from store series
  presence within the 15-day horizon. **Telemetry never supplies identity**
  — no parsing of cAdvisor's `id` label, no other series-derived UID. If
  the API cannot name it, the interval is a gap.
- **Residual, stated honestly**: an agent outage longer than store
  retention, during which pods were created *and* deleted and the API has
  since forgotten them, loses their lifecycle. The residual is larger than
  a design that scraped identity out of telemetry labels would be — and it
  is the correct trade: unbilled time, never wrong billing. Mitigations:
  one small process, durable WAL, alerting on age and depth.

### 9. Component topology rule

- **`metering-agent`: StatefulSet, replicas: 1, `volumeClaimTemplate`
  `whenDeleted: Retain` / `whenScaled: Retain`** —
  one writer, always (Decision 4). This is the coordination problem: an
  evaluator whose journal must not fork.
- **`metering-ingest`: replicas: 2 behind a Service, active/standby under
  the consumer lease** (Decision 6) — the standby exists for failover, not
  for parallel draining; safety is single-holder ordering + contiguous
  checkpoint advancement, and a single replica would be an unnecessary hub
  SPOF. The guard clause — *raising the agent above 1 requires
  leader-election first* — applies to the evaluator and its WAL only,
  never to the consumer, whose serialization is the fenced lease.
- Honesty about upstream: the Lago light profile's own api/worker/clock
  run as single instances; ingest HA does not make the engine HA. Engine
  availability risk is accepted and visible (Decision 13's SLOs measure
  end-to-end ingestion, engine included).

### 10. Dimension authority table

Every dimension that can change a price has exactly one authority, and the
agent reads only this table:

| Dimension | Authority | Why it cannot be gamed |
|---|---|---|
| `tenant_id`, `app_id` | Namespace/pod ABI labels, enforced by Kyverno (`kyverno-tenant-abi.yaml`, Enforce) — with the namespace-level rule a prerequisite (Decision 13) | Admission rejects workloads and namespaces without them |
| `cost-center` | Same ABI enforcement | Same |
| `cell_id` | Platform-owned injection (`CELL_ID` env; the cluster identity fact) | Workload labels are never read for this |
| Burst price dimension | Pod `spec.priorityClassName == burst-tenant`, written by platform admission, denied to fleets (`burst-placement-not-fleet-declarable`, Enforce) | Pod spec immutable post-admission; the policy rejects fleet-authored claims |
| Resource identity | Kubernetes **UID** from the API | Names are reusable; UIDs are not |
| Fleet / deployment instance | **Namespace + `cell_id`** (ADR-047 lineage, split by ADR-088) | Both members are platform-rendered |

The rule behind the table: **identity and price dimensions are read only
from the columns above; any other source is discarded.** Nothing a tenant
can author is on the list.

**Price-affecting namespace dimensions are immutable in v1.** The
namespace ABI admission rule rejects *updates* to `tenant-id`, `app-id`
and `cost-center` after creation (the same policy that reserves them at
creation — Decision 13), so a label change can never open a mid-window
dimension boundary. There is therefore exactly **one** boundary source
for events — the `metering-boundaries` ConfigMap of Decision 12 — not an
unspecified set. A governed flow for changing these labels is a future
capability and would have to reuse that same boundary contract.

### 11. Sources, and what cannot write a record

- `properties.source` ∈ **{`metric-window`, `api-lifecycle`,
  `gap-marker`}**. Nothing else exists in v1 (corrections, Decision 3, are
  a gated future capability).
- **`manual adjustment` does not exist.** There is no ad-hoc write path to
  the record — no direct database mutation, no admin endpoint of ours. The
  future, deliberately out of scope, is Lago's own governed adjustment
  surfaces behind its roles and audit; designing ours would duplicate them.

### 12. The catalog: versioned, fail-closed, race-aware

**ADR-012 is amended, explicitly.** ADR-012 separates a static Git
catalog (meters, features, plans) from dynamic runtime data (users,
subjects, subscriptions, invoices, usage queries) and makes
subscription creation/updates/cancellation REST runtime operations. This
ADR keeps that split intact: **meters, features and plans remain GitOps
catalog resources; subscription state remains runtime state.** Git may
declare the *desired subscription and effective-date intent*, but the
resulting Lago subscription is runtime state created and changed through
the governed subscription lifecycle — declarative intent, attributed
imperative creation (principle 5: no undefined financial write paths).
The catalog decision itself — declarative, git-versioned, rollback-safe
management — stands; its **backend is the Lago API**. The reconciler
(in the kube-sbt codebase, constructor-injected) renders fleet values into
Lago objects: plans, billable metrics, customers, and subscription intent
(`external_subscription_id` = `<tenant-id>:<app-id>`, unique per box).

**Billing semantics are immutable and versioned.** A billable metric's
quantity property, unit, aggregation and dimensions are its *accounting
semantics*; they may never be mutated in place once events exist. A
semantic change creates a new versioned code — `container-cpu-seconds.v1`,
`container-cpu-seconds.v2` — with effective dating on plans and
subscriptions. The reconciler **fails closed** if a Git change would mutate
an existing metric's semantics, and the deterministic id embeds the
**meter code and its version** (Decision 4) so events computed under v1
remain v1 events
forever. *Git rollback is not financial rollback*: reverting a commit does
not un-invoice an invoice, which is precisely why semantics version
forward. Effective-dating is **not merely catalog-side bookkeeping**: it
supplies the window boundaries for Decision 3's one-window-one-snapshot
invariant through a named, platform-owned delivery contract — this is
load-bearing, because the quantity is computed at the spoke *before*
ingestion, so the agent must know the boundaries in advance:

```text
Git subscription/plan intent (effective dates)
        ↓
catalog reconciler — validates fail-closed, stamps a generation
        ↓
`metering-boundaries` ConfigMap — platform-rendered, one per spoke,
    written into the spoke's existing GitOps source (same channel as
    spoke-catalog values); platform-authored, tenant-unwritable
        ↓
standard GitOps delivery to the spoke (platform namespace)
        ↓
metering-agent watches the ConfigMap
        ↓
window split exactly at declared boundaries → two deterministic
events with distinct `dimension_snapshot_version` values
```

The ConfigMap manifest and the Git intent are both platform-controlled
(repo RBAC + spoke namespace RBAC); the agent fails closed if the
boundary generation it is building against disappears or fails
validation — an unknown boundary state stops publication (gap), never a
guessed split. "Exports" is deliberately not left as an implementation
detail: **the ConfigMap is the contract.**

**Boundary activation is part of the financial contract, not just
delivery.** GitOps propagation can race the effective date — Git
declares 12:05, the ConfigMap reaches the spoke at 12:06 — and v1 has no
corrections, so an interval that fired before its boundary arrived cannot
be repaired afterward. The full lifecycle:

```text
Git subscription intent
  → reconciler creates/updates the Lago runtime subscription
  → runtime state confirmed
  → boundary prepared with lead time:
        effective_at ≥ boundary_ready_at + propagation_safety_window
        (platform constant, default 24 h; the reconciler fail-closes
         any declaration that violates it)
  → GitOps delivery
  → agent loads + validates the generation and commits a durable
        `boundary-loaded` observation fact to its WAL (the local ack)
  → effective date becomes usable
```

Three rules make this complete:

1. **Lead time is enforced, not hoped for** — no boundary may take effect
   inside the propagation safety window; Git-side fail-closed validation
   plus the local load ack make "present at the spoke before the
   effective date" a checked precondition.
2. **The agent only builds windows across a boundary it has loaded** — if
   an effective date arrives without its generation present and
   validated, publication stops exactly at that boundary (gap + alert,
   never a guessed split). The safety window makes this path
   exceptional; fail-closed makes it safe when it happens.
3. **Boundaries are append-only.** Once a generation is active or usage
   events exist under it, its effective timestamp and snapshot are
   immutable; a Git rollback that would remove or move an active or
   published boundary is rejected by the reconciler — the same fail-closed
   class as the semantics-mutation rule below. Rollback may affect only
   future, never-yet-effective intent.

**Reconciler contract (fail-closed, not convention):**

- `metering-gap` **MUST NOT** be referenced by any billable charge or plan;
  its non-billable classification is immutable and re-validated on every
  reconciliation; a violation fails the reconciliation, it does not warn.
- Catalog convergence races with usage are first-class states, not
  dead-letters: **unknown code due to catalog-convergence lag** and
  **subscription not yet reconciled** are *retryable* (return to queue
  with backoff; the durable checkpoint and object retention make waiting
  free) — a declaration merged to Git cannot be permanently lost or
  permanently rejected by an eventual-consistency window. **Permanent**
  failures (schema violation, unknown code that *has* been resolvable
  beyond a grace watermark, engine rejection) move to `failed/` with full
  context.
- **Poison isolation happens outside Lago, because Lago's batch ingestion
  is atomic — an invalid event rejects the whole batch.** The consumer
  therefore never posts a known-mixed batch:

  ```text
  validate individually → construct valid subset → POST valid subset

  if Lago still rejects the batch (engine-side validation we could not
  replicate):
      isolate → bisect / per-event retry → quarantine the offender
      in failed/ with full context
  checkpoint object only after every event reaches a terminal state
      (accepted or quarantined)
  ```

  One bad record must never block billing records behind it, and the
  guarantee must not depend on partial-batch persistence that the engine
  does not provide. The exact rejection mechanism and bisect behaviour are
  proven in the v1.53 engine suite (Decision 13).

### 13. Prerequisites, financial operations, SLOs, and what stays unproven

**Prerequisites** (all must land before `metering` leaves `planned`):

- **Collection floor**: spoke Alloy gains kubelet/cAdvisor scrape and the
  KSM carve-out of ADR-078 add.1 §6 for `tenant-*` container series, as
  accounting data.
- **Reserved namespace identity**: a `kind: Namespace` rule added to the
  ABI policy (Enforce, `failurePolicy: Fail`) that **reserves** rather than
  merely requires — `tenant-id`, `app-id`, `cost-center` on namespaces are
  **platform-rendered values**, tenant actors may not supply or mutate them
  (admission rejects tenant-authored values; RBAC protects the namespace
  object), price-affecting labels are **immutable on `Namespace` update**
  (admission rejects changes after creation — Decision 10's v1 rule), and
  workload labels are **validated against the authoritative
  namespace values** at admission. Requiring the labels without reserving
  them would leave tenants holding the authority.
- **Object store**: the `metering-events` bucket, its SIOE lifecycle rules,
  bucket access policy for credential scoping (Decision 6), metering key
  pair in Infisical (`/spoke-pool/<spoke>/metering/`), ExternalSecret
  templates on spoke and hub, NetworkPolicy egress to the store endpoint —
  all of it **declared external state in git, managed through the
  platform's declarative infrastructure boundary** (the Day-0/bootstrap
  path with drift detection — ADR-014's model of centralized, GitOps-managed
  stateful infrastructure applied to account-level resources). Where that
  boundary lacks an object-store primitive today, establishing it is part
  of this prerequisite; **runbook-only management is explicitly rejected**.
- **WAL storage**: agent `StatefulSet` + `volumeClaimTemplate` on
  `hcloud-volumes`, retention `whenDeleted: Retain` /
  `whenScaled: Retain` — fully specified (Decision 4).
- **Lago state**: migration step, RSA/encryption keys from Infisical, the
  `pg_partman` decision for this schema on CNPG, `metering_consumer`
  schema — checkpoint, **fenced lease with epoch**, and
  `metering_processed_objects` index.
- **Financial operations**: the retention/RPO policy below, the invoice
  export procedure, and a **tested restore** of the Lago schema.

**Financial retention and DR.** Within the SIOE, ingestion RPO = 0
(Decision 6). Beyond the pipeline: `platform-db` PITR must be demonstrated
to restore the Lago schema specifically (cluster-level barman configured ≠
schema recoverable — verification item); invoices and adjustments are
exported as documents on issue to a **separate financial-documents bucket
with its own lifecycle policy (default: 7 years)** — never the metering
bucket, whose 120-day metering lifecycle would otherwise destroy them —
independent of engine-side event retention; RTO
and the restore drill are documented in a runbook and exercised before
this ADR's capability ships.

**Accounting SLOs — separate from observability health.** Metrics missing
now means *financial quantity unavailable*, so metering carries its own
contract, not Alloy's: **ingestion freshness** (committed event → Lago
accepted, target 99 % within 5 minutes) *and, separately*,
**materialization freshness** (Lago accepted → event reflected in
billable metric evaluation / charge state, target 99 % within 15 minutes)
— Lago's processing is asynchronous, so "accepted" must never be allowed
to mask a broken worker or aggregation path; two SLOs, two `metering-*`
alerts — coverage-gap rate (gap-markers as
a share of windows, alerted on trend), consumer lag
(`oldest-unprocessed-object-age` against the SIOE: 7d critical, 30d page),
**producer clock-skew bound (`S` ≤ 2 min, NTP alerting) and
maximum request-lifetime bound (`T` ≤ 5 min, hard `retry_attempts` cap)**
— the watermark
inputs of Decision 6, which are configured bounds and SLOs, not
assumptions —
WAL depth and fsync-failure alerting — all as dedicated `metering-*`
alert rules that fire whether or not Grafana looks healthy.

**Blocking verification suites** (must pass before this ADR's capability
ships; until then `metering` stays `planned` and this ADR `Proposed`):

1. **Lago engine suite** (against vendored v1.53 — requires initialising
   the `reference-projects/lago/api` submodule): quantity-in-properties
   aggregation field mapping; duplicate `transaction_id` (incl. after
   timeout); **atomic batch rejection** (invalid event rejects the batch —
   the premise of the consumer's bisect/quarantine isolation, Decision
   12); late/backfilled timestamps; event retention/purge; plan and metric
   immutability after events exist; customer/subscription provisioning
   calls; **API-key scope granularity** (or documented limitation +
   compensating controls, Decision 13 security); schema-level PITR
   restore; **accepted → materialized** timing for the materialization
   SLO. Corrections remain excluded until their own future suite.
2. **Vector contract suite** (version pinned by digest at implementation):
   **sealed-only visibility — the active file is never in the source
   glob; no read, upload or checkpoint movement for unsealed bytes;
   anything shipped has crossed the WAL durability boundary**;
   the removal chain — **crash before S3 → checkpoint frozen → WAL
   replay; S3 2xx → checkpoint advances; removal only after the advanced
   checkpoint** (with no disk buffer configured, proving `Delivered`
   cannot be satisfied by local persistence); file-source checkpoint
   survival across WAL rotations and restarts; `retry_attempts` cap and
   observed request-lifetime bound (`T`); S3 sink batch-key reuse across
   retries (key built once at request build).
3. **Hetzner store suite**: PUT/LIST/repeated-LIST stability, GET, late
   PUT arriving after its prefix was finalizable (processed-object index
   recovery), bounded retry exhaustion, consumer crash mid-prefix,
   duplicate objects, prefix rollover, observed `S`/`T` against declared
    bounds, fencing-epoch takeover (stale holder cannot mutate checkpoint),
    bucket-policy credential scoping **including positive cross-box denial
    — Box N's hub key cannot `GetObject` or `ListBucket` any Box M prefix**
    (or the per-box-bucket fallback is selected, Decision 6),
    **object-mutation detection — same key with a changed ETag quarantines
    and alerts (Decision 6)** — all against
    Hetzner Object Storage
    itself, not generic S3.

## Alternatives considered

- **Spoke-local NATS JetStream, hub dials the spokes** — rejected: it
  reinstates ADR-080's weight class and adds an inbound spoke endpoint
  when the box's object store already exists on both sides. Stream
  semantics at sub-1 event/s do not pay for that cost. ADR-080 stands
  untouched.
- **Two local files — journal + separate outbox** — rejected: no atomic
  boundary covers the state transition and its event, so every crash
  window between the two writes is a silent divergence. One WAL with one
  commit boundary replaces it.
- **A hand-rolled HTTP retry loop** — rejected: retry, backoff,
  backpressure, in-memory buffering and acknowledgement come from the shipper.
- **Derived in-memory consumer cursor with bounded replay** — rejected: a
  consumer down longer than the replay window forgets objects that still
  exist in the bucket; for billing ingestion, the position must be durable.
- **S3 event notifications as the consumer trigger** — unavailable:
  Hetzner Object Storage does not support notifications; polling with a
  durable checkpoint is the design.
- **Reusing the Alloy DaemonSet as the shipper** — rejected: store-shaped
  protocols, and it couples the two lanes' ownership (ADR-078's component
  would change when metering transport changes).
- **Fluent Bit instead of Vector** — acceptable runner-up; Vector chosen
  for end-to-end acknowledgement semantics, VRL, and Rust
  memory-safety. Footprint is not binding beside Alloy.
- **Hub poller over ADR-083's query path** — rejected in the sourcing
  analysis: lifecycle decay, 15d bound, correctness-via-ingress.

## Ownership

| Resource Class | System of Record | Lifecycle Owner | Reconciler | Consumer | Phase |
|---|---|---|---|---|---|
| Usage record (events) | Lago events (platform-db, Lago schema) | Platform | `metering-ingest` (write path) | Lago billable-metric evaluation | Day-1+ |
| Charges / invoices / adjustments | Lago fees, invoices, adjustments | Platform | Lago engine | Finance, cost queries | Day-1+ |
| Billing catalog — plans, metrics (static) | Git (fleet values) → Lago API | Tenant declares, Platform reconciles | Catalog reconciler (kube-sbt, injected) | Lago, agent (metric codes + versions) | Day-1+ |
| Subscription intent + effective dates (static) | Git → Lago API (creates the runtime subscription) + `metering-boundaries` ConfigMap | Tenant/platform declares, Platform reconciles (fail-closed, lead-time enforced) | Catalog reconciler | Lago (runtime subscription), `metering-agent` (window splits) | Day-1+ |
| Subscription state (dynamic) | Lago runtime — governed REST lifecycle (ADR-012's dynamic class; **not Git, never Git-as-SoR**) | Platform | Lago / governed subscription lifecycle | Lago billing, catalog reconciler | Day-1+ |
| Consumer checkpoint (position) | `metering_consumer.metering_ingest_checkpoint` — **consumer state, not a financial record** | Platform | `metering-ingest` | itself (crash recovery) | Day-1+ |
| Metering events in flight | `s3://metering-events/<box>/inbox/` (SIOE lifecycle) after the handoff; pre-handoff: WAL (spoke PVC) | Platform (metering lane) | vector (PUT) / `metering-ingest` (drain) | `metering-ingest` | Day-1+ |
| Agent observation state | WAL replay (spoke PVC), derived — never separately persisted | Platform | `metering-agent` | lifecycle recovery (Decision 8) | Day-1+ |
| Identity and lifecycle facts | Kubernetes API | Platform (admission) | `metering-agent` (WAL) | Usage record enrichment | Day-1+ |
| Quantities | Metrics store (15d, evidence) | Platform (observability lane) | — | `metering-agent` (coverage-gated reads) | Day-1+ |

## Consequences

### Positive

- The record's semantics — identity, interval, unit, idempotency, gaps —
  are carried by an engine built for billing, mapped row by row (Decision
  3) instead of asserted for a schema we would have to invent.
- The OpenMeter objection is stood on its head: the heavy footprint that
  withdrew the last candidate is *excluded by decision*, and the engine
  reuses the database and cache every hub already runs.
- Source-side publishing fixes the lifecycle problem structurally, and the
  single WAL gives one atomic durability boundary with a fully stated
  commit invariant and deterministic identity — replay from any crash
  converges.
- The handoff is **egress-only and bus-free**: no inbound endpoint on any
  cluster, no NATS (ADR-080 untouched), retries bought from a shipper, and
  **neither side depends on the other** — spokes depend on the store, the
  hub depends on the store — so hub downtime cannot backpressure spokes.
- The consumer is crash-safe for money: durable checkpoint, re-read-free
  idempotency, retention ≥ SIOE + margin, RPO 0 within the SIOE.
- `unbilled, never estimated` has an enforcement mechanism (publish nothing
  for unobserved time), a recorded form (gap markers), and a fail-closed
  reconciler invariant — plus a raw-sample extraction rule that bans
  extrapolation by construction.
- One system of record per resource (ADR-039), one authority per dimension
  (Decision 10), versioned accounting semantics (Decision 12), and
  ADR-012's GitOps catalog properties survive intact.

### Negative

- **A new component enters the fleet** (Vector sidecar) plus new
  externally managed stateful infrastructure resources (bucket,
  lifecycle, policies, keys) — with declarative provisioning and drift
  detection (ADR-014's model, per
  Decision 13's object-store prerequisite); runbooks remain for financial
  restore drills and operational procedures, not for creating buckets —
  all gated with the `metering`
  capability, never baseline
  (ADR-070's budget), with ADR-069 answerability.
- **Two durable stages to reason about** — WAL and bucket — plus Vector's
  in-memory hop and
  the consumer checkpoint; reconciled by the deterministic-id chain but
  real for whoever debugs delivery at 3 a.m.
- **The platform owns a small custom durability protocol** (WAL framing,
  fsync boundary, rotation + sealing): explicitly specified, deliberately
  minimal, and honestly claimed — the shipper removes the transport loop,
  not the local commit.
- **Provider couplings to verify**: Hetzner's S3 subset (no
  notifications), default project-wide keys, watermark inputs
  (clock skew `S`, bounded request lifetime `T`) — all
  in the blocking suites, all with stated fallbacks.
- **Five more containers on every hub** (Lago light profile) and an AGPLv3
  component with upstream's release cadence; upstream's own singletons
  mean ingest HA does not make the engine HA.
- **The platform owns billing correctness end to end**: engine behaviours
  are gates, not reads; `metering` stays `planned` until the suites pass.
- **Metering records leave the spoke** where telemetry must not
  (ADR-083 §6): a boundary statement — billing records for a box-level
  engine, the same cross-cluster record egress class as ADR-014's backup
  archives, not telemetry.
- **Hybrid boxes stay unmetered in v1** (storage semantics, ADR-046
  placement); **invoices are box-local** (Decision 2) — a product
  invariant, not a bug.
- The residual of Decision 8 (agent down beyond store retention *and* API
  forgetting) is unbilled time. Stated, alerted, accepted.

## Impact

**Status lines added with this ADR:**

- **ADR-012** — **amended**: the static/dynamic split is preserved
  (meters/features/plans GitOps catalog; subscription state runtime);
  Git may declare subscription intent and effective dates; reconciler
  target and backend move to the Lago API (Decision 12).
- **ADR-052** — the Burst Compute Usage ownership row's System of Record
  moves from OpenMeter to the usage record (Lago events), and the metering
  sentence now reads: quantities derive from container usage series
  collected under this ADR's collection floor and recorded as events.
  *Both edits are applied to ADR-052's body with this ADR.*
- **ADR-066** — add.3's replacement sentence ("the PostgreSQL
  implementation already written at `providers/metering`") is replaced by
  this design; the capability stays `declared`/`planned` until the floor,
  agent, shipper, consumer and engine ship; "an ADR for it" is answered.
- **ADR-078** — add.1 §6's tenant exclusion gains the metering carve-out
  (accounting reads of in-cluster container series); nothing else changes.
  The lane boundary also works in this direction: no metering transport is
  added to Alloy (Alternatives).
- **ADR-039 / ADR-043** — the OpenMeter rows are corrected in place
  (telemetry SoR never included OpenMeter; Billing Catalog SoR is now Lago
  API). *Applied with this ADR.*

**Registry:** `manifests/architecture/components.yaml` — this ADR joins the
`declared_by` of `metering` and `observability`, and the metering note
names the WAL + Vector + object-store + Lago design instead of the
withdrawn PostgreSQL provider.

**Code and manifests (this ADR decides, does not implement):**

- `metering-agent` and `metering-ingest` modules with composition-root
  construction (no global registry or singleton); the OpenMeter-shaped
  `IMetering` / `IBilling` redesigned around Lago's model; the broken
  `providers/metering`, `providers/billing` packages repaired or deleted —
  `go build ./...` returns to CI as a gate. The agent implements
  **window splitting at declared `metering-boundaries`**
  (Decision 3's one-window-one-snapshot invariant), the
  **step-function
  byte-seconds integration** (Decision 7), and `observed_at` stamping
  (Decision 3).
- The agent manifest (StatefulSet, two containers, `volumeClaimTemplate`
  PVC with `whenDeleted`/`whenScaled: Retain`, WAL + Vector state paths,
  storage class), the WAL implementation (framing,
  fsync commit, rotation + seal, deterministic id), the Vector config
  (file source globbed to sealed `wal.*` only — active file excluded,
  in-memory buffer with explicit retry cap, `s3` sink,
  SigV4 credentials),
  `metering-ingest` (poll loop + watermark, durable checkpoint +
  processed-object index, fenced lease,
  poison isolation,
  replicas: 2 active/standby + Service), the `metering-events` bucket +
  financial-documents bucket (declarative provisioning, lifecycle +
  policy + NetworkPolicy), `metering_consumer` schema, Lago light
  manifests on platform-db (migration step, keys), spoke collection floor,
  KSM carve-out, the reserved fail-closed Namespace ABI rule **including
  update-immutability of price-affecting labels**, catalog
  reconciler **+ `metering-boundaries` ConfigMap rendering and GitOps
  delivery** +
  fleet-values templates, `metering-*` alert rules, and Vector metrics
  scraped by Alloy on the pod's localhost.

This ADR stays **Proposed** until the `metering` capability is `shipped`
— a decision may not rest on components nothing deploys (ADR-066's
discipline) — and the Decision 13 blocking suites are its other gate.

```architecture
capabilities:
  - metering
  - observability
```

## References

- `reference-projects/lago/` — vendored engine: `connectors/README.md`
  (events contract), `deploy/docker-compose.light.yml` (footprint),
  `events-processor/README.md` (the excluded heavy path),
  `docs/architecture.md` (engine internals); the `api/` submodule must be
  initialised for the Decision 13 engine suite
- `reference-projects/litellm/litellm/integrations/lago.py` — the
  same-fate, plain-HTTP ingestion pattern this design extends
- Vector file source (rotation-aware checkpoints, `remove_after_secs`),
  S3 sink (end-to-end acknowledgements, bounded retry), guarantees —
  https://vector.dev/docs/reference/sources/file/,
  https://vector.dev/docs/reference/sinks/s3/,
  https://vector.dev/docs/architecture/guarantees/
- Hetzner Object Storage docs — S3-compatible subset and unsupported
  actions (no notifications): https://docs.hetzner.com/storage/object-storage/supported-actions/;
  default project-wide key access and bucket policies: https://docs.hetzner.com/storage/object-storage/faq/buckets-objects/;
  durability model: https://docs.hetzner.com/storage/object-storage/overview/
- Conduktor, "Outbox Pattern for Reliable Event Publishing" — the pattern
  Decision 4/5 implement
- ADR-012, ADR-014, ADR-066, ADR-078, ADR-080 — the decisions this one
  amends, borrows its object store and credential pattern from, or leaves
  standing
- OpenTelemetry Metrics Data Model — why cumulative streams need an
  explicit completeness rule (resets, gaps, interval conversion)
  (https://opentelemetry.io/docs/specs/otel/metrics/data-model/)
- Prometheus query functions — `rate()`/`increase()` extrapolation, the
  reason they are banned from accounting (https://prometheus.io/docs/prometheus/latest/querying/functions/)
- Kubernetes API Concepts — watch semantics are not a historical store
  (https://kubernetes.io/docs/reference/using-api/api-concepts/)
- Object Names and IDs — UID is the identity, the name is not
  (https://kubernetes.io/docs/concepts/overview/working-with-objects/names/)
