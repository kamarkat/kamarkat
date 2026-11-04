package controller

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/kamarkat/kamarkat/operators/hub-operator/internal/secrets"
)

// A platform capability with one caller: a selectable service of the box, run
// on the hub and turned on by capabilities.<name>.enabled, which exactly one
// application of the box consumes -- the Credit Authority (ADR-109,
// credit_authority.go) and the billing engine (ADR-113, billing_engine.go).
//
// Each is reached only through its own gateway, which admits one caller. What
// this file owns is what exists only while the capability runs:
//
//	identity  its project in the platform's own organisation, provisioned when
//	          the capability is enabled -- the hub component's ConfigMap (its
//	          marker) appearing is that event
//	caller    the ONE client its gateway admits: the service client of the one
//	          application that declares `capabilities.<name>.enabled`, and that
//	          client's organisation. Absent -- capability off, no declaration, or
//	          the owner's identity not provisioned yet -- means nobody is admitted.
//
// An application consumes one by that declaration, which is the single source
// of its caller and its audience; a second application declaring it is refused.
type platformCapability struct {
	// The declaration's key under spec.capabilities, the platform project the
	// gateway requires in a token's audience, and the owner's backend audience
	// (the key suffix of OIDC_BACKEND_PROJECT_ID_<NAME> in its folder).
	name string
	// How logs and conditions name it.
	title string

	// The hub component's own ConfigMap. Rendered only while the capability is
	// enabled, so its presence is the enablement acted on.
	namespace string
	marker    string

	// Hub keys, at the root of the secrets project the hub's ClusterSecretStore
	// reads, beside the platform services' own.
	projectIDKey      string
	callerClientIDKey string
	callerOrgIDKey    string

	// Recorded on every application that declares the capability: True on the
	// owner, False on a declaration refused because another application owns it.
	ownerCondition string

	// The controller's name, unique in the manager.
	controllerName string

	// provision, when set, is the capability's own provisioning, run while it is
	// enabled and before its caller is published.
	provision func(ctx context.Context, r *AINativeSaaSReconciler) error
}

// platformCapabilities are every such capability of the box.
var platformCapabilities = []*platformCapability{&creditAuthority, &billingEngine}

// hubSecretPath is the folder of the capabilities' hub keys.
const hubSecretPath = "/"

// capabilityPendingRequeue is how soon to look again while a caller or an
// audience waits on something another reconcile writes to Infisical (the
// owner's service client, the capability's project), which raises no event here.
const capabilityPendingRequeue = time.Minute

var ainativesaasGVK = schema.GroupVersionKind{Group: "nutgraf.in", Version: "v1alpha1", Kind: "AINativeSaaS"}

// declares reports whether this application declares the capability.
func (c *platformCapability) declares(obj *unstructured.Unstructured) bool {
	on, _, _ := unstructured.NestedBool(obj.Object, "spec", "capabilities", c.name, "enabled")
	return on
}

// ownerAmong is the ONE application of the box that owns the capability's
// declaration, and the declarations refused because of it.
//
// The earliest declaration wins and keeps winning: a later application
// declaring the capability cannot take it over, it is refused with a reason.
// Ordered by creation time, then name, so every reconcile reaches the same
// answer.
func (c *platformCapability) ownerAmong(items []unstructured.Unstructured) (owner *unstructured.Unstructured, refused []string) {
	var declared []unstructured.Unstructured
	for _, it := range items {
		if c.declares(&it) {
			declared = append(declared, it)
		}
	}
	if len(declared) == 0 {
		return nil, nil
	}
	sort.Slice(declared, func(i, j int) bool {
		ti, tj := declared[i].GetCreationTimestamp(), declared[j].GetCreationTimestamp()
		if !ti.Equal(&tj) {
			return ti.Before(&tj)
		}
		return declared[i].GetName() < declared[j].GetName()
	})
	for _, d := range declared[1:] {
		refused = append(refused, d.GetName())
	}
	return &declared[0], refused
}

// isMarker selects the capability's hub ConfigMap and nothing else.
func (c *platformCapability) isMarker(obj client.Object) bool {
	return obj.GetNamespace() == c.namespace && obj.GetName() == c.marker
}

func (c *platformCapability) request() reconcile.Request {
	return reconcile.Request{NamespacedName: types.NamespacedName{Namespace: c.namespace, Name: c.marker}}
}

// capabilityOf is the capability whose marker this object is, or nil.
func capabilityOf(obj client.Object) *platformCapability {
	for _, c := range platformCapabilities {
		if c.isMarker(obj) {
			return c
		}
	}
	return nil
}

