package controller

import (
	"context"
	"strings"
	"testing"

	computev1alpha1 "github.com/kamarkat/kamarkat/operators/ephemeral-job-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

func envMap(c corev1.Container) map[string]string {
	m := map[string]string{}
	for _, v := range c.Env {
		m[v.Name] = v.Value
	}
	return m
}

// TestSharedWorkspaceIsTheSameMechanismPointedElsewhere pins the properties that
// make a second sync instance safe to run in the same pod.
//
// Each assertion here stands for a failure that is silent rather than loud:
// a shared readiness marker makes one instance's probe pass on the other's
// restore; the wrong mount propagation writes beneath an overlay where nothing
// reading /workspace can see it; and a privileged container copied for a
// capability it never uses widens the blast radius §19.6 exists to bound.
func TestSharedWorkspaceIsTheSameMechanismPointedElsewhere(t *testing.T) {
	ws := &computev1alpha1.WorkspacePersistenceSpec{
		WorkspaceID:       "playground-abc",
		AppID:             "app123",
		SharedWorkspaceID: "globals",
	}

	session := workspaceSyncContainer(ws, 5, "default", "fleet-a")
	shared := workspaceSyncContainerFor(ws, 5, "default", "fleet-a", workspaceSyncTarget{
		name:        "workspace-sync-shared",
		workspaceID: ws.SharedWorkspaceID,
		root:        WorkspaceMountPath + "/" + sharedWorkspaceDirName,
		staging:     workspaceStagingPath + "/" + sharedWorkspaceDirName,
		noArchive:   true,
	})

	se, sh := envMap(session), envMap(shared)

	// Same app root: both trees belong to one app, which is what allows an app
	// to be deleted with a single prefix operation (§14.4).
	if se["APP_ID"] != sh["APP_ID"] || sh["APP_ID"] != "app123" {
		t.Errorf("app root must match: session=%q shared=%q", se["APP_ID"], sh["APP_ID"])
	}

	// Different workspace ids, which is the whole point: a distinct prefix,
	// manifest chain and LATEST pointer, shared by every session of the app.
	if se["WORKSPACE_ID"] == sh["WORKSPACE_ID"] {
		t.Fatalf("both instances claim workspace %q; the shared tree would be a per-session copy", se["WORKSPACE_ID"])
	}
	if sh["WORKSPACE_ID"] != "globals" {
		t.Errorf("shared workspace id = %q, want the value the fleet asked for", sh["WORKSPACE_ID"])
	}

	// Roots: the shared tree lives inside the session tree, at the directory
	// workspace-sync's skipDir excludes from the session's own snapshot.
	if se["WORKSPACE_ROOT"] != WorkspaceMountPath {
		t.Errorf("session root = %q", se["WORKSPACE_ROOT"])
	}
	if want := WorkspaceMountPath + "/" + sharedWorkspaceDirName; sh["WORKSPACE_ROOT"] != want {
		t.Errorf("shared root = %q, want %q", sh["WORKSPACE_ROOT"], want)
	}

	// Loopback port must not be shared. Containers in a pod share a network
	// namespace, so the second binder exits with "address already in use" and
	// crashloops the pod before the workload starts — observed on a live pod.
	if se["WORKSPACE_SYNC_PORT"] == sh["WORKSPACE_SYNC_PORT"] {
		t.Fatalf("both instances bind port %q; the second cannot listen and the pod never starts",
			se["WORKSPACE_SYNC_PORT"])
	}
	// The session instance keeps the default, because that is the port
	// harness-runtime proxies /workspace/checkpoint to.
	if se["WORKSPACE_SYNC_PORT"] != "7070" {
		t.Errorf("session instance port = %q, want 7070 — harness-runtime's checkpoint proxy targets it",
			se["WORKSPACE_SYNC_PORT"])
	}

	// Staging must not be shared: the readiness marker is named from it.
	if se["STAGING_ROOT"] == sh["STAGING_ROOT"] {
		t.Fatalf("both instances stage in %q; one's .ready marker would satisfy the other's startup probe "+
			"and the workload could start against a tree that was never restored", se["STAGING_ROOT"])
	}
	sessionProbe := session.StartupProbe.ProbeHandler.Exec.Command
	sharedProbe := shared.StartupProbe.ProbeHandler.Exec.Command
	if sessionProbe[len(sessionProbe)-1] == sharedProbe[len(sharedProbe)-1] {
		t.Errorf("startup probes watch the same marker: %v", sessionProbe)
	}

	// NEITHER instance mounts, so neither takes the squashfs path (§14.7).
	//
	// §14.2 gave the fast path to the session instance and privilege with it.
	// Pod Security Admission judges the whole Pod, and this Pod runs tenant
	// code, so that made every workspace-bearing sandbox inadmissible. The
	// archive materialisation moves to platform-owned node storage; what runs
	// here restores from content-addressed objects and needs nothing special.
	for name, env := range map[string]map[string]string{"session": se, "shared": sh} {
		if env["WORKSPACE_NO_ARCHIVE"] != "true" {
			t.Errorf("%s instance must not attempt a mount: it has no device, no setuid helper and no propagation", name)
		}
	}

	// RESTRICTED, identically, both instances. This is the assertion that would
	// have caught §14.2 before it shipped: it encodes the namespace's own
	// pod-security level rather than the operator's intent.
	for name, c := range map[string]corev1.Container{"session": session, "shared": shared} {
		sc := c.SecurityContext
		if sc.Privileged != nil && *sc.Privileged {
			t.Errorf("%s instance is privileged; a tenant namespace enforces restricted and refuses the Pod", name)
		}
		if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
			t.Errorf("%s instance must set runAsNonRoot", name)
		}
		if sc.RunAsUser == nil || *sc.RunAsUser != 1000 {
			t.Errorf("%s instance RunAsUser = %v, want 1000 — it must match the workload that shares the volume",
				name, sc.RunAsUser)
		}
		if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
			t.Errorf("%s instance must not allow privilege escalation", name)
		}
		if sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
			t.Errorf("%s instance must drop ALL capabilities", name)
		}
		if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
			t.Errorf("%s instance must set seccompProfile RuntimeDefault", name)
		}
	}

	// uid 1000 on BOTH is what removes the last reason either held root. The
	// restore chowns the tree it wrote, and a process chowning its own files to
	// its own uid needs no privilege. Running one as root was what made the
	// other's chown fail — "lchown /workspace/.global: operation not permitted"
	// — and that asymmetry is what this loop forbids returning.

	// No propagation on either: with no mount to propagate, Bidirectional is
	// both meaningless and rejected outright on a non-privileged container.
	for name, c := range map[string]corev1.Container{"session": session, "shared": shared} {
		if c.VolumeMounts[0].MountPropagation != nil {
			t.Errorf("%s instance sets mount propagation %v; the volume is shared directly and nothing is mounted",
				name, *c.VolumeMounts[0].MountPropagation)
		}
	}

	// Both carry the same credential channel and retention, by construction.
	for _, k := range []string{"KEEP_CHECKPOINTS", "WORKSPACE_UID"} {
		if se[k] != sh[k] {
			t.Errorf("%s differs: session=%q shared=%q", k, se[k], sh[k])
		}
	}
}

