package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	computev1alpha1 "github.com/kamarkat/kamarkat/operators/ephemeral-job-operator/api/v1alpha1"
)

// EphemeralJobReconciler owns the job lifecycle and nothing else (ADR-052 §7).
//
// What it is forbidden to do, per the ADR-041 matrix entry, is worth stating
// where the code is rather than only in the ADR: it provisions no
// infrastructure, never creates or patches MachineDeployment replicas, holds no
// provider credential, and authors no placement *policy*. It writes placement
// fields into a Job it owns, which is a different thing — the values come from
// placement.go, which is platform code in this repo, not from the request.
type EphemeralJobReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// APIReader reads straight from the API server, bypassing the manager's
	// cache. It exists for one job: reading a Job's Events. Events are numerous
	// and short-lived, and a cached read would make this controller maintain an
	// informer over every Event in the cluster to answer a question it asks only
	// when a job looks stuck.
	APIReader client.Reader

	// Clientset reads pod logs, which a controller-runtime client cannot: a log is a
	// subresource request. Used only to explain a failure, so it is optional -- a nil
	// one costs the log tail and nothing else.
	Clientset kubernetes.Interface

	// ProvisioningBudget is the p95 cold-start budget for the placement class
	// (ADR-052 §11). A job that has been waiting for capacity longer than this
	// is reported as such — but it is NOT deleted, because deletion during
	// provisioning is indistinguishable to the autoscaler from deletion of a
	// healthy workload and causes the scale-up/scale-down thrash §11 describes.
	ProvisioningBudget time.Duration

	// NetworkPolicy is how this cluster enforces sandbox network policy
	// (ADR-106). Stated by the install, never detected: see ParseNetworkPolicyMode.
	NetworkPolicy NetworkPolicyMode
}

const (
	jobOwnerKey   = ".metadata.controller"
	finalizerName = "compute.nutgraf.in/ephemeraljob"
	labelJobUID   = "compute.nutgraf.in/ephemeraljob-uid"
	// The CR's NAME, carried on the pod so a consumer that knows only the
	// request — the SDK knows a sandbox by name, not by UID — can find its
	// compute without the pod having to be named after it.
	labelJobName   = "compute.nutgraf.in/ephemeraljob-name"
	defaultRequeue = 10 * time.Second

	// Bounded so a slow or hanging receiver cannot stall the work queue for
	// every other job. The callback is a notification, not a transaction.
	callbackTimeout = 10 * time.Second

	// The workload container's name, which readiness is judged on.
	workloadContainerName = "workload"

	// Bounds on the failure log tail. All three exist because this read happens
	// inside Reconcile: the line and byte caps keep a verbose workload out of a
	// status field, and the timeout keeps a slow read from stalling the work queue
	// for every other job.
	logTailLines   = 20
	logTailBytes   = 2048
	logTailTimeout = 5 * time.Second

	// Where spec.input reaches the workload. The field is opaque to this
	// operator: it is passed through verbatim, never read, never reshaped.
	// Without this the field was accepted, stored, and then dropped — a
	// workload started with no idea what it had been asked to do.
	envJobInput = "EPHEMERAL_JOB_INPUT"

	// Where spec.output reaches the workload. A workload writes its own result,
	// so it has to be told where — the field said so and nothing carried it,
	// which left results landing at the bucket root.
	envJobOutput = "EPHEMERAL_JOB_OUTPUT"

	// The envelope a request gets when it names none. See the comment at the
	// assignment for why an absent value cannot be left absent.
	defaultRequestCPU    = "2"
	defaultRequestMemory = "4Gi"

	// Sidecars get their own, much smaller floor. A sidecar is auxiliary by
	// definition — a credential broker, a log shipper — so giving it the
	// workload's envelope would multiply a sandbox's quota footprint several
	// times over for containers that idle.
	defaultSidecarRequestCPU    = "50m"
	defaultSidecarRequestMemory = "64Mi"

	// ADR-106: the bound a container gets when it states none. A sandbox runs
	// tenant-authored code, and with requests alone one runaway session could
	// take a whole node from every other session on it.
	//
	// These are DEFAULTS, not a ceiling: a request that states a limit keeps
	// it, and a limit is never set below the container's own request. No
	// ceiling is enforced until the heaviest workload (video export) has been
	// measured -- a ceiling below it would kill that workload, not bound it.
	defaultLimitCPU              = "4"
	defaultLimitMemory           = "8Gi"
	defaultLimitEphemeralStorage = "20Gi"

	defaultSidecarLimitCPU              = "1"
	defaultSidecarLimitMemory           = "1Gi"
	defaultSidecarLimitEphemeralStorage = "1Gi"
)

// callbackHTTP is shared so connections are reused across reconciles rather
// than a new transport being built per callback.
var callbackHTTP = &http.Client{Timeout: callbackTimeout}

func (r *EphemeralJobReconciler) callbackClient() *http.Client { return callbackHTTP }

// +kubebuilder:rbac:groups=compute.nutgraf.in,resources=ephemeraljobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=compute.nutgraf.in,resources=ephemeraljobs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=compute.nutgraf.in,resources=ephemeraljobs/finalizers,verbs=update
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;delete
// Read-only, and only to explain a failure. The pod is deleted with its Job, so the
// terminal callback is the last moment the workload's own output can be captured --
// without this the message says FAILED and nothing else, and a caller cannot tell a
// bad input from an infrastructure fault.
// +kubebuilder:rbac:groups="",resources=pods/log,verbs=get
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;delete
// Nodes, list only, and read UNCACHED (see APIReader). Needed to tell a pod
// waiting for capacity from one whose selector no node can satisfy; without it
// the read fails and every such job waits out maxLifetimeSeconds instead.
// +kubebuilder:rbac:groups="",resources=nodes,verbs=list
// Namespaces, GET only, and read UNCACHED (see APIReader). A fleet's S3 Secret
// and ConfigMap are named from its namespace's `app-id` label, so that label has
// to be read; without this the read fails and the reference renders incomplete.
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get
// +kubebuilder:rbac:groups="",resources=events,verbs=get;list;watch;create;patch
// Workspace PVCs (ADR-052 §14). No `delete`: this operator creates and reads
// them, and reclaiming one is the separate reaper's job. Withholding the verb
// means a bug in the hot path cannot delete a user's workspace.
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get
// +kubebuilder:rbac:groups=cilium.io,resources=ciliumnetworkpolicies,verbs=get;list;watch;create;update

