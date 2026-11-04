package controller

import (
	"context"
	"reflect"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	computev1alpha1 "github.com/kamarkat/kamarkat/operators/ephemeral-job-operator/api/v1alpha1"
)

// TestWorkspaceSpecCannotExpressStorage is the storage analogue of
// TestSpecCannotExpressPlacement, and it exists for the same reason: ADR-052
// §14 keeps StorageClass, size and PVC naming with the platform, and the only
// enforcement that survives a refactor is the fleet-facing type being incapable
// of saying them.
//
// If someone later adds StorageClass or Size here, a fleet can place its data
// on storage it was not granted, and — because a PVC bound to the wrong class
// is unreachable from the node the pod is placed on — do it in a way that
// presents as a scheduling fault rather than a policy breach.
func TestWorkspaceSpecCannotExpressStorage(t *testing.T) {
	forbidden := []string{"storageclass", "size", "capacity", "accessmode", "volumename", "provisioner"}

	st := reflect.TypeOf(computev1alpha1.WorkspacePersistenceSpec{})
	for i := 0; i < st.NumField(); i++ {
		name := strings.ToLower(st.Field(i).Name)
		tag := strings.ToLower(st.Field(i).Tag.Get("json"))
		for _, f := range forbidden {
			if strings.Contains(name, f) || strings.Contains(tag, f) {
				t.Errorf("WorkspacePersistenceSpec.%s lets a fleet name its own storage; that is platform-supplied (ADR-052 §14)",
					st.Field(i).Name)
			}
		}
	}
}

