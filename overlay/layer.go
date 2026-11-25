package overlay

import (
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/stencil-hq/ovnfs/internal/ident"
)

// Bottom is the read-only filesystem beneath an overlay's immutable layers.
// Every FileInfo it returns satisfies [ident.Of] with its entry's identity.
type Bottom interface {
	Lstat(p string) (os.FileInfo, error)
	// ReadDir returns a caller-owned slice sorted by name. It rejects missing
	// paths and non-directories, including final symlinks.
	ReadDir(p string) ([]os.FileInfo, error)
	Open(p string) (Content, error)
	Readlink(p string) (string, error)
	CopyTo(p, dst string) error
	// Locate returns the path of the bottom entry whose identity is id, or
	// false; [FS.Locate] verifies that the overlay still shows it there. It
	// must not need an earlier lookup of the entry, except for identities
	// derived from a path ([ident.Named]), which this process handed out.
	Locate(id ident.ID) (string, bool)
}

// AliasLocator is implemented by a bottom that can give a file several
// names. When its first answer to Bottom.Locate is hidden, the overlay
// tries the others with their merged visibility (including redirects).
type AliasLocator interface {
	LocateAlias(id ident.ID, visible func(string) bool) bool
}

// VanishedLocator is implemented by a bottom whose fenced cache entries can
// leave the view after the host changes them. Vanished returns a remembered
// pre-fence path and metadata for id when ok accepts the path and the entry
// is no longer shown by the fence.
type VanishedLocator interface {
	Vanished(id ident.ID, ok func(string) bool) (string, os.FileInfo, bool)
}

// InfoOpener is implemented by a bottom that returns a regular file's
// metadata alongside its open Content, so [FS.openLower] opens a bottom file
// in one resolve instead of stating and resolving it twice. Non-regular
// entries return (nil, info, nil).
type InfoOpener interface {
	OpenInfo(p string) (Content, os.FileInfo, error)
}

// Awaiter is implemented by bottoms whose first access to some paths waits
// for background work (base cache snapshots). [FS] calls Await(p) before
// taking its lock in every entry point taking a path (p clean,
// checkout-relative; for Rename and Link both paths), so a waiting access
// does not hold up the checkout's other operations.
type Awaiter interface {
	Await(p string)
}

// Kind identifies a frozen layer entry, including deletion markers.
type Kind uint8

// Layer entry kinds distinguish data from deletion markers.
const (
	KindFile Kind = iota + 1
	KindDir
	KindSymlink
	KindWhiteout
)

// Entry describes one indexed path; whiteouts have no file information.
type Entry struct {
	Kind   Kind
	Opaque bool
	Info   os.FileInfo
	Origin string // path before this upper's renames; a delta-base hint, not file identity
	// Redirect, on a directory a rename moved from a tree the levels below
	// hold, is the path those levels know that tree by: they show it below
	// the directory, under whatever the layer holds there itself, while its
	// old name is whited out. See [FS.Rename] and [Layer.redirected].
	Redirect string
}

// Layer is a frozen directory indexed once and safe for concurrent readers.
// Its files start raw in the directory; [Layer.Store] moves those snapshot
// hashing kept in the store out of it, and both backings serve the same view.
type Layer struct {
	dir      string
	entries  map[string]Entry
	children map[string]map[string]Entry
	gits     map[string]bool // paths of entries named .git, see [Layer.GitEntries]
	// redirects maps each directory with an [Entry.Redirect] to that origin.
	redirects map[string]string
	// fold, set on a mutable upper whose volume takes different names for
	// one entry, gives the form of a name under which the volume finds it;
	// folded maps each entry's [foldedPath] to its path, see [FS.claimName].
	fold     func(string) string
	folded   map[string]string
	storing  sync.Mutex        // serializes [Layer.Store]
	mu       sync.RWMutex      // guards blobs, ids and tree; held shared while raw files are read, see [Layer.Content]
	blobs    Blobs             // nil while every entry is raw
	ids      map[string]string // path → blob id of a stored file's content or symlink's target
	tree     string            // store tree reaching every stored blob; "" when none
	metadata []byte            // immutable compressed metadata once stored

	// A layer restored from metadata keeps its index only while used, see [Layer.view].
	resident atomic.Pointer[Layer] // the index; nil while cold
	loading  sync.Mutex            // serializes rebuilding resident
	used     atomic.Bool           // set by view, cleared by [Layer.Cool]
	idle     int                   // Cool calls since the last use; Cool's caller owns it

	synthetic uint64 // Synthetic high-water mark of the upper the layer was, see [synthetic]
	legacy    uint64 // key of hashed identities of a version 1 manifest, see [legacyIdentity]

	shadowed []string // a live upper's names whose deletion marker a crash left beside their data, see [FS.loadIndex]

	locating sync.Once             // builds byID and moreIDs on the index's first [Layer.locate]
	byID     map[ident.ID]string   // a path of each identity
	moreIDs  map[ident.ID][]string // further hard-link paths of identities in byID
}

