package controller

import (
	"context"
	"os"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	computev1alpha1 "github.com/kamarkat/kamarkat/operators/ephemeral-job-operator/api/v1alpha1"
)

// The bounds the CRD advertises, restated here because this operator clamps to
// them rather than trusting that admission enforced them.
const (
	minKeepCheckpoints int32 = 1
	maxKeepCheckpoints int32 = 50
	// unboundedKeepCheckpoints is resolveKeepCheckpoints' answer for
	// RetainAllCheckpoints. Never a value a fleet can write: the CRD's minimum
	// for keepCheckpoints is 1.
	unboundedKeepCheckpoints int32 = 0
)

// The platform's workspace persistence agent (ADR-052 §14, §14.2). Image and
// secret are operator configuration, never fleet input — a fleet that could name
// either could substitute its own agent or point it at its own bucket.
var (
	workspaceSyncImage = envOr("WORKSPACE_SYNC_IMAGE", "workspace-sync:dev")
	// The object-store credential, by reference (§19.6).
	//
	// A TEMPLATE, not a name. `{namespace}` expands to the EphemeralJob's own
	// namespace at reconcile time.
	//
	// This operator serves every fleet on the spoke, and each one keeps its S3
	// credential in its own Secret in its own namespace. A literal name here
	// would be a tenant identifier in platform code — which ADR-047's addendum
	// forbids, and which the preflight check rejects — and it would also be
	// simply wrong for the second fleet onboarded.
	//
	// The tenant namespace IS the fleet id (the registry renders workloads into
	// `namespace: {TENANT_ID}`), so `{namespace}-app-secrets` resolves to
	// exactly the Secret each fleet's registry already declares — for waypoint,
	// the one its own SDK binds S3_* from
	// (workloads/base/sdk/rollout-patch.yaml, rendered by the ExternalSecret in
	// values.yaml). Reusing the fleet's own Secret rather than rendering a
	// parallel platform-owned one means a credential rotation lands in one
	// place, and the sidecar and the SDK cannot disagree about which bucket a
	// workspace lives in.
	//
	// Two earlier values were wrong, and both failed SILENTLY, because every
	// reference the operator injects is optional (a cluster without Infisical
	// must still start a sandbox): the sidecar simply reported "object storage
	// not configured" and no-opped every checkpoint.
	//   - `hcloud-token` — a KEY, not a Secret name.
	//   - `hetzner-credentials` — a real Secret, but platform INFRASTRUCTURE
	//     (hub-operator's constant: "For CAPI/CCM/CSI"). It carries S3 keys only
	//     in waypoint's local Kind manifest; no registry binds it for an SDK, so
	//     it worked locally and in no deployed environment.
	//   - `{namespace}-app-secrets` — the THIRD wrong value, and wrong in a new
	//     way: no Secret of that name exists in any tenant namespace. The
	//     reasoning above ("the Secret each fleet's registry already declares")
	//     was simply not true of any fleet. Verified on nutgraf-01: the namespace
	//     holds no `*-app-secrets` at all.
	//
	// THE REAL SHAPE: a fleet splits these across TWO objects, and no single
	// name can satisfy the sidecar.
	//
	//   credentials  Secret     <appId>-sdk-secrets   S3_ACCESS_KEY_ID,
	//                                                 S3_SECRET_ACCESS_KEY
	//   location     ConfigMap  <appId>-config        S3_ENDPOINT_URL,
	//                                                 S3_BUCKET_NAME
	//
	// That is how the SDK itself binds them, read off the running rollout rather
	// than inferred. So the sidecar reads both, from the same two objects, and a
	// rotation or a bucket change still lands in exactly one place each.
	//
	// Keyed on appId, NOT namespace. After ADR-088 the namespace is
	// `tenant-<tenantId>-<appId>` while the fleet's own objects are named from the
	// appId alone, so `{namespace}-sdk-secrets` would resolve to
	// `tenant-nutgraf-waypoint-sdk-secrets` — a fourth name that does not exist.
	// `{namespace}` is still accepted for a deployment that wants it.
	workspaceSyncSecretTemplate = envOr("WORKSPACE_SYNC_SECRET", "{fleetId}-sdk-secrets")

	// The fleet's non-secret S3 location. Separate object, separate template:
	// endpoint and bucket are not credentials and the fleet does not store them
	// as such, so injecting them from a Secret was always going to miss.
	workspaceSyncConfigTemplate = envOr("WORKSPACE_SYNC_CONFIG", "{fleetId}-config")
)

