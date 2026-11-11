package export

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

// Cold identities retain their exact names in a disposable disk index. A
// listing alone cannot reconstruct an issued id after an external removal or a
// new name with the same hash. These files are not the identity log: they last
// only for this process, and a restart still uses ordinary derived resolution.
// Each file is a sorted array of 16-byte (hash, name length, offset) records,
// followed by name bytes. Recovery reads at most one directory at a time.
func (t *nodeTable) coldPath(d *node) string {
	return coldIndexPath(t.coldRoot, d.key)
}

// Low key bits distribute consecutive directory keys across 256 shards.
func coldIndexPath(root string, key uint64) string {
	return filepath.Join(root, fmt.Sprintf("%02x", key&255), strconv.FormatUint(key, 10))
}

// readColdName searches the sorted index without allocating its other owners.
// size is the last published index size, validated again by the caller after I/O.
func readColdName(file string, size int64, hash uint32) (string, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var row [16]byte
	if _, err := f.ReadAt(row[:4], 0); err != nil {
		return "", err
	}
	count := int64(binary.LittleEndian.Uint32(row[:4]))
	end := 4 + count*16
	if end > size {
		return "", io.ErrUnexpectedEOF
	}
	lo, hi := int64(0), count
	for lo < hi {
		mid := lo + (hi-lo)/2
		if _, err := f.ReadAt(row[:], 4+mid*16); err != nil {
			return "", err
		}
		h := binary.LittleEndian.Uint32(row[:4])
		if h < hash {
			lo = mid + 1
			continue
		}
		if h > hash {
			hi = mid
			continue
		}
		length, offset := uint64(binary.LittleEndian.Uint32(row[4:8])), binary.LittleEndian.Uint64(row[8:])
		if offset < uint64(end) || offset > uint64(size) || length > uint64(size)-offset {
			return "", io.ErrUnexpectedEOF
		}
		name := make([]byte, int(length))
		if _, err := f.ReadAt(name, int64(offset)); err != nil {
			return "", err
		}
		return string(name), nil
	}
	return "", nil
}

// readColdNames reads one directory's issued identities for a rewrite or subtree
// invalidation. The temporary allocation is at most one directory's identities.
func readColdNames(file string) (map[uint32]string, error) {
	names := make(map[uint32]string)
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}

	if len(data) < 4 {
		return nil, io.ErrUnexpectedEOF
	}
	n := binary.LittleEndian.Uint32(data[:4])
	if uint64(n)*16+4 > uint64(len(data)) {
		return nil, io.ErrUnexpectedEOF
	}
	for i := uint32(0); i < n; i++ {
		row := data[4+int(i)*16:]
		h, size, offset := binary.LittleEndian.Uint32(row[:4]), binary.LittleEndian.Uint32(row[4:8]), binary.LittleEndian.Uint64(row[8:16])
		if offset > uint64(len(data)) || uint64(size) > uint64(len(data))-offset {
			return nil, io.ErrUnexpectedEOF
		}
		names[h] = string(data[offset : offset+uint64(size)])
	}
	return names, nil
}

