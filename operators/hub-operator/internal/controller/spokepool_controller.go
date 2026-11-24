package controller

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/yaml"

	"github.com/kamarkat/kamarkat/internal/platform/encryption"
	"github.com/kamarkat/kamarkat/internal/platform/escrow"
	opsv1alpha1 "github.com/kamarkat/kamarkat/operators/hub-operator/api/v1alpha1"
	"github.com/kamarkat/kamarkat/operators/hub-operator/internal/secrets"
)

// SpokePoolReconciler reconciles SpokePool XRs to generate per-spoke secrets
type SpokePoolReconciler struct {
	client.Client
	// UncachedClient reads Secrets directly from the API server, bypassing the
	// cache transform that strips .data payloads (AC 19.3/19.4). Required for
	// any Secret whose data this controller consumes (CAPI kubeconfig, TLS
	// bootstrap cert material).
	UncachedClient client.Client
	Store          *secrets.Store
	EscrowClient   escrow.EscrowClient
}

//+kubebuilder:rbac:groups=nutgraf.in,resources=spokepools,verbs=get;list;watch;update
//+kubebuilder:rbac:groups=nutgraf.in,resources=spokepools/status,verbs=get;update;patch
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch
//+kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters,verbs=get;list;watch
//+kubebuilder:rbac:groups=ops.nutgraf.in,resources=hubenvironments,verbs=get;list;watch

