package controller

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cnpgv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	"github.com/kamarkat/kamarkat/internal/pki"
	"github.com/kamarkat/kamarkat/internal/platform/escrow"
	opsv1alpha1 "github.com/kamarkat/kamarkat/operators/hub-operator/api/v1alpha1"
	infisicalclient "github.com/kamarkat/kamarkat/operators/hub-operator/internal/client"
	"github.com/kamarkat/kamarkat/operators/hub-operator/internal/database"
	"github.com/kamarkat/kamarkat/operators/hub-operator/internal/infisical"
	"github.com/kamarkat/kamarkat/operators/hub-operator/internal/secrets"
)

// HubEnvironmentReconciler reconciles a HubEnvironment object
type HubEnvironmentReconciler struct {
	client.Client
	// UncachedClient reads secrets directly from API server (bypasses cache)
	// Use for operational secrets: infisical-db-superuser, infisical-db-ca, infisical-auth
	// These must not have their data stripped by the cache transformer
	UncachedClient client.Client
	Scheme         *runtime.Scheme
}

//+kubebuilder:rbac:groups=ops.nutgraf.in,resources=hubenvironments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=ops.nutgraf.in,resources=hubenvironments/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=ops.nutgraf.in,resources=hubenvironments/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch
//+kubebuilder:rbac:groups=postgresql.cnpg.io,resources=clusters,verbs=get;list;watch
//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=admissionregistration.k8s.io,resources=validatingwebhookconfigurations,verbs=get;list;watch
//+kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch
//+kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop
// Requirement 9.3: Implement Reconcile() main entry point
func (r *HubEnvironmentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// 1. Fetch HubEnvironment CR
	hubEnv := &opsv1alpha1.HubEnvironment{}
	if err := r.Get(ctx, req.NamespacedName, hubEnv); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// REQ-10: Handle finalizer for graceful teardown
	// REQ-13: Remove finalizers from secrets when HubEnvironment is being deleted
	if !hubEnv.DeletionTimestamp.IsZero() {
		logger.Info("HubEnvironment is being deleted, removing finalizers from secrets")

		// Remove finalizers from infisical-secrets
		if err := r.removeFinalizer(ctx, "infisical-secrets", infisical.InfisicalServiceNamespace); err != nil {
			logger.Error(err, "Failed to remove finalizer from infisical-secrets")
			return ctrl.Result{RequeueAfter: 5 * time.Second}, err
		}

		// Remove finalizers from infisical-redis-credentials
		dataNamespace := hubEnv.Spec.Database.Namespace
		if err := r.removeFinalizer(ctx, "infisical-redis-credentials", dataNamespace); err != nil {
			logger.Error(err, "Failed to remove finalizer from infisical-redis-credentials")
			return ctrl.Result{RequeueAfter: 5 * time.Second}, err
		}

		logger.Info("Successfully removed finalizers from secrets, allowing deletion to proceed")
		return ctrl.Result{}, nil
	}

	// ADR-078 add.5 §4: the hub's Loki writes to <clusterName>-logs. Non-fatal:
	// the hub's logs are degraded without it, the box is not.
	if err := ensureLogsBucket(ctx, r.UncachedClient, hubEnv.Spec.ClusterName); err != nil {
		logger.Error(err, "Failed to ensure the hub's log bucket", "cluster", hubEnv.Spec.ClusterName)
	}

	// Requirement 9.4: Handle reconcile-trigger annotation
	if triggerTime, ok := hubEnv.Annotations["ops.nutgraf.in/reconcile-trigger"]; ok {
		logger.Info("Manual reconciliation triggered", "timestamp", triggerTime)

		// Clear ALL conditions and uploaded secrets to force full Phase 1 re-run
		// This ensures infisical-secrets gets recreated in the correct namespace
		hubEnv.Status.Conditions = []metav1.Condition{}
		hubEnv.Status.UploadedSecrets = []string{}

		// Remove the annotation after processing
		delete(hubEnv.Annotations, "ops.nutgraf.in/reconcile-trigger")
		if err := r.Update(ctx, hubEnv); err != nil {
			return ctrl.Result{}, err
		}

		// Update status to clear conditions
		if err := r.Status().Update(ctx, hubEnv); err != nil {
			return ctrl.Result{}, err
		}

		logger.Info("Cleared all conditions and uploaded secrets, forcing full reconciliation")
		return ctrl.Result{Requeue: true}, nil
	}

	// Keep Infisical's copy of the CNPG CA current.
	//
	// DB_ROOT_CERT is how Infisical verifies TLS to Postgres. It is written once at
	// Day-0 by the bootstrap CLI, and nothing reconciled it afterwards — so any
	// event that regenerates the cluster CA (a CNPG Cluster rebuild, which is what
	// a lost node-pinned volume forces) left Infisical trusting a CA that no longer
	// signs anything. It then failed its boot migration with SELF_SIGNED_CERT_IN_CHAIN
	// and crash-looped, which blocked Phase 0, which blocked database roles, which
	// left the whole auth tier in init. One stale copied certificate, total outage.
	//
	// This runs before Phase 1 because Infisical must be able to reach its database
	// before anything else in this reconcile is meaningful.
	if err := r.ensureInfisicalDBRootCert(ctx); err != nil {
		logger.Error(err, "Failed to reconcile Infisical DB_ROOT_CERT; continuing")
	}

	// The hub's public ingress address, read from kube-system/kubeadm-config and
	// published for the gateway chart (see ingress_address.go). First, because it
	// depends on nothing else in this reconcile and every public hostname on the
	// box depends on it -- and because it is the piece that makes a broken box
	// repairable by a bundle upgrade alone, with no CLI and no repository edit.
	if err := r.reconcileIngressAddress(ctx, hubEnv); err != nil {
		logger.Error(err, "Failed to publish the hub ingress address; continuing")
	}

	// The escrow, on every reconcile.
	//
	// It used to be built inside the Phase 1 block, which is entered only while
	// BootstrapSecretsGenerated is stale or infisical-secrets is missing. So the
	// escrow was written at most once, on the pass that generated the keys, and
	// never again -- which made two claims false at once. The kubeconfig escrow
	// said "every reconcile rather than once ... a rotated credential replaces a
	// stale escrowed copy", and it did not run at all after bootstrap. And an
	// escrow that was empty for any reason -- a write that failed, credentials
	// supplied later, the keys written under a different name -- stayed empty for
	// the life of the box, because the only code that would have filled it was
	// behind a condition that never came true again.
	//
	// Hoisted here it is a reconciler like any other: it states what must be true
	// and makes it true on every pass, rather than acting once and assuming.
	clusterID := strings.TrimSpace(hubEnv.Spec.ClusterName)
	if clusterID == "" {
		err := fmt.Errorf("spec.clusterName is empty: this box's master keys and " +
			"admin kubeconfig would be escrowed under a name nothing reads back")
		logger.Error(err, "Refusing to reconcile an unnamed cluster")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, err
	}

	// The box this cluster belongs to, which is what the escrow keys everything
	// that must outlive the cluster by (ADR-111 ss1).
	//
	// Not defaulted from the cluster name. A default would put one box's master
	// keys under its first cluster's name, so the successor that replaced that
	// cluster would look for them under its own and find nothing -- the secret
	// store unrecoverable at precisely the moment the escrow exists to serve.
	boxID := strings.TrimSpace(hubEnv.Spec.BoxID)
	if boxID == "" {
		err := fmt.Errorf("spec.boxId is empty: the escrow keys this box's master keys " +
			"and its identity provider's encryption key by the box, because they outlive " +
			"any one cluster, and there is no safe value to assume for it (ADR-111)")
		logger.Error(err, "Refusing to reconcile a box with no id")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, err
	}
	escrowID := escrow.ForCluster(boxID, clusterID)

	// Fatal. This was an Info log and a nil client, which meant a box whose escrow
	// credentials were missing, wrong, or unreachable bootstrapped normally and
	// reported healthy while nothing was ever escrowed -- the master keys existing
	// only inside the cluster they decrypt, which is the one state ADR-076 exists
	// to prevent.
	escrowClient, err := escrow.NewEscrowClient(ctx, infisical.InfisicalBaseURL)
	if err != nil {
		logger.Error(err, "escrow unavailable")
		return ctrl.Result{RequeueAfter: 30 * time.Second},
			fmt.Errorf("escrow unavailable, and this box's irreplaceable material must "+
				"not exist only inside the cluster (ADR-076): %w", err)
	}

	if err := r.reconcileEscrow(ctx, escrowClient, escrowID); err != nil {
		logger.Error(err, "Failed to reconcile the escrow", "clusterID", clusterID)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, err
	}

	// Phase 1: Generate Bootstrap Secrets Only
	// MUST run before Phase 0 (Infisical bootstrap) because Infisical itself depends on
	// secrets created here (infisical-db-credentials, infisical-postgres-connection).
	// Without these secrets the setup-infisical-role job cannot create the DB role,
	// Infisical pods crash with "no such user", and Phase 0 deadlocks forever.
	//
	// Bootstrap secrets are required for infrastructure to start (CNPG, Infisical)
	// Application secrets are created by ESO from Infisical
	// REQ-7: Always check if infisical-secrets exists, even if BootstrapSecretsGenerated=true
	// This handles the case where the secret was deleted and needs restoration from AWS
	securityNamespace := infisical.InfisicalServiceNamespace
	infisicalSecret := &corev1.Secret{}
	infisicalSecretExists := true
	if err := r.UncachedClient.Get(ctx, client.ObjectKey{Name: "infisical-secrets", Namespace: securityNamespace}, infisicalSecret); err != nil {
		infisicalSecretExists = false
	}

	if !isConditionTrueAndUpToDate(hubEnv.Status.Conditions, "BootstrapSecretsGenerated", hubEnv.Generation) || !infisicalSecretExists {
		if !infisicalSecretExists {
			logger.Info("Phase 1: infisical-secrets missing, attempting restore from AWS backup")
		} else {
			logger.Info("Phase 1: Generating Bootstrap Secrets")
		}

		dataNamespace := hubEnv.Spec.Database.Namespace
		// The bootstrap cluster the CR declares, not a literal. These secrets carry
		// Infisical's own database connection, so the cluster is Infisical's
		// (Spec.Database.ClusterRef, ADR-112) and no other application's.
		// PoolerHost, not PrimaryHost: this is Infisical's own application connection
		// and it goes through the cluster's PgBouncer (infisical-db-pooler). The
		// operator's role management and migrations still use PrimaryHost -- see
		// database.Endpoint -- because those are writes that must not be pooled.
		dbHost := database.Endpoint{
			Cluster:   hubEnv.Spec.Database.ClusterRef,
			Namespace: dataNamespace,
		}.PoolerHost()

		// Create owner reference.
		//
		// GVK comes from the scheme, not from hubEnv.TypeMeta: a typed client clears
		// TypeMeta on Get, so APIVersion/Kind read back empty and the API server
		// rejects the reference with "version must not be empty". The object's own
		// GroupVersion is the authority here and is always populated.
		owner := metav1.OwnerReference{
			APIVersion: opsv1alpha1.GroupVersion.String(),
			Kind:       "HubEnvironment",
			Name:       hubEnv.Name,
			UID:        hubEnv.UID,
			Controller: func() *bool { b := true; return &b }(),
		}

		// Build list of bootstrap secret names only
		// These are required for infrastructure bootstrap (CNPG, Infisical, Redis)
		secretNamesInData := []string{
			"infisical-db-credentials",    // Infisical database user
			"infisical-db-ca",             // TLS certificate authority
			"infisical-redis-credentials", // Redis authentication
		}

		secretNamesInSecurity := []string{
			"infisical-secrets",             // Infisical encryption keys
			"infisical-postgres-connection", // Infisical DB connection
		}

		// Read existing secrets for idempotency
		existingSecrets := make(map[string]*corev1.Secret)

		// Read secrets from data namespace using UncachedClient (need secret data, not just metadata)
		for _, name := range secretNamesInData {
			secret := &corev1.Secret{}
			if err := r.UncachedClient.Get(ctx, client.ObjectKey{Name: name, Namespace: dataNamespace}, secret); err == nil {
				existingSecrets[name] = secret
			}
		}

		// Read secrets from security namespace using UncachedClient (need secret data, not just metadata)
		for _, name := range secretNamesInSecurity {
			secret := &corev1.Secret{}
			if err := r.UncachedClient.Get(ctx, client.ObjectKey{Name: name, Namespace: securityNamespace}, secret); err == nil {
				existingSecrets[name] = secret
			}
		}

		// REQ-7: Bootstrap detection - determine if this is first-time bootstrap
		isFirstTime := !isConditionTrueAndUpToDate(hubEnv.Status.Conditions, "BootstrapSecretsGenerated", hubEnv.Generation)
		logger.Info("Bootstrap detection", "isFirstTime", isFirstTime, "infisicalSecretExists", infisicalSecretExists)

		logger.Info("Starting bootstrap secrets generation", "clusterID", clusterID)

		// Generate Bootstrap Secrets with AWS backup/restore support
		result, err := secrets.GenerateBootstrapSecrets(ctx, dataNamespace, securityNamespace, dbHost, owner, existingSecrets, isFirstTime, escrowID, escrowClient)
		if err != nil {
			logger.Error(err, "Failed to generate Bootstrap Secrets")
			return ctrl.Result{RequeueAfter: 10 * time.Second}, err
		}

		logger.Info("Bootstrap secrets generated successfully")

		// Create bootstrap secrets only
		secretsToCreate := []*corev1.Secret{
			result.InfisicalDBCredentials,
			result.InfisicalSecrets,
			result.InfisicalRedisCredentials,
			result.InfisicalPostgresConnection,
		}

		for _, secret := range secretsToCreate {
			if secret == nil {
				continue
			}
			if err := r.Create(ctx, secret); err != nil {
				if !errors.IsAlreadyExists(err) {
					logger.Error(err, "Failed to create secret", "secret", secret.Name, "namespace", secret.Namespace)
					return ctrl.Result{RequeueAfter: 10 * time.Second}, err
				}
			}
			logger.Info("Created bootstrap secret", "secret", secret.Name, "namespace", secret.Namespace)
		}

		// The admin kubeconfig, escrowed beside the master keys (ADR-076).
		//
		// Every reconcile rather than once: the client certificate CAPI issues is on
		// kubeadm's one-year default, and re-reading each pass means a rotated
		// credential replaces a stale escrowed copy without anyone remembering to.
		// A copy that silently expired is the failure this is guarding against.

		// Reaching here means every escrow write above succeeded, so the message
		// states it plainly. It used to say "generated and escrowed" whenever a
		// client merely existed, which was true of a run whose only escrow write
		// had failed and been logged at Info -- a status claiming the backup that
		// the same reconcile had just not made.
		statusMessage := "Bootstrap secrets generated and escrowed"
		if !isFirstTime {
			statusMessage = "Bootstrap secrets restored from the escrow"
		}

		meta.SetStatusCondition(&hubEnv.Status.Conditions, metav1.Condition{
			Type:               "BootstrapSecretsGenerated",
			Status:             metav1.ConditionTrue,
			Reason:             "Generated",
			Message:            statusMessage,
			ObservedGeneration: hubEnv.Generation,
		})

		if err := r.Status().Update(ctx, hubEnv); err != nil {
			return ctrl.Result{}, err
		}

		logger.Info("Phase 1 complete: Bootstrap secrets generated and escrowed", "clusterID", clusterID, "isFirstTime", isFirstTime)
		return ctrl.Result{Requeue: true}, nil
	}

	// Phase 0: Bootstrap Infisical (Day 0 initialization)
	// Runs after Phase 1 so that infisical-db-credentials and infisical-postgres-connection
	// already exist. The setup-infisical-role job (ArgoCD wave 3) mounts these secrets to
	// create the "infisical" PostgreSQL role. Only once that role exists can the Infisical
	// pods start successfully and allow this phase to proceed.
	logger.Info("Phase 0: Bootstrapping Infisical")

	infisicalReady, err := r.isInfisicalReady(ctx, hubEnv)
	if err != nil {
		logger.Error(err, "Failed to check Infisical readiness")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, err
	}
	if !infisicalReady {
		logger.Info("Waiting for Infisical to be ready")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Bootstrap Infisical (creates admin user, project, machine identity)
	// Returns (true, nil) if bootstrap was performed, (false, nil) if already bootstrapped
	bootstrapClient := infisical.NewBootstrapClient(r.Client, r.UncachedClient)
	bootstrapped, err := bootstrapClient.Bootstrap(ctx)
	if err != nil {
		logger.Error(err, "Failed to bootstrap Infisical")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, err
	}

	if bootstrapped {
		logger.Info("Infisical bootstrapped successfully")
	} else {
		logger.Info("Infisical already bootstrapped, skipping")
	}

	// Upload CLI-injected secrets to Infisical (bootstrap secrets from K8s)
	// This runs regardless of whether bootstrap just occurred or was already done
	// Makes Infisical the Source of Truth for all secrets
	secretUploader := infisical.NewSecretUploader(r.Client, r.UncachedClient)
	if err := secretUploader.UploadCLISecrets(ctx); err != nil {
		logger.Error(err, "Failed to upload CLI secrets, continuing...")
	} else {
		logger.Info("CLI secrets uploaded to Infisical")
	}

	// Upload application secrets directly to Infisical (without creating K8s secrets)
	// ESO will create K8s secrets by syncing from Infisical
	appSecretUploader := infisical.NewApplicationSecretUploader(r.Client, r.UncachedClient, escrowClient, escrowID)
	if err := appSecretUploader.UploadApplicationSecrets(ctx); err != nil {
		logger.Error(err, "Failed to upload application secrets, continuing...")
	} else {
		logger.Info("Application secrets uploaded to Infisical")
	}

	// Enforce PKI Templates (Self-Healing)
	if err := r.ensurePKITemplates(ctx, hubEnv); err != nil {
		logger.Error(err, "Failed to ensure PKI templates")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, err
	}

	logger.Info("Phase 0 complete: Infisical bootstrapped")

	// Phase 1b: Wait for ESO to create application secrets
	// Application secrets are created by ESO from Infisical (creationPolicy: Owner)
	// We must wait for them to exist before creating database roles
	if !isConditionTrueAndUpToDate(hubEnv.Status.Conditions, "ApplicationSecretsReady", hubEnv.Generation) {
		logger.Info("Phase 1b: Waiting for ESO to create application secrets")

		dataNamespace := hubEnv.Spec.Database.Namespace

		// The application credentials to wait for, DERIVED from the roles this box
		// declares rather than listed here.
		//
		// A hardcoded list is a second declaration of the same fact, and it drifts: it
		// outlives the roles it was written for, so the gate waits a minute for
		// credentials nothing will deliver and then reports the box not ready.
		//
		// Derived, the gate covers exactly the roles RoleManager will provision -- it
		// reads the same declaration through the same naming function -- so a role added
		// to the CR cannot be left out of the gate, and one removed cannot linger in it.
		requiredSecrets := map[string]string{}
		for _, role := range hubEnv.Spec.Database.Roles {
			requiredSecrets[database.RoleSecretName(role.Name)] =
				database.RoleSecretNamespace(role.Name, dataNamespace)
		}

		var missingSecrets []string
		var allSecretsExist bool

		// Wait IN-PLACE for ESO to create application secrets instead of requeuing
		// This prevents Phase 0 from re-running every 10s and bombarding Infisical with API requests
		err := wait.PollImmediateWithContext(ctx, 2*time.Second, 1*time.Minute, func(ctx context.Context) (bool, error) {
			allSecretsExist = true
			missingSecrets = []string{}

			for secretName, namespace := range requiredSecrets {
				secret := &corev1.Secret{}
				if err := r.Get(ctx, client.ObjectKey{
					Name:      secretName,
					Namespace: namespace,
				}, secret); err != nil {
					if errors.IsNotFound(err) {
						logger.Info("Waiting for ESO to create secret", "secret", secretName, "namespace", namespace)
						allSecretsExist = false
						missingSecrets = append(missingSecrets, secretName)
					} else {
						return false, err
					}
				}
			}
			return allSecretsExist, nil
		})

		if err != nil || !allSecretsExist {
			meta.SetStatusCondition(&hubEnv.Status.Conditions, metav1.Condition{
				Type:               "ApplicationSecretsReady",
				Status:             metav1.ConditionFalse,
				Reason:             "WaitingForESO",
				Message:            fmt.Sprintf("Waiting for ESO to create secrets: %s", strings.Join(missingSecrets, ", ")),
				ObservedGeneration: hubEnv.Generation,
			})
			if updateErr := r.Status().Update(ctx, hubEnv); updateErr != nil {
				return ctrl.Result{}, updateErr
			}
			// If we timed out after 1 minute, requeue and try again
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}

		meta.SetStatusCondition(&hubEnv.Status.Conditions, metav1.Condition{
			Type:               "ApplicationSecretsReady",
			Status:             metav1.ConditionTrue,
			Reason:             "ESOSynced",
			Message:            "ESO created all application secrets successfully",
			ObservedGeneration: hubEnv.Generation,
		})

		if err := r.Status().Update(ctx, hubEnv); err != nil {
			return ctrl.Result{}, err
		}

		logger.Info("Phase 1b complete: Application secrets ready")
		return ctrl.Result{Requeue: true}, nil
	}

	// Requirement 9.11: Wait for CNPG Cluster Ready
	cnpgReady, err := r.isCNPGReady(ctx, hubEnv)
	if err != nil {
		logger.Error(err, "Failed to check CNPG readiness")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, err
	}
	if !cnpgReady {
		logger.Info("Waiting for CNPG Cluster to be ready")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Phase 2: Provision database roles
	// Must run after ApplicationSecretsReady (ESO has created the credential
	// secrets that RoleManager reads) and after CNPG is ready.
	//
	// The condition alone does not gate this phase. A condition records what this
	// operator did; it says nothing about what the database currently contains,
	// and the two diverge whenever the cluster's data directory is lost — a node
	// rebuilt under node-pinned local-path storage being the case that actually
	// occurred. PostgreSQL then holds only its bootstrap user while the condition
	// still reads True, so provisioning is skipped permanently and every service
	// fails authentication against a role that no longer exists.
	//
	// Verifying costs one query per declared role against a database this
	// reconcile has already connected to, and it turns a permanent outage into a
	// self-healing one.
	rolesProvisioned := isConditionTrueAndUpToDate(hubEnv.Status.Conditions, "DatabaseRolesProvisioned", hubEnv.Generation)
	if rolesProvisioned {
		// Every cluster the declared roles live on, not just the box's own. After
		// ADR-112 a role can be on an application's cluster, and verifying only the
		// shared one reports roles as present because it never looked where they are.
		var missing []string
		for _, cluster := range hubEnv.Spec.DatabaseClusters() {
			verifier, err := database.NewRoleManager(ctx, r.UncachedClient, database.Endpoint{
				Cluster:   cluster,
				Namespace: hubEnv.Spec.Database.Namespace,
			})
			if err != nil {
				// Unreachable database is not evidence the roles are gone. Retry
				// rather than re-provisioning against a cluster we cannot see.
				logger.Error(err, "Failed to create RoleManager for role verification",
					"cluster", cluster)
				return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
			}
			absent, err := verifier.MissingRoles(ctx, hubEnv)
			verifier.Close()
			if err != nil {
				logger.Error(err, "Failed to verify database roles", "cluster", cluster)
				return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
			}
			missing = append(missing, absent...)
		}
		if len(missing) > 0 {
			logger.Info("Declared database roles are absent despite DatabaseRolesProvisioned=True; re-provisioning",
				"missing", missing)
			meta.SetStatusCondition(&hubEnv.Status.Conditions, metav1.Condition{
				Type:               "DatabaseRolesProvisioned",
				Status:             metav1.ConditionFalse,
				Reason:             "RolesMissing",
				Message:            fmt.Sprintf("Roles absent from PostgreSQL and being re-provisioned: %v", missing),
				ObservedGeneration: hubEnv.Generation,
			})
			if err := r.Status().Update(ctx, hubEnv); err != nil {
				return ctrl.Result{}, err
			}
			rolesProvisioned = false
		}
	}

	if !rolesProvisioned {
		logger.Info("Phase 2: Provisioning database roles")

		// One manager per cluster. Each acts only on the roles declared for it
		// (RolesFor), so a role on an application's cluster is created there and
		// nowhere else.
		provisionErr := func() error {
			for _, cluster := range hubEnv.Spec.DatabaseClusters() {
				roleManager, err := database.NewRoleManager(ctx, r.UncachedClient, database.Endpoint{
					Cluster:   cluster,
					Namespace: hubEnv.Spec.Database.Namespace,
				})
				if err != nil {
					return fmt.Errorf("role manager for %s: %w", cluster, err)
				}
				// TODO(ADR-023): Replace with Crossplane provider-sql for roles.
				// Migrations have no declarative owner since Atlas Operator was removed.
				err = roleManager.CreateOrUpdateRoles(ctx, hubEnv)
				roleManager.Close()
				if err != nil {
					return fmt.Errorf("roles on %s: %w", cluster, err)
				}
			}
			return nil
		}()
		if err := provisionErr; err != nil {
			// This phase blocks every phase after it, so the message says so.
			// A reconcile that stops here leaves the identity provider unable to
			// authenticate to Postgres, and the only visible symptom is a
			// CrashLoop in a different namespace.
			logger.Error(err, "Failed to provision database roles — reconcile stops here; no later phase runs",
				"nextPhasesBlocked", []string{"IdentityReady"})
			return ctrl.Result{RequeueAfter: 15 * time.Second}, err
		}
		logger.Info("Phase 2 complete: database roles provisioned")

		meta.SetStatusCondition(&hubEnv.Status.Conditions, metav1.Condition{
			Type:               "DatabaseRolesProvisioned",
			Status:             metav1.ConditionTrue,
			Reason:             "Provisioned",
			Message:            "Database roles created/updated successfully",
			ObservedGeneration: hubEnv.Generation,
		})

		if err := r.Status().Update(ctx, hubEnv); err != nil {
			return ctrl.Result{}, err
		}

		logger.Info("Phase 2 complete: Database roles provisioned")
		return ctrl.Result{Requeue: true}, nil
	}

	// Requirement 9.8: Phase 3 - Upload Secrets to Infisical
	if !isConditionTrueAndUpToDate(hubEnv.Status.Conditions, "SecretsBackedUp", hubEnv.Generation) {
		logger.Info("Phase 3: Uploading secrets to Infisical")

		// Requirement 9.11: Check if Infisical is ready
		infisicalReady, err := r.isInfisicalReady(ctx, hubEnv)
		if err != nil {
			logger.Error(err, "Failed to check Infisical readiness")
			return ctrl.Result{RequeueAfter: 10 * time.Second}, err
		}
		if !infisicalReady {
			logger.Info("Waiting for Infisical to be ready")
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}

		// Check if infisical-auth secret exists
		infisicalAuth := &corev1.Secret{}
		if err := r.Get(ctx, client.ObjectKey{
			Name:      "infisical-auth",
			Namespace: infisical.NamespaceOps,
		}, infisicalAuth); err != nil {
			if errors.IsNotFound(err) {
				logger.Info("infisical-auth secret not found, suspending Phase 3")
				meta.SetStatusCondition(&hubEnv.Status.Conditions, metav1.Condition{
					Type:               "SecretsBackedUp",
					Status:             metav1.ConditionFalse,
					Reason:             "AuthenticationFailed",
					Message:            "infisical-auth secret not found. Phase 3 suspended.",
					ObservedGeneration: hubEnv.Generation,
				})
				if err := r.Status().Update(ctx, hubEnv); err != nil {
					return ctrl.Result{}, err
				}
				return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
			}
			return ctrl.Result{RequeueAfter: 10 * time.Second}, err
		}

		// All secrets are now uploaded via SecretUploader in Phase 0
		// No need for separate Phase 3 upload logic

		meta.SetStatusCondition(&hubEnv.Status.Conditions, metav1.Condition{
			Type:               "SecretsBackedUp",
			Status:             metav1.ConditionTrue,
			Reason:             "Uploaded",
			Message:            "Secrets uploaded to Infisical successfully",
			ObservedGeneration: hubEnv.Generation,
		})

		if err := r.Status().Update(ctx, hubEnv); err != nil {
			return ctrl.Result{}, err
		}

		logger.Info("Phase 3a complete: Secrets backed up to Infisical")
		return ctrl.Result{Requeue: true}, nil
	}

	// Identity readiness is no longer gated on a Deployment this operator watches,
	// and OAuth clients are no longer registered here (ADR-060).
	//
	// Zitadel generates client secrets itself and discloses them once, so there
	// is nothing for this operator to push, and the Tenant Identity Service owns
	// provisioning (ADR-041).
	//
	// Leaving the readiness gate in place would be worse than leaving it out: the
	// Deployment it waits for cannot exist, so the reconcile would stall on
	// "Waiting for identity" for ever while reporting a healthy operator.
	logger.Info("Phase 3: identity provider lifecycle is external (ADR-060); marking IdentityReady")
	meta.SetStatusCondition(&hubEnv.Status.Conditions, metav1.Condition{
		Type:               "IdentityReady",
		Status:             metav1.ConditionTrue,
		Reason:             "IdentityProviderExternal",
		Message:            "Identity provider lifecycle is owned by the Tenant Identity Service (ADR-060)",
		ObservedGeneration: hubEnv.Generation,
	})
	if err := r.Status().Update(ctx, hubEnv); err != nil {
		return ctrl.Result{}, err
	}

	// 6. Set Ready condition
	if !isConditionTrueAndUpToDate(hubEnv.Status.Conditions, "Ready", hubEnv.Generation) {
		logger.Info("All phases complete, setting Ready condition")

		meta.SetStatusCondition(&hubEnv.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			Reason:             "AllPhasesComplete",
			Message:            "Hub environment is ready",
			ObservedGeneration: hubEnv.Generation,
		})

		if err := r.Status().Update(ctx, hubEnv); err != nil {
			return ctrl.Result{}, err
		}

		logger.Info("HubEnvironment reconciliation complete")
	}

	// Certificate rotation is managed by cert-manager per ADR-035. Workloads restarted via Stakater Reloader annotations.
	if isConditionTrueAndUpToDate(hubEnv.Status.Conditions, "Ready", hubEnv.Generation) {
		// Requirement 23.15-23.16: Handle password rotation
		if err := r.handlePasswordRotation(ctx, hubEnv); err != nil {
			logger.Error(err, "Failed to handle password rotation")
			// Don't fail reconciliation, just log and requeue
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
	}

	return ctrl.Result{}, nil
}

// isCNPGReady reports whether the BOOTSTRAP cluster is ready.
//
// Spec.Database.ClusterRef, not every cluster the box runs: this gate exists so the
// operator does not act before the database it reads itself is up. An application's own
// cluster is a prerequisite for that application, and RoleManager already fails on it
// per cluster rather than holding up the others.
//
// Requirement 9.11: Implement dependency readiness checks
func (r *HubEnvironmentReconciler) isCNPGReady(ctx context.Context, hubEnv *opsv1alpha1.HubEnvironment) (bool, error) {
	cluster := &cnpgv1.Cluster{}
	if err := r.Get(ctx, client.ObjectKey{
		Name:      hubEnv.Spec.Database.ClusterRef,
		Namespace: hubEnv.Spec.Database.Namespace,
	}, cluster); err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}

	// Check if cluster is ready
	for _, condition := range cluster.Status.Conditions {
		if condition.Type == "Ready" && condition.Status == metav1.ConditionTrue {
			return true, nil
		}
	}

	return false, nil
}

// isInfisicalReady checks if Infisical Deployment has at least one ready replica
// Requirement 9.11: Implement dependency readiness checks
func (r *HubEnvironmentReconciler) isInfisicalReady(ctx context.Context, hubEnv *opsv1alpha1.HubEnvironment) (bool, error) {
	deployment := &appsv1.Deployment{}
	if err := r.Get(ctx, client.ObjectKey{
		Name:      infisical.InfisicalServiceName,
		Namespace: infisical.InfisicalServiceNamespace,
	}, deployment); err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return deployment.Status.ReadyReplicas > 0, nil
}

// uploadSecretsToInfisical uploads all secrets to Infisical
// handlePasswordRotation detects password changes and executes ALTER ROLE
// Requirement 23.15-23.16: Implement password rotation handling
func (r *HubEnvironmentReconciler) handlePasswordRotation(ctx context.Context, hubEnv *opsv1alpha1.HubEnvironment) error {
	logger := log.FromContext(ctx)
	namespace := hubEnv.Spec.Database.Namespace

	// Check all database role secrets for password changes
	for _, roleSpec := range hubEnv.Spec.Database.Roles {
		secretName := roleSpec.Name + "-db-credentials"

		// Read the secret
		// Task 13: Use UncachedClient - credential secret, must not have data stripped
		secret := &corev1.Secret{}
		if err := r.UncachedClient.Get(ctx, client.ObjectKey{
			Name:      secretName,
			Namespace: namespace,
		}, secret); err != nil {
			if errors.IsNotFound(err) {
				// Secret doesn't exist yet, skip
				continue
			}
			return err
		}

		// Check if secret has the db-credentials label (ESO-managed)
		if labels := secret.GetLabels(); labels == nil || labels["ops.nutgraf.in/db-credentials"] != "true" {
			// Not an ESO-managed secret, skip
			continue
		}

		// Extract username and password
		username, ok := secret.Data["username"]
		if !ok {
			logger.Info("Secret missing username field", "secret", secretName)
			continue
		}

		password, ok := secret.Data["password"]
		if !ok {
			logger.Info("Secret missing password field", "secret", secretName)
			continue
		}

		// Requirement 23.15: Execute ALTER ROLE to update PostgreSQL password
		// The cluster this ROLE lives on, which after ADR-112 is not necessarily the
		// box's shared one. Rotating a password against the wrong cluster either fails
		// on a role that is not there or, if a role of the same name happens to exist,
		// changes a credential nothing is using while the real one goes stale.
		roleManager, err := database.NewRoleManager(ctx, r.UncachedClient, database.Endpoint{
			Cluster:   hubEnv.Spec.ClusterFor(roleSpec),
			Namespace: namespace,
		})
		if err != nil {
			logger.Error(err, "Failed to create role manager for password rotation",
				"cluster", hubEnv.Spec.ClusterFor(roleSpec), "role", roleSpec.Name)
			continue
		}

		// Check if password needs update (drift detection)
		needsUpdate, err := roleManager.PasswordNeedsUpdate(ctx, string(username), string(password))
		if err != nil {
			roleManager.Close()
			logger.Error(err, "Failed to check password drift", "role", string(username))
			continue
		}

		if !needsUpdate {
			roleManager.Close()
			// Password hasn't changed, skip
			continue
		}

		logger.Info("Password rotation detected, updating role", "role", string(username))

		// Execute ALTER ROLE
		if err := roleManager.UpdateRolePassword(ctx, string(username), string(password)); err != nil {
			roleManager.Close()
			logger.Error(err, "Failed to update role password", "role", string(username))
			continue
		}

		roleManager.Close()
		logger.Info("Updated role password in PostgreSQL", "role", string(username))

		// Requirement 23.16: Restart consuming Deployment/StatefulSet
		// Extract service name from role name (e.g., "infisical" from "infisical-db-credentials")
		serviceName := roleSpec.Name

		// Map role names to service deployments/statefulsets
		serviceMap := map[string]struct {
			kind      string
			name      string
			namespace string
		}{
			"infisical":  {"Deployment", infisical.InfisicalServiceName, infisical.InfisicalServiceNamespace},
			"redis":      {"StatefulSet", "redis", namespace},
			"zitadel":    {"Deployment", "zitadel", infisical.NamespaceIdentity},
			"mcp_server": {"Deployment", "mcp-server", infisical.NamespaceOps},
		}

		if svc, ok := serviceMap[serviceName]; ok {
			if svc.kind == "Deployment" {
				if err := r.restartDeployment(ctx, svc.name, svc.namespace); err != nil {
					logger.Error(err, "Failed to restart deployment after password rotation", "name", svc.name)
				} else {
					logger.Info("Restarted deployment after password rotation", "name", svc.name)
				}
			} else if svc.kind == "StatefulSet" {
				if err := r.restartStatefulSet(ctx, svc.name, svc.namespace); err != nil {
					logger.Error(err, "Failed to restart statefulset after password rotation", "name", svc.name)
				} else {
					logger.Info("Restarted statefulset after password rotation", "name", svc.name)
				}
			}
		}
	}

	return nil
}

// restartDeployment patches a Deployment with restartedAt annotation
// Requirement 23.5: Implement restartDeployment() helper
func (r *HubEnvironmentReconciler) restartDeployment(ctx context.Context, name, namespace string) error {
	deployment := &appsv1.Deployment{}
	if err := r.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, deployment); err != nil {
		if errors.IsNotFound(err) {
			// Deployment doesn't exist yet, skip restart
			return nil
		}
		return err
	}

	// Requirement 23.4: Use kubectl.kubernetes.io/restartedAt annotation pattern
	if deployment.Spec.Template.Annotations == nil {
		deployment.Spec.Template.Annotations = make(map[string]string)
	}
	deployment.Spec.Template.Annotations["kubectl.kubernetes.io/restartedAt"] = time.Now().Format(time.RFC3339)

	return r.Update(ctx, deployment)
}