// resolveWorkspaceSyncSecret expands the template for one job's namespace.
//
// A deployment that sets WORKSPACE_SYNC_SECRET to a literal name still works —
// the expansion is a no-op on a string with no placeholder — which is what a
// spoke with a single fleet and a non-conventional Secret name needs.
const (
	// workspaceStagingPath is the sync container's scratch mount. Each instance
	// gets its OWN subdirectory of it, because the readiness marker
	// (<staging>/.ready) and the archive/FUSE working dirs are named from it —
	// two instances sharing one staging dir would see each other's marker and
	// report ready before their own restore had run.
	workspaceStagingPath = "/ws-staging"

	// workspaceSyncPort is the loopback port the session instance serves on, and
	// the one harness-runtime proxies /workspace/checkpoint to. Left at the
	// binary's own default so there is one number, not two that must agree.
	workspaceSyncPort = "7070"

	// sharedWorkspaceSyncPort is the SECOND instance's port.
	//
	// Containers in a pod share a network namespace, so two instances binding
	// the same loopback port is not two listeners — it is the second one exiting
	// with "address already in use" and crashlooping the pod before the workload
	// starts. code-builders never hit this because it runs exactly one instance.
	//
	// The session instance keeps 7070 deliberately: it owns the tree that
	// /workspace/checkpoint refers to, so harness-runtime's default needs no
	// knowledge of this one.
	sharedWorkspaceSyncPort = "7071"

	// sharedWorkspaceDirName is where the shared workspace is mounted inside the
	// session's tree. It MUST match workspace-sync's skipDir entry: that is what
	// keeps the session's own snapshot from also capturing these files.
	sharedWorkspaceDirName = ".global"
)

func resolveWorkspaceSyncSecret(namespace, fleetID string) string {
	return expandWorkspaceSyncRef(workspaceSyncSecretTemplate, namespace, fleetID)
}

// resolveWorkspaceSyncConfig expands the ConfigMap template for one job.
func resolveWorkspaceSyncConfig(namespace, fleetID string) string {
	return expandWorkspaceSyncRef(workspaceSyncConfigTemplate, namespace, fleetID)
}

