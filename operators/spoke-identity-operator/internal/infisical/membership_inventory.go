package infisical

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// Read-only observation of what a Machine Identity is authorized to do.
//
// WHY IT EXISTS SEPARATELY FROM THE GRANT. The operator asks Infisical for an
// "admin" project membership and has always treated the response code as the
// answer. A 400 or 409 saying the membership already exists was counted as
// success, which establishes that A membership is there and nothing about what it
// grants -- so an identity carrying a role nobody intended looks identical to one
// carrying the intended role.
//
// WHAT THIS IS NOT. Retrieving a membership does not establish effective
// authorization. Three different things:
//
//	observed   the memberships and roles were read back
//	configured they match an intended policy
//	verified   permitted operations succeed and forbidden ones fail
//
// This file produces the FIRST only. There is no intended policy to compare
// against yet -- project-per-cluster is designed and not reconciled -- and a role
// name is not an access decision; only attempting an operation establishes that.
// Nothing here may be reported as compliant or verified.
//
// UNKNOWN STAYS UNKNOWN. A query that fails is recorded as un-retrieved with the
// reason, never as an absent membership. The two are different facts and the
// difference is the point: reading "no membership" off a 500 is how an identity
// with broad access gets reported as having none.

// notFoundMembership is the message the pinned Infisical returns when an identity
// genuinely has no membership of a project (membership-identity-service
// getMembershipByIdentityId). Recorded here because the status code alone does not
// carry that meaning; the route and this body are checked against the pinned
// source by scripts/validate/preflight/031-infisical-api-contract.py.
const notFoundMembership = "Identity membership not found"

// firstLine keeps an error body to one short line for a log message, and never
// returns a credential: this endpoint's errors carry a message, not secrets.
func firstLine(body []byte) string {
	line := strings.TrimSpace(strings.SplitN(string(body), "\n", 2)[0])
	if len(line) > 160 {
		return line[:160] + "..."
	}
	if line == "" {
		return "(empty body)"
	}
	return line
}

// MembershipRole is one role on one membership, as Infisical returns it.
//
// Temporary roles are kept apart rather than flattened: a temporary admin is
// present now and gone later, so folding it into the role list would make the
// same identity read differently depending on when it was observed.
type MembershipRole struct {
	Role        string
	IsTemporary bool
}

// ProjectMembership is one identity's membership of one project, or the reason it
// could not be read.
type ProjectMembership struct {
	ProjectID string

	// Retrieved is true only when Infisical answered the question. False means
	// this project's authorization is UNKNOWN, not empty.
	Retrieved bool

	// Present distinguishes "Infisical says there is no membership" (a 404, which
	// is an answer) from "we could not ask". Meaningful only when Retrieved.
	Present bool

	Roles []MembershipRole

	// Why carries the reason a membership could not be read, for the operator's
	// log and the condition message. Never a credential or a secret value.
	Why string
}

// Summary renders one membership for a log line: the roles, or why they are
// unknown. Deliberately terse and free of identifiers a reader does not need.
func (m ProjectMembership) Summary() string {
	switch {
	case !m.Retrieved:
		return fmt.Sprintf("%s: unknown (%s)", m.ProjectID, m.Why)
	case !m.Present:
		return fmt.Sprintf("%s: no membership", m.ProjectID)
	case len(m.Roles) == 0:
		return fmt.Sprintf("%s: membership with no roles", m.ProjectID)
	}
	names := make([]string, 0, len(m.Roles))
	for _, r := range m.Roles {
		if r.IsTemporary {
			names = append(names, r.Role+" (temporary)")
			continue
		}
		names = append(names, r.Role)
	}
	sort.Strings(names)
	return fmt.Sprintf("%s: %s", m.ProjectID, strings.Join(names, ", "))
}

// AuthorizationInventory is everything one read established, and everything it
// did not.
type AuthorizationInventory struct {
	IdentityID   string
	Memberships  []ProjectMembership
	NotInspected []string
}

// Complete reports whether every project asked about was answered. A false here
// is why the inventory cannot be the basis of an authorization decision.
func (i AuthorizationInventory) Complete() bool {
	for _, m := range i.Memberships {
		if !m.Retrieved {
			return false
		}
	}
	return len(i.NotInspected) == 0
}

// Unresolved names what this read left open, so a caller reports it rather than
// rounding it to nothing.
func (i AuthorizationInventory) Unresolved() []string {
	var open []string
	for _, m := range i.Memberships {
		if !m.Retrieved {
			open = append(open, fmt.Sprintf("%s (%s)", m.ProjectID, m.Why))
		}
	}
	return append(open, i.NotInspected...)
}