// TestPersistedWorkspaceIsAnEmptyDir is the behaviour the whole feature rests
// on (§14.2): workspace identity lives in S3, not in a PVC. Every pod starts
// with an emptyDir and the sidecar restores via FUSE mount.
func TestPersistedWorkspaceIsAnEmptyDir(t *testing.T) {
	r := &EphemeralJobReconciler{}
	p, _ := ResolvePlacement("home")

	ej := &computev1alpha1.EphemeralJob{
		Spec: computev1alpha1.EphemeralJobSpec{
			Image:                "example.com/img@sha256:" + strings.Repeat("a", 64),
			WorkspacePersistence: &computev1alpha1.WorkspacePersistenceSpec{WorkspaceID: "app-1", AppID: "app-1"},
		},
	}

	spec := r.buildPodSpec(context.Background(), ej, p, corev1.Container{Name: "workload"})

	// 1. Workspace volume must be emptyDir, not PVC.
	var ws *corev1.Volume
	for i := range spec.Volumes {
		if spec.Volumes[i].Name == WorkspaceVolumeName {
			ws = &spec.Volumes[i]
		}
	}
	if ws == nil {
		t.Fatalf("no %q volume in the pod spec", WorkspaceVolumeName)
	}
	if ws.PersistentVolumeClaim != nil {
		t.Fatalf("%q volume is PVC-backed (got %+v) — §14.2 says workspace identity lives in S3, not a PVC",
			WorkspaceVolumeName, ws.VolumeSource)
	}
	if ws.EmptyDir == nil {
		t.Fatalf("%q volume is not emptyDir (got %+v)", WorkspaceVolumeName, ws.VolumeSource)
	}

	// 2. Exactly one workspace volume.
	n := 0
	for _, v := range spec.Volumes {
		if v.Name == WorkspaceVolumeName {
			n++
		}
	}
	if n != 1 {
		t.Errorf("found %d %q volumes, want exactly 1", n, WorkspaceVolumeName)
	}

	// 3. Workload mounts it.
	mounted := false
	for _, m := range spec.Containers[0].VolumeMounts {
		if m.Name == WorkspaceVolumeName && m.MountPath == WorkspaceMountPath {
			mounted = true
		}
	}
	if !mounted {
		t.Errorf("workload does not mount %q at %s", WorkspaceVolumeName, WorkspaceMountPath)
	}

	// 4. Staging emptyDir present for FUSE mounts (§14.2).
	var staging *corev1.Volume
	for i := range spec.Volumes {
		if spec.Volumes[i].Name == "ws-staging" {
			staging = &spec.Volumes[i]
		}
	}
	if staging == nil {
		t.Error("no ws-staging volume — squashfs archive needs a staging dir for FUSE mounts (§14.2)")
	} else if staging.EmptyDir == nil {
		t.Error("ws-staging volume is not emptyDir")
	}

	// 5. The sidecar is privileged, and the workload is NOT.
	//
	// SYS_ADMIN was tried and cannot work: Bidirectional mount propagation is
	// refused on a non-privileged container, /dev/fuse cannot be exposed
	// without privileged or a device plugin, and allowPrivilegeEscalation=false
	// blocks the setuid fusermount3 both tools mount through. Privileged is the
	// real cost of the FUSE path — so this asserts it deliberately rather than
	// letting it drift in unnoticed, and asserts just as deliberately that the
	// blast radius stops at the platform's own container.
	var sync *corev1.Container
	for i := range spec.InitContainers {
		if spec.InitContainers[i].Name == "workspace-sync" {
			sync = &spec.InitContainers[i]
		}
	}
	if sync == nil {
		t.Fatal("no workspace-sync container")
	}
	// §14.7: the sidecar is restricted, like every other container in this Pod.
	//
	// It was privileged under §14.2 so squashfuse and fuse-overlayfs could
	// mount. Pod Security Admission judges the whole Pod, this Pod runs tenant
	// code, and the result was that no workspace-bearing sandbox was ever
	// admitted into a tenant namespace. Archive materialisation moves to
	// platform-owned node storage; nothing in the Pod mounts anything.
	sc := sync.SecurityContext
	if sc == nil {
		t.Fatal("workspace-sync has no security context")
	}
	if sc.Privileged != nil && *sc.Privileged {
		t.Error("workspace-sync is privileged; a tenant namespace enforces restricted and refuses the Pod")
	}
	if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Error("workspace-sync must set runAsNonRoot")
	}
	if sc.RunAsUser == nil || *sc.RunAsUser != 1000 {
		t.Errorf("workspace-sync RunAsUser = %v, want 1000 — it shares the workspace volume with the workload",
			sc.RunAsUser)
	}
	if sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
		t.Error("workspace-sync must drop ALL capabilities")
	}
	if sc := spec.Containers[0].SecurityContext; sc != nil {
		if sc.Privileged != nil && *sc.Privileged {
			t.Error("the WORKLOAD container is privileged; the FUSE requirement must not leak " +
				"out of the platform's own sidecar (§19.6)")
		}
		if sc.Capabilities != nil && len(sc.Capabilities.Add) > 0 {
			t.Errorf("workload container gained capabilities %v; §19.6 keeps tenant code unprivileged",
				sc.Capabilities.Add)
		}
	}

	// 6. Mount propagation is GONE, and its absence is the assertion.
	//
	// §14.2 needed a Bidirectional/HostToContainer pair to carry the sidecar's
	// fuse-overlayfs mount into the workload's mount namespace. Restoring into
	// the shared volume directly needs no such thing: a volume is visible to
	// every container that mounts it. Bidirectional is also rejected on a
	// non-privileged container, so leaving it would make the Pod inadmissible.
	if len(sync.VolumeMounts) == 0 {
		t.Fatal("workspace-sync has no volume mounts")
	}
	var syncWs *corev1.VolumeMount
	for i := range sync.VolumeMounts {
		if sync.VolumeMounts[i].Name == WorkspaceVolumeName {
			syncWs = &sync.VolumeMounts[i]
		}
	}
	if syncWs == nil {
		t.Fatal("workspace-sync does not mount the workspace volume")
	}
	if syncWs.MountPropagation != nil {
		t.Errorf("workspace-sync sets mount propagation %v; nothing is mounted, and Bidirectional "+
			"is rejected outright on a non-privileged container", *syncWs.MountPropagation)
	}
	var loadWs *corev1.VolumeMount
	for i := range spec.Containers[0].VolumeMounts {
		if spec.Containers[0].VolumeMounts[i].Name == WorkspaceVolumeName {
			loadWs = &spec.Containers[0].VolumeMounts[i]
		}
	}
	if loadWs == nil {
		t.Fatal("workload does not mount the workspace volume")
	}
	// The workload's half may remain HostToContainer -- it is valid on an
	// unprivileged container and harmless -- but it must never be Bidirectional,
	// which would demand privilege on the container running tenant code.
	if loadWs.MountPropagation != nil && *loadWs.MountPropagation == corev1.MountPropagationBidirectional {
		t.Error("workload requests Bidirectional propagation, which requires privilege on tenant code")
	}

	// 7. The staging volume must NOT reach the workload: it holds the overlay's
	// upper layer, and tenant write access there bypasses the filesystem
	// presenting the workspace.
	for _, m := range spec.Containers[0].VolumeMounts {
		if m.Name == "ws-staging" {
			t.Error("workload mounts ws-staging; the overlay upper layer must stay platform-only")
		}
	}
}

