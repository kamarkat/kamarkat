package components

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/constants"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/versions"
)

// Installer plants the Day-0 ArgoCD seed on the management cluster.
//
// ArgoCD is the ONLY thing this type installs. Infrastructure components (Cilium
// CNI, Hetzner CCM, CSI) are delivered declaratively by CAPI ClusterResourceSet
// per ADR-041 -- the CLI is forbidden from infrastructure provisioning. See
// internal/assets/manifests/addons/ for the hub payloads and
// manifests/spoke/spoke-bootstrap/ for the spoke ones.
//
// The seed is superseded immediately: the 'platform-argocd' Application in
// boundary 01 adopts this release at sync wave 1. Everything the seed sets must
// therefore match what that Application declares, or the adoption is a change and
// ArgoCD restarts itself mid-bootstrap.
type Installer struct {
	Kubeconfig string
	// EnvironmentSlug scopes the ADR-045 artifacts this installer writes.
	//
	// They were written to manifests/environments/base/generated/, which every
	// overlay inherits, so one file held whichever cluster bootstrapped last and
	// prod rendered dev's Infisical project. ADR-037 isolates environments by
	// directory; the artifact now sits in the environment's own.
	EnvironmentSlug string

	// GitopsDir is a checkout of the tenant's own repository.
	//
	// Set, ADR-045 artifacts are written there rather than into the platform's
	// repository. They are per-cluster instance data (ADR-062), so a tenant
	// bootstrap writing them into `zero-ops` would put one tenant's Infisical
	// coordinates in the types repository -- the defect removed from the charts,
	// reappearing in the bootstrap that produces them.
	GitopsDir string

	// ClusterName names the cluster a tenant artifact belongs to.
	ClusterName string
}

// artifactRoot is where this installer's generated artifacts belong: the
// tenant's repository when bootstrapping one, the working tree otherwise.
func (i *Installer) artifactRoot() (string, error) {
	if i.GitopsDir != "" {
		return i.GitopsDir, nil
	}
	return os.Getwd()
}

// GetArgoCDPassword retrieves ArgoCD admin password
func (i *Installer) GetArgoCDPassword(ctx context.Context) (string, error) {
	// Wait a bit for secret to be created
	time.Sleep(5 * time.Second)

	cmd := exec.CommandContext(ctx, "kubectl",
		"--kubeconfig", i.Kubeconfig,
		"get", "secret", "argocd-initial-admin-secret",
		"-n", constants.NamespaceOps,
		"-o", "jsonpath={.data.password}",
	)

	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get ArgoCD password: %w", err)
	}

	// Decode base64
	decoded, err := base64.StdEncoding.DecodeString(string(output))
	if err != nil {
		return "", fmt.Errorf("failed to decode password: %w", err)
	}

	return string(decoded), nil
}

