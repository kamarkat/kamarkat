package gitops

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/kamarkat/kamarkat/internal/kube-sbt/interfaces"
)

// Config for the GitOps Helm Provisioner.
type Config struct {
	// Git repository settings
	RepoURL    string // e.g. https://github.com/org/gitops-repo
	Branch     string // default: main
	TenantsDir string // default: tenants
	ChartsDir  string // default: base-charts/tenant-factory

	// Authentication — token (GitHub PAT / GitLab token)
	GitToken string

	// ArgoCD webhook URL for instant sync (15.13)
	ArgoCDWebhookURL string

	// ArgoCD API for direct sync trigger
	ArgoCDAPIURL   string
	ArgoCDAPIToken string

	// Warm pool settings
	WarmPoolTarget int // default: 10 slots per tier
	
	// Storage for Status Controller Pattern
	Storage interfaces.IStorage
}

func (c *Config) defaults() {
	if c.Branch == "" {
		c.Branch = "main"
	}
	if c.TenantsDir == "" {
		c.TenantsDir = "tenants"
	}
	if c.ChartsDir == "" {
		c.ChartsDir = "base-charts/tenant-factory"
	}
	if c.WarmPoolTarget == 0 {
		c.WarmPoolTarget = 10
	}
}

// gitHubFile represents a GitHub API file object for create/update.
type gitHubFile struct {
	Message string `json:"message"`
	Content string `json:"content"` // base64
	SHA     string `json:"sha,omitempty"`
	Branch  string `json:"branch"`
}

// gitHubFileResponse is the response from GitHub Contents API.
type gitHubFileResponse struct {
	Content string `json:"content"` // base64
	SHA     string `json:"sha"`
}

// gitClient is a minimal GitHub Contents API client (15.1).
type gitClient struct {
	cfg    Config
	http   *http.Client
	apiURL string // https://api.github.com/repos/<owner>/<repo>
}

func newGitClient(cfg Config) (*gitClient, error) {
	if cfg.RepoURL == "" {
		return nil, fmt.Errorf("gitops: RepoURL is required")
	}
	// Derive GitHub API URL from repo URL
	// https://github.com/org/repo → https://api.github.com/repos/org/repo
	apiURL, err := repoURLToAPIURL(cfg.RepoURL)
	if err != nil {
		return nil, err
	}
	return &gitClient{
		cfg:    cfg,
		http:   &http.Client{Timeout: 30 * time.Second},
		apiURL: apiURL,
	}, nil
}

func repoURLToAPIURL(repoURL string) (string, error) {
	// Supports https://github.com/org/repo and https://github.com/org/repo.git
	var org, repo string
	_, err := fmt.Sscanf(repoURL, "https://github.com/%s", &org)
	if err != nil {
		return "", fmt.Errorf("gitops: unsupported repo URL format: %s", repoURL)
	}
	// org is "org/repo" or "org/repo.git"
	if len(org) > 4 && org[len(org)-4:] == ".git" {
		org = org[:len(org)-4]
	}
	// Split org/repo
	for i, c := range org {
		if c == '/' {
			repo = org[i+1:]
			org = org[:i]
			break
		}
	}
	if repo == "" {
		return "", fmt.Errorf("gitops: cannot parse org/repo from %s", repoURL)
	}
	return fmt.Sprintf("https://api.github.com/repos/%s/%s", org, repo), nil
}

// getFile fetches a file's content and SHA from GitHub (15.2, 15.3).
func (g *gitClient) getFile(ctx context.Context, path string) (content []byte, sha string, err error) {
	url := fmt.Sprintf("%s/contents/%s?ref=%s", g.apiURL, path, g.cfg.Branch)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	g.setHeaders(req)
	resp, err := g.http.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, "", nil // file doesn't exist yet
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("gitops: getFile %s: HTTP %d", path, resp.StatusCode)
	}
	var f gitHubFileResponse
	if err := json.NewDecoder(resp.Body).Decode(&f); err != nil {
		return nil, "", err
	}
	data, err := base64.StdEncoding.DecodeString(
		// GitHub wraps base64 with newlines
		replaceNewlines(f.Content),
	)
	return data, f.SHA, err
}

// putFile creates or updates a file in the Git repository (15.2).
func (g *gitClient) putFile(ctx context.Context, path, message string, content []byte, sha string) error {
	body := gitHubFile{
		Message: message,
		Content: base64.StdEncoding.EncodeToString(content),
		Branch:  g.cfg.Branch,
		SHA:     sha,
	}
	data, _ := json.Marshal(body)
	url := fmt.Sprintf("%s/contents/%s", g.apiURL, path)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(data))
	g.setHeaders(req)
	resp, err := g.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("gitops: putFile %s: HTTP %d: %s", path, resp.StatusCode, body)
	}
	return nil
}

// deleteFile removes a file from the Git repository (15.3).
func (g *gitClient) deleteFile(ctx context.Context, path, message, sha string) error {
	body := map[string]string{
		"message": message,
		"sha":     sha,
		"branch":  g.cfg.Branch,
	}
	data, _ := json.Marshal(body)
	url := fmt.Sprintf("%s/contents/%s", g.apiURL, path)
	req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, url, bytes.NewReader(data))
	g.setHeaders(req)
	resp, err := g.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("gitops: deleteFile %s: HTTP %d", path, resp.StatusCode)
	}
	return nil
}

func (g *gitClient) setHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+g.cfg.GitToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
}

func replaceNewlines(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '\n' {
			out = append(out, s[i])
		}
	}
	return string(out)
}
