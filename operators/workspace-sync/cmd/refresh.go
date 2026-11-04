package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"

	"github.com/kamarkat/kamarkat/operators/workspace-sync/internal/store"
)

// generations is the shared workspace of a READER that may refresh it
// (WORKSPACE_REFRESH, ADR-052 §14.9).
//
// The workload sees one path, WORKSPACE_ROOT (/workspace/.global). In this mode
// that path is a relative symlink to one complete restore of a checkpoint:
//
//	/workspace/.global      -> .global-gen/<n>
//	/workspace/.global-gen/<n>
//
// A refresh restores the newer checkpoint into generation n+1 beside the one in
// use and only then repoints the link, with a rename -- atomic, so a reader
// sees either the whole old tree or the whole new one, never a mix. A refresh
// that fails anywhere before the rename leaves the link, and the tree it names,
// as they were.
//
// The previous generation is kept, so a process that resolved the link just
// before the swap keeps reading a tree that exists; the one before that is
// removed. Both names are derived content the session never checkpoints: the
// snapshot walk skips symlinks, and .global-gen is in store.skipDir.
type generations struct {
	link string // WORKSPACE_ROOT: the path the workload reads
	dir  string // <parent>/.global-gen
	n    int    // the generation the link names
	id   string // the checkpoint that generation holds; "" for none yet
}

const generationsDirSuffix = "-gen"

// refreshEnabled reports whether this instance was asked to serve /refresh, and
// refuses a configuration in which a refresh could not mean what it says.
func refreshEnabled(s *store.Store) (bool, error) {
	if os.Getenv("WORKSPACE_REFRESH") != "true" {
		return false, nil
	}
	switch {
	case s == nil:
		return false, fmt.Errorf("WORKSPACE_REFRESH is set but object storage is not configured: there is nothing to refresh from")
	case !s.ReadOnly():
		return false, fmt.Errorf("WORKSPACE_REFRESH is set on a writable workspace: only a reader refreshes, a writer's tree is its own")
	case s.Pinned() != "":
		return false, fmt.Errorf("WORKSPACE_REFRESH is set on a workspace pinned to %s: a pin is one exact revision", s.Pinned())
	case !s.NoArchive():
		return false, fmt.Errorf("WORKSPACE_REFRESH is set on an archive-mounted workspace: refresh restores file by file")
	}
	return true, nil
}

// startGenerations lays the workspace out as generation 1 holding LATEST (or
// nothing, for a workspace never checkpointed) and points the link at it.
//
// Whatever is at the link and the generations directory is removed first. Both
// are derived and never checkpointed, and nothing reads them yet: the workload
// starts only after the readiness marker this restore precedes.
func startGenerations(ctx context.Context, root string, s *store.Store) (*generations, error) {
	g := &generations{link: root, dir: root + generationsDirSuffix}
	if err := os.RemoveAll(g.link); err != nil {
		return nil, fmt.Errorf("clearing %s: %w", g.link, err)
	}
	if err := os.RemoveAll(g.dir); err != nil {
		return nil, fmt.Errorf("clearing %s: %w", g.dir, err)
	}
	if err := os.MkdirAll(g.dir, 0o777); err != nil {
		return nil, err
	}
	uid := workspaceUID()
	if err := chownRootIfNeeded(g.dir, uid); err != nil {
		return nil, fmt.Errorf("chown %s to uid %d: %w", g.dir, uid, err)
	}
	id, err := s.Latest(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading LATEST pointer: %w", err)
	}
	if id == "" {
		log.Print("no checkpoint for the shared workspace yet — generation 1 is empty")
	}
	files, err := g.build(ctx, s, 1, id)
	if err != nil {
		return nil, err
	}
	if err := g.point(1); err != nil {
		os.RemoveAll(g.path(1))
		return nil, err
	}
	g.n, g.id = 1, id
	log.Printf("shared workspace generation 1 holds checkpoint %q (%d files); refresh enabled", id, files)
	return g, nil
}

func (g *generations) path(n int) string { return filepath.Join(g.dir, strconv.Itoa(n)) }

// build restores checkpoint id into generation n, owned by the workload. On
// failure nothing of generation n remains.
func (g *generations) build(ctx context.Context, s *store.Store, n int, id string) (files int, err error) {
	p := g.path(n)
	if err := os.RemoveAll(p); err != nil {
		return 0, fmt.Errorf("clearing generation %d: %w", n, err)
	}
	defer func() {
		if err != nil {
			os.RemoveAll(p)
		}
	}()
	if err := os.Mkdir(p, 0o777); err != nil {
		return 0, err
	}
	if id != "" {
		m, err := s.GetManifest(ctx, id)
		if err != nil {
			return 0, fmt.Errorf("reading checkpoint %s: %w", id, err)
		}
		if err := s.Restore(ctx, p, m); err != nil {
			return 0, fmt.Errorf("restoring checkpoint %s: %w", id, err)
		}
		files = len(m.Entries)
	}
	uid := workspaceUID()
	if err := chownTree(p, uid); err != nil {
		return 0, fmt.Errorf("chown generation %d to uid %d: %w", n, uid, err)
	}
	return files, nil
}

// point repoints the link at generation n: a new link beside it, renamed over
// it. rename(2) replaces the old link atomically.
func (g *generations) point(n int) error {
	target, err := filepath.Rel(filepath.Dir(g.link), g.path(n))
	if err != nil {
		return err
	}
	tmp := g.link + ".next"
	if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clearing %s: %w", tmp, err)
	}
	if err := os.Symlink(target, tmp); err != nil {
		return fmt.Errorf("creating link to generation %d: %w", n, err)
	}
	if err := os.Rename(tmp, g.link); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("switching %s to generation %d: %w", g.link, n, err)
	}
	return nil
}

// advance makes checkpoint id the workspace the workload sees. The caller holds
// the save slot.
func (g *generations) advance(ctx context.Context, s *store.Store, id string) (int, error) {
	next := g.n + 1
	files, err := g.build(ctx, s, next, id)
	if err != nil {
		return 0, err
	}
	if err := g.point(next); err != nil {
		os.RemoveAll(g.path(next))
		return 0, err
	}
	prev := g.n
	g.n, g.id = next, id
	// Keep the generation just replaced; remove the one before it.
	if prev > 1 {
		if err := os.RemoveAll(g.path(prev - 1)); err != nil {
			log.Printf("WARNING: could not remove shared workspace generation %d: %v", prev-1, err)
		}
	}
	return files, nil
}
