package overlay

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"syscall"
	"time"

	"github.com/stencil-hq/ovnfs/internal/ident"
)

// Directory times with the memory tier. A name change in an upper directory
// moves its modification time, and copy-up, spill and deletion markers must
// not: clients cache a directory's listing until its time changes. Without
// the memory tier the host keeps the times: each change stats the directory
// afterwards, and each invisible one restores the time it read first. With
// it, while the index is loaded, the index holds every upper directory's
// time and the host's may differ: a name change stamps the index alone
// ([FS.touchDir]), an invisible host change leaves it alone
// ([FS.preserveDirTime]), and neither makes a syscall. A directory whose host
// time differs is unsettled; [FS.settleDirs] writes the index's times to the
// host before anything reads the upper from disk: at [FS.Flush], [FS.Freeze]
// and [FS.Cool], and [FS.SyncFile] settles the directories it syncs. A crash
// leaves a time the host chose, which clients see as one more change.

// lazyDirs reports whether f keeps its directories' times in the index (see
// above): only with the memory tier, whose contract it extends.
func (f *FS) lazyDirs() bool { return f.mem != nil && f.index != nil }

// touchDir stamps directory d's modification and change times as a host
// create, unlink or rename in it would, for a name change made only in the
// index (a memory file's) or already made on the host. Where index entries
// can carry their own times this makes no syscall; elsewhere the host's
// clock stamps the directory and the index reads it back.
func (f *FS) touchDir(d string, onHost bool) error {
	if f.lazyDirs() {
		if e, ok := f.index.entries[d]; ok && e.Kind == KindDir {
			now := time.Now()
			if info, ok := retimed(e.Info, now, now); ok {
				e.Info = info
				f.index.set(d, e)
				f.unsettled[d] = struct{}{}
				return nil
			}
		}
	}
	if !onHost {
		if err := os.Chtimes(f.up(d), time.Time{}, time.Now()); err != nil {
			return err
		}
	}
	return f.refreshDir(d)
}

// preserveDirTime runs fn, a host change in directory d that clients must
// not see as one (a copy-up, a spill, a deletion marker), keeping d's time.
func (f *FS) preserveDirTime(d string, fn func() error) error {
	if f.lazyDirs() {
		if _, ok := f.index.entries[d]; ok {
			err := fn()
			f.unsettled[d] = struct{}{}
			return err
		}
	}
	info, err := os.Lstat(f.up(d))
	if err != nil {
		return err
	}
	opErr := fn()
	timeErr := os.Chtimes(f.up(d), info.ModTime(), info.ModTime())
	refreshErr := f.refresh(d)
	if opErr != nil {
		return opErr
	}
	if timeErr != nil {
		return timeErr
	}
	return refreshErr
}

// refreshDir indexes directory d's host metadata after a visible change of
// its names on the host. An unsettled d keeps the index's later time: the
// host's clock may lag it (Linux stamps files with a coarse clock).
func (f *FS) refreshDir(d string) error {
	info, err := f.stat(d)
	if err != nil {
		return err
	}
	if _, ok := f.unsettled[d]; ok {
		if logical := f.index.entries[d].Info; logical != nil && info.ModTime().Before(logical.ModTime()) {
			if timed, ok := retimed(info, logical.ModTime(), time.Time{}); ok {
				info = timed
			} else {
				delete(f.unsettled, d)
			}
		} else {
			delete(f.unsettled, d)
		}
	}
	f.cacheInfo(d, info)
	return nil
}

// refreshUnsettled indexes unsettled directory d's host metadata after a
// change the index's time hides from clients: d keeps its indexed time.
// Where entries cannot carry their own times the host takes it now.
func (f *FS) refreshUnsettled(d string) error {
	info, err := f.stat(d)
	if err != nil {
		return err
	}
	logical := f.index.entries[d].Info
	if logical == nil {
		delete(f.unsettled, d)
		f.cacheInfo(d, info)
		return nil
	}
	if timed, ok := retimed(info, logical.ModTime(), time.Time{}); ok {
		f.cacheInfo(d, timed)
		return nil
	}
	if err := f.settleDir(d, f.up(d)); err != nil {
		return upperError(err)
	}
	if info, err = f.stat(d); err != nil {
		return err
	}
	f.cacheInfo(d, info)
	return nil
}

// settleDir writes directory d's indexed time to the host directory real
// and forgets that it was unsettled.
func (f *FS) settleDir(d, real string) error {
	e, ok := f.index.entries[d]
	if !ok || e.Info == nil {
		delete(f.unsettled, d)
		return nil
	}
	if err := os.Chtimes(real, time.Time{}, e.Info.ModTime()); err != nil {
		return err
	}
	delete(f.unsettled, d)
	return nil
}

