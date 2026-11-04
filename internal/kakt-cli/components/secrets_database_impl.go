package components

import (
	"context"
	"fmt"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/tenant"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/constants"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/health"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/infisical"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// InstallInfisicalAuthFromInfisical fully automates the Infisical Day-0 bootstrap:
// 1. Runs `infisical bootstrap` via pod exec to create admin + org
// 2. Creates the hub-platform project via REST API
// 3. Creates a Machine Identity with Universal Auth + client secret
// 4. Grants project admin role
// 5. Creates the `infisical-auth` Secret in platform-ops
// 6. Generates ADR-045 artifacts (infisical-fleet-issuer-patch.yaml, hub-bootstrap-config-patch.yaml)
// infisicalURL is where THIS box's Infisical answers: infisical.<its domain>.
//
// Read from the box's HubEnvironment, which ADR-051 makes the sole authority for
// the base domain, rather than plumbed through as a field -- the cluster is
// already reachable here and the CR is the thing that decides.
//
// It is carried in infisical-auth because a consumer needed it and could not
// derive it. spoke-identity-operator renders every spoke's ClusterIssuer, and its
// URL was a compile-time constant naming the PLATFORM's Infisical -- so each
// spoke asked a stranger to sign its certificates, the issuer reported
// "healthcheck failed: ... EOF", and every Certificate naming it stayed pending.
// On a tenant's box that host is not merely wrong, it is another tenant's secret
// store.
func infisicalURL(ctx context.Context, kubeconfig string) (string, error) {
	out, err := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig,
		"get", "hubenvironment", "hub-environment", "-n", constants.NamespaceOps,
		"-o", "jsonpath={.spec.domain}").Output()
	if err != nil {
		return "", fmt.Errorf("cannot read the box's domain from its HubEnvironment: %w", err)
	}
	domain := strings.TrimSpace(string(out))
	if domain == "" {
		return "", fmt.Errorf("the box's HubEnvironment declares no domain, so its Infisical has no address")
	}
	return "https://infisical." + domain, nil
}

