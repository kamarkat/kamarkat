package controller

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	opsv1alpha1 "github.com/kamarkat/kamarkat/operators/hub-operator/api/v1alpha1"
)

// The hub's public ingress address, resolved in-cluster.
//
// This is the address the hub Gateway and its HTTPRoutes publish their hostnames
// at -- the CAPH-managed control-plane load balancer, which also carries public
// ingress on 80/443 (ADR-046 §25.3). Every public URL for the box depends on it.
// Without it external-dns has no target, reports "All records are already up to
// date" against an empty zone, and infisical, argocd, auth, id, dashboard and api
// are all NXDOMAIN with ACME unable to issue for any of them.
//
// It used to arrive as a Day-0 chart value, which was wrong twice over.
//
// It did not work: ADR-072 made the tenant's repository authoritative, so the
// seed a tenant box runs is the one that repository declares, and a value the CLI
// resolved and injected into its OWN rendered seed reached nothing.
//
// And it could not be repaired the way this platform promises repairs arrive. A
// customer upgrades a bundle version; they do not run a CLI, edit their
// repository, or rebuild a cluster. A fix that needs Day-0 to re-run reaches only
// clusters built after the fix, which in production is none of them.
//
// The address was never a Day-0 fact. kubeadm records it on every cluster, in
// kube-system/kubeadm-config, because it is how the cluster describes itself.
// Reading it there makes the repair a bundle upgrade and nothing else, and makes
// it self-healing: a load balancer replaced later is picked up on the next
// reconcile rather than frozen at whatever Day-0 saw once.
//
// kubeadm-config rather than the HetznerCluster, though both carry it: this is
// provider-neutral, needs no CAPI CRDs present, and cannot be confused with a
// SPOKE's address. A hub that has provisioned spokes holds several
// HetznerClusters -- acme-hub-gx2ps at 65.109.41.89 beside
// spoke-pool-hybrid-dev-01-mqcqk at 65.109.41.65 -- and selecting the wrong one
// publishes a spoke's address for every hub hostname: DNS resolves, the port
// answers, and it is the wrong cluster. kubeadm-config is about THIS cluster by
// definition, so that mistake is not available.
//
// The general rule: if the platform can read it, the platform reads it. Day-0
// carries only what is genuinely external -- a tailnet key, a provider token,
// the tenant's domain.
const (
	// IngressAddressConfigMap is where the resolved address is published for the
	// gateway chart to consume.
	//
	// A ConfigMap, not an annotation written onto the Gateway and its routes.
	// Those are owned by ArgoCD with selfHeal; an operator writing to them fights
	// the reconciler and loses within a sync -- the same lesson as the ArgoCD
	// credential and the Infisical upload. A ConfigMap is a value the chart
	// reads, so declared state keeps a single writer.
	IngressAddressConfigMap = "hub-ingress"
	IngressAddressKey       = "address"

	// IngressAddressNamespace is where the ConfigMap is written: the namespace of
	// the component that READS it, not the one this operator runs in.
	//
	// configMapKeyRef resolves in the POD's namespace. external-dns runs in
	// platform-edge; writing to platform-ops -- where this operator lives and
	// where every other object it manages sits -- put the value somewhere the
	// consumer could not see. With `optional: true` on the reference that was
	// silent: the container started cleanly, EXTERNAL_DNS_DEFAULT_TARGETS was
	// unset, external-dns logged "config: {... DefaultTargets:[] ...}" and then
	// "All records are already up to date" against an empty zone, and every hub
	// hostname stayed NXDOMAIN with nothing unhealthy anywhere.
	IngressAddressNamespace = "platform-edge"

	// ConditionIngressAddressResolved reports whether the address is known.
	//
	// False is not cosmetic: no hub hostname can be published. The bootstrap
	// waits on HubEnvironment conditions, so this surfaces as a phase failure
	// rather than a warning that scrolls past and is discovered from a browser.
	ConditionIngressAddressResolved = "IngressAddressResolved"

	kubeadmConfigMap       = "kubeadm-config"
	kubeadmConfigNamespace = "kube-system"
	kubeadmConfigKey       = "ClusterConfiguration"
)

