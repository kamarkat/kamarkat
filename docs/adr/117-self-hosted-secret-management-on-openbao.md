# ADR-117: Self-Hosted Secret Management on OpenBao

**Date:** 2026-11-24

**Status:** Accepted

**Supersedes:** ADR-003 §2 (storage in Infisical); ADR-035's designation of Infisical as the intermediate PKI authority, and its offline-root custody requirement

**Amends:** ADR-003, ADR-023, ADR-030, ADR-031, ADR-032, ADR-035, ADR-039, ADR-040, ADR-076, ADR-078, ADR-079, ADR-092, ADR-111, ADR-112; Standing Decision 5 of `adr-template.md`

**Relates to:** ADR-011 (declarative state over imperative jobs), ADR-014 (platform-owned stateful infrastructure), ADR-043 (authority model), ADR-063/068 (bundle and release), ADR-100 (deprecated KMS model), ADR-115 (cluster acceptance)

**Design record:** `docs/plans/openbao-cnpg-secret-management-proposal.md` holds the dependency inventory, source citations at the pinned versions, and the behavioural acceptance scenarios.

## Architecture at a glance

```
 TENANT-CONTROLLED, OFF-BOX: Infisical Cloud, escrow only (Class E) ─────────────────────────────
 ┌─────────────────────────────────────────────┐  ┌─────────────────────────────────────────────┐
 │ BOX ESCROW PROJECT                          │  │ CUSTODY ESCROW PROJECT                      │
 │ members: administrator, automation identity │  │ members: administrator ONLY                 │
 │  seal-key generations + seal-config history │  │  root CA private key                        │
 │  recovery shares (PGP-encrypted, inert)     │  │  recovery PGP private key                   │
 │  recovery PGP public key                    │  │                                             │
 │  openbao-db archive credential              │  │ no runtime or CI identity can read it       │
 │  existing: kubeconfig, spoke PKI, SSE-C ... │  │                                             │
 └──────────┬──────────────────────────────────┘  └───────────────┬─────────────────────────────┘
            │ CLI delivers seal config (never ESO)                │ administrator session (kakt) signs
            │                                                     │ OpenBao's intermediate CSR; returns
            ▼                                                     ▼ the certificate only (root and keys
                                                                    never enter a cluster or CI)
 HUB: namespace platform-data ────────────────────────────────────────────────────────────────────
 ┌─────────────────────────────────────────────────────────────────────────────────────────────┐
 │ OpenBao 2.7.3   active/standby, standby reads off, replicas 1 / 2 / 3                        │
 │   seal: static (delivered config)      init: declarative self-init, root token revoked       │
 │   listener TLS ◀── cert-manager CA issuer; CA public cert distributed as a ConfigMap         │
 │   audit: native device to stdout, HMAC-hashed ──▶ Alloy ──▶ Loki (asynchronous, best effort)│
 │                                                                                              │
 │   kv/        secret values (System of Record)                                                │
 │   pki_int/   issuing intermediate: key generated inside OpenBao, CSR signed by the root      │
 │   database/  static roles: hub_<app> passwords; dynamic roles available, not adopted         │
 │   auth/kubernetes  hub identities          auth/spoke-<c>  spoke identities (JWT)            │
 │   auth/oidc        humans                  break-glass: recovery key (custody-held)          │
 └──────┬──────────────────────────────┬───────────────────────────────────────────────────────┘
        │ storage: direct, client      │ rotation: authenticates AS hub_<app> (certificate),
        │ cert, no pooler; OpenBao     │ maintenance database only
        ▼ owns its schema              ▼
 ┌──────────────────────────────┐  ┌──────────────────────────────────────────────────────────┐
 │ openbao-db   (CNPG)          │  │ zitadel-db  credits-db  grafana-db   (CNPG)              │
 │  instances 1 / 2 / 3         │  │  <app>_owner : owns the database; cannot log in          │
 │  sync: off / preferred /     │  │  hub_<app>   : exists via CNPG; password via OpenBao     │
 │        required              │  │ billing-db  : hub_billing password generated into KV,    │
 │  archive + PITR; backup      │  │               applied by CNPG (no rotation)              │
 │  before each version change  │  └──────────────────────────────────────────────────────────┘
 └──────────────────────────────┘
 ┌─────────────────────────────────────────────────────────────────────────────────────────────┐
 │ ESO (vault provider)          KV values, database/static-creds ──▶ Secret ──▶ Reloader       │
 │ cert-manager (Vault issuer)   pki_int/sign/<role> ──▶ leaves (subject, SAN, usage, TTL bound)│
 │ hub-operator                  desired policies, roles, mounts, static roles, KV generation,  │
 │                               durable recovery records                                       │
 └─────────────────────────────────────────────────────────────────────────────────────────────┘
                                         ▲  HTTPS via hub Gateway (public certificate; backend TLS
                                         │  validated against the listener CA and server name)
 SPOKE (each workload cluster) ──────────┴────────────────────────────────────────────────────────
 ┌─────────────────────────────────────────────────────────────────────────────────────────────┐
 │ ESO ClusterSecretStore      JWT on auth/spoke-<c>           → this cluster's platform paths   │
 │ tenant SecretStore per app  JWT, that namespace's SA        → this application's paths only   │
 │ cert-manager ClusterIssuer  JWT on auth/spoke-<c>           → this cluster's identities only  │
 │ Crossplane provider-sql     applies tenant DB passwords (generated into KV, delivered by ESO)│
 └─────────────────────────────────────────────────────────────────────────────────────────────┘

 Removed: self-hosted Infisical (+ its Redis grant), infisical-db, Infisical PKI issuer (hub + spokes),
          spoke-identity-operator, hub-operator Infisical client / uploaders / RoleManager /
          master-key escrow, CLI Infisical bootstrap
```

