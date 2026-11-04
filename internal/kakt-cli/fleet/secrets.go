// Package fleet reads what a fleet declares and reports what has been supplied.
//
// ADR-087 splits a fleet's runtime input by who owns the value. The secrets
// half is declared in the fleet's own repository and supplied out of band, and
// this package is the join between those two halves: the declaration, from git,
// and the key names present in the provider.
//
// Nothing here handles a secret VALUE. Supplying one is a write the caller
// performs; reporting on one is done by name. That is a property of the types,
// not a convention -- see infisical.ListSecretNames.
package fleet

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"
)

// Declaration is one entry in a fleet's `secrets:` list.
type Declaration struct {
	// Name is the key, identical in the provider and in the delivered Secret.
	Name string `yaml:"name"`
	// Capability is what the fleet loses without it, and is what a report names
	// -- a key alone is not something an operator can act on.
	Capability string `yaml:"capability"`
	// Workloads read it, and therefore decide which ExternalSecret it lands in.
	Workloads []string `yaml:"workloads"`
}

// Fleet is the part of environments/<env>/<app>/values.yaml this package reads.
//
// TenantID is the organisation whose box this is; AppID is the product
// (ADR-088). They were one field until 2026-09-24, which is why the path below
// read /tenants/<product>/ while the cluster segment above it already carried the
// customer.
type Fleet struct {
	TenantID string `yaml:"tenantId"`
	AppID    string `yaml:"appId"`

	// ClusterName is the managed cluster this application runs on. It was
	// `cellId`, which named a cluster in the vocabulary of policy groups: the
	// label it selects on has always been patched from the cluster's own name,
	// so placement was already by cluster identity under a key that said
	// otherwise. A cell is the policy group of ADR-064 and groups nothing here.
	ClusterName string        `yaml:"clusterName"`
	Secrets     []Declaration `yaml:"secrets"`
}

// SecretPath is where this fleet's values live in the provider.
//
// DERIVED from the fleet's own declaration, never passed in. The legacy fleet
// wrote this path out per key, twenty-three times, each one an opportunity to
// name a cluster the fleet does not run on -- which fails as "could not get secret
// data from provider", indistinguishable from a store or auth fault.
func (f Fleet) SecretPath() string {
	// The `/spoke-pool/<name>/` segment keeps its spelling. The value in it has
	// always been the cluster's name, so renaming the field that supplies it
	// moves no secret; renaming the segment would orphan every one already
	// written, on every box.
	return fmt.Sprintf("/spoke-pool/%s/tenants/%s", f.ClusterName, f.AppID)
}

// Load reads one environment's fleet declaration.
func Load(repoRoot, environment, app string) (*Fleet, error) {
	path := filepath.Join(repoRoot, "environments", environment, app, "values.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("no app at %s: this command runs from the root of a "+
			"tenant's GitOps repository, and <env> names a directory under environments/: %w",
			path, err)
	}

	var f Fleet
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	// Both halves of the path, checked here rather than producing a path with an
	// empty segment. "/spoke-pool//tenants/waypoint" is a valid string and a
	// folder that will never hold anything.
	if f.TenantID == "" {
		return nil, fmt.Errorf("%s declares no tenantId, which is half of this fleet's secret path", path)
	}
	if f.ClusterName == "" {
		return nil, fmt.Errorf("%s declares no clusterName, which is half of this fleet's secret path", path)
	}
	if err := VerifyTargetCluster(repoRoot, environment, f.ClusterName); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &f, nil
}

// registryClustersDir holds this box's cluster declarations. The authority for
// the name is tenant.RegistryDir; repeated rather than imported because that
// package carries the whole scaffolder and this one reads two fields.
const registryClustersDir = "registry/clusters"

