package source

import (
	"io/fs"
	"os"
	"path"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
)

// A walk reads one directory at a time, and most of a warm capture's time is
// spent waiting for those reads. While the walk handles a directory, readers
// list its subdirectories ahead of it; the walk takes a listing a reader
// finished, waits for one in progress, or reads one no reader has started
// itself, so it never waits behind the queue.

// listing is a directory's entries sorted by name, with each entry's stat data
// or the error reading it.
type listing struct {
	entries []os.DirEntry
	infos   []os.FileInfo
	errs    []error
	keyOf   func(os.FileInfo) fileKey
	err     error
}

// readListing lists host directory dir and stats its entries.
func readListing(dir string) *listing {
	entries, keyOf, err := readDir(dir)
	l := &listing{entries: entries, keyOf: keyOf, err: err, infos: make([]os.FileInfo, len(entries)), errs: make([]error, len(entries))}
	for i, e := range entries {
		l.infos[i], l.errs[i] = e.Info()
	}
	return l
}

// ahead is one directory queued to be read ahead, read by whoever claims it first.
type ahead struct {
	rel     string
	claimed atomic.Bool
	done    chan struct{} // closed once a reader that claimed it has set l
	l       *listing
}

// readAhead holds the walk's queued directories.
type readAhead struct {
	queue   chan *ahead
	queued  map[string]*ahead   // by root-relative path; walk goroutine only
	open    map[string]*listing // directories the walk is in; walk goroutine only
	stopped atomic.Bool
	readers sync.WaitGroup
}

// startReaders starts reading directories ahead of the walk.
func (w *walker) startReaders() {
	w.ahead = readAhead{queue: make(chan *ahead, 4096), queued: map[string]*ahead{}, open: map[string]*listing{}}
	for range readers {
		w.ahead.readers.Go(func() {
			for a := range w.ahead.queue {
				if !w.ahead.stopped.Load() && a.claimed.CompareAndSwap(false, true) {
					a.l = readListing(w.abs(a.rel))
					close(a.done)
				}
			}
		})
	}
}

// stopReaders drops the directories still queued and waits for the readers.
func (w *walker) stopReaders() {
	w.ahead.stopped.Store(true)
	close(w.ahead.queue)
	w.ahead.readers.Wait()
}

// queueAhead queues root-relative directory rel; a full queue leaves it to the walk.
// Walk goroutine only.
func (w *walker) queueAhead(rel string) {
	a := &ahead{rel: rel, done: make(chan struct{})}
	select {
	case w.ahead.queue <- a:
		w.ahead.queued[rel] = a
	default:
	}
}

// listing returns root-relative directory rel's listing, read ahead or now.
// Walk goroutine only.
func (w *walker) listing(rel string) *listing {
	a := w.ahead.queued[rel]
	delete(w.ahead.queued, rel)
	if a == nil || a.claimed.CompareAndSwap(false, true) {
		return readListing(w.abs(rel))
	}
	<-a.done
	return a.l
}

// listed returns root-relative directory dir's listing when the walk is in it or
// a reader has finished it, else nil. Walk goroutine only.
func (w *walker) listed(dir string) *listing {
	l := w.ahead.open[dir]
	if a := w.ahead.queued[dir]; l == nil && a != nil {
		select {
		case <-a.done:
			l = a.l
		default:
		}
	}
	if l == nil || l.err != nil {
		return nil
	}
	return l
}

// find returns the index of the entry named name, as the host's lookup matches
// names (ignoring case on Windows and macOS), or -1.
func (l *listing) find(name string) int {
	if i, ok := slices.BinarySearchFunc(l.entries, name, func(e os.DirEntry, name string) int { return strings.Compare(e.Name(), name) }); ok {
		return i
	}
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return slices.IndexFunc(l.entries, func(e os.DirEntry) bool { return strings.EqualFold(e.Name(), name) })
	}
	return -1
}

// walkView is the folder as rule facts read it during the walk: names in a
// directory already listed are answered from the listing, without a system call.
type walkView struct{ w *walker }

// Lstat returns a root-relative path's stat data.
func (v walkView) Lstat(p string) (os.FileInfo, error) {
	if l := v.w.listed(parent(p)); l != nil {
		i := l.find(path.Base(p))
		if i < 0 {
			return nil, &fs.PathError{Op: "lstat", Path: p, Err: fs.ErrNotExist}
		}
		return l.infos[i], l.errs[i]
	}
	return Dir(v.w.o.Root).Lstat(p)
}

// ReadFile reads a root-relative file, failing at once for one its directory's
// listing lacks.
func (v walkView) ReadFile(p string) ([]byte, error) {
	if l := v.w.listed(parent(p)); l != nil && l.find(path.Base(p)) < 0 {
		return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist}
	}
	return Dir(v.w.o.Root).ReadFile(p)
}