// applications lists every AINativeSaaS of the box.
func (r *AINativeSaaSReconciler) applications(ctx context.Context) ([]unstructured.Unstructured, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(ainativesaasGVK.GroupVersion().WithKind("AINativeSaaSList"))
	if err := r.List(ctx, list); err != nil {
		return nil, fmt.Errorf("list applications: %w", err)
	}
	return list.Items, nil
}

// publishHubSecret writes one of a capability's hub keys.
func (r *AINativeSaaSReconciler) publishHubSecret(ctx context.Context, key, value string) error {
	if current, err := r.InfisicalClient.GetSecret(ctx, hubSecretPath, key); err == nil && current == value {
		return nil
	}
	exists, err := r.InfisicalClient.SecretExists(ctx, hubSecretPath, key)
	if err != nil {
		return fmt.Errorf("check %s: %w", key, err)
	}
	if exists {
		return r.InfisicalClient.UpdateSecret(ctx, hubSecretPath, key, value)
	}
	return r.InfisicalClient.CreateSecret(ctx, hubSecretPath, key, value)
}

// ownedCapabilities are the capabilities this application owns the declaration
// of, with the answer for each it declares recorded on its status.
func (r *AINativeSaaSReconciler) ownedCapabilities(ctx context.Context, logger logr.Logger, obj *unstructured.Unstructured) []*platformCapability {
	var owned []*platformCapability
	var apps []unstructured.Unstructured
	listed := false
	for _, c := range platformCapabilities {
		if !c.declares(obj) {
			continue
		}
		if !listed {
			var err error
			if apps, err = r.applications(ctx); err != nil {
				logger.Error(err, "Could not decide the owners of this application's capabilities; the declarations wait")
				return nil
			}
			listed = true
		}
		owner, _ := c.ownerAmong(apps)
		if owner == nil || owner.GetName() != obj.GetName() {
			ownerName := ""
			if owner != nil {
				ownerName = owner.GetName()
			}
			logger.Error(nil, "Refusing this application's capability declaration: another application owns it",
				"capability", c.name, "app", obj.GetName(), "owner", ownerName)
			_ = r.mergeCondition(ctx, obj, metav1.Condition{
				Type:    c.ownerCondition,
				Status:  metav1.ConditionFalse,
				Reason:  "Conflict",
				Message: fmt.Sprintf("%s is owned by %s; one application may own it", c.title, ownerName),
			})
			continue
		}
		_ = r.mergeCondition(ctx, obj, metav1.Condition{
			Type:    c.ownerCondition,
			Status:  metav1.ConditionTrue,
			Reason:  "Declared",
			Message: fmt.Sprintf("this application is %s's one caller", c.title),
		})
		owned = append(owned, c)
	}
	return owned
}

// dependency is the capability as its owner's backend audience, or nil while
// it has no project -- it is not enabled, or not provisioned yet.
func (r *AINativeSaaSReconciler) capabilityDependency(ctx context.Context, c *platformCapability) *resolvedDependency {
	pid, err := r.InfisicalClient.GetSecret(ctx, hubSecretPath, c.projectIDKey)
	if err != nil || pid == "" {
		return nil
	}
	return &resolvedDependency{AppID: c.name, ProjectID: pid}
}

// capabilityDeclarers maps a capability being enabled or disabled to the
// applications that declare it, so the owner's audience follows.
func (r *AINativeSaaSReconciler) capabilityDeclarers(ctx context.Context, obj client.Object) []reconcile.Request {
	c := capabilityOf(obj)
	if c == nil {
		return nil
	}
	apps, err := r.applications(ctx)
	if err != nil {
		log.FromContext(ctx).Error(err, "Could not route a capability's change to its declarers", "capability", c.name)
		return nil
	}
	var out []reconcile.Request
	for i := range apps {
		if c.declares(&apps[i]) {
			out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&apps[i])})
		}
	}
	return out
}

// isPlatformCapabilityAudience reports whether a backend audience is a
// platform capability's rather than another application's.
func isPlatformCapabilityAudience(appID string) bool {
	for _, c := range platformCapabilities {
		if c.name == appID {
			return true
		}
	}
	return false
}

// isCapabilityMarker selects any capability's hub ConfigMap and nothing else.
func isCapabilityMarker(obj client.Object) bool {
	return capabilityOf(obj) != nil
}

// PlatformCapabilityReconciler provisions one capability's identity while it is
// enabled, and publishes or revokes its one caller.
type PlatformCapabilityReconciler struct {
	*AINativeSaaSReconciler
	capability *platformCapability
}

// PlatformCapabilityReconcilers are the reconcilers of every platform
// capability with one caller, sharing the applications' clients.
func PlatformCapabilityReconcilers(apps *AINativeSaaSReconciler) []*PlatformCapabilityReconciler {
	out := make([]*PlatformCapabilityReconciler, 0, len(platformCapabilities))
	for _, c := range platformCapabilities {
		out = append(out, &PlatformCapabilityReconciler{AINativeSaaSReconciler: apps, capability: c})
	}
	return out
}

