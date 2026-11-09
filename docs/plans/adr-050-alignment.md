# ADR-050 Alignment Plan — spoke-terminated ingress

**Date:** 2026-08-21 (final)
**Goal:** Satisfy ADR-050's security contract, land per-environment subdomains with
browser-trusted TLS, and close the authentication gaps — with the gateway co-located with
the workloads it protects.

> **Revision history.** v1 aligned by migration (wire MCS, then cut DNS over). v2 aligned by
> rebuilding both clusters. **Both are superseded.** Moving AgentGateway from the hub to each
> spoke satisfies the same security property while deleting the entire cross-cluster
> prerequisite chain — no ClusterMesh, no MCS, no hub-on-tailnet, no pod-CIDR re-IP, and
> therefore no cluster rebuild. Oathkeeper was evaluated and **is not adopted** (Appendix A).

## The decision

**Tenant traffic terminates at the spoke that runs the workload.** Each spoke runs its own
AgentGateway; the hub keeps only platform services.

```
browser -> DNS -> spoke LB (CAPH extraServices 80/443)
        -> Cilium Gateway (hostNetwork Envoy, terminates TLS)
        -> HTTPRoute host-match -> spoke AgentGateway (oidc policy)
        -> frontend/bff Service  (cluster.local - same cluster)
```

Identity stays on the hub. The spoke AgentGateway redirects the browser to
`auth.<env>.nutgraf.in` (Hydra) -> auth-proxy login/consent -> back to the spoke's callback.
All public HTTPS via browser redirects — **no cluster-to-cluster network path**.

**Routing to the right spoke is DNS.** `<tenant>.<env>.nutgraf.in` A-records point at the LB
of the spoke hosting that tenant. external-dns runs per spoke and derives records from the
Gateway/HTTPRoute objects present locally; since a tenant's Gateway exists only on its owning
spoke, record sets are naturally disjoint. The TXT registry with a per-cluster
`--txt-owner-id` prevents cross-ownership edits. Tenant migration = the Gateway moves,
records follow, bounded by TTL.

**Multiple tenants per spoke is the normal case**, and is already the deployed shape: one
AgentGateway, per-hostname route blocks, each with its own `oidc` policy and `clientId`,
backing onto different namespaces. Two independent isolation mechanisms (verified in the
vendored source): cookie names derive from `agw_oidc_` + `sha256(policy_id)[0..8]`, and
cookies are host-only with `SameSite=Lax`.

**No OAuth client changes.** `waypoint-public-client` already registers
`redirectURI: https://waypoint.nutgraf.in/oauth/callback` — keyed to the *tenant hostname*,
which follows the tenant rather than the cluster. Only the hostname changes (env zone).

### Why this satisfies ADR-050

Hardening item 5 requires the BFF never be publicly reachable except through an
authenticating gateway. Here the spoke HTTPRoute maps the hostname to the **spoke's**
AgentGateway, which forwards to the BFF; the BFF is never directly reachable. The property
holds — but ADR-050's wording says *"the hub AgentGateway"*, so **it needs a deliberate
amendment** (Phase 5), not an implied reinterpretation.

## Hostname model

Env is a DNS zone; production is bare.

```
platform (hub)   api.dev.nutgraf.in  auth.dev.nutgraf.in  console.dev.nutgraf.in  argocd.dev.nutgraf.in
tenants (spoke)  waypoint.dev.nutgraf.in  api.waypoint.dev.nutgraf.in  oranger.dev.nutgraf.in
prod             api.nutgraf.in ... / waypoint.nutgraf.in ...        (no env label)
```

---

## Verified constraints (do not re-litigate)

