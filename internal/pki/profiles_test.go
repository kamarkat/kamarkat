package pki

import "testing"

// Every profile named by an infisical-issuer ClusterIssuer must exist, or
// certificates using that issuer cannot be issued at all. These two are live on the
// hub today; signing-keys being absent from the operator's old list is exactly what
// left argocd-agent-principal unable to start.
func TestIssuerReferencedProfilesArePresent(t *testing.T) {
	for _, slug := range []string{"infrastructure-services", "signing-keys"} {
		found := false
		for _, p := range RequiredProfiles {
			if p.Slug == slug {
				found = true
			}
		}
		if !found {
			t.Errorf("profile %q is named by a ClusterIssuer but missing from RequiredProfiles", slug)
		}
	}
}

// The TTL is a CEILING. Lowering it below a Certificate's requested duration does
// not fail at apply time — it breaks renewal silently, one full lifetime later, so
// the ordering (reduce Certificates first, then the ceiling) has to hold.
//
// ADR-035 addendum §5 fixes the infrastructure-services ceiling at 7 days: long
// enough that a control-plane component which cannot hot-reload is not restarted
// daily, and that an Infisical outage does not immediately become an mTLS outage;
// short enough that a stolen key is not trusted for a quarter, given the platform
// implements no CRL or OCSP.
//
// Every Certificate issued through openbao-fleet must therefore request
// 168h or less. The 24h spoke client certificates sit well under it.
func TestInfrastructureServicesCeilingMatchesPolicy(t *testing.T) {
	const wantDays = 7
	found := false
	for _, p := range RequiredProfiles {
		if p.Slug != "infrastructure-services" {
			continue
		}
		found = true
		if p.TTLDays != wantDays {
			t.Errorf("infrastructure-services ceiling is %d days, want %d (ADR-035 addendum §5). "+
				"Raising it weakens the only compromise containment the platform has; "+
				"lowering it breaks renewal of any certificate requesting more.",
				p.TTLDays, wantDays)
		}
	}
	if !found {
		t.Error("infrastructure-services missing from RequiredProfiles")
	}
}

// argocd-agent-jwt requests duration 43800h (5 years).
func TestSigningKeysCapCoversFiveYearJWTKey(t *testing.T) {
	for _, p := range RequiredProfiles {
		if p.Slug == "signing-keys" && p.TTLDays < 1825 {
			t.Errorf("signing-keys TTL cap is %d days; argocd-agent-jwt requests ~1825", p.TTLDays)
		}
	}
}

func TestNoDuplicateSlugs(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range RequiredProfiles {
		if seen[p.Slug] {
			t.Errorf("duplicate profile slug %q", p.Slug)
		}
		seen[p.Slug] = true
	}
}
