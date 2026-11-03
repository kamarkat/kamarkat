package support

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// fixtureReader presents a cluster whose every surface carries identifying
// material: the tenant's name in labels, their hostnames on certificates, their
// images, their node names, their namespaces. The assertion is that none of it
// reaches a payload.
type fixtureReader struct{ metrics map[string]string }

func (f fixtureReader) Metrics(_ context.Context, ns, svc string, _ int) (string, error) {
	if m, ok := f.metrics[ns+"/"+svc]; ok {
		return m, nil
	}
	return "", os.ErrNotExist
}
func (f fixtureReader) Artefact(context.Context, string) ([]byte, error) {
	return nil, os.ErrNotExist
}

// The identifiers that must never appear, and the reason each is a realistic
// leak rather than a contrived one.
var forbidden = []string{
	"acme",                 // the tenant
	"acme-hub",             // cluster name, in node and Application names
	"payments-prod",        // a tenant namespace
	"api.acme.example",     // a hostname, from a certificate's DNS names
	"ghcr.io/acme/billing", // an image reference
	"10.0.3.14",            // an address
	"customer-ledger",      // a resource name
}

const fixtureMetrics = `
# HELP argocd_app_info Application information
argocd_app_info{name="acme-hub-platform-ops",project="platform",sync_status="Synced",health_status="Healthy",dest_server="https://10.0.3.14:6443"} 1
argocd_app_info{name="payments-prod-billing",project="tenant-workloads",sync_status="OutOfSync",health_status="Degraded",dest_server="https://10.0.3.14:6443"} 1
`

const fixturePolicy = `
kyverno_policy_results_total{policy_name="require-limits",policy_type="ClusterPolicy",rule_result="fail",resource_namespace="payments-prod",resource_name="customer-ledger"} 7
`

const fixtureCerts = `
certmanager_certificate_expiration_timestamp_seconds{name="acme-wildcard",namespace="platform-ops",issuer_name="letsencrypt-prod",dnsNames="api.acme.example"} 1789000000
`

func allowlistFromRepo(t *testing.T) *Allowlist {
	t.Helper()
	// The shipped file, not a copy. A test against its own fixture asserts the
	// engine and not the contract, and the contract is the thing under review.
	raw, err := os.ReadFile(filepath.Join("..", "..",
		"manifests", "hub-core-services", "support-agent", "allowlist.yaml"))
	if err != nil {
		t.Fatalf("read the shipped allowlist: %v", err)
	}
	var cm struct {
		Data map[string]string `yaml:"data"`
	}
	if err := yamlUnmarshal(raw, &cm); err != nil {
		t.Fatalf("parse the ConfigMap: %v", err)
	}
	body, ok := cm.Data["allowlist.yaml"]
	if !ok {
		t.Fatal("the ConfigMap has no allowlist.yaml key")
	}
	a, err := Parse([]byte(body))
	if err != nil {
		t.Fatalf("the shipped allowlist does not parse: %v", err)
	}
	return a
}

// ADR-077's central claim, asserted against the allowlist that actually ships.
func TestNoIdentifyingMaterialReachesThePayload(t *testing.T) {
	a := allowlistFromRepo(t)
	r := fixtureReader{metrics: map[string]string{
		"platform-ops/argocd-applicationset-controller-metrics": fixtureMetrics,
		"platform-ops/kyverno-reports-controller-metrics":       fixturePolicy,
		"platform-ops/cert-manager-metrics":                     fixtureCerts,
	}}

	p := Collect(context.Background(), a, r, "hashed-cluster-id")
	if len(p.Records) == 0 {
		t.Fatal("the fixture produced no records at all; this test would pass vacuously")
	}

	b, err := json.Marshal(p.Records)
	if err != nil {
		t.Fatal(err)
	}
	payload := string(b)
	for _, bad := range forbidden {
		if strings.Contains(payload, bad) {
			t.Errorf("the payload carries %q, which identifies the tenant:\n%s", bad, payload)
		}
	}
}

// The engine must not be the only thing standing between an unlisted field and
// the wire. Adding a label to a scraped series is something an upstream
// component does without asking anyone here.
func TestAnUnlistedFieldIsNeverEmitted(t *testing.T) {
	c := Collector{
		Name: "t", Source: Source{Kind: KindMetrics, Query: "m"},
		Schema: []string{"a", "b"}, Emit: []string{"a"},
	}
	got := c.Project(map[string]string{"a": "kept", "b": "dropped", "surprise": "never-listed"})
	if got["a"] != "kept" {
		t.Error("an emitted field was lost")
	}
	if len(got) != 1 {
		t.Errorf("Project returned %v; only listed fields may survive", got)
	}
}

