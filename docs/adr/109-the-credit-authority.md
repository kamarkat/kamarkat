# ADR-109: The Credit Authority

**Date:** 2026-10-26
**Status:** Accepted — the capability is not offered to any tenant until the isolation proof in Decision §7 passes against a deployed box.

## Context

A product built on the platform may charge for consumption in prepaid credits: a customer's credits are reserved before work starts, captured or released when it ends, and must never be spent twice under concurrency. That needs an authoritative balance with transactional guarantees. Pricing, the commercial lifecycle around the credits, and the money that buys them are the product's.

The facts this decision rests on:

- **No credit ledger, billing or payment capability is deployed** in any repository. `metering` is a declared capability with `status: planned` (`manifests/architecture/components.yaml`).
- **Formance Ledger v2.4.16 is MIT-licensed and runs on PostgreSQL**, tested against PostgreSQL 16, the version the platform's CloudNativePG clusters run. Verified at the `v2.4.16` source:
  - ordered sources drain accounts in sequence;
  - a transaction whose sources cannot cover it fails with no postings;
  - an account other than the issuance account goes negative only when the request opts in: `force`, as a query parameter or a body field, turns a posting transaction's balance check into an unbounded overdraft (`internal/api/v2/controllers_transactions_create.go:42`, `internal/api/bulking/elements.go:125`), and a Numscript source may declare its own overdraft;
  - the request body is decoded by Go's `encoding/json`, which matches field names case-insensitively;
  - several transfers commit atomically in one transaction;
  - idempotency keys are unique per ledger, and a reused key with different input is refused;
  - balances are locked per account in a fixed order, with deadlock retry;
  - failed transactions are rolled back without storing their key;
  - transactions carry metadata, and history is queryable by account and metadata;
  - a ledger lives in a bucket, and each bucket is its own PostgreSQL schema (`internal/storage/ledger/store.go:161`).
- **Formance Ledger v2.4.16 authenticates and does not authorise.** One JWT middleware guards every route (`internal/api/v2/routes.go`), including bucket deletion and restore, exporter management and ledger creation. No per-ledger or per-operation permission exists.
- **Formance Ledger v2.4.16 routes the raw request path and listens on a configurable address.** No path-cleaning middleware is mounted (`internal/api`, `cmd`), and `--bind` sets the listen address (`cmd/serve.go:208`). Besides the ledger-scoped routes, the server answers system routes (`/_`), ledger listing and creation, ledger metadata, bulk, logs import and export, schemas, pipelines, queries, transaction revert and metadata writes, and the v1 API.
- **AgentGateway v1.6.0 is the tenant gateway already deployed** (`universal-tenant` chart). Verified at the `v1.6.0` source:
  - JWT authentication has a strict mode that refuses a request without a valid token (`http/jwt.rs:582`);
  - authorisation rules are CEL expressions over the request and the validated JWT claims, in `allow`, `require` and `deny` modes. Every `require` must hold; when `allow` rules exist and none matches, the request is denied (`http/authorization.rs:254`);
  - an expression that fails to evaluate counts as false (`cel/types.rs:796`), so a failing `require` or `allow` denies;
  - `request.path` is the raw URI path, without the query and without decoding (`cel/types.rs:1157`); the query is in `request.uri` and `request.pathAndQuery`;
  - `json()` parses a request body, and a rule that reads the body buffers it (`celx/src/general.rs:22`, `cel/mod.rs:357`).
- **Ledger v3 is a release candidate** on embedded storage, with no licence file.
- **A product's coordinating service calls the platform as itself,** with client credentials (ADR-097).

## Decision

### 1. Scope

The platform offers a Credit Authority: an authoritative ledger of credits with transactional guarantees, isolated per tenant. It offers no monetary billing, payment collection or pricing capability. Those belong to the product layer that coordinates credits, and to the external providers it integrates.

*Amended by ADR-113 (2026-11-03):* the platform also offers a billing capability, a billing engine with one caller run as this capability is. Payment collection and pricing remain the product layer's.

### 2. The Credit Authority