| Fact | Evidence |
|---|---|
| **Hydra has no RFC 8693 token exchange.** Advertised grants: `authorization_code, implicit, client_credentials, refresh_token, device_code`. The `oauth2TokenExchange` symbols are Hydra's *name for the token endpoint handler*, not RFC 8693 | `reference-projects/ory/hydra`, `oauth2/handler.go:536` |
| **AgentGateway `jwtSign` cannot carry user claims** — `claims: BTreeMap<String, serde_json::Value>`, static only | `agentgateway/src/http/auth/jwt_sign.rs:75-79` |
| => **ID-token-as-credential stands as an accepted deviation.** The mitigation is mandatory: the BFF must validate `aud` explicitly | see 3b-2 |
| AgentGateway JWKS **does** refresh (15 min default, `Cache-Control` aware, 60s floor). ADR-050's "restart required" claim is wrong; the real gap is no `kid`-miss refresh | `agentgateway/src/resource_manager.rs:21-25,466-524` |
| AgentGateway strips the client `Authorization` before transformation | `agentgateway/src/http/jwt.rs:596-600` |
| No RP-initiated logout exists upstream | verified absent |
| `auth-proxy` injects **singular `role`** | `internal/auth-proxy/validate.go:55` |
| **The hcloud token is also the DNS credential.** There is no separate Hetzner DNS key; `hetzner-dns-es.yaml` correctly reads `hcloud-token` per ADR-003 (Infisical is the sole System of Record; ESO is the sole delivery mechanism) | `manifests/hub-core-services/external-dns/hetzner-dns-es.yaml` |
| **Dev topology is now CP-in-Hetzner + Flatcar homelab workers on BOTH clusters.** The hub's Hetzner worker pool is gone; `flatcar-hub-node-1` carries hub workloads, including Infisical and `platform-db` | verified live |

---

## Already done

- **BFF auth bypass closed** + 9 regression tests; frontend `Bearer <tenantId>` removed.
  Committed in waypoint `30f7c79`.
- **Environment isolation**: both ClusterSecretStores now resolve `environmentSlug` per
  environment (hub via `01-platform-infra-appset.yaml` templatePatch + a new
  `kustomization.yaml`; spoke via the `03-platform-services-appset.yaml` patch block).
  Verified `dev->dev`, `stg->stg`, `prod->prod`.
- Env-zone `DOMAIN` values, `stg/patch-hubenvironment.yaml`, per-tenant ACME solvers,
  spoke `external-dns.yaml`, ADR-046 addenda 19/20, ADR-051.

---

## Phase 0 — Unblock the hub edge (prerequisite, independent value)

The hub edge is deployed but unreachable: ingress-nginx LB `65.109.41.89`, `agentgateway`
and `auth-proxy` both 1/1, Ingresses present — yet **no hub hostname resolves**, so six
certificates have been `pending` for days, all failing HTTP-01 self-check.

Hydra must be publicly reachable for the spoke OIDC redirect to work, so this gates
everything.

1. DNS A-records for the hub platform hostnames -> `65.109.41.89`.
2. Confirm the six certificates issue; delete the accumulated stale `cm-acme-http-solver-*`
   Ingresses and orphaned Challenges.
3. Move hub Ingress hosts and `hub-core-services/ingress/dev-certificates.yaml` to the
   env-zone names.

**Gate:** `curl https://api.dev.nutgraf.in` trusted; `auth.dev.nutgraf.in` serves Hydra
discovery and JWKS.

## Phase 1 — Bootstrap from scratch

The hub is rebuilt from zero rather than repaired in place, which exercises the whole path
end-to-end. Data loss is accepted: Infisical's contents — PKI hierarchy, machine identities,
every platform secret — go with it, and are re-seeded from the Infisical console once the
new instance is up.

### 1a. Why the current cluster is wedged (the reason the wave fix below matters)

Not a storage problem — a circular dependency. Verified live before teardown:

1. Hub workloads moved to homelab Flatcar workers; the Hetzner worker pool is gone.
2. **Eight PVCs stayed bound to `hcloud-volumes`** — `platform-db`, `platform-redis`, `nats`,
   `openmeter-kafka`, `vmstorage` (x2), `vmselect`, `spire-server` — with
   `nodeAffinity: csi.hetzner.cloud/location In [hel1]`, so they cannot attach to a homelab
   node (`volume node affinity conflict`).
3. StorageClass is immutable on a bound PVC, so the `local-path` change applies only to new
   claims.
4. ArgoCD could not apply it anyway: `platform-database` was stuck
   `waiting for healthy state of external-secrets.io/ExternalSecret/s3-credentials`.
5. `s3-credentials` was `SecretSyncedError` — its keys absent from Infisical.
6. Infisical's store **is** `platform-db` (`DB_HOST: platform-db-pooler.platform-data.svc`),
   the pod that could not schedule.