// restartStatefulSet patches a StatefulSet with restartedAt annotation
// Requirement 23.6: Implement restartStatefulSet() helper
func (r *HubEnvironmentReconciler) restartStatefulSet(ctx context.Context, name, namespace string) error {
	statefulSet := &appsv1.StatefulSet{}
	if err := r.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, statefulSet); err != nil {
		if errors.IsNotFound(err) {
			// StatefulSet doesn't exist yet, skip restart
			return nil
		}
		return err
	}

	// Requirement 23.4: Use kubectl.kubernetes.io/restartedAt annotation pattern
	if statefulSet.Spec.Template.Annotations == nil {
		statefulSet.Spec.Template.Annotations = make(map[string]string)
	}
	statefulSet.Spec.Template.Annotations["kubectl.kubernetes.io/restartedAt"] = time.Now().Format(time.RFC3339)

	return r.Update(ctx, statefulSet)
}

// escrowKubeconfig copies this cluster's admin kubeconfig into the escrow.
//
// The source is the Secret CAPI maintains -- <cluster>-kubeconfig in platform-capi,
// with the kubeconfig in .data.value. CAPI owns it and rotates it; this only ever
// reads.
//
// It is the credential that reaches the API server when the identity provider
// cannot be used (ADR-076). Day-0 wrote it to a runner that no longer exists, so
// the copy inside the cluster is the only one -- which is no use for reaching a
// cluster that is broken.
// reconcileEscrow makes the escrow hold what this box cannot regenerate, and
// does it on every pass rather than once.
//
// Two artifacts, for two different reasons:
//
//   - The Infisical master keys. Escrowed only at the moment they were generated,
//     so a box whose escrow write failed, whose credentials arrived afterwards, or
//     whose keys were written under a different name, had no copy and no code path
//     that would ever make one. The keys still exist in the cluster, so the repair
//     is to read them back and escrow those rather than declare the box unfixable.
//
//   - The admin kubeconfig. CAPI issues it on kubeadm's one-year client
//     certificate, so the escrowed copy expires; re-reading each pass replaces a
//     stale copy without anyone remembering to.
//
// Writing an existing key back is not a rotation: BackupMasterKeys stores what is
// already in use, so a box that is working keeps working and a box that was one
// disk failure from unrecoverable stops being so.
func (r *HubEnvironmentReconciler) reconcileEscrow(ctx context.Context, store escrow.EscrowClient, escrowID escrow.Identity) error {
	logger := log.FromContext(ctx)

	// Both names, because this function needs both: the master keys are the BOX's
	// and the kubeconfig Secret CAPI writes is the CLUSTER's.
	boxID := escrowID.BoxID
	clusterID := escrowID.ClusterName

	backup, err := store.RestoreMasterKeys(ctx, escrowID)
	if err != nil {
		return fmt.Errorf("read the escrow for %s: %w", boxID, err)
	}

	if backup == nil {
		// No copy. The cluster's own are authoritative -- Infisical is decrypting
		// with them right now -- so they are what gets escrowed.
		var sec corev1.Secret
		key := types.NamespacedName{Name: "infisical-secrets", Namespace: infisical.InfisicalServiceNamespace}
		if err := r.UncachedClient.Get(ctx, key, &sec); err != nil {
			// Before Day-0 has written it there is nothing to escrow and nothing
			// wrong: Phase 1 below generates the keys and escrows them itself.
			if errors.IsNotFound(err) {
				logger.Info("the escrow holds no master keys and the cluster has none "+
					"yet; Phase 1 will generate and escrow them", "clusterID", clusterID)
				return nil
			}
			return fmt.Errorf("read %s: %w", key, err)
		}

		encryptionKey := strings.TrimSpace(string(sec.Data["ENCRYPTION_KEY"]))
		authSecret := strings.TrimSpace(string(sec.Data["AUTH_SECRET"]))

		// Validated before storing. An escrow holding a malformed key is worse than
		// one holding nothing: it reports a backup exists and fails on restore, at
		// the one moment there is no other copy left to try.
		if err := secrets.ValidateEncryptionKey(encryptionKey); err != nil {
			return fmt.Errorf("%s holds an ENCRYPTION_KEY that cannot be escrowed: %w", key, err)
		}
		if err := secrets.ValidateAuthSecret(authSecret); err != nil {
			return fmt.Errorf("%s holds an AUTH_SECRET that cannot be escrowed: %w", key, err)
		}

		if err := store.BackupMasterKeys(ctx, escrowID, map[string]interface{}{
			"encryptionKey": encryptionKey,
			"authSecret":    authSecret,
			"createdAt":     time.Now().UTC(),
			"clusterId":     boxID,
			"version":       "1",
		}); err != nil {
			return fmt.Errorf("escrow the master keys for %s: %w", boxID, err)
		}
		logger.Info("master keys escrowed from the cluster's own copy", "clusterID", clusterID)
	}

	// The admin kubeconfig, once CAPI has issued one.
	//
	// Absent is not a failure here and must not be treated as one: on a fresh box
	// CAPI has not written <cluster>-kubeconfig until the control plane is up, and
	// failing the reconcile for it would block Phase 1 -- which generates the
	// secrets that box needs to come up at all. A deadlock in the name of a backup.
	//
	// It is safe to pass over precisely because this runs every reconcile: the
	// secret appears and the next pass escrows it. What is NOT passed over is a
	// secret that exists and cannot be escrowed -- an unreadable one, an empty
	// one, a write the escrow rejected -- which is a real failure of the guarantee
	// and is returned.
	var capiSecret corev1.Secret
	capiKey := types.NamespacedName{Name: clusterID + "-kubeconfig", Namespace: "platform-capi"}
	if err := r.UncachedClient.Get(ctx, capiKey, &capiSecret); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("CAPI has not issued the admin kubeconfig yet; it will be "+
				"escrowed on a later pass", "secret", capiKey.String())
			return nil
		}
		return fmt.Errorf("read %s: %w", capiKey, err)
	}

	if err := r.ensureBackupEncryptionKey(ctx, store, escrowID); err != nil {
		return fmt.Errorf("establish the management-state backup encryption key for %s: %w", boxID, err)
	}

	if err := r.escrowClusterPKI(ctx, store, escrowID); err != nil {
		return fmt.Errorf("escrow the certificate authorities for %s: %w", clusterID, err)
	}

	if err := r.labelBootstrapData(ctx, clusterID); err != nil {
		return fmt.Errorf("mark %s's bootstrap data for the management-state backup: %w",
			clusterID, err)
	}

	if err := r.escrowKubeconfig(ctx, store, escrowID); err != nil {
		return fmt.Errorf("escrow the admin kubeconfig for %s: %w", clusterID, err)
	}
	logger.Info("admin kubeconfig escrowed", "clusterID", clusterID)

	// EVERY CLUSTER THIS BOX MANAGES, not only the management cluster.
	//
	// ADR-076's recovery contract scopes `admin-kubeconfig` PER CLUSTER and requires
	// it, because a catastrophic failure of this cluster or of the automation must
	// not leave a workload cluster with no way in. Until 2026-10-04 only the line
	// above ran, so every workload cluster had no break-glass credential anywhere --
	// and nothing reported it, because the escrow was only ever asked about this one.
	//
	// Done here rather than in the SpokePool reconciler because every cluster's CAPI
	// kubeconfig Secret lives in this cluster's platform-capi namespace, and so does
	// the escrow client. Splitting it would put half the recovery contract in a
	// reconciler that would have to be given both.
	if err := r.escrowWorkloadKubeconfigs(ctx, logger, store, escrowID); err != nil {
		return fmt.Errorf("escrow workload cluster kubeconfigs: %w", err)
	}
	return nil
}