func (r *EphemeralJobReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx)

	var ej computev1alpha1.EphemeralJob
	if err := r.Get(ctx, req.NamespacedName, &ej); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Deletion needs nothing from this operator (ADR-052 §14).
	//
	// There was a workspace-flush finalizer here that POSTed to the sidecar to
	// take a final checkpoint. It was wrong three times over and is gone:
	//
	//   - it dialled the pod IP while the sidecar binds 127.0.0.1, so the
	//     request could never connect from another pod;
	//   - it ran on CR deletion, but the pod is reaped at idle timeout long
	//     before anything deletes the CR, so by then there was no sidecar left
	//     to ask — the lifetimes being coordinated were never the same one;
	//   - and it held deletion open on a call that could not succeed.
	//
	// The final checkpoint now happens where the state actually lives: the
	// sidecar takes it on SIGTERM, which the kubelet sends only after the
	// workload containers have exited. The operator owns the workload's
	// lifecycle; the workload owns the durability of its own state.
	if !ej.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	if isTerminal(ej.Status.Phase) {
		// Cancelling something already terminal is a no-op (ADR-052 §16.4). A
		// result that happened is not undone by a later cancel, and overwriting
		// the terminal phase would lose why it actually ended.
		return r.reconcileTTL(ctx, &ej)
	}

	// Cancellation, before any provisioning work (ADR-052 §16.4).
	//
	// Checked here so it resolves from ANY non-terminal phase with one rule.
	//
	// This used to defer cancellation while the phase was Checkpointing, to
	// avoid interrupting an in-flight workspace flush. Two reasons that is
	// gone: nothing ever assigned that phase, so the branch was unreachable,
	// and the flush it protected no longer exists. Cancelling now deletes the
	// pod, the kubelet SIGTERMs the sidecar after the workload exits, and the
	// final checkpoint is taken on the way out like any other teardown.
	if ej.Spec.Cancelled {
		l.Info("cancelling on request", "name", ej.Name, "phase", ej.Status.Phase)
		return ctrl.Result{}, r.markTerminal(ctx, &ej, computev1alpha1.PhaseCancelled,
			"Cancelled", "cancelled by the submitter")
	}

	// Reject an unrunnable request once, here, instead of letting it fail later
	// somewhere that does not say why. Both checks below describe pods the API
	// server would refuse to create, and refusing them at submission is the
	// difference between a submitter seeing the reason and a CR that silently
	// never produces a pod.
	if reason, msg, invalid := validateSpec(&ej); invalid {
		return ctrl.Result{}, r.markTerminal(ctx, &ej, computev1alpha1.PhaseFailed, reason, msg)
	}

	// Before any pod exists: a peer's policies must be in place by the time it
	// can send a packet, or its first connections race the policy.
	if reason, msg, invalid, err := r.reconcileSessionPeer(ctx, &ej); err != nil {
		return ctrl.Result{}, err
	} else if invalid {
		return ctrl.Result{}, r.markTerminal(ctx, &ej, computev1alpha1.PhaseFailed, reason, msg)
	}

	// Service mode has no Job and no completion. It is a different lifecycle over
	// the SAME pod description, so it branches here rather than inside every step
	// below.
	//
	// IT DOES FIRE CALLBACKS, and this comment said otherwise until 2026-10-02.
	// Service mode notifies the caller twice: once when the workload starts
	// serving, and once when it ends -- pod failure, pod exit, the never-ready
	// bound, idle reap or timeout. Both are supported contract (ADR-052 §16.2).
	//
	// What it does NOT notify is cancellation or deletion: `spec.cancelled` reaches
	// a terminal phase through markTerminal, which fires nothing, and a deleted
	// EphemeralJob is left to garbage collection. Both are caller-initiated, so the
	// caller already knows.
	// Both modes, before any pod exists, so a pod's first packet already has
	// its storage (ADR-106 §1). Sandboxes are Service mode.
	if err := r.reconcileStorageEgress(ctx, &ej); err != nil {
		return ctrl.Result{}, err
	}

	if ej.Spec.Mode == computev1alpha1.ModeService {
		return r.reconcileServiceMode(ctx, &ej)
	}

	// The absolute bound, checked before anything that can stall.
	//
	// Service mode has always checked it; Job mode did not, so a request whose
	// Job could never be created retried for as long as the CR existed. One sat
	// for 45 hours with no phase, its workflow step waiting on a callback that
	// nothing was ever going to send.
	if ej.Spec.MaxLifetimeSeconds > 0 {
		if lived := time.Since(ej.CreationTimestamp.Time); lived > time.Duration(ej.Spec.MaxLifetimeSeconds)*time.Second {
			l.Info("reaping job at its lifetime bound", "ephemeralJob", req.NamespacedName, "lived", lived.Truncate(time.Second))
			if err := r.deleteJob(ctx, &ej); err != nil {
				return ctrl.Result{}, err
			}
			ej.Status.Message = fmt.Sprintf("reached the %ds maximum lifetime without finishing", ej.Spec.MaxLifetimeSeconds)
			return ctrl.Result{}, r.markFinished(ctx, &ej, computev1alpha1.PhaseTimedOut, nil)
		}
	}

	job, err := r.ensureJob(ctx, &ej)
	if err != nil {
		// Refused at admission: a policy, Pod Security or schema said no. The
		// same Job is refused the same way on every retry, so retrying only
		// hides the reason in this operator's log. Fail the request with the
		// API server's own message and tell the caller, which is what a
		// workflow waiting on this job needs.
		if jobRefusedAtAdmission(err) {
			ej.Status.Message = err.Error()
			return ctrl.Result{}, r.markFinishedWithReason(ctx, &ej, computev1alpha1.PhaseFailed,
				computev1alpha1.ReasonAdmissionRejected, nil)
		}
		return ctrl.Result{}, err
	}
	if job == nil {
		// A finished Job of the same name is being deleted so this request can
		// have a fresh one. Creating it now would race the collection and hit
		// AlreadyExists, so the work resumes on the next pass.
		return ctrl.Result{RequeueAfter: defaultRequeue}, nil
	}

	// A Job whose pod cannot be admitted never produces a pod. The Job's own
	// failure condition carries the reason, and a ResourceQuota rejection is
	// the expected one at volume (ADR-052 §Consequences). It is terminal: the
	// fleet's burst quota is full, and retrying cannot change that.
	if reason, msg, rejected := jobAdmissionRejected(job); rejected {
		return ctrl.Result{}, r.markTerminal(ctx, &ej, computev1alpha1.PhaseFailed,
			computev1alpha1.ReasonQuotaRejected, fmt.Sprintf("%s: %s", reason, msg))
	}

	// The same outcome by the other route. The check above sees a Job the
	// controller gave up on; this sees one it will retry forever because each
	// pod is refused before it exists. Both mean no pod will run, and neither
	// resolves by waiting — but only this one is reached in practice, because a
	// refused create sets no condition for the check above to read.
	if reason, msg, blocked := jobPodCreationBlocked(ctx, r.APIReader, job); blocked {
		return ctrl.Result{}, r.markTerminal(ctx, &ej, computev1alpha1.PhaseFailed,
			computev1alpha1.ReasonQuotaRejected, fmt.Sprintf("%s: %s", reason, msg))
	}

	if done, phase, exit, detail := jobFinished(job); done {
		// WHY IT FAILED, while the pod still exists to say so.
		//
		// The pod is deleted with the Job, so this is the only moment the reason is
		// available anywhere: a caller investigating later finds neither the pod nor
		// its logs. A failed render reported as FAILED with nothing after the colon
		// is what this replaces.
		if phase != computev1alpha1.PhaseSucceeded {
			if d, code := r.failureDetail(ctx, job, detail); d != "" {
				ej.Status.Message = d
				if exit == nil {
					exit = code
				}
			}
		}
		return ctrl.Result{}, r.markFinished(ctx, &ej, phase, exit)
	}

	cap, err := AssessCapacity(ctx, r.Client, r.APIReader, ej.Namespace, job.Name)
	if err != nil {
		return ctrl.Result{}, err
	}

	switch {
	case cap.Ready:
		// The workload clock starts when the pod runs, not when the request was
		// created: time spent waiting for a node is not the workload's time
		// (ADR-052 §11).
		if ej.Status.StartTime == nil {
			now := metav1.Now()
			ej.Status.StartTime = &now
			if err := r.Status().Update(ctx, &ej); err != nil {
				return ctrl.Result{}, client.IgnoreNotFound(err)
			}
		}
		// Workload timeout, measured from first execution. Deleting the Job is
		// what actually stops the workload; unlike a deletion during
		// provisioning (§11 rule 3) this one is correct, because the pod is
		// running and the budget it was given is spent.
		if ran := time.Since(ej.Status.StartTime.Time); ran > time.Duration(ej.Spec.TimeoutSeconds)*time.Second {
			l.Info("workload exceeded its execution budget", "ephemeralJob", req.NamespacedName, "ran", ran)
			policy := metav1.DeletePropagationBackground
			if err := r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &policy}); err != nil &&
				!apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
			ej.Status.Message = fmt.Sprintf("workload ran %s, exceeding its %ds execution budget",
				ran.Truncate(time.Second), ej.Spec.TimeoutSeconds)
			return ctrl.Result{}, r.markFinished(ctx, &ej, computev1alpha1.PhaseTimedOut, nil)
		}
		r.setPhase(&ej, computev1alpha1.PhaseRunning, cap)

	case cap.Reason == computev1alpha1.ReasonPlacementUnsatisfiable:
		// FAILS, where capacity waits.
		//
		// ADR-052 §11 rule 3 forbids deleting a job for want of capacity, and
		// rightly: a burst node is minutes away and deleting would destroy work
		// about to run while losing the pod's place in the queue. This is not
		// that. No node carries the labels this pod requires and none is being
		// provisioned, so every additional second is spent waiting for
		// something that is not coming.
		//
		// Left in Provisioning it is invisible in the only way that matters:
		// the caller's step does not fail, it hangs, and the three bounds that
		// exist do not apply -- readinessDeadlineSeconds starts when the pod
		// RUNS, idleTimeoutSeconds when the workload first SERVES, and
		// maxLifetimeSeconds is eight hours away.
		//
		// The pod is deleted with the job so the selector cannot be mistaken
		// for a pod that might still schedule; the scheduler's own message is
		// kept, because it names the mismatch precisely and is what an operator
		// acts on.
		l.Info("placement unsatisfiable", "ephemeralJob", req.NamespacedName, "message", cap.Message)
		if pod, err := r.podForJob(ctx, job); err == nil && pod != nil {
			if err := r.deletePod(ctx, pod); err != nil {
				return ctrl.Result{}, err
			}
		}
		ej.Status.Message = cap.Message
		return ctrl.Result{}, r.markFinished(ctx, &ej, computev1alpha1.PhaseFailed, nil)

	default:
		if ej.Spec.CapacityWaitSeconds != nil && *ej.Spec.CapacityWaitSeconds > 0 {
			if waited := time.Since(ej.CreationTimestamp.Time); waited > time.Duration(*ej.Spec.CapacityWaitSeconds)*time.Second {
				l.Info("capacity wait deadline exceeded", "ephemeralJob", req.NamespacedName, "waited", waited)
				policy := metav1.DeletePropagationBackground
				if err := r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &policy}); err != nil &&
					!apierrors.IsNotFound(err) {
					return ctrl.Result{}, err
				}
				ej.Status.Message = fmt.Sprintf("capacity wait deadline exceeded: waiting %s for capacity, over %ds limit: %s",
					waited.Truncate(time.Second), *ej.Spec.CapacityWaitSeconds, cap.Message)
				meta_SetStatusCondition(&ej.Status.Conditions, metav1.Condition{
					Type:               computev1alpha1.ConditionCapacity,
					Status:             metav1.ConditionFalse,
					Reason:             computev1alpha1.ReasonCapacityTimeout,
					Message:            ej.Status.Message,
					ObservedGeneration: ej.Generation,
				})
				return ctrl.Result{}, r.markFinishedWithReason(ctx, &ej, computev1alpha1.PhaseTimedOut, computev1alpha1.ReasonCapacityTimeout, nil)
			}
		}

		if cap.Terminal {
			r.setPhase(&ej, computev1alpha1.PhaseProvisioning, cap)
			l.Info("burst capacity unavailable", "ephemeralJob", req.NamespacedName, "message", cap.Message)
			break
		}

		r.setPhase(&ej, computev1alpha1.PhaseProvisioning, cap)
		// A container that cannot start looks identical to capacity that has not
		// arrived: both sit in Provisioning. Reported first, because it is the
		// one of the two that will never resolve on its own.
		if pod, err := r.podForJob(ctx, job); err == nil && pod != nil {
			if blocker := podStartupBlocker(pod); blocker != "" {
				ej.Status.Message = blocker
				break
			}
		}
		if waited := time.Since(ej.CreationTimestamp.Time); waited > r.ProvisioningBudget {
			// Over budget is reported, never acted on by deletion. See §11.
			ej.Status.Message = fmt.Sprintf(
				"waiting %s for burst capacity, over the %s budget for this placement class: %s",
				waited.Truncate(time.Second), r.ProvisioningBudget, cap.Message)
		}
	}

	if err := r.Status().Update(ctx, &ej); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	return ctrl.Result{RequeueAfter: defaultRequeue}, nil
}

// ensureJob creates the batch/v1 Job for this request if it does not exist.
//
// The Job is named for and owned by the EphemeralJob, so garbage collection is
// structural. The predecessor implementation swept for orphaned compute on a
// timer because it provisioned VMs that nothing owned; owner references remove
// the need for that entirely.
// jobIsFinished reports whether a Job has reached a terminal condition and can
// therefore never run anything again.
//
// Conditions, not counters: `Succeeded`/`Failed` counts describe pods, and a
// Job whose pods have all terminated may still be retried by the controller
// while its backoff budget lasts. `JobComplete` and `JobFailed` are the two
// states from which nothing further happens.
func jobIsFinished(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if (c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed) && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func (r *EphemeralJobReconciler) ensureJob(ctx context.Context, ej *computev1alpha1.EphemeralJob) (*batchv1.Job, error) {
	name := jobNameFor(ej)

	var existing batchv1.Job
	err := r.Get(ctx, client.ObjectKey{Namespace: ej.Namespace, Name: name}, &existing)
	if err == nil {
		// Reuse only a Job that can still do the work.
		//
		// The name is derived from the request, so a new EphemeralJob for the
		// same logical work finds the PREVIOUS Job sitting there — and if that
		// one is finished, adopting it hands back compute that has already
		// exited. Observed live: a sandbox whose `agent-vault` container was
		// OOM-killed two days earlier was adopted by a fresh request, the CR
		// went `Failed` within seconds with "container agent-vault terminated",
		// and every wake for that sandbox failed the same way until the Job was
		// deleted by hand. The sandbox was unrecoverable through the product.
		//
		// Idempotency is still the point — a Job that is running or pending is
		// returned untouched, so a retry never creates a second one. What
		// changes is that "exists" is no longer mistaken for "usable".
		if !jobIsFinished(&existing) {
			return &existing, nil
		}
		// A finished Job THIS CR owns is this request's own completed work, not
		// a leftover from an earlier one, and must be handed back rather than
		// replaced.
		//
		// The replacement above is for a NEW EphemeralJob that finds a previous
		// request's Job under the same derived name. It cannot tell the two
		// apart by name, because the name is derived from the request — but it
		// can by ownership, since every Job this controller creates is owned by
		// the CR that asked for it.
		//
		// Without this, a status update that loses an optimistic-concurrency
		// race ("the object has been modified") leaves the CR non-terminal
		// while its Job is already Complete. The next pass reaches here, sees
		// "finished", deletes the Job and runs the work again — and because the
		// same race recurs, it runs again, and again. Observed live: a 20-scene
		// render finished and uploaded video.mp4, then re-rendered from scratch
		// twice more while the workflow that submitted it sat waiting for a
		// callback that the replaced Job could no longer send.
		if metav1.IsControlledBy(&existing, ej) {
			return &existing, nil
		}
		log.FromContext(ctx).Info("replacing a finished Job from an earlier request",
			"job", name, "succeeded", existing.Status.Succeeded, "failed", existing.Status.Failed)
		// Foreground deletion, so the replacement is not created while the old
		// pods are still being reaped — two Jobs with one name is not a state
		// the API server allows, and a background delete makes the create race
		// the collection.
		policy := metav1.DeletePropagationForeground
		if err := r.Delete(ctx, &existing, &client.DeleteOptions{PropagationPolicy: &policy}); err != nil {
			if !apierrors.IsNotFound(err) {
				return nil, err
			}
		}
		// Requeue rather than create now: deletion is not instantaneous, and a
		// create against a name still held returns AlreadyExists.
		return nil, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}

	placement, ok := ResolvePlacement(ej.Spec.PlacementClass)
	if !ok {
		return nil, fmt.Errorf("unknown placement class %q", ej.Spec.PlacementClass)
	}

	job := r.buildJob(ctx, ej, name, placement)
	if err := ctrl.SetControllerReference(ej, job, r.Scheme); err != nil {
		return nil, err
	}
	if err := r.Create(ctx, job); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return &existing, r.Get(ctx, client.ObjectKey{Namespace: ej.Namespace, Name: name}, &existing)
		}
		return nil, err
	}

	ej.Status.JobName = name
	return job, nil
}

