package pivot

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/kamarkat/kamarkat/internal/assets"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/binaries"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/constants"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/health"
)

type resourceMetadata struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name        string            `yaml:"name"`
		Annotations map[string]string `yaml:"annotations"`
	} `yaml:"metadata"`
}

// Orchestrator manages CAPI pivot from bootstrap to management cluster
type Orchestrator struct {
	BootstrapKubeconfig string
	// BootstrapContext names the bootstrap cluster explicitly. A kubeconfig
	// alone is not enough: its current-context is ambient state that an earlier
	// phase can retarget (bootstrap writes the hub kubeconfig, and
	// hub-bootstrap.sh exports KUBECONFIG to it whenever that file already
	// exists — true on every resumed run). Inheriting it sent these calls to the
	// hub, where platform-capi does not exist yet, and the failure read as a
	// missing namespace rather than as the wrong-cluster error it was.
	// Empty falls back to current-context.
	BootstrapContext string
	ClusterName      string
	Namespace        string
	OSType           string // node OS; "ubuntu" is the only supported value (ADR-001)
	Debug            bool
}

// ExecuteMove performs the pivot move operation (without waiting for ready)
func (o *Orchestrator) ExecuteMove(ctx context.Context) (string, error) {
	// 0. Ensure clusterctl is installed
	clusterctlMgr, err := binaries.NewClusterctlManager()
	if err != nil {
		return "", fmt.Errorf("failed to create clusterctl manager: %w", err)
	}

	if err := clusterctlMgr.EnsureInstalled(ctx); err != nil {
		return "", fmt.Errorf("failed to install clusterctl: %w", err)
	}

	// 1. Retrieve Management Cluster kubeconfig
	mgmtKubeconfig, err := o.getKubeconfig(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to retrieve kubeconfig: %w", err)
	}

	// 2. Install cluster-api-operator on Management Cluster
	if err := o.installOperatorOnMgmt(ctx, mgmtKubeconfig); err != nil {
		return "", fmt.Errorf("failed to install operator on mgmt cluster: %w", err)
	}

	// 2.5. Create namespace and Hetzner credentials secret (required before move)
	fmt.Println("[pivot] Creating namespace...")
	nsCmd := exec.CommandContext(ctx, "kubectl",
		"--kubeconfig", mgmtKubeconfig,
		"create", "namespace", o.Namespace,
	)
	if output, err := nsCmd.CombinedOutput(); err != nil && !bytes.Contains(output, []byte("AlreadyExists")) {
		return "", fmt.Errorf("failed to create namespace: %w\n%s", err, output)
	}

	fmt.Println("[pivot] Creating Hetzner credentials secret...")
	hcloudToken := os.Getenv("HCLOUD_TOKEN")
	if hcloudToken == "" {
		return "", fmt.Errorf("HCLOUD_TOKEN environment variable not set")
	}

	// Read template from file
	tmplData, err := assets.ReadManifest("secrets/hetzner-credentials.yaml")
	if err != nil {
		return "", fmt.Errorf("failed to read hetzner secret template: %w", err)
	}

	// Parse and execute template
	tmpl, err := template.New("hetzner-secret").Parse(string(tmplData))
	if err != nil {
		return "", fmt.Errorf("failed to parse template: %w", err)
	}

	var buf bytes.Buffer
	data := struct {
		Namespace   string
		HCloudToken string
	}{
		Namespace:   o.Namespace,
		HCloudToken: hcloudToken,
	}

	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("failed to execute template: %w", err)
	}

	applyCmd := exec.CommandContext(ctx, "kubectl",
		"--kubeconfig", mgmtKubeconfig,
		"apply", "-f", "-",
	)
	applyCmd.Stdin = &buf
	if output, err := applyCmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("failed to create hetzner secret: %w\n%s", err, output)
	}

	// 3. Execute clusterctl move
	if err := o.move(ctx, mgmtKubeconfig); err != nil {
		return "", fmt.Errorf("clusterctl move failed: %w", err)
	}

	return mgmtKubeconfig, nil
}

