# kamarkat-auth

Platform-wide authentication and authorization primitives for tenant services.

## Installation

```bash
npm install kamarkat-auth
# or
pnpm add kamarkat-auth
``` 

## Features

- **JWT Validation** — JWKS-based validation with RS256/ES256 support, per-key TTL cache, rate-limited refresh
- **Hono Middleware** — Drop-in `authMiddleware()` for Hono applications
- **OAuth Lifecycle** — OIDC client with PKCE, code exchange, refresh, and revocation
- **Token Store** — Pluggable token store interface with Redis-backed implementation (cross-replica mutex)
- **Tenant Boundary** — `requireTenantBoundary()` middleware for tenant isolation
- **Role Authorization** — `requireRole()` middleware for role-based access control

## Quick Start

### JWT Validation + Hono Middleware

```typescript
import { Hono } from "hono";
import { JwtValidator, authMiddleware } from "kamarkat-auth";

const app = new Hono();

const validator = new JwtValidator({
  jwksUrl: "https://auth.nutgraf.in/.well-known/jwks.json",
  issuer: "https://auth.nutgraf.in",
  audience: "waypoint-public-client",
});

app.use("/api/*", authMiddleware({ validator }));

app.get("/api/me", (c) => {
  const principal = c.get("auth:principal");
  return c.json({ user: principal });
});

export default app;
```

### Tenant Boundary

```typescript
import { Hono } from "hono";
import { JwtValidator, authMiddleware, requireTenantBoundary } from "kamarkat-auth";

const app = new Hono();
app.use("/api/*", authMiddleware({ validator }));

// Enforces: principal.tenantId == :tenantId
app.get("/api/v1/spokes/:tenantId", requireTenantBoundary(), (c) => {
  return c.json({ spokes: [] });
});

export default app;
```

### Role-Based Access Control

```typescript
import { Hono } from "hono";
import { JwtValidator, authMiddleware, requireRole } from "kamarkat-auth";

const app = new Hono();
app.use("/api/*", authMiddleware({ validator }));

app.delete("/api/:id", requireRole("admin"), async (c) => {
  // Only admins can delete
  return c.json({ deleted: true });
});

export default app;
```

### OAuth Delegation

```typescript
import { OidcClient, RedisTokenStore } from "kamarkat-auth";
import Redis from "ioredis";

const redis = new Redis();
const tokenStore = new RedisTokenStore(redis);

const client = new OidcClient({
  issuerUrl: "https://auth.nutgraf.in",
  clientId: "waypoint-bff-client",
  clientSecret: process.env.CLIENT_SECRET,
  redirectUri: "https://waypoint.nutgraf.in/api/v1/auth/oauth/callback",
  scopes: ["openid", "offline_access"],
  audience: "https://api.nutgraf.in/mcp",
});

// Build authorization URL with PKCE
const { codeVerifier, codeChallenge } = OidcClient.generatePkce();
const { url, state } = await client.buildAuthorizationUrl({
  codeChallenge,
  codeChallengeMethod: "S256",
});
// Redirect browser to url

// Exchange code for tokens
const tokens = await client.exchangeCode({ code, codeVerifier });
await tokenStore.put(principal.subject, tokens);

// Refresh tokens (with cross-replica mutex)
const freshTokens = await tokenStore.refreshToken(
  principal.subject,
  () => client.refreshTokens(tokens.refreshToken!),
);
```

### Browser: reacting to the session

A tenant application should not implement authentication logic. It listens.

```ts
import { createAuthClient } from "kamarkat-auth/client";

const auth = createAuthClient({ baseUrl: "" });

const unlisten = auth.listen((e) => {
  switch (e.event) {
    case "signedIn":
      console.log("welcome", e.data.email);
      break;
    case "sessionExpired":
      // The person did NOT ask to leave. Tear down session-scoped UI and
      // send them to sign in again.
      window.location.href = "/";
      break;
    case "checkFailed":
      // Unknown, not signed out. Do nothing.
      break;
  }
});

await auth.refresh();
const stopWatch = auth.startSessionWatch(); // polls; emits sessionExpired

// On teardown:
unlisten();
stopWatch();
```

`auth.signOut()` emits `signedOut` rather than `sessionExpired`, so a deliberate
departure is never reported as an expiry.

`listen` returns an unsubscribe function, and a listener that throws is caught
and logged rather than being allowed to stop the others — both matching
Amplify's `Hub.listen`. There is no public `dispatch`: events are emitted only
by this package, so an application cannot fake a `signedIn`.

#### Amplify parity

Event names are AWS Amplify's wherever the concept exists, so Amplify's
documentation applies here. Where an event is absent it is because the gateway,
not this client, performs OAuth and owns the session cookie — Amplify's client
holds tokens and runs the redirect itself, so it can observe steps that are not
visible from here. Absences were verified against the agentgateway source
(`http/oidc/callback.rs`, `http/oidc/mod.rs`); an event that could never fire is
not declared, because it would invite a handler that never runs.