// VerifyTargetCluster checks that the cluster an application names is somewhere
// it can run.
//
// Three independent facts, and this is the only place they meet: the
// application names a cluster, that cluster declares the environment it serves,
// and the application's own directory names the environment it belongs to.
// Nothing derives any of them from the others -- several clusters may serve one
// environment (ADR-082), and during a replacement two deliberately do (ADR-111),
// so an environment does not name a cluster and a cluster does not claim an
// environment's applications.
//
// What is checked is the pair being coherent:
//
//   - the cluster exists at all. A name with no cluster behind it produces a
//     clusters generator that matches nothing, which ArgoCD reports as Healthy
//     while generating zero Applications -- indistinguishable from an
//     environment nobody has deployed to yet.
//   - it is a workload cluster. A management cluster's directory holds root.yaml
//     and no spoke-pool claim; a tenant's secrets scoped under the control plane
//     would be a boundary violation that nothing downstream would notice.
//   - it serves this application's environment. environments/stg/app naming a
//     dev cluster is a staging application running on development capacity, with
//     its secrets in the development scope, and both halves look correct in
//     isolation.
func VerifyTargetCluster(repoRoot, environment, clusterName string) error {
	dir := filepath.Join(repoRoot, registryClustersDir, clusterName)
	declaration := filepath.Join(dir, "values.yaml")

	raw, err := os.ReadFile(declaration)
	if err != nil {
		return fmt.Errorf("declares clusterName %q, and this box has no such cluster at %s/%s: "+
			"an application placed on a cluster that does not exist generates no Application "+
			"and reports nothing", clusterName, registryClustersDir, clusterName)
	}

	// spokepool*.yaml, not spokepool-*.yaml. The TEMPLATE ships one claim per
	// provider -- spokepool-hetzner.yaml, spokepool-hybrid.yaml -- but
	// adjustDriver keeps the one in use and renames it to spokepool.yaml
	// (workload_cluster.go), so that is what a hydrated cluster actually holds.
	// Matching only the template's spelling matched no real cluster at all, and
	// every application would have been refused as targeting the management
	// cluster. nutgraf-01 is the counter-example: registry/clusters/nutgraf-01/
	// infrastructure/spokepool.yaml.
	claims, err := filepath.Glob(filepath.Join(dir, "infrastructure", "spokepool*.yaml"))
	if err != nil {
		return fmt.Errorf("read %s: %w", dir, err)
	}
	if len(claims) == 0 {
		return fmt.Errorf("declares clusterName %q, which is not a workload cluster: it has no "+
			"spoke-pool claim, so it is this box's management cluster and applications do not "+
			"run on it", clusterName)
	}

	var cluster struct {
		EnvironmentSlug string `yaml:"environmentSlug"`
	}
	if err := yaml.Unmarshal(raw, &cluster); err != nil {
		return fmt.Errorf("parse %s: %w", declaration, err)
	}
	if cluster.EnvironmentSlug != environment {
		return fmt.Errorf("declares clusterName %q, which serves environmentSlug %q and not %q: "+
			"the cluster and the directory disagree about which stage this application is, "+
			"and each is correct on its own",
			clusterName, cluster.EnvironmentSlug, environment)
	}
	return nil
}

// Declared reports whether the fleet declares this key, and returns it.
func (f Fleet) Declared(name string) (Declaration, bool) {
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

// Status is the join of what a fleet declares against what has been supplied.
type Status struct {
	Fleet   Fleet
	Path    string
	Entries []Entry
}

// Join builds a Status from a declaration and the key names present.
//
// Takes NAMES, not secrets. A caller that has no values cannot print one.
func Join(f Fleet, present []string) Status {
	have := make(map[string]bool, len(present))
	for _, n := range present {
		have[n] = true
	}

	s := Status{Fleet: f, Path: f.SecretPath()}
	declared := make(map[string]bool, len(f.Secrets))
	for _, d := range f.Secrets {
		declared[d.Name] = true
		s.Entries = append(s.Entries, Entry{Declaration: d, Present: have[d.Name]})
	}

	// Orphans are reported, never removed. A key this fleet does not declare may
	// still be read by something else -- the platform writes OAUTH_*,
	// CACHE_PASSWORD and db-credentials to this same folder, and none of those
	// appear in a fleet's `secrets:` by design. Deleting what looks unclaimed
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
