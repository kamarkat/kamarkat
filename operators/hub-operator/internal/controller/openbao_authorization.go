package controller

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kamarkat/kamarkat/operators/hub-operator/internal/openbao"
)

// incarnationAnnotation records the openbao-db incarnation an application was
// last brought up to date with.
const incarnationAnnotation = "ops.nutgraf.in/openbao-incarnation"

// reconcileOpenBaoAuthorization applies, in one pass, every authorization object
// hub-operator owns in OpenBao. The per-object controllers converge the same
// objects one at a time; this pass exists for a restored store, where everything
// must be back to desired state before recovery can be recorded (ADR-117 §7).
// It fails on the first object it cannot converge, so a pass that returns nil
// covered all of them.
func reconcileOpenBaoAuthorization(ctx context.Context, c, uncached client.Client, bao *openbao.Client, incarnation string) error {
	if err := bao.EnsurePKIRoles(ctx); err != nil {
		return err
	}

	spokes := &unstructured.UnstructuredList{}
	spokes.SetGroupVersionKind(schema.GroupVersionKind{Group: "nutgraf.in", Version: "v1alpha1", Kind: "SpokePoolList"})
	if err := c.List(ctx, spokes); err != nil {
		return fmt.Errorf("list SpokePools: %w", err)
	}
	for _, sp := range spokes.Items {
		if !sp.GetDeletionTimestamp().IsZero() {
			continue
		}
		key, err := spokeSigningKey(ctx, uncached, sp.GetName())
		if err != nil {
			return fmt.Errorf("spoke %s: its current signing key is unreadable: %w", sp.GetName(), err)
		}
		if err := bao.EnsureSpokeAuth(ctx, sp.GetName(), string(sp.GetUID()), key); err != nil {
			return fmt.Errorf("spoke %s: %w", sp.GetName(), err)
		}
	}

	apps := &unstructured.UnstructuredList{}
	apps.SetGroupVersionKind(schema.GroupVersionKind{Group: "nutgraf.in", Version: "v1alpha1", Kind: "AINativeSaaSList"})
	if err := c.List(ctx, apps); err != nil {
		return fmt.Errorf("list AINativeSaaS: %w", err)
	}
	for _, a := range apps.Items {
		if !a.GetDeletionTimestamp().IsZero() {
			continue
		}
		tenant, _, _ := unstructured.NestedString(a.Object, "spec", "tenantId")
		app, _, _ := unstructured.NestedString(a.Object, "spec", "appId")
		cluster, _, _ := unstructured.NestedString(a.Object, "spec", "clusterName")
		if strings.TrimSpace(tenant) == "" || strings.TrimSpace(app) == "" || strings.TrimSpace(cluster) == "" {
			continue // the AINativeSaaS controller skips these too
		}
		if err := bao.EnsureTenantAuth(ctx, cluster, tenant, app, string(a.GetUID())); err != nil {
			return fmt.Errorf("application %s on %s: %w", app, cluster, err)
		}
		// Stamp the incarnation: the metadata change wakes the application's own
		// reconcile, which re-checks its credentials against the restored store.
		if a.GetAnnotations()[incarnationAnnotation] != incarnation {
			patch := []byte(fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, incarnationAnnotation, incarnation))
			if err := c.Patch(ctx, &a, client.RawPatch(types.MergePatchType, patch)); err != nil {
				return fmt.Errorf("application %s on %s: mark incarnation: %w", app, cluster, err)
			}
		}
	}
	return nil
}