func (i *Installer) InstallInfisicalAuthFromInfisical(ctx context.Context) (bool, error) {
	config, err := clientcmd.BuildConfigFromFlags("", i.Kubeconfig)
	if err != nil {
		return false, fmt.Errorf("failed to load kubeconfig: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return false, fmt.Errorf("failed to create kubernetes client: %w", err)
	}

	// The Day-0 bootstrap shells out to kubectl. Point it at the same kubeconfig
	// this Installer is using, so those calls cannot silently target a different
	// cluster (or localhost:8080) when KUBECONFIG is not exported.
	infisical.SetKubeconfig(i.Kubeconfig)

	result, err := infisical.BootstrapInfisicalDayZeroWithRetry(ctx, 10*time.Minute)
	if err != nil {
		return false, fmt.Errorf("infisical bootstrap failed: %w", err)
	}

	infisicalAddr, err := infisicalURL(ctx, i.Kubeconfig)
	if err != nil {
		return false, err
	}

	fmt.Println("[bootstrap-secrets] Creating infisical-auth secret...")
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "infisical-auth",
			Namespace: constants.NamespaceOps,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "zero-ops-hub-cli",
				"app.kubernetes.io/component":  "secret-zero",
			},
		},
		Type: corev1.SecretTypeOpaque,
		StringData: map[string]string{
			"client-id":        result.ClientID,
			"client-secret":    result.ClientSecret,
			"clientSecret":     result.ClientSecret, // pki-issuer v0.2.0 compat
			"orgId":            result.OrgID,
			"projectId":        result.ProjectID,
			"secretsProjectId": result.SecretsProjectID,
			"url":              infisicalAddr,
		},
	}

	_, err = clientset.CoreV1().Secrets(constants.NamespaceOps).Create(ctx, secret, metav1.CreateOptions{})
	if err != nil {
		if k8serrors.IsAlreadyExists(err) {
			_, err = clientset.CoreV1().Secrets(constants.NamespaceOps).Update(ctx, secret, metav1.UpdateOptions{})
			if err != nil {
				return false, fmt.Errorf("failed to update infisical-auth: %w", err)
			}
			fmt.Println("[bootstrap-secrets] ✓ infisical-auth secret updated")
		} else {
			return false, fmt.Errorf("failed to create infisical-auth: %w", err)
		}
	} else {
		fmt.Println("[bootstrap-secrets] ✓ infisical-auth secret created")
	}

	// Create a copy of infisical-auth in platform-security for infisical-issuer
	securitySecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "infisical-auth",
			Namespace: constants.NamespaceSecurity,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "zero-ops-hub-cli",
				"app.kubernetes.io/component":  "secret-zero",
			},
		},
		Type: corev1.SecretTypeOpaque,
		StringData: map[string]string{
			"client-id":        result.ClientID,
			"client-secret":    result.ClientSecret,
			"clientSecret":     result.ClientSecret, // pki-issuer v0.2.0 compat
			"orgId":            result.OrgID,
			"projectId":        result.ProjectID,
			"secretsProjectId": result.SecretsProjectID,
			"url":              infisicalAddr,
		},
	}
	_, err = clientset.CoreV1().Secrets(constants.NamespaceSecurity).Create(ctx, securitySecret, metav1.CreateOptions{})
	if err != nil {
		if k8serrors.IsAlreadyExists(err) {
			_, err = clientset.CoreV1().Secrets(constants.NamespaceSecurity).Update(ctx, securitySecret, metav1.UpdateOptions{})
			if err != nil {
				return false, fmt.Errorf("failed to update infisical-auth in %s: %w", constants.NamespaceSecurity, err)
			}
			fmt.Printf("[bootstrap-secrets] ✓ infisical-auth secret updated in %s\n", constants.NamespaceSecurity)
		} else {
			return false, fmt.Errorf("failed to create infisical-auth in %s: %w", constants.NamespaceSecurity, err)
		}
	} else {
		fmt.Printf("[bootstrap-secrets] ✓ infisical-auth secret created in %s\n", constants.NamespaceSecurity)
	}

	fmt.Println("[bootstrap-secrets] Generating ADR-045 artifacts...")

	projectRoot, err := i.artifactRoot()
	if err != nil {
		return false, fmt.Errorf("failed to get working directory: %w", err)
	}

	// The Infisical PKI coordinates were written here as a Kustomize patch under
	// manifests/hub-core-services/security/generated/ and committed. They are
	// per-cluster instance data, and ADR-062 keeps that out of the types
	// repository; leaving them here also made the component impossible to publish
	// as a chart, since every cluster would have pulled this cluster's project.
	//
	// They are now read back from the infisical-auth Secret written above and
	// passed to the seed Application as values (see infisicalCoordinates in
	// internal/kakt-cli/bootstrap/seed.go), so nothing is generated into the tree.

	// Write hub-bootstrap-config-patch.yaml into environments/base kustomization's generated/ dir
	env := i.EnvironmentSlug
	if env == "" {
		return false, fmt.Errorf("cannot write the ADR-045 bootstrap config: no environment slug.\n" +
			"It would land in a directory shared by every overlay, where the last cluster\n" +
			"bootstrapped wins and an environment renders another's Infisical project")
	}
	// A tenant's repository holds its artifacts under the cluster they belong to;
	// the platform's own tree holds them under the environment overlay that
	// renders them (ADR-045, ADR-072). Same file, addressed by whose repository
	// it is in.
	// VALUES, not a ConfigMap.
	//
	// This wrote a partial ConfigMap and called it a patch. On the platform's own
	// tree kustomize merged it into the base and that was true; in a tenant's
	// repository the same file is applied as a whole object by the Application
	// that owns generated/, and applying a ConfigMap REPLACES its data. So the
	// tenant's copy deleted CLUSTER_ID, hub-environment's copy deleted the
	// identity, and the two alternated -- an operator that could not start, then
	// an operator authenticating against the wrong Infisical.
	//
	// hub-environment now renders the whole ConfigMap and reads these through
	// global.infisical (templated-fields.yaml on the environment overlay), so
	// there is one writer and this file competes with nothing.
	configPath := filepath.Join(projectRoot, "manifests", "environments", env, "generated", "values", "infisical-identity.yaml")
	if i.GitopsDir != "" {
		if i.ClusterName == "" {
			return false, fmt.Errorf("cannot write the ADR-045 bootstrap config into " +
				"the tenant repository: no cluster name, and these artifacts belong to " +
				"one cluster rather than to the repository")
		}
		configPath = filepath.Join(projectRoot, tenant.RegistryDir, "clusters", i.ClusterName, "generated", "values", "infisical-identity.yaml")
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		return false, fmt.Errorf("failed to create %s: %w", filepath.Dir(configPath), err)
	}
	configPatch := fmt.Sprintf(`# This box's own Infisical identity, read by hub-environment through
# global.infisical. Not an object: see secrets_database_impl.go.
global:
  infisical:
    organizationId: "%s"
    projectId: "%s"
    secretsProjectId: "%s"
`, result.OrgID, result.ProjectID, result.SecretsProjectID)
	if err := os.WriteFile(configPath, []byte(configPatch), 0644); err != nil {
		return false, fmt.Errorf("failed to write %s: %w", configPath, err)
	}
	fmt.Printf("[bootstrap-secrets] ✓ %s\n", configPath)

	// Validate all artifacts exist
	if _, err := os.Stat(configPath); err != nil {
		return false, fmt.Errorf("infisical-identity.yaml not found after generation: %w", err)
	}

	fmt.Println("[bootstrap-secrets]")
	fmt.Println("[bootstrap-secrets] ═══════════════════════════════════════════════════════")
	fmt.Println("[bootstrap-secrets]  ADR-045: Bootstrap-Generated GitOps Artifacts")
	fmt.Println("[bootstrap-secrets]  Generated artifacts:")

	fmt.Printf("[bootstrap-secrets]    manifests/environments/%s/generated/\n", env)
	fmt.Println("[bootstrap-secrets]      └── values/infisical-identity.yaml")
	fmt.Println("[bootstrap-secrets]  Commit and push before platform readiness checks pass.")
	fmt.Println("[bootstrap-secrets] ═══════════════════════════════════════════════════════")
	fmt.Println("[bootstrap-secrets]")

	return true, nil
}

