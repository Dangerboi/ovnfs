// Package overlay merges a writable upper over indexed immutable layers and a
// read-only bottom. AUFS whiteouts and opaque directories remain a 1:1 on-disk
// diff. The filesystem is upper's only writer; it is not a symlink sandbox.
package overlay

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/helper/chroot"
	"github.com/stencil-hq/ovnfs/internal/hostfile"
	"github.com/stencil-hq/ovnfs/internal/ident"
)

// Marker names are reserved in every component of an overlay path.
const (
	whPrefix   = ".wh."
	metaPrefix = ".wh..wh."
	opaqueName = metaPrefix + ".opq"
	// redirectName holds a directory's [Entry.Redirect] origin as its content.
	redirectName = metaPrefix + "redirect"
	workName     = metaPrefix + "work"
	writeFlags   = os.O_WRONLY | os.O_RDWR | os.O_APPEND | os.O_CREATE | os.O_TRUNC
)

// FS is a concurrent-safe writable upper over immutable layers and a bottom.
// The index and cached inode metadata are kept current under mu.
type FS struct {
	mu          sync.RWMutex
	upper, work string
	layers      []*Layer
	bottom      Bottom
	bottomEpoch uint64
	index       *Layer
	tmpSeq      uint64
	pathIDs     map[string]inodeKey
	aliases     map[inodeKey]inodeNames
	paths       map[ident.ID]string   // upper name holding each identity while the index is loaded, see [FS.Locate]
	removed     map[ident.ID]struct{} // State identities removed in this overlay; never revive via [FS.Vanished]
	ids         *idStore              // identities of upper files; nil for a read-only stack
	mem         *Memory               // nil: every write goes to upper on disk
	files       map[string]*memFile   // upper names whose data is in memory
	hosts       map[*file]struct{}    // open descriptors of upper files, see [FS.CloseFiles]
	writers     map[inodeKey]int      // open writable descriptors per upper inode, see [FS.upperInfo]
	staging     sync.RWMutex          // shared by an unlocked writeback, see [FS.writeBack]
	spillLogged atomic.Bool           // the first failed spill is logged, see [Memory.park]

	rootInfo      os.FileInfo         // root attributes while the index is cold
	cold          coldDirs            // upper directories cold lookups listed, see [FS.lookupRead]
	origins       map[string]string   // rename hints kept while the index is cold
	lastAccess    atomic.Int64        // Unix nanoseconds of the last index use, see [FS.Cool]
	memoryHandles int                 // open memory handles, including unlinked files'
	names         atomic.Uint64       // see [FS.Names]
	volume        nameRule            // what the upper's volume does with names, see names.go
	unsettled     map[string]struct{} // upper directories whose host time differs from the index's, see [FS.lazyDirs]
	ops           []*hostOp           // host changes in flight outside mu or queued, oldest first, see [FS.lockPaths]
	byPath        opIndex             // ops by the names they touch
	byChange      opIndex             // ops by the names they change

	// The queue of host changes the index made already, see queue.go.
	queued     int             // queued changes in ops
	appliers   int             // workers making queued changes, see [FS.applier]
	parkedOp   *hostOp         // a queued change that failed and is retried
	parkedErr  error           // its failure
	parkSignal chan struct{}   // closed and replaced when the queue parks
	applyLimit *int            // a test's count of changes the applier may still make; nil: any
	urgent     map[*hostOp]int // queued changes calls wait for, made first, see [FS.next]
	ready      []*hostOp       // queued changes no older one must precede, oldest first
}

// Names returns the generation of the overlay's namespace, which advances
// after every change that may move or remove a visible entry: Remove,
// Rename, Rebase, Discard, a copy-up that separates hard links, or recovering
// a missing upper directory. Creating an entry moves none, nor does an
// ordinary copy-up, spill or freeze.
// A path [FS.Locate] returned for an identity stays that identity's while
// Names, read before the Locate, is unchanged:
// the export caches resolutions on it.
func (f *FS) Names() uint64 { return f.names.Load() }

// Filesystem interface assertions keep the overlay compatible with protocols.
var (
	_ billy.Filesystem = (*FS)(nil)
	_ billy.Change     = (*FS)(nil)

	// Every handle FS opens knows its entry's identity.
	_ ident.Identifier = (*file)(nil)
	_ ident.Identifier = (*contentFile)(nil)
	_ ident.Identifier = (*memHandle)(nil)
)

// New opens upper over layers (top first) and bottom. An empty upper is
// read-only. With mem, small written files stay in memory under its budget
// until they spill into upper (see [Memory]); nil keeps every write on disk.
func New(upper string, layers []*Layer, bottom Bottom, mem *Memory) (*FS, error) {
	f := &FS{layers: slices.Clone(layers), bottom: bottom, hosts: make(map[*file]struct{}), writers: make(map[inodeKey]int), unsettled: make(map[string]struct{}), parkSignal: make(chan struct{}), urgent: make(map[*hostOp]int)}
	for _, l := range layers {
		if l == nil {
			return nil, errors.New("overlay: nil layer")
		}
	}
	if upper == "" {
		return f, nil
	}
	f.pathIDs = make(map[string]inodeKey)
	f.aliases = make(map[inodeKey]inodeNames)
	if mem != nil {
		f.mem, f.files = mem, make(map[string]*memFile)
	}
	var err error
	f.upper, err = filepath.Abs(upper)
	if err != nil {
		return nil, err
	}
	for _, l := range layers {
		if within(f.upper, l.dir) || within(l.dir, f.upper) {
			return nil, errors.New("overlay: upper and layers must not contain each other")
		}
	}
	if err := os.MkdirAll(f.upper, 0o755); err != nil {
		return nil, err
	}
	if f.ids, err = newUpperStore(f.upper, f.layers); err != nil {
		return nil, err
	}
	if err := f.inheritRoot(); err != nil {
		return nil, err
	}
	f.work = filepath.Join(f.upper, workName)
	if err := os.RemoveAll(f.work); err != nil {
		return nil, err
	}
	if err := os.Mkdir(f.work, 0o700); err != nil {
		return nil, err
	}
	if f.volume, err = probeNames(f.work); err != nil {
		return nil, err
	}
	f.index, err = walkLayer(f.upper, true, false, "", f.ids)
	if err != nil {
		return nil, err
	}
	// A fresh upper must not change the visible root's timestamp.
	if len(f.index.entries) == 1 {
		idx := f.index
		f.index = nil
		_, info, e := f.lookup("")
		f.index = idx
		if e == nil {
			if err := os.Chtimes(f.upper, info.ModTime(), info.ModTime()); err != nil {
				return nil, err
			}
			if err := f.refresh(""); err != nil {
				return nil, err
			}
		}
	}
	f.rootInfo = f.index.entries[""].Info
	f.index = nil
	return f, nil
}

// inheritRoot gives the upper's root directory the identity of the lower
// root it covers, so the checkout's root keeps one identity while freezes
// replace its upper: the frozen root carries it on into the next layer.
func (f *FS) inheritRoot() error {
	if id, err := f.ids.stored(f.upper); err != nil || id != 0 {
		return err
	}
	var lower os.FileInfo
	if len(f.layers) != 0 {
		lower = f.layers[0].view().entries[""].Info
	} else if f.bottom != nil {
		info, err := f.bottom.Lstat("")
		if err != nil && !missing(err) {
			return err
		}
		lower = info
	}
	if lower == nil {
		return nil
	}
	if id, ok := ident.Of(lower); ok {
		return f.ids.persist(f.upper, id)
	}
	return nil
}

// Rebase changes the immutable bottom without disturbing open files or inode identities.
func (f *FS) Rebase(bottom Bottom) {
	defer f.names.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bottom = bottom
	f.bottomEpoch++
}

// Relayer replaces frozen layer old with fresh, the same layer restored from its
// metadata, without disturbing open files or inode identities.
func (f *FS) Relayer(old, fresh *Layer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, l := range f.layers {
		if l == old {
			f.layers[i] = fresh
		}
	}
}

// Upper returns the host directory receiving writes, or empty for read-only views.
func (f *FS) Upper() string { return f.upper }

// UpperFile reports a regular file owned by this writable upper, including
// memory files. Lease policies use it to exclude immutable and live lower data;
// disk metadata is refreshed so hard-link aliases cannot gain separate caches.
func (f *FS) UpperFile(name string) (os.FileInfo, bool) {
	f.await(name)
	f.mu.RLock()
	defer f.mu.RUnlock()
	p := Clean(name)
	if f.upper == "" || reserved(p) {
		return nil, false
	}
	if f.index == nil {
		// A cold index has no memory files (see [FS.Cool]), so the host decides.
		s, info, err := f.lookupRead(p)
		return info, err == nil && s.upper && info.Mode().IsRegular()
	}
	entry, ok := f.index.entries[p]
	if !ok || entry.Kind != KindFile {
		return nil, false
	}
	if m := f.files[p]; m != nil {
		return m.info(path.Base(p)), true
	}
	if f.changing(p) != nil {
		// A host operation is changing the name; the index holds it until done.
		return entry.Info, entry.Info.Mode().IsRegular()
	}
	info, err := lstatIndexed(f.up(p), entry.Info)
	if err != nil {
		return nil, false
	}
	info = linksOf(info, entry.Info)
	return info, info.Mode().IsRegular()
}