## Context

Self-hosted Infisical (v0.167.12) is the box's System of Record for secret material (ADR-003), the intermediate PKI authority (ADR-035), and the identity provider for ESO and the certificate issuers (ADR-031). Infisical Cloud, or an Infisical the tenant runs, is the external escrow (ADR-076).

The following facts are established by the ADRs named and by the source inspection in the design record:

- **Authorization cannot be scoped below the project in Infisical OSS.**
  - Built-in roles carry no environment or path condition, and conditional roles need a licensed feature (ADR-031 correction, 2026-11-23).
  - Isolation therefore needs one project and one identity per boundary.
  - A per-cluster identity with project-wide read on `hub-secrets` is copied into tenant namespaces. This is derived from source; runtime confirmation is pending.
- **One escrow credential serves both CI and the runtime.**
  - Each box has its own escrow project, created by `kakt escrow init` as the administrator. The organisation has one automation identity, which is added as `member` to each box's project (ADR-076 amendment 2026-11-10 §4a; `kakt escrow init`).
  - hub-operator and CI read and write that box's escrow artefacts with that identity.
  - Anything placed in a box's escrow project is therefore readable by that box's runtime and CI, and by any holder of the organisation's automation credential.
- **Database password ownership is split.**
  - `zitadel-db`, `credits-db` and `grafana-db` make their application role the CloudNativePG initdb owner.
  - CloudNativePG's instance controller re-applies that owner's password from its own Secret, while hub-operator's role manager applies a different, ESO-delivered value.
- **ADR-030 claims a zero-downtime rotation that is not built.** ADR-003 §5's dual-phase overlap does not exist; the implementation issues a single password change.
- **ADR-035 requires an offline root CA, but the Fleet CA is a self-signed root held inside Infisical** (ADR-032 amendment, 2026-08-31).
- **Spoke identity is custom-built.** Spoke identity lifecycle, client-secret delivery through ClusterResourceSet, and membership narrowing are custom code. Certificate issuance needs a third-party external issuer on every cluster.
- **No authoritative record of an unactivated initialization attempt exists.**
  - ADR-111 Revision 6 §6 serializes bootstrap runs; it does not record their outcome.
  - ADR-115's acceptance assessment is a point-in-time verdict, not a lifecycle record of a database incarnation.
- **Kamarkat has no production users.** A clean replacement needs no data migration (ADR-112 §5 precedent).

OpenBao (MPL-2.0) provides, without a licence:

- PostgreSQL storage with active/standby leader election;
- a static auto-unseal mechanism;
- declarative self-initialization, with an initialization lock that handles concurrent PostgreSQL writers;
- path-scoped ACL policies, resolved by name on every request;
- Kubernetes and JWT authentication;
- KV v2, PKI and a database secrets engine;
- audit devices with HMAC-hashed values.

GitLab Secrets Manager runs OpenBao on PostgreSQL with static unseal and self-initialization in production. That is precedent, not proof for Kamarkat's topology.

## Decision

### 1. OpenBao replaces self-hosted Infisical entirely

- **One store.** OpenBao v2.7.3, from Helm chart 0.30.8, is the box's only self-hosted secret store. No second self-hosted store, synchronization service, compatibility path or alias survives the replacement.
- **Escrow only.** Infisical Cloud is retained only as the external escrow. No workload, spoke or application depends on it for routine secret management.
- **Delivery is unchanged.** ESO remains the only delivery mechanism, Kubernetes Secrets remain a delivery cache, and PushSecret remains banned.
- **Credentials flow one way.** Every credential is generated into OpenBao or by OpenBao, and delivered outward. Nothing is published into OpenBao from a cluster.

### 2. Storage, placement and transport

**Placement.**
- OpenBao runs in `platform-data` (ADR-014), beside its own CloudNativePG cluster, `openbao-db` (ADR-112).
- Isolation from the namespace's other database clusters is enforced by purpose-specific ServiceAccounts, RBAC and pod-selected NetworkPolicies, never by the namespace boundary.

**Storage connection.**
- OpenBao connects directly to `openbao-db`'s read-write service, with no pooler. This is an exception to ADR-112 §9: OpenBao's connection count is bounded by its replicas rather than by the fleet, and a CloudNativePG pooler cannot carry client-certificate authentication.
- OpenBao authenticates with a CloudNativePG-issued client certificate, never a password.
- A renewed certificate must take effect on the next connection, without a restart.