| Amplify | This package | |
|---|---|---|
| `signedIn` | `signedIn` | payload is `data: UserIdentity` |
| `signedOut` | `signedOut` | |
| `signInWithRedirect` | — | on success the gateway redirects to the originally requested URI with no `code`/`state`, so the return is indistinguishable from any authenticated load |
| `signInWithRedirect_failure` | — | the gateway answers 400/500 **at the callback path**, so the application never loads and no listener can run |
| `tokenRefresh_failure` | `sessionExpired` | the gateway owns the cookie and any refresh; the expiry is the only observable consequence |
| `tokenRefresh` | — | not observable from this side |
| `customOAuthState` | — | the gateway performs the exchange and does not surface `state`; keep pre-redirect state in `sessionStorage` |
| — | `checkFailed` | determining the session is a network call, so "unknown" exists here and must not be read as "signed out" |

## API Reference

### `JwtValidator`

Validates JWTs using JWKS with support for RS256 and ES256 algorithms.

```typescript
new JwtValidator({
  jwksUrl: string,      // JWKS endpoint URL
  issuer?: string,       // Expected issuer (optional)
  audience?: string,     // Expected audience (optional)
  algorithms?: string[], // Allowed algorithms (default: ["RS256", "ES256"])
})

await validator.validate(token: string): Promise<TenantClaims>
```

#### A BFF: `bffAuth`

```typescript
import { bffAuth } from "kamarkat-auth";

const auth = bffAuth({ appId: "shop", publicPaths: ["/api/v1/auth"] });
app.get("/health", (c) => c.text("ok"));       // liveness: never depends on the issuer
app.get("/health/ready", auth.readiness);      // readiness: 503 until the keys are held
app.use("/api/*", auth.middleware);
```

It starts the validator, serves readiness, guards the mount, and on a local
stack (`SHOP_ENV=local`, refused under `NODE_ENV=production`) gives every request
a stand-in principal instead. The lifecycle below is what it does for you.

#### Keeping the issuer's keys (required for a running service)

A service calls `start()` once at boot and serves `status().ready` from its
readiness probe (ADR-022, stable-but-not-ready):

```typescript
const shutdown = new AbortController();
validator.start({ signal: shutdown.signal }); // never throws; retries with full jitter until the keys are held

app.get("/health/ready", (c) => (validator.status().ready ? c.text("ok") : c.text("jwks unavailable", 503)));
app.get("/health/live", (c) => c.text("ok"));                // never depends on the issuer
process.on("SIGTERM", () => shutdown.abort());               // or validator.stop()
```

`status().ready` is true while a key set holding at least one usable public
signing key was fetched less than `jwksCache.maxStaleMs` (15 min) ago. With
`start()` running the keys refresh every `jwksCache.keyTtlMs` (5 min, ±10%).
**Without `start()`**, keys are fetched on the first request and refetched only
once older than `maxStaleMs` or when a token names an unknown `kid`.

For ADR-022's metrics: increment `dependency_unavailable_total` from
`jwksCache.onFetchFailure`, and export `dependency_status` from `status().ready`.

A JWKS that cannot be fetched is a `JwksFetchError` (`JWKS_FETCH_ERROR`), which
`authMiddleware` answers **503** with a generic message; the detail goes to
`onDependencyError`. The JWKS URL must be `https:` (`http:` only for a loopback
host, or with `jwksCache.allowInsecureHttp`).

### `authMiddleware`

Hono middleware that extracts Bearer token, validates it, and sets principal on context.

```typescript
authMiddleware({
  validator: JwtValidator,
  requiredScopes?: string[],
}): MiddlewareHandler
// c.get("auth:principal") → AuthenticatedPrincipal
```

### `requireTenantBoundary`

Enforces `principal.tenantId == resource.tenantId`. Returns 403 on mismatch.

```typescript
requireTenantBoundary({
  getTenantId?: (c: Context) => string, // default: reads from :tenantId param or ?tenant_id
}): MiddlewareHandler
```

### `requireRole`

Checks that the principal has one of the required roles. Returns 403 if not.

```typescript
requireRole(...roles: string[]): MiddlewareHandler
```

### `OidcClient`

OIDC client for OAuth authorization code flow with PKCE.

```typescript
new OidcClient({
  issuerUrl: string,
  clientId: string,
  clientSecret?: string,
  redirectUri: string,
  scopes: string[],
  audience?: string,
})

await client.buildAuthorizationUrl(): Promise<{ url, state, codeVerifier? }>
await client.exchangeCode({ code, codeVerifier? }): Promise<TokenSet>
await client.refreshTokens(refreshToken): Promise<TokenSet>
await client.revokeToken(token): Promise<void>
OidcClient.generatePkce(): { codeVerifier, codeChallenge }
```

### `RedisTokenStore`

Redis-backed token store with cross-replica mutex for safe refresh rotation.

```typescript
new RedisTokenStore(redis: Redis, {
  tokenPrefix?: string,  // default: "auth:tokens:"
  flowPrefix?: string,   // default: "auth:flows:"
  lockPrefix?: string,   // default: "auth:lock:"
})

await store.get(subject): Promise<TokenSet | null>
await store.put(subject, tokens): Promise<void>
await store.delete(subject): Promise<void>
await store.refreshToken(subject, refreshFn): Promise<TokenSet>
await store.beginFlow(subject): Promise<{ state, stateHash }>
await store.resolveFlow(state): Promise<PendingOAuthFlow | undefined>
```

## License

MIT
