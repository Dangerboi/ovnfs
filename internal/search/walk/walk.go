// Package walk is the parallel directory walker the search tools traverse
// with: gitignore-aware, cache-backed, and reading only through [fsys].
//
// Its traversal contract includes hidden-file, .git, and
// node_modules pruning; .ignore/.gitignore/.git/info/exclude/global-excludes
// semantics of the `ignore` crate; and path-ordered collection, while minimising
// syscalls per directory and spreading directories over worker goroutines:
//
//	err := walk.Walk(ctx, root, walk.Options{Gitignore: true, SkipGit: true},
//		func(w *walk.Worker, e *walk.Entry) walk.Action {
//			if e.Type == fsys.File {
//				counts[w.ID()]++
//			}
//			return walk.Continue
//		})
//
// Directories are opened relative to their parent's handle, ignore files are
// read only when a listing contains them, unchanged directories are served
// from a [Cache], and visitors open files through [Worker.OpenFile], which
// opens them relative to the directory being visited. Entry paths are carved
// out of per-worker arenas, so steady-state walking allocates a few objects
// per directory, not per entry.
package walk

import (
	"context"
	"errors"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/stencil-hq/ovnfs/internal/search/fsys"
)

// Options configures a walk. The zero value walks the native filesystem,
// skipping dot-prefixed entries and honouring no ignore files.
type Options struct {
	// FS is the filesystem walked; nil means [fsys.Native].
	FS fsys.FS
	// Config is the filesystem git's configuration and global excludes file
	// are read from (host paths found through $HOME, $XDG_CONFIG_HOME and
	// $GIT_CONFIG_*); nil means FS. A filesystem whose paths are not host
	// paths, like a checkout overlay, needs [fsys.Native] here.
	Config fsys.FS
	// Hidden includes dot-prefixed entries.
	Hidden bool
	// Gitignore honours .ignore, .gitignore, .git/info/exclude, and git's
	// global excludes file with the precedence documented in ignore.go.
	Gitignore bool
	// SkipGit prunes entries named ".git".
	SkipGit bool
	// SkipNodeModules prunes entries named "node_modules".
	SkipNodeModules bool
	// FollowLinks reports symlinks as their targets (type, size, mtime) and
	// descends into linked directories. Dangling links, links to special
	// files, and links to a directory enclosing them (which would loop) are
	// left out, as are linked directories on filesystems without
	// [fsys.Session.Identify].
	FollowLinks bool
	// Detail selects listing metadata; see [fsys.Full] for its cost.
	Detail fsys.Detail
	// MaxDepth bounds entry depth (the root's children have depth 1); zero
	// means unlimited.
	MaxDepth int
	// Sorted makes [Collect] return entries in byte order of Path. Workers
	// sort each directory's share of the order in parallel; Collect only
	// concatenates. [Walk] ignores it.
	Sorted bool
	// Filter decides during [Collect] which entries are kept (Continue),
	// dropped (Skip), or dropped with their subtree (SkipDir); it runs
	// concurrently on the workers and may use the worker's session. It may
	// rewrite a kept entry's Type, Size, and Mtime (say, to describe a
	// symlink's target); descent still follows the listed type. nil keeps
	// everything. [Walk] uses its visitor instead.
	Filter Visitor
	// Workers is the number of walking goroutines; zero picks
	// min(GOMAXPROCS, FS.Workers()), the parallelism past which kernel
	// contention costs more than it gains (4 on macOS, 8 elsewhere).
	Workers int
	// Cache, when set, reuses unchanged directories' listings from earlier
	// walks (see [Cache]). Only Minimal-detail walks consult it.
	Cache *Cache
}

// Entry is one walked filesystem entry.
type Entry struct {
	// Path is relative to the walk root and '/'-separated. Paths returned by
	// [Collect] (and seen by Options.Filter) are ordinary immutable strings.
	// A [Visitor]'s entry and its Path are only valid until the visitor
	// returns (the bytes are reused), so visitors that keep a path must copy
	// it with strings.Clone.
	Path  string
	Type  fsys.Type
	Depth int
	// Size is the byte size of regular files when known (Full detail or
	// Windows), else -1.
	Size int64
	// Mtime is the modification time in Unix nanoseconds when known (Full
	// detail or Windows), else 0.
	Mtime int64
}

