package base

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/stencil-hq/ovnfs/overlay"
	"github.com/stencil-hq/ovnfs/rules"
	"github.com/stencil-hq/ovnfs/source"
	"github.com/stencil-hq/ovnfs/store"
)

// Remote bases serve the attached snapshot from metadata alone: listing and
// stat never move content. The first open of a file copies its blob into the
// store, where it stays cached and verified by id: from the mirror of the
// source's git origins when that holds it, else from the source machine. An
// open that crosses the link also prefetches the directory's other small files
// the mirror lacks, in one batch.

// fetchTimeout bounds how long a read waits for the source machine.
const fetchTimeout = 2 * time.Minute

// Prefetch limits for the batch an open sends for its siblings.
const (
	prefetchFileLimit  = 256 << 10
	prefetchBatchFiles = 500
	prefetchBatchBytes = 16 << 20
)

// remoteState is the lazily fetched side of a remote base.
type remoteState struct {
	b          *Base
	emptyDirs  map[string]bool // cache roots: present as empty directories on this machine
	mu         sync.Mutex
	prefetched map[string]bool // directories whose siblings were already requested
}

// newRemoteState indexes cache roots as empty directories visible in listings.
func newRemoteState(b *Base) *remoteState {
	r := &remoteState{b: b, emptyDirs: map[string]bool{}, prefetched: map[string]bool{}}
	for root, class := range b.classes {
		if class != rules.Cache {
			continue
		}
		if _, isFile := b.capture.Info[root]; isFile {
			continue
		}
		r.emptyDirs[root] = true
		parent := path.Dir(root)
		if parent == "." {
			parent = ""
		}
		if _, ok := b.sources[parent]; ok {
			b.children[parent] = append(b.children[parent], root)
		}
	}
	return r
}

// CreateRemote records a snapshot attached from another machine. res carries
// the offered leaves and their sizes and mtimes; its tree must already be in the store.
func CreateRemote(c Config, res source.Result, r Remote) (*Base, error) {
	if c.Store == nil {
		return nil, fmt.Errorf("base: missing store")
	}
	if err := os.MkdirAll(c.StateDir, 0o700); err != nil {
		return nil, err
	}
	if err := saveCapture(c.StateDir, &res); err != nil {
		return nil, err
	}
	m := Meta{ID: c.ID, ForkTime: time.Now().UTC(), Tree: res.Tree, Remote: &r}
	return load(c, m, &res)
}

// ensure fetches one blob unless the store already holds it.
func (r *remoteState) ensure(p string, id store.OID) error {
	if r.b.config.Store.Has(id) || r.fromMirror(p, id) {
		return nil
	}
	if r.b.config.Fetch == nil {
		return &fs.PathError{Op: "fetch", Path: p, Err: syscall.EIO}
	}
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()
	if err := r.b.config.Fetch(ctx, []Want{{ID: id, Path: p, Size: r.b.capture.Info[p].Size}}); err != nil {
		if r.b.config.OnFault != nil {
			r.b.config.OnFault(fmt.Sprintf("fetch %s from %s: %v", p, r.b.meta.Remote.Source, err))
		}
		return &fs.PathError{Op: "fetch", Path: p, Err: syscall.EIO}
	}
	return nil
}

// fromMirror copies id's blob from the mirror into the store; false when the
// mirror lacks it or returned bytes that do not hash to id.
func (r *remoteState) fromMirror(p string, id store.OID) bool {
	if !r.b.mirrored(id) {
		return false
	}
	data, err := r.b.config.Mirror.Blob(id)
	if err == nil {
		var got store.OID
		if got, err = r.b.config.Store.PutBytes(data); err == nil && got != id {
			err = fmt.Errorf("content hashes to %s", got)
		}
	}
	if err != nil {
		if r.b.config.OnFault != nil {
			r.b.config.OnFault(fmt.Sprintf("read %s from the git mirror: %v; fetching it from %s", p, err, r.b.meta.Remote.Source))
		}
		return false
	}
	return true
}

// mirrored reports whether the mirror of this base's git origins holds id.
func (b *Base) mirrored(id store.OID) bool { return b.config.Mirror != nil && b.config.Mirror.Has(id) }

