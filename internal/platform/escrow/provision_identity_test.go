package escrow

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeOrg is an Infisical with one organisation, enough of the identity API to
// exercise initialisation, and a record of every call.
//
// Counting the calls is the point of several tests below: "did this create a
// second identity" and "did this issue another client secret" are questions about
// what was CALLED, and a fake that only returned plausible bodies could not
// answer either.
type fakeOrg struct {
	mu sync.Mutex
	t  *testing.T

	// identities by id -> name, and the Universal Auth attached to each.
	identities map[string]string
	ua         map[string]string // identity id -> client id
	members    map[string][]string

	calls        map[string]int
	nextID       int
	denyFrom     string // path prefix that answers 403
	failFrom     string // path prefix that answers 500
	identityFull bool
}

func newFakeOrg(t *testing.T) *fakeOrg {
	return &fakeOrg{
		t: t, identities: map[string]string{}, ua: map[string]string{},
		members: map[string][]string{}, calls: map[string]int{},
	}
}

func (f *fakeOrg) count(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[key]
}

func (f *fakeOrg) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		key := r.Method + " " + strings.Split(r.URL.Path, "?")[0]
		f.calls[key]++

		if f.denyFrom != "" && strings.HasPrefix(r.URL.Path, f.denyFrom) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"permission denied"}`))
			return
		}
		if f.failFrom != "" && strings.HasPrefix(r.URL.Path, f.failFrom) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"boom"}`))
			return
		}

		switch {
		case r.Method == http.MethodPost && r.URL.Path == pathProjects:
			f.nextID++
			id := fmt.Sprintf("proj-%d", f.nextID)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"project": map[string]string{"id": id, "type": projectTypeSecretManager}})

		case r.Method == http.MethodGet && r.URL.Path == pathIdentities:
			// The query matters, and asserting only the path is what let a
			// wrong parameter name ship: the LIST route takes `orgId`, while
			// the CREATE route's body takes `organizationId`, and sending the
			// body's spelling here is a 422 the fake would otherwise answer
			// cheerfully.
			if q := r.URL.Query(); q.Get("orgId") == "" {
				f.t.Errorf("identity listing called with %q, want an orgId parameter", r.URL.RawQuery)
			}
			var list []map[string]interface{}
			for id, name := range f.identities {
				list = append(list, map[string]interface{}{
					"identity": map[string]string{"id": id, "name": name}})
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"identities": list})

		case r.Method == http.MethodPost && r.URL.Path == pathIdentities:
			if f.identityFull {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"message":"identity limit reached for your plan"}`))
				return
			}
			var body struct{ Name, Role string }
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Role != orgRoleNoAccess {
				f.t.Errorf("identity created with org role %q, want %q", body.Role, orgRoleNoAccess)
			}
			f.nextID++
			id := fmt.Sprintf("ident-%d", f.nextID)
			f.identities[id] = body.Name
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"identity": map[string]string{"id": id}})

		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, pathIdentities+"/"):
			delete(f.identities, strings.TrimPrefix(r.URL.Path, pathIdentities+"/"))
			w.WriteHeader(http.StatusOK)

		case strings.HasSuffix(r.URL.Path, "/client-secrets"):
			_ = json.NewEncoder(w).Encode(map[string]string{"clientSecret": "st-secret-value"})

		case strings.HasPrefix(r.URL.Path, pathUniversalAuth+"/"):
			id := strings.TrimPrefix(r.URL.Path, pathUniversalAuth+"/")
			if r.Method == http.MethodPost {
				f.ua[id] = "client-" + id
				w.WriteHeader(http.StatusOK)
				return
			}
			cid, has := f.ua[id]
			if !has {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"identityUniversalAuth": map[string]string{"clientId": cid}})

		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/memberships/identities/"):
			// The membership route, with the identity in the path. The
			// neighbouring POST /projects/<id>/identities CREATES one and is
			// answered below with the 422 Infisical returns, so a regression
			// back to it fails here rather than passing quietly.
			rest := strings.TrimPrefix(r.URL.Path, pathProjects+"/")
			proj := strings.Split(rest, "/")[0]
			id := strings.TrimPrefix(rest, proj+"/memberships/identities/")
			var body struct {
				Role string `json:"role"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Role != projectRoleMember {
				f.t.Errorf("identity admitted with project role %q, want %q",
					body.Role, projectRoleMember)
			}
			for _, already := range f.members[proj] {
				if already == id {
					w.WriteHeader(http.StatusConflict)
					_, _ = w.Write([]byte(`{"message":"identity already has membership"}`))
					return
				}
			}
			f.members[proj] = append(f.members[proj], id)
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/identities") &&
			strings.HasPrefix(r.URL.Path, pathProjects+"/"):
			// createProjectMachineIdentity, which is NOT the membership route.
			// Answered the way Infisical answers it, so using it by mistake is
			// a failing test instead of a 422 found against the real service.
			w.WriteHeader(422)
			_, _ = w.Write([]byte(`{"statusCode":422,"message":[{"path":["name"],"message":"Required"}]}`))

		default:
			f.t.Errorf("unexpected call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// A FIRST INITIALISATION does all of it, and chooses both roles itself.
//
// The roles are asserted in the fake rather than here: org `no-access` and
// project `member`, checked at the moment they are sent, because they are what
// the operator used to have to choose and what nothing verified.
func TestFirstInitialisationCreatesEverythingOnce(t *testing.T) {
	f := newFakeOrg(t)
	srv := f.server()
	defer srv.Close()

	p, err := ProvisionEscrow(context.Background(), srv.URL, "jwt", "org-1", "acme-escrow")
	if err != nil {
		t.Fatal(err)
	}
	if p.ProjectID == "" || p.IdentityID == "" {
		t.Fatalf("incomplete result: %+v", p)
	}
	if !p.IdentityCreated || !p.SecretIssued || p.Reused {
		t.Errorf("a first run should create the identity and issue one secret: %+v", p)
	}
	if p.ClientSecret == "" {
		t.Error("no client secret returned, so the box's automation has nothing to authenticate with")
	}
	if p.ClientID == "" {
		t.Error("no client id returned")
	}
	if n := f.count("POST " + pathIdentities); n != 1 {
		t.Errorf("created %d identities, want 1", n)
	}
}

// A REPEAT INITIALISATION in the same organisation creates no second identity
// and issues no second secret.
//
// This is the five-identity budget in practice: a second box must reuse the
// first box's identity, or the fifth box cannot be built.
func TestASecondBoxReusesTheOrganisationIdentity(t *testing.T) {
	f := newFakeOrg(t)
	srv := f.server()
	defer srv.Close()

	first, err := ProvisionEscrow(context.Background(), srv.URL, "jwt", "org-1", "acme-escrow")
	if err != nil {
		t.Fatal(err)
	}
	second, err := ProvisionEscrow(context.Background(), srv.URL, "jwt", "org-1", "acme-stg-escrow")
	if err != nil {
		t.Fatal(err)
	}

	if second.IdentityID != first.IdentityID {
		t.Errorf("the second box got identity %s, want the organisation's %s",
			second.IdentityID, first.IdentityID)
	}
	if !second.Reused || second.IdentityCreated {
		t.Errorf("the second box created an identity: %+v", second)
	}
	// The second box gets its OWN client secret. The existing one cannot be read
	// back, so the alternative was asking an operator to go and find the first
	// box's copy -- and per-box secrets are separately revocable anyway.
	if !second.SecretIssued || second.ClientSecret == "" {
		t.Error("the second box was left without a client secret, so its automation has " +
			"nothing to authenticate with")
	}
	if n := f.count("POST " + pathIdentities); n != 1 {
		t.Errorf("%d identities created across two boxes, want 1", n)
	}
	// One per box, on the one identity: issuing does not revoke or replace the
	// others, which is what lets the first box keep running.
	if n := f.count("POST " + pathUniversalAuth + "/" + first.IdentityID + "/client-secrets"); n != 2 {
		t.Errorf("%d client secrets issued across two boxes, want one each", n)
	}
	if second.ProjectID == first.ProjectID {
		t.Error("both boxes share one escrow project; each box gets its own")
	}
}

// Re-running the same initialisation completes what is missing and nothing else.
func TestInitialisationIsIdempotent(t *testing.T) {
	f := newFakeOrg(t)
	srv := f.server()
	defer srv.Close()

	// An identity that exists with Universal Auth already attached, as a half-done
	// first run would leave it.
	f.identities["ident-0"] = AutomationIdentityName
	f.ua["ident-0"] = "client-ident-0"

	p, err := ProvisionEscrow(context.Background(), srv.URL, "jwt", "org-1", "acme-escrow")
	if err != nil {
		t.Fatal(err)
	}
	if p.IdentityID != "ident-0" || !p.Reused {
		t.Errorf("did not find the existing identity: %+v", p)
	}
	if p.ClientSecret == "" {
		t.Error("a reused identity left this box with no client secret")
	}
	if n := f.count("POST " + pathUniversalAuth + "/ident-0"); n != 0 {
		t.Errorf("re-attached Universal Auth %d times to an identity that had it", n)
	}
	if p.ClientID != "client-ident-0" {
		t.Errorf("client id = %q, want the existing one", p.ClientID)
	}
}

// AN ORGANISATION MEMBER IS REFUSED, and told what is actually missing.
//
// Verified against the vendored Infisical: org member carries Identity.Create
// but not EditAuth or CreateToken, so a member can create an identity and can
// never make it usable. The refusal has to say "organisation admin", because
// "403" sends a person to the project's settings instead.
func TestAnOrganisationMemberIsRefusedWithTheReason(t *testing.T) {
	f := newFakeOrg(t)
	f.denyFrom = pathUniversalAuth
	srv := f.server()
	defer srv.Close()

	_, err := ProvisionEscrow(context.Background(), srv.URL, "jwt", "org-1", "acme-escrow")
	if err == nil {
		t.Fatal("an account that cannot configure authentication was allowed to initialise")
	}
	for _, want := range []string{"ORGANISATION ADMIN", "cannot give it an authentication method"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q:\n%v", want, err)
		}
	}
	// And the identity it created on the way is gone: an inert identity left
	// behind consumes one of five.
	if len(f.identities) != 0 {
		t.Errorf("an identity was left behind after the failure: %v", f.identities)
	}
}

