package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	computev1alpha1 "github.com/kamarkat/kamarkat/operators/ephemeral-job-operator/api/v1alpha1"
)

// CapacityState is what the operator could determine about why a pod is not
// running, and whether waiting will help.
//
// ADR-052 §12 requires these three to be distinguishable and to not share a
// code path. The distinction that matters most is the first: a quota rejection
// means the Pod was never created, so a caller that treats it as "still
// pending" waits out its entire cold-start budget before reporting a condition
// that was knowable at admission.
type CapacityState struct {
	Reason   string
	Message  string
	Terminal bool // waiting will not change the outcome
	Ready    bool
}

// Event reasons emitted by cluster-autoscaler onto a pending Pod. These are
// namespaced objects, which is what makes ADR-052 §12's contract satisfiable
// without granting a fleet — or this operator — cluster-scoped Node read.
const (
	eventTriggeredScaleUp  = "TriggeredScaleUp"
	eventNotTriggerScaleUp = "NotTriggerScaleUp"
	eventFailedScheduling  = "FailedScheduling"
)

// AssessCapacity determines, from namespaced objects only, why the job's pod is
// not yet running.
//
// It deliberately reads the Pod and its Events and nothing else. Reading Nodes
// or the MachineDeployment would be simpler and is refused: this operator runs
// in the platform's namespace but the contract it implements is the one a fleet
// must be able to satisfy, and building it on cluster-scoped reads would make
// the fleet-facing half unimplementable.
func AssessCapacity(ctx context.Context, c client.Client, nodes client.Reader, ns, jobName string) (CapacityState, error) {
	// batch/v1 stamps job-name onto the pods it creates.
	return AssessCapacityBySelector(ctx, c, nodes, ns, client.MatchingLabels{"job-name": jobName})
}

// AssessCapacityBySelector is the same assessment for a pod this operator owns
// directly (Service mode), which carries no job-name because no Job created it.
// The capacity question is identical either way — a pod is pending, and the
// reason is on the pod and its events — so the logic must not be duplicated.
func AssessCapacityBySelector(
	ctx context.Context, c client.Client, nodes client.Reader, ns string, sel client.MatchingLabels,
) (CapacityState, error) {
	var pods corev1.PodList
	if err := c.List(ctx, &pods, client.InNamespace(ns), sel); err != nil {
		return CapacityState{}, fmt.Errorf("listing pods in %s for %v: %w", ns, map[string]string(sel), err)
	}

	// No Pod yet. The Job controller creates it promptly, so persistent absence
	// means admission refused it — most commonly the fleet's priority-scoped
	// ResourceQuota (ADR-052 §5). The Job's own failure condition carries the
	// reason; the caller supplies it via CheckQuotaRejection.
	if len(pods.Items) == 0 {
		return CapacityState{
			Reason:  computev1alpha1.ReasonWaitingForCapacity,
			Message: "pod not yet created",
		}, nil
	}

	pod := pods.Items[0]

	if pod.Status.Phase == corev1.PodRunning || pod.Status.Phase == corev1.PodSucceeded {
		return CapacityState{Reason: computev1alpha1.ReasonScheduled, Ready: true}, nil
	}

	scheduled := podCondition(&pod, corev1.PodScheduled)
	if scheduled != nil && scheduled.Status == corev1.ConditionTrue {
		return CapacityState{Reason: computev1alpha1.ReasonScheduled, Ready: true}, nil
	}

	// Unschedulable. The autoscaler's own events on this Pod say whether that is
	// a transient state (a node is coming) or a bound (maxNodes reached).
	var events corev1.EventList
	if err := c.List(ctx, &events, client.InNamespace(ns),
		client.MatchingFields{"involvedObject.name": pod.Name}); err != nil {
		// Event read is best-effort: without it we still know the pod is
		// unschedulable, we just cannot say why. Reporting "waiting" is the
		// safe default — it does not delete anything (ADR-052 §11 rule 3).
		return CapacityState{
			Reason:  computev1alpha1.ReasonWaitingForCapacity,
			Message: unschedulableMessage(scheduled),
		}, nil
	}

	for _, e := range events.Items {
		switch e.Reason {
		case eventTriggeredScaleUp:
			return CapacityState{
				Reason:  computev1alpha1.ReasonWaitingForCapacity,
				Message: "burst capacity is being provisioned: " + e.Message,
			}, nil
		case eventNotTriggerScaleUp:
			// maxNodes reached, or no node group fits this pod's envelope.
			// Terminal for now — and explicitly NOT a reason to delete and
			// recreate, which would only re-enter the same state having lost
			// the pod's place (ADR-052 §11 rule 3).
			return CapacityState{
				Reason:   computev1alpha1.ReasonCapacityUnavailable,
				Message:  "burst capacity ceiling reached: " + e.Message,
				Terminal: true,
			}, nil
		}
	}

	// The autoscaler said nothing at all, and that means two different things.
	//
	// For a class it manages, silence is transient: it has not reacted yet, and
	// waiting is right. For a class it does not manage — home-lab hardware is
	// fixed, nothing provisions it — silence is permanent, and so is the pod's
	// Pending.
	//
	// The cluster itself distinguishes them. If some node already carries every
	// label this pod requires, the pod is queued behind resources and will run.
	// If none does, the selector names a node that does not exist, and no event
	// is coming to say so.
	if none, err := noNodeMatchesSelector(ctx, nodes, &pod); err == nil && none {
		return CapacityState{
			Reason:   computev1alpha1.ReasonPlacementUnsatisfiable,
			Message:  unschedulableMessage(scheduled),
			Terminal: true,
		}, nil
	}

	return CapacityState{
		Reason:  computev1alpha1.ReasonWaitingForCapacity,
		Message: unschedulableMessage(scheduled),
	}, nil
}