- **Formance Ledger v2.4.16**, on a database the platform provisions (ADR-099). Ledger v3 is not eligible.
- **It holds credits, never money.** Every movement is a balanced transaction between credit accounts.
- **It executes authorised movements and determines no price.** Which movement is posted, for what amount and why is the coordinating product layer's decision.
- **It is independent of every billing and payment provider.** Several product-defined credit models can share its balances, reservations and settlement guarantees without coupling their pricing to a commercial provider. Replacing a billing or payment provider migrates no balance, reservation or credit history.

### 3. Guarantees

The platform guarantees, for every tenant's ledger:

- **Atomicity.** A transaction's postings commit together or not at all.
- **No overdraft.** No account but the issuance account goes negative. A transaction its sources cannot cover is refused whole. This holds because the gateway admits no request that opts out of the balance check (§4).
- **Idempotency.** A transaction carries a caller-supplied key, unique per ledger. A repeat returns the original; a repeat with different input is refused; a failed transaction stores no key, so it can be retried.
- **Serialisable balances.** Concurrent transactions on one account are ordered by its lock, so concurrent reservations never exceed a balance.
- **Traceability.** Every transaction keeps its metadata and is queryable by account and metadata.
- **Durability.** Committed transactions survive the loss of a node, and the ledger's database is backed up and restorable.

These guarantees hold however often the coordinating layer repeats a call. Financial integrity never depends on any caller executing exactly once.

### 4. Tenant isolation

**One Credit Authority per box, as a selectable platform capability run on the hub.** A box belongs to one tenant, so this is one per tenant. It runs where the box's other platform services run, in `platform-credits` on the hub, and is turned on by `capabilities.credits.enabled`. It is a capability rather than machinery because not every box needs it: a box run as an internal platform bills nobody (README, "What the platform is"). It is not an application: it has no gateway hostname of a tenant's, sign-in identity, browser client, owner or OAuth clients.

**What exists on every box, and what runs only when enabled:**

| | Credits disabled | Credits enabled |
|---|---|---|
| `credits` database on `platform-db` (owner `hub_credits`, `databaseReclaimPolicy: retain`) | Present | Present |
| `hub_credits` role and its generated credential | Present | Present |
| `platform-credits` namespace | Present | Present |
| The capability's Zitadel project | Absent | Present |
| Ledger, gateway sidecar, provisioner | Absent | Running |
| `credits.<hubDomain>` listener and route | Absent | Present |
| The admitted caller | Absent | The declaring application's, or none |

The database, role and credential are prerequisites on every box, delivered with the other hub databases, because the hub's role manager is not capability-aware. It reads each declared role's credential and stops at the first one missing, so a role present only while the capability ran would block every hub role on a box with it off. The cost is one dormant database, role and credential per box.

**Provisioned by the platform's existing hub mechanisms:**
- **Database:** a CNPG `Database` on `platform-db`, its role in `HubEnvironment`, and its password generated by the hub-operator and delivered by ESO, exactly as Zitadel's are.
- **Service:** an entry in the platform-services ApplicationSet under `capabilities.credits.enabled`, inline as ADR-066 requires for selectable capabilities.
- **Identity:** while enabled, the hub-operator asks the tenant identity service for a `credits` project in the platform's own organisation. That is the same organisation and path as the platform's own clients. The issuer puts any requested project into a token's audience whatever the client's organisation, so a tenant application's service client can carry it.
- **Exposure:** a listener and an HTTPRoute on the hub Gateway at `credits.<hubDomain>`, like every hub service's hostname. Fleets receive the URL from the environment while the capability is enabled.

**One declaration authorises the caller.** One application of the tenant declares, in its own fleet values:

```yaml
capabilities:
  credits:
    enabled: true
```

That declaration is the single source of who may call:
- **identity:** the platform publishes the declaring application's service client (ADR-097), and that client's organisation, as the one caller the gateway admits;
- **audience:** the capability's project joins the declaring application's backend audiences (ADR-094);
- **endpoint:** the declaring application receives `CREDITS_URL`. A fleet declaring the capability on a box that does not run it fails to render.

**Exactly one owner per box.** The earliest declaration owns the capability. A second application declaring it is refused by the platform and recorded on its status (`CreditAuthorityOwner=False`, reason `Conflict`). It receives no caller binding and no audience.

