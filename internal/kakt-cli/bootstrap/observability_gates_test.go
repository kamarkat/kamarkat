package bootstrap

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// alloyConfigs are every Grafana Alloy configuration the platform ships.
func alloyConfigs(t *testing.T) map[string]string {
	t.Helper()
	root := repoRoot(t)
	out := map[string]string{}
	for _, rel := range []string{
		// One file, two Alloy configs since ADR-078 add.1 §5 split the workload.
		"manifests/hub-core-services/grafana-alloy/deployment.yaml",
		"manifests/spoke/spoke-catalog/infra/grafana-alloy.yaml",
	} {
		raw, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		out[rel] = string(raw)
	}
	return out
}

// scrapeBlock matches a prometheus.scrape component and captures its body.
var scrapeBlock = regexp.MustCompile(`(?s)prometheus\.scrape\s+"([^"]+)"\s*\{(.*?)\n    \}`)

// Collection is bounded and named: no scrape may read raw discovery output.
//
// ADR-078 §3. A prometheus.scrape whose targets are discovery.kubernetes.*
// directly dials every port of every object the cluster has, including ports
// that do not speak HTTP. That is not theoretical -- it is what shipped, and on
// 2026-09-18 it sent `GET /metrics HTTP/1.1` to argocd-redis:6379 every thirty
// seconds. Redis logged "Possible SECURITY ATTACK detected ... Cross Protocol
// Scripting" and aborted the connection, on a loop, forever. platform-db:5432
// and platform-redis:6379 received the same.
//
// It also carried tenant workload telemetry off the box, which ADR-066 does not
// permit and §3 excludes by default.
//
// A scrape must therefore read either a named target list or the output of a
// discovery.relabel that filters. This is asserted rather than reviewed because
// the failure is silent in every other check: the config is valid, Alloy starts,
// metrics flow, and the damage is visible only in another component's logs.
func TestAlloyScrapesAreNeverRawDiscovery(t *testing.T) {
	for path, body := range alloyConfigs(t) {
		for _, m := range scrapeBlock.FindAllStringSubmatch(body, -1) {
			name, block := m[1], m[2]
			targets := ""
			for _, line := range strings.Split(block, "\n") {
				if s := strings.TrimSpace(line); strings.HasPrefix(s, "targets") {
					targets = s
					break
				}
			}
			if targets == "" {
				t.Errorf("%s: prometheus.scrape %q declares no targets", path, name)
				continue
			}
			if strings.Contains(targets, "discovery.kubernetes.") {
				t.Errorf("%s: prometheus.scrape %q reads raw discovery output:\n    %s\n"+
					"ADR-078 §3 makes collection bounded and named. Route it through a "+
					"discovery.relabel that keeps only platform-owned namespaces and "+
					"endpoints named for metrics, or name the targets literally.",
					path, name, targets)
			}
		}
	}
}

// Every Alloy config must scope what it collects to platform-owned namespaces.
//
// ADR-078 §3: "Tenant namespaces are not collected by default. Forwarding a
// tenant's workload telemetry off-cluster without being asked is on the wrong
// side of ADR-066." The label is the mechanism, and it is the same one ADR-077
// relies on.
func TestAlloyScopesCollectionToPlatformNamespaces(t *testing.T) {
	for path, body := range alloyConfigs(t) {
		if !strings.Contains(body, "__meta_kubernetes_namespace_label_topology_platform_io_role") {
			t.Errorf("%s: nothing scopes collection to platform-owned namespaces.\n"+
				"ADR-078 §3 selects them by the topology.platform.io/role label; without "+
				"it this config collects tenant namespaces and forwards them off-cluster.",
				path)
		}
	}
}

