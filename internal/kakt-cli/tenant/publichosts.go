package tenant

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
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
	Public struct {
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
		if decl.TenantID == "" || decl.AppID == "" {
			return nil, fmt.Errorf("%s declares public hosts but not both tenantId and appId, which the certificate Secret name is derived from", rel)
		}
		environment := filepath.Base(filepath.Dir(filepath.Dir(path)))

		// WHICH CLUSTER SERVES THE HOSTNAME is not the same question as where the
		// application runs. It runs on every cluster serving its environment, but
		// a public hostname has exactly one owner: two Gateways claiming one host
		// each order a certificate for it and each publish the same DNS record,
		// and external-dns ignores records it does not own.
		//
		// One cluster in the environment needs no answer -- it is the one. More
		// than one must say which, with `publicIngress: true`, and this refuses
		// the ambiguity rather than choosing.
		ingress, err := ingressClusterFor(gitopsDir, environment)
		if err != nil {
			return nil, fmt.Errorf("%s declares public hosts: %w", rel, err)
		}
		for _, host := range decl.Public.Hosts {
			// Refused here as well as in preflight. Two apps sharing a hostname
			// means one listener and two Certificates racing for it, and the
			// producer must not quietly pick a winner.
			if prev, dup := owner[host]; dup {
				return nil, fmt.Errorf("hostname %q is declared by both %s and %s; a public hostname has one owner", host, prev, rel)
			}
			owner[host] = rel
			bySpoke[ingress] = append(bySpoke[ingress], PublicHost{
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

// ingressClusterFor names the one cluster that serves an environment's public
// hostnames.
//
// Deliberately separate from placement: placement is every cluster declaring the
// environment, and hostname ownership is exclusive. One function for both would
// have made the exclusive thing look derivable from the inclusive one.
func ingressClusterFor(gitopsDir, environment string) (string, error) {
	clustersDir := filepath.Join(gitopsDir, RegistryDir, "clusters")
	entries, err := os.ReadDir(clustersDir)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", clustersDir, err)
	}
	var serving, declared []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		claims, _ := filepath.Glob(filepath.Join(clustersDir, e.Name(), "infrastructure", "spokepool*.yaml"))
		if len(claims) == 0 {
			continue // the management cluster serves no application hostnames
		}
		raw, err := os.ReadFile(filepath.Join(clustersDir, e.Name(), "values.yaml"))
		if err != nil {
			continue
		}
		var decl struct {
			EnvironmentSlug string `yaml:"environmentSlug"`
			PublicIngress   bool   `yaml:"publicIngress"`
		}
		if err := yaml.Unmarshal(raw, &decl); err != nil {
			return "", fmt.Errorf("parse %s/values.yaml: %w", e.Name(), err)
		}
		if decl.EnvironmentSlug != environment {
			continue
		}
		serving = append(serving, e.Name())
		if decl.PublicIngress {
			declared = append(declared, e.Name())
		}
	}
	sort.Strings(serving)
	sort.Strings(declared)

	switch {
	case len(serving) == 0:
		return "", fmt.Errorf("no workload cluster serves environment %q, so there is nowhere "+
			"to put the listener. Add one: kakt tenant add-cluster --environment %s",
			environment, environment)
	case len(declared) == 1:
		return declared[0], nil
	case len(declared) > 1:
		return "", fmt.Errorf("%d clusters in environment %q declare publicIngress: true (%s). "+
			"A public hostname has one owner: two Gateways claiming it would each order a "+
			"certificate and each publish the same DNS record. Leave it true on exactly one",
			len(declared), environment, strings.Join(declared, ", "))
	case len(serving) == 1:
		// The only cluster serving the environment is the one, and a field whose
		// value has no alternative is a field nobody should have to set.
		return serving[0], nil
	default:
		return "", fmt.Errorf("environment %q is served by %d clusters (%s) and none declares "+
			"publicIngress: true. A public hostname has one owner and this will not choose: set "+
			"publicIngress: true on the cluster that serves this environment's hostnames",
			environment, len(serving), strings.Join(serving, ", "))
	}
}
