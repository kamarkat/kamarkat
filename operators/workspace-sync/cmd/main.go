// Command workspace-sync is the platform's workspace persistence agent
// (ADR-052 §14).
//
// It runs in two shapes inside a sandbox pod, from one image:
//
//	workspace-sync restore   init container — runs once, before the workload
//	workspace-sync serve     sidecar — on-demand checkpoints + periodic backstop
//
// It is platform-owned and holds the object-store credential. The fleet's
// workload container never receives that credential and never speaks to S3
// (§19.6): it asks this process for a checkpoint over localhost, which is why
// the control endpoint below needs no authentication of its own — reaching it
// already requires being inside this pod.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kamarkat/kamarkat/operators/workspace-sync/internal/store"
)

const defaultRoot = "/workspace"

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	if len(os.Args) < 2 {
		log.Fatal("usage: workspace-sync <restore|serve>")
	}
	root := os.Getenv("WORKSPACE_ROOT")
	if root == "" {
		root = defaultRoot
	}

	switch os.Args[1] {
	case "restore":
		if err := runRestore(root); err != nil {
			log.Fatalf("restore failed: %v", err)
		}
	case "serve":
		if err := runServe(root); err != nil {
			log.Fatalf("serve failed: %v", err)
		}
	default:
		log.Fatalf("unknown command %q", os.Args[1])
	}
}

// runRestore is a one-shot restore, kept for debugging and ops use only.
//
// The pod does NOT run it. Under §14.2 the FUSE mount belongs to the native
// sidecar and nothing else: a mount made by an init container dies with that
// container's mount namespace, so an init container can prepare a workspace
// the sidecar then hides, or download an archive the sidecar downloads again.
// One owner of restore, and it is `serve`.
func runRestore(root string) error {
	cfg, err := store.FromEnv()
	if errors.Is(err, store.ErrNotConfigured) {
		log.Print("object storage not configured — nothing to restore")
		return nil
	}
	if err != nil {
		return err
	}
	s, err := store.New(cfg)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	_, err = restoreWorkspace(ctx, root, s)
	return err
}

// restoreWorkspace prepares root before the workload starts, and reports
// whether the result is a FUSE overlay (true) or a plain directory (false).
//
// Three outcomes, and telling them apart is the whole correctness of this
// function:
//
//   - an archive exists          → mount it, O(1) (§14.2);
//   - no archive, no manifest    → a genuinely new workspace, start empty;
//   - no archive, manifests exist → file-by-file restore (pre-§14.2 workspace,
//     or one whose archive creation failed).
//
// Anything else is an ERROR, and the caller must let it be fatal.
//
// That last rule is not defensive style, it is a data-loss guard. If S3 is
// unreachable and this returns "empty workspace" instead of failing, the
// workload starts against an empty tree, the periodic backstop uploads that
// emptiness as a new checkpoint, and retention (§14.2, keep 5) then evicts the
// real checkpoints behind it. A few backstop intervals of an unreachable
// bucket would destroy the workspace it exists to protect. Refusing to start
// is always recoverable; silently starting empty is not.
func restoreWorkspace(ctx context.Context, root string, s *store.Store) (bool, error) {
	if err := os.MkdirAll(root, 0o777); err != nil {
		return false, err
	}
	// Hand the ROOT DIRECTORY to the workload, always — before any restore
	// decision below, and whether or not there is anything to restore.
	//
	// workspaceUID's contract already says "every directory the restore itself
	// creates" must belong to the workload, but the only chown was inside the
	// file-by-file path. A workspace with no checkpoint yet returns early from
	// restoreFromManifest, so this directory kept the owner of whoever created
	// it — this process. That was invisible while the process ran as the
	// workload's own uid; it became a bug the moment it had to run as root to
	// chown restored files at all.
	//
	// The visible symptom is not a permission error from the restore, which
	// succeeds. It is the AGENT failing to write into its own workspace much
	// later — "/workspace/.global is read-only" — with nothing connecting that
	// to a directory created empty at pod start. MkdirAll's mode is also
	// subject to umask (0777 lands as 0755 under the usual 022), so the mode
	// alone cannot be relied on either.
	uid := workspaceUID()
	// Same reasoning as chownTree: nothing to hand over when this process is
	// already the workload's uid, and the root is frequently a kubelet-created
	// mount point it cannot chown at all.
	if err := chownRootIfNeeded(root, uid); err != nil {
		// Not fatal: a root that is a mount point the kubelet owns may refuse,
		// and the workload can still be fine if the mode is permissive. Logged
		// loudly because if it is NOT fine, this line is the only warning.
		log.Printf("WARNING: could not chown %s to uid %d — the workload may be unable to write it: %v", root, uid, err)
	}
	staging := s.StagingDir()
	archivePath := filepath.Join(staging, "archive.sqsh")

	// A pinned checkpoint takes a different route entirely (§14.3): the
	// workspace-level archive.sqsh is whatever was checkpointed LAST, so using
	// it for a pin would restore the wrong revision while reporting success.
	if pin := s.Pinned(); pin != "" {
		id, rErr := s.Resolve(ctx)
		if rErr != nil {
			return false, rErr
		}
		aErr := s.DownloadCheckpointArchive(ctx, id, archivePath)
		switch {
		case aErr == nil:
			if mErr := mountFuse(ctx, root, staging, archivePath); mErr != nil {
				// A failed mount is a failed restore. Unmount, report.
				//
				// This used to reconstruct the tree file-by-file instead and
				// log a line about it. That is the wrong shape twice over: a
				// pinned checkpoint is an EXACT revision the caller asked for,
				// and a silent second route to it means a mount that has been
				// broken for days presents as a session that is merely slow.
				//
				// The Cloudflare SDK this restore path was adopted from does
				// not do it either -- backup-service.ts returns
				// BACKUP_RESTORE_FAILED on both the squashfuse and the
				// fuse-overlayfs branch, unmounting first. The fallback was
				// ours, and it existed only because §14's content-addressed
				// store happened to predate §14.2's archive.
				unmountFuse(root, staging)
				return false, fmt.Errorf("cannot mount pinned checkpoint %s: %w", id, mErr)
			}
			log.Printf("restored pinned checkpoint %s via squashfs overlay", id)
			return true, nil
		case errors.Is(aErr, store.ErrNoArchive):
			// The checkpoint exists — Resolve proved it — but predates
			// archives or had archive creation fail. Its content-addressed
			// objects still describe it exactly.
			return false, restoreFromCheckpoint(ctx, root, s, id)
		default:
			return false, fmt.Errorf("cannot fetch pinned checkpoint %s: %w", id, aErr)
		}
	}

	// The globals instance restores file-by-file, always. Its root is INSIDE
	// another workspace's tree, so a FUSE overlay here would nest inside a mount
	// it does not own — and skipping this path is what lets that sidecar run
	// unprivileged. See store.Config.NoArchive.
	if s.NoArchive() {
		return false, restoreFromManifest(ctx, root, s)
	}

	err := s.DownloadArchive(ctx, archivePath)
	switch {
	case err == nil:
		if mErr := mountFuse(ctx, root, staging, archivePath); mErr != nil {
			// A failed mount is a failed restore. Unmount, report.
			//
			// The previous comment here argued that a present-but-unmountable
			// archive is not a reason to start empty, because the data exists
			// in the content-addressed objects. Both halves are true and the
			// conclusion still does not follow: the alternative to starting
			// empty is FAILING, not quietly taking a different route and
			// reporting success.
			//
			// What that cost is not hypothetical. Asked whether a fleet had
			// been restoring through this branch, the answer was unobtainable
			// -- the only evidence would have been a log line in a pod whose
			// logs had been collected. A mount that stops working is supposed
			// to be loud.
			//
			// Parity with the SDK this was adopted from: backup-service.ts
			// returns BACKUP_RESTORE_FAILED from both mount branches and
			// unmounts on the way out. It keeps no second representation to
			// fall back to, and neither does this path now.
			unmountFuse(root, staging)
			return false, fmt.Errorf("cannot mount workspace archive: %w", mErr)
		}
		log.Printf("restored via squashfs overlay at %s", root)
		return true, nil

	case errors.Is(err, store.ErrNoArchive):
		return false, restoreFromManifest(ctx, root, s)

	default:
		return false, fmt.Errorf("cannot reach object storage to restore workspace: %w", err)
	}
}

