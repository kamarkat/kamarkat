package controller

import (
	"context"
	"fmt"
	"net/url"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	computev1alpha1 "github.com/kamarkat/kamarkat/operators/ephemeral-job-operator/api/v1alpha1"
)

// ADR-106 §4 / ADR-107: a session peer, and the isolation it always runs under.
//
// A peer (a renderer, for instance) is a second job in a session that loads the
// session's app. It must reach that one app and nothing else, and nothing but
// its owning job may reach it. Cilium cannot say "pods whose session label has
// the same value as mine", so the pairing is rendered per session, here, as two
// CiliumNetworkPolicies naming both pods by the operator's own UID label --
// which a submitter cannot forge, because the operator's labels win on the pods
// it authors (authoredLabels).
//
// The base posture for every peer is the platform's session-peer-default-deny
// (spoke-catalog): ingress from the node only, egress DNS only. These policies
// add exactly the pair's traffic on top of it.

const (
	// labelSessionPeer marks a peer's pod. The platform's session-peer
	// default-deny selects on it, so it is set by the operator and never left
	// to the submitter.
	labelSessionPeer = "compute.nutgraf.in/session-peer"

	// labelSandbox is what the sandbox policies select -- the platform's
	// default-deny and the application's declared external hosts
	// (sandbox-egress-declared). A peer must not carry it: Cilium allows are
	// additive, so a peer with both labels would inherit every host the
	// application declared for its sandboxes, where a peer may reach only its
	// session's app and storage.
	labelSandbox = "agents.x-k8s.io/sandbox"

	// sandboxedRuntimeClass is the platform's RuntimeClass for Isolation:
	// Sandboxed (ADR-107, spoke-catalog sandbox-runtime.yaml). The name is the
	// capability, not the implementation.
	sandboxedRuntimeClass = "sandboxed"
)

var ciliumNetworkPolicyGVK = schema.GroupVersionKind{Group: "cilium.io", Version: "v2", Kind: "CiliumNetworkPolicy"}

// NetworkPolicyMode is how a cluster enforces sandbox network policy.
type NetworkPolicyMode string

const (
	// NetworkPolicyCilium renders the per-job CiliumNetworkPolicies (storage
	// egress, peer pairs). A cluster without cilium.io/v2 then fails the job's
	// reconcile, loudly: it was declared to enforce and cannot.
	NetworkPolicyCilium NetworkPolicyMode = "cilium"
	// NetworkPolicyNone is a cluster that enforces no network policy at all (the
	// local profile). Nothing is rendered, because nothing would read it; every
	// other ADR-106/107 rule -- peer validation, limits, the sandboxed runtime --
	// still applies.
	NetworkPolicyNone NetworkPolicyMode = "none"
)

// ParseNetworkPolicyMode accepts exactly the two modes, and nothing by default.
func ParseNetworkPolicyMode(s string) (NetworkPolicyMode, error) {
	switch NetworkPolicyMode(s) {
	case NetworkPolicyCilium, NetworkPolicyNone:
		return NetworkPolicyMode(s), nil
	case "":
		return "", fmt.Errorf("--network-policy is required: cilium or none (ADR-106)")
	default:
		return "", fmt.Errorf("--network-policy=%q: must be cilium or none (ADR-106)", s)
	}
}

// runtimeClassFor is the RuntimeClass a job's pod runs under, or nil for the
// ordinary runtime. A peer is always Sandboxed, whatever it asked for.
func runtimeClassFor(ej *computev1alpha1.EphemeralJob) *string {
	if ej.Spec.Isolation == computev1alpha1.IsolationSandboxed || ej.Spec.Peer != nil {
		return ptr(sandboxedRuntimeClass)
	}
	return nil
}

func hasContainerPort(ports []corev1.ContainerPort, port int32) bool {
	return slices.ContainsFunc(ports, func(p corev1.ContainerPort) bool { return p.ContainerPort == port })
}