// TestWithoutPersistenceWorkspaceStaysEphemeral guards the other direction: a
// job that asked for nothing must be byte-for-byte unaffected by this feature
// existing. Renders and every batch workload rely on that.
func TestWithoutPersistenceWorkspaceStaysEphemeral(t *testing.T) {
	r := &EphemeralJobReconciler{}
	p, _ := ResolvePlacement("burst")

	ej := &computev1alpha1.EphemeralJob{
		Spec: computev1alpha1.EphemeralJobSpec{
			Image: "example.com/img@sha256:" + strings.Repeat("a", 64),
		},
	}

	spec := r.buildPodSpec(context.Background(), ej, p, corev1.Container{Name: "workload"})

	for _, v := range spec.Volumes {
		if v.Name == WorkspaceVolumeName {
			if v.PersistentVolumeClaim != nil {
				t.Errorf("workspace is PVC-backed without workspacePersistence being requested")
			}
			if v.EmptyDir == nil {
				t.Errorf("workspace lost its emptyDir source")
			}
		}
	}
	// No staging volume without persistence.
	for _, v := range spec.Volumes {
		if v.Name == "ws-staging" {
			t.Error("ws-staging volume present without workspacePersistence — should only be added when persistence is requested")
		}
	}
}

// TestNoServiceAccountTokenInTenantPods guards ADR-052 §19.6.
//
// AutomountServiceAccountToken was UNSET here, which defaults to true, so every
// sandbox pod carried a projected API credential that tenant-authored code
// could read. The threat model is untrusted code with a shell, and the failure
// is silent — nothing reports a token that is merely present.
func TestNoServiceAccountTokenInTenantPods(t *testing.T) {
	r := &EphemeralJobReconciler{}
	p, _ := ResolvePlacement("home")
	ej := &computev1alpha1.EphemeralJob{
		Spec: computev1alpha1.EphemeralJobSpec{
			Image: "example.com/img@sha256:" + strings.Repeat("a", 64),
		},
	}

	spec := r.buildPodSpec(context.Background(), ej, p, corev1.Container{Name: "workload"})

	if spec.AutomountServiceAccountToken == nil {
		t.Fatal("AutomountServiceAccountToken is unset, which defaults to TRUE — tenant code can read an API credential (ADR-052 §19.6)")
	}
	if *spec.AutomountServiceAccountToken {
		t.Error("AutomountServiceAccountToken is true; agent-vault must be the only credential channel (ADR-052 §19.6)")
	}
}

// TestTerminalEventIDIsStablePerOutcome is the property a callback receiver
// deduplicates on (ADR-052 §16.2).
//
// Delivery is at-least-once: the window between a receiver committing its side
// effect and this operator persisting ConditionCallbackDelivered cannot be
// closed from this side. So the key must be identical across redeliveries of
// ONE outcome and different across outcomes — otherwise a receiver either
// double-applies or silently drops a real second event.
func TestTerminalEventIDIsStablePerOutcome(t *testing.T) {
	ej := &computev1alpha1.EphemeralJob{
		Spec: computev1alpha1.EphemeralJobSpec{RequestID: "req-1"},
	}

	a := terminalEventID(ej, computev1alpha1.PhaseSucceeded)
	b := terminalEventID(ej, computev1alpha1.PhaseSucceeded)
	if a != b {
		t.Errorf("same outcome produced different ids: %q then %q — a redelivery would be treated as new work", a, b)
	}

	if f := terminalEventID(ej, computev1alpha1.PhaseFailed); f == a {
		t.Error("Succeeded and Failed share a terminal event id; a receiver would drop the second as a duplicate")
	}

	other := &computev1alpha1.EphemeralJob{Spec: computev1alpha1.EphemeralJobSpec{RequestID: "req-2"}}
	if terminalEventID(other, computev1alpha1.PhaseSucceeded) == a {
		t.Error("different requests share a terminal event id")
	}

	// No RequestID: must still produce a usable key rather than collapsing every
	// unnamed request onto one id.
	u1 := &computev1alpha1.EphemeralJob{}
	u1.UID = "uid-1"
	u2 := &computev1alpha1.EphemeralJob{}
	u2.UID = "uid-2"
	if terminalEventID(u1, computev1alpha1.PhaseSucceeded) == terminalEventID(u2, computev1alpha1.PhaseSucceeded) {
		t.Error("requests without a RequestID collide on UID fallback")
	}
}