// reconcileIngressAddress resolves the hub's public ingress address and
// publishes it, setting a condition either way.
func (r *HubEnvironmentReconciler) reconcileIngressAddress(
	ctx context.Context, hubEnv *opsv1alpha1.HubEnvironment,
) error {
	namespace := IngressAddressNamespace
	logger := ctrl.LoggerFrom(ctx)

	address, err := r.hubIngressAddress(ctx)
	if err != nil || address == "" {
		msg := "the hub's control-plane endpoint could not be read from " +
			kubeadmConfigNamespace + "/" + kubeadmConfigMap
		if err != nil {
			msg = fmt.Sprintf("%s: %v", msg, err)
		}
		meta.SetStatusCondition(&hubEnv.Status.Conditions, metav1.Condition{
			Type:               ConditionIngressAddressResolved,
			Status:             metav1.ConditionFalse,
			Reason:             "NotResolved",
			ObservedGeneration: hubEnv.Generation,
			Message: msg + ". No hub hostname can be published and ACME cannot " +
				"issue for any of them",
		})
		logger.Info("hub ingress address unresolved", "error", err)
		r.persistIngressCondition(ctx, hubEnv)
		return nil // reported as a condition, not a reconcile error
	}

	if err := r.publishIngressAddress(ctx, namespace, address); err != nil {
		return err
	}

	meta.SetStatusCondition(&hubEnv.Status.Conditions, metav1.Condition{
		Type:               ConditionIngressAddressResolved,
		Status:             metav1.ConditionTrue,
		Reason:             "Resolved",
		ObservedGeneration: hubEnv.Generation,
		Message:            fmt.Sprintf("hub ingress address is %s", address),
	})
	r.persistIngressCondition(ctx, hubEnv)
	return nil
}

// persistIngressCondition writes the condition now, rather than leaving it to a
// later phase's status update.
//
// This runs near the top of Reconcile, and three phases between here and the
// first Status().Update() can return early. On any of those paths the condition
// was computed and then dropped -- so the one signal that says "no hub hostname
// can be published" was the signal most likely to be lost, because the failures
// that trigger those early returns are exactly the ones worth reporting during.
//
// Failure to write is logged, not returned: a condition that cannot be recorded
// must not also stop the reconcile that was about to fix the thing it describes.
func (r *HubEnvironmentReconciler) persistIngressCondition(
	ctx context.Context, hubEnv *opsv1alpha1.HubEnvironment,
) {
	if err := r.Status().Update(ctx, hubEnv); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "could not record IngressAddressResolved")
	}
}

// hubIngressAddress reads the control-plane endpoint this cluster was built with.
func (r *HubEnvironmentReconciler) hubIngressAddress(ctx context.Context) (string, error) {
	var cm corev1.ConfigMap
	key := types.NamespacedName{Name: kubeadmConfigMap, Namespace: kubeadmConfigNamespace}
	if err := r.Get(ctx, key, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return "", fmt.Errorf("%s/%s does not exist; this cluster was not built by kubeadm",
				kubeadmConfigNamespace, kubeadmConfigMap)
		}
		return "", err
	}
	return controlPlaneHost(cm.Data[kubeadmConfigKey])
}

// controlPlaneHost extracts the host from kubeadm's controlPlaneEndpoint.
//
// Parsed as text rather than by unmarshalling ClusterConfiguration: that type
// lives in kubeadm's API packages, which pull in a large dependency tree for one
// string, and its shape has changed across the v1beta3/v1beta4 boundary this
// operator would then have to track.
//
// The port is dropped. The endpoint is host:port (65.109.41.89:6443) and what
// external-dns publishes is an A record, which has no port. An IPv6 endpoint is
// bracketed, so the last colon is the separator only outside the brackets.
func controlPlaneHost(clusterConfiguration string) (string, error) {
	const field = "controlPlaneEndpoint:"

	for _, line := range strings.Split(clusterConfiguration, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, field) {
			continue
		}
		endpoint := strings.TrimSpace(strings.TrimPrefix(trimmed, field))
		endpoint = strings.Trim(endpoint, `"'`)
		if endpoint == "" {
			return "", fmt.Errorf("controlPlaneEndpoint is empty")
		}

		if strings.HasPrefix(endpoint, "[") {
			if end := strings.Index(endpoint, "]"); end > 0 {
				return endpoint[1:end], nil
			}
			return "", fmt.Errorf("controlPlaneEndpoint %q has an unterminated IPv6 literal", endpoint)
		}
		if i := strings.LastIndex(endpoint, ":"); i > 0 {
			endpoint = endpoint[:i]
		}
		return endpoint, nil
	}
	return "", fmt.Errorf("ClusterConfiguration declares no controlPlaneEndpoint")
}

// publishIngressAddress writes the address where the gateway chart reads it.
func (r *HubEnvironmentReconciler) publishIngressAddress(
	ctx context.Context, namespace, address string,
) error {
	key := types.NamespacedName{Name: IngressAddressConfigMap, Namespace: namespace}

	var existing corev1.ConfigMap
	err := r.Get(ctx, key, &existing)
	switch {
	case apierrors.IsNotFound(err):
		return r.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      IngressAddressConfigMap,
				Namespace: namespace,
				Labels:    map[string]string{"app.kubernetes.io/managed-by": "hub-operator"},
				Annotations: map[string]string{
					"ops.nutgraf.in/description": "The hub's public ingress address, read from " +
						"kube-system/kubeadm-config. Written by hub-operator; do not edit.",
				},
			},
			Data: map[string]string{IngressAddressKey: address},
		})
	case err != nil:
		return err
	}

	if existing.Data[IngressAddressKey] == address {
		return nil // current; no write, no churn
	}
	if existing.Data == nil {
		existing.Data = map[string]string{}
	}
	existing.Data[IngressAddressKey] = address
	return r.Update(ctx, &existing)
}