// reconcileSessionPeer validates a peer against its owning job and renders the
// pair's network policies. invalid is a request that can never work, and ends
// the job with reason/message; err is a transient failure to retry.
func (r *EphemeralJobReconciler) reconcileSessionPeer(
	ctx context.Context, ej *computev1alpha1.EphemeralJob,
) (reason, message string, invalid bool, err error) {
	peer := ej.Spec.Peer
	if peer == nil {
		return "", "", false, nil
	}
	if _, ok := ej.Labels[labelSandbox]; ok {
		return "InvalidPeer", fmt.Sprintf("a session peer must not carry the %s label: it would inherit the "+
			"external hosts declared for sandboxes, which a peer must not reach", labelSandbox), true, nil
	}
	if !hasContainerPort(ej.Spec.Ports, peer.ListenPort) {
		return "InvalidPeer", fmt.Sprintf("spec.peer.listenPort %d is not one of this job's spec.ports", peer.ListenPort), true, nil
	}

	var owner computev1alpha1.EphemeralJob
	if err := r.Get(ctx, client.ObjectKey{Namespace: ej.Namespace, Name: peer.JobName}, &owner); err != nil {
		if apierrors.IsNotFound(err) {
			return "PeerOwnerNotFound", fmt.Sprintf("spec.peer.jobName %q is not an EphemeralJob in %s",
				peer.JobName, ej.Namespace), true, nil
		}
		return "", "", false, err
	}
	switch {
	case owner.Spec.Peer != nil:
		return "InvalidPeer", fmt.Sprintf("%q is itself a session peer; a peer must belong to the session's own job", owner.Name), true, nil
	case isTerminal(owner.Status.Phase):
		return "PeerOwnerFinished", fmt.Sprintf("%q has finished (%s)", owner.Name, owner.Status.Phase), true, nil
	case !hasContainerPort(owner.Spec.Ports, peer.TargetPort):
		return "InvalidPeer", fmt.Sprintf("spec.peer.targetPort %d is not one of %q's spec.ports", peer.TargetPort, owner.Name), true, nil
	}

	storageHost, err := r.sessionStorageHost(ctx, ej.Namespace)
	if err != nil {
		return "", "", false, err
	}

	if r.NetworkPolicy != NetworkPolicyCilium {
		return "", "", false, nil
	}
	for _, p := range sessionPeerPolicies(ej, &owner, storageHost) {
		if err := controllerutil.SetControllerReference(ej, p, r.Scheme); err != nil {
			return "", "", false, err
		}
		if err := r.applyPolicy(ctx, p); err != nil {
			return "", "", false, err
		}
	}
	return "", "", false, nil
}

// sessionStorageHost is the host of the session's object storage (ADR-052): the
// one external destination a peer may reach. Read from the same fleet ConfigMap
// the workload's S3 environment comes from, so the policy and the workload can
// never disagree about where storage is.
func (r *EphemeralJobReconciler) sessionStorageHost(ctx context.Context, namespace string) (string, error) {
	name := resolveWorkspaceSyncConfig(namespace, r.resolveFleetID(ctx, namespace))
	var cm corev1.ConfigMap
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &cm); err != nil {
		return "", fmt.Errorf("reading %s/%s for the session's storage endpoint: %w", namespace, name, err)
	}
	raw := cm.Data["S3_ENDPOINT_URL"]
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return "", fmt.Errorf("%s/%s S3_ENDPOINT_URL %q has no host", namespace, name, raw)
	}
	return u.Hostname(), nil
}