// WaitForReady waits for providers and cluster to be ready after move
func (o *Orchestrator) WaitForReady(ctx context.Context, mgmtKubeconfig string) error {
	// Recreate Hetzner credentials secret (clusterctl move doesn't move secrets)
	fmt.Println("[pivot] Recreating Hetzner credentials secret...")
	hcloudToken := os.Getenv("HCLOUD_TOKEN")
	if hcloudToken == "" {
		return fmt.Errorf("HCLOUD_TOKEN environment variable required")
	}

	// Read template from file
	tmplData, err := assets.ReadManifest("secrets/hetzner-credentials.yaml")
	if err != nil {
		return fmt.Errorf("failed to read hetzner secret template: %w", err)
	}

	// Parse and execute template
	tmpl, err := template.New("hetzner-secret").Parse(string(tmplData))
	if err != nil {
		return fmt.Errorf("failed to parse template: %w", err)
	}

	var buf bytes.Buffer
	data := struct {
		Namespace   string
		HCloudToken string
	}{
		Namespace:   o.Namespace,
		HCloudToken: hcloudToken,
	}

	if err := tmpl.Execute(&buf, data); err != nil {
		return fmt.Errorf("failed to execute template: %w", err)
	}

	cmd := exec.CommandContext(ctx, "kubectl", "apply",
		"--kubeconfig", mgmtKubeconfig,
		"-f", "-",
	)
	cmd.Stdin = &buf
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to create hetzner secret: %w\n%s", err, output)
	}

	// Wait for providers ready on Management Cluster
	if err := o.waitForProvidersReady(ctx, mgmtKubeconfig, 20*time.Minute); err != nil {
		return fmt.Errorf("providers not ready after pivot: %w", err)
	}

	// Wait for cluster ready on Management Cluster
	if err := o.waitForClusterReady(ctx, mgmtKubeconfig, 20*time.Minute); err != nil {
		return fmt.Errorf("cluster not ready after pivot: %w", err)
	}

	return nil
}

// Execute performs the complete pivot operation (deprecated, use ExecuteMove + WaitForReady)
func (o *Orchestrator) Execute(ctx context.Context) (string, error) {
	// 0. Ensure clusterctl is installed
	clusterctlMgr, err := binaries.NewClusterctlManager()
	if err != nil {
		return "", fmt.Errorf("failed to create clusterctl manager: %w", err)
	}

	if err := clusterctlMgr.EnsureInstalled(ctx); err != nil {
		return "", fmt.Errorf("failed to install clusterctl: %w", err)
	}

	// 1. Retrieve Management Cluster kubeconfig
	mgmtKubeconfig, err := o.getKubeconfig(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to retrieve kubeconfig: %w", err)
	}

	// 2. Install cluster-api-operator on Management Cluster
	if err := o.installOperatorOnMgmt(ctx, mgmtKubeconfig); err != nil {
		return "", fmt.Errorf("failed to install operator on mgmt cluster: %w", err)
	}

	// 4. Execute clusterctl move
	if err := o.move(ctx, mgmtKubeconfig); err != nil {
		return "", fmt.Errorf("clusterctl move failed: %w", err)
	}

	// Wait for ready
	if err := o.WaitForReady(ctx, mgmtKubeconfig); err != nil {
		return "", err
	}

	return mgmtKubeconfig, nil
}

func (o *Orchestrator) getKubeconfig(ctx context.Context) (string, error) {
	secretName := fmt.Sprintf("%s-kubeconfig", o.ClusterName)

	// Always fetch fresh kubeconfig (load balancer IP may have changed)
	args := []string{"--kubeconfig", o.BootstrapKubeconfig}
	if o.BootstrapContext != "" {
		args = append(args, "--context", o.BootstrapContext)
	}
	args = append(args,
		"get", "secret", secretName,
		"-n", o.Namespace,
		"-o", "jsonpath={.data.value}",
	)
	cmd := exec.CommandContext(ctx, "kubectl", args...)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("kubectl get secret failed: %w\nOutput: %s", err, string(output))
	}

	decoded, decodeErr := base64.StdEncoding.DecodeString(string(output))
	if decodeErr != nil {
		return "", decodeErr
	}

	// Save to k8-secrets/kubeconfig directory
	kubeconfigDir := "k8-secrets/kubeconfig"
	if err := os.MkdirAll(kubeconfigDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create kubeconfig directory: %w", err)
	}

	path := filepath.Join(kubeconfigDir, fmt.Sprintf("%s.kubeconfig", o.ClusterName))
	if err = os.WriteFile(path, decoded, 0600); err != nil {
		return "", err
	}

	fmt.Printf("[pivot] ✓ Kubeconfig saved to %s\n", path)
	return path, nil
}