// escrowWorkloadKubeconfigs escrows the admin kubeconfig of every cluster this box
// manages except the management cluster, which the caller has already done.
//
// A cluster whose kubeconfig Secret does not exist yet is skipped rather than failed:
// CAPI writes it once the control plane is reachable, so a cluster mid-provision has
// none and the next reconcile catches it. A cluster whose escrow write FAILS is an
// error, because that is the state the contract forbids.
func (r *HubEnvironmentReconciler) escrowWorkloadKubeconfigs(
	ctx context.Context, logger logr.Logger, store escrow.EscrowClient, hubID escrow.Identity,
) error {
	// Unstructured, as this operator reads every other CAPI resource. A typed client
	// would add the cluster-api module to this binary for one field read.
	clusters := &unstructured.UnstructuredList{}
	clusters.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "cluster.x-k8s.io", Version: "v1beta1", Kind: "ClusterList",
	})
	if err := r.List(ctx, clusters, client.InNamespace("platform-capi")); err != nil {
		return fmt.Errorf("list clusters: %w", err)
	}

	var failed []string
	for i := range clusters.Items {
		name := clusters.Items[i].GetName()
		if name == hubID.ClusterName {
			continue
		}
		// Keyed by the SPOKE, under the same box: a spoke's break-glass credential
		// must not move when the hub managing it is replaced (ADR-111 ss1).
		spokeID := escrow.ForCluster(hubID.BoxID, name)
		if err := r.labelBootstrapData(ctx, name); err != nil {
			logger.Error(err, "could not mark a workload cluster's bootstrap data for backup",
				"cluster", name)
			failed = append(failed, name)
			continue
		}
		if err := r.escrowClusterPKI(ctx, store, spokeID); err != nil {
			logger.Error(err, "could not escrow a workload cluster's certificate authorities",
				"cluster", name)
			failed = append(failed, name)
			continue
		}
		if err := r.escrowKubeconfig(ctx, store, spokeID); err != nil {
			// Not yet written by CAPI is not a failure; anything else is.
			if apierrors.IsNotFound(err) || strings.Contains(err.Error(), "not found") {
				logger.Info("workload cluster has no kubeconfig secret yet; will escrow on a later pass",
					"cluster", name)
				continue
			}
			logger.Error(err, "could not escrow a workload cluster's admin kubeconfig",
				"cluster", name)
			failed = append(failed, name)
			continue
		}
		logger.Info("admin kubeconfig escrowed", "clusterID", name)
	}

	if len(failed) > 0 {
		return fmt.Errorf("clusters left with no break-glass credential in the escrow, "+
			"which ADR-076's recovery contract requires: %v", failed)
	}
	return nil
}