// The cluster's identity in exported labels is the box's own, never a constant.
//
// ADR-078's Context records the defect: Alloy "stamps external_labels =
// { cluster = "hub" }, a constant, so two clusters arriving at one destination
// are indistinguishable". Every box that pulled the bundle reported itself as
// "hub", which makes a multi-box estate unqueryable and a support conversation
// impossible to ground in a specific cluster.
func TestAlloyClusterLabelIsNotAConstant(t *testing.T) {
	root := repoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root,
		"manifests/hub-core-services/grafana-alloy/deployment.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)

	if regexp.MustCompile(`cluster\s*=\s*"hub"`).MatchString(body) {
		t.Error(`external_labels carries the literal cluster = "hub". ` +
			`It must read the box's own name (sys.env("CLUSTER_NAME"), substituted ` +
			`from global.clusterName by templated-fields.yaml).`)
	}
	if !strings.Contains(body, `sys.env("CLUSTER_NAME")`) {
		t.Error("the hub collector does not read CLUSTER_NAME, so its telemetry " +
			"cannot say which box it came from")
	}
	tf := filepath.Join(root, "manifests/hub-core-services/grafana-alloy/templated-fields.yaml")
	if _, err := os.Stat(tf); err != nil {
		t.Errorf("no templated-fields.yaml for grafana-alloy: CLUSTER_NAME would ship "+
			"as the literal in the manifest to every box (%v)", err)
	}
}

// ADR-078 §8: the Support Agent and the observability backend share sources and
// share nothing else.
//
// "A release gate asserts that no Support Agent collector names an
// observability component, so the coupling cannot be introduced later by
// someone wiring the agent to the store because the store is convenient."
//
// The property being protected is that deleting VictoriaMetrics does not stop
// support telemetry, and unenrolling from support does not degrade
// observability. A collector that read the store would make each a dependency
// of the other, and the ADR-077 allowlist would stop being the only thing
// deciding what leaves the box.
func TestSupportAgentNamesNoObservabilityComponent(t *testing.T) {
	root := repoRoot(t)

	// The backends, by the names they are addressable at.
	forbidden := []string{
		"vmsingle-", "vlogs-", "vmalert-",
		"victoriametrics.platform-observability", "victoria-metrics",
		"grafana.platform-observability",
	}

	for _, rel := range []string{
		"manifests/hub-core-services/support-agent",
		"manifests/spoke/spoke-catalog/infra/support-agent.yaml",
		"operators/support-agent",
	} {
		p := filepath.Join(root, rel)
		info, err := os.Stat(p)
		if err != nil {
			continue // not built yet; ADR-077 is Proposed
		}
		walk := func(f string) {
			raw, err := os.ReadFile(f)
			if err != nil {
				return
			}
			body := string(raw)
			for _, bad := range forbidden {
				if strings.Contains(body, bad) {
					r, _ := filepath.Rel(root, f)
					t.Errorf("%s names the observability component %q.\n"+
						"ADR-078 §8: the Support Agent and the observability backend share "+
						"evidence SOURCES and nothing else. Reading the store here would make "+
						"support telemetry depend on a component a tenant may delete, and put a "+
						"second path around the ADR-077 allowlist.", r, bad)
				}
			}
		}
		if info.IsDir() {
			_ = filepath.WalkDir(p, func(f string, d os.DirEntry, err error) error {
				if err == nil && !d.IsDir() {
					walk(f)
				}
				return nil
			})
		} else {
			walk(p)
		}
	}
}