// expandWorkspaceSyncRef substitutes both placeholders.
//
// A literal name with no placeholder passes through unchanged, which is what a
// spoke with a single fleet and a non-conventional name needs. An appId that is
// empty leaves `{appId}` unexpanded rather than producing `-sdk-secrets`: a
// reference to a Secret named `-sdk-secrets` is a silent miss, and a visibly
// wrong name in `kubectl describe` is the better failure.
// An appId is lowercased on the way in. They are ULIDs, which are uppercase by
// specification, and a Secret or ConfigMap reference is an RFC 1123 subdomain —
// lower case only. An uppercase appId therefore produced a name the API server
// rejects, and the rejection lands on the POD, not on the EphemeralJob: the CR
// keeps an empty status with no events while the controller retries with
// backoff, so a sandbox that can never be admitted looks like one still being
// provisioned. The objects themselves can only ever carry the lowercase form,
// for the same reason, so lowercasing here is what makes the reference resolve.
func expandWorkspaceSyncRef(template, namespace, fleetID string) string {
	out := strings.ReplaceAll(template, "{namespace}", namespace)
	// Unconditional. The guard that used to stand here left `{appId}` in the
	// rendered name whenever appID was empty, which is a fallback masquerading as
	// safety: a reference to a Secret literally named `{appId}-sdk-secrets`
	// resolves to nothing, and because every reference the operator injects is
	// optional that produces empty env and a sidecar reporting "object storage not
	// configured" -- the exact silent failure this whole path has produced four
	// times. appId is required by the CRD (MinLength=1), so an empty value is not
	// a state to degrade into; if one ever arrives, a visibly wrong name in
	// `kubectl describe` is the failure worth having.
	//
	// Lowercased because these become Kubernetes object names and must be RFC 1123
	// subdomains. An uppercase identifier builds a reference the API server
	// refuses, and the refusal lands on the POD -- so the EphemeralJob sits with an
	// empty status, reading as "still provisioning" rather than as a bad name.
	return strings.ReplaceAll(out, "{fleetId}", strings.ToLower(fleetID))
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// workspaceSyncContainers builds the restore init container and the sync
// sidecar for a persisted workspace. BOTH belong in `.spec.initContainers`.
//
// The sidecar is a NATIVE sidecar — an init container carrying
// `restartPolicy: Always` — and that is load-bearing in three separate ways
// (ADR-052 §14):
//
//  1. Job completion. As an ordinary entry in `.spec.containers`, this
//     process would never exit, and a pod reaches Succeeded only when every
//     one of its containers has. Every `mode: Job` workload asking for a
//     persisted workspace would hang until a deadline killed it. Nothing
//     caught this because every sandbox so far is `mode: Service`.
//
//  2. Termination ordering. The kubelet stops native sidecars only AFTER the
//     regular containers have exited, which is exactly the ordering a final
//     checkpoint needs: the workload stops mutating the tree, and only then
//     is the tree read. An ordinary sidecar gets SIGTERM alongside the
//     workload and would snapshot a workspace still being written.
//
//  3. Startup ordering. Init containers run in order, and a native sidecar
//     starts only after the plain init containers before it have completed.
//     So `workspace-restore` finishes populating the volume before this
//     process starts, and this process is serving before the workload runs.
//
// The ordering in 3 is why the caller must append these two in this order and
// keep the sidecar last.
//
// §14.2 changes: both containers mount a staging emptyDir (`ws-staging`) for
// squashfs archive and FUSE working directories. The sidecar additionally
// requires FUSE device access (device 229) for squashfuse and fuse-overlayfs.
func workspaceSyncContainer(ws *computev1alpha1.WorkspacePersistenceSpec, keepCheckpoints int32, namespace, fleetID string) corev1.Container {
	return workspaceSyncContainerFor(ws, keepCheckpoints, namespace, fleetID, workspaceSyncTarget{
		name:        "workspace-sync",
		workspaceID: ws.WorkspaceID,
		root:        WorkspaceMountPath,
		staging:     workspaceStagingPath,
		port:        workspaceSyncPort,
	})
}

// workspaceSyncTarget is the per-instance part of a sync container: everything
// that differs between the session workspace and the shared one mounted inside
// it. The rest — credentials, retention, security context — is identical by
// construction, which is the point: the shared instance is not a second
// mechanism, it is the same one pointed at a different workspace id.
type workspaceSyncTarget struct {
	name        string
	workspaceID string
	root        string
	staging     string

	// port is the loopback port this instance serves on. Distinct per instance —
	// see sharedWorkspaceSyncPort.
	port string

	// readOnly makes THIS instance restore-only: no periodic backstop, no
	// teardown snapshot, no retention pass. Per instance rather than per spec,
	// because a session that writes its own workspace and only reads the shared
	// one needs exactly one of the two silenced.
	readOnly bool

	// noArchive disables the squashfs fast path. Set for the shared instance,
	// whose root sits inside the session workspace: a FUSE overlay there would
	// nest inside a mount it does not own, and skipping it is what allows the
	// container to run unprivileged.
	noArchive bool

	// refresh enables POST /refresh and the generation layout it needs (ADR-052
	// §14.9). Only for the shared instance of a session that reads it.
	refresh bool
}

func workspaceSyncContainerFor(
	ws *computev1alpha1.WorkspacePersistenceSpec,
	keepCheckpoints int32,
	namespace, fleetID string,
	target workspaceSyncTarget,
) corev1.Container {
	var sideC corev1.Container
	workspaceID := target.workspaceID
	uid := int64(1000)
	// Both run as uid 1000, matching the workload rather than the image's own
	// nonroot uid. The two processes write the same volume, and a uid mismatch
	// produces files the other cannot modify — which on a git tree surfaces as
	// a permission error deep inside an unrelated operation rather than as
	// anything about ownership.
	//
	// The sidecar's SecurityContext is extended with FUSE device access (§14.2)
	// because squashfuse and fuse-overlayfs require /dev/fuse.
	// The FUSE sidecar is PRIVILEGED, and that is the honest cost of §14.2.
	//
	// An earlier version of this asked for `SYS_ADMIN` with
	// allowPrivilegeEscalation=false and runAsNonRoot=true, described in the
	// ADR as "narrower than SYS_ADMIN". That combination cannot mount anything,
	// for four independent reasons:
	//
	//  1. Bidirectional mount propagation — without which the workload cannot
	//     see the overlay at all — is rejected by the kubelet on a
	//     non-privileged container.
	//  2. /dev/fuse does not exist in the container. Kubernetes SecurityContext
	//     has no `devices` field; exposing a device needs a device plugin or
	//     privileged mode. squashfuse fails with "device not found".
	//  3. allowPrivilegeEscalation=false sets NoNewPrivs, which blocks setuid
	//     execution — and squashfuse and fuse-overlayfs both mount through
	//     fusermount3, which is setuid root.
	//  4. A capability added to a container that then setuids to a non-root
	//     user is dropped from the effective set unless the binary carries file
	//     capabilities. These do not.
	//
	// So the choice is privileged or no FUSE, and the ADR should say so rather
	// than describe a middle ground that does not exist.
	//
	// SUPERSEDED BY ADR-052 §14.7. The justification that followed here -- that the
	// container is platform-authored and platform-owned, so the tenant's own
	// container being unprivileged is enough -- does not hold. Pod Security
	// Admission evaluates the WHOLE Pod, and this Pod exists to run tenant
	// code. A privileged container beside that code produces a privileged Pod
	// containing untrusted workload; who wrote the Pod spec is not a trust
	// boundary, and no namespace this Pod could be moved to changes that.
	//
	// The result is that no sandbox requesting a durable workspace is admitted
	// into a tenant namespace at all:
	//
	//   pods "ej-sandbox-..." is forbidden: violates PodSecurity
	//   "restricted:latest": privileged (container "workspace-sync" must not
	//   set securityContext.privileged=true) ...
	//
	// §14.7 moves the mount to platform-owned node storage infrastructure, where
	// the privilege belongs to a component running no tenant code. This container
	// is what that replaces.
	//
	// The SHARED instance is separable and smaller: oranger ADR-002 §5 already
	// decided it runs unprivileged, and it does drop `privileged` -- it holds uid
	// 0 only to chown a reserved directory its sibling created as root. See
	// §14.7's conformance fix.
	// RESTRICTED, both instances, no exceptions (§14.7).
	//
	// §14.2 ran this container `privileged` with `runAsUser: 0` so squashfuse
	// and fuse-overlayfs could mount an archive. Pod Security Admission
	// evaluates the WHOLE Pod and this Pod runs tenant code, so that made every
	// workspace-bearing sandbox inadmissible in a tenant namespace -- the
	// EphemeralJob was created, the operator retried forever, and nothing
	// reported a policy decision.
	//
	// §14.7 takes the exit §14.2 named for itself: "the fallback is the §14
	// content-addressed path, which needs none of it." Restore is file-by-file
	// from content-addressed objects, which needs no device, no setuid helper
	// and no mount propagation -- so none of the four reasons privilege was
	// unavoidable for the archive path applies here.
	//
	// uid 1000 matches the workload and the pod's own RunAsUser. That equality
	// is load-bearing: the two processes write one volume, and a mismatch
	// produces files the other cannot modify. It is also what removes the last
	// reason this container held root -- the restore chowns the tree it just
	// wrote, and a process chowning its own files to its own uid needs no
	// privilege. Running ONE instance as root was what made the other's chown
	// fail; running both as 1000 makes the operation a no-op that succeeds.
	sidecarSec := &corev1.SecurityContext{
		RunAsUser:                &uid,
		RunAsNonRoot:             ptr(true),
		AllowPrivilegeEscalation: ptr(false),
		ReadOnlyRootFilesystem:   ptr(true),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
	// Mapped key by key, not `envFrom`.
	//
	// The key names happen to equal the env names here, so `envFrom` would
	// technically work — but it would also inject every OTHER key the fleet
	// keeps in this Secret (LLM keys, database URLs, provider tokens) into a
	// container that has no business holding them. §19.6 is about what a
	// process can reach, and an explicit map is the only form that bounds it.
	//
	// Every reference is optional for the same reason the SDK's are: a cluster
	// without Infisical (any local Kind cluster) may have no such Secret, and a
	// sandbox must still start there — checkpoints then no-op, which §14 names
	// as the one legitimate no-op.
	secretName := resolveWorkspaceSyncSecret(namespace, fleetID)
	configName := resolveWorkspaceSyncConfig(namespace, fleetID)
	// The location half. A ConfigMap, because that is where the fleet keeps it --
	// endpoint and bucket are not credentials, and reading them from a Secret is
	// what left every checkpoint no-opping while the sidecar reported only
	// "object storage not configured".
	configEnv := func(name, key string) corev1.EnvVar {
		return corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{
			ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: configName},
				Key:                  key,
				Optional:             ptr(true),
			},
		}}
	}
	secretEnv := func(name, key string) corev1.EnvVar {
		return corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
				Key:                  key,
				Optional:             ptr(true),
			},
		}}
	}
	env := []corev1.EnvVar{
		{Name: "WORKSPACE_ID", Value: workspaceID},
		// Which tree this instance owns. Always explicit, even for the session
		// instance where it equals the default: two instances run in this pod
		// and an implied root is one nobody can check in `kubectl describe`.
		{Name: "WORKSPACE_ROOT", Value: target.root},
		// Per-instance scratch. See workspaceStagingPath — a shared staging dir
		// would make one instance's readiness marker satisfy the other's probe.
		{Name: "STAGING_ROOT", Value: target.staging},
		// Per-instance loopback port. See sharedWorkspaceSyncPort.
		{Name: "WORKSPACE_SYNC_PORT", Value: target.port},
		// The key root (§14.4). Every object this sidecar reads or writes lives
		// under <appId>/<workspaceId>/, so getting this wrong does not
		// error — it silently addresses a workspace nobody else can see.
		{Name: "APP_ID", Value: ws.AppID},
		// Key names as the registry's ExternalSecret renders them
		// (fleet-registry/tenants/waypoint/dev/values.yaml): SCREAMING_SNAKE,
		// matching the env names the SDK binds them to.
		// Location from the ConfigMap, credentials from the Secret -- exactly as
		// the SDK's own rollout binds them, read off the running object rather
		// than inferred. Injecting all four from one Secret is what failed
		// silently three times.
		configEnv("S3_ENDPOINT_URL", "S3_ENDPOINT_URL"),
		configEnv("S3_BUCKET_NAME", "S3_BUCKET_NAME"),
		secretEnv("S3_ACCESS_KEY_ID", "S3_ACCESS_KEY_ID"),
		secretEnv("S3_SECRET_ACCESS_KEY", "S3_SECRET_ACCESS_KEY"),
		// Optional in a stronger sense than the rest: the registry declares no
		// S3_REGION at all, so this is absent in every deployed environment and
		// the store falls back to us-east-1. That is correct for an
		// S3-compatible endpoint that ignores region, and wrong for one that
		// signs against it — Hetzner does. Local setup derives it from the
		// endpoint host; deployed environments need the key added to the
		// ExternalSecret before a region-checking provider will authenticate.
		configEnv("S3_REGION", "S3_REGION"),
	}
	// NO MOUNT PROPAGATION, because there is no longer a mount to propagate.
	//
	// §14.2 needed Bidirectional here: containers in a pod share a network
	// namespace but NOT a mount namespace, so a fuse-overlayfs mount made in
	// this container was invisible to the workload, which would go on seeing
	// the bare emptyDir while the restore logged success. Bidirectional pushed
	// the mount back to the host and onward into the workload's
	// HostToContainer mount -- and the kubelet permits Bidirectional only on a
	// privileged container, which is the single reason this one was privileged.
	//
	// Restoring file-by-file writes into the shared emptyDir instead. A volume
	// is visible to every container that mounts it without any propagation
	// setting, so the workload sees the restored tree because it is the same
	// volume, not because a mount was pushed into its namespace.
	//
	// Leaving Bidirectional set would not merely be redundant: it is rejected
	// outright on a non-privileged container, so this has to go for the
	// security context above to be admissible at all.
	volumeMounts := []corev1.VolumeMount{
		{
			Name:      WorkspaceVolumeName,
			MountPath: WorkspaceMountPath,
		},
		{Name: "ws-staging", MountPath: workspaceStagingPath},
	}

	always := corev1.ContainerRestartPolicyAlways
	sideC = corev1.Container{
		Name:            target.name,
		Image:           workspaceSyncImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Args:            []string{"serve"},
		Env:             env,
		VolumeMounts:    volumeMounts,
		SecurityContext: sidecarSec,

		// Gates the workload's start on the workspace actually being ready
		// (§14.2). For a native sidecar the kubelet waits for the startup probe
		// before starting the next container, so this is what guarantees the
		// agent never opens a half-restored tree.
		//
		// exec, not httpGet: the kubelet probes from the node against the pod
		// IP, and this process binds 127.0.0.1, so an HTTP probe could never
		// succeed. The marker lives in the staging emptyDir, which is fresh on
		// every pod, so it cannot survive from a previous life.
		//
		// failureThreshold × periodSeconds = 10 minutes, matching the restore
		// timeout in the sidecar. A restore slower than that is a fault, and
		// the pod should fail rather than hang indefinitely.
		StartupProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				Exec: &corev1.ExecAction{
					Command: []string{"test", "-f", target.staging + "/.ready"},
				},
			},
			InitialDelaySeconds: 1,
			PeriodSeconds:       2,
			FailureThreshold:    300,
		},
		// What makes it a native sidecar rather than a plain init container
		// that would block the pod from ever starting.
		RestartPolicy: &always,

		// No probes, deliberately — and this is a consequence of the loopback
		// bind, not an oversight.
		//
		// The kubelet runs an httpGet probe from the NODE against the pod IP,
		// so it cannot reach a listener bound to 127.0.0.1 inside the pod: an
		// HTTP liveness probe here would fail permanently and restart a
		// perfectly healthy sidecar forever. An exec probe is the usual way
		// out, but this image is now debian-slim — curl is available, but
		// probes would gate the workload's start and a degraded object store
		// should not deny the user their workspace.
		//
		// Readiness would be actively harmful besides. On a native sidecar it
		// gates the workload's start, and this process must never be able to
		// keep a sandbox from running: an unreachable object store is
		// degraded durability, not a reason to deny the user their workspace.
		//
		// What is lost is small and already covered. A wedged sidecar shows up
		// as checkpoints that stop appearing, which the restore path reports
		// on the next session ("no checkpoint for this workspace yet"), and
		// the live tree is held in the overlay filesystem. `/healthz` stays
		// in the server for in-pod debugging.
	}
	sideC.Env = append(sideC.Env,
		corev1.EnvVar{
			// The uid the workload runs as, so a file-by-file restore hands the
			// tree back to it rather than leaving it root-owned (see the
			// sidecar's workspaceUID). Kept in sync with the pod
			// securityContext below.
			Name: "WORKSPACE_UID", Value: "1000",
		},
		corev1.EnvVar{
			// Retention, resolved by the caller from the CR (§14.2).
			//
			// Always set explicitly, even when it equals the default, so the
			// value that governs a running pod is visible in `kubectl describe`
			// rather than implied by the sidecar's own fallback. A retention
			// bound that has to be inferred is one nobody checks.
			Name: "KEEP_CHECKPOINTS", Value: keepCheckpointsEnv(keepCheckpoints),
		},
	)

	// §14.3. Both are set only when asked for, so `kubectl describe` on an
	// ordinary sandbox stays free of fields that describe the default.
	if ws.CheckpointID != "" {
		sideC.Env = append(sideC.Env, corev1.EnvVar{Name: "CHECKPOINT_ID", Value: ws.CheckpointID})
	}
	// Either the whole spec is read-only, or this instance is. The per-instance
	// flag is what lets a session write its own workspace while only reading the
	// shared one -- the case in which checkpointing is not merely wasteful but
	// overwrites another session's newer copy.
	if ws.ReadOnly || target.readOnly {
		sideC.Env = append(sideC.Env, corev1.EnvVar{Name: "WORKSPACE_READ_ONLY", Value: "true"})
	}
	if target.refresh {
		sideC.Env = append(sideC.Env, corev1.EnvVar{Name: "WORKSPACE_REFRESH", Value: "true"})
	}
	// ALWAYS, for every instance (§14.7).
	//
	// The squashfs fast path mounts through squashfuse and fuse-overlayfs, and
	// both are gone with the privilege that made them possible. Setting this
	// unconditionally is what makes that explicit to the binary rather than
	// leaving it to discover at runtime that it cannot mount and fall back --
	// a fallback whose log line ("archive present but could not be mounted")
	// would read as a fault on every single sandbox.
	//
	// `target.noArchive` stays as a field because the shared instance has
	// always set it for a DIFFERENT reason -- its root sits inside the session
	// tree, where a FUSE overlay would nest inside another mount -- and that
	// reason survives the storage layer arriving in §14.7. The two are
	// collapsed here only while no instance can mount at all.
	sideC.Env = append(sideC.Env, corev1.EnvVar{Name: "WORKSPACE_NO_ARCHIVE", Value: "true"})
	// The periodic backstop, for a fleet that asked for one. Both instances get
	// it: an unattended workspace and the app-scoped tree beside it are lost the
	// same way, and a brand brief written once at the start of a run is exactly
	// the kind of thing a crash an hour later would otherwise take.
	// Only when turned OFF: the sidecar's default is to save, and an env var
	// restating the default is one more thing that can disagree with it.
	if ws.CheckpointOnShutdown != nil && !*ws.CheckpointOnShutdown {
		sideC.Env = append(sideC.Env, corev1.EnvVar{Name: "WORKSPACE_SYNC_TEARDOWN", Value: "false"})
	}
	if ws.CheckpointIntervalSeconds != nil && *ws.CheckpointIntervalSeconds > 0 {
		sideC.Env = append(sideC.Env, corev1.EnvVar{
			Name:  "WORKSPACE_SYNC_INTERVAL_SECONDS",
			Value: strconv.Itoa(int(*ws.CheckpointIntervalSeconds)),
		})
	}
	if len(ws.PinnedCheckpoints) > 0 {
		// Comma-separated: a short list of ids, where JSON encoding would add a
		// parsing failure mode for no benefit.
		sideC.Env = append(sideC.Env, corev1.EnvVar{
			Name: "PINNED_CHECKPOINTS", Value: strings.Join(ws.PinnedCheckpoints, ","),
		})
	}
	return sideC
}

