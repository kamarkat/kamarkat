# ADR-116: The Box Serves Its Platform over MCP

**Date:** 2026-11-24

**Status:** Proposed

**Relates to:** ADR-043 (authority model), ADR-053 (fleet-declared clients), ADR-058 (OIDC
group-based RBAC), ADR-060 (Zitadel), ADR-065 (the control plane ships into the box),
ADR-066 (the platform boundary), ADR-067 (support telemetry), ADR-071 (every capability
ships to every tenant), ADR-076 (reaching a box you own), ADR-077 (the support agent),
ADR-078 (observability, addenda 8 and 10), ADR-094 (one identity project per
application), ADR-095 (access tokens for APIs), ADR-097 (service-to-service is client
credentials), ADR-103 (declared public routes), ADR-110 (an application acting for its
user carries the user's access token), ADR-115 (a cluster is accepted, or it is not)

## Context

- A box is operated by people through Git (ADR-065), through the box's identity provider
  (ADR-076), and through the hub's Grafana (ADR-078 addendum 10). An agent has no
  equivalent surface: to answer "why is boundary 05 degraded" it needs the same facts a
  person reads from ArgoCD, Crossplane, CAPI, cert-manager, CNPG and Grafana, and today it
  has none of them.
- Kamarkat's agents run in a client the platform's creator hosts (kakt), or in a copy of
  that client the tenant runs. Either way the agent is outside the box.
- `cmd/mcp-server` exists and is deployed at boundary 04
  (`manifests/argocd/components/04/mcp-server.yaml`). It serves a hardcoded `tenant_list`
  and agent-registry tools against a PostgREST address that does not resolve. It has no
  authentication, and no box depends on it.
- The box runs Zitadel (ADR-060, v4.19.2), one organisation per tenant with one project
  per application (ADR-094). Receivers validate access tokens, never ID tokens (ADR-095).
  An application acting for a person uses the delegated flow and keeps the tokens
  server-side (ADR-110). An application acting as itself uses client credentials
  (ADR-097).
- Zitadel offers OAuth 2.0 Dynamic Client Registration (RFC 7591), which generic MCP
  clients use to register themselves. Open registration is instance-wide, places clients
  in the instance's default organisation, and gives every registered client one shared
  `ZITADEL DCR` project audience. The `resource` parameter (RFC 8707) is accepted and
  ignored. A token can be revoked only by rotating or deleting its client.
- The hub's Grafana signs people in through Zitadel and is the one read interface over
  every cluster's telemetry (ADR-078 addendum 8). `grafana/mcp-grafana` is the upstream MCP
  server for Grafana. It has a read-only mode (`--disable-write`), and it authenticates its
  callers with a static bearer token.

## Decision

**The box serves its platform's state to agents over the Model Context Protocol, from
inside the box, read-only, as the person who asked. Changes are still made only by
committing to the box's repository.**

### 1. Two MCP servers, one front door

- **`kamarkat-mcp`** replaces `cmd/mcp-server`. It is a platform component on the
  management cluster, and it serves the platform's own state.
- **`mcp-grafana`** is deployed unmodified, in read-only mode, beside the hub's Grafana.
  It holds a Grafana service account with the Viewer role, and only `kamarkat-mcp` can
  reach it.
- Both are reached at one public route, `mcp.<hub-domain>` (ADR-103):
  `/platform` is served by `kamarkat-mcp`, and `/grafana` is proxied by `kamarkat-mcp` to
  `mcp-grafana`. Every request is authenticated once, by `kamarkat-mcp`, as §3 describes.
  `mcp-grafana`'s caller token never leaves the cluster.
- Each is a capability under ADR-071. It ships in every bundle, does nothing until the
  management cluster's `values.yaml` enables it (`capabilities.platformMcp`,
  `capabilities.grafanaMcp`), and is enabled independently of the other.

### 2. What `kamarkat-mcp` serves

Its tools are grouped in categories, and each category can be disabled:

| Category | Answers |
|---|---|
| `box` | what the box declares, and whether running state matches the declaration |
| `clusters` | each cluster's claim, CAPI and node conditions, and its ADR-115 acceptance verdict, reported separately from convergence |
| `gitops` | each boundary's Applications, their health, sync state, diff and events |
| `platform` | boundary readiness, component versions, policy violations |
| `apps` | declared applications, their workloads and status, and which declared secrets are present, **by name only** |
| `certs`, `backups` | certificate expiry and issuance; database backup and WAL archive state |
| `envs` | ephemeral environments: their claim and their acceptance |
| `verify` | runs a cluster's acceptance suite and returns its verdict (off by default) |

