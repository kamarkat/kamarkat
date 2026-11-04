package tenant

import (
	"context"
	"os"
	"testing"
)

// Opt-in: reaches GitHub. Run with KAKT_DISCOVER_ORG set.
func TestDiscoverAgainstGitHub(t *testing.T) {
	org := os.Getenv("KAKT_DISCOVER_ORG")
	gitops := os.Getenv("KAKT_DISCOVER_GITOPS")
	if org == "" || gitops == "" {
		t.Skip("set KAKT_DISCOVER_ORG and KAKT_DISCOVER_GITOPS")
	}
	repos, err := ReposNeedingGitopsToken(context.Background(), org, gitops)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("repositories needing the credential: %v", repos)
}