**Schema.** OpenBao owns its storage schema and creates it idempotently at start. CloudNativePG owns the PostgreSQL infrastructure. No DDL is copied out of OpenBao, and no schema job exists.

**HA and replication.**
- HA is active/standby, with standby reads disabled.
- CloudNativePG instances and OpenBao replicas follow one ladder: 1 in development, 2 in staging, 3 in production.
- Synchronous replication:

| Environment | Synchronous standbys | Data durability |
|---|---|---|
| Development | none | asynchronous |
| Staging | 1 | `preferred` |
| Production | 1 | `required` |

- No unconditional zero-data-loss guarantee is made.

**Listener certificate and its trust.** OpenBao's PKI plays no part, so start, unseal and recovery work while that PKI is unavailable.

- **The CA.** The listener's certificate is issued by a cert-manager CA issuer whose CA is generated by the CLI during Day-0. A successor hub creates a new one; it is Class N.
- **Private key.** The CA's private key is a Secret readable only by cert-manager.
- **Public certificate.** Only the public certificate is distributed, as a ConfigMap, by the same Day-0 CLI delivery that writes the box's other bootstrap inputs. No consumer is granted access to the Secret that holds the key, because Kubernetes RBAC authorizes whole Secrets, not single properties.
- **Lifetimes.**
  - The CA lifetime is bounded and configurable.
  - The listener certificate's validity and renewal stay inside the CA's remaining lifetime.
  - The CA's expiry is monitored and alerted through the existing observability stack.
- **CA rollover** uses ADR-035's dual-trust procedure: the ConfigMap carries the old and new certificates during the overlap.
- **Reload.** The listener must reload its certificate without a restart.
- **Spokes** reach OpenBao through the hub Gateway's public listener. The Gateway validates the backend against the distributed CA certificate and OpenBao's service name.

### 3. Unseal, escrow custody and initialization

**Two escrow projects per box.** Both are box-scoped. The boundary is the project, because Infisical enforces membership per project and paths are not access boundaries.
- The **box escrow project** holds artefacts the runtime may read: seal-key generations and their delivery history, the recovery PGP public key, the PGP-encrypted recovery shares, the `openbao-db` archive credential, and the existing runtime-written artefacts.
- The **custody escrow project** holds the box's root CA private key and the recovery PGP private key. Only the declared administrator (ADR-076 amendment 2026-11-10) is a member.
- `kakt escrow init`, already run by the administrator before any box infrastructure exists, establishes custody as well as the box escrow project. It creates the custody project, generates the box's root CA and the recovery PGP key pair, places the private keys in custody, and places the public halves in the box escrow project.
- No automation identity, and so no runtime or CI identity, is a member of the custody project.
- The runtime's obsolete escrow permissions, the Infisical master-key backup and restore, are removed.

**Seal.**
- OpenBao uses the static seal. The escrow records every seal-key generation and every seal configuration delivered to the box (current and previous key identifiers, with delivery times).
- A recovery point is restored with the seal configuration that was in force at that point, taken from that history.
- Every generation is retained for as long as any retained `openbao-db` backup may need it.

**Accepted posture.**
- The delivered seal key is a Kubernetes Secret, encrypted at rest and restricted by RBAC.
- Key material never appears in Git, logs, process arguments or status.
- The static seal does not protect against a runtime compromise of the cluster as an independently secured, hardware-backed KMS would.
- No external KMS is introduced (ADR-111 §2).

**Recovery key.**
- Self-initialization produces no recovery key and revokes its root token.
- At bootstrap, a recovery key is initialized encrypted to the escrowed PGP public key, using OpenBao's native encrypted backup.
- The encrypted shares are escrowed and checked to be byte-identical to OpenBao's backup and bound to the custody key's fingerprint. The temporary backup and the one-time bootstrap identity are then retired. Every step is re-entrant after interruption.
- Runtime and CI identities can read only ciphertext. Decrypting it needs the custody project.
- No permanent root token exists, and no permanent identity keeps recovery-key initialization rights.
- The recovery key is the break-glass.

**Interrupted self-initialization: fail closed.** An interrupted self-initialization leaves storage that OpenBao refuses to unseal.
- Because no authoritative lifecycle record establishes that a database incarnation belongs to an unactivated, failed fresh-box attempt, `openbao-db` is never destroyed automatically.
- Bootstrap fails closed, preserves the database, and reports OpenBao's own failure.
- Recreating the store is a deliberate administrator decision under ADR-092.
- The absence of an escrow entry, a Secret, a backup or a condition never authorizes destroying state.

### 4. Identity, authorization and revocation

**Hub and spoke identities.**
- Hub identities authenticate with Kubernetes authentication against the hub's own API server, with token review.
- Each workload cluster has one JWT mount, verified against the public half of that cluster's Cluster API service-account signing key. The private half is never read or distributed.

