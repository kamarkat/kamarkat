# ADR-060: Zitadel as the Platform Identity Provider

**Date:** 2026-09-04
**Status:** Accepted

## Context

The platform authenticated tenants against Ory (Hydra for OAuth2, Kratos for
identities, Keto for relationships). In that stack a tenant is an ATTRIBUTE
written onto an identity, not a property of the identity's existence. Four
consequences followed from that single fact, each observed in the platform:

- An identity could be created with no tenant. Tokens minted for such an
  identity were rejected downstream as `Token missing required tenant_id claim`,
  after a login had already succeeded — the failure surfaces on the first API
  call, not at sign-in.
- A platform administrator legitimately belongs to no tenant, so the rule "every
  token carries a tenant" made administrators unrepresentable. The platform
  added a group-based exemption to admit them, which is a hole widened by
  anything able to claim membership of that group.
- Authorisation had no home in the identity provider, so the platform built one:
  a ConfigMap of group assignments reconciled into Kratos `metadata_public` by a
  controller in the hub-operator (ADR-058). This modelled authorisation BESIDE
  the provider rather than in it, and the two could drift with nothing to
  reconcile them.
- Self-service registration produced identities with no tenant, since nothing in
  the registration flow carried one.

ADR-057 established tenant user management on the Ory stack. ADR-058 established
group-based Kubernetes RBAC on top of it. ADR-041 assigns tenant identity
lifecycle to the Tenant Identity Service and forbids the Hub Operator from
managing tenant credentials. ADR-053 derives a tenant's OAuth client identifier
from its tenant id.

## Decision

The platform's identity provider is Zitadel, selected because its tenancy is
STRUCTURAL: an Organization owns a user rather than describing one. A user
belonging to no tenant is not expressible, so the four consequences above cease
to be conditions the platform must detect and become states the provider cannot
produce.

The platform adopts the provider's own model rather than reproducing its
previous one:

| Platform concept | Provider mechanism |
|---|---|
| Tenant | Organization, which owns the user and appears as the token's resource owner |
| Authorisation | Project roles granted to a user within an organisation |
| Tenant administrator | Organization owner membership, scoped to one organisation |
| Cross-tenant access | Project grant from the owning organisation to another |
| Tenant discovery at login | Organization scope on the authorization request |
| Registration policy | Organization-level login policy |
| Platform's own identities | The platform is itself an organisation, with no special case |

Two provider-side checks move tenant isolation in front of token issuance. One
denies authentication to a user holding no role in the project; the other denies
it to a user whose organisation was never granted the project, with the owning
organisation passing implicitly. A user of another tenant is refused at the
authorization endpoint, so no token reaches an application that would otherwise
have to check — and a check that lives in an application is one an application
can omit.

Because the provider denies a user holding no role, granting a role is part of
creating a usable account rather than a later step. Accounts arriving by
self-service registration have no such grant and receive the default role by
reconciliation, since the provider exposes no event at sign-up. This makes the
grant eventually consistent, which is accepted deliberately: the alternative is
disabling the role check, and that check is also what refuses users of other
organisations.

Group-based RBAC is not reproduced. Kubernetes authorises on users and groups,
and the provider emits roles as a nested structure keyed by granting
organisation, so the claim cannot be consumed by the API server directly. Until
that mapping exists, Kubernetes RBAC binds to the username and the group claim
is left unset rather than pointed at a claim that yields nothing.

### OAuth client provisioning inverts

A tenant needs two OAuth clients, and they differ in a way that decides who
creates them. The gateway authenticates a PERSON with a public client, where PKCE
carries the proof and no secret exists. The BFF then exchanges that session for a
token of its own to call the platform API with — a server-side flow, which by
definition authenticates the CLIENT and therefore needs a secret.

Under Hydra the platform was the producer of that secret: it generated one into
the tenant's Infisical path and hydra-maester reconciled an `OAuth2Client` CR to
push it into the issuer. That is why clients could be declared per fleet as a
list — each entry cost only a generated value and a CR.

Zitadel will not accept a supplied client secret. It generates one and discloses
it exactly once, at creation. The direction of the credential therefore inverts:
the issuer is the producer, and the platform's job is to capture the value and
publish it. There is no configuration that restores the old direction, so this is
a structural consequence of the provider rather than a preference.

Three things follow.