func (r *EphemeralJobReconciler) buildJob(
	ctx context.Context, ej *computev1alpha1.EphemeralJob, name string, p Placement,
) *batchv1.Job {
	backoff := int32(0) // no retry: a burst node join per attempt is too costly to spend blindly
	ttl := ej.Spec.TTLSecondsAfterFinished
	fleetID := r.resolveFleetID(ctx, ej.Namespace)

	// Object storage is bound in buildPodSpec, as for every pod of ours.
	container := r.buildWorkloadContainer(ej)

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ej.Namespace,
			Labels: authoredLabels(ej, map[string]string{
				labelJobUID: string(ej.UID),
				"tenant-id": tenantFromNamespace(ej.Namespace),
				// The application (ADR-088), read from the namespace label the
				// tenant registry writes. require-cost-labels demands it since
				// 2026-09-24, and its absence refused every Job-mode request in
				// every tenant namespace. Never taken from the CR, so a
				// submitter cannot attribute its work to another app. A
				// namespace without the label renders it empty, which the
				// policy refuses and the request fails naming.
				"app-id": fleetID,
				// enforce-tenant-abi/require-cost-labels matches every Job in a
				// tenant-* namespace and demands BOTH labels. tenant-id alone
				// meant the Job was rejected at admission — and rejected inside
				// this operator's reconcile, where the tenant sees only a CR
				// that never produces a pod.
				//
				// "platform" for the same reason the tenant gateway carries it:
				// this operator authors the pod, and its placement class,
				// priority and resource envelope are platform decisions with no
				// fleet-supplied input. Charging burst compute back to the
				// submitting tenant needs a real per-tenant source, which no
				// tenant namespace carries today; when one exists it belongs
				// here, read from the namespace rather than from the CR, so a
				// tenant cannot label its own spend.
				"cost-center": "platform",
			}),
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoff,
			// Failure classification expressed in native Job semantics, not
			// re-derived here (ADR-052 §16.3).
			//
			// The API server owns this judgement and makes it consistently; an
			// operator inspecting pod status to decide "was that a disruption or
			// a real failure?" is reimplementing a decision Kubernetes already
			// makes, and getting it subtly different for this one workload type.
			//
			// Ignore on DisruptionTarget is the load-bearing half: preemption,
			// node drain and eviction are INFRASTRUCTURE-transient (§16.3), and
			// counting them against the retry budget would fail a workload for
			// something it did not do — on burst capacity, where preemption is
			// routine, that is the difference between a job that survives
			// scale-down and one that does not.
			PodFailurePolicy: &batchv1.PodFailurePolicy{
				Rules: []batchv1.PodFailurePolicyRule{{
					Action: batchv1.PodFailurePolicyActionIgnore,
					OnPodConditions: []batchv1.PodFailurePolicyOnPodConditionsPattern{{
						Type:   corev1.DisruptionTarget,
						Status: corev1.ConditionTrue,
					}},
				}},
			},
			// ActiveDeadlineSeconds is DELIBERATELY NOT SET.
			//
			// batch/v1 measures it from the Job's own start, which includes the
			// entire wait for burst capacity. Setting it to the workload's
			// timeout therefore spends the workload's budget on the node join:
			// on a pool whose cold start exceeds the timeout, every job fails
			// before it runs, and reports TimedOut — blaming the workload for
			// an infrastructure wait. ADR-052 §11 requires the two clocks to be
			// separate, so the workload timeout is enforced by this operator
			// from status.startTime, which is set when the pod actually runs.
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: authoredLabels(ej, map[string]string{
						labelJobUID: string(ej.UID),
						"app-id":    fleetID,
					}),
				},
				Spec: r.buildPodSpec(ctx, ej, p, container),
			},
		},
	}
}

// reconcileTTL removes the EphemeralJob once its retention window has passed.
// The Job itself is reclaimed by ttlSecondsAfterFinished, and its pods with it.
func (r *EphemeralJobReconciler) reconcileTTL(ctx context.Context, ej *computev1alpha1.EphemeralJob) (ctrl.Result, error) {
	if ej.Status.CompletionTime == nil {
		return ctrl.Result{}, nil
	}
	ttl := time.Duration(ej.Spec.TTLSecondsAfterFinished) * time.Second
	elapsed := time.Since(ej.Status.CompletionTime.Time)
	if elapsed < ttl {
		return ctrl.Result{RequeueAfter: ttl - elapsed}, nil
	}
	return ctrl.Result{}, client.IgnoreNotFound(r.Delete(ctx, ej))
}

func (r *EphemeralJobReconciler) setPhase(ej *computev1alpha1.EphemeralJob, phase computev1alpha1.Phase, c CapacityState) {
	ej.Status.Phase = phase
	ej.Status.Message = c.Message
	ej.Status.ObservedGeneration = ej.Generation
	status := metav1.ConditionFalse
	if c.Ready {
		status = metav1.ConditionTrue
	}
	meta_SetStatusCondition(&ej.Status.Conditions, metav1.Condition{
		Type:               computev1alpha1.ConditionCapacity,
		Status:             status,
		Reason:             c.Reason,
		Message:            c.Message,
		ObservedGeneration: ej.Generation,
	})
}

func (r *EphemeralJobReconciler) markFinished(
	ctx context.Context, ej *computev1alpha1.EphemeralJob, phase computev1alpha1.Phase, exit *int32,
) error {
	return r.markFinishedWithReason(ctx, ej, phase, string(phase), exit)
}

func (r *EphemeralJobReconciler) markFinishedWithReason(
	ctx context.Context, ej *computev1alpha1.EphemeralJob, phase computev1alpha1.Phase, reason string, exit *int32,
) error {
	now := metav1.Now()
	ej.Status.Phase = phase
	ej.Status.CompletionTime = &now
	ej.Status.ExitCode = exit
	ej.Status.ObservedGeneration = ej.Generation
	// Set before the callback fires, so every delivery of this outcome — including
	// a redelivery after a crash — carries the same dedupe key (ADR-052 §16.2).
	ej.Status.TerminalEventID = terminalEventID(ej, phase)
	status := metav1.ConditionTrue
	if phase != computev1alpha1.PhaseSucceeded {
		status = metav1.ConditionFalse
	}
	meta_SetStatusCondition(&ej.Status.Conditions, metav1.Condition{
		Type:               computev1alpha1.ConditionComplete,
		Status:             status,
		Reason:             reason,
		Message:            ej.Status.Message,
		ObservedGeneration: ej.Generation,
	})
	if err := client.IgnoreNotFound(r.Status().Update(ctx, ej)); err != nil {
		return err
	}
	// Terminal state is recorded; now tell whoever is waiting for it. Ordered
	// after the status write so the CR is the source of truth even if the
	// callback fails and this is retried.
	return r.fireCallback(ctx, ej, phase, exit)
}

func (r *EphemeralJobReconciler) markTerminal(
	ctx context.Context, ej *computev1alpha1.EphemeralJob, phase computev1alpha1.Phase, reason, msg string,
) error {
	now := metav1.Now()
	ej.Status.Phase = phase
	ej.Status.CompletionTime = &now
	ej.Status.Message = msg
	ej.Status.ObservedGeneration = ej.Generation
	ej.Status.TerminalEventID = terminalEventID(ej, phase)
	meta_SetStatusCondition(&ej.Status.Conditions, metav1.Condition{
		Type:               computev1alpha1.ConditionCapacity,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: ej.Generation,
	})
	return client.IgnoreNotFound(r.Status().Update(ctx, ej))
}

// terminalEventID is the receiver's deduplication key (ADR-052 §16.2).
//
// Derived from the request's identity and its terminal phase, so it is
// identical across every REDELIVERY of one outcome and different for any other
// outcome. That is the property a receiver needs and the only one this side can
// actually provide: delivery is at-least-once, because the window between the
// receiver committing a side effect and this operator persisting
// ConditionCallbackDelivered cannot be closed from here.
//
// Prefers Spec.RequestID and falls back to the CR's UID. The UID is stable for
// the object's lifetime, which is enough to deduplicate redeliveries of the
// same terminal outcome; RequestID is better because it also survives the
// object being recreated for the same logical work.
func terminalEventID(ej *computev1alpha1.EphemeralJob, phase computev1alpha1.Phase) string {
	id := ej.Spec.RequestID
	if id == "" {
		id = string(ej.UID)
	}
	sum := sha256.Sum256([]byte(id + "|" + string(phase)))
	return hex.EncodeToString(sum[:])[:32]
}

// readyEventID is the deduplication key for a "now serving" event.
//
// Keyed on the POD, not the request: a Service-mode workload whose pod is lost
// gets a new one rather than a terminal state, and the receiver must be able to
// tell the second Running from a redelivery of the first. Two incarnations have
// two pod names, so they have two event ids.
//
// The uid is included so the key cannot collide with a previous EphemeralJob of
// the same name, and the phase so it cannot collide with that incarnation's
// terminal event.
func readyEventID(ej *computev1alpha1.EphemeralJob, podName string) string {
	sum := sha256.Sum256([]byte(string(ej.UID) + "|" + podName + "|ready"))
	return hex.EncodeToString(sum[:])[:32]
}

func callbackReason(ej *computev1alpha1.EphemeralJob) string {
	if c := meta_FindStatusCondition(ej.Status.Conditions, computev1alpha1.ConditionCapacity); c != nil && c.Reason == computev1alpha1.ReasonCapacityTimeout {
		return c.Reason
	}
	if c := meta_FindStatusCondition(ej.Status.Conditions, computev1alpha1.ConditionComplete); c != nil && c.Reason != "" {
		return c.Reason
	}
	return ""
}