// IndexLayer indexes a frozen upper after its staging work directory is removed.
func IndexLayer(dir string) (*Layer, error) { return indexLayer(dir, false) }

// IndexUpper indexes a live upper for a read-only pass (diff, plan); the result is a
// point-in-time view that later writes to the upper do not update.
func IndexUpper(dir string) (*Layer, error) { return indexLayer(dir, true) }

// indexLayer also supports a live upper whose staging directory is private.
// It reads every entry's persisted identity, and a frozen layer's Synthetic
// high-water mark.
func indexLayer(dir string, live bool) (*Layer, error) {
	ids, err := layerStore(dir)
	if err != nil {
		return nil, err
	}
	l, err := walkLayer(dir, live, true, "", ids)
	if err == nil && !live {
		l.synthetic, err = readAttr(l.dir, nextAttr)
	}
	return l, err
}

// walkLayer can visit only a listing's ancestors and direct children. ids
// holds the identities of dir's files (see [describe]).
func walkLayer(dir string, live, recursive bool, listing string, ids *idStore) (*Layer, error) {
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	l := &Layer{dir: root, entries: make(map[string]Entry), children: make(map[string]map[string]Entry)}
	err = filepath.WalkDir(root, func(real string, item fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, real)
		if err != nil {
			return err
		}
		p := Clean(rel)
		// A shallow listing needs its ancestors, their markers and its own children.
		if !recursive && p != "" && parent(p) != listing && p != listing && !strings.HasPrefix(listing, p+"/") &&
			!strings.HasPrefix(path.Base(p), whPrefix) {
			if item.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		// Not item.Info(): on Windows that is the copy NTFS keeps in the parent's
		// listing, which lags the file's own record (a grown directory lists size
		// 0), while every other index update stats the file itself.
		info, err := os.Lstat(real)
		if err != nil {
			return err
		}
		if p == "" {
			if !info.IsDir() {
				return &fs.PathError{Op: "index", Path: root, Err: fs.ErrInvalid}
			}
			if info, err = describe(real, info, ids); err != nil {
				return err
			}
			l.set(p, entry(info))
			return nil
		}
		n := info.Name()
		switch {
		case n == workName:
			if !live || parent(p) != "" {
				return fmt.Errorf("overlay: staging directory remains in layer: %s", real)
			}
			if info.IsDir() {
				return filepath.SkipDir
			}
			return fmt.Errorf("overlay: staging path is not a directory: %s", real)
		case n == opaqueName:
			e := l.entries[parent(p)]
			e.Opaque = true
			l.set(parent(p), e)
		case n == redirectName:
			origin, err := readRedirect(real)
			if err != nil {
				return err
			}
			e := l.entries[parent(p)]
			e.Redirect = origin
			l.set(parent(p), e)
		case strings.HasPrefix(n, metaPrefix):
		case strings.HasPrefix(n, whPrefix):
			q := path.Join(parent(p), strings.TrimPrefix(n, whPrefix))
			if e, ok := l.entries[q]; !ok {
				l.set(q, Entry{Kind: KindWhiteout})
			} else if live && e.Kind != KindWhiteout {
				l.shadowed = append(l.shadowed, q)
			}
		default:
			if info, err = describe(real, info, ids); err != nil {
				return err
			}
			if live && l.entries[p].Kind == KindWhiteout {
				l.shadowed = append(l.shadowed, p)
			}
			l.set(p, entry(info))
			if info.IsDir() && !recursive && p != listing && !strings.HasPrefix(listing, p+"/") {
				return filepath.SkipDir
			}
			return nil
		}
		if info.IsDir() {
			return filepath.SkipDir
		}
		return nil
	})
	if err == nil {
		err = l.loadOrigins()
	}
	return l, err
}

// Dir returns the host path of the immutable layer: the directory of its raw
// files and, once stored, its manifest; or the manifest file alone once
// nothing raw remains. Garbage collection removes it with the layer.
func (l *Layer) Dir() string { return l.dir }

