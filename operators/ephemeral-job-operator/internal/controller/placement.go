package controller

import (
	corev1 "k8s.io/api/core/v1"
)

// Placement is the concrete node placement for one placement class
// (ADR-046 §11, ADR-052 §4).
//
// These values are the platform's, not the fleet's. They are resolved from a
// class name the fleet may select among, never from fields the fleet supplies,
// and they are written by this operator into the Job it authors. That is the
// whole of the ADR-052 §4 guarantee on the job path: a fleet cannot express
// placement because EphemeralJobSpec has no field carrying it, so there is
// nothing to mutate afterwards and nothing to reject.
//
// §14.2: StorageClass is removed. Workspace identity lives in S3, not in a
// PVC — every pod starts with an emptyDir and the sidecar restores via FUSE
// mount. Node affinity is gone.
type Placement struct {
	NodeSelector      map[string]string
	Tolerations       []corev1.Toleration
	PriorityClassName string
}

// The burst node's identity. These constants MUST match, exactly, the node
// labels and taint the burst worker registers with in
// manifests/providers/hetzner/base/spokepool-clusterclass-v1.yaml, and the
// capacity.cluster-autoscaler.kubernetes.io/{labels,taints} annotations on the
// burst MachineDeployment.
//
// The coupling is a correctness requirement, not documentation. A taint present
// on the real node but absent from the autoscaler's annotations makes the
// autoscaler simulate a node the pending pod cannot tolerate; it concludes
// scale-up will not help, declines to scale, and logs no error. The pod pends
// forever. scripts/validate/preflight/86-burst-node-identity.sh asserts the
// three copies agree, because three hand-maintained strings for one taint is a
// silent-outage generator.
const (
	// Taint: what the burst node repels. Keyed on workload CLASS, not location:
	// the pure-hetzner spoke's general workers also carry
	// workload-location=hetzner and must NOT be tainted, or every workload on
	// that spoke becomes unschedulable.
	BurstTaintKey   = "workload-class"
	BurstTaintValue = "burst"

	// Labels: what the burst node advertises.
	WorkloadLocationKey   = "workload-location"
	WorkloadLocationValue = "hetzner"

	// The home-lab side of the hybrid spoke (ADR-046 §13).
	//
	// "on-prem", NOT "home". The value is what provision-flatcar-worker.sh puts
	// on the node at join time, and every other consumer agrees with it: the
	// hybrid composition's default nodeSelector, the local-path and Hetzner CSI
	// addons' node affinity, hub-bootstrap.sh's worker selector, and the
	// bootstrap orchestrator's postconditions.
	//
	// This constant said "home" and nothing anywhere sets that. The pod was
	// therefore well-formed, admissible and permanently unschedulable:
	//
	//   0/3 nodes are available: 1 node(s) had untolerated taint
	//   {node-role.kubernetes.io/control-plane: }, 2 node(s) didn't match Pod's
	//   node affinity/selector.
	//
	// which surfaces as EphemeralJob phase Provisioning / WaitingForCapacity --
	// a message about capacity for what is actually a label mismatch, so it
	// reads as "the cluster is full" rather than "this selector matches nothing".
	//
	// ADR-046's prose is inconsistent on this and is the likely origin: §13 says
	// home workers are labelled workload-location=home, while §11's own table of
	// default workloads says on-prem. The running cluster and every script that
	// writes the label say on-prem.
	WorkloadLocationHome = "on-prem"

	// ADR-046 §11: workload-location is never sufficient alone. Propagated from
	// the MachineDeployment by CAPI — kubelet cannot self-register a
	// node-role.kubernetes.io/* label, NodeRestriction rejects it.
	NodeRoleWorkerKey = "node-role.kubernetes.io/worker"

	BurstPriorityClass = "burst-tenant"

	// minWorkspaceGraceSeconds is the floor on terminationGracePeriodSeconds
	// for a pod with a persisted workspace — the time the native sidecar has
	// to take its final checkpoint after the workload exits and before the
	// kubelet SIGKILLs it (ADR-052 §14). Replace with measured p95 checkpoint
	// latency when there is some; until then it is deliberately generous.
	minWorkspaceGraceSeconds = 120

	// WorkspaceMountPath is where a persisted workspace appears in the
	// workload. Fixed, not configurable: it is the path the harness and every
	// skill already assume, and a per-job override would let two sandboxes
	// sharing a workspace disagree about where it is.
	WorkspaceMountPath = "/workspace"
	// WorkspaceVolumeName is the pod-local volume name for that mount. It
	// deliberately matches the default emptyDir volume it replaces, so the
	// workload container's mount is the same either way.
	WorkspaceVolumeName = "workspace"
)

