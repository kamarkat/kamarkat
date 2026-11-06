package bootstrap

import (
	"strings"
	"testing"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/cluster"
)

// Hub topology is keyed on the PROVIDER. Choosing --provider=hybrid is itself the
// statement that worker capacity comes from home-lab hardware, so the hub runs a
// single Hetzner control-plane node and nothing else.
func TestHybridDriverProducesSingleNodeHub(t *testing.T) {
	d := &HybridDriver{
		Driver:        &HetznerDriver{Region: "hel1", OS: "ubuntu", NetworkCIDR: "10.0.0.0/16"},
		OnPremEnabled: true,
	}

	var cfg cluster.Config
	d.PopulateClusterConfig(&cfg)

	if cfg.WorkerReplicas != 0 {
		t.Errorf("WorkerReplicas = %d, want 0 (hybrid hub workers are home-lab nodes)", cfg.WorkerReplicas)
	}
	// With home workers the control plane KEEPS its taint: ADR-046 §11 places every
	// platform workload on worker nodes, and the on-prem-join phase guarantees
	// one exists before boundary-01.
	if cfg.ControlPlaneSchedulable {
		t.Error("ControlPlaneSchedulable = true with home workers enabled: the control plane must keep its ADR-014 taint")
	}
	if cfg.CiliumOperatorReplicas != 1 {
		t.Errorf("CiliumOperatorReplicas = %d, want 1 (hostPorts stop two replicas sharing one node)", cfg.CiliumOperatorReplicas)
	}
}

// The pure-Hetzner path is the tested one and must be untouched by any of the
// hybrid work: real workers, a tainted control plane, and no operator rescaling.
func TestHetznerDriverKeepsMultiNodeHub(t *testing.T) {
	d := &HetznerDriver{Region: "hel1", OS: "ubuntu", NetworkCIDR: "10.0.0.0/16"}

	var cfg cluster.Config
	d.PopulateClusterConfig(&cfg)

	if cfg.WorkerReplicas != 2 {
		t.Errorf("WorkerReplicas = %d, want 2 (unchanged Hetzner hub)", cfg.WorkerReplicas)
	}
	if cfg.ControlPlaneSchedulable {
		t.Error("ControlPlaneSchedulable = true: the Hetzner hub must keep its control-plane taint")
	}
	if cfg.CiliumOperatorReplicas != 0 {
		t.Errorf("CiliumOperatorReplicas = %d, want 0 (leave the shared addon at its HA default of 2)", cfg.CiliumOperatorReplicas)
	}
}

// Zero workers and a tainted control plane is the combination that hangs a
// bootstrap with every pod Pending, so the two settings must never drift apart.
func TestZeroWorkersImpliesSchedulableControlPlane(t *testing.T) {
	homeWorkers := map[string]bool{"hybrid": true, "hetzner": false}
	for name, d := range map[string]interface {
		PopulateClusterConfig(*cluster.Config)
	}{
		"hybrid": &HybridDriver{
			Driver:        &HetznerDriver{Region: "hel1", OS: "ubuntu", NetworkCIDR: "10.0.0.0/16"},
			OnPremEnabled: true,
		},
		"hetzner": &HetznerDriver{Region: "hel1", OS: "ubuntu", NetworkCIDR: "10.0.0.0/16"},
	} {
		var cfg cluster.Config
		d.PopulateClusterConfig(&cfg)
		if cfg.WorkerReplicas == 0 && !cfg.ControlPlaneSchedulable && !homeWorkers[name] {
			t.Errorf("%s: 0 Hetzner workers, a tainted control plane and no home workers — nothing can schedule", name)
		}
	}
}

// A hybrid cell with NO home workers has no other node at all, so the control
// plane has to carry the platform.
func TestHybridWithoutHomeWorkersUntaintsControlPlane(t *testing.T) {
	d := &HybridDriver{
		Driver:        &HetznerDriver{Region: "hel1", OS: "ubuntu", NetworkCIDR: "10.0.0.0/16"},
		OnPremEnabled: false,
	}

	var cfg cluster.Config
	d.PopulateClusterConfig(&cfg)

	if cfg.WorkerReplicas != 0 {
		t.Errorf("WorkerReplicas = %d, want 0", cfg.WorkerReplicas)
	}
	if !cfg.ControlPlaneSchedulable {
		t.Error("ControlPlaneSchedulable = false with no workers of any kind: the hub could not schedule anything")
	}
}