// Get looks up a canonical overlay path without touching the filesystem.
func (l *Layer) Get(p string) (Entry, bool) { e, ok := l.view().entries[Clean(p)]; return e, ok }

// Children returns an independent map of direct entries, including whiteouts.
func (l *Layer) Children(p string) map[string]Entry { return maps.Clone(l.view().children[Clean(p)]) }

// Redirects returns an independent map of the layer's redirected directories
// to their origins (see [Entry.Redirect]), for callers resolving paths across
// layers without walking them.
func (l *Layer) Redirects() map[string]string { return maps.Clone(l.view().redirects) }

// GitEntries lists the paths of the layer's entries named .git, in no order. The
// index keeps them as it changes, so the git adapter finds a checkout's gitfiles
// without walking every entry.
func (l *Layer) GitEntries() []string { return slices.Collect(maps.Keys(l.view().gits)) }

// Extract moves entry p, with everything below it, out of a layer that
// [FS.Freeze] just returned and nothing reads yet, to the new host path dst.
// The directory that held p keeps its indexed modification time.
func (l *Layer) Extract(p, dst string) error {
	p = Clean(p)
	if e, ok := l.entries[p]; p == "" || !ok || e.Kind == KindWhiteout {
		return &fs.PathError{Op: "extract", Path: p, Err: fs.ErrNotExist}
	}
	if err := os.Rename(filepath.Join(l.dir, filepath.FromSlash(p)), dst); err != nil {
		return err
	}
	l.drop(p)
	dir := l.entries[parent(p)].Info
	return os.Chtimes(filepath.Join(l.dir, filepath.FromSlash(parent(p))), dir.ModTime(), dir.ModTime())
}

// Walk visits entries in deterministic pre-order, including the root directory.
func (l *Layer) Walk(fn func(p string, e Entry) error) error {
	l = l.view()
	var visit func(string) error
	visit = func(p string) error {
		if err := fn(p, l.entries[p]); err != nil {
			return err
		}
		for _, n := range slices.Sorted(maps.Keys(l.children[p])) {
			if err := visit(path.Join(p, n)); err != nil {
				return err
			}
		}
		return nil
	}
	return visit("")
}

// locate returns a path of the layer's entry with identity id that visible
// accepts: the overlay shows it there. The index of identities is built on
// first use and kept as long as the index itself.
func (l *Layer) locate(id ident.ID, visible func(string) bool) (string, bool) {
	v := l.view()
	v.locating.Do(v.indexIDs)
	if p, ok := v.byID[id]; ok && visible(p) {
		return p, true
	}
	for _, p := range v.moreIDs[id] {
		if visible(p) {
			return p, true
		}
	}
	return "", false
}

// indexIDs maps the identities of the entries of an index that no longer changes.
func (l *Layer) indexIDs() {
	l.byID = make(map[ident.ID]string, len(l.entries))
	for p, e := range l.entries {
		if e.Info == nil {
			continue
		}
		id, ok := ident.Of(e.Info)
		if !ok {
			continue
		}
		if _, dup := l.byID[id]; dup {
			if l.moreIDs == nil {
				l.moreIDs = make(map[ident.ID][]string)
			}
			l.moreIDs[id] = append(l.moreIDs[id], p)
			continue
		}
		l.byID[id] = p
	}
}

// entry translates host metadata into an indexed data entry.
func entry(info os.FileInfo) Entry {
	kind := KindFile
	if info.IsDir() {
		kind = KindDir
	} else if info.Mode()&os.ModeSymlink != 0 {
		kind = KindSymlink
	}
	return Entry{Kind: kind, Info: info}
}

// set updates a mutable upper's path and direct-child indexes together.
func (l *Layer) set(p string, e Entry) {
	if (e.Kind == KindFile || e.Kind == KindSymlink) && e.Origin == "" {
		e.Origin = l.entries[p].Origin
	}
	l.entries[p] = e
	if l.fold != nil && p != "" {
		k := foldedPath(parent(p), l.fold(path.Base(p)))
		if e.Kind != KindWhiteout {
			l.folded[k] = p
		} else if l.folded[k] == p {
			delete(l.folded, k)
		}
	}
	if e.Redirect != "" {
		if l.redirects == nil {
			l.redirects = make(map[string]string)
		}
		l.redirects[p] = e.Redirect
	} else {
		delete(l.redirects, p)
	}
	if path.Base(p) == ".git" {
		if l.gits == nil {
			l.gits = make(map[string]bool)
		}
		l.gits[p] = true
	}
	if p != "" {
		d := parent(p)
		if l.children[d] == nil {
			l.children[d] = make(map[string]Entry)
		}
		l.children[d][path.Base(p)] = e
	}
}