func (o *Orchestrator) installOperatorOnMgmt(ctx context.Context, mgmtKubeconfig string) error {
	// 1. Install cert-manager (required for operator webhooks)
	fmt.Println("[pivot] Installing cert-manager...")
	certMgrManifest, err := assets.ReadManifest("core/cert-manager/install.yaml")
	if err != nil {
		return fmt.Errorf("failed to read cert-manager manifest: %w", err)
	}

	// Use create with --save-config for initial install (faster than apply)
	cmd := exec.CommandContext(ctx, "kubectl", "create",
		"--kubeconfig", mgmtKubeconfig,
		"--save-config",
		"--validate=false",
		"-f", "-",
	)
	cmd.Stdin = bytes.NewReader(certMgrManifest)
	output, err := cmd.CombinedOutput()
	if err != nil && !bytes.Contains(output, []byte("AlreadyExists")) {
		return fmt.Errorf("cert-manager install failed: %w\n%s", err, output)
	}
	fmt.Println("[pivot] ✓ cert-manager manifests applied")

	// 2. Wait for all cert-manager deployments
	fmt.Println("[pivot] Waiting for cert-manager API...")
	deployments := []string{"cert-manager", "cert-manager-webhook", "cert-manager-cainjector"}
	for _, dep := range deployments {
		cmd = exec.CommandContext(ctx, "kubectl",
			"--kubeconfig", mgmtKubeconfig,
			"wait", "deployment",
			"-n", constants.NamespaceCertManager,
			dep,
			"--for=condition=Available",
			"--timeout=5m",
		)
		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("cert-manager deployment %s not ready: %w\n%s", dep, err, output)
		}
	}

	fmt.Println("[pivot] Waiting for cert-manager webhook to become fully functional...")
	dummyIssuer := []byte(`
apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: test-webhook-readiness
  namespace: default
spec:
  selfSigned: {}
`)

	deadline := time.Now().Add(5 * time.Minute)
	webhookReady := false
	for time.Now().Before(deadline) {
		cmd = exec.CommandContext(ctx, "kubectl",
			"--kubeconfig", mgmtKubeconfig,
			"apply", "--dry-run=server", "-f", "-",
		)
		cmd.Stdin = bytes.NewReader(dummyIssuer)
		if err := cmd.Run(); err == nil {
			webhookReady = true
			break
		}
		time.Sleep(2 * time.Second)
	}

	if !webhookReady {
		return fmt.Errorf("timeout waiting for cert-manager webhook to become fully functional")
	}

	// 3. Install operator deterministically
	fmt.Println("[pivot] Installing cluster-api-operator...")
	operatorManifest, err := assets.ReadManifest("core/capi-operator/install.yaml")
	if err != nil {
		return fmt.Errorf("failed to read operator manifest: %w", err)
	}

	// Clear the placeholder caBundle (base64 of '\n') to empty so Kubernetes accepts
	// the apply; cert-manager cainjector will inject the real CA bundle once the cert is ready.
	operatorManifest = bytes.ReplaceAll(operatorManifest, []byte("caBundle: Cg=="), []byte(`caBundle: ""`))

	if err := o.applyManifestSafely(ctx, mgmtKubeconfig, operatorManifest); err != nil {
		return err
	}

	// 4. Wait for operator ready
	fmt.Println("[pivot] Waiting for operator...")
	cmd = exec.CommandContext(ctx, "kubectl",
		"--kubeconfig", mgmtKubeconfig,
		"wait", "deployment",
		"-n", constants.NamespaceCAPI,
		"capi-operator-controller-manager",
		"--for=condition=Available",
		"--timeout=3m",
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("operator not ready: %w\n%s", err, output)
	}

	// 5. Apply provider manifests
	fmt.Println("[pivot] Applying provider manifests...")
	if err := o.applyProviders(ctx, mgmtKubeconfig); err != nil {
		return fmt.Errorf("failed to apply providers: %w", err)
	}

	// 6. Wait for CAPI CRDs to be installed by providers
	fmt.Println("[pivot] Waiting for CAPI CRDs...")
	if err := o.waitForCAPICRDs(ctx, mgmtKubeconfig, 5*time.Minute); err != nil {
		return fmt.Errorf("CAPI CRDs not ready: %w", err)
	}

	return nil
}

