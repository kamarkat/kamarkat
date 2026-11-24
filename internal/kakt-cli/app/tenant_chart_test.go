package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// `kakt app secrets set|import` writes one KV v2 secret per application per
// cluster (SecretPathOn); the tenant chart's ExternalSecrets must read exactly
// that secret, with each declared key as its property, and the identity that
// writes must be allowed to. Three files, three owners: if any one drifts, a
// deployed application starts without its secrets and nothing names why.
func TestAppSecretsWriteWhereTheTenantChartReads(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not installed")
	}
	_, here, _, _ := runtime.Caller(0)
	repo := filepath.Join(filepath.Dir(here), "..", "..", "..")
	chart := filepath.Join(repo, "manifests", "tenants", "charts", "universal-tenant")

	values := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(values, []byte(`
tenantId: acme
appId: shop
clusterName: spoke-a
hubDomain: example.test
secrets:
  - name: PAYMENTS_API_KEY
    capability: payments
    workloads: [api]
`), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("helm", "template", "t", chart, "-f", values).CombinedOutput()
	if err != nil {
		t.Fatalf("the tenant chart does not render: %v\n%s", err, out)
	}

	wrote := App{AppID: "shop"}.SecretPathOn("spoke-a")

	var read *struct{ key, property string }
	for _, doc := range strings.Split(string(out), "\n---") {
		var es struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Data []struct {
					RemoteRef struct {
						Key      string `json:"key"`
						Property string `json:"property"`
					} `json:"remoteRef"`
				} `json:"data"`
			} `json:"spec"`
		}
		if yaml.Unmarshal([]byte(doc), &es) != nil || es.Kind != "ExternalSecret" ||
			es.Metadata.Name != "shop-api-secrets" {
			continue
		}
		for _, d := range es.Spec.Data {
			if d.RemoteRef.Property == "PAYMENTS_API_KEY" {
				read = &struct{ key, property string }{d.RemoteRef.Key, d.RemoteRef.Property}
			}
		}
	}
	if read == nil {
		t.Fatalf("the chart rendered no ExternalSecret shop-api-secrets reading PAYMENTS_API_KEY")
	}
	if read.key != wrote {
		t.Fatalf("kakt app secrets writes %q but the application reads %q", wrote, read.key)
	}

	// The writer's OpenBao policy must cover that path.
	policy, err := os.ReadFile(filepath.Join(repo, "manifests", "hub-core-services", "openbao", "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)sys/policies/acl/openbao-app-secrets".*?path "([^"]+)"\s*\{\s*capabilities = \[([^\]]*)\]`).
		FindSubmatch(policy)
	if m == nil {
		t.Fatal("no openbao-app-secrets policy found in the OpenBao configuration")
	}
	if !aclMatches(string(m[1]), "kv/data/"+wrote) || !strings.Contains(string(m[2]), `"update"`) {
		t.Fatalf("the openbao-app-secrets policy (%s %s) does not let kakt app secrets write kv/data/%s",
			m[1], m[2], wrote)
	}
}

// aclMatches applies OpenBao's path glob: `+` is one segment, a trailing `*` any suffix.
func aclMatches(pattern, path string) bool {
	re := regexp.QuoteMeta(pattern)
	re = strings.ReplaceAll(re, `\+`, `[^/]+`)
	re = strings.ReplaceAll(re, `\*`, `.*`)
	return regexp.MustCompile("^" + re + "$").MatchString(path)
}