**Role bindings and enforced lifetimes.**
- Every role binds an exact subject, an issuer and the `openbao` audience.
- Every role sets explicit, bounded clock-skew, expiration and not-before leeways. A value of 0 does **not** mean zero tolerance: it selects the defaults, which are 60 seconds for clock skew and 150 seconds each for expiration and not-before. So 0 is never used, and the effective combined allowance is verified at v2.7.3.
- Every role issues non-renewable service tokens with an explicit maximum TTL of 10 minutes. The auth mount's maximum lease TTL is also 10 minutes, so no token outlives 10 minutes whatever a role requests.
- OpenBao's JWT login cannot cap a presented token's lifetime; its maximum-age setting applies only to the OIDC flow. **The bound on a Kubernetes service-account token is therefore enforced where it is issued**, through the spoke ClusterClass, once compatibility with existing platform consumers is confirmed:
  - the API server's maximum service-account token lifetime is **1 hour**, the lowest value Kubernetes accepts;
  - token-expiration extension is **disabled**, so no projected token is extended past that maximum.
- ESO and cert-manager request 10-minute tokens. The bound follows from the API-server configuration, not from the request.
- Legacy long-lived ServiceAccount tokens are not accepted. Every role requires the `openbao` audience and the spoke's service-account issuer, and legacy Secret-based tokens carry neither.
- **Policy names are never reused.** Each per-tenant and per-cluster policy name carries the immutable identity of the object it serves, so an old token cannot regain authority when a deleted tenant's name is reused.
- **No impersonation of platform identities.** Platform roles bind only ServiceAccounts in platform namespaces. Tenants can neither run workloads in those namespaces nor create tokens for ServiceAccounts outside their own namespace (ADR-021 tenant boundary).

**Authorization boundaries.** Authorization is ACL policies, resolved by name on every request. They cover:
- the hub's platform secrets;
- each cluster's platform secrets;
- each tenant application's secrets, read through a namespace-scoped store using that namespace's own ServiceAccount;
- each cluster's certificate issuance.

Paths express ownership. Policies enforce it.

**Lifecycle contract.**

| Event | Required behaviour |
|---|---|
| Tenant application deleted | Its policy and role are deleted. Existing tokens lose all authority on their next request, because policies are resolved at request time. No new login is possible. Role deletion alone is never relied on, since it does not revoke issued tokens |
| Spoke deleted | Its JWT mount is disabled, which revokes every token issued through it, and its policies are deleted |
| ServiceAccount deleted on a spoke, independently of the OpenBao authorization lifecycle | A token already minted can still log in until its expiry plus the effective validation allowance. An OpenBao token obtained at the last moment lives at most 10 minutes more. **Residual ≤ maximum Kubernetes token lifetime (1 hour) + effective JWT validation allowance + maximum OpenBao token lifetime (10 minutes): about 70 minutes plus the configured allowance.** This is accepted in exchange for offline JWT verification without depending on every spoke's API server. It applies only to this event: deleting a tenant or a spoke ends access as in the rows above |
| Signing key rotated | The verification-key list holds old and new keys during the rotation, and the old key is retired when Cluster API retires it |
| OpenBao restored | §7: restored authorization cannot outlive current desired state, and restored tokens are revoked |

This replaces the per-cluster Infisical projects, the shared identity copied into tenant namespaces, client-secret delivery through ClusterResourceSet, and spoke-identity-operator.

### 5. Database credentials

The separation between a database's owner and its application login is approved. For every credential class, the responsibilities are:

| Credential | Generator | System of Record | Lifecycle owner | SQL reconciler | Delivery |
|---|---|---|---|---|---|
| `hub_<app>` login password (Zitadel, credits, Grafana) | OpenBao static role | OpenBao | OpenBao | OpenBao, authenticating as that role by certificate, to the maintenance database only | ESO from `database/static-creds`, then Reloader |
| `hub_<app>` existence, membership in `<app>_owner`, client certificate | CloudNativePG | Kubernetes API | CloudNativePG | CloudNativePG | certificate mounted into OpenBao only |
| `<app>_owner` (database owner) | CloudNativePG | — | CloudNativePG | CloudNativePG | none. Client authentication rejects every login by an owner role, so its CloudNativePG-held password is inert |
| `openbao` storage login | CloudNativePG CA (certificate) | Kubernetes API | CloudNativePG | — | mounted into OpenBao |
| `hub_billing` (superuser, initdb owner) | hub-operator | OpenBao KV | hub-operator (generate once, never regenerate) | CloudNativePG, from the ESO-delivered Secret | ESO, then CloudNativePG `initdb.secret` |
| spoke tenant role passwords | hub-operator | OpenBao KV | hub-operator | Crossplane `provider-sql` | ESO on the spoke |
| spoke `crossplane-admin` | hub-operator | OpenBao KV | hub-operator | spoke CloudNativePG | ESO on the spoke |

**Rules that apply to every class.**
- Each class has exactly one System of Record and one SQL reconciler, and values flow one way.
- OpenBao holds no superuser, `CREATEROLE` or `ADMIN OPTION` privilege.
- Client authentication rules allow a login role's certificate only for the maintenance database, and its password only for the application database.
- hub-operator's role manager is removed.