// InstallArgoCD installs ArgoCD via Helm
func (i *Installer) InstallArgoCD(ctx context.Context) error {
	// Check if ArgoCD is already deployed and running
	checkCmd := exec.CommandContext(ctx, "kubectl", "--kubeconfig", i.Kubeconfig,
		"get", "deployment", "argocd-server",
		"--namespace", constants.NamespaceOps,
	)
	if err := checkCmd.Run(); err == nil {
		fmt.Println("[postboot] ✓ argocd already installed")
		return nil
	}

	fmt.Println("[postboot] Installing argocd...")

	// Add ArgoCD Helm repo
	cmd := exec.CommandContext(ctx, "helm", "repo", "add", "argo", "https://argoproj.github.io/argo-helm")
	if output, err := cmd.CombinedOutput(); err != nil {
		if !bytes.Contains(output, []byte("already exists")) {
			return fmt.Errorf("failed to add helm repo: %w\n%s", err, output)
		}
	}

	// Update repos
	cmd = exec.CommandContext(ctx, "helm", "repo", "update", "argo")
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to update helm repos: %w\n%s", err, output)
	}

	// Install ArgoCD
	//
	// Every --set below MUST have an identical declaration in the
	// 'platform-argocd' element of 01-platform-infra-appset.yaml. That
	// Application adopts this release at wave 1, and anything set here but not
	// declared there is reverted on the first sync -- silently, because the
	// revert looks like a normal reconcile.
	//
	// Removed on 2026-09-05: --set networkPolicy.enabled=true and
	// networkPolicy.defaultDeny=false. Neither is a key the argo-cd chart reads
	// (it gates on global.networkPolicy.create / .defaultDenyIngress), so Helm
	// accepted both silently and rendered ZERO NetworkPolicies. They were not
	// codified into Git because switching to the real keys renders five
	// NetworkPolicies where none existed -- a behaviour change on a live hub that
	// needs its own validation, not a silent ride-along on a parity fix.
	cmd = exec.CommandContext(ctx, "helm", "upgrade", "--install", "argocd", "argo/argo-cd",
		"--version", versions.ArgoCDChartVersion,
		"--namespace", constants.NamespaceOps,
		"--create-namespace",
		"--kubeconfig", i.Kubeconfig,
		"--set", "repoServer.env[0].name=ARGOCD_EXEC_TIMEOUT",
		"--set", "repoServer.env[0].value=600s",
		// Server-side DIFF, not just server-side apply.
		//
		// Applications here sync with ServerSideApply=true, which never writes
		// kubectl.kubernetes.io/last-applied-configuration. ArgoCD's default
		// diff is a client-side three-way merge that uses exactly that
		// annotation to tell "a field the user manages" from "a field the API
		// server defaulted". Without it every defaulted field reads as drift.
		//
		// The result on 2026-09-02 was eleven Applications permanently
		// OutOfSync-but-Healthy, all on an ExternalSecret, because the
		// ExternalSecret CRD defaults six fields nobody declares —
		// conversionStrategy, decodingStrategy, metadataPolicy, deletionPolicy,
		// engineVersion, mergePolicy. Nothing was wrong with any of them; the
		// diff could not be computed correctly, and real drift was
		// indistinguishable from that noise.
		//
		// This makes the diff use the same server-side apply dry-run the sync
		// uses, so fields no manager owns are ignored. The controller flag is
		// --server-side-diff-enabled, wired from this key via
		// ARGOCD_APPLICATION_CONTROLLER_SERVER_SIDE_DIFF, and defaults false.
		"--set", `configs.params.controller\.diff\.server\.side=true`,
		"--wait",
		// 25m, not 10m.
		//
		// This budget covers pulling every ArgoCD image onto whichever node the
		// scheduler picks, and on a hybrid box that node is the tenant's own
		// hardware -- so the pull crosses a home internet connection rather than
		// a datacenter backplane. Measured on one: the pre-upgrade hook's image
		// (quay.io/argoproj/argocd:v2.14.1) went ImagePullBackOff, retried, and
		// then reported "Successfully pulled image in 59.539s". The hook Job
		// completed; Helm had already given up at 10m and failed the release with
		// "pre-upgrade hooks failed: timed out waiting for the condition".
		//
		// A failed release is not free: it leaves the release in `failed`, which
		// the next run has to upgrade over. Paying for a slow pull is cheaper
		// than a release that has to be recovered.
		"--timeout", "25m",
	)

	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to install argocd: %w\n%s", err, output)
	}

	fmt.Println("[postboot] ✓ argocd ready")
	return nil
}

// Cilium CNI is now managed by CAPI ClusterResourceSet per ADR-041.
// The CLI is forbidden from infrastructure provisioning (CNI, CCM, CSI).
// See: manifests/clusters/capi/cluster-resource-set/cilium/

// Hetzner CCM is now managed by CAPI ClusterResourceSet per ADR-041.
// The CLI is forbidden from infrastructure provisioning (CNI, CCM, CSI).
// See: manifests/clusters/capi/cluster-resource-set/ccm-hetzner/