// TestJobIgnoresDisruptionFailures covers the half of §16.3 with a real cost on
// burst capacity: preemption and node drain are infrastructure-transient, and
// counting them against the retry budget fails a workload for something it did
// not do. Expressed as podFailurePolicy so the API server makes the judgement
// rather than this operator re-deriving it from pod status.
func TestJobIgnoresDisruptionFailures(t *testing.T) {
	r := &EphemeralJobReconciler{}
	p, _ := ResolvePlacement("burst")
	ej := &computev1alpha1.EphemeralJob{
		Spec: computev1alpha1.EphemeralJobSpec{
			Image: "example.com/img@sha256:" + strings.Repeat("a", 64),
		},
	}

	job := r.buildJob(context.Background(), ej, "j", p)
	if job.Spec.PodFailurePolicy == nil {
		t.Fatal("no podFailurePolicy: disruption would consume the retry budget (ADR-052 §16.3)")
	}
	found := false
	for _, rule := range job.Spec.PodFailurePolicy.Rules {
		for _, c := range rule.OnPodConditions {
			if c.Type == corev1.DisruptionTarget && rule.Action == batchv1.PodFailurePolicyActionIgnore {
				found = true
			}
		}
	}
	if !found {
		t.Error("podFailurePolicy does not Ignore DisruptionTarget; preemption on burst capacity would fail the job")
	}
}

// TestWorkspaceSyncIsANativeSidecar pins the three lifecycle properties the
// teardown checkpoint depends on (ADR-052 §14). Each one was wrong before, and
// each failed silently rather than loudly.
//
// The predecessor of this design put workspace-sync in .spec.containers and had
// the operator POST /flush at the pod IP. That could never work: the sidecar
// binds loopback, and the flush fired on CR deletion — long after the pod was
// reaped. Nothing caught it because the path never ran.
func TestWorkspaceSyncIsANativeSidecar(t *testing.T) {
	r := &EphemeralJobReconciler{}
	p, _ := ResolvePlacement("home")

	ej := &computev1alpha1.EphemeralJob{
		Spec: computev1alpha1.EphemeralJobSpec{
			Image:                "example.com/img@sha256:" + strings.Repeat("a", 64),
			WorkspacePersistence: &computev1alpha1.WorkspacePersistenceSpec{WorkspaceID: "app-1", AppID: "app-1"},
		},
	}
	spec := r.buildPodSpec(context.Background(), ej, p, corev1.Container{Name: "workload"})

	// 1. It must NOT be an ordinary container. A never-exiting process in
	//    .spec.containers means a mode: Job pod can never reach Succeeded,
	//    because a pod succeeds only when every container has exited.
	for _, c := range spec.Containers {
		if c.Name == "workspace-sync" {
			t.Fatal("workspace-sync is in .spec.containers; a mode: Job pod with a persisted " +
				"workspace could never complete, because this process never exits")
		}
	}

	var sync *corev1.Container
	for i := range spec.InitContainers {
		switch spec.InitContainers[i].Name {
		case "workspace-sync":
			sync = &spec.InitContainers[i]
		case "workspace-restore":
			// A separate restore init container cannot work under §14.2: a FUSE
			// mount dies with the mount namespace of the container that made
			// it, so this one could only download an archive the sidecar
			// downloads again, or mount an overlay destroyed before the
			// workload ever starts.
			t.Error("a workspace-restore init container is present; restore has exactly one " +
				"owner, and it is the native sidecar that can keep the mount alive")
		}
	}
	if sync == nil {
		t.Fatal("no workspace-sync container in .spec.initContainers")
	}

	// 2. restartPolicy: Always is the whole difference between a native sidecar
	//    and a plain init container that would block the pod from ever starting.
	if sync.RestartPolicy == nil || *sync.RestartPolicy != corev1.ContainerRestartPolicyAlways {
		t.Errorf("workspace-sync restartPolicy = %v, want Always — without it this is a plain "+
			"init container and the pod hangs before the workload ever starts", sync.RestartPolicy)
	}

	// 3. A startup probe, and specifically an EXEC one.
	//
	// This reverses an earlier decision, because §14.2 changed what the sidecar
	// does. It now performs the restore the workload depends on, so the
	// workload must not start until that finishes — and for a native sidecar,
	// the startup probe is what the kubelet gates the next container on.
	//
	// It must not be httpGet: the kubelet probes from the node against the pod
	// IP, and this process binds 127.0.0.1, so an HTTP probe can only ever
	// fail. Liveness and readiness stay absent — a wedged or unreachable object
	// store is degraded durability, not a reason to restart a healthy sidecar
	// or to deny the user their workspace.
	if sync.StartupProbe == nil {
		t.Error("workspace-sync has no startupProbe; the workload would start against a " +
			"workspace that is still being restored")
	} else {
		if sync.StartupProbe.Exec == nil {
			t.Error("workspace-sync startupProbe is not exec; an httpGet probe cannot reach a " +
				"listener bound to 127.0.0.1 and would fail forever")
		}
		if sync.StartupProbe.HTTPGet != nil {
			t.Error("workspace-sync startupProbe uses httpGet against a loopback-bound server")
		}
	}
	if sync.LivenessProbe != nil || sync.ReadinessProbe != nil {
		t.Error("workspace-sync declares a liveness or readiness probe; neither can reach a " +
			"loopback-bound server, and readiness on a native sidecar gates the workload")
	}
}