**Rotation semantics.** ADR-030's zero-downtime claim is withdrawn and replaced by the following:
- credentials converge eventually;
- sessions that are already authenticated are preserved;
- new authentications may fail temporarily, for an expected interval of about the delivery refresh plus the rollout, which is not bounded if reconciliation fails;
- failures are observable;
- recovery is through existing reconciliation.

Thirty days is the initial rotation policy, not an invariant. There are no dual login roles and no rotation coordinator.

**Dynamic credentials** are available. No platform application adopts them until lease recovery and orphaned-role behaviour after independent restores are established.

### 6. PKI

**Root CA and intermediate provisioning.**
- **Root key.** The box's root private key is held only in its custody escrow project. `kakt escrow init` creates it under the administrator's session, before CI bootstrap, so custody exists before OpenBao does.
- **Separate responsibilities.**
  - `kakt escrow init` establishes custody: it generates the root and stores it.
  - `kakt pki sign-intermediate` uses custody. It reads the root, retrieves OpenBao's CSR, signs it, and uploads only the chain.
  - It stores no CA material in the escrow. No intermediate private key is ever stored there.
  - The two are not merged: custody is established once, before OpenBao exists, while signing recurs at bootstrap, renewal and compromise replacement.
- **Intermediate key.** The issuing intermediate's private key is generated **inside OpenBao** and never leaves it. OpenBao exports only a certificate signing request.
- **Signing.** Under the administrator's session, the CLI reads the root from custody, signs that request, and returns only the signed certificate chain, which OpenBao imports. No intermediate private key passes through the escrow, CI or the CLI.
- **Ordering.** Signing happens after OpenBao is available and before any consumer that depends on its PKI is admitted. Until then, issuance is unavailable and certificate-dependent components wait, converging through GitOps once the chain is installed.
- **How the administrator authorizes it.** Signing is **one explicit, idempotent administrator command** (`kakt pki sign-intermediate`), run under the administrator's authenticated session.
  - Before signing, the command validates the box's identity, the CSR (subject, key, CA request) and the expected trust hierarchy.
  - It returns only the certificate chain.
  - It may be run as soon as the declaration is pushed, because it waits for OpenBao's request.
  - This is the one CLI addition this ADR authorizes, and the only quickstart change. The quickstart references the canonical CLI documentation rather than repeating the procedure.
  - Today's bootstrap is asynchronous and Git-triggered, so no existing administrator command runs while the request is pending.
  - **Signing is never folded into an unrelated command.** `kakt verify` remains a read-only assessment (ADR-115 unchanged): it never reads CA keys, signs, modifies OpenBao, or completes unfinished bootstrap work. `kakt kubeconfig` remains an access command.
  - No CA service, signing controller, persistent administrator session, CI custody identity or workflow engine is introduced. CI never holds root material.
- **Day-0 completion semantics.**
  - Provisioning infrastructure and accepting the platform are different states.
  - Day-0 waits, for a bounded period, for the administrator's signature. Once the chain is installed, Day-0 proceeds to its normal completion and acceptance.
  - If the period expires, the run fails with a specific "administrator signing pending" reason. It never reports the platform as bootstrapped, healthy or accepted while mandatory PKI-dependent components are unavailable. Once signed, re-running it resumes from the phases already recorded as complete (ADR-079).
  - Existing failure semantics and mandatory workload checks are unchanged. Nothing is made best-effort to accommodate the signing.
- **Validity.** The intermediate's validity is configurable, defaults to five years, and is always bounded by the root's remaining validity. OpenBao's issuer refuses any leaf that would outlive the intermediate.
- **Signing certificates.**
  - Certificate lifetime and signing-key lifetime are separate decisions.
  - Signing certificates, such as the Argo CD agent's token-signing certificate, are renewable, with a validity ceiling well inside the intermediate's, instead of the previous ten-year profile ceiling. Their key rotates on renewal unless a consumer's verified contract requires otherwise.
  - Consumers reload on renewal through the existing mechanism.
  - No second CA hierarchy exists for long-lived signing certificates.
- **Scheduled renewal** is the same administrator operation, under ADR-035's dual-trust rollover. ADR-035 already provides for root use "during Day-0 bootstrap or formal CA rollover".
- **Compromise is a different operation from renewal.**

| Event | Required behaviour |
|---|---|
| Scheduled intermediate renewal | ADR-035's dual-trust rollover. The old intermediate stays trusted until its leaves have expired |
| Intermediate private-key compromise | A replacement intermediate alone does **not** stop relying parties from accepting certificates signed with the compromised key, because they trust the root. Compromise is therefore handled by **root rollover**: a new root and intermediate are created under the administrator's session; every trust bundle is replaced, removing the old root; and every leaf is reissued, under ADR-035's emergency fast path. Short leaf lifetimes do not contain an issuer compromise |
| Root compromise | The same trust replacement |

