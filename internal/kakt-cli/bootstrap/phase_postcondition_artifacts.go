package bootstrap

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/tenant"
)

// generatedIdentityValuesPresent reports whether this box's own Infisical
// identity is on disk where the rest of the bootstrap reads it.
//
// Cheap, read-only, and a point-in-time probe, which is what a postcondition is
// allowed to be. It answers one question: did the effect of
// bootstrap-infisical-api survive to this run? On a fresh runner that means the
// file was committed, because nothing else carries it between runs.
//
// The path is the one secrets_database_impl.go writes
// (registry/clusters/<cluster>/generated/values/infisical-identity.yaml), and
// the two are kept in step by the ADR-045 artifact registry that validates it.
func (o *Orchestrator) generatedIdentityValuesPresent() error {
	if o.GitopsDir == "" {
		// Not a tenant bootstrap. The file lives under manifests/environments in
		// that case and the phase's own writer owns it; nothing here to assert.
		return nil
	}
	if o.ClusterName == "" {
		return fmt.Errorf("no cluster name, so the generated artifacts cannot be located; " +
			"these belong to one cluster rather than to the repository")
	}
	path := filepath.Join(o.GitopsDir, tenant.RegistryDir, "clusters", o.ClusterName,
		"generated", "values", "infisical-identity.yaml")
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return fmt.Errorf("%s is absent: the phase that writes it is recorded complete, "+
			"but the file did not reach git, so this run has no copy of it",
			filepath.Join(tenant.RegistryDir, "clusters", o.ClusterName, "generated",
				"values", "infisical-identity.yaml"))
	}
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	if info.Size() == 0 {
		return fmt.Errorf("%s is empty, which renders no identity at all", path)
	}
	return nil
}