**Turning it off removes the service, never the records.** Disabling `capabilities.credits.enabled` removes the ledger, its gateway and the route, and the platform revokes the published caller. The database and its records stay, and so do the role and credential. Withdrawing the declaration revokes the caller in the same way. With no caller published the gateway admits nobody: its caller Secret is deleted, and a gateway without it does not start. Financial history is removed only by an explicit operator action, never by a reconcile.

It holds one bucket and one ledger.

**The ledger is created by the platform, in the pod, once.** The pod starts in a fixed order:
1. the ledger, as a native sidecar, bound to the pod's loopback for the pod's life;
2. a one-shot provisioner, which waits for the ledger's health check, creates the tenant's ledger in its bucket over loopback, and exits. An existing ledger is accepted only when it is the expected one, by name and bucket. It never deletes, resets or recreates, and any other answer fails it;
3. the gateway, which starts only after provisioning succeeds, so the Credit Authority is unavailable rather than half-provisioned.

The ledger runs without authentication of its own: only containers of its pod can reach its loopback.

**The gateway is the only way in.** The ledger runs in one pod with an AgentGateway sidecar:
- the ledger listens on the pod's loopback address only, so nothing outside the pod reaches it except through the sidecar;
- the sidecar's listener is the pod's only exposed port;
- network policy admits only the hub Gateway's identity to that port. Nothing in any namespace, the capability's own included, reaches the pod except through `credits.<hubDomain>`. The caller is on a spoke, so the network cannot name it; the credential decides who is calling (ADR-097 invariant 10).

**The sidecar authenticates and authorises every request, declaratively:**
- **Authentication, strict.** A request without a valid token from the platform's issuer is refused.
- **Caller binding**, as `require` rules:
  - the token's client identity is the one client the platform published from the declaration: the declaring application's machine client (ADR-097). Its tokens carry no person, so no user's token can match (ADR-110);
  - the token's organisation claim is that client's organisation;
  - the token is an access token, never an ID token (ADR-095).
- **No opt-out of the balance check**, as `require` rules on creating a transaction:
  - the body's top-level fields are exactly drawn from `postings`, `metadata` and `reference`, matched case-sensitively, so `force` in any spelling, a Numscript `script`, and a back-dated `timestamp` are refused;
  - the request carries no query string, so the `force` query parameter is refused;
  - a body that does not parse fails its rule, and is refused.

  Credit movements are therefore posting transactions. An ordered draw across several accounts is computed by the coordinating layer; a concurrent spend makes the transaction fail whole for insufficient funds, never overdraw.
- **Operation allowlist**, as `allow` rules over the method and the raw path, each anchored at both ends, with the tenant's ledger name fixed and path segments restricted to letters, digits, `_`, `-` and, for account addresses, `:`. A path containing `.`, `%` or an empty segment therefore never matches. Only these are allowed:
  - create a transaction; list transactions; read one transaction;
  - list accounts; read one account;
  - read aggregated balances; read volumes.
- **Everything else is denied** because it matches no `allow` rule, including:
  - every system route: bucket deletion and restore, exporter management, `/_info`;
  - listing and creating ledgers, and ledger metadata;
  - bulk, logs import and export, schemas, pipelines and queries;
  - transaction revert, transaction and account metadata writes;
  - the whole v1 API.
- **Release is never a revert** (it is a new transaction), so revert is not needed and stays closed.

The sidecar bounds which operations a caller can perform. Which credit movements are correct is the coordinating layer's decision (§6).

**The Credit Authority API** is the ledger's own API, as narrowed by this allowlist. No custom API service is built in front of it unless a required contract cannot be met this way. The platform offers credit primitives only; it offers no general billing API, and no billing or payment provider is integrated in the platform.

**Applications never reach the ledger.** Consumer applications reach credits only through the coordinating product layer.

**The gateway's own routes.** `/health` answers liveness; `/health/ready` is the ledger's health check and nothing else of it. Every other path matches no route.