func (i *Installer) InstallPostgresConnectionSecret(ctx context.Context) (bool, error) {
	// Load kubeconfig and create clientset
	config, err := clientcmd.BuildConfigFromFlags("", i.Kubeconfig)
	if err != nil {
		return false, fmt.Errorf("failed to load kubeconfig: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return false, fmt.Errorf("failed to create kubernetes client: %w", err)
	}

	// Define explicit bounded contexts - "Secrets live exactly where they are consumed"
	dataNamespace := constants.NamespaceData         // platform-data: CNPG, Redis, DB Init Jobs
	securityNamespace := constants.NamespaceSecurity // platform-security: Infisical pods

	// Step 1: Generate and inject infisical-db-credentials (Infisical Secret Zero)
	// MUST be in dataNamespace - consumed by DB Init Job that creates infisical role
	infDbSecret, err := clientset.CoreV1().Secrets(dataNamespace).Get(ctx, "infisical-db-credentials", metav1.GetOptions{})
	var infPassword string
	if err != nil {
		if !k8serrors.IsNotFound(err) {
			return false, fmt.Errorf("failed to check infisical-db-credentials secret: %w", err)
		}

		fmt.Println("[bootstrap-secrets] Generating infisical-db-credentials (Infisical Secret Zero)...")
		infPassword, err = generateSecurePassword(32)
		if err != nil {
			return false, fmt.Errorf("failed to generate infisical password: %w", err)
		}

		infDbSecret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "infisical-db-credentials",
				Namespace: dataNamespace, // DB Init Job in platform-data
				Labels: map[string]string{
					"app.kubernetes.io/managed-by": "zero-ops-hub-cli",
					"app.kubernetes.io/component":  "secret-zero",
				},
			},
			Type: corev1.SecretTypeOpaque,
			StringData: map[string]string{
				"username": "infisical",
				"password": infPassword,
			},
		}

		_, err = clientset.CoreV1().Secrets(dataNamespace).Create(ctx, infDbSecret, metav1.CreateOptions{})
		if err != nil {
			return false, fmt.Errorf("failed to create infisical-db-credentials secret: %w", err)
		}
		fmt.Println("[bootstrap-secrets] ✓ infisical-db-credentials created")
	} else {
		infPassword = string(infDbSecret.Data["password"])
		fmt.Println("[bootstrap-secrets] ✓ infisical-db-credentials already exists")
	}

	// Step 2: Create the connection string secret for Infisical to use
	// MUST be in securityNamespace - consumed by Infisical pods
	// TLS Configuration: End-to-end encryption (production-grade)
	// - Infisical → PgBouncer: TLS (using CNPG-provided certificates)
	// - PgBouncer → PostgreSQL: TLS (handled by CNPG)
	// DB_ROOT_CERT is provided separately via infisical-secrets (injected via envFrom)
	connSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "infisical-postgres-connection",
			Namespace: securityNamespace, // Infisical pods in platform-security
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "zero-ops-hub-cli",
				"app.kubernetes.io/component":  "secret-zero",
			},
		},
		Type: corev1.SecretTypeOpaque,
		StringData: map[string]string{
			// The secret store's own cluster (ADR-112 §1, §4), declared in
			// hub-core-services/infisical/infisical-db.yaml beside Infisical itself.
			//
			// The PRIMARY service, not a pooler: platform-db had one and infisical-db
			// does not. A Pooler is provisioned per cluster only where the application
			// needs one (ADR-112 §2), and nothing has shown that Infisical does --
			// adding one to mirror the old topology would be copying a shape rather
			// than meeting a need.
			"DB_HOST":     infisicalDBCluster + "-rw." + constants.NamespaceData + ".svc",
			"DB_PORT":     "5432",
			"DB_USER":     "infisical",
			"DB_PASSWORD": infPassword,
			"DB_NAME":     "infisical",
			"DB_SSL_MODE": "require", // TLS from client to the primary
		},
	}

	// Create or Update connSecret in securityNamespace
	_, err = clientset.CoreV1().Secrets(securityNamespace).Update(ctx, connSecret, metav1.UpdateOptions{})
	if err != nil {
		if k8serrors.IsNotFound(err) {
			_, err = clientset.CoreV1().Secrets(securityNamespace).Create(ctx, connSecret, metav1.CreateOptions{})
			if err != nil {
				return false, fmt.Errorf("failed to create infisical-postgres-connection: %w", err)
			}
			fmt.Println("[bootstrap-secrets] ✓ infisical-postgres-connection created")
		} else {
			return false, fmt.Errorf("failed to update infisical-postgres-connection: %w", err)
		}
	} else {
		fmt.Println("[bootstrap-secrets] ✓ infisical-postgres-connection updated")
	}

	return true, nil
}