// mountFuse mounts the archive read-only and lays a writable overlay over it.
//
// Both mounts live in THIS process's mount namespace. They are visible to the
// workload container only because the operator sets mountPropagation
// (Bidirectional here, HostToContainer there) — without it the workload sees
// the bare emptyDir and the restore is invisible to the thing it was for.
func mountFuse(ctx context.Context, workspaceRoot, staging, archivePath string) error {
	lowerDir := filepath.Join(staging, "lower")
	upperDir := filepath.Join(staging, "upper")
	workDir := filepath.Join(staging, "work")

	for _, d := range []string{lowerDir, upperDir, workDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("create staging dir %s: %w", d, err)
		}
	}

	// upperDir backs every NEW entry the merged mount will ever show — not just
	// its contents once something exists, but the top-level directory itself.
	// fuse-overlayfs reports the merge point's own permission bits from upperDir
	// (upperDir already contains the root, so it is never "copied up" the way a
	// child directory is); those bits are otherwise whatever MkdirAll left them
	// as a moment ago: root:root, 0755. A workload running as uid 1000 then gets
	// EACCES on anything it tries to create directly under /workspace — `mkdir
	// node_modules`, in particular — while files that already exist (restored
	// from the read-only squashfs lower layer, correctly owned from when that
	// layer was built) remain readable and writable, because THEIR ownership
	// comes from the lower layer, not upperDir's.
	//
	// Confirmed live: `ls -lad /workspace` showed `root root`, while every entry
	// inside it — including subdirectories the agent had already been writing
	// into — showed `1000 root`. Only the mount's own top-level entry was wrong.
	//
	// This is also why the symptom outlives any one restore. The next snapshot
	// walks the live (merged) tree with mksquashfs, which preserves ownership by
	// default — so an unfixed root directory gets baked into the NEXT archive
	// too, and the corruption survives every subsequent restore of this
	// workspace until something explicitly re-chowns the mount root.
	if err := os.Chown(upperDir, workspaceUID(), workspaceUID()); err != nil {
		return fmt.Errorf("chown upper dir %s to workload uid: %w", upperDir, err)
	}

	// allow_other on BOTH mounts, and it is not optional.
	//
	// A FUSE mount is accessible only to the uid that created it. This process
	// runs as root (privileged, for the mount itself); the workload runs as
	// 1000. Without allow_other the agent gets EACCES on its own /workspace —
	// a restore that succeeded, a mount that exists, and a workload that cannot
	// read a single byte of it.
	//
	// Permitted here because the mounting user is root; unprivileged callers
	// would additionally need user_allow_other in /etc/fuse.conf.
	cmd := exec.CommandContext(ctx, "squashfuse", "-o", "allow_other", archivePath, lowerDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("squashfuse: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}

	cmd = exec.CommandContext(ctx, "fuse-overlayfs",
		"-o", fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s,allow_other", lowerDir, upperDir, workDir),
		workspaceRoot)
	if out, err := cmd.CombinedOutput(); err != nil {
		exec.Command("fusermount3", "-u", lowerDir).Run()
		return fmt.Errorf("fuse-overlayfs: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// workspaceUID is the uid the workload container runs as, and therefore the
// uid every restored file — and every directory the restore itself creates —
// must belong to.
//
// This process runs as root because FUSE mounting requires it. Anything it
// writes directly is therefore root-owned, and the agent — uid 1000 — cannot
// modify it. On a git tree that surfaces as a permission error deep inside an
// unrelated operation rather than as anything about ownership, which is the
// same failure that once took down every resumed session.
//
// The FUSE path is NOT exempt from this, despite this function previously
// claiming it was. Existing content is fine — its ownership comes from the
// squashfs image, which recorded the workload's uid when the archive was
// built — but upperDir, the writable layer mountFuse creates fresh on every
// restore, is written by this process directly and needs the same chown as
// the slow path's chownTree below. See mountFuse for what shipped without it.
// chownRootIfNeeded is chownTree's single-entry twin, kept separate because the
// root is the one path most likely to be a mount point and least likely to
// matter: the workload writes INTO it, not to it.
func chownRootIfNeeded(root string, uid int) error {
	if os.Getuid() == uid {
		return nil
	}
	return os.Chown(root, uid, uid)
}

func workspaceUID() int {
	if v := os.Getenv("WORKSPACE_UID"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return 1000
}

// chownTree gives a file-by-file restore back to the workload.
// chownTree hands a restored tree to the workload.
//
// It is a NO-OP when this process already runs as that uid, and returns
// immediately rather than walking. That is not an optimisation: a process is
// permitted to chown a file to its own uid only where it already owns it, and
// the one thing it never owns is the mount point the kubelet created --
// root:root, whatever is mounted there. Walking into it fails:
//
//	serve failed: restore on startup: chown restored workspace to uid 1000:
//	lchown /workspace/.global: operation not permitted
//
// and the pod crashlooped on a directory it did not need to own. Mode 0777 on
// an emptyDir already lets it write there, which is all the restore requires.
//
// The chown exists for the case this binary ran as root and had to hand
// ownership over. Running as the workload's own uid, every file it writes is
// correctly owned when it is written, so there is nothing to hand over.
func chownTree(root string, uid int) error {
	if os.Getuid() == uid {
		return nil
	}
	return filepath.Walk(root, func(p string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(p, uid, uid)
	})
}

// restoreFromManifest is the content-addressed slow path: correct everywhere,
// O(number of files), and the only path that works without FUSE at all.
//
// No checkpoint is SUCCESS, not failure. It means a workspace that has never
// been checkpointed, which is every workspace's first run. Returning an error
// here made the process exit non-zero and the pod fail to start — so enabling
// object storage, which is meant to add durability, made every new sandbox
// unstartable.
func restoreFromManifest(ctx context.Context, root string, s *store.Store) error {
	id, err := s.Latest(ctx)
	if err != nil {
		return fmt.Errorf("reading LATEST pointer: %w", err)
	}
	if id == "" {
		log.Print("no checkpoint for this workspace yet — starting with an empty workspace")
		return nil
	}
	return restoreFromCheckpoint(ctx, root, s, id)
}

// restoreFromCheckpoint materialises one named checkpoint file-by-file.
//
// Unlike restoreFromManifest there is no "nothing to restore" case: the caller
// named a checkpoint, so failing to produce it is an error rather than an empty
// workspace.
func restoreFromCheckpoint(ctx context.Context, root string, s *store.Store, id string) error {
	m, err := s.GetManifest(ctx, id)
	if err != nil {
		return fmt.Errorf("reading checkpoint %s: %w", id, err)
	}
	if err := s.Restore(ctx, root, m); err != nil {
		return fmt.Errorf("restoring checkpoint %s: %w", id, err)
	}
	// Hand the tree to the workload — see workspaceUID.
	uid := workspaceUID()
	if err := chownTree(root, uid); err != nil {
		return fmt.Errorf("chown restored workspace to uid %d: %w", uid, err)
	}
	log.Printf("restored checkpoint %s file-by-file (%d files, taken %s), owned by uid %d",
		m.ID, len(m.Entries), m.CreatedAt.Format(time.RFC3339), uid)
	return nil
}

// unmountFuse tears down any active FUSE mounts at workspaceRoot and in stagingRoot.
func unmountFuse(workspaceRoot, stagingRoot string) {
	// Unmount overlay first (the merged view).
	exec.Command("fusermount3", "-u", workspaceRoot).Run()
	// Then unmount squashfs lower.
	exec.Command("fusermount3", "-u", stagingRoot+"/lower").Run()
	// Clean staging dirs.
	os.RemoveAll(stagingRoot + "/lower")
	os.RemoveAll(stagingRoot + "/upper")
	os.RemoveAll(stagingRoot + "/work")
}

// runServe is the sidecar: one snapshot engine behind two triggers.
//
// On-demand is the primary path — the agent knows when a unit of work is worth
// recording and calls POST /checkpoint at that boundary. The periodic backstop
// exists only for the case an on-demand call cannot cover: the agent crashed,
// or the node died, before it ever asked.
func runServe(root string) error {
	cfg, err := store.FromEnv()
	notConfigured := errors.Is(err, store.ErrNotConfigured)
	if err != nil && !notConfigured {
		return err
	}

	var s *store.Store
	if !notConfigured {
		if s, err = store.New(cfg); err != nil {
			return err
		}
	} else {
		log.Print("object storage not configured — checkpoints will be accepted and no-op")
	}

	stagingRoot := "/ws-staging"
	if s != nil {
		stagingRoot = s.StagingDir()
	} else if v := os.Getenv("STAGING_ROOT"); v != "" {
		stagingRoot = v
	}

	// The staging directory must exist before anything writes into it.
	//
	// It used to be the emptyDir's own mount point, which the kubelet creates,
	// so nothing here ever had to. A second instance in the same pod stages in a
	// SUBDIRECTORY of that mount — the readiness marker is named from it, and two
	// instances sharing one directory would let each satisfy the other's startup
	// probe — and no one creates a subdirectory of an emptyDir. The shared
	// instance therefore restored correctly and then died writing its marker,
	// crashlooping the pod before the workload ever started.
	if err := os.MkdirAll(stagingRoot, 0o777); err != nil {
		return fmt.Errorf("creating staging directory %s: %w", stagingRoot, err)
	}

	// Read-only: mounted, serving, and incapable of writing (§14.3).
	//
	// Declared here because every path below consults it — the restore, the
	// HTTP handlers, the backstop and the shutdown snapshot. In this mode the
	// backstop and teardown snapshot are never STARTED, rather than started and
	// then refused by the store's own guard: a build has no business generating
	// a checkpoint attempt every five minutes, and the log noise of refusing
	// them would bury a real failure.
	readOnly := s != nil && s.ReadOnly()
	if readOnly {
		log.Print("workspace is READ-ONLY — no periodic backstop, no teardown checkpoint, no retention")
	}

	// Phase 1: restore, once, before anything else (§14.2).
	//
	// This is the ONLY restore in the pod. It used to run here AND in an init
	// container, which downloaded the same archive twice and mounted an overlay
	// that was destroyed the moment the init container exited.
	//
	// A failure returns, and returning kills the process. That is deliberate:
	// this is a native sidecar with restartPolicy Always, so it will
	// CrashLoopBackOff with the reason in its logs, the readiness marker below
	// is never written, the startup probe never passes, and the WORKLOAD NEVER
	// STARTS. A sandbox that refuses to run is the correct outcome when its
	// workspace could not be restored — the alternative is an agent editing an
	// empty tree that will be checkpointed over the real one.
	//
	// A reader that may refresh (§14.9) lays its workspace out as generations
	// instead: the same file-by-file restore, behind a link /refresh can swap.
	refresh, err := refreshEnabled(s)
	if err != nil {
		return err
	}
	var gens *generations
	if refresh {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		gens, err = startGenerations(ctx, root, s)
		cancel()
		if err != nil {
			return fmt.Errorf("restore on startup: %w", err)
		}
	} else if !notConfigured {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		fused, rErr := restoreWorkspace(ctx, root, s)
		cancel()
		if rErr != nil {
			return fmt.Errorf("restore on startup: %w", rErr)
		}
		if fused {
			defer unmountFuse(root, stagingRoot)
		}
	}

	// Phase 2: announce readiness.
	//
	// A marker file, not an HTTP endpoint, because the kubelet probes from the
	// node against the pod IP and this process binds loopback — an httpGet
	// probe here can never succeed. The operator gates the workload's start on
	// an exec probe testing for this file, which is what guarantees the agent
	// never sees a half-restored workspace.
	//
	// It lives in the staging emptyDir, which is fresh on every pod, so a stale
	// marker from a previous life cannot make an unrestored workspace look
	// ready.
	readyMarker := filepath.Join(stagingRoot, ".ready")
	if err := os.WriteFile(readyMarker, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644); err != nil {
		return fmt.Errorf("write readiness marker: %w", err)
	}
	defer os.Remove(readyMarker)
	log.Printf("workspace ready at %s", root)

	// Parent pointer, for checkpoint lineage.
	//
	// Guarded because three goroutines reach it — the periodic ticker, the
	// /checkpoint handler, and the shutdown path — and the mutex also
	// serialises the snapshots themselves. That serialisation is the point as
	// much as the field is: two concurrent walks of the same tree would upload
	// interleaved views of it and race to claim the same parent.
	//
	// A one-slot semaphore, not a sync.Mutex, because a waiter must be able to
	// give up. With a mutex an on-demand /checkpoint queued silently behind a
	// stalled periodic save -- 8m33s on 2026-10-05 -- and its caller timed out
	// with no answer; the shutdown save waited the same way, outside its budget.
	// A waiter now stops at its own bound and is told what holds the slot.
	var (
		slot     = make(chan struct{}, 1)
		stateMu  sync.Mutex // guards the fields below
		last     string
		current  *saveState // the save holding the slot, if any
		lastDone *saveState // the most recent save to finish
	)

	// acquire takes the slot, waiting at most `wait` (negative: until ctx ends).
	acquire := func(ctx context.Context, wait time.Duration) error {
		var timeout <-chan time.Time
		if wait >= 0 {
			t := time.NewTimer(wait)
			defer t.Stop()
			timeout = t.C
		}
		select {
		case slot <- struct{}{}:
			return nil
		case <-timeout:
			return errSaveInProgress
		case <-ctx.Done():
			return fmt.Errorf("%w: %v", errSaveInProgress, ctx.Err())
		}
	}
	release := func() { <-slot }

	// busy describes the slot's holder and the last finished save, for a caller
	// that could not get it.
	busy := func() map[string]any {
		stateMu.Lock()
		defer stateMu.Unlock()
		out := map[string]any{"error": "a save is in progress; retry when it completes"}
		if current != nil {
			out["inProgress"] = current.view()
		}
		if lastDone != nil {
			out["lastCompleted"] = lastDone.view()
		}
		return out
	}

	snapshotCtx := func(ctx context.Context, name, desc, trigger string, wait time.Duration) (*store.Manifest, error) {
		if s == nil {
			return nil, store.ErrNotConfigured
		}
		if err := acquire(ctx, wait); err != nil {
			return nil, err
		}
		defer release()

		stateMu.Lock()
		current = &saveState{Trigger: trigger, Since: time.Now()}
		parent := last
		stateMu.Unlock()

		m, err := s.Snapshot(ctx, root, name, desc, trigger, parent)

		stateMu.Lock()
		defer stateMu.Unlock()
		done := *current
		done.Finished = time.Now()
		switch {
		case err == nil:
			last = m.ID
			done.CheckpointID, done.Outcome = m.ID, "saved"
		case errors.Is(err, store.ErrUnchanged), errors.Is(err, store.ErrNothingToSave):
			done.Outcome = "skipped"
		default:
			done.Outcome = "failed"
		}
		current, lastDone = nil, &done
		return m, err
	}

	// The ordinary path: a generous ceiling, since a first checkpoint of a
	// large workspace is genuinely slow and nothing is waiting on it. Each
	// storage call inside it is bounded separately (store.callBudget), so the
	// ceiling is reached only by a save that is making progress. The shutdown
	// path passes its own, much shorter budget instead.
	snapshot := func(name, desc, trigger string, wait time.Duration) (*store.Manifest, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		return snapshotCtx(ctx, name, desc, trigger, wait)
	}

	// How long an on-demand /checkpoint waits for a save already in progress
	// before answering 503 with what is in progress. Below the 120s tool
	// timeout that calls it, so the caller gets an answer rather than a timeout.
	checkpointWait := 60 * time.Second
	if v := os.Getenv("WORKSPACE_SYNC_CHECKPOINT_WAIT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			checkpointWait = time.Duration(n) * time.Second
		}
	}

	mux := http.NewServeMux()

	// POST /checkpoint — the agent's boundary call (§14).
	mux.HandleFunc("/checkpoint", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)

		if readOnly {
			// 409, not 500: the request is well-formed and the caller is simply
			// asking something this workspace cannot do. A build image that
			// checkpoints out of habit should see a clear refusal, not a
			// failure that looks like object storage being broken.
			writeJSON(w, http.StatusConflict, map[string]any{
				"error": "workspace is read-only; checkpoints are disabled for this job",
			})
			return
		}
		wait := checkpointWait
		if v := r.URL.Query().Get("wait"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= 300 {
				wait = time.Duration(n) * time.Second
			}
		}
		m, err := snapshot(req.Name, req.Description, "on-demand", wait)
		if errors.Is(err, errSaveInProgress) {
			// 503 with what holds the slot, not a hung request: the caller can
			// retry when the in-progress save completes, and sees which save it
			// was waiting on.
			w.Header().Set("Retry-After", "15")
			writeJSON(w, http.StatusServiceUnavailable, busy())
			return
		}
		if errors.Is(err, store.ErrUnchanged) {
			// 200, not an error: the request was correct and the workspace is
			// already represented by the checkpoint it was restored from. Writing
			// one would move LATEST over a copy another session may have written
			// since (see store.ErrUnchanged).
			writeJSON(w, http.StatusOK, map[string]any{"checkpointId": "", "skipped": "unchanged"})
			return
		}
		if errors.Is(err, store.ErrNothingToSave) {
			// 200 with no id, like the not-configured case: the request was
			// correct and there is simply nothing here yet. A 4xx would read as
			// "you did something wrong".
			writeJSON(w, http.StatusOK, map[string]any{"checkpointId": "", "skipped": "nothing-to-save"})
			return
		}
		if errors.Is(err, store.ErrNotConfigured) {
			// 200 with no id: the caller asked correctly and this deployment
			// has nowhere to put it. Failing here would make an unconfigured
			// dev cluster look like a broken agent.
			writeJSON(w, http.StatusOK, map[string]any{"checkpointId": "", "skipped": "not-configured"})
			return
		}
		if err != nil {
			log.Printf("checkpoint failed: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		log.Printf("checkpoint %s (%d files, name=%q)", m.ID, len(m.Entries), m.Name)
		writeJSON(w, http.StatusOK, map[string]any{"checkpointId": m.ID, "files": len(m.Entries)})
	})

	// POST /flush — an on-demand snapshot, forced, from inside the pod.
	//
	// This was the operator's finalizer-gated teardown call. It is no longer:
	// teardown is handled by the SIGTERM path below, which needs no caller.
	// The endpoint stays because it is the only way to force a checkpoint
	// without waiting out the backstop interval, which makes it how this layer
	// gets tested — `kubectl exec` into the workload container and POST to
	// localhost, since the sidecar's own image is distroless and has no shell.
	mux.HandleFunc("/flush", func(w http.ResponseWriter, r *http.Request) {
		if readOnly {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error": "workspace is read-only; checkpoints are disabled for this job",
			})
			return
		}
		m, err := snapshot("", "", "teardown", checkpointWait)
		if errors.Is(err, errSaveInProgress) {
			w.Header().Set("Retry-After", "15")
			writeJSON(w, http.StatusServiceUnavailable, busy())
			return
		}
		if errors.Is(err, store.ErrNotConfigured) {
			writeJSON(w, http.StatusOK, map[string]any{"skipped": "not-configured"})
			return
		}
		if err != nil {
			log.Printf("flush failed: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"checkpointId": m.ID})
	})

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// POST /restore {checkpointId?} — undo-to-checkpoint (§14.2).
	//
	// Tears the overlay down and rebuilds it from the requested checkpoint's
	// archive, or from the latest if none is named.
	//
	// DESTRUCTIVE, and the caller has to understand that: everything written
	// since the target checkpoint lives in the overlay's upper layer, which
	// this discards. The tree the workload sees changes underneath it, so any
	// file descriptor the agent holds open across this call is invalidated —
	// the agent must be quiesced first. This endpoint does not and cannot
	// enforce that; it is reachable only from inside the pod.
	//
	// Serialised against snapshots with the same mutex. A checkpoint racing a
	// restore would otherwise capture a half-swapped tree and upload it as the
	// new latest, turning an undo into corruption.
	mux.HandleFunc("/restore", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		if notConfigured {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "object storage not configured"})
			return
		}
		if readOnly {
			// A read-only job was given one exact checkpoint to work from
			// (§14.3). Letting it swap to another mid-run would mean a build
			// producing an artifact labelled with a revision it did not build.
			writeJSON(w, http.StatusConflict, map[string]any{
				"error": "workspace is pinned and read-only; restore is disabled for this job",
			})
			return
		}

		var req struct {
			CheckpointID string `json:"checkpointId"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()

		if err := acquire(ctx, checkpointWait); err != nil {
			w.Header().Set("Retry-After", "15")
			writeJSON(w, http.StatusServiceUnavailable, busy())
			return
		}
		defer release()

		// Resolve the target first, so a bad id is a 404 BEFORE anything is
		// unmounted. Rejecting after teardown would leave the workspace in
		// neither the old state nor the new one.
		target := req.CheckpointID
		if target == "" {
			id, err := s.Latest(ctx)
			if err != nil || id == "" {
				writeJSON(w, http.StatusNotFound, map[string]any{"error": "no checkpoint to restore"})
				return
			}
			target = id
		}
		m, err := s.GetManifest(ctx, target)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": fmt.Sprintf("checkpoint %s not found: %v", target, err)})
			return
		}

		archivePath := filepath.Join(stagingRoot, "archive.sqsh")
		archiveErr := s.DownloadCheckpointArchive(ctx, target, archivePath)
		if archiveErr != nil && !errors.Is(archiveErr, store.ErrNoArchive) {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": archiveErr.Error()})
			return
		}

		// Only now tear down the old view.
		unmountFuse(root, stagingRoot)

		if archiveErr == nil {
			if err := mountFuse(ctx, root, stagingRoot, archivePath); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
		} else {
			// No archive for this checkpoint — rebuild it file-by-file from the
			// manifest instead. unmountFuse already cleared the staging dirs, so
			// this writes into a bare workspace rather than over stale content.
			if err := s.Restore(ctx, root, m); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
		}

		// Lineage: work resumed after an undo descends from the checkpoint it
		// was restored to, not from whatever was checkpointed last.
		stateMu.Lock()
		last = target
		stateMu.Unlock()
		log.Printf("restored to checkpoint %s (%d files)", target, len(m.Entries))
		writeJSON(w, http.StatusOK, map[string]any{"restored": true, "checkpoint": target})
	})

	// POST /refresh — a reader moves to the shared workspace's newest checkpoint
	// (§14.9), without a restart.
	//
	// The answer is one of three outcomes, so a caller can tell "already
	// current" from "moved" from "could not":
	//
	//	200 {"outcome":"unchanged","checkpoint":id}
	//	200 {"outcome":"refreshed","checkpoint":id,"files":n}
	//	non-2xx {"outcome":"failed","error":...}  -- the workspace is as it was
	//
	// A checkpoint the writer finished (its /checkpoint answered 200) is LATEST
	// by then, so a refresh after it sees it. Serialised with the same slot as
	// every other save or restore.
	mux.HandleFunc("/refresh", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		if gens == nil {
			writeJSON(w, http.StatusConflict, map[string]any{
				"outcome": "failed",
				"error":   "refresh is not enabled for this workspace (sharedWorkspaceRefresh, on a read-only, unpinned shared workspace)",
			})
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()

		if err := acquire(ctx, checkpointWait); err != nil {
			out := busy()
			out["outcome"] = "failed"
			w.Header().Set("Retry-After", "15")
			writeJSON(w, http.StatusServiceUnavailable, out)
			return
		}
		defer release()

		id, err := s.Latest(ctx)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"outcome": "failed", "error": fmt.Sprintf("reading LATEST pointer: %v", err)})
			return
		}
		if id == gens.id {
			writeJSON(w, http.StatusOK, map[string]any{"outcome": "unchanged", "checkpoint": id})
			return
		}
		if id == "" {
			writeJSON(w, http.StatusConflict, map[string]any{
				"outcome": "failed",
				"error":   fmt.Sprintf("the shared workspace has no LATEST checkpoint, but this reader holds %s", gens.id),
			})
			return
		}
		files, err := gens.advance(ctx, s, id)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"outcome": "failed", "error": err.Error()})
			return
		}
		log.Printf("refreshed shared workspace to checkpoint %s (%d files, generation %d)", id, files, gens.n)
		writeJSON(w, http.StatusOK, map[string]any{"outcome": "refreshed", "checkpoint": id, "files": files})
	})

	// GET /list-checkpoints — returns checkpoint IDs in reverse chronological
	// order, for the undo-to-checkpoint UI.
	mux.HandleFunc("/list-checkpoints", func(w http.ResponseWriter, r *http.Request) {
		if notConfigured {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "object storage not configured"})
			return
		}
		ids, err := s.ListCheckpoints(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, ids)
	})

	port := os.Getenv("WORKSPACE_SYNC_PORT")
	if port == "" {
		port = "7070"
	}
	srv := &http.Server{Addr: "127.0.0.1:" + port, Handler: mux}

	// Periodic backstop — OFF by default (ADR-052 §14.6).
	//
	// A checkpoint is taken when something meaningful happens: the user asks
	// for one, or the pod is shutting down. Not on a clock.
	//
	// This matched the Cloudflare sandbox-sdk reference
	// (reference-projects/sandbox/sandbox-sdk) only after being turned off:
	// that design has NO periodic snapshot anywhere — its container saves on
	// suspend, and the platform deliberately refrains from destroying the
	// container so that save can finish (devin/src/index.ts's `stop`).
	//
	// The ticker ran unconditionally every 5 minutes, and the cost was not
	// theoretical. Each run writes a full compressed squashfs image twice (the
	// per-checkpoint archive and the workspace `archive.sqsh`) whether or not a
	// byte changed, so an idle sandbox shipped a whole workspace to object
	// storage twelve times an hour. Worse, at 12 checkpoints/hour against a
	// retention bound of 5, the checkpoints a user actually cared about were
	// evicted by identical idle ones within about twenty-five minutes.
	//
	// Set WORKSPACE_SYNC_INTERVAL_SECONDS to re-enable it where an unattended
	// workload has no one to press Save — a long batch job, say. The teardown
	// checkpoint still covers orderly shutdown either way; what no design can
	// cover is SIGKILL or node loss, and that is the exposure a fleet accepts
	// by leaving this off.
	interval := time.Duration(0)
	if v := os.Getenv("WORKSPACE_SYNC_INTERVAL_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			interval = time.Duration(n) * time.Second
		}
	}
	stop := make(chan struct{})
	go func() {
		if readOnly || interval <= 0 {
			return
		}
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				// wait 0: a save already running (an on-demand one) covers this
				// tick, so the backstop skips rather than queueing behind it.
				_, err := snapshot("", "", "periodic", 0)
				switch {
				case err == nil, errors.Is(err, store.ErrNotConfigured), errors.Is(err, errSaveInProgress):
				case errors.Is(err, store.ErrUnchanged), errors.Is(err, store.ErrNothingToSave):
					// Not a failure and not worth a line every interval: a
					// read-mostly session ticks like this for its whole life. The
					// skip is the CORRECT outcome -- writing would move LATEST over
					// another session's newer copy (store.ErrUnchanged).
				default:
					// Logged, not fatal: the backstop failing must not take the
					// sandbox down with it. The on-demand path is the primary
					// mechanism and is unaffected.
					log.Printf("periodic checkpoint failed: %v", err)
				}
			case <-stop:
				return
			}
		}
	}()

	// Teardown checkpoint, on SIGTERM (ADR-052 §14).
	//
	// This process is a NATIVE sidecar, so the kubelet signals it only after
	// the workload containers have exited. That ordering is what makes the
	// snapshot below meaningful: nothing is writing the tree any more, so this
	// is the one checkpoint in the whole design that is application-consistent
	// rather than merely crash-consistent.
	//
	// It replaces an operator-driven HTTP /flush that could never work — it
	// dialled the pod IP against a listener bound to loopback, and it fired on
	// CR deletion, by which time the pod was already gone. Doing it here needs
	// no network call, no authentication and no finalizer.
	//
	// It is an OPTIMISATION, not the durability mechanism. A node failure,
	// hard eviction or SIGKILL gives no graceful window at all, and no
	// signal-based design can promise one. The periodic backstop above is what
	// actually bounds loss; this narrows the window in the common case where
	// the shutdown is orderly.
	done := make(chan struct{})
	go func() {
		defer close(done)
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		<-sig
		log.Print("shutdown signal received")

		// 1. Stop the backstop, so the ticker cannot start an upload that
		//    races the final one for the same tree.
		close(stop)

		// 2. Unmount FUSE overlays before teardown checkpoint (§14.2).
		//    This ensures the final snapshot captures a clean tree, not a
		//    FUSE-mounted overlay that may be stale.
		unmountFuse(root, stagingRoot)

		// 2. Stop accepting new control requests, but let an in-flight
		//    /checkpoint finish — Shutdown waits for active handlers rather
		//    than cutting them off. Doing this BEFORE the final snapshot is
		//    what makes "no new sync work" true while it runs.
		shutCtx, cancelShut := context.WithTimeout(context.Background(), 30*time.Second)
		_ = srv.Shutdown(shutCtx)
		cancelShut()

		// 3. Take the final checkpoint, and only then exit.
		//
		// Bounded, because the kubelet SIGKILLs whatever is left when
		// terminationGracePeriodSeconds expires: better to give up a little
		// early and log it than to be killed mid-upload with no record of
		// having tried. The operator sets a floor on that grace period for
		// exactly this call (minWorkspaceGraceSeconds), and this budget is
		// deliberately shorter so the log line survives the deadline.
		budget := 90 * time.Second
		if v := os.Getenv("WORKSPACE_SYNC_SHUTDOWN_BUDGET_SECONDS"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				budget = time.Duration(n) * time.Second
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), budget)
		defer cancel()

		// Opt-out, not opt-in. A fleet that turns this off is saying its
		// workspace is reproducible or disposable, which is a claim only the
		// fleet can make — but the default has to be "keep the work", because
		// the cost of being wrong is a user's run and the cost of being right
		// is one upload at shutdown.
		if os.Getenv("WORKSPACE_SYNC_TEARDOWN") == "false" {
			log.Print("teardown checkpoint disabled by configuration — shutting down without saving")
			return
		}

		if readOnly {
			log.Print("read-only workspace — no teardown checkpoint")
			return
		}

		// Waits for an in-progress save only within its own budget.
		m, err := snapshotCtx(ctx, "", "", "teardown", -1)
		switch {
		case errors.Is(err, store.ErrNotConfigured):
			log.Print("teardown checkpoint skipped — object storage not configured")
		case errors.Is(err, store.ErrUnchanged):
			log.Print("teardown checkpoint skipped — workspace unchanged since restore")
		case errors.Is(err, store.ErrNothingToSave):
			log.Print("teardown checkpoint skipped — workspace has no files to save")
		case err != nil:
			// Loud, and still a clean exit. Failing the container here would
			// mark the pod as failed for a durability miss the backstop
			// already bounds, and would make an orderly shutdown look like a
			// crashed workload.
			// Says what was actually lost. With the backstop off, everything
			// since the last user-requested checkpoint is gone — quoting an
			// interval that is not running would understate it.
			lost := "all work since the last saved checkpoint"
			if interval > 0 {
				lost = fmt.Sprintf("up to %s of work", interval)
			}
			log.Printf("TEARDOWN CHECKPOINT FAILED (%s not in object storage): %v", lost, err)
		default:
			log.Printf("teardown checkpoint %s (%d files)", m.ID, len(m.Entries))
		}
	}()

	backstop := "off (checkpoints on request and at shutdown)"
	if interval > 0 {
		backstop = fmt.Sprintf("every %s", interval)
	}
	log.Printf("workspace-sync serving on 127.0.0.1:%s (periodic backstop: %s)", port, backstop)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	// ListenAndServe returns as soon as Shutdown is called, which is step 2 of
	// four. Returning here would exit before the checkpoint is written and
	// silently discard the very work this shutdown path exists to save.
	<-done
	return nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// errSaveInProgress: the snapshot slot was not free within the caller's wait.
var errSaveInProgress = errors.New("a save is in progress")

// saveState is one save, as reported to a caller that found the slot taken.
type saveState struct {
	Trigger      string
	Since        time.Time
	Finished     time.Time
	Outcome      string
	CheckpointID string
}

func (st *saveState) view() map[string]any {
	v := map[string]any{"trigger": st.Trigger, "since": st.Since.UTC().Format(time.RFC3339)}
	if st.Finished.IsZero() {
		v["elapsedSeconds"] = int(time.Since(st.Since).Seconds())
	} else {
		v["finished"] = st.Finished.UTC().Format(time.RFC3339)
		v["outcome"] = st.Outcome
		if st.CheckpointID != "" {
			v["checkpointId"] = st.CheckpointID
		}
	}
	return v
}
