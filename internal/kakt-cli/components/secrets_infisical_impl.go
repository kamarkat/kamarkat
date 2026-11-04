package components

import (
	"context"
	"encoding/base64"
	"fmt"
	"time"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/constants"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/health"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

func (i *Installer) InstallInfisicalAuth(ctx context.Context, clientID, clientSecret string) error {
	fmt.Println("[bootstrap] Creating infisical-auth secret...")

	// Load kubeconfig and create clientset
	config, err := clientcmd.BuildConfigFromFlags("", i.Kubeconfig)
	if err != nil {
		return fmt.Errorf("failed to load kubeconfig: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("failed to create kubernetes client: %w", err)
	}

	// Create namespace if it doesn't exist
	namespace := constants.NamespaceOps
	_, err = clientset.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if err != nil {
		// Namespace doesn't exist, create it
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		}
		_, err = clientset.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("failed to create namespace %s: %w", namespace, err)
		}
		fmt.Printf("[bootstrap] Created namespace %s\n", namespace)
	}

	// Create the secret
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "infisical-auth",
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "zero-ops-hub-cli",
				"app.kubernetes.io/component":  "secret-zero",
			},
		},
		Type: corev1.SecretTypeOpaque,
		StringData: map[string]string{
			"client-id":     clientID,
			"client-secret": clientSecret,
			"clientSecret":  clientSecret, // pki-issuer v0.2.0 compat
		},
	}

	// Try to create, if exists then update
	_, err = clientset.CoreV1().Secrets(namespace).Create(ctx, secret, metav1.CreateOptions{})
	if err != nil {
		// Secret might already exist, try to update
		_, err = clientset.CoreV1().Secrets(namespace).Update(ctx, secret, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("failed to create or update infisical-auth secret: %w", err)
		}
		fmt.Println("[bootstrap] ✓ infisical-auth secret updated")
	} else {
		fmt.Println("[bootstrap] ✓ infisical-auth secret created")
	}

	return nil
}

// FixArgoCDGitHubAuth creates the ArgoCD GitHub repository secret
// This is Secret Zero for ArgoCD - it fixes the chicken-and-egg problem.
// CRITICAL: This secret MUST be injected via client-go, NEVER stored in Git.
//
// Chicken-and-Egg Problem:
// - ArgoCD needs GitHub auth to pull ESO manifests from Git
// - ESO manages GitHub credentials declaratively
// - But ESO manifests are in Git, which ArgoCD can't pull without auth
//
// Solution:
// 1. This method injects temporary GitHub PAT using client-go
// 2. ArgoCD can now pull ESO manifests (waves 4-6)
// 3. ESO deploys and takes over credential management
// 4. ESO replaces this bootstrap secret with Infisical-backed secret
// 5. Future credential rotations happen via Infisical + ESO (GitOps)
//
// Production Workflow:
// 1. Developer runs: hub platform-core configure-eso --github-token=<pat>
// 2. This method uses client-go to inject the secret with proper labels
// 3. ArgoCD auto-discovers the secret (via argocd.argoproj.io/secret-type label)
// 4. ArgoCD syncs ESO manifests from GitHub
// 5. ESO takes over and replaces this secret with Infisical-backed version