**The Tenant Identity Service provisions both clients**, because it is already the
component that talks to the issuer and already publishes an allocated value
(ADR-041 assigns it both identity lifecycle and credential upload). The Hub
Operator stops generating OAuth credentials entirely. Two producers writing the
same key would take turns overwriting each other, and the workload would hold
whichever wrote last while the issuer knew the other — a mismatch that surfaces
only at token exchange, naming neither writer.

**The client set is fixed rather than declared.** The platform creates
`<tenant>-public-client` and `<tenant>-bff`, which are the two the architecture
uses. A fleet-declared list no longer buys anything, since a fleet cannot supply
the credential, and it cost something: a list a fleet could extend was a list a
fleet could use to register redirect URIs the platform never reviewed. A third
client is a change to the service.

**Reads precede writes.** Because a generated secret cannot be read back, the only
way to recover one is to regenerate it — which invalidates the credential the
running workload holds. Provisioning therefore asks the secret store first and
regenerates only when nothing is stored, so a reconcile of an already-provisioned
tenant touches nothing.

### How a tenant request is authenticated

```
                      ┌──────────────────────────────────────────┐
   browser ──(1)────► │  agentgateway (tenant namespace, spoke)  │
                      └────┬────────────────────────────┬────────┘
                           │ (2) OIDC authorization     │ (5) forwards id_token
                           ▼                            ▼
                  ┌─────────────────┐            ┌─────────────┐
                  │     Zitadel     │            │     BFF     │
                  │  id.<zone>      │◄──(6)──────┤             │
                  │                 │  code      └──────┬──────┘
                  │  org  = tenant  │  exchange         │ (7) delegated token
                  │  proj = roles   │  (confidential)   ▼
                  └────────┬────────┘            ┌─────────────────┐
                           │ (3) iss / JWKS      │  api.<zone>     │
                           │                     │  MCP gateway    │
                           ▼                     └─────────────────┘
                  /oauth/v2/keys ──(4)──► validated by gateway AND BFF

  Provisioning (out of band, on tenant reconcile):

    AINativeSaaS XR ──► Hub Operator ──► Tenant Identity Service
                                                  │
                            creates in Zitadel:   │
                              <tenant>-public-client   (auth method NONE)
                              <tenant>-bff             (auth method BASIC)
                                                  │
                            publishes to Infisical:
                              /spoke-pool/<cell>/tenants/<tenant>/
                                  OIDC_CLIENT_ID
                                  OAUTH_BFF_CLIENT_ID
                                  OAUTH_BFF_CLIENT_SECRET
                                                  │
                                                  ▼
                            ExternalSecret ──► tenant namespace Secret
                                                  │
                                                  ▼
                            agentgateway reads OIDC_CLIENT_ID
                            BFF reads WAYPOINT_BFF_CLIENT_ID / _SECRET
```

Steps (1)–(4) are the browser login: the gateway sends the user to Zitadel, and
both the gateway and the BFF validate the returned token against the issuer's
signing keys at `/oauth/v2/keys`. Zitadel returns 404 for the conventional
`/.well-known/jwks.json`, which is why the JWKS URL is published beside the issuer
rather than assembled from it. Steps (5)–(7) are the delegated exchange, and are
the reason the confidential client exists at all.

The `auth.<zone>` hostname is split rather than retired. Its `/.well-known/` and
`/internal/` prefixes are served by auth-proxy, which passes Zitadel's discovery
document through unmodified — the document must keep naming `id.<zone>` as the
issuer, because that is what tokens carry and what a relying party compares
byte-for-byte. Everything else on that hostname redirects to Zitadel, which hosts
its own login.

### Configuration abstraction

Identity configuration is split by who may decide it, extending the boundary
ADR-050 draws for the tenant gateway. A fleet declares what it wants for its own
tenant; the platform decides how that is delivered and what a fleet may not
reach.

Platform-owned, supplied by the environment and never by a fleet:

| Setting | What it fixes |
|---|---|
| Issuer URL | Which identity provider a spoke trusts |
| JWKS URL | Where that issuer publishes signing keys, which is not derivable from the issuer |
| Additional scopes | Which provider-specific claims a token must carry |
| Legacy stack enablement | Whether the superseded provider is deployed at all |

A fleet able to set any of these could point its own login flow at an issuer the
platform does not trust.

Fleet-owned, declared on the tenant record:

| Setting | What it decides |
|---|---|
| Gateway enablement and hostnames | Whether this fleet has a browser surface, and the names it answers on |
| Owner email | Who administers this tenant |
| Self-service registration | Whether people may create their own accounts in this tenant |
| OAuth clients | Which clients this fleet needs credentials for (ADR-053) |