// Reconcile converges a workload cluster's standing in the box's OpenBao
// (ADR-117 §4): its JWT auth mount and platform role, its crossplane-admin
// password, and the fleet credentials copied into its own platform scope.
func (r *SpokePoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// 1. Fetch SpokePool XR using dynamic client (unstructured)
	spokePool := &unstructured.Unstructured{}
	spokePool.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "nutgraf.in",
		Version: "v1alpha1",
		Kind:    "SpokePool",
	})

	if err := r.Get(ctx, req.NamespacedName, spokePool); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	spokeName := spokePool.GetName()
	logger.Info("Reconciling SpokePool", "spoke", spokeName)

	// hybrid provider cell: mint home-worker join tokens and expose the join payload
	// (ADR-046 §WS4). Gated on spec.provider == hybrid + home-worker-enabled annotation.
	// Runs BEFORE the Infisical gate because home-worker join is independent of Infisical
	// and must not be blocked by Infisical connectivity failures (ADR-046 §WS4).
	if err := r.reconcileHomeWorkerJoin(ctx, spokePool); err != nil {
		logger.Error(err, "Failed to reconcile home-worker join", "spoke", spokeName)
		// Non-fatal: retried on next reconcile; token rotation is time-driven.
	}

	// The spoke's OWN Infisical environment, not the hub's.
	//
	// Every credential below belongs to this cluster, and the spoke reads them
	// with its own slug: 03-platform-services-appset.yaml patches the spoke's
	// ClusterSecretStore with `index .metadata.labels "environment"`, so a hub
	// managing a stg spoke would write to `dev` and the spoke would look in `stg`.
	// Nothing reports that -- the ExternalSecret stays unsynced and the component
	// waiting on the credential fails for its own reasons somewhere else.
	//
	// The label is on the SpokePool claim the tenant wrote
	// (templates/workload-cluster/infrastructure/spokepool-*.yaml), which is the
	// same declaration the composition copies onto the cluster. Missing is fatal
	// and not defaulted: a secret written to a guessed environment is unreadable
	// by its consumer and indistinguishable from one that was never written.
	//
	// Placed AFTER the home-worker join deliberately: that path is independent of
	// Infisical and must not be blocked by an Infisical misconfiguration.
	// Authorization first: the cluster's mount trusts exactly its own
	// ServiceAccount signing key, which Cluster API writes with the control plane.
	// Until it exists the cluster cannot log in anyway, so the rest proceeds and
	// this is retried.
	var requeue time.Duration
	if key, err := r.spokeSigningKey(ctx, spokeName); err != nil {
		logger.Info("Spoke ServiceAccount signing key not available yet; its OpenBao mount waits",
			"spoke", spokeName, "reason", err.Error())
		requeue = 30 * time.Second
	} else if err := r.Store.Bao.EnsureSpokeAuth(ctx, spokeName, string(spokePool.GetUID()), key); err != nil {
		logger.Error(err, "Failed to authorize the spoke in OpenBao", "spoke", spokeName)
		return ctrl.Result{}, err
	}

	crossplaneIsFirstTime := !r.isStatusConditionTrue(spokePool, "CrossplaneAdminPasswordGenerated")
	crossplaneResult, err := r.Store.EnsureCrossplaneAdminPassword(ctx, spokeName, crossplaneIsFirstTime)
	if err != nil {
		logger.Error(err, "Failed to ensure crossplane-admin password for SpokePool", "spoke", spokeName)
		if crossplaneResult == secrets.EnsureMissing {
			_ = r.updateStatusCondition(ctx, spokePool, spokeName, false, false, crossplaneResult)
		}
		return ctrl.Result{}, err
	}

	if fleet, err := r.Store.MaterialiseFleetCredentials(ctx, spokeName); err != nil {
		logger.Error(err, "Failed to materialise fleet credentials into the cluster's scope", "spoke", spokeName)
	} else if len(fleet.MissingAtRoot) > 0 {
		logger.Error(nil, "Fleet credentials absent from the hub scope; seed them or the consuming feature stays down",
			"spoke", spokeName, "missing", fleet.MissingAtRoot)
	}

	if err := ensureLogsBucket(ctx, r.UncachedClient, spokeName); err != nil {
		logger.Error(err, "Failed to ensure the spoke's log bucket", "spoke", spokeName)
	}

	// The per-cluster OIDC session signing key is produced by the hub-operator's
	// application-secret uploader (internal/infisical), which writes ClusterScoped
	// mappings into /spoke-pool/<clusterName>/shared. It is deliberately NOT produced
	// here: this reconciler creates the cluster's Infisical folder, and a producer
	// that runs in the same pass races its own prerequisite.

	// create bootstrap certificate for ArgoCD Agent mTLS via cert-manager (ADR-035)
	if err := r.ensureBootstrapCertificate(ctx, spokePool); err != nil {
		logger.Error(err, "Failed to ensure bootstrap certificate", "spoke", spokeName)
		// Non-fatal: Spoke can bootstrap without a cert-manager Certificate CR,
		// but ArgoCD Agent mTLS won't work until it's created.
	}

	// create bootstrap certificate CRS wrapper for ClusterResourceSet delivery
	if err := r.ensureBootstrapCertCRSWrapper(ctx, spokePool); err != nil {
		logger.Error(err, "Failed to ensure bootstrap CRS wrapper", "spoke", spokeName)
		// Non-fatal: CRS wrapper creation can be retried on next reconcile.
	}

	// create the agent root-CA CRS wrapper for ClusterResourceSet delivery
	if err := r.ensureBootstrapCACRSWrapper(ctx, spokePool); err != nil {
		logger.Error(err, "Failed to ensure agent-ca CRS wrapper", "spoke", spokeName)
		// Non-fatal: retried on next reconcile.
	}

	// render the spoke's cilium-config with its control-plane endpoint (ADR-046 §24,
	// ADR-048). Must precede anything that expects the spoke to have a working CNI.
	if err := r.ensureCiliumConfigCRSWrapper(ctx, spokePool); err != nil {
		logger.Error(err, "Failed to ensure cilium-config CRS wrapper", "spoke", spokeName)
		// Non-fatal: retried on next reconcile. Fail-closed means a deferral is not
		// an error, so anything reaching here is a real fault worth surfacing.
	}

	// Ensure the encryption configuration Secret exists in platform-capi (ADR-100).
	// Without it, KubeadmConfig bootstrap data cannot be generated by CAPI.
	if err := r.ensureEncryptionSecret(ctx, spokePool); err != nil {
		logger.Error(err, "Failed to ensure encryption secret", "spoke", spokeName)
		// Non-fatal: retried on next reconcile.
	}

	return ctrl.Result{RequeueAfter: requeue}, r.updateStatusCondition(ctx, spokePool, spokeName, requeue == 0, true, crossplaneResult)
}

// isStatusConditionTrue checks if a condition is set to True
func (r *SpokePoolReconciler) isStatusConditionTrue(spokePool *unstructured.Unstructured, conditionType string) bool {
	conditions, found, err := unstructured.NestedSlice(spokePool.Object, "status", "conditions")
	if err != nil || !found {
		return false
	}

	for _, c := range conditions {
		if condMap, ok := c.(map[string]interface{}); ok {
			if condMap["type"] == conditionType && condMap["status"] == string(metav1.ConditionTrue) {
				return true
			}
		}
	}

	return false
}