// GenerateInfisicalCryptoSecrets generates the initial cryptographic keys and creates
// the Kubernetes Secrets that Infisical and CNPG need to boot. This MUST run before
// B02 (CNPG Cluster) so that infisical-db-credentials exists when CNPG's initdb runs.
//
// Creates: infisical-secrets (without DB_ROOT_CERT — deferred), infisical-redis-credentials, infisical-db-credentials
//
// Called by: GenerateLocalSecrets (between B01 and B02)
func (i *Installer) GenerateInfisicalCryptoSecrets(ctx context.Context) error {
	config, err := clientcmd.BuildConfigFromFlags("", i.Kubeconfig)
	if err != nil {
		return fmt.Errorf("failed to load kubeconfig: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("failed to create kubernetes client: %w", err)
	}

	securityNamespace := constants.NamespaceSecurity
	dataNamespace := constants.NamespaceData

	infSecret, err1 := clientset.CoreV1().Secrets(securityNamespace).Get(ctx, "infisical-secrets", metav1.GetOptions{})

	secretsExist := err1 == nil &&
		len(infSecret.Data["ENCRYPTION_KEY"]) > 0 &&
		len(infSecret.Data["REDIS_URL"]) > 0

	if secretsExist {
		fmt.Println("[bootstrap-secrets] Infisical secrets already exist, reusing")
	} else {
		fmt.Println("[bootstrap-secrets] Generating initial Infisical secrets...")
	}

	var encryptionKey, authSecret string

	if secretsExist {
		encryptionKey = string(infSecret.Data["ENCRYPTION_KEY"])
		authSecret = string(infSecret.Data["AUTH_SECRET"])
		fmt.Println("[bootstrap-secrets] Reusing existing ENCRYPTION_KEY and AUTH_SECRET")
	} else {
		encryptionKey, err = generateSecurePassword(32)
		if err != nil {
			return fmt.Errorf("failed to generate encryption key: %w", err)
		}
		authSecret, err = generateSecurePassword(32)
		if err != nil {
			return fmt.Errorf("failed to generate auth secret: %w", err)
		}
		fmt.Println("[bootstrap-secrets] Generated new ENCRYPTION_KEY and AUTH_SECRET")
	}
	redisURL := "redis://platform-redis.platform-data.svc:6379"

	secretData := map[string]string{
		"ENCRYPTION_KEY": encryptionKey,
		"AUTH_SECRET":    authSecret,
		"REDIS_URL":      redisURL,
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "infisical-secrets",
			Namespace: securityNamespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "zero-ops-hub-cli",
				"app.kubernetes.io/component":  "secret-zero",
			},
		},
		Type:       corev1.SecretTypeOpaque,
		StringData: secretData,
	}

	_, err = clientset.CoreV1().Secrets(securityNamespace).Create(ctx, secret, metav1.CreateOptions{})
	if err != nil {
		_, err = clientset.CoreV1().Secrets(securityNamespace).Update(ctx, secret, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("failed to create or update infisical-secrets: %w", err)
		}
		fmt.Println("[bootstrap-secrets] ✓ infisical-secrets updated (ENCRYPTION_KEY, AUTH_SECRET, REDIS_URL)")
	} else {
		fmt.Println("[bootstrap-secrets] ✓ infisical-secrets created (ENCRYPTION_KEY, AUTH_SECRET, REDIS_URL)")
	}

	// infisical-redis-credentials generation removed per ADR-014

	// Secret Zero: the bootstrap cluster's owner credential, created HERE because of
	// ordering.
	//
	// infisical-db's initdb reads this Secret (ADR-112), so it must exist before
	// boundary 02 creates the cluster. hub-operator also generates it and reuses an
	// existing one, but the operator is installed in B01 and reconciles on the
	// HubEnvironment CR -- later than this phase. Creating it here is what guarantees
	// initdb finds it rather than racing the operator.
	//
	// username is `infisical` to match the cluster's declared owner:
	// CloudNativePG takes the role name from this Secret when initdb is given one, so
	// the two must agree or the owner is created under the wrong name.
	_, err = clientset.CoreV1().Secrets(dataNamespace).Get(ctx, "infisical-db-credentials", metav1.GetOptions{})
	if err != nil {
		if !k8serrors.IsNotFound(err) {
			return fmt.Errorf("failed to check infisical-db-credentials secret: %w", err)
		}

		fmt.Println("[bootstrap-secrets] Generating infisical-db-credentials (CNPG Secret Zero)...")
		appPassword, err := generateSecurePassword(32)
		if err != nil {
			return fmt.Errorf("failed to generate bootstrap database password: %w", err)
		}

		appSecretObj := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "infisical-db-credentials",
				Namespace: dataNamespace,
				Labels: map[string]string{
					"app.kubernetes.io/managed-by":  "zero-ops-hub-cli",
					"app.kubernetes.io/component":   "secret-zero",
					"ops.nutgraf.in/db-credentials": "true",
				},
			},
			Type: corev1.SecretTypeBasicAuth,
			StringData: map[string]string{
				"username": "infisical",
				"password": appPassword,
			},
		}

		_, err = clientset.CoreV1().Secrets(dataNamespace).Create(ctx, appSecretObj, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("failed to create infisical-db-credentials secret: %w", err)
		}
		fmt.Println("[bootstrap-secrets] ✓ infisical-db-credentials created")
	} else {
		fmt.Println("[bootstrap-secrets] ✓ infisical-db-credentials already exists")
	}

	fmt.Println("[bootstrap-secrets] ✓ Cryptographic secrets ready (infisical-secrets, infisical-db-credentials)")
	return nil
}