func (r *EphemeralJobReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&computev1alpha1.EphemeralJob{}).
		Owns(&batchv1.Job{}).
		// Service mode owns its Pod and Service directly rather than through a
		// Job, so both must wake this controller too — otherwise a sandbox pod
		// going Ready or dying would be noticed only on the next timed requeue.
		Owns(&corev1.Pod{}).
		Owns(&corev1.Service{}).
		Complete(r)
}

// fireCallback notifies Spec.CallbackURL that the job reached a terminal state.
//
// This is the completion signal the workflow is suspended on. The SDK's step
// creates a single-use hook, puts its URL here, and returns pending_hitl; until
// something POSTs, the run waits indefinitely. Rendering takes tens of minutes,
// so polling for this would be wasteful and slow — the Job's own status change
// already wakes this controller (Owns(&batchv1.Job{})), and this turns that
// event into the message the workflow is waiting for.
//
// It is deliberately the OPERATOR that reports, not the workload. A container
// can only report outcomes it survives: an OOM kill, an image that will not
// pull, a pod evicted, a deadline exceeded — in each case the workload posts
// nothing and the run hangs. jobFinished already classifies those, so reporting
// from here covers the failures the job itself cannot.
//
// Delivery is at-least-once and marked with ConditionCallbackDelivered so a
// later reconcile does not repeat it. The receiver claims a single-use token,
// so a duplicate that races the marker is refused rather than double-resuming.
func (r *EphemeralJobReconciler) fireCallback(
	ctx context.Context, ej *computev1alpha1.EphemeralJob, phase computev1alpha1.Phase, exit *int32,
) error {
	return r.fireLifecycleCallback(ctx, ej, phase, exit,
		computev1alpha1.ConditionCallbackDelivered, terminalEventID(ej, phase))
}

