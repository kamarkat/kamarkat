package bootstrap

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/constants"
)

// The Gateway API CRDs Cilium REQUIRES before it will run its Gateway API
// controller at all, taken from its own source rather than from documentation:
// operator/pkg/gateway-api/helpers/schemes.go, RequiredGVKs, at the deployed
// tag. All seven at gateway.networking.k8s.io/v1.
//
// ListenerSet, ServiceImport, TCPRoute, UDPRoute and InferencePool are in that
// file's AllOptionalKinds and are deliberately not delivered: the operator
// records which optional kinds it found and runs without the rest, and nothing
// on this platform declares one.
var requiredGatewayAPICRDs = []string{
	"gatewayclasses.gateway.networking.k8s.io",
	"gateways.gateway.networking.k8s.io",
	"httproutes.gateway.networking.k8s.io",
	"grpcroutes.gateway.networking.k8s.io",
	"tlsroutes.gateway.networking.k8s.io",
	"referencegrants.gateway.networking.k8s.io",
	"backendtlspolicies.gateway.networking.k8s.io",
}

// missingGatewayAPICRDs names the required CRDs this cluster does not have.
func (o *Orchestrator) missingGatewayAPICRDs(ctx context.Context, kubeconfig string) ([]string, error) {
	out, err := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig,
		"get", "crd", "-o", `jsonpath={range .items[*]}{.metadata.name}{"\n"}{end}`).Output()
	if err != nil {
		return nil, fmt.Errorf("could not list this cluster's CRDs: %w", err)
	}
	present := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		if name := strings.TrimSpace(line); name != "" {
			present[name] = true
		}
	}
	var missing []string
	for _, name := range requiredGatewayAPICRDs {
		if !present[name] {
			missing = append(missing, name)
		}
	}
	return missing, nil
}

// gatewayClassAccepted reports whether Cilium has accepted the GatewayClass, which
// is the only observable proof that its Gateway API controller is running.
//
// ASSERTED RATHER THAN ASSUMED, because the failure is silent in both directions
// that matter. An HTTPRoute whose controller never ran has no status at all, so
// ArgoCD reports it Progressing for ever and the Application it belongs to never
// leaves its first wave -- which is how a missing CRD surfaced as "Zitadel will not
// start" twelve hours after the Gateway was created (2026-11-18). And the hub's
// public endpoints are all behind this Gateway, so a box in this state has no
// ingress whatsoever while every object reads as applied.
func (o *Orchestrator) gatewayClassAccepted(ctx context.Context, kubeconfig string) error {
	if missing, err := o.missingGatewayAPICRDs(ctx, kubeconfig); err != nil {
		return err
	} else if len(missing) > 0 {
		return fmt.Errorf("this cluster is missing %d of the Gateway API CRDs Cilium "+
			"requires: %s.\n\nThey are delivered with the CNI, in the same addon "+
			"(internal/assets/manifests/addons/gateway-api-crds.yaml), because nothing "+
			"ArgoCD installs can arrive before the CNI that needs them. Cilium disables "+
			"its Gateway API controller when any is absent, so no Gateway, HTTPRoute or "+
			"GRPCRoute on this box will ever get a status",
			len(missing), strings.Join(missing, ", "))
	}

	out, err := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig,
		"get", "gatewayclass", constants.GatewayClassName, "-o",
		`jsonpath={.status.conditions[?(@.type=="Accepted")].status}`).Output()
	if err != nil {
		return fmt.Errorf("could not read GatewayClass %s, which Cilium accepts once its "+
			"Gateway API controller is running: %w", constants.GatewayClassName, err)
	}
	if status := strings.TrimSpace(string(out)); status != "True" {
		return fmt.Errorf("GatewayClass %s is not Accepted (%q). Cilium checks for the "+
			"Gateway API CRDs ONCE, at operator startup, and runs without the controller "+
			"for the life of that process if any was missing",
			constants.GatewayClassName, status)
	}
	return nil
}

// ensureGatewayController repairs the one case the CRDs arriving late produces.
//
// Cilium's operator probes for these CRDs at startup and never again: if they were
// absent then, it logs "Required GatewayAPI resources are not found", disables the
// controller, and that process goes on serving the CNI perfectly while no Gateway
// object is ever reconciled. Delivering the CRDs afterwards changes nothing until
// the operator restarts, which is why a box whose addon shipped an incomplete
// bundle stays broken after the bundle is fixed.
//
// A PREPARE STEP, not a gate: gates only read (see awaitBoundaryInventory). This is
// allowed to mutate because it is the repair, and it is bounded -- one restart per
// run, only when the CRDs are all present and the class is still not accepted.
//
// Does nothing on a box that was built with a complete bundle: the class is accepted
// before this runs, and the first check returns.
func (o *Orchestrator) ensureGatewayController(ctx context.Context, kubeconfig string) error {
	if err := o.gatewayClassAccepted(ctx, kubeconfig); err == nil {
		return nil
	}

	// WAITED FOR, not applied. Two things deliver these CRDs and neither is this
	// function: the CNI's ClusterResourceSet addon, at provisioning, and this
	// boundary's own gateway component, which reconciles the same objects on a box
	// whose addon shipped an incomplete bundle. Applying them here would be a third
	// deliverer, free to disagree with both about which version a cluster has.
	//
	// The wait exists because this step runs once the boundary's Applications have
	// been GENERATED, which is not the same as synced -- so on the box this repair
	// is for, the CRDs are moments away rather than absent.
	crdDeadline := time.Now().Add(5 * time.Minute)
	for {
		missing, err := o.missingGatewayAPICRDs(ctx, kubeconfig)
		if err != nil {
			return err
		}
		if len(missing) == 0 {
			break
		}
		if time.Now().After(crdDeadline) {
			fmt.Printf("[boundary03] the Gateway API CRDs have not arrived in 5m: %s\n",
				strings.Join(missing, ", "))
			return o.gatewayClassAccepted(ctx, kubeconfig)
		}
		fmt.Printf("[boundary03] waiting for %d Gateway API CRD(s) this boundary delivers\n",
			len(missing))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}

	fmt.Printf("[boundary03] the Gateway API CRDs are present but GatewayClass %s is not "+
		"accepted; restarting cilium-operator so it probes for them again\n",
		constants.GatewayClassName)
	restart := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig,
		"-n", constants.NamespaceKubeSystem, "rollout", "restart", "deployment/cilium-operator")
	if out, err := restart.CombinedOutput(); err != nil {
		return fmt.Errorf("could not restart cilium-operator to re-enable its Gateway API "+
			"controller: %w\n%s", err, strings.TrimSpace(string(out)))
	}

	// Waited for here rather than left to the readiness check, so the restart and
	// its outcome are reported together: a restart that did not help is a different
	// fault from one that was never performed.
	deadline := time.Now().Add(3 * time.Minute)
	for {
		if err := o.gatewayClassAccepted(ctx, kubeconfig); err == nil {
			fmt.Printf("[boundary03] ✓ GatewayClass %s accepted\n", constants.GatewayClassName)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("cilium-operator was restarted and GatewayClass %s is still not "+
				"accepted after 3m. Its startup log says which resources it looked for: "+
				"kubectl -n %s logs deployment/cilium-operator | grep -i gatewayapi",
				constants.GatewayClassName, constants.NamespaceKubeSystem)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
}