**Issuing intermediate.** OpenBao holds the intermediate. cert-manager remains the only certificate lifecycle authority, through its built-in Vault issuer.

**Issuance restrictions.** Each issuing role constrains:
- subject common names and SANs to an explicit list, with no wildcards, globs, IP SANs or arbitrary names;
- key usages and extended key usages, separating server from client;
- TTL ceilings.

Each cluster's issuer may use only its own roles. **A spoke issuer can never request another cluster's identity.** Ordinary leaf revocation remains by short lifetime, with no CRL or OCSP for consumers. Issuer compromise is handled by trust replacement, as above.

**Custody boundary.**
- Runtime and CI identities are prohibited from retrieving root material: they are not members of the custody project.
- A compromise of the hub cluster, of OpenBao or of the CI automation identity yields at most the intermediate. Its containment is the compromise procedure above, not ordinary rollover.
- The root remains exposed to compromise of the administrator's escrow account, of the escrow provider, or of the administrator's workstation during signing.
- It does not provide hardware non-exportability, tamper evidence or quorum control.
- ADR-035's requirement changes from "offline, hardware-isolated for enterprise production" to "custody outside every runtime and automation identity; escrow-held by default; offline or hardware custody optional". Escrow custody is not equivalent to offline or HSM custody.

**Trust domain.** The replacement starts a new trust domain once. That is acceptable because no production users exist.

### 7. Recovery

**Classification (ADR-111 §2).**

| State | Class |
|---|---|
| `openbao-db` | B |
| Seal-key generations and seal-configuration history, recovery material, root CA, archive credential | E |
| Listener CA | N |
| Spoke service-account keys | M |
| Configuration and desired authorization | R |

**Recovery incarnations and records.**
- A restored database is recognised by its new cluster identity. That identifies a recovery incarnation; it does not prove recovery is complete.
- Completion is a durable, step-by-step record per incarnation, kept in OpenBao.
- Each recovery step runs once per incarnation, resumes from the record after interruption, and never runs on ordinary reconciliation.

**Recovery authority.**
- Recovery runs under a dedicated recovery identity: a Kubernetes-auth role bound to a recovery ServiceAccount, whose tokens only cluster administrators can create.
- It authenticates **freshly** after the restore. Its policy covers only recovery: listing and revoking token accessors, re-establishing static passwords, and writing recovery records.
- Revoking restored tokens excludes only that fresh session. hub-operator, like every other client, logs in again.
- Recovery is driven within ADR-092's administrator-entered recovery mode. No new recovery controller or workflow engine exists.

**Records represent effects.** A step is recorded complete only after its effect is verified:
- desired authorization is applied;
- only tokens issued after the recovery began remain;
- every static role shows a rotation after the incarnation began, and its new credential authenticates;
- spoke verification keys match the current Cluster API Secrets.

**Restoring OpenBao.** This follows ADR-092: a deliberate recovery mode with a machine-enforced exit. In recovery mode, OpenBao's consumer admission is not declared. Admission is restored by settling the manifest only after the incarnation's record shows, as verified effects, that:
- current GitOps and platform-owned desired authorization is applied: obsolete identities, policies, auth roles and mounts are removed, missing ones recreated, and policy contents rewritten to desired state;
- every service token present in the restored store, other than the recovery session, has been revoked;
- each spoke mount's verification keys are current;
- static database passwords have been re-established for every application database **not** itself being restored;
- a generated value missing from the restored state is reported and not regenerated (ADR-003 §1).

**Ordering when application databases are restored too.** An application database's restore completes **before** that database's credential reconciliation. Rotating first and restoring afterwards would bring back the old password hash. The application stays unavailable until its own database recovery and its credential reconciliation are both recorded complete.

**Restoring an application database alone.** OpenBao re-establishes that role's password once per incarnation, after the restore completes. The application cannot authenticate until it has.

**Hub reconstruction.** The order is: escrow material; `openbao-db` restored; authorization reconciled and tokens revoked; application databases restored; each application's credential reconciled; admission. This preserves the trust domain and spoke authentication: the restored intermediate and the unchanged spoke keys carry them, and the root never left custody.

**OpenBao version changes.**
- Each change of OpenBao version is preceded by a CloudNativePG backup of `openbao-db`, declared as part of the component. It must be complete before the new version starts.
- The backup's identity is unique to the upgrade operation: the target OpenBao version, the bundle version, and the archive identity of the `openbao-db` incarnation, which changes whenever a restore recreates the cluster. So a backup completed for an earlier operation, including one before a rollback, never satisfies a later upgrade.
- Routine synchronization, configuration changes and self-healing never create a backup. A backup is created only when the declared version changes.
- Rolling back means restoring that backup, with the recorded version and the seal configuration in force at the backup. Reverting the chart alone does not revert storage. Changes made after the recovery point are lost.

### 8. Audit

