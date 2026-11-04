package zitadel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kamarkat/kamarkat/internal/kube-sbt/models"
)

// ownerRecorder is an issuer that already holds the owner's email address in
// SOME organisation, which is the ordinary case: an operator's address exists
// in the platform's own organisation long before any tenant is provisioned.
type ownerRecorder struct {
	// foundInOrg is the organisation the global user search reports the address
	// belongs to. Empty means the search finds nobody.
	foundInOrg string
	calls      []string
	createdIn  string
}

func (r *ownerRecorder) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.calls = append(r.calls, req.Method+" "+req.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case req.URL.Path == "/admin/v1/orgs/_search":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"result": []map[string]any{{"id": "org-acme", "name": "acme"}},
			})
		case req.URL.Path == "/management/v1/projects/_search":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"result": []map[string]any{{"id": "proj-1", "name": "platform", "state": "PROJECT_STATE_ACTIVE"}},
			})
		// The project GET, which ensureRoleAssertion reads so its update keeps
		// the project's own name instead of renaming it (ADR-094).
		case strings.HasPrefix(req.URL.Path, "/management/v1/projects/") &&
			req.Method == http.MethodGet && strings.Count(req.URL.Path, "/") == 4:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"project": map[string]any{"id": "proj-1", "name": "platform"},
			})
		case strings.HasSuffix(req.URL.Path, "/apps/_search"):
			_ = json.NewEncoder(w).Encode(map[string]any{"result": []any{}})
		case strings.HasSuffix(req.URL.Path, "/apps/oidc"):
			_ = json.NewEncoder(w).Encode(map[string]any{"appId": "app-1", "clientId": "client-1"})
		case req.URL.Path == "/v2/users":
			if r.foundInOrg == "" {
				_ = json.NewEncoder(w).Encode(map[string]any{"result": []any{}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"result": []map[string]any{{
				"userId":  "user-existing",
				"details": map[string]any{"resourceOwner": r.foundInOrg},
			}}})
		case req.URL.Path == "/v2/users/human":
			r.createdIn = req.Header.Get(orgHeader)
			_ = json.NewEncoder(w).Encode(map[string]any{"userId": "user-new"})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{})
		}
	})
}

func newOwnerAuth(t *testing.T, rec *ownerRecorder) (*Auth, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(rec.handler())
	a, err := NewAuth(Config{Issuer: srv.URL, ServiceToken: "test-token", ProjectName: "platform"})
	if err != nil {
		srv.Close()
		t.Fatalf("NewAuth: %v", err)
	}
	return a, srv
}

// An address that exists in ANOTHER organisation is not this tenant's owner.
//
// The user search carries no organisation, so it answers "does this email exist
// anywhere in the issuer" rather than "does it exist here". Adopting its answer
// leaves the tenant's own organisation empty — every login at the tenant's
// hostname is then refused with "User not found in the system" while the
// tenant reports itself fully provisioned — and grants a foreign account admin
// on this tenant's project.
//
// An operator's address is exactly the address most likely to be present
// already, because it is the one used to administer the platform.
func TestEnsureTenantIdentity_DoesNotAdoptAnOwnerFromAnotherOrganisation(t *testing.T) {
	rec := &ownerRecorder{foundInOrg: "org-platform"}
	a, srv := newOwnerAuth(t, rec)
	defer srv.Close()

	if _, err := a.EnsureTenantIdentity(context.Background(), "acme", "org-acme", models.ProjectBinding{},
		"operator@example.test", false, nil, nil, nil); err != nil {
		t.Fatalf("EnsureTenantIdentity: %v", err)
	}

	var created bool
	for _, c := range rec.calls {
		if strings.HasSuffix(c, "/v2/users/human") {
			created = true
		}
	}
	if !created {
		t.Error("no owner was created: the address exists in another organisation, so " +
			"this tenant has none — its login answers \"User not found in the system\" " +
			"while every other signal says the tenant is ready")
	}
	if rec.createdIn != "org-acme" {
		t.Errorf("owner created in org %q, want org-acme", rec.createdIn)
	}
}

// An owner already in THIS organisation is reused. Creating a second account
// for the same address in the same organisation is what the lookup exists to
// prevent.
func TestEnsureTenantIdentity_ReusesAnOwnerAlreadyInThisOrganisation(t *testing.T) {
	rec := &ownerRecorder{foundInOrg: "org-acme"}
	a, srv := newOwnerAuth(t, rec)
	defer srv.Close()

	if _, err := a.EnsureTenantIdentity(context.Background(), "acme", "org-acme", models.ProjectBinding{},
		"operator@example.test", false, nil, nil, nil); err != nil {
		t.Fatalf("EnsureTenantIdentity: %v", err)
	}
	for _, c := range rec.calls {
		if strings.HasSuffix(c, "/v2/users/human") {
			t.Error("a duplicate owner was created for an address already in this organisation")
		}
	}
}
