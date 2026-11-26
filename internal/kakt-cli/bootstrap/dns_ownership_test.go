package bootstrap

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHubHostnamesComeFromTheGatewayChart(t *testing.T) {
	fromRepoRoot(t)
	labels, err := hubHostnames()
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(labels, ",")
	for _, want := range []string{"id", "console", "argocd", "grafana"} {
		if !strings.Contains(","+got+",", ","+want+",") {
			t.Errorf("hub hostnames %q do not include %s", got, want)
		}
	}
}

// A hub hostname held by another owner stops the bootstrap at once, naming the
// owner and the command that releases it.
func TestBootstrapRefusesHubHostnamesHeldByAnotherOwner(t *testing.T) {
	fromRepoRoot(t)
	t.Setenv("HETZNER_DNS_API_TOKEN", "tok")
	dir := t.TempDir()
	p := filepath.Join(dir, "registry", "clusters", "c1", "values.yaml")
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte("domain: nutgraf.in\ndns:\n  ownerId: \"nutgraf-new\"\n"), 0o644)

	var asked []string
	old := dnsForeignOwners
	dnsForeignOwners = func(_ context.Context, token, zone, owner string, hosts []string) (map[string]string, error) {
		asked = hosts
		if token != "tok" || zone != "nutgraf.in" || owner != "nutgraf-new" {
			t.Errorf("asked with token=%q zone=%q owner=%q", token, zone, owner)
		}
		return map[string]string{"id.nutgraf.in": "nutgraf-old"}, nil
	}
	t.Cleanup(func() { dnsForeignOwners = old })

	err := (&Orchestrator{GitopsDir: dir, ClusterName: "c1"}).checkHubHostnamesUnclaimed(context.Background())
	if err == nil || !strings.Contains(err.Error(), "kakt teardown --dns-only --dns-owner nutgraf-old --dns-zone nutgraf.in --confirm") {
		t.Fatalf("err = %v, want a refusal naming the release command", err)
	}
	if !strings.Contains(strings.Join(asked, ","), "id.nutgraf.in") {
		t.Errorf("hosts asked %v do not include id.nutgraf.in", asked)
	}

	dnsForeignOwners = func(context.Context, string, string, string, []string) (map[string]string, error) {
		return nil, nil
	}
	if err := (&Orchestrator{GitopsDir: dir, ClusterName: "c1"}).checkHubHostnamesUnclaimed(context.Background()); err != nil {
		t.Fatalf("unclaimed hostnames refused: %v", err)
	}
}
