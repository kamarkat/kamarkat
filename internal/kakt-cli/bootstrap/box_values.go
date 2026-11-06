package bootstrap

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/tenant"
	"sigs.k8s.io/yaml"
)

// hubOverlayDir is the platform tree's single hub overlay (ADR-111 Revision 7).
//
// manifests/environments/ held one overlay per environment -- dev, stg, prod --
// because a hub belonged to one. One shared management hub has one overlay, so
// ADR-045 artifacts have one directory to be written to and Day-0 has nothing to
// choose. A tenant's repository holds the same artifacts under the cluster they
// belong to instead (ADR-072), which the registry rewrites.
//
// Deliberately NOT the management secret scope. They are both the word
// "management" today and they are different things: one is a directory in this
// repository, the other an environment in Infisical. Deriving either from the
// other would mean activating the scope silently moved the artifacts.
const HubOverlayDir = "management"

// boxValues are the declarations a box makes about itself, read from the one file
// that holds them: registry/clusters/<cluster>/values.yaml.
//
// NOT A NEW AUTHORITY. The environment-manager chart reads these same keys from
// this same file; Day-0 reads them so each value is declared once and both
// consume it. Before this, Day-0 derived two of them from `--environment`:
//
//   - the management secret scope, which made the hub's own credentials live in a
//     workload environment (ADR-111 Revision 7 §2);
//   - the public TLS issuer, which was a second authority for a policy the chart
//     already requires as a value -- two mappings to keep in agreement, and the
//     chart's was the one that shipped.
//
// Reading the declaration is also what makes activating the management scope a
// one-line edit to this file rather than a code change.
type BoxValues struct {
	Management struct {
		InfisicalEnvironmentSlug string `json:"infisicalEnvironmentSlug"`
	} `json:"management"`
	PublicTlsIssuer string `json:"publicTlsIssuer"`
}

// ReadBoxValues loads them, or says which file was missing what.
//
// No defaults. A guess at the management scope is a guess at which environment
// someone else's secrets are in, and it is silent: the secrets resolve, from the
// wrong scope. A guess at the TLS issuer is a certificate the browser refuses, or
// a quota spent on a long-lived cluster.
func ReadBoxValues(gitopsDir, clusterName string) (BoxValues, error) {
	var v BoxValues
	if gitopsDir == "" {
		return v, fmt.Errorf("no gitops directory: the box's own declarations " +
			"(registry/clusters/<cluster>/values.yaml) are the authority for the " +
			"management secret scope and the public TLS issuer, and there is nowhere to read them")
	}
	if clusterName == "" {
		return v, fmt.Errorf("no cluster name: these declarations belong to one " +
			"cluster rather than to the repository")
	}
	path := filepath.Join(gitopsDir, tenant.RegistryDir, "clusters", clusterName, "values.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return v, fmt.Errorf("read the box's declarations at %s: %w", path, err)
	}
	if err := yaml.Unmarshal(data, &v); err != nil {
		return v, fmt.Errorf("parse %s: %w", path, err)
	}
	if v.Management.InfisicalEnvironmentSlug == "" {
		return v, fmt.Errorf("%s declares no management.infisicalEnvironmentSlug: it is "+
			"the Infisical environment holding THIS HUB's own credentials (ADR-111 "+
			"Revision 7 §2), and it is not a workload environment. Day-0 writes the "+
			"hub's secrets there and the ClusterSecretStores read them from there, so "+
			"there is no value to assume", path)
	}
	if v.PublicTlsIssuer == "" {
		return v, fmt.Errorf("%s declares no publicTlsIssuer: the box owns its own "+
			"certificate policy (ADR-111 Revision 7 §3) and the chart requires the "+
			"value, so Day-0 will not guess one", path)
	}
	return v, nil
}

// ManagementScope is the Infisical environment holding this hub's own credentials.
//
// An accessor rather than a bare field so the one place that names the YAML key is
// the struct tag: a caller asks for the management scope and cannot accidentally
// read it as a workload environment.
func (v BoxValues) ManagementScope() string { return v.Management.InfisicalEnvironmentSlug }