// TestPersistedWorkspaceGetsAGracePeriodFloor guards the window the teardown
// checkpoint runs in. The kubelet SIGKILLs whatever is still running when the
// grace period expires, so the default 30s is a ceiling on the final snapshot
// that nobody chose.
func TestPersistedWorkspaceGetsAGracePeriodFloor(t *testing.T) {
	r := &EphemeralJobReconciler{}
	p, _ := ResolvePlacement("home")
	img := "example.com/img@sha256:" + strings.Repeat("a", 64)

	// Unset: the floor applies.
	spec := r.buildPodSpec(context.Background(), &computev1alpha1.EphemeralJob{
		Spec: computev1alpha1.EphemeralJobSpec{
			Image:                img,
			WorkspacePersistence: &computev1alpha1.WorkspacePersistenceSpec{WorkspaceID: "app-1", AppID: "app-1"},
		},
	}, p, corev1.Container{Name: "workload"})
	if spec.TerminationGracePeriodSeconds == nil || *spec.TerminationGracePeriodSeconds != minWorkspaceGraceSeconds {
		t.Errorf("grace period = %v, want the %ds floor — the teardown checkpoint would be "+
			"SIGKILLed at the 30s default", spec.TerminationGracePeriodSeconds, minWorkspaceGraceSeconds)
	}

	// A fleet asking for MORE keeps it: this is a floor, not an override.
	longer := int64(minWorkspaceGraceSeconds + 300)
	spec = r.buildPodSpec(context.Background(), &computev1alpha1.EphemeralJob{
		Spec: computev1alpha1.EphemeralJobSpec{
			Image:                         img,
			WorkspacePersistence:          &computev1alpha1.WorkspacePersistenceSpec{WorkspaceID: "app-1", AppID: "app-1"},
			TerminationGracePeriodSeconds: &longer,
		},
	}, p, corev1.Container{Name: "workload"})
	if spec.TerminationGracePeriodSeconds == nil || *spec.TerminationGracePeriodSeconds != longer {
		t.Errorf("grace period = %v, want the fleet's %d preserved", spec.TerminationGracePeriodSeconds, longer)
	}

	// And a job without persistence is untouched by any of this.
	shorter := int64(5)
	spec = r.buildPodSpec(context.Background(), &computev1alpha1.EphemeralJob{
		Spec: computev1alpha1.EphemeralJobSpec{
			Image:                         img,
			TerminationGracePeriodSeconds: &shorter,
		},
	}, p, corev1.Container{Name: "workload"})
	if spec.TerminationGracePeriodSeconds == nil || *spec.TerminationGracePeriodSeconds != shorter {
		t.Errorf("grace period = %v on a job with no persisted workspace, want the fleet's %d untouched",
			spec.TerminationGracePeriodSeconds, shorter)
	}
}

