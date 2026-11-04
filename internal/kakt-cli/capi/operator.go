package capi

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/kamarkat/kamarkat/internal/assets"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/constants"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/versions"
)

type resourceMetadata struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name        string            `yaml:"name"`
		Annotations map[string]string `yaml:"annotations"`
	} `yaml:"metadata"`
}

// CAPIProvider represents a CAPI provider to install via the cluster-api-operator
type CAPIProvider struct {
	Kind     string // CoreProvider, BootstrapProvider, ControlPlaneProvider, InfrastructureProvider
	Name     string // e.g., cluster-api, kubeadm, hetzner, docker
	Version  string
	Manifest string // path to embedded manifest, e.g., "core/capi-operator/providers/infrastructure-provider-hetzner.yaml"
}

// OperatorInstaller installs cluster-api-operator
type OperatorInstaller struct {
	Kubeconfig string
	Context    string
	Namespace  string
	Providers  []CAPIProvider
	Debug      bool
}

func (i *OperatorInstaller) kubectlArgs(args ...string) []string {
	var result []string

	// Skip --kubeconfig if using default location (kubectl v1.34+ bug workaround)
	homeDir, _ := os.UserHomeDir()
	defaultKubeconfig := filepath.Join(homeDir, ".kube", "config")
	if i.Kubeconfig != "" && i.Kubeconfig != defaultKubeconfig {
		result = append(result, "--kubeconfig", i.Kubeconfig)
	}

	if i.Context != "" {
		result = append(result, "--context", i.Context)
	}
	return append(result, args...)
}

func (i *OperatorInstaller) runKubectl(ctx context.Context, args ...string) error {
	fullArgs := i.kubectlArgs(args...)
	if i.Debug {
		fmt.Printf("[DEBUG] kubectl %v\n", fullArgs)
	}
	cmd := exec.CommandContext(ctx, "kubectl", fullArgs...)
	output, err := cmd.CombinedOutput()
	if i.Debug && len(output) > 0 {
		fmt.Printf("[DEBUG] Output: %s\n", string(output))
	}
	if err != nil && i.Debug {
		fmt.Printf("[DEBUG] Error: %v\n", err)
	}
	return err
}

func (i *OperatorInstaller) Install(ctx context.Context) error {
	if i.Debug {
		fmt.Println("[DEBUG] OperatorInstaller.Install() started")
	}

	if err := i.ensureCertManager(ctx); err != nil {
		return err
	}

	if err := i.installOperator(ctx); err != nil {
		return err
	}

	if err := i.waitForOperator(ctx, 3*time.Minute); err != nil {
		return err
	}

	if err := i.applyProviders(ctx); err != nil {
		return err
	}

	return i.waitForProviders(ctx, 5*time.Minute)
}

func (i *OperatorInstaller) ensureCertManager(ctx context.Context) error {
	// Check if cert-manager namespace exists (upstream uses cert-manager namespace)
	cmd := exec.CommandContext(ctx, "kubectl", i.kubectlArgs("get", "namespace", constants.NamespaceCertManager)...)

	if err := cmd.Run(); err == nil {
		fmt.Println("[capi-init] ✓ cert-manager already installed")
		return i.waitForCertManagerAPI(ctx)
	}

	// Install cert-manager
	fmt.Println("[capi-init] Installing cert-manager...")
	certManagerURL := fmt.Sprintf("https://github.com/cert-manager/cert-manager/releases/download/%s/cert-manager.yaml", versions.CertManagerVersion)

	cmd = exec.CommandContext(ctx, "kubectl", i.kubectlArgs("apply", "-f", certManagerURL)...)

	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("cert-manager install failed: %w\n%s", err, output)
	}

	return i.waitForCertManagerAPI(ctx)
}