// noNodeMatchesSelector reports whether the cluster holds no node carrying every
// label in the pod's nodeSelector.
//
// Deliberately narrow. It answers only "do these labels exist anywhere", not
// "would this pod schedule" — taints, resources, affinity and topology are the
// scheduler's business and are why the pod may still be Pending when this
// returns false. A false answer therefore means "keep waiting", which is the
// safe direction: the only thing this promotes to terminal is a selector no node
// can satisfy no matter what else changes.
//
// An error is not a verdict. A node list that fails leaves the caller reporting
// WaitingForCapacity, exactly as before.
func noNodeMatchesSelector(ctx context.Context, r client.Reader, pod *corev1.Pod) (bool, error) {
	if len(pod.Spec.NodeSelector) == 0 {
		return false, nil
	}
	// UNCACHED, for the same reason the Event read is (see APIReader): a cached
	// List starts an informer, and this controller would then watch every Node
	// in the cluster to answer a question it asks only when a pod will not
	// schedule. The first version used the cache-backed client and the operator
	// stopped reconciling entirely -- the informer could not start without
	// cluster-scoped node RBAC, and a cache that cannot sync blocks every
	// controller behind it, not just the read that caused it.
	var nodeList corev1.NodeList
	if err := r.List(ctx, &nodeList); err != nil {
		return false, err
	}
	for i := range nodeList.Items {
		labels := nodeList.Items[i].Labels
		match := true
		for k, v := range pod.Spec.NodeSelector {
			if labels[k] != v {
				match = false
				break
			}
		}
		if match {
			return false, nil
		}
	}
	return true, nil
}

func unschedulableMessage(c *corev1.PodCondition) string {
	if c == nil {
		return "pod is not yet scheduled"
	}
	if c.Message != "" {
		return c.Message
	}
	return string(c.Reason)
}

func podCondition(p *corev1.Pod, t corev1.PodConditionType) *corev1.PodCondition {
	for i := range p.Status.Conditions {
		if p.Status.Conditions[i].Type == t {
			return &p.Status.Conditions[i]
		}
	}
	return nil
}
