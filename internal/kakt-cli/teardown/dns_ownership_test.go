package teardown

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fakeDNSAPI(t *testing.T, rrsets []hetznerRRSet) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/zones":
			json.NewEncoder(w).Encode(map[string]any{"zones": []hetznerZone{{ID: 7, Name: "nutgraf.in"}}})
		case r.URL.Path == "/zones/7/rrsets":
			json.NewEncoder(w).Encode(map[string]any{"rrsets": rrsets})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	old := hetznerAPIBase
	hetznerAPIBase = srv.URL
	t.Cleanup(func() { hetznerAPIBase = old })
}

func ownerTXT(name, owner string) hetznerRRSet {
	return hetznerRRSet{Name: name, Type: "TXT", Records: []hetznerRecord{{
		Value: `"heritage=external-dns,external-dns/owner=` + owner + `,external-dns/resource=httproute/x/y"`}}}
}

// A torn-down box's records hold this box's hostnames; another box's records on
// a deeper subdomain, and this box's own, are not reported.
func TestForeignDNSOwnersNamesOnlyAnotherOwnersHoldOnOurHosts(t *testing.T) {
	fakeDNSAPI(t, []hetznerRRSet{
		ownerTXT("extdns-a-id", "nutgraf-1794929668"),
		ownerTXT("extdns-id", "nutgraf-1794929668"),
		ownerTXT("extdns-a-console", "nutgraf-1795584132"),
		ownerTXT("extdns-a-id.dev", "dev-box"),
		{Name: "id", Type: "A", Records: []hetznerRecord{{Value: "37.27.181.99"}}},
	})
	got, err := ForeignDNSOwners(context.Background(), "tok", "nutgraf.in", "nutgraf-1795584132",
		[]string{"id.nutgraf.in", "console.nutgraf.in", "grafana.nutgraf.in"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got["id.nutgraf.in"] != "nutgraf-1794929668" {
		t.Fatalf("foreign owners = %v, want only id.nutgraf.in held by nutgraf-1794929668", got)
	}
}

func writeClusterValues(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "registry", "clusters", "c1", "values.yaml")
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestDeclaredDNSOwnershipReadsTheBoxDeclaration(t *testing.T) {
	dir := writeClusterValues(t, "domain: nutgraf.in\nsubdomain: \"dev\"\ndns:\n  provider: hetzner\n  ownerId: \"nutgraf-1\"\n")
	owner, zone, err := DeclaredDNSOwnership(dir, "c1")
	if err != nil || owner != "nutgraf-1" || zone != "dev.nutgraf.in" {
		t.Fatalf("got %q %q %v, want nutgraf-1 dev.nutgraf.in", owner, zone, err)
	}
}

// With the cluster unreachable, ownership comes from the declaration, so a box
// that is already gone still has its records released.
func TestDNSOwnershipFallsBackToTheDeclaration(t *testing.T) {
	t.Setenv("HETZNER_DNS_API_TOKEN", "")
	t.Setenv("HCLOUD_DNS_TOKEN", "")
	t.Setenv("HCLOUD_TOKEN", "")
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "absent"))
	t.Setenv("HOME", t.TempDir())
	dir := writeClusterValues(t, "domain: nutgraf.in\ndns:\n  ownerId: \"nutgraf-1\"\n")
	o := &Orchestrator{GitopsDir: dir, ClusterName: "c1", ProviderToken: "tok"}
	own, ok := o.resolveDNSOwnership(context.Background())
	if !ok || own.ownerID != "nutgraf-1" || own.zoneHost != "nutgraf.in" || own.token != "tok" {
		t.Fatalf("got %+v %v, want owner nutgraf-1 in nutgraf.in with the provider token", own, ok)
	}
}

// Records that could not be released fail the teardown and name the command.
func TestUnreleasedDNSRecordsAreOutstanding(t *testing.T) {
	o := &Orchestrator{ClusterName: "c1"}
	o.reportDNSResidue(dnsOwnership{ownerID: "nutgraf-1", zoneHost: "nutgraf.in"}, "the zone could not be resolved")
	err := o.result()
	if err == nil || !strings.Contains(err.Error(),
		"kakt teardown --dns-only --dns-owner nutgraf-1 --dns-zone nutgraf.in --confirm") {
		t.Fatalf("result = %v, want an incomplete teardown naming the release command", err)
	}
}