func (i *OperatorInstaller) waitForCertManagerAPI(ctx context.Context) error {
	fmt.Println("[capi-init] Waiting for cert-manager API...")

	deployments := []string{"cert-manager", "cert-manager-webhook", "cert-manager-cainjector"}
	for _, dep := range deployments {
		cmd := exec.CommandContext(ctx, "kubectl", i.kubectlArgs("wait", "deployment",
			"-n", constants.NamespaceCertManager,
			dep,
			"--for=condition=Available",
			"--timeout=2m")...)

		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("cert-manager deployment %s not ready: %w\n%s", dep, err, output)
		}
	}

	fmt.Println("[capi-init] Waiting for cert-manager webhook to become fully functional...")

	dummyIssuer := []byte(`
apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: test-webhook-readiness
  namespace: default
spec:
  selfSigned: {}
`)

	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		cmd := exec.CommandContext(ctx, "kubectl", i.kubectlArgs("apply", "--dry-run=server", "-f", "-")...)
		cmd.Stdin = bytes.NewReader(dummyIssuer)
		if err := cmd.Run(); err == nil {
			fmt.Println("[capi-init] ✓ cert-manager API ready")
			return nil
		}
		time.Sleep(2 * time.Second)
	}

	return fmt.Errorf("timeout waiting for cert-manager webhook to become fully functional")
}

func (i *OperatorInstaller) installOperator(ctx context.Context) error {
	manifest, err := assets.ReadManifest("core/capi-operator/install.yaml")
	if err != nil {
		return fmt.Errorf("failed to read embedded capi-operator manifest: %w", err)
	}

	// Clear the placeholder caBundle (base64 of '\n') to empty so Kubernetes accepts
	// the apply; cert-manager cainjector will inject the real CA bundle once the cert is ready.
	manifest = bytes.ReplaceAll(manifest, []byte("caBundle: Cg=="), []byte(`caBundle: ""`))

	return i.applyManifestSafely(ctx, manifest)
}

func (i *OperatorInstaller) applyManifestSafely(ctx context.Context, manifest []byte) error {
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

	fmt.Println("[capi-init] Applying prerequisites (Namespaces, Certificates, RBAC)...")
	if err := i.applyBatch(ctx, prereqs); err != nil {
		return fmt.Errorf("failed to apply prerequisites: %w", err)
	}

	// Give cert-manager a tiny window to process the newly created Certificate/Issuer
	time.Sleep(2 * time.Second)

	fmt.Println("[capi-init] Applying CRDs and Webhooks...")
	if err := i.applyBatch(ctx, injectables); err != nil {
		return fmt.Errorf("failed to apply CRDs and Webhooks: %w", err)
	}

	if len(crdsToInject) > 0 {
		fmt.Println("[capi-init] Waiting for cert-manager cainjector to inject CA bundles...")
		for _, crdName := range crdsToInject {
			if i.Debug {
				fmt.Printf("[DEBUG] Waiting for CA injection on CRD %s...\n", crdName)
			}
			if err := i.waitForCAInjection(ctx, crdName); err != nil {
				return err
			}
		}
	}

	if len(crdsToWait) > 0 {
		fmt.Println("[capi-init] Waiting for CRDs to be Established...")
		for _, crdName := range crdsToWait {
			waitCmd := exec.CommandContext(ctx, "kubectl", i.kubectlArgs("wait", "--for=condition=Established", "crd/"+crdName, "--timeout=60s")...)
			if out, err := waitCmd.CombinedOutput(); err != nil {
				return fmt.Errorf("timeout waiting for CRD %s to establish: %w\n%s", crdName, err, out)
			}
		}
	}

	fmt.Println("[capi-init] Applying Operator Deployments...")
	if err := i.applyBatch(ctx, deployments); err != nil {
		return fmt.Errorf("failed to apply operator deployments: %w", err)
	}

	return nil
}