func (o *Orchestrator) applyManifestSafely(ctx context.Context, kubeconfig string, manifest []byte) error {
	docs := bytes.Split(manifest, []byte("\n---"))
	var prereqs, injectables, deployments [][]byte
	var crdsToWait []string
	var crdsToInject []string

	for _, doc := range docs {
		doc = bytes.TrimSpace(doc)
		if len(doc) == 0 {
			continue
		}

		var m resourceMetadata
		if err := yaml.Unmarshal(doc, &m); err != nil {
			prereqs = append(prereqs, doc)
			continue
		}

		switch m.Kind {
		case "CustomResourceDefinition":
			injectables = append(injectables, doc)
			crdsToWait = append(crdsToWait, m.Metadata.Name)
			if m.Metadata.Annotations != nil && m.Metadata.Annotations["cert-manager.io/inject-ca-from"] != "" {
				crdsToInject = append(crdsToInject, m.Metadata.Name)
			}
		case "MutatingWebhookConfiguration", "ValidatingWebhookConfiguration":
			injectables = append(injectables, doc)
		case "Deployment", "StatefulSet":
			deployments = append(deployments, doc)
		default:
			prereqs = append(prereqs, doc)
		}
	}

	fmt.Println("[pivot] Applying prerequisites (Namespaces, Certificates, RBAC)...")
	if err := o.applyBatch(ctx, kubeconfig, prereqs); err != nil {
		return fmt.Errorf("failed to apply prerequisites: %w", err)
	}

	// Give cert-manager a tiny window to process the newly created Certificate/Issuer
	time.Sleep(2 * time.Second)

	fmt.Println("[pivot] Applying CRDs and Webhooks...")
	if err := o.applyBatch(ctx, kubeconfig, injectables); err != nil {
		return fmt.Errorf("failed to apply CRDs and Webhooks: %w", err)
	}

	if len(crdsToInject) > 0 {
		fmt.Println("[pivot] Waiting for cert-manager cainjector to inject CA bundles...")
		for _, crdName := range crdsToInject {
			if o.Debug {
				fmt.Printf("[DEBUG] Waiting for CA injection on CRD %s...\n", crdName)
			}
			if err := o.waitForCAInjection(ctx, kubeconfig, crdName); err != nil {
				return err
			}
		}
	}

	if len(crdsToWait) > 0 {
		fmt.Println("[pivot] Waiting for CRDs to be Established...")
		for _, crdName := range crdsToWait {
			waitCmd := exec.CommandContext(ctx, "kubectl", "wait", "--for=condition=Established", "crd/"+crdName, "--timeout=60s", "--kubeconfig", kubeconfig)
			if out, err := waitCmd.CombinedOutput(); err != nil {
				return fmt.Errorf("timeout waiting for CRD %s to establish: %w\n%s", crdName, err, out)
			}
		}
	}

	fmt.Println("[pivot] Applying Operator Deployments...")
	if err := o.applyBatch(ctx, kubeconfig, deployments); err != nil {
		return fmt.Errorf("failed to apply operator deployments: %w", err)
	}

	return nil
}

func (o *Orchestrator) waitForCAInjection(ctx context.Context, kubeconfig, crdName string) error {
	deadline := time.Now().Add(2 * time.Minute)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			cmd := exec.CommandContext(ctx, "kubectl", "get", "crd", crdName, "-o", "jsonpath={.spec.conversion.webhook.clientConfig.caBundle}", "--kubeconfig", kubeconfig)
			out, err := cmd.Output()
			if err == nil {
				caBundle := strings.TrimSpace(string(out))
				caBundle = strings.Trim(caBundle, "'\"") // Cleanup potential JSONPath formatting
				// "Cg==" is the base64 encoded "\n" placeholder. We wait for cainjector to overwrite it.
				if caBundle != "" && caBundle != "Cg==" {
					return nil
				}
			}
		}
	}
	return fmt.Errorf("timeout waiting for cert-manager cainjector to inject CA bundle for CRD %s", crdName)
}