**Root cause: no sync waves.** Every manifest in `manifests/hub-core-services/database/`
carried no `argocd.argoproj.io/sync-wave`, so ArgoCD applied the directory as one wave and
gated on the unhealthiest member — a *backup* credential blocking the *database's own
creation*. **Fixed**: database and connection credentials at wave 0-1, jobs at 3-4,
`s3-credentials` at 5, `scheduled-backup` at 6.

This directly enables the intended bootstrap flow: the database comes up at wave 0 without
`s3-credentials`, Infisical starts, secrets are added from the console, and the backup path
completes on a later pass. Without the fix, a fresh bootstrap would deadlock identically.

### 1b. ~~Blank the stale ADR-045 artifacts~~ — **RETRACTED, this was wrong**

An earlier revision of this plan claimed the generated identity artifacts had to be blanked
before bootstrap. That is incorrect and would have been busywork. The CLI **regenerates**
them during the `bootstrap-infisical-api` phase —
`internal/hub-cli/components/secrets_database_impl.go:122-163` writes both
`infisical-fleet-issuer-patch.yaml` and `hub-bootstrap-config-patch.yaml` — and the history
shows them recommitted each run (`chore: bootstrap-generated-gitops-artifacts [skip ci]`).
Commit `d1b1a387` already handled the stale-ID case explicitly. No action.

**Related retraction: the database sync-wave change has been reverted.** It was derived from
the *in-place repair* deadlock (1a), which a from-scratch bootstrap does not hit: with no
pre-existing PVC bound to `hcloud-volumes`, CNPG provisions on the `local-path` default and
an unhealthy `s3-credentials` merely leaves the app Progressing rather than blocking
creation. Adding waves to a tested bootstrap path — particularly to the two `Sync` /
`PostSync` hook Jobs, where waves and hooks interact — was risk without benefit. The
ordering weakness is real and worth fixing, but **after** a green bootstrap, not before.

### 1c. Bootstrap flags — two defaults are wrong

| Flag | Default | Consequence | Use |
|---|---|---|---|
| `--environment` | coerced `""` -> **`prod`** (`cmd/hub/bootstrap.go:214-218`) | AppSet path `spoke-pools/prod/hybrid` does not exist -> **spoke never provisions** | `--environment=dev` |
| `--topology` | **`single`** | path `spoke-pools/dev/hybrid/single` does not exist | `--topology=` (explicit empty) |

`scripts/hub-bootstrap.sh` supplies both correctly; driving `bin/hub` by hand does not.
**Never call `hub configure-eso`** — it is hard-deprecated and always returns an error.

### 1d. Expect the ADR-045 commit gate

The `adr045-commit` phase performs `git commit` **and `git push` on `main`**, then fails the
whole bootstrap if `platform-security-infra` and `hub-environment` are not Synced+Healthy
within 15 minutes. Start from a clean working tree; a failed run leaves commits behind.

### 1e. Seed Infisical from the console once it is up

Beyond the S3 keys: `hcloud-token` (also the DNS credential — there is no separate Hetzner
DNS key), GitHub PAT, Grafana, AWS, Tailscale authkey. `S3_ACCESS_KEY_ID` /
`S3_SECRET_ACCESS_KEY` go at the hub-secrets project root and unblock wave 5-6.

### 1f. Post-bootstrap, out of band

Re-add the Hetzner LB->node firewall rules (ADR-046 addendum 4 — not codified anywhere) and
rejoin the home workers with `scripts/hybrid/provision-flatcar-worker.sh` for both clusters.

## Phase 1g — Backup gap (after bootstrap)

`platform-data/platform-db` backs Infisical — the PKI root of trust and every platform
secret — and has **never** backed up successfully. Since commit `1e14d528` it also sits on
`local-path`, i.e. ephemeral node-local storage. Verified live:

```
lastSuccessfulBackup: <empty>    lastFailedBackup: 2026-08-20T17:17:44Z
externalsecret/s3-credentials:   SecretSyncedError
```

**This is a secret-seeding problem, not a manifest problem.** The ExternalSecret and
`ScheduledBackup` already exist and are delivered (`02-platform-data-appset.yaml:32`,
`directoryRecurse: true`). Every *other* ExternalSecret in that namespace is
`SecretSynced=True`, which isolates the failure to its two `remoteRef` keys being absent in
Infisical.

