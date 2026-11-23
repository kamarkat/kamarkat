package controller

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	identityv1alpha1 "github.com/kamarkat/kamarkat/operators/spoke-identity-operator/api/v1alpha1"
	"github.com/kamarkat/kamarkat/operators/spoke-identity-operator/internal/infisical"
)

// The narrowing waits for the credentials to move, and that wait is the whole
// safety property: an identity scoped to a project that does not yet hold its
// secrets is a working spoke broken by a security correction.

type scopeServer struct {
	projects    map[string]string // slug -> id
	memberships map[string][]string
	calls       []string
}

func (s *scopeServer) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		// The login is answered here rather than injected, so the client
		// authenticates the way it does in production and no production code
		// grows a test hook.
		if r.URL.Path == "/api/v1/auth/universal-auth/login" {
			w.Write([]byte(`{"accessToken":"test-token","expiresIn":3600}`))
			return
		}
		s.calls = append(s.calls, r.Method+" "+r.URL.Path)
		switch {
		case r.URL.Path == "/api/v1/projects" && r.Method == http.MethodGet:
			out := map[string]any{"projects": []map[string]string{}}
			list := []map[string]string{}
			for slug, id := range s.projects {
				list = append(list, map[string]string{"id": id, "slug": slug})
			}
			out["projects"] = list
			out["totalCount"] = len(list)
			b, _ := json.Marshal(out)
			w.Write(b)
		case strings.Contains(r.URL.Path, "/memberships/identities/"):
			parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
			project := parts[3]
			switch r.Method {
			case http.MethodGet:
				roles, ok := s.memberships[project]
				if !ok {
					w.WriteHeader(http.StatusNotFound)
					w.Write([]byte(`{"message":"Identity membership not found"}`))
					return
				}
				payload := map[string]any{"identityMembership": map[string]any{"roles": []map[string]any{}}}
				rs := []map[string]any{}
				for _, role := range roles {
					rs = append(rs, map[string]any{"role": role, "isTemporary": false})
				}
				payload["identityMembership"].(map[string]any)["roles"] = rs
				b, _ := json.Marshal(payload)
				w.Write(b)
			case http.MethodPost, http.MethodPatch:
				// THE REQUESTED ROLE, not an assumed one. This stored "viewer"
				// whatever was asked for, so a change granting admin passed every
				// assertion here -- the one value this correction is about.
				var in struct {
					Roles []struct {
						Role string `json:"role"`
					} `json:"roles"`
				}
				body, _ := io.ReadAll(r.Body)
				if err := json.Unmarshal(body, &in); err != nil || len(in.Roles) == 0 {
					t.Errorf("%s sent no roles array: %s", r.Method, body)
				}
				var roles []string
				for _, role := range in.Roles {
					roles = append(roles, role.Role)
				}
				s.memberships[project] = roles
				w.Write([]byte(`{}`))
			case http.MethodDelete:
				delete(s.memberships, project)
				w.Write([]byte(`{}`))
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func scopeReconciler(t *testing.T, srv *scopeServer) (*SpokeMachineIdentityReconciler, func()) {
	t.Helper()
	// The client reads its credentials from the environment, as it does in the
	// operator; the test server answers the login.
	t.Setenv("SECRET_STORE_CLIENT_ID", "test-client")
	t.Setenv("SECRET_STORE_CLIENT_SECRET", "test-secret")
	ts := httptest.NewServer(srv.handler(t))
	c := fake.NewClientBuilder().WithScheme(newSchemeForTest(t)).Build()
	ic := infisical.NewClient(ts.URL)
	ic.HTTP = ts.Client()
	return &SpokeMachineIdentityReconciler{
		Client:           c,
		InfisicalClient:  ic,
		ProjectID:        "hub-platform",
		SecretsProjectID: "hub-secrets",
	}, ts.Close
}

func smiWithSlug(slug string) *identityv1alpha1.SpokeMachineIdentity {
	smi := newTestSMI("spoke-a", "client-id")
	smi.Spec.Infisical.SecretsProjectSlug = slug
	return smi
}

// No published project -> nothing is touched, and the condition says why.
func TestTheNarrowingWaitsForTheClusterProject(t *testing.T) {
	srv := &scopeServer{
		projects:    map[string]string{},
		memberships: map[string][]string{"hub-secrets": {"admin"}, "hub-platform": {"admin"}},
	}
	r, stop := scopeReconciler(t, srv)
	defer stop()

	smi := smiWithSlug("")
	if err := r.ensureWorkloadIdentityScope(context.Background(), smi, "identity-1"); err != nil {
		t.Fatalf("waiting must not be an error: %v", err)
	}
	if len(srv.calls) != 0 {
		t.Errorf("the identity was touched while its project was unpublished: %v", srv.calls)
	}
	for _, p := range []string{"hub-secrets", "hub-platform"} {
		if _, still := srv.memberships[p]; !still {
			t.Errorf("%s membership was removed before the credentials moved", p)
		}
	}
	cond := conditionByType(smi, conditionTypeWorkloadScoped)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "AwaitingSecretsProject" {
		t.Errorf("the wait is not reported as a condition: %+v", cond)
	}
}

// Published project -> viewer there, member of nothing else.
func TestThePublishedProjectNarrowsTheIdentity(t *testing.T) {
	srv := &scopeServer{
		projects:    map[string]string{"spoke-spoke-a-secrets": "proj-a"},
		memberships: map[string][]string{"hub-secrets": {"admin"}, "hub-platform": {"admin"}},
	}
	r, stop := scopeReconciler(t, srv)
	defer stop()

	smi := smiWithSlug("spoke-spoke-a-secrets")
	if err := r.ensureWorkloadIdentityScope(context.Background(), smi, "identity-1"); err != nil {
		t.Fatalf("scope: %v", err)
	}
	if got := srv.memberships["proj-a"]; len(got) != 1 || got[0] != "viewer" {
		t.Errorf("the identity is not a sole viewer of its own project: %v", got)
	}
	for _, p := range []string{"hub-secrets", "hub-platform"} {
		if _, still := srv.memberships[p]; still {
			t.Errorf("the %s membership survived the narrowing", p)
		}
	}
	cond := conditionByType(smi, conditionTypeWorkloadScoped)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("the narrowing is not reported: %+v", cond)
	}
}

// A slug published for a project that is not there is an ERROR, not a narrowing
// to nothing: the slug is a statement the project exists.
func TestAMissingPublishedProjectIsRefused(t *testing.T) {
	srv := &scopeServer{
		projects:    map[string]string{},
		memberships: map[string][]string{"hub-secrets": {"admin"}},
	}
	r, stop := scopeReconciler(t, srv)
	defer stop()

	err := r.ensureWorkloadIdentityScope(context.Background(), smiWithSlug("spoke-spoke-a-secrets"), "identity-1")
	if err == nil {
		t.Fatal("a published slug with no project was accepted")
	}
	// The REASON, not just the slug. Naming the slug is not enough: the wrapper
	// around the membership reconciler names it too, so this test passed on the
	// reconciler's refusal of an empty project and would have survived deleting
	// the check above it. Found by mutation, twice.
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("the refusal does not say the published project is missing, so it came from a "+
			"downstream guard rather than the check that establishes it: %v", err)
	}
	if _, still := srv.memberships["hub-secrets"]; !still {
		t.Error("the existing membership was removed despite the destination being absent")
	}
}

func conditionByType(smi *identityv1alpha1.SpokeMachineIdentity, t string) *metav1.Condition {
	for i := range smi.Status.Conditions {
		if smi.Status.Conditions[i].Type == t {
			return &smi.Status.Conditions[i]
		}
	}
	return nil
}
