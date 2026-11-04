package teardown

import (
	"strings"
	"testing"
)

// The zone as it actually stood on 2026-09-15, which is the case that motivated
// this code: six records belonging to the live box, two belonging to its spoke,
// one orphan left by a box that no longer exists, and a pair created by hand.
func productionZone() []hetznerRRSet {
	txt := func(name, owner, resource string) hetznerRRSet {
		return hetznerRRSet{
			ID:   name + "/TXT",
			Name: name,
			Type: "TXT",
			Records: []hetznerRecord{{Value: "heritage=external-dns,external-dns/owner=" +
				owner + ",external-dns/resource=httproute/" + resource}},
		}
	}
	a := func(name, ip string) hetznerRRSet {
		return hetznerRRSet{ID: name + "/A", Name: name, Type: "A", Records: []hetznerRecord{{Value: ip}}}
	}

	var z []hetznerRRSet
	for _, h := range []string{"api", "argocd", "auth", "console", "id", "infisical"} {
		z = append(z, a(h+".dev", "65.109.41.89"))
		z = append(z, txt("extdns-"+h+".dev", "dev-hybrid", "platform-ops/"+h))
		z = append(z, txt("extdns-a-"+h+".dev", "dev-hybrid", "platform-ops/"+h))
	}

	// The orphan: a previous box's owner id, pointing at a load balancer that
	// was deleted with it.
	z = append(z, a("dashboard.dev", "65.109.41.23"))
	z = append(z, txt("extdns-dashboard.dev", "hub-hybrid-dev", "platform-ops/dashboard"))
	z = append(z, txt("extdns-a-dashboard.dev", "hub-hybrid-dev", "platform-ops/dashboard"))

	// Another box's records, sharing the zone legitimately. Named for its role
	// rather than for a tenant: what this asserts is that a record owned by
	// someone else is refused, and whose it is does not matter.
	z = append(z, a("app.dev", "65.109.41.89"))
	z = append(z, txt("extdns-app.dev", "spoke-pool-hybrid-dev-01", "platform-ops/app"))

	// Hand-made, no ownership TXT anywhere.
	z = append(z, a("infisical", "65.109.41.89"))
	z = append(z, hetznerRRSet{ID: "infisical/AAAA", Name: "infisical", Type: "AAAA",
		Records: []hetznerRecord{{Value: "2a01:4f9:c01d:374::1"}}})
	return z
}

func TestOwnedRecordNamesClaimsOnlyThisBox(t *testing.T) {
	own := dnsOwnership{ownerID: "dev-hybrid", txtPrefix: "extdns-"}
	owned := ownedRecordNames(productionZone(), own)

	for _, want := range []string{"api.dev", "argocd.dev", "auth.dev", "console.dev", "id.dev", "infisical.dev"} {
		if !owned[want] {
			t.Errorf("%s is this box's record but was not claimed", want)
		}
	}
	if len(owned) != 6 {
		t.Errorf("claimed %d hostnames, want exactly 6: %v", len(owned), owned)
	}
}

// The property that makes this safe to run: a teardown deletes ONLY what its own
// box published. Getting this wrong takes out another cluster's ingress, or a
// record a person created by hand, and neither is recoverable from the cluster.
func TestOwnedRecordNamesRefusesEverythingElse(t *testing.T) {
	own := dnsOwnership{ownerID: "dev-hybrid", txtPrefix: "extdns-"}
	owned := ownedRecordNames(productionZone(), own)

	for name, why := range map[string]string{
		"dashboard.dev": "owned by a previous box (hub-hybrid-dev)",
		"app.dev":       "owned by another box (spoke-pool-hybrid-dev-01)",
		"infisical":     "created by hand, carries no ownership TXT",
	} {
		if owned[name] {
			t.Errorf("claimed %s, which is %s — teardown must never delete it", name, why)
		}
	}
}

// A box that never published anything must delete nothing, rather than treating
// an empty owner id as matching every record.
func TestOwnedRecordNamesEmptyOwnerClaimsNothing(t *testing.T) {
	// readDNSOwnership refuses to return an empty owner id, but the substring
	// match would otherwise make "owner=" match every record in the zone, so the
	// guard is asserted here too.
	owned := ownedRecordNames(productionZone(), dnsOwnership{ownerID: "", txtPrefix: "extdns-"})
	for _, rr := range productionZone() {
		if rr.Type == "TXT" {
			continue
		}
		if owned[rr.Name] {
			t.Fatalf("an empty owner id claimed %s; it must claim nothing", rr.Name)
		}
	}
}

// An owner id that is a prefix of another box's must not match it.
// "dev" and "dev-hybrid" would collide under a naive substring test.
func TestOwnedRecordNamesDoesNotMatchOwnerPrefix(t *testing.T) {
	owned := ownedRecordNames(productionZone(), dnsOwnership{ownerID: "dev", txtPrefix: "extdns-"})
	if len(owned) != 0 {
		t.Errorf("owner id 'dev' claimed %v; it owns nothing, and dev-hybrid's records are not its", owned)
	}
}

// The zone is not the domain filter. external-dns is scoped to dev.nutgraf.in
// while the zone is nutgraf.in -- looking the filter up as a zone name finds
// nothing, and the teardown would report success having deleted none of its
// records.
func TestZoneMatchPrefersLongestSuffix(t *testing.T) {
	zones := []hetznerZone{
		{ID: 1, Name: "nutgraf.in"},
		{ID: 2, Name: "example.com"},
	}
	got := matchZone(zones, "dev.nutgraf.in")
	if got.ID != 1 {
		t.Fatalf("dev.nutgraf.in resolved to zone %+v, want nutgraf.in", got)
	}

	// A delegated sub-zone wins over its parent when both are present.
	zones = append(zones, hetznerZone{ID: 3, Name: "dev.nutgraf.in"})
	if got := matchZone(zones, "dev.nutgraf.in"); got.ID != 3 {
		t.Fatalf("resolved to zone %+v, want the delegated dev.nutgraf.in", got)
	}

	// A domain in no zone in the account matches nothing rather than falling
	// back to a zone that merely looks similar.
	if got := matchZone(zones, "nutgraf.industries"); got.ID != 0 {
		t.Fatalf("nutgraf.industries resolved to %+v, want no match", got)
	}
}

// The credential must never reach a log line or an error message.
func TestAPIErrorsDoNotCarryTheToken(t *testing.T) {
	own := dnsOwnership{token: "sekret-token-value", ownerID: "dev-hybrid", zoneHost: "dev.nutgraf.in"}
	err := own.do(t.Context(), "GET", "://malformed", nil)
	if err == nil {
		t.Fatal("expected an error from a malformed URL")
	}
	if strings.Contains(err.Error(), own.token) {
		t.Errorf("error message leaked the API token: %v", err)
	}
}