// spillNames preserves issued unlogged ownership before reclaiming its memory.
func (t *nodeTable) spillNames(d *node) (bool, error) {
	if d.dir.cold && !d.dir.coldDirty {
		return true, nil
	}
	names := make(map[uint32]string)
	var err error
	if d.dir.cold && !d.dir.coldLoaded {
		s := d.dir
		gen, location, file := s.gen, nodeLocation(d), t.coldPath(d)
		if s.indexing != nil {
			return false, nil
		}
		s.reading++
		t.mu.Unlock()
		if t.indexIO != nil {
			t.indexIO()
		}
		names, err = readColdNames(file)
		t.mu.Lock()
		s.reading--
		if t.byID[d.id] != d || s.gen != gen || !sameLocation(d, location) {
			return false, nil
		}
	}
	if err != nil {
		t.cacheFailure(err) // this index already owns evicted identities
		return false, err
	}
	changed := !d.dir.cold
	for h, name := range d.dir.leaves {
		changed = changed || names[h] != name
		names[h] = name
	}
	for name, k := range d.kids {
		if k.key == 0 {
			changed = changed || names[nameHash(name)] != name
			names[nameHash(name)] = name
		}
	}
	for h := range names {
		if _, gone := d.dir.retired[h]; gone || t.byID[derivedID(d.key, h)] != nil && t.byID[derivedID(d.key, h)].key != 0 {
			delete(names, h)
			changed = true
		}
	}
	if len(names) == 0 {
		d.dir.coldDirty = false
		return true, nil
	}
	if !changed {
		d.dir.coldDirty = false
		return true, nil
	}
	if t.coldRoot == "" {
		if t.coldInit {
			return false, nil
		}
		t.coldInit = true
		parent := t.coldParent
		d.dir.reading++
		t.mu.Unlock()
		root, err := os.MkdirTemp(parent, "instance-")
		t.mu.Lock()
		d.dir.reading--
		t.coldInit = false
		if err != nil {
			return false, err
		}
		t.coldRoot = root
		if t.byID[d.id] != d {
			return false, nil
		}
		// Ownership may have changed during directory creation. Start over.
		return t.spillNames(d)
	}
	s := d.dir
	if s.indexing != nil {
		return false, nil
	}
	gen, version, location := s.gen, s.version, nodeLocation(d)
	resident := !s.cold || s.coldLoaded
	target := t.coldPath(d)
	s.indexing = &coldOperation{done: make(chan struct{})}
	s.reading++
	t.mu.Unlock()
	// Only this operation owns the temporary file. Publish it after revalidation.
	if t.indexIO != nil {
		t.indexIO()
	}
	err = os.MkdirAll(filepath.Dir(target), 0700)
	var file string
	var size int64
	if err == nil {
		file, size, err = writeColdNames(filepath.Dir(target), names)
	}
	t.mu.Lock()
	s.reading--
	close(s.indexing.done)
	s.indexing = nil
	valid := t.byID[d.id] == d && s.gen == gen && s.version == version && sameLocation(d, location)
	if err != nil {
		return false, err
	}
	if !valid {
		// The uncommitted replacement never owns any handles.
		t.mu.Unlock()
		os.Remove(file)
		t.mu.Lock()
		return false, nil
	}
	// Renaming the temporary file may block too. Keep indexing set so readers
	// use resident owners, and leave coldDirty set if anything changes meanwhile.
	s.indexing = &coldOperation{done: make(chan struct{})}
	s.reading++
	t.mu.Unlock()
	err = os.Rename(file, target)
	if err != nil {
		os.Remove(file)
	}
	t.mu.Lock()
	s.reading--
	close(s.indexing.done)
	s.indexing = nil
	if err != nil {
		return false, err
	}
	t.coldBytes += size - s.coldBytes
	s.coldBytes, s.cold = size, true
	// Publishing the first index does not discard its resident owners. A
	// guard acquired during the write must still finish without recovery.
	s.coldLoaded = s.coldLoaded || resident
	valid = t.byID[d.id] == d && s.gen == gen && s.version == version && sameLocation(d, location)
	s.coldDirty = !valid
	return valid, nil
}

func writeColdNames(root string, names map[uint32]string) (file string, size int64, err error) {
	hashes := make([]uint32, 0, len(names))
	for h := range names {
		hashes = append(hashes, h)
	}
	slices.Sort(hashes)
	data := make([]byte, 4+16*len(hashes))
	binary.LittleEndian.PutUint32(data[:4], uint32(len(hashes)))
	for i, h := range hashes {
		row := data[4+16*i:]
		binary.LittleEndian.PutUint32(row[:4], h)
		binary.LittleEndian.PutUint32(row[4:8], uint32(len(names[h])))
		binary.LittleEndian.PutUint64(row[8:16], uint64(len(data)))
		data = append(data, names[h]...)
	}
	f, err := os.CreateTemp(root, ".names-*")
	if err != nil {
		return "", 0, err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Close()
	} else {
		f.Close()
	}
	if err != nil {
		os.Remove(f.Name())
		return "", 0, err
	}
	return f.Name(), int64(len(data)), nil
}

// cacheFailure fails closed only when an authoritative cold owner cannot be read.
// Spill write failures leave ownership resident and use a separate retry state.
func (t *nodeTable) cacheFailure(err error) {
	if t.cacheErr == nil {
		t.cacheErr = fmt.Errorf("node cache: %w: %v", syscall.EIO, err)
		t.fault.Store(&nodeCacheFault{t.cacheErr})
	}
}