// prefetch requests, in one batch, the small uncached files of the directory
// holding p and then of its subdirectories, breadth first, within a budget.
// Every directory the batch covered completely is not prefetched again: a
// recursive reader (grep, a build) then pays one round trip per subtree, not
// one per directory.
func (r *remoteState) prefetch(p string) {
	dir := path.Dir(p)
	if dir == "." {
		dir = ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.prefetched[dir] || r.b.config.Fetch == nil {
		return
	}
	var wants []Want
	var total int64
	full := false
	for queue := []string{dir}; len(queue) > 0 && !full; queue = queue[1:] {
		d := queue[0]
		if r.prefetched[d] {
			continue
		}
		var subdirs []string
		for _, q := range r.b.children[d] {
			e, ok := r.b.sources[q]
			if !ok {
				continue
			}
			if e.mode.IsDir() {
				subdirs = append(subdirs, q)
				continue
			}
			size := r.b.capture.Info[q].Size
			if q == p || !e.mode.IsRegular() || size > prefetchFileLimit || r.b.config.Store.Has(e.oid) || r.b.mirrored(e.oid) {
				continue
			}
			if len(wants) == prefetchBatchFiles || total+size > prefetchBatchBytes {
				full = true
				break
			}
			wants = append(wants, Want{ID: e.oid, Path: q, Size: size})
			total += size
		}
		if !full {
			r.prefetched[d] = true
			queue = append(queue, subdirs...)
		}
	}
	if len(wants) == 0 {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
		defer cancel()
		_ = r.b.config.Fetch(ctx, wants)
	}()
}

// Uncached returns the blob a remote base would fetch over the link to read the
// regular file p; false for local bases, other entries, and content already
// stored or held by the git mirror.
func (b *Base) Uncached(p string) (Want, bool) {
	if b.remote == nil {
		return Want{}, false
	}
	p = overlay.Clean(p)
	e, ok := b.sources[p]
	if !ok || !e.mode.IsRegular() {
		return Want{}, false
	}
	return b.want(p, e)
}

// Missing lists, in no particular order, the blobs a remote base would still
// fetch over the link: those of its regular files and symlinks that neither the
// store nor the git mirror holds. Once they are fetched, the base reads without
// the link. Local bases miss nothing.
func (b *Base) Missing() []Want {
	if b.remote == nil {
		return nil
	}
	var wants []Want
	for p, e := range b.sources {
		if !e.mode.IsRegular() && e.mode&os.ModeSymlink == 0 {
			continue
		}
		if w, ok := b.want(p, e); ok {
			wants = append(wants, w)
		}
	}
	return wants
}

// want is the fetch the blob of entry e at p needs, unless the store or the
// git mirror already holds it.
func (b *Base) want(p string, e entry) (Want, bool) {
	if b.config.Store.Has(e.oid) || b.mirrored(e.oid) {
		return Want{}, false
	}
	return Want{ID: e.oid, Path: p, Size: b.capture.Info[p].Size}, true
}

// FetchMatching fetches, in one batch, every uncached regular file of a
// remote base that keep selects, so readers that need a known set of files
// pay one round trip instead of one per file. Local bases fetch nothing.
func (b *Base) FetchMatching(ctx context.Context, keep func(p string) bool) error {
	if b.remote == nil || b.config.Fetch == nil {
		return nil
	}
	var wants []Want
	for p := range b.sources {
		if keep(p) {
			if w, ok := b.Uncached(p); ok {
				wants = append(wants, w)
			}
		}
	}
	if len(wants) == 0 {
		return nil
	}
	return b.config.Fetch(ctx, wants)
}

// remoteBottom serves a remote base to the overlay.
type remoteBottom struct{ b *Base }

// info describes p from the offer, without fetching anything.
func (v remoteBottom) info(p string) (os.FileInfo, error) {
	b := v.b
	p = overlay.Clean(p)
	if e, ok := b.sources[p]; ok {
		meta := b.capture.Info[p]
		mtime := meta.Mtime
		if mtime.IsZero() {
			mtime = b.meta.ForkTime
		}
		return capturedInfo{name: path.Base("/" + p), mode: e.mode, size: meta.Size, mtime: mtime}, nil
	}
	if b.remote.emptyDirs[p] {
		return capturedInfo{name: path.Base(p), mode: os.ModeDir | 0o755, mtime: b.meta.ForkTime}, nil
	}
	return nil, absent(p)
}

// Lstat returns the offered metadata; it never fetches content.
func (v remoteBottom) Lstat(p string) (os.FileInfo, error) { return v.info(p) }

// ReadDir lists offered children (and cache roots as empty directories).
func (v remoteBottom) ReadDir(p string) ([]os.FileInfo, error) {
	p = overlay.Clean(p)
	info, err := v.info(p)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, &fs.PathError{Op: "readdir", Path: p, Err: syscall.ENOTDIR}
	}
	children := v.b.children[p]
	out := make([]os.FileInfo, 0, len(children))
	for _, name := range children {
		if child, err := v.info(name); err == nil {
			out = append(out, child)
		}
	}
	return out, nil
}