func (o *Orchestrator) applyBatch(ctx context.Context, kubeconfig string, batch [][]byte) error {
	if len(batch) == 0 {
		return nil
	}
	manifest := bytes.Join(batch, []byte("\n---\n"))
	cmd := exec.CommandContext(ctx, "kubectl", "apply", "--kubeconfig", kubeconfig, "-f", "-")
	cmd.Stdin = bytes.NewReader(manifest)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Fallback to server-side apply ONLY if we hit the annotation length limit (safe enterprise workaround)
		if bytes.Contains(out, []byte("Too long: must have at most 262144 bytes")) {
			if o.Debug {
				fmt.Println("[DEBUG] Resource too large for client-side apply, falling back to server-side apply...")
			}
			cmd = exec.CommandContext(ctx, "kubectl", "apply", "--server-side", "--kubeconfig", kubeconfig, "-f", "-")
			cmd.Stdin = bytes.NewReader(manifest)
			out, err = cmd.CombinedOutput()
			if err != nil {
				return fmt.Errorf("%s", string(out))
			}
			return nil
		}
		return fmt.Errorf("%s", string(out))
	}
	return nil
}

// waitForCAPICRDs polls the API server until the core CAPI CRDs are
// registered. This is the pre-condition for any CAPI resource to be
// applied (Cluster, Machine, KubeadmControlPlane, etc.).
func (o *Orchestrator) waitForCAPICRDs(ctx context.Context, kubeconfig string, timeout time.Duration) error {
	waiter := &health.HealthWaiter{
		Checkers: []health.HealthChecker{
			health.NewCRDRegisteredHealth(
				"clusters.cluster.x-k8s.io",
				"machines.cluster.x-k8s.io",
				"machinedeployments.cluster.x-k8s.io",
				// NOT waited on here: providers.clusterctl.cluster.x-k8s.io.
				// 96368f91 added it on the premise that cluster-api-operator
				// creates it asynchronously. It does not. That CRD is a
				// `clusterctl init` artifact, and this platform initialises via
				// the operator, whose install.yaml defines seven CRDs, all in the
				// operator.cluster.x-k8s.io group. Verified absent on both the
				// kind source and the pivoted hub while every provider read Ready,
				// so the wait could only ever time out at 5m and made a working
				// bootstrap fail 100% of the time.
				//
				// If `clusterctl move` fails with
				//   no matches for kind "Provider" in version
				//   "clusterctl.cluster.x-k8s.io/v1alpha3"
				// the inventory is genuinely missing on the SOURCE and must be
				// created there — blocking on the target cannot supply it.
			),
		},
		Interval: 10 * time.Second,
		Timeout:  timeout,
		OnCheckPass: func(_ health.HealthChecker) {
			fmt.Println("[pivot] ✓ CAPI CRDs ready")
		},
	}
	return waiter.Wait(ctx, kubeconfig)
}

// waitForOperatorCRDs waits for the capi-operator's Provider CRDs to be
// established and for the webhook service endpoint to have ready
// addresses before applying Provider CRs.
//
// Two checks are composed in order:
//  1. Provider CRDs established
//  2. Webhook endpoint has at least one ready address
//
// "Established" CRD registration races with the webhook goroutine
// binding its port — a registered CRD does not mean the webhook is
// serving, so the second check is load-bearing.
func (o *Orchestrator) waitForOperatorCRDs(ctx context.Context, kubeconfig string, timeout time.Duration) error {
	webhookChecker := health.NewKubectlChecker(
		"capi-operator-webhook-service endpoint",
		[]string{
			"get", "endpoints", "capi-operator-webhook-service",
			"-n", constants.NamespaceCAPI,
			"-o", "jsonpath={.subsets[0].addresses[0].ip}",
		},
	)

	waiter := &health.HealthWaiter{
		Checkers: []health.HealthChecker{
			health.NewCRDRegisteredHealth(
				"coreproviders.operator.cluster.x-k8s.io",
				"bootstrapproviders.operator.cluster.x-k8s.io",
				"controlplaneproviders.operator.cluster.x-k8s.io",
				"infrastructureproviders.operator.cluster.x-k8s.io",
			),
			webhookChecker,
		},
		Interval: 5 * time.Second,
		Timeout:  timeout,
		OnCheckPass: func(_ health.HealthChecker) {
			fmt.Println("[pivot] ✓ Operator CRDs established and webhook endpoint ready")
		},
	}
	return waiter.Wait(ctx, kubeconfig)
}

