// Package pki holds the platform's certificate-profile definitions.
//
// This package exists because the same list was previously hardcoded twice — once
// in the CLI (PKI_READY, which ADR-042 makes CLI-owned) and once in the
// hub-operator's self-healing reconcile — and the two drifted apart until they
// shared exactly ONE entry:
//
//	CLI                       operator
//	argocd-bootstrap          infrastructure-services
//	infrastructure-services   argocd-principals
//	database-clients          argocd-agents
//	service-mesh
//	human-access
//	signing-keys
//
// The operator's comment claimed the lists matched. The consequences were real and
// pointed nowhere near PKI: the operator never created `signing-keys`, so a rebuilt
// Infisical could not issue the argocd-agent-jwt certificate, and
// argocd-agent-principal sat in ContainerCreating on a missing Secret while
// cert-manager reported only "Certificate template with name signing-keys not
// found". Meanwhile `argocd-principals` and `argocd-agents` are referenced by no
// issuer, no Certificate and no ADR — the operator was creating profiles nothing
// consumes.
//
// Both consumers now derive from RequiredProfiles. They still use their own clients
// to talk to Infisical; only the data is shared, which is the seam that was missing.
package pki

// Profile is one certificate profile in the Fleet Intermediate CA.
//
// TTLDays is a SERVER-SIDE CAP on the lifetime of any certificate issued through
// the profile. It is not the certificate's lifetime — cert-manager's
// Certificate.spec.duration decides that, subject to this cap. A cap below the
// requested duration does not fail loudly at apply time; it silently shortens or
// refuses issuance later, at renewal.
type Profile struct {
	Slug    string
	TTLDays int
}

// RequiredProfiles is the authoritative set. Every profile named by an
// infisical-issuer ClusterIssuer MUST appear here, or certificates using that
// issuer cannot be issued at all.
//
// Currently consumed by an issuer:
//   - infrastructure-services  openbao-fleet   (hub + spoke)
//   - signing-keys             openbao-signing (argocd-agent-jwt)
//
// The remainder are mandated by ADR-035's profile table and are provisioned ahead
// of the issuers that will use them.
//
// TTL POLICY (ADR-035 addendum §5 — resolves the 24h/90d conflict).
//
// The TTL here is a CEILING, not a mandate: each Certificate requests the duration
// it needs and the profile refuses anything longer. That is what lets one profile
// serve both classes of consumer without weakening either.
//
//	24h   default for workload / client identities (spoke agent, alloy)
//	7d    ceiling, for SERVING identities that cannot hot-reload their certificate
//
// Three constraints fix the ceiling at 7 days, and they pull in opposite directions:
//
//  1. There is no revocation. ADR-035 implements no CRL and no OCSP — "compromised
//     certificates expire naturally" — so the TTL IS the containment mechanism. The
//     previous 90-day cap meant a stolen principal key, which the whole fleet
//     trusts, stayed valid for up to 90 days with no way to withdraw it.
//  2. Neither the principal nor the agent hot-reloads its certificate (verified in
//     upstream source; see ADR-035 addendum §4), so every rotation costs a process
//     restart. A 24h ceiling would restart a control-plane component ~1.5x/day.
//  3. Infisical is a single point of failure and the TTL doubles as the outage
//     tolerance window: ADR-035's failure analysis notes that when Infisical is
//     down, renewal fails and existing certificates keep working until expiry. Too
//     short a ceiling converts a brief Infisical outage into a fleet mTLS outage.
//
// 7 days cuts the unrevocable compromise window by ~13x versus 90, still tolerates a
// multi-day Infisical outage (renewBefore 48h leaves ~5 days of slack), and restarts
// the affected components weekly rather than daily.
//
// Lowering this value below a Certificate's requested duration does NOT fail at
// apply time — it breaks renewal silently, one full lifetime later. Reduce the
// Certificates first, then the ceiling.
var RequiredProfiles = []Profile{
	{Slug: "argocd-bootstrap", TTLDays: 3},        // 72h bootstrap exception (ADR-035)
	{Slug: "infrastructure-services", TTLDays: 7}, // ceiling; see TTL POLICY above
	{Slug: "database-clients", TTLDays: 1},        // 4h cap, rounded up (ADR-035)
	{Slug: "service-mesh", TTLDays: 1},            // 1h cap, rounded up (ADR-035)
	{Slug: "human-access", TTLDays: 1},            // 15m cap, rounded up (ADR-035)
	{Slug: "signing-keys", TTLDays: 3650},         // long-lived asymmetric JWT signing key
}