// resolveKeepCheckpoints turns the fleet's optional request into the value the
// sidecar runs with (§14.2).
//
// Clamped as well as defaulted. The CRD's Minimum/Maximum already reject an
// out-of-range value at admission, but this operator must not depend on that:
// a CR applied before the schema was updated, or through a path that skipped
// validation, would otherwise reach the sidecar — and `keep: 0` there means
// "prune everything but the newest", while a negative would be read as
// "unbounded". Neither is a value a fleet can be assumed to have meant.
//
// Zero is returned for RetainAllCheckpoints, and only for it: the env writer
// renders it as KEEP_CHECKPOINTS=unbounded (§14.8), never as a number the
// sidecar could read as a bound.
func resolveKeepCheckpoints(ws *computev1alpha1.WorkspacePersistenceSpec) int32 {
	if ws != nil && ws.RetainAllCheckpoints {
		return unboundedKeepCheckpoints
	}
	if ws == nil || ws.KeepCheckpoints == nil {
		return computev1alpha1.DefaultKeepCheckpoints
	}
	switch n := *ws.KeepCheckpoints; {
	case n < minKeepCheckpoints:
		return minKeepCheckpoints
	case n > maxKeepCheckpoints:
		return maxKeepCheckpoints
	default:
		return n
	}
}

