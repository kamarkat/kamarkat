import { describe, expect, it } from "vitest";
import { claimsFromPayload } from "../../src/jwt/claims";

/**
 * Payload copied verbatim from a real Zitadel id_token, obtained on 2026-09-03 by
 * running the full authorization-code flow against id.dev.nutgraf.in for a user
 * in the `waypoint` organisation. Kept literal rather than reduced to the two
 * interesting keys so that a future provider upgrade that renames or drops a
 * claim fails here, against the shape actually issued.
 */
const IDENTITY_PROVIDER_ID_TOKEN = {
  amr: ["pwd"],
  aud: ["389096262521061709", "389096260524573005"],
  azp: "389096262521061709",
  client_id: "389096262521061709",
  email: "arun4infra@gmail.com",
  email_verified: true,
  iss: "https://id.dev.nutgraf.in",
  preferred_username: "arun4infra@gmail.com",
  sub: "389091433551757645",
  "urn:zitadel:iam:user:resourceowner:id": "389091373187334477",
  "urn:zitadel:iam:user:resourceowner:name": "waypoint",
  "urn:zitadel:iam:user:resourceowner:primary_domain": "waypoint.id.dev.nutgraf.in",
};

describe("Zitadel tenant claims", () => {
  it("reads the organisation as the tenant", () => {
    const c = claimsFromPayload(IDENTITY_PROVIDER_ID_TOKEN);
    expect(c.tenant_id).toBe("389091373187334477");
    expect(c.tenant_name).toBe("waypoint");
    expect(c.email).toBe("arun4infra@gmail.com");
  });

  // The organisation claims are minted only for the
  // urn:zitadel:iam:user:resourceowner scope. A token requested without it is
  // tenant-LESS and must be rejected, not defaulted — this is the exact shape
  // that produced "Token missing required tenant_id claim".
  it("leaves tenant_id empty when the resourceowner scope was not requested", () => {
    const { ["urn:zitadel:iam:user:resourceowner:id"]: _id, ["urn:zitadel:iam:user:resourceowner:name"]: _n, ...rest } =
      IDENTITY_PROVIDER_ID_TOKEN;
    expect(claimsFromPayload(rest).tenant_id).toBe("");
  });

  // Hydra must be unaffected: this change is additive, not a migration.
  it("prefers tenant_id when the issuer emits it", () => {
    const c = claimsFromPayload({ ...IDENTITY_PROVIDER_ID_TOKEN, tenant_id: "waypoint" });
    expect(c.tenant_id).toBe("waypoint");
  });

  it("still yields a tenant for a Hydra token with no Zitadel claims", () => {
    const c = claimsFromPayload({ sub: "u1", email: "a@b.c", tenant_id: "waypoint" });
    expect(c.tenant_id).toBe("waypoint");
    expect(c.tenant_name).toBeUndefined();
  });
});
