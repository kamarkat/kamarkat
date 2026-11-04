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

func spokeDir(t *testing.T, root, name string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, RegistryDir, "clusters", name), 0o755); err != nil {
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

func TestAggregateGroupsHostsBySpoke(t *testing.T) {
	root := t.TempDir()
	spokeDir(t, root, "spoke-a")
	spokeDir(t, root, "spoke-b")
	writeApp(t, root, "dev", "waypoint", "tenantId: nutgraf\nappId: waypoint\ncellId: spoke-a\npublic:\n  hosts: [waypoint.example.test]\n")
	writeApp(t, root, "dev", "oranger", "tenantId: nutgraf\nappId: oranger\ncellId: spoke-a\npublic:\n  hosts: [oranger.example.test]\n")
	writeApp(t, root, "dev", "other", "tenantId: acme\nappId: other\ncellId: spoke-b\npublic:\n  hosts: [other.example.test]\n")

	if _, err := AggregatePublicHosts(root); err != nil {
		t.Fatalf("aggregate: %v", err)
	}

	a := readAggregate(t, root, "spoke-a")
	if len(a.PublicHosts) != 2 {
		t.Fatalf("spoke-a: want 2 listeners, got %d", len(a.PublicHosts))
	}
	// Sorted, so a re-run produces a byte-identical file and does not show up as
	// a diff every time an unrelated app is added.
	if a.PublicHosts[0].Host != "oranger.example.test" || a.PublicHosts[1].Host != "waypoint.example.test" {
		t.Errorf("not sorted: %+v", a.PublicHosts)
	}
	// The Secret name must match what tenant-public-tls' Certificate writes.
	if a.PublicHosts[1].TLSSecret != "nutgraf-waypoint-public-tls" {
		t.Errorf("tlsSecret = %q, want nutgraf-waypoint-public-tls", a.PublicHosts[1].TLSSecret)
	}
	if b := readAggregate(t, root, "spoke-b"); len(b.PublicHosts) != 1 {
		t.Errorf("spoke-b: want 1 listener, got %d", len(b.PublicHosts))
	}
}

func TestAggregateWritesAnEmptyFileForASpokeWithNoPublicApp(t *testing.T) {
	// Not "writes nothing". An empty aggregate is what makes REMOVING the last
	// hostname converge; skipping the file would leave the previous listener
	// list in place forever.
	root := t.TempDir()
	spokeDir(t, root, "spoke-a")
	writeApp(t, root, "dev", "internal", "tenantId: nutgraf\nappId: internal\ncellId: spoke-a\npublic:\n  hosts: []\n")

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
	writeApp(t, root, "dev", "waypoint", "tenantId: nutgraf\nappId: waypoint\ncellId: spoke-a\npublic:\n  hosts: [shared.example.test]\n")
	writeApp(t, root, "dev", "oranger", "tenantId: nutgraf\nappId: oranger\ncellId: spoke-a\npublic:\n  hosts: [shared.example.test]\n")

	_, err := AggregatePublicHosts(root)
	if err == nil {
		t.Fatal("expected a refusal for a duplicated hostname")
	}
	if !strings.Contains(err.Error(), "one owner") {
		t.Errorf("error should say a hostname has one owner, got: %v", err)
	}
}

func TestAggregateRefusesAPublicAppWithNoSpoke(t *testing.T) {
	// Without cellId there is no Gateway to add the listener to, and writing it
	// nowhere would present as a hostname that simply does not resolve.
	root := t.TempDir()
	spokeDir(t, root, "spoke-a")
	writeApp(t, root, "dev", "waypoint", "tenantId: nutgraf\nappId: waypoint\npublic:\n  hosts: [w.example.test]\n")

	if _, err := AggregatePublicHosts(root); err == nil || !strings.Contains(err.Error(), "cellId") {
		t.Fatalf("expected a refusal naming cellId, got: %v", err)
	}
}

func TestAggregateIsDeterministic(t *testing.T) {
	root := t.TempDir()
	spokeDir(t, root, "spoke-a")
	writeApp(t, root, "dev", "b", "tenantId: t\nappId: b\ncellId: spoke-a\npublic:\n  hosts: [b.example.test]\n")
	writeApp(t, root, "dev", "a", "tenantId: t\nappId: a\ncellId: spoke-a\npublic:\n  hosts: [a.example.test]\n")

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