// Stack returns an independent slice of immutable layers, top first.
func (f *FS) Stack() []*Layer {
	f.mu.RLock() // [FS.Relayer] swaps entries
	defer f.mu.RUnlock()
	return slices.Clone(f.layers)
}

// Freeze transfers this upper's index to a frozen layer after the caller has
// flushed it ([FS.Flush]), renamed the upper to dir and removed its work
// directory. The caller must exclude concurrent operations and never use this
// FS after a successful call.
func (f *FS) Freeze(dir string) (*Layer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.drainLocked(); err != nil {
		return nil, upperError(err)
	}
	if f.upper == "" {
		return nil, &fs.PathError{Op: "freeze", Path: dir, Err: syscall.EROFS}
	}
	if len(f.files) != 0 {
		return nil, errors.New("overlay: freeze before flushing memory files")
	}
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := f.settleDirs(root); err != nil {
		return nil, err
	}
	info, err := statHost(root, f.ids)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, &fs.PathError{Op: "freeze", Path: root, Err: syscall.ENOTDIR}
	}
	l := f.index
	if l == nil {
		// A cold upper is indexed where the caller renamed it.
		if l, err = indexLayer(root, false); err != nil {
			return nil, err
		}
		l.addOrigins(f.origins)
	}
	if wrote, err := l.saveOrigins(root); err != nil {
		return nil, err
	} else if wrote {
		if info, err = statHost(root, f.ids); err != nil {
			return nil, err
		}
	}
	l.synthetic = f.ids.syn.mark()
	e := entry(info)
	e.Opaque = l.entries[""].Opaque
	l.set("", e)
	l.dir = root
	f.index = nil
	f.pathIDs = nil
	f.aliases = nil
	f.paths = nil
	f.ids = nil
	f.upper, f.work = "", ""
	return l, nil
}

// Empty reports whether the upper has no data or markers outside its work
// dir and its identity table.
func (f *FS) Empty() (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.upper == "" {
		return true, nil
	}
	if len(f.files) != 0 {
		return false, nil
	}
	// The upper on disk is complete only once the queue is made.
	if err := f.drainLocked(); err != nil {
		return false, err
	}
	items, err := os.ReadDir(f.upper)
	if err != nil {
		return false, err
	}
	for _, item := range items {
		if item.Name() != workName && item.Name() != tableName {
			return false, nil
		}
	}
	return true, nil
}

// source identifies the upper, a frozen layer, or the bottom for one lookup,
// and the path that level holds the entry at: the overlay path, translated
// through the redirects of the levels above (see [Entry.Redirect]).
type source struct {
	layer *Layer
	upper bool
	path  string
}

// up maps an overlay path into its writable upper.
func (f *FS) up(p string) string { return filepath.Join(f.upper, p) }

// lookup walks only indexed layers before consulting the bottom.
func (f *FS) lookup(p string) (source, os.FileInfo, error) {
	if reserved(p) {
		return source{}, nil, notExist("lstat", p)
	}
	if p == "" && f.index == nil && f.upper != "" && f.rootInfo != nil {
		return source{upper: true, path: p}, f.rootInfo, nil
	}
	q := p
	if f.index != nil {
		if e, ok := f.index.entries[p]; ok {
			if e.Kind == KindWhiteout {
				return source{}, nil, notExist("lstat", p)
			}
			if m := f.files[p]; m != nil {
				return source{layer: f.index, upper: true, path: p}, m.info(path.Base(p)), nil
			}
			info, err := f.upperInfo(p, e)
			return source{layer: f.index, upper: true, path: p}, info, err
		}
		if f.index.barrier(p) {
			return source{}, nil, notExist("lstat", p)
		}
		q = f.index.redirected(p)
	}
	return f.lookupBelow(p, q)
}

// lookupBelow looks up overlay path p, which the upper does not hold, in the
// frozen layers and the bottom, the topmost of which holds it at q.
func (f *FS) lookupBelow(p, q string) (source, os.FileInfo, error) {
	for _, l := range f.layers {
		v := l.view()
		if e, ok := v.entries[q]; ok {
			if e.Kind == KindWhiteout {
				return source{}, nil, notExist("lstat", p)
			}
			return source{layer: l, path: q}, e.Info, nil
		}
		if v.barrier(q) {
			return source{}, nil, notExist("lstat", p)
		}
		q = v.redirected(q)
	}
	if f.bottom == nil {
		return source{}, nil, notExist("lstat", p)
	}
	info, err := f.bottom.Lstat(q)
	if missing(err) {
		return source{}, nil, notExist("lstat", p)
	}
	return source{path: q}, info, err
}

// lowerPath is upper path p as the levels below the upper would hold it if
// the upper had no entry at p: translated through its ancestors' redirects.
func (f *FS) lowerPath(p string) string {
	if p == "" || f.index == nil {
		return p
	}
	return path.Join(f.index.redirected(parent(p)), path.Base(p))
}

// lowerHas detects a name in any immutable source, even if currently hidden.
func (f *FS) lowerHas(p string) (bool, error) {
	q := f.lowerPath(p)
	for _, l := range f.layers {
		v := l.view()
		if e, ok := v.entries[q]; ok && e.Kind != KindWhiteout {
			return true, nil
		}
		q = v.redirected(q)
	}
	if f.bottom == nil {
		return false, nil
	}
	_, err := f.bottom.Lstat(q)
	if missing(err) {
		return false, nil
	}
	return err == nil, err
}

// readlink reads the selected backing source without doing another lookup.
func (f *FS) readlink(s source, p string) (string, error) {
	if s.upper && s.layer == nil {
		target, err := os.Readlink(f.up(p))
		return filepath.ToSlash(target), err
	}
	if s.layer != nil {
		return s.layer.Readlink(s.path)
	}
	return f.bottom.Readlink(s.path)
}

// InBottom reports whether name, without following a final symlink, is
// served by the bottom, neither the upper nor a frozen layer providing or
// hiding it, and the path the bottom holds it at: name's, unless a renamed
// directory above it redirects there (see [Entry.Redirect]).
func (f *FS) InBottom(name string) (string, bool) {
	f.await(name)
	f.mu.RLock()
	defer f.mu.RUnlock()
	s, _, err := f.lookupRead(Clean(name))
	return s.path, err == nil && !s.upper && s.layer == nil
}

// Lstat describes name without following its final symlink.
func (f *FS) Lstat(name string) (os.FileInfo, error) {
	f.await(name)
	f.mu.RLock()
	defer f.mu.RUnlock()
	_, info, err := f.lookupRead(Clean(name))
	return info, err
}

// Stat describes name, resolving relative final symlinks within the overlay.
func (f *FS) Stat(name string) (os.FileInfo, error) {
	f.await(name)
	f.mu.RLock()
	defer f.mu.RUnlock()
	p := Clean(name)
	for range 40 {
		s, info, err := f.lookupRead(p)
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			return info, err
		}
		target, err := f.readlink(s, p)
		if err != nil {
			return nil, err
		}
		if filepath.IsAbs(target) {
			return os.Stat(target)
		}
		p = Clean(path.Join(path.Dir(p), target))
	}
	return nil, &fs.PathError{Op: "stat", Path: name, Err: syscall.ELOOP}
}

// Readlink returns a symlink's stored target.
func (f *FS) Readlink(name string) (string, error) {
	f.await(name)
	f.mu.RLock()
	defer f.mu.RUnlock()
	p := Clean(name)
	s, info, err := f.lookupRead(p)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		// As readlink(2) answers, also for a file held in memory, which no host path holds.
		return "", &fs.PathError{Op: "readlink", Path: name, Err: syscall.EINVAL}
	}
	return f.readlink(s, p)
}

// ReadDir merges direct entries from all visible levels in name order.
func (f *FS) ReadDir(name string) ([]os.FileInfo, error) {
	f.await(name)
	p := Clean(name)
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.index == nil && f.upper != "" {
		index, err := walkLayer(f.upper, true, false, p, f.ids)
		if err != nil {
			return nil, err
		}
		view := FS{upper: f.upper, index: index, layers: f.layers, bottom: f.bottom}
		return view.readDir(p)
	}
	return f.readDir(p)
}

// readDir applies ancestor barriers, whiteouts, opaque directories and shadows.
func (f *FS) readDir(p string) ([]os.FileInfo, error) {
	if q, ok := f.bottomOnly(p); ok && !reserved(p) {
		items, err := f.bottom.ReadDir(q)
		if err != nil {
			return nil, err
		}
		return visibleEntries(items), nil
	}
	_, info, err := f.lookup(p)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, &fs.PathError{Op: "readdir", Path: p, Err: syscall.ENOTDIR}
	}
	seen := make(map[string]bool)
	out := make([]os.FileInfo, 0)
	// q is p as the level merged next holds it (see [Entry.Redirect]).
	q := p
	merge := func(l *Layer, upper bool) (bool, error) {
		l = l.view()
		defer func() { q = l.redirected(q) }()
		e, present := l.entries[q]
		if present && e.Kind != KindDir {
			return true, nil
		}
		for n, c := range l.children[q] {
			if seen[n] {
				continue
			}
			seen[n] = true
			if c.Kind == KindWhiteout {
				continue
			}
			ci := c.Info
			if upper {
				child := path.Join(p, n)
				if m := f.files[child]; m != nil {
					ci = m.info(n)
				} else {
					var err error
					ci, err = f.upperInfo(child, c)
					if err != nil {
						return false, err
					}
				}
			}
			out = append(out, ci)
		}
		// Its own redirect has the levels below list q elsewhere, whatever is above.
		return (present && e.Opaque) || (e.Redirect == "" && l.barrier(q)), nil
	}
	stop := false
	if f.index != nil {
		stop, err = merge(f.index, true)
		if err != nil {
			return nil, err
		}
	}
	for _, l := range f.layers {
		if stop {
			break
		}
		stop, err = merge(l, false)
		if err != nil {
			return nil, err
		}
	}
	if !stop && f.bottom != nil {
		items, err := f.bottom.ReadDir(q)
		if err != nil && !missing(err) {
			return nil, err
		}
		for _, item := range items {
			name := item.Name()
			if !seen[name] && !strings.HasPrefix(name, whPrefix) {
				out = append(out, item)
			}
		}
	}
	slices.SortFunc(out, func(a, b os.FileInfo) int { return strings.Compare(a.Name(), b.Name()) })
	return out, nil
}

