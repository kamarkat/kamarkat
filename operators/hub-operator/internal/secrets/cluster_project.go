package secrets

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/kamarkat/kamarkat/operators/hub-operator/internal/constant"
)

// A workload cluster's own Infisical project: the only secret boundary an
// unlicensed Infisical enforces.
//
// WHY NOT A PATH OR AN ENVIRONMENT. A built-in project role carries no
// conditions. `viewer` grants DescribeSecret and ReadValue on Secrets across
// every environment and every path of its project (Infisical v0.167.12
// default-roles.ts buildViewerPermissionRules), and conditioning a role on an
// environment or a secretPath requires a custom role, which createRole refuses
// without PlanFeature.Rbac -- false on the default plan, along with
// secretsFolderRbac. So /spoke-pool/<cluster>/ and the store's environmentSlug
// say who OWNS a secret, and the project is what stops one cluster reading
// another's.
//
// HUB-OPERATOR OWNS THIS because it owns secret materialisation. The identity
// operator scopes identities to the project by the slug published on the
// SpokeMachineIdentity; it does not create the project. One creator, one writer.

// ClusterProjectSlug is the deterministic slug of a workload cluster's secrets
// project.
//
// MUST MATCH two other places, and a source check asserts it:
// the spoke's ClusterSecretStore patch in
// manifests/argocd/environment-manager/templates/03-platform-services-appset.yaml,
// and nothing else -- the identity operator reads the slug from the CR rather
// than recomputing it.
//
// No box id in the name. Infisical runs inside the box
// (manifests/hub-core-services/infisical), so its organization holds one box's
// projects; repeating the box would restate the container in everything it holds.
func ClusterProjectSlug(clusterName string) string {
	return fmt.Sprintf("spoke-%s-secrets", strings.ToLower(strings.TrimSpace(clusterName)))
}

// ClusterProjectName is what a person sees in Infisical.
func ClusterProjectName(clusterName string) string {
	return fmt.Sprintf("Workload cluster %s", strings.TrimSpace(clusterName))
}

// projectCache avoids listing every reconcile for a project that cannot change
// identity: the slug is derived from the cluster's name, and Infisical allocates
// the id once.
var (
	projectIDBySlug = map[string]string{}
	projectCacheMu  sync.RWMutex
)

// EnsureClusterProject returns the id of the cluster's secrets project, creating
// it and its one environment when they do not exist.
//
// WITHOUT INFISICAL'S DEFAULT ENVIRONMENTS. Those are dev, staging and prod, and
// a cluster serving `stg` selects an environment whose slug is `stg` -- one word
// from `staging`, and silently empty. The declared slug is created explicitly
// instead, so what the store selects is what exists.
func (c *InfisicalClient) EnsureClusterProject(ctx context.Context, clusterName, environmentSlug string) (string, error) {
	slug := ClusterProjectSlug(clusterName)
	if strings.TrimSpace(environmentSlug) == "" {
		return "", fmt.Errorf("cluster %s declares no environment, so its secrets project has "+
			"no environment to create: the store selects one by slug, and defaulting it would "+
			"point a cluster at secrets that are not its own", clusterName)
	}

	projectCacheMu.RLock()
	cached := projectIDBySlug[slug]
	projectCacheMu.RUnlock()

	projectID := cached
	if projectID == "" {
		found, err := c.findProjectBySlug(ctx, slug)
		if err != nil {
			return "", err
		}
		projectID = found
	}

	if projectID == "" {
		created, err := c.createProject(ctx, clusterName, slug)
		if err != nil {
			return "", err
		}
		projectID = created
	}

	if err := c.ensureProjectEnvironment(ctx, projectID, environmentSlug); err != nil {
		return "", err
	}

	projectCacheMu.Lock()
	projectIDBySlug[slug] = projectID
	projectCacheMu.Unlock()
	return projectID, nil
}