func (r *HubEnvironmentReconciler) escrowKubeconfig(ctx context.Context, store escrow.EscrowClient, escrowID escrow.Identity) error {
	clusterID := escrowID.ClusterName
	// UncachedClient, because the cache strips Secret payloads.
	//
	// cmd/main.go installs a cache transform that removes .data from every Secret
	// except those labelled managed-by=zero-ops-hub-cli, to cut memory ~90%, and
	// its own comment records the consequence: "Operational secrets also use
	// UncachedClient". This read did not, so it found the CAPI kubeconfig secret,
	// saw an empty .data, and reported "carries no kubeconfig under .data.value"
	// about a secret holding 7440 bytes of exactly that.
	//
	// It was survivable while escrowing the kubeconfig was best-effort. It is not
	// now: the reconcile returns the error, so the admin kubeconfig is never
	// escrowed and HubEnvironment never advances past it.
	var secret corev1.Secret
	key := types.NamespacedName{Name: clusterID + "-kubeconfig", Namespace: "platform-capi"}
	if err := r.UncachedClient.Get(ctx, key, &secret); err != nil {
		return fmt.Errorf("read %s: %w", key, err)
	}

	// CAPI stores it under "value". An empty one is not a kubeconfig, and
	// escrowing it would overwrite a good copy with nothing.
	payload, ok := secret.Data["value"]
	if !ok || len(payload) == 0 {
		return fmt.Errorf("%s carries no kubeconfig under .data.value", key)
	}

	return store.BackupArtifact(ctx, escrowID, escrow.ArtifactKubeconfig, string(payload))
}

