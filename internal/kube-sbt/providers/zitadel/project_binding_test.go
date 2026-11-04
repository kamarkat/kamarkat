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

// ADR-094: one project per application. These cover the three ways an
// application's project is chosen, because each wrong choice fails differently
// and none of them fails where it is made:
//
//	a new app placed in the legacy project   gets the sibling's client id, and its
//	                                         login fails with redirect_uri_mismatch
//	a legacy app moved to a new project      gets a NEW client id; everyone is
//	                                         signed out and the gateway's pinned
//	                                         id stops working
//	a bound project silently recreated       loses every role and grant

// projectRecorder is an issuer holding one organisation and a configurable set
// of projects, recording which project each app was created in.
type projectRecorder struct {
	calls        []string
	projects     map[string]string // name -> id
	liveIDs      map[string]bool   // ids GET /projects/<id> answers for
	createdNames []string
	// putBodies is what each PUT /projects/<id> carried, by id. The project
	// UPDATE replaces the whole object, name included, so what it sends decides
	// whether a project keeps its name (ADR-094).
	putBodies map[string]map[string]any
}

// nameOf reverses the name->id map, so a project GET can answer with the name
// it was created under rather than omitting it.
func (r *projectRecorder) nameOf(id string) string {
	for name, pid := range r.projects {
		if pid == id {
			return name
		}
	}
	// A live project always has a name. A test that registers an id without one
	// still gets a plausible answer, so the fake cannot make the production code
	// look broken for a reason the real issuer never produces.
	return id
}

func (r *projectRecorder) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.calls = append(r.calls, req.Method+" "+req.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		p := req.URL.Path
		switch {
		case p == "/management/v1/projects/_search":
			var body struct {
				Queries []struct {
					NameQuery struct {
						Name string `json:"name"`
					} `json:"nameQuery"`
				} `json:"queries"`
			}
			_ = json.NewDecoder(req.Body).Decode(&body)
			var out []map[string]any
			if len(body.Queries) > 0 {
				name := body.Queries[0].NameQuery.Name
				if id, ok := r.projects[name]; ok {
					out = append(out, map[string]any{"id": id, "name": name})
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"result": out})
		case p == "/management/v1/projects" && req.Method == http.MethodPost:
			var body struct {
				Name string `json:"name"`
			}
			_ = json.NewDecoder(req.Body).Decode(&body)
			r.createdNames = append(r.createdNames, body.Name)
			id := "proj-" + body.Name
			r.projects[body.Name] = id
			// A project is live the moment it is created. Without this the fake
			// 404s a GET for a project it just returned, which no real issuer does.
			if r.liveIDs == nil {
				r.liveIDs = map[string]bool{}
			}
			r.liveIDs[id] = true
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id})
		case strings.HasPrefix(p, "/management/v1/projects/") && req.Method == http.MethodGet &&
			strings.Count(p, "/") == 4:
			id := strings.TrimPrefix(p, "/management/v1/projects/")
			if !r.liveIDs[id] {
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]any{"code": 5, "message": "not found"})
				return
			}
			// The NAME is part of the answer, because ensureRoleAssertion reads
			// it to avoid renaming the project it is updating.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"project": map[string]any{"id": id, "name": r.nameOf(id)},
			})
		case strings.HasPrefix(p, "/management/v1/projects/") && req.Method == http.MethodPut &&
			strings.Count(p, "/") == 4:
			id := strings.TrimPrefix(p, "/management/v1/projects/")
			var body map[string]any
			_ = json.NewDecoder(req.Body).Decode(&body)
			if r.putBodies == nil {
				r.putBodies = map[string]map[string]any{}
			}
			r.putBodies[id] = body
			// A rename onto a name the organisation already holds is what the
			// real issuer refuses, so the fake refuses it too -- otherwise the
			// bug this guards against passes the test suite.
			if n, _ := body["name"].(string); n != "" && r.projects[n] != "" && r.projects[n] != id {
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"code": 6, "message": "Project already exists on organization (V3-DKcYh)"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{})
		case strings.HasSuffix(p, "/apps/oidc"):
			_ = json.NewEncoder(w).Encode(map[string]any{"appId": "app-1", "clientId": "client-1", "clientSecret": "s"})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{})
		}
	})
}

