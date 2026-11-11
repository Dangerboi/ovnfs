package export

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/stencil-hq/ovnfs/internal/durable"
)

// ownersName is the manifest a clean close leaves in the store once the index
// holds every issued owner and retired hash: the size of the durable identity
// log, the index instance and its pack, and each indexed directory's key,
// index size and, when packed, its offset in the pack.
const ownersName = "owners"

var ownersMagic = []byte("ovnfs owners 1\n")

// openColdStore adopts the index a clean close completed for the log of
// logSize bytes just replayed, or else detaches the entire previous store
// without visiting its files. The state lock excludes other users of
// coldParent. The discard directory is separate: even a cleaner still
// returning from I/O after close cannot reach the next instance's
// authoritative owners. Windows never renames an open index: readers belong
// to the previous, already closed export.
func (t *nodeTable) openColdStore(logSize int64) error {
	discard := t.coldParent + ".discard"
	if err := os.MkdirAll(discard, 0700); err != nil {
		return err
	}
	if !t.adoptColdStore(logSize, discard) {
		// Publish the detached tree with one rename. No empty container is
		// exposed to an older cleaner still returning from a filesystem call.
		old := filepath.Join(discard, "store-"+rand.Text())
		if err := os.Rename(t.coldParent, old); err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := os.MkdirAll(t.coldParent, 0700); err != nil {
			return err
		}
	}
	t.coldStop = make(chan struct{})
	go func() {
		// Errors leave garbage for the next open; they cannot invalidate
		// any live owner or turn a cache cleanup failure into a serving fault.
		reclaimCold(discard, t.coldStop, t.reclaimIO)
	}()
	return nil
}

// indexedDir is a directory's entry in the owners manifest.
type indexedDir struct {
	key, size uint64
	base      uint64 // one past its offset in the pack; zero for a file of its own
}

// adoptColdStore resumes the index named by a manifest that matches the log,
// marking each indexed directory cold with owners from before this open, and
// detaches every other entry of the store. The manifest goes first, durably,
// so a crash before the next clean close discards the index instead. It
// reports false, changing no directory, when there is nothing to adopt.
func (t *nodeTable) adoptColdStore(logSize int64, discard string) bool {
	manifest := filepath.Join(t.coldParent, ownersName)
	data, err := os.ReadFile(manifest)
	if err != nil {
		return false
	}
	if os.Remove(manifest) != nil || durable.Dir(t.coldParent) != nil {
		return false
	}
	head, dirs, ok := parseOwners(data)
	if !ok || head.logSize != uint64(logSize) {
		return false
	}
	for _, dir := range dirs {
		if t.byKey[dir.key] == nil {
			return false // not the log this index was completed for
		}
	}
	root := filepath.Join(t.coldParent, head.instance)
	if !t.detachExcept(t.coldParent, head.instance, discard) || !t.detachExcept(root, head.pack, discard) {
		return false
	}
	for _, dir := range dirs {
		s := t.byKey[dir.key].state()
		s.cold, s.prior, s.coldBytes = true, true, int64(dir.size)
		s.packed, s.base = dir.base != 0, int64(dir.base)-1
		t.coldBytes += int64(dir.size)
	}
	t.coldRoot = root
	if head.pack != "" {
		t.coldPack = filepath.Join(root, head.pack)
	}
	return true
}

// detachExcept moves into discard every entry of dir that is not keep, apart
// from the index shards, and reports whether keep exists. An empty keep
// exists trivially.
func (t *nodeTable) detachExcept(dir, keep, discard string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	found := keep == ""
	for _, e := range entries {
		if e.Name() == keep {
			found = true
			continue
		}
		if dir != t.coldParent && e.IsDir() && len(e.Name()) == 2 {
			continue // a shard of directory index files
		}
		if os.Rename(filepath.Join(dir, e.Name()), filepath.Join(discard, "store-"+rand.Text())) != nil {
			return false
		}
	}
	return found
}

// ownersHeader is what a manifest records besides its directories.
type ownersHeader struct {
	logSize  uint64 // of the durable identity log the index was completed for
	instance string // the index directory below the store
	pack     string // the pack file in instance, if any directory is packed
}