// TestKeepCheckpointsIsFleetSelectableAndBounded covers the one storage knob a
// fleet may set (ADR-052 §14.2).
//
// Both halves matter. Unset must mean the default rather than zero — and zero
// reaching the sidecar would mean "keep only the newest", quietly destroying a
// fleet's history. Out-of-range must clamp rather than pass through: the CRD's
// minimum/maximum reject those at admission, but a CR applied before the schema
// was updated would otherwise arrive here unvalidated.
func TestKeepCheckpointsIsFleetSelectableAndBounded(t *testing.T) {
	keep := func(n int32) *int32 { return &n }

	tests := []struct {
		name string
		in   *int32
		want int32
	}{
		{"unset uses the default", nil, computev1alpha1.DefaultKeepCheckpoints},
		{"a fleet's own value is honoured", keep(20), 20},
		{"the minimum is allowed", keep(1), 1},
		{"the maximum is allowed", keep(50), 50},
		{"zero clamps up, never to 'keep only the newest'", keep(0), minKeepCheckpoints},
		{"negative clamps up", keep(-3), minKeepCheckpoints},
		{"above the bound clamps down", keep(10000), maxKeepCheckpoints},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveKeepCheckpoints(&computev1alpha1.WorkspacePersistenceSpec{
				WorkspaceID: "app-1", AppID: "app-1", KeepCheckpoints: tc.in,
			})
			if got != tc.want {
				t.Errorf("resolveKeepCheckpoints = %d, want %d", got, tc.want)
			}
		})
	}

	if got := resolveKeepCheckpoints(nil); got != computev1alpha1.DefaultKeepCheckpoints {
		t.Errorf("resolveKeepCheckpoints(nil) = %d, want the %d default",
			got, computev1alpha1.DefaultKeepCheckpoints)
	}
}

// TestKeepCheckpointsReachesTheSidecar closes the loop: a value accepted in the
// CR is worthless if it never reaches the process that acts on it.
//
// It is always set explicitly, even at the default, so the retention governing
// a running pod is visible in `kubectl describe` rather than implied by a
// fallback inside the sidecar.
func TestKeepCheckpointsReachesTheSidecar(t *testing.T) {
	r := &EphemeralJobReconciler{}
	p, _ := ResolvePlacement("home")
	img := "example.com/img@sha256:" + strings.Repeat("a", 64)

	env := func(ws *computev1alpha1.WorkspacePersistenceSpec) string {
		spec := r.buildPodSpec(context.Background(), &computev1alpha1.EphemeralJob{
			Spec: computev1alpha1.EphemeralJobSpec{Image: img, WorkspacePersistence: ws},
		}, p, corev1.Container{Name: "workload"})
		for _, c := range spec.InitContainers {
			if c.Name != "workspace-sync" {
				continue
			}
			for _, e := range c.Env {
				if e.Name == "KEEP_CHECKPOINTS" {
					return e.Value
				}
			}
			t.Fatal("workspace-sync has no KEEP_CHECKPOINTS env; the fleet's retention " +
				"choice would be silently ignored")
		}
		t.Fatal("no workspace-sync container")
		return ""
	}

	if got := env(&computev1alpha1.WorkspacePersistenceSpec{WorkspaceID: "app-1", AppID: "app-1"}); got != "5" {
		t.Errorf("KEEP_CHECKPOINTS with no fleet value = %q, want \"5\"", got)
	}
	n := int32(12)
	if got := env(&computev1alpha1.WorkspacePersistenceSpec{
		WorkspaceID: "app-1", AppID: "app-1", KeepCheckpoints: &n,
	}); got != "12" {
		t.Errorf("KEEP_CHECKPOINTS with a fleet value of 12 = %q, want \"12\"", got)
	}
	// §14.8: keeping everything is a word the sidecar reads as "no bound",
	// never a number it could read as a count.
	if got := env(&computev1alpha1.WorkspacePersistenceSpec{
		WorkspaceID: "app-1", AppID: "app-1", RetainAllCheckpoints: true,
	}); got != "unbounded" {
		t.Errorf("KEEP_CHECKPOINTS with retainAllCheckpoints = %q, want \"unbounded\"", got)
	}
}