// SetupWithManager sets up the controller with the Manager.
// Requirement 12: Configure controller watches for all external dependencies and secrets
func (r *HubEnvironmentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		// Requirement 12.2: Primary resource
		For(&opsv1alpha1.HubEnvironment{}).
		// Requirement 12.3: Owned secrets (operator-created)
		Owns(&corev1.Secret{}).
		// Requirement 12.4: Watch CNPG Cluster for readiness
		Watches(
			&cnpgv1.Cluster{},
			handler.EnqueueRequestsFromMapFunc(r.findHubEnvironmentForCNPG),
			builder.WithPredicates(predicate.ResourceVersionChangedPredicate{}),
		).
		// Requirement 12.6: Watch secrets with label ops.nutgraf.in/db-credentials=true for password rotation
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.findHubEnvironmentForSecret),
			builder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool {
				secret := obj.(*corev1.Secret)
				if labels := secret.GetLabels(); labels != nil {
					return labels["ops.nutgraf.in/db-credentials"] == "true"
				}
				return false
			})),
		).
		// Requirement 12.8: Watch Infisical Deployment
		Watches(
			&appsv1.Deployment{},
			handler.EnqueueRequestsFromMapFunc(r.findHubEnvironmentForDeployment),
			builder.WithPredicates(
				predicate.ResourceVersionChangedPredicate{},
				predicate.NewPredicateFuncs(func(obj client.Object) bool {
					return obj.GetName() == infisical.InfisicalServiceName && obj.GetNamespace() == infisical.InfisicalServiceNamespace
				}),
			),
		).
		Complete(r)
}