// settleDirs settles every unsettled directory of the upper rooted at root.
// The caller holds f.mu exclusively.
func (f *FS) settleDirs(root string) error {
	for len(f.unsettled) != 0 {
		for d := range f.unsettled {
			err := f.settleDir(d, filepath.Join(root, d))
			if errors.Is(err, fs.ErrNotExist) {
				err = f.reconcileDir(root, d, err)
			}
			if err != nil {
				return upperError(err)
			}
		}
	}
	return nil
}

// reconcileDir forgets a vanished upper-only directory tree if it contains
// no data or markers. Check from the highest missing ancestor: a child whose
// time is pending may be the only sign that the entire subtree disappeared.
func (f *FS) reconcileDir(root, d string, cause error) error {
	lost := d
	for q := d; ; q = parent(q) {
		info, err := os.Lstat(filepath.Join(root, q))
		switch {
		case q != "" && errors.Is(err, fs.ErrNotExist):
			lost = q
		case err != nil:
			return err // includes a missing upper root, which must not be dropped
		case q == d || !info.IsDir():
			return cause // reappeared, or the path was replaced (e.g. by a symlink)
		}
		if q == "" {
			break
		}
	}
	real := filepath.Join(root, lost)
	if p := f.lostDirData(lost); p != "" {
		return fmt.Errorf("overlay: upper directory %s disappeared with indexed data or markers at %s: %w", real, p, syscall.EIO)
	}
	if !f.index.barrier(lost) {
		// A copy-up may hold intentional mode, owner or time changes. Do
		// not fall through to the lower entry after losing that metadata.
		_, _, err := f.lookupBelow(lost, f.lowerPath(lost))
		if err == nil {
			return fmt.Errorf("overlay: upper directory %s disappeared over a lower entry: %w", real, syscall.EIO)
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}

	// The surviving parent's listing changed. Refresh its host metadata
	// and stamp a visible change even if a pending mtime hid the deletion
	// or the parent was already settled during this pass.
	par := parent(lost)
	info, err := statHost(filepath.Join(root, par), f.ids)
	if err != nil {
		return err
	}
	if e := f.index.entries[par]; e.Kind != KindDir || !info.IsDir() || !sameEntry(e.Info, info) {
		return cause
	}
	now := time.Now()
	if timed, ok := retimed(info, now, now); ok {
		info = timed
		f.unsettled[par] = struct{}{}
	} else {
		delete(f.unsettled, par)
	}
	f.dropIndex(lost)
	f.cacheInfo(par, info)
	f.names.Add(1)
	return nil
}

// lostDirData returns the first path unsafe to forget, or "" if the entire
// non-root subtree consists of unmarked directories.
func (f *FS) lostDirData(d string) string {
	e := f.index.entries[d]
	if e.Kind != KindDir || e.Info == nil || e.Opaque || e.Redirect != "" || f.files[d] != nil {
		return d
	}
	for n := range f.index.children[d] {
		if p := f.lostDirData(path.Join(d, n)); p != "" {
			return p
		}
	}
	return ""
}

// upperError changes missing internal paths to I/O errors without masking
// other host failures. Such paths do not mean the user's name was missing.
func upperError(err error) error {
	if missing(err) {
		return fmt.Errorf("overlay: upper filesystem: %v: %w", err, syscall.EIO)
	}
	return err
}

// settleAncestors writes the indexed times of p's unsettled ancestors to the
// host without forgetting them, for [FS.SyncFile], which holds f.mu shared:
// a later settle writes the same times again.
func (f *FS) settleAncestors(p string) error {
	if len(f.unsettled) == 0 {
		return nil
	}
	for d := parent(p); ; d = parent(d) {
		if _, ok := f.unsettled[d]; ok {
			if err := os.Chtimes(f.up(d), time.Time{}, f.index.entries[d].Info.ModTime()); err != nil {
				return upperError(err)
			}
		}
		if d == "" {
			return nil
		}
	}
}

// moveUnsettled carries the unsettled state of directory a to b, where a
// rename moved it on the host.
func (f *FS) moveUnsettled(a, b string) {
	if _, ok := f.unsettled[a]; ok {
		delete(f.unsettled, a)
		f.unsettled[b] = struct{}{}
	}
}

// timedInfo is a directory's host metadata with the times the index keeps
// for it, in its native stat record too (see [retimed]).
type timedInfo struct {
	os.FileInfo
	mtime time.Time
	sys   any
}

// ModTime returns the indexed modification time.
func (i timedInfo) ModTime() time.Time { return i.mtime }

// Sys returns the native stat record with the indexed modification and change times.
func (i timedInfo) Sys() any { return i.sys }

// Identity forwards the identity of the directory, which embedding hides.
func (i timedInfo) Identity() ident.ID { id, _ := ident.Of(i.FileInfo); return id }

// untimed returns the host metadata beneath a [timedInfo].
func untimed(info os.FileInfo) os.FileInfo {
	if t, ok := info.(timedInfo); ok {
		return t.FileInfo
	}
	return info
}