// TestDefaultKeepCheckpointsMatchesTheSidecar guards a number that exists twice.
//
// The operator and workspace-sync are separate Go modules and cannot import one
// another, so this constant is duplicated. The operator always sets the env var
// explicitly, so a drift would not change behaviour today — it would make the
// two sources of truth disagree about what "the default" is, which is how the
// next reader gets it wrong.
func TestDefaultKeepCheckpointsMatchesTheSidecar(t *testing.T) {
	// Mirrors store.DefaultKeepCheckpoints in
	// operators/workspace-sync/internal/store/store.go.
	const sidecarDefault int32 = 5
	if computev1alpha1.DefaultKeepCheckpoints != sidecarDefault {
		t.Errorf("api DefaultKeepCheckpoints = %d but workspace-sync uses %d; update both",
			computev1alpha1.DefaultKeepCheckpoints, sidecarDefault)
	}
}

// TestPinnedReadOnlyWorkspaceReachesTheSidecar covers §14.3, where getting it
// wrong is destructive rather than merely incorrect.
//
// A build that inherits read-write persistence uploads its node_modules and
// dist as checkpoints via the backstop and the teardown snapshot, and retention
// then evicts the user's real checkpoints to make room. With source history
// living only in S3 (§14.2), that loss has no second copy.
func TestPinnedReadOnlyWorkspaceReachesTheSidecar(t *testing.T) {
	r := &EphemeralJobReconciler{}
	p, _ := ResolvePlacement("home")
	img := "example.com/img@sha256:" + strings.Repeat("a", 64)

	envOf := func(ws *computev1alpha1.WorkspacePersistenceSpec) map[string]string {
		spec := r.buildPodSpec(context.Background(), &computev1alpha1.EphemeralJob{
			Spec: computev1alpha1.EphemeralJobSpec{Image: img, WorkspacePersistence: ws},
		}, p, corev1.Container{Name: "workload"})
		for _, c := range spec.InitContainers {
			if c.Name != "workspace-sync" {
				continue
			}
			out := map[string]string{}
			for _, e := range c.Env {
				out[e.Name] = e.Value
			}
			return out
		}
		t.Fatal("no workspace-sync container")
		return nil
	}

	// A build: pinned and read-only.
	build := envOf(&computev1alpha1.WorkspacePersistenceSpec{
		WorkspaceID: "app-1", CheckpointID: "abc123", ReadOnly: true,
	})
	if build["CHECKPOINT_ID"] != "abc123" {
		t.Errorf("CHECKPOINT_ID = %q, want abc123 — unpinned, a build races the session that "+
			"triggered it and ships unreviewed code", build["CHECKPOINT_ID"])
	}
	if build["WORKSPACE_READ_ONLY"] != "true" {
		t.Errorf("WORKSPACE_READ_ONLY = %q, want \"true\" — without it this job's build output is "+
			"checkpointed over the user's workspace history", build["WORKSPACE_READ_ONLY"])
	}

	// An ordinary sandbox: neither field set, so its describe output stays free
	// of variables that only restate the default.
	sandbox := envOf(&computev1alpha1.WorkspacePersistenceSpec{WorkspaceID: "app-1", AppID: "app-1"})
	if _, ok := sandbox["CHECKPOINT_ID"]; ok {
		t.Error("CHECKPOINT_ID is set on an unpinned sandbox; it must restore latest")
	}
	if _, ok := sandbox["WORKSPACE_READ_ONLY"]; ok {
		t.Error("WORKSPACE_READ_ONLY is set on an ordinary sandbox; it must be read-write")
	}

	// Undo: pinned but writable — the user keeps working from that point.
	undo := envOf(&computev1alpha1.WorkspacePersistenceSpec{
		WorkspaceID: "app-1", AppID: "app-1", CheckpointID: "abc123",
	})
	if undo["CHECKPOINT_ID"] != "abc123" {
		t.Errorf("CHECKPOINT_ID = %q, want abc123", undo["CHECKPOINT_ID"])
	}
	if _, ok := undo["WORKSPACE_READ_ONLY"]; ok {
		t.Error("WORKSPACE_READ_ONLY set on an undo; a restored workspace must remain writable")
	}
}