// The rescale must hit the cilium-operator Deployment only. The addon also
// carries the cilium DaemonSet and other resources; a blanket replacement would
// corrupt them.
func TestScaleCiliumOperatorTargetsOnlyTheOperator(t *testing.T) {
	const manifest = `apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: cilium
spec:
  selector:
    matchLabels:
      k8s-app: cilium
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: cilium-operator
spec:
  replicas: 2
  selector:
    matchLabels:
      io.cilium/app: operator
`

	got := cluster.ScaleCiliumOperatorForTest(manifest, 1)

	if !strings.Contains(got, "replicas: 1") {
		t.Errorf("operator was not rescaled:\n%s", got)
	}
	if strings.Contains(got, "replicas: 2") {
		t.Errorf("a replicas: 2 survived the rewrite:\n%s", got)
	}
	if !strings.Contains(got, "kind: DaemonSet") || !strings.Contains(got, "k8s-app: cilium") {
		t.Errorf("the DaemonSet was damaged:\n%s", got)
	}
}

// replicas <= 0 means "leave it alone" — the multi-node default.
func TestScaleCiliumOperatorNoOpOnZero(t *testing.T) {
	const manifest = "kind: Deployment\nmetadata:\n  name: cilium-operator\nspec:\n  replicas: 2\n"
	if got := cluster.ScaleCiliumOperatorForTest(manifest, 0); got != manifest {
		t.Errorf("manifest changed when replicas was 0:\n%s", got)
	}
}

// Every environment provisions cloud workers by default, development included.
//
// Development briefly defaulted to zero so its capacity would come from the
// tenant's own hardware (ADR-075). That is the right end state and the wrong
// default to reach it by: it made on-prem hardware a precondition for the simplest
// box the platform can build. A tenant who wants it asks with --workers 0.
// Two, declared, with no environment in the question (ADR-111 Revision 7 §3).
// This iterated dev, stg, prod and "" to assert the count never varied -- which
// documented that the driver's Environment field changed nothing, so the field
// is gone and the dimension with it.
func TestHetznerWorkerReplicasIsDeclaredNotDerived(t *testing.T) {
	d := &HetznerDriver{}
	if got := d.PlannedWorkerReplicas(); got != 2 {
		t.Errorf("PlannedWorkerReplicas() = %d, want 2", got)
	}
	zero := 0
	d = &HetznerDriver{WorkerReplicas: &zero}
	if got := d.PlannedWorkerReplicas(); got != 0 {
		t.Errorf("an explicit zero must survive: PlannedWorkerReplicas() = %d, want 0", got)
	}
}

// Hybrid provisions none at all: the cell exists so capacity comes from the
// tenant's own hardware.
func TestHybridWorkerReplicasAlwaysZero(t *testing.T) {
	{
		d := &HybridDriver{Driver: &HetznerDriver{}}
		if got := d.PlannedWorkerReplicas(); got != 0 {
			t.Errorf("hybrid PlannedWorkerReplicas() = %d, want 0", got)
		}
	}
}

// A box with no cloud workers and no on-prem nodes has no worker capacity at all,
// and the control plane does not make up the difference: it keeps its taint, and
// every platform workload pins node-role.kubernetes.io/worker, which a
// control-plane node does not carry (ADR-014). Such a box comes up with its
// control plane Ready and everything else Pending, roughly ninety minutes in.
//
// The check used to key on the provider being hybrid. Development boxes now
// provision no cloud workers on hetzner too (ADR-075), so it keys on capacity.
func TestBootstrapRefusesABoxWithNoWorkerCapacity(t *testing.T) {
	cases := []struct {
		name     string
		provider Provider
		wantErr  bool
	}{
		{
			// Explicitly asked for no cloud workers, and no on-prem to replace them.
			name: "no cloud workers and no on-prem has nothing to schedule on",
			provider: NewCloudProvider(
				&HetznerDriver{WorkerReplicas: intPtr(0)}, "c", false),
			wantErr: true,
		},
		{
			name:     "hetzner provisions cloud workers by default",
			provider: NewCloudProvider(&HetznerDriver{}, "c", false),
			wantErr:  false,
		},
		{
			name: "on-prem alone is capacity, with no cloud workers",
			provider: NewCloudProvider(
				&HybridDriver{Driver: &HetznerDriver{}, OnPremEnabled: true}, "c", false),
			wantErr: false,
		},
		{
			name: "hybrid without on-prem has nothing at all",
			provider: NewCloudProvider(
				&HybridDriver{Driver: &HetznerDriver{}}, "c", false),
			wantErr: true,
		},
	}

	for _, tc := range cases {
		o := &Orchestrator{Provider: tc.provider}
		err := o.checkPlacementCapacityRequested()
		if tc.wantErr && err == nil {
			t.Errorf("%s: expected a refusal, got none", tc.name)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("%s: expected no refusal, got: %v", tc.name, err)
		}
	}
}