// fireLifecycleCallback posts one lifecycle event to Spec.CallbackURL.
//
// gate is the condition that makes this event fire ONCE. It is a parameter and
// not a constant because the terminal event and the ready event must not share
// one: a ready notification that set ConditionCallbackDelivered would suppress
// the terminal callback, so a sandbox would report that it started and never
// that it ended.
//
// eventID is the receiver's deduplication key for THIS event. Distinct per event
// per incarnation, so a redelivered event from a previous incarnation cannot be
// mistaken for this one's.
// THE GATE IS PERSISTED IMMEDIATELY AFTER THE POST, and that ordering is the whole
// of what makes an event fire once.
//
// A previous version deferred it to the caller's own status write later in the same
// reconcile, on the reasoning that two writes in one pass is a second chance to lose
// a conflict. The mechanism was real and the conclusion was backwards: when that
// deferred write lost its race, the gate was never stored, the reconcile requeued,
// and the event was POSTed again. Observed as two identical Running deliveries 11 ms
// apart for one ready transition.
//
// A conflict on a LATER write is harmless once the gate is durable -- the next pass
// sees it and sends nothing. A conflict on the gate write itself is not harmless and
// is reported, because it means a duplicate is coming.
func (r *EphemeralJobReconciler) fireLifecycleCallback(
	ctx context.Context, ej *computev1alpha1.EphemeralJob, phase computev1alpha1.Phase, exit *int32,
	gate string, eventID string,
) error {
	l := log.FromContext(ctx)

	// Optional by design: a job nobody is waiting on needs no callback.
	if ej.Spec.CallbackURL == "" {
		return nil
	}
	if meta_IsStatusConditionTrue(ej.Status.Conditions, gate) {
		return nil
	}

	payload := map[string]any{
		"jobId":    ej.Name,
		"status":   string(phase),
		"exitCode": exit,
		"output":   ej.Spec.Output,
		"message":  ej.Status.Message,
		// The receiver's deduplication key (ADR-052 §16.2). Delivery is
		// at-least-once — ConditionCallbackDelivered suppresses the common
		// reconcile-driven duplicate but cannot close the crash window between
		// the receiver committing and this operator persisting that condition.
		// A receiver that ignores this field is unprotected against redelivery.
		// The receiver's deduplication key for THIS event (ADR-052 §16.2). Named
		// terminalEventId for compatibility with receivers that already read it;
		// it now carries ready events too, which are not terminal.
		"terminalEventId": eventID,
		"requestId":       ej.Spec.RequestID,
		// WHICH INCARNATION. A caller may recycle a workload by deleting the
		// EphemeralJob and creating another under the same name, so jobId is the
		// same for both and cannot tell them apart. Without this, a redelivered
		// terminal event from the previous incarnation arrives after the new one
		// is serving and reads as the new one ending.
		"uid": string(ej.UID),
		// When the operator observed the event, not when the receiver read it.
		// A receiver processing events out of order needs its own ordering, and
		// redelivery makes arrival order no guide at all.
		"timestamp": metav1.Now().UTC().Format(time.RFC3339),
		// The workload's labels, so a receiver can route the event without
		// holding a map from job name to whatever it cares about.
		"labels": ej.Labels,
	}
	if reason := callbackReason(ej); reason != "" {
		payload["reason"] = reason
	}
	body, err := json.Marshal(payload)
	if err != nil {
		// Unmarshallable payload is a programming error, not a transient one:
		// retrying cannot fix it, and blocking the TTL on it would leak the CR.
		l.Error(err, "callback payload could not be marshalled", "ephemeralJob", ej.Name)
		return nil
	}

	// Bounded: this runs inside Reconcile, and a hanging callback would stall
	// the work queue for every other job.
	cctx, cancel := context.WithTimeout(ctx, callbackTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(cctx, http.MethodPost, ej.Spec.CallbackURL, bytes.NewReader(body))
	if err != nil {
		l.Error(err, "callback request could not be built", "ephemeralJob", ej.Name)
		return nil
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := r.callbackClient().Do(req)
	if err != nil {
		// Requeue rather than swallow: a dropped callback strands the workflow
		// forever, which is a worse failure than a late one.
		l.Info("callback failed, will retry", "ephemeralJob", ej.Name, "err", err.Error())
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 500 {
		l.Info("callback returned a server error, will retry",
			"ephemeralJob", ej.Name, "status", resp.StatusCode)
		return fmt.Errorf("callback to %s returned %d", ej.Spec.CallbackURL, resp.StatusCode)
	}

	// A 4xx is not retried. The token is single-use, so 404/409 is the expected
	// answer to a duplicate that beat the marker, and every other 4xx means the
	// receiver rejected this payload — neither improves by repeating.
	if resp.StatusCode >= 400 {
		l.Info("callback rejected, not retrying",
			"ephemeralJob", ej.Name, "status", resp.StatusCode)
	}

	meta_SetStatusCondition(&ej.Status.Conditions, metav1.Condition{
		Type:               gate,
		Status:             metav1.ConditionTrue,
		Reason:             string(phase),
		Message:            fmt.Sprintf("callback responded %d", resp.StatusCode),
		ObservedGeneration: ej.Generation,
	})
	// Not deferred to the caller. See the note above: a deferred gate that loses a
	// race re-sends the event.
	//
	// A failure here IS worth surfacing, unlike a conflict on the phase write below:
	// it means this event will be sent again on the retry. At-least-once is what the
	// contract promises (ADR-052 §16.2) and why the payload carries a per-event key,
	// but a receiver that is not idempotent would act twice, so the operator says so
	// rather than passing it over.
	return client.IgnoreNotFound(r.Status().Update(ctx, ej))
}

// buildWorkloadContainer builds the container the request describes. Shared by
// both modes: a sandbox and a render job differ in lifecycle, not in how the
// workload container itself is assembled.
func (r *EphemeralJobReconciler) buildWorkloadContainer(ej *computev1alpha1.EphemeralJob) corev1.Container {
	env := make([]corev1.EnvVar, 0, len(ej.Spec.Env)+1)
	for k, v := range ej.Spec.Env {
		env = append(env, corev1.EnvVar{Name: k, Value: v})
	}

	// spec.input, serialised as the field documents. A request that sets its
	// own EPHEMERAL_JOB_INPUT in spec.env keeps it: what a caller states wins
	// over what the platform derives.
	if raw := ej.Spec.Input; raw != nil && len(raw.Raw) > 0 {
		if _, stated := ej.Spec.Env[envJobInput]; !stated {
			env = append(env, corev1.EnvVar{Name: envJobInput, Value: string(raw.Raw)})
		}
	}

	// spec.output, on the same terms: serialised as stated, never interpreted.
	if out := ej.Spec.Output; out != nil {
		if _, stated := ej.Spec.Env[envJobOutput]; !stated {
			if encoded, err := json.Marshal(out); err == nil {
				env = append(env, corev1.EnvVar{Name: envJobOutput, Value: string(encoded)})
			}
		}
	}

	container := corev1.Container{
		Name:    workloadContainerName,
		Image:   ej.Spec.Image,
		Command: ej.Spec.Command,
		Args:    ej.Spec.Args,
		Env:     env,
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr(false),
			RunAsNonRoot:             ptr(true),
			ReadOnlyRootFilesystem:   ptr(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "workspace", MountPath: "/workspace"},
			{Name: "result", MountPath: "/result"},
		},
	}
	// The request's envelope wins; the platform supplies one only when the
	// request names none.
	//
	// Resources are optional on the CR but MANDATORY on the pod: every burst pod
	// runs under the burst-tenant priority class, and the burst-compute
	// ResourceQuota is scoped to it — a quota naming requests.cpu/memory refuses
	// any pod that omits them. Left absent, the Job's every pod creation is
	// forbidden, and invisibly: a refused create is not a failed pod, so the job
	// controller retries forever, status.failed stays 0, JobFailed is never set,
	// and the request reports WaitingForCapacity indefinitely.
	//
	// The floor is deliberately modest and a REQUEST, not a limit, so a workload
	// still bursts above it on an idle node while four fit concurrently in the
	// quota. A workload that needs a different envelope states it and this is
	// not consulted — which is the normal case: both callers now do.
	if ej.Spec.Resources != nil {
		container.Resources = *ej.Spec.Resources
	}
	withRequests(&container, defaultRequestCPU, defaultRequestMemory)
	withLimits(&container, defaultLimitCPU, defaultLimitMemory, defaultLimitEphemeralStorage)
	if ej.Spec.ImagePullPolicy != "" {
		container.ImagePullPolicy = ej.Spec.ImagePullPolicy
	}
	container.ReadinessProbe = ej.Spec.ReadinessProbe
	container.LivenessProbe = ej.Spec.LivenessProbe
	container.Ports = ej.Spec.Ports
	container.WorkingDir = ej.Spec.WorkingDir
	if len(ej.Spec.VolumeMounts) > 0 {
		container.VolumeMounts = ej.Spec.VolumeMounts
	}
	return container
}

// buildPodSpec is the ONE place a pod belonging to this operator is described.
//
// Both modes go through it, which is the point of the merge: placement is
// written here, so there is no path — Job or Service — by which a pod of ours
// reaches a node without it. Previously the sandbox path had exactly such a
// path, because its pod was authored upstream and placement was bolted on by a
// mutating policy that could simply not match.
// ctx is threaded in for resolveFleetID: the Secret and ConfigMap holding a
// fleet's S3 configuration are named from its namespace's `app-id` label, and
// that label has to be read.
func (r *EphemeralJobReconciler) buildPodSpec(
	ctx context.Context, ej *computev1alpha1.EphemeralJob, p Placement, container corev1.Container,
) corev1.PodSpec {
	fleetID := r.resolveFleetID(ctx, ej.Namespace)
	// Here and not in buildWorkloadContainer: the references are named from the
	// fleet id, which only this function resolves.
	container.Env = withOutputStorageEnv(ej, container.Env, fleetID)
	volumes := ej.Spec.Volumes
	if len(volumes) == 0 {
		volumes = []corev1.Volume{
			{Name: WorkspaceVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			{Name: "result", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		}
	}

	// Operator-authored containers, kept separate from the fleet's until the
	// pod spec is assembled so a fleet's Sidecars list cannot displace them.
	// Both operator-authored containers are init containers now: the restore
	// step, and the sync sidecar that is native (restartPolicy: Always) rather
	// than an entry in .spec.containers.
	var spec_initContainers []corev1.Container

	// A persisted workspace REPLACES the workspace volume with an emptyDir
	// (ADR-052 §14, §14.2).
	//
	// With emptyDir, workspace identity lives in S3, not in a PVC. Every pod
	// starts empty, the sidecar restores via FUSE mount (§14.2), and the
	// overlay filesystem holds the live tree for the pod's lifetime.
	//
	// A staging emptyDir is also added for squashfs archive and FUSE working
	// directories (§14.2).
	if ej.Spec.WorkspacePersistence != nil {
		replaced := false
		for i := range volumes {
			if volumes[i].Name == WorkspaceVolumeName {
				volumes[i].VolumeSource = corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}
				replaced = true
				break
			}
		}
		if !replaced {
			volumes = append(volumes, corev1.Volume{
				Name:         WorkspaceVolumeName,
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
			})
		}
		// HostToContainer is the workload's half of the propagation pair.
		//
		// The sidecar mounts fuse-overlayfs at this path in ITS mount
		// namespace, and containers in a pod do not share mount namespaces. The
		// sidecar's mount is Bidirectional, which pushes it out to the host;
		// this makes the workload pick it up from there. Set on one side only
		// and the agent sees an empty emptyDir while restore reports success.
		hostToContainer := corev1.MountPropagationHostToContainer
		container.VolumeMounts = appendMountIfAbsent(container.VolumeMounts, corev1.VolumeMount{
			Name:             WorkspaceVolumeName,
			MountPath:        WorkspaceMountPath,
			MountPropagation: &hostToContainer,
		})

		// Staging emptyDir for the squashfs archive, the FUSE working
		// directories, and the readiness marker (§14.2). Deliberately NOT
		// mounted into the workload: it holds the overlay's upper layer, and a
		// tenant able to write there could edit its own workspace out from
		// under the filesystem presenting it.
		if !hasVolume(volumes, "ws-staging") {
			volumes = append(volumes, corev1.Volume{
				Name:         "ws-staging",
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
			})
		}

		// The platform's persistence agent — ONE container, not two (§14.2).
		//
		// It is added by the OPERATOR, never by the fleet: it carries the
		// object-store credential, and §19.6 makes agent-vault the only
		// credential channel into the workload container. A fleet-supplied
		// sidecar could not be trusted with this.
		//
		// The `workspace-restore` init container that used to precede it is
		// gone. A FUSE mount does not outlive the container that made it — the
		// mount namespace goes with the process — so an init container could
		// only ever download the archive the sidecar downloads again, or mount
		// an overlay destroyed before the workload starts. Restore has one
		// owner now, and the startup probe on this container is what holds the
		// workload back until that restore is finished.
		keep := resolveKeepCheckpoints(ej.Spec.WorkspacePersistence)
		spec_initContainers = append(spec_initContainers,
			workspaceSyncContainer(ej.Spec.WorkspacePersistence, keep, ej.Namespace, fleetID))

		// A second instance for the app-shared workspace, when the fleet asked
		// for one. Same image, same code, same key layout — only the workspace
		// id and the root differ, which is what makes the shared tree shared
		// rather than a per-session copy (see SharedWorkspaceID).
		//
		// AFTER the session instance, and that order is load-bearing: native
		// sidecars start sequentially and each gates on the previous one's
		// startup probe, so the session tree (and any overlay over it) exists
		// before this container mounts a directory inside it.
		if shared := ej.Spec.WorkspacePersistence.SharedWorkspaceID; shared != "" {
			spec_initContainers = append(spec_initContainers,
				workspaceSyncContainerFor(ej.Spec.WorkspacePersistence, keep, ej.Namespace, fleetID, workspaceSyncTarget{
					name:        "workspace-sync-shared",
					workspaceID: shared,
					root:        WorkspaceMountPath + "/" + sharedWorkspaceDirName,
					staging:     workspaceStagingPath + "/" + sharedWorkspaceDirName,
					port:        sharedWorkspaceSyncPort,
					noArchive:   true,
					// Restore-only when the fleet says this session only READS the
					// shared tree. See SharedWorkspaceReadOnly for the data loss
					// this exists to stop.
					readOnly: ej.Spec.WorkspacePersistence.SharedWorkspaceReadOnly,
					// On-demand refresh of that read-only tree (ADR-052 §14.9).
					refresh: ej.Spec.WorkspacePersistence.SharedWorkspaceRefresh,
				}))
		}
	}

	// A writable /tmp, always.
	//
	// Every container here runs with ReadOnlyRootFilesystem, which is right —
	// but a read-only root with no tmpfs is not a hardened container, it is a
	// broken one. Python resolves a temp directory at import time and aborts
	// when it cannot: "No usable temporary directory found in ['/tmp',
	// '/var/tmp', '/usr/tmp', '/app']". The sandbox harness died on that while
	// its health endpoint kept answering 200, so the pod read as healthy with
	// the agent runtime dead underneath.
	//
	// The path this replaced did not set ReadOnlyRootFilesystem at all, so the
	// harness had a writable root and never noticed. Tightening that without
	// supplying a tmpfs is what broke it.
	//
	// Added rather than made optional: no workload is improved by having
	// nowhere to write a temp file, and requiring every caller to remember one
	// reproduces this failure for the next one.
	if !hasVolume(volumes, "tmp") {
		volumes = append(volumes, corev1.Volume{
			Name:         "tmp",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		})
	}
	container.VolumeMounts = appendMountIfAbsent(container.VolumeMounts, corev1.VolumeMount{
		Name: "tmp", MountPath: "/tmp",
	})

	// ── The pod-bound identity, on the WORKLOAD container only (ADR-052).
	//
	// Not on the sidecars, and that is the point of doing it here rather than in
	// the shared loop below: the sidecars are auxiliary processes the platform
	// supplies, and the agent-vault sidecar in particular proxies outbound calls
	// for tenant-authored code. Handing it an identity that a receiver accepts
	// would make the proxy a caller in its own right.
	//
	// The path is the PLATFORM's, not the fleet's. A tool reads it to find its
	// token, so it is a contract between the platform and the application, and a
	// fleet-supplied path would be a contract the fleet could change under its own
	// tools. It is also why the mount is read-only: the kubelet rewrites the file
	// in place on renewal and nothing in the pod should be writing there.
	if it := ej.Spec.IdentityToken; it != nil {
		exp := int64(identityTokenDefaultExpirySeconds)
		if it.ExpirationSeconds != nil {
			exp = *it.ExpirationSeconds
		}
		volumes = appendVolumeIfAbsent(volumes, corev1.Volume{
			Name: identityTokenVolumeName,
			VolumeSource: corev1.VolumeSource{
				Projected: &corev1.ProjectedVolumeSource{
					Sources: []corev1.VolumeProjection{{
						ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
							Audience:          it.Audience,
							ExpirationSeconds: &exp,
							Path:              identityTokenFileName,
						},
					}},
				},
			},
		})
		container.VolumeMounts = appendMountIfAbsent(container.VolumeMounts, corev1.VolumeMount{
			Name:      identityTokenVolumeName,
			MountPath: IdentityTokenMountPath,
			ReadOnly:  true,
		})
	}

	// Every container, not just the workload.
	//
	// The burst-compute ResourceQuota refuses a POD in which any container omits
	// requests.cpu/memory, so defaulting only the primary left the sidecar to
	// fail the whole pod: "must specify requests.cpu for: agent-vault". The
	// rejection names the container, but it is the pod that is never created —
	// and the request that named an envelope for its workload looks, from the
	// outside, like one that did not.
	sidecars := make([]corev1.Container, 0, len(ej.Spec.Sidecars))
	for _, c := range ej.Spec.Sidecars {
		withRequests(&c, defaultSidecarRequestCPU, defaultSidecarRequestMemory)
		withLimits(&c, defaultSidecarLimitCPU, defaultSidecarLimitMemory, defaultSidecarLimitEphemeralStorage)
		sidecars = append(sidecars, c)
	}
	// Covers the operator's own containers too, both of which are now init
	// containers. A native sidecar is quota-accounted like any other init
	// container, so workspace-sync omitting requests would fail the pod the
	// same way agent-vault did.
	for i := range spec_initContainers {
		withRequests(&spec_initContainers[i], defaultSidecarRequestCPU, defaultSidecarRequestMemory)
		withLimits(&spec_initContainers[i], defaultSidecarLimitCPU, defaultSidecarLimitMemory, defaultSidecarLimitEphemeralStorage)
	}

	// Never for a job; Always for a service.
	//
	// A batch workload that exits non-zero has failed and the Job controller
	// decides whether to retry, so restarting the container underneath it would
	// hide the outcome. A Service-mode workload is the opposite: it is expected
	// to keep running, and a transient startup failure must not be terminal.
	//
	// This was Never for both, and the cost was immediate. A sandbox scheduled
	// onto a burst node 60 seconds old lost the DNS race — cluster DNS had not
	// converged for the new node — and the harness exited 3 on
	// "Temporary failure in name resolution". With Never it stayed dead, so the
	// pod existed, the CR read Running, and the sandbox was permanently broken.
	// Cold-start races are inherent to scale-from-zero, so recovery has to be.
	restart := corev1.RestartPolicyNever
	if ej.Spec.Mode == computev1alpha1.ModeService {
		restart = corev1.RestartPolicyAlways
	}

	// No ServiceAccount token in a tenant workload (ADR-052 §19.6).
	//
	// This was UNSET, which defaults to true, so every sandbox pod carried a
	// projected token at /var/run/secrets/kubernetes.io/serviceaccount that
	// tenant-authored agent code could read and present to the API server. The
	// threat model here is explicitly untrusted code with a shell, and §19.6
	// requires agent-vault to be the only credential channel — a mounted API
	// credential is a second one, obtained without asking.
	//
	// Nothing in a workload needs it: the operator talks to the API server, the
	// workload does not.
	automount := false

	// The window the final checkpoint gets to run in (ADR-052 §14).
	//
	// The kubelet SIGTERMs the native sidecar after the workload exits, then
	// SIGKILLs whatever is still running when the grace period expires — so
	// this value is the hard ceiling on a teardown snapshot, and the default
	// of 30s is not a considered number, it is just the default. A snapshot
	// that is cut off mid-upload leaves no checkpoint at all (the manifest is
	// written last, so a partial run is absent rather than corrupt): it fails
	// safe, but it fails.
	//
	// A floor, not an override — a fleet asking for MORE time keeps it. The
	// value is a starting point to be replaced by measured p95 checkpoint
	// latency once there is any; it is deliberately generous rather than
	// tuned, because being wrong long costs a slower teardown and being wrong
	// short costs the user's uncommitted work.
	grace := ej.Spec.TerminationGracePeriodSeconds
	if ej.Spec.WorkspacePersistence != nil {
		if grace == nil || *grace < minWorkspaceGraceSeconds {
			grace = ptr(int64(minWorkspaceGraceSeconds))
		}
	}

	spec := corev1.PodSpec{
		RestartPolicy:                restart,
		AutomountServiceAccountToken: &automount,

		// ── ADR-052 §4: placement, written by the component that authors the
		// pod. There is no fleet-supplied input to any of these three fields.
		NodeSelector:      p.NodeSelector,
		Tolerations:       p.Tolerations,
		PriorityClassName: p.PriorityClassName,

		// Resolve a service FQDN in one query, not five. The cluster default of
		// ndots:5 makes any name with fewer than 5 dots relative, so an ordinary
		// service address is tried against every search domain first — one of
		// which, on a hybrid spoke, is the node's tailnet domain that forwards
		// off-cluster. Under load that lookup fails outright.
		DNSConfig: &corev1.PodDNSConfig{
			Options: []corev1.PodDNSConfigOption{{Name: "ndots", Value: ptr("2")}},
		},

		SecurityContext: &corev1.PodSecurityContext{
			RunAsNonRoot:   ptr(true),
			RunAsUser:      ptr(int64(1000)),
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		InitContainers:                spec_initContainers,
		Containers:                    append([]corev1.Container{container}, sidecars...),
		Volumes:                       volumes,
		ImagePullSecrets:              ej.Spec.ImagePullSecrets,
		TerminationGracePeriodSeconds: grace,
		// ADR-107: nil is the ordinary runtime. The RuntimeClass carries its own
		// node selector, so a Sandboxed pod lands only where the runtime exists.
		RuntimeClassName: runtimeClassFor(ej),
	}
	return spec
}

// buildPod is the Service-mode workload: a bare Pod, not a Job.
//
// A batch/v1 Job exists to drive something to completion and reports failure
// when its pod exits non-zero. A sandbox has no completion — it serves until it
// goes idle — so wrapping it in a Job would either report a permanent failure
// or a success that never arrives. The Pod is owned by the EphemeralJob, so it
// is still garbage-collected structurally.
func (r *EphemeralJobReconciler) buildPod(
	ctx context.Context, ej *computev1alpha1.EphemeralJob, name string, p Placement,
) *corev1.Pod {
	container := r.buildWorkloadContainer(ej)
	fleetID := r.resolveFleetID(ctx, ej.Namespace)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			// GenerateName, not Name — see ensurePod. The server picks a unique
			// suffix, so a leftover object can never hold the name this request
			// needs.
			GenerateName: name + "-",
			Namespace:    ej.Namespace,
			Labels: authoredLabels(ej, map[string]string{
				labelJobUID:  string(ej.UID),
				labelJobName: ej.Name,
				"tenant-id":  tenantFromNamespace(ej.Namespace),
				"app-id":     fleetID,
				// enforce-tenant-abi/require-cost-labels does not match bare
				// Pods, but the label is carried anyway so Job-mode and
				// Service-mode workloads attribute identically.
				"cost-center": "platform",
			}),
		},
		Spec: r.buildPodSpec(ctx, ej, p, container),
	}
}

// buildService fronts a Service-mode workload with a stable in-cluster address.
//
// The selector is the EphemeralJob's UID, not its name: a name can be reused
// after deletion, and a Service that outlived its pod would then silently
// forward a new session's traffic to whatever claimed the old name.
func (r *EphemeralJobReconciler) buildService(
	ej *computev1alpha1.EphemeralJob, name string,
) *corev1.Service {
	svcType := ej.Spec.Service.Type
	if svcType == "" {
		svcType = corev1.ServiceTypeClusterIP
	}
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ej.Namespace,
			Labels: authoredLabels(ej, map[string]string{
				labelJobUID: string(ej.UID),
				"tenant-id": tenantFromNamespace(ej.Namespace),
			}),
		},
		Spec: corev1.ServiceSpec{
			Type:     svcType,
			Selector: map[string]string{labelJobUID: string(ej.UID)},
			Ports:    ej.Spec.Service.Ports,
		},
	}
}