// Alloy runs as a DaemonSet AND a Deployment, and neither scrapes the other's
// targets.
//
// ADR-078 add.1 §5. A DaemonSet scraping a cluster singleton emits one copy of
// every series PER NODE: "it makes every alert expression that counts something
// wrong." A Deployment tailing pod logs sees one node's and silently loses the
// rest. Both are silent -- the data arrives, it is simply wrong, and an alert
// built on it fires or does not for reasons nobody can see.
//
// Asserted on the configs rather than left to review because the split is easy
// to undo by moving one scrape block between two files that look alike.
func TestAlloyNodeAndClusterCollectorsDoNotOverlap(t *testing.T) {
	root := repoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root,
		"manifests/hub-core-services/grafana-alloy/deployment.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)

	nodeCfg, clusterCfg := splitAlloyConfigs(t, body)

	// Cluster singletons: exactly once, from the Deployment.
	for _, singleton := range []string{"kube_state_metrics", "platform_metrics"} {
		if strings.Contains(nodeCfg, `prometheus.scrape "`+singleton+`"`) {
			t.Errorf("the DaemonSet config scrapes %q, a cluster singleton. Every node "+
				"would emit its own copy of those series.", singleton)
		}
		if !strings.Contains(clusterCfg, `prometheus.scrape "`+singleton+`"`) {
			t.Errorf("the Deployment config does not scrape %q; nothing else does", singleton)
		}
	}

	// Node-local: only from the DaemonSet.
	for _, local := range []string{"kubelet", "cadvisor", "node_exporter"} {
		if strings.Contains(clusterCfg, `prometheus.scrape "`+local+`"`) {
			t.Errorf("the Deployment config scrapes %q, which is node-local. One replica "+
				"would report one node and the rest would be invisible.", local)
		}
		if !strings.Contains(nodeCfg, `prometheus.scrape "`+local+`"`) {
			t.Errorf("the DaemonSet config does not scrape %q", local)
		}
	}

	// Logs are node-local too, and the DaemonSet must read only ITS node's pods.
	if strings.Contains(clusterCfg, "loki.source.file") {
		t.Error("the Deployment config collects pod logs; from one replica that is " +
			"one node's logs and silently no others")
	}
	if !strings.Contains(nodeCfg, `field = "spec.nodeName=" + sys.env("NODE_NAME")`) {
		t.Error("the DaemonSet does not restrict pod discovery to its own node, so every " +
			"Alloy pod tails every pod's logs on every node")
	}
}

// Everything Alloy sends carries the cluster AND the tenant.
//
// ADR-078 add.1 §7: "cluster alone is unique within a box and not at a corporate
// Prometheus receiving three of them -- which is precisely the complaint this
// ADR's Context raises about the constant label." The pair was cluster +
// substrate, and substrate says hub-or-spoke, which identifies no box.
func TestAlloyStampsClusterAndTenant(t *testing.T) {
	for path, body := range alloyConfigs(t) {
		if !strings.Contains(body, `sys.env("TENANT_ID")`) {
			t.Errorf("%s does not stamp a tenant label; telemetry reaching a shared "+
				"destination cannot say which box produced it", path)
		}
		if regexp.MustCompile(`substrate\s*=\s*sys\.env`).MatchString(body) {
			t.Errorf("%s still uses substrate as an identity label", path)
		}
	}
}

// kube-state-metrics is bounded at the SERIES, because it cannot be bounded at
// the target.
//
// ADR-078 add.1 §6. It is cluster-scoped: one endpoint describes objects in
// every namespace, so keeping or dropping the target is all-or-nothing. Without
// a series-level keep, a tenant's pod names, workload names and object labels
// leave the box through the one collector a namespace selector cannot bound --
// while every other path correctly excludes them, which is what makes it easy
// to miss.
func TestKubeStateMetricsIsBoundedAtTheSeries(t *testing.T) {
	root := repoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root,
		"manifests/hub-core-services/grafana-alloy/deployment.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	_, clusterCfg := splitAlloyConfigs(t, string(raw))

	if !strings.Contains(clusterCfg, `prometheus.relabel "platform_namespaces_only"`) {
		t.Fatal("kube-state-metrics is forwarded with no series-level namespace filter; " +
			"every tenant namespace's object metadata leaves the box")
	}
	// The scrape must forward INTO the filter, not around it.
	i := strings.Index(clusterCfg, `prometheus.scrape "kube_state_metrics"`)
	if i < 0 {
		t.Fatal("no kube_state_metrics scrape")
	}
	block := clusterCfg[i:]
	if j := strings.Index(block, "\n    }"); j > 0 {
		block = block[:j]
	}
	if !strings.Contains(block, "prometheus.relabel.platform_namespaces_only.receiver") {
		t.Error("the kube_state_metrics scrape forwards straight to the destination, " +
			"bypassing the namespace filter that is the only thing bounding it")
	}
}

