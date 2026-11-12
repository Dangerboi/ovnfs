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
// log, the owner index instance and its pack, and the logged node index whose
// slots locate each key's owner index.
const ownersName = "owners"

var ownersMagic = []byte("ovnfs owners 2\n")

// openColdStore adopts the owner index a clean close completed for the log
// of logSize bytes just replayed, returning the closed logged node index
// that locates its owners, or else detaches the entire previous store
// without visiting its files. The state lock excludes other users of
// coldParent. The discard directory is separate: even a cleaner still
// returning from I/O after close cannot reach the next instance's
// authoritative owners. Windows never renames an open index: readers belong
// to the previous, already closed export.
func (t *nodeTable) openColdStore(logSize int64) (*priorIndex, error) {
	discard := t.coldParent + ".discard"
	if err := os.MkdirAll(discard, 0700); err != nil {
		return nil, err
	}
	prior := t.adoptColdStore(logSize, discard)
	if prior == nil {
		// Publish the detached tree with one rename. No empty container is
		// exposed to an older cleaner still returning from a filesystem call.
		old := filepath.Join(discard, "store-"+rand.Text())
		if err := os.Rename(t.coldParent, old); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		if err := os.MkdirAll(t.coldParent, 0700); err != nil {
			return nil, err
		}
	}
	t.coldStop = make(chan struct{})
	go func() {
		// Errors leave garbage for the next open; they cannot invalidate
		// any live owner or turn a cache cleanup failure into a serving fault.
		reclaimCold(discard, t.coldStop, t.reclaimIO)
	}()
	return prior, nil
}

// adoptColdStore resumes the owner index instance named by a manifest that
// matches the log, and detaches every other entry of the store but the
// closed logged node index, which the next build reads and then detaches.
// The manifest goes first, durably, so a crash before the next clean close
// discards the index instead. It returns nil, changing nothing, when there
// is nothing to adopt.
func (t *nodeTable) adoptColdStore(logSize int64, discard string) *priorIndex {
	manifest := filepath.Join(t.coldParent, ownersName)
	data, err := os.ReadFile(manifest)
	if err != nil {
		return nil
	}
	if os.Remove(manifest) != nil || durable.Dir(t.coldParent) != nil {
		return nil
	}
	head, ok := parseOwners(data)
	if !ok || head.logSize != uint64(logSize) {
		return nil
	}
	root := filepath.Join(t.coldParent, head.instance)
	if !t.detachExcept(t.coldParent, discard, head.instance, head.nodes) || !t.detachExcept(root, discard, head.pack) {
		return nil
	}
	t.coldRoot = root
	prior := &priorIndex{nodes: filepath.Join(t.coldParent, head.nodes)}
	if head.pack != "" {
		prior.pack = filepath.Join(root, head.pack)
	}
	return prior
}

// detachExcept moves into discard every entry of dir that is not kept, apart
// from the index shards, and reports whether every kept name exists. An
// empty name exists trivially.
func (t *nodeTable) detachExcept(dir, discard string, keep ...string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	found := 0
	for _, name := range keep {
		if name == "" {
			found++
		}
	}
	for _, e := range entries {
		if slices.Contains(keep, e.Name()) {
			found++
			continue
		}
		if dir != t.coldParent && e.IsDir() && len(e.Name()) == 2 {
			continue // a shard of directory index files
		}
		if os.Rename(filepath.Join(dir, e.Name()), filepath.Join(discard, "store-"+rand.Text())) != nil {
			return false
		}
	}
	return found == len(keep)
}

// ownersHeader is what a manifest records.
type ownersHeader struct {
	logSize  uint64 // of the durable identity log the index was completed for
	instance string // the owner index directory below the store
	pack     string // the pack file in instance, if any directory is packed
	nodes    string // the logged node index below the store
}

