import { describe, it, expect, beforeAll } from "vitest";
import * as jose from "jose";
import { JwtValidator } from "../../src/jwt/validator.js";
import { CallerNotAllowedError, IdTokenPresentedError, TenantMismatchError } from "../../src/types.js";

// ADR-095 RELEASE GATE: the Zitadel compatibility invariant, against a real issuer.
//
//   every ID token carries at_hash; no JWT access token does
//
// The unit tests beside this one assert the RULE against fixtures. They cannot
// fail when Zitadel changes, because they mint their own tokens -- so on their
// own they would keep passing while the platform silently started accepting ID
// tokens again. This one asks the issuer.
//
// SKIPPED unless pointed at an instance, so `vitest run` stays hermetic:
//
//   IDENTITY_PROVIDER_ISSUER=https://id.example        the issuer
//   IDENTITY_PROVIDER_EXCHANGE_CLIENT_ID=...           the gateway's confidential client
//   IDENTITY_PROVIDER_EXCHANGE_CLIENT_SECRET=...       its secret
//   IDENTITY_PROVIDER_SUBJECT_ID_TOKEN=...             an ID token from a real login
//   IDENTITY_PROVIDER_PROJECT_ID=...                   the audience to exchange for
//
// The ID token is obtained by logging in and reading the session cookie the
// gateway set, or from the token endpoint directly. It expires quickly; this is
// a gate to run at release, not a watch.
//
// The second block is the ADR-094 cross-project role EXPERIMENT. It is an open
// question, not a settled invariant, and it has its own env vars so it can be
// run without the first block's.

const env = (k: string) => process.env[k] ?? "";
const ISSUER = env("IDENTITY_PROVIDER_ISSUER");
const CLIENT_ID = env("IDENTITY_PROVIDER_EXCHANGE_CLIENT_ID");
const CLIENT_SECRET = env("IDENTITY_PROVIDER_EXCHANGE_CLIENT_SECRET");
const SUBJECT = env("IDENTITY_PROVIDER_SUBJECT_ID_TOKEN");
const PROJECT_ID = env("IDENTITY_PROVIDER_PROJECT_ID");

const configured = Boolean(ISSUER && CLIENT_ID && CLIENT_SECRET && SUBJECT && PROJECT_ID);

// The same request the gateway's backendAuth policy makes. Kept in step with
// manifests/tenants/charts/universal-tenant/templates/agentgateway.yaml: if the
// two drift, this gate stops testing what actually runs.
async function exchange(extraScopes: string[] = [], extraAudiences: string[] = []): Promise<string> {
  const body = new URLSearchParams({
    grant_type: "urn:ietf:params:oauth:grant-type:token-exchange",
    subject_token: SUBJECT,
    subject_token_type: "urn:ietf:params:oauth:token-type:id_token",
    // Without this Zitadel returns an OPAQUE token, regardless of the app's
    // accessTokenType=JWT. Asserting the result parses as a JWT is what catches
    // its removal.
    requested_token_type: "urn:ietf:params:oauth:token-type:jwt",
    audience: PROJECT_ID,
    // No `resource`: Zitadel rejects it outright.
  });
  for (const a of extraAudiences) body.append("audience", a);
  if (extraScopes.length > 0) body.set("scope", extraScopes.join(" "));

  const res = await fetch(new URL("/oauth/v2/token", ISSUER), {
    method: "POST",
    headers: {
      "content-type": "application/x-www-form-urlencoded",
      authorization: `Basic ${Buffer.from(`${encodeURIComponent(CLIENT_ID)}:${encodeURIComponent(CLIENT_SECRET)}`).toString("base64")}`,
    },
    body,
  });
  const json = (await res.json()) as Record<string, unknown>;
  if (!res.ok) {
    throw new Error(`token exchange failed (${res.status}): ${JSON.stringify(json)}`);
  }
  return json.access_token as string;
}

