package overlay

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/stencil-hq/ovnfs/internal/ident"
)

// resolveLower finds whether clean path p is served by a frozen layer or
// falls through to the bottom, without touching the bottom under f.mu.RLock.
// ok is false when p is reserved, the root, held or hidden by the upper, or
// not a regular file in a frozen layer.
func (f *FS) resolveLower(p string) (layer *Layer, q string, bottom Bottom, ok bool) {
	if p == "" || reserved(p) {
		return nil, "", nil, false
	}
	q = p
	if f.index != nil {
		if _, present := f.index.entries[p]; present || f.index.barrier(p) {
			return nil, "", nil, false
		}
		q = f.index.redirected(p)
	} else if f.upper != "" {
		d, err := f.cold.dir(f.upper, "")
		if err != nil {
			return nil, "", nil, false
		}
		blocked := d.opaque
		at, origin := "", ""
		start := 0
		for {
			stop := len(p)
			if i := strings.IndexByte(p[start:], '/'); i >= 0 {
				stop = start + i
			}
			child, name := p[:stop], p[start:stop]
			has, isDir, err := d.has(f, child, name)
			if err != nil {
				return nil, "", nil, false
			}
			if !has {
				if gone, err := d.whiteout(f, child, name); err != nil || gone || blocked {
					return nil, "", nil, false
				}
				if at != "" {
					q = origin + p[len(at):]
				}
				break
			}
			if stop == len(p) || !isDir {
				return nil, "", nil, false
			}
			if d, err = f.cold.dir(f.upper, child); err != nil {
				return nil, "", nil, false
			}
			blocked = blocked || d.opaque
			if d.redirect != "" {
				at, origin, blocked = child, d.redirect, false
			}
			start = stop + 1
		}
	}
	for _, l := range f.layers {
		v := l.view()
		if e, present := v.entries[q]; present {
			if e.Kind == KindWhiteout || !e.Info.Mode().IsRegular() {
				return nil, "", nil, false
			}
			return l, q, nil, true
		}
		if v.barrier(q) {
			return nil, "", nil, false
		}
		q = v.redirected(q)
	}
	if f.bottom == nil {
		return nil, "", nil, false
	}
	return nil, q, f.bottom, true
}

// lookupRead checks only the requested path and ancestor barriers while cold.
// A native stat follows intermediate symlinks, so inspect the ancestors first.
// The upper directories it passes are listed once and kept (coldDirs), so
// a cold lookup stats nothing in the upper unless the upper holds the name.
func (f *FS) lookupRead(p string) (source, os.FileInfo, error) {
	if f.index != nil || f.upper == "" || p == "" || reserved(p) {
		return f.lookup(p)
	}
	d, err := f.cold.dir(f.upper, "")
	if err != nil {
		return source{}, nil, err
	}
	blocked := d.opaque
	// at is the nearest upper directory passed that redirects (see
	// [Entry.Redirect]) and origin its origin, for the levels below.
	at, origin := "", ""
	// Each round looks up the component p[start:stop] in the upper
	// directory p[:start-1] (the root when start is 0), which d describes.
	start := 0
	for {
		stop := len(p)
		if i := strings.IndexByte(p[start:], '/'); i >= 0 {
			stop = start + i
		}
		child, name := p[:stop], p[start:stop]
		has, isDir, err := d.has(f, child, name)
		if err != nil {
			return source{}, nil, err
		}
		if !has {
			// Descendants of a missing upper directory are missing too.
			if gone, err := d.whiteout(f, child, name); err != nil {
				return source{}, nil, err
			} else if gone || blocked {
				return source{}, nil, notExist("lstat", p)
			}
			if at == "" {
				return f.lookupBelow(p, p)
			}
			return f.lookupBelow(p, origin+p[len(at):])
		}
		if stop == len(p) {
			real := f.up(p)
			info, err := os.Lstat(real) // only the final component needs its stored mode
			if err != nil {
				return source{}, nil, err
			}
			info, err = describe(real, info, f.ids)
			return source{upper: true, path: p}, info, err
		}
		if !isDir {
			return source{}, nil, notExist("lstat", p)
		}
		if d, err = f.cold.dir(f.upper, child); err != nil {
			return source{}, nil, err
		}
		blocked = blocked || d.opaque
		if d.redirect != "" {
			// The levels below hold what is beneath child elsewhere, past
			// any opaque directory above it.
			at, origin, blocked = child, d.redirect, false
		}
		start = stop + 1
	}
}

// Bounds of what coldDirs keeps: a directory with more entries than
// coldDirNames is not kept listed, nor any once coldNames names are kept;
// past coldDirsMax directories, new ones are listed for one lookup only.
const (
	coldDirNames = 4096
	coldNames    = 1 << 16
	coldDirsMax  = 8192
)

// coldDirs keeps, while the upper's index is cold, the upper directories
// lookupRead passed: whether each is opaque and the names it holds. The
// upper changes only through mutations, which load the index first, and
// loading or cooling the index resets it.
type coldDirs struct {
	mu    sync.Mutex
	dirs  map[string]*coldDir
	names int
}

// coldDir is one upper directory as a cold lookup listed it.
type coldDir struct {
	opaque   bool
	redirect string // the directory's [Entry.Redirect]
	// entries maps each name to whether it is a directory; nil when the
	// directory was too large to keep, and the host is asked instead.
	entries map[string]bool
}

// reset forgets every directory.
func (c *coldDirs) reset() {
	c.mu.Lock()
	c.dirs, c.names = nil, 0
	c.mu.Unlock()
}