// A full organisation is reported as a quota, not as a status code.
func TestAFullOrganisationIsReportedAsTheQuota(t *testing.T) {
	f := newFakeOrg(t)
	f.identityFull = true
	srv := f.server()
	defer srv.Close()

	_, err := ProvisionEscrow(context.Background(), srv.URL, "jwt", "org-1", "acme-escrow")
	if err == nil {
		t.Fatal("expected a refusal on a full organisation")
	}
	for _, want := range []string{"five identities", "no room for another identity"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not explain the limit (%q):\n%v", want, err)
		}
	}
}

// PARTIAL FAILURE undoes only what this run created.
func TestAPartialFailureRemovesOnlyTheIdentityItCreated(t *testing.T) {
	f := newFakeOrg(t)
	f.failFrom = pathProjects + "/proj-1/memberships"
	srv := f.server()
	defer srv.Close()

	_, err := ProvisionEscrow(context.Background(), srv.URL, "jwt", "org-1", "acme-escrow")
	if err == nil {
		t.Fatal("expected the membership failure to be reported")
	}
	if len(f.identities) != 0 {
		t.Errorf("the identity this run created was left behind: %v", f.identities)
	}
	if n := f.count("DELETE " + pathIdentities + "/ident-2"); n != 1 {
		t.Errorf("rollback deleted the identity %d times, want 1", n)
	}
}