- Seed real Hetzner Object Storage keys as `S3_ACCESS_KEY_ID` / `S3_SECRET_ACCESS_KEY` at
  the hub-secrets project root — **not** `hcloud-token` (ADR-046 addendum 12: Object Storage
  keys start `0A...`, never `HT...`).
- Add a gate to `scripts/post-bootstrap-validate.sh`: fail on an empty
  `lastSuccessfulBackup`, so a never-backed-up root of trust cannot pass silently.

Note the misdirection for whoever debugs this: CNPG reports the *Secret* missing, pointing
one layer below the actual failure.

## Phase 2 — Spoke-terminated ingress

### 2a. Move AgentGateway to the spoke

Add an AgentGateway deployment to the spoke catalog, modelled on
`manifests/hub-core-services/api-gateway/` (`agentgateway.yaml`, `agentgateway-config.yaml`,
`oidc-cookie-secret-es.yaml`, `network-policy.yaml`).

- Route blocks per tenant hostname, each with its own `oidc` policy and `clientId`.
- Backends are same-cluster: `frontend-workload.tenant-waypoint.svc.cluster.local:3000`,
  `bff-workload....:3001`.
- `OIDC_COOKIE_SECRET` per spoke via ExternalSecret — config compile hard-fails without it.
- `issuer` -> `https://auth.<env>.nutgraf.in` (hub Hydra, public).

**The hub keeps its AgentGateway** for `/mcp` on `api.<env>.nutgraf.in`. Two roles, two
places: the MCP resource-server stays hub-side; the tenant browser gateway moves.

### 2b. Wire the spoke ingress path

- Keep `extraServices` 80/443 on the CAPH LB (already present) — Cilium Gateway terminates
  TLS and forwards to AgentGateway. Do **not** have AgentGateway bind 80/443 itself; that
  would duplicate the hostNetwork/mangle-guard work.
- Keep PROXY protocol **off** on both sides (ADR-046 addendum 17.4).
- HTTPRoutes map `<tenant>.<env>.nutgraf.in` -> AgentGateway Service.
- Spoke-side ACME issues the tenant certificates (`acme-cluster-issuer.yaml`, per-tenant
  `dnsZones` solvers — already in the tree).

### 2b-bis. Routing ownership — the gateway is platform-owned

**DECIDED: tenants do not own HTTPRoutes.** Under ADR-050 the gateway is a security
control. If a tenant owns a route it can add one that bypasses the gateway, so hardening
item 5 would rest on review rather than on structure.

- **Platform owns** the `Gateway` and the HTTPRoute `hostname -> AgentGateway Service`, both
  in the platform namespace. The reference is same-namespace, so **no `ReferenceGrant` is
  required and none exists to grant** — the bypass is structurally impossible rather than
  policy-enforced.
- **AgentGateway config** carries `hostname -> tenant backend Services`. That is
  AgentGateway's own routing, not Gateway API, so crossing namespaces there is a
  NetworkPolicy question and never a `RefNotPermitted` failure. The hub config already works
  this way — it routes to `bff-workload.tenant-waypoint.svc.cluster.local` with no Gateway
  API cross-namespace reference.
- **Tenants declare data, not routing** — hostname and backend Services as fleet-registry
  values, rendered by the platform into AgentGateway config.

Rejected alternatives: routes in the tenant namespace with a per-tenant `ReferenceGrant` on
the platform side (preserves tenant ownership but makes bypass merely disallowed rather than
impossible, and adds a platform object per tenant); and moving tenant routes wholesale into
the platform namespace while leaving them tenant-authored (same bypass risk, no ownership
gain).

Cost, stated plainly: tenants can no longer self-serve routing changes — adding a hostname
becomes a platform-side change. For a security control that is the correct trade, for the
same reason tenants do not edit their own NetworkPolicies.

### 2c. external-dns per spoke

`manifests/spoke/spoke-catalog/infra/external-dns.yaml` is in the tree. `--domain-filter`
and `--txt-owner-id` are `args[0]`/`args[1]` by contract, patched per spoke by the
ApplicationSet. Confirm it publishes only the hostnames whose Gateways exist locally.

### 2d. Retire the hub tenant routes

Remove the `waypoint.nutgraf.in` / `api.waypoint.nutgraf.in` route blocks from
`hub-core-services/api-gateway/agentgateway-config.yaml`. They have no live backend today
and would otherwise become a second, competing path.

