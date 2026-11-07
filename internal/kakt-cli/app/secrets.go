// Package app reads what an application declares and reports what has been
// supplied.
//
// ADR-087 splits an application's runtime input by who owns the value. The secrets
// half is declared in the application's own repository and supplied out of band, and
// this package is the join between those two halves: the declaration, from git,
// and the key names present in the provider.
//
// Nothing here handles a secret VALUE. Supplying one is a write the caller
// performs; reporting on one is done by name. That is a property of the types,
// not a convention -- see infisical.ListSecretNames.
package app

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"
)

// Declaration is one entry in an application's `secrets:` list.
type Declaration struct {
	// Name is the key, identical in the provider and in the delivered Secret.
	Name string `yaml:"name"`
	// Capability is what the application loses without it, and is what a report names
	// -- a key alone is not something an operator can act on.
	Capability string `yaml:"capability"`
	// Workloads read it, and therefore decide which ExternalSecret it lands in.
	Workloads []string `yaml:"workloads"`
}

// App is the part of environments/<env>/<app>/values.yaml this package reads.
//
// TenantID is the organisation whose box this is; AppID is the product
// (ADR-088). They were one field until 2026-09-24, which is why the path below
// read /tenants/<product>/ while the cluster segment above it already carried the
// customer.
type App struct {
	TenantID string `yaml:"tenantId"`
	AppID    string `yaml:"appId"`

	// NO ClusterName. An application is deployed to every workload cluster
	// serving its environment, so it declares no destination -- and its secrets
	// are therefore not in one place either: see SecretPathOn.
	Secrets []Declaration `yaml:"secrets"`
}

// SecretPath is where this application's values live in the provider.
//
// DERIVED from the application's own declaration, never passed in. The retired
// fleet-registry arrangement wrote this path out per key, twenty-three times,
// each one an opportunity to name a cluster the application does not run on -- which fails as "could not get secret
// data from provider", indistinguishable from a store or auth fault.
// ONE PATH PER CLUSTER, because a deployment's secrets are the cluster's.
//
// An application runs on every cluster serving its environment, and each of
// those deployments reads from its own cluster's folder -- the store on a
// managed cluster is scoped to /spoke-pool/<that cluster>/ and can reach no
// other. So supplying a value means writing it once per cluster, which is what
// the callers loop over; a single path would have written it where exactly one
// deployment could read it.
//
// The `/spoke-pool/<name>/` segment keeps its spelling: renaming it would orphan
// every secret already written, on every box.
func (f App) SecretPathOn(clusterName string) string {
	return fmt.Sprintf("/spoke-pool/%s/tenants/%s", clusterName, f.AppID)
}

// ClustersServing is every workload cluster this environment's applications are
// deployed to: the ones whose own declaration carries this environmentSlug.
//
// The placement rule, read from the clusters rather than from the application --
// which is why it lives here beside the paths derived from it. A cluster with no
// spoke-pool claim is the management cluster and is excluded, exactly as the
// boundary ApplicationSets exclude it by requiring the `spoke-type: pool` label.
func ClustersServing(repoRoot, environment string) ([]string, error) {
	clustersDir := filepath.Join(repoRoot, registryClustersDir)
	entries, err := os.ReadDir(clustersDir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", clustersDir, err)
	}
	var serving []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		claims, _ := filepath.Glob(filepath.Join(clustersDir, e.Name(), "infrastructure", "spokepool*.yaml"))
		if len(claims) == 0 {
			continue // the management cluster: applications do not run on it
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
			return nil, fmt.Errorf("parse %s/values.yaml: %w", e.Name(), err)
		}
		if decl.EnvironmentSlug == environment {
			serving = append(serving, e.Name())
		}
	}
	sort.Strings(serving)
	return serving, nil
}

// Load reads one environment's application declaration.
func Load(repoRoot, environment, app string) (*App, error) {
	path := filepath.Join(repoRoot, "environments", environment, app, "values.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("no app at %s: this command runs from the root of a "+
			"tenant's GitOps repository, and <env> names a directory under environments/: %w",
			path, err)
	}

	var f App
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	// Both halves of the path, checked here rather than producing a path with an
	// empty segment. "/spoke-pool//tenants/waypoint" is a valid string and a
	// folder that will never hold anything.
	if f.TenantID == "" {
		return nil, fmt.Errorf("%s declares no tenantId, which is half of this application's secret path", path)
	}
	// NOTHING ABOUT PLACEMENT IS CHECKED HERE ANY MORE. The application names no
	// cluster, so there is no pair to be coherent: where it runs is decided by
	// which clusters declare this environment, and ClustersServing reads that
	// when a caller needs it.
	return &f, nil
}