// bottomOnly reports whether no upper or layer level has an entry, child or
// barrier at directory p, so the bottom listing alone is the result, and the
// path the bottom holds p at (see [Entry.Redirect]).
func (f *FS) bottomOnly(p string) (string, bool) {
	if f.bottom == nil {
		return "", false
	}
	q := p
	if f.index != nil {
		if !f.index.untouched(q) {
			return "", false
		}
		q = f.index.redirected(q)
	}
	for _, l := range f.layers {
		v := l.view()
		if !v.untouched(q) {
			return "", false
		}
		q = v.redirected(q)
	}
	return q, true
}

// untouched reports whether view l has no entry, child or barrier for directory p.
func (l *Layer) untouched(p string) bool {
	_, present := l.entries[p]
	return !present && len(l.children[p]) == 0 && !l.barrier(p)
}

// visibleEntries filters reserved names in the bottom's caller-owned listing.
func visibleEntries(items []os.FileInfo) []os.FileInfo {
	if items == nil {
		return []os.FileInfo{}
	}
	// The bottom lists in name order, so all reserved names form one run.
	first := sort.Search(len(items), func(i int) bool { return items[i].Name() >= whPrefix })
	end := first
	for end < len(items) && strings.HasPrefix(items[end].Name(), whPrefix) {
		end++
	}
	return slices.Delete(items, first, end)
}

// Open opens name read-only from whichever level provides it.
func (f *FS) Open(name string) (billy.File, error) { return f.OpenFile(name, os.O_RDONLY, 0) }

// openLower opens a regular file served by a frozen layer or the bottom for
// reading, holding the shared lock only for the lookup: that content never
// changes under f, so readers of different files do not queue behind each
// other's host I/O. A write that copies the file up meanwhile orders after
// this open. ok is false for anything else (a symlink, a directory, an upper
// or missing name), which [FS.OpenFile] decides under the exclusive lock.
func (f *FS) openLower(name string, flag int) (h billy.File, ok bool, err error) {
	p := Clean(name)
	f.mu.RLock()
	layer, q, bottom, hit := f.resolveLower(p)
	f.mu.RUnlock()
	if !hit {
		return nil, false, nil
	}
	if layer != nil {
		h, err := layer.open(q, p, flag)
		return h, true, err
	}
	if opener, has := bottom.(InfoOpener); has {
		c, info, err := opener.OpenInfo(q)
		if err != nil {
			if missing(err) {
				return nil, false, nil
			}
			return nil, true, err
		}
		if c == nil {
			return nil, false, nil
		}
		return wrapContent(c, p, info), true, nil
	}
	info, err := bottom.Lstat(q)
	if err != nil || !info.Mode().IsRegular() {
		return nil, false, nil
	}
	c, err := bottom.Open(q)
	if err != nil {
		return nil, true, err
	}
	return wrapContent(c, p, info), true, nil
}

// Create creates or truncates a file in the writable upper.
func (f *FS) Create(name string) (billy.File, error) {
	return f.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o666)
}

// OpenFile copies immutable data up before opening with any write intent.
func (f *FS) OpenFile(name string, flag int, perm os.FileMode) (billy.File, error) {
	f.await(name)
	writing := flag&writeFlags != 0
	if !writing {
		if h, ok, err := f.openLower(name, flag); ok {
			return h, err
		}
	}
	f.staging.RLock()
	defer f.staging.RUnlock()
	// A memory file needs no host state; every other branch waits for the
	// queue on its name first ([FS.queuedOn]).
	f.lockSync(Clean(name))
	defer f.mu.Unlock()
retry:
	p := Clean(name)
	f.syncOps(p) // a copy-up released mu, and another call's host change may run on p
	if !writing && f.index == nil {
		if h, ok, err := f.openUpperCold(p, flag); ok {
			return h, err
		}
	}
	if err := f.loadIndex(); err != nil {
		return nil, err
	}
	if writing {
		if err := f.mutable("open", p); err != nil {
			return nil, err
		}
		if p == "" || reserved(p) {
			return nil, &fs.PathError{Op: "open", Path: name, Err: syscall.EPERM}
		}
	}
	original := p
	s, info, err := f.lookup(p)
	if err == nil && writing && flag&os.O_CREATE != 0 && flag&os.O_EXCL != 0 {
		return nil, &fs.PathError{Op: "open", Path: name, Err: syscall.EEXIST}
	}
	for links := 0; err == nil && info.Mode()&os.ModeSymlink != 0; links++ {
		if links == 40 {
			return nil, &fs.PathError{Op: "open", Path: name, Err: syscall.ELOOP}
		}
		var target string
		var linkErr error
		if s.layer != nil {
			target, linkErr = f.readlink(s, p)
		} else {
			bottom, epoch := f.bottom, f.bottomEpoch
			f.mu.Unlock()
			target, linkErr = bottom.Readlink(s.path)
			f.mu.Lock()
			current, currentInfo, currentErr := f.lookup(p)
			if f.bottomEpoch != epoch || currentErr != nil || current != s || currentInfo.Mode()&os.ModeSymlink == 0 {
				goto retry
			}
		}
		if linkErr != nil {
			return nil, linkErr
		}
		if filepath.IsAbs(target) {
			return open(target, original, flag, perm, nil)
		}
		p = Clean(path.Join(parent(p), target))
		if op := f.busySync(p); op != nil {
			if err := f.waitOp(op); err != nil {
				return nil, err
			}
			goto retry
		}
		s, info, err = f.lookup(p)
	}
	if err == nil && s.upper && f.files[p] == nil && info.Mode().IsRegular() && flag&os.O_TRUNC == 0 && f.lazyDirs() {
		if op := f.changing(p); op != nil && op.queued {
			if err := f.waitOp(op); err != nil {
				return nil, err
			}
			goto retry
		}
		return f.openOutside(p, original, flag)
	}
	if !writing {
		if err != nil {
			return nil, err
		}
		if m := f.files[p]; m != nil {
			return f.openMem(m, original, flag)
		}
		if op := f.changing(p); op != nil && op.queued && s.upper {
			if err := f.waitOp(op); err != nil {
				return nil, err
			}
			goto retry
		}
		if s.upper {
			h, err := open(f.up(p), original, flag, 0, nil)
			if err != nil {
				return nil, err
			}
			f.adopt(h)
			f.bind(h, p)
			return h, nil
		}
		if s.layer != nil {
			return s.layer.open(s.path, original, flag)
		}
		if !info.Mode().IsRegular() {
			return nil, &fs.PathError{Op: "open", Path: name, Err: syscall.EISDIR}
		}
		bottom := f.bottom
		f.mu.Unlock()
		fh, err := bottom.Open(s.path)
		f.mu.Lock()
		if err != nil {
			return nil, err
		}
		return wrapContent(fh, original, info), nil
	}
	if p == "" || reserved(p) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: syscall.EPERM}
	}
	// startMem makes p an empty memory file with identity id (0: a new
	// one); nil means disk.
	startMem := func(mode os.FileMode, id ident.ID) *memFile {
		if f.mem == nil {
			return nil
		}
		return f.addMem(p, mode, time.Now(), nil, id)
	}
	creating := err != nil
	if err == nil {
		if m := f.files[p]; m != nil {
			return f.openMem(m, original, flag)
		}
		if op := f.queuedOn(p); op != nil {
			if err := f.waitOp(op); err != nil {
				return nil, err
			}
			goto retry
		}
		if !s.upper {
			// A refused open must leave the file as it was, so a copy, and a
			// truncated one, is published only for an open that succeeds.
			if info.Mode().IsRegular() {
				if err := ownerAccess(info.Mode(), original, flag); err != nil {
					return nil, err
				}
			}
			if info.Mode().IsRegular() && flag&os.O_TRUNC != 0 {
				if err := f.ensureDir(parent(p)); err != nil {
					return nil, err
				}
				// The replacement keeps the identity of the file it truncates.
				id := f.inherit(s, p, info)
				if m := startMem(info.Mode().Perm(), id); m != nil {
					return f.openMem(m, original, flag)
				}
				err := f.preserveDirTime(parent(p), func() error {
					fh, err := os.OpenFile(f.up(p), os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
					if err != nil {
						return err
					}
					if err := fh.Chmod(info.Mode().Perm()); err != nil {
						fh.Close()
						return err
					}
					if err := fh.Close(); err != nil {
						return err
					}
					if err := f.ids.persist(f.up(p), id); err != nil {
						return err
					}
					return keepMode(f.up(p), info.Mode())
				})
				if err != nil {
					return nil, err
				}
				if err := f.refresh(p); err != nil {
					return nil, err
				}
			} else if s.layer == nil && info.Mode().IsRegular() {
				changed, err := f.copyUpBottomForOpen(p, s.path, info)
				if err != nil {
					return nil, err
				}
				if changed {
					goto retry
				}
				if m := f.files[p]; m != nil {
					return f.openMem(m, original, flag)
				}
			} else if err := f.copyUp(p); err != nil {
				return nil, err
			} else if m := f.files[p]; m != nil {
				return f.openMem(m, original, flag)
			}
		}
	} else {
		if !errors.Is(err, fs.ErrNotExist) || flag&os.O_CREATE == 0 {
			return nil, err
		}
		if err := f.claimName("open", p, ""); err != nil {
			return nil, err
		}
		if op := f.queuedOn(parent(p)); op != nil && f.index.entries[parent(p)].Kind != KindDir {
			if err := f.waitOp(op); err != nil {
				return nil, err
			}
			goto retry
		}
		if err := f.ensureDir(parent(p)); err != nil {
			return nil, err
		}
		if m := startMem(perm&memModeBits&^umask, 0); m != nil {
			// A new name changes the parent like a host create; the creating open skips the permission check.
			if err := f.touchDir(parent(p), false); err != nil {
				return nil, err
			}
			return f.newHandle(m, original, flag), nil
		}
	}
	if op := f.queuedOn(p); op != nil {
		if err := f.waitOp(op); err != nil {
			return nil, err
		}
		goto retry
	}
	marked := f.mayHoldMarker(p)
	fh, err := open(f.up(p), p, flag, perm, f)
	if err != nil {
		return nil, err
	}
	if creating {
		err = keepMode(f.up(p), perm&^umask)
		if err == nil {
			err = f.ids.created(f.up(p))
		}
	}
	if err == nil {
		err = f.clearWhiteout(p, marked)
	}
	if err == nil {
		err = f.refresh(p)
	}
	if err == nil && creating {
		err = f.touchDir(parent(p), true)
	}
	if err != nil {
		delete(f.hosts, fh)
		fh.File.Close()
		return nil, err
	}
	fh.name = original
	f.bind(fh, p)
	return fh, nil
}