describe.skipIf(!configured)("ADR-095 gate: the Zitadel at_hash invariant, live", () => {
  let exchanged: Record<string, unknown>;
  let subjectClaims: Record<string, unknown>;

  beforeAll(async () => {
    exchanged = jose.decodeJwt(await exchange()) as Record<string, unknown>;
    subjectClaims = jose.decodeJwt(SUBJECT) as Record<string, unknown>;
  }, 30_000);

  it("the subject really is an ID token: it carries at_hash", () => {
    // If this fails the gate proves nothing -- it would be comparing two access
    // tokens and finding them alike.
    expect(subjectClaims).toHaveProperty("at_hash");
  });

  it("the exchanged token is a JWT and carries NO at_hash", () => {
    // The invariant itself.
    expect(exchanged).not.toHaveProperty("at_hash");
  });

  it("is audienced to the project it was exchanged for", () => {
    const aud = exchanged.aud;
    expect(Array.isArray(aud) ? aud : [aud]).toContain(PROJECT_ID);
  });

  it("carries azp and the project roles", () => {
    // ADR-094 requires azp. Roles must survive the exchange -- createExchangeJWT
    // asserts them, and an exchange that dropped them would authenticate every
    // user as having none, which reads as an authorisation bug, not an identity one.
    expect(exchanged.azp ?? exchanged.client_id).toBeTruthy();
    expect(exchanged).toHaveProperty("urn:zitadel:iam:org:project:roles");
  });

  it("the receiver accepts the exchanged token and refuses the subject", async () => {
    // End to end, through the real validator against the real JWKS: the whole
    // point of ADR-095 in two assertions.
    const validator = new JwtValidator({
      jwksUrl: new URL("/oauth/v2/keys", ISSUER).toString(),
      issuer: ISSUER,
      requireTenantId: false,
    });

    await expect(validator.validate(await exchange())).resolves.toBeDefined();
    await expect(validator.validate(SUBJECT)).rejects.toThrow(IdTokenPresentedError);
  }, 30_000);
});

// ── ADR-094 EXPERIMENT: can one exchanged token carry BOTH projects' roles? ──
//
// The cross-app gap: roles are resolved against the EXCHANGE CLIENT'S project
// (`getUserInfo(subjectToken.userID, client.client.ProjectID, ...)`), so a token
// oranger mints for waypoint carries ORANGER's roles. waypoint can then answer
// "is this token for me?" and "which app called?" but not "what may this user do
// here?".
//
// The candidate fix is Zitadel's plural roles scope. Read from source before
// writing this, because two details differ from how it is usually described and
// both change what must be sent:
//
//   1. `urn:zitadel:iam:org:projects:roles` is driven by the `:aud` SCOPES in
//      the request, not by the token's audience. prepareRoles builds its
//      roleAudience with AddAudScopeToAudience(ctx, roleAudience, scope), which
//      parses `urn:zitadel:iam:org:project:id:<id>:aud` out of the SCOPE list.
//      The RFC 8693 `audience` parameter is a different input entirely, so both
//      are sent below.
//
//   2. The plural scope is NOT subject to the cross-project role filter.
//      isScopeAllowed returns true for it before reaching
//      `slices.Contains(allowedScopes, scope)`, and allowedScopes is built from
//      the AUTHENTICATING client's ProjectRoleKeys. That check is what filters a
//      SPECIFIC role scope (`...:project:role:<key>`) by the caller's own
//      project -- the defect surface in the report against 4.17.2. So this
//      deliberately requests NO specific role scopes.
//
// Also: `ProjectRoleAssertion` cannot be varied per client. It is a column on
// the PROJECT (internal/query/project.go), shared by every app in it. What IS
// per-app is AccessTokenRoleAssertion, and it must stay true -- assertRoles
// early-returns on it, so false yields no roles at all rather than unfiltered
// ones.
//
//   IDENTITY_PROVIDER_PEER_PROJECT_ID=...   the OTHER project (e.g. waypoint's)
//   IDENTITY_PROVIDER_PEER_ROLE=...         a role the test user holds in THAT project
//
// A failure here is a FINDING, not a broken build: it decides between the
// audience-carried roles below and receiver-side exchange. Read the assertion
// messages rather than only the pass/fail.

const PEER_PROJECT = env("IDENTITY_PROVIDER_PEER_PROJECT_ID");
const PEER_ROLE = env("IDENTITY_PROVIDER_PEER_ROLE");
const crossConfigured = configured && Boolean(PEER_PROJECT && PEER_ROLE);