// parseOwners decodes a manifest written by saveColdStore.
func parseOwners(data []byte) (h ownersHeader, dirs []indexedDir, ok bool) {
	rest, ok := bytes.CutPrefix(data, ownersMagic)
	if !ok {
		return h, nil, false
	}
	next := func() (uint64, bool) {
		v, n := binary.Uvarint(rest)
		if n <= 0 {
			return 0, false
		}
		rest = rest[n:]
		return v, true
	}
	name := func() (string, bool) {
		length, ok := next()
		if !ok || length > uint64(len(rest)) {
			return "", false
		}
		s := string(rest[:length])
		rest = rest[length:]
		return s, true
	}
	var count uint64
	if h.logSize, ok = next(); !ok {
		return h, nil, false
	}
	if h.instance, ok = name(); !ok {
		return h, nil, false
	}
	if h.pack, ok = name(); !ok {
		return h, nil, false
	}
	if count, ok = next(); !ok || count > uint64(len(rest)) {
		return h, nil, false
	}
	dirs = make([]indexedDir, 0, count)
	for range count {
		var dir indexedDir
		var okKey, okSize, okBase bool
		dir.key, okKey = next()
		dir.size, okSize = next()
		dir.base, okBase = next()
		if !okKey || !okSize || !okBase || dir.base != 0 && h.pack == "" {
			return h, nil, false
		}
		dirs = append(dirs, dir)
	}
	return h, dirs, len(rest) == 0
}

// completeIndex writes every resident owner and retired hash the index lacks,
// at close. A directory with an index file of its own has it rewritten in
// place, in parallel; the others, with those the adopted pack holds, go into
// one new pack, so closing costs one file rather than one per directory. The
// caller holds mu throughout: nothing changes or reads the index meanwhile,
// and a crash before the manifest discards it all anyway. It reports whether
// the index now holds every owner.
func (t *nodeTable) completeIndex() bool {
	type job struct {
		d       *node
		from    coldIndex // the published index, if cold
		read    bool      // from holds evicted owners to merge with resident ones
		carry   bool      // from moves to the new pack unchanged
		names   map[uint32]string
		retired []uint32
		data    []byte // a rewritten file of the directory's own
		base    int64  // in the new pack
		size    int64
		err     error
	}
	if t.cacheErr != nil {
		return false
	}
	var jobs []*job
	for _, d := range t.byKey {
		s := d.dir
		if s == nil || s.cold && !s.coldDirty && !s.packed {
			continue
		}
		j := &job{d: d, read: s.cold && s.coldDirty && !s.coldLoaded, carry: s.cold && !s.coldDirty}
		if s.cold {
			j.from = t.coldIndexOf(d)
		}
		jobs = append(jobs, j)
	}
	inParallel(jobs, func(j *job) {
		if j.read {
			j.names, j.retired, j.err = readColdNames(j.from)
		}
	})
	var files, packs []*job
	for _, j := range jobs {
		s := j.d.dir
		if j.err != nil {
			if !s.prior {
				t.cacheFailure(j.err)
				return false
			}
			t.forgetPrior(j.d)
			j.names, j.retired = nil, nil
		}
		if !j.carry {
			if j.names == nil {
				j.names = make(map[uint32]string)
			}
			// Unchanged owners stay in place; a packed directory's move along.
			if j.carry = !t.ownersToWrite(j.d, j.names, j.retired); j.carry && !s.packed {
				continue
			}
		}
		if s.cold && !s.packed {
			j.data = encodeColdNames(j.names, s.retired)
			files = append(files, j)
		} else {
			packs = append(packs, j)
		}
	}
	inParallel(files, func(j *job) { j.err = os.WriteFile(j.from.file, j.data, 0600) })
	for _, j := range files {
		if j.err != nil {
			return false
		}
		s := j.d.dir
		t.coldBytes += int64(len(j.data)) - s.coldBytes
		s.coldBytes, s.coldDirty, s.prior = int64(len(j.data)), false, false
		s.coldLoaded = s.coldLoaded || !j.read
	}
	if !slices.ContainsFunc(packs, func(j *job) bool { return !j.carry }) {
		// The adopted pack already holds every packed directory, if any remain.
		if len(packs) == 0 {
			t.coldPack = ""
		}
		return true
	}
	var pack, old *os.File
	var w *bufio.Writer
	var buf []byte
	var size int64
	written := packs[:0]
	for _, j := range packs {
		s := j.d.dir
		var data []byte
		if j.carry {
			var err error
			if old == nil {
				old, err = os.Open(t.coldPack)
			}
			if err == nil {
				data = slices.Grow(buf[:0], int(j.from.size))[:j.from.size]
				buf = data
				_, err = old.ReadAt(data, j.from.base)
			}
			if err != nil {
				if !s.prior {
					t.cacheFailure(err)
					return false
				}
				t.forgetPrior(j.d)
				j.carry, j.names = false, make(map[uint32]string)
				if !t.ownersToWrite(j.d, j.names, nil) {
					continue
				}
			}
		}
		if !j.carry {
			data = encodeColdNames(j.names, s.retired)
		}
		if pack == nil {
			if t.coldRoot == "" {
				root, err := os.MkdirTemp(t.coldParent, "instance-")
				if err != nil {
					return false
				}
				t.coldRoot = root
			}
			var err error
			if pack, err = os.CreateTemp(t.coldRoot, "pack-"); err != nil {
				return false
			}
			defer pack.Close()
			w = bufio.NewWriterSize(pack, 1<<20)
		}
		if _, err := w.Write(data); err != nil {
			return false
		}
		j.base, j.size = size, int64(len(data))
		size += j.size
		written = append(written, j)
	}
	if old != nil {
		old.Close()
	}
	if pack != nil && (w.Flush() != nil || pack.Close() != nil) {
		return false
	}
	for _, j := range written {
		s := j.d.dir
		if !j.carry {
			s.coldLoaded = s.coldLoaded || !j.read
		}
		t.coldBytes += j.size - s.coldBytes
		s.coldBytes, s.cold, s.coldDirty, s.prior, s.packed, s.base = j.size, true, false, false, true, j.base
	}
	t.coldPack = ""
	if pack != nil {
		t.coldPack = pack.Name()
	}
	return true
}