// TempFile creates a uniquely named upper file in an existing merged directory.
func (f *FS) TempFile(dir, prefix string) (billy.File, error) {
	f.await(dir)
	d := Clean(dir)
	err := f.lockPaths(d)
	defer f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if err := f.mutable("createtemp", d); err != nil {
		return nil, err
	}
	if reserved(d) || strings.HasPrefix(prefix, whPrefix) {
		return nil, &fs.PathError{Op: "createtemp", Path: dir, Err: syscall.EPERM}
	}
	if err := f.ensureDir(d); err != nil {
		return nil, err
	}
	fh, err := hostfile.CreateTemp(f.up(d), prefix)
	if err != nil {
		return nil, err
	}
	p := path.Join(d, filepath.Base(fh.Name()))
	if err := keepMode(fh.Name(), 0o600); err != nil {
		fh.Close()
		return nil, err
	}
	if err := f.ids.created(fh.Name()); err != nil {
		fh.Close()
		return nil, err
	}
	if err := f.clearWhiteout(p, f.mayHoldMarker(p)); err != nil {
		fh.Close()
		return nil, err
	}
	if err := f.refresh(p); err != nil {
		fh.Close()
		return nil, err
	}
	if err := f.touchDir(d, true); err != nil {
		fh.Close()
		return nil, err
	}
	h := f.adopt(&file{File: fh, name: p, writable: true})
	f.bind(h, p)
	return h, nil
}

// MkdirAll creates missing directories, making recreated lower names opaque.
func (f *FS) MkdirAll(name string, perm os.FileMode) error {
	f.await(name)
	p := Clean(name)
	f.lockSync(p)
	defer f.mu.Unlock()
retry:
	f.syncOps(p)
	if err := f.mutable("mkdir", p); err != nil {
		return err
	}
	if reserved(p) {
		return &fs.PathError{Op: "mkdir", Path: p, Err: syscall.EPERM}
	}
	for i := range len(p) + 1 {
		if i < len(p) && p[i] != '/' {
			continue
		}
		q := p[:i]
		_, info, err := f.lookup(q)
		if err == nil {
			if !info.IsDir() {
				return &fs.PathError{Op: "mkdir", Path: q, Err: syscall.ENOTDIR}
			}
			continue
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := f.claimName("mkdir", q, ""); err != nil {
			return err
		}
		if f.queueable() && f.index.entries[parent(q)].Kind == KindDir && !f.mayHoldMarker(q) {
			lower, err := f.lowerHas(q)
			if err != nil {
				return err
			}
			if !lower {
				if err := f.queueMkdir(q, perm); err == nil {
					continue
				} else if !errors.Is(err, errNotQueued) {
					return err
				}
			}
		}
		if op := f.queuedOn(q); op != nil {
			if err := f.waitOp(op); err != nil {
				return err
			}
			goto retry
		}
		if err := f.ensureDir(parent(q)); err != nil {
			return err
		}
		marked := f.mayHoldMarker(q)
		if err := os.Mkdir(f.up(q), perm); err != nil {
			return err
		}
		if err := keepMode(f.up(q), perm&^umask); err != nil {
			return err
		}
		if err := f.ids.created(f.up(q)); err != nil {
			return err
		}
		if err := f.refresh(q); err != nil {
			return err
		}
		if err := f.placeDir(q, false, marked); err != nil {
			return err
		}
		if err := f.refreshDir(parent(q)); err != nil {
			return err
		}
	}
	return nil
}

// Mknod creates a socket or FIFO in the writable upper.
func (f *FS) Mknod(name string, mode os.FileMode) error {
	f.await(name)
	p := Clean(name)
	err := f.lockPaths(p)
	defer f.mu.Unlock()
	if err != nil {
		return err
	}
	if err := f.mutable("mknod", p); err != nil {
		return err
	}
	if p == "" || reserved(p) {
		return &fs.PathError{Op: "mknod", Path: name, Err: syscall.EPERM}
	}
	if _, _, err := f.lookup(p); err == nil {
		return &fs.PathError{Op: "mknod", Path: name, Err: syscall.EEXIST}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := f.claimName("mknod", p, ""); err != nil {
		return err
	}
	if err := f.ensureDir(parent(p)); err != nil {
		return err
	}
	marked := f.mayHoldMarker(p)
	if err := makeNode(f.up(p), f.stagingName(), mode); err != nil {
		return err
	}
	if err := f.ids.created(f.up(p)); err != nil {
		f.ids.discard(f.up(p))
		return err
	}
	if err := f.clearWhiteout(p, marked); err != nil {
		return err
	}
	if err := f.refresh(p); err != nil {
		return err
	}
	return f.touchDir(parent(p), true)
}

// Symlink creates a symlink without modifying any immutable source.
func (f *FS) Symlink(target, link string) error {
	f.await(link)
	p := Clean(link)
	err := f.lockPaths(p)
	defer f.mu.Unlock()
	if err != nil {
		return err
	}
	if err := f.mutable("symlink", p); err != nil {
		return err
	}
	if p == "" || reserved(p) {
		return &fs.PathError{Op: "symlink", Path: link, Err: syscall.EPERM}
	}
	if _, _, err := f.lookup(p); err == nil {
		return &fs.PathError{Op: "symlink", Path: link, Err: syscall.EEXIST}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := f.claimName("symlink", p, ""); err != nil {
		return err
	}
	if err := f.ensureDir(parent(p)); err != nil {
		return err
	}
	marked := f.mayHoldMarker(p)
	if err := os.Symlink(target, f.up(p)); err != nil {
		return err
	}
	if err := f.ids.created(f.up(p)); err != nil {
		f.ids.discard(f.up(p))
		return err
	}
	if err := f.clearWhiteout(p, marked); err != nil {
		return err
	}
	if err := f.refresh(p); err != nil {
		return err
	}
	return f.touchDir(parent(p), true)
}

// Remove removes a file or merged-empty directory and masks all lower copies.
// It fails with EIO, never ENOENT, when the upper on disk lacks what the
// index shows of name.
func (f *FS) Remove(name string) (err error) {
	defer f.names.Add(1) // after the removal, see [FS.Names]
	f.await(name)
	p := Clean(name)
	f.lockSync(p)
	defer f.mu.Unlock()
	// Once the lookup below finds p under f.mu, a host call that reports p
	// or a directory above it missing means the upper lost what the index
	// shows. The macOS NFS client takes ENOENT from REMOVE or RMDIR for the
	// reply to a retransmission and reports success, so rm would go on as if
	// the name were gone.
	found := false
	defer func() {
		if found && errors.Is(err, fs.ErrNotExist) {
			err = &fs.PathError{Op: "remove", Path: name, Err: syscall.EIO}
		}
	}()
retry:
	found = false
	f.syncOps(p)
	if err := f.mutable("remove", p); err != nil {
		return err
	}
	if p == "" {
		return &fs.PathError{Op: "remove", Path: name, Err: syscall.EBUSY}
	}
	s, info, err := f.lookup(p)
	if err != nil {
		return err
	}
	found = true
	if info.IsDir() {
		items, err := f.readDir(p)
		if err != nil {
			return err
		}
		if len(items) != 0 {
			return &fs.PathError{Op: "remove", Path: name, Err: syscall.ENOTEMPTY}
		}
	}
	lower, err := f.lowerHas(p)
	if err != nil {
		return err
	}
	// Another name of the file shows its new link count afterwards.
	alias := f.otherAlias(p)
	if op := f.busySync(alias); alias != "" && op != nil {
		if err := f.waitOp(op); err != nil {
			return err
		}
		goto retry
	}
	markDead := func() {
		if id, ok := ident.Of(info); ok && id.Hosted() && !info.IsDir() && alias == "" {
			f.markRemoved(id)
		}
	}
	if !lower && f.queueable() {
		switch {
		case f.hostFile(p):
			err := f.queueRemove(p, info, alias)
			if err == nil {
				markDead()
			}
			return err
		case s.upper && info.IsDir() && f.index.entries[p].Kind == KindDir:
			if err := f.queueRmdir(p, info); !errors.Is(err, errNotQueued) {
				return err
			}
		}
	}
	if op := f.queuedOn(nonEmpty(p, alias)...); op != nil && !(f.files[p] != nil && !lower) {
		// The host change below follows the queued ones on these names.
		if err := f.waitOp(op); err != nil {
			return err
		}
		goto retry
	}
	if !lower && f.hostFile(p) && f.lazyDirs() {
		err := f.removeOutside(p, info, alias)
		if err == nil {
			markDead()
		}
		return err
	}
	markDead()
	// A memory file's name may keep a marker on disk, which then stays.
	marked := f.files[p] != nil && f.files[p].markedAt(p)
	if s.upper {
		// Before the marker replaces the entry: another name takes the identity.
		f.unregister(p)
	}
	if lower {
		if err := f.whiteout(p); err != nil {
			return err
		}
	}
	inMem := f.files[p] != nil
	if s.upper {
		switch {
		case inMem:
		case info.IsDir():
			// Merged-empty: only markers can remain beneath it.
			if err := f.ids.release(info); err != nil {
				return err
			}
			if err := os.RemoveAll(f.up(p)); err != nil {
				return err
			}
		default:
			if err := f.ids.release(info); err != nil {
				return err
			}
			// Unlike os.Remove on Windows, keeps a read-only hard link's other names read-only.
			if err := hostfile.Remove(f.up(p)); err != nil {
				return err
			}
		}
		f.dropIndex(p)
	}
	if alias != "" {
		if err := f.refresh(alias); err != nil {
			return err
		}
	}
	if lower || marked {
		f.index.set(p, Entry{Kind: KindWhiteout})
	}
	if info.IsDir() {
		return f.refreshDir(parent(p))
	}
	return f.touchDir(parent(p), !inMem)
}

// Link creates a hard link after copying an immutable source into upper.
func (f *FS) Link(oldname, newname string) error {
	f.await(oldname)
	f.await(newname)
	src, dst := Clean(oldname), Clean(newname)
	f.lockSync(src, dst)
	defer f.mu.Unlock()
retry:
	f.syncOps(src, dst)
	if err := f.mutable("link", dst); err != nil {
		return err
	}
	if src == "" || dst == "" || reserved(src) || reserved(dst) {
		return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: syscall.EPERM}
	}
	_, info, err := f.lookup(src)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: syscall.EPERM}
	}
	if _, _, err := f.lookup(dst); err == nil {
		return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: syscall.EEXIST}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := f.claimName("link", dst, ""); err != nil {
		return err
	}
	inPlace := f.index.entries[parent(dst)].Kind == KindDir
	if inPlace && f.hostFile(src) && f.queueable() {
		if err := f.queueLink(src, dst); !errors.Is(err, errNotQueued) {
			return err
		}
	}
	if m := f.files[src]; m != nil && inPlace && f.queueable() {
		// Both names reach disk when the file spills.
		return f.linkMem(m, dst)
	}
	if op := f.queuedOn(src, dst); op != nil {
		if err := f.waitOp(op); err != nil {
			return err
		}
		goto retry
	}
	if m := f.files[src]; (f.hostFile(src) || m != nil && len(m.handles) == 0 && len(m.others) == 0) && inPlace && f.lazyDirs() {
		return f.linkOutside(src, dst)
	}
	if err := f.copyUp(src); err != nil {
		return err
	}
	if m := f.files[src]; m != nil {
		linked := len(m.others) != 0
		if err := f.spillAs(m, src); err != nil {
			return err
		}
		if linked {
			goto retry // after the queued links of its other names
		}
	}
	if err := f.ensureDir(parent(dst)); err != nil {
		return err
	}
	marked := f.mayHoldMarker(dst)
	if err := os.Link(f.up(src), f.up(dst)); err != nil {
		return err
	}
	if err := f.clearWhiteout(dst, marked); err != nil {
		return err
	}
	if err := f.refresh(dst); err != nil {
		return err
	}
	return f.touchDir(parent(dst), true)
}