**Gate:** `https://waypoint.dev.nutgraf.in` returns the frontend, browser-trusted, through
the spoke AgentGateway; a full OIDC login round-trip succeeds against `auth.dev...`; the BFF
is not directly reachable.

## Phase 3 — Close the authentication gaps

### 3a. `zero-ops-auth` fixes (`zero-ops/packages/auth`)

Two are fail-open or fail-always and block real authorization work:

| # | Defect | File |
|---|---|---|
| 1 | **Scope check fails open** — a token with no `scope` claim skips the check and is admitted; `InsufficientScopeError` never thrown | `src/middleware/hono.ts:39` |
| 2 | **`requireRole()` can never pass** — reads `roles`, auth-proxy emits singular `role`, no fallback | `src/authz/role.ts`, `src/jwt/claims.ts` |
| 3 | Issuer/audience errors mis-mapped (`ERR_JWT_INVALIDIssuer` typo) -> P0-1 violations indistinguishable from any bad token | `src/jwt/validator.ts:83` |
| 4 | `releaseRefreshLock` is non-atomic `GET`-compare-`DEL`, not a Lua CAS | `src/oauth/token-store.ts:108-115` |
| 5 | `refreshToken()` missing from the `TokenStore` interface — typing the field as `TokenStore` silently loses the mutex | `src/oauth/token-store.ts:11-19,122` |
| 6 | `buildAuthorizationUrl` never returns `codeVerifier` (dead ternary) | `src/oauth/client.ts:158` |
| 7 | `JwksCacheOptions` inert for `validate()` | `src/jwt/jwks-cache.ts:43` |

Coverage is two files; `JwtValidator`, `OidcClient`, `RedisTokenStore`, `authMiddleware`,
`requireTenantBoundary`, `requireRole` are **untested**. Add tests with the fixes. Delivery
is via npm — bump the version, CI publishes on push to `main` touching `packages/auth/**`.

### 3b. Waypoint BFF (`waypoint/packages/bff`)

| # | Work | Contract |
|---|---|---|
| 1 | Register `authMiddleware({ validator })` in `src/app.ts` — the chain is only `logger()` + `cors()`, so every `/api/v1/*` route is unauthenticated | P0-3 |
| 2 | **Pass `audience` to `JwtValidator`** — `lib/auth-config.ts` omits it, so P0-1 is unenforced. Mandatory given no token exchange | **P0-1** |
| 3 | Enforce `requireTenantBoundary()`. **Resolution: the claim is authoritative** — derive tenant from `principal.tenantId`; a request-supplied `tenant_id` must equal it or 403, never be the source. ~20 call sites across `routes/{apps,workflows,chat,eval,bmc,businesses,webhooks}.ts` | **P0-2 L1** |
| 4 | Enforce `requireRole()` (blocked on 3a-2). Today the only role check is `routes/webhooks.ts:102-105`, trusting `payload.user_roles` from the request body | **P0-2 L2** |
| 5 | Adopt `getPrincipal()`/`PRINCIPAL_KEY`; drop the parallel `AuthenticatedIdentity` | consistency |
| 6 | Fix the SDK fail-open guard: `waypoint-sdk/src/index.ts:54-59` returns `true` when `WAYPOINT_INTERNAL_TOKEN` is unset | P1-5 |
| 7 | `lib/delegated-token.ts:85-88` accepts `flowTtlMs` and drops it | P1-1 |
| 8 | Remove hardcoded production defaults for every auth env var; update `.env.example`, which documents none | P1-5 |
| 9 | Add a BFF Kubernetes manifest — `packages/bff/k8s/` does not exist | — |
| 10 | Guard the HMR WebSocket upgrade (`src/index.ts:39-49`), authenticated on a preview cookie only | — |

**DECIDED — wire up the delegated-token lifecycle.** `delegatedTokenFor` is fully
implemented with zero callers; every downstream call sends the shared
`WAYPOINT_INTERNAL_TOKEN`, so no user identity reaches the SDK and every audit record says
"the BFF" rather than who acted. The platform already has the shape that justifies
delegation: `/mcp` is audience-restricted (`https://api.nutgraf.in/mcp`) with real scopes
(`tenant:read`, `tenant:write`), and two OAuth clients per tenant already encode the
distinction. This is the standard BFF token-relay pattern, not extra machinery.