It serves no tool that creates, changes or deletes a resource, except `verify`, which
creates the acceptance Job and nothing else. No tool returns secret material. Each tool
declares the Kubernetes permissions it needs and whether it is read-only, and the build
fails a tool that declares neither.

This is deliberate. A change to a box is a commit to its repository, reconciled by the
box's own controllers (ADR-043, ADR-065). An MCP server that applied changes would be a
second authority over every domain it touched.

### 3. Identity: declared clients, the person's own token, no dynamic registration

- **`kamarkat-mcp` is an application** under ADR-094: a `kamarkat-mcp` project in the
  tenant's organisation, with the roles `platform.viewer`, `platform.observer` and
  `platform.verifier`.
- **Its clients are declared, never registered at runtime.** Enabling the capability
  declares them (ADR-053):
  - `kakt`: a **public** client using the authorization-code grant with PKCE, plus the
    refresh grant, with the redirect URIs of the kakt deployments the tenant names.
    It is public on purpose. A confidential client's secret is issued in the box and
    would have to be carried to kakt, which is a box secret leaving the box (ADR-065).
    A public client has nothing to carry, and its id is published in the resource
    metadata (below). Tokens are held server-side by kakt, sealed, per person
    (ADR-110). The browser and the agent's sandbox never hold them. It holds no
    token-exchange grant.
  - `kakt-cli`: a public client using PKCE with a loopback redirect, for `kakt mcp`.
  - `kakt-agent` (optional, off by default): a service user that authenticates with a
    private-key JWT and holds `platform.viewer` only. It is used by agents that run with
    no person present. The tenant enables it, and revokes it by deleting its key.
- **Dynamic client registration stays disabled.** It would put clients in the wrong
  organisation, give them a shared audience, and leave nothing that could be revoked
  except the client itself. Declaring a client is a commit, and a commit is the tenant's
  consent.
- **Each request is authorised twice.**
  - `kamarkat-mcp` validates the access token: the issuer, the `kamarkat-mcp` audience,
    `at_hash` absent (ADR-095), and an authorised party it declared. It then admits a
    tool category only if the token carries that category's role (`platform.viewer` for
    the platform categories, `platform.observer` for `/grafana`, `platform.verifier` for
    `verify`).
  - It then **impersonates** the person against the Kubernetes API: their email, which
    is the name the API server gives an OIDC user (`--oidc-username-claim=email`), plus
    one group per project role the token carries (`kamarkat-mcp:platform.viewer`, …).
    The capability binds `kamarkat-mcp:platform.viewer` to a read-only ClusterRole with
    no access to Secrets. So a role granted in the issuer is what gives read access,
    and revoking it there takes effect on the person's next token. Its own
    ServiceAccount may impersonate those three groups and any user, and nothing else.
- `kamarkat-mcp` publishes OAuth Protected Resource Metadata (RFC 9728) at
  `/.well-known/oauth-protected-resource/platform` (and `/grafana`). It names the box's
  issuer, plus a `kamarkat` member with the `kamarkat-mcp` project id, the declared
  client ids and the enabled capabilities. Ids and names are not credentials, so any
  MCP client can discover how to sign in without being given anything.

### 4. Who can connect

| Client | Where it runs | How it signs in |
|---|---|---|
| kakt, hosted by the platform's creator | the creator's cloud | `kakt` client, the person's delegated token |
| kakt, run by the tenant or a developer | in the box, or on a workstation | the same client, with that deployment's redirect URI declared |
| any MCP client (Claude Code, Cursor, …) | a workstation | `kakt mcp`, a local stdio server that reads with the person's own kubeconfig: the OIDC one ADR-076 issues, or the break-glass one when the issuer is what is down. The `kakt-cli` client is provisioned for a later sign-in flow of its own. |

### 5. The connection is a person's, not the platform's

ADR-067 says no platform component initiates a connection into a tenant's cluster. A
kakt connection does not breach that rule, but only because of how it is built:

- The connection is made **as a person**, with that person's token, issued by the box's
  own identity provider, to a client the tenant declared.
- It reads only what that person's RBAC allows, and it changes nothing.
- The tenant ends it by removing the client from the declaration, by disabling the
  capability, or by revoking the person's sessions.

The one exception is `kakt-agent`, which is standing access by design. It is off unless
the tenant turns it on, limited to `platform.viewer`, and stated as such. The support
agent (ADR-077) is unchanged. It reports outward, holds no Kubernetes read access, and is
not this component.

### Alternatives considered

- **Open dynamic client registration.** Rejected for the reasons in §3.
- **An outbound tunnel from the box to kakt.** It would keep ADR-067 literal, but it
  needs a relay service the creator would run, it would not serve the tenant's own MCP
  clients, and it changes nothing about who is authorised.