// Name is the capability's controller name.
func (r *PlatformCapabilityReconciler) Name() string { return r.capability.controllerName }

//+kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch

// Reconcile acts on the capability's enablement and on every application change.
func (r *PlatformCapabilityReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	c := r.capability
	logger := log.FromContext(ctx).WithValues("capability", c.name)

	marker := &corev1.ConfigMap{}
	err := r.Get(ctx, req.NamespacedName, marker)
	switch {
	case apierrors.IsNotFound(err):
		// Disabled: nothing runs, and nothing is admitted if it runs again before
		// a reconcile publishes a caller.
		logger.Info("The capability is not enabled; revoking its caller")
		return ctrl.Result{}, r.revokeCaller(ctx)
	case err != nil:
		return ctrl.Result{}, err
	}

	if r.IdentityClient == nil {
		logger.Error(nil, "The capability is enabled but this environment provisions no identity; it admits nobody")
		return ctrl.Result{}, r.revokeCaller(ctx)
	}
	if c.provision != nil {
		if err := c.provision(ctx, r.AINativeSaaSReconciler); err != nil {
			return ctrl.Result{}, fmt.Errorf("provision %s: %w", c.title, err)
		}
	}
	_, projectID, err := r.IdentityClient.EnsurePlatformProject(ctx, c.name)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("provision %s's project: %w", c.title, err)
	}
	if err := r.publishHubSecret(ctx, c.projectIDKey, projectID); err != nil {
		return ctrl.Result{}, fmt.Errorf("publish %s's project id: %w", c.title, err)
	}

	return r.publishCaller(ctx, logger)
}

// publishCaller publishes the one client the capability admits -- the owning
// application's service client -- and the organisation that client belongs to.
func (r *PlatformCapabilityReconciler) publishCaller(ctx context.Context, logger logr.Logger) (ctrl.Result, error) {
	c := r.capability
	apps, err := r.applications(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	owner, _ := c.ownerAmong(apps)
	if owner == nil {
		logger.Info("No application declares the capability; it admits nobody")
		return ctrl.Result{}, r.revokeCaller(ctx)
	}

	ownerApp, _, _ := unstructured.NestedString(owner.Object, "spec", "appId")
	ownerCell, _, _ := unstructured.NestedString(owner.Object, "spec", "clusterName")
	ownerOrg, _, _ := unstructured.NestedString(owner.Object, "status", "identity", "zitadelOrgId")
	callerID, err := r.InfisicalClient.GetSecret(ctx, fmt.Sprintf(secrets.InfisicalTenantPathFormat, ownerCell, ownerApp), serviceClientIDKey)
	if err != nil || callerID == "" || ownerOrg == "" {
		logger.Info("The capability waits on its owner's service identity; it admits nobody until then",
			"owner", owner.GetName(), "expected", serviceUserName(ownerApp))
		return ctrl.Result{RequeueAfter: capabilityPendingRequeue}, r.revokeCaller(ctx)
	}
	if err := r.publishHubSecret(ctx, c.callerClientIDKey, callerID); err != nil {
		return ctrl.Result{}, fmt.Errorf("publish %s's caller: %w", c.title, err)
	}
	if err := r.publishHubSecret(ctx, c.callerOrgIDKey, ownerOrg); err != nil {
		return ctrl.Result{}, fmt.Errorf("publish %s's caller organisation: %w", c.title, err)
	}
	return ctrl.Result{}, nil
}

// revokeCaller removes the admitted caller, so a gateway admits nobody.
func (r *PlatformCapabilityReconciler) revokeCaller(ctx context.Context) error {
	c := r.capability
	for _, key := range []string{c.callerClientIDKey, c.callerOrgIDKey} {
		if err := r.InfisicalClient.DeleteSecret(ctx, hubSecretPath, key); err != nil {
			return fmt.Errorf("revoke %s's caller (%s): %w", c.title, key, err)
		}
	}
	return nil
}

// SetupWithManager reconciles on the capability's ConfigMap and on every
// application change, both mapped to the one capability.
func (r *PlatformCapabilityReconciler) SetupWithManager(mgr ctrl.Manager) error {
	c := r.capability
	app := &unstructured.Unstructured{}
	app.SetGroupVersionKind(ainativesaasGVK)
	return ctrl.NewControllerManagedBy(mgr).
		Named(c.controllerName).
		For(&corev1.ConfigMap{}, builder.WithPredicates(predicate.NewPredicateFuncs(c.isMarker))).
		Watches(app, handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
			return []reconcile.Request{c.request()}
		})).
		Complete(r)
}