**Port-forward and exec bypass the gateway.** Either reaches the ledger's loopback. Nothing in a Credit Authority's namespace grants `pods/exec` or `pods/portforward` to anyone but platform operators, and the live check verifies it.

### 5. Availability and lifecycle

- The platform owns the ledger's deployment, database, backup, restore, version and schema migrations.
- While the ledger is unavailable, the coordinating layer authorises no credit movement. After an outage, an operation whose outcome is unknown is resolved by its idempotency key or by reading its transaction.
- A version change preserves every transaction, balance and idempotency key.
- **Recovery is an activation requirement.** The ledger's database is on `platform-db`. Restoring a database to an earlier point can lose committed transactions, and a lost balance or idempotency record can let a completed credit movement execute again. Credit movements are enabled only once a financially safe recovery procedure for `platform-db` is established, including reconciliation of the ledger against the product layer's records before operations resume. `databaseReclaimPolicy: retain` protects against deletion, not against a restore.

### 6. The boundary with the coordinating product layer

| Concern | Owner |
|---|---|
| Ledger deployment, database, backup, restore and upgrades | Platform |
| Tenant isolation, ledger access and ledger credentials | Platform |
| Atomicity, overdraft prevention, idempotency and serialisable balances | Platform, through the ledger |
| Pricing models, prices and credit policies | Product layer, for its applications |
| Which movements are posted, and their idempotency keys | Product layer |
| Orchestration of grants, reservations, settlement, expiry and recovery | Product layer |
| Funding accounts, memberships and authorisation of spenders | Product layer |
| Monetary billing and payment collection | External providers, integrated by the product layer |
| Reconciliation of provider facts with credit movements | Product layer |

### 7. The isolation proof

Isolation is proven, not inferred from configuration. Two checks, both named for this ADR:

**At render time** (`scripts/validate/preflight/109-credit-authority-isolation.sh`), on the rendered manifests:
- the ledger is a native sidecar on loopback, provisioning is one-shot before the gateway, and the chart refuses to render without the issuer, with an unpinned image, or with a ledger name its rules cannot be built from;
- ingress to the pod is the hub Gateway's identity only; the route and its listener exist only while enabled, admit only the capability's namespace, and reach only the gateway; the caller's Secret is revocable;
- the prerequisites are on every box: the database (owned, retained), role, credential, namespace and database access;
- the service, the route and the fleets' URL follow `capabilities.credits.enabled`, and a declaration without it does not render;
- the ledger binds the loopback address, and the sidecar's port is the pod's only exposed port;
- JWT authentication is strict, and the caller binding is expressed as `require` rules;
- the authorisation uses `allow` and `require` rules only, never `deny`, since a failing `deny` expression admits;
- every `allow` rule is anchored, names the tenant's ledger, and restricts its path characters as §4 states;
- transaction creation requires the exact body-field allow-list and no query string;
- network policy selects the ledger pod.

**Against a deployed box** (`scripts/validate/cluster/109-credit-authority-isolation.sh`, running `test/e2e/credit-authority`), through the gateway, with each outcome asserted:

| Request | Expected |
|---|---|
| No token; an ID token; an expired token | Refused |
| Another application's machine token; a token with another organisation's claim | Refused |
| Each system route, ledger listing and creation, ledger metadata | Refused |
| Bulk, logs, schemas, pipelines, queries, revert, metadata writes, the v1 API | Refused |
| Dot-segment, percent-encoded and double-slash variants of an allowed path reaching another route | Refused |
| Another ledger's name in an allowed path | Refused |
| Creating a transaction with `force` (query, or a body field in any letter case), a Numscript `script`, a `timestamp`, or an unparseable body | Refused |
| Postings whose source cannot cover them | Refused for insufficient funds, with no postings |
| A connection to the ledger's port on the pod's address, from another hub namespace | No connection |
| A connection to the gateway's port on the pod's address, not through the hub Gateway | No connection |
| `credits.<hubDomain>` from the declaring application's SDK on its spoke | Answers through the Gateway |
| Each allowed operation with the coordinating service's token | Succeeds |
| A grant of `pods/exec` or `pods/portforward` in the namespace | None exists |
| Concurrent spends of one balance | Exactly as many commit as it covers; it never goes below zero |
| A repeated idempotency key; the same key with other input | The original transaction, nothing moved again; refused |

