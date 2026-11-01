package zitadel

import (
	"context"
	"fmt"
	"net/http"
)

// accessPageLimit is the issuer's per-request maximum for the searches below. Every
// one of them is paginated against the reported total rather than assumed to
// fit: the platform organisation is where every service account lives, so it
// grows, and a search that silently returns a first page turns "every intended
// grant" into "the first hundred rows, in whatever order the issuer chose".
const accessPageLimit = 100

// EnsurePlatformProjectAccess admits the platform's own ADMINISTRATORS to a
// platform project: it makes the named role exist, then grants it to the
// organisation's owners.
//
// WHY THIS IS NEEDED AT ALL (ADR-078 add.5 §7)
//
// Every project this provider creates carries projectRoleCheck and
// hasProjectCheck, so the issuer refuses a token for an application in the
// project to anyone holding no role in it:
//
//	Errors.User.GrantRequired
//
// The same property that makes the observability project meaningful locks the
// box's own operators out of it, and nothing granted them anything: the client
// was registered, its credentials published, Grafana configured, and every
// sign-in refused at the issuer, where neither Grafana nor the gateway can
// explain it.
//
// ORG OWNERS, NOT EVERY MEMBER. An earlier pass granted the role to every human
// in the organisation, which is a different statement than it looks: the
// platform organisation is where self-registered accounts would land and where
// every service account lives, so "member" is not a synonym for "administrator
// of this box". Ownership is the provider's own administrator model -- the same
// authority EnsureOrgOwner confers for a tenant -- so it is the set that already
// means "runs this box".
//
// Anyone else is granted the role explicitly, in the issuer, by one of those
// owners. There is no allowlist here to maintain and no deny list to forget.
//
// CALLED ONCE PER PROJECT, by a caller that records that it has. Nothing here
// knows whether a user never held the role or held it and had it taken away, so
// a function that grants whatever is missing cannot do anything but restore a
// revocation on the next reconcile. The caller's marker is what makes a
// revocation durable; this function's job is to be correct when it is called,
// and to report honestly whether it was.
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

	owners, err := a.orgOwners(ctx, orgID)
	if err != nil {
		return err
	}
	if len(owners) == 0 {
		return fmt.Errorf("zitadel: the platform organisation %s has no owners, so there is nobody to admit to project %s: a box whose administrators are not owners of its own organisation cannot be granted access by this path, and the grant has to be made in the issuer", orgID, projectID)
	}

	granted, err := a.projectGrantees(ctx, orgID, projectID)
	if err != nil {
		return err
	}

	// EVERY FAILURE IS COUNTED AND REPORTED. One account the issuer refuses must
	// not stop the rest -- otherwise a single bad member keeps every other
	// operator locked out -- but discarding the failures reproduces the exact
	// silent lockout this function exists to fix, one layer up, while the
	// reconcile logs success.
	var attempted, failed int
	var firstErr error
	for _, userID := range owners {
		if granted[userID] {
			continue
		}
		attempted++
		if err := a.GrantRole(ctx, orgID, projectID, userID, []string{roleKey}); err != nil {
			failed++
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	if failed > 0 {
		return fmt.Errorf("zitadel: %d of %d grants of %q on project %s failed (first: %w); the service credential may hold neither PROJECT_OWNER nor ORG_USER_MANAGER on the platform organisation, and every refused owner is locked out of the project at sign-in",
			failed, attempted, roleKey, projectID, firstErr)
	}
	return nil
}

// orgOwners returns the user ids of an organisation's owners, every page of them.
func (a *Auth) orgOwners(ctx context.Context, orgID string) ([]string, error) {
	var ids []string
	for offset := 0; ; offset += accessPageLimit {
		var page struct {
			Result []struct {
				UserID string `json:"userId"`
			} `json:"result"`
			Details struct {
				TotalResult string `json:"totalResult"`
			} `json:"details"`
		}
		body := map[string]any{
			"query": map[string]any{"offset": fmt.Sprint(offset), "limit": accessPageLimit},
			// Server-side, so the page boundary cannot hide an owner behind a
			// hundred service accounts.
			"queries": []any{map[string]any{"roleQuery": map[string]any{"role": "ORG_OWNER"}}},
		}
		if err := a.api.do(ctx, http.MethodPost,
			"/management/v1/orgs/me/members/_search", orgID, body, &page); err != nil {
			return nil, fmt.Errorf("list owners of the platform organisation %q: %w", orgID, err)
		}
		for _, m := range page.Result {
			if m.UserID != "" {
				ids = append(ids, m.UserID)
			}
		}
		if len(page.Result) < accessPageLimit {
			return ids, nil
		}
	}
}

// projectGrantees returns the set of users already holding a grant on a project.
//
// Filtered on the project by the ISSUER, not here. Pulling the organisation's
// whole grant list and filtering client-side made the page boundary decide both
// whether a user needed a grant and whether they already had one -- and on a
// miss, GrantRole finds the existing grant and REPLACES its roles, so a
// truncated page silently overwrote a role an operator had raised by hand.
func (a *Auth) projectGrantees(ctx context.Context, orgID, projectID string) (map[string]bool, error) {
	granted := map[string]bool{}
	for offset := 0; ; offset += accessPageLimit {
		var page struct {
			Result []struct {
				UserID string `json:"userId"`
			} `json:"result"`
		}
		body := map[string]any{
			"query":   map[string]any{"offset": fmt.Sprint(offset), "limit": accessPageLimit},
			"queries": []any{map[string]any{"projectIdQuery": map[string]any{"projectId": projectID}}},
		}
		if err := a.api.do(ctx, http.MethodPost,
			"/management/v1/users/grants/_search", orgID, body, &page); err != nil && !isNotFound(err) {
			return nil, fmt.Errorf("list grants on project %q: %w", projectID, err)
		}
		for _, g := range page.Result {
			granted[g.UserID] = true
		}
		if len(page.Result) < accessPageLimit {
			return granted, nil
		}
	}
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
		map[string]any{"query": map[string]any{"limit": accessPageLimit}}, &existing); err != nil {
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