// fleetIDLabel is the namespace label the tenant registry writes beside
// `tenant-id`. It holds the fleet's ADR-088 appId -- `waypoint`, `oranger` --
// which is what the fleet's own Kubernetes objects are named from.
const fleetIDLabel = "app-id"

// resolveFleetID returns the fleet whose Secret and ConfigMap hold this
// namespace's S3 configuration.
//
// NOT spec.workspacePersistence.appId. That field is the tenant APPLICATION id, a
// ULID, and it is load-bearing elsewhere: it is the S3 object-key root
// (<appId>/<workspaceId>/) and, for an app-scoped session, the workspace id
// itself. It cannot also name a Secret, and using it to do so produced
// `01m37pyr33yjdcc6zh338a2gzd-sdk-secrets` -- the fourth wrong value for this
// reference. Two identifiers wear the word "app" in this system; this function
// exists so they are not mixed.
//
// Read from the namespace label rather than parsed out of the namespace name:
// splitting `tenant-<tenantId>-<appId>` is unsafe as soon as a tenantId contains a
// hyphen, and inference is what produced every earlier wrong value.
//
// UNCACHED, and that is not incidental. A cached Get on a Namespace makes the
// manager start a cluster-wide Namespace informer, which needs list and watch on
// every namespace and BLOCKS MANAGER STARTUP until that cache syncs. The same
// mistake on Nodes stopped this operator reconciling entirely in September 2026:
// it came up, failed to sync a cache it had no RBAC for, and picked up no
// EphemeralJob at all, with nothing in its status to say so. One GET per pod
// build is the cheaper side of that trade (see APIReader).
//
// NO FALLBACK. An unreadable namespace or a missing label yields the empty
// string, and the reference then renders bare -- visibly wrong in
// `kubectl describe` rather than quietly pointing at something that does not
// exist. A deployment whose namespaces carry no such label sets
// WORKSPACE_SYNC_SECRET and WORKSPACE_SYNC_CONFIG explicitly, which is
// configuration, not a guess.
func (r *EphemeralJobReconciler) resolveFleetID(ctx context.Context, namespace string) string {
	if r == nil || r.APIReader == nil {
		return ""
	}
	var ns corev1.Namespace
	if err := r.APIReader.Get(ctx, client.ObjectKey{Name: namespace}, &ns); err != nil {
		log.FromContext(ctx).Error(err, "could not read the namespace for its fleet id; "+
			"the workspace-sync S3 reference will render incomplete and checkpoints will no-op",
			"namespace", namespace, "label", fleetIDLabel)
		return ""
	}
	return ns.Labels[fleetIDLabel]
}

// keepCheckpointsEnv renders the resolved retention for the sidecar. The
// unbounded case is a word, not 0, so no reader can mistake it for a count.
func keepCheckpointsEnv(keep int32) string {
	if keep == unboundedKeepCheckpoints {
		return "unbounded"
	}
	return strconv.Itoa(int(keep))
}