**It must be wired as a pair, or not at all:**
1. `lib/sdk-client.ts` calls `delegatedTokenFor()` and forwards the user's token downstream.
2. `waypoint-sdk` validates that token and enforces tenant/scope, **failing closed**.

Today the SDK fails **open** — `waypoint-sdk/src/index.ts:54-59` returns `true` when
`WAYPOINT_INTERNAL_TOKEN` is unset (3b-6). Forwarding a delegated token to a service that
accepts anything adds a lifecycle with no security gain and *looks* enforced, which is worse
than the present state. Half of this change is a regression.

### 3c. Frontend (`waypoint/packages/frontend`)

`src/contexts/auth-context.tsx:17-27` hardcodes a `platform_admin` identity on localhost and
skips `/auth/me`. Bring it under the same explicit, fail-closed opt-in as the BFF's
`devAuthEnabled()`.

### 3d. fleet-registry

Tenant manifests ship **no public Gateway or HTTPRoute** that bypasses AgentGateway;
hostnames move to the env zone; the `waypoint-tls` Certificate `issuerRef` moves to the spoke
ACME issuer.

### 3e. Declarative OAuth client registration via `hydra-maester`

**Adopted.** Today `auth-proxy` registers three OAuth clients imperatively at startup
(`internal/auth-proxy/hydra.go:37,62,87` — `mcp-public-client`, `waypoint-public-client`,
`waypoint-bff-client`). That is a silent startup dependency: if registration fails or
drifts, nothing reports it and the symptom surfaces later as a broken login.

`hydra-maester` (vendored at `reference-projects/ory/hydra-maester`, v0.0.42) reconciles an
`OAuth2Client` CRD against Hydra's admin API. The CRD covers every field auth-proxy sets —
`clientName`, `grantTypes`, `responseTypes`, `redirectUris`, `postLogoutRedirectUris`,
`audience`, `scope`/`scopeArray`, `tokenEndpointAuthMethod`, `skipConsent`, `metadata` — and
`secretName` points at the K8s Secret holding the client id/secret, which fits the existing
ESO delivery for `waypoint-bff-client`.

Work:
- Deploy the `hydra-maester` chart (`reference-projects/ory/k8s/helm/charts/hydra-maester`)
  alongside the Hydra chart already used by `04-tenant-services-multi-appset.yaml:21-24`.
- Express the three clients as `OAuth2Client` resources. Their `redirectUris` become
  env-zone hostnames, so this lands naturally with Phase 4.
- Remove `RegisterClient` from `auth-proxy` once the CRDs reconcile clean, leaving
  auth-proxy with only its login/consent, metadata, JWKS-proxy and validate roles.

**This does not eliminate `auth-proxy`.** Hydra is headless and requires a login/consent
provider, and auth-proxy injects the platform's `tenant_id`/`role` claims at consent time
(`internal/auth-proxy/validate.go:55`) — business logic no stock Ory UI provides. Client
registration is one of six responsibilities; only that one moves.

**Gate:** the three `OAuth2Client` resources reconcile to Ready, ArgoCD reports them Synced,
and a login round-trip succeeds with `auth-proxy`'s `RegisterClient` removed.

## Phase 4 — Identity stack follows the env zone

Widest blast radius; all must move together or login breaks with `redirect_uri mismatch`:

- `ory-hydra/values.yaml` — issuer -> `https://auth.dev.nutgraf.in`
- `ory-kratos/values.yaml` — `base_url`, allowed return URLs
- `agentgateway-config.yaml` — OAuth `redirectURI` values
- **`internal/auth-proxy/hydra.go:67,92`** — callbacks compiled into the binary; make them configuration
- Registered Hydra client redirect URIs must match

## Phase 5 — Reconcile the ADRs

- **Amend ADR-050**: hardening item 5 wording `"the hub AgentGateway"` -> `"an AgentGateway"`;
  move the tenant-hostname ownership row from hub ingress to the spoke; correct upstream gap
  5 (JWKS refreshes; the real gap is no `kid`-miss refresh); record the ID-token deviation
  with its `aud`-validation mitigation.
- **Rewrite ADR-051** so hostnames terminate at the spoke; its Impact section currently flags
  the ADR-050 conflict as unresolved, and this decision replaces that flag.
