# Proposal: replace self-hosted Infisical with OpenBao on CloudNativePG

**Status:** Revised after the 2026-11-24 architecture review. Submitted for final approval. Nothing here is implemented, and no accepted ADR has been edited.
**Date:** 2026-11-24
**Scope:** The box's own secret store, machine identity, internal PKI and database credential lifecycle. The external escrow (Infisical Cloud, or an Infisical the tenant runs) is unchanged.

## Evidence labels

Every material claim carries one of these labels.

| Label | Meaning |
|---|---|
| **[SF]** | Source fact. The cited repository file says this directly. |
| **[UF]** | Upstream fact. Read from upstream documentation or release pages on 2026-11-24. It has not been checked against upstream source code unless this says so. |
| **[DB]** | Derived behavior. Traced through more than one source. Not observed at runtime. |
| **[AS]** | Assumption. Not verified. Must be verified before the change it supports. |
| **[OQ]** | Open question. Unresolved. It can change the decision. |

No runtime evidence was collected for this proposal. Per the team-lead instructions §13.1, live verification goes to the Release Engineer (§13 of this document).

---

## 1. Decision record

### 1.1 Settled by architecture review (2026-11-24, final review incorporated)

The direction is final. This document now records the target design contracts, not a selection between options.

| Decision | Verdict |
|---|---|
| OpenBao v2.7.3, chart 0.30.8 (normal compatibility verification), CNPG v1.30.1 | Approved |
| Infisical Cloud only as the external escrow | Approved |
| PostgreSQL storage, active/standby HA, standby reads disabled | Approved |
| OpenBao runs in `platform-data` | Approved |
| Static auto-unseal with an escrowed key | Approved. The security posture is accepted (§6.4) |
| Certificate-based database authentication, direct connection, no PgBouncer | Approved |
| Certificate renewal without a workload restart | Approved |
| Native static password rotation; native dynamic credentials available (consumer adoption out of scope) | Approved |
| Automatic backup before an OpenBao version change (not on routine sync) | Approved (§9.4) |
| Crash-safe PGP-encrypted recovery-key escrow | Approved (§6.5) |
| Replication ladder (OQ-9) | Approved as in §5.1 |
| Automatic database deletion after interrupted initialization | **Rejected.** No authoritative lifecycle record exists, so bootstrap fails closed (§6.2) |
| Backup on every GitOps sync | **Rejected.** Backup on version change only (§9.4) |
| Runtime or CI access to root or recovery material | **Rejected.** Custody escrow project, administrator only (§6.0) |
| Permanent online root CA | **Rejected.** Escrow-held root in the custody project (§8.3) |
| Phased release gates, rubric, fallback architectures | **Rejected.** Removed (§12–§14) |

### 1.2 Why this architecture

These are the reasons the review accepted. They are kept for the ADR that will supersede ADR-003/035.

- **Enforceable OSS authorization.** Infisical OSS has no path-scoped roles without a licence ([SF] ADR-031 correction). OpenBao ACL policies, bound to Kubernetes and JWT identities, enforce cluster, tenant and component boundaries without distributed client secrets. This closes F1 by construction (§3.3).
- **Native credential lifecycle.** Static-role rotation, dynamic credentials and PKI replace `RoleManager`, the uploaders, spoke-identity-operator and the Infisical issuer (§11.2).
- **Operational simplification, not resource savings.** At HA, OpenBao costs more memory than Infisical (§5.6). The saving is fewer components and less custom code.

### 1.3 Findings outside this decision

F1, F2 and F3 (§2.4) stand independently. F2 (a password logged on role creation) and the runtime check of F1 (RE-0) should not wait for the replacement.

## 2. Current architecture: Infisical dependency inventory

The search covered `grep -rliI infisical` over the repository, excluding `reference-projects`, `node_modules`, `.git`, `dist` and `bin`. It found 510 files. The known-positive control was `operators/hub-operator/internal/secrets/infisical.go`, which was found. The inventory below is grouped by function, not listed file by file. Generated copies under `internal/platform/embedded/` mirror `manifests/` and are not counted separately. **This search is not exhaustive for conventions-only references.** ADR-112 §8 records that credential names derived in code (for example `database.RoleSecretName`) appear in no manifest.

### 2.1 Classification key

- **SH** — self-hosted secret-management dependency (replaced)
- **ESC** — external escrow dependency (retained)
- **PI** — provider-independent contract (retained, re-pointed)
- **OBS** — obsolete (removed, not ported)

### 2.2 Inventory

| # | Area | Path(s) | What it does | Class |
|---|---|---|---|---|
| 1 | Deployment | `manifests/hub-core-services/infisical/` (vendored `infisical-standalone-1.12.2` chart, `values.yaml`, migration-unlock init) | Runs Infisical v0.167.12, 1 replica, 350m / 2000Mi request ([SF] `values.yaml:150-155`) | SH |
| 2 | Database | `manifests/hub-core-services/infisical/database/infisical-db.yaml`, `database-recovery/`, `argocd/components/02/platform-infisical-db.yaml` | Bootstrap CNPG cluster. `managed.roles: infisical`. `initdb.secret`. Pooler. Archive from escrow ([SF] ADR-112 §6-§9) | SH → re-targeted to `openbao-db` |
| 3 | Master keys | `operators/hub-operator/internal/secrets/generator.go:223` `GenerateInfisicalSecrets` | Generates or restores `ENCRYPTION_KEY` and `AUTH_SECRET`, escrows them, writes `infisical-secrets` (+ `REDIS_URL`, `DB_ROOT_CERT`) | SH (escrow call is ESC) |
| 4 | DB credentials for Infisical | `generator.go:168,389` `GenerateInfisicalDBCredentials`, `GenerateInfisicalPostgresConnection`; `constants.BootstrapDBCluster/BootstrapDBPooler` (CLI) | Password and connection Secret, written in two Go modules ([SF] ADR-112 §9) | OBS (cert auth replaces it) |
| 5 | Day-0 bootstrap | `internal/kakt-cli/infisical/bootstrap.go` (1747 lines), `cert_profiles.go`, `client.go`, `portforward.go`, `statefile.go` | `kubectl exec … curl` into the pod. Admin bootstrap. Creates `hub-platform` and `hub-secrets` projects and an org-admin machine identity. Creates the Fleet CA (a **root**, `type: root`, valid to 2036) and the profiles ([SF] `cert_profiles.go:600-651`, `bootstrap.go:1583`) | SH |
| 6 | Admin credential | `internal/platform/escrow/admin_credential.go`; escrow artefact `infisical-admin-credential` | The box's store administrator, held only in the escrow ([SF] ADR-076 recovery table) | SH artefact in ESC store → replaced by `openbao-recovery-key` (§6.5) |
| 7 | Hub-operator Infisical client | `operators/hub-operator/internal/secrets/infisical.go` (1733), `cluster_project.go`, `cluster_credentials.go`; `internal/infisical/*` (2654); `internal/client/infisical.go`; `internal/constant/endpoints.go` | Phase 0/1/1b/3 of `HubEnvironment`. Uploads CLI and application secrets. Per-spoke machine identity `spoke-pool-<cluster>`. Per-cluster project. Fleet credential materialisation. Tenant folders. OAuth client secrets. PKI template self-heal | SH |
| 8 | Hub DB roles | `operators/hub-operator/internal/database/roles.go` `RoleManager` | Creates hub app roles and rotates their passwords from ESO-delivered Secrets ([SF] `README.md`) | PI (role ownership), replaced by CNPG `DatabaseRole` + OpenBao static roles (§7) |
| 9 | Spoke identity | `operators/spoke-identity-operator/` (whole operator: `internal/infisical/*` 1952, `internal/controller/*` 1877) | One identity per spoke. Universal Auth client secret packaged into a CRS wrapper. Rotation. Membership inventory and narrowing. Separate PKI identity ([SF] ADR-035 "Operator Responsibilities", ADR-031 correction) | OBS (replaced by JWT auth, §8.2) |
| 10 | ESO stores | `hub-core-services/cluster-secret-store/`, `spoke/spoke-catalog/infra/cluster-secret-store.yaml` (`infisical-backend`, Universal Auth), `tenants/charts/universal-tenant/templates/tenant-secret-store.yaml` | Infisical provider stores | PI (ESO stays; provider becomes `vault`) |
| 11 | ExternalSecrets | Spoke catalog (`s3-credentials-es`, `crossplane-admin-eso`, `loki/object-storage-es`, `external-dns`, victoriametrics), tenant chart (`workload-secrets`, `workload-oauth-secrets`, `pull-secret-es`, `agentgateway`), `tenant-db/composition.yaml` | `remoteRef.key` paths in Infisical folder syntax | PI (keys re-pathed) |
| 12 | PKI issuer | `hub-core-services/security/` (`infisical-issuer-crd.yaml`, `infisical-issuer.yaml`, approver RBAC, `templates/infisical-issuers.yaml`); spoke-catalog equivalents; `internal/pki/profiles.go` | `pki-issuer` v1.1.0 external issuer. `infisical-fleet-issuer` and `infisical-signing-issuer` ([SF]) | SH (controller and CRDs OBS; profile data PI) |
| 13 | Certificate consumers | `argocd-principal/certificates.yaml` (4), `spoke-catalog/infra/certificates.yaml` (3), `support-agent/certificate.yaml`, `spokepool_controller.go:420-480` (per-spoke `argocd-agent-<spoke>` bootstrap cert), AINativeSaaS composition | Ordinary cert-manager `Certificate`s | PI (`issuerRef` changes only) |
| 14 | Exposure | `hub-core-services/gateway/templates/hub-gateway.yaml:163` (listener `infisical`), `backend-policies.yaml` | Public `infisical.<hubDomain>`, used by spoke ESO and the issuers | PI (listener renamed) |
| 15 | Redis | `generator.go` `REDIS_URL`; `hub-core-services/redis/network-policy.yaml` | Infisical is one Redis consumer. kube-sbt and billing are others ([SF] network policy) | Infisical's grant OBS; Redis stays |
| 16 | Escrow client | `internal/platform/escrow/*` (`escrow.go`, `provision_identity.go`, `box_credentials.go`, `active_box.go`, `store.go`), `internal/kakt-cli/hubaccess/` | Its own HTTP client to the tenant's Infisical (Cloud by default, `https://app.infisical.com`). Shares no code with item 7 ([SF] imports) | **ESC: retained** |
| 17 | Escrow artefacts (Infisical-specific) | `infisical-master-keys`, `infisical-admin-credential` | ADR-076 recovery table | ESC artefacts, renamed (§6) |
| 18 | CLI orchestration | `internal/kakt-cli/bootstrap/orchestrator.go`, `components/secrets_*`, `health/{waiter,cnpg,converged,phases}.go`, `tenant/*`, `teardown`, `session`, `cmd/kakt/{bootstrap,init_secrets*,box,app,…}.go` | Readiness waits and handover text that name the store | PI (re-pointed); Infisical-specific waits OBS |
| 19 | CRD fields | `hub-operator/api/v1alpha1/hubenvironment_types.go`; box declaration `management.infisicalEnvironmentSlug` | API surface that names the product ([SF] ADR-076 amendment §8) | OBS (clean replacement: no production users) |
| 20 | Validators | `scripts/validate/preflight/{031-infisical-api-contract,037-cluster-secret-project,95-infisical-key-producers,96-spoke-secret-authz,91-escrow-contents…}`, `scripts/validate/infisical-api-contract.yaml`, `scripts/validate/cluster/{031-cluster-secret-isolation,50-root-of-trust-backup,20-secrets-resolve,31-certificate-issuers,…}` | Contract and isolation gates | SH gates OBS. Behavior gates PI (re-targeted, §13) |
| 21 | Support Agent | `support-agent/allowlist.yaml:109` (`backup-health-infisical`), certificate | Backup evidence collector and mTLS client cert | PI |
| 22 | Component catalog | `manifests/architecture/component-catalog.yaml:422-442` (Infisical, Infisical PKI issuer) | Release inventory | SH entries removed; OpenBao added |
| 23 | Docs and specs | `docs/infisical/*`, ADR-003/030/031/032/035, `.kiro/specs/*infisical*` | — | Superseded by new ADRs (not edited before review) |

### 2.3 What Kamarkat actually requires from the store

These requirements come from the inventory, not from a product feature list.

1. **R1. System of record for platform and application secret values** — KV with versioning. Written by hub-operator and the CLI. Read only by ESO (ADR-003).
2. **R2. Isolated read scopes:** the hub; each workload cluster's platform credentials (ADR-031 correction); each tenant application (`tenant-secret-store`). Boundaries must be **enforced**, not naming conventions.
3. **R3. Machine identity for ESO and issuers** on the hub and on every spoke, with no human step and with revocation on spoke deletion (ADR-035 deletion sequence).
4. **R4. One internal CA** issuing through cert-manager, with server-side TTL ceilings per profile (`internal/pki/profiles.go`): `infrastructure-services` 7d, `signing-keys` 3650d, and others.
5. **R5. Database credential generation and rotation** for hub application roles and spoke tenant roles, with exactly one password authority per role.
6. **R6. Recovery from Class B + Class E alone** (ADR-111 Revision 5, ADR-112 §4). The store must not depend on itself or on ESO to start.
7. **R7. Audit** of secret reads and administrative changes (ADR-035 rollover invariant 4).
8. **R8. Human administration** for the box owner, plus break-glass.

