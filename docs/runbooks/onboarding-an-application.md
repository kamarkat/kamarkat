# Onboarding an application to the platform

**For product teams.** What the platform gives you, what you must declare, and
what breaks if you get it wrong.

This is a guide, not a decision. Every rule here is decided in an ADR and linked
to it; where the two disagree, the ADR is right and this file is stale — say so.

> **Why this exists.** The rules were spread across ten ADRs, each recording one
> decision correctly and none answering "what do I do". A team could read all ten
> and still not know which environment variables arrive on their own. That gap
> was costing a day at a time to failures whose message named a Kubernetes object
> rather than the declaration that produced it.

---

## 1. The shape of it

Two repositories, and the split matters:

```
<tenant>-gitops          your fleet DECLARATION — one values.yaml per app, per environment
your application repo    your charts and code
```

You declare *what you need* in the gitops repo. The platform renders *how it is
delivered*. You never write a Secret, an ExternalSecret, a NetworkPolicy, a
Gateway listener, or an OAuth client — if you are writing one of those, stop and
ask, because the platform already renders it and two producers of one fact is the
failure this model exists to prevent (ADR-047).

---

## 2. What arrives without you asking

Deployed into your namespace by the platform. **Do not declare these, do not
re-derive them, do not hard-code their values.**

### ConfigMap `tenant-public-endpoint`

| key | what it is |
|---|---|
| `PUBLIC_BASE_URL` | your app's public origin, scheme included |
| `OIDC_ISSUER_URL` | the issuer your workloads validate against |
| `OIDC_JWKS_URL` | **not derivable** from the issuer — see below |
| `OAUTH_<NAME>_REDIRECT_URI` | the callback the platform registered, per declared client |

`OIDC_JWKS_URL` is published rather than assembled because `/.well-known/jwks.json`
is a convention, not a specification. Zitadel serves `/oauth/v2/keys` and 404s the
conventional path, and the failure arrives *after* a login has succeeded, as
`Expected 200 OK from the JSON Web Key Set HTTP response` — which reads as an
unreachable issuer rather than a wrong path.

### Secret `agentgateway-<app>-oidc-client`

`oidc-client-id`, `oidc-org-id`, `oidc-project-id`, `oidc-exchange-client-id`,
`oidc-exchange-client-secret`, plus one pair per declared dependency.

These are **allocated by the issuer at provisioning time**, so they cannot exist
when your chart is rendered. That is why they travel as a Secret and not as a Helm
value (ADR-053).

### Secrets delivered to your workloads

**This is what you mount.** The gateway's Secret above is the gateway's; these are
yours. Keys are already spelled as the `PLATFORM_ENV` names, so map them straight
into your container environment.

| Secret | keys | who reads it |
|---|---|---|
| `<app>-platform-identity` | `OIDC_CLIENT_ID`, `OIDC_ORG_ID`, `OIDC_PROJECT_ID` | any workload validating a token |
| `<app>-platform-credentials` | `<APPID>_INTERNAL_TOKEN`, `OIDC_EXCHANGE_CLIENT_ID` | your BFF and SDK |
| `<app>-consumer-callers` | `OIDC_ALLOWED_AZP` | your SDK, **only if** you declared `allowedConsumers` |

Three objects rather than one, because an ExternalSecret is **atomic** — one
unresolvable key fails the whole object. They are grouped by *when the value
exists*, not by who reads it: `OIDC_ALLOWED_AZP` is written only when another
application declares a dependency on you, so for an application nobody calls it
does not exist, and putting it in with the others would fail all of them.

`<app>-consumer-callers` is rendered from the same `allowedConsumers` declaration
as your cross-app ingress, so the Secret and the network rule cannot disagree
about who may call you.

### Credentials the platform generates for you

Written to your Infisical folder and delivered by ExternalSecret. You declare the
*capability*; the credential follows (ADR-087):

- database credentials — owner and runtime roles (ADR-093)
- `CACHE_PASSWORD` — if your app has a cache
- `<APPID>_INTERNAL_TOKEN` — the BFF→SDK channel (§6)
- `OWNER_INITIAL_PASSWORD` — first-login credential for the tenant owner
- `OAUTH_<NAME>_CLIENT_ID` / `_SECRET` — per confidential client you declare

