package bootstrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// One Gateway per host port, per spoke.
//
// A Cilium Gateway in hostNetwork mode (ADR-046 §8) binds a REAL host port, so
// two Gateways claiming the same port on one node is a port conflict. Envoy
// accepts whichever arrives first and NACKs the other for the life of the
// cluster:
//
//	NACK received: Error adding/updating listener(s)
//	platform-ops/cilium-gateway-tenant-tls-gateway/listener: error adding
//	listener: has duplicate address '0.0.0.0:443' as existing listener
//
// Nothing above envoy reports it, which is what makes a gate worth having.
// Both Gateways reported Programmed=True, both their routes were Accepted with
// ResolvedRefs=True, both certificates were issued and synced into
// cilium-secrets -- and the losing hostname reset every TLS ClientHello without
// a ServerHello, which reads as a broken certificate rather than a port
// conflict. It cost a tenant's entire public ingress.
//
// tenant-public-tls had stated the invariant in a comment -- "this :443 Gateway
// must never coexist with another claimant of :443" -- and ADR-083's query
// endpoint then added exactly that from the other side, because a comment in
// one chart cannot constrain another.
//
// The remedy is not one Gateway per purpose on separate ports; it is one
// Gateway per port with a listener per purpose. spec.listeners is
// `x-kubernetes-list-type: map` keyed on `name`, so under ServerSideApply
// several owners contribute listeners to one Gateway without removing each
// other's -- the Gateway API equivalent of one ingress-nginx controller merging
// many Ingress objects by host.
func TestOneGatewayPerHostPortInTheSpokeCatalog(t *testing.T) {
	// BOTH trees that put a Gateway on a spoke, and the pair is the point: the
	// original conflict had one claimant in each, so a gate walking only one of
	// them would have passed while the cluster was broken.
	base := filepath.Join("..", "..", "..", "manifests")
	roots := []string{
		filepath.Join(base, "spoke", "spoke-catalog"),
		filepath.Join(base, "tenants", "charts", "tenant-public-tls"),
	}

	// tenant-public-tls names its Gateway through a value. Resolved from the
	// chart's own defaults so the comparison is against the name that actually
	// lands, not against the template text.
	tlsGatewayName := chartDefault(t,
		filepath.Join(base, "tenants", "charts", "tenant-public-tls", "values.yaml"),
		"tlsGatewayName")

	claimants := map[int64][]string{}   // port -> gateway names
	listeners := map[string][]string{}  // gateway -> listener names

	walk := func(root string) error {
		return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		// probes/ is never built into any overlay -- it holds a DELIBERATE
		// same-port clash that proved this very behaviour on this spoke's
		// Cilium version. Walking it would make the gate fail on the
		// experiment that justifies it.
		if info.IsDir() {
			if info.Name() == "probes" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".yaml") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		dec := yaml.NewDecoder(strings.NewReader(stripTemplating(string(raw))))
		for {
			var doc struct {
				Kind     string `yaml:"kind"`
				Metadata struct {
					Name string `yaml:"name"`
				} `yaml:"metadata"`
				Spec struct {
					Listeners []struct {
						Name string `yaml:"name"`
						Port int64  `yaml:"port"`
					} `yaml:"listeners"`
				} `yaml:"spec"`
			}
			if err := dec.Decode(&doc); err != nil {
				break
			}
			if doc.Kind != "Gateway" {
				continue
			}
			name := doc.Metadata.Name
			if strings.Contains(name, "TEMPLATED") {
				name = tlsGatewayName
			}
			for _, l := range doc.Spec.Listeners {
				if !gatewayListed(claimants[l.Port], name) {
					claimants[l.Port] = append(claimants[l.Port], name)
				}
				listeners[name] = append(listeners[name], l.Name)
			}
		}
			return nil
		})
	}
	for _, r := range roots {
		if err := walk(r); err != nil {
			t.Fatal(err)
		}
	}
	if len(claimants) == 0 {
		t.Fatal("no Gateways found in the spoke catalog: the walk has drifted, and an " +
			"empty result would make this gate vacuous")
	}

	for port, gws := range claimants {
		if len(gws) > 1 {
			t.Errorf("port %d is claimed by %d Gateways %v: a hostNetwork Cilium Gateway "+
				"binds a real host port, so envoy NACKs all but the first and the losing "+
				"hostname resets every connection while reporting Programmed=True. "+
				"Use ONE Gateway on this port with a listener per purpose.", port, len(gws), gws)
		}
	}

	// Listener names are the ServerSideApply merge key. A duplicate does not
	// error -- it silently replaces, which is the same outage by another route.
	for gw, ls := range listeners {
		seen := map[string]bool{}
		for _, l := range ls {
			if seen[l] {
				t.Errorf("Gateway %q declares listener %q twice: the name is the "+
					"ServerSideApply merge key, so one silently replaces the other", gw, l)
			}
			seen[l] = true
		}
	}
}

// stripTemplating makes a Helm template parseable as YAML.
//
// Required, not cosmetic: tenant-public-tls's Gateway IS a Helm template, so a
// plain YAML decode fails on it and the file is skipped silently -- which is
// exactly how a first version of this gate passed while both :443 claimants
// were present. Whole-line template directives are dropped and inline
// expressions become a placeholder; what survives is the structure this gate
// reads, the kind, the name and the ports.
func stripTemplating(in string) string {
	var out []string
	for _, line := range strings.Split(in, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "{{") && strings.HasSuffix(t, "}}") {
			continue
		}
		for {
			i := strings.Index(line, "{{")
			if i < 0 {
				break
			}
			j := strings.Index(line[i:], "}}")
			if j < 0 {
				break
			}
			line = line[:i] + "TEMPLATED" + line[i+j+2:]
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// chartDefault reads one top-level scalar from a chart's values.yaml.
func chartDefault(t *testing.T, path, key string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var vals map[string]any
	if err := yaml.Unmarshal(raw, &vals); err != nil {
		t.Fatal(err)
	}
	v, ok := vals[key].(string)
	if !ok || v == "" {
		t.Fatalf("%s does not define %s: the Gateway name cannot be resolved, so this "+
			"gate could not compare it", path, key)
	}
	return v
}

func gatewayListed(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