// dir returns upper directory q, which must exist, listing it on first use.
func (c *coldDirs) dir(upper, q string) (*coldDir, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if d := c.dirs[q]; d != nil {
		return d, nil
	}
	real := filepath.Join(upper, q)
	d := &coldDir{}
	keep := len(c.dirs) < coldDirsMax
	if keep && c.names < coldNames {
		fh, err := os.Open(real)
		if err != nil {
			return nil, err
		}
		list, err := fh.ReadDir(coldDirNames + 1)
		fh.Close()
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		if len(list) <= coldDirNames && c.names+len(list) <= coldNames {
			d.entries = make(map[string]bool, len(list))
			for _, e := range list {
				d.entries[e.Name()] = e.IsDir()
			}
			_, d.opaque = d.entries[opaqueName]
			c.names += len(list)
		}
	}
	if d.entries == nil {
		if _, err := os.Lstat(filepath.Join(real, opaqueName)); err == nil {
			d.opaque = true
		} else if !missing(err) {
			return nil, err
		}
	}
	if _, listed := d.entries[redirectName]; listed || d.entries == nil {
		origin, err := readRedirect(filepath.Join(real, redirectName))
		switch {
		case err == nil:
			d.redirect = origin
		case !missing(err) || listed:
			return nil, err
		}
	}
	if keep {
		if c.dirs == nil {
			c.dirs = make(map[string]*coldDir)
		}
		c.dirs[q] = d
	}
	return d, nil
}

// has reports whether the directory holds name, upper path p, and whether
// that is a directory.
func (d *coldDir) has(f *FS, p, name string) (has, isDir bool, err error) {
	if d.entries != nil {
		isDir, has = d.entries[name]
		return has, isDir, nil
	}
	info, err := os.Lstat(f.up(p))
	if err == nil {
		return true, info.IsDir(), nil
	}
	if missing(err) {
		err = nil
	}
	return false, false, err
}

// whiteout reports whether the directory holds a deletion marker for name,
// upper path p, which it does not hold.
func (d *coldDir) whiteout(f *FS, p, name string) (bool, error) {
	if d.entries != nil {
		_, ok := d.entries[whPrefix+name]
		return ok, nil
	}
	_, err := os.Lstat(f.whiteoutPath(p))
	if err == nil {
		return true, nil
	}
	if missing(err) {
		err = nil
	}
	return false, err
}

// loadIndex is called under mu exclusively, never for a cold directory listing.
func (f *FS) loadIndex() error {
	if f.upper == "" {
		return nil
	}
	f.lastAccess.Store(time.Now().UnixNano())
	if f.index != nil {
		return nil
	}
	f.cold.reset()
	index, err := walkLayer(f.upper, true, true, "", f.ids)
	if err != nil {
		return err
	}
	// The data wins over a marker beside it; dropping the marker keeps the
	// index the record of every marker ([FS.mayHoldMarker]).
	for _, p := range index.shadowed {
		if err := removeMarker(f.whiteoutPath(p)); err != nil {
			return err
		}
	}
	index.shadowed = nil
	if !f.ids.table.empty() {
		live := make(map[uint64]bool, len(index.entries))
		for _, e := range index.entries {
			if e.Info == nil {
				continue // a whiteout names no file
			}
			if key, ok := hostKey(e.Info); ok {
				live[key.inode] = true
			}
		}
		if err := f.ids.table.retain(live); err != nil {
			return err
		}
	}
	index.addOrigins(f.origins)
	if f.volume.folds() {
		index.foldNames(f.volume.key)
	}
	f.index = index
	f.pathIDs = make(map[string]inodeKey)
	f.aliases = make(map[inodeKey]inodeNames)
	f.paths = make(map[ident.ID]string, len(index.entries))
	for p, e := range index.entries {
		f.track(p, e.Info)
	}
	for h := range f.hosts {
		if h.cold != "" {
			f.bind(h, h.cold) // nothing renames upper files while the index is cold
			h.cold = ""
		}
	}
	f.origins = nil
	return nil
}

// openUpperCold opens an upper regular file for reading without loading the
// index, which only mutations and writable descriptors need. ok is false when
// the full path must decide. The caller holds mu.
func (f *FS) openUpperCold(p string, flag int) (billy.File, bool, error) {
	s, info, err := f.lookupRead(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, true, err
	}
	if err != nil || !s.upper || !info.Mode().IsRegular() {
		return nil, false, nil
	}
	h, err := open(f.up(p), p, flag, 0, f)
	if err != nil {
		return nil, true, err
	}
	h.cold, h.id = p, handleID(info)
	return h, true, nil
}

// Cool drops reconstructible metadata only. Handles and staged or pending writes
// pin it; no file, checkout generation or exported identity is changed.
func (f *FS) Cool(before time.Time) {
	if !f.staging.TryLock() {
		return
	}
	defer f.staging.Unlock()
	if !f.mu.TryLock() {
		return
	}
	defer f.mu.Unlock()
	if f.index == nil || f.upper == "" || f.lastAccess.Load() >= before.UnixNano() ||
		len(f.hosts) != 0 || len(f.files) != 0 || f.memoryHandles != 0 || len(f.ops) != 0 {
		return
	}
	// A cold lookup reads directory times from the host.
	if f.settleDirs(f.upper) != nil {
		return
	}
	f.rootInfo, f.origins = f.index.entries[""].Info, f.index.origins()
	f.index, f.pathIDs, f.aliases, f.paths = nil, nil, nil, nil
	f.ids.known.reset()
	f.cold.reset()
}