// An explicit count wins over the declared default, including an explicit zero.
// This is what lets a tenant with no hardware run a box on cloud capacity, and a
// tenant with plenty run one on none.
func TestExplicitWorkerCountOverridesTheDefault(t *testing.T) {
	zero, four := 0, 4
	if got := (&HetznerDriver{WorkerReplicas: &zero}).PlannedWorkerReplicas(); got != 0 {
		t.Errorf("explicit zero = %d, want 0", got)
	}
	if got := (&HetznerDriver{WorkerReplicas: &four}).PlannedWorkerReplicas(); got != 4 {
		t.Errorf("explicit four = %d, want 4", got)
	}
	// And a driver that never had the field set still gets the declared default.
	// An int sentinel got this wrong: Go's zero value read as an explicit zero, so
	// every box silently provisioned nothing.
	if got := (&HetznerDriver{}).PlannedWorkerReplicas(); got != 2 {
		t.Errorf("unset override in prod = %d, want 2", got)
	}
}

// On-prem is a capability of the box, not of the hybrid provider (ADR-075).
//
// It lived only on HybridDriver, so `--provider hetzner --on-prem` set a flag
// nothing read: the pre-flight capacity check and the on-prem join phase both ask
// the provider, a hetzner driver could not answer, and the answer was taken as no.
func TestOnPremIsAnsweredByBothProviders(t *testing.T) {
	cases := []struct {
		name string
		p    Provider
		want bool
	}{
		{"hetzner with on-prem", NewCloudProvider(&HetznerDriver{OnPremEnabled: true}, "c", false), true},
		{"hetzner without", NewCloudProvider(&HetznerDriver{}, "c", false), false},
		{"hybrid with on-prem",
			NewCloudProvider(&HybridDriver{Driver: &HetznerDriver{}, OnPremEnabled: true}, "c", false), true},
		{"hybrid without",
			NewCloudProvider(&HybridDriver{Driver: &HetznerDriver{}}, "c", false), false},
	}
	for _, tc := range cases {
		hw, ok := tc.p.(interface{ OnPremRequested() bool })
		if !ok {
			t.Errorf("%s: provider cannot answer OnPremRequested at all", tc.name)
			continue
		}
		if got := hw.OnPremRequested(); got != tc.want {
			t.Errorf("%s: OnPremRequested() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The control plane's tailnet name must carry the cluster's name. Moving the
// derivation onto the embedded driver would have silently regressed every hybrid
// box to the "hub-cp" fallback, because only the outer driver was given a name.
func TestControlPlaneTailnetHostnameCarriesTheClusterName(t *testing.T) {
	if got := (&HetznerDriver{ClusterName: "devbox"}).hubTailnetHostname(); got != "devbox-cp" {
		t.Errorf("hubTailnetHostname() = %q, want %q", got, "devbox-cp")
	}
	if got := (&HetznerDriver{}).hubTailnetHostname(); got != "hub-cp" {
		t.Errorf("unnamed cluster = %q, want the %q fallback", got, "hub-cp")
	}
}

func intPtr(n int) *int { return &n }

// The platform's own box has an instance repository even with no --gitops-dir.
//
// Removing the fallback made every non-tenant bootstrap fail before the seed was
// applied -- boundaries 05 and 06 never rendered, and boundary 01 waited ten
// minutes for ApplicationSets that could not appear. ADR-062 keeps the name
// `fleet-registry` for exactly this repository, so it is the box naming itself
// rather than a default standing in for a tenant.
func TestPlatformOwnBoxResolvesItsInstanceRepository(t *testing.T) {
	o := &Orchestrator{}
	repo, err := o.instanceRepoURL()
	if err != nil {
		t.Fatalf("a box with no --gitops-dir must still resolve one: %v", err)
	}
	if repo == "" {
		t.Fatal("resolved an empty instance repository")
	}

	// And the ArgoCD credential must be scopeable from it, or the seed's
	// Applications cannot read the repository they reconcile.
	org, err := o.boxOrgURL()
	if err != nil {
		t.Fatalf("boxOrgURL: %v", err)
	}
	if org != "https://github.com/kamarkat" {
		t.Errorf("boxOrgURL() = %q, want the organisation that owns it", org)
	}
}
