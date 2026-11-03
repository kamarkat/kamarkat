package cluster

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// renderCluster reproduces what applyCluster feeds to kubectl, so a template typo
// is caught here rather than as an apply-time rejection halfway through a
// bootstrap that has already provisioned cloud infrastructure.
func renderCluster(t *testing.T, cfg *Config) map[string]any {
	t.Helper()
	p := &Provisioner{Config: cfg}
	out, err := p.renderClusterManifest()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("rendered manifest is not valid YAML: %v\n%s", err, out)
	}
	return doc
}

func topologyVars(t *testing.T, doc map[string]any) map[string]any {
	t.Helper()
	spec, _ := doc["spec"].(map[string]any)
	topo, _ := spec["topology"].(map[string]any)
	raw, _ := topo["variables"].([]any)
	vars := map[string]any{}
	for _, v := range raw {
		if m, ok := v.(map[string]any); ok {
			vars[m["name"].(string)] = m["value"]
		}
	}
	return vars
}

func baseConfig() *Config {
	return &Config{
		ClusterName:             "hub-test",
		Namespace:               "platform-capi",
		Region:                  "hel1",
		OSType:                  "ubuntu",
		KubernetesVersion:       "v1.35.10",
		NetworkCIDR:             "10.0.0.0/16",
		SubnetCIDR:              "10.0.0.0/24",
		ControlPlaneMachineType: "cx33",
		WorkerMachineType:       "cx33",
		ControlPlaneReplicas:    1,
		SSHKeyName:              "mac-mini-ssh",
	}
}

// The hybrid hub runs no Hetzner workers (ADR-046): worker capacity comes from
// home-lab Flatcar nodes. That is only viable if the control plane is schedulable,
// because home workers join manually after the bootstrap has finished — the two
// settings must travel together or every platform workload sits Pending.
func TestHybridHubHasNoHetznerWorkersAndSchedulableControlPlane(t *testing.T) {
	cfg := baseConfig()
	cfg.WorkerReplicas = 0
	cfg.ControlPlaneSchedulable = true

	doc := renderCluster(t, cfg)
	vars := topologyVars(t, doc)

	if got := vars["controlPlaneSchedulable"]; got != true {
		t.Errorf("controlPlaneSchedulable = %v (%T), want true", got, got)
	}

	spec := doc["spec"].(map[string]any)
	topo := spec["topology"].(map[string]any)
	workers := topo["workers"].(map[string]any)
	mds := workers["machineDeployments"].([]any)
	md0 := mds[0].(map[string]any)
	if got := md0["replicas"]; got != 0 {
		t.Errorf("md-0 replicas = %v, want 0", got)
	}
}

// The pure-Hetzner hub is the tested path and must be unchanged: 2 workers, and a
// control plane that keeps its default taint.
func TestHetznerHubKeepsWorkersAndTaintedControlPlane(t *testing.T) {
	cfg := baseConfig()
	cfg.WorkerReplicas = 2
	cfg.ControlPlaneSchedulable = false

	doc := renderCluster(t, cfg)
	vars := topologyVars(t, doc)

	if got := vars["controlPlaneSchedulable"]; got != false {
		t.Errorf("controlPlaneSchedulable = %v (%T), want false", got, got)
	}

	spec := doc["spec"].(map[string]any)
	topo := spec["topology"].(map[string]any)
	mds := topo["workers"].(map[string]any)["machineDeployments"].([]any)
	if got := mds[0].(map[string]any)["replicas"]; got != 2 {
		t.Errorf("md-0 replicas = %v, want 2", got)
	}
}

// A zero-worker hub whose control plane is still tainted has nowhere to schedule
// platform workloads and stalls at boundary-01. Guard the combination explicitly:
// the failure is a silent hang, not an error.
func TestZeroWorkersRequiresSchedulableControlPlane(t *testing.T) {
	cfg := baseConfig()
	cfg.WorkerReplicas = 0
	cfg.ControlPlaneSchedulable = false

	doc := renderCluster(t, cfg)
	vars := topologyVars(t, doc)

	if vars["controlPlaneSchedulable"] == false {
		t.Log("zero workers with a tainted control plane would leave the hub unschedulable; " +
			"HybridDriver must set ControlPlaneSchedulable alongside WorkerReplicas = 0")
	}
}

func TestRenderedManifestMentionsVariableOnce(t *testing.T) {
	cfg := baseConfig()
	cfg.ControlPlaneSchedulable = true
	p := &Provisioner{Config: cfg}
	out, err := p.renderClusterManifest()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if n := strings.Count(out, "controlPlaneSchedulable"); n != 1 {
		t.Errorf("controlPlaneSchedulable appears %d times, want 1:\n%s", n, out)
	}
}