// Rename moves a name in the upper. A file is copied up first; a directory
// moves without its tree, which the levels below keep: one showing part of
// theirs takes a redirect to where they hold it (see [Entry.Redirect]), and
// redirects below it move along. Either way a name the levels below hold is
// whited out, and the cost is the upper's part of the tree. It fails with
// EIO, never ENOENT, when the upper on disk lacks what the index shows of
// from or of to's directory.
func (f *FS) Rename(from, to string) (err error) {
	defer f.names.Add(1) // after the move, see [FS.Names]
	f.await(from)
	f.await(to)
	src, dst := Clean(from), Clean(to)
	f.lockSync(src, dst)
	defer f.mu.Unlock()
	// As in [FS.Remove]: once the lookups below find src under f.mu, a host
	// call that reports src, dst's directory or one above them missing means
	// the upper lost what the index shows. The macOS NFS client takes ENOENT
	// from RENAME for the reply to a retransmission and reports success.
	// Windows numbers ENOTDIR as ERROR_PATH_NOT_FOUND, which reads as
	// not-exist, so each ENOTDIR that is the caller's clears found.
	found := false
	defer func() {
		if found && errors.Is(err, fs.ErrNotExist) {
			err = &os.LinkError{Op: "rename", Old: from, New: to, Err: syscall.EIO}
		}
	}()
retry:
	found = false
	f.syncOps(src, dst)
	if err := f.mutable("rename", src); err != nil {
		return err
	}
	if src == "" || dst == "" || reserved(src) || reserved(dst) {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: syscall.EPERM}
	}
	ss, sinfo, err := f.lookup(src)
	if err != nil {
		return err
	}
	if src == dst {
		return nil
	}
	if strings.HasPrefix(dst, src+"/") {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: syscall.EINVAL}
	}
	ds, dinfo, derr := f.lookup(dst)
	if derr != nil && !errors.Is(derr, fs.ErrNotExist) {
		return derr
	}
	found = true
	if derr != nil {
		if err := f.claimName("rename", dst, src); err != nil {
			return err
		}
	}
	// A copied-up bottom hard link keeps its exported ID but is no longer
	// the same file as an untouched alias on a lower level.
	if derr == nil && ss.layer == ds.layer && sameEntry(sinfo, dinfo) {
		return nil
	}
	if derr == nil {
		if sinfo.IsDir() && !dinfo.IsDir() {
			found = false
			return &os.LinkError{Op: "rename", Old: from, New: to, Err: syscall.ENOTDIR}
		}
		if !sinfo.IsDir() && dinfo.IsDir() {
			return &os.LinkError{Op: "rename", Old: from, New: to, Err: syscall.EISDIR}
		}
		if dinfo.IsDir() {
			items, err := f.readDir(dst)
			if err != nil {
				return err
			}
			if len(items) != 0 {
				return &os.LinkError{Op: "rename", Old: from, New: to, Err: syscall.ENOTEMPTY}
			}
		}
	}
	lower, err := f.lowerHas(src)
	if err != nil {
		return err
	}
	// Another name of a file it replaces shows the new link count afterwards.
	dstAlias := f.otherAlias(dst)
	if op := f.busySync(dstAlias); dstAlias != "" && op != nil {
		if err := f.waitOp(op); err != nil {
			return err
		}
		goto retry
	}
	origin := ""
	if sinfo.IsDir() {
		origin = f.origin(src)
	}
	inPlace := !lower && f.index.entries[parent(dst)].Kind == KindDir
	if inPlace && f.hostFile(src) && (derr != nil || f.hostFile(dst) && dstAlias == "") && f.queueable() {
		err := f.queueRename(src, dst, dinfo)
		if err == nil && derr == nil && !dinfo.IsDir() {
			if id, ok := ident.Of(dinfo); ok && id.Hosted() {
				f.markRemoved(id)
			}
		}
		return err
	}
	// Moving a memory file to a free name or over another changes the index alone.
	indexOnly := inPlace && f.files[src] != nil && (derr != nil || f.files[dst] != nil)
	if op := f.queuedOn(nonEmpty(src, dst, dstAlias)...); op != nil && !indexOnly {
		if err := f.waitOp(op); err != nil {
			return err
		}
		goto retry
	}
	if inPlace && f.hostFile(src) && f.lazyDirs() && (derr != nil || f.hostFile(dst) && dstAlias == "") {
		err := f.renameOutside(src, dst, dinfo)
		if err == nil && derr == nil && !dinfo.IsDir() {
			if id, ok := ident.Of(dinfo); ok && id.Hosted() {
				f.markRemoved(id)
			}
		}
		return err
	}
	if !sinfo.IsDir() {
		err = f.copyUp(src)
	} else {
		err = f.ensureDir(src)
	}
	if err != nil {
		return err
	}
	if err := f.ensureDir(parent(dst)); err != nil {
		if _, info, lerr := f.lookup(parent(dst)); lerr != nil || !info.IsDir() {
			found = false // dst's directory is not there, or no directory: the caller's error
		}
		return err
	}
	if origin != "" {
		// The markers move with the directory: a crash leaves them at either name.
		if err := f.markBelow(src); err != nil {
			return err
		}
		if err := f.setRedirect(src, origin); err != nil {
			return err
		}
	}
	if derr == nil && dinfo.IsDir() && ds.upper {
		if err := f.ids.release(dinfo); err != nil {
			return err
		}
		if err := os.RemoveAll(f.up(dst)); err != nil {
			return err
		}
		f.dropIndex(dst)
	}
	// A memory file over an upper file on disk spills first, so the host
	// rename replaces the old copy atomically and a crash finds one of the two.
	if m := f.files[src]; m != nil && derr == nil && ds.upper && !dinfo.IsDir() && f.files[dst] == nil {
		linked := len(m.others) != 0
		if err := f.spillAs(m, src); err != nil {
			return err
		}
		if linked {
			goto retry // after the queued links of its other names
		}
	}
	if derr == nil && !dinfo.IsDir() && dstAlias == "" {
		if id, ok := ident.Of(dinfo); ok && id.Hosted() {
			f.markRemoved(id)
		}
	}
	// Any other memory file moves only in the index.
	srcMem := f.files[src] != nil
	if !srcMem {
		// The rename removes the name of a file it replaces.
		if derr == nil && ds.upper && !dinfo.IsDir() && f.files[dst] == nil {
			if err := f.ids.release(dinfo); err != nil {
				return err
			}
		}
		if err := hostfile.Rename(f.up(src), f.up(dst)); err != nil {
			return err
		}
	}
	// A marker of either name stays on disk unless replaced below; a memory
	// file keeps the one at its new name until it spills ([FS.installed]).
	dstMarked := f.mayHoldMarker(dst)
	srcMarked := srcMem && f.files[src].markedAt(src)
	f.dropIndex(dst)
	f.moveEntry(src, dst)
	f.dropIndex(src)
	if m := f.files[dst]; m != nil && !sinfo.IsDir() {
		m.setMarked(dst, dstMarked)
	}
	if dstAlias != "" {
		if err := f.refresh(dstAlias); err != nil {
			return err
		}
	}
	switch {
	case origin != "":
		err = f.placeRedirect(dst, dstMarked)
	case sinfo.IsDir():
		err = f.placeDir(dst, true, dstMarked)
	default:
		if f.files[dst] == nil {
			err = f.clearWhiteout(dst, dstMarked)
		}
		if err == nil {
			err = f.refresh(dst)
		}
	}
	if err != nil {
		return err
	}
	if lower {
		if err := f.whiteout(src); err != nil {
			return err
		}
	} else if srcMarked {
		f.index.set(src, Entry{Kind: KindWhiteout})
	}
	// Moving a directory changes its parents' link counts, which the host keeps.
	update := func(d string) error { return f.touchDir(d, !srcMem) }
	if sinfo.IsDir() {
		update = f.refreshDir
	}
	if err := update(parent(src)); err != nil {
		return err
	}
	if parent(dst) == parent(src) {
		return nil // a move within one directory updated it already
	}
	return update(parent(dst))
}