// ADR-077: effective scope is allowlist − denylist, and no value can add.
func TestTenantDenylistOnlySubtracts(t *testing.T) {
	a := &Allowlist{Collectors: []Collector{
		{Name: "one", Source: Source{Kind: KindMetrics, Query: "m"},
			Schema: []string{"x", "y"}, Emit: []string{"x", "y"}},
		{Name: "two", Source: Source{Kind: KindMetrics, Query: "m"},
			Schema: []string{"z"}, Emit: []string{"z"}},
	}}

	e := a.Effective([]string{"two", "one.y"})
	if len(e.Collectors) != 1 || e.Collectors[0].Name != "one" {
		t.Fatalf("denying a collector did not remove it: %+v", e.Collectors)
	}
	if strings.Join(e.Collectors[0].Emit, ",") != "x" {
		t.Errorf("denying one.y left %v", e.Collectors[0].Emit)
	}

	// A denylist naming something absent is not an error and adds nothing.
	before := len(a.Effective(nil).Collectors)
	after := len(a.Effective([]string{"three", "one.nonexistent"}).Collectors)
	if before != after {
		t.Errorf("an irrelevant denylist changed the scope: %d -> %d", before, after)
	}
	// And a denylist cannot introduce a field.
	for _, c := range a.Effective([]string{"nothing"}).Collectors {
		for _, f := range c.Emit {
			if f != "x" && f != "y" && f != "z" {
				t.Errorf("a field appeared that no collector declared: %q", f)
			}
		}
	}
}

// A malformed allowlist must not load as a permissive one.
func TestParseRefusesAnAllowlistThatWouldNotBoundAnything(t *testing.T) {
	cases := map[string]string{
		"emit escapes schema": `
- collector: c
  source: {kind: metrics, query: m}
  schema: [a]
  emit: [a, b]`,
		"no schema": `
- collector: c
  source: {kind: metrics, query: m}
  emit: [a]`,
		"unknown kind": `
- collector: c
  source: {kind: kubernetes, query: m}
  schema: [a]
  emit: [a]`,
		"metrics with no query": `
- collector: c
  source: {kind: metrics}
  schema: [a]
  emit: [a]`,
		"duplicate collector": `
- collector: c
  source: {kind: metrics, query: m}
  schema: [a]
  emit: [a]
- collector: c
  source: {kind: metrics, query: m}
  schema: [b]
  emit: [b]`,
		"empty": ``,
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(doc)); err == nil {
				t.Error("accepted an allowlist that does not bound what leaves")
			}
		})
	}
}

// An exact metric-name match, so a longer series does not get projected against
// a schema written for a different one.
func TestQueryMatchesTheSeriesNameExactly(t *testing.T) {
	c := Collector{
		Name: "t", Source: Source{Kind: KindMetrics, Namespace: "n", Service: "s", Query: "kube_node_status_condition"},
		Schema: []string{"condition", "status"}, Emit: []string{"condition", "status"},
	}
	r := fixtureReader{metrics: map[string]string{"n/s": `
kube_node_status_condition{condition="Ready",status="true",node="acme-hub-md-0"} 1
kube_node_status_condition_info{condition="Bogus",status="true",node="acme-hub-md-0"} 1
`}}
	got, err := collectMetrics(context.Background(), r, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("matched %d samples, want 1 (the _info series must not match)", len(got))
	}
	if got[0]["condition"] != "Ready" {
		t.Errorf("matched the wrong series: %v", got[0])
	}
}

// A collector that fails must not take the run down with it.
func TestAFailedCollectorIsRecordedNotFatal(t *testing.T) {
	a := &Allowlist{Collectors: []Collector{
		{Name: "reachable", Source: Source{Kind: KindMetrics, Namespace: "n", Service: "s", Query: "m"},
			Schema: []string{"a"}, Emit: []string{"a"}},
		{Name: "gone", Source: Source{Kind: KindMetrics, Namespace: "n", Service: "absent", Query: "m"},
			Schema: []string{"a"}, Emit: []string{"a"}},
	}}
	r := fixtureReader{metrics: map[string]string{"n/s": `m{a="1"} 1`}}
	p := Collect(context.Background(), a, r, "c")
	if len(p.Records) != 1 {
		t.Errorf("one unreachable component cost the run its other evidence: %+v", p.Records)
	}
	if p.Skipped["gone"] == "" {
		t.Error("a collector that produced nothing must say why, or it reads as a healthy box")
	}
}

// The shipped allowlist is the contract, so it has to stay reviewable: every
// collector annotated, and no emit field that looks like a free-text carrier.
func TestShippedAllowlistEmitsNoFreeTextField(t *testing.T) {
	a := allowlistFromRepo(t)
	// Error strings and messages carry anything the component put in them,
	// which is the leak an allowlist of label names does not otherwise catch.
	risky := regexp.MustCompile(`(?i)message|error|reason|description|detail|url|host|dns`)
	for _, c := range a.Collectors {
		for _, f := range c.Emit {
			if risky.MatchString(f) {
				t.Errorf("collector %q emits %q, which carries free text; "+
					"ADR-077 bounds what is gathered, and a message field is "+
					"unbounded by construction", c.Name, f)
			}
		}
	}
}