**You never supply any of these.** If one is missing, that is a platform bug —
report it, don't work around it.

---

## 3. What you declare

`environments/<env>/<app>/values.yaml` in the gitops repo.

```yaml
tenantId: nutgraf          # the CUSTOMER
appId: oranger             # the PRODUCT — these are different axes (ADR-088)
cellId: nutgraf-01         # which spoke this runs on
tier: starter

public:
  hosts: [oranger.dev.nutgraf.in]

gateway:
  enabled: true
  hostnames: [oranger.dev.nutgraf.in]

identity:
  selfRegistration: true
  backendDependencies: []  # apps THIS app calls        (§7)
  allowedConsumers: []     # apps allowed to call THIS  (§7)

database: {}               # you get an empty database and a role; the schema is yours (§6)
config: {}                 # non-secret values your workloads read
secrets: []                # secrets a HUMAN must supply (§5)
```

`tenantId` and `appId` are **not** the same thing and neither defaults to the
other. A customer running two products is one tenant with two apps; collapsing
them is the mistake ADR-088 exists to remove. Their combined length is capped at
49 characters, because Postgres truncates identifiers over 63 bytes *silently*,
which would map two apps onto one role.

---


## 6. Your database

The platform provisions a **database and a role**. Nothing is inside it (ADR-099).

No tables, no functions, no policies, no migration Job — the schema is yours,
applied by your own migrations, changed on your own schedule. A library the
platform ships cannot break your queue, because the platform owns no object in
your database.

**If you authenticate people, start from `zero-ops-auth/schema/identity.sql`.**
Copy it into your migrations. It is the two tables `resolveUser()` expects —
`users` and `identities` — and it exists because there is one way to get the
mapping wrong that never announces itself:

> Join on the **subject**, never on the email address. An address is mutable at
> the provider and reassignable between people, so a lookup keyed on it merges
> two accounts the moment an address changes hands, and the result is one person
> reading another's records with nothing reporting an error.

`resolveUser()` handles that, and the first-sighting race, which needs the
`UNIQUE (provider, provider_user_id)` constraint the file declares.

**Applications provisioned before 2026-10-01**: these tables already exist in
your database with your users in them. Adopting the file is a no-op —
`CREATE TABLE IF NOT EXISTS`. Do not drop and recreate: `identities` is how your
existing people are found. Two things have already run and must not be repeated:
the rewrite of `identities.provider` from `ory` to `zitadel`, and the removal of
the baseline's row-level security policies. `sessions`, `buckets` and `objects`
are also there, used by nothing; drop them whenever you like — they are yours.

## 4. What your chart must carry

The platform's policies key on these. A workload without them is rejected by
admission, not by review.

**Labels on every workload** (`kyverno-tenant-abi`, ADR-088):

```yaml
tenant-id: <tenantId>
app-id: <appId>
cost-center: <something>
```

**Role labels**, because platform network policy selects on them:

```
app: bff        your backend-for-frontend   :3001
app: sdk        your domain API             :3000
app: frontend   your SPA                    :3000
```

**Pod security**: `runAsNonRoot: true` is required.

Getting a role label wrong does not fail loudly — it produces a workload no policy
selects, so it is unreachable in a way that looks like a routing problem.

---

## 5. Secrets you actually supply

Only ones a **human chooses**: a third-party API key, a partner credential.
Anything the platform can generate, it does (§2).

Declare it, naming what is lost without it:

```yaml
secrets:
  - name: WHATSAPP_API_KEY
    capability: "outbound WhatsApp notifications from the SDK"
    workloads: [sdk]
```

Then supply the value — never in git, in any form:

```bash
kakt fleet secrets status <env> <app>      # what is declared vs present, by NAME
kakt fleet secrets set <env> <app> <KEY>   # echo disabled, or --stdin from a password manager
```

`workloads:` decides which ExternalSecret the key lands in, and that matters: an
ExternalSecret is **atomic**. One unresolvable key fails the whole object, so
every other key in it is withheld and every container reading any of them stays in
`CreateContainerConfigError`. Group by workload so a failure has a useful blast
radius (ADR-087).

---

## 6. Authenticating

**Use the platform's surfaces. Do not configure a validator by hand.**