// findHubEnvironmentForCNPG maps CNPG Cluster to HubEnvironment CR
// Requirement 12.10: Implement mapper function for CNPG
func (r *HubEnvironmentReconciler) findHubEnvironmentForCNPG(ctx context.Context, obj client.Object) []reconcile.Request {
	cluster := obj.(*cnpgv1.Cluster)

	// List all HubEnvironment CRs
	hubEnvList := &opsv1alpha1.HubEnvironmentList{}
	if err := r.List(ctx, hubEnvList); err != nil {
		return []reconcile.Request{}
	}

	// Find the HubEnvironment that references this cluster.
	//
	// EVERY cluster it declares, not just the bootstrap one. A box runs one cluster per
	// platform application (ADR-112), and each one becoming Ready is what unblocks that
	// application's roles. Matching a single name meant an event from any other cluster
	// mapped to no request at all, so its roles waited for an unrelated reconcile.
	var requests []reconcile.Request
	for _, hubEnv := range hubEnvList.Items {
		if hubEnv.Spec.Database.Namespace != cluster.Namespace {
			continue
		}
		for _, declared := range hubEnv.Spec.DatabaseClusters() {
			if declared != cluster.Name {
				continue
			}
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      hubEnv.Name,
					Namespace: hubEnv.Namespace,
				},
			})
			break
		}
	}

	return requests
}

// findHubEnvironmentForSecret maps Secret to HubEnvironment CR
// Requirement 12.11: Implement mapper function for Secret
func (r *HubEnvironmentReconciler) findHubEnvironmentForSecret(ctx context.Context, obj client.Object) []reconcile.Request {
	secret := obj.(*corev1.Secret)

	// List all HubEnvironment CRs
	hubEnvList := &opsv1alpha1.HubEnvironmentList{}
	if err := r.List(ctx, hubEnvList); err != nil {
		return []reconcile.Request{}
	}

	// Find HubEnvironment in the same namespace as the secret
	var requests []reconcile.Request
	for _, hubEnv := range hubEnvList.Items {
		if hubEnv.Spec.Database.Namespace == secret.Namespace {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      hubEnv.Name,
					Namespace: hubEnv.Namespace,
				},
			})
		}
	}

	return requests
}

// findHubEnvironmentForDeployment maps Deployment to HubEnvironment CR
// Requirement 12.12: Implement mapper function for Deployment
func (r *HubEnvironmentReconciler) findHubEnvironmentForDeployment(ctx context.Context, obj client.Object) []reconcile.Request {
	// List all HubEnvironment CRs
	hubEnvList := &opsv1alpha1.HubEnvironmentList{}
	if err := r.List(ctx, hubEnvList); err != nil {
		return []reconcile.Request{}
	}

	// Trigger reconciliation for all HubEnvironments
	// The reconciler will check if the deployment is ready
	var requests []reconcile.Request
	for _, hubEnv := range hubEnvList.Items {
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      hubEnv.Name,
				Namespace: hubEnv.Namespace,
			},
		})
	}

	return requests
}

