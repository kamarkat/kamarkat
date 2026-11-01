package zitadel

import (
	"context"
	"fmt"
	"net/http"
)

// EnsurePlatformProjectAccess admits the platform's own people to a platform
// project: it makes the named role exist on the project and grants it to every
// human member of the platform organisation that holds no grant there.
//
// WHY THIS IS NEEDED AT ALL (ADR-078 add.5 §7)
//
// Every project this provider creates carries projectRoleCheck and
// hasProjectCheck (projectSettings), so the issuer refuses a token for an
// application in the project to anyone holding no role in it:
//
//	Errors.User.GrantRequired
//
// That is the property the observability project exists for -- it is what makes
// an `aud` claim naming this project mean "someone granted observability
// access" rather than "any user of this issuer", which is what the spoke's
// VMAuth matches on. The same property locks out the box's own operators until
// something grants them the role, and nothing did: the viewer client was
// registered, its credentials published, Grafana configured, and every sign-in
// refused at the issuer. Nothing between Grafana and the issuer reports why.
//
// # WHY A SEPARATE CALL, not part of ensuring the project
//
// Granting every platform member a role is right for a project people SIGN IN
// to and wrong for one that exists only to be an audience. The credit
// authority's project (ADR-109) admits exactly one machine caller; handing its
// role to every administrator would widen a boundary that was drawn narrow
// deliberately. So the caller says which kind of project it is asking for.
//
// HUMANS ONLY. A machine user cannot sign in to Grafana, and the platform
// organisation is where every service account lives, so granting them the role
// would add nothing and make the project's grant list say something untrue
// about who can read the box's telemetry.
//
// Only users holding NO grant are touched, so an operator who raised someone to
// a different role by hand keeps it on the next reconcile.
func (a *Auth) EnsurePlatformProjectAccess(ctx context.Context, orgID, projectID, roleKey, roleDisplay string) error {
	if orgID == "" || projectID == "" {
		return fmt.Errorf("zitadel: platform project access needs the resolved organisation and project; ensure the project first")
	}
	if roleKey == "" {
		return fmt.Errorf("zitadel: platform project access needs a role key")
	}
	if roleDisplay == "" {
		roleDisplay = roleKey
	}

	if err := a.ensurePlatformRole(ctx, orgID, projectID, roleKey, roleDisplay); err != nil {
		return err
	}

	var users struct {
		Result []struct {
			UserID string `json:"userId"`
			// Presence is the whole test: the issuer sets `human` on a person
			// and `machine` on a service account.
			Human *struct{} `json:"human"`
		} `json:"result"`
	}
	q := map[string]any{
		"query":   map[string]any{"limit": 500},
		"queries": []any{map[string]any{"organizationIdQuery": map[string]any{"organizationId": orgID}}},
	}
	if err := a.api.do(ctx, http.MethodPost, "/v2/users", orgID, q, &users); err != nil {
		return fmt.Errorf("list members of the platform organisation %q: %w", orgID, err)
	}

	var grants struct {
		Result []struct {
			UserID    string `json:"userId"`
			ProjectID string `json:"projectId"`
		} `json:"result"`
	}
	if err := a.api.do(ctx, http.MethodPost, "/management/v1/users/grants/_search", orgID,
		map[string]any{"query": map[string]any{"limit": 500}}, &grants); err != nil && !isNotFound(err) {
		return fmt.Errorf("list grants in the platform organisation %q: %w", orgID, err)
	}
	granted := map[string]bool{}
	for _, g := range grants.Result {
		if g.ProjectID == projectID {
			granted[g.UserID] = true
		}
	}

	for _, u := range users.Result {
		if u.Human == nil || granted[u.UserID] {
			continue
		}
		// Best effort per user: one account the issuer refuses must not stop the
		// rest, or a single bad member keeps every operator locked out.
		_ = a.GrantRole(ctx, orgID, projectID, u.UserID, []string{roleKey})
	}
	return nil
}

// ensurePlatformRole makes one role exist on a platform project.
//
// Find-or-create on the role KEY, which is what a grant names. The display name
// is not compared: renaming a role in the console is an operator's choice to
// keep, and a reconcile that reverted it would be a reconcile fighting a person.
func (a *Auth) ensurePlatformRole(ctx context.Context, orgID, projectID, roleKey, roleDisplay string) error {
	var existing struct {
		Result []struct {
			Key string `json:"key"`
		} `json:"result"`
	}
	if err := a.api.do(ctx, http.MethodPost,
		"/management/v1/projects/"+projectID+"/roles/_search", orgID,
		map[string]any{"query": map[string]any{"limit": 100}}, &existing); err != nil {
		return fmt.Errorf("list roles of platform project %q: %w", projectID, err)
	}
	for _, r := range existing.Result {
		if r.Key == roleKey {
			return nil
		}
	}
	body := map[string]any{"roleKey": roleKey, "displayName": roleDisplay, "group": "platform"}
	if err := a.api.do(ctx, http.MethodPost,
		"/management/v1/projects/"+projectID+"/roles", orgID, body, nil); err != nil {
		return fmt.Errorf("create role %q on platform project %q: %w", roleKey, projectID, err)
	}
	return nil
}
