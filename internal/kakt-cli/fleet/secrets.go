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
// read /tenants/<product>/ while the cell segment above it already carried the
// customer.
type Fleet struct {
	TenantID string        `yaml:"tenantId"`
	AppID    string        `yaml:"appId"`
	CellID   string        `yaml:"cellId"`
	Secrets  []Declaration `yaml:"secrets"`
}

// SecretPath is where this fleet's values live in the provider.
//
// DERIVED from the fleet's own declaration, never passed in. The legacy fleet
// wrote this path out per key, twenty-three times, each one an opportunity to
// name a cell the fleet does not run on -- which fails as "could not get secret
// data from provider", indistinguishable from a store or auth fault.
func (f Fleet) SecretPath() string {
	return fmt.Sprintf("/spoke-pool/%s/tenants/%s", f.CellID, f.AppID)
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
	if f.CellID == "" {
		return nil, fmt.Errorf("%s declares no cellId, which is half of this fleet's secret path", path)
	}
	return &f, nil
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