// reconcileServiceMode drives a long-lived workload: a Pod, an optional Service
// in front of it, and an idle clock instead of a completion.
//
// The idle clock is the only thing that ends it. A Job-mode request is bounded
// by its own exit; a sandbox serves until nobody is using it, so if
// LastActivityTime is never refreshed the pod is reaped at IdleTimeoutSeconds.
// That is deliberately fail-closed: a client that crashes without releasing its
// sandbox loses it on the timeout rather than holding burst capacity forever.
func (r *EphemeralJobReconciler) reconcileServiceMode(
	ctx context.Context, ej *computev1alpha1.EphemeralJob,
) (ctrl.Result, error) {
	l := log.FromContext(ctx)

	placement, ok := ResolvePlacement(ej.Spec.PlacementClass)
	if !ok {
		return ctrl.Result{}, fmt.Errorf("unknown placement class %q", ej.Spec.PlacementClass)
	}

	name := jobNameFor(ej)

	// Service first: it selects on the EphemeralJob UID, so it is correct the
	// moment the pod appears and there is no window where a caller resolves the
	// address and reaches nothing.
	if ej.Spec.Service != nil {
		if err := r.ensureService(ctx, ej, serviceNameFor(ej)); err != nil {
			return ctrl.Result{}, err
		}
	}

	pod, err := r.ensurePod(ctx, ej, name, placement)
	if err != nil {
		// The same rule as Job mode: a pod the API server refuses -- a
		// RuntimeClass this cluster does not offer (ADR-107), a quota, a policy --
		// will be refused on every retry. Retrying forever left the request
		// Pending with no reason anywhere; it ends now, with the server's own
		// message, which the lifecycle callback carries to the submitter.
		if jobRefusedAtAdmission(err) {
			ej.Status.Message = err.Error()
			return ctrl.Result{}, r.markFinishedWithReason(ctx, ej, computev1alpha1.PhaseFailed,
				computev1alpha1.ReasonAdmissionRejected, nil)
		}
		return ctrl.Result{}, err
	}
	if pod == nil {
		// A terminated pod of the same name is being deleted so this request
		// can have a fresh one; it does not exist yet.
		return ctrl.Result{RequeueAfter: defaultRequeue}, nil
	}

	switch pod.Status.Phase {
	case corev1.PodFailed:
		ej.Status.Message = podTerminationMessage(pod)
		return ctrl.Result{}, r.markFinished(ctx, ej, computev1alpha1.PhaseFailed, podExitCode(pod))
	case corev1.PodSucceeded:
		// A service that exited on its own. Not an error, but it is over.
		return ctrl.Result{}, r.markFinished(ctx, ej, computev1alpha1.PhaseSucceeded, podExitCode(pod))
	}

	cap, err := AssessCapacityBySelector(ctx, r.Client, r.APIReader, ej.Namespace,
		client.MatchingLabels{labelJobUID: string(ej.UID)})
	if err != nil {
		return ctrl.Result{}, err
	}

	// The absolute bound, checked before anything a client can influence.
	//
	// Everything below is refreshable or conditional; this is not. Burst
	// capacity bills per node-hour, and a workload whose end depends on a
	// cooperative client has no end at all.
	if ej.Spec.MaxLifetimeSeconds > 0 {
		if lived := time.Since(ej.CreationTimestamp.Time); lived > time.Duration(ej.Spec.MaxLifetimeSeconds)*time.Second {
			l.Info("reaping workload at its lifetime bound", "ephemeralJob", ej.Name, "lived", lived.Truncate(time.Second))
			if err := r.deletePod(ctx, pod); err != nil {
				return ctrl.Result{}, err
			}
			ej.Status.Message = fmt.Sprintf("reached the %ds maximum lifetime", ej.Spec.MaxLifetimeSeconds)
			if meta_FindStatusCondition(ej.Status.Conditions, computev1alpha1.ConditionCapacity) == nil {
				meta_SetStatusCondition(&ej.Status.Conditions, metav1.Condition{
					Type:               computev1alpha1.ConditionCapacity,
					Status:             metav1.ConditionFalse,
					Reason:             cap.Reason,
					Message:            cap.Message,
					ObservedGeneration: ej.Generation,
				})
			}
			return ctrl.Result{}, r.markFinished(ctx, ej, computev1alpha1.PhaseSucceeded, nil)
		}
	}

	if !cap.Ready {
		if ej.Spec.CapacityWaitSeconds != nil && *ej.Spec.CapacityWaitSeconds > 0 {
			if waited := time.Since(ej.CreationTimestamp.Time); waited > time.Duration(*ej.Spec.CapacityWaitSeconds)*time.Second {
				l.Info("capacity wait deadline exceeded", "ephemeralJob", ej.Name, "waited", waited)
				if err := r.deletePod(ctx, pod); err != nil {
					return ctrl.Result{}, err
				}
				ej.Status.Message = fmt.Sprintf(
					"capacity wait deadline exceeded: waiting %s for capacity, over %ds limit: %s",
					waited.Truncate(time.Second), *ej.Spec.CapacityWaitSeconds, cap.Message)
				meta_SetStatusCondition(&ej.Status.Conditions, metav1.Condition{
					Type:               computev1alpha1.ConditionCapacity,
					Status:             metav1.ConditionFalse,
					Reason:             computev1alpha1.ReasonCapacityTimeout,
					Message:            ej.Status.Message,
					ObservedGeneration: ej.Generation,
				})
				return ctrl.Result{}, r.markFinishedWithReason(ctx, ej, computev1alpha1.PhaseTimedOut, computev1alpha1.ReasonCapacityTimeout, nil)
			}
		}

		r.setPhase(ej, computev1alpha1.PhaseProvisioning, cap)
		if waited := time.Since(ej.CreationTimestamp.Time); waited > r.ProvisioningBudget {
			ej.Status.Message = fmt.Sprintf(
				"waiting %s for burst capacity, over the %s budget for this placement class: %s",
				waited.Truncate(time.Second), r.ProvisioningBudget, cap.Message)
		}
		if res, err := r.updateServiceStatus(ctx, ej); err != nil || res.Requeue {
			return res, err
		}
		return ctrl.Result{RequeueAfter: defaultRequeue}, nil
	}

	// StartTime is seeded as soon as the pod runs, ready or not: the readiness
	// deadline below measures from it, and a workload that never becomes ready
	// would otherwise have no clock at all.
	now := metav1.Now()
	if ej.Status.StartTime == nil {
		ej.Status.StartTime = &now
	}

	// A workload that never becomes ready is broken, and IDLENESS CANNOT SEE
	// THAT. The idle clock is refreshed by attempts, so a client retrying
	// against a dead sandbox keeps it alive indefinitely — observed on a
	// sandbox whose harness had been dead 37 minutes with an idle age of 30
	// seconds, holding a burst node throughout. This is the bound that catches
	// it.
	if !workloadReady(pod) {
		if waited := time.Since(ej.Status.StartTime.Time); waited > time.Duration(ej.Spec.ReadinessDeadlineSeconds)*time.Second {
			l.Info("workload never became ready", "ephemeralJob", ej.Name, "waited", waited.Truncate(time.Second))
			if err := r.deletePod(ctx, pod); err != nil {
				return ctrl.Result{}, err
			}
			ej.Status.Message = fmt.Sprintf(
				"workload did not become ready within %ds of starting", ej.Spec.ReadinessDeadlineSeconds)
			return ctrl.Result{}, r.markFinished(ctx, ej, computev1alpha1.PhaseFailed, nil)
		}
		r.setPhase(ej, computev1alpha1.PhaseProvisioning, cap)
		// Say WHICH container is stuck and why, when that is knowable. The
		// generic message below is true of every unhealthy pod and diagnoses
		// none of them.
		if blocker := podStartupBlocker(pod); blocker != "" {
			ej.Status.Message = blocker
		} else {
			ej.Status.Message = "workload container is not ready"
		}
		ej.Status.PodName = pod.Name
		if res, err := r.updateServiceStatus(ctx, ej); err != nil || res.Requeue {
			return res, err
		}
		return ctrl.Result{RequeueAfter: defaultRequeue}, nil
	}

	// ── Serving. Tell the caller, once per POD (ADR-052 §16.2).
	//
	// Service mode has no completion, so without this a caller learns a sandbox
	// exists only by polling it. The event fires on the transition to ready and
	// not on every reconcile, which is what the gate condition is for.
	//
	// RESET WHEN THE POD CHANGED. A Service-mode workload whose pod is lost gets a
	// replacement rather than a terminal state (see ensurePod), so the caller needs
	// to hear that it is serving again -- a second Running, with its own event id,
	// which a receiver reads as "replaced" rather than as a duplicate. Comparing
	// against the recorded PodName is what detects that; the condition alone would
	// stay true across the replacement and the caller would never be told.
	if ej.Status.PodName != pod.Name {
		meta_SetStatusCondition(&ej.Status.Conditions, metav1.Condition{
			Type:               computev1alpha1.ConditionReadyNotified,
			Status:             metav1.ConditionFalse,
			Reason:             "PodReplaced",
			Message:            "a new pod is serving; the caller is told again",
			ObservedGeneration: ej.Generation,
		})
	}
	// PodName is recorded further down, with the rest of the status, so the
	// comparison above reads the PREVIOUS incarnation's name -- which is the
	// whole point of it.

	// The idle clock starts only once the workload has actually served, so a
	// slow start is not charged against the session's idle budget.
	if ej.Status.LastActivityTime == nil {
		ej.Status.LastActivityTime = &now
	}

	// Phase Running, not a terminal one: this says the workload is serving, and
	// the caller will hear separately when it ends.
	if err := r.fireLifecycleCallback(ctx, ej, computev1alpha1.PhaseRunning, nil,
		computev1alpha1.ConditionReadyNotified, readyEventID(ej, pod.Name)); err != nil {
		// Logged, not fatal. A caller that missed the ready event still receives
		// the terminal one, and failing the reconcile here would stop the idle
		// clock below from ever starting -- which holds a burst node open.
		log.FromContext(ctx).Error(err, "ready callback failed",
			"ephemeralJob", ej.Name, "pod", pod.Name)
	}

	idle := time.Since(ej.Status.LastActivityTime.Time)
	budget := time.Duration(ej.Spec.IdleTimeoutSeconds) * time.Second
	if idle > budget {
		l.Info("reaping idle service workload", "ephemeralJob", ej.Name, "idle", idle.Truncate(time.Second))
		policy := metav1.DeletePropagationBackground
		if err := r.Delete(ctx, pod, &client.DeleteOptions{PropagationPolicy: &policy}); err != nil &&
			!apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		ej.Status.Message = fmt.Sprintf("idle for %s, exceeding the %ds idle budget",
			idle.Truncate(time.Second), ej.Spec.IdleTimeoutSeconds)
		return ctrl.Result{}, r.markFinished(ctx, ej, computev1alpha1.PhaseSucceeded, nil)
	}

	r.setPhase(ej, computev1alpha1.PhaseRunning, cap)
	ej.Status.PodName = pod.Name
	if ej.Spec.Service != nil {
		ej.Status.ServiceName = serviceNameFor(ej)
	}
	if res, err := r.updateServiceStatus(ctx, ej); err != nil || res.Requeue {
		return res, err
	}
	// Wake when the idle budget could next expire, so a quiet sandbox is reaped
	// promptly rather than at the next unrelated event.
	return ctrl.Result{RequeueAfter: minDuration(budget-idle, defaultRequeue)}, nil
}

