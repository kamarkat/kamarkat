package infisical

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Looking up a workload cluster's secrets project.
//
// LOOKUP ONLY. hub-operator creates the project and its environment, because
// hub-operator writes the secrets that go in it; this operator scopes an identity
// to it. Two creators would race to create one project per cluster and the loser
// would find a project whose id it did not record.
//
// The slug arrives on the SpokeMachineIdentity
// (spec.infisical.secretsProjectSlug), published by hub-operator only after the
// project holds this cluster's credentials. So it is both the destination and the
// readiness signal, and this operator never computes it -- one formula, in the
// component that owns the project.

// FindProjectBySlug returns the project id for a slug, or "" when the
// organization has no such project.
//
// An unreadable listing is an error and never "". Reading it as "absent" would
// make the caller treat a transient failure as "the project is gone", and the
// narrowing that follows would be decided on a guess.
func (c *Client) FindProjectBySlug(ctx context.Context, slug string) (string, error) {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return "", fmt.Errorf("no project slug to look up")
	}

	token, err := c.getToken(ctx)
	if err != nil {
		return "", fmt.Errorf("authenticate: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", c.BaseURL+PathProjects+"?limit=100", nil)
	if err != nil {
		return "", fmt.Errorf("list projects request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("list projects: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("list projects: Infisical answered %d: %s", resp.StatusCode, firstLine(raw))
	}

	// `projects`, per project-router.ts's declared 200 body. Reading `workspaces`
	// -- the key the DIFFERENT /api/v1/workspace endpoint uses -- made every
	// published project look absent, which is what reported nutgraf-stg-01's
	// secrets project as non-existent on 0.3.45-rc.1.
	var listed struct {
		Projects []struct {
			ID   string `json:"id"`
			Slug string `json:"slug"`
		} `json:"projects"`
		TotalCount int `json:"totalCount"`
	}
	if err := json.Unmarshal(raw, &listed); err != nil {
		return "", fmt.Errorf("list projects: a 200 this operator could not read: %w", err)
	}
	for _, p := range listed.Projects {
		if p.Slug == slug {
			return p.ID, nil
		}
	}
	if listed.TotalCount > len(listed.Projects) {
		return "", fmt.Errorf("the organization has %d projects and this page carried %d, so "+
			"%q cannot be said to be absent", listed.TotalCount, len(listed.Projects), slug)
	}
	return "", nil
}