// Name returns the final component of e.Path.
func (e *Entry) Name() string { return e.Path[strings.LastIndexByte(e.Path, '/')+1:] }

// Action steers a walk from a visitor or filter.
type Action uint8

const (
	// Continue keeps the entry; directories are descended.
	Continue Action = iota
	// Skip leaves the entry out of a [Collect] but still descends into it.
	Skip
	// SkipDir leaves the entry out and does not descend into it.
	SkipDir
	// Stop ends the walk as soon as in-flight directories finish.
	Stop
)

// Visitor receives entries from [Walk]. It runs concurrently on up to
// Options.Workers goroutines; w identifies the calling worker, so per-worker
// state indexed by w.ID() needs no locking. e and e.Path are reused once
// the visitor returns.
type Visitor func(w *Worker, e *Entry) Action

// Walk visits every entry below root that opts admits, in no particular
// order, calling visit concurrently from the worker goroutines. Unreadable
// directories below the root are skipped. It returns an error when the root
// cannot be opened as a directory or ctx ends the walk.
func Walk(ctx context.Context, root string, opts Options, visit Visitor) error {
	w, err := newWalker(ctx, root, opts)
	if err != nil {
		return err
	}
	w.visit = visit
	defer w.release()
	return w.run(ctx)
}

// Collect returns every entry below root that opts admits; see
// Options.Sorted for ordering and Options.Filter for selection. Errors are
// as for [Walk].
func Collect(ctx context.Context, root string, opts Options) ([]Entry, error) {
	w, err := newWalker(ctx, root, opts)
	if err != nil {
		return nil, err
	}
	if opts.Sorted {
		w.sortedOut = new(dirOut)
	}
	defer w.release()
	if err := w.run(ctx); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make([]Entry, 0, w.total.Load())
	if opts.Sorted {
		out = flatten(ctx, out, w.sortedOut)
	} else {
		for _, wk := range w.workers {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			out = append(out, wk.out...)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// maxRetainedDirs caps directory handles kept open so pending children can
// open relative to them. Past the cap, children open by full path.
var maxRetainedDirs int32 = 1024

type walker struct {
	ctx       context.Context
	opts      Options
	fs        fsys.FS
	sep       byte   // fs.Separator()
	root      string // absolute, clean
	ign       *ignorer
	cache     *Cache // nil unless opts.Cache applies to this walk
	visit     Visitor
	sortedOut *dirOut
	workers   []*Worker
	total     atomic.Int64 // collected entries
	retained  atomic.Int32
	stop      atomic.Bool

	mu       sync.Mutex
	cond     sync.Cond
	queue    []*dirJob    // shared stack, guarded by mu
	sleeping int          // workers in cond.Wait, guarded by mu
	done     bool         // guarded by mu
	queued   atomic.Int32 // len(queue), for lock-free polling
	finished atomic.Bool  // done, for lock-free polling
	idle     atomic.Int32 // workers in take
	pending  atomic.Int64 // jobs created and not yet finished
}

// dirJob is a directory waiting to be listed.
type dirJob struct {
	parent *dirRef // open parent to resolve name against; nil opens by path
	name   string  // final component, for parent-relative opens
	rel    string  // walk-relative path; "" for the root
	depth  int     // root is 0
	count  uint32  // child count from the parent's listing, 0 if unknown
	ign    *ignoreState
	out    *dirOut // sorted collection slot
}

// dirRef is a directory handle shared by the jobs of its subdirectories and
// closed when the last of them has opened its own handle.
type dirRef struct {
	h    fsys.Handle
	refs atomic.Int32
	w    *walker
}

// release drops one reference; the last closes the handle with s.
func (d *dirRef) release(s fsys.Session) {
	if d.refs.Add(-1) == 0 {
		s.Close(d.h)
		d.w.retained.Add(-1)
		d.w = nil
		refPool.Put(d)
	}
}

// Jobs and handle references are recycled; a directory costs no heap
// allocations of its own in steady state.
var (
	jobPool = sync.Pool{New: func() any { return new(dirJob) }}
	refPool = sync.Pool{New: func() any { return new(dirRef) }}
)

func newDirRef(h fsys.Handle, w *walker) *dirRef {
	d := refPool.Get().(*dirRef)
	d.h, d.w = h, w
	d.refs.Store(1)
	w.retained.Add(1)
	return d
}

// dirOut holds one directory's entries for sorted collection.
type dirOut struct {
	entries []Entry
	kids    []*dirOut // subdirectory results
	// order lists entries (index ≥ 0) and kids (index -k-1) in Path order.
	order []int32
}

func flatten(ctx context.Context, dst []Entry, o *dirOut) []Entry {
	for _, i := range o.order {
		if ctx.Err() != nil {
			return dst
		}
		if i >= 0 {
			dst = append(dst, o.entries[i])
		} else {
			dst = flatten(ctx, dst, o.kids[-i-1])
		}
	}
	return dst
}

// orderItem is an entry (key name) or a subtree (key name + "/") of one
// directory. Sorting these keys orders the directory's part of the output
// exactly as sorting full paths would: a sibling such as "a-b" lands between
// directory "a" and its contents "a/…" because '-' < '/'.
type orderItem struct {
	name  string
	slash bool
	idx   int32
}

func compareOrderItems(a, b orderItem) int {
	n := min(len(a.name), len(b.name))
	if c := strings.Compare(a.name[:n], b.name[:n]); c != 0 {
		return c
	}
	// One name is a prefix of the other; compare the next key byte, where a
	// subtree key continues with '/' and an entry key ends.
	next := func(it orderItem) int {
		switch {
		case len(it.name) > n:
			return int(it.name[n])
		case it.slash:
			return '/'
		}
		return -1
	}
	return next(a) - next(b)
}

// Filesystem returns o.FS, defaulting to [fsys.Native].
func (o *Options) Filesystem() fsys.FS {
	if o.FS == nil {
		return fsys.Native
	}
	return o.FS
}

// Parallelism returns how many workers a walk with o runs, so callers can
// size per-worker state indexed by [Worker.ID] before walking.
func (o *Options) Parallelism() int {
	if o.Workers > 0 {
		return o.Workers
	}
	return min(runtime.GOMAXPROCS(0), o.Filesystem().Workers())
}

func newWalker(ctx context.Context, root string, opts Options) (*walker, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fs := opts.Filesystem()
	abs, err := fs.Abs(root)
	if cancelErr := ctx.Err(); cancelErr != nil {
		return nil, cancelErr
	}
	if err != nil {
		return nil, err
	}
	opts.Workers = opts.Parallelism()
	w := &walker{ctx: ctx, opts: opts, fs: fs, sep: fs.Separator(), root: abs}
	if opts.Detail == fsys.Minimal {
		w.cache = opts.Cache
	}
	w.cond.L = &w.mu
	return w, nil
}

// workerPool recycles workers' scratch across walks of the same FS.
var workerPool sync.Pool

func (w *walker) stopped() bool { return w.stop.Load() || w.ctx.Err() != nil }

func (w *walker) run(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	w.workers = make([]*Worker, w.opts.Workers)
	for i := range w.workers {
		wk, _ := workerPool.Get().(*Worker)
		if wk == nil || wk.fs != w.fs {
			wk = &Worker{fs: w.fs, s: w.fs.NewSession()}
		}
		wk.id, wk.w = i, w
		w.workers[i] = wk
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	first := w.workers[0]
	h, err := first.s.OpenDir(w.root)
	if cancelErr := ctx.Err(); cancelErr != nil {
		if err == nil {
			first.s.Close(h)
		}
		return cancelErr
	}
	if err != nil {
		return &os.PathError{Op: "open", Path: w.root, Err: err}
	}
	var rootIgn *ignoreState
	if w.opts.Gitignore {
		cwd, _ := os.Getwd()
		config := first.s
		if w.opts.Config != nil {
			config = w.opts.Config.NewSession()
		}
		w.ign = &ignorer{global: globalExcludes(ctx, config, &first.ibuf), cwd: cwd, root: w.root}
		rootIgn = ancestorStates(ctx, first.s, &first.ibuf, w.root, w.sep)
	}
	if err := ctx.Err(); err != nil {
		first.s.Close(h)
		return err
	}
	rootJob := jobPool.Get().(*dirJob)
	*rootJob = dirJob{parent: newDirRef(h, w), out: w.sortedOut, ign: rootIgn}
	w.pending.Store(1)
	first.local = append(first.local, rootJob)
	var wg sync.WaitGroup
	for _, wk := range w.workers[1:] {
		wg.Go(func() {
			wk.loop()
		})
	}
	first.loop()
	wg.Wait()
	return ctx.Err()
}

// release returns worker scratch to the pool once results are copied out.
// The arena keeps its unused tail: earlier strings never see those bytes.
func (w *walker) release() {
	for _, wk := range w.workers {
		clear(wk.out)
		clear(wk.items)
		clear(wk.local)
		wk.out, wk.items, wk.local = wk.out[:0], wk.items[:0], wk.local[:0]
		wk.w, wk.entry, wk.dir = nil, Entry{}, fsys.Handle{}
		workerPool.Put(wk)
	}
	w.workers = nil
}

// takeSpins is how many times an idle worker polls the shared stack
// (yielding in between) before sleeping. A directory takes microseconds, so
// sleeping right away would make thread wake-up latency the bottleneck.
const takeSpins = 128

// take returns a shared job, or nil once the walk is done.
func (w *walker) take() *dirJob {
	w.idle.Add(1)
	defer w.idle.Add(-1)
	for range takeSpins {
		if w.queued.Load() > 0 || w.finished.Load() {
			w.mu.Lock()
			j, done := w.popLocked()
			w.mu.Unlock()
			if j != nil || done {
				return j
			}
		}
		runtime.Gosched()
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for {
		if j, done := w.popLocked(); j != nil || done {
			return j
		}
		w.sleeping++
		w.cond.Wait()
		w.sleeping--
	}
}

func (w *walker) popLocked() (j *dirJob, done bool) {
	if n := len(w.queue); n > 0 {
		j = w.queue[n-1]
		w.queue = w.queue[:n-1]
		w.queued.Add(-1)
		return j, false
	}
	return nil, w.done
}

// share moves jobs to the shared stack for idle workers.
func (w *walker) share(jobs []*dirJob) {
	w.mu.Lock()
	w.queue = append(w.queue, jobs...)
	w.queued.Add(int32(len(jobs)))
	wake := min(len(jobs), w.sleeping)
	w.mu.Unlock()
	for range wake {
		w.cond.Signal()
	}
}

func (w *walker) finish(n int64) {
	if w.pending.Add(-n) == 0 {
		w.mu.Lock()
		w.done = true
		w.finished.Store(true)
		w.mu.Unlock()
		w.cond.Broadcast()
	}
}

// Worker is one walking goroutine. Visitors receive it to key per-worker
// state ([Worker.ID]) and to open the files they visit ([Worker.OpenFile]).
type Worker struct {
	id    int
	w     *walker
	fs    fsys.FS
	s     fsys.Session
	dir   fsys.Handle // directory being emitted, when open
	local []*dirJob   // LIFO: depth-first locally, oldest half shared
	arena []byte      // retained path strings
	rel   []byte      // volatile path of the entry being visited
	abs   []byte      // absolute path scratch (cache keys, open by path)
	out   []Entry     // unordered collection
	items []orderItem // sorted collection scratch
	buf   []byte      // ignore-path scratch
	ibuf  []byte      // ignore-file read buffer
	entry Entry
}

// ID is the worker's index, 0 ≤ ID < Options.Workers.
func (wk *Worker) ID() int { return wk.id }

// Session is the worker's filesystem session, for reading files opened with
// OpenFile and anything else. It must only be used from the visitor call.
func (wk *Worker) Session() fsys.Session { return wk.s }

// Abs returns e's absolute path in the walked filesystem, for
// [fsys.Session] calls on it. The string is only valid until the next Abs
// or OpenFile call on this worker.
func (wk *Worker) Abs(e *Entry) string {
	abs := wk.absPath(e.Path)
	return unsafe.String(unsafe.SliceData(abs), len(abs))
}

// OpenFile opens the regular file e (the entry being visited) for reading
// through Session; close it there too. While the walk holds e's directory
// open the file is opened relative to it (a one-component lookup); entries
// served from a [Cache] are opened by absolute path. Errors are as for
// [fsys.Session.OpenFile].
func (wk *Worker) OpenFile(e *Entry) (fsys.Handle, fsys.FileInfo, error) {
	if err := wk.w.ctx.Err(); err != nil {
		return fsys.Handle{}, fsys.FileInfo{}, err
	}
	if wk.dir.Valid() {
		return wk.s.OpenFile(wk.dir, e.Name())
	}
	return wk.s.OpenFile(fsys.Handle{}, wk.Abs(e))
}

func (wk *Worker) loop() {
	w := wk.w
	for {
		var j *dirJob
		if n := len(wk.local); n > 0 {
			j = wk.local[n-1]
			wk.local = wk.local[:n-1]
		} else if j = w.take(); j == nil {
			return
		}
		wk.process(j)
		*j = dirJob{}
		jobPool.Put(j)
		if n := len(wk.local); n > 1 {
			if idle := int(w.idle.Load()); idle > 0 {
				// Share the oldest (shallowest, largest) jobs: at least half,
				// or one per idle worker while keeping one.
				give := max(n/2, min(n-1, idle))
				w.share(wk.local[:give])
				wk.local = append(wk.local[:0], wk.local[give:]...)
			}
		}
		w.finish(1)
	}
}

// path returns a string of dir + "/" + name carved from the worker arena.
func (wk *Worker) path(dir string, name []byte) string {
	n := len(name)
	if dir != "" {
		n += len(dir) + 1
	}
	if cap(wk.arena)-len(wk.arena) < n {
		wk.arena = make([]byte, 0, max(32<<10, n))
	}
	start := len(wk.arena)
	if dir != "" {
		wk.arena = append(append(wk.arena, dir...), '/')
	}
	wk.arena = append(wk.arena, name...)
	return unsafe.String(&wk.arena[start], n)
}

func (wk *Worker) process(j *dirJob) {
	w := wk.w
	s := wk.s
	if w.stopped() {
		if j.parent != nil {
			j.parent.release(s)
		}
		return
	}
	ign := j.ign
	if c := w.cache; c != nil {
		if d := c.cachedListing(w.ctx, s, wk.absPath(j.rel), w.sep, w.ign != nil); d != nil {
			if j.parent != nil {
				j.parent.release(s)
			}
			if w.ign != nil {
				ign = d.ignores.state(ign, d.flags, j.rel)
			}
			wk.emit(j, d.listing, ign, nil, fsys.Handle{})
			return
		}
	}

	if w.stopped() {
		if j.parent != nil {
			j.parent.release(s)
		}
		return
	}
	var h fsys.Handle
	var err error
	var self *dirRef
	switch {
	case j.rel == "":
		h, self = j.parent.h, j.parent // the root handle opened by run, with its reference
	case j.parent != nil:
		h, err = s.OpenChild(j.parent.h, j.name)
		j.parent.release(s)
	default:
		abs := wk.absPath(j.rel)
		h, err = s.OpenDir(unsafe.String(unsafe.SliceData(abs), len(abs)))
	}
	if err != nil {
		return
	}
	defer func() {
		if self != nil {
			self.release(s)
		} else {
			s.Close(h)
		}
	}()

	if w.stopped() {
		return
	}
	var st fsys.Stamp
	caching := w.cache != nil
	if caching {
		// Stamp before listing: any later change moves the stamp on.
		st, err = s.StatDir(h)
		caching = err == nil
	}
	if w.stopped() {
		return
	}
	ls, err := s.List(h, w.opts.Detail, j.count)
	if err != nil || w.stopped() {
		return
	}
	var names entryNames
	if w.ign != nil || caching {
		for i := range ls.Entries {
			if w.stopped() {
				return
			}
			names.record(ls.Name(&ls.Entries[i]), ls.Entries[i].Type)
		}
	}
	var ignores *dirIgnores
	if w.ign != nil {
		d := loadIgnores(w.ctx, s, &wk.ibuf, h, names, caching)
		ignores = &d
		ign = d.state(ign, names, j.rel)
	}
	if w.stopped() {
		return
	}
	if caching {
		w.cache.store(w.ctx, s, wk.absPath(j.rel), h, st, ls, names, ignores)
	}
	wk.emit(j, ls, ign, &self, h)
}

// follow resolves the symlink name in the walk-relative directory dir for
// Options.FollowLinks. It reports false for dangling links, links to
// special files, and links to dir or one of its ancestors.
func (wk *Worker) follow(dir string, name []byte) (fsys.FileInfo, bool) {
	if wk.w.stopped() {
		return fsys.FileInfo{}, false
	}
	abs := wk.absPath(dir)
	if abs[len(abs)-1] != wk.w.sep {
		abs = append(abs, wk.w.sep)
	}
	wk.abs = append(abs, name...)
	p := unsafe.String(unsafe.SliceData(wk.abs), len(wk.abs))
	info, err := wk.s.Stat(p, true)
	switch {
	case err != nil || wk.w.stopped() || (info.Type != fsys.File && info.Type != fsys.Dir):
		return info, false
	case info.Type == fsys.File:
		return info, true
	}
	id, err := wk.s.Identify(p)
	if err != nil || wk.w.stopped() {
		return info, false
	}
	// Cycles are rare and shallow: identify the enclosing directories only
	// when a linked directory needs checking.
	for rel := dir; ; rel = rel[:max(strings.LastIndexByte(rel, '/'), 0)] {
		if wk.w.stopped() {
			return info, false
		}
		var a fsys.ID
		var err error
		if rel == dir && wk.dir.Valid() {
			// The directory being emitted is already open: ask its descriptor.
			a, err = wk.s.IdentifyDir(wk.dir)
		} else {
			err = errors.ErrUnsupported
		}
		if err != nil {
			up := wk.absPath(rel)
			a, err = wk.s.Identify(unsafe.String(unsafe.SliceData(up), len(up)))
		}
		if err == nil && a == id {
			return info, false
		}
		if rel == "" {
			return info, true
		}
	}
}

// absPath returns the absolute path of the walk-relative rel in the
// worker's scratch buffer, valid until the next call.
func (wk *Worker) absPath(rel string) []byte {
	sep := wk.w.sep
	wk.abs = append(wk.abs[:0], wk.w.root...)
	if rel != "" {
		if len(wk.abs) == 0 || wk.abs[len(wk.abs)-1] != sep {
			wk.abs = append(wk.abs, sep)
		}
		start := len(wk.abs)
		wk.abs = append(wk.abs, rel...)
		if sep != '/' {
			for i := start; i < len(wk.abs); i++ {
				if wk.abs[i] == '/' {
					wk.abs[i] = sep
				}
			}
		}
	}
	return wk.abs
}

// emit filters and delivers one directory's listing and queues its
// subdirectories. When self is non-nil, children open relative to h through
// the shared reference *self (created on first use) and visitors read files
// relative to h; cached listings pass nil and no handle.
func (wk *Worker) emit(j *dirJob, ls *fsys.Listing, ign *ignoreState, self **dirRef, h fsys.Handle) {
	w := wk.w
	opts := &w.opts
	wk.dir = h
	defer func() { wk.dir = fsys.Handle{} }()
	var out *dirOut
	if j.out != nil {
		out = j.out
		out.entries = make([]Entry, 0, len(ls.Entries))
		wk.items = wk.items[:0]
	}
	depth := j.depth + 1
	descend := opts.MaxDepth == 0 || depth < opts.MaxDepth
	firstChild := len(wk.local)
	var collected int64
	e := &wk.entry
	for i := range ls.Entries {
		if w.stopped() {
			break
		}
		re := &ls.Entries[i]
		name := ls.Name(re)
		if name[0] == '.' && !opts.Hidden {
			continue
		}
		if (opts.SkipGit && string(name) == ".git") || (opts.SkipNodeModules && string(name) == "node_modules") {
			continue
		}
		typ, size, mtime := re.Type, re.Size, re.Mtime
		if typ == fsys.Symlink && opts.FollowLinks {
			info, ok := wk.follow(j.rel, name)
			if !ok {
				continue
			}
			typ, size, mtime = info.Type, info.Size, info.Stamp.Mtime
			if typ == fsys.Dir {
				size = -1
			}
		}
		isDir := typ == fsys.Dir
		var rel string
		if w.visit == nil || (isDir && descend) {
			rel = wk.path(j.rel, name)
		} else {
			if j.rel == "" {
				wk.rel = append(wk.rel[:0], name...)
			} else {
				wk.rel = append(append(append(wk.rel[:0], j.rel...), '/'), name...)
			}
			rel = unsafe.String(unsafe.SliceData(wk.rel), len(wk.rel))
		}
		if w.ign != nil && w.ign.ignored(ign, rel, isDir, &wk.buf) {
			continue
		}
		*e = Entry{Path: rel, Type: typ, Depth: depth, Size: size, Mtime: mtime}
		action := Continue
		switch {
		case w.visit != nil:
			action = w.visit(wk, e)
		case opts.Filter != nil:
			action = opts.Filter(wk, e)
		}
		if w.stopped() {
			break
		}
		switch action {
		case Stop:
			w.stop.Store(true)
			for _, c := range wk.local[firstChild:] {
				if c.parent != nil {
					c.parent.release(wk.s)
				}
				*c = dirJob{}
				jobPool.Put(c)
			}
			wk.local = wk.local[:firstChild]
			return
		case SkipDir:
			continue
		case Continue:
			if w.visit != nil {
				break
			}
			if out != nil {
				wk.items = append(wk.items, orderItem{name: rel[len(rel)-len(name):], idx: int32(len(out.entries))})
				out.entries = append(out.entries, *e)
			} else {
				wk.out = append(wk.out, *e)
			}
			collected++
		}
		if !isDir || !descend {
			continue
		}
		child := jobPool.Get().(*dirJob)
		*child = dirJob{name: rel[len(rel)-len(name):], rel: rel, depth: depth, count: re.Children, ign: ign}
		// A linked directory opens by path: OpenChild refuses symlinks.
		if self != nil && re.Type != fsys.Symlink {
			if *self == nil && w.retained.Load() < maxRetainedDirs {
				*self = newDirRef(h, w)
			}
			if *self != nil {
				(*self).refs.Add(1)
				child.parent = *self
			}
		}
		if out != nil {
			child.out = new(dirOut)
			out.kids = append(out.kids, child.out)
			wk.items = append(wk.items, orderItem{name: child.name, slash: true, idx: -int32(len(out.kids))})
		}
		wk.local = append(wk.local, child)
	}
	if collected > 0 {
		w.total.Add(collected)
	}
	if out != nil && !w.stopped() {
		slices.SortFunc(wk.items, compareOrderItems)
		out.order = make([]int32, len(wk.items))
		for i := range wk.items {
			out.order[i] = wk.items[i].idx
		}
	}
	if n := int64(len(wk.local) - firstChild); n > 0 {
		w.pending.Add(n)
		// Pop order is LIFO; reverse so the first child is walked first.
		slices.Reverse(wk.local[firstChild:])
	}
}