// A PRE-EXISTING identity is never deleted by a rollback, however late the
// failure is.
func TestARollbackNeverDeletesAPreExistingIdentity(t *testing.T) {
	f := newFakeOrg(t)
	f.identities["ident-0"] = AutomationIdentityName
	f.ua["ident-0"] = "client-ident-0"
	f.failFrom = pathProjects + "/proj-1/memberships"
	srv := f.server()
	defer srv.Close()

	_, err := ProvisionEscrow(context.Background(), srv.URL, "jwt", "org-1", "acme-escrow")
	if err == nil {
		t.Fatal("expected the membership failure to be reported")
	}
	if _, still := f.identities["ident-0"]; !still {
		t.Fatal("the organisation's existing automation identity was deleted by a rollback; " +
			"other boxes authenticate with it")
	}
	if n := f.count("DELETE " + pathIdentities + "/ident-0"); n != 0 {
		t.Errorf("rollback attempted to delete a pre-existing identity %d times", n)
	}
}

// An unreadable identity listing does not become a new identity.
func TestAnUnreadableListingDoesNotCreateAnIdentity(t *testing.T) {
	f := newFakeOrg(t)
	f.failFrom = pathIdentities
	srv := f.server()
	defer srv.Close()

	if _, err := ProvisionEscrow(context.Background(), srv.URL, "jwt", "org-1", "acme-escrow"); err == nil {
		t.Fatal("an unreadable listing was treated as an empty organisation")
	}
	if n := f.count("POST " + pathIdentities); n != 0 {
		t.Errorf("created %d identities while the listing was unreadable", n)
	}
}

