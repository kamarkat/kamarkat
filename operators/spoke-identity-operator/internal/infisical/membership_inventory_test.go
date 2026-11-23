package infisical

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The inventory's whole value is that it does not round an unanswered question
// down to "nothing there". These tests hold that line, because the mistake it
// guards against -- reading absence off a failure -- is the one that produced a
// wrong architecture finding on 2026-11-22.

// clientFor returns a Client pointed at a test server, with a token already in
// hand so no login round-trip is needed.
func clientFor(t *testing.T, h http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	c.token = "test-token"
	c.tokenExp = time.Now().Add(time.Hour)
	return c, srv
}

func TestAMembershipIsReadBackWithItsRoles(t *testing.T) {
	c, _ := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("the inventory used %s; it must only ever read", r.Method)
		}
		w.Write([]byte(`{"identityMembership":{"roles":[
			{"role":"admin","isTemporary":false},
			{"role":"viewer","isTemporary":true}]}}`))
	})

	inv, err := c.InventoryProjectMemberships(context.Background(), "id-1", []string{"proj-a"})
	if err != nil {
		t.Fatalf("inventory: %v", err)
	}
	m := inv.Memberships[0]
	if !m.Retrieved || !m.Present {
		t.Fatalf("a 200 with roles was not recorded as a retrieved, present membership: %+v", m)
	}
	if len(m.Roles) != 2 {
		t.Fatalf("expected two roles, got %+v", m.Roles)
	}
	// A temporary role must stay distinguishable: it is present now and gone
	// later, so flattening it makes one identity read differently over time.
	if !strings.Contains(m.Summary(), "viewer (temporary)") || !strings.Contains(m.Summary(), "admin") {
		t.Errorf("the summary loses the temporary/permanent distinction: %q", m.Summary())
	}
}

func TestANotFoundIsAnAnswerAndAnErrorIsNot(t *testing.T) {
	for _, tc := range []struct {
		name             string
		status           int
		body             string
		retrieved        bool
		present          bool
		expectUnresolved bool
	}{
		// Infisical SAYING there is no membership is knowledge. The message carries
		// that meaning; getMembershipByIdentityId is the only thing that raises it.
		{"404 naming a missing membership is an answer", http.StatusNotFound,
			`{"message":"Identity membership not found"}`, true, false, false},
		// A bare 404 is not. A mistyped project id, an identity that does not exist
		// or a route that no longer resolves all land here, and reading any of them
		// as "no membership" would report broad access as none.
		{"404 without that message is unknown", http.StatusNotFound, "", false, false, true},
		{"404 for an unknown project is unknown", http.StatusNotFound,
			`{"message":"Project not found"}`, false, false, true},
		// A refusal to answer is not a statement that there is nothing to tell.
		{"403 means unknown", http.StatusForbidden, "", false, false, true},
		{"500 means unknown", http.StatusInternalServerError, "", false, false, true},
	} {
		c, _ := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			if tc.body != "" {
				w.Write([]byte(tc.body))
			}
		})
		inv, err := c.InventoryProjectMemberships(context.Background(), "id-1", []string{"proj-a"})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		m := inv.Memberships[0]
		if m.Retrieved != tc.retrieved || m.Present != tc.present {
			t.Errorf("%s: retrieved=%v present=%v, want retrieved=%v present=%v -- an "+
				"unanswered question must never read as an absent membership",
				tc.name, m.Retrieved, m.Present, tc.retrieved, tc.present)
		}
		if tc.expectUnresolved {
			if inv.Complete() {
				t.Errorf("%s: the inventory called itself complete while a project was unreadable", tc.name)
			}
			if len(inv.Unresolved()) == 0 || !strings.Contains(strings.Join(inv.Unresolved(), " "), "proj-a") {
				t.Errorf("%s: the unreadable project is not named as unresolved: %v", tc.name, inv.Unresolved())
			}
		}
	}
}

// A 200 whose body this operator cannot parse is also unknown. Treating it as an
// empty role list would report an identity as having no roles because our struct
// was out of date.
func TestAnUnreadableBodyIsUnknownNotEmpty(t *testing.T) {
	c, _ := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`this is not json`))
	})
	inv, err := c.InventoryProjectMemberships(context.Background(), "id-1", []string{"proj-a"})
	if err != nil {
		t.Fatalf("inventory: %v", err)
	}
	if inv.Memberships[0].Retrieved {
		t.Error("a body the operator could not read was recorded as a successful observation")
	}
}

// A custom role is reported by its slug. Built-in roles arrive in `role`; a custom
// one arrives in `customRole`, and reading only the former would report a
// custom-scoped identity as having no role at all.
func TestACustomRoleIsReportedByItsSlug(t *testing.T) {
	c, _ := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"identityMembership":{"roles":[
			{"role":"custom","customRole":{"Slug":"spoke-nutgraf-stg-01"},"isTemporary":false}]}}`))
	})
	inv, _ := c.InventoryProjectMemberships(context.Background(), "id-1", []string{"proj-a"})
	if got := inv.Memberships[0].Summary(); !strings.Contains(got, "spoke-nutgraf-stg-01") {
		t.Errorf("a custom role was not reported by its slug: %q", got)
	}
}

// Never report on an identity we were not given. An empty id would read whatever
// Infisical returns for a malformed path and attribute it to this identity.
func TestAnEmptyIdentityIsRefused(t *testing.T) {
	c, _ := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("the inventory issued a request for an empty identity id")
	})
	if _, err := c.InventoryProjectMemberships(context.Background(), "  ", []string{"proj-a"}); err == nil {
		t.Fatal("an empty identity id was accepted")
	}
}

// The surfaces this read does not cover are always named, so a caller cannot
// mistake "memberships look narrow" for "this identity is narrowly authorized".
func TestUninspectedSurfacesAreAlwaysNamed(t *testing.T) {
	c, _ := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"identityMembership":{"roles":[{"role":"viewer"}]}}`))
	})
	inv, _ := c.InventoryProjectMemberships(context.Background(), "id-1", []string{"proj-a"})
	if len(inv.NotInspected) == 0 {
		t.Fatal("no uninspected surface was named")
	}
	if inv.Complete() {
		t.Error("an inventory that inspected no additional privileges called itself complete")
	}
	joined := strings.Join(inv.NotInspected, " ")
	for _, want := range []string{"additional privilege", "group"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the %q surface is not named as uninspected: %v", want, inv.NotInspected)
		}
	}
}