// TestSharedWorkspaceIsOptional: a fleet that does not ask for one gets exactly
// today's pod — one sync container, nothing mounted inside the workspace.
func TestSharedWorkspaceIsOptional(t *testing.T) {
	ws := &computev1alpha1.WorkspacePersistenceSpec{WorkspaceID: "ws1", AppID: "app1"}
	if ws.SharedWorkspaceID != "" {
		t.Fatal("unset SharedWorkspaceID must be empty")
	}
	c := workspaceSyncContainer(ws, 5, "default", "fleet-a")
	if got := envMap(c)["WORKSPACE_ROOT"]; got != WorkspaceMountPath {
		t.Errorf("WORKSPACE_ROOT = %q, want %q", got, WorkspaceMountPath)
	}
	if c.Name != "workspace-sync" {
		t.Errorf("container name changed to %q; the session container's name is a stable identifier", c.Name)
	}
}

// TestCheckpointIntervalIsOptInAndReachesBothInstances covers the gap between an
// orderly shutdown and every other way a pod dies.
//
// The teardown checkpoint handles SIGTERM — idle reap, CR delete, drain — and is
// the only application-consistent one in the design. It cannot help with
// SIGKILL, an OOM kill or node loss, where nothing runs at all. For an
// unattended fleet that is the difference between losing one interval and losing
// an entire pipeline run, because there is no person present to save.
func TestCheckpointIntervalIsOptInAndReachesBothInstances(t *testing.T) {
	// Unset: no periodic upload. This is what an interactive fleet wants — a
	// timer checkpointing a tree mid-edit mostly records half-finished states.
	off := &computev1alpha1.WorkspacePersistenceSpec{WorkspaceID: "ws1", AppID: "app1"}
	if v := envMap(workspaceSyncContainer(off, 5, "default", "fleet-a"))["WORKSPACE_SYNC_INTERVAL_SECONDS"]; v != "" {
		t.Errorf("interval = %q with none requested; the periodic backstop must be opt-in", v)
	}

	every := int32(120)
	on := &computev1alpha1.WorkspacePersistenceSpec{
		WorkspaceID: "ws1", AppID: "app1", SharedWorkspaceID: "globals",
		CheckpointIntervalSeconds: &every,
	}
	session := envMap(workspaceSyncContainer(on, 5, "default", "fleet-a"))
	shared := envMap(workspaceSyncContainerFor(on, 5, "default", "fleet-a", workspaceSyncTarget{
		name: "workspace-sync-shared", workspaceID: "globals",
		root:    WorkspaceMountPath + "/" + sharedWorkspaceDirName,
		staging: workspaceStagingPath + "/" + sharedWorkspaceDirName,
		port:    sharedWorkspaceSyncPort, noArchive: true,
	}))

	if session["WORKSPACE_SYNC_INTERVAL_SECONDS"] != "120" {
		t.Errorf("session interval = %q, want 120", session["WORKSPACE_SYNC_INTERVAL_SECONDS"])
	}
	// The app-scoped tree needs it just as much: a brand brief written once at
	// the start of a run is exactly what a crash an hour later would take.
	if shared["WORKSPACE_SYNC_INTERVAL_SECONDS"] != "120" {
		t.Errorf("shared interval = %q, want 120 — app-scoped artifacts are lost the same way",
			shared["WORKSPACE_SYNC_INTERVAL_SECONDS"])
	}
}