Allocated by the provider and published to a fleet's workloads, because the
values do not exist when a fleet's manifests are rendered:

| Value | Delivery |
|---|---|
| Issuer URL, JWKS URL, public base URL | ConfigMap in the tenant namespace |
| OAuth client id, organisation id | Secret, via the platform's secret path (ADR-003) |

The client id is published rather than derived because the provider allocates it
and will not accept a chosen one. ADR-053's authority rule is unchanged — the
value remains platform-owned — but its origin moves from derivation to lookup.

### Alternatives considered

**Remain on Ory and add tenant enforcement.** Rejected: every mechanism above
would have to be built and maintained by the platform — a required-claim check,
an exemption for identities the rule cannot represent, a reconciler for group
assignments, and a registration flow that carries a tenant. Each is a place the
platform's model can drift from the provider's.

**A broker in front of the provider.** Recorded in ADR-059 as the answer for
consumers whose configuration is baked in rather than read at runtime. Not
adopted here: it adds a component to the authentication path, and the only
consumer needing it today is the Kubernetes API server, which one control-plane
rollout already serves.

## Ownership

| Resource Class | System of Record | Lifecycle Owner | Reconciler | Consumer | Phase |
|---|---|---|---|---|---|
| Tenant Organization | Identity Provider | Tenant Identity Service | Tenant Identity Service | Tenant users | Day-1+ |
| Tenant Project and Roles | Identity Provider | Tenant Identity Service | Tenant Identity Service | Tenant gateway, tenant workloads | Day-1+ |
| Tenant OAuth Client | Identity Provider | Tenant Identity Service | Tenant Identity Service | Tenant gateway | Day-1+ |
| Tenant OAuth Client ID | Infisical | Tenant Identity Service | Hub Operator | Tenant gateway, fleet workloads | Day-1+ |
| Tenant Role Grants | Identity Provider | Tenant Identity Service | Tenant Identity Service | Identity Provider | Day-1+ |
| Organization Login Policy | Identity Provider | Tenant Identity Service | Tenant Identity Service | Identity Provider | Day-1+ |
| Platform OAuth Clients | Identity Provider | Tenant Identity Service | Tenant Identity Service | Kubernetes API server | Day-1+ |
| Issuer Endpoint Configuration | Bootstrap artifacts | CLI | Environment Manager | Tenant gateways, fleet workloads | Day-0 |
| API Server OIDC Configuration | Cluster topology | CLI | CAPI topology controller | kubectl, dashboards | Day-0 |

## Consequences

### Positive

- A tenant-less identity is not expressible, so the class of failure that
  produced repeated post-login rejections cannot occur.
- Tenant isolation is enforced before a token exists, so an application cannot
  omit the check.
- A platform administrator is an ordinary member of the platform's own
  organisation, which removes the exemption that previously admitted them.
- Authorisation lives in one model rather than two, so there is nothing to keep
  in step and nothing to drift.
- Registration is decided per tenant, so one fleet's choice does not bind
  another and the platform's own organisation stays closed regardless.

### Negative

- Role grants for self-registered users are eventually consistent. A person
  registering between reconciliations is refused until the next one.
- Self-service registration does not restrict who may sign up. Anyone reaching a
  tenant's hostname can create an account in it, and restricting that requires
  invitations, which require a mail sender the issuer does not have.
  **Amended 2026-09-26 — the issuer now has one; see the amendment below.**
- Group-based Kubernetes RBAC does not survive the change, so cluster access is
  bound per identity rather than per group.
- The provider allocates identifiers the platform previously chose, so values
  that were derived must now be published through the credential path.
- Organizations, projects and policies have no declarative resource type, so
  desired state is expressed on the tenant record and reconciled through the
  provider's API rather than applied directly.

## Impact

- **Completes the supersession of ADR-058** recorded by ADR-059. Its
  ConfigMap-to-identity-metadata pipeline is removed, and the requirement it
  served — that Kubernetes receives a group claim — remains unmet pending a
  mapping from the provider's role structure.
- **Amends ADR-057.** Tenant users are owned by an organisation rather than
  carrying a tenant attribute, and the platform no longer maintains that
  attribute.