type nodeCacheFault struct{ err error }

// IdentityError reports an unusable disposable index without taking the node
// mutex on each protocol request. Restart is required to clear a fault.
func (n *nodeFS) IdentityError() error {
	if fault := n.nodes.fault.Load(); fault != nil {
		return fault.err
	}
	return nil
}

// loadCold recovers a whole directory, retaining its state while I/O runs
// outside mu. Namespace changes discard the snapshot; callers restart their
// path traversal before using any pointers obtained before the unlock.
func (t *nodeTable) loadCold(d *node) bool {
	if t.cacheErr != nil {
		return false
	}
	s := d.dir
	if s == nil || !s.cold || s.coldLoaded {
		return true
	}
	names, valid := t.readOwners(d)
	if !valid {
		return false
	}
	for h, name := range names {
		if !t.takenExceptCold(d, h) {
			t.addLeaf(d, name)
		}
	}
	s.coldLoaded = true
	return true
}

// recoverPaths pins directory caches until the caller finishes its in-memory
// operation. It restarts from the root after each read, including ancestor
// moves, rather than allowing a stale pointer to publish a path identity.
func (t *nodeTable) recoverPaths(paths ...string) (func(), error) {
	if t.coldBytes == 0 {
		return func() {}, nil
	}
	return t.keepPaths(paths...)
}

func (t *nodeTable) keepPaths(paths ...string) (func(), error) {
	var kept []*dirState
	release := func() {
		for _, s := range kept {
			s.reading--
		}
	}
	for failures := 0; failures < 16; {
		restart := false
		for _, p := range paths {
			d := t.root
			for {
				if s := d.state(); s != nil {
					if !slices.Contains(kept, s) {
						s.reading++
						kept = append(kept, s)
					}
					if s.cold && !s.coldLoaded {
						if t.spillErr != nil {
							return release, syscall.ENOSPC
						}
						if !t.loadCold(d) {
							if t.cacheErr != nil {
								return release, t.cacheErr
							}
							failures++
						}
						restart = true
						break
					}
				}
				name, rest, _ := strings.Cut(p, "/")
				p = rest
				d = d.kids[name]
				if d == nil || name == "" {
					break
				}
			}
			if restart {
				break
			}
		}
		if !restart {
			return release, nil
		}
	}
	return release, syscall.EAGAIN
}

// readOwners keeps the authoritative index and operation state alive, without
// admitting the recovered names. Degraded lookups use this transient result so
// an unavailable spill volume cannot turn retained-handle reads into growth.
// coldOperation remains owned by its waiters after it leaves dirState.
// Read results are immutable and shared, so serialization alone never consumes
// a namespace retry or makes an issued owner appear absent.
type coldOperation struct {
	done     chan struct{}
	read     bool
	single   bool
	hash     uint32
	name     string
	gen      uint64
	location []nodePosition
	names    map[uint32]string
	err      error
}

func (t *nodeTable) readOwners(d *node) (map[uint32]string, bool) {
	s := d.dir
	for {
		if t.cacheErr != nil || t.byID[d.id] != d {
			return nil, false
		}
		if op := s.indexing; op != nil {
			t.mu.Unlock()
			if t.indexWait != nil {
				t.indexWait()
			}
			<-op.done
			t.mu.Lock()
			if !op.read || op.single {
				continue
			}
			return t.readResult(d, op)
		}
		op := &coldOperation{done: make(chan struct{}), read: true, gen: s.gen, location: nodeLocation(d)}
		s.indexing = op
		s.reading++
		file := t.coldPath(d)
		t.mu.Unlock()
		if t.indexIO != nil {
			t.indexIO()
		}
		names, err := readColdNames(file)
		t.mu.Lock()
		s.reading--
		op.names, op.err = names, err
		close(op.done)
		s.indexing = nil
		return t.readResult(d, op)
	}
}

func (t *nodeTable) readResult(d *node, op *coldOperation) (map[uint32]string, bool) {
	if t.cacheErr != nil || t.byID[d.id] != d || d.dir.gen != op.gen || !sameLocation(d, op.location) {
		return nil, false
	}
	if op.err != nil {
		t.cacheFailure(op.err)
		return nil, false
	}
	return op.names, true
}