// indexWorkers bounds the index files completeIndex reads or writes at once.
const indexWorkers = 16

// inParallel calls f on every item with at most indexWorkers goroutines.
func inParallel[T any](items []T, f func(T)) {
	var next atomic.Int64
	var wg sync.WaitGroup
	for range min(len(items), indexWorkers) {
		wg.Go(func() {
			for i := next.Add(1) - 1; i < int64(len(items)); i = next.Add(1) - 1 {
				f(items[i])
			}
		})
	}
	wg.Wait()
}

// saveColdStore writes the manifest for an index completeIndex completed and an
// identity log of logSize bytes made durable, then stops all index writes:
// a request still running after close must not change what the manifest
// describes. A failure leaves the index to be discarded by the next open.
func (t *nodeTable) saveColdStore(logSize int64) error {
	t.coldSaved = true
	if t.coldRoot == "" {
		return nil // nothing was ever indexed
	}
	b := binary.AppendUvarint(bytes.Clone(ownersMagic), uint64(logSize))
	pack := ""
	if t.coldPack != "" {
		pack = filepath.Base(t.coldPack)
	}
	for _, name := range []string{filepath.Base(t.coldRoot), pack} {
		b = binary.AppendUvarint(b, uint64(len(name)))
		b = append(b, name...)
	}
	var dirs []indexedDir
	for key, d := range t.byKey {
		if s := d.dir; s != nil && s.cold {
			dir := indexedDir{key: key, size: uint64(s.coldBytes)}
			if s.packed {
				dir.base = uint64(s.base) + 1
			}
			dirs = append(dirs, dir)
		}
	}
	b = binary.AppendUvarint(b, uint64(len(dirs)))
	for _, dir := range dirs {
		b = binary.AppendUvarint(binary.AppendUvarint(binary.AppendUvarint(b, dir.key), dir.size), dir.base)
	}
	f, err := os.CreateTemp(t.coldParent, ".owners-*")
	if err != nil {
		return err
	}
	if _, err = f.Write(b); err == nil {
		err = f.Close()
	} else {
		f.Close()
	}
	if err == nil {
		err = os.Rename(f.Name(), filepath.Join(t.coldParent, ownersName))
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
}

// reclaimCold removes detached stores in bounded batches. It leaves its root
// in place so an older worker cannot remove a newer opener's discard directory.
// A stop or crash leaves the remaining entries for the next open. Directory
// streams close before their directory is removed, including on Windows.
func reclaimCold(root string, stop <-chan struct{}, beforeRemove func()) error {
	for {
		select {
		case <-stop:
			return nil
		default:
		}
		f, err := os.Open(root)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		for {
			names, err := f.Readdirnames(128)
			if err != nil && err != io.EOF {
				f.Close()
				return err
			}
			removed := 0
			for _, name := range names {
				if beforeRemove != nil {
					beforeRemove()
				}
				select {
				case <-stop:
					f.Close()
					return nil
				default:
				}
				path := filepath.Join(root, name)
				info, err := os.Lstat(path)
				if os.IsNotExist(err) {
					removed++
					continue
				}
				if err != nil {
					continue // retry inaccessible garbage on a later open
				}
				if info.IsDir() {
					if err := reclaimCold(path, stop, beforeRemove); err != nil {
						continue
					}
				}
				// Never follow a symlink; removal failures remain garbage.
				if err := os.Remove(path); err == nil || os.IsNotExist(err) {
					removed++
				}
			}
			if len(names) < 128 {
				f.Close()
				return nil
			}
			if removed > 0 {
				// Deletion can reshuffle directory offsets. Reopen after
				// progress so the next batch cannot skip surviving entries.
				f.Close()
				break
			}
			// A full batch of failures must advance, not reopen and spin.
		}
	}
}
