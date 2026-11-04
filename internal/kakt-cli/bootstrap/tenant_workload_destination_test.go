package bootstrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Where a tenant workload may land, checked against ArgoCD's own matching rules.
//
// The AppProject allowlist is the only thing standing between a tenant's
// Application and the management cluster, and it fails silently in both
// directions: too narrow and the Application sits at Unknown/Unknown reporting
// a destination that "does not match any of the allowed destinations", which
// reads as a misconfigured Application; too wide and tenant code runs beside
// etcd with nothing reporting anything at all.
//
// Two defects this encodes, both real:
//
//  1. The list allowed `name: 'spoke-pool-*'`, a cluster-naming convention
//     ADR-082 withdrew when it made a workload cluster's name the tenant's to
//     choose. Every tenant workload on a box whose cluster is named anything
//     else was refused.
//
//  2. The first correction wrote the deny entries with `namespace: '*'`. ArgoCD
//     evaluates an ALLOW as (name AND namespace) but a deny-by-name
//     independently of the namespace, so those entries denied the management
//     cluster and simultaneously allowed every OTHER cluster in EVERY
//     namespace -- kube-system included. Strictly worse than the bug it fixed,
//     and nothing would have reported it.
func TestTenantWorkloadDestinationsPermitOnlyTenantNamespacesOffTheManagementCluster(t *testing.T) {
	path := filepath.Join("..", "..", "..", "manifests", "argocd", "environment-manager",
		"templates", "tenant-workloads-appproject.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	dests := parseDestinations(t, string(raw))
	if len(dests) == 0 {
		t.Fatal("no destinations parsed: the allowlist is the control, so an empty one is a failure")
	}

	// The management cluster's name is templated. Substitute what this box calls
	// it; the assertions below are about the SHAPE, which is what regresses.
	for i := range dests {
		if strings.Contains(dests[i].name, "{{") {
			dests[i].name = "!mgmt-cluster"
		}
	}

	for _, tc := range []struct {
		what    string
		cluster string
		ns      string
		want    bool
	}{
		{"a workload cluster, tenant namespace", "workload-01", "tenant-acme", true},
		{"the management cluster by name", "mgmt-cluster", "tenant-acme", false},
		{"the management cluster as in-cluster", "in-cluster", "tenant-acme", false},
		{"a workload cluster, kube-system", "workload-01", "kube-system", false},
		{"a workload cluster, a platform namespace", "workload-01", "platform-ops", false},
	} {
		if got := destinationPermitted(dests, tc.cluster, tc.ns); got != tc.want {
			t.Errorf("%s: permitted=%v, want %v", tc.what, got, tc.want)
		}
	}
}

type projectDestination struct{ namespace, name, server string }

// parseDestinations reads the `destinations:` block. Hand-rolled rather than
// through a YAML parser because the file is a Helm template and does not parse
// as YAML until it is rendered.
func parseDestinations(t *testing.T, s string) []projectDestination {
	t.Helper()
	lines := strings.Split(s, "\n")
	var out []projectDestination
	in := false
	for _, l := range lines {
		trimmed := strings.TrimSpace(l)
		switch {
		case trimmed == "destinations:":
			in = true
			continue
		case !in:
			continue
		case strings.HasPrefix(trimmed, "#") || trimmed == "":
			continue
		case !strings.HasPrefix(l, "    ") && trimmed != "":
			in = false // dedented out of the block
			continue
		}
		if strings.HasPrefix(trimmed, "- ") {
			out = append(out, projectDestination{})
			trimmed = strings.TrimPrefix(trimmed, "- ")
		}
		if len(out) == 0 {
			continue
		}
		k, v, ok := strings.Cut(trimmed, ":")
		if !ok {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `'"`)
		switch strings.TrimSpace(k) {
		case "namespace":
			out[len(out)-1].namespace = v
		case "name":
			out[len(out)-1].name = v
		case "server":
			out[len(out)-1].server = v
		}
	}
	return out
}

// destinationPermitted reproduces ArgoCD's isDestinationMatched
// (pkg/apis/application/v1alpha1/app_project_types.go, v2.14.11). Reproduced
// rather than approximated: the deny clause's asymmetry -- a deny by name
// ignores the namespace, an allow does not -- is the whole reason defect 2
// above was not obvious by reading.
func destinationPermitted(dests []projectDestination, cluster, ns string) bool {
	any := false
	for _, d := range dests {
		nameMatched := cluster != "" && globMatchNeg(d.name, cluster)
		serverMatched := false // destinations here are named, never server-addressed
		nsMatched := globMatchNeg(d.namespace, ns)

		if (serverMatched || nameMatched) && nsMatched {
			any = true
		} else if (!nameMatched && strings.HasPrefix(d.name, "!")) ||
			((!serverMatched && strings.HasPrefix(d.server, "!")) && nsMatched) {
			return false
		} else if !nsMatched && strings.HasPrefix(d.namespace, "!") && serverMatched {
			return false
		}
	}
	return any
}

func globMatchNeg(pattern, val string) bool {
	if strings.HasPrefix(pattern, "!") {
		return !globLike(pattern[1:], val)
	}
	if pattern == "*" {
		return true
	}
	return globLike(pattern, val)
}

// globLike covers the two forms these patterns use: an exact name, and a
// trailing-* prefix.
func globLike(pattern, val string) bool {
	if pattern == "*" {
		return true
	}
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(val, strings.TrimSuffix(pattern, "*"))
	}
	return pattern == val
}
