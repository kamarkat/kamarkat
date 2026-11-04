package previewaccess

import (
	"context"
	"fmt"
	"sort"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	computev1alpha1 "github.com/kamarkat/kamarkat/operators/ephemeral-job-operator/api/v1alpha1"
)

// Reconciler maintains each record's platform side: its host, whether it is
// the one record serving its target, and the network policy that lets this
// service -- and nothing else -- reach the sandbox's preview port through the
// ADR-106 ingress deny.
type Reconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Index  *Index

	// Domain is the preview registrable domain (or a subdomain of it).
	// Required; the process refuses to start without it.
	Domain string

	// Namespace and PodLabels select this service's own pods, for the
	// sandbox-side ingress rule.
	Namespace string
	PodLabels map[string]string

	// RenderPolicies is whether this cluster enforces network policy through
	// Cilium (--network-policy=cilium). On a cluster that enforces none there
	// is no sandbox ingress deny to open, and no CRD to write one to.
	RenderPolicies bool
}

var ciliumNetworkPolicyGVK = schema.GroupVersionKind{Group: "cilium.io", Version: "v2", Kind: "CiliumNetworkPolicy"}

const (
	ReasonTargetInUse       = "PreviewTargetInUse"
	ReasonNameInUse         = "PreviewNameInUse"
	ReasonServing           = "Serving"
	labelJobUID             = "compute.nutgraf.in/ephemeraljob-uid"
	labelPreviewAccessOwner = "compute.nutgraf.in/previewaccess"
)

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var rec computev1alpha1.PreviewAccess
	if err := r.Get(ctx, req.NamespacedName, &rec); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !rec.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	want := rec.Status.DeepCopy()
	want.ObservedGeneration = rec.Generation
	cond := metav1.Condition{Type: "Ready", ObservedGeneration: rec.Generation}

	winner, err := r.targetOwner(ctx, &rec)
	if err != nil {
		return ctrl.Result{}, err
	}
	switch {
	case winner != rec.Name:
		cond.Status, cond.Reason = metav1.ConditionFalse, ReasonTargetInUse
		cond.Message = fmt.Sprintf("PreviewAccess %q already serves %s port %d", winner, rec.Spec.JobRef, rec.Spec.Port)
	default:
		want.Host = HostFor(rec.UID, r.Domain)
		cond.Status, cond.Reason = metav1.ConditionTrue, ReasonServing
		cond.Message = "served at https://" + want.Host
	}
	meta.SetStatusCondition(&want.Conditions, cond)

	if r.RenderPolicies {
		if err := r.reconcileIngress(ctx, &rec, cond.Status == metav1.ConditionTrue); err != nil {
			return ctrl.Result{}, err
		}
	}

	if !equality.Semantic.DeepEqual(&rec.Status, want) {
		rec.Status = *want
		if err := r.Status().Update(ctx, &rec); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

// targetOwner is the name of the one record serving rec's (jobRef, port): the
// oldest, then the lexically first. Admission refuses a second record
// (webhook.go); this is what makes the answer the same when two creates race
// past admission together -- the newer never becomes Ready.
func (r *Reconciler) targetOwner(ctx context.Context, rec *computev1alpha1.PreviewAccess) (string, error) {
	var l computev1alpha1.PreviewAccessList
	if err := r.List(ctx, &l, client.InNamespace(rec.Namespace),
		client.MatchingFields{IndexTarget: TargetKey(rec.Spec.JobRef, rec.Spec.Port)}); err != nil {
		return "", err
	}
	items := l.Items
	if len(items) == 0 {
		return rec.Name, nil
	}
	sort.Slice(items, func(i, j int) bool {
		a, b := items[i].CreationTimestamp, items[j].CreationTimestamp
		if !a.Equal(&b) {
			return a.Before(&b)
		}
		return items[i].Name < items[j].Name
	})
	return items[0].Name, nil
}

// reconcileIngress renders <record>-preview: ingress to the record's sandbox
// pod, on the record's port, from this service's pods only. It exists only
// while the record is serving and its job exists, and is owned by the record,
// so deleting the record removes it.
func (r *Reconciler) reconcileIngress(ctx context.Context, rec *computev1alpha1.PreviewAccess, serving bool) error {
	name := rec.Name + "-preview"
	var ej computev1alpha1.EphemeralJob
	err := r.Get(ctx, client.ObjectKey{Namespace: rec.Namespace, Name: rec.Spec.JobRef}, &ej)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if !serving || apierrors.IsNotFound(err) {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(ciliumNetworkPolicyGVK)
		u.SetNamespace(rec.Namespace)
		u.SetName(name)
		return client.IgnoreNotFound(r.Delete(ctx, u))
	}

	from := map[string]any{"k8s:io.kubernetes.pod.namespace": r.Namespace}
	for k, v := range r.PodLabels {
		from[k] = v
	}
	want := &unstructured.Unstructured{}
	want.SetGroupVersionKind(ciliumNetworkPolicyGVK)
	want.SetNamespace(rec.Namespace)
	want.SetName(name)
	want.SetLabels(map[string]string{labelPreviewAccessOwner: rec.Name})
	want.Object["spec"] = map[string]any{
		"endpointSelector": map[string]any{"matchLabels": map[string]any{labelJobUID: string(ej.UID)}},
		"ingress": []any{map[string]any{
			"fromEndpoints": []any{map[string]any{"matchLabels": from}},
			"toPorts": []any{map[string]any{"ports": []any{map[string]any{
				"port": fmt.Sprint(rec.Spec.Port), "protocol": "TCP"}}}},
		}},
	}
	if err := controllerutil.SetControllerReference(rec, want, r.Scheme); err != nil {
		return err
	}
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(ciliumNetworkPolicyGVK)
	err = r.Get(ctx, client.ObjectKeyFromObject(want), got)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, want)
	}
	if err != nil {
		return err
	}
	if equality.Semantic.DeepEqual(got.Object["spec"], want.Object["spec"]) {
		return nil
	}
	want.SetResourceVersion(got.GetResourceVersion())
	return r.Update(ctx, want)
}