// updateStatusCondition sets CrossplaneAdminSecretGenerated and
// CrossplaneAdminPasswordGenerated conditions on SpokePool XR.
// CrossplaneAdminSecretGenerated tracks Machine Identity credentials at
// /spoke-pool/<clusterName>/shared/infisical-credentials (ADR-031 Cell-Based Identity
// Topology). CrossplaneAdminPasswordGenerated tracks the platform-bootstrap
// crossplane_admin password at /<spoke>-crossplane-admin-password.
func (r *SpokePoolReconciler) updateStatusCondition(ctx context.Context, spokePool *unstructured.Unstructured, spokeName string, success bool, alreadyExisted bool, crossplaneResult secrets.EnsureResult) error {
	logger := log.FromContext(ctx)

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Re-fetch the latest version to avoid resource version conflicts when two
		// replicas reconcile the same SpokePool simultaneously.
		latest := &unstructured.Unstructured{}
		latest.SetGroupVersionKind(spokePool.GroupVersionKind())
		if err := r.Get(ctx, client.ObjectKeyFromObject(spokePool), latest); err != nil {
			logger.Error(err, "Failed to re-fetch SpokePool before status update", "spoke", spokeName)
			return err
		}

		conditions, found, err := unstructured.NestedSlice(latest.Object, "status", "conditions")
		if err != nil {
			logger.Error(err, "Failed to get status conditions", "spoke", spokeName)
			return err
		}

		if !found {
			conditions = []interface{}{}
		}

		// Convert to metav1.Condition slice
		var metaConditions []metav1.Condition
		for _, c := range conditions {
			if condMap, ok := c.(map[string]interface{}); ok {
				// Skip conditions with missing required fields
				typeVal, typeOk := condMap["type"].(string)
				statusVal, statusOk := condMap["status"].(string)
				reasonVal, reasonOk := condMap["reason"].(string)
				messageVal, messageOk := condMap["message"].(string)

				if !typeOk || !statusOk || !reasonOk || !messageOk {
					continue
				}

				var transitionTime metav1.Time
				if timeStr, ok := condMap["lastTransitionTime"].(string); ok {
					if parsedTime, err := time.Parse(time.RFC3339, timeStr); err == nil {
						transitionTime = metav1.NewTime(parsedTime)
					}
				}

				metaConditions = append(metaConditions, metav1.Condition{
					Type:               typeVal,
					Status:             metav1.ConditionStatus(statusVal),
					Reason:             reasonVal,
					Message:            messageVal,
					LastTransitionTime: transitionTime,
				})
			}
		}

		// Set or update condition based on success/failure and whether credentials already existed
		var condition metav1.Condition
		if success {
			if alreadyExisted {
				condition = metav1.Condition{
					Type:    "CrossplaneAdminSecretGenerated",
					Status:  metav1.ConditionTrue,
					Reason:  "AlreadyExists",
					Message: fmt.Sprintf("auth/spoke-%s trusts this cluster's ServiceAccount signing key", spokeName),
				}
			} else {
				condition = metav1.Condition{
					Type:    "CrossplaneAdminSecretGenerated",
					Status:  metav1.ConditionTrue,
					Reason:  "Generated",
					Message: fmt.Sprintf("auth/spoke-%s created for this cluster's ServiceAccount signing key", spokeName),
				}
			}
		} else {
			condition = metav1.Condition{
				Type:    "CrossplaneAdminSecretGenerated",
				Status:  metav1.ConditionFalse,
				Reason:  "SigningKeyPending",
				Message: "the cluster's ServiceAccount signing key (<cluster>-sa) is not available yet, so its OpenBao mount is not configured",
			}
		}

		meta.SetStatusCondition(&metaConditions, condition)

		// CrossplaneAdminPasswordGenerated — platform bootstrap password lifecycle.
		crossplaneCondition := metav1.Condition{Type: "CrossplaneAdminPasswordGenerated"}
		switch crossplaneResult {
		case secrets.EnsureCreated:
			crossplaneCondition = metav1.Condition{
				Type:    "CrossplaneAdminPasswordGenerated",
				Status:  metav1.ConditionTrue,
				Reason:  "Generated",
				Message: fmt.Sprintf("Crossplane-admin password generated and uploaded to Infisical at %s-crossplane-admin-password", spokeName),
			}
		case secrets.EnsureAlreadyExists:
			crossplaneCondition = metav1.Condition{
				Type:    "CrossplaneAdminPasswordGenerated",
				Status:  metav1.ConditionTrue,
				Reason:  "AlreadyExists",
				Message: fmt.Sprintf("Crossplane-admin password already exists in Infisical at %s-crossplane-admin-password", spokeName),
			}
		default:
			crossplaneCondition = metav1.Condition{
				Type:    "CrossplaneAdminPasswordGenerated",
				Status:  metav1.ConditionFalse,
				Reason:  "PasswordMissing",
				Message: "CRITICAL: Crossplane-admin password missing from Infisical for already-provisioned SpokePool. Manual recovery required.",
			}
		}
		meta.SetStatusCondition(&metaConditions, crossplaneCondition)

		// Serialise back to unstructured map format for Crossplane.
		var newConditions []interface{}
		for _, c := range metaConditions {
			newConditions = append(newConditions, map[string]interface{}{
				"type":               c.Type,
				"status":             string(c.Status),
				"reason":             c.Reason,
				"message":            c.Message,
				"lastTransitionTime": c.LastTransitionTime.Format(time.RFC3339),
			})
		}

		if err := unstructured.SetNestedSlice(latest.Object, newConditions, "status", "conditions"); err != nil {
			logger.Error(err, "Failed to set status conditions", "spoke", spokeName)
			return err
		}

		if err := r.Status().Update(ctx, latest); err != nil {
			// Do not log error here; let RetryOnConflict handle the retry/backoff silently
			// until it exceeds max retries.
			return err
		}

		return nil
	})

	if err != nil {
		logger.Error(err, "Failed to update status after retries", "spoke", spokeName)
		return err
	}

	return nil
}

