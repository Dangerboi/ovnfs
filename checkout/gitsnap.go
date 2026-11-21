package checkout

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/stencil-hq/ovnfs/base"
	"github.com/stencil-hq/ovnfs/git"
	"github.com/stencil-hq/ovnfs/internal/hostfile"
	"github.com/stencil-hq/ovnfs/source"
	"github.com/stencil-hq/ovnfs/store"
)

// A snapshot of a git checkout keeps its private git directory (minus the shared
// objects link) as a tree in the store: files are content-addressed blobs, so an
// unchanged index, config or ref costs nothing per snapshot, and the snapshot
// record holds just the tree id, which garbage collection treats as a root.

// gitRacy is how recently a file may have changed and still change again without
// changing its size or mtime; such files are read again rather than trusted to
// the cache (git's racy-clean rule, as in package source's stat cache).
const gitRacy = 2 * time.Second

// gitCache remembers which stored blob holds each file of a checkout's private git
// directory, so a snapshot neither reads nor stores files unchanged since the last one.
type gitCache struct {
	mu    sync.Mutex // serializes saves of one checkout's git directory
	epoch string     // store epoch the cached blobs were known stored in
	files map[string]gitCached
}

// gitCached is a cached file's identity when it was stored.
type gitCached struct {
	size  int64
	mtime int64
	mode  fs.FileMode
	file  store.File
}

// saveGitdir stores dir, a checkout's private git directory, as a tree and returns
// its id. Left out are the shared objects links (the root's and its absorbed
// submodules'), symlinks named hooks of any target (the links to the source
// repositories' hooks that linkHooks makes; a real hooks directory is kept), and
// lock files of in-flight git commands. Empty directories are kept, since git
// requires refs/ to exist.
func saveGitdir(st *store.Store, dir string, c *gitCache) (store.OID, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// No object is removed while held, and Build keeps the tree's objects safe from
	// Sweep afterwards, so cached blobs are stored if the epoch has not moved.
	release := st.Hold()
	defer release()
	epoch := st.Epoch()
	if c.epoch != epoch {
		c.files = nil
	}
	trusted := time.Now().Add(-gitRacy).UnixNano()
	files := make(map[string]store.File)
	next := make(map[string]gitCached, len(c.files))
	var dirs []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p != dir && errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if p == dir {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "objects" || (d.Type()&fs.ModeSymlink != 0 && d.Name() == "objects") || isHooksLink(d.Name(), d.Type()&fs.ModeSymlink != 0) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		// A directory named *.lock is a submodule's git directory, not a lock.
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".lock") {
			return nil
		}
		switch {
		case d.IsDir():
			dirs = append(dirs, rel)
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return skipVanished(err)
			}
			id, err := st.PutBytes([]byte(target))
			if err != nil {
				return err
			}
			files[rel] = store.File{Mode: store.ModeSymlink, ID: id}
		case d.Type().IsRegular():
			info, err := d.Info()
			if err != nil {
				return skipVanished(err)
			}
			mtime := info.ModTime().UnixNano()
			old, ok := c.files[rel]
			if ok && old.size == info.Size() && old.mtime == mtime && old.mode == info.Mode() && mtime < trusted {
				files[rel], next[rel] = old.file, old
				return nil
			}
			mode, _ := store.ModeOf(info.Mode())
			// A rewritten index mostly repeats the last one: keep it as a delta.
			id, _, err := st.PutFileLike(p, old.file.ID)
			if err != nil {
				return skipVanished(err)
			}
			f := store.File{Mode: mode, ID: id}
			files[rel] = f
			next[rel] = gitCached{size: info.Size(), mtime: mtime, mode: info.Mode(), file: f}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	// Only the innermost empty directories are leaves: one holding nothing but
	// empty directories is their parent, not a leaf itself.
	parents := make(map[string]bool, len(dirs))
	markParents := func(p string) {
		for d := path.Dir(p); d != "." && !parents[d]; d = path.Dir(d) {
			parents[d] = true
		}
	}
	for p := range files {
		markParents(p)
	}
	for _, d := range dirs {
		markParents(d)
	}
	for _, d := range dirs {
		if !parents[d] {
			files[d] = store.File{Mode: store.ModeDir, ID: store.EmptyTree}
		}
	}
	tree, err := st.Build(files)
	if err != nil {
		return "", err
	}
	c.epoch, c.files = epoch, next
	return tree, nil
}