- **Device.** OpenBao's native audit device is enabled by self-initialization, before any consumer is admitted.
- **Coverage.** It records API requests and responses, including authentication, secret reads and writes, certificate issuance and administrative changes. The exceptions are the endpoints OpenBao documents as unaudited, such as unauthenticated health and seal-status probes. No claim of unconditional coverage of every endpoint is made.
- **Hashing.** Sensitive values are HMAC-hashed, which is the native default. Raw logging is never enabled.
- **Path to storage.** The device writes locally, to the process's standard output. The existing Alloy and Loki pipeline (ADR-078) collects it asynchronously, so no secret request depends on a remote endpoint. Collection is best effort: durable retention of every emitted record is not guaranteed.
- **Failure is closed and visible.** If no audit device can record a request, OpenBao refuses the request. That is accepted. It is observable through OpenBao's audit-failure metric and alerted on through the existing alerting.
- **Retention and access.**
  - Audit records are retained under ADR-078's retention for security-relevant logs.
  - Reading them is restricted to the box's administrators.
  - Records lost because node-local log rotation outpaced collection are a stated limitation, observable as collection lag.
- No additional audit database, SIEM or logging service is introduced.

### 9. Delivery

- The replacement ships in one release through the existing bundle lifecycle.
- There are no intermediate releases, phase gates, fallback architectures or additional release gates.
- Verification is behavioural, inside the existing verification framework.

## Ownership

See ADR-039.

| Resource Class | System of Record | Lifecycle Owner | Reconciler | Consumer | Phase |
|---|---|---|---|---|---|
| Platform and application secret values | OpenBao KV | Per ADR-039 generator class | ESO (delivery) | Workloads | Day-1+ |
| `openbao-db` cluster, its owner role, the OpenBao login role and its client certificate | Kubernetes API | CloudNativePG | CloudNativePG | OpenBao | Day-1+ |
| OpenBao storage schema | PostgreSQL | OpenBao | OpenBao | OpenBao | Day-1+ |
| Seal-key generations and seal-configuration history | Box escrow project | CLI | — | OpenBao | Day-0 (rotation: maintenance) |
| Recovery key shares (PGP-encrypted), PGP public key | Box escrow project | CLI | — | Break-glass only | Day-0 |
| Root CA private key, recovery PGP private key | Custody escrow project | CLI, administrator session | — | Intermediate signing, break-glass | Day-0 (rollover: maintenance) |
| Issuing intermediate CA (key internal to OpenBao) | OpenBao | OpenBao (key and request); CLI under the administrator session (signature, rollover, compromise replacement) | — | cert-manager | Day-0 |
| Initial mounts, auth methods, base policies, audit device | OpenBao | OpenBao self-initialization | — | — | Day-0 |
| Per-cluster auth mounts, per-application policies and roles, PKI roles, static roles | OpenBao | hub-operator | hub-operator | ESO, cert-manager | Day-1+ |
| Recovery incarnation records | OpenBao KV | Recovery identity (ADR-092 recovery mode) | — | Recovery verification (ADR-092) | Day-1+ |
| Listener CA (key Secret, public ConfigMap) | Kubernetes API | CLI | CLI (rollover) | cert-manager (key); in-cluster clients and Gateway (public) | Day-0 |
| Listener certificate | Kubernetes API | cert-manager | cert-manager | OpenBao | Day-1+ |
| Hub application login role passwords | OpenBao | OpenBao | OpenBao (static role) | Applications via ESO | Day-1+ |
| Hub database owner roles and login-role existence | Kubernetes API | CloudNativePG | CloudNativePG | — | Day-1+ |
| Billing, spoke tenant and `crossplane-admin` passwords | OpenBao KV | hub-operator | CloudNativePG / `provider-sql` (SQL) | Their applications | Day-1+ |
| Leaf certificates | Kubernetes API | cert-manager | cert-manager | Workloads | Day-1+ |
| Audit records | Loki | OpenBao (emission) | Alloy (collection) | Administrators | Day-1+ |

## Consequences

### Positive

- **Enforceable isolation.** Cluster, tenant and component boundaries are enforced by OSS policies bound to workload identity, with no distributed client secrets.
- **One authority per credential.** Every database credential has one generator, one System of Record and one SQL reconciler, which closes the split ownership between CloudNativePG and hub-operator.
- **Less custom machinery.** Native rotation, dynamic credentials and PKI replace spoke-identity-operator, the external certificate issuer on every cluster, the CLI's Infisical bootstrap, and hub-operator's Infisical client, uploaders and role manager.
- **Root custody outside every runtime and automation identity.** A cluster or CI compromise does not yield the root.
- **Defined recovery and data-preservation contracts** for each database, for the store and for the whole hub. Persistent state is never destroyed on inference.
- **Designed for unattended operation.** Unseal, initialization, recovery-key enrolment and certificate renewal are designed to need no human step in normal operation.
- **Enforced audit at the point of emission.** Audited requests are refused if they cannot be recorded locally, and values are hashed.

### Negative