// notInspectedSurfaces are the authorization sources this read does NOT cover.
//
// Named rather than omitted. An inventory that silently covered memberships alone
// would report an identity as narrowly scoped while an additional privilege
// granted it everything -- and the reader would have no way to tell that the
// question had not been asked. Each needs its own endpoint recorded in
// scripts/validate/infisical-api-contract.yaml before it can be inspected, and
// inventing those paths from a router file is the mistake that produced a wrong
// architecture finding on 2026-11-22.
var notInspectedSurfaces = []string{
	"identity additional privileges (a separate v2 endpoint, not yet in the API contract)",
	"group-derived permissions (whether an identity can hold them at all is unestablished)",
	"organization-level role (read at creation, not re-read here)",
}

// InventoryProjectMemberships reads this identity's membership of each project.
//
// Read-only against Infisical: GET alone, no grant, no replacement, no deletion.
// It never returns an error for an unreadable project -- that is recorded in the
// inventory, because a partial answer is still worth having and a caller that
// aborts on the first unreadable project learns nothing about the others.
func (c *Client) InventoryProjectMemberships(ctx context.Context, identityID string, projectIDs []string) (*AuthorizationInventory, error) {
	if strings.TrimSpace(identityID) == "" {
		return nil, fmt.Errorf("no identity to inventory: an empty identity id would read " +
			"whatever Infisical returns for a missing path and report it as this identity's")
	}

	inv := &AuthorizationInventory{IdentityID: identityID, NotInspected: notInspectedSurfaces}
	for _, pid := range projectIDs {
		if strings.TrimSpace(pid) == "" {
			continue
		}
		inv.Memberships = append(inv.Memberships, c.projectMembership(ctx, identityID, pid))
	}
	if len(inv.Memberships) == 0 {
		return nil, fmt.Errorf("no project named to inventory for identity %s", identityID)
	}
	return inv, nil
}

func (c *Client) projectMembership(ctx context.Context, identityID, projectID string) ProjectMembership {
	out := ProjectMembership{ProjectID: projectID}

	token, err := c.getToken(ctx)
	if err != nil {
		out.Why = fmt.Sprintf("could not authenticate to Infisical: %v", err)
		return out
	}

	url := c.BaseURL + fmt.Sprintf(PathProjectMembershipsIdentities, projectID, identityID)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		out.Why = fmt.Sprintf("could not build the request: %v", err)
		return out
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		out.Why = fmt.Sprintf("the request did not complete: %v", err)
		return out
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	switch {
	case resp.StatusCode == http.StatusNotFound:
		// A 404 IS NOT AUTOMATICALLY AN ABSENT MEMBERSHIP. Only one of the ways to
		// get one is an answer to the question asked: getMembershipByIdentityId
		// raises NotFoundError("Identity membership not found") when the identity
		// has no membership of the project. A mistyped project id, an identity that
		// does not exist, or a path that no longer resolves also produce 404s, and
		// reading those as "no membership" would report an identity with broad
		// access as having none -- the exact inversion this file exists to prevent.
		if strings.Contains(string(body), notFoundMembership) {
			out.Retrieved, out.Present = true, false
			return out
		}
		out.Why = fmt.Sprintf("Infisical answered 404 without saying the membership is "+
			"missing, so this may be an unknown project or identity rather than an "+
			"absent membership: %s", firstLine(body))
		return out
	case resp.StatusCode != http.StatusOK:
		// Includes 403. A refusal to tell us is not a statement that there is
		// nothing to tell.
		out.Why = fmt.Sprintf("Infisical answered %d", resp.StatusCode)
		return out
	}

	var parsed struct {
		Membership struct {
			Roles []struct {
				Role        string                 `json:"role"`
				CustomRole  *struct{ Slug string } `json:"customRole"`
				IsTemporary bool                   `json:"isTemporary"`
			} `json:"roles"`
		} `json:"identityMembership"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		out.Why = fmt.Sprintf("Infisical answered 200 with a body this operator could not read: %v", err)
		return out
	}

	out.Retrieved, out.Present = true, true
	for _, r := range parsed.Membership.Roles {
		name := r.Role
		if r.CustomRole != nil && r.CustomRole.Slug != "" {
			name = r.CustomRole.Slug
		}
		out.Roles = append(out.Roles, MembershipRole{Role: name, IsTemporary: r.IsTemporary})
	}
	return out
}