- **Exposing `mcp-grafana` directly.** Rejected. Its static caller token cannot be tied
  to a person, a role or a revocation.
- **Token exchange with an `act` claim naming the agent.** It is deferred. The agent's
  client would need the token-exchange grant, which ADR-110 withholds from user clients,
  and its actor would need `ORG_ADMIN_IMPERSONATOR` to act for any administrator. Calls
  are attributed through the authorised party and the caller's session id instead.

## Ownership

| Resource Class | System of Record | Lifecycle Owner | Reconciler | Consumer | Phase |
|---|---|---|---|---|---|
| `kamarkat-mcp` and `mcp-grafana` Deployments | platform bundle | platform | ArgoCD (boundary 04) | MCP clients | Day-1+ |
| Capability enablement, client redirect URIs, `kakt-agent` | management cluster `values.yaml` in the tenant's repository | tenant | ArgoCD | `kamarkat-mcp`, Zitadel provisioning | Day-1+ |
| `kamarkat-mcp` project, roles, clients | Zitadel | tenant (declared) | platform identity provisioning (ADR-053) | `kamarkat-mcp` | Day-1+ |
| Role grants to people | Zitadel | tenant administrators | — | `kamarkat-mcp` | Day-1+ |
| `mcp-grafana` Grafana service account token | Infisical | platform | ESO | `mcp-grafana` | Day-1+ |
| `mcp-grafana` caller token | Infisical | platform | ESO | `kamarkat-mcp`, `mcp-grafana` | Day-1+ |

## Consequences

### Positive

- An agent sees what an operator sees, under the same identity and the same RBAC, with no
  credential the tenant did not issue to a person.
- The same server serves the creator's hosted client, a self-hosted client, and any
  MCP client on a workstation, so an open-source user needs no hosted service to operate
  a box with an agent.
- Logs and metrics reach agents through Grafana's own MCP server, with no second query
  implementation to maintain.

### Negative

- `mcp.<hub-domain>` is a new public surface. It is authenticated on every request, but
  it exists whenever the capability is enabled.
- Grafana sees `mcp-grafana`'s service account rather than the person. People are
  separated at `kamarkat-mcp` by the `platform.observer` role, not inside Grafana.
- Every redirect URI a kakt deployment uses must be declared before that deployment can
  connect.

## Impact

- `cmd/mcp-server` keeps its path and image name and now builds `kamarkat-mcp`, from
  `internal/platform/boxmcp`. Its boundary-04 descriptor and
  `manifests/hub-core-services/api-gateway/mcp-server/` are removed, replaced by
  `manifests/hub-core-services/kamarkat-mcp` and `manifests/hub-core-services/mcp-grafana`,
  each applied by a boundary-03 element gated on its capability. The hub gateway gains
  the `mcp` listener and route under the same gate.
- kube-sbt gains `POST /internal/platform/projects/roles` (several roles in one grant)
  and `POST /internal/platform/public-clients`. The hub-operator gains
  `PlatformMcpReconciler`, watching the `platform-mcp/kamarkat-mcp` marker.
- The `kakt-agent` is built with a key pair kakt generates. The tenant declares the
  public half (`capabilities.platformMcp.agentPublicKey`). The hub-operator registers
  it for a machine user holding `platform.viewer` (kube-sbt
  `POST /internal/platform/machine-key-users`). kamarkat-mcp accepts that user only
  while the declaration stands. No private key exists in the box.
- The `verify` category serves no tools, deliberately. Assessing acceptance needs
  the management cluster's escrowed kubeconfig, which is privileged work, so it runs
  in the box repository's **Acceptance** workflow (scheduled and on demand,
  `kakt verify --accept`), next to Day-0. Agents read its verdicts through GitHub.
- Declaring a managed cluster no longer needs escrow access at the keyboard:
  `kakt tenant add-cluster --declare-only` writes the declaration, and the
  repository's **Finalize cluster** workflow runs `kakt tenant finalize-cluster` on
  the pull request.
- ADR-067 is clarified by §5: a connection made as a person, with that person's token
  and the tenant's declared consent, is not a platform component initiating access.
- `kakt` gains `kakt mcp`.
- `tenant add-cluster` is unaffected by this ADR. Its privileged steps are addressed
  separately.

## References

- ADR-039: Platform Ownership Model
- ADR-043: Control Plane Authority Model
- ADR-065, ADR-067, ADR-071, ADR-077, ADR-078, ADR-094, ADR-095, ADR-097, ADR-110
- `kamarkat/reference-projects/mcp-grafana`: category toggles, `--disable-write`,
  per-tool required permissions, caller authentication
- Zitadel: Dynamic Client Registration, Token Exchange, Identity Patterns for AI
- RFC 7591, RFC 8693, RFC 8707, RFC 9728