// ensureBootstrapCertificate creates a 72-hour Certificate CR for ArgoCD Agent mTLS bootstrap.
// cert-manager + openbao-fleet fulfills this Certificate declaratively per ADR-035.
// The resulting TLS Secret is read by ensureBootstrapCertCRSWrapper for ClusterResourceSet delivery.
// Domain: platform-capi (ADR-015), Certificate declares intent only — PKI is cert-manager's domain (ADR-035).
func (r *SpokePoolReconciler) ensureBootstrapCertificate(ctx context.Context, spokePool *unstructured.Unstructured) error {
	logger := log.FromContext(ctx)
	spokeName := spokePool.GetName()

	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "cert-manager.io",
		Version: "v1",
		Kind:    "Certificate",
	})
	cert.SetName(fmt.Sprintf("argocd-agent-%s", spokeName))
	cert.SetNamespace("platform-capi")

	// Check if already exists (idempotent)
	err := r.Get(ctx, client.ObjectKeyFromObject(cert), cert)
	if err == nil {
		logger.Info("Bootstrap Certificate already exists", "certificate", cert.GetName())
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	// Safe map assignment to preserve GVK
	if cert.Object["metadata"] == nil {
		cert.Object["metadata"] = map[string]interface{}{}
	}
	cert.Object["metadata"].(map[string]interface{})["name"] = cert.GetName()
	cert.Object["metadata"].(map[string]interface{})["namespace"] = cert.GetNamespace()
	cert.Object["metadata"].(map[string]interface{})["labels"] = map[string]interface{}{
		"platform.nutgraf.in/bootstrap": "true",
	}
	cert.Object["metadata"].(map[string]interface{})["ownerReferences"] = []interface{}{
		map[string]interface{}{
			"apiVersion":         "nutgraf.in/v1alpha1",
			"kind":               "SpokePool",
			"name":               spokePool.GetName(),
			"uid":                string(spokePool.GetUID()),
			"controller":         true,
			"blockOwnerDeletion": true,
		},
	}

	cert.Object["spec"] = map[string]interface{}{
		"secretName":  fmt.Sprintf("argocd-agent-%s-tls", spokeName),
		"duration":    "72h",
		"renewBefore": "36h",
		"commonName":  fmt.Sprintf("argocd-agent.%s", spokeName),
		"isCA":        false,
		"usages": []interface{}{
			"server auth",
			"client auth",
		},
		"issuerRef": map[string]interface{}{
			"name":  "openbao-fleet",
			"kind":  "ClusterIssuer",
			"group": "cert-manager.io",
		},
	}

	logger.Info(
		"Certificate before create",
		"gvk", cert.GroupVersionKind().String(),
		"object", cert.Object,
	)

	if err := r.Create(ctx, cert); err != nil {
		return fmt.Errorf("create bootstrap Certificate CR: %w", err)
	}

	logger.Info("Created bootstrap Certificate CR", "certificate", cert.GetName(), "spoke", spokeName)
	return nil
}

// +kubebuilder:rbac:groups=cert-manager.io,resources=certificates,verbs=get;list;watch;create;update;patch