// batchRecovery distinguishes a directory rewalk from sparse retained handles.
// Four consecutive different misses in one directory warrant a batch even
// under pressure. The history is constant-size and carries no owner pointers.
func (t *nodeTable) batchRecovery(d *node, hash uint32) bool {
	if t.recoveryKey != d.key || t.recoveryHash == hash {
		t.recoveryRun = 1
	} else if t.recoveryRun < 4 {
		t.recoveryRun++
	}
	t.recoveryKey, t.recoveryHash = d.key, hash
	return t.spillErr == nil && (t.recoveryRun >= 4 || d.dir.coldBytes <= (t.cacheLimit-t.cacheUsed)/4)
}

// readOwner shares an in-flight full read when available, but otherwise reads
// only the requested hash. Partial admission never marks the index fully loaded.
func (t *nodeTable) readOwner(d *node, hash uint32) (string, bool) {
	s := d.dir
	for {
		if t.cacheErr != nil || t.byID[d.id] != d {
			return "", false
		}
		if op := s.indexing; op != nil {
			t.mu.Unlock()
			if t.indexWait != nil {
				t.indexWait()
			}
			<-op.done
			t.mu.Lock()
			if !op.read {
				continue
			}
			if !op.single {
				names, valid := t.readResult(d, op)
				return names[hash], valid
			}
			if op.hash != hash {
				continue
			}
			_, valid := t.readResult(d, op)
			return op.name, valid
		}
		op := &coldOperation{done: make(chan struct{}), read: true, single: true, hash: hash, gen: s.gen, location: nodeLocation(d)}
		s.indexing = op
		s.reading++
		file, size := t.coldPath(d), s.coldBytes
		t.mu.Unlock()
		if t.indexIO != nil {
			t.indexIO()
		}
		name, err := readColdName(file, size, hash)
		t.mu.Lock()
		s.reading--
		op.name, op.err = name, err
		close(op.done)
		s.indexing = nil
		_, valid := t.readResult(d, op)
		return name, valid
	}
}

// issuedID finds an existing name without recovering unrelated cold owners.
// Healthy ID requests admit partial results and batch dense rewalks; transient
// callers leave admission and mutation preparation to their own operations.
func (t *nodeTable) issuedID(p string, admit bool) (uint64, bool) {
	dir, name := split(p)
	for {
		if n := t.find(p); n != nil {
			if n.parent != nil {
				t.touch(n.parent)
			}
			return n.id, true
		}
		if id, ok := t.leafID(p); ok {
			t.touch(t.find(dir))
			return id, true
		}
		d := t.find(dir)
		if d == nil || d.dir == nil || !d.dir.cold || d.dir.coldLoaded {
			return 0, false
		}
		h := nameHash(name)
		if admit && t.batchRecovery(d, h) {
			if !t.loadCold(d) && t.cacheErr != nil {
				return 0, false
			}
			continue
		}
		owner, valid := t.readOwner(d, h)
		if t.find(p) != nil {
			continue
		}
		if _, ok := t.leafID(p); ok {
			continue
		}
		if !valid {
			if t.cacheErr != nil {
				return 0, false
			}
			continue
		}
		if t.find(dir) != d {
			continue
		}
		if owner == name && !t.takenExceptCold(d, h) {
			if admit && t.spillErr == nil {
				t.addLeaf(d, owner)
			}
			return derivedID(d.key, h), true
		}
		return 0, false
	}
}

// Retired directory keys are never reused. Delete their indexes after any
// in-flight replacement has finished, outside the node mutex.
type coldGarbage struct {
	path string
	dir  *node
}

func (t *nodeTable) cleanCold() {
	for len(t.coldGarbage) > 0 {
		old := t.coldGarbage[0]
		t.coldGarbage[0] = coldGarbage{}
		t.coldGarbage = t.coldGarbage[1:]
		if len(t.coldGarbage) == 0 {
			t.coldGarbage = nil
		}
		s := old.dir.dir
		for s.indexing != nil {
			done := s.indexing.done
			t.mu.Unlock()
			<-done
			t.mu.Lock()
		}
		size := s.coldBytes
		t.mu.Unlock()
		err := os.Remove(old.path)
		t.mu.Lock()
		if err != nil && !os.IsNotExist(err) {
			t.coldGarbage = append(t.coldGarbage, old)
			return
		}
		t.coldBytes -= size
		s.coldBytes = 0
		s.cold = false
	}
}