// sessionPeerPolicies is the pair, as two policies: one selecting the peer, one
// selecting its owning job. Each states both directions of its own pod's side.
func sessionPeerPolicies(peer, owner *computev1alpha1.EphemeralJob, storageHost string) []*unstructured.Unstructured {
	peerPod := map[string]any{"matchLabels": map[string]any{labelJobUID: string(peer.UID)}}
	ownerPod := map[string]any{"matchLabels": map[string]any{labelJobUID: string(owner.UID)}}
	tcp := func(port int32) []any {
		return []any{map[string]any{"ports": []any{map[string]any{"port": fmt.Sprint(port), "protocol": "TCP"}}}}
	}
	listen, target := peer.Spec.Peer.ListenPort, peer.Spec.Peer.TargetPort

	// The peer: its owner in on the listen port; out to its owner's app port
	// and to the session's storage by name. DNS -- with the L7 rule that makes
	// the name resolvable for toFQDNs -- comes from session-peer-default-deny.
	peerSide := map[string]any{
		"endpointSelector": peerPod,
		"ingress":          []any{map[string]any{"fromEndpoints": []any{ownerPod}, "toPorts": tcp(listen)}},
		"egress": []any{
			map[string]any{"toEndpoints": []any{ownerPod}, "toPorts": tcp(target)},
			map[string]any{"toFQDNs": []any{map[string]any{"matchName": storageHost}}, "toPorts": tcp(443)},
		},
	}
	// The owner: its peer in on the app port; out to its peer's listen port.
	ownerSide := map[string]any{
		"endpointSelector": ownerPod,
		"ingress":          []any{map[string]any{"fromEndpoints": []any{peerPod}, "toPorts": tcp(target)}},
		"egress":           []any{map[string]any{"toEndpoints": []any{peerPod}, "toPorts": tcp(listen)}},
	}

	mk := func(suffix string, spec map[string]any) *unstructured.Unstructured {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(ciliumNetworkPolicyGVK)
		u.SetNamespace(peer.Namespace)
		u.SetName(peer.Name + suffix)
		u.SetLabels(map[string]string{labelJobUID: string(peer.UID), labelJobName: peer.Name})
		u.Object["spec"] = spec
		return u
	}
	return []*unstructured.Unstructured{mk("-peer", peerSide), mk("-peer-owner", ownerSide)}
}

// reconcileStorageEgress grants a job's pod its session object storage, by
// name, on :443 (ADR-106 §1).
//
// A sandbox has no other route to the internet: the platform's sandbox policy
// grants DNS and the pooler, the application's declared hosts are rendered by
// its chart, and storage is rendered here because only the operator knows
// which jobs receive storage credentials -- and the host is read from the very
// ConfigMap those credentials' endpoint comes from, so the policy and the
// workload cannot disagree. A peer's storage grant is part of its pair policy.
func (r *EphemeralJobReconciler) reconcileStorageEgress(ctx context.Context, ej *computev1alpha1.EphemeralJob) error {
	if r.NetworkPolicy != NetworkPolicyCilium || ej.Spec.Peer != nil ||
		(ej.Spec.Output == nil && ej.Spec.WorkspacePersistence == nil) {
		return nil
	}
	host, err := r.sessionStorageHost(ctx, ej.Namespace)
	if err != nil {
		return err
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(ciliumNetworkPolicyGVK)
	u.SetNamespace(ej.Namespace)
	u.SetName(ej.Name + "-storage")
	u.SetLabels(map[string]string{labelJobUID: string(ej.UID), labelJobName: ej.Name})
	u.Object["spec"] = map[string]any{
		"endpointSelector": map[string]any{"matchLabels": map[string]any{labelJobUID: string(ej.UID)}},
		"egress": []any{map[string]any{
			"toFQDNs": []any{map[string]any{"matchName": host}},
			"toPorts": []any{map[string]any{"ports": []any{map[string]any{"port": "443", "protocol": "TCP"}}}},
		}},
	}
	if err := controllerutil.SetControllerReference(ej, u, r.Scheme); err != nil {
		return err
	}
	return r.applyPolicy(ctx, u)
}

// applyPolicy creates the policy, or replaces it when its spec or labels differ.
// Both are wholly the operator's, so replacement is correct; an unchanged policy
// is left alone so a reconcile does not churn the agents' policy maps.
func (r *EphemeralJobReconciler) applyPolicy(ctx context.Context, want *unstructured.Unstructured) error {
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(ciliumNetworkPolicyGVK)
	err := r.Get(ctx, client.ObjectKeyFromObject(want), got)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, want)
	}
	if err != nil {
		return err
	}
	if equality.Semantic.DeepEqual(got.Object["spec"], want.Object["spec"]) &&
		equality.Semantic.DeepEqual(got.GetLabels(), want.GetLabels()) {
		return nil
	}
	want.SetResourceVersion(got.GetResourceVersion())
	return r.Update(ctx, want)
}
