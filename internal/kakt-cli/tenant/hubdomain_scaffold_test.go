package tenant

import "testing"

// The domain is composed from a declared subdomain, not from the environment.
//
// It read the environment and special-cased "prod" to mean the apex, which made
// a box's DNS a function of its environment -- and so made the MANAGEMENT cluster
// have an environment, because it has hostnames. ADR-051's amendment of
// 2026-09-18 separates them: a box declares a subdomain, or none for the apex.
//
// "prod" is no longer special. A box that wants the apex declares no subdomain;
// one that declares "prod" gets prod.<domain>, which is what it asked for.
func TestHubDomainFromSubdomain(t *testing.T) {
	for _, c := range []struct{ subdomain, domain, want string }{
		{"dev", "acme.example", "dev.acme.example"},
		{"stg", "acme.example", "stg.acme.example"},
		{"", "acme.example", "acme.example"},
		{"dev", "acme.example.", "dev.acme.example"},
		{" dev ", "acme.example", "dev.acme.example"},
		{".dev.", "acme.example", "dev.acme.example"},
		// No longer special-cased: a declared subdomain is used as given.
		{"prod", "acme.example", "prod.acme.example"},
	} {
		if got := hubDomain(c.subdomain, c.domain); got != c.want {
			t.Errorf("hubDomain(%q,%q) = %q, want %q", c.subdomain, c.domain, got, c.want)
		}
	}
}