- **Supersedes ADR-053's mechanism.** A tenant's OAuth client identifier is
  allocated by the provider rather than derived from the tenant id, and the
  confidential client's secret is GENERATED BY THE PROVIDER rather than minted by
  the platform and pushed. The hydra-maester `OAuth2Client` pipeline and the
  per-fleet client list are removed with it; the requirement ADR-053 served — that
  a tenant's server-side client has a credential nothing else can read — is kept.
  Authority over the value is unchanged.
- **Amends ADR-050.** The environment supplies a JWKS URL and additional scopes
  alongside the issuer, all fleet-forbidden.
- **Depends on ADR-059** for the contract that makes the provider replaceable,
  and for the constraint that a provider must be able to mint an issuer the
  platform chooses.
- **Depends on ADR-041** for the assignment of tenant identity lifecycle, which
  places provisioning in the Tenant Identity Service rather than in the Hub
  Operator or Crossplane.

## Amendment 2026-09-26: the issuer owns the credential lifecycle

**The Negative above — "invitations, which require a mail sender the issuer does
not have" — no longer holds, and while it held it cost more than it recorded.**

### What the original entry missed

It named the consequence for self-service REGISTRATION and stopped there. The same
absent mail sender also means no first-login mail and no password-reset mail for an
owner the platform PROVISIONED, and that is the consequence that failed in
practice. Because the ADR recorded the limitation only as a registration
restriction, the provisioning path was written as though the gap were closed:

- `kube-sbt` generated a random password (`generateInitialPassword`) and created
  the owner with it, its comment restating this ADR's reasoning almost verbatim;
- it was sent with `changeRequired: false`, so the issuer never prompted for a
  replacement and that random string became the account's standing credential;
- `hub-operator` filed it in the tenant's secret store as
  `OWNER_INITIAL_PASSWORD`, which **nothing in the repository ever read**.

The owner could therefore authenticate only if a human first read a secret out of
Infisical on their behalf. Observed on a tenant owner created 2026-09-22: the
bootstrap hash still in place four days later, exactly one
`user.human.password.check.succeeded` in the account's entire history, and nothing
but `user.human.password.check.failed` after it. A browser holding the session
minted by that single success kept working, so the account presented as
intermittently broken rather than permanently un-enterable — the session masked it,
and the first device without a cookie exposed it.

A generated credential that no one can retrieve is not a credential. Writing it to
a store nothing reads is the same as discarding it.

### The decision

**Outbound transactional mail is delivered through an external provider, and the
issuer owns the credential lifecycle from account creation onward.** The platform
does not generate, store, or deliver end-user passwords.

- The issuer is configured with a mail sender, so its initialisation and
  password-reset flows are available. This is an external sending service, not a
  mail server the platform operates: ADR-054's rejection is untouched.
- A provisioned owner is created WITHOUT a password. The issuer emails an
  initialisation link and the user chooses their own credential. `shouldAddInitCode`
  requires both that no password was supplied and that the address is not
  pre-asserted as verified, so BOTH conditions move — a supplied password or a
  pre-verified address independently suppresses the mail.
- `OWNER_INITIAL_PASSWORD` is retired, and with it the Infisical write no reader
  consumed.
- `changeRequired: true` remains for any caller that still supplies a password. It
  is a safety net, not the mechanism: it bounds the damage of a bootstrap
  credential rather than removing the need for one.
- Invitations, named above as unavailable, are now available — so restricting who
  may sign up becomes a decision a fleet can make rather than a capability the
  platform lacks.

### What this does not change

Registration policy is still per tenant, and enabling invitations does not by
itself close open registration. That remains a per-fleet choice.

## Open

**The MCP gateway does not validate an audience.** Hydra minted whatever audience
the consent step granted, so `https://api.<zone>/mcp` could simply be required.
Zitadel mints the client id and, with the project-audience scope, the project id —
both allocated, so neither can be written into a manifest at render time. Keeping
the old value would reject every token; omitting it means the gateway validates
the issuer and the signature but not the intended recipient, so a token minted for
another relying party of the same issuer is accepted. The issuer is the platform's
own, which bounds the exposure, but it is a real reduction and it is recorded here
rather than absorbed silently.

Closing it needs the platform project id published the way `OIDC_CLIENT_ID`
already is, and the authorization request carrying
`urn:zitadel:iam:org:project:id:<projectID>:aud` so the claim is present to check.

## References

