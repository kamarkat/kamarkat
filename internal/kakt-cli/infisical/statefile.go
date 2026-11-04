package infisical

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const stateFileName = "infisical-bootstrap.json"

func stateDir() string {
	// .state, the one place a box keeps what has been done to it. This was
	// .zero-ops, which also held logs and a generated kind config, so a durable
	// record sat among run artefacts and "delete the logs" could delete it.
	cwd, err := os.Getwd()
	if err != nil {
		return ".state"
	}
	return filepath.Join(cwd, ".state")
}

// stateFilePath is the one place this record lives.
//
// It briefly also read .zero-ops/, the directory this replaced, so a box
// bootstrapped by an earlier build would still be found. That made the path
// depend on what happened to exist on disk: nothing has written to .zero-ops
// since the move, so a file still sitting there is stale by definition, and
// preferring it silently reused Day-0 Infisical credentials the current box
// never issued.
func stateFilePath() string {
	return filepath.Join(stateDir(), stateFileName)
}

// BootstrapState is the on-disk cache written after a successful Day-0
// bootstrap. On re-run (local development) the saved state is returned
// immediately, skipping all API calls — no duplicate Machine Identities,
// no failed grants, no stale ESO credentials.
type BootstrapState struct {
	OrgID              string `json:"orgId"`
	ProjectID          string `json:"projectId"`
	ProjectSlug        string `json:"projectSlug"`
	SecretsProjectID   string `json:"secretsProjectId"`
	SecretsProjectSlug string `json:"secretsProjectSlug"`
	ClientID           string `json:"clientId"`
	ClientSecret       string `json:"clientSecret"`
	IdentityID         string `json:"identityId"`
}

func saveBootstrapState(r *BootstrapResult) error {
	state := &BootstrapState{
		OrgID:              r.OrgID,
		ProjectID:          r.ProjectID,
		ProjectSlug:        r.ProjectSlug,
		SecretsProjectID:   r.SecretsProjectID,
		SecretsProjectSlug: r.SecretsProjectSlug,
		ClientID:           r.ClientID,
		ClientSecret:       r.ClientSecret,
		IdentityID:         r.IdentityID,
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal bootstrap state: %w", err)
	}

	dir := stateDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create state dir %s: %w", dir, err)
	}

	if err := os.WriteFile(stateFilePath(), data, 0600); err != nil {
		return fmt.Errorf("write bootstrap state: %w", err)
	}

	fmt.Printf("[infisical-bootstrap] Saved bootstrap state to %s\n", stateFilePath())
	return nil
}

func loadBootstrapState() (*BootstrapState, error) {
	data, err := os.ReadFile(stateFilePath())
	if err != nil {
		return nil, fmt.Errorf("read bootstrap state: %w", err)
	}

	var state BootstrapState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("unmarshal bootstrap state: %w", err)
	}
	return &state, nil
}