// findHubEnvironmentForStatefulSet maps StatefulSet to HubEnvironment CR
// Requirement 12.13: Implement mapper function for StatefulSet
func (r *HubEnvironmentReconciler) findHubEnvironmentForStatefulSet(ctx context.Context, obj client.Object) []reconcile.Request {
	// List all HubEnvironment CRs
	hubEnvList := &opsv1alpha1.HubEnvironmentList{}
	if err := r.List(ctx, hubEnvList); err != nil {
		return []reconcile.Request{}
	}

	// Trigger reconciliation for all HubEnvironments
	// The reconciler will check if the statefulset is ready
	var requests []reconcile.Request
	for _, hubEnv := range hubEnvList.Items {
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      hubEnv.Name,
				Namespace: hubEnv.Namespace,
			},
		})
	}

	return requests
}

// removeFinalizer removes the encryption key protection finalizer from a secret
// REQ-10: Implement finalizer controller for graceful teardown
// REQ-13: Graceful teardown support - remove finalizers when HubEnvironment is deleted
func (r *HubEnvironmentReconciler) removeFinalizer(ctx context.Context, secretName, namespace string) error {
	secret := &corev1.Secret{}
	err := r.Get(ctx, client.ObjectKey{Name: secretName, Namespace: namespace}, secret)
	if err != nil {
		if errors.IsNotFound(err) {
			// Secret doesn't exist, nothing to do
			return nil
		}
		return fmt.Errorf("failed to get secret %s/%s: %w", namespace, secretName, err)
	}

	// Check if finalizer exists
	finalizerExists := false
	for _, f := range secret.Finalizers {
		if f == secrets.EncryptionKeyProtectionFinalizer {
			finalizerExists = true
			break
		}
	}

	if !finalizerExists {
		// Finalizer doesn't exist, nothing to do
		return nil
	}

	// Remove the finalizer
	var newFinalizers []string
	for _, f := range secret.Finalizers {
		if f != secrets.EncryptionKeyProtectionFinalizer {
			newFinalizers = append(newFinalizers, f)
		}
	}
	secret.Finalizers = newFinalizers

	// Update the secret
	if err := r.Update(ctx, secret); err != nil {
		return fmt.Errorf("failed to remove finalizer from secret %s/%s: %w", namespace, secretName, err)
	}

	log.FromContext(ctx).Info("Removed finalizer from secret", "secret", secretName, "namespace", namespace)
	return nil
}

// ensurePKITemplates enforces the existence of required PKI templates in Infisical.
func (r *HubEnvironmentReconciler) ensurePKITemplates(ctx context.Context, hubEnv *opsv1alpha1.HubEnvironment) error {
	logger := log.FromContext(ctx)

	infisicalClient, err := infisicalclient.NewInfisicalClient(ctx, r.UncachedClient, "")
	if err != nil {
		return fmt.Errorf("failed to create Infisical client for PKI templates: %w", err)
	}

	projectSlug := "hub-platform"
	if hubEnv.Spec.Secrets.Infisical.ProjectSlug != "" {
		projectSlug = hubEnv.Spec.Secrets.Infisical.ProjectSlug
	}

	// Single authoritative definition, shared with the CLI's PKI_READY phase
	// (ADR-042 makes that phase CLI-owned; this reconcile is the self-healing half).
	// This list used to be a second hardcoded copy under a comment claiming it
	// matched the CLI's. It did not: it omitted signing-keys, so a rebuilt Infisical
	// could not issue argocd-agent-jwt and argocd-agent-principal never started,
	// while it created argocd-principals and argocd-agents, which no issuer,
	// Certificate or ADR references. See internal/pki.
	// Before the templates, because every template hangs off this CA and a CA
	// still awaiting its own certificate accepts template creation while
	// refusing every issuance request with "CA is not active".
	if err := infisicalClient.EnsureCAActive(ctx, projectSlug, "fleet-intermediate-ca"); err != nil {
		return fmt.Errorf("ensure CA active failed: %w", err)
	}

	for _, p := range pki.RequiredProfiles {
		if err := infisicalClient.EnsurePKITemplate(ctx, projectSlug, "fleet-intermediate-ca", p.Slug, p.TTLDays); err != nil {
			return fmt.Errorf("ensure PKI template %q failed: %w", p.Slug, err)
		}
	}

	logger.Info("PKI templates ensured successfully")
	return nil
}

// ensureInfisicalDBRootCert keeps infisical-secrets.DB_ROOT_CERT in step with the
// live CNPG cluster CA.
//
// The encoding is not obvious and getting it wrong fails silently in a way that
// looks like a wrong certificate rather than a wrong format: Infisical expects the
// env var to be a base64-encoded PEM, and Kubernetes base64-encodes Secret data on
// the wire, so the stored value is the PEM encoded twice. Copying the CA Secret's
// data field across verbatim yields a value whose bytes match the CA exactly and
// still does not work.
func (r *HubEnvironmentReconciler) ensureInfisicalDBRootCert(ctx context.Context) error {
	logger := log.FromContext(ctx)

	caSecret := &corev1.Secret{}
	if err := r.UncachedClient.Get(ctx, client.ObjectKey{
		Name: "infisical-db-ca", Namespace: "platform-data",
	}, caSecret); err != nil {
		// Before CNPG exists there is nothing to mirror; that is not an error.
		return client.IgnoreNotFound(err)
	}
	caPEM, ok := caSecret.Data["ca.crt"]
	if !ok || len(caPEM) == 0 {
		return nil
	}
	want := []byte(base64.StdEncoding.EncodeToString(caPEM))

	target := &corev1.Secret{}
	if err := r.UncachedClient.Get(ctx, client.ObjectKey{
		Name: "infisical-secrets", Namespace: infisical.InfisicalServiceNamespace,
	}, target); err != nil {
		return client.IgnoreNotFound(err)
	}
	if bytes.Equal(target.Data["DB_ROOT_CERT"], want) {
		return nil
	}

	if target.Data == nil {
		target.Data = map[string][]byte{}
	}
	target.Data["DB_ROOT_CERT"] = want
	if err := r.UncachedClient.Update(ctx, target); err != nil {
		return err
	}
	logger.Info("Refreshed Infisical DB_ROOT_CERT from the live CNPG CA")
	return nil
}

// escrowClusterPKI copies a cluster's Cluster API trust roots into the escrow.
//
// WHY THIS IS THE ONE WRITE THAT REFUSES TO UPDATE. Every other escrow write takes
// the live cluster as the source of truth and rewrites its copy, which is right for
// a credential that rotates. A certificate authority does not rotate: it is created
// once with the cluster and lasts its life, so a LIVE value that differs from the
// escrowed one does not mean the escrow is stale. It means something generated a new
// trust root -- a reconstructed hub whose Cluster API found no PKI and made its own
// is exactly how that happens -- and rewriting would replace the only copy of the
// authority the running nodes actually trust with one that cannot talk to them.
//
// So the first write wins and a divergence is an error. The cost of being wrong in
// the other direction is unbounded: without the original `ca` key no hub can ever
// issue a node certificate for that cluster again, and no amount of later
// reconciliation recovers it (ADR-111 ss2, Class E).
//
// A cluster whose Secret does not exist yet is skipped, as for the kubeconfig: CAPI
// writes these when it creates the control plane, so a cluster mid-provision has
// none and the next pass catches it.
func (r *HubEnvironmentReconciler) escrowClusterPKI(
	ctx context.Context, store escrow.EscrowClient, id escrow.Identity,
) error {
	logger := log.FromContext(ctx)

	for _, purpose := range escrow.SpokePKIPurposes {
		name := types.NamespacedName{
			Name:      id.ClusterName + "-" + purpose,
			Namespace: "platform-capi",
		}
		var secret corev1.Secret
		if err := r.UncachedClient.Get(ctx, name, &secret); err != nil {
			if apierrors.IsNotFound(err) {
				logger.Info("no certificate authority secret yet; will escrow on a later pass",
					"cluster", id.ClusterName, "purpose", purpose)
				continue
			}
			return fmt.Errorf("read %s: %w", name, err)
		}

		// The whole data map, not one key: `ca`, `etcd` and `proxy` carry tls.crt
		// and tls.key, and `sa` carries a key pair under the same two names. Taking
		// the map whole means a purpose that carries something else is still
		// escrowed faithfully rather than silently truncated.
		if len(secret.Data) == 0 {
			return fmt.Errorf("%s carries no data, so there is nothing to escrow and "+
				"escrowing nothing would report this cluster as covered", name)
		}
		payload, err := json.Marshal(secret.Data)
		if err != nil {
			return fmt.Errorf("serialise %s: %w", name, err)
		}

		// Keep this Secret out of the management-state backup, every pass.
		//
		// WHY A LABEL AND WHY HERE. The PKI is Class E and must have exactly ONE
		// recovery authority (ADR-111 §2), so it has to be absent from the Class M
		// archive -- and it cannot be filtered out by anything else. A bootstrap-data
		// Secret and a certificate authority carry the SAME type
		// (cluster.x-k8s.io/secret) and the SAME cluster-name label, so no selector
		// over type or those labels separates them. Cluster API's own discriminator is
		// the name suffix (`-ca`, `-etcd`, `-sa`, `-proxy`; util/secret's
		// HasPurposeSuffix), and Velero cannot select on a name suffix.
		//
		// So the per-object exclusion label is the mechanism, and this loop is where
		// it is maintained: it already reads every one of these Secrets on every
		// reconcile, so a newly created authority is marked on the next pass and a
		// label someone removed is put back.
		if err := r.excludeFromManagementBackup(ctx, &secret); err != nil {
			return fmt.Errorf("mark %s out of the management-state backup: %w", name, err)
		}

		artifact := escrow.SpokePKIArtifact(purpose)
		existing, err := store.RestoreArtifact(ctx, id, artifact)
		if err != nil {
			// Unreadable is not absent. Treating it as absent would write over a
			// copy that may be the only one.
			return fmt.Errorf("read the escrowed %s for %s: %w", artifact, id.ClusterName, err)
		}
		switch {
		case existing == string(payload):
			continue
		case existing != "":
			return fmt.Errorf("the escrowed %s for %s does NOT match the live one, and a "+
				"certificate authority does not rotate: something has generated a new trust "+
				"root for this cluster. Refusing to overwrite the escrowed copy, because the "+
				"nodes trust that one and nothing can re-create it", artifact, id.ClusterName)
		}

		if err := store.BackupArtifact(ctx, id, artifact, string(payload)); err != nil {
			return fmt.Errorf("escrow %s for %s: %w", artifact, id.ClusterName, err)
		}
		logger.Info("certificate authority escrowed", "cluster", id.ClusterName, "purpose", purpose)
	}
	return nil
}