// Join joins overlay path elements with slashes.
func (f *FS) Join(elem ...string) string { return path.Join(elem...) }

// Chroot exposes a merged subtree as a billy filesystem.
func (f *FS) Chroot(p string) (billy.Filesystem, error) { return chroot.New(f, Clean(p)), nil }

// Root returns the virtual root of the merged filesystem.
func (f *FS) Root() string { return "/" }

// Capabilities reports the operations supported by this filesystem.
func (f *FS) Capabilities() billy.Capability { return billy.AllCapabilities }

// Chmod changes mode after copying the entry up. A symlink's own mode changes,
// never its target's; hosts without symlink modes refuse it as unsupported.
func (f *FS) Chmod(name string, mode os.FileMode) error {
	entry := Clean(name)
	return f.change(name, "chmod", func(p string) error {
		// change holds f.mu, so the index has the copied-up entry's kind.
		if f.index.entries[entry].Kind == KindSymlink {
			return lchmod(p, mode)
		}
		if err := os.Chmod(p, mode); err != nil {
			return err
		}
		return keepMode(p, mode)
	}, func(m *memFile) { m.mode = mode & memModeBits })
}

// Lchown changes ownership without following the final symlink.
func (f *FS) Lchown(name string, uid, gid int) error {
	return f.change(name, "lchown", func(p string) error { return os.Lchown(p, uid, gid) }, nil)
}

// Chown changes ownership after copying the entry up. Like Lchown, it never
// follows a symlink: its target may lie outside the checkout.
func (f *FS) Chown(name string, uid, gid int) error {
	return f.change(name, "chown", func(p string) error { return os.Lchown(p, uid, gid) }, nil)
}

// Chtimes changes access and modification times in upper only, of a symlink
// itself rather than its target; a zero time leaves that time unchanged, as
// for os.Chtimes.
func (f *FS) Chtimes(name string, atime, mtime time.Time) error {
	return f.change(name, "chtimes", func(p string) error { return hostfile.Lchtimes(p, atime, mtime) },
		func(m *memFile) {
			if !atime.IsZero() {
				m.atime = atime
			}
			if !mtime.IsZero() {
				m.mtime = mtime
			}
		})
}

// change applies one metadata mutation under the structural lock: host to an
// upper path, or mem to a memory file. Without mem, a memory file spills first
// (ownership needs the kernel's permission checks). host must not follow a
// symlink: a target inside the mount would re-enter the daemon, which waits
// for f.mu.
func (f *FS) change(name, op string, host func(string) error, mem func(*memFile)) error {
	f.await(name)
	p := Clean(name)
	err := f.lockPaths(p)
	defer f.mu.Unlock()
	if err != nil {
		return err
	}
retry:
	if err := f.idleOps(p); err != nil {
		return err
	}
	if err := f.mutable(op, p); err != nil {
		return err
	}
	if err := f.copyUp(p); err != nil {
		return err
	}
	if m := f.files[p]; m != nil {
		if mem != nil {
			mem(m)
			m.ctime = time.Now()
			m.version++
			return f.refresh(p)
		}
		if err := f.spillAs(m, p); err != nil {
			return err
		}
		if op := f.queuedOn(p); op != nil {
			// The links of its other names come first.
			if err := f.waitOp(op); err != nil {
				return err
			}
			goto retry
		}
	}
	if _, ok := f.unsettled[p]; ok {
		// The host change re-reads the directory, whose time must be the index's.
		if err := f.settleDir(p, f.up(p)); err != nil {
			return upperError(err)
		}
	}
	if err := host(f.up(p)); err != nil {
		return err
	}
	return f.refresh(p)
}

// mutable rejects all mutations of a read-only frozen stack.
func (f *FS) mutable(op, p string) error {
	if f.upper == "" {
		return &fs.PathError{Op: op, Path: p, Err: syscall.EROFS}
	}
	return f.loadIndex()
}

// refresh synchronizes one upper entry's metadata without forgetting
// opacity. An unsettled directory keeps its indexed time (see [FS.lazyDirs]).
func (f *FS) refresh(p string) error {
	if _, ok := f.unsettled[p]; ok {
		return f.refreshUnsettled(p)
	}
	if m := f.files[p]; m != nil {
		info := m.info(path.Base(p))
		f.index.set(p, Entry{Kind: KindFile, Info: info})
		f.register(p, info)
		return nil
	}
	info, err := f.stat(p)
	if err != nil {
		return err
	}
	f.cacheInfo(p, info)
	return nil
}

// stat reads upper entry p's host metadata with its identity.
func (f *FS) stat(p string) (os.FileInfo, error) { return statHost(f.up(p), f.ids) }

// placeDir isolates a new or renamed directory from old content beneath it;
// marked is [FS.mayHoldMarker] of p before p took its new entry.
func (f *FS) placeDir(p string, complete, marked bool) error {
	lower, err := f.lowerHas(p)
	if err != nil {
		return err
	}
	if complete || lower {
		err := f.preserveDirTime(p, func() error {
			fh, err := os.OpenFile(filepath.Join(f.up(p), opaqueName), os.O_CREATE|os.O_WRONLY, 0o644)
			if err != nil {
				return err
			}
			if err := fh.Close(); err != nil {
				return err
			}
			e := f.index.entries[p]
			e.Opaque = true
			f.index.set(p, e)
			return nil
		})
		if err != nil {
			return err
		}
	}
	if err := f.clearWhiteout(p, marked); err != nil {
		return err
	}
	return f.refresh(p)
}

