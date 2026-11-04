package bootstrap

import (
	"context"
	"os"
	"strings"
	"testing"
)

// The tailnet credential must be re-staged on the pivoted hub.
//
// It is written at capi-init into the BOOTSTRAP cluster, because the hub's own
// ClusterClass reads it through contentFrom.secret while the hub Cluster is
// created. `clusterctl move` carries CAPI-owned objects; a plain Secret is not
// one, so it does not survive the pivot -- and nothing notices, because the hub
// is already built.
//
// The first object to need it on the hub is the first SPOKE:
//
//	BootstrapReady False  secret not found: platform-capi/tailscale-hybrid-psk
//
// Every spoke machine then sits Pending for ever, control plane included, so the
// spoke has no API server and the burst autoscaler crash-loops against an
// endpoint that will never answer.
func TestTheTailnetCredentialIsRestagedAfterPivot(t *testing.T) {
	src, err := os.ReadFile("provider_cloud.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	start := strings.Index(body, "func (p *CloudProvider) PivotReady(")
	if start < 0 {
		t.Fatal("PivotReady is gone")
	}
	fn := body[start:]
	fn = fn[:strings.Index(fn, "\n}\n")]
	if !strings.Contains(fn, "stageTailnetCredentialOnHub") {
		t.Error("PivotReady does not re-stage the tailnet credential; every spoke " +
			"created on this hub will fail to render its KubeadmConfig")
	}
}

// Staged into platform-capi, which is where the ClusterClass resolves it.
func TestTheTailnetCredentialIsStagedIntoTheCAPINamespace(t *testing.T) {
	src, err := os.ReadFile("provider_cloud.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	start := strings.Index(body, "func (p *CloudProvider) stageTailnetCredentialOnHub(")
	if start < 0 {
		t.Fatal("stageTailnetCredentialOnHub is gone")
	}
	fn := body[start:]
	fn = fn[:strings.Index(fn, "\n}\n")]
	if !strings.Contains(fn, "constants.NamespaceCAPI") {
		t.Error("the credential is not staged into the CAPI namespace, which is the " +
			"only place the ClusterClass looks for it")
	}
}

// EVERY driver that can have a tailnet must expose the stager -- and the one that
// matters is the hybrid driver, not the Hetzner one.
//
// This test previously asserted only *HetznerDriver, which passed while the bug
// was live. HybridDriver holds its Hetzner driver as a NAMED field, not an
// embedded one, so nothing is promoted; PivotReady reaches the stager by type
// assertion, an assertion against a type missing the method does not fail
// loudly, and pivot-ready reported success in 15 seconds having staged nothing.
func TestBothDriversExposeTheStager(t *testing.T) {
	type stager interface {
		StageTailscaleCredentials(context.Context, string, string) error
	}
	var _ stager = (*HetznerDriver)(nil)
	var _ stager = (*HybridDriver)(nil)
}

// The same trap for every other capability PivotReady reaches by assertion. A
// named field promotes nothing, so each must be forwarded on HybridDriver by
// hand, and a missing one is silent.
func TestHybridForwardsEveryAssertedCapability(t *testing.T) {
	var d any = (*HybridDriver)(nil)

	if _, ok := d.(interface{ OnPremRequested() bool }); !ok {
		t.Error("HybridDriver does not expose OnPremRequested; on-prem phases silently skip")
	}
	if _, ok := d.(interface {
		StageTailscaleCredentials(context.Context, string, string) error
	}); !ok {
		t.Error("HybridDriver does not expose StageTailscaleCredentials; pivot-ready " +
			"stages no tailnet credential and reports success")
	}
	if _, ok := d.(interface{ CAPIPlacement() string }); !ok {
		t.Error("HybridDriver does not expose CAPIPlacement; the CAPI controllers " +
			"keep whatever placement they already have and pivot-ready reports success")
	}
}

// CAPI placement is hybrid-only, and must stay that way.
//
// Only a box with on-prem nodes has a placement question to answer. On a
// pure-Hetzner hub every node is cloud, so the rule is a no-op and the driver
// should not carry it -- one fewer thing that can be wrong on the box that does
// not need it.
func TestCAPIPlacementIsHybridOnly(t *testing.T) {
	var hybrid any = (*HybridDriver)(nil)
	if _, ok := hybrid.(interface{ CAPIPlacement() string }); !ok {
		t.Error("the hybrid driver does not declare CAPI placement; its controllers " +
			"stay wherever the scheduler put them, which on this box is the tenant's premises")
	}

	var hetzner any = (*HetznerDriver)(nil)
	if _, ok := hetzner.(interface{ CAPIPlacement() string }); ok {
		t.Error("the Hetzner driver declares CAPI placement; a pure-cloud hub has " +
			"no on-prem nodes and needs no rule")
	}
}

// The selector is applied after the pivot and before the readiness wait.
//
// Not at capi-init: that installs the providers into the KIND cluster, where no
// node carries a placement label, so every provider would be Pending and the
// install would fail. Not after WaitForReady either -- the wait is precisely what
// was failing, so the controllers must be given somewhere to run first.
func TestCAPIPlacementHappensBeforeTheReadinessWait(t *testing.T) {
	src, err := os.ReadFile("provider_cloud.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	start := strings.Index(body, "func (p *CloudProvider) PivotReady(")
	if start < 0 {
		t.Fatal("PivotReady is gone")
	}
	fn := body[start:]
	fn = fn[:strings.Index(fn, "\n}\n")]

	// Code only. The comment explaining this ordering names WaitForReady, and a
	// search over comments finds the explanation before the call it describes --
	// which is exactly how this test first failed against correct code.
	var code strings.Builder
	for _, line := range strings.Split(fn, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		code.WriteString(line)
		code.WriteString("\n")
	}
	fn = code.String()

	place := strings.Index(fn, "placeCAPIControllers")
	wait := strings.Index(fn, "WaitForReady")
	if place < 0 || wait < 0 {
		t.Fatal("PivotReady no longer both places the controllers and waits for them")
	}
	if place > wait {
		t.Error("the controllers are placed after the readiness wait, so the wait " +
			"still runs against controllers on the saturated node")
	}
}

// The selector goes on the provider CRs, not the Deployments: capi-operator owns
// those Deployments and would revert a direct patch on its next reconcile.
func TestCAPIPlacementPatchesTheProviderCRs(t *testing.T) {
	src, err := os.ReadFile("provider_cloud.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	start := strings.Index(body, "func (p *CloudProvider) placeCAPIControllers(")
	if start < 0 {
		t.Fatal("placeCAPIControllers is gone")
	}
	fn := body[start:]
	fn = fn[:strings.Index(fn, "\n}\n")]

	for _, kind := range []string{"coreprovider", "bootstrapprovider", "controlplaneprovider", "infrastructureprovider"} {
		if !strings.Contains(fn, kind) {
			t.Errorf("%s is not placed; one unplaced provider keeps the load on the "+
				"control plane", kind)
		}
	}
	if strings.Contains(fn, `"deployment"`) && strings.Contains(fn, `"patch", "deployment"`) {
		t.Error("the patch targets Deployments directly; capi-operator reverts those")
	}
}

// kubectl patch takes a resource NAME, never --all.
//
// Passing --all fails with "unknown flag: --all" rather than patching
// everything, which is how the CAPI placement first shipped: pivot-ready died
// in 0s on a flag that does not exist.
func TestCAPIPlacementPatchesByName(t *testing.T) {
	src, err := os.ReadFile("provider_cloud.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	start := strings.Index(body, "func (p *CloudProvider) placeCAPIControllers(")
	if start < 0 {
		t.Fatal("placeCAPIControllers is gone")
	}
	fn := body[start:]
	fn = fn[:strings.Index(fn, "\n}\n")]

	var code strings.Builder
	for _, line := range strings.Split(fn, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		code.WriteString(line)
		code.WriteString("\n")
	}
	c := code.String()

	if strings.Contains(c, `"--all"`) {
		t.Error(`the patch passes --all, which kubectl patch does not accept`)
	}
	if !strings.Contains(c, `"get", kind`) {
		t.Error("the providers are not listed before patching, so there are no names " +
			"to patch by")
	}
}