// TestAppIDReachesTheSidecar closes the loop on the key root (§14.4).
//
// APP_ID is half the prefix every object lives under. A missing or wrong value
// does not error anywhere — it addresses a workspace nobody else can see, and
// surfaces much later as an empty restore.
func TestAppIDReachesTheSidecar(t *testing.T) {
	r := &EphemeralJobReconciler{}
	p, _ := ResolvePlacement("home")

	spec := r.buildPodSpec(context.Background(), &computev1alpha1.EphemeralJob{
		Spec: computev1alpha1.EphemeralJobSpec{
			Image: "example.com/img@sha256:" + strings.Repeat("a", 64),
			WorkspacePersistence: &computev1alpha1.WorkspacePersistenceSpec{
				WorkspaceID: "ws-42", AppID: "app-123",
			},
		},
	}, p, corev1.Container{Name: "workload"})

	for _, c := range spec.InitContainers {
		if c.Name != "workspace-sync" {
			continue
		}
		got := map[string]string{}
		for _, e := range c.Env {
			got[e.Name] = e.Value
		}
		if got["APP_ID"] != "app-123" {
			t.Errorf("APP_ID = %q, want app-123 — without it the sidecar cannot build "+
				"<appId>/<workspaceId> and refuses to start", got["APP_ID"])
		}
		if got["WORKSPACE_ID"] != "ws-42" {
			t.Errorf("WORKSPACE_ID = %q, want ws-42", got["WORKSPACE_ID"])
		}
		return
	}
	t.Fatal("no workspace-sync container")
}

// TestWorkspaceSpecRequiresBothIds guards the CRD contract in Go, so a struct
// change cannot quietly make the app root optional again.
func TestWorkspaceSpecRequiresBothIds(t *testing.T) {
	st := reflect.TypeOf(computev1alpha1.WorkspacePersistenceSpec{})
	for _, name := range []string{"WorkspaceID", "AppID"} {
		f, ok := st.FieldByName(name)
		if !ok {
			t.Fatalf("WorkspacePersistenceSpec has no %s", name)
		}
		// Required means no `omitempty`: with it, an empty value marshals away
		// and the API server's required check never sees a missing field.
		if strings.Contains(f.Tag.Get("json"), "omitempty") {
			t.Errorf("%s is tagged omitempty; both ids are required to address a workspace (§14.4)", name)
		}
	}
}

// TestPodStartupBlockerNamesTheCause covers the diagnostic that was missing
// when the workspace-sync sidecar crash-looped.
//
// The CR read `Provisioning / CapacityAvailable=Scheduled()` while the real
// cause — a sidecar exiting on a TLS failure — was visible only in a container
// status nobody reads until they already suspect it. What the user saw was a
// chat request timing out with a 504, several layers away.
func TestPodStartupBlockerNamesTheCause(t *testing.T) {
	waiting := func(name, reason string, last *corev1.ContainerStateTerminated) corev1.ContainerStatus {
		cs := corev1.ContainerStatus{
			Name:  name,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason}},
		}
		if last != nil {
			cs.LastTerminationState = corev1.ContainerState{Terminated: last}
		}
		return cs
	}

	// The observed failure: a crash-looping INIT container, with the process's
	// own reason in its last termination message.
	pod := &corev1.Pod{Status: corev1.PodStatus{
		InitContainerStatuses: []corev1.ContainerStatus{
			waiting("workspace-sync", "CrashLoopBackOff", &corev1.ContainerStateTerminated{
				ExitCode: 1, Message: "cannot reach object storage: x509: certificate signed by unknown authority",
			}),
		},
	}}
	got := podStartupBlocker(pod)
	for _, want := range []string{"workspace-sync", "CrashLoopBackOff", "x509"} {
		if !strings.Contains(got, want) {
			t.Errorf("message %q does not mention %q — it has to name the container and the reason", got, want)
		}
	}

	// Init containers take precedence: under §14.1 the workspace sidecar must be
	// ready before the workload starts, so when both look unhappy the init
	// container is the cause and the workload is the symptom.
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{waiting("workload", "CrashLoopBackOff", nil)}
	if got := podStartupBlocker(pod); !strings.Contains(got, "workspace-sync") {
		t.Errorf("reported %q; the init container is the cause when both are waiting", got)
	}

	// Transient states must stay SILENT. Every pod passes through these, and
	// reporting them would turn a normal start into an alarming message.
	for _, reason := range []string{"ContainerCreating", "PodInitializing"} {
		p := &corev1.Pod{Status: corev1.PodStatus{
			InitContainerStatuses: []corev1.ContainerStatus{waiting("workspace-sync", reason, nil)},
		}}
		if got := podStartupBlocker(p); got != "" {
			t.Errorf("reason %q reported as a blocker (%q); it is an ordinary transient state", reason, got)
		}
	}

	// A healthy pod reports nothing.
	if got := podStartupBlocker(&corev1.Pod{}); got != "" {
		t.Errorf("empty pod reported %q, want no blocker", got)
	}
}