describe.skipIf(!crossConfigured)("ADR-094 experiment: cross-project roles in one exchanged token", () => {
  let claims: Record<string, unknown>;

  beforeAll(async () => {
    const token = await exchange(
      [
        "openid",
        // The plural scope. Singular `...:project:role:<key>` is deliberately
        // absent -- that is the filtered path.
        "urn:zitadel:iam:org:projects:roles",
        // What actually drives roleAudience.
        `urn:zitadel:iam:org:project:id:${PROJECT_ID}:aud`,
        `urn:zitadel:iam:org:project:id:${PEER_PROJECT}:aud`,
      ],
      [PEER_PROJECT],
    );
    claims = jose.decodeJwt(token) as Record<string, unknown>;
  }, 30_000);

  it("is audienced to BOTH projects", () => {
    const aud = claims.aud;
    const list = Array.isArray(aud) ? aud : [aud];
    expect(list).toContain(PROJECT_ID);
    expect(list).toContain(PEER_PROJECT);
  });

  it("carries the OWN project's roles, project-qualified", () => {
    // setUserInfoRoleClaims emits urn:zitadel:iam:org:project:<id>:roles for
    // every project whose grants were fetched, plus an unqualified claim for the
    // requesting project only.
    expect(claims).toHaveProperty(`urn:zitadel:iam:org:project:${PROJECT_ID}:roles`);
  });

  it("carries the PEER project's roles — this is the whole experiment", () => {
    const key = `urn:zitadel:iam:org:project:${PEER_PROJECT}:roles`;
    expect(
      claims,
      `No ${key} in the exchanged token. If this fails, the audience cannot carry ` +
        `cross-project authorization and the choice is receiver-side exchange, not ` +
        `a second authorization store. Claims present: ${Object.keys(claims).join(", ")}`,
    ).toHaveProperty(key);
    expect(Object.keys(claims[key] as object)).toContain(PEER_ROLE);
  });

  it("does not collapse the peer's roles into the requesting project's claim", () => {
    // The failure mode worth naming: roles arriving under the WRONG project key
    // would let waypoint authorise on oranger's roles while looking correct.
    const own = (claims[`urn:zitadel:iam:org:project:${PROJECT_ID}:roles`] ?? {}) as object;
    expect(Object.keys(own)).not.toContain(PEER_ROLE);
  });
});

// ── ADR-110 GATE: a delegated user token and a machine token, told apart ──────
//
// What a receiver relies on, asked of the deployed issuer rather than fixtures:
//
//   - a token the consumer's backend obtained through the delegated flow is
//     admitted as a `user` caller, its subject the person who authorized;
//   - the consumer's machine token is admitted as a `service` caller, and names
//     a different client;
//   - a token from a client the caller map does not hold, an ID token, and a
//     token for another tenant are refused;
//   - the access-token lifetime is what the instance is set to.
//
// SKIPPED unless pointed at an instance:
//
//   IDENTITY_PROVIDER_ISSUER=https://id.example
//   IDENTITY_PROVIDER_TARGET_PROJECT_ID=...         the receiver's project (its audience)
//   IDENTITY_PROVIDER_TENANT_ORG_ID=...             the receiver's organisation
//   IDENTITY_PROVIDER_DELEGATED_CLIENT_ID=...       the consumer's actsForUser client
//   IDENTITY_PROVIDER_DELEGATED_ACCESS_TOKEN=...    a token that client obtained for a
//                                         real person (read from the consumer's
//                                         token store after authorizing)
//   IDENTITY_PROVIDER_SERVICE_CLIENT_ID=...         the consumer's machine client
//   IDENTITY_PROVIDER_SERVICE_CLIENT_SECRET=...
//   IDENTITY_PROVIDER_ID_TOKEN=...                  an ID token from a real login
//   IDENTITY_PROVIDER_MAX_ACCESS_TOKEN_SECONDS=900  optional: the lifetime to assert
//
// A gate to run before enabling delegated-user authentication, not a watch: the
// delegated token expires within the instance's lifetime.

const T_PROJECT = env("IDENTITY_PROVIDER_TARGET_PROJECT_ID");
const T_ORG = env("IDENTITY_PROVIDER_TENANT_ORG_ID");
const DELEGATED_CLIENT = env("IDENTITY_PROVIDER_DELEGATED_CLIENT_ID");
const DELEGATED_TOKEN = env("IDENTITY_PROVIDER_DELEGATED_ACCESS_TOKEN");
const SERVICE_CLIENT = env("IDENTITY_PROVIDER_SERVICE_CLIENT_ID");
const SERVICE_SECRET = env("IDENTITY_PROVIDER_SERVICE_CLIENT_SECRET");
const ID_TOKEN = env("IDENTITY_PROVIDER_ID_TOKEN");
const MAX_LIFETIME = Number(env("IDENTITY_PROVIDER_MAX_ACCESS_TOKEN_SECONDS") || "0");