// findProjectBySlug returns "" only when the organization genuinely has no such
// project. An unreadable listing is an error: reading it as "absent" would create
// a second project for a cluster, and then the store would read one while the
// publisher wrote the other.
func (c *InfisicalClient) findProjectBySlug(ctx context.Context, slug string) (string, error) {
	// limit=100 is the schema's maximum, and totalCount below says whether one page
	// was enough. The default page size is not documented, so asking for the
	// maximum and then checking the count is the difference between "no such
	// project" and "not on the first page" -- and this function's "" means the
	// former, which a caller acts on by CREATING a project.
	req, err := c.newAuthenticatedRequest(ctx, "GET", c.BaseURL+constant.APIEndpointProjects+"?limit=100", nil)
	if err != nil {
		return "", err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("list Infisical projects: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("list Infisical projects: status %d", resp.StatusCode)
	}

	// `projects`, which is what the route returns at both pinned versions
	// (project-router.ts `response: { 200: z.object({ projects, totalCount }) }`).
	// This read `workspaces` -- the key /api/v1/workspace uses, a different
	// endpoint -- so every lookup came back empty: the identity operator reported
	// a published project as non-existent, and this function told its own caller to
	// create a project that was already there.
	var listed struct {
		Projects []struct {
			ID   string `json:"id"`
			Slug string `json:"slug"`
		} `json:"projects"`
		TotalCount int `json:"totalCount"`
	}
	if err := json.Unmarshal(raw, &listed); err != nil {
		return "", fmt.Errorf("list Infisical projects: a 200 this operator could not read: %w", err)
	}
	for _, p := range listed.Projects {
		if p.Slug == slug {
			return p.ID, nil
		}
	}
	if listed.TotalCount > len(listed.Projects) {
		// Not found AND not fully read. Returning "" would have the caller create a
		// second project for a cluster that already has one.
		return "", fmt.Errorf("the organization has %d projects and this page carried %d, so "+
			"%q cannot be said to be absent", listed.TotalCount, len(listed.Projects), slug)
	}
	return "", nil
}

func (c *InfisicalClient) createProject(ctx context.Context, clusterName, slug string) (string, error) {
	body, err := json.Marshal(map[string]any{
		"projectName": ClusterProjectName(clusterName),
		"slug":        slug,
		// The cluster's environment is created explicitly; see EnsureClusterProject.
		"shouldCreateDefaultEnvs": false,
		// A project holding a workload cluster's credentials is not something to
		// lose to a stray call.
		"hasDeleteProtection": true,
	})
	if err != nil {
		return "", fmt.Errorf("marshal project request: %w", err)
	}
	req, err := c.newAuthenticatedRequest(ctx, "POST", c.BaseURL+constant.APIEndpointProjects, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("create project %s: %w", slug, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("create project %s: status %d: %s", slug, resp.StatusCode, firstLine(raw))
	}

	var created struct {
		Project struct {
			ID string `json:"id"`
		} `json:"project"`
	}
	if err := json.Unmarshal(raw, &created); err != nil || created.Project.ID == "" {
		return "", fmt.Errorf("created project %s and could not read its id, so nothing can be "+
			"written to it: %v", slug, err)
	}
	return created.Project.ID, nil
}

// ensureProjectEnvironment is idempotent through the by-slug lookup, so a
// reconcile neither duplicates an environment nor fails on one that exists.
func (c *InfisicalClient) ensureProjectEnvironment(ctx context.Context, projectID, envSlug string) error {
	lookup := c.BaseURL + fmt.Sprintf(constant.APIEndpointProjectEnvironmentSlug, projectID, envSlug)
	req, err := c.newAuthenticatedRequest(ctx, "GET", lookup, nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("look up environment %s: %w", envSlug, err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	if resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("look up environment %s in project %s: status %d", envSlug, projectID, resp.StatusCode)
	}

	body, err := json.Marshal(map[string]any{"name": envSlug, "slug": envSlug})
	if err != nil {
		return fmt.Errorf("marshal environment request: %w", err)
	}
	create := c.BaseURL + fmt.Sprintf(constant.APIEndpointProjectEnvironments, projectID)
	creq, err := c.newAuthenticatedRequest(ctx, "POST", create, bytes.NewReader(body))
	if err != nil {
		return err
	}
	creq.Header.Set("Content-Type", "application/json")

	cresp, err := c.httpClient.Do(creq)
	if err != nil {
		return fmt.Errorf("create environment %s: %w", envSlug, err)
	}
	defer cresp.Body.Close()
	raw, _ := io.ReadAll(cresp.Body)
	if cresp.StatusCode != http.StatusOK && cresp.StatusCode != http.StatusCreated {
		return fmt.Errorf("create environment %s in project %s: status %d: %s",
			envSlug, projectID, cresp.StatusCode, firstLine(raw))
	}
	return nil
}

// firstLine keeps an error body to one short line, and never returns a secret:
// these endpoints answer errors with a message.
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