// updateServiceStatus writes the status and treats a lost race as a retry.
//
// A CONFLICT IS NOT A FAULT, and reporting it as one is misleading. This status
// subresource has two writers: this controller, and the fleet's SDK marking
// activity on the sandbox it owns. A lost race is the expected outcome of that and
// is resolved by re-reading, which is what a requeue does. Returning it as an error
// produced a "Reconciler error" line at the same instant as every Running callback
// -- which reads as the callback having failed, and would mask a real error in the
// same place.
//
// Nothing is lost: the next pass re-reads and rewrites. If the write that lost was
// the one carrying ConditionReadyNotified, the event is re-POSTed on the retry,
// which is at-least-once as promised (ADR-052 §16.2) and why the payload carries a
// per-event key.
func (r *EphemeralJobReconciler) updateServiceStatus(
	ctx context.Context, ej *computev1alpha1.EphemeralJob,
) (ctrl.Result, error) {
	if err := r.Status().Update(ctx, ej); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	return ctrl.Result{}, nil
}

// failureDetail assembles why a Job-mode workload failed, from everything knowable
// at the moment it failed and nothing that is not.
//
// Three sources, widening in specificity:
//
//	the Job's condition      why the controller gave up (BackoffLimitExceeded)
//	the pod's termination    which container, which reason, which exit code
//	the container's log tail what the workload itself said
//
// The log tail is the only one that separates a bad input from an infrastructure
// fault, which is what a caller needs in order to decide whether a retry is worth
// attempting. The first two describe the SHAPE of the failure; only the workload's
// own output describes its cause.
//
// Every source is best-effort and absence is not an error: a detail that cannot be
// read is worth less than the ones that can, and failing here would turn a workload
// failure into a reconcile failure.
func (r *EphemeralJobReconciler) failureDetail(
	ctx context.Context, job *batchv1.Job, jobDetail string,
) (string, *int32) {
	parts := []string{}
	if jobDetail != "" {
		parts = append(parts, jobDetail)
	}

	pod, err := r.podForJob(ctx, job)
	if err != nil || pod == nil {
		return strings.Join(parts, "; "), nil
	}
	if m := podTerminationMessage(pod); m != "" && m != "pod failed" {
		parts = append(parts, m)
	}
	exit := podExitCode(pod)
	if exit != nil {
		parts = append(parts, fmt.Sprintf("exit code %d", *exit))
	}
	if tail := r.containerLogTail(ctx, pod); tail != "" {
		parts = append(parts, "last output: "+tail)
	}
	return strings.Join(parts, "; "), exit
}

// containerLogTail returns the end of the workload container's log.
//
// Needs a typed clientset: a log is a subresource request, not an object read, so the
// controller-runtime client cannot serve it. A nil Clientset yields no tail rather
// than no detail -- the operator runs without one under test, and a missing tail
// degrades the message instead of failing the reconcile.
//
// Bounded three ways, because this runs inside Reconcile: a line cap, a byte cap and a
// timeout. A workload that logged a gigabyte must not put that in a status field, and
// a slow read must not stall the work queue for every other job.
func (r *EphemeralJobReconciler) containerLogTail(ctx context.Context, pod *corev1.Pod) string {
	if r.Clientset == nil {
		return ""
	}
	lctx, cancel := context.WithTimeout(ctx, logTailTimeout)
	defer cancel()

	lines := int64(logTailLines)
	stream, err := r.Clientset.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
		Container: workloadContainerName,
		TailLines: &lines,
	}).Stream(lctx)
	if err != nil {
		return ""
	}
	defer func() { _ = stream.Close() }()

	buf := make([]byte, logTailBytes)
	n, _ := io.ReadFull(stream, buf)
	if n == 0 {
		return ""
	}
	return strings.TrimSpace(string(buf[:n]))
}

func (r *EphemeralJobReconciler) ensurePod(
	ctx context.Context, ej *computev1alpha1.EphemeralJob, name string, p Placement,
) (*corev1.Pod, error) {
	// Compute is identified by OWNERSHIP, never by name.
	//
	// This used to get-or-create a Pod called `ej-<request>`. A deterministic
	// name makes the compute's identity a string that anything can hold: a pod
	// left behind by a previous incarnation — terminated, or stuck Terminating
	// because the node it ran on is gone — keeps that name indefinitely, and
	// every later attempt either adopted a corpse or could not create. Observed
	// for two days on one sandbox: adopted, `Failed` within seconds, repeat.
	//
	// Every remedy for that is a remedy for the NAMING: check whether the
	// adopted pod is usable, delete it when it is not, force-delete it when the
	// delete does not take. Force-deleting removes the API object while the
	// container may still run on a partitioned node, which is a safety decision
	// this controller has no business making on a timer — and reclaiming pods
	// whose node has gone is the node-lifecycle controller's job, not ours.
	//
	// So the name stops being an identifier. Pods are created with
	// `GenerateName` and found by label, exactly as ReplicaSet and Job do, and
	// the label is the CR's UID, which a recreated CR does not share. A stale
	// pod is then not tolerated but invisible: it matches no selector this
	// controller uses, holds no name this controller wants, and is collected by
	// Kubernetes when its own owner goes.
	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(ej.Namespace),
		client.MatchingLabels{labelJobUID: string(ej.UID)},
	); err != nil {
		return nil, err
	}
	for i := range pods.Items {
		// A pod on its way out is not this request's compute, even though it
		// still matches the selector; reporting its phase would report the
		// shutdown as the job's outcome.
		if pods.Items[i].DeletionTimestamp == nil {
			return &pods.Items[i], nil
		}
	}

	pod := r.buildPod(ctx, ej, name, p)
	if err := ctrl.SetControllerReference(ej, pod, r.Scheme); err != nil {
		return nil, err
	}
	if err := r.Create(ctx, pod); err != nil {
		return nil, err
	}
	return pod, nil
}