The deployed check also verifies that the admitted caller is the declaring application's own service client, so the gateway's binding traces to the one declaration.

A suite that skips for want of a target, or tokens it was not given, proves nothing. CI, deployment health and the infrastructure checks establish readiness only; they do not activate the capability.

**Activation evidence.** The deployed check writes a record of every run to `docs/evidence/109-credit-authority/`, passed or not: when and where it ran, the deployed image digests, the ledger, the admitted caller, and every test's verdict. Tokens are never written. The record is committed.

A tenant's Credit Authority is activated only on a record in which the guarantees suite (atomicity, concurrent spends, idempotency, and the refused opt-outs of the balance check) and the isolation suite both ran in full and passed. No credit movement for that tenant is enabled before it. A change to the ledger version, the gateway version or the allowlist requires a new record.

### Alternatives considered

- **A shared ledger deployment with one bucket per tenant.** The ledger has no per-ledger authorisation, so isolation between tenants would rest wholly on path rules in a shared gateway. One deployment per tenant makes another tenant's ledger unreachable rather than forbidden.
- **A custom API service in front of the ledger.** The deployed gateway expresses the caller binding and the allowlist declaratively, and fails closed.
- **Cilium L7 policy as the enforcement point.** It can restrict methods and paths, but not by the caller's token claims, and its source is not vendored to verify its matching at the deployed version.
- **TigerBeetle.** Native pending transfers with timeouts, but a new non-PostgreSQL stateful system, and no history queries for per-member usage. Retained as the alternative if the selected ledger becomes ineligible.
- **A custom PostgreSQL ledger.** It would reimplement double entry, overdraft prevention and idempotency that the selected ledger provides.
- **A billing provider's wallets or credits as the authority.** No synchronous reservation under concurrency, and balances would migrate with any change of provider.
- **Formance Ledger v3.** A release candidate on embedded storage, with no licence file.
- **Authorisation inside the ledger.** It has none to configure. Isolation is enforced by the gateway sidecar in front of it.

## Ownership