// ensureBootstrapCertCRSWrapper reads the cert-manager-issued TLS Secret for the spoke's bootstrap
// certificate and packages it into a ClusterResourceSet wrapper Secret (type addons.cluster.x-k8s.io/resource-set).
// The wrapper is immutable once created — it is an ApplyOnce bootstrap artifact and must never
// be regenerated after initial creation, even if the underlying Certificate renews (ADR-035).
func (r *SpokePoolReconciler) ensureBootstrapCertCRSWrapper(ctx context.Context, spokePool *unstructured.Unstructured) error {
	logger := log.FromContext(ctx)
	spokeName := spokePool.GetName()

	wrapperName := fmt.Sprintf("%s-bootstrap-cert", spokeName)
	tlsSecretName := fmt.Sprintf("argocd-agent-%s-tls", spokeName)

	// Immutable guard: if wrapper already exists with correct type and non-empty
	// data, do not regenerate. If it exists but is empty (created before
	// cert-manager issued the certificate), delete it so it is recreated with
	// real material on the next reconcile.
	//
	// Read via UncachedClient: the manager cache transform strips Secret .data
	// (cmd/main.go), so a cached read always appears empty and would delete +
	// recreate the wrapper on every reconcile — dropping the ClusterResourceSet
	// ownerRef and stalling CRS delivery.
	existing := &corev1.Secret{}
	if err := r.UncachedClient.Get(ctx, client.ObjectKey{Name: wrapperName, Namespace: "platform-capi"}, existing); err == nil {
		if existing.Type == "addons.cluster.x-k8s.io/resource-set" {
			if len(existing.Data) > 0 && len(existing.Data["bootstrap-cert.yaml"]) > 0 {
				logger.Info("Bootstrap CRS wrapper already exists, skipping", "wrapper", wrapperName)
				return nil
			}
			logger.Info("Bootstrap CRS wrapper is empty, deleting to allow regeneration", "wrapper", wrapperName)
			if err := r.Delete(ctx, existing); err != nil {
				return fmt.Errorf("delete empty bootstrap CRS wrapper: %w", err)
			}
		}
	}

	// Read the cert-manager-issued TLS Secret (uncached — data is stripped by
	// the cache transform; only metadata is preserved).
	tlsSecret := &corev1.Secret{}
	if err := r.UncachedClient.Get(ctx, client.ObjectKey{Name: tlsSecretName, Namespace: "platform-capi"}, tlsSecret); err != nil {
		return fmt.Errorf("read TLS Secret %s: %w", tlsSecretName, err)
	}

	// Extract certificate material
	tlsCrt := string(tlsSecret.Data["tls.crt"])
	tlsKey := string(tlsSecret.Data["tls.key"])
	caCrt := string(tlsSecret.Data["ca.crt"])

	// ADR-035: never wrap an un-issued certificate. The wrapper is immutable
	// once created, so creating it with empty data permanently breaks the spoke's
	// ArgoCD Agent mTLS. Defer to the next reconcile instead (non-fatal).
	if tlsCrt == "" || tlsKey == "" || caCrt == "" {
		logger.Info("Bootstrap TLS secret not yet issued, deferring CRS wrapper", "secret", tlsSecretName, "spoke", spokeName)
		return fmt.Errorf("bootstrap TLS secret %s not yet issued (empty data)", tlsSecretName)
	}

	// Build embedded YAML manifests for the CRS wrapper (matches cert-operator pattern)
	// The TLS private key is included in the wrapper as required by the CRS delivery model,
	// but is discarded by cert-manager on the spoke after the bootstrap cert is applied.
	certYAML := fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: argocd-agent-client-cert
  namespace: argocd
  labels:
    platform.nutgraf.in/bootstrap: "true"
type: kubernetes.io/tls
stringData:
  tls.crt: |
%s
  tls.key: |
%s
  ca.crt: |
%s
`, indentYAML(tlsCrt), indentYAML(tlsKey), indentYAML(caCrt))

	wrapper := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      wrapperName,
			Namespace: "platform-capi",
			Labels:    map[string]string{"addons.cluster.x-k8s.io/resource-set": "true"},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         "nutgraf.in/v1alpha1",
					Kind:               "SpokePool",
					Name:               spokePool.GetName(),
					UID:                spokePool.GetUID(),
					Controller:         ptr.To(true),
					BlockOwnerDeletion: ptr.To(true),
				},
			},
		},
		Type:       "addons.cluster.x-k8s.io/resource-set",
		StringData: map[string]string{"bootstrap-cert.yaml": certYAML},
	}

	if err := r.Create(ctx, wrapper); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("create bootstrap CRS wrapper: %w", err)
	}

	logger.Info("Created bootstrap CRS wrapper Secret", "wrapper", wrapperName, "spoke", spokeName)
	return nil
}

// ensureBootstrapCACRSWrapper packages the principal CA into a ClusterResourceSet
// wrapper that delivers the `argocd-agent-ca` Secret to the spoke. The agent
// mounts this CA (agent.tls.root-ca-secret-name) to verify the principal (hub)
// server certificate over mTLS. The CA is the same principal root CA carried in
// the spoke's issued TLS secret (ca.crt).
func (r *SpokePoolReconciler) ensureBootstrapCACRSWrapper(ctx context.Context, spokePool *unstructured.Unstructured) error {
	logger := log.FromContext(ctx)
	spokeName := spokePool.GetName()

	wrapperName := fmt.Sprintf("%s-agent-ca", spokeName)

	// Immutable guard: skip if the wrapper already exists with data.
	existing := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Name: wrapperName, Namespace: "platform-capi"}, existing); err == nil {
		if existing.Type == "addons.cluster.x-k8s.io/resource-set" {
			logger.Info("Agent-CA CRS wrapper already exists, skipping", "wrapper", wrapperName)
			return nil
		}
	}

	// Read the principal CA from the spoke's issued TLS secret (ca.crt).
	tlsSecret := &corev1.Secret{}
	if err := r.UncachedClient.Get(ctx, client.ObjectKey{Name: fmt.Sprintf("argocd-agent-%s-tls", spokeName), Namespace: "platform-capi"}, tlsSecret); err != nil {
		return fmt.Errorf("read TLS Secret for agent CA: %w", err)
	}
	caCrt := string(tlsSecret.Data["ca.crt"])
	if caCrt == "" {
		return fmt.Errorf("TLS secret ca.crt not yet issued")
	}

	caYAML := fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: argocd-agent-ca
  namespace: argocd
  labels:
    platform.nutgraf.in/bootstrap: "true"
type: Opaque
stringData:
  ca.crt: |
%s
`, indentYAML(caCrt))

	wrapper := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      wrapperName,
			Namespace: "platform-capi",
			Labels:    map[string]string{"addons.cluster.x-k8s.io/resource-set": "true"},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         "nutgraf.in/v1alpha1",
					Kind:               "SpokePool",
					Name:               spokePool.GetName(),
					UID:                spokePool.GetUID(),
					Controller:         ptr.To(true),
					BlockOwnerDeletion: ptr.To(true),
				},
			},
		},
		Type:       "addons.cluster.x-k8s.io/resource-set",
		StringData: map[string]string{"agent-ca.yaml": caYAML},
	}

	if err := r.Create(ctx, wrapper); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("create agent-ca CRS wrapper: %w", err)
	}

	logger.Info("Created Agent-CA CRS wrapper Secret", "wrapper", wrapperName, "spoke", spokeName)
	return nil
}