// ensureBackupEncryptionKey establishes the key the management-state backups are
// encrypted with, once per BOX, and never again.
//
// The key is the tenant's half of SSE-C on the object store holding Class M
// (ADR-111 ss10). It is escrowed rather than merely held in the secret store because
// it has to be readable when neither the hub that wrote those backups nor the
// Infisical inside it is running -- which is every circumstance in which the backups
// are wanted.
//
// GENERATE-IF-ABSENT IS SAFE HERE ONLY BECAUSE THE ARTEFACT IS BOX-SCOPED. A
// reconstructed hub reads the same box path its predecessor wrote, finds the key and
// leaves it alone, so the only way to reach the empty case is a box that has never
// had one -- a box with no backups to make unreadable. Were this cluster-scoped, the
// first successor would find nothing, mint a second key, and every backup taken by
// its predecessor would become undecryptable with nothing reporting it.
//
// An unreadable escrow is not an absent key, and the difference is the whole point:
// treating a failed read as absent is how a second key gets minted over a first.
func (r *HubEnvironmentReconciler) ensureBackupEncryptionKey(
	ctx context.Context, store escrow.EscrowClient, id escrow.Identity,
) error {
	logger := log.FromContext(ctx)

	existing, err := store.RestoreArtifact(ctx, id, escrow.ArtifactBackupSSECKey)
	if err != nil {
		return fmt.Errorf("read the escrowed backup encryption key: %w", err)
	}
	if existing != "" {
		// Present. Not re-derived, not rotated, not compared against anything in
		// the cluster: the escrow is the authority for this one, because the
		// backups it protects outlive every cluster that held a copy.
		return nil
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Errorf("generate the backup encryption key: %w", err)
	}
	key := hex.EncodeToString(raw)

	if err := store.BackupArtifact(ctx, id, escrow.ArtifactBackupSSECKey, key); err != nil {
		return fmt.Errorf("the backup encryption key was generated but could not be escrowed, "+
			"so it is discarded rather than used: %w", err)
	}
	logger.Info("management-state backup encryption key generated and escrowed", "box", id.BoxID)
	return nil
}

// veleroExcludeLabel is Velero's documented per-object opt-out.
const veleroExcludeLabel = "velero.io/exclude-from-backup"

// excludeFromManagementBackup marks one Secret as not belonging in Class M.
//
// Idempotent, and a patch rather than an update so nothing else on the object is
// touched: these are Cluster API's Secrets, and this adds one label to them.
//
// The residual window is worth naming rather than hiding. A certificate authority
// created between one reconcile and the next could be captured by a backup taken in
// that gap. It is harmless rather than merely unlikely: `kakt spoke restore-pki` is
// the only writer of PKI, it reads from the escrow alone, and Velero does not replace
// an existing resource -- so a stray copy in an archive is never promoted into a
// second authority. The label removes it from the archive; the single-writer rule is
// what makes the gap safe.
func (r *HubEnvironmentReconciler) excludeFromManagementBackup(
	ctx context.Context, secret *corev1.Secret,
) error {
	if secret.Labels[veleroExcludeLabel] == "true" {
		return nil
	}
	patch := []byte(`{"metadata":{"labels":{"` + veleroExcludeLabel + `":"true"}}}`)
	if err := r.Client.Patch(ctx, secret, client.RawPatch(types.MergePatchType, patch)); err != nil {
		return fmt.Errorf("label %s/%s: %w", secret.Namespace, secret.Name, err)
	}
	log.FromContext(ctx).Info("certificate authority marked out of the management-state backup",
		"secret", secret.Name)
	return nil
}

// classMBootstrapLabel marks the Secrets the management-state backup's credentials
// stream SELECTS. It is an inclusion label, not an exclusion one.
const classMBootstrapLabel = "recovery.platform.io/class-m"

// labelBootstrapData marks a cluster's bootstrap-data Secrets for inclusion in Class M.
//
// POSITIVE SELECTION, because exclusion alone was not safe. Excluding the certificate
// authorities by label leaves a window: an authority created between two reconciles can
// be captured by a backup taken in that gap, and a captured authority is a private
// trust root inside a Class M archive -- outside the Class E boundary it is supposed to
// live in. Worse, if it were later restored, VELERO would create it, and Velero is a
// writer. An earlier revision of this design argued the window was harmless because
// `kakt spoke restore-pki` reads only the escrow; that conclusion was wrong, because
// it reasoned about one writer and there are two.
//
// So the credentials stream selects on this label and takes nothing else. Any Secret
// that has not been identified here -- including an unlabelled authority -- is excluded
// by construction rather than by a rule that has to keep up with it. The exclusion
// label on the authorities stays as well: two independent reasons for the same object
// to be absent.
//
// WHICH SECRETS, from Cluster API's own contract rather than from a name convention:
// a Machine names its bootstrap data at `spec.bootstrap.dataSecretName`, and a nil
// value means the Machine is still Pending (api/v1beta1/machine_types.go at the
// deployed version). So a Machine without one is mid-provision and not a gap; a Machine
// WITH one whose Secret cannot be found or labelled is a real gap, and is returned as
// an error rather than counted as covered.
func (r *HubEnvironmentReconciler) labelBootstrapData(ctx context.Context, clusterName string) error {
	logger := log.FromContext(ctx)

	machines := &unstructured.UnstructuredList{}
	machines.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "cluster.x-k8s.io", Version: "v1beta1", Kind: "MachineList",
	})
	if err := r.List(ctx, machines, client.InNamespace("platform-capi"),
		client.MatchingLabels{"cluster.x-k8s.io/cluster-name": clusterName}); err != nil {
		return fmt.Errorf("list machines for %s: %w", clusterName, err)
	}

	var gaps []string
	pending, labelled := 0, 0

	for i := range machines.Items {
		m := machines.Items[i]
		name, found, err := unstructured.NestedString(m.Object, "spec", "bootstrap", "dataSecretName")
		if err != nil {
			gaps = append(gaps, fmt.Sprintf("%s: reading spec.bootstrap.dataSecretName: %v",
				m.GetName(), err))
			continue
		}
		if !found || name == "" {
			// Pending by Cluster API's own definition. Nothing to protect yet.
			pending++
			continue
		}

		key := types.NamespacedName{Name: name, Namespace: "platform-capi"}
		var secret corev1.Secret
		if err := r.UncachedClient.Get(ctx, key, &secret); err != nil {
			// A Machine naming bootstrap data that is not there is a COVERAGE GAP,
			// not a transient state: that Machine cannot be recovered, because
			// `IsBootstrapDataReady` gates adoption on this Secret existing.
			gaps = append(gaps, fmt.Sprintf("%s names bootstrap data %s, which cannot be read: %v",
				m.GetName(), name, err))
			continue
		}

		if secret.Labels[classMBootstrapLabel] == "bootstrap-data" {
			labelled++
			continue
		}
		patch := []byte(`{"metadata":{"labels":{"` + classMBootstrapLabel + `":"bootstrap-data"}}}`)
		if err := r.Client.Patch(ctx, &secret, client.RawPatch(types.MergePatchType, patch)); err != nil {
			gaps = append(gaps, fmt.Sprintf("%s's bootstrap data %s could not be marked: %v",
				m.GetName(), name, err))
			continue
		}
		labelled++
		logger.Info("bootstrap data marked for the management-state backup",
			"cluster", clusterName, "secret", name)
	}

	if len(gaps) > 0 {
		return fmt.Errorf("%d machine(s) of %s have bootstrap data that is NOT covered by the "+
			"management-state backup, and a backup of the rest would report success while "+
			"leaving them unrecoverable:\n  %s", len(gaps), clusterName, strings.Join(gaps, "\n  "))
	}
	logger.Info("bootstrap data coverage", "cluster", clusterName,
		"covered", labelled, "pending", pending)
	return nil
}