// A supplied identity is admitted, and nothing about it is created or rotated.
func TestASuppliedIdentityIsAdmittedAndNotTouched(t *testing.T) {
	f := newFakeOrg(t)
	f.identities["ident-0"] = "someone-elses-automation"
	f.ua["ident-0"] = "client-ident-0"
	srv := f.server()
	defer srv.Close()

	p, err := ProvisionEscrowWithIdentity(context.Background(), srv.URL, "jwt", "org-1",
		"acme-escrow", "client-ident-0")
	if err != nil {
		t.Fatal(err)
	}
	if p.IdentityID != "ident-0" {
		t.Errorf("admitted %q, want ident-0", p.IdentityID)
	}
	if p.IdentityCreated || p.SecretIssued {
		t.Errorf("a supplied identity was modified: %+v", p)
	}
	if n := f.count("POST " + pathIdentities); n != 0 {
		t.Error("created an identity although one was supplied")
	}
}

// An unknown client id is refused rather than guessed at.
func TestAnUnknownSuppliedClientIDIsRefused(t *testing.T) {
	f := newFakeOrg(t)
	f.identities["ident-0"] = "other"
	f.ua["ident-0"] = "client-ident-0"
	srv := f.server()
	defer srv.Close()

	_, err := ProvisionEscrowWithIdentity(context.Background(), srv.URL, "jwt", "org-1",
		"acme-escrow", "client-does-not-exist")
	if err == nil {
		t.Fatal("an unknown client id was accepted")
	}
	if !strings.Contains(err.Error(), "no identity in this organisation carries client id") {
		t.Errorf("unhelpful refusal:\n%v", err)
	}
}

// AN EXISTING BOX'S ESCROW IS PREPARED, NOT REPLACED.
//
// The failure this closes: a box already running declares the project holding its
// master keys, and `escrow init` created a second, empty one beside it. The real
// escrow was left untouched and unmentioned while the operator's settings pointed
// at the replacement.
func TestAnExistingProjectIsAdoptedRatherThanRecreated(t *testing.T) {
	f := newFakeOrg(t)
	srv := f.server()
	defer srv.Close()

	const declared = "e532d07f-already-holds-the-master-keys"
	p, err := ProvisionEscrowInProject(context.Background(), srv.URL, "jwt", "org-1",
		declared, "nutgraf-hub")
	if err != nil {
		t.Fatal(err)
	}
	if p.ProjectID != declared {
		t.Errorf("prepared project %q, want the declared %q", p.ProjectID, declared)
	}
	if n := f.count("POST " + pathProjects); n != 0 {
		t.Errorf("created %d projects while adopting one that already exists", n)
	}
	// And the box still gets what it came for.
	if p.IdentityID == "" || p.ClientSecret == "" {
		t.Errorf("adoption left the box without an identity or a secret: %+v", p)
	}
	if len(f.members[declared]) != 1 {
		t.Errorf("the automation identity was not given access to %s: %v",
			declared, f.members[declared])
	}
}

// Adoption with no project is refused rather than quietly creating one.
func TestAdoptionWithoutAProjectIsRefused(t *testing.T) {
	f := newFakeOrg(t)
	srv := f.server()
	defer srv.Close()

	if _, err := ProvisionEscrowInProject(context.Background(), srv.URL, "jwt", "org-1",
		"  ", "nutgraf-hub"); err == nil {
		t.Fatal("an empty project id was accepted, which would adopt nothing")
	}
	if n := f.count("POST " + pathProjects); n != 0 {
		t.Error("created a project on the adoption path")
	}
}