// ciliumConfigBaseName is the provider-owned static half of the spoke's Cilium
// configuration, delivered to the hub by ArgoCD from manifests/providers/<p>/k8s/.
func ciliumConfigBaseName(provider string) string {
	return fmt.Sprintf("cilium-config-base-%s", provider)
}

// ensureCiliumConfigCRSWrapper renders the spoke's `cilium-config` ConfigMap into a
// ClusterResourceSet wrapper Secret, injecting the control-plane endpoint that only
// exists at runtime. This is the ADR-048 two-stage lifecycle applied to the CNI:
// cluster lifecycle owns injection of cluster-specific platform configuration.
//
// Why it exists (ADR-046 §24). The shared ClusterClass deletes kube-proxy right after
// `kubeadm init`, because its REJECT chains for endpoint-less services break Gateway
// API under Cilium's full kube-proxy-replacement. Cilium is the intended replacement,
// but with no `k8s-service-host` it discovers the API server through the in-cluster
// ClusterIP (10.96.0.1) — the very address kube-proxy used to route and that Cilium
// itself has not yet programmed. On a fresh spoke that is a hard deadlock: Cilium's
// `config` init container times out, the CNI never initialises, the node stays
// NotReady, and every platform workload sits Pending behind it.
//
// ONE OWNER. The delivered ConfigMap is rendered only here. The addon Secrets
// deliberately no longer carry a `cilium-config` document, so nothing else can write
// the object on a spoke. The settings themselves stay in Git (the base ConfigMap);
// this function contributes exactly the two keys that Git cannot know.
//
// FAIL CLOSED. While `controlPlaneEndpoint` is unset, or the base ConfigMap is
// absent, NO payload is published. A wrapper carrying an empty host would be applied
// by CRS and reproduce the deadlock it exists to prevent — silently, and on a cluster
// that reports nothing wrong until its first pod fails to schedule.
//
// Unlike the bootstrap certificate wrapper this one is NOT immutable: if CAPH ever
// rebuilds the load balancer the endpoint changes, and the spoke CRS (strategy
// Reconcile) re-applies on payload change. An immutable wrapper would pin a spoke to
// a dead address.
func (r *SpokePoolReconciler) ensureCiliumConfigCRSWrapper(ctx context.Context, spokePool *unstructured.Unstructured) error {
	logger := log.FromContext(ctx)
	spokeName := spokePool.GetName()
	wrapperName := fmt.Sprintf("%s-cilium-config", spokeName)

	provider, _, _ := unstructured.NestedString(spokePool.Object, "spec", "provider")
	if provider == "" {
		logger.Info("SpokePool has no spec.provider; deferring cilium-config wrapper", "spoke", spokeName)
		return nil
	}

	// The control-plane endpoint is the SOLE source for the API address. CAPH
	// populates it when it creates the load balancer, which happens before any
	// machine is provisioned — so it is always available before a node could need
	// it. That ordering is the whole basis of the cold-boot fix and is asserted by
	// TestCiliumConfigWrapper_EndpointPrecedesNodeBoot.
	capiCluster := &unstructured.Unstructured{}
	capiCluster.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "cluster.x-k8s.io",
		Version: "v1beta1",
		Kind:    "Cluster",
	})
	if err := r.Get(ctx, client.ObjectKey{Name: spokeName, Namespace: "platform-capi"}, capiCluster); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("CAPI Cluster not created yet; deferring cilium-config wrapper", "spoke", spokeName)
			return nil
		}
		return fmt.Errorf("read CAPI Cluster %s: %w", spokeName, err)
	}

	host, _, _ := unstructured.NestedString(capiCluster.Object, "spec", "controlPlaneEndpoint", "host")
	port, portFound, _ := unstructured.NestedInt64(capiCluster.Object, "spec", "controlPlaneEndpoint", "port")
	if host == "" || !portFound || port == 0 {
		logger.Info("controlPlaneEndpoint not populated yet; deferring cilium-config wrapper (fail-closed)",
			"spoke", spokeName, "host", host, "port", port)
		return nil
	}

	baseName := ciliumConfigBaseName(provider)
	base := &corev1.ConfigMap{}
	if err := r.Get(ctx, client.ObjectKey{Name: baseName, Namespace: "platform-capi"}, base); err != nil {
		return fmt.Errorf("read cilium-config base %s (ADR-046 §24 — delivered by "+
			"manifests/providers/%s/k8s/cilium-config-base.yaml): %w", baseName, provider, err)
	}
	if len(base.Data) == 0 {
		return fmt.Errorf("cilium-config base %s is empty; refusing to render a spoke CNI config from it", baseName)
	}

	// Copy so the cached base object is never mutated, then contribute exactly the
	// two runtime keys. Everything else is whatever Git says.
	data := make(map[string]string, len(base.Data)+2)
	for k, v := range base.Data {
		data[k] = v
	}
	data["k8s-service-host"] = host
	data["k8s-service-port"] = strconv.FormatInt(port, 10)

	cm := &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cilium-config",
			Namespace: "kube-system",
			Labels: map[string]string{
				"platform.nutgraf.in/rendered-by": "hub-operator",
			},
		},
		Data: data,
	}
	rendered, err := yaml.Marshal(cm)
	if err != nil {
		return fmt.Errorf("marshal cilium-config for %s: %w", spokeName, err)
	}

	desired := map[string]string{"cilium-config.yaml": string(rendered)}

	existing := &corev1.Secret{}
	if err := r.UncachedClient.Get(ctx, client.ObjectKey{Name: wrapperName, Namespace: "platform-capi"}, existing); err == nil {
		if existing.Type == "addons.cluster.x-k8s.io/resource-set" {
			if string(existing.Data["cilium-config.yaml"]) == string(rendered) {
				return nil
			}
			existing.StringData = desired
			existing.Data = nil
			if err := r.Update(ctx, existing); err != nil {
				return fmt.Errorf("update cilium-config CRS wrapper: %w", err)
			}
			logger.Info("Updated cilium-config CRS wrapper", "wrapper", wrapperName, "endpoint", fmt.Sprintf("%s:%d", host, port))
			return nil
		}
	}

	wrapper := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      wrapperName,
			Namespace: "platform-capi",
			Labels:    map[string]string{"addons.cluster.x-k8s.io/resource-set": "true"},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         "nutgraf.in/v1alpha1",
					Kind:               "SpokePool",
					Name:               spokePool.GetName(),
					UID:                spokePool.GetUID(),
					Controller:         ptr.To(true),
					BlockOwnerDeletion: ptr.To(true),
				},
			},
		},
		Type:       "addons.cluster.x-k8s.io/resource-set",
		StringData: desired,
	}

	if err := r.Create(ctx, wrapper); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("create cilium-config CRS wrapper: %w", err)
	}

	logger.Info("Created cilium-config CRS wrapper", "wrapper", wrapperName,
		"spoke", spokeName, "endpoint", fmt.Sprintf("%s:%d", host, port), "keys", len(data))
	return nil
}

