// Package support implements the bounded evidence collection ADR-077 describes.
//
// The privacy boundary here is the allowlist, not anonymisation. Hashing a
// cluster name leaves identifying material in labels, image references, node
// names, annotations, addresses and error strings, so what is bounded has to be
// what is GATHERED rather than how it is disguised. Two levels enforce that:
//
//	collectors  where evidence comes from, and the schema of what comes back.
//	            Each runs a defined query whose result shape is known in
//	            advance. An agent that issued a caller-supplied query and
//	            shipped the response would have an allowlist filtering labels
//	            over an unbounded payload, which is a preference and not a
//	            boundary.
//	emit        which fields of that known schema leave the cluster. Default
//	            exclusion: a field nobody listed is a field that does not go.
//
// The tenant may subtract further. Effective scope is `allowlist − denylist`,
// computed in the cluster, so the platform proposes and can never silently add.
package support

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// SourceKind distinguishes where a collector reads from. Both are platform-owned
// interfaces; neither is the tenant's observability backend, which ADR-078 §8
// keeps independent of this path.
type SourceKind string

const (
	// KindMetrics scrapes a platform component's own /metrics endpoint.
	KindMetrics SourceKind = "metrics"
	// KindArtefact reads a file the platform itself renders, such as a
	// cluster's bundle.yaml.
	KindArtefact SourceKind = "artefact"
)

// Source is where one collector reads.
type Source struct {
	Kind      SourceKind `yaml:"kind"`
	Namespace string     `yaml:"namespace"`
	Service   string     `yaml:"service"`
	// Port the component serves /metrics on. Required for a metrics source.
	//
	// Per collector, because exporters do not agree on a port: kube-state-metrics
	// serves 8080 and a CloudNativePG instance serves 9187. One port for the whole
	// agent could only ever be right for some of them, and the collectors it was wrong
	// for failed into the payload's Skipped map -- which reads as a component with
	// nothing to report rather than a collector pointed at the wrong port.
	Port  int    `yaml:"port"`
	Query string `yaml:"query"`
	Path  string `yaml:"path"`
}

// Collector is one allowlist entry.
type Collector struct {
	Name   string   `yaml:"collector"`
	Source Source   `yaml:"source"`
	Schema []string `yaml:"schema"`
	Emit   []string `yaml:"emit"`
}

// Allowlist is the parsed contract.
type Allowlist struct {
	Collectors []Collector
}

// Parse reads the allowlist document -- the body of the ConfigMap's
// allowlist.yaml key, not the ConfigMap itself.
//
// Every entry is validated on the way in. A malformed allowlist must not load
// as a permissive one: an entry whose emit set escapes its schema would emit a
// field no reviewer ever agreed to, and the document is the contract precisely
// because someone reads it.
func Parse(doc []byte) (*Allowlist, error) {
	var raw []Collector
	if err := yaml.Unmarshal(doc, &raw); err != nil {
		return nil, fmt.Errorf("allowlist is not a list of collectors: %w", err)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("allowlist declares no collectors; an agent with " +
			"an empty allowlist emits nothing, which is safe, but an allowlist " +
			"that failed to parse looks exactly the same")
	}

	seen := map[string]bool{}
	for i, c := range raw {
		switch {
		case c.Name == "":
			return nil, fmt.Errorf("collector %d has no name", i)
		case seen[c.Name]:
			return nil, fmt.Errorf("collector %q is declared twice; one of the "+
				"two emit sets would silently win", c.Name)
		case c.Source.Kind != KindMetrics && c.Source.Kind != KindArtefact:
			return nil, fmt.Errorf("collector %q has source kind %q; a kind this "+
				"agent does not implement would be skipped at collection time "+
				"and read as a collector that found nothing", c.Name, c.Source.Kind)
		case len(c.Schema) == 0:
			return nil, fmt.Errorf("collector %q declares no schema, so what "+
				"comes back is unbounded and `emit` would be filtering it rather "+
				"than bounding it", c.Name)
		}
		seen[c.Name] = true

		inSchema := map[string]bool{}
		for _, f := range c.Schema {
			inSchema[f] = true
		}
		for _, f := range c.Emit {
			if !inSchema[f] {
				return nil, fmt.Errorf("collector %q emits %q, which is not in "+
					"its schema. Either the field is real and the schema is "+
					"wrong, or it is not and this emits nothing under a name "+
					"that looks reviewed", c.Name, f)
			}
		}
		if c.Source.Kind == KindMetrics && c.Source.Port <= 0 {
			return nil, fmt.Errorf("collector %q reads metrics and names no port; "+
				"exporters do not share one, and a scrape of the wrong port is "+
				"recorded as a collector that found nothing", c.Name)
		}
		if c.Source.Kind == KindMetrics && c.Source.Query == "" {
			return nil, fmt.Errorf("collector %q reads metrics and names no "+
				"query; the agent would have to choose what to ask for", c.Name)
		}
	}
	return &Allowlist{Collectors: raw}, nil
}

// Effective applies the tenant's denylist. ADR-077: effective scope is
// `allowlist − denylist`, and no value can ADD a field.
//
// Entries are dropped rather than errored when a denylist names something not
// present: a tenant subtracting a field the platform later removed should not
// have their agent stop.
func (a *Allowlist) Effective(deny []string) *Allowlist {
	denied := map[string]bool{}
	for _, d := range deny {
		denied[strings.TrimSpace(d)] = true
	}
	out := &Allowlist{}
	for _, c := range a.Collectors {
		if denied[c.Name] {
			continue
		}
		kept := c
		kept.Emit = nil
		for _, f := range c.Emit {
			if denied[f] || denied[c.Name+"."+f] {
				continue
			}
			kept.Emit = append(kept.Emit, f)
		}
		// A collector with nothing left to emit is not collected at all. Running
		// the query and discarding the answer would read the tenant's cluster
		// for no purpose.
		if len(kept.Emit) == 0 {
			continue
		}
		out.Collectors = append(out.Collectors, kept)
	}
	return out
}

// Project reduces one observed record to what may leave.
//
// The only way a field reaches a payload. It builds a new map rather than
// deleting from the observed one: deletion leaves anything the caller forgot to
// name, and "what did we forget" is the question this design refuses to depend
// on anyone answering correctly.
func (c Collector) Project(observed map[string]string) map[string]string {
	out := make(map[string]string, len(c.Emit))
	for _, f := range c.Emit {
		if v, ok := observed[f]; ok {
			out[f] = v
		}
	}
	return out
}

// Fields lists every field that may leave, across all collectors, sorted.
// Used by `soloz support preview` and by the release gate.
func (a *Allowlist) Fields() []string {
	seen := map[string]bool{}
	for _, c := range a.Collectors {
		for _, f := range c.Emit {
			seen[c.Name+"."+f] = true
		}
	}
	out := make([]string, 0, len(seen))
	for f := range seen {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}