// parseOwners decodes a manifest written by saveColdStore.
func parseOwners(data []byte) (h ownersHeader, ok bool) {
	rest, ok := bytes.CutPrefix(data, ownersMagic)
	if !ok {
		return h, false
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
	if h.logSize, ok = next(); !ok {
		return h, false
	}
	for _, field := range []*string{&h.instance, &h.pack, &h.nodes} {
		if *field, ok = name(); !ok {
			return h, false
		}
	}
	return h, len(rest) == 0 && h.nodes != ""
}

// completeIndex writes every resident owner and retired hash the index lacks,
// at close. A directory with an index file of its own has it rewritten in
// place, in parallel; the others are appended to the pack, so closing costs
// one file rather than one per directory. The caller holds mu throughout:
// nothing changes or reads the index meanwhile, and a crash before the
// manifest discards it all anyway. It reports whether the index now holds
// every owner.
func (t *nodeTable) completeIndex() bool {
	type job struct {
		d       *node
		from    coldIndex // the published index, if cold
		read    bool      // from holds evicted owners to merge with resident ones
		names   map[uint32]string
		retired []uint32
		data    []byte // a rewritten file of the directory's own
		base    int64  // in the pack
		size    int64
		err     error
	}
	if t.cacheErr != nil {
		return false
	}
	var jobs []*job
	for _, d := range t.byKey {
		s := d.dir
		if s == nil || s.cold && !s.coldDirty {
			continue
		}
		j := &job{d: d, read: s.cold && !s.coldLoaded}
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
		if j.names == nil {
			j.names = make(map[uint32]string)
		}
		if !t.ownersToWrite(j.d, j.names, j.retired) {
			continue // unchanged owners stay in place
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
	if len(packs) == 0 {
		return true
	}
	var pack *os.File
	var size int64
	var err error
	if t.coldPack != "" {
		if pack, err = os.OpenFile(t.coldPack, os.O_WRONLY, 0); err == nil {
			size, err = pack.Seek(0, io.SeekEnd)
		}
	} else {
		if t.coldRoot == "" {
			root, err := os.MkdirTemp(t.coldParent, "instance-")
			if err != nil {
				return false
			}
			t.coldRoot = root
		}
		pack, err = os.CreateTemp(t.coldRoot, "pack-")
	}
	if err != nil {
		if pack != nil {
			pack.Close()
		}
		return false
	}
	defer pack.Close()
	w := bufio.NewWriterSize(pack, 1<<20)
	for _, j := range packs {
		data := encodeColdNames(j.names, j.d.dir.retired)
		if _, err := w.Write(data); err != nil {
			return false
		}
		j.base, j.size = size, int64(len(data))
		size += j.size
	}
	if w.Flush() != nil || pack.Close() != nil {
		return false
	}
	t.coldPack = pack.Name()
	for _, j := range packs {
		s := j.d.dir
		s.coldLoaded = s.coldLoaded || !j.read
		t.coldBytes += j.size - s.coldBytes
		s.coldBytes, s.cold, s.coldDirty, s.prior, s.packed, s.base = j.size, true, false, false, true, j.base
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

// saveOwners writes into the logged node index the owner index location of
// every resident logged node whose slot differs, at close, so the index
// locates every key's owners for the next open.
func (t *nodeTable) saveOwners() error {
	for key, d := range t.byKey {
		if s := d.dir; s != nil {
			if owner := ownerOf(s); owner != s.loaded {
				if err := t.index.publish(key, nil, owner); err != nil {
					return err
				}
				s.loaded = owner
			}
		}
	}
	return nil
}

// saveColdStore writes the manifest for an index completeIndex and saveOwners
// completed and an identity log of logSize bytes made durable, then stops all
// index writes: a request still running after close must not change what the
// manifest describes. A failure leaves the index to be discarded by the next
// open.
func (t *nodeTable) saveColdStore(logSize int64) error {
	t.coldSaved = true
	if t.coldRoot == "" || t.index == nil {
		return nil // nothing was ever indexed
	}
	b := binary.AppendUvarint(bytes.Clone(ownersMagic), uint64(logSize))
	pack := ""
	if t.coldPack != "" {
		pack = filepath.Base(t.coldPack)
	}
	for _, name := range []string{filepath.Base(t.coldRoot), pack, filepath.Base(t.index.dir)} {
		b = binary.AppendUvarint(b, uint64(len(name)))
		b = append(b, name...)
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
