package infisical

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// Making an identity a member of exactly one project, with exactly one role.
//
// WHY REPLACE AND NOT ADD. A membership carries a SET of roles -- the API takes a
// `roles` array, and `{"role": x}` is shorthand the handler expands to one entry.
// Adding a narrow role beside a broad one narrows nothing. PATCH is what replaces
// the set: membership-identity-service's updateMembership deletes every
// membershipRole for the membership and inserts the given ones in one
// transaction. So a PATCH carrying [viewer] removes admin; a POST would not.
//
// WHY MEMBERSHIP AND NOT A ROLE DEFINITION. Narrowing by conditioning a role on an
// environment or a path requires a custom role, which an unlicensed Infisical
// refuses. The project is the boundary, so the lever is which projects the
// identity belongs to.
//
// WHAT THIS DOES NOT ESTABLISH. That the identity's effective authorization is
// what was asked for. Memberships are one source; additional privileges and group
// inheritance are others, and this writes none of them. The canary checks are what
// turn a configured membership into a verified boundary.

// MembershipChange is one thing this reconciliation did, for an operator's log.
// Never a credential: a project id and a role name.
type MembershipChange struct {
	ProjectID string
	Action    string // "created", "replaced", "removed", "already correct"
	Roles     []string
}

func (c MembershipChange) String() string {
	if len(c.Roles) == 0 {
		return fmt.Sprintf("%s: %s", c.ProjectID, c.Action)
	}
	return fmt.Sprintf("%s: %s (%s)", c.ProjectID, c.Action, strings.Join(c.Roles, ", "))
}

// ReconcileSoleMembership gives identityID exactly `role` on keepProjectID and
// removes its membership of every project in dropProjectIDs.
//
// ORDER IS DELIBERATE: the keep project is established FIRST. An identity whose
// only membership were removed before its new one existed would be unable to read
// anything in between, and ESO on the spoke would fail every sync in that window.
//
// A removal that Infisical refuses is reported and does not fail the whole
// reconciliation: a project that would be left with no administrator refuses the
// delete (assertProductWillRetainAdmin), and that is a fact about the project
// rather than a reason to leave the identity unscoped.
func (c *Client) ReconcileSoleMembership(ctx context.Context, identityID, keepProjectID, role string, dropProjectIDs []string) ([]MembershipChange, error) {
	if strings.TrimSpace(identityID) == "" || strings.TrimSpace(keepProjectID) == "" {
		return nil, fmt.Errorf("an identity and the project it belongs to are both required: " +
			"reconciling against an empty project would remove every membership and grant none")
	}
	if strings.TrimSpace(role) == "" {
		return nil, fmt.Errorf("no role named for identity %s in project %s: a membership with "+
			"no role reads nothing, and defaulting one here would decide an authorization "+
			"question in the wrong place", identityID, keepProjectID)
	}

	var changes []MembershipChange

	current := c.projectMembership(ctx, identityID, keepProjectID)
	switch {
	case !current.Retrieved:
		// Unknown is not absent. Creating a membership on the strength of an
		// unreadable answer would be acting on a guess about authorization.
		return changes, fmt.Errorf("cannot establish whether identity %s is a member of "+
			"project %s (%s), so its membership is left alone", identityID, keepProjectID, current.Why)
	case !current.Present:
		if err := c.createMembership(ctx, identityID, keepProjectID, role); err != nil {
			return changes, err
		}
		changes = append(changes, MembershipChange{keepProjectID, "created", []string{role}})
	case isExactly(current.Roles, role):
		changes = append(changes, MembershipChange{keepProjectID, "already correct", []string{role}})
	default:
		had := roleNames(current.Roles)
		if err := c.replaceMembershipRoles(ctx, identityID, keepProjectID, role); err != nil {
			return changes, err
		}
		changes = append(changes, MembershipChange{keepProjectID, "replaced " + strings.Join(had, "+"), []string{role}})
	}

	for _, pid := range dropProjectIDs {
		pid = strings.TrimSpace(pid)
		if pid == "" || pid == keepProjectID {
			continue
		}
		m := c.projectMembership(ctx, identityID, pid)
		switch {
		case !m.Retrieved:
			changes = append(changes, MembershipChange{pid, "left alone: " + m.Why, nil})
		case !m.Present:
			changes = append(changes, MembershipChange{pid, "already absent", nil})
		default:
			if err := c.deleteMembership(ctx, identityID, pid); err != nil {
				changes = append(changes, MembershipChange{pid, "could not be removed: " + err.Error(), roleNames(m.Roles)})
				continue
			}
			changes = append(changes, MembershipChange{pid, "removed", roleNames(m.Roles)})
		}
	}

	return changes, nil
}

// isExactly reports whether the membership holds precisely one permanent role,
// and that it is the wanted one. A temporary role beside it is not "exactly":
// it expires, and an identity whose authorization changes with the clock is not
// reconciled.
func isExactly(roles []MembershipRole, want string) bool {
	return len(roles) == 1 && !roles[0].IsTemporary && roles[0].Role == want
}

func roleNames(roles []MembershipRole) []string {
	out := make([]string, 0, len(roles))
	for _, r := range roles {
		if r.IsTemporary {
			out = append(out, r.Role+"(temporary)")
			continue
		}
		out = append(out, r.Role)
	}
	sort.Strings(out)
	return out
}

func (c *Client) membershipRequest(ctx context.Context, method, identityID, projectID string, body []byte) (*http.Response, error) {
	token, err := c.getToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("authenticate: %w", err)
	}
	url := c.BaseURL + fmt.Sprintf(PathProjectMembershipsIdentities, projectID, identityID)
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, fmt.Errorf("%s membership request: %w", method, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.HTTP.Do(req)
}

// rolesPayload is the array form, always. The single-role shorthand exists, but
// writing the array is what makes it obvious at the call site that this replaces
// a set rather than adding to one.
func rolesPayload(role string) ([]byte, error) {
	return json.Marshal(map[string]any{
		"roles": []map[string]any{{"role": role, "isTemporary": false}},
	})
}

func (c *Client) createMembership(ctx context.Context, identityID, projectID, role string) error {
	body, err := rolesPayload(role)
	if err != nil {
		return fmt.Errorf("marshal membership request: %w", err)
	}
	resp, err := c.membershipRequest(ctx, "POST", identityID, projectID, body)
	if err != nil {
		return fmt.Errorf("create membership for %s in %s: %w", identityID, projectID, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("create membership for %s in %s: Infisical answered %d: %s",
			identityID, projectID, resp.StatusCode, firstLine(raw))
	}
	return nil
}

func (c *Client) replaceMembershipRoles(ctx context.Context, identityID, projectID, role string) error {
	body, err := rolesPayload(role)
	if err != nil {
		return fmt.Errorf("marshal membership request: %w", err)
	}
	resp, err := c.membershipRequest(ctx, "PATCH", identityID, projectID, body)
	if err != nil {
		return fmt.Errorf("replace membership roles for %s in %s: %w", identityID, projectID, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("replace membership roles for %s in %s: Infisical answered %d: %s",
			identityID, projectID, resp.StatusCode, firstLine(raw))
	}
	return nil
}

func (c *Client) deleteMembership(ctx context.Context, identityID, projectID string) error {
	resp, err := c.membershipRequest(ctx, "DELETE", identityID, projectID, nil)
	if err != nil {
		return fmt.Errorf("remove membership of %s from %s: %w", identityID, projectID, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("Infisical answered %d: %s", resp.StatusCode, firstLine(raw))
	}
	return nil
}
