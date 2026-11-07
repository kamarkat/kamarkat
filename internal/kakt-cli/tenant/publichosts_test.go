package tenant

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// ADR-096: the aggregate is the shared Gateway's listener list, and it has one
// producer. These tests pin the properties the Gateway depends on.

func writeApp(t *testing.T, root, env, app, body string) {
	t.Helper()
	dir := filepath.Join(root, "environments", env, app)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "values.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// spokeDir declares a workload cluster: the directory the aggregate is written
// into, plus the two facts VerifyCell reads -- the environment it serves and the
// spoke-pool claim that tells it from the management cluster.
//
// Defaults to dev, because most of these tests are about grouping rather than
// about environments, and because two spokes serving ONE environment is a
// capability (ADR-082) rather than a case to avoid.
func spokeDir(t *testing.T, root, name string) {
	t.Helper()
	spokeDirIn(t, root, name, "dev")
}

func spokeDirIn(t *testing.T, root, name, environment string) {
	spokeDirServing(t, root, name, environment, false)
}

// spokeDirServing declares a workload cluster, optionally as the one serving the
// environment's public hostnames.
func spokeDirServing(t *testing.T, root, name, environment string, ingress bool) {
	t.Helper()
	dir := filepath.Join(root, RegistryDir, "clusters", name)
	if err := os.MkdirAll(filepath.Join(dir, "infrastructure"), 0o755); err != nil {
		t.Fatal(err)
	}
	values := "clusterName: " + name + "\nenvironmentSlug: " + environment + "\n"
	if ingress {
		values += "publicIngress: true\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "values.yaml"), []byte(values), 0o644); err != nil {
		t.Fatal(err)
	}
	// The hydrated name, not the template's: see writeWorkloadCluster in the
	// fleet package.
	claim := filepath.Join(dir, "infrastructure", "spokepool.yaml")
	if err := os.WriteFile(claim, []byte("kind: SpokePool\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readAggregate(t *testing.T, root, spoke string) publicHostsDoc {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, RegistryDir, "clusters", spoke, PublicHostsFile))
	if err != nil {
		t.Fatalf("no aggregate for %s: %v", spoke, err)
	}
	var doc publicHostsDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

// Every application in an environment runs on every cluster serving it, but the
// environment's public hostnames land on ONE of them.
//
// This replaces a test that grouped listeners by each application's clusterName.
// That field is gone: placement is the environment, and hostname ownership is
// exclusive because two Gateways claiming one host would each order a
// certificate for it and each publish the same DNS record.
func TestAggregatePutsAnEnvironmentsHostsOnItsIngressCluster(t *testing.T) {
	root := t.TempDir()
	spokeDirServing(t, root, "spoke-a", "dev", true)
	spokeDirServing(t, root, "spoke-b", "dev", false)
	writeApp(t, root, "dev", "waypoint", "tenantId: nutgraf\nappId: waypoint\npublic:\n  hosts: [waypoint.example.test]\n")
	writeApp(t, root, "dev", "oranger", "tenantId: nutgraf\nappId: oranger\npublic:\n  hosts: [oranger.example.test]\n")

	if _, err := AggregatePublicHosts(root); err != nil {
		t.Fatalf("aggregate: %v", err)
	}

	a := readAggregate(t, root, "spoke-a")
	if len(a.PublicHosts) != 2 {
		t.Fatalf("the ingress cluster should carry both listeners, got %d", len(a.PublicHosts))
	}
	// Sorted, so a re-run is byte-identical and does not show up as a diff every
	// time an unrelated app is added.
	if a.PublicHosts[0].Host != "oranger.example.test" || a.PublicHosts[1].Host != "waypoint.example.test" {
		t.Errorf("not sorted: %+v", a.PublicHosts)
	}
	// The Secret name must match what tenant-public-tls' Certificate writes.
	if a.PublicHosts[1].TLSSecret != "nutgraf-waypoint-public-tls" {
		t.Errorf("tlsSecret = %q, want nutgraf-waypoint-public-tls", a.PublicHosts[1].TLSSecret)
	}
	// The other cluster runs the same applications and claims none of the
	// hostnames.
	if b := readAggregate(t, root, "spoke-b"); len(b.PublicHosts) != 0 {
		t.Errorf("a cluster that is not the ingress claimed %d listener(s)", len(b.PublicHosts))
	}
}

// An environment with one cluster needs no declaration: that cluster is the one.
func TestAggregateNeedsNoIngressFlagForASingleClusterEnvironment(t *testing.T) {
	root := t.TempDir()
	spokeDirIn(t, root, "only", "dev")
	writeApp(t, root, "dev", "waypoint", "tenantId: nutgraf\nappId: waypoint\npublic:\n  hosts: [waypoint.example.test]\n")
	if _, err := AggregatePublicHosts(root); err != nil {
		t.Fatalf("a single-cluster environment required a publicIngress flag: %v", err)
	}
	if got := readAggregate(t, root, "only"); len(got.PublicHosts) != 1 {
		t.Errorf("want 1 listener on the only cluster, got %d", len(got.PublicHosts))
	}
}

// Ambiguity is refused, not resolved by picking one.
func TestAggregateRefusesAmbiguousOrAbsentIngress(t *testing.T) {
	for _, c := range []struct {
		why     string
		ingress []bool
		expect  string
	}{
		{"two clusters, neither claims the hostnames", []bool{false, false}, "none declares publicIngress"},
		{"two clusters, both claim them", []bool{true, true}, "declare publicIngress: true"},
	} {
		root := t.TempDir()
		spokeDirServing(t, root, "spoke-a", "dev", c.ingress[0])
		spokeDirServing(t, root, "spoke-b", "dev", c.ingress[1])
		writeApp(t, root, "dev", "waypoint", "tenantId: nutgraf\nappId: waypoint\npublic:\n  hosts: [waypoint.example.test]\n")

		_, err := AggregatePublicHosts(root)
		if err == nil {
			t.Errorf("%s: accepted", c.why)
			continue
		}
		if !strings.Contains(err.Error(), c.expect) {
			t.Errorf("%s: error does not say %q: %v", c.why, c.expect, err)
		}
	}
}

func TestAggregateWritesAnEmptyFileForASpokeWithNoPublicApp(t *testing.T) {
	// Not "writes nothing". An empty aggregate is what makes REMOVING the last
	// hostname converge; skipping the file would leave the previous listener
	// list in place forever.
	root := t.TempDir()
	spokeDir(t, root, "spoke-a")
	writeApp(t, root, "dev", "internal", "tenantId: nutgraf\nappId: internal\nclusterName: spoke-a\npublic:\n  hosts: []\n")

	if _, err := AggregatePublicHosts(root); err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if got := readAggregate(t, root, "spoke-a"); len(got.PublicHosts) != 0 {
		t.Errorf("want no listeners, got %+v", got.PublicHosts)
	}
}

func TestAggregateRefusesAHostnameClaimedTwice(t *testing.T) {
	// One listener, two Certificates racing for it. The producer must not pick.
	root := t.TempDir()
	spokeDir(t, root, "spoke-a")
	writeApp(t, root, "dev", "waypoint", "tenantId: nutgraf\nappId: waypoint\nclusterName: spoke-a\npublic:\n  hosts: [shared.example.test]\n")
	writeApp(t, root, "dev", "oranger", "tenantId: nutgraf\nappId: oranger\nclusterName: spoke-a\npublic:\n  hosts: [shared.example.test]\n")

	_, err := AggregatePublicHosts(root)
	if err == nil {
		t.Fatal("expected a refusal for a duplicated hostname")
	}
	if !strings.Contains(err.Error(), "one owner") {
		t.Errorf("error should say a hostname has one owner, got: %v", err)
	}
}

// An application with public hosts in an environment no cluster serves is
// refused, because the listener would be written nowhere and the hostname would
// simply not resolve.
//
// This replaces a test that refused an application with no clusterName. The
// field is gone; what can still be missing is a cluster serving the environment.
func TestAggregateRefusesPublicHostsInAnEnvironmentNoClusterServes(t *testing.T) {
	root := t.TempDir()
	spokeDirIn(t, root, "spoke-dev", "dev")
	writeApp(t, root, "stg", "waypoint", "tenantId: nutgraf\nappId: waypoint\npublic:\n  hosts: [w.example.test]\n")

	_, err := AggregatePublicHosts(root)
	if err == nil {
		t.Fatal("public hosts were accepted in an environment with no cluster")
	}
	for _, want := range []string{"no workload cluster serves", "add-cluster --environment stg"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
}

func TestAggregateIsDeterministic(t *testing.T) {
	root := t.TempDir()
	spokeDir(t, root, "spoke-a")
	writeApp(t, root, "dev", "b", "tenantId: t\nappId: b\nclusterName: spoke-a\npublic:\n  hosts: [b.example.test]\n")
	writeApp(t, root, "dev", "a", "tenantId: t\nappId: a\nclusterName: spoke-a\npublic:\n  hosts: [a.example.test]\n")

	if _, err := AggregatePublicHosts(root); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(filepath.Join(root, RegistryDir, "clusters", "spoke-a", PublicHostsFile))
	if _, err := AggregatePublicHosts(root); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(filepath.Join(root, RegistryDir, "clusters", "spoke-a", PublicHostsFile))
	if string(first) != string(second) {
		t.Error("re-running the aggregation changed the file; it would show as a diff on every run")
	}
}

// WHY THERE IS NO TEST FOR "a cluster serving another environment".
//
// It asserted that an application's clusterName had to name a cluster serving
// the application's own environment. Neither half of that pair exists now: the
// application names no cluster, and the aggregation reads which clusters serve
// the environment rather than checking a claim about one.