// placements maps the classes EphemeralJobSpec.PlacementClass may name.
//
// ADR-046 §11 is explicit that workload-location is never sufficient on its
// own — the worker role label must accompany it, or a workload can land on a
// control-plane node that happens to carry the location label.
var placements = map[string]Placement{
	// Home-lab capacity, for workloads that must be REACHABLE from inside the
	// cluster.
	//
	// A sandbox is connected to: the SDK holds an SSE stream against
	// sandbox-<id>-svc.<ns>.svc.cluster.local, so the workload has to be a pod
	// with a Service. A Hetzner VM created outside the cluster cannot provide
	// that — it has no ClusterIP — which is why the provisioner path serves
	// renders (S3 plus a public callback, nothing inbound) and this one serves
	// sandboxes.
	//
	// On a hybrid spoke these are the only nodes a pod can occupy today: a
	// CAPI-provisioned burst node cannot yet hold both a tailnet InternalIP,
	// which Cilium needs to reach home-lab nodes, and CCM initialisation.
	//
	// The burst-tenant priority class is kept deliberately. It places this
	// BELOW every platform workload, so tenant execution can never preempt
	// infrastructure — a property that matters more on home-lab hardware than
	// on elastic capacity, not less. It also keeps these pods inside the
	// burst-compute ResourceQuota, which is scoped to that class, so home
	// capacity stays bounded rather than open-ended.
	//
	// ADR-046 §11: workload-location is never sufficient alone. Without the
	// worker role a pod can land on a control-plane node carrying the label.
	//
	// §14.2: No StorageClass — workspace is emptyDir, restored via FUSE.
	"home": {
		NodeSelector: map[string]string{
			WorkloadLocationKey: WorkloadLocationHome,
			NodeRoleWorkerKey:   "",
		},
		// No toleration: home workers carry no burst taint. Adding one would be
		// inert here and misleading to the next reader.
		PriorityClassName: BurstPriorityClass,
	},

	"burst": {
		NodeSelector: map[string]string{
			WorkloadLocationKey: WorkloadLocationValue,
			NodeRoleWorkerKey:   "",
		},
		Tolerations: []corev1.Toleration{{
			Key:      BurstTaintKey,
			Operator: corev1.TolerationOpEqual,
			Value:    BurstTaintValue,
			Effect:   corev1.TaintEffectNoSchedule,
		}},
		// Below every platform workload (ADR-052 §5), so burst work can never
		// preempt infrastructure. This is also the scope the per-fleet
		// ResourceQuota selects on, so the bound and the preemption order come
		// from the same object.
		PriorityClassName: BurstPriorityClass,
	},
}

// ResolvePlacement returns the placement for a class name. An unknown class is
// an error rather than a pass-through: silently running an unplaced workload
// would put tenant demand on home-lab capacity, which is the outcome ADR-052
// exists to prevent.
func ResolvePlacement(class string) (Placement, bool) {
	if class == "" {
		class = "burst"
	}
	p, ok := placements[class]
	return p, ok
}

// Workspace volume constants (ADR-052 §14, §14.2).
const (
	// annotationWorkspaceID records the workspace id on resources for
	// observability.
	annotationWorkspaceID = "compute.nutgraf.in/workspace-id"
)
