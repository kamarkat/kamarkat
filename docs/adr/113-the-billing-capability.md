# ADR-113: The Billing Capability

**Date:** 2026-11-03
**Status:** Accepted — the capability is not offered to any tenant until the isolation proof in §7 passes against a deployed box, billing-db's recovery is qualified, and the engine's encryption keys are escrowed (§5).

## Context

A product that sells plans needs subscriptions, invoices and the schedule that raises them. The platform's Credit Authority (ADR-109) holds credits and nothing else; ADR-109 §1 and ADR-098's amendment of 2026-10-27 left monetary billing to the product layer and its external providers. A product that bills through a self-hosted engine then has to run that engine itself, on a box whose platform already runs databases, gateways and identity for every other stateful service. Running it as a platform capability gives it the same database, backup, gateway and caller model as the Credit Authority.

The facts this decision rests on:

- **Lago OSS v1.55.0 is AGPLv3, run unmodified.** Verified at the `v1.55.0` source:
  - on an empty database, `db:migrate` loads `db/structure.sql` (Rails `schema_format :sql`), which drops and recreates its extensions, among them `pg_partman` (line 1551). `pg_partman` is not a trusted extension, so only a superuser may create or drop it;
  - the API is a Rails server whose start script binds `::` (`scripts/start.api.sh`); `rails s -b` sets the address;
  - the worker is Sidekiq on Redis; the clock (`clock.rb`) is the billing schedule, of which exactly one may run;
  - `scripts/migrate.sh` with `LAGO_CREATE_ORG=true` seeds one organisation with the API key it is given;
  - the API authenticates an organisation by a bearer API key; it has no per-caller or per-operation permission;
  - invoice documents are links at `LAGO_API_URL` to Active Storage paths, signed and expiring;
  - webhooks are signed RS256 with the RSA key it is given (`LAGO_RSA_PRIVATE_KEY`, base64 of a PKCS#1 PEM), and the API serves the public key;
  - three keys encrypt its records (`LAGO_ENCRYPTION_*`).
- **The image runs as `nonroot` (65532)** on a Wolfi base, with bash.
- **AgentGateway v1.6.0** (ADR-109's facts) strips a validated JWT from the request and can present a configured key to the backend instead (`backendAuth.key`, `http/auth/mod.rs:81`), as `Authorization: Bearer` by default (`:372`).
- **CloudNativePG 1.29 on Kubernetes 1.31** cannot add an extension to a cluster without an image that ships it. Image-volume extensions need PostgreSQL 18 and Kubernetes image volumes (alpha and off by default in 1.31), and CloudNativePG's extension catalogue has no pg_partman.
- **The engine calls `partman.create_parent` with pg_partman 5 arguments** (`db/migrate/20260109132143_partition_enriched_events.rb`) and never runs pg_partman's maintenance. It names no supported PostgreSQL range: its compose file runs 15 (`getlago/postgres-partman:15.0-alpine`), its single-image build 17 (`docker/Dockerfile`).
- **PGDG packages pg_partman 5.5.0 for PostgreSQL 18** on bookworm, and the spoke clusters already run PostgreSQL 18.

## Decision

### 1. Scope

The platform offers a billing capability: a billing engine that records plans, subscriptions and invoices for one application of the box. It collects no payment, holds no credits and sets no price. Payment collection stays with the product and its payment provider; credits stay with the Credit Authority. This amends ADR-109 §1 ("no monetary billing") and ADR-098's amendment: the billing engine is a platform capability; the commercial interpretation of usage remains the product's.

### 2. The engine

Lago OSS v1.55.0, unmodified, pinned by digest (`hub-core-services/billing-engine`). Selectable, off by default, turned on by `capabilities.billing.enabled`, run on the hub in `platform-billing`.

One pod, in the order Kubernetes starts it: Redis as a native sidecar bound to `127.0.0.1` with its append-only file on the engine's volume; the migration and the organisation's seeding as a one-shot; the API bound to `127.0.0.1`, the worker and the clock; and the gateway, the pod's only port. One replica, `Recreate`: two clocks would bill twice. The document renderer is its own Deployment that only the engine reaches.

### 3. Its database

Its own CloudNativePG cluster (ADR-112), `billing-db` in `platform-data`, boundary 02, ungated, like `credits-db`:

- **Image.** PostgreSQL 18 with `pg_partman` 5.5.0 added (`images/postgres-partman`), both pinned, verified and pinned by digest by its workflow. It is replaced by an image-volume extension once the boxes run Kubernetes 1.33. 18 because image-volume extensions require it, so that replacement needs no major upgrade; the engine does not ship 18 itself, so its first deployment is what proves it there.
- **Superuser owner.** The `lago` database's owner, `hub_billing`, is made superuser once by `postInitSQL`, because the engine's schema drops and creates `pg_partman`. The cluster holds that database and nothing else, so the role's reach is this cluster.
- **Credential.** The owner's password is generated by hub-operator on every box (`hub-billing-db-*`) and given to `initdb`. It is not a HubEnvironment role: RoleManager grants DML on a database it does not own.
- **Records.** Retained when the capability is turned off, and kept for 90 days, as the ledger's are.

### 4. The one caller

The billing engine is a platform capability with one caller, exactly as the Credit Authority is (ADR-109 §4). hub-operator runs both from one controller (`platform_capability.go`).

- An application consumes it by declaring `capabilities.billing.enabled`. That declaration is the single source of its caller and its audience; the earliest declaration owns it, and a later one is refused with the `BillingEngineOwner` condition.
- While the capability is enabled, hub-operator provisions its project (`billing`), the audience the gateway requires. It publishes the owner's service client and organisation as the gateway's one caller, and revokes them when the declaration or the capability goes.
- The gateway requires a strict JWT from that client, of that organisation, as an access token. It then removes the token and presents the organisation's API key to the engine (`backendAuth.key`). No application ever holds the engine's key.
- The caller has the organisation's whole API (`/api/v1/`), because it is the organisation's owner. Documents are reachable without a token only as GET of the signed storage paths the engine issues.
- The application reaches the engine at `billing.<hubDomain>`, as `PROVIDER_BILLING_BASE_URL` in its `<app>-billing` ConfigMap. It registers its own webhook through the API: the engine's notifications go to the application's public address, signed with the engine's key.

### 5. Keys

hub-operator generates the engine's keys once, when the capability is first enabled, into the hub's secret store: the key base, the three encryption keys, the RSA signing key, and the organisation's API key and admin password. It never rotates them.

- **Rotatable without loss:** the key base (sessions), the RSA key (its caller re-reads the public key) and the API key (re-seeded).
- **Not rotatable:** the three encryption keys. Losing them makes the engine's encrypted records unreadable after a restore. They must join the escrow registry (ADR-076) before the capability is offered; until then a box that loses its secret store loses them.

### 6. Availability and lifecycle

Off by default. Turning it off removes the engine, its route and its caller, and keeps the database, the keys and the engine's volume. A fleet that declares the capability on a box without it fails to render.

### 7. The isolation proof

`scripts/validate/preflight/113-billing-engine-isolation.sh` checks the rendered shape:

- B1–B6: the pod, the routes, the caller binding and the chart's refusals;
- B7: exposure;
- B8: the prerequisites;
- B9: enablement.

The live half, against a deployed box, follows ADR-109 §7's method and is required before the capability is offered:

- a token of any other client, organisation or kind is refused;
- the API and the queue are unreachable except through the gateway;
- the engine never sees the caller's token.

### Alternatives considered

- **The product runs the engine itself, in its own namespace.** That gives the product a superuser PostgreSQL, a Redis and a public route outside the platform's backup, gateway and identity model, on every box it runs on. It also puts the engine's key in the product.
- **The engine's own database image (`getlago/postgres-partman`) in a plain Deployment.** That bypasses CloudNativePG's backup and recovery (ADR-014, ADR-111).
- **Distribute the engine's API key to the caller.** That removes the gateway's binding of one caller and puts a standing, unrotated key in the product.

## Ownership

| Resource | System of record | Lifecycle owner | Consumer |
|---|---|---|---|
| Engine, renderer, queue, gateway | `hub-core-services/billing-engine` | Platform (`capabilities.billing`) | The declaring application |
| `billing-db`, `hub_billing`, its credential | `billing-engine/database`, secret mappings | Platform, every box | The engine |
| Engine keys | Hub secret store | hub-operator, on first enablement | The engine; its gateway (API key) |
| Caller, audience | Hub secret store | hub-operator, from the declaration | The gateway; the owner's service identity |
| Plans, subscriptions, invoices | The engine | The declaring application | The declaring application |

## Consequences

### Positive

- Billing gets the platform's database, backup, gateway and identity model, and the product holds no engine credential.
- The second capability with one caller shares the first one's controller, so the two cannot drift.

### Negative

- A PostgreSQL image the platform builds, until image-volume extensions are available.
- A superuser application role, contained to the engine's own cluster.
- An AGPLv3 service in the box, run unmodified.
- A 256Mi PostgreSQL on every box, used or not, as `credits-db` is.
- The encryption keys need escrow before the capability is offered (§5).

## References

- ADR-014, ADR-076, ADR-098 (amendment 2026-10-27), ADR-109, ADR-111, ADR-112
- waypoint ADR-058: the product's billing coordination, the first declaring application