func (o *Orchestrator) applyProviders(ctx context.Context, kubeconfig string) error {
	// Wait for the operator's CRDs to be established before applying Provider CRs.
	// The webhook (vcoreprovider.kb.io) rejects requests until the operator pod is
	// fully serving — deployment Available != webhook listening.
	if err := o.waitForOperatorCRDs(ctx, kubeconfig, 3*time.Minute); err != nil {
		return fmt.Errorf("operator CRDs not ready: %w", err)
	}

	providers := []string{
		"core-provider.yaml",
		"bootstrap-provider-kubeadm.yaml",
		"controlplane-provider-kubeadm.yaml",
		"infrastructure-provider-hetzner.yaml",
	}

	for _, provider := range providers {
		manifest, err := assets.ReadManifest(fmt.Sprintf("core/capi-operator/providers/%s", provider))
		if err != nil {
			return fmt.Errorf("failed to read %s: %w", provider, err)
		}

		cmd := exec.CommandContext(ctx, "kubectl", "apply",
			"--kubeconfig", kubeconfig,
			"-f", "-",
		)
		cmd.Stdin = bytes.NewReader(manifest)
		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("failed to apply %s: %w\n%s", provider, err, output)
		}
	}

	return nil
}

func (o *Orchestrator) move(ctx context.Context, mgmtKubeconfig string) error {
	clusterctlMgr, _ := binaries.NewClusterctlManager()
	clusterctlPath := clusterctlMgr.GetPath()

	// Pin the SOURCE to the bootstrap kubeconfig explicitly. clusterctl move
	// without --kubeconfig resolves the source from the ambient current-context
	// (KUBECONFIG/~/.kube/config), which may not be the bootstrap cluster after
	// kubeconfig patching — that silently moves nothing while still exiting 0.
	args := []string{"move", "--to-kubeconfig", mgmtKubeconfig}
	if o.BootstrapKubeconfig != "" {
		args = append(args, "--kubeconfig", o.BootstrapKubeconfig)
		if o.BootstrapContext != "" {
			// clusterctl's own flag name — it is not kubectl and does not
			// accept --context.
			args = append(args, "--kubeconfig-context", o.BootstrapContext)
		}
	}
	args = append(args, "--namespace", o.Namespace)

	cmd := exec.CommandContext(ctx, clusterctlPath, args...)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("move failed: %w\n%s", err, output)
	}

	return nil
}

func (o *Orchestrator) countResources(ctx context.Context, kubeconfig string) (int, error) {
	cmd := exec.CommandContext(ctx, "kubectl",
		"--kubeconfig", kubeconfig,
		"get", "clusters,machines,hetznerclusters",
		"-n", o.Namespace,
		"-o", "json",
	)

	output, err := cmd.Output()
	if err != nil {
		return 0, err
	}

	// Simple count: count occurrences of "kind"
	count := bytes.Count(output, []byte(`"kind":`))
	return count, nil
}

// waitForProvidersReady waits for all CAPI core providers to report
// Ready=True on the management cluster after the pivot move.
//
// The CAPIProvidersReadiness phase (defined in the health package)
// encodes the four providers that every zero-ops hub needs: Core,
// Bootstrap, ControlPlane, and Infrastructure. The OS-specific
// provider names are computed in the phase's Checkers() method.
func (o *Orchestrator) waitForProvidersReady(ctx context.Context, kubeconfig string, timeout time.Duration) error {
	phase := &health.CAPIProvidersReadiness{
		Namespace: constants.NamespaceCAPI,
		OSType:    o.OSType,
	}
	waiter := &health.HealthWaiter{
		Checkers: phase.Checkers(),
		Interval: 10 * time.Second,
		Timeout:  timeout,
		OnCheckPass: func(c health.HealthChecker) {
			fmt.Printf("[pivot] ✓ %s ready on Management Cluster\n", c.Name())
		},
	}
	return waiter.Wait(ctx, kubeconfig)
}

// waitForClusterReady waits for the spoke cluster's Ready condition to
// become True on the management cluster after the pivot move.
func (o *Orchestrator) waitForClusterReady(ctx context.Context, kubeconfig string, timeout time.Duration) error {
	waiter := &health.HealthWaiter{
		Checkers: []health.HealthChecker{
			health.NewCAPIResourceReadyHealth(health.CAPIClusterKind, o.ClusterName, o.Namespace),
		},
		Interval: 30 * time.Second,
		Timeout:  timeout,
		OnCheckPass: func(_ health.HealthChecker) {
			fmt.Println("[pivot] ✓ Cluster Ready condition satisfied")
		},
	}
	return waiter.Wait(ctx, kubeconfig)
}