// ── Shared workspace read-only (ADR-052 §14.3) ───────────────────────────────
//
// A session that writes its own workspace and only READS the shared one had no
// way to say so: ReadOnly governs both instances. So the shared instance
// checkpointed on every timer and at teardown, and a session holding an older
// copy overwrote a newer one another session had written. oranger lost a brand
// brief that way on 2026-10-01.

func sharedReadOnlyEJ(sharedReadOnly, allReadOnly bool) *computev1alpha1.EphemeralJob {
	return &computev1alpha1.EphemeralJob{
		Spec: computev1alpha1.EphemeralJobSpec{
			Image: "example.com/img@sha256:" + strings.Repeat("a", 64),
			WorkspacePersistence: &computev1alpha1.WorkspacePersistenceSpec{
				WorkspaceID:             "session-1",
				AppID:                   "app-1",
				SharedWorkspaceID:       "globals",
				SharedWorkspaceReadOnly: sharedReadOnly,
				ReadOnly:                allReadOnly,
			},
		},
	}
}

func syncContainers(spec corev1.PodSpec) (session, shared *corev1.Container) {
	for i := range spec.InitContainers {
		switch spec.InitContainers[i].Name {
		case "workspace-sync":
			session = &spec.InitContainers[i]
		case "workspace-sync-shared":
			shared = &spec.InitContainers[i]
		}
	}
	return
}

// The whole point: one instance silenced, the other not.
func TestSharedWorkspaceReadOnlyLeavesTheSessionWorkspaceWritable(t *testing.T) {
	r := &EphemeralJobReconciler{}
	p, _ := ResolvePlacement("home")

	spec := r.buildPodSpec(context.Background(), sharedReadOnlyEJ(true, false), p,
		corev1.Container{Name: "workload"})

	session, shared := syncContainers(spec)
	if session == nil || shared == nil {
		t.Fatalf("expected both sync instances, got session=%v shared=%v", session != nil, shared != nil)
	}
	if v, ok := envMap(*shared)["WORKSPACE_READ_ONLY"]; !ok || v != "true" {
		t.Fatalf("shared instance WORKSPACE_READ_ONLY = %q (set=%v), want true", v, ok)
	}
	// The session workspace is this sandbox's own. Silencing it too would lose
	// the session's work, which is the opposite failure.
	if _, ok := envMap(*session)["WORKSPACE_READ_ONLY"]; ok {
		t.Fatal("session instance was made read-only by a SHARED-only setting")
	}
}

// The existing spec-wide flag must keep governing both, so nothing that relied on
// it changes behaviour.
func TestSpecWideReadOnlyStillSilencesBothInstances(t *testing.T) {
	r := &EphemeralJobReconciler{}
	p, _ := ResolvePlacement("home")

	spec := r.buildPodSpec(context.Background(), sharedReadOnlyEJ(false, true), p,
		corev1.Container{Name: "workload"})

	session, shared := syncContainers(spec)
	for name, c := range map[string]*corev1.Container{"session": session, "shared": shared} {
		if c == nil {
			t.Fatalf("no %s instance", name)
		}
		if v, ok := envMap(*c)["WORKSPACE_READ_ONLY"]; !ok || v != "true" {
			t.Fatalf("%s instance WORKSPACE_READ_ONLY = %q (set=%v), want true", name, v, ok)
		}
	}
}

// Default unchanged: both instances checkpoint, as every existing sandbox does.
func TestNeitherInstanceIsReadOnlyByDefault(t *testing.T) {
	r := &EphemeralJobReconciler{}
	p, _ := ResolvePlacement("home")

	spec := r.buildPodSpec(context.Background(), sharedReadOnlyEJ(false, false), p,
		corev1.Container{Name: "workload"})

	session, shared := syncContainers(spec)
	for name, c := range map[string]*corev1.Container{"session": session, "shared": shared} {
		if _, ok := envMap(*c)["WORKSPACE_READ_ONLY"]; ok {
			t.Fatalf("%s instance is read-only without being asked", name)
		}
	}
}