// Open fetches a regular file's blob on first use and serves the stored copy,
// from memory when the store caches it.
func (v remoteBottom) Open(p string) (overlay.Content, error) {
	p = overlay.Clean(p)
	e, ok := v.b.sources[p]
	if !ok {
		return nil, absent(p)
	}
	if !e.mode.IsRegular() {
		return nil, &fs.PathError{Op: "open", Path: p, Err: syscall.EISDIR}
	}
	if !v.b.config.Store.Has(e.oid) {
		// Siblings ride along on a link miss's round trip; a hit, or a blob the
		// mirror holds, has none to share.
		if !v.b.mirrored(e.oid) {
			v.b.remote.prefetch(p)
		}
		if err := v.b.remote.ensure(p, e.oid); err != nil {
			return nil, err
		}
	}
	if data, ok := v.b.config.Store.Cached(e.oid); ok {
		return overlay.ByteContent(data), nil
	}
	return v.b.config.Store.Open(e.oid)
}

// Readlink returns a symlink's target, fetching it if needed.
func (v remoteBottom) Readlink(p string) (string, error) {
	p = overlay.Clean(p)
	e, ok := v.b.sources[p]
	if !ok || e.mode&os.ModeSymlink == 0 {
		return "", &fs.PathError{Op: "readlink", Path: p, Err: syscall.EINVAL}
	}
	data, err := v.b.Blob(p, e.oid)
	return string(data), err
}

// CopyTo materializes p (a whole directory tree, for renames) at the new path dst.
func (v remoteBottom) CopyTo(p, dst string) error {
	p = overlay.Clean(p)
	info, err := v.info(p)
	if err != nil {
		return err
	}
	switch {
	case info.IsDir():
		if err := os.Mkdir(dst, info.Mode().Perm()|0o700); err != nil {
			return err
		}
		for _, child := range v.b.children[p] {
			if err := v.CopyTo(child, filepath.Join(dst, path.Base(child))); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		if err := os.Chmod(dst, info.Mode().Perm()); err != nil {
			return err
		}
	case info.Mode()&os.ModeSymlink != 0:
		target, err := v.Readlink(p)
		if err != nil {
			return err
		}
		if err := os.Symlink(target, dst); err != nil {
			return err
		}
	default:
		e := v.b.sources[p]
		if err := v.b.remote.ensure(p, e.oid); err != nil {
			return err
		}
		if err := v.b.config.Store.CloneBlob(e.oid, dst, info.Mode().Perm()); err != nil {
			return err
		}
	}
	return setTimes(dst, info.ModTime(), info.Mode()&os.ModeSymlink != 0)
}

// capturedInfo is captured or offered metadata presented as a FileInfo owned by this process's user.
type capturedInfo struct {
	name  string
	mode  os.FileMode
	size  int64
	mtime time.Time
}

// Name returns the base name.
func (i capturedInfo) Name() string { return i.name }

// Size returns the offered size (0 for directories).
func (i capturedInfo) Size() int64 { return i.size }

// Mode returns the offered type and permissions.
func (i capturedInfo) Mode() os.FileMode { return i.mode }

// ModTime returns the offered modification time.
func (i capturedInfo) ModTime() time.Time { return i.mtime }

// IsDir reports a directory.
func (i capturedInfo) IsDir() bool { return i.mode.IsDir() }

// Sys supplies host-style stat data so frontends report this user as owner.
func (i capturedInfo) Sys() any { return syntheticSys(i.mode, i.size, i.mtime) }