// indentYAML indents each line of s by 4 spaces for embedding in YAML stringData fields.
func indentYAML(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if line != "" {
			lines[i] = "    " + line
		}
	}
	return strings.Join(lines, "\n")
}

// mapCertificateToSpokePool maps a cert-manager Certificate to its parent SpokePool.
// Uses the naming convention: Certificate name = "argocd-agent-{spokeName}".
func (r *SpokePoolReconciler) mapCertificateToSpokePool(ctx context.Context, obj client.Object) []reconcile.Request {
	cert, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil
	}
	name := cert.GetName()
	spokeName := strings.TrimPrefix(name, "argocd-agent-")
	if spokeName == name {
		return nil
	}
	return []reconcile.Request{
		{NamespacedName: types.NamespacedName{Name: spokeName}},
	}
}

// mapEncryptionSecretToSpokePool maps an encryption Secret in platform-capi to its parent SpokePool.
func (r *SpokePoolReconciler) mapEncryptionSecretToSpokePool(ctx context.Context, obj client.Object) []reconcile.Request {
	if obj.GetNamespace() != "platform-capi" {
		return nil
	}
	name := obj.GetName()
	if !strings.HasSuffix(name, "-encryption-config") {
		return nil
	}
	spokeName := strings.TrimSuffix(name, "-encryption-config")
	return []reconcile.Request{
		{NamespacedName: types.NamespacedName{Name: spokeName}},
	}
}