- **More memory, not less.** At HA, OpenBao costs more memory than Infisical; production precedent sizes each replica at 2 GB or more. The gain is operational, not in resources.
- **Static-seal exposure.** The static seal leaves the store readable by anyone who can read the delivered seal Secret and reach `openbao-db`.
- **Root custody weaker than ADR-035 required, and one administrator step.** Escrow custody is weaker than offline or hardware custody. The root is used only under the administrator's session, so every box bootstrap includes one explicit administrator signing command, which can be run as soon as the declaration is pushed. If it is not run within Day-0's bounded wait, Day-0 fails with that reason and is re-run after signing. Certificate-dependent components converge after it. The quickstart gains this one step. Renewal and compromise replacement are administrator operations too. This bounded ceremony is preferred to weakening root custody.
- **Issuer compromise means trust replacement.** Recovering from an intermediate compromise needs a root rollover, with fleet-wide trust-bundle replacement and leaf reissue.
- **Bounded spoke revocation lag.** A spoke ServiceAccount deleted independently of OpenBao can retain access for about 70 minutes plus the configured validation allowance.
- **Short service-account tokens on spokes.** Disabling token-expiration extension and capping lifetimes at one hour requires every spoke workload to refresh projected tokens. This is confirmed per consumer before the ClusterClass changes.
- **Rotation can interrupt new connections.** Rotation can fail new authentications for an interval that has no hard bound.
- **Upgrades take OpenBao down.** OpenBao upgrades are not zero-downtime, and a rollback loses changes made after the pre-upgrade backup.
- **Failed initialization needs a human decision.** A failed fresh initialization stops bootstrap until an administrator decides, because no lifecycle record can authorize automatic recreation.
- **Audit failure stops secret operations.** A failure of local audit emission stops secret operations. Collection to Loki is best effort: node-local log rotation can outpace it, so durable retention of every record is not guaranteed.
- **A new trust domain.** Every certificate and trust bundle is reissued once.

## Impact

- **ADR-003:** OpenBao replaces Infisical as the System of Record. Credentials flow one way. Rotation is OpenBao-native. Standing Decision 5 in `adr-template.md` reads "Storage (OpenBao)".
- **ADR-023:** CloudNativePG owns owner roles and login-role existence. Owner roles cannot log in. OpenBao owns hub login-role passwords. The 2026-08-22 Day-0 bootstrap-role addendum is retired with `infisical-db`.
- **ADR-030:** the zero-downtime claim is replaced by the rotation semantics in Decision §5.
- **ADR-031:** policies and per-cluster JWT mounts replace per-cluster Infisical projects and identities, under the lifecycle contract in Decision §4.
- **ADR-032 and ADR-035:**
  - the custody amendment and the issuance restrictions (Decision §6);
  - OpenBao replaces Infisical as the issuing authority;
  - cert-manager's built-in Vault issuer replaces the Infisical issuer;
  - spoke deletion disables the cluster's mount.
- **ADR-039 and ADR-040:**
  - the ownership rows above;
  - the seal keys, recovery material and root CA are Day-0 CLI artefacts;
  - seal rotation and intermediate rollover are maintenance operations under ADR-035's rollover provision.
- **ADR-076:**
  - a per-box custody escrow project whose only member is the administrator, established by `kakt escrow init` together with the box's root CA and recovery PGP key pair;
  - seal-key generations, seal-configuration history and encrypted recovery shares replace `infisical-master-keys` and `infisical-admin-credential`;
  - the runtime's master-key escrow access is removed.
- **ADR-078:** OpenBao's audit stream is a security-relevant log source with restricted access.
- **ADR-079:** a failed OpenBao self-initialization is not re-entrant. It fails closed.
- **ADR-092:** OpenBao recovery mode omits consumer admission. Exit requires a complete recovery record.
- **ADR-111:**
  - the Class E inventory;
  - the reconstruction order (escrow material; restore `openbao-db`; recovery reconciliation; admission; application databases);
  - recovery incarnations.
- **ADR-112:**
  - `infisical-db` becomes `openbao-db`, with no pooler (a §9 exception);
  - application clusters separate the owner role from the login role;
  - `openbao-db` takes a backup before each OpenBao version change.
- **Removed:**
  - Infisical and its vendored chart;
  - the Infisical PKI issuer;
  - spoke-identity-operator;
  - hub-operator's Infisical client, uploaders, role manager and master-key escrow access;
  - the CLI's Infisical bootstrap.

  Their validators are replaced by behavioural checks.
- **Component catalogue:** it records OpenBao and its chart as MPL-2.0, with source references and version provenance. Infisical entries are removed.

## References

- ADR-039: Platform Ownership Model
- ADR-040: Day-0 vs Day-1 Lifecycle Boundary
- ADR-041: Controller Responsibility Matrix
- ADR-043: Control Plane Authority Model
- ADR-003, ADR-023, ADR-030, ADR-031, ADR-032, ADR-035, ADR-076, ADR-078, ADR-079, ADR-092, ADR-111, ADR-112, ADR-115
- Design record: `docs/plans/openbao-cnpg-secret-management-proposal.md`
- OpenBao v2.7.3 and CloudNativePG v1.30.1 source (`reference-projects/vault/`)
- GitLab Secrets Manager: OpenBao chart and administration documentation (production precedent)