### 2.4 Findings made during the inventory (independent of this decision)

These do not depend on the proposal and should not wait for it.

- **F1. A tenant namespace holds a credential with project-wide read on `hub-secrets`.** [DB]
  - `SpokePoolReconciler` calls `EnsureInfisicalCredentials` through `r.InfisicalClient.ForEnvironment(...)`. That client's project is `secretsProjectID` ([SF] `cmd/main.go:347-359`, `spokepool_controller.go:101-114`).
  - The function creates identity `spoke-pool-<cluster>` and grants it `viewer` on that project ([SF] `secrets/infisical.go:1023-1080`).
  - Its credential is stored at `/spoke-pool/<cluster>/shared/infisical-credentials` and copied into each tenant path (`EnsureTenantFolderAndCredentials`, [SF] `infisical.go:1264+`).
  - The chart materialises it as `<tenant>-infisical-credentials` in the tenant namespace and uses it for `tenant-secret-store` against `hub-secrets`/`management` ([SF] `tenant-secret-store.yaml`).
  - ADR-031's own correction establishes that `viewer` reads **every path** of a project ([SF]).
  - **Unverified boundaries:** (a) whether the `viewer` grant is effective on the deployed box; (b) whether a tenant-authored workload can mount that Secret or otherwise read it, which depends on the tenant AppProject and RBAC; (c) reachability of `infisical.<hubDomain>` from tenant pods.
  - Until (a) and (b) are checked at runtime, this is a high-consequence hypothesis, not a confirmed exposure. **Recommended now:** Release Engineer runtime check (§13, RE-0).
- **F2. `RoleManager` logs a cleartext password when it creates a role.** [SF] `operators/hub-operator/internal/database/roles.go:312-315` logs `createSQL`, which embeds the escaped password, at Info level and again on error. The `ALTER ROLE` path deliberately does not log it (`roles.go:352-355`). Any role created since that code shipped has its initial password in the operator's logs, and therefore in Loki (ADR-078).
- **F3. ADR-035's "offline root" is not what is built.** The Fleet CA is created inside Infisical as `type: root`, self-signed, valid to 2036-06-07 ([SF] `cert_profiles.go:600-651`). ADR-035 §Trust Hierarchy states that no Infisical instance may retain the root key. ADR-032's 2026-08-31 amendment already records the related single-authority concentration. This proposal preserves the built behavior (an online root) and does not silently promote it to the ADR's intent (§8.3).

---

## 3. Target architecture

### 3.1 Topology

The authoritative diagram is ADR-117, "Architecture at a glance". It is not duplicated here, so that the two cannot drift apart.

### 3.2 Ownership matrix

One owner per row, per ADR-039.

| Resource | System of record | Generator | Lifecycle owner | Reconciler | Consumer |
|---|---|---|---|---|---|
| `openbao-db` cluster, `openbao_owner` (initdb owner), `openbao` login role and its client cert | PostgreSQL / K8s | CNPG | CNPG | CNPG | OpenBao |
| OpenBao storage schema | PostgreSQL | OpenBao (`skip_create_table=false`, §5.3) | OpenBao | OpenBao (idempotent at start) | OpenBao |
| Seal-key generations and seal-configuration history | Box escrow project | CLI | CLI (rotation, retention §6.3) | — (delivered by the CLI) | OpenBao |
| Root CA private key, recovery PGP private key | **Custody** escrow project (administrator only) | CLI, administrator session | CLI, administrator session (§6.0, §8.3) | — | intermediate signing, break-glass |
| Recovery key shares (PGP-encrypted), PGP public key | Box escrow project | OpenBao, encrypted to the custody key | CLI (§6.5) | — | break-glass |
| Listener CA | key: K8s Secret readable by cert-manager only; public: `openbao-listener-trust` ConfigMaps | CLI (Day-0) | CLI (rollover) | — | cert-manager (key); clients and Gateway (public) |
| Listener certificate | K8s Secret | cert-manager | cert-manager | cert-manager | OpenBao (auto-reload) |
| Initial mounts, auth backends, base policies | OpenBao storage | OpenBao self-init (config in Git) | OpenBao | — (first start only) | — |
| Per-spoke auth mounts, per-app policies and roles, PKI roles, DB static roles | OpenBao storage | hub-operator | hub-operator | hub-operator (idempotent PUTs) | ESO, cert-manager |
| Platform / application secret values (KV v2) | OpenBao | hub-operator / CLI / human | as today (ADR-039) | ESO delivers | workloads |
| Hub app database owner `<app>_owner` (initdb owner) and its password | — (inert: `pg_hba` rejects owner logins) | CNPG | CNPG (instance controller) | CNPG | none |
| Hub app login role `hub_<app>`: existence, membership, certificate | PostgreSQL | CNPG `DatabaseRole` (no `passwordSecret`) | CNPG | CNPG | — |
| Hub app login role **password** | OpenBao | OpenBao static role | OpenBao | OpenBao (self-rotation, `rotation_period`) | ESO → app |
| Billing, spoke tenant and `crossplane-admin` passwords | OpenBao KV | hub-operator (once) | hub-operator | CNPG / `provider-sql` applies (SQL) | their applications (§7.4) |
| Issuing intermediate CA | OpenBao `pki_int/` (key generated internally, never exported) | OpenBao (key and CSR); CLI under the administrator session (signature) | CLI under the administrator session (renewal, compromise replacement) | — | cert-manager |
| Recovery records | OpenBao KV `platform/recovery/<cluster>/<uid>` | recovery identity (`kakt`, ADR-092 recovery mode) | recovery identity | — | ADR-092 recovery verification |
| Audit records | Loki | OpenBao audit device | OpenBao | Alloy (collection) | administrators |
| Leaf certificates | K8s Secret | cert-manager | cert-manager | cert-manager | workloads |

**Invariant:** for every PostgreSQL role, exactly one of CNPG, provider-sql, or OpenBao sets its password. CNPG never has a `passwordSecret` on, or initdb ownership of, a role that OpenBao rotates (§7.1). hub-operator never issues `ALTER ROLE … PASSWORD` again.

### 3.3 Trust boundaries

| Boundary | Enforced by | Replaces |
|---|---|---|
| Hub platform secrets vs. everything else | Policy `hub-platform` on `kv/data/hub/*`, bound to named hub ServiceAccounts via `auth/kubernetes` roles | `hub-secrets` project membership |
| Workload cluster A vs. B | Separate JWT mount `auth/spoke-<cluster>`, keyed to that cluster's SA signing key. Policy limited to `kv/data/spokes/<cluster>/platform/*` | One Infisical project per cluster + spoke-identity-operator narrowing |
| Tenant app X vs. Y on one cluster | JWT role on `auth/spoke-<cluster>` with `bound_claims.sub = system:serviceaccount:<ns>:<eso-sa>`. Policy on `kv/data/spokes/<cluster>/tenants/<app>/*` only | Shared `spoke-pool-<cluster>` identity copied into each namespace (F1) |
| PKI issuance per cluster | Policy on `pki_int/sign/spoke-<cluster>` only. Role `allowed_domains` limited to that cluster's names (colon-form CNs such as `argocd-agent:<cluster>` verified by G3) | Any PKI identity could request any CN the profile allows |

Paths alone are not the boundary. Each row is only as strong as (a) the policy attached to (b) an auth role whose bound claims identify one workload. The acceptance tests in §13 check denied operations, not just permitted ones.

---

## 4. Capability parity matrix

| Domain | Kamarkat requirement | Current mechanism | OpenBao equivalent | Compatibility | Changes | Limitations / evidence |
|---|---|---|---|---|---|---|
| Static secrets | R1: CRUD, versioning, metadata | Infisical secrets in folders | KV v2 (`kv/`) | Full | Path scheme `kv/data/{hub,spokes/<c>/platform,spokes/<c>/tenants/<app>}/<key>` | [UF] ESO `version: v2` default. Folder-per-key becomes key-per-secret with properties. Infisical-specific keys re-seeded (no production data, §11) |
| Machine identity (hub) | R3 | Universal Auth client ID/secret in `infisical-auth` (platform-ops) and its copy for the issuer | `auth/kubernetes` with local SA token review | Better: no stored client secret | ESO and cert-manager stores use `serviceAccountRef` | [UF] k8s auth uses TokenReview. Reviewer needs `system:auth-delegator`. Leave `kubernetes_ca_cert` unset so a successor cluster works (§9) |
| Machine identity (spoke) | R3 | spoke-identity-operator: identity + Universal Auth secret via CRS, rotation, revocation | `auth/jwt` mount per spoke, `jwt_validation_pubkeys` = public half of CAPI's `<cluster>-sa` | Better: no secret crosses clusters | hub-operator reads `<cluster>-sa`, configures the mount. Deleting the mount revokes | [UF] static pubkeys supported. [AS-1] CAPI KubeadmControlPlane (used by `spokepool-v1` for both providers, [SF]) stores `<cluster>-sa` in `platform-capi`. No TokenReview, so a deleted SA's tokens live until they expire (≤10 min ESO default) |
| Authorization | R2 | Project-scoped built-in roles. Path not enforced ([SF] ADR-031 corr.) | ACL policies on paths, bound per auth role | Better: enforceable in OSS | Policies reconciled by hub-operator | Policies are default-deny. Tests must check denial (§13) |
| Secret delivery | R1 | ESO Infisical provider | ESO `vault` provider (`kv` v2, auth `kubernetes` / `jwt` with `kubernetesServiceAccountToken`) | Full ([UF] ESO docs; an OpenBao provider page also exists, not read) | Rewrite of 3 store templates. Re-pathing of `remoteRef.key` | [UF] ESO notes Vault ≥1.21 requires an audience. Set `audiences: [openbao]` explicitly everywhere |
| Credential generation | R1, R5 | Go generators in hub-operator and CLI | Keep the hub-operator generator for opaque values. Optionally OpenBao password policies (`sys/policies/password/*/generate`) [AS-2] | Equivalent | — | No reason to move non-DB generation. The value is that the store is no longer Infisical |
| Static DB creds | R5 | `RoleManager` ALTER ROLE from ESO Secret (hub). provider-sql (spoke) | Database engine static roles, `rotation_period` (≥5s) | Partial: no overlap (§7.4) | `DatabaseRole` without password. Static role per hub app role | [UF] "OpenBao will rotate the password when creating a static role". No `rotation_schedule` on the fetched page |
| Dynamic DB creds | — (not established today) | none | Database engine dynamic roles + leases | New capability | Only for consumers that re-read credentials (§7.5) | Long-running hub apps cannot use it without changes |
| PKI | R4 | pki-issuer v1.1.0 → Infisical cert-manager API, profiles with TTL caps | `pki_int/` issuing intermediate under an escrow-held root (§8.3). Roles with `max_ttl` replace profiles. cert-manager **built-in** Vault issuer | Full for issuance and renewal | ClusterIssuers rewritten. `internal/pki` profiles become PKI roles | New CA: one-time trust reset at cut-over (§8.3). [UF] cert-manager Vault issuer k8s auth with `serviceAccountRef`, audiences ≥1.15 |
| Revocation | ADR-035: no CRL/OCSP, short TTLs | — | PKI supports CRL/OCSP. Keep it **disabled for consumers** to preserve the ADR-035 decision | Parity | — | Not a scope expansion |
| Audit | R7 | Infisical audit log (licence position at v0.167.12 not verified) | Native `file` audit device to stdout, HMAC-hashed, collected by Alloy into Loki (§9.5) | Full | Audit device in self-init | A failure of every device refuses requests, which is accepted and alerted |
| Administration | R8 | Infisical UI, admin email/password from escrow | OpenBao UI + `auth/oidc` via Zitadel for the declared administrator (ADR-076 amend. 2026-11-10). Break-glass is the escrowed recovery key, used through `generate-root` (§6.5) | Equivalent | — | UI is less friendly for non-operators. That is a product consideration, not a capability gap |
| Recovery | R6 | `infisical-db` archive + `infisical-master-keys` escrow | `openbao-db` archive + seal-key generations, recovery key and root CA in escrow | Equivalent, plus independent recovery scenarios | Escrow artefacts §6.6 | §9 |
| Multi-tenancy | — | Projects | Namespaces exist in OSS 2.7.x ([UF] namespace fixes in OSS release notes). **Not used**: one box, one secret store (ADR-031 header) | n/a | — | Not needed. Avoids scope creep |

---

## 5. OpenBao + CNPG deployment

### 5.1 `openbao-db` (ADR-112 shape)

- Rename `infisical/database/` to `openbao/database/`. It stays a boundary 02 component, for the reason in ADR-112 §7: a database is ordered before its application.
- Worker-only placement (ADR-014). Its own archive and `ScheduledBackup`. The archive credential is `s3-credentials-escrowed`, delivered from the escrow ([SF] ADR-112 §6), unchanged.
- **Roles.** The application-owner path and the login path are separate (§7.2 explains why):