- **ADR-046**: no longer needs the hub-tailnet or pod-CIDR amendments.

## Cross-cutting

Carried forward from the design review (`adr-050-gaps.md`); none block a phase, all are
real and none are recorded elsewhere.

| # | Finding | Status |
|---|---|---|
| 1 | **`hub bootstrap --environment` coerces `""` -> `prod`** (`cmd/hub/bootstrap.go:214-218`), and `--topology` defaults to `single`. Together they point the spoke AppSet at `spoke-pools/prod/hybrid[/single]`, which does not exist — the spoke silently never provisions. The v2 plan carried this in a "Traps" table that went away with the rebuild; it is still a live footgun for any future bootstrap | open |
| 2 | **`hub spoke teardown --name=X` is a silent no-op** — it rebuilt the name by splitting the CAPH label and taking parts 2-5, yielding `spoke-pool-hybrid`, which never matched. Reported success, deleted nothing | closed: the command is withdrawn (ADR-114 §6). Destroying a managed cluster is `kakt tenant remove-cluster`, or `kakt teardown --destroy-managed` when the box is going |
| 3 | **The hub pod CIDR is a Go string literal** (`internal/hub-cli/cluster/provisioner.go`) — a day-0 topology input with no flag or ClusterClass patch. Spoke-terminated ingress removed the *consequence* (no rebuild needed), but the smell remains: changing it still means editing a string and rebuilding the binary | open, deprioritised |
| 4 | **`zero-ops-auth` is Hono-only.** A platform-wide auth primitive normally ships a framework-agnostic core with thin adapters; this locks every tenant to one framework | open, design debt |

---

## Verification

```sh
# Phase 0 - hub edge reachable and trusted
kubectl --kubeconfig=$HUB_KC get certificate -A | grep -v True     # expect none
curl -sS -o /dev/null -w '%{http_code}\n' https://api.dev.nutgraf.in
curl -sS https://auth.dev.nutgraf.in/.well-known/openid-configuration | head -c 200

# Phase 1 - backup gap closed
kubectl --kubeconfig=$HUB_KC get cluster.postgresql.cnpg.io -n platform-data platform-db \
  -o jsonpath='{.status.lastSuccessfulBackup}'                     # expect non-empty

# Phase 2 - tenant served from the spoke, trusted, gateway-fronted
dig +short waypoint.dev.nutgraf.in                                 # expect the SPOKE LB
curl -sS -o /dev/null -w '%{http_code}\n' https://waypoint.dev.nutgraf.in
echo | openssl s_client -connect waypoint.dev.nutgraf.in:443 \
  -servername waypoint.dev.nutgraf.in 2>&1 | grep -E 'issuer=|Verify return code'
kubectl --kubeconfig=$SPOKE_KC get httproute -A                    # hostnames -> agentgateway, not bff

# Phase 3 - auth actually enforced
curl -sS -o /dev/null -w '%{http_code}\n' https://waypoint.dev.nutgraf.in/api/v1/apps   # expect 401/302
curl -sS -H 'x-auth-subject: attacker' -H 'x-auth-tenant: victim' \
  -o /dev/null -w '%{http_code}\n' https://waypoint.dev.nutgraf.in/api/v1/auth/me       # expect 401

cd waypoint/packages/bff && npx vitest run
bash scripts/post-bootstrap-validate.sh
```

## Sequencing

Execution is a **from-scratch bootstrap**, so the repo must be correct before teardown —
everything below the bootstrap line is a repo change with no cluster dependency.

**Before teardown (repo only):**
1. **1b** — blank the stale ADR-045 artifacts. Nothing else matters if the new hub wires
   ESO to a dead identity.
2. **3a** — `zero-ops-auth` fixes, published. Unblocks 3b-4.
3. **Phase 2** manifests — spoke AgentGateway, platform-owned routes, ACME, external-dns.
4. **Phase 4** hostnames and **3e** `OAuth2Client` resources **together** — redirect URIs
   and identity-stack hostnames must move in one change or login breaks.
5. **3b-3d** — BFF, frontend, fleet-registry.

**Bootstrap** (1c-1d), then:

6. **1e** — seed Infisical from the console; wave 5-6 completes and 1g closes.
7. **1f** — firewall rules, rejoin home workers.
8. **Phase 0** — hub edge DNS/TLS; gates the OIDC redirect.
9. **Phase 5** — ADR reconciliation.