| Resource Class | System of Record | Lifecycle Owner | Reconciler | Consumer | Phase |
|---|---|---|---|---|---|
| Credit balances, reservations and transactions | Formance Ledger (the tenant's ledger) | Platform (capability); product layer (movements) | Formance Ledger | Coordinating product layer | Day-1+ |
| Whether the box runs the Credit Authority | Git (`capabilities.credits.enabled`, the box's values) | Box owner | ArgoCD | Platform | Day-1+ |
| The ledger deployment, bucket and ledger | Git (`hub-core-services/credit-authority`) | Platform | ArgoCD | Coordinating product layer | Day-1+ |
| The ledger's database, role and credential | CloudNativePG on `platform-db` (retained); Infisical | Platform | CloudNativePG operator, hub-operator, ESO | Formance Ledger | Day-1+ |
| The capability's Zitadel project | Zitadel (platform organisation) | Platform | hub-operator, kube-sbt | AgentGateway sidecar, the declaring application | Day-1+ |
| The capability declaration | Git (the declaring application's fleet values) | Tenant | ArgoCD, hub-operator | Platform | Day-1+ |
| Sidecar authentication, caller binding and operation allowlist; network policy and route | Git (`hub-core-services/credit-authority`, the hub gateway) | Platform | ArgoCD | AgentGateway sidecar, CNI, hub Gateway | Day-1+ |
| The admitted caller and its organisation | Infisical | Platform | hub-operator, ESO | AgentGateway sidecar | Day-1+ |
| Ledger caller credentials | Infisical | Platform | ESO | Coordinating product layer | Day-1+ |

## Consequences

### Positive

- Concurrent spending cannot exceed a balance, and double entry, overdraft prevention and idempotency are properties of an open-source ledger rather than platform or product code.
- Correctness does not depend on the coordinating layer running each step exactly once.
- Credit balances and history are independent of billing and payment providers, and survive a change of either.
- The ledger runs on the PostgreSQL the platform already operates, under MIT.

### Negative

- A stateful financial component joins the box, needing backup, restore and upgrade discipline.
- The ledger's lack of authorisation puts its protection on the sidecar's rules. A fault in them exposes the tenant's own system routes, which is why the isolation proof gates the capability and reruns on every change.
- One ledger deployment and database per tenant, rather than one shared.
- Credit movements are only as correct as the product layer's choice of movements and keys. The platform guarantees how a movement executes, not whether it should.

## Impact

- **`manifests/architecture/components.yaml`** — a `credits` capability is declared, `status: planned`, `declared_by` this ADR.
- **`manifests/hub-core-services/credit-authority`** — the ledger, its gateway sidecar and provisioner, the network policy admitting only the hub Gateway, and the ExternalSecrets for its database credential, project and caller. Images are pinned by digest by the platform.
- **`manifests/hub-core-services/database`** — the `credits` database on `platform-db` (owned by `hub_credits`, retained), the role's credential for the role manager, and `platform-db` admitting `platform-credits`.
- **`manifests/environments/base/hubenvironment.yaml`** and **hub-operator's application secret mappings** — the `hub_credits` role and its generated credential, on every box.
- **`manifests/hub-core-services/platform-namespaces`** — `platform-credits`.
- **`manifests/hub-core-services/gateway`** — the `credits` listener and route, rendered only while enabled.
- **environment-manager** — `capabilities.credits.enabled` (default off); the service's entry in the platform-services ApplicationSet under it; the gateway's flag; and `creditAuthorityUrl` for fleets while enabled.
- **AINativeSaaS XRD** — `spec.capabilities.credits.enabled`: an application's declaration that it consumes the capability. Its composition is unchanged.
- **hub-operator** — a controller acting on the capability's enablement (its own ConfigMap in `platform-credits`) and on every application change: the capability's project while enabled, and its one caller, revoked when disabled or undeclared. The application controller enforces one owner per box (`CreditAuthorityOwner`) and adds the capability's project to the owner's audiences.
- **kube-sbt** — `POST /internal/platform/projects`: a project in the platform's own organisation.
- **universal-tenant chart** — a declaring application's `CREDITS_URL`, and its service identity with the capability's project; a declaration on a box without the capability does not render.
- **The OpenMeter billing code is removed**, not left beside this capability:
  - hub-operator's `Meter`, `Plan` and `Feature` APIs: their controllers (registered in no manager), types, CRDs (installed on every hub with nothing reconciling them) and RBAC, and the bootstrap check that asserted the CRDs;
  - kube-sbt's OpenMeter metering and billing providers, the usage, catalog, subscription and invoice routes they served, the metering step of user creation, `IBilling`, `IMetering` and their models;
  - kube-sbt's `controlplane` and `systemadmin` packages, which did not compile and nothing imported.

  Plans and features are the product layer's (waypoint ADR-058); meter definitions are ADR-098's.
- **Tenant GitOps** — the coordinating application declares `capabilities.credits.enabled: true` in its own values. The box enables the capability in its own values.
- **CI** — `kube-sbt.yml` vets and tests kube-sbt; `hub-operator.yml` runs `make test` and fails on stale generated files; `pr-validate.yaml` compiles `test/e2e/credit-authority`.
- **Tests** — `test/e2e/credit-authority`: the guarantees and isolation suites the deployed proof runs.
- **ADR-098** — not changed by this ADR. Where infrastructure usage is recorded is decided separately.

## References

- ADR-003: ESO–Infisical Secret Management
- ADR-039: Platform Ownership Model
- ADR-040: Day-0 vs Day-1 Lifecycle Boundary
- ADR-043: Control Plane Authority Model
- ADR-066: The Platform Boundary
- ADR-094: One identity project per application, and audience is not authorisation
- ADR-095: Moving the API credential from the ID token to an access token
- ADR-097: An application calls another application as itself, with client credentials
- `reference-projects/agentgateway` (tag `v1.6.0`)
- ADR-099: The platform provisions a database, not a schema
- ADR-110: An application acting for its user carries the user's own access token (a user's token never reaches the Credit Authority)
- `reference-projects/ledger` (tag `v2.4.16`)
