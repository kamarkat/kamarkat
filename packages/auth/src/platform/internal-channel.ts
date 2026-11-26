import { timingSafeEqual } from "node:crypto";
import type { MiddlewareHandler } from "hono";
import { type AuthenticatedPrincipal, PRINCIPAL_KEY } from "../index.js";

/**
 * The trusted channel between an application's BFF and its own SDK (ADR-057).
 *
 * Both halves of one rule, in one module, for one reason: they are a matched
 * pair of header names, and they were being written out by hand on both sides of
 * a call that crosses a repository boundary. oranger's SDK says so in its own
 * comment -- "the two are separate services with no shared module, so a rename
 * changes both files in one commit" -- which is a convention, not a mechanism.
 *
 * In waypoint the caller half is spelled in EIGHT places, each as
 * `process.env.WAYPOINT_INTERNAL_TOKEN ? { ... } : {}`. That ternary is the
 * defect this module removes: with the token absent the call is made with no
 * header at all, so it fails at the far end as a bare 401 naming nothing. A
 * missing credential should stop the caller, not travel.
 *
 * ADR-057 makes this safe only when BOTH hold: the token proves which service is
 * calling, AND a network policy lets nothing else reach the receiver. This module
 * is the first half. The second is the CiliumNetworkPolicy in the receiver's
 * chart, and neither is sufficient alone.
 *
 * SCOPE. This is an intra-application channel. It is not a cross-application
 * mechanism and must not become one: the identity here is ASSERTED by the caller,
 * which is acceptable between two workloads of one application in one namespace
 * and is not acceptable across an application boundary, where identity must come
 * from a validated token (ADR-094 invariant 2b).
 */

/** Header names for one application, derived from its id. */
export interface InternalChannelHeaders {
  readonly token: string;
  readonly subject: string;
  readonly email: string;
  readonly tenant: string;
  readonly roles: string;
}

/**
 * Header names for an application, derived rather than written.
 *
 * `x-<appId>-internal-token`, `x-<appId>-user-subject`, and so on — the spelling
 * both applications already use, now produced from the one input that decides it.
 */
export function internalChannelHeaders(appId: string): InternalChannelHeaders {
  const a = appId.toLowerCase();
  return {
    token: `x-${a}-internal-token`,
    subject: `x-${a}-user-subject`,
    email: `x-${a}-user-email`,
    tenant: `x-${a}-user-tenant`,
    roles: `x-${a}-user-roles`,
  };
}

/** Raised when a caller tried to make an internal call with no credential. */
export class InternalTokenMissingError extends Error {
  constructor(appId: string) {
    super(
      `No internal token for ${appId}, so this call cannot be authenticated.\n\n` +
        `The platform seeds ${appId.toUpperCase().replace(/-/g, "_")}_INTERNAL_TOKEN into this ` +
        `application's OpenBao path and delivers it by ExternalSecret. A workload ` +
        `without it has not received its secret — check the ExternalSecret, rather than ` +
        `making the call without the header.`,
    );
    this.name = "InternalTokenMissingError";
  }
}

/**
 * THE CALLER HALF: the headers a BFF sends to its own SDK.
 *
 * Throws when the token is absent. That is the whole point — see the module note
 * above. An application that genuinely wants an unauthenticated internal call
 * does not want this function.
 *
 * The acting user is optional: ADR-057 permits an internal call with no acting
 * user, and such a call simply runs without one. What is not permitted is a call
 * with a user and no token, because then the identity headers are an assertion
 * anything on the network could make.
 */
export function internalCallHeaders(
  appId: string,
  token: string | undefined,
  actingUser?: Pick<AuthenticatedPrincipal, "subject" | "email" | "tenantId" | "roles">,
): Record<string, string> {
  if (!token) throw new InternalTokenMissingError(appId);

  const h = internalChannelHeaders(appId);
  const out: Record<string, string> = { [h.token]: token };
  if (!actingUser) return out;

  out[h.subject] = actingUser.subject;
  if (actingUser.email) out[h.email] = actingUser.email;
  if (actingUser.tenantId) out[h.tenant] = actingUser.tenantId;
  // Carried, not dropped. An empty list here is indistinguishable from "this user
  // holds no roles", so discarding them would silently deny every role-gated
  // operation the receiver ever grows.
  if (actingUser.roles?.length) out[h.roles] = actingUser.roles.join(",");
  return out;
}

/**
 * THE RECEIVER HALF: refuse any caller that cannot prove it is this
 * application's own BFF.
 *
 * Constant-time comparison, and length-checked first because `timingSafeEqual`
 * throws on a length mismatch rather than returning false.
 */
export function requireInternalCaller(appId: string, token: string): MiddlewareHandler {
  if (!token) throw new InternalTokenMissingError(appId);
  const header = internalChannelHeaders(appId).token;
  const expected = Buffer.from(token);

  return async (c, next) => {
    const got = Buffer.from(c.req.header(header) ?? "");
    if (got.length !== expected.length || !timingSafeEqual(got, expected)) {
      return c.json({ error: "Unauthorized" }, 401);
    }
    await next();
  };
}

/**
 * THE RECEIVER HALF: establish the principal the BFF validated, so the platform's
 * own `actingUserMiddleware` resolves it exactly as it would a token this service
 * had validated itself.
 *
 * Mount ONLY after `requireInternalCaller`. Without that, these headers are an
 * assertion any caller can make, and this function would turn it into an
 * authenticated principal.
 */
export function forwardedPrincipal(appId: string): MiddlewareHandler {
  const h = internalChannelHeaders(appId);

  return async (c, next) => {
    const subject = c.req.header(h.subject);
    if (subject) {
      const principal: AuthenticatedPrincipal = {
        subject,
        email: c.req.header(h.email) ?? "",
        tenantId: c.req.header(h.tenant) ?? "",
        roles: (c.req.header(h.roles) ?? "")
          .split(",")
          .map((r) => r.trim())
          .filter(Boolean),
      };
      c.set(PRINCIPAL_KEY, principal);
    }
    await next();
  };
}
