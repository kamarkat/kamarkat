package infisical

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// recorder answers membership requests from a per-project script and records the
// method/project of every call, so a test asserts what the reconciler DID rather
// than only what it returned.
type recorder struct {
	roles map[string][]MembershipRole // project -> current roles; absent key = 404
	calls []string
	deny  map[string]int // project -> status to answer a DELETE with
}

func (rec *recorder) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		// /api/v1/projects/{project}/memberships/identities/{identity}
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) < 4 {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		project := parts[3]
		rec.calls = append(rec.calls, r.Method+" "+project)

		switch r.Method {
		case http.MethodGet:
			roles, ok := rec.roles[project]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"message":"Identity membership not found"}`))
				return
			}
			out := struct {
				M struct {
					Roles []map[string]any `json:"roles"`
				} `json:"identityMembership"`
			}{}
			for _, role := range roles {
				out.M.Roles = append(out.M.Roles, map[string]any{"role": role.Role, "isTemporary": role.IsTemporary})
			}
			b, _ := json.Marshal(out)
			w.Write(b)
		case http.MethodPost, http.MethodPatch:
			body, _ := io.ReadAll(r.Body)
			var in struct {
				Roles []struct {
					Role        string `json:"role"`
					IsTemporary bool   `json:"isTemporary"`
				} `json:"roles"`
			}
			if err := json.Unmarshal(body, &in); err != nil || len(in.Roles) == 0 {
				t.Errorf("%s sent a body without a roles array: %s", r.Method, body)
			}
			var next []MembershipRole
			for _, role := range in.Roles {
				next = append(next, MembershipRole{Role: role.Role, IsTemporary: role.IsTemporary})
			}
			rec.roles[project] = next
			w.Write([]byte(`{}`))
		case http.MethodDelete:
			if status, ok := rec.deny[project]; ok {
				w.WriteHeader(status)
				w.Write([]byte(`{"message":"Project must retain an administrator"}`))
				return
			}
			delete(rec.roles, project)
			w.Write([]byte(`{}`))
		}
	}
}

func (rec *recorder) did(method, project string) bool {
	for _, c := range rec.calls {
		if c == method+" "+project {
			return true
		}
	}
	return false
}

// The case this whole change exists for: an identity holding admin on two
// projects ends up a viewer of its own project and a member of nothing else.
func TestABroadlyPrivilegedIdentityIsNarrowedToItsOwnProject(t *testing.T) {
	rec := &recorder{roles: map[string][]MembershipRole{
		"hub-secrets":  {{Role: "admin"}},
		"hub-platform": {{Role: "admin"}},
	}}
	c, _ := clientFor(t, rec.handler(t))

	changes, err := c.ReconcileSoleMembership(context.Background(), "id-1", "spoke-stg-secrets",
		"viewer", []string{"hub-secrets", "hub-platform"})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if got := rec.roles["spoke-stg-secrets"]; len(got) != 1 || got[0].Role != "viewer" {
		t.Errorf("the identity is not a sole viewer of its own project: %+v", got)
	}
	for _, p := range []string{"hub-secrets", "hub-platform"} {
		if _, still := rec.roles[p]; still {
			t.Errorf("the membership of %s survived; admin there is what the correction removes", p)
		}
		if !rec.did("DELETE", p) {
			t.Errorf("no DELETE was issued for %s", p)
		}
	}
	// The keep project is established before anything is removed, so there is no
	// window in which the identity can read nothing.
	if first := rec.calls[0]; !strings.HasSuffix(first, "spoke-stg-secrets") {
		t.Errorf("the first call was %q; the identity's own project must be established "+
			"before a membership is removed", first)
	}
	if len(changes) != 3 {
		t.Errorf("expected a change per project, got %d: %v", len(changes), changes)
	}
}

// A membership that already holds a broader role is REPLACED, not added to -- the
// roles field is a set, and adding viewer beside admin narrows nothing.
func TestAnExistingBroadRoleIsReplacedNotAddedTo(t *testing.T) {
	rec := &recorder{roles: map[string][]MembershipRole{
		"spoke-stg-secrets": {{Role: "admin"}, {Role: "member", IsTemporary: true}},
	}}
	c, _ := clientFor(t, rec.handler(t))

	changes, err := c.ReconcileSoleMembership(context.Background(), "id-1", "spoke-stg-secrets", "viewer", nil)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !rec.did("PATCH", "spoke-stg-secrets") {
		t.Error("no PATCH was issued; POST would have left the existing roles in place")
	}
	got := rec.roles["spoke-stg-secrets"]
	if len(got) != 1 || got[0].Role != "viewer" || got[0].IsTemporary {
		t.Errorf("roles after reconcile are %+v, want exactly one permanent viewer", got)
	}
	if !strings.Contains(changes[0].String(), "admin") {
		t.Errorf("the change does not name what was replaced: %q", changes[0])
	}
}

// Already correct is a no-op. A reconciler that rewrites an identical membership
// on every loop makes an audit log of its own noise.
func TestACorrectMembershipIsLeftAlone(t *testing.T) {
	rec := &recorder{roles: map[string][]MembershipRole{
		"spoke-stg-secrets": {{Role: "viewer"}},
	}}
	c, _ := clientFor(t, rec.handler(t))

	changes, err := c.ReconcileSoleMembership(context.Background(), "id-1", "spoke-stg-secrets", "viewer", nil)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rec.did("PATCH", "spoke-stg-secrets") || rec.did("POST", "spoke-stg-secrets") {
		t.Error("an already-correct membership was rewritten")
	}
	if len(changes) != 1 || !strings.Contains(changes[0].Action, "already correct") {
		t.Errorf("unexpected changes: %v", changes)
	}
}

// A temporary role beside the right one is NOT correct: it expires, and an
// identity whose authorization changes with the clock is not reconciled.
func TestATemporaryRoleBesideTheRightOneIsReplaced(t *testing.T) {
	rec := &recorder{roles: map[string][]MembershipRole{
		"spoke-stg-secrets": {{Role: "viewer"}, {Role: "admin", IsTemporary: true}},
	}}
	c, _ := clientFor(t, rec.handler(t))
	if _, err := c.ReconcileSoleMembership(context.Background(), "id-1", "spoke-stg-secrets", "viewer", nil); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !rec.did("PATCH", "spoke-stg-secrets") {
		t.Error("a temporary admin beside the viewer role was accepted as already correct")
	}
}

// An unreadable membership stops the reconciliation rather than guessing. Creating
// one on the strength of a 500 would be acting on an assumption about authorization.
func TestAnUnreadableMembershipStopsRatherThanGuesses(t *testing.T) {
	c, _ := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("the reconciler wrote after an unreadable read: %s", r.Method)
		}
		w.WriteHeader(http.StatusInternalServerError)
	})
	if _, err := c.ReconcileSoleMembership(context.Background(), "id-1", "proj", "viewer", nil); err == nil {
		t.Fatal("an unreadable membership did not stop the reconciliation")
	}
}

// A refused removal is reported and does not undo the narrowing that succeeded.
// Infisical refuses to leave a project with no administrator, and that is a fact
// about the project, not a reason to leave this identity unscoped.
func TestARefusedRemovalIsReportedAndDoesNotFailTheNarrowing(t *testing.T) {
	rec := &recorder{
		roles: map[string][]MembershipRole{"hub-secrets": {{Role: "admin"}}},
		deny:  map[string]int{"hub-secrets": http.StatusBadRequest},
	}
	c, _ := clientFor(t, rec.handler(t))

	changes, err := c.ReconcileSoleMembership(context.Background(), "id-1", "spoke-stg-secrets",
		"viewer", []string{"hub-secrets"})
	if err != nil {
		t.Fatalf("a refused removal failed the whole reconciliation: %v", err)
	}
	if got := rec.roles["spoke-stg-secrets"]; len(got) != 1 || got[0].Role != "viewer" {
		t.Errorf("the narrowing did not survive the refused removal: %+v", got)
	}
	var reported bool
	for _, ch := range changes {
		if ch.ProjectID == "hub-secrets" && strings.Contains(ch.Action, "could not be removed") {
			reported = true
			if !strings.Contains(strings.Join(ch.Roles, ","), "admin") {
				t.Errorf("the refusal does not say what is still held: %v", ch.Roles)
			}
		}
	}
	if !reported {
		t.Errorf("the refused removal was not reported: %v", changes)
	}
}

func TestAnEmptyProjectOrRoleIsRefused(t *testing.T) {
	c, _ := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("a request was issued for an incomplete reconciliation: %s %s", r.Method, r.URL.Path)
	})
	for _, tc := range []struct{ identity, project, role string }{
		{"", "proj", "viewer"},
		{"id-1", "", "viewer"},
		{"id-1", "proj", "  "},
	} {
		if _, err := c.ReconcileSoleMembership(context.Background(), tc.identity, tc.project, tc.role, nil); err == nil {
			t.Errorf("accepted identity=%q project=%q role=%q", tc.identity, tc.project, tc.role)
		}
	}
}

var _ = fmt.Sprintf