- ADR-003: Secret Management Architecture
- ADR-039: Platform Ownership Model
- ADR-040: Day-0 vs Day-1 Lifecycle Boundary
- ADR-041: Controller Responsibility Matrix
- ADR-043: Control Plane Authority Model
- ADR-050: Tenant Authentication via AgentGateway
- ADR-053: Tenant OAuth Confidential Client Lifecycle
- ADR-057: Tenant User Management
- ADR-058: OIDC Group-Based Kubernetes RBAC (superseded by ADR-059)
- ADR-059: Provider-Agnostic OIDC Contract

## Addendum (2026-10-31): the login service's keypair is the platform's, not a render's — **Proposed**

The chart generates the login service's signing keypair unless it is given one.
Its reuse branch is guarded by Helm's `lookup`:

```
{{- $existingSecret := lookup "v1" "Secret" .Release.Namespace $secretName }}
{{- if and $existingSecret $existingSecret.data ... }}   # reuse
{{- else }}  $cert := genSelfSignedCert "login-service" ...
```

**ArgoCD renders with `helm template`, where `lookup` always returns empty.** The
reuse branch could therefore never fire on this platform: every sync of the
`zitadel` Application minted a new keypair, server-side-applied it, and rolled both
Deployments through the `checksum/secret-login-service-key` annotation.

### Why that was survivable until it wasn't

Zitadel reads `SystemAPIUsers` **once, at startup**. Kubernetes keeps the mounted
file current, but the process does not re-read it. So each regeneration opened a
window in which the login service signed assertions with a key the running issuer
did not trust, and the window closed only when the replacement Zitadel pod became
Ready.

On 2026-10-30 it did not close. The replacement pod could not schedule — the hub's
single worker was 25Mi short of its 512Mi request — so the old pod held the old
certificate in memory while the new login pod presented the new one. Every assertion
failed `AUTH-7fs1e` for five hours. Interactive sign-in survived only because the
superseded login pod was still serving; the deployment was deadlocked, not degraded,
because `maxUnavailable: 0` means the old pod cannot go until the new one is Ready
and the new one could not schedule until the old one went.

Deleting the old Zitadel pod resolved it in nine seconds: the replacement scheduled,
loaded the current certificate, and the login pod passed readiness two seconds later.

### Decision

**The platform owns this credential.** `login.loginServiceKeySecretName` names an
ESO-delivered Secret, `zitadel-login-service-key-managed`, so the chart's generation
branch is never reached.

- **The material is the pair the chart last rendered**, published to Infisical by
  `CLISecretMappings` from the live Secret.

  This was first written as "the pair already trusted ... naming it rotates nothing:
  no new key, no second rollout, no session disruption." That was wrong, and the
  rc.183 rollout showed why. The defect being fixed regenerated the keypair on
  *every* sync, so the live Secret held the most recently rendered pair — not the
  pair the running Zitadel read at startup 29 hours earlier. Publishing it froze a
  pair that was already newer than the one in the issuer's memory, the Zitadel pod
  template changed with the certificate inside `SystemAPIUsers`, and a restart
  became necessary rather than avoidable. Observed 2026-11-01: the new login pod
  signed with the frozen pair, the running Zitadel rejected it
  (`invalid signature`, AUTH-7fs1e), and sign-in survived only because the old login
  pod was still serving the old pair to the old issuer.

  The correct statement of the decision is narrower: **the material is frozen, not
  chosen.** Whatever pair was live at publication becomes the permanent one, and
  exactly one restart of the issuer is needed to adopt it. After that restart the
  certificate never changes again, which is the whole point — the recurring
  regeneration is what caused both incidents, not the value of any particular key.
- **`kubernetes.io/tls` with `tls.crt` and `tls.key`**, the shape the chart's own
  Secret had and its volume mount expects.
- **`deletionPolicy: Retain` and `Prune=false`.** Once the chart stops rendering a
  login-service-key Secret, ArgoCD sees the managed one as belonging to no rendered
  manifest. Pruned, the login service loses the key it signs with and the issuer
  refuses every sign-in on the box.

### What this does not fix

The hub worker runs at 96% of allocatable memory with 381Mi of headroom, so any
rollout of a 512Mi pod can deadlock the same way for a different reason. That is a
capacity decision — a second hub worker, or the dev store count ADR-078 add.9 leaves
open — and it is recorded here because this incident was its first symptom, not its
last.

A chart whose correctness depends on `lookup` is a general hazard under ArgoCD, not
a Zitadel one. Any chart that generates a credential when it cannot see the cluster
regenerates it on every sync here.