const delegationConfigured = Boolean(
  ISSUER && T_PROJECT && T_ORG && DELEGATED_CLIENT && DELEGATED_TOKEN && SERVICE_CLIENT && SERVICE_SECRET && ID_TOKEN,
);

// The machine token, minted exactly as serviceTokenSource mints it (ADR-097).
async function serviceToken(): Promise<string> {
  const res = await fetch(new URL("/oauth/v2/token", ISSUER), {
    method: "POST",
    headers: {
      "content-type": "application/x-www-form-urlencoded",
      authorization: `Basic ${Buffer.from(`${encodeURIComponent(SERVICE_CLIENT)}:${encodeURIComponent(SERVICE_SECRET)}`).toString("base64")}`,
    },
    body: new URLSearchParams({
      grant_type: "client_credentials",
      scope: `urn:zitadel:iam:org:project:id:${T_PROJECT}:aud urn:zitadel:iam:user:resourceowner`,
    }),
  });
  const json = (await res.json()) as Record<string, unknown>;
  if (!res.ok) throw new Error(`client credentials failed (${res.status}): ${JSON.stringify(json)}`);
  return json.access_token as string;
}

// The receiver as consumerApiValidator builds it, with the typed caller map.
function receiver(callers: Record<string, { appId: string; kind: "user" | "service" }>, tenant = T_ORG) {
  return new JwtValidator({
    jwksUrl: new URL("/oauth/v2/keys", ISSUER).toString(),
    issuer: ISSUER,
    audience: T_PROJECT,
    allowedCallers: callers,
    rejectIdTokens: true,
    requireTenantId: true,
    expectedTenantId: tenant,
  });
}

describe.skipIf(!delegationConfigured)("ADR-110 gate: delegated user tokens and machine tokens, live", () => {
  const map = {
    [DELEGATED_CLIENT]: { appId: "consumer", kind: "user" as const },
    [SERVICE_CLIENT]: { appId: "consumer", kind: "service" as const },
  };

  it("admits the delegated token as a user caller, its subject a person", async () => {
    const claims = await receiver(map).validate(DELEGATED_TOKEN);
    expect(claims.caller_kind).toBe("user");
    expect(claims.consumer_app_id).toBe("consumer");
    expect(claims.sub).toBeTruthy();
    // A person's token carries their address; a machine user has none.
    expect(claims.email).toMatch(/@/);
    expect(jose.decodeJwt(DELEGATED_TOKEN)).not.toHaveProperty("at_hash");
  }, 30_000);

  it("admits the machine token as a service caller, naming another client", async () => {
    const token = await serviceToken();
    const claims = await receiver(map).validate(token);
    expect(claims.caller_kind).toBe("service");
    expect(claims.azp).toBe(SERVICE_CLIENT);
    expect(claims.azp).not.toBe(DELEGATED_CLIENT);
  }, 30_000);

  it("refuses a client the caller map does not hold (cross-application)", async () => {
    const onlyService = { [SERVICE_CLIENT]: { appId: "consumer", kind: "service" as const } };
    await expect(receiver(onlyService).validate(DELEGATED_TOKEN)).rejects.toThrow(CallerNotAllowedError);
  }, 30_000);

  it("refuses an ID token and a token for another tenant", async () => {
    await expect(receiver(map).validate(ID_TOKEN)).rejects.toThrow(IdTokenPresentedError);
    await expect(receiver(map, `${T_ORG}-other`).validate(DELEGATED_TOKEN)).rejects.toThrow(TenantMismatchError);
  }, 30_000);

  it.skipIf(!MAX_LIFETIME)("issues access tokens within the instance's lifetime", async () => {
    for (const token of [DELEGATED_TOKEN, await serviceToken()]) {
      const { iat, exp } = jose.decodeJwt(token);
      expect(Number(exp) - Number(iat)).toBeLessThanOrEqual(MAX_LIFETIME);
    }
  }, 30_000);
});