```ts
import { browserSessionValidator, consumerApiValidator } from 'zero-ops-auth';

const validator = browserSessionValidator();   // a BFF behind your gateway
const validator = consumerApiValidator();      // a surface another APP calls
```

Both read their configuration from the environment, which comes from
`<app>-platform-identity` and `<app>-platform-credentials` above. Both **compare**
the tenant against `OIDC_ORG_ID` rather than merely requiring the claim to be
present — a sibling tenant's user carries a valid tenant claim, just a different
one, so presence alone is not isolation (ADR-094 invariant 3). You do not need a
separate tenant-boundary check on these surfaces.

`JwtValidator` has four security-relevant switches whose correct settings depend
on facts about the issuer and the gateway that are not visible from your
repository. **A wrong switch fails by accepting a token, not by erroring** — so
the platform owns them (ADR-050). There is no option to accept an ID token.

**BFF → your own SDK** uses the internal channel, not a token:

```ts
import { internalCallHeaders, requireInternalCaller, forwardedPrincipal } from 'zero-ops-auth';
```

It throws when the token is absent rather than sending the call without it. This
is an **intra-application** channel only — the identity is asserted by the caller,
which is fine between two workloads of one app behind a network policy and is not
fine across an application boundary (ADR-057).

---

## 7. Calling another application

**Both sides declare, or it is not a dependency** (ADR-088, ADR-094).

```yaml
# oranger's values.yaml            # waypoint's values.yaml
identity:                          identity:
  backendDependencies: [waypoint]    allowedConsumers: [oranger]
```

From those two lines the platform renders four things: your egress, the audience
scope your login requests, their ingress, and your client in their caller
allowlist. You write none of them.

**One token serves both hops.** Your gateway mints a token whose `audiences`
include your own project *and* each declared target's, so the same token
satisfies your own BFF and travels unchanged to theirs. Your SDK forwards what it
received and performs no exchange of its own — the exchange client's credentials
are delivered to the gateway alone, deliberately.

A preflight fails the build if only one side declares. Both one-sided states are
silent at runtime — a missing ingress **hangs** rather than refusing, because
Cilium drops denied ingress without an RST.

The receiver gets exactly three facts, all from the validated token: `tenant_id`,
`consumer_app_id` (from `azp`), and `sub`. **No email, no name, no roles, no user
id.** Your users stay yours; the receiver keys its records by an opaque subject
(waypoint ADR-042).

What the token does **not** establish is what that user may *do* in the receiver.
That is ADR-094 Part 3 and it is **not decided** — do not build against it.

---

## 8. When it breaks

| symptom | actual cause |
|---|---|
| `CreateContainerConfigError` naming a K8s Secret | an ExternalSecret could not resolve — check the *Infisical key*, not the Secret |
| ExternalSecret `SecretSyncedError` | a declared key has no value, or the path is outside your cell's prefix |
| login succeeds, first API call 401s | issuer or JWKS mismatch between gateway and workload — you hard-coded one |
| `redirect_uri_mismatch` before any credential is typed | you typed a callback instead of reading `OAUTH_<NAME>_REDIRECT_URI` |
| cross-app call **hangs** to timeout | the target did not declare `allowedConsumers` — ingress is denied and Cilium sends no RST |
| `invalid_target` on token exchange | you did not declare `backendDependencies`, so no audience scope was requested |
| `CALLER_NOT_ALLOWED` | your `azp` is not in the target's allowlist — usually the dependency was never reconciled |
| workload unreachable, no error anywhere | a missing or wrong `app:` role label — no policy selects it |

The pattern worth internalising: **the platform's failures name the delivery
object, not the declaration that produced it.** Read one hop back.

---

## References

ADR-047 a fleet declares, the platform renders ·
ADR-050 tenant authentication, and who owns validator configuration ·
ADR-053 OAuth client secret delivery ·
ADR-057 tenant user management, the BFF→SDK channel ·
ADR-087 configuration and secrets are separate ·
ADR-088 tenant and application are different axes ·
ADR-093 database owner vs runtime role ·
ADR-094 one identity project per application ·
ADR-095 access tokens for APIs ·
ADR-096 one owner for the shared TLS Gateway ·
waypoint ADR-042 consumer applications call the SDK directly