// splitAlloyConfigs returns the node and cluster config bodies.
func splitAlloyConfigs(t *testing.T, manifest string) (node, cluster string) {
	t.Helper()
	cut := func(name string) string {
		marker := "name: " + name
		i := strings.Index(manifest, marker)
		if i < 0 {
			t.Fatalf("no ConfigMap %q in the Alloy manifest", name)
		}
		rest := manifest[i:]
		// to the start of the next document
		if j := strings.Index(rest, "\n---"); j > 0 {
			rest = rest[:j]
		}
		return rest
	}
	return cut("grafana-alloy-node"), cut("grafana-alloy-cluster")
}

// Everything that names kube-state-metrics names the same Service.
//
// Three files address it independently: the component descriptor that deploys
// it, the Alloy config that scrapes it, and the Support Agent's node-pressure
// collector. Nothing joined them, and they drifted -- the descriptor deployed to
// platform-ops while Alloy scraped platform-observability, so the target never
// resolved and every kube_* series was absent. That is silent: Alloy reports a
// down target in its own metrics and nowhere else, and the 211 lines of
// platform-core-alerts that read kube_pod_* evaluate against nothing.
//
// The namespace is not arbitrary. kube-state-metrics is required collection
// machinery (ADR-066 add.4), not part of the selectable observability
// capability, because the Support Agent depends on it (ADR-077 add.4 §13) and
// ADR-078 §8 forbids support evidence resting on a capability a tenant may
// disable. It therefore lives outside platform-observability, and this gate is
// what keeps the three references agreeing on where.
func TestKubeStateMetricsAddressAgreesEverywhere(t *testing.T) {
	root := repoRoot(t)

	read := func(rel string) string {
		raw, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		return string(raw)
	}

	// The descriptor decides where it is deployed.
	desc := read("manifests/argocd/components/03/kube-state-metrics.yaml")
	m := regexp.MustCompile(`destinationNamespace:\s*(\S+)`).FindStringSubmatch(desc)
	if m == nil {
		t.Fatal("the kube-state-metrics descriptor declares no destinationNamespace")
	}
	ns := m[1]

	// Alloy must scrape it there.
	alloy := read("manifests/hub-core-services/grafana-alloy/deployment.yaml")
	want := "kube-state-metrics." + ns + ".svc"
	if !strings.Contains(alloy, want) {
		t.Errorf("the descriptor deploys kube-state-metrics to %q but the Alloy config "+
			"does not scrape %q.\nA target that does not resolve is silent: every kube_* "+
			"series is simply absent, and the alerts reading them evaluate against nothing.",
			ns, want)
	}

	// And the Support Agent's collector must name the same namespace.
	allowlist := read("manifests/hub-core-services/support-agent/allowlist.yaml")
	if strings.Contains(allowlist, "service: kube-state-metrics") {
		i := strings.Index(allowlist, "service: kube-state-metrics")
		window := allowlist[max(0, i-400):i]
		if !strings.Contains(window, "namespace: "+ns) {
			t.Errorf("the Support Agent's kube-state-metrics collector does not name "+
				"namespace %q, which is where the descriptor deploys it", ns)
		}
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// A descriptor's helmValues may only read values the environment-manager has.
//
// _distribution.tpl:55 runs `tpl` over helmValues in the ENVIRONMENT-MANAGER's
// own context. `global` is what that chart EMITS to component charts, not
// something it reads, so `.Values.global.anything` in a descriptor is nil.
//
// The failure is late and total: the released render in bundle-chart.sh dies
// with "nil pointer evaluating interface {}.provider" and NO BUNDLE IS
// PRODUCED. It costs a full publish to discover, and nothing before that point
// touches the released path -- the development render does not exercise it.
func TestDescriptorHelmValuesReadEnvironmentManagerValues(t *testing.T) {
	root := repoRoot(t)
	dir := filepath.Join(root, "manifests", "argocd", "components")

	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".yaml" {
			return err
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel, _ := filepath.Rel(root, path)
		for _, line := range strings.Split(string(raw), "\n") {
			// Skip comments -- a comment may legitimately mention the wrong form
			// while explaining why it is wrong.
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			if strings.Contains(line, ".Values.global.") {
				t.Errorf("%s reads .Values.global in a descriptor:\n    %s\n"+
					"helmValues is templated in the environment-manager's context, where "+
					"global is emitted rather than read. Use .Values.<name>; the released "+
					"render fails on a nil pointer otherwise, and produces no bundle.",
					rel, strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", dir, err)
	}
}

// No Application's helm values may declare `global:` twice.
//
// YAML has no merge for a duplicate key at the same level: the second mapping
// REPLACES the first outright. An ApplicationSet that emits globalValues and
// then appends its own `global:` to override one key silently drops every
// other global with it.
//
// That shipped. The spoke catalogue overrode environmentSlug that way, on the
// reasoning that "last wins in Helm" -- true of a scalar, false of the mapping
// around it. global.provider arrived nil on every spoke, the catalogue's
// provider-gated template block rendered NOTHING, and ArgoCD PRUNED what that
// block owns: VMSingle, VLogs and the query endpoint's ConfigMap were deleted
// as no longer declared. The Application reported Synced throughout -- 354
// resources synced, 11 pruned -- so nothing anywhere said the cluster had lost
// its telemetry store. Verified on nutgraf-01, 2026-09-20.
//
// The fix is to pass the override INTO the helper, not to append a block after
// it, and this asserts nobody appends one again.
func TestAppSetHelmValuesDeclareGlobalOnce(t *testing.T) {
	root := repoRoot(t)
	dir := filepath.Join(root, "manifests", "argocd", "environment-manager", "templates")

	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, "appset.yaml") {
			return err
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel, _ := filepath.Rel(root, path)

		// Each `values: |` block is one Application's values. Count the
		// `global:` keys emitted into it -- both the literal and the helper.
		for _, block := range strings.Split(string(raw), "values: |")[1:] {
			// The block ends where the indentation returns to the list level.
			if i := strings.Index(block, "\n      - "); i > 0 {
				block = block[:i]
			}
			literal := 0
			for _, line := range strings.Split(block, "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), "#") {
					continue
				}
				if strings.TrimSpace(line) == "global:" {
					literal++
				}
			}
			helper := strings.Count(block, `include "environment-manager.globalValues"`)
			if literal+helper > 1 {
				t.Errorf("%s: an Application's helm values declare global %d time(s) "+
					"(%d literal, %d via globalValues).\n"+
					"A second `global:` replaces the first entirely -- provider, hubDomain, "+
					"clusterName and the Infisical ids all become nil, and ArgoCD prunes "+
					"whatever a provider-gated template owns. Pass the override into "+
					"globalValues instead.", rel, literal+helper, literal, helper)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", dir, err)
	}
}

// An image that runs as root may not be given runAsNonRoot without runAsUser.
//
// runAsNonRoot is a REQUIREMENT, not a setting: it tells the kubelet to refuse
// an image that would run as root. Most platform images declare a USER and are
// unaffected -- which is why this checks the CONTAINER's image rather than
// counting occurrences in a file. Two earlier versions of this test got that
// wrong: one flagged every runAsNonRoot in the repository, the other counted
// per file and so mixed containers running different images.
//
// registry.k8s.io/kubectl carries no USER. Kyverno's five cleanup CronJobs used
// it with runAsNonRoot and no uid, failed admission with "container has
// runAsNonRoot and image will run as root", and left a
// CreateContainerConfigError pod behind every ten minutes for a day. Nothing
// reported it: the CronJobs exist and the Application is Synced.
//
// The list is what has been verified, not a guess. Adding an image is a claim
// that it runs as root, checkable with:
//
//	docker inspect --format '{{.Config.User}}' <image>
func TestRootImagesCarryRunAsUser(t *testing.T) {
	root := repoRoot(t)

	runsAsRoot := []string{"registry.k8s.io/kubectl"}

	// podSpecs yields every container in a manifest, whatever wraps it.
	var walkSpec func(v any, fn func(map[string]any))
	walkSpec = func(v any, fn func(map[string]any)) {
		switch t := v.(type) {
		case map[string]any:
			if cs, ok := t["containers"].([]any); ok {
				for _, c := range cs {
					if cm, ok := c.(map[string]any); ok {
						fn(cm)
					}
				}
			}
			for _, vv := range t {
				walkSpec(vv, fn)
			}
		case []any:
			for _, vv := range t {
				walkSpec(vv, fn)
			}
		}
	}

	for _, dir := range []string{
		filepath.Join(root, "manifests", "spoke", "spoke-catalog"),
		filepath.Join(root, "manifests", "hub-core-services"),
	} {
		err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || filepath.Ext(path) != ".yaml" {
				return err
			}
			raw, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil
			}
			rel, _ := filepath.Rel(root, path)

			dec := yaml.NewDecoder(bytes.NewReader(raw))
			for {
				var doc any
				if err := dec.Decode(&doc); err != nil {
					break // malformed or end of stream; other gates cover parseability
				}
				walkSpec(doc, func(c map[string]any) {
					img, _ := c["image"].(string)
					hit := false
					for _, r := range runsAsRoot {
						if strings.Contains(img, r) {
							hit = true
						}
					}
					if !hit {
						return
					}
					sc, _ := c["securityContext"].(map[string]any)
					if sc == nil {
						return
					}
					if nr, _ := sc["runAsNonRoot"].(bool); !nr {
						return
					}
					if _, ok := sc["runAsUser"]; !ok {
						name, _ := c["name"].(string)
						t.Errorf("%s: container %q runs %s, which has no USER, and declares "+
							"runAsNonRoot with no runAsUser.\nThe kubelet refuses it outright "+
							"-- \"image will run as root\" -- and the pod never starts, "+
							"silently, while its owner reports Synced.", rel, name, img)
					}
				})
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", dir, err)
		}
	}
}

// No vendored chart may ship a workload that invokes a shell in a distroless
// image.
//
// registry.k8s.io/kubectl has neither /bin/bash nor /bin/sh -- verified by
// running both on a live cluster. A container whose command is a shell fails at
// init, every schedule, and the owning Application still reports Synced.
//
// Kyverno 3.2.6 shipped eight such workloads: five report-sweeping CronJobs and
// three lifecycle hooks. They failed for over a day on nutgraf-01 before anyone
// looked at a pod listing.
//
// The fix was not to swap the shell -- there is none to swap to. Upstream
// commit f5ac632b3 deleted those hooks in the same change that adopted the
// distroless image, and 3.6.4 renders zero shell-based workloads. This gate
// exists so a future re-vendor cannot walk back into a chart version that has
// them.
func TestVendoredChartsHaveNoShellInDistrolessImages(t *testing.T) {
	root := repoRoot(t)

	// Images with no shell. Verified by execing one on a cluster, not assumed.
	shellless := []string{"registry.k8s.io/kubectl"}

	var walk func(v any, fn func(map[string]any))
	walk = func(v any, fn func(map[string]any)) {
		switch tv := v.(type) {
		case map[string]any:
			if cs, ok := tv["containers"].([]any); ok {
				for _, c := range cs {
					if cm, ok := c.(map[string]any); ok {
						fn(cm)
					}
				}
			}
			for _, vv := range tv {
				walk(vv, fn)
			}
		case []any:
			for _, vv := range tv {
				walk(vv, fn)
			}
		}
	}

	for _, dir := range []string{
		filepath.Join(root, "manifests", "spoke", "spoke-catalog"),
		filepath.Join(root, "manifests", "hub-core-services"),
	} {
		err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || filepath.Ext(path) != ".yaml" {
				return err
			}
			raw, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil
			}
			rel, _ := filepath.Rel(root, path)

			dec := yaml.NewDecoder(bytes.NewReader(raw))
			for {
				var doc any
				if err := dec.Decode(&doc); err != nil {
					break
				}
				walk(doc, func(c map[string]any) {
					img, _ := c["image"].(string)
					hit := false
					for _, s := range shellless {
						if strings.Contains(img, s) {
							hit = true
						}
					}
					if !hit {
						return
					}
					cmd, _ := c["command"].([]any)
					if len(cmd) == 0 {
						return
					}
					first, _ := cmd[0].(string)
					if strings.HasSuffix(first, "/bash") || strings.HasSuffix(first, "/sh") {
						name, _ := c["name"].(string)
						t.Errorf("%s: container %q runs %s with command %q.\n"+
							"That image is distroless and has no shell -- the container fails "+
							"at init on every run while its Application reports Synced. Invoke "+
							"the binary by args instead, or take the upstream version that "+
							"removed the workload.", rel, name, img, first)
					}
				})
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", dir, err)
		}
	}
}

// A vendored chart may not carry a helm hook ArgoCD cannot run, nor an
// upgradeable-in-place one that lacks a delete policy.
//
// A helm hook is something helm runs at a moment in a release's life. A
// vendored manifest is applied by ArgoCD from a kustomize build, so there is no
// helm and no release -- but ArgoCD DOES map some hooks onto its own phases,
// which is why this is two rules rather than a ban:
//
//	pre-install, pre-upgrade    -> PreSync     runs, and is fine
//	post-install, post-upgrade  -> PostSync    runs, and is fine
//	pre-delete, post-delete     -> nothing     applied as an ordinary object
//	test                        -> nothing     applied as an ordinary object
//
// The second group is the dangerous one. Kyverno 3.2.6's three PRE-DELETE Jobs
// scale kyverno to zero and strip its webhooks; applied at install time they
// would dismantle the thing being installed. They stayed inert only because
// ArgoCD declines to map pre-delete at all -- luck, not design. Its seven test
// Pods ran once and lingered on every spoke.
//
// The first group still needs a delete policy, because a Job's pod template is
// IMMUTABLE. kyverno-migrate-resources carried none, so the 3.2.6 -> 3.6.4
// upgrade failed on it:
//
//	Job.batch "kyverno-migrate-resources" is invalid:
//	spec.template.metadata.labels[controller-uid]: Required value
//
// which wedged the whole catalogue sync and would wedge every later upgrade.
// cert-manager's startupapicheck is the counter-example that proves the rule:
// same post-install shape, but `hook-delete-policy: before-hook-creation,...`,
// so it is removed and recreated rather than patched, and it upgrades cleanly.
func TestVendoredChartsCarryNoUnrunnableHelmHooks(t *testing.T) {
	root := repoRoot(t)

	// Hooks ArgoCD maps to a phase of its own. Anything else is applied as an
	// ordinary object at install time.
	runnable := map[string]bool{
		"pre-install": true, "pre-upgrade": true,
		"post-install": true, "post-upgrade": true,
	}

	for _, dir := range []string{
		filepath.Join(root, "manifests", "spoke", "spoke-catalog"),
		filepath.Join(root, "manifests", "hub-core-services"),
	} {
		err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || filepath.Ext(path) != ".yaml" {
				return err
			}
			raw, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil
			}
			rel, _ := filepath.Rel(root, path)

			dec := yaml.NewDecoder(bytes.NewReader(raw))
			for {
				var doc map[string]any
				if err := dec.Decode(&doc); err != nil {
					break
				}
				meta, _ := doc["metadata"].(map[string]any)
				if meta == nil {
					continue
				}
				ann, _ := meta["annotations"].(map[string]any)
				if ann == nil {
					continue
				}
				hook, _ := ann["helm.sh/hook"].(string)
				if hook == "" {
					continue
				}
				name, _ := meta["name"].(string)
				kind, _ := doc["kind"].(string)

				// A hook may name several phases.
				for _, phase := range strings.Split(hook, ",") {
					phase = strings.TrimSpace(phase)
					if !runnable[phase] {
						t.Errorf("%s: %s/%s carries helm.sh/hook: %s, which ArgoCD does "+
							"not map to any phase.\nIt is applied as an ordinary object at "+
							"install time -- a pre-delete hook would dismantle what is being "+
							"installed. Exclude it when vendoring.", rel, kind, name, phase)
					}
				}

				// A runnable hook still has to be replaceable.
				if kind == "Job" {
					pol, _ := ann["helm.sh/hook-delete-policy"].(string)
					if !strings.Contains(pol, "before-hook-creation") {
						t.Errorf("%s: Job/%s is a %s hook with no before-hook-creation in its "+
							"delete policy (%q).\nA Job's pod template is immutable, so the "+
							"next chart upgrade fails on it and wedges the sync.",
							rel, name, hook, pol)
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", dir, err)
		}
	}
}