| Role | Declared by | Authenticates with | Password owner | Purpose |
|---|---|---|---|---|
| `openbao_owner` | CNPG `bootstrap.initdb.owner` + `Database.owner` | nothing: `pg_hba` rejects its logins; CNPG keeps `openbao-db-app` | **CNPG** (instance controller) | owns database `openbao`, and through `pg_database_owner` the `public` schema |
| `openbao` | CNPG `DatabaseRole` `openbao-db-openbao` (`login`, `inRoles: [openbao_owner]`, `inherit: true`, `clientCertificate.enabled`, **no `passwordSecret`**) | client certificate | none (CNPG ignores it, [SF] `ApplyPassword`) | OpenBao's runtime connection; creates and owns OpenBao's tables (§5.3) |

- `pg_hba`, in order:
  1. `host all openbao_owner all reject`, so the owner's CNPG-held password is inert;
  2. `hostssl openbao openbao all cert`;
  3. `hostnossl openbao all all reject`.

  No password rule exists for `openbao`.
- `REVOKE CONNECT ON DATABASE openbao FROM PUBLIC` is declared through the `Database` resource where CNPG 1.30.1 supports it. Otherwise this is recorded as a gap pending CNPG #10831, and no Job is added for it.

**Replication ladder (OQ-9, approved).** The same ladder applies to `openbao-db` and to OpenBao:

| | Dev | Staging | Production |
|---|---|---|---|
| CNPG instances | 1 | 2 | 3 |
| OpenBao replicas | 1 | 2 | 3 |
| Synchronous standbys | off | `method: any, number: 1` | `method: any, number: 1` |
| `dataDurability` | n/a (asynchronous) | `preferred` | `required` |
| OpenBao standby reads | disabled | disabled | disabled |

- Staging keeps writing when its only standby is down, and may then lose acknowledged writes on a primary failure.
- Production with `required` does not acknowledge a write until one standby has it. It pauses writes if no standby is available. Losing the primary and that standby together can still lose data.
- **No unconditional zero-data-loss claim is made.**

### 5.2 OpenBao storage stanza

```hcl
storage "postgresql" {
  connection_url           = "postgres://openbao@openbao-db-rw.platform-data.svc:5432/openbao?sslmode=verify-full&sslrootcert=/pg/ca/ca.crt&sslcert=/pg/client/tls.crt&sslkey=/pg/client/tls.key"
  ha_enabled               = "true"
  skip_create_table        = "false"  # OpenBao owns its schema (§5.3)
  max_parallel             = "5"
  transaction_max_parallel = "3"      # below max_parallel: HA lock renewal always has a connection
  max_idle_connections     = "2"
  max_connect_retries      = "0"      # startup Ping only: wait for the database instead of crash-looping
}
disable_standby_reads = true
```

- All keys are parsed at v2.7.3 ([SF] `postgresql.go:115-288`).
- The pool values are GitLab's production defaults ([UF]).
- `max_connect_retries` applies only to the startup `Ping` ([SF] `doRetryConnect`).

### 5.3 Storage schema: owned by OpenBao

**OpenBao owns its storage schema; CNPG owns the PostgreSQL infrastructure.** This closes OQ-2 by ownership.

- With `skip_create_table=false`, OpenBao runs `createTables()` at start: `CREATE TABLE IF NOT EXISTS` for `openbao_kv_store` and its index, and for `openbao_ha_locks` when HA is enabled ([SF] `postgresql.go:314+`, in a REPEATABLE READ transaction). The statements are idempotent, so every replica and every restart may run them.
- **Scoped permissions.** The `openbao` role is not a superuser. It holds no `CREATEDB` or `CREATEROLE`, and it connects to no other database. It gets `CREATE` on `public` only by inheriting `openbao_owner`. On PostgreSQL 15 and later `public` is owned by `pg_database_owner`, which is the database owner ([AS-10], to verify on 18.6).
- No hardcoded upstream DDL, no `postInitApplicationSQL`, no Job and no migration framework. A future OpenBao release that changes the schema does so itself, under the pre-upgrade backup (§9.4).
- The CNCF article's split between an owner role and a read-write runtime role is not adopted. It exists only because that recipe ran the DDL out of band.

### 5.4 PgBouncer: not used for OpenBao (exception to ADR-112 §9)

- ADR-112 §9 justifies a pooler by fleet-proportional connection growth. OpenBao's connections are bounded by `replicas × max_parallel` and do not grow with tenants. [DB]
- OpenBao authenticates with a client certificate, and a CNPG `Pooler` cannot carry that through. In CNPG v1.30.1 the pooler's generated `pg_hba.conf` defaults clients to `md5`. Its server-side certificate is used only for the pooler's own `auth_query` user ([SF] `pkg/management/pgbouncer/config/config.go:94-176`). Putting a pooler in front of OpenBao would bring passwords back. (OQ-3 closed.)
- GitLab's Secrets Manager docs, read in full, do not mention PgBouncer for OpenBao ([UF]).
- The HA lock is a row with `valid_until` in `openbao_ha_locks` ([UF] schema), not a session-scoped advisory lock. So session pooling is not *required* for correctness. [DB] That is a property of the documented schema, not of the code.
- **Decision proposed:** connect directly to `openbao-db-rw`. Record the exception in ADR-112 §9 with these three reasons.

### 5.5 Failover and certificate renewal

- **CNPG failover.** The `-rw` Service moves to the new primary, and `database/sql` replaces broken connections. If the switchover outlasts the 15 s lock TTL (renewed every 5 s, [SF]), the active OpenBao node steps down and the lock is reacquired on the new primary. Behavioural scenario G4 measures this.
- **Database client-certificate renewal: no restart.**
  - OpenBao opens its pool with `sql.Open("pgx", dsn)`.
  - In pgx v5.10.0, the version OpenBao 2.7.3 pins, `stdlib.driverConnector.Connect` runs `pgx.ParseConfig` on every new physical connection, and that re-reads `sslcert` and `sslkey` from disk ([SF] `stdlib/sql.go`, `pgconn/config.go:895,937`).
  - PostgreSQL authenticates only when a connection opens, so connections already open survive both the renewal and the old certificate's expiry.
  - The Secret is mounted as a whole volume, never with `subPath`, so kubelet updates the files.
  - The CNCF article's restart requirement does not apply at the pinned versions.
  - **No Reloader and no restart fallback.** G9 verifies the behaviour.