// SetupWithManager registers the indexes the request path uses and the
// watches: a record changes, or the job it points at does.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	ctx := context.Background()
	fi := mgr.GetFieldIndexer()
	pa := &computev1alpha1.PreviewAccess{}
	if err := fi.IndexField(ctx, pa, IndexHost, func(o client.Object) []string {
		if h := o.(*computev1alpha1.PreviewAccess).Status.Host; h != "" {
			return []string{h}
		}
		return nil
	}); err != nil {
		return err
	}
	if err := fi.IndexField(ctx, pa, IndexTarget, func(o client.Object) []string {
		s := o.(*computev1alpha1.PreviewAccess).Spec
		return []string{TargetKey(s.JobRef, s.Port)}
	}); err != nil {
		return err
	}
	if err := fi.IndexField(ctx, pa, IndexJobRef, func(o client.Object) []string {
		return []string{o.(*computev1alpha1.PreviewAccess).Spec.JobRef}
	}); err != nil {
		return err
	}
	if err := fi.IndexField(ctx, pa, IndexEmbeddableBy, func(o client.Object) []string {
		rec := o.(*computev1alpha1.PreviewAccess)
		keys := make([]string, 0, len(rec.Spec.EmbeddableBy))
		for _, app := range rec.Spec.EmbeddableBy {
			keys = append(keys, EmbedKey(app, rec.Name))
		}
		return keys
	}); err != nil {
		return err
	}

	byJob := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
		var l computev1alpha1.PreviewAccessList
		if err := mgr.GetClient().List(ctx, &l, client.InNamespace(o.GetNamespace()),
			client.MatchingFields{IndexJobRef: o.GetName()}); err != nil {
			return nil
		}
		out := make([]reconcile.Request, 0, len(l.Items))
		for _, it := range l.Items {
			out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&it)})
		}
		return out
	})
	// A record losing or winning its target re-ranks its siblings.
	byTarget := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
		s := o.(*computev1alpha1.PreviewAccess).Spec
		var l computev1alpha1.PreviewAccessList
		if err := mgr.GetClient().List(ctx, &l, client.InNamespace(o.GetNamespace()),
			client.MatchingFields{IndexTarget: TargetKey(s.JobRef, s.Port)}); err != nil {
			return nil
		}
		out := make([]reconcile.Request, 0, len(l.Items))
		for _, it := range l.Items {
			out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&it)})
		}
		return out
	})

	// A record usually goes by garbage collection (its ownerReference), which
	// never shows this controller a deletion timestamp. The delete event is the
	// one place its host can be kept, so a refusal page for it can still
	// address the apps that may have embedded it.
	tombstone := handler.Funcs{
		DeleteFunc: func(_ context.Context, e event.TypedDeleteEvent[client.Object], _ workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			if rec, ok := e.Object.(*computev1alpha1.PreviewAccess); ok && rec.Status.Host != "" {
				r.Index.noteEnded(rec.Status.Host, rec.Namespace, rec.Spec.EmbeddableBy)
			}
		},
	}

	return ctrl.NewControllerManagedBy(mgr).
		Named("previewaccess").
		For(&computev1alpha1.PreviewAccess{}).
		Watches(&computev1alpha1.PreviewAccess{}, byTarget).
		Watches(&computev1alpha1.PreviewAccess{}, tombstone).
		Watches(&computev1alpha1.EphemeralJob{}, byJob).
		Complete(r)
}