// origin returns the redirect a directory renamed from src moves with: where
// the levels below hold the tree it shows of theirs, or "" when it shows
// none (an opaque directory or one below it, or one only the upper holds,
// whose redirected subdirectories show theirs wherever they move).
func (f *FS) origin(src string) string {
	e := f.index.entries[src]
	if e.Redirect != "" {
		return e.Redirect // whatever is above, see [Layer.barrier]
	}
	if e.Opaque || f.index.barrier(src) {
		return ""
	}
	q := f.lowerPath(src)
	if _, info, err := f.lookupBelow(src, q); err == nil && info.IsDir() {
		return q
	}
	return ""
}

// setRedirect points upper directory p at origin (see [Entry.Redirect]), or
// drops its redirect for "". The marker is staged and renamed into place, so
// a crash leaves the old or the new one whole.
func (f *FS) setRedirect(p, origin string) error {
	e := f.index.entries[p]
	if e.Redirect == origin {
		return nil
	}
	return f.preserveDirTime(p, func() error {
		marker := filepath.Join(f.up(p), redirectName)
		if origin == "" {
			if err := removeMarker(marker); err != nil {
				return err
			}
		} else {
			tmp := f.stagingName()
			err := os.WriteFile(tmp, []byte(origin), 0o644)
			if err == nil {
				err = hostfile.Rename(tmp, marker)
			}
			if err != nil {
				return errors.Join(err, removeMarker(tmp))
			}
		}
		e.Redirect = origin
		f.index.set(p, e)
		return nil
	})
}

// markBelow gives each name at or below directory p of a memory file that
// a lower level also holds a deletion marker on disk, kept until the file
// spills ([memFile.marked]): a crash would otherwise show what the file
// shadows, and under a directory renamed with a redirect at a name it never
// had. Unlike a spill it opens no host file below p, which Windows would
// refuse to rename then. The caller waited for the host changes on p.
func (f *FS) markBelow(p string) error {
	for q, m := range f.files {
		if q != p && !strings.HasPrefix(q, p+"/") || m.markedAt(q) {
			continue
		}
		lower, err := f.lowerHas(q)
		if err != nil {
			return err
		}
		if !lower {
			continue
		}
		if err := f.ensureDir(parent(q)); err != nil {
			return err
		}
		err = f.preserveDirTime(parent(q), func() error {
			fh, err := os.OpenFile(f.whiteoutPath(q), os.O_CREATE|os.O_WRONLY, 0o644)
			if err != nil {
				return err
			}
			return fh.Close()
		})
		if err != nil {
			return err
		}
		m.setMarked(q, true)
	}
	return nil
}

// placeRedirect settles directory p, renamed with its redirect, at its new
// name; marked is [FS.mayHoldMarker] of p before p took its new entry. One
// renamed back to where the levels below hold its tree needs no redirect,
// unless an opaque directory above would hide that tree then.
func (f *FS) placeRedirect(p string, marked bool) error {
	if f.index.entries[p].Redirect == f.lowerPath(p) && !f.index.barrier(p) {
		if err := f.setRedirect(p, ""); err != nil {
			return err
		}
	}
	if err := f.clearWhiteout(p, marked); err != nil {
		return err
	}
	return f.refresh(p)
}

// whiteout places a marker before removing an upper entry.
func (f *FS) whiteout(p string) error {
	if err := f.ensureDir(parent(p)); err != nil {
		return err
	}
	fh, err := os.OpenFile(f.whiteoutPath(p), os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if err := fh.Close(); err != nil {
		return err
	}
	if _, err := os.Lstat(f.up(p)); missing(err) {
		f.index.set(p, Entry{Kind: KindWhiteout})
	}
	return nil
}

// clearWhiteout drops the deletion marker after a data entry has replaced
// it; marked is [FS.mayHoldMarker] of p before p took the new entry, and
// without it there is no marker to remove.
func (f *FS) clearWhiteout(p string, marked bool) error {
	if p == "" || !marked {
		return nil
	}
	if err := removeMarker(f.whiteoutPath(p)); err != nil {
		return err
	}
	if e, ok := f.index.entries[p]; ok && e.Kind == KindWhiteout {
		f.dropIndex(p)
	}
	return nil
}

// removeMarker removes the deletion marker at host path real, if any.
func removeMarker(real string) error {
	if err := os.Remove(real); err != nil && !missing(err) {
		return err
	}
	return nil
}

// mayHoldMarker reports whether a deletion marker for p may be on disk, for
// a caller about to give p a new entry. The index records every marker as a
// whiteout entry, except one a memory file keeps until it spills
// ([memFile.marked]); loading the index removes markers a crash left beside
// a data entry.
func (f *FS) mayHoldMarker(p string) bool {
	e, ok := f.index.entries[p]
	switch {
	case !ok:
		return false
	case e.Kind == KindWhiteout:
		return true
	}
	if m := f.files[p]; m != nil {
		return m.markedAt(p)
	}
	return false
}

// whiteoutPath gives the host path of a name's AUFS marker.
func (f *FS) whiteoutPath(p string) string {
	return filepath.Join(f.up(parent(p)), whPrefix+path.Base(p))
}

// ensureDir copies existing merged parents up with their modes and mtimes.
func (f *FS) ensureDir(p string) error {
	if e, ok := f.index.entries[p]; ok && e.Kind != KindWhiteout {
		if e.Kind != KindDir {
			return &fs.PathError{Op: "mkdir", Path: p, Err: syscall.ENOTDIR}
		}
		return nil
	}
	_, info, err := f.lookup(p)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return &fs.PathError{Op: "mkdir", Path: p, Err: syscall.ENOTDIR}
	}
	if err := f.ensureDir(parent(p)); err != nil {
		return err
	}
	err = f.preserveDirTime(parent(p), func() error {
		if err := os.Mkdir(f.up(p), info.Mode().Perm()); err != nil {
			return err
		}
		id, _ := ident.Of(info)
		if err := f.ids.persist(f.up(p), id); err != nil {
			return err
		}
		if err := os.Chmod(f.up(p), info.Mode().Perm()); err != nil {
			return err
		}
		if err := keepMode(f.up(p), info.Mode()); err != nil {
			return err
		}
		return os.Chtimes(f.up(p), info.ModTime(), info.ModTime())
	})
	if err != nil {
		return err
	}
	return f.refresh(p)
}

// copyUpBottomForOpen stages immutable bottom file q, shown at p, without
// holding mu. On reacquiring mu it either publishes that same file or tells
// OpenFile to resolve the name again if another operation changed its source.
func (f *FS) copyUpBottomForOpen(p, q string, info os.FileInfo) (bool, error) {
	bottom, epoch, ids := f.bottom, f.bottomEpoch, f.ids
	id, _ := ident.Of(info)
	linked := hardLinks(info) > 1
	inMemory := f.mem != nil && info.Size() <= f.mem.fileMax
	for {
		var tmp string
		if !inMemory {
			tmp = f.stagingName()
		}
		f.mu.Unlock()
		var data []byte
		var err error
		if inMemory {
			var c Content
			c, err = bottom.Open(q)
			if err == nil {
				data, err = readBounded(c, info.Size(), f.mem.fileMax)
				err = errors.Join(err, c.Close())
			}
		} else {
			err = bottom.CopyTo(q, tmp)
			if err == nil && !linked {
				err = ids.persist(tmp, id)
			}
			if err == nil {
				err = keepMode(tmp, info.Mode())
			}
		}
		f.mu.Lock()
		current, currentInfo, lookupErr := f.lookup(p)
		if f.bottomEpoch != epoch || lookupErr != nil || current != (source{path: q}) || !currentInfo.Mode().IsRegular() {
			if tmp != "" {
				ids.discard(tmp)
			}
			return true, nil
		}
		if err != nil {
			if tmp != "" {
				ids.discard(tmp)
			}
			return false, err
		}
		if err := f.ensureDir(parent(p)); err != nil {
			if tmp != "" {
				ids.discard(tmp)
			}
			return false, err
		}
		// Another name of this bottom inode may have been copied up while
		// staging. Only one copy can take its identity into the upper.
		if linked {
			id = f.inherit(source{}, p, info)
			if !inMemory {
				if err := ids.persist(tmp, id); err != nil {
					ids.discard(tmp)
					return false, err
				}
			}
		}
		if inMemory {
			if int64(len(data)) <= f.mem.fileMax {
				if f.addMem(p, info.Mode(), info.ModTime(), data, id) != nil {
					return false, nil
				}
			}
			inMemory = false
			continue
		}
		defer ids.discard(tmp)
		if err := f.preserveDirTime(parent(p), func() error { return hostfile.Rename(tmp, f.up(p)) }); err != nil {
			return false, err
		}
		return false, f.refresh(p)
	}
}

// copyUp stages the selected immutable entry before publishing it atomically.
func (f *FS) copyUp(p string) error {
	s, info, err := f.lookup(p)
	if err != nil {
		return err
	}
	if s.upper {
		return nil
	}
	if info.IsDir() {
		return f.ensureDir(p)
	}
	if err := f.ensureDir(parent(p)); err != nil {
		return err
	}
	if ok, err := f.copyUpMem(s, p, info); ok || err != nil {
		return err
	}
	f.tmpSeq++
	tmp := filepath.Join(f.work, strconv.FormatUint(f.tmpSeq, 36))
	defer f.ids.discard(tmp)
	if s.layer != nil {
		err = s.layer.copyTo(s.path, tmp, info)
	} else {
		err = f.bottom.CopyTo(s.path, tmp)
	}
	if err == nil {
		err = f.ids.persist(tmp, f.inherit(s, p, info))
	}
	if err == nil && info.Mode()&os.ModeSymlink == 0 {
		err = keepMode(tmp, info.Mode())
	}
	if err != nil {
		return err
	}
	if err := f.preserveDirTime(parent(p), func() error { return hostfile.Rename(tmp, f.up(p)) }); err != nil {
		return err
	}
	return f.refresh(p)
}