The sync-wave fix in 1a is what makes step 6 possible: the database no longer waits on a
backup credential, so Infisical can start before any secret exists.

## Risks

- **The root of trust is on ephemeral storage with no backup** (Phase 1). Independent of
  everything else here; it does not survive a node reschedule.
- **Identity-stack completeness** (Phase 4): one missed redirect URI breaks login, and the
  two compiled Go constants are easiest to miss.
- **ADR-050 amendment is required, not optional.** Shipping spoke termination while the ADR
  says "hub" leaves the platform contradicting its own accepted decision.
- **Dormant delegation machinery** (3b): built, wired, never called. Decide it in or out.
- **hydra-maester cutover is a two-writer window** (3e): until `RegisterClient` is removed
  from `auth-proxy`, both it and the controller reconcile the same Hydra clients. Land the
  CRDs first, confirm they reconcile clean, then remove the Go path — not the reverse.
- **Multi-spoke tenants have no answer** — one tenant across two spokes would need real
  GSLB; DNS round-robin has no health awareness. Out of scope, named deliberately.
- **The spoke AgentGateway is a shared failure domain within its spoke.** Better than today
  (shared across all tenants everywhere), but per-tenant isolation would mean a second
  gateway per spoke, not a second spoke.

---

## Appendix A — Third-party components: evaluated

Three were assessed against vendored source. **Only `hydra-maester` is adopted** (plan
item 3e); the other two are recorded so the analysis is not repeated.

### `ts-hydra-rfc8693` — not adopted

`@apeleghq/hydra-rfc8693` v1.1.9 (Apache-2.0, Exact-Realty; explicitly *"not affiliated with
or endorsed by Ory"*). Deployed as your own service exposing `/token`, driving Hydra's
public + admin APIs. Because it mints **through Hydra**, the result is signed by Hydra's
keys, so existing JWKS validators keep working — the property that makes it usable at all.

**It does not validate the subject token.** It checks `subject_token` is present and
`subject_token_type` is in an allowed list, then hands the raw request body to a caller
supplied `userinfo: (body: URLSearchParams) => TSessionInfo` callback
(`src/exchangeTokenEndpoint.ts:39,216`) which returns the subject and claims. There is no
signature verification, introspection or JWKS anywhere in `src/`. The README example
hardcodes `subject: 'alice@example.com'` — as written, a total authentication bypass.

So it would close the audience-confusion deviation, but the security-critical half is yours
to write — and getting it wrong is a strictly worse failure than the deviation it fixes.
Deferred: revisit once the BFF authorization work is done and a second consumer of these
tokens exists.

### Oathkeeper — not adopted



Assessed against the vendored source (`reference-projects/ory/oathkeeper`, v26.2.0-83).

**Cannot replace `auth-proxy`.** Its pipeline is `authn -> authz -> mutate`, validating
credentials already on a request. There is no login/consent endpoint and no Hydra client
registration. Hydra still requires a login/consent app.

**Cannot fully replace `zero-ops-auth`.** No OAuth2 client-side flows exist in `pipeline/` —
it cannot obtain, refresh or store per-user delegated tokens. Object-level authorization
(principal tenant vs *resource* tenant) is also outside what a request-validating proxy can
decide, though `keto_engine_acp_ory` could carry it if resources were modelled in Keto.

**Cannot be wired as ext_authz behind AgentGateway.** AgentGateway's extAuthz speaks Envoy
`authorization` **gRPC** (`ext_authz.rs:15-19`); Oathkeeper's decision API is **HTTP**
`GET /decisions`. It would have to sit as an additional in-path reverse-proxy hop
(`proxy/proxy.go`).

**What it uniquely offers.** The `id_token` mutator renders `Claims` as a Go `text/template`
against the authentication session (`mutator_id_token.go:136`), so it mints a new
Oathkeeper-signed JWT carrying real per-user claims with a configurable audience. That is the
only mechanism in this stack that could fix the audience-confusion problem — Hydra has no
RFC 8693 and AgentGateway's `jwtSign` claims are static.

**Decision: not adopted.** It replaces roughly a third of one package and none of the other,
at the cost of a sixth identity component. Revisit only if closing the ID-token deviation
becomes a hard requirement.