// resolveBoxID returns the boxID by checking HubEnvironment first, then hub-bootstrap-config ConfigMap, then os.Getenv("BOX_ID").
func (r *SpokePoolReconciler) resolveBoxID(ctx context.Context) (string, error) {
	hubEnvList := &opsv1alpha1.HubEnvironmentList{}
	if err := r.List(ctx, hubEnvList); err == nil {
		for _, env := range hubEnvList.Items {
			if b := strings.TrimSpace(env.Spec.BoxID); b != "" {
				return b, nil
			}
		}
	}

	var cm corev1.ConfigMap
	if err := r.Get(ctx, client.ObjectKey{Namespace: "platform-ops", Name: "hub-bootstrap-config"}, &cm); err == nil {
		if b := strings.TrimSpace(cm.Data["BOX_ID"]); b != "" {
			return b, nil
		}
	}

	if b := strings.TrimSpace(os.Getenv("BOX_ID")); b != "" {
		return b, nil
	}

	return "", fmt.Errorf("unable to resolve boxId: HubEnvironment.spec.boxId is empty and BOX_ID not set")
}

// ensureEncryptionSecret ensures the encryption configuration Secret exists in platform-capi (ADR-100).
func (r *SpokePoolReconciler) ensureEncryptionSecret(ctx context.Context, spokePool *unstructured.Unstructured) error {
	spokeName := spokePool.GetName()
	boxID, err := r.resolveBoxID(ctx)
	if err != nil {
		return fmt.Errorf("resolve boxId for encryption of spoke %s: %w", spokeName, err)
	}

	var escrowClient escrow.EscrowClient
	if r.EscrowClient != nil {
		escrowClient = r.EscrowClient
	} else {
		escrowClient, err = escrow.NewEscrowClient(ctx, "")
		if err != nil {
			return fmt.Errorf("connect to escrow for encryption of spoke %s: %w", spokeName, err)
		}
	}

	secReader := r.Client
	if r.UncachedClient != nil {
		secReader = r.UncachedClient
	}
	secClient := encryption.CtrlSecretClient{Client: secReader}
	opts := encryption.EnsureOptions{
		BoxID:        boxID,
		ClusterName:  spokeName,
		Namespace:    "platform-capi",
		EscrowClient: escrowClient,
		SecretClient: secClient,
	}

	res, err := encryption.EnsureClusterEncryption(ctx, opts)
	if err != nil {
		return fmt.Errorf("ensure encryption secret for spoke %s: %w", spokeName, err)
	}

	log.FromContext(ctx).Info("Encryption configuration reconciled for spoke",
		"spoke", spokeName,
		"keyRestored", res.KeyRestored,
		"keyGenerated", res.KeyGenerated,
		"secretCreated", res.SecretCreated,
		"secretAlreadyExisted", res.SecretAlreadyExisted,
	)
	return nil
}

// SetupWithManager watches SpokePools, their bootstrap Certificates and their
// encryption Secrets.
func (r *SpokePoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "nutgraf.in",
		Version: "v1alpha1",
		Kind:    "SpokePool",
	})

	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "cert-manager.io",
		Version: "v1",
		Kind:    "Certificate",
	})

	b := ctrl.NewControllerManagedBy(mgr).
		For(u).
		Watches(
			cert,
			handler.EnqueueRequestsFromMapFunc(r.mapCertificateToSpokePool),
		).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.mapEncryptionSecretToSpokePool),
		)

	// Certificate is NOT treated as optional. cert-manager is boundary 01
	// infrastructure that every box has before this controller exists, so its
	// absence is a broken cluster rather than a capability not yet installed.
	// Marking a genuinely required kind optional would hide that.
	return b.Complete(r)
}

// spokeSigningKey returns the public half of the workload cluster's ServiceAccount
// signing key: Cluster API's <cluster>-sa Secret, tls.crt. Only the public key is
// read; the private half never leaves that Secret.
func (r *SpokePoolReconciler) spokeSigningKey(ctx context.Context, cluster string) (string, error) {
	var sec corev1.Secret
	if err := r.UncachedClient.Get(ctx, types.NamespacedName{Namespace: "platform-capi", Name: cluster + "-sa"}, &sec); err != nil {
		return "", err
	}
	pub := strings.TrimSpace(string(sec.Data["tls.crt"]))
	if pub == "" {
		return "", fmt.Errorf("platform-capi/%s-sa carries no tls.crt", cluster)
	}
	return pub, nil
}