// skipVanished ignores a file a concurrent git command removed during the walk.
func skipVanished(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// materializeGitdir writes the git directory stored as tree into the new path dst,
// linking its objects, and those of the absorbed submodules' git directories it
// holds, to the base's shared object directories (objects of a source.Repo.Module).
// It leaves out symlinks named hooks, which older builds stored; the hooks are
// linked afterwards, with the other git directories of a new checkout (linkHooks).
func materializeGitdir(st *store.Store, tree store.OID, dst string, objects func(module string) string, modules []source.Repo) error {
	files, err := st.Flatten(tree)
	if err != nil {
		return err
	}
	if err := os.Mkdir(dst, 0o755); err != nil {
		return err
	}
	for p, f := range files {
		if isHooksLink(path.Base(p), f.Mode == store.ModeSymlink) {
			continue // one an older build stored, often the user's own; linkHooks makes the new checkout's
		}
		target := filepath.Join(dst, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		switch f.Mode {
		case store.ModeDir:
			err = os.MkdirAll(target, 0o755)
		case store.ModeSymlink:
			var link []byte
			if link, err = st.ReadBlob(f.ID); err == nil {
				err = os.Symlink(string(link), target)
			}
		case store.ModeExec:
			err = st.CloneBlob(f.ID, target, 0o755)
		default:
			err = st.CloneBlob(f.ID, target, 0o644)
		}
		if err != nil {
			return err
		}
	}
	for _, sub := range modules {
		dir := filepath.Join(dst, filepath.FromSlash(sub.Module))
		if _, err := os.Lstat(filepath.Join(dir, "HEAD")); errors.Is(err, os.ErrNotExist) {
			continue // removed in the checkout the snapshot was taken of
		}
		if _, err := os.Lstat(filepath.Join(dir, "objects")); err == nil {
			continue // a real object directory, of a submodule cloned again in the checkout
		}
		if err := hostfile.Symlink(objects(sub.Module), filepath.Join(dir, "objects")); err != nil {
			return err
		}
	}
	return hostfile.Symlink(objects(""), filepath.Join(dst, "objects"))
}

// saveGit stores a git checkout's private git directory for its next snapshot,
// outside the registry lock; it returns "" for a checkout without one.
func (r *Registry) saveGit(h *checkoutRecord) (store.OID, error) {
	if !h.meta.Git {
		return "", nil
	}
	tree, err := saveGitdir(r.store, r.gitdirPath(h.meta.dir()), &h.git)
	if err != nil && h.retired.Load() {
		return "", failure("not_found", "no checkout %s", h.meta.Name)
	}
	return tree, err
}

// prepareGit writes the private git directory a new checkout from a snapshot starts
// with into a fresh directory below git/, outside the registry lock; installation
// renames it into place, and reopening the registry removes it if it never is. It
// returns "" when the snapshot has no git directory. A snapshot taken by a build
// that copied whole directories (legacy, no tree) is read from its copy. Every
// submodule git directory in it loses the work tree git may have recorded for
// the checkout the snapshot was taken of (git.UnsetModuleWorktrees), and it gets
// the hooks links of a new checkout (linkHooks), unless it holds a real hooks
// directory; a hooks symlink the snapshot held is never kept.
func (r *Registry) prepareGit(ctx context.Context, tree store.OID, legacy, baseID string) (string, error) {
	if tree == "" && (legacy == "" || !exists(legacy)) {
		return "", nil
	}
	tmp, err := os.MkdirTemp(filepath.Join(r.config.State, "git"), ".new-")
	if err != nil {
		return "", err
	}
	dst := filepath.Join(tmp, "git")
	var view *base.Base
	var modules []source.Repo
	r.mu.RLock()
	if b := r.bases[baseID]; b != nil {
		view = b.view
		modules = submodules(view)
	}
	r.mu.RUnlock()
	if tree != "" {
		objects := func(module string) string { return r.objectsPath(baseID, module) }
		err = materializeGitdir(r.store, tree, dst, objects, modules)
	} else {
		err = git.CopyGitdir(legacy, dst, r.objectsPath(baseID, ""))
		if err == nil {
			err = dropHooksLinks(dst)
		}
	}
	if err == nil {
		err = git.UnsetModuleWorktrees(dst)
	}
	if err == nil && view != nil {
		err = linkHooks(dst, view)
	}
	if err != nil {
		return "", errors.Join(err, os.RemoveAll(tmp))
	}
	return dst, nil
}

// isHooksLink reports whether a git directory entry with this name, a symlink or
// not (a junction counts as one), is a hooks link, which snapshots never keep (a
// real hooks directory is kept).
func isHooksLink(name string, symlink bool) bool {
	return name == "hooks" && symlink
}

// dropHooksLinks removes every hooks symlink (or junction) from a copied git
// directory, at any depth, as saveGitdir leaves them out and materializeGitdir
// skips them; a real hooks directory stays.
func dropHooksLinks(gitdir string) error {
	return filepath.WalkDir(gitdir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		symlink := d.Type()&fs.ModeSymlink != 0
		if d.Type()&fs.ModeIrregular != 0 { // a Windows junction
			if info, err := d.Info(); err == nil {
				symlink = hostfile.Linked(info, p).Mode()&fs.ModeSymlink != 0
			}
		}
		if !isHooksLink(d.Name(), symlink) {
			return nil
		}
		return os.Remove(p)
	})
}

// discardGit removes a prepared git directory installation did not move into place.
func discardGit(ready string) error {
	if ready == "" {
		return nil
	}
	return os.RemoveAll(filepath.Dir(ready))
}