// UpdateInfisicalSecretsWithCNPGCert waits for the CNPG Cluster to be Ready,
// reads the ca.crt from the CNPG-generated infisical-db-ca Secret, and injects
// DB_ROOT_CERT into infisical-secrets. This MUST run after B02 (CNPG Cluster)
// has been applied and the Cluster is healthy.
//
// Called by: BootstrapInfisicalAPI (after B03)
func (i *Installer) UpdateInfisicalSecretsWithCNPGCert(ctx context.Context) error {
	config, err := clientcmd.BuildConfigFromFlags("", i.Kubeconfig)
	if err != nil {
		return fmt.Errorf("failed to load kubeconfig: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("failed to create kubernetes client: %w", err)
	}

	securityNamespace := constants.NamespaceSecurity
	dataNamespace := constants.NamespaceData

	// Poll for CNPG Cluster to be Ready
	clusterWaiter := &health.HealthWaiter{
		Checkers: []health.HealthChecker{
			health.NewKubectlChecker("CNPG cluster "+constants.BootstrapDBCluster,
				[]string{"get", "clusters.postgresql.cnpg.io", constants.BootstrapDBCluster,
					"-n", dataNamespace,
					"-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}",
				},
			),
		},
		Timeout: 30 * time.Minute,
	}
	clusterWaiter.Checkers[0].(*health.KubectlChecker).Expected = "True"
	if err := clusterWaiter.Wait(ctx, i.Kubeconfig); err != nil {
		return fmt.Errorf("CNPG cluster %s not ready: %w\nEnsure %s created it and the CNPG operator is running",
			constants.BootstrapDBCluster, err, constants.BootstrapDBOwnerApplication)
	}
	fmt.Printf("✓ CNPG cluster %s is Ready\n", constants.BootstrapDBCluster)

	// Poll for infisical-db-ca Secret to exist and contain ca.crt.
	// CNPG creates this Secret during cluster bootstrap, but there is a
	// brief propagation window between "Cluster Ready" and the Secret being
	// fully populated. This bounded poll handles that edge case.
	fmt.Println("Waiting for infisical-db-ca Secret to be populated...")
	var caCert []byte
	pollErr := wait.PollImmediateWithContext(ctx, 2*time.Second, 1*time.Minute, func(ctx context.Context) (bool, error) {
		caSecret, err := clientset.CoreV1().Secrets(dataNamespace).Get(ctx, "infisical-db-ca", metav1.GetOptions{})
		if err != nil {
			return false, nil // keep polling
		}
		cert, ok := caSecret.Data["ca.crt"]
		if !ok || len(cert) == 0 {
			return false, nil // keep polling
		}
		caCert = cert
		return true, nil
	})
	if pollErr != nil {
		return fmt.Errorf("infisical-db-ca Secret not populated: %w", pollErr)
	}
	fmt.Println("✓ infisical-db-ca Secret found and populated")

	dbRootCert := base64.StdEncoding.EncodeToString(caCert)
	fmt.Println("Updating infisical-secrets with DB_ROOT_CERT...")

	updateSecret, err := clientset.CoreV1().Secrets(securityNamespace).Get(ctx, "infisical-secrets", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to read infisical-secrets for DB_ROOT_CERT update: %w", err)
	}
	if existingCert, ok := updateSecret.Data["DB_ROOT_CERT"]; ok {
		if string(existingCert) == dbRootCert {
			fmt.Println("✓ DB_ROOT_CERT already configured with correct value")
			return nil
		}
		fmt.Println("⚠️  DB_ROOT_CERT exists but differs, updating...")
	}
	if updateSecret.Data == nil {
		updateSecret.Data = make(map[string][]byte)
	}
	updateSecret.Data["DB_ROOT_CERT"] = []byte(dbRootCert)
	_, err = clientset.CoreV1().Secrets(securityNamespace).Update(ctx, updateSecret, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update infisical-secrets with DB_ROOT_CERT: %w", err)
	}
	fmt.Println("✓ Added DB_ROOT_CERT to infisical-secrets")
	return nil
}

func (i *Installer) UpgradeInfisicalTLS(ctx context.Context) error {
	// Load kubeconfig and create clientset
	config, err := clientcmd.BuildConfigFromFlags("", i.Kubeconfig)
	if err != nil {
		return fmt.Errorf("failed to load kubeconfig: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("failed to create kubernetes client: %w", err)
	}

	namespace := constants.NamespaceData

	fmt.Println("\n[1/3] Reading infisical-db-ca secret...")

	// Read CA certificate from CNPG-managed secret
	caSecret, err := clientset.CoreV1().Secrets(namespace).Get(ctx, "infisical-db-ca", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to read infisical-db-ca secret: %w\nEnsure the cluster is deployed: kubectl -n %s get clusters.postgresql.cnpg.io %s",
			err, constants.NamespaceData, constants.BootstrapDBCluster)
	}

	caCert, ok := caSecret.Data["ca.crt"]
	if !ok {
		return fmt.Errorf("ca.crt not found in infisical-db-ca secret")
	}

	// Base64 encode the CA certificate for Infisical
	dbRootCert := base64.StdEncoding.EncodeToString(caCert)
	fmt.Println("✓ Read CA certificate from infisical-db-ca")

	fmt.Println("\n[2/3] Updating infisical-secrets with DB_ROOT_CERT...")

	// Read existing infisical-secrets
	infSecret, err := clientset.CoreV1().Secrets(namespace).Get(ctx, "infisical-secrets", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to read infisical-secrets: %w", err)
	}

	// Check if already has DB_ROOT_CERT
	if existingCert, ok := infSecret.Data["DB_ROOT_CERT"]; ok {
		if string(existingCert) == dbRootCert {
			fmt.Println("✓ DB_ROOT_CERT already configured with correct value")
			fmt.Println("   Infisical is already using TLS connection")
			return nil
		}
		fmt.Println("⚠️  DB_ROOT_CERT exists but differs, updating...")
	}

	// Add DB_ROOT_CERT to existing secret
	if infSecret.Data == nil {
		infSecret.Data = make(map[string][]byte)
	}
	infSecret.Data["DB_ROOT_CERT"] = []byte(dbRootCert)

	// Update the secret
	_, err = clientset.CoreV1().Secrets(namespace).Update(ctx, infSecret, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update infisical-secrets: %w", err)
	}
	fmt.Println("✓ Added DB_ROOT_CERT to infisical-secrets")

	fmt.Println("\n[3/3] Restarting Infisical pods to apply TLS configuration...")

	// Restart Infisical pods by deleting them (Deployment will recreate)
	pods, err := clientset.CoreV1().Pods(constants.NamespaceSecurity).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=infisical",
	})
	if err != nil {
		return fmt.Errorf("failed to list Infisical pods: %w", err)
	}

	if len(pods.Items) == 0 {
		fmt.Println("⚠️  No Infisical pods found, skipping restart")
		fmt.Println("   Pods will use TLS when they start")
		return nil
	}

	for _, pod := range pods.Items {
		err = clientset.CoreV1().Pods(constants.NamespaceSecurity).Delete(ctx, pod.Name, metav1.DeleteOptions{})
		if err != nil {
			fmt.Printf("⚠️  Failed to delete pod %s: %v\n", pod.Name, err)
		} else {
			fmt.Printf("✓ Deleted pod %s (will be recreated with TLS)\n", pod.Name)
		}
	}

	// Wait for pods to be recreated
	fmt.Println("\nWaiting for Infisical pods to be ready...")
	for i := 0; i < 60; i++ {
		time.Sleep(2 * time.Second)

		pods, err := clientset.CoreV1().Pods(constants.NamespaceSecurity).List(ctx, metav1.ListOptions{
			LabelSelector: "app.kubernetes.io/name=infisical",
		})
		if err != nil {
			continue
		}

		allReady := true
		for _, pod := range pods.Items {
			if pod.Status.Phase != corev1.PodRunning {
				allReady = false
				break
			}
			for _, condition := range pod.Status.Conditions {
				if condition.Type == corev1.PodReady && condition.Status != corev1.ConditionTrue {
					allReady = false
					break
				}
			}
		}

		if allReady && len(pods.Items) > 0 {
			fmt.Println("✓ Infisical pods are ready")
			return nil
		}
	}

	fmt.Println("⚠️  Timeout waiting for Infisical pods to be ready")
	fmt.Println("   Check pod status: kubectl get pods -n platform-security")
	return nil
}