// registryClustersDir holds this box's cluster declarations. The authority for
// the name is tenant.RegistryDir; repeated rather than imported because that
// package carries the whole scaffolder and this one reads two fields.
const registryClustersDir = "registry/clusters"

// WHY THERE IS NO VerifyTargetCluster. It checked three facts against each
// other -- the application named a cluster, the cluster declared an
// environment, the application's directory named one -- and the first of those
// no longer exists. An application is deployed to every cluster serving its
// environment, so the only question left is which clusters those are, which is
// ClustersServing above and is not a validation.
// Declared reports whether the application declares this key, and returns it.
func (f App) Declared(name string) (Declaration, bool) {
	for _, d := range f.Secrets {
		if d.Name == name {
			return d, true
		}
	}
	return Declaration{}, false
}

// Entry is one line of a status report.
type Entry struct {
	Declaration
	// Present in the provider.
	Present bool
	// Orphan: present in the provider, declared by nothing. Left behind by a
	// rename or a rotation that wrote the new key without removing the old.
	Orphan bool
}

// Status is the join of what an application declares against what has been supplied.
type Status struct {
	App     App
	Path    string
	Entries []Entry
}

// Join builds a Status from a declaration and the key names present.
//
// Takes NAMES, not secrets. A caller that has no values cannot print one.
// The cluster is passed in, because status is per deployment: the same
// application has one folder per cluster serving its environment, and "is this
// secret supplied" has a different answer on each.
func Join(f App, clusterName string, present []string) Status {
	have := make(map[string]bool, len(present))
	for _, n := range present {
		have[n] = true
	}

	s := Status{App: f, Path: f.SecretPathOn(clusterName)}
	declared := make(map[string]bool, len(f.Secrets))
	for _, d := range f.Secrets {
		declared[d.Name] = true
		s.Entries = append(s.Entries, Entry{Declaration: d, Present: have[d.Name]})
	}

	// Orphans are reported, never removed. A key this application does not declare may
	// still be read by something else -- the platform writes OAUTH_*,
	// CACHE_PASSWORD and db-credentials to this same folder, and none of those
	// appear in an application's `secrets:` by design. Deleting what looks unclaimed
	// would take those with it.
	for _, n := range present {
		if !declared[n] {
			s.Entries = append(s.Entries, Entry{
				Declaration: Declaration{Name: n},
				Present:     true,
				Orphan:      true,
			})
		}
	}

	sort.SliceStable(s.Entries, func(i, j int) bool {
		if s.Entries[i].Orphan != s.Entries[j].Orphan {
			return !s.Entries[i].Orphan
		}
		return s.Entries[i].Name < s.Entries[j].Name
	})
	return s
}

// Missing is every declared key that has not been supplied.
func (s Status) Missing() []Entry {
	var out []Entry
	for _, e := range s.Entries {
		if !e.Orphan && !e.Present {
			out = append(out, e)
		}
	}
	return out
}

// WithheldWorkloads names the workloads that have NO secrets at all because one
// of their keys is absent, and how many keys each loses.
//
// This is the consequence a per-key list does not show. An ExternalSecret is
// atomic: one key the provider cannot supply fails the whole object, so every
// other key in it is withheld too and every container reading any of them stays
// in CreateContainerConfigError. Reporting "AI_GATEWAY_API_KEY is absent"
// understates it by a factor of however many keys that workload reads.
func (s Status) WithheldWorkloads() map[string]int {
	broken := map[string]bool{}
	for _, e := range s.Missing() {
		for _, w := range e.Workloads {
			broken[w] = true
		}
	}

	out := map[string]int{}
	for _, e := range s.Entries {
		if e.Orphan {
			continue
		}
		for _, w := range e.Workloads {
			if broken[w] {
				out[w]++
			}
		}
	}
	return out
}