// appCreatedIn reports which project the OIDC app was created under.
func (r *projectRecorder) appCreatedIn() string {
	for _, c := range r.calls {
		if strings.HasPrefix(c, "POST /management/v1/projects/") && strings.HasSuffix(c, "/apps/oidc") {
			return strings.TrimSuffix(strings.TrimPrefix(c, "POST /management/v1/projects/"), "/apps/oidc")
		}
	}
	return ""
}

func newProjectAuth(t *testing.T, rec *projectRecorder) (*Auth, func()) {
	t.Helper()
	srv := httptest.NewServer(rec.handler())
	a, err := NewAuth(Config{Issuer: srv.URL, ServiceToken: "t", ProjectName: "platform"})
	if err != nil {
		srv.Close()
		t.Fatalf("NewAuth: %v", err)
	}
	return a, srv.Close
}

func ensure(t *testing.T, a *Auth, b models.ProjectBinding) (*models.TenantIdentity, error) {
	t.Helper()
	// knownOrgID set: the org is bound, so no org search or creation happens and
	// the test isolates the project decision.
	return a.EnsureTenantIdentity(context.Background(), "nutgraf", "org-1", b, "", false,
		[]string{"https://oranger.example/oauth/callback"}, nil, nil)
}

func TestProjectBinding_NewApplicationGetsItsOwnProject(t *testing.T) {
	rec := &projectRecorder{
		projects: map[string]string{"platform": "proj-platform"},
		liveIDs:  map[string]bool{"proj-platform": true},
	}
	a, done := newProjectAuth(t, rec)
	defer done()

	id, err := ensure(t, a, models.ProjectBinding{Name: "oranger"})
	if err != nil {
		t.Fatalf("EnsureTenantIdentity: %v", err)
	}
	if id.ProjectRef != "proj-oranger" {
		t.Errorf("ProjectRef = %q, want proj-oranger", id.ProjectRef)
	}
	if got := rec.appCreatedIn(); got != "proj-oranger" {
		t.Errorf("browser client created in %q, want the app's own project", got)
	}
	if len(rec.createdNames) != 1 || rec.createdNames[0] != "oranger" {
		t.Errorf("created projects %v, want exactly [oranger]", rec.createdNames)
	}
}

func TestProjectBinding_EmptyBindingStaysOnTheLegacyProject(t *testing.T) {
	rec := &projectRecorder{
		projects: map[string]string{"platform": "proj-platform"},
		liveIDs:  map[string]bool{"proj-platform": true},
	}
	a, done := newProjectAuth(t, rec)
	defer done()

	id, err := ensure(t, a, models.ProjectBinding{})
	if err != nil {
		t.Fatalf("EnsureTenantIdentity: %v", err)
	}
	if id.ProjectRef != "proj-platform" {
		t.Errorf("ProjectRef = %q, want the legacy proj-platform", id.ProjectRef)
	}
	if len(rec.createdNames) != 0 {
		t.Errorf("created %v; an app already provisioned must not be moved", rec.createdNames)
	}
}

func TestProjectBinding_KnownIDWinsOverName(t *testing.T) {
	rec := &projectRecorder{
		projects: map[string]string{"platform": "proj-platform", "oranger": "proj-renamed"},
		liveIDs:  map[string]bool{"proj-bound": true},
	}
	a, done := newProjectAuth(t, rec)
	defer done()

	id, err := ensure(t, a, models.ProjectBinding{Name: "oranger", KnownID: "proj-bound"})
	if err != nil {
		t.Fatalf("EnsureTenantIdentity: %v", err)
	}
	if id.ProjectRef != "proj-bound" {
		t.Errorf("ProjectRef = %q, want the bound proj-bound", id.ProjectRef)
	}
	for _, c := range rec.calls {
		if c == "POST /management/v1/projects/_search" {
			t.Errorf("searched by name with a binding present: %v", rec.calls)
		}
	}
}

