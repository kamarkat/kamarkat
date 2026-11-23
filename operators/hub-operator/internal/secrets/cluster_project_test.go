package secrets

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// Finding a cluster's project, and the two ways that went wrong on 0.3.45-rc.1.
//
// The listing route answers `{projects, totalCount}`. Reading any other key makes
// every lookup come back empty -- and empty is not inert here: it tells the caller
// to CREATE a project that already exists, and it makes a published project look
// absent to the identity operator. The parse succeeds either way, so nothing
// errors; the zero value just reads as an answer.

func projectListServer(t *testing.T, body map[string]any) (*InfisicalClient, *int32) {
	t.Helper()
	var lists int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/auth/universal-auth/login":
			_ = json.NewEncoder(w).Encode(map[string]any{"accessToken": "t", "expiresIn": 3600})
		case r.URL.Path == "/api/v1/projects" && r.Method == http.MethodGet:
			atomic.AddInt32(&lists, 1)
			// The page size this platform asks for is the schema's maximum; a
			// listing that ignored it would hide a project behind a default page.
			if r.URL.Query().Get("limit") != "100" {
				t.Errorf("the listing did not ask for a full page: %q", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	c := NewInfisicalClient(srv.URL, "client", "secret", "proj-hub", "org")
	c.EnvironmentSlug = "dev"
	return c, &lists
}

func TestFindProjectBySlugReadsTheDeclaredResponseKey(t *testing.T) {
	c, _ := projectListServer(t, map[string]any{
		"projects": []map[string]string{
			{"id": "other", "slug": "spoke-a-secrets"},
			{"id": "wanted", "slug": "spoke-nutgraf-stg-01-secrets"},
		},
		"totalCount": 2,
	})

	got, err := c.findProjectBySlug(context.Background(), "spoke-nutgraf-stg-01-secrets")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got != "wanted" {
		t.Errorf("the project was not found; got %q. The route answers `projects`, and "+
			"reading another key makes every lookup empty", got)
	}
}

// The key the DIFFERENT /api/v1/workspace endpoint uses must not satisfy this one.
// A body shaped that way carries no `projects`, so the lookup must report absence
// rather than silently matching.
func TestFindProjectBySlugDoesNotReadTheWorkspaceKey(t *testing.T) {
	c, _ := projectListServer(t, map[string]any{
		"workspaces": []map[string]string{
			{"id": "wanted", "slug": "spoke-nutgraf-stg-01-secrets"},
		},
	})

	got, err := c.findProjectBySlug(context.Background(), "spoke-nutgraf-stg-01-secrets")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got != "" {
		t.Errorf("a /api/v1/workspace-shaped body satisfied the project lookup (%q); the two "+
			"endpoints are different and only one of them answers this question", got)
	}
}

// NOT FOUND AND NOT FULLY READ IS AN ERROR. "" tells the caller to create a
// project, so a partial page must never produce it: that is how one cluster ends
// up with two projects, the store reading one while the publisher writes the other.
func TestAPartialListingIsNotAnAbsentProject(t *testing.T) {
	c, _ := projectListServer(t, map[string]any{
		"projects":   []map[string]string{{"id": "a", "slug": "something-else"}},
		"totalCount": 7,
	})

	got, err := c.findProjectBySlug(context.Background(), "spoke-nutgraf-stg-01-secrets")
	if err == nil {
		t.Fatalf("a partial page reported the project absent (%q), which makes the caller "+
			"create a second one", got)
	}
	if !strings.Contains(err.Error(), "cannot be said to be absent") {
		t.Errorf("the error does not explain why absence is unestablished: %v", err)
	}
}

func TestAFullListingWithoutTheProjectIsAbsence(t *testing.T) {
	c, _ := projectListServer(t, map[string]any{
		"projects":   []map[string]string{{"id": "a", "slug": "something-else"}},
		"totalCount": 1,
	})

	got, err := c.findProjectBySlug(context.Background(), "spoke-nutgraf-stg-01-secrets")
	if err != nil || got != "" {
		t.Errorf("a complete listing without the project must be absence, got %q / %v", got, err)
	}
}

func TestClusterProjectSlugIsDeterministicAndLowercase(t *testing.T) {
	if got := ClusterProjectSlug("Nutgraf-STG-01"); got != "spoke-nutgraf-stg-01-secrets" {
		t.Errorf("slug is %q; it keys the store patch and the identity's scope, so it cannot "+
			"vary with the case a tenant typed", got)
	}
}
