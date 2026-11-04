package tenant

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/fleet"
)

// PublicHostsFile is where the aggregate for one spoke is written, relative to
// registry/clusters/<spoke>/. It is a Helm VALUES file, so it lives under
// generated/values/ rather than generated/ -- the per-cluster generated
// Application applies everything in generated/ as objects, and a values file
// there would be applied as a manifest and fail.
const PublicHostsFile = "generated/values/public-hosts.yaml"

// PublicHost is one listener on a spoke's shared TLS Gateway.
type PublicHost struct {
	Host string `yaml:"host"`
	// TLSSecret is the Secret tenant-public-tls' Certificate writes. It is
	// derived from the same scope that chart uses (<tenantId>-<appId>), because
	// the Gateway must name the Secret that chart actually produces -- deriving
	// it differently here would render a listener pointing at a Secret nothing
	// creates, which Gateway API reports as a listener condition rather than as
	// anything the aggregate looks wrong about.
	TLSSecret string `yaml:"tlsSecret"`
	// App is the declaration this entry came from. Written so the file says
	// where each listener originates -- it is generated, and a reader otherwise
	// has no way back to the source.
	App string `yaml:"app"`
}

type publicHostsDoc struct {
	PublicHosts []PublicHost `yaml:"publicHosts"`
}

// appDeclaration is the subset of an app's values.yaml this reads.
type appDeclaration struct {
	TenantID string `yaml:"tenantId"`
	AppID    string `yaml:"appId"`
	// The managed cluster this application runs on. Was `cellId`; a cell is a
	// policy group (ADR-064) and never named this.
	ClusterName string `yaml:"clusterName"`
	Public      struct {
		Hosts []string `yaml:"hosts"`
	} `yaml:"public"`
}

// AggregatePublicHosts derives each spoke's complete public-hostname set from
// the app declarations and writes it as a generated values file (ADR-096).
//
// THE AGGREGATE HAS ONE PRODUCER. The shared :443 Gateway has one owner, and
// its listener list comes from here -- not from each app's own Application,
// which is the arrangement that flapped public DNS. Hand-editing the output
// reintroduces two sources of truth for the same fact, so the preflight check
// asserts the file equals this function's result exactly.
//
// Returns the spokes it wrote, so a caller can report them.
func AggregatePublicHosts(gitopsDir string) ([]string, error) {
	envRoot := filepath.Join(gitopsDir, "environments")
	matches, err := filepath.Glob(filepath.Join(envRoot, "*", "*", "values.yaml"))
	if err != nil {
		return nil, fmt.Errorf("scan %s: %w", envRoot, err)
	}
	sort.Strings(matches)

	bySpoke := map[string][]PublicHost{}
	owner := map[string]string{} // host -> the app that declared it

	for _, path := range matches {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		var decl appDeclaration
		if err := yaml.Unmarshal(raw, &decl); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		if len(decl.Public.Hosts) == 0 {
			continue
		}
		rel, _ := filepath.Rel(gitopsDir, path)
		if decl.ClusterName == "" {
			return nil, fmt.Errorf("%s declares public hosts but no clusterName, so there is no spoke to put the listener on", rel)
		}
		if decl.TenantID == "" || decl.AppID == "" {
			return nil, fmt.Errorf("%s declares public hosts but not both tenantId and appId, which the certificate Secret name is derived from", rel)
		}
		// The named cluster has to exist, be a workload cluster, and serve this
		// directory's environment. The listener is written into that cluster's
		// aggregate, so a name with nothing behind it publishes a hostname onto
		// nothing, and one serving another environment publishes a staging
		// hostname on development capacity -- neither of which the Gateway can
		// report.
		environment := filepath.Base(filepath.Dir(filepath.Dir(path)))
		if err := fleet.VerifyTargetCluster(gitopsDir, environment, decl.ClusterName); err != nil {
			return nil, fmt.Errorf("%s %w", rel, err)
		}
		for _, host := range decl.Public.Hosts {
			// Refused here as well as in preflight. Two apps sharing a hostname
			// means one listener and two Certificates racing for it, and the
			// producer must not quietly pick a winner.
			if prev, dup := owner[host]; dup {
				return nil, fmt.Errorf("hostname %q is declared by both %s and %s; a public hostname has one owner", host, prev, rel)
			}
			owner[host] = rel
			bySpoke[decl.ClusterName] = append(bySpoke[decl.ClusterName], PublicHost{
				Host:      host,
				TLSSecret: fmt.Sprintf("%s-%s-public-tls", decl.TenantID, decl.AppID),
				App:       rel,
			})
		}
	}

	// Every registered spoke gets a file, including spokes with no public app.
	// Writing an empty aggregate rather than no file is deliberate: it is what
	// makes removing the last hostname from a spoke converge, instead of
	// leaving the previous file in place forever.
	clustersDir := filepath.Join(gitopsDir, RegistryDir, "clusters")
	entries, err := os.ReadDir(clustersDir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", clustersDir, err)
	}

	var written []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		spoke := e.Name()
		hosts := bySpoke[spoke]
		sort.Slice(hosts, func(i, j int) bool { return hosts[i].Host < hosts[j].Host })

		out := filepath.Join(clustersDir, spoke, PublicHostsFile)
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return nil, fmt.Errorf("create %s: %w", filepath.Dir(out), err)
		}
		body, err := yaml.Marshal(publicHostsDoc{PublicHosts: hosts})
		if err != nil {
			return nil, fmt.Errorf("render %s: %w", out, err)
		}
		header := strings.Join([]string{
			"# GENERATED by `kakt fleet hosts aggregate` (ADR-096). Do not edit.",
			"#",
			"# The spoke's complete public-hostname set, derived from every app's",
			"# public.hosts on this spoke. It is the listener list of the shared :443",
			"# Gateway, which has exactly one owner -- the platform-spoke-gateway",
			"# Application. Editing this by hand puts two sources of truth behind one",
			"# object again, which is what flapped public DNS before ADR-096.",
			"#",
			"# Re-run the aggregation after changing any app's public.hosts. A preflight",
			"# check asserts this file equals the declarations exactly, in both",
			"# directions, so drift fails review rather than becoming a hostname that",
			"# does not resolve.",
			"",
		}, "\n")
		if err := os.WriteFile(out, []byte(header+string(body)), 0o644); err != nil {
			return nil, fmt.Errorf("write %s: %w", out, err)
		}
		written = append(written, spoke)
	}
	return written, nil
}