- CNPG issues 90-day client certificates and renews them about 7 days before expiry ([UF]). Per-role duration is not configurable yet (CNPG #11312).

### 5.6 Placement and resources

| | Current | Proposed |
|---|---|---|
| Store pods | Infisical 1 × (350m / 2000Mi req) [SF] | OpenBao 1/2/3 × (500m / 2Gi req, memory limit = request, CPU limit 2× request), following GitLab's production floor ([UF]) |
| Store DB | `infisical-db` 1 × 100m / 256Mi / 15Gi [SF] | `openbao-db` same floor |
| Pooler | 1 pgbouncer pod [SF ADR-112 §9] | none (§5.4) |
| Redis | shared, Infisical is a consumer | shared, one less consumer |
| Issuer controller | pki-issuer on hub + every spoke | none (cert-manager built-in) |
| Identity operator | spoke-identity-operator on hub | none |

**Cost correction.** The earlier draft proposed a 256Mi request. GitLab's production guidance is a minimum of 2 GB per replica. For the smallest tier (up to 3 secret fetches per second) it specifies 500m CPU, 2 GB memory and 2 replicas. It adds: "OpenBao crashes immediately when it exceeds its memory limit with no graceful degradation" ([UF]). GitLab's memory model is about 1 MB per namespace plus a 1 GB margin. Kamarkat uses no namespaces, so its real floor may be lower, but nothing this proposal has read shows that. Until it is measured:
- at 1 replica, OpenBao costs about the same memory as Infisical, minus the pooler pod and Infisical's Redis traffic;
- at the 2 and 3 replica tiers, OpenBao costs **more**.

The architecture is not cheaper to run. The savings are operational: fewer components and less custom code. The review accepted this.

Minimum topology (a single-worker development hub) fits at 1 replica, with a 2Gi request. HA claims follow ADR-112 §3 (separate nodes and exercised failover) and are not asserted from the replica count.

---

### 5.7 OpenBao listener TLS: independent of OpenBao PKI

**CA.**
- The CLI generates the listener CA at Day-0. It is Class N: a successor hub creates a new one.
- Its private key goes into one Secret in cert-manager's cluster-resource namespace, backing the CA ClusterIssuer `openbao-listener`. Only cert-manager's ServiceAccount can read that Secret.
- No consumer reads any Secret holding a key. Kubernetes RBAC authorizes whole Secrets, so selecting `ca.crt` in an ExternalSecret would still grant the key. The earlier ESO-projection design is withdrawn.

**Public trust material.**
- The CA certificate alone is written as ConfigMap `openbao-listener-trust` into each consumer namespace: `platform-data`, the ESO namespace, the cert-manager namespace and the Gateway's namespace.
- It is written by the same Day-0 CLI delivery that writes `openbao-seal` and `s3-credentials-escrowed`. No trust-distribution controller is added.

**Clients.**
- ESO `caProvider` (type ConfigMap).
- cert-manager Vault issuer `caBundle`, from the ConfigMap's content, rendered at Day-0.
- hub-operator's client.
- The Gateway's backend TLS policy (`caCertificateRefs`: the ConfigMap; hostname `openbao.platform-data.svc`), which checks the server name as well as the chain. [Impl] Confirm the Gateway implementation's BackendTLSPolicy support at its pinned version.

**Lifetimes.**
- CA: 5 years (configurable).
- Listener certificate: 90 days, renewed 30 days before expiry.
- Rollover begins when the CA has under 1 year left, so a listener certificate never outlives its CA. cert-manager's CA issuer does not enforce this by itself.
- CA expiry is exported from the public ConfigMap through the existing observability collectors and alerted. [Impl] Collector placement per ADR-077/078.

**Rollover (ADR-035 dual trust).**
1. The CLI generates the new CA.
2. The ConfigMaps carry old and new.
3. The issuer switches to the new key, and the listener certificate is reissued.
4. Once the old listener certificate has expired, the old CA is removed from the ConfigMaps.

Rotation in response to a compromise follows the same steps compressed: in-cluster TLS to OpenBao is interrupted until the ConfigMaps update.

**Reload.** `tls_auto_reload = true` ([SF] `listener_tcp.go:76`): renewal must take effect without a restart.

## 6. Bootstrap, unseal and escrow

### 6.0 Escrow custody boundary (ADR-117 §3)

**Established facts.**
- **Each box has its own escrow project**, created by `kakt escrow init` as the administrator: "Create the Secret Manager project a box escrows its master keys to"; "a second box … is added to its own project" ([SF] `cmd/kakt/escrow.go:9,21-23`; `kamarkat-docs/cli/escrow-and-encryption.mdx:45,52`).
  - *Correction:* an earlier revision of this record, following ADR-076's 2026-11-17 amendment wording ("one escrow project serves every box"), said one project served every box. The command is authoritative, and that statement is withdrawn.
- The organisation has **one automation identity**, added as `member` to each box's project (ADR-076 amendment 2026-11-10 §4a).
- CI and hub-operator authenticate with that identity, hub-operator through the `ESCROW_PROVIDER_*` variables in its manager manifest ([SF] `operators/hub-operator/config/manager/manager.yaml:135-145`).
- The runtime still writes artefacts it generates itself ([SF] `hubenvironment_controller.go:891-1546`, `spokepool_controller.go:1046`, `application_secret_uploader.go`):
  - `admin-kubeconfig`;
  - spoke PKI;
  - the backup SSE-C key;
  - spoke encryption keys;
  - the `zitadel-masterkey` copy.

  It also writes the Infisical master keys, which this design removes.

**Consequence.** Anything in a box's escrow project is readable by that box's runtime and CI, and by any holder of the organisation's automation credential. Infisical enforces access per project; paths are names, not boundaries (ADR-031 correction). So:

| Project | Members | Holds |
|---|---|---|
| **Box escrow** (existing) | administrator, automation identity | seal-key generations; seal-configuration history; recovery PGP **public** key; PGP-encrypted recovery shares (inert without the private key); the root CA **certificate** (public); `openbao-db` archive credential; the existing runtime-written artefacts |
| **Custody escrow** (new, **one per box**, created by `kakt escrow init`) | **administrator only** | the box's root CA private key and certificate; recovery PGP **private** key |

**Changes and open checks.**
- The runtime's master-key escrow calls (`GenerateInfisicalSecrets`, and the `RestoreMasterKeys`/`BackupMasterKeys` paths in the controller) are removed with Infisical.
- The runtime keeps `member` on the box project for the artefacts it still writes.
- The organisation-wide automation identity is a member of every box's escrow project, so one box's credential can reach another box's escrow project. That is pre-existing, outside this decision, and recorded here. It never reaches a custody project.
- [Impl] The Infisical Cloud plan's project allowance for a second project must be checked. ADR-076 §6 records that Cloud plan entitlements are decided outside the vendored source.

### 6.1 Dependency chain (Day-0, new box)

1. **`kakt escrow init`** runs as the administrator (`kakt login`), as the quickstart already does before any box infrastructure exists. Besides the box escrow project and the automation identity, as today, it **establishes custody**:
   - creates the box's custody project, whose only member is the administrator;
   - generates the box's **root CA**, with the key to custody and the certificate to the box escrow project;
   - generates the **recovery PGP key pair**, with the private key to custody and the public key to the box escrow project, and verifies the pair in memory.

   It is idempotent: an existing custody project, root or PGP pair is found and kept, never regenerated. **No intermediate is generated here**, because OpenBao does not exist yet.
1b. **`kakt box init`** (administrator, existing) generates seal-key generation 1 and its seal-configuration record into the box escrow project, as part of the credentials it already collects.
2. Cluster, CNPG operator, cert-manager and ESO CRDs come up, unchanged.
3. Boundary 02 creates `openbao-db` and the `openbao` `DatabaseRole`. The client certificate comes from CNPG's CA.
4. The CLI generates the listener CA, then writes its key Secret and the public `openbao-listener-trust` ConfigMaps (§5.7). cert-manager issues the listener certificate.
5. The CLI delivers the seal configuration as `openbao-seal`. It is never delivered through ESO.
6. OpenBao starts, creates its tables, auto-unseals, and on empty storage self-initializes:
   - mounts and auth roles, including the one-shot `openbao-trust-bootstrap`;
   - the audit device;
   - base policies;
   - `pki_int/intermediate/generate/internal`, which **generates the intermediate key inside OpenBao and produces a CSR**;
   - revocation of the root token.
7. **Day-0 bootstrap in CI** runs as the automation identity, authenticated to OpenBao as `openbao-trust-bootstrap`. It:
   - (a) reads the CSR from `pki_int` and records it as pending;
   - (b) runs the recovery-key sequence (§6.5).

   Everything that does not depend on the platform PKI proceeds. Certificate-dependent components, such as cert-manager's Vault issuers and spoke agents, wait until the intermediate is signed (step 8).
8. **Intermediate signing (administrator session).** `kakt pki sign-intermediate`, run by the administrator. It may be run immediately after the push: it **waits** until the CSR exists. It then:
   - reaches OpenBao with the box's escrowed `admin-kubeconfig`, which the administrator session can read;
   - authenticates with a short-lived token for the `openbao-pki-signing` ServiceAccount. Only cluster administrators can mint it. Its policy covers only reading the pending CSR and calling `pki_int/intermediate/set-signed`, and it is retained for renewal and compromise replacement;
   - reads the CSR from OpenBao and **validates** it before signing:
     - **box identity**: the CSR's subject and the OpenBao instance are those of the `boxId` the administrator is bound to (ADR-076 active box), and the cluster is that box's management cluster;
     - **CSR**: the key algorithm and size are as declared, the request is for a CA, and there are no unexpected extensions or SANs;
     - **trust hierarchy**: `pki_int` has no default issuer yet, or the existing one chains to the same custody root, and the root certificate in the box project matches the custody key;
   - reads the root key from custody into process memory;
   - signs the CSR (path length 0; validity configurable, bounded by the root's remaining validity);
   - posts **only the certificate chain** to `pki_int/intermediate/set-signed`;
   - sets the issuer's `leaf_not_after_behavior` to `err`, so a leaf that would outlive the intermediate is refused, not silently truncated ([SF] issuer field present at v2.7.3);
   - verifies the chain to the root certificate;
   - zeroes the root key in memory.

   No intermediate private key exists outside OpenBao. The command is idempotent: if a valid signed issuer chaining to the root already exists, it does nothing. Day-0 reports the pending signature and the exact command to run.
9. After step 7b, CI deletes the `openbao-trust-bootstrap` role, its policy and its ServiceAccount. `openbao-pki-signing` is narrower and administrator-mintable only, and it remains for renewal and compromise replacement.
10. hub-operator reconciles policies, mounts, PKI roles, static roles and KV generation. Consumer admission comes from the steady-state manifests (§9.2).

The custody project is read only under the administrator's session: in steps 1 and 8, and for renewal and compromise replacement (§8.3). It is never read in a cluster or in CI.

**Day-0 completion semantics (review decision, Option C).**
- Day-0 runs a phase, `pki-signed`, that waits for the `pki_int` default issuer to chain to the custody root. The wait is bounded and configurable, with a default of 2 hours.
- Meanwhile Day-0's log and job summary show the pending state and point to the CLI documentation for `kakt pki sign-intermediate`.
- **Signed within the wait:** Day-0 continues, certificate-dependent components converge through GitOps, and Day-0 finishes with its normal acceptance.
- **Not signed:** the phase **fails**, reason "administrator signing pending". The workflow fails as an ordinary failed phase: no `continue-on-error`, no weakened checks, and no green run with an unaccepted platform. After signing, re-dispatching the workflow resumes, because phases recorded complete are skipped (ADR-079).
- `kakt verify` stays read-only (ADR-115 unchanged). After signing it reports acceptance or names what is outstanding. It never signs.

**Why signing is a separate command rather than part of one attached journey.** This was evaluated against the code, as the final review asked:
- No `kakt` command pushes the box declaration or stays attached to the CI run. `kakt tenant add-management` prints `git push` for the administrator ([SF] `cmd/kakt/tenant_add_management.go:144,168`).
- Day-0 runs asynchronously from that push ([SF] `internal/kakt-cli/tenant/handover.go:658-660`).
- Attaching signing to the push would need new push-and-watch orchestration.
- Folding signing into `kakt verify` or `kakt kubeconfig` was rejected by the review: it would mix mutation into assessment and access commands.
- So the explicit, idempotent, waiting command is retained. The handover text that tells the administrator to push adds this one command, by reference to the CLI documentation.

**Consequence:** one administrator command per box bootstrap. Certificate-dependent components converge through GitOps once the chain is installed.

### 6.2 Interrupted self-initialization: fail closed

[SF] v2.7.3: an interrupted self-init leaves a marker, and OpenBao then refuses to unseal (`ErrSelfInitFailed`).

**No automatic destruction.** ADR-117 §3 records that Kamarkat has no authoritative lifecycle record that positively establishes a database incarnation as an unactivated, failed fresh-box attempt:
- ADR-111 Revision 6 §6 is a workflow concurrency group;
- ADR-115's assessment is a point-in-time verdict.

**So Day-0:**
- detects the condition from OpenBao's own startup failure;
- **stops**;
- preserves `openbao-db`;
- reports the evidence, and names ADR-092 as the path for an administrator who decides to recreate the store.

No lifecycle database or controller is added to automate this.

Scenario G2 asserts that nothing is deleted in any case.

### 6.3 Seal-key generations and seal-configuration history

- **Generations.** Every generation is escrowed as `openbao-seal-key/<keyId>`, with its key ID. The static wrapper requires a permanent `current_key_id` ([SF] go-kms-wrapping static v2.2.0 `static.go:110`).
- **Seal-configuration history.** Every delivery of `openbao-seal` appends a record to the box project:
  - a configuration ID;
  - `current` and `previous` key IDs;
  - the delivery time;
  - the OpenBao version then running.

  This history is the record of which seal configuration was in force at any recovery point.
- **Rotation.** Generation n+1 is added, then configuration `{current: n+1, previous: n}` is delivered and recorded. Once the re-wrap is verified, `{current: n+1}` is delivered and recorded. Do not rotate again before the re-wrap completes ([UF] GitLab).
- **Retention.** A generation is deleted only when no retained `openbao-db` backup precedes the time of the first configuration that excluded it.
- **Restore.** Restoring to a recovery point at time t delivers **the configuration recorded as in force at t**, for example `{current: n+1, previous: n}` if t fell inside a rotation. Rotating forward to the newest generation afterwards is an ordinary rotation. No single configuration is prescribed for every historical backup.

### 6.4 Accepted seal posture

The properties are as in ADR-117 §3:
- Secrets encrypted at rest;
- RBAC on `openbao-seal` limited to the OpenBao ServiceAccount and the CLI's delivery identity;
- off-box custody of every generation;
- historical decryptability through §6.3;
- no key material in Git, logs, process arguments or status.

**Not claimed:** equivalence with a hardware-backed KMS against runtime cluster compromise.

### 6.5 Recovery key (crash-safe, PGP-encrypted)

The actor is the CLI in CI (step 7b), authenticated as `openbao-trust-bootstrap`. It needs only the box project.

| Step | Action | Re-entry rule |
|---|---|---|
| 1 | Read the recovery PGP **public** key from the box project. It was generated and verified at `kakt escrow init`; the private half is in custody | missing → fail closed with an error naming `kakt escrow init` |
| 2 | `POST sys/rotate/recovery/init` with `secret_shares` 1, `secret_threshold` 1, `pgp_keys: [public]`, `backup: true` ([SF] `logical_system_rotate.go`) | if OpenBao already has a recovery key, skip |
| 3 | `GET sys/rotate/recovery/backup` returns the encrypted shares ([SF] `:899`) | if the backup is gone and step 4's artefact is absent → **fail closed** |
| 4 | Escrow `openbao-recovery-key-encrypted` to the box project: ciphertext plus key fingerprint | if present with a matching fingerprint, continue |
| 5 | **Verify** without decrypting: the escrowed ciphertext is byte-identical to OpenBao's backup, and its PGP recipient fingerprint equals the custody key's public fingerprint. The private key's ability to decrypt was proven at `kakt escrow init`. A full decrypt check runs whenever the administrator next uses a session (`kakt` verifies recoverability opportunistically) | repeat until it passes |
| 6 | `DELETE sys/rotate/recovery/backup`, then delete the bootstrap role, policy and ServiceAccount | tolerate "already absent" |

Runtime and CI identities can read only ciphertext.

### 6.6 External escrow contract changes

| ADR-076 artefact | Change |
|---|---|
| `infisical-master-keys` | replaced by `openbao-seal-key/<keyId>` and the seal-configuration history (box project) |
| `infisical-admin-credential` | replaced by `openbao-recovery-pgp-public` and `openbao-recovery-key-encrypted` (box project), and `openbao-recovery-pgp-private` (custody) |
| (new) `pki-root` | key in the custody project only. Its public certificate is in the box project |
| runtime escrow permission for master keys | removed |
| everything else | unchanged |

## 7. Native database credential lifecycle

### 7.1 Why omitting `passwordSecret` is not enough

There are two CNPG password paths. They are separate, and both were verified at v1.30.1.

1. **The declarative role path.** A `DatabaseRole` with no `passwordSecret` and `disablePassword` unset makes `ApplyPassword` set `ignorePassword = true`, so no password is issued ([SF] `roles/contract.go:138`).
2. **The application-owner path.** Whenever `ShouldCreateApplicationDatabase()` is true, the instance controller's `refreshCredentialsFromSecret` calls `reconcileUser(GetApplicationDatabaseOwner(), GetApplicationSecretName())`. That re-applies the initdb owner's password from `<cluster>-app`, or from `initdb.secret` when one is set ([SF] `instance_controller.go:1427-1460`). Omitting `passwordSecret` on a `DatabaseRole` does not touch this path.

**The current defect, located.**
- `zitadel-db`, `credits-db` and `grafana-db` make `hub_zitadel`, `hub_credits` and `hub_grafana` the **initdb owner** ([SF] `*/database/*-db.yaml`, `bootstrap.initdb.owner`). They set no `initdb.secret`, so CNPG applies the password from its generated `<cluster>-app`.
- At the same time, `RoleManager` applies the ESO-delivered password from `hub-<app>-db-credentials` ([SF] `roles.go`; `environments/base/hubenvironment.yaml:63-96`).
- So two controllers write one role's password.

### 7.2 Target ownership: owner identity separated from login identity

| Responsibility | Owner |
|---|---|
| Database lifecycle | CNPG |
| Database-ownership role `<app>_owner` (initdb owner; its password lives in `<cluster>-app`, has **no consumer**, and cannot authenticate, because `pg_hba` rejects every owner-role login) | CNPG (instance controller) |
| Login role `hub_<app>`: existence, `LOGIN`, `inRoles: [<app>_owner]` with `inherit: true`, connection limit | CNPG `DatabaseRole` `<cluster>-hub-<app>`, **no `passwordSecret`** |
| Login role password | OpenBao static role `hub-<app>` |
| The login role's **certificate**, used only by OpenBao to rotate the role's own password (§7.3) | CNPG (`clientCertificate` on the same `DatabaseRole`) |
| Credential delivery | ESO (`VaultDynamicSecret`, `GET database/static-creds/hub-<app>`) |
| Application rollout on password change | Reloader |

- **Application privileges are preserved.** `hub_<app>` inherits `<app>_owner`. It therefore keeps ownership-level rights on the database and on `public` (on PostgreSQL 15+ that schema is owned by `pg_database_owner`), so applications that migrate themselves continue to work. These are Zitadel's migrations, Grafana's, and the billing engine's `migrate` initContainer ([SF] ADR-112 §9).
- Objects an application creates are owned by `hub_<app>`. The role name is permanent, because static roles rotate the password, not the name. [AS-10] Verify on 18.6 that inherited `pg_database_owner` membership grants `CREATE` on `public`.
- **Resource names are unique per database.** Each `DatabaseRole` name carries its cluster prefix (`zitadel-db-hub-zitadel`, `credits-db-hub-credits`, `grafana-db-hub-grafana`, `openbao-db-openbao`). Every one of them lives in `platform-data`, and CNPG writes `<DatabaseRole-name>-client-cert` (CRD description), so the certificate Secrets are unique too. Each is mounted into the OpenBao pods at `/pg/<cluster>/`.
- **Migration of the existing clusters.** Changing `initdb.owner` affects only a new cluster. With no production users (brief §11), the clusters are recreated empty in the replacement release, as ADR-112 §5 did. No data migration is proposed.

### 7.3 How OpenBao rotates a password without a privileged rotator

**Constraint.** On PostgreSQL 16 and later, `CREATEROLE` alone cannot alter an existing role. The altering role also needs `ADMIN OPTION` on it ([UF] PostgreSQL docs). CNPG 1.30.1 grants `inRoles` as a plain `GRANT %s TO %s`, with no `ADMIN OPTION` ([SF] `roles/postgres.go:266`), and has no field for it. A rotator role therefore has no declarative owner for its essential grant. Issuing that grant from bootstrap SQL would be a second, non-reconciling owner.

**Design: self-rotation.** PostgreSQL lets every role change its own password, with no `CREATEROLE` and no `ADMIN OPTION`. OpenBao's database connection for each application therefore authenticates **as `hub_<app>` itself**, by certificate, and **only to the `postgres` maintenance database**. Roles are cluster-wide, so `ALTER ROLE` works from any database.

```
# pg_hba on <app>-db (order matters; first match wins)
hostssl postgres  hub_<app>  all  cert            # OpenBao, certificate only
hostssl <appdb>   hub_<app>  all  scram-sha-256   # the application (directly or via PgBouncer)
```

```
database/config/<app>:
  connection_url = postgres://hub_<app>@<cluster>-rw.platform-data.svc:5432/postgres?sslmode=verify-full&sslcert=/pg/<cluster>/tls.crt&sslkey=/pg/<cluster>/tls.key&sslrootcert=/pg/<cluster>/ca.crt
  allowed_roles  = hub-<app>
database/static-roles/hub-<app>:
  db_name = <app>, username = hub_<app>, rotation_period = 720h
  (default rotation statement: ALTER ROLE "{{username}}" WITH PASSWORD '{{password}}', [SF] postgresql.go:31-32)
```

**Privilege analysis.**
- The certificate grants a connection only to `postgres` as `hub_<app>`, where the role owns nothing.
- Its only material power is setting that role's password, and so becoming it.
- Any rotator able to change the password has exactly that power too.
- So this grants **no more** than a minimally privileged rotator, and **less** than one holding `CREATEROLE`.
- There is no `SUPERUSER`, no `CREATEROLE`, no `ADMIN OPTION` and no extra role.
- The certificate Secret is mounted only into OpenBao's pods. RBAC does not let any other ServiceAccount read it (§3.3).

**Why OpenBao's warning does not apply.** OpenBao warns: "Do not manage the same root database credentials that you provide to OpenBao in config/ with static roles" ([UF]). That warning is about a *password* configured as the connection credential: rotating it would break the engine's own connection. Here the connection uses no password, and rotating `hub_<app>`'s password does not affect certificate authentication. [AS-11] Verify in scenario G6 that a static role whose username equals the connection user rotates and keeps connecting at v2.7.3.

**No root rotation.** The engine holds no password, so `rotate-root` is never used.

### 7.4 Complete database credential inventory

Each credential has one generator, one System of Record (SoR), one lifecycle owner, one SQL reconciler and one delivery path, and values flow one way. **Nothing is published from a cluster into OpenBao**; PushSecret stays banned.

| Credential | Generator | SoR | Lifecycle owner | SQL reconciler | Delivery |
|---|---|---|---|---|---|
| `hub_zitadel`, `hub_credits`, `hub_grafana` login password | OpenBao static role | OpenBao | OpenBao | OpenBao (self-rotation, §7.3) | ESO `VaultDynamicSecret` `GET database/static-creds/hub-<app>`, then Secret, then Reloader |
| `hub_<app>` existence, `inRoles: [<app>_owner]`, client certificate | CNPG `DatabaseRole` (no `passwordSecret`) | Kubernetes API | CNPG | CNPG | certificate Secret mounted into OpenBao pods only |
| `<app>_owner` and `openbao_owner` (initdb owners) | CNPG (`<cluster>-app`) | — (inert) | CNPG | CNPG instance controller | **none**. `pg_hba` rejects every connection by an owner role (`host all <owner> all reject`, before any other rule), so the CNPG-held password cannot authenticate and is not secret material of consequence. Ownership rights reach the application through `hub_<app>`'s inherited membership. CNPG manages the cluster through its superuser over the local socket, unaffected |
| `openbao` storage login | CNPG client CA (certificate) | Kubernetes API (CNPG-native PKI, ADR-035 bounded exception) | CNPG | — | mounted into OpenBao |
| `hub_billing` (superuser, initdb owner and login) | hub-operator, once | OpenBao KV | hub-operator (never regenerates, ADR-003 §1) | CNPG instance controller, from `initdb.secret` | ESO, then `hub-billing-db-credentials`, then CNPG and the billing engine |
| spoke tenant role passwords | hub-operator, once | OpenBao KV `spokes/<c>/tenants/<app>` | hub-operator | Crossplane `provider-sql` | ESO on the spoke |
| spoke `crossplane-admin` | hub-operator, once | OpenBao KV `spokes/<c>/platform` | hub-operator | spoke CNPG `managed.roles` | ESO on the spoke |
| `postgres` superuser, `streaming_replica`, `cnpg_pooler_pgbouncer` | CNPG | Kubernetes API (CNPG-native) | CNPG | CNPG | CNPG only |

Changing `hub_billing` away from `SUPERUSER`, which would let it move to OpenBao self-rotation, is a billing-engine decision outside this scope.

### 7.5 Rotation and availability semantics

ADR-030 is corrected to state the following:
- **Eventual convergence.** After OpenBao rotates `hub_<app>`:
  1. ESO's next refresh delivers the new value;
  2. Reloader rolls the consumer;
  3. the consumer reconnects with the new password.
- **Existing sessions are preserved.** PostgreSQL authenticates only when a connection starts, so sessions that are already authenticated continue.
- **New authentication attempts may fail temporarily.** Between the rotation and the consumer's rollout, a new connection made with the old password fails. That includes connections opened by PgBouncer on the consumer's behalf.
  - The duration is *expected* to be about the ESO refresh interval plus rollout time when reconciliation succeeds.
  - It is **not bounded**: a failed ESO refresh or a failed rollout extends it.
- **Observable and self-recovering.** The failure surfaces in:
  - the ExternalSecret's `Ready` condition;
  - the rollout status;
  - application connection errors.

  Recovery is the existing reconciliation; no human step is designed in.
- **No unconditional zero-downtime guarantee.**
- No dual login roles and no rotation coordinator.
- 30 days is the initial policy, not an invariant.

### 7.6 Dynamic credentials

The capability is available, and consumer adoption is out of scope. It is not used by any platform application until lease recovery and orphaned-role semantics after independent restores are established (§9).

## 8. PKI and machine identity

### 8.1 What is eliminated

| Item | Why it can go |
|---|---|
| `pki-issuer` v1.1.0 + CRDs + approver RBAC, on hub and every spoke | cert-manager's built-in Vault issuer ([UF]) |
| spoke-identity-operator (whole) | JWT auth per spoke. Mount deletion is the revocation (ADR-035 deletion step 3 becomes "hub-operator deletes `auth/spoke-<c>`") |
| CRS wrapper for identity credentials | no client secret exists to deliver |
| CLI `cert_profiles.go`, profile self-heal in `hubenvironment_controller.go:1411-1445` | PKI roles from self-init + hub-operator |
| Membership inventory and validators 031/037 for Infisical | replaced by behavior tests on policy denial |

### 8.2 Kept

- The ArgoCD agent bootstrap `Certificate` per spoke (hub-operator declares it, cert-manager issues it, CRS delivers it: ADR-035's bounded transit exception).
- The CA's TTL ceilings, expressed as PKI role `max_ttl`. `internal/pki/profiles.go` becomes the role list.
- Support Agent and Alloy client certs.
- Argo CD principal certs and the `argocd-agent-jwt` signing key (`signing-keys`, 3650d cap).

### 8.3 Root and intermediate CA custody

```
Root CA ── key only in the custody escrow project (administrator only)
   │ signs a CSR, under the administrator's session (`kakt pki sign-intermediate`)
   ▼
OpenBao pki_int ── intermediate key generated inside OpenBao (generate/internal);
   │               only the CSR leaves; only the signed chain returns (set-signed)
   ▼
pki_int roles (subject, SAN, usage, TTL restrictions) ──▶ cert-manager Vault issuer ──▶ leaves
```

This is OpenBao's recommended pattern: generate the intermediate key internally and export only the CSR ([UF] OpenBao PKI documentation). No intermediate key passes through the escrow, CI or the CLI.

**Validity.**
- Root: configurable, 20 years by default.
- Intermediate: configurable, and always bounded by the root's remaining validity. The default is 5 years. It is **not** an architectural invariant.
- The issuer's `leaf_not_after_behavior` prevents leaves outliving the intermediate.
- Leaf ceilings follow `internal/pki/profiles.go`. Each issuing role's `max_ttl` stays below the intermediate's remaining life at signing.
- **Signing certificates (review decision).** The `signing-keys` ceiling is **reduced** from 3650 days to **1 year** (configurable). The intermediate is not lengthened to fit it.
  - Certificate lifetime and signing-key lifetime are decided separately.
  - `argocd-agent-jwt` is used today with `duration: 43800h` (5 years), `renewBefore: 720h`, RSA 4096, and no `rotationPolicy`, so cert-manager's default applies ([SF] `manifests/argocd-principal/certificates.yaml:67-86`). It becomes `duration: 8760h`, `renewBefore: 720h`, with `privateKey.rotationPolicy: Always`. The key rotates with the certificate.
  - That key signs the session tokens the Argo CD principal issues to agents. A rotation invalidates outstanding session tokens, and agents re-authenticate over mTLS. [Impl] Verify that contract against the pinned argocd-agent, and confirm the principal reloads the renewed key through the existing restart-on-renewal mechanism (ADR-035 addendum §4).
  - If a consumer is shown to need key continuity across renewals, that consumer alone uses `rotationPolicy: Never`, and its key lifetime is recorded. The certificate still renews within the intermediate.
  - The issuer's `leaf_not_after_behavior: err` makes any request beyond the intermediate fail visibly.

**Renewal versus compromise.**

| Event | Procedure |
|---|---|
| Scheduled intermediate renewal | (1) OpenBao generates a new key and CSR as a second issuer in the multi-issuer mount ([SF] `path_manage_issuers.go`). (2) The administrator signs it (`kakt pki sign-intermediate`). (3) ADR-035 dual-trust overlap: the trust bundle holds the root, which is unchanged, so the overlap concerns only which issuer signs. (4) Switch the default issuer. (5) Retire the old issuer after its leaves expire. Triggered when the remaining life falls below a configured threshold, and alerted |
| Intermediate private-key compromise | Relying parties trust the **root**, so a new intermediate does not stop them accepting certificates signed with the compromised key. Short leaf lifetimes do not contain an issuer compromise. **Root rollover is mandatory**, under ADR-035's emergency fast path: (1) the administrator creates a new root (custody) and signs a new intermediate CSR; (2) every trust bundle on the hub and the spokes is replaced with the new root only, and the old root is removed; (3) every leaf is reissued from the new intermediate; (4) consumers reload. Fleet-wide mTLS is interrupted until each component has both the new bundle and a new leaf. That cost is stated |
| Root compromise | Identical trust replacement |

Ordinary leaf revocation stays by short lifetime, with no CRL or OCSP for consumers (ADR-035).

**Custody boundary.**
- Runtime and CI identities are not members of the custody project, so they cannot retrieve root material.
- **Exposed to:** the administrator's escrow account, the escrow provider, and the administrator's workstation during signing.
- **Not provided:** hardware non-exportability, tamper evidence or quorum control. Offline or HSM signing remains possible, because the CSR can be signed by any external CA process and posted back.

**Issuance restrictions.** These are per role, and every field exists at v2.7.3 ([SF] `pki/path_roles.go`):

| Role | Constraints |
|---|---|
| `spoke-<c>-client` | `allowed_domains` = the exact client identities of cluster c (`argocd-agent:<c>`, `alloy:<c>`, `support-agent:<c>`), `allow_bare_domains` true, `allow_subdomains`/`allow_glob_domains`/`allow_wildcard_certificates`/`allow_any_name`/`allow_ip_sans` false, `cn_validations` disabled (CNs are not hostnames), `allowed_uri_sans`/`allowed_other_sans` empty, `client_flag` true, `server_flag` false, `ext_key_usage` clientAuth, `max_ttl` 24h |
| `hub-server` | exact hub service names; server only; `max_ttl` 7 days |
| `hub-client` | exact hub client identities; client only; `max_ttl` 24h |
| `signing-keys` | the exact CN `argocd-agent-jwt`; digital-signature usage only; `max_ttl` 1 year (configurable), always inside the intermediate |
| `argocd-agent-bootstrap` | `argocd-agent:<c>` per cluster; client only; `max_ttl` 72h |

Each spoke's issuer policy allows `pki_int/sign/spoke-<its cluster>-client` only. **A spoke issuer can never request another cluster's identity** (G3).

### 8.4 JWT identity, token lifecycle and revocation (spokes)

| Element | Contract |
|---|---|
| Mount | `auth/spoke-<cluster>`, one per workload cluster, created with the `SpokePool` |
| Verification key | `jwt_validation_pubkeys` = the public `tls.crt` of CAPI `<cluster>-sa` ([UF] Cluster API book). The reader holds `get` on that Secret and never reads, copies or logs `tls.key`. [Impl] Confirm the pinned KubeadmControlPlane follows the convention |
| Role binding | exact `bound_subject` (`system:serviceaccount:<ns>:<sa>`), `bound_issuer`, `bound_audiences: [openbao]` |
| OpenBao token | **service** token. Role: `token_ttl` 10m, `token_max_ttl` 10m, `token_explicit_max_ttl` 10m, `token_period` unset (non-renewable beyond the explicit maximum). Mount tune: `max_lease_ttl` 10m |
| JWT validation | Explicit `clock_skew_leeway` 30s, `expiration_leeway` 30s, `not_before_leeway` 30s. **0 is never used**: at v2.7.3 it selects the defaults, which are 150s for expiration and not-before (`claimDefaultLeeway`, [SF] `credential/jwt/path_role.go:26,115-131`) and 60s for clock skew (`jwt.DefaultLeeway`; [UF] OpenBao JWT API). −1 disables a leeway. The **effective combined allowance**, which depends on whether clock-skew and expiration leeway add, is measured in G3 and used in the bound below. No precise bound is claimed until then |
| Kubernetes token lifetime | OpenBao's JWT login cannot cap a presented token's age: `max_age` applies only to the OIDC flow ([SF] `path_oidc.go:873`). Enforced on each **spoke API server** through the `spokepool-v1` ClusterClass (review decision): `--service-account-max-token-expiration=1h`, the minimum Kubernetes accepts, and `--service-account-extend-token-expiration=false`, so projected tokens are never extended beyond it. ESO and cert-manager request 10 minutes. [Impl] Before changing the ClusterClass, confirm every existing spoke consumer refreshes projected tokens within 1 hour (the kubelet does; long-lived clients must use a current client library) |
| Legacy tokens | Secret-based legacy ServiceAccount tokens are rejected: roles require `bound_audiences: [openbao]` and `bound_issuer` = the spoke's service-account issuer, and legacy tokens carry neither |
| Residual after an independent SA deletion | ≤ maximum Kubernetes token lifetime (1h) + effective JWT validation allowance (measured; ≤ 60s by configuration if the two leeways add) + `token_explicit_max_ttl` (10m). **About 70 minutes plus the allowance. Accepted by the review.** It applies only when a ServiceAccount is deleted outside the OpenBao authorization lifecycle; deleting a tenant or a spoke cuts access off immediately (rows below) |
| Policy name uniqueness | Per-tenant and per-cluster policy and role names embed the owning object's immutable UID (`tenant-<appUID>`, `spoke-<clusterUID>`). A recreated tenant with the same name gets a new UID, so old tokens referencing the deleted policy name never regain authority |
| Platform-identity impersonation | Platform roles bind only ServiceAccounts in platform namespaces. Tenants cannot schedule workloads there and hold no `serviceaccounts/token` create outside their own namespace (ADR-021 AppProject and RBAC boundary). G3 asserts a tenant cannot obtain a platform token |
| Tenant application deleted | Delete its **policy** and role. Policies are resolved by name on every request, and a missing policy contributes nothing ([SF] `policy/policy_store.go:608-614`), so existing tokens lose authority on their next request. Role deletion alone "does not revoke tokens issued by this role" ([SF] `website/content/docs/api/auth/jwt.mdx:807`) and is never relied on alone |
| Spoke deleted | Disable `auth/spoke-<cluster>`, which revokes the tokens it issued ([UF] docs; [AS-12] verified by G3), and delete its policies |
| Signing key rotated | The verification-key list carries old and new keys, and the old one is removed when CAPI retires it |
| OpenBao restored | §9.2: policies rewritten to desired state, and every restored service token revoked |
| Hub identities | `auth/kubernetes` with TokenReview against the local API server. Deleting a ServiceAccount takes effect at once. `kubernetes_ca_cert` unset, for successor hubs |

## 9. Backup, recovery, audit and availability

### 9.1 Recovery state

| State | Where it lives | Recovery authority | Class |
|---|---|---|---|
| KV values, policies, auth configuration, `pki_int` key and issuers, DB engine configuration, leases, tokens, recovery records | `openbao-db` (barrier-encrypted) | `openbao-db` archive (CNPG/Barman, PITR) | B |
| Seal-key generations and seal-configuration history; encrypted recovery shares and PGP public key; archive credential | box escrow project | Infisical Cloud | E |
| Root CA; recovery PGP private key | custody escrow project | Infisical Cloud (administrator only) | E |
| Listener CA and certificate | cert-manager | recreated | N |
| Spoke SA public keys | CAPI `<cluster>-sa` | Class M, then reconciled | M |
| OpenBao configuration, self-init, desired authorization | Git and hub-operator | GitOps | R |

### 9.2 Restoring OpenBao: records, admission and revoked access

**Recovery incarnation and durable records.**
- A restore creates a new `Cluster`, so the UID changes (ADR-092: `spec.bootstrap` is read once). That identifies a recovery incarnation. It does **not** show that recovery is complete.
- hub-operator writes a step-by-step record to `kv/platform/recovery/<cluster>/<uid>`, one entry per completed step.
- A new UID has no record in any earlier state, so a restored store never carries a "complete" record for itself.
- After interruption, the steps resume from the record. They never run on ordinary reconciliation.

**Admission: ADR-092's existing orchestration, not a controller.**
- OpenBao's consumer admission is the `openbao-consumers` NetworkPolicy, which admits ESO, cert-manager and the Gateway. It is part of the component's **steady-state** manifests.
- The component's `database-recovery/` overlay, the deliberate recovery mode ADR-092 governs, omits it. While the store is in recovery mode, only hub-operator, the OpenBao peers and the CLI reach it.
- ADR-092's sequence is: a human enters recovery, the restore runs, the restored cluster is verified, and the human settles the manifest. "Verified" now includes "the incarnation's recovery record is complete".
- The existing exit invariant (preflight) proves no recovery mode remains.

**Recovery authority.**
- Steps run under the **recovery identity**: Kubernetes-auth role `openbao-recovery`, bound to ServiceAccount `openbao-recovery` in `platform-data`. Only cluster administrators can create its tokens (RBAC).
- Its policy covers only: `auth/token/accessors` (list), `auth/token/revoke-accessor`, `database/rotate-role/*`, `database/static-roles/*` (read), `kv/data/platform/recovery/*`.
- It is driven by `kakt` within ADR-092's administrator-entered recovery mode. It authenticates **freshly** after the restore. No controller or workflow engine is added.
- hub-operator keeps reconciling desired authorization as it always does. It is an ordinary client, and logs in again after its restored token is revoked.

**Steps for an `openbao-db` recovery incarnation.** Each is recorded only after its **effect is verified**:

| # | Step | Effect verified before recording |
|---|---|---|
| 1 | Desired authorization applied (hub-operator's normal reconcile on the new incarnation) | every policy, role and mount equals desired state. hub-operator records the incarnation UID it reconciled against |
| 2 | Revoke every service-token accessor except the recovery session's own ([SF] `token_store.go:239,448`) | an accessor listing shows only tokens issued after step 2 began |
| 3 | Spoke verification keys rewritten from current CAPI `tls.crt` | each mount's `jwt_validation_pubkeys` equals its source |
| 4 | Static passwords re-established for every application database **not** itself in recovery | each `database/static-roles/<r>` reports `last_vault_rotation` after the incarnation began, **and** a login with the delivered credential succeeds |
| 5 | Generated values checked | missing generated KV values are listed; none is regenerated (ADR-003 §1) |

Admission returns when the administrator settles the manifest, after the record is complete (ADR-092).

**Ordering when application databases are restored as well**, for example in hub reconstruction or a multi-database restore:
1. Restore `openbao-db`, then steps 1–3 and 5.
2. Restore each application database. **Its restore completes first.**
3. Only then run step 4 for that database: rotate, verify `last_vault_rotation`, verify a login.

Rotating before the restore completes would let the restore bring back the old hash. Each application stays unavailable until its own database recovery and credential reconciliation are both recorded complete: its database is in recovery mode, and it cannot authenticate until step 4.

**An application database restored alone:** its restore completes, then step 4 runs for that role only, recorded under `kv/platform/recovery/<cluster>/<uid>`.

**Hub reconstruction** (ADR-111):
1. Escrow material.
2. `openbao-db` restored in recovery mode.
3. Auto-unseal with the recorded seal configuration (§6.3).
4. Steps 1–3 and 5.
5. Application databases restored.
6. Step 4 per database.
7. Manifests settled, and admission returns.

The trust domain is preserved by the restored `pki_int` and the unchanged spoke keys. The root stays in custody.

### 9.3 Seal configuration on restore

§6.3: the configuration recorded as in force at the recovery point is delivered. A configuration is never inferred.

### 9.4 Backup before an OpenBao version change

This is owned by the OpenBao component's declarative upgrade contract (ADR-063/068: a box upgrades by syncing a new bundle version).

- **Declaration.** The component declares one CNPG `Backup` resource whose **name** is unique to the upgrade operation: `openbao-db-pre-<openbaoVersion>-<bundleVersion>-<serverName>`.
  - All three values are known at render time.
  - The bundle version is distinct per release (ADR-068).
  - `serverName` (`openbao-db-vN`) is bumped whenever a restore recreates the cluster (ADR-112).
  - CNPG gives a manually created `Backup` no owner and never resets a completed one ([SF] CNPG v1.30.1 `backup_controller.go:152-155`). A name keyed only by OpenBao version could therefore be satisfied by an older completed backup, for example when the same bundle is re-applied after a rollback. This naming makes that impossible. It sits in an earlier Argo CD sync wave than the OpenBao StatefulSet. A health check for `postgresql.cnpg.io/Backup` (`completed` = Healthy, `failed` = Degraded) makes the later wave wait.
- **Only on a version change.**
  - A routine sync, a configuration change or self-healing finds the `Backup` of that name already present and unchanged, so no backup is taken.
  - A version change renders a new name. Argo creates it, waits for `completed`, and only then rolls OpenBao.
  - A failed backup leaves the old version running.
  - The previous version's `Backup` object is pruned. Pruning the object does not delete the backup data, which follows the cluster's retention ([Impl] verify against CNPG v1.30.1).
- **Recorded with the backup.** It carries the target and running versions as labels. The seal configuration in force is taken from §6.3's history at its completion time.
- **Rollback.**
  1. ADR-092 recovery from that `Backup`.
  2. Pin the running version recorded on it.
  3. Deliver the recorded seal configuration.
  4. §9.2's steps.

  Reverting the chart alone does not revert storage. Writes after the backup are lost.
- No hook, controller, release gate or upgrade framework is added.

### 9.5 Audit (ADR-117 §8)

| Requirement | Design |
|---|---|
| Enabled before consumers | The `file` audit device, `file_path=stdout`, is enabled in self-init ([SF] `builtin/audit/file/backend.go:41`) |
| Coverage | API requests and responses, including auth logins, KV reads and writes, `static-creds` reads, PKI issuance, and `sys/*` and policy changes. **Excludes** endpoints OpenBao documents as unaudited (for example unauthenticated health and seal-status). No claim of coverage of every endpoint |
| Value protection | HMAC hashing, the default. `log_raw` is never set ([SF] `backend.go:72`). `hmac_accessor` stays at its default |
| Path to storage | stdout, then the container log, collected asynchronously by Alloy into Loki (ADR-078). No request waits on Loki. **Best effort:** durable retention of every emitted record is not guaranteed |
| Failure | If no device records a request, OpenBao refuses it ([SF] `audit_broker.go:166,231`). The `audit.log_request_failure` counter ([SF] `:130`) is scraped and alerted through the existing alerting |
| Retention and access | Loki stream labelled as security audit. Retention follows ADR-078 for security-relevant logs. Read access restricted to the box's administrators through ADR-078's access controls |
| Stated limitation | Node-local log rotation can outpace collection under extreme load. This is observable as collection lag |

## 10. Versions, licensing and compatibility

### 10.1 Proposed pins (latest compatible)

| Component | Pin | Basis |
|---|---|---|
| OpenBao | **2.7.3** | [UF] GitHub "Latest", assets 2026-11-19. Includes GHSA-fgmw-h9wp-wc7g. Leader-election HA predates 2.7 ([SF] v2.6.1); the 2.7 line adds the HA lock-starvation fix GH-3913. 2.7.2 (2026-10-26) is older than two weeks but lacks the advisory fix, so 2.7.3 is approved (review §1) and verified by the §13 scenarios |
| openbao-helm chart | **0.30.8** | [UF] 2026-11-19, "bump OpenBao to 2.7.3". Image pinned explicitly, as Infisical's is today |
| CloudNativePG | 1.30.1 (unchanged) | [SF] inventory. `DatabaseRole.clientCertificate` present in the vendored CRD |
| PostgreSQL (`openbao-db`) | 18.6 (unchanged) | [SF]. OpenBao minimum 9.5 [UF] |
| External Secrets Operator | v2.15.0 (unchanged) | [SF]. `vault` provider, `VaultDynamicSecret` generator [AS-7] |
| cert-manager | v1.21.3 (unchanged) | [SF]. Vault issuer `serviceAccountRef.audiences` needs ≥1.15 [UF] |
| Reloader | v1.4.24 (unchanged) | [SF] |
| Removed | Infisical v0.167.12, chart `infisical-standalone` 1.12.2, pki-issuer v1.1.0 | — |

The three parity gates from the upgrade skill (`verify-vendor-digest.sh`, `validate-cluster-name-contract.sh`, `go test ./internal/kakt-cli/versions/...`) run before and after the pin change.

### 10.2 Licence (MPL-2.0, approved)

- **The obligations follow distribution, not modification.**
  - MPL-2.0 §3.2 requires anyone distributing a Covered Software executable to tell recipients how they can obtain the Source Code Form.
  - §3.1 requires that distributed Source Code Form carry the licence.
  - Using unmodified upstream binaries does **not** remove §3.2 when Kamarkat itself distributes them. The earlier draft's statement otherwise is withdrawn.
- **What Kamarkat distributes**, determined per artefact:
  - **Container images.** The bundle *references* upstream images by registry and tag or digest. If the bundle only references them, Kamarkat does not distribute the executable. If a box or a release process **mirrors** the image, for example into a tenant registry through `registry-credential` (ADR-076), Kamarkat or the tenant is redistributing it, and §3.2 applies. [Impl] Determine for OpenBao which path the bundle uses.
  - **The Helm chart.** `openbao-helm` is MPL-2.0 ([Impl] confirm from its `LICENSE`). If the chart is vendored into the bundle, as Kamarkat vendors other charts, that is distribution of Source Code Form, and the chart's `LICENSE` file ships unchanged alongside it.
- **The record**, in the existing component inventory (`manifests/architecture/component-catalog.yaml`, rendered into `components.json`). No new licensing framework is added. The entry carries:
  - `license: MPL-2.0`;
  - `repo: openbao/openbao`, which is the "how to obtain the source" notice;
  - the exact version and image digest from the release inventory, which give version provenance;
  - and for the chart, its own entry with `repo: openbao/openbao-helm` and its version.

  The release documentation generated from the catalog is the recipient-facing notice.
- `Infisical` and `Infisical PKI issuer` are removed from the catalog. Infisical Cloud (escrow) is a SaaS in the tenant's account and is not distributed.

## 11. Change and removal inventory

### 11.1 Changed

| Path | Change |
|---|---|
| `manifests/hub-core-services/infisical/` → `openbao/` | OpenBao chart values (namespace `platform-data`), HCL, self-init, NetworkPolicy |
| `…/infisical/database{,-recovery}/` → `openbao/database{,-recovery}/`; `argocd/components/02/platform-infisical-db.yaml`, `03/platform-infisical.yaml` | Renamed. `openbao_owner` initdb owner + `openbao` `DatabaseRole` with client cert. No `initdb.secret`. No Pooler. Replication ladder §5.1 |
| `hub-core-services/{zitadel,credit-authority,grafana}/database/` | `initdb.owner` → `<app>_owner`. `DatabaseRole` `<cluster>-hub-<app>` (no password, `inRoles`, client cert). `pg_hba` per §7.3. `hub-<app>-db-credentials` ExternalSecret → `VaultDynamicSecret` on `database/static-creds/hub-<app>` |
| `hub-core-services/billing-engine/database/` | Unchanged mechanism. The ExternalSecret store becomes the `vault` provider |
| `hub-core-services/openbao/` (new, in `platform-data`) | listener CA ClusterIssuer and the public `openbao-listener-trust` ConfigMaps written at Day-0 (§5.7); version-named `Backup` in an earlier sync wave (§9.4); base NetworkPolicy; `openbao-consumers` NetworkPolicy (steady state only); `database-recovery/` overlay without admission (§9.2) |
| Argo CD configuration | Health check for `postgresql.cnpg.io/Backup` (§9.4) |
| `hub-core-services/cluster-secret-store/`, `spoke-catalog/infra/cluster-secret-store.yaml`, `tenants/charts/universal-tenant/templates/tenant-secret-store.yaml` | `vault` provider, k8s/jwt auth, SA refs |
| All `ExternalSecret` `remoteRef.key` (§2.2 row 11) | KV v2 paths |
| `hub-core-services/security/` | cert-manager Vault ClusterIssuers + RBAC for `serviceaccounts/token`. Infisical issuer files deleted |
| `spoke-catalog/infra/{certificates.yaml, cert-manager-approve-…}` | issuerRef → Vault ClusterIssuer |
| `argocd-principal/certificates.yaml`, `support-agent/certificate.yaml`, `spokepool_controller.go:477` | issuerRef |
| `hub-core-services/gateway/templates/hub-gateway.yaml` | listener `openbao` |
| `operators/hub-operator/` | New `internal/openbao/` (KV write, policy/role/mount reconcile, static roles, spoke JWT mounts from `<cluster>-sa` `tls.crt` only). `status.databaseIncarnations` and the `openbao-consumers` NetworkPolicy (§9.2). `HubEnvironment` phases simplified. `api/v1alpha1` fields renamed |
| `internal/kakt-cli/bootstrap/orchestrator.go`, `components/secrets_*`, `health/*`, `tenant/*`, `cmd/kakt/*` | Readiness waits point at OpenBao. Seal-key generations, `pki-root`, Day-0 trust bootstrap (§6.5, §8.3), interrupted-init handling (§6.2), seal-generation selection on restore (§9.3). Infisical bootstrap removed |
| `internal/platform/escrow/escrow.go` | Artefact constants (§6.6). Client unchanged |
| `manifests/architecture/component-catalog.yaml` | §10.2 |
| `scripts/validate/**` | §13 behavioural scenarios replace the Infisical contract checks |
| `cmd/kakt/` | **One new command, `kakt pki sign-intermediate`** (review-authorized; the only CLI addition). It also performs scheduled renewal and compromise replacement through flags on the same command, not new commands |
| `internal/kakt-cli/bootstrap/orchestrator.go` | Phases `openbao-trust-bootstrap` and `pki-signed` (bounded wait, specific failure reason) replace `bootstrap-infisical-api` and its certificate-backoff repair (`orchestrator.go:604-635`) |
| `internal/kakt-cli/tenant/handover.go` | The push instruction gains a pointer to `kakt pki sign-intermediate` |
| `kamarkat-docs/cli/` (canonical command reference) | Document `kakt pki sign-intermediate`: what it validates, what it signs, idempotency, renewal and compromise modes |
| `kamarkat-docs/quickstart/build-the-management-cluster.mdx` | **The only quickstart change** (review-authorized): one step after `git push`, "sign the box's intermediate CA", linking the CLI reference. No procedure is duplicated. It ships with the release that ships the command, and is not published before it |
| ADRs (after approval only) | As listed in §12 |

### 11.2 Removed (obsolete in the target)

- `internal/kakt-cli/infisical/` (all), `operators/hub-operator/internal/infisical/` (all), `operators/hub-operator/internal/secrets/{infisical.go, cluster_project.go, cluster_credentials.go}` and Infisical parts of `generator.go`, `operators/hub-operator/internal/client/infisical.go`, Infisical endpoints in `internal/constant/`.
- `operators/hub-operator/internal/database/roles.go` (`RoleManager`). This also removes F2.
- `operators/spoke-identity-operator/` (operator, CRD `SpokeMachineIdentity`, image build, catalog entry, release pin).
- Vendored chart `infisical-standalone-1.12.2` (including bundled postgresql, redis and ingress-nginx subcharts), migration-unlock init container, `infisical-secrets`, `infisical-postgres-connection`, `infisical-db-credentials`, `infisical-auth`, Redis network-policy grant for platform-security.
- Infisical issuer CRDs, deployment and approver on hub and spokes.
- Validators: `031-infisical-api-contract.*`, `infisical-api-contract.yaml`, `037-cluster-secret-project.*`, `95-infisical-key-producers.sh`, `96-spoke-secret-authz.*`, `cluster/031-cluster-secret-isolation.*`. Each is **replaced** by the §13 behavior tests, not dropped.
- `docs/infisical/*` (operational notes for a removed component), archived with the ADR supersession.
- Box-declaration field `management.infisicalEnvironmentSlug` and CRD properties naming Infisical. No compatibility aliases.

There is no dual-write, no sync service, no path aliases, and no fallback.

---

## 12. Delivery model

The complete replacement ships **in one iteration**, through the existing bundle and release lifecycle (ADR-063/068).
- There are no intermediate release candidates holding partial target architecture.
- There are no phase-specific merge gates.
- There is no parallel self-hosted store, no compatibility bridge and no fallback to Infisical or Raft.

The work is ordered for implementation convenience only. The release contains all of it:
- **Data layer:**
  - `openbao/database/` with its `DatabaseRole`s;
  - the per-application owner and login split and `pg_hba` (§7.2–§7.3);
  - `billing-db` unchanged in mechanism.
- **OpenBao:**
  - the component in `platform-data`;
  - listener CA chain;
  - static seal and self-init;
  - the version-named `Backup` and the `Backup` health check (§9.4);
  - the recovery overlay without consumer admission (§9.2).
- **CLI:**
  - `kakt escrow init`: custody project, per-box root CA, recovery PGP pair (§6.1 step 1);
  - `kakt box init`: seal generation 1 (§6.1 step 1b);
  - Day-0 listener CA and trust ConfigMaps (§5.7);
  - `kakt pki sign-intermediate`, which signs OpenBao's CSR and returns the chain only (§6.1 step 8, §8.3);
  - root rollover for intermediate or root compromise (§8.3);
  - seal-configuration history and rotation (§6.3);
  - Day-0 trust bootstrap: pending-CSR report and the recovery-key sequence (§6.1, §6.5);
  - fail-closed detection of interrupted initialization (§6.2);
  - seal-configuration selection on restore (§9.3);
  - deletion of `internal/kakt-cli/infisical/`.
- **hub-operator:**
  - `internal/openbao` (KV, policies, auth roles, spoke JWT mounts from `<cluster>-sa` `tls.crt`, PKI roles, static roles);
  - desired-authorization reconcile, recording the incarnation it reconciled against (§9.2);
  - removal of its master-key escrow access (§6.0);
  - deletion of `RoleManager`, the Infisical client and the uploaders.
- **Delivery and issuance:** ESO stores and `VaultDynamicSecret` generators; cert-manager Vault ClusterIssuers on hub and spokes; tenant stores.
- **Removals:** all of §11.2.
- **Fixes:** F2 is fixed as part of `RoleManager`'s deletion. If the in-flight credential-ownership correction lands first, F2 is fixed there.
- **ADRs:** ADR-117, plus amendments to ADR-003, 023, 030 (§7.5), 031, 032, 035 (§8.3), 039, 040, 076 (§6.6), 078, 079 (§6.2), 111 (§9) and 112 (§5.1, §5.3, §5.4, §7.2).

Verification is behavioural (§13), inside the existing verification framework.

## 13. Behavioural acceptance scenarios

These run as ordinary tests in the existing verification framework, not as release gates.
- Denials are asserted as specific `403` responses.
- "Could not ask" is reported as `unverified`, never as a pass (ADR-031).
- Live runs are handed to the Release Engineer.

| ID | Scenario | Expected result |
|---|---|---|
| G1 Bootstrap | (a) New box: `kakt escrow init`, `kakt box init`, push, `kakt pki sign-intermediate` run immediately (it waits), Day-0 in CI. (b) Same, but signing withheld past the bounded wait. (c) A CSR for a different box presented to the command | (b) Day-0 **fails** at `pki-signed` with reason "administrator signing pending", and does not report acceptance. After signing, re-dispatch resumes and completes. (c) The command refuses before signing. In (a), before signing, Day-0 reports the pending CSR and certificate-dependent components wait. `kakt verify` makes no write in any case (audit log shows only reads). After signing: the `pki_int` key was never exported (issuer key reported as internal; no key in escrow or CI logs); the chain verifies to the root certificate; `leaf_not_after_behavior` is set. OpenBao initialized and unsealed. No root token. The audit device is enabled before admission. The automation identity is **not** a member of the custody project (404). `sys/rotate/recovery/backup` deleted. Bootstrap role, policy and ServiceAccount gone |
| G2 Interrupted initialization | Kill `openbao-0` during self-init on a new box | Day-0 stops and reports `ErrSelfInitFailed`. **`openbao-db`, its PVCs and its archive objects are unchanged.** No deletion under any combination of conditions |
| G3 Isolation, revocation and lifetimes | Tenant X's ESO ServiceAccount reads X's path, then Y's, `spokes/<c>/platform/*` and `hub/*`. Spoke A's ESO reads spoke B's paths. Spoke A's issuer requests B's CN, a wildcard, an IP SAN and a server-auth certificate. Delete tenant X while one of its tokens is live. Disable spoke A's mount while one of its tokens is live | X's own path → 200; every other read → 403. Every out-of-scope issuance is refused. X's live token → 403 on its next request (policy deleted). A's live token → 403 after the mount is disabled ([AS-12]). Recreate tenant X under the same name: X's old token stays 403 (new UID-bound policy name). A TokenRequest for more than 1h is capped by the spoke API server to 1h. A projected token is never extended (expiration extension disabled). A login with a token past `exp` + the effective allowance is refused, and the measured allowance is recorded. A legacy Secret-based token is refused. An OpenBao token cannot be renewed past 10 minutes. A tenant workload cannot obtain a token for a platform ServiceAccount |
| G4 Failover | `openbao-db` switchover under KV load, at each ladder tier | No sealed state. Resume time recorded. No acknowledged write lost in production (`required`). Staging behaviour recorded |
| G5 Rotation | `rotate-role hub-grafana` under load | Existing sessions continue. The new-connection failure interval is recorded. Convergence through ESO and Reloader. The `ALTER ROLE` comes only from `hub_grafana`'s certificate session on database `postgres` |
| G6 Engine authentication | Self-rotation ([AS-11]). Attempt `ALTER ROLE` on another role through that session. Password login as `hub_<app>` to `postgres`. Any login as `<app>_owner` | Rotation succeeds repeatedly. The other-role alter is refused. Both logins are refused by `pg_hba` |
| G7 Application database restore | Restore `grafana-db` to an earlier point | Exactly one `rotate-role`, with a complete record under `kv/platform/recovery/grafana-db/<uid>`. Later reconciles do not rotate. Grafana reconnects without a manual step. Kill hub-operator mid-step: it resumes, with no duplicate rotation |
| G8 OpenBao restore and reconstruction | (a) Restore `openbao-db` to a point before tenant X and spoke B were deleted, before a token T was revoked, and during a seal rotation; (b) replace the hub per ADR-111, restoring `grafana-db` too, and kill `kakt` between its restore and its rotation | (a) Recovery mode has no `openbao-consumers`. The delivered seal configuration equals the one recorded as in force at the recovery point, and unseal succeeds. X's policy and role and B's mount are gone. T and every other restored token → 403. Spoke keys come from CAPI. The recovery session survives revocation, and hub-operator logs in again. Every static role not in recovery is rotated once, with the effect verified. A missing generated KV value is reported and not regenerated. Admission returns only after the record is complete and the manifest is settled. In (b), `grafana-db`'s rotation happens only after its restore completes, and resumes after the kill with no rotation before the restore. (b) Auto-unseal. Existing spoke leaves still validate. Spoke ESO resumes |
| G9 Certificate renewal | Force renewal of the storage, `hub_<app>` engine and listener certificates, including after the old certificate's `notAfter` | New connections succeed with the renewed certificates. Pod start times unchanged: renewal must take effect without a restart |
| G10 Lease expiry | Dynamic role, 2-minute TTL (capability only) | Works, is refused after the TTL, and the role is dropped |
| G11 Version-change backup | (a) Bump the OpenBao version; (b) run ten routine syncs, a configuration change and a self-heal; (c) a failing backup | (a) `openbao-db-pre-<openbaoVersion>-<bundleVersion>-<serverName>` reaches `completed` before the new pod starts. (d) After an ADR-092 rollback, which bumps `serverName`, re-applying the same bundle creates a **new** `Backup` rather than reusing the old one. (b) **No new `Backup`.** (c) The old version keeps running |
| G14 Intermediate compromise drill | In a test box: declare the intermediate compromised and run root rollover | Trust bundles on hub and spokes contain only the new root. A certificate signed with the old intermediate is rejected by every relying party. All leaves are reissued. The mTLS interruption is recorded |
| G15 Signing-certificate renewal | Force renewal of `argocd-agent-jwt` | The new certificate has a validity of 1 year or less, inside the intermediate. The key is rotated per policy. The principal loads it, and agents re-authenticate with no manual step. A `signing-keys` request beyond the intermediate is refused (`leaf_not_after_behavior: err`) |
| G12 Custody boundary | Using the hub-operator and CI automation identity, read `pki-root` and the PGP private key | Refused (not a project member). The same reads in the administrator session succeed |
| G13 Audit | KV read, login, issuance and policy change. Then make the audit device fail | Each appears in Loki with HMAC-hashed values and no raw secrets. With the device failing, requests are refused and `audit.log_request_failure` raises the alert |

**Release Engineer actions:** RE-0 (F1, now). Then RE-1 to RE-15 run G1–G15 against the built release.

## 14. Remaining implementation verifications

None of these is a decision point.

| Item | Verified by |
|---|---|
| AS-10: inherited `pg_database_owner` gives `CREATE` on `public` (PostgreSQL 18.6) | G1 |
| AS-11: a static role whose username equals its certificate-authenticated connection user | G6 |
| AS-12: disabling an auth mount revokes its service tokens | G3 |
| AS-1: `<cluster>-sa` convention in the pinned KubeadmControlPlane | implementation check |
| AS-7: ESO `VaultDynamicSecret` `GET database/static-creds/<role>` | **resolved [SF]**: at ESO v2.15.0 (`reference-projects/external-secrets`), an empty method or `GET` calls `Logical().ReadWithDataWithContext(path, getParameters)` on an arbitrary path and returns `result.Data` (`generators/v1/vault/vault.go:118-161`). A contract test against OpenBao is kept |
| ESO `caProvider` from a ConfigMap (listener trust, §5.7) | **resolved [SF]**: `CAProviderTypeConfigMap` (`apis/externalsecrets/v1/secretstore_types.go:298`), resolved in `runtime/esutils/utils.go:674` |
| Infisical Cloud plan allows the custody project | escrow initialization check |
| CNPG: pruning a `Backup` object retains its data | G11 |
| Gateway BackendTLSPolicy (ConfigMap CA plus hostname) at its pinned version | G1 or G3 |
| Spoke ClusterClass: `service-account-max-token-expiration=1h`, extension disabled | **decided** (review). Implementation must first confirm spoke consumers refresh projected tokens |
| JWT leeways: 0 selects the defaults (150s / 150s / 60s) | **resolved [SF]**. Explicit 30s values are used; the effective combined allowance is measured in G3 |
| `signing-keys` ceiling 1 year; `argocd-agent-jwt` renewal and key policy | **decided** (review). The argocd-agent key-rotation contract is verified in G15 |
| Intermediate signing: one explicit administrator command | **decided** (review, Option C). `kakt verify` and `kakt kubeconfig` stay non-mutating. The quickstart gets one step, by reference to the CLI documentation |
| `hub_billing` remains `SUPERUSER` | recorded; changing it belongs to a billing-engine decision |
| OQ-7: the in-flight credential correction (staged in the working tree on 2026-11-24, not yet committed) | **Consistent as an interim step.** It makes CNPG the single SQL writer for each `hub_<app>` initdb owner (`initdb.secret` → `<cluster>-db-owner` ExternalSecret), removes `RoleManager`'s `ALTER ROLE` for those roles, and adds preflight `023-one-password-owner-per-role`. The ADR-117 release then replaces it with the owner/login split (§7.2), recreating the clusters empty. That preflight is kept and extended to assert §7.4's one-SQL-reconciler-per-role rule |

## Appendix A. Precedent and reference comparison

**Prior art: GitLab Secrets Manager** (generally available in GitLab 19.5; chart and admin docs read on 2026-11-24, [UF]). It is the closest production precedent for this proposal.

| Aspect | GitLab | This proposal | Note |
|---|---|---|---|
| Storage | OpenBao on its own logical database, which may share the PostgreSQL server | own CNPG cluster (ADR-112) | GitLab's design ADR (decision 013) could not be read; the reason for its separate database is unconfirmed |
| Seal | `static` by default; the key is a Kubernetes Secret `<release>-openbao-unseal`, `currentKeyId: gl-unseal-1` | same mechanism, with the key from escrow | GitLab: "the Toolbox does not back up the unseal key. Save a copy … yourself". Kamarkat's escrow makes that automatic |
| Init | declarative self-init | same | — |
| Recovery key | stored afterwards with a Rake task; without it, "no recovery key found" on every start | the CLI escrows it PGP-encrypted during Day-0, crash-safe (§6.5) | same gap, closed the same way but without a human |
| HA | one active node plus standbys; no standby reads on PostgreSQL; 2 replicas recommended | ladder 1/2/3, `disable_standby_reads` | matches |
| Pool | `maxParallel` 5, `transactionMaxParallel` 3, idle 2 | adopted | §5.2 |
| DB auth | password Secret | client certificate | Kamarkat goes beyond the precedent; the source supports it (§5.5) |
| Upgrades | "You can't upgrade OpenBao without downtime"; "upgrades can make changes to the PostgreSQL data that are not backwards compatible" | must be added to the upgrade skill: an OpenBao bump needs a backup at the pre-upgrade version, and rollback means restoring that backup | **new constraint** for ADR-111 Revision 9 and the upgrade skill |
| Restore | scale OpenBao down so it does not race the restore | CNPG restores into a new cluster (ADR-092), and OpenBao must not start until the restore completes | the boundary-02 ordering already holds this; G8 checks it |
| Build | a modified OpenBao image | upstream image | Kamarkat takes no GitLab patches |

**Where Kamarkat deliberately departs from the CNCF reference** (full article read on 2026-11-24):

| Reference | Kamarkat | Why |
|---|---|---|
| Shamir seal, `bao operator init`, three unseal keys typed into each pod after every restart | Static seal from escrow + self-init | No manual init or unseal (brief §5, §6) |
| Scheduled rolling restart for cert renewal | No restart. The pinned pgx re-reads the cert files on each new connection (§5.5) | Source contradicts the article's claim |
| Schema Job | OpenBao creates its own tables (`skip_create_table=false`) | clear ownership; no Job and no DDL copy |
| `tls_disable = 1` on the listener | TLS on the listener (internal cert from the PKI mount for in-cluster clients; public listener via the Gateway) | The reference is a playground setup |
| No backup configured ("production workloads need more than that") | ADR-112 per-cluster archive, archive credential from escrow | ADR-111 Class B |
| Dedicated Postgres nodes, zone anti-affinity | ADR-014 worker placement | Kamarkat's minimum topology |
| No failover test | Behavioural scenario G4 | The reference gives no evidence for it |

Also unresolved: the docs say PostgreSQL HA arrived in v2.7.0, while the CNCF article shows "HA Enabled true" on 2.6.1. Resolve this against OpenBao source before pinning.

---

## Appendix B. Other reference projects reviewed (2026-11-24)

| Project | What it does | Disposition |
|---|---|---|
| Kubefirst (`reference-projects/civo/kubefirst`, k3d flavour) | Initializes Vault with Shamir shards. It stores **the unseal shards and the root token** in a Kubernetes Secret (`vault-unseal-secret`) in the same cluster (`cmd/k3d/vault.go:29,120-135`). Unsealing is an operator command (`kubefirst k3d unseal-vault`), and the root credentials are printed or copied to the clipboard (`cmd/k3d/root-credentials.go`) | **Not adopted.** It is the pattern ADR-117 rejects: recovery material inside the cluster it protects, a persistent root token, and manual unseal. Its cloud flavours are in repositories not vendored here, so no claim is made about them |
| Qovery (`reference-projects/qovery`) | Vault appears only in engine test helpers, to fetch CI credentials (`engine/tests/helpers/*.rs`). Azure Key Vault is used for etcd secret encryption (`engine/lib/azure/bootstrap/terraform`) | No secret-store, workload-identity or PKI pattern applies |
| External Secrets Operator (`reference-projects/external-secrets`, v2.15.0) | Source of the pinned ESO | Used as evidence: §14 AS-7 and the ConfigMap `caProvider` |

## Sources

- Repository files cited inline, at commit `3d74cf26`.
- OpenBao docs (v2.7.x): [PostgreSQL storage](https://openbao.org/docs/configuration/storage/postgresql/), [static seal](https://openbao.org/docs/configuration/seal/static/), [self-init](https://openbao.org/docs/configuration/self-init/), [database secrets](https://openbao.org/docs/secrets/databases/), [database API](https://openbao.org/docs/api/secret/databases/), [JWT auth](https://openbao.org/docs/auth/jwt/), [Kubernetes auth](https://openbao.org/docs/auth/kubernetes/), [namespaces](https://openbao.org/docs/concepts/namespaces/)
- [OpenBao releases](https://github.com/openbao/openbao/releases), [openbao-helm releases](https://github.com/openbao/openbao-helm/releases)
- [CNCF: Running OpenBao on Kubernetes with a CloudNativePG PostgreSQL backend](https://cncf.io/blog/2026/09/16/running-openbao-on-kubernetes-with-a-cloudnativepg-postgresql-backend) (Bartolini & Kenefeck, 2026-09-16; read in full, verbatim)
- Vendored sources: `reference-projects/vault/openbao` (tags v2.6.1, v2.7.3), `reference-projects/vault/cloudnative-pg` (tag v1.30.1); module cache: `github.com/jackc/pgx/v5@v5.10.0`, `github.com/openbao/go-kms-wrapping/wrappers/static/v2@v2.2.0`
- GitLab: [OpenBao chart](https://docs.gitlab.com/charts/charts/openbao/), [Secrets Manager administration](https://docs.gitlab.com/administration/secrets_manager/), [design decision 013](https://handbook.gitlab.com/handbook/engineering/architecture/design-documents/secret_manager/decisions/013_openbao_separate_database/) (not readable by the fetcher; body unconfirmed)
- [ESO HashiCorp Vault provider](https://external-secrets.io/latest/provider/hashicorp-vault/), [cert-manager Vault issuer](https://cert-manager.io/docs/configuration/vault/)