func (r *EphemeralJobReconciler) ensureService(
	ctx context.Context, ej *computev1alpha1.EphemeralJob, name string,
) error {
	var existing corev1.Service
	err := r.Get(ctx, client.ObjectKey{Namespace: ej.Namespace, Name: name}, &existing)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	svc := r.buildService(ej, name)
	if err := ctrl.SetControllerReference(ej, svc, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, svc); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// podStartupBlocker names the container that is stopping this pod from
// starting, and why — or "" when nothing is obviously wrong.
//
// This exists because "workload container is not ready" is true of every
// unhealthy pod and useful for none of them. A crash-looping init container is
// invisible in the CR: the phase reads Provisioning, the capacity condition
// reads Scheduled, and the actual cause lives in a container status nobody
// looks at until they already suspect it.
//
// Observed: the workspace-sync sidecar could not verify a TLS certificate, so
// it exited, its startup probe never passed, and the workload never started.
// What a user saw was a chat request timing out with a 504, several layers
// away, with nothing connecting the two.
//
// INIT CONTAINERS FIRST, deliberately. Under §14.1 the workspace sidecar is a
// native init container that must be ready before the workload starts, so when
// both are unhappy the init container is the cause and the workload is the
// symptom.
// podForJob returns the one pod a Job created, or nil.
//
// Job mode has no direct handle on its pod — the Job controller owns it — so it
// is found by the same label the Job sets on its template. Best-effort by
// design: the caller uses it only to improve a status message, so a lookup
// failure must degrade to the generic message rather than fail a reconcile.
func (r *EphemeralJobReconciler) podForJob(ctx context.Context, job *batchv1.Job) (*corev1.Pod, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(job.Namespace),
		client.MatchingLabels{"job-name": job.Name},
	); err != nil {
		return nil, err
	}
	if len(pods.Items) == 0 {
		return nil, nil
	}
	return &pods.Items[0], nil
}

func podStartupBlocker(p *corev1.Pod) string {
	// Reasons that mean "this will not fix itself". ContainerCreating and
	// PodInitializing are ordinary transient states and must NOT be reported —
	// every pod passes through them, and naming them would turn a normal start
	// into an alarming message.
	blocking := map[string]bool{
		"CrashLoopBackOff":           true,
		"ImagePullBackOff":           true,
		"ErrImagePull":               true,
		"CreateContainerConfigError": true,
		"CreateContainerError":       true,
		"RunContainerError":          true,
		"InvalidImageName":           true,
	}

	describe := func(kind string, cs []corev1.ContainerStatus) string {
		for i := range cs {
			w := cs[i].State.Waiting
			if w == nil || !blocking[w.Reason] {
				continue
			}
			msg := fmt.Sprintf("%s %q is %s", kind, cs[i].Name, w.Reason)
			if w.Message != "" {
				msg += ": " + w.Message
			}
			// The last termination message is where the process's own reason
			// lives — for a crash loop the Waiting state says only
			// "CrashLoopBackOff", which names the symptom and not the fault.
			if t := cs[i].LastTerminationState.Terminated; t != nil {
				if t.Message != "" {
					msg += " (last exit: " + strings.TrimSpace(t.Message) + ")"
				} else if t.ExitCode != 0 {
					msg += fmt.Sprintf(" (last exit code %d)", t.ExitCode)
				}
			}
			return msg
		}
		return ""
	}

	if m := describe("init container", p.Status.InitContainerStatuses); m != "" {
		return m
	}
	return describe("container", p.Status.ContainerStatuses)
}

func podExitCode(p *corev1.Pod) *int32 {
	for i := range p.Status.ContainerStatuses {
		if t := p.Status.ContainerStatuses[i].State.Terminated; t != nil {
			code := t.ExitCode
			return &code
		}
	}
	return nil
}

// podTerminationMessage is why the pod failed, in the workload's own words when
// it gave any. A container that writes its reason to its termination message
// (/dev/termination-log) has said exactly what went wrong -- "1.4 s of speech;
// at least 3 s is needed" -- and that is the job's failure, the same as a RunPod
// handler's returned error. Only without one does the kubelet's reason
// ("Error", "OOMKilled") stand in.
func podTerminationMessage(p *corev1.Pod) string {
	for i := range p.Status.ContainerStatuses {
		if t := p.Status.ContainerStatuses[i].State.Terminated; t != nil && t.ExitCode != 0 {
			if m := strings.TrimSpace(t.Message); m != "" {
				return m
			}
		}
	}
	for i := range p.Status.ContainerStatuses {
		if t := p.Status.ContainerStatuses[i].State.Terminated; t != nil && t.Reason != "" {
			return fmt.Sprintf("container %s terminated: %s", p.Status.ContainerStatuses[i].Name, t.Reason)
		}
	}
	if p.Status.Reason != "" {
		return p.Status.Reason
	}
	return "pod failed"
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

// authoredLabels merges the request's own labels into the ones this operator
// stamps on what it authors.
//
// The submitter's labels have to reach the POD, not just the CR, because that
// is where cluster policy selects: the platform's sandbox default-deny
// NetworkPolicy matches agents.x-k8s.io/sandbox on the pod. Under the previous
// design the upstream controller copied podTemplate labels through; now that
// this operator authors the pod, dropping them here would silently remove a
// workload from the policy that is supposed to contain it.
//
// The operator's own labels win on conflict. They are the ones other platform
// components key on — ownership, attribution — and a submitter must not be able
// to reassign its pod's tenant by relabelling its request.
func authoredLabels(ej *computev1alpha1.EphemeralJob, own map[string]string) map[string]string {
	merged := make(map[string]string, len(ej.Labels)+len(own))
	for k, v := range ej.Labels {
		merged[k] = v
	}
	for k, v := range own {
		merged[k] = v
	}
	// A peer is marked by the operator, so the platform's peer default-deny
	// selects it whatever the submitter labelled.
	if ej.Spec.Peer != nil {
		merged[labelSessionPeer] = "true"
	}
	return merged
}

// validateSpec rejects requests that describe a pod the API server would refuse,
// so the submitter learns at submission instead of watching a CR that never
// produces one.
func validateSpec(ej *computev1alpha1.EphemeralJob) (reason, message string, invalid bool) {
	// A Service-mode workload has no completion, so without an idle bound it
	// never terminates and holds burst capacity indefinitely.
	if ej.Spec.Mode == computev1alpha1.ModeService && ej.Spec.IdleTimeoutSeconds <= 0 {
		return "InvalidSpec", "mode=Service requires idleTimeoutSeconds: without it the workload would never be reaped", true
	}
	return "", "", false
}

// serviceNameFor is the address a client resolves.
//
// "<name>-svc" is the convention the SDK's serviceName() already used when it
// created this Service itself, and it is kept so that moving ownership into the
// operator does not also move every caller's address. status.serviceName is
// published alongside it for anything that would rather read than derive.

func serviceNameFor(ej *computev1alpha1.EphemeralJob) string {
	return ej.Name + "-svc"
}

// withRequests fills in any resource request a container leaves unset.
//
// It exists because the requirement is per-CONTAINER: the burst-compute
// ResourceQuota refuses a pod in which any container omits requests.cpu or
// requests.memory, so the workload and every sidecar must each carry them. A
// stated value is never overwritten.
func withRequests(c *corev1.Container, cpu, memory string) {
	if c.Resources.Requests == nil {
		c.Resources.Requests = corev1.ResourceList{}
	}
	if _, ok := c.Resources.Requests[corev1.ResourceCPU]; !ok {
		c.Resources.Requests[corev1.ResourceCPU] = resource.MustParse(cpu)
	}
	if _, ok := c.Resources.Requests[corev1.ResourceMemory]; !ok {
		c.Resources.Requests[corev1.ResourceMemory] = resource.MustParse(memory)
	}
}

// withLimits fills in any resource limit a container leaves unset (ADR-106).
//
// A stated limit is never overwritten. A default is never placed below the
// container's own request for that resource -- the API server refuses a pod
// whose limit is under its request, so a job that asked for more than the
// default would otherwise never start. In that case the request is the limit.
func withLimits(c *corev1.Container, cpu, memory, ephemeralStorage string) {
	if c.Resources.Limits == nil {
		c.Resources.Limits = corev1.ResourceList{}
	}
	for name, def := range map[corev1.ResourceName]string{
		corev1.ResourceCPU:              cpu,
		corev1.ResourceMemory:           memory,
		corev1.ResourceEphemeralStorage: ephemeralStorage,
	} {
		if _, ok := c.Resources.Limits[name]; ok {
			continue
		}
		limit := resource.MustParse(def)
		if req, ok := c.Resources.Requests[name]; ok && req.Cmp(limit) > 0 {
			limit = req.DeepCopy()
		}
		c.Resources.Limits[name] = limit
	}
}

// workloadReady reports whether the workload container itself is ready.
//
// It looks at the workload container by name rather than at the pod phase:
// a pod carrying a sidecar reports Running as long as ANY container runs, so
// pod phase cannot distinguish "serving" from "the harness died and the
// credential broker is still up".
func workloadReady(p *corev1.Pod) bool {
	for i := range p.Status.ContainerStatuses {
		if p.Status.ContainerStatuses[i].Name == workloadContainerName {
			return p.Status.ContainerStatuses[i].Ready
		}
	}
	return false
}

// deletePod removes a Service-mode workload's pod. The Service and the
// EphemeralJob outlive it until TTL, so the terminal status stays readable.
func (r *EphemeralJobReconciler) deletePod(ctx context.Context, pod *corev1.Pod) error {
	policy := metav1.DeletePropagationBackground
	if err := r.Delete(ctx, pod, &client.DeleteOptions{PropagationPolicy: &policy}); err != nil &&
		!apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// The identity token's location, owned by the PLATFORM.
//
// A tool inside the sandbox opens IdentityTokenPath to authenticate, so this is a
// contract between the platform and every application that uses it -- which is
// why it is a constant here and not a field on the spec. A fleet-supplied path
// would be a contract the fleet could move under its own tools, and the failure
// would be a tool that reads an empty file and sends no credential.
const (
	identityTokenVolumeName = "platform-identity-token"
	identityTokenFileName   = "token"
	// IdentityTokenMountPath is the directory; IdentityTokenPath is the file a
	// tool reads. Under /var/run/secrets so it shares the convention of every
	// other credential a kubelet projects, and on a tmpfs rather than a disk.
	IdentityTokenMountPath = "/var/run/secrets/platform.soloz.io/identity"
	IdentityTokenPath      = IdentityTokenMountPath + "/" + identityTokenFileName
	// The API server refuses a projected token below 600s, so this is the floor
	// and not a preference.
	identityTokenDefaultExpirySeconds = 600
)

func appendVolumeIfAbsent(vs []corev1.Volume, v corev1.Volume) []corev1.Volume {
	if hasVolume(vs, v.Name) {
		return vs
	}
	return append(vs, v)
}

func hasVolume(vs []corev1.Volume, name string) bool {
	for i := range vs {
		if vs[i].Name == name {
			return true
		}
	}
	return false
}

// appendMountIfAbsent adds a mount unless the caller already mounted that path
// or that volume — a workload that supplies its own /tmp keeps it.
func appendMountIfAbsent(ms []corev1.VolumeMount, m corev1.VolumeMount) []corev1.VolumeMount {
	for i := range ms {
		if ms[i].MountPath == m.MountPath || ms[i].Name == m.Name {
			return ms
		}
	}
	return append(ms, m)
}