func (i *OperatorInstaller) waitForCAInjection(ctx context.Context, crdName string) error {
	deadline := time.Now().Add(2 * time.Minute)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			cmd := exec.CommandContext(ctx, "kubectl", i.kubectlArgs("get", "crd", crdName, "-o", "jsonpath={.spec.conversion.webhook.clientConfig.caBundle}")...)
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

// applyBatch applies a group of manifests, retrying while an admission webhook is
// still coming up.
//
// Every batch here passes through cert-manager's validating webhook, and on a
// hybrid hub that webhook runs on the home-lab worker: a ~200ms link, with images
// pulled over the same link. A batch applied while the pod is still starting fails
// with
//
//	failed calling webhook "webhook.cert-manager.io": ... net/http: TLS handshake timeout
//
// even though the datapath is healthy — verified by hand: TCP to the webhook pod
// opens and TLS responds once it has settled. Waiting for the Deployment is not
// enough, because Ready precedes the webhook actually serving TLS.
func (i *OperatorInstaller) applyBatch(ctx context.Context, batch [][]byte) error {
	const attempts = 12 // ~2 minutes at 10s spacing

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		err := i.applyBatchOnce(ctx, batch)
		if err == nil {
			if attempt > 1 {
				fmt.Printf("[capi-init] ✓ batch applied on attempt %d\n", attempt)
			}
			return nil
		}
		lastErr = err

		if !transientWebhookError(err.Error()) {
			return err
		}
		if attempt == attempts {
			break
		}
		if attempt == 1 {
			fmt.Println("[capi-init] admission webhook not serving yet, retrying...")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
	return fmt.Errorf("admission webhook never became serviceable after %d attempts: %w", attempts, lastErr)
}

func (i *OperatorInstaller) applyBatchOnce(ctx context.Context, batch [][]byte) error {
	if len(batch) == 0 {
		return nil
	}
	manifest := bytes.Join(batch, []byte("\n---\n"))
	cmd := exec.CommandContext(ctx, "kubectl", i.kubectlArgs("apply", "-f", "-")...)
	cmd.Stdin = bytes.NewReader(manifest)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Fallback to server-side apply ONLY if we hit the annotation length limit (safe enterprise workaround)
		if bytes.Contains(out, []byte("Too long: must have at most 262144 bytes")) {
			if i.Debug {
				fmt.Println("[DEBUG] Resource too large for client-side apply, falling back to server-side apply...")
			}
			cmd = exec.CommandContext(ctx, "kubectl", i.kubectlArgs("apply", "--server-side", "-f", "-")...)
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

func (i *OperatorInstaller) waitForOperator(ctx context.Context, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "kubectl", i.kubectlArgs("wait", "deployment",
		"-n", constants.NamespaceCAPI,
		"capi-operator-controller-manager",
		"--for=condition=Available",
		fmt.Sprintf("--timeout=%s", timeout))...)

	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("operator not ready: %w\n%s", err, output)
	}

	return nil
}

// transientWebhookError reports whether a kubectl failure is the CAPI operator's
// startup race rather than a real problem with the manifest.
//
// The operator serves its own validating webhooks with a cert-manager certificate.
// waitForCRDs only proves the CRDs are Established and their CA bundle injected —
// it says nothing about whether the webhook POD has loaded a serving cert matching
// that bundle yet. Applying a provider inside that window fails with:
//
//	failed calling webhook "vcoreprovider.kb.io": ... tls: failed to verify
//	certificate: x509: certificate signed by unknown authority
//
// which resolves itself within seconds. Retrying a genuinely bad manifest would
// only delay the real error, so only these signatures are retried.
func transientWebhookError(output string) bool {
	for _, sig := range []string{
		"failed calling webhook",
		"certificate signed by unknown authority",
		"connection refused",
		"no endpoints available",
		"context deadline exceeded",
		"i/o timeout",
		"TLS handshake timeout",
		"EOF",
	} {
		if strings.Contains(output, sig) {
			return true
		}
	}
	return false
}

// applyWithWebhookRetry applies a provider manifest, retrying while the operator's
// webhook is still coming up.
func (i *OperatorInstaller) applyWithWebhookRetry(ctx context.Context, name string, manifest []byte) error {
	const attempts = 12 // ~2 minutes at 10s spacing

	var lastOutput string
	for attempt := 1; attempt <= attempts; attempt++ {
		cmd := exec.CommandContext(ctx, "kubectl", i.kubectlArgs("apply", "-f", "-")...)
		cmd.Stdin = bytes.NewReader(manifest)

		output, err := cmd.CombinedOutput()
		if err == nil {
			if attempt > 1 {
				fmt.Printf("[capi-init] ✓ %s applied on attempt %d\n", name, attempt)
			}
			return nil
		}

		lastOutput = string(output)
		if !transientWebhookError(lastOutput) {
			return fmt.Errorf("failed to apply %s: %w\n%s", name, err, lastOutput)
		}

		if attempt == attempts {
			break
		}
		if attempt == 1 {
			fmt.Printf("[capi-init] %s: operator webhook not serving yet, retrying...\n", name)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}

	return fmt.Errorf("failed to apply %s after %d attempts; the capi-operator webhook "+
		"never became serviceable:\n%s", name, attempts, lastOutput)
}

func (i *OperatorInstaller) applyProviders(ctx context.Context) error {
	// Five minutes, not two. This waits for the cluster-api-operator to pull its
	// image and register four CRDs, and the bound was set close enough to the
	// observed time that a slow pull failed the run: a successful bootstrap
	// recorded capi-init at 174.9s against a 180s phase, and the next one timed
	// out. The wait returns the moment the CRDs appear, so a generous bound
	// costs a healthy run nothing and spares an unhealthy one a false negative
	// -- which here means a published version and three servers spent on a
	// timeout that was never about the cluster.
	if err := i.waitForCRDs(ctx, 5*time.Minute); err != nil {
		return err
	}

	for _, provider := range i.Providers {
		manifest, err := assets.ReadManifest(provider.Manifest)
		if err != nil {
			return fmt.Errorf("failed to read %s: %w", provider.Manifest, err)
		}

		if err := i.applyWithWebhookRetry(ctx, provider.Manifest, manifest); err != nil {
			return err
		}

		if i.Debug {
			fmt.Printf("[capi-init] ✓ Applied %s/%s\n", provider.Kind, provider.Name)
		}
	}

	return nil
}

func (i *OperatorInstaller) waitForCRDs(ctx context.Context, timeout time.Duration) error {
	crds := []string{
		"coreproviders.operator.cluster.x-k8s.io",
		"bootstrapproviders.operator.cluster.x-k8s.io",
		"controlplaneproviders.operator.cluster.x-k8s.io",
		"infrastructureproviders.operator.cluster.x-k8s.io",
	}

	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for _, crd := range crds {
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
				if time.Now().After(deadline) {
					// Names where to look. The CRD is registered by the
					// operator, so its absence is almost always the operator
					// pod not running -- an image it could not pull, or a
					// cert-manager webhook it is still waiting on -- and none
					// of that is visible in a message about a CRD.
					return fmt.Errorf("timeout waiting for CRD %s.\n\n"+
						"The cluster-api-operator registers it, so check the pod rather than the CRD:\n"+
						"  kubectl --kubeconfig %s get pods -n capi-operator-system\n"+
						"  kubectl --kubeconfig %s describe pod -n capi-operator-system\n\n"+
						"An image it cannot pull and a cert-manager webhook not yet serving both look like this",
						crd, i.Kubeconfig, i.Kubeconfig)
				}

				cmd := exec.CommandContext(ctx, "kubectl", i.kubectlArgs("get", "crd", crd)...)

				if err := cmd.Run(); err == nil {
					fmt.Printf("[capi-init] ✓ CRD %s registered\n", crd)
					goto nextCRD
				}

				fmt.Printf("[capi-init] Waiting for CRD %s...\n", crd)
			}
		}
	nextCRD:
	}

	return nil
}

func (i *OperatorInstaller) waitForProviders(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for _, provider := range i.Providers {
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
				if time.Now().After(deadline) {
					return fmt.Errorf("timeout waiting for %s/%s", provider.Kind, provider.Name)
				}

				cmd := exec.CommandContext(ctx, "kubectl", i.kubectlArgs("get", provider.Kind, provider.Name,
					"-n", constants.NamespaceCAPI,
					"-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}")...)

				output, err := cmd.Output()
				if err != nil {
					continue
				}

				if string(output) == "True" {
					fmt.Printf("[capi-init] ✓ %s/%s ready\n", provider.Kind, provider.Name)
					goto nextProvider
				}

				fmt.Printf("[capi-init] Waiting for %s/%s...\n", provider.Kind, provider.Name)
			}
		}
	nextProvider:
	}

	return nil
}