func TestProjectBinding_ABoundProjectThatVanishedIsNotRecreated(t *testing.T) {
	rec := &projectRecorder{
		projects: map[string]string{},
		liveIDs:  map[string]bool{},
	}
	a, done := newProjectAuth(t, rec)
	defer done()

	if _, err := ensure(t, a, models.ProjectBinding{Name: "oranger", KnownID: "proj-gone"}); err == nil {
		t.Fatal("expected an error for a bound project that no longer exists")
	}
	if len(rec.createdNames) != 0 {
		t.Errorf("recreated %v; a new project would drop every role and grant", rec.createdNames)
	}
}

func TestEnsureConfidentialClient_LandsInTheResolvedProject(t *testing.T) {
	rec := &projectRecorder{
		projects: map[string]string{"platform": "proj-platform"},
		liveIDs:  map[string]bool{},
	}
	a, done := newProjectAuth(t, rec)
	defer done()

	rec.calls = nil
	if _, _, err := a.EnsureConfidentialClient(context.Background(), "org-1", "proj-oranger",
		"nutgraf-oranger-gateway-exchange", nil, false, true); err != nil {
		t.Fatalf("EnsureConfidentialClient: %v", err)
	}
	if got := rec.appCreatedIn(); got != "proj-oranger" {
		t.Errorf("confidential client created in %q, want proj-oranger beside the browser client", got)
	}
	for _, c := range rec.calls {
		if strings.Contains(c, "/orgs/_search") || c == "POST /management/v1/projects/_search" {
			t.Errorf("looked something up by name instead of using the resolved refs: %s", c)
		}
	}
}

// ADR-094: turning on role assertion must not rename the project.
//
// Zitadel's project UPDATE replaces the whole object, name included, so
// ensureRoleAssertion sending the CONFIGURED name renamed whatever project it
// was given to the shared default. Invisible while every application lived in
// that one project -- the PUT set the name to what it already was. The moment an
// application got a project of its own it became a rename into a name the
// organisation already holds:
//
//	409 Project already exists on organization (V3-DKcYh)
//
// which reached the operator as a 502, left status.identity empty, left
// OIDC_CLIENT_ID unpublished, left the ExternalSecret in SecretSyncedError, and
// ended as a gateway pod in CreateContainerConfigError behind "no healthy
// upstream". Six symptoms, one field.

func TestRoleAssertionKeepsTheApplicationsOwnProjectName(t *testing.T) {
	rec := &projectRecorder{
		// The tenant already has the shared project, which is what the rename
		// would collide with. Without it the bug cannot reproduce.
		projects: map[string]string{"platform": "proj-platform"},
		liveIDs:  map[string]bool{"proj-platform": true},
	}
	a, done := newProjectAuth(t, rec)
	defer done()

	if _, err := ensure(t, a, models.ProjectBinding{Name: "oranger"}); err != nil {
		t.Fatalf("EnsureTenantIdentity: %v", err)
	}

	body, ok := rec.putBodies["proj-oranger"]
	if !ok {
		t.Fatal("role assertion was never applied to oranger's project")
	}
	if body["name"] != "oranger" {
		t.Errorf("PUT name = %v, want \"oranger\"; sending the configured name renames the project and the issuer answers 409", body["name"])
	}
	// The settings this call exists for must still be applied.
	for _, k := range []string{"projectRoleAssertion", "projectRoleCheck", "hasProjectCheck"} {
		if body[k] != true {
			t.Errorf("%s = %v, want true", k, body[k])
		}
	}
}

func TestRoleAssertionOnTheSharedProjectIsUnchanged(t *testing.T) {
	// The path that always worked keeps working: an application whose project IS
	// the shared one PUTs that same name, which is not a rename.
	rec := &projectRecorder{
		projects: map[string]string{"platform": "proj-platform"},
		liveIDs:  map[string]bool{"proj-platform": true},
	}
	a, done := newProjectAuth(t, rec)
	defer done()

	if _, err := ensure(t, a, models.ProjectBinding{Name: "platform"}); err != nil {
		t.Fatalf("EnsureTenantIdentity: %v", err)
	}
	if body := rec.putBodies["proj-platform"]; body["name"] != "platform" {
		t.Errorf("PUT name = %v, want \"platform\"", body["name"])
	}
}