// foldNames makes the index of a mutable upper find its entries by the
// form fold gives their names, see [FS.claimName].
func (l *Layer) foldNames(fold func(string) string) {
	l.fold = fold
	l.folded = make(map[string]string, len(l.entries))
	for p, e := range l.entries {
		if p != "" && e.Kind != KindWhiteout {
			l.folded[foldedPath(parent(p), fold(path.Base(p)))] = p
		}
	}
}

// drop removes a mutable upper subtree from both indexes.
func (l *Layer) drop(p string) {
	for n := range l.children[p] {
		l.drop(path.Join(p, n))
	}
	delete(l.children, p)
	delete(l.entries, p)
	delete(l.gits, p)
	delete(l.redirects, p)
	if l.fold != nil && p != "" {
		if k := foldedPath(parent(p), l.fold(path.Base(p))); l.folded[k] == p {
			delete(l.folded, k)
		}
	}
	if p != "" {
		delete(l.children[parent(p)], path.Base(p))
	}
}

// barrier reports whether this layer prevents lookup in all deeper layers:
// an ancestor of p is opaque or no directory. Above the nearest redirected
// ancestor none counts, as the deeper layers hold p elsewhere.
func (l *Layer) barrier(p string) bool {
	l = l.view()
	for q := parent(p); ; q = parent(q) {
		if e, ok := l.entries[q]; ok {
			if e.Kind != KindDir || e.Opaque {
				return true
			}
			if e.Redirect != "" {
				return false
			}
		}
		if q == "" {
			return false
		}
	}
}

// redirected translates path p of l's view into the view of the levels below
// l: below the nearest of p and its ancestors with an [Entry.Redirect], they
// hold p at that origin. l is a view.
func (l *Layer) redirected(p string) string {
	if len(l.redirects) == 0 {
		return p
	}
	for d := p; d != ""; d = parent(d) {
		if origin, ok := l.redirects[d]; ok {
			return origin + p[len(d):]
		}
	}
	return p
}

// Unredirected translates path p of the levels below l into l's view, the
// inverse of [Layer.redirected]: the redirect whose origin holds p most
// closely names where l shows it. [FS.Locate] and the checkout's gitfile
// discovery use it to find where an entry of a deeper layer shows.
func (l *Layer) Unredirected(p string) string {
	l = l.view()
	at, origin := "", ""
	for d, o := range l.redirects {
		if (p == o || strings.HasPrefix(p, o+"/")) && (len(o) > len(origin) || len(o) == len(origin) && d < at) {
			at, origin = d, o
		}
	}
	if at == "" {
		return p
	}
	return at + p[len(origin):]
}

// readRedirect reads the origin a directory's redirect marker at host path real names.
func readRedirect(real string) (string, error) {
	data, err := os.ReadFile(real)
	if err != nil {
		return "", err
	}
	origin := string(data)
	if origin == "" || Clean(origin) != origin || reserved(origin) {
		return "", fmt.Errorf("overlay: redirect marker %s names %q", real, origin)
	}
	return origin, nil
}

// view returns the layer holding the index: l itself, or the index a restored
// layer decodes again from its metadata after [Layer.Cool] dropped it. Index
// readers of a frozen layer use it; the index never changes once decoded.
func (l *Layer) view() *Layer {
	if l.entries != nil { // indexed directly; only a restored layer leaves entries nil
		return l
	}
	if !l.used.Load() {
		l.used.Store(true)
	}
	if v := l.resident.Load(); v != nil {
		return v
	}
	l.loading.Lock()
	defer l.loading.Unlock()
	if v := l.resident.Load(); v != nil {
		return v
	}
	v, err := l.decode()
	if err != nil {
		panic(err) // RestoreLayer decoded the same immutable metadata
	}
	l.resident.Store(v)
	return v
}

// Cool drops a restored layer's index after checks calls without use in
// between. One goroutine calls it; readers keep the index they hold.
func (l *Layer) Cool(checks int) {
	if l.used.Swap(false) {
		l.idle = 0
	} else if l.idle++; l.idle >= checks {
		l.resident.Store(nil)
	}
}
