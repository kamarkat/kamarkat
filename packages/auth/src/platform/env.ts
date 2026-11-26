/**
 * The environment contract between the platform and an application.
 *
 * THE ONE PLACE these names are spelled. Every key here is written by the
 * platform -- the hub-operator into the tenant's OpenBao path, delivered by
 * an ExternalSecret, or a ConfigMap the tenant chart mounts -- and an application
 * that spells one itself has taken a copy of a platform decision.
 *
 * Why that matters beyond tidiness: when the platform renamed the caller
 * allowlist from a list to a map, every application that had written
 * `process.env.OIDC_ALLOWED_AZP` and split it on spaces was silently wrong, and
 * nothing in its own repository said so. A name read through this module moves
 * when the platform moves it, and a name that disappears is a type error rather
 * than an empty string.
 *
 * Nothing here is a default. A missing value is reported by the reader that needs
 * it, naming the key and what the application loses without it -- never filled in
 * with a fallback, because every fallback this package has carried pointed at
 * production.
 */

/** Keys the platform publishes. Read them through the helpers below. */
export const PLATFORM_ENV = {
  /** The issuer's base URL. */
  issuerUrl: "OIDC_ISSUER_URL",
  /**
   * The issuer's JWKS endpoint, published because it cannot be derived.
   *
   * Assembling it from the issuer defeats an application on this platform:
   * `/.well-known/jwks.json` is Hydra's convention, and Zitadel serves
   * `/oauth/v2/keys` and answers 404 there. The failure arrives after a login
   * has already succeeded, as "Expected 200 OK from the JSON Web Key Set HTTP
   * response", naming neither the path nor the provider.
   */
  jwksUrl: "OIDC_JWKS_URL",
  /** This application's own OAuth client id, which is also its own audience. */
  clientId: "OIDC_CLIENT_ID",
  /** The organisation that owns this application's identities. */
  orgId: "OIDC_ORG_ID",
  /** This application's identity project, which is what its audience names. */
  projectId: "OIDC_PROJECT_ID",
  /**
   * The callers this application admits, as `<clientId>=<appId>` entries
   * (ADR-094 invariant 2). Read with `parseAllowedCallers`.
   */
  allowedCallers: "OIDC_ALLOWED_AZP",
  /**
   * The audience scopes this application must request at login so its gateway
   * can exchange for a token another application will accept (ADR-095).
   */
  backendAudienceScopes: "OIDC_BACKEND_AUDIENCE_SCOPES",
  /** The confidential client this application's gateway exchanges as. */
  exchangeClientId: "OIDC_EXCHANGE_CLIENT_ID",
  exchangeClientSecret: "OIDC_EXCHANGE_CLIENT_SECRET",
  /**
   * The machine identity this application calls ANOTHER APPLICATION as, with no
   * user in the request (ADR-097).
   *
   * Not a variant of the exchange client above, and the distinction is the whole
   * point: that one authenticates a token exchange carrying a person, this one
   * authenticates this application acting as itself. A receiver admits both,
   * because it matches `azp || client_id` and they fill different claims -- but
   * nothing may read a person out of a token minted from this pair.
   *
   * Present only for an application that declares `backendDependencies`.
   */
  serviceClientId: "OIDC_SERVICE_CLIENT_ID",
  serviceClientSecret: "OIDC_SERVICE_CLIENT_SECRET",
} as const;

/**
 * The scopes a login must request to produce a token this platform can use.
 *
 * Raw issuer vocabulary, kept here so an application never writes it. Both of
 * the reserved scopes are load-bearing and neither is guessable:
 *
 *   - without `urn:zitadel:iam:user:resourceowner` the token carries NO tenant
 *     claim, and every receiver refuses it as tenant-less;
 *   - without `urn:zitadel:iam:org:project:roles` it carries no roles at all.
 *
 * Zitadel does not reject an unrecognised scope, it simply does not grant it. So
 * an application that invents its own scope names gets a token that verifies
 * correctly and arrives empty, and the failure shows up as an empty permission
 * set on the first API call rather than as anything wrong with the login.
 */
export const PLATFORM_LOGIN_SCOPES: readonly string[] = [
  "openid",
  "profile",
  "email",
  "offline_access",
  "urn:zitadel:iam:user:resourceowner",
  "urn:zitadel:iam:org:project:roles",
];

/** Raised when the platform has not supplied a value the application needs. */
export class PlatformConfigError extends Error {
  readonly missing: readonly string[];
  constructor(missing: readonly string[], context: string) {
    super(
      `The platform has not supplied ${missing.join(", ")}, which ${context}.\n\n` +
        `These are delivered by the platform, not set by this application: check the ` +
        `ExternalSecret and ConfigMap this workload mounts before changing anything here.`,
    );
    this.name = "PlatformConfigError";
    this.missing = missing;
  }
}

/** A source of configuration. Defaults to the process environment. */
export type EnvSource = Record<string, string | undefined>;

/**
 * Read the named keys, or throw naming every one that is missing at once.
 *
 * All of them, not the first: an application started with three values missing
 * should learn that in one restart rather than three.
 */
export function requirePlatformEnv<K extends readonly (keyof typeof PLATFORM_ENV)[]>(
  keys: K,
  context: string,
  env: EnvSource = process.env,
): Record<K[number], string> {
  const out = {} as Record<K[number], string>;
  const missing: string[] = [];
  for (const key of keys) {
    const name = PLATFORM_ENV[key];
    const value = env[name]?.trim();
    if (!value) {
      missing.push(name);
      continue;
    }
    out[key as K[number]] = value;
  }
  if (missing.length > 0) throw new PlatformConfigError(missing, context);
  return out;
}

/** Read one optional key. Absent and empty are the same answer. */
export function platformEnv(
  key: keyof typeof PLATFORM_ENV,
  env: EnvSource = process.env,
): string | undefined {
  return env[PLATFORM_ENV[key]]?.trim() || undefined;
}