// upperInfo returns an indexed upper entry's metadata. Every change made
// through the overlay refreshes the index, so it is the host's, except while
// a writable descriptor of the entry's inode is open (writes go straight to
// the host file) or the entry has no known inode; only then is the host asked.
// The caller holds f.mu.
func (f *FS) upperInfo(p string, e Entry) (os.FileInfo, error) {
	if e.Kind == KindDir {
		return e.Info, nil
	}
	// A cold listing's transient view (no pathIDs) has no open descriptors.
	key, ok := f.pathIDs[p]
	if f.pathIDs == nil || ok && f.writers[key] == 0 {
		return e.Info, nil
	}
	if f.changing(p) == nil {
		info, err := lstatIndexed(f.up(p), e.Info)
		if err != nil {
			return nil, err
		}
		return linksOf(info, e.Info), nil
	}
	// The host name is changing or trails the index (queue.go); a writer's
	// descriptor still reaches the file.
	if ok && queueHost {
		for h := range f.hosts {
			if h.tracked && h.writable && h.inode == key {
				if info, err := h.File.Stat(); err == nil {
					return aliasInfo{FileInfo: linksOf(keepIdentity(info, e.Info), e.Info), name: path.Base(p)}, nil
				}
			}
		}
	}
	return e.Info, nil
}

// linksOf returns host metadata info of an upper file with the link count
// of indexed, the index's metadata of the same file: the index counts the
// names it shows, which a queued link or unlink has not reached the host
// with yet (queue.go).
func linksOf(info, indexed os.FileInfo) os.FileInfo {
	if d := int(hardLinks(indexed)) - int(hardLinks(info)); d != 0 && queueHost {
		if relinked, ok := relinked(info, d); ok {
			return relinked
		}
	}
	return info
}

// file exposes an OS file with an overlay name. A writable upper descriptor
// writes without the overlay lock; its inode's indexed metadata is refreshed
// when it closes (see [FS.upperInfo]).
type file struct {
	*os.File
	name        string
	id          ident.ID // identity of the entry opened; 0 for a host file outside the overlay
	inode       inodeKey
	tracked     bool
	writable    bool
	fs          *FS
	ownerClosed bool
	closeErr    error
	cold        string // upper path of a read opened without the index, see [FS.openUpperCold]
}

// open wraps a host file without losing its overlay-relative name; an owner
// tracks it as one of its upper descriptors.
func open(real, name string, flag int, perm os.FileMode, owner *FS) (*file, error) {
	fh, err := hostfile.OpenFile(real, flag, perm)
	if err != nil {
		return nil, err
	}
	h := &file{File: fh, name: name, writable: writable(flag)}
	if owner != nil {
		owner.adopt(h)
	}
	return h, nil
}

// adopt makes h one of f's upper descriptors, closed by [FS.CloseFiles]. The caller holds f.mu.
func (f *FS) adopt(h *file) *file {
	h.fs = f
	f.hosts[h] = struct{}{}
	return h
}

// CloseFiles closes every open descriptor of an upper file, because Windows
// refuses to rename a directory while any file below it is open. The caller
// must exclude I/O on those handles: the export reopens them in the checkout's
// next generation. Their eventual Close consumes the original close result,
// so retiring a cached protocol writer does not report a second-close error.
func (f *FS) CloseFiles() {
	f.lockSync("")
	defer f.mu.Unlock()
	for h := range f.hosts {
		h.closeErr = h.File.Close()
		h.ownerClosed = true
		if h.writable {
			h.syncInfo()
		}
	}
	clear(f.hosts)
	clear(f.writers)
}

// Close releases the descriptor. Closing a writable one refreshes its inode's
// indexed metadata: its writes bypassed the index, and on Windows the close
// itself can still move the mtime.
func (f *file) Close() error {
	if f.fs == nil {
		return f.File.Close()
	}
	// A parked queued change on its name skips only the metadata refresh.
	parked := f.fs.lockNames(f)
	defer f.fs.mu.Unlock()
	if _, open := f.fs.hosts[f]; !open {
		if f.ownerClosed {
			f.ownerClosed = false
			return f.closeErr
		}
		return f.File.Close()
	}
	delete(f.fs.hosts, f)
	err := f.File.Close()
	if f.writable && f.tracked {
		if f.fs.writers[f.inode]--; f.fs.writers[f.inode] <= 0 {
			delete(f.fs.writers, f.inode)
		}
		if parked == nil {
			f.syncInfo()
		}
	}
	return err
}

// Name returns the overlay-relative name instead of the private host path.
func (f *file) Name() string { return f.name }

// Identity returns the identity of the entry the file was opened as, which
// the export keeps to find the entry again after the handle closes; 0 for a
// host file outside the overlay (an absolute symlink's target).
func (f *file) Identity() ident.ID { return f.id }

// Stat describes the open file with the identity of the entry it was opened as.
func (f *file) Stat() (os.FileInfo, error) {
	if f.fs != nil {
		f.fs.mu.RLock()
		defer f.fs.mu.RUnlock()
	}
	info, err := f.File.Stat()
	if err != nil || f.id == 0 {
		return info, err
	}
	// The namespace index can already show a queued link/unlink; its mode
	// also carries permissions the native Windows descriptor cannot report.
	if f.fs != nil && f.tracked {
		if name := f.fs.aliases[f.inode].first; name != "" {
			indexed := f.fs.index.entries[name].Info
			info = linksOf(indexedMode(info, indexed), indexed)
		}
	}
	return idInfo{FileInfo: info, id: f.id}, nil
}

// handleID returns the identity of the entry info describes for a handle
// opened as it; 0 when it has none.
func handleID(info os.FileInfo) ident.ID {
	if info == nil {
		return 0
	}
	id, _ := ident.Of(info)
	return id
}

// Chmod applies descriptor metadata changes without leaving the upper index stale.
func (f *file) Chmod(mode os.FileMode) error {
	if f.fs == nil {
		return f.File.Chmod(mode)
	}
	if err := f.fs.lockNames(f); err != nil {
		f.fs.mu.Unlock()
		return err
	}
	defer f.fs.mu.Unlock()
	err := f.File.Chmod(mode)
	if err == nil {
		name := f.File.Name()
		if p := f.fs.aliases[f.inode].first; f.tracked && p != "" {
			name = f.fs.up(p) // the host name may have been renamed since the open
		}
		err = keepMode(name, mode)
	}
	f.syncInfo()
	return err
}

// Chown applies descriptor ownership changes under the upper's structural lock.
func (f *file) Chown(uid, gid int) error {
	if f.fs == nil {
		return f.File.Chown(uid, gid)
	}
	if err := f.fs.lockNames(f); err != nil {
		f.fs.mu.Unlock()
		return err
	}
	defer f.fs.mu.Unlock()
	err := f.File.Chown(uid, gid)
	f.syncInfo()
	return err
}

// syncInfo re-reads the metadata of every surviving name of the descriptor's
// inode; renames since it was opened move the names, not the inode.
func (f *file) syncInfo() {
	if !f.tracked {
		return
	}
	if p := f.fs.aliases[f.inode].first; p != "" {
		_ = f.fs.restat(p)
	}
}

// AppleDouble reports macOS xattr sidecar names ("._name"). Clients without
// native xattrs store xattrs in such files, and the macOS kernel tags every
// new file with com.apple.provenance, so exports keep them reachable by name
// but never list them, and searches skip them.
func AppleDouble(name string) bool { return strings.HasPrefix(name, "._") }

// Clean canonicalizes a path to slash-separated root-relative form.
func Clean(p string) string {
	p = filepath.ToSlash(p) // no-op (and allocation-free) where the separator is '/'
	if slashRelativeClean(p) {
		return p
	}
	return strings.TrimPrefix(path.Clean("/"+p), "/")
}

// slashRelativeClean reports whether p is already its own Clean form: empty,
// or non-rooted segments with no empty, "." or ".." segment and no trailing slash.
func slashRelativeClean(p string) bool {
	for start := 0; start < len(p); {
		end := strings.IndexByte(p[start:], '/')
		if end < 0 {
			end = len(p)
		} else {
			end += start
		}
		seg := p[start:end]
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
		start = end + 1
		if end == len(p)-1 {
			return false // trailing slash
		}
	}
	return true
}

// parent returns the canonical parent path, with the root represented by empty.
func parent(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[:i]
	}
	return ""
}

// reserved detects marker names in every component of a path.
func reserved(p string) bool {
	if !strings.Contains(p, whPrefix) {
		return false
	}
	for c := range strings.SplitSeq(p, "/") {
		if strings.HasPrefix(c, whPrefix) {
			return true
		}
	}
	return false
}

// within reports whether p is dir or a descendant of dir.
func within(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// missing includes ENOTDIR because a non-directory ancestor hides a subtree.
func missing(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

// notExist attaches the virtual path to a missing-entry error.
func notExist(op, p string) error { return &fs.PathError{Op: op, Path: p, Err: syscall.ENOENT} }