// InstallPlatformDatabaseCredentials uploads the bootstrap database credential to
// Infisical, making Infisical the source of truth for it.
//
// Only Secret Zero is handled here. infisical-db-credentials is created in the cluster
// before Infisical exists -- it has to be, since Infisical cannot supply the credential
// to its own database -- so this is the one credential that travels INTO Infisical rather
// than out of it. Every application role's credential is generated in Infisical and
// reaches the cluster through ExternalSecrets, and hub-operator's RoleManager applies it
// to the role (ADR-023 add.).
//
// IDEMPOTENCY: CreateOrUpdateSecret is safe to repeat.
func (i *Installer) InstallPlatformDatabaseCredentials(ctx context.Context) (bool, error) {
	// Load kubeconfig and create clientset
	config, err := clientcmd.BuildConfigFromFlags("", i.Kubeconfig)
	if err != nil {
		return false, fmt.Errorf("failed to load kubeconfig: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return false, fmt.Errorf("failed to create kubernetes client: %w", err)
	}

	namespace := constants.NamespaceData

	// Create Infisical API client
	infisicalClient, err := infisical.NewClient(ctx, clientset)
	if err != nil {
		return false, fmt.Errorf("failed to create Infisical client: %w", err)
	}

	// Get Infisical configuration (project slug and environment)
	infisicalConfig, err := infisical.GetInfisicalConfig(ctx, clientset)
	if err != nil {
		return false, fmt.Errorf("failed to get Infisical config: %w", err)
	}

	secretPath := "/"

	// Step 1: Upload Layer 1 Bootstrap Credentials to Infisical (making Infisical the SOT)
	fmt.Println("[bootstrap-secrets] Uploading Layer 1 (Secret Zero) credentials to Infisical...")

	// Upload infisical-db-credentials
	infDbSecret, err := clientset.CoreV1().Secrets(namespace).Get(ctx, "infisical-db-credentials", metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("failed to read infisical-db-credentials: %w", err)
	}

	if err := infisicalClient.CreateOrUpdateSecret(ctx, infisicalConfig.ProjectSlug, infisicalConfig.EnvironmentSlug, secretPath, infisical.KeyInfisicalDBUsername, string(infDbSecret.Data["username"])); err != nil {
		return false, fmt.Errorf("failed to upload %s: %w", infisical.KeyInfisicalDBUsername, err)
	}
	if err := infisicalClient.CreateOrUpdateSecret(ctx, infisicalConfig.ProjectSlug, infisicalConfig.EnvironmentSlug, secretPath, infisical.KeyInfisicalDBPassword, string(infDbSecret.Data["password"])); err != nil {
		return false, fmt.Errorf("failed to upload %s: %w", infisical.KeyInfisicalDBPassword, err)
	}
	fmt.Println("[bootstrap-secrets] ✓ infisical-db credentials uploaded to Infisical")

	fmt.Println("[bootstrap-secrets] ✓ Bootstrap credential stored in Infisical. ExternalSecrets will sync the rest.")

	return true, nil
}

// WaitForInfisicalAuth waits for the infisical-auth secret to be created by 'hub configure-eso'.
// This secret contains the Machine Identity credentials needed to authenticate with the Infisical API.
// It must exist before Steps 4-5 of init-secrets can run.
func (i *Installer) WaitForInfisicalAuth(ctx context.Context) error {
	printed := false
	waiter := &health.HealthWaiter{
		Checkers: []health.HealthChecker{
			health.NewSecretKeyHealth(
				constants.NamespaceOps,
				"infisical-auth",
				"client-id",
				"client-secret",
			),
		},
		Interval: 15 * time.Second,
		Timeout:  30 * time.Minute,
		OnCheckStart: func(c health.HealthChecker) {
			if !printed {
				fmt.Println("\n⏳ Waiting for infisical-auth secret...")
				fmt.Println("   NOTE: This secret is now auto-created by 'hub init-secrets' Step 3.5.")
				fmt.Println("   Run: hub init-secrets --kubeconfig=<path>")
				fmt.Println("   This process will continue automatically once the secret is created.")
				printed = true
			}
			fmt.Printf("   %s not found yet, checking again in 15s...\n", c.Name())
		},
	}
	if err := waiter.Wait(ctx, i.Kubeconfig); err != nil {
		return fmt.Errorf("timeout after 30m waiting for infisical-auth secret — run 'hub init-secrets' to create it: %w", err)
	}
	fmt.Println("✓ infisical-auth secret found — Machine Identity credentials ready")
	return nil
}

// WaitForInfisicalHealth waits for the data layer (CNPG + PgBouncer) and
// Infisical itself to be ready.
//
// This is the single entry point used by `hub init-secrets` Step 3 to
// guarantee that the REST API call in Step 3.5 will succeed. The
// dependency chain is encoded by composing the DataLayerReadiness and
// InfisicalReadiness phases from the health package — adding a new
// prerequisite (e.g., Redis) is a one-line change to phases.go.
//
// Per ADR-022 (Stable-but-Not-Ready Application Semantics), Day-0
// choreography embraces Kubernetes' eventual consistency. We do NOT
// require the full desired replica count to be Ready — that creates a
// fatal bottleneck when ArgoCD is mid-reconciliation (e.g., the
// multi-source race where the chart briefly renders with upstream
// defaults before our valueFiles resolve).
//
// Note: the bash bootstrap's step5 has a separate, stricter verification
// of the `infisical-auth` Secret artifact before marking init_secrets
// complete — that is the real safety net.
// infisicalDBCluster is the cluster Infisical uses: Infisical's own (ADR-112), and the
// box's bootstrap cluster because Day-0 cannot proceed without it.
//
// It resolves to the declaration in package constants rather than to a literal here.
// Day-0's readiness gate and this connection must name the SAME cluster, and when each
// spelled its own literal, correcting one left the bootstrap waiting on one database and
// connecting to another.
const infisicalDBCluster = constants.BootstrapDBCluster

func (i *Installer) WaitForInfisicalHealth(ctx context.Context) error {
	waiter := &health.HealthWaiter{
		Checkers: append(
			// The cluster Infisical actually uses. This wait exists to close the boot
			// race described above -- Infisical crash-looping on a database service
			// that does not exist yet -- so it must name the cluster Infisical
			// connects to; a gate on any other one leaves the race open while
			// appearing to guard against it.
			//
			// Named once, at infisicalDBCluster, so the gate and the connection cannot
			// diverge. No Pooler is named because infisical-db has none (ADR-112 §2).
			(&health.DataLayerReadiness{
				CNPGClusterName:  infisicalDBCluster,
				OwnerApplication: constants.BootstrapDBOwnerApplication,
			}).Checkers(),
			(&health.InfisicalReadiness{}).Checkers()...,
		),
		Interval: 5 * time.Second,
		Timeout:  45 * time.Minute,
		// No OnCheckStart/OnCheckPass: the waiter's own output carries the
		// step numbering, elapsed time and the reason it is still waiting.
	}
	if err := waiter.Wait(ctx, i.Kubeconfig); err != nil {
		return fmt.Errorf("infisical health check failed: %w", err)
	}
	fmt.Println("✓ Infisical is healthy (data layer + workload)")
	return nil
}

// RestartPlatformWorkloads performs a rolling restart of StatefulSets/Deployments
func (i *Installer) RestartPlatformWorkloads(ctx context.Context) error {
	config, err := clientcmd.BuildConfigFromFlags("", i.Kubeconfig)
	if err != nil {
		return fmt.Errorf("failed to load kubeconfig: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("failed to create kubernetes client: %w", err)
	}

	dataNamespace := constants.NamespaceData
	securityNamespace := constants.NamespaceSecurity
	patchData := []byte(fmt.Sprintf(`{"spec":{"template":{"metadata":{"annotations":{"kubectl.kubernetes.io/restartedAt":"%s"}}}}}`, time.Now().Format(time.RFC3339)))

	fmt.Println("[bootstrap-secrets] Changes detected. Triggering workload rollouts to sync...")

	// Restart Redis StatefulSet (Redis is in platform-data)
	if _, err = clientset.AppsV1().StatefulSets(dataNamespace).Patch(ctx, "redis-master", types.StrategicMergePatchType, patchData, metav1.PatchOptions{}); err != nil {
		if !k8serrors.IsNotFound(err) {
			return fmt.Errorf("failed to restart redis-master: %w", err)
		}
	}

	// Restart Infisical (chart may create StatefulSet or Deployment)
	if _, err = clientset.AppsV1().StatefulSets(securityNamespace).Patch(ctx, "infisical-standalone-infisical", types.StrategicMergePatchType, patchData, metav1.PatchOptions{}); err != nil {
		if k8serrors.IsNotFound(err) {
			// Try Deployment if StatefulSet not found (current chart)
			if _, depErr := clientset.AppsV1().Deployments(securityNamespace).Patch(ctx, "infisical-standalone-infisical", types.StrategicMergePatchType, patchData, metav1.PatchOptions{}); depErr != nil {
				if !k8serrors.IsNotFound(depErr) {
					return fmt.Errorf("failed to restart infisical: %w", depErr)
				}
			}
		} else {
			return fmt.Errorf("failed to restart infisical: %w", err)
		}
	}

	fmt.Println("[bootstrap-secrets] ✓ Workloads restarted successfully.")
	return nil
}
