package overlay

import (
	"container/list"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/stencil-hq/ovnfs/internal/hostfile"
	"github.com/stencil-hq/ovnfs/internal/ident"
)

// fileOverhead is a memory file's charge beyond its data, so that many empty
// files still count against the budget.
const fileOverhead = 256

// smallFileMax is the data capacity a file may claim by evicting peers;
// larger allocations use spare budget only. A cargo build's median rlib was
// about 130 KB, and spilling at 64 KiB slowed a cold scripts/buildbench replay
// by 16%. Keep room for those artifacts without letting large outputs crowd
// them out or making a small writer pay a large output's spill cost.
const smallFileMax = 256 << 10

// victimScan bounds how many cold files one admission inspects for spilling.
const victimScan = 64

// memModeBits are the mode bits a memory file keeps: permissions plus setuid,
// setgid and sticky, the bits chmod(2) sets on a regular file.
const memModeBits = os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky

// errAppendWriteAt matches the os package's refusal of positioned appends.
var errAppendWriteAt = errors.New("os: invalid use of WriteAt on file opened with O_APPEND")

// root skips the owner permission check, as the kernel does for root.
var root = os.Geteuid() == 0

// The process's effective owner, which new memory files get: read once, as
// the daemon never changes credentials and a create should not cost two
// system calls for it.
var euid, egid = uint32(os.Geteuid()), uint32(os.Getegid())

// Memory is the byte budget overlays share for keeping written upper files in
// memory instead of on disk. New, truncated and small copied-up regular files
// start in memory; a file moves into its upper (spills) once it has been left
// untouched for the idle age, when it outgrows the per-file limit, when newer
// files need the budget (least recently used first), before an operation that
// needs a host file (hard link, chown, lock), and at [FS.Flush]. Short-lived
// files never reach the disk. Directories, symlinks and whiteouts always live
// on disk. Memory contents do not survive a crash unless synced with
// SyncOnFsync enabled; pass no Memory to [New] to write all files to disk.
type Memory struct {
	// SyncOnFsync spills and syncs a named memory file when its handle is
	// synced. Set before sharing the budget with any overlay.
	SyncOnFsync bool

	// Logf reports the first failed spill of each overlay; nil is silent.
	// Set before sharing the budget with any overlay.
	Logf func(format string, args ...any)

	limit, fileMax int64
	idle           time.Duration
	retryEvery     time.Duration // period of the writeback retry of parked files

	mu      sync.Mutex
	queues  map[*FS]error // overlays whose queue of host changes is parked, see [FS.applier]
	used    int64
	lru     list.List // *memFile, most recently used first
	parked  list.List // *memFile whose spill failed, oldest first
	lastErr error     // the latest spill failure of a parked file
	writing bool      // the writeback worker runs
}

// DefaultFileMax caps one file's data at half the budget, leaving the rest for other files.
func DefaultFileMax(limit int64) int64 { return limit / 2 }

// NewMemory returns a budget of limit bytes that keeps files of up to fileMax
// bytes, writing back files untouched for idle (0 keeps them until memory
// pressure or [FS.Flush]).
func NewMemory(limit, fileMax int64, idle time.Duration) *Memory {
	return &Memory{limit: limit, fileMax: min(fileMax, limit), idle: idle, retryEvery: time.Second}
}

// writeback spills files left untouched for the idle age, oldest first, so
// snapshots and crashes find only recent files in memory, and retries parked
// files every retryEvery. With idle 0 it retries parked files only. It exits
// once no file is waiting; admit and park restart it.
func (mem *Memory) writeback() {
	var retryAt time.Time
	for {
		mem.mu.Lock()
		if mem.parked.Len() > 0 && !time.Now().Before(retryAt) {
			files := make([]*memFile, 0, mem.parked.Len())
			for e := mem.parked.Front(); e != nil; e = e.Next() {
				files = append(files, e.Value.(*memFile))
			}
			mem.mu.Unlock()
			for _, m := range files {
				m.owner.writeBack(m)
			}
			retryAt = time.Now().Add(mem.retryEvery)
			continue
		}
		var wait time.Duration
		if e := mem.lru.Back(); mem.idle > 0 && e != nil {
			m := e.Value.(*memFile)
			if wait = mem.idle - time.Since(m.touched); wait <= 0 {
				mem.mu.Unlock()
				m.owner.writeBack(m)
				continue
			}
		} else if mem.parked.Len() == 0 {
			mem.writing = false
			mem.mu.Unlock()
			return
		}
		if mem.parked.Len() > 0 {
			if r := time.Until(retryAt); wait == 0 || r < wait {
				wait = r
			}
		}
		mem.mu.Unlock()
		time.Sleep(wait)
	}
}

// park moves m from the LRU to the parked list after its spill failed with
// err, keeping its charge: eviction skips it, the writeback worker retries it
// every retryEvery, and admit refuses new files until it leaves. The first
// failure of each overlay is logged. A file that already left its overlay's
// namespace is ignored. The caller holds m.owner.mu.
func (mem *Memory) park(m *memFile, err error) {
	f := m.owner
	if f.files[m.path] != m {
		return
	}
	mem.mu.Lock()
	if m.elem != nil {
		mem.lru.Remove(m.elem)
		m.elem = nil
	}
	if m.pelem == nil {
		m.pelem = mem.parked.PushBack(m)
	}
	mem.lastErr = err
	if !mem.writing {
		mem.writing = true
		go mem.writeback()
	}
	logf := mem.Logf
	mem.mu.Unlock()
	if logf != nil && f.spillLogged.CompareAndSwap(false, true) {
		logf("overlay %s: keeping a file in memory after its write to disk failed, retrying: %v", f.upper, err)
	}
}

// Parked reports how many files wait in memory after a failed spill and the
// latest failure, or 0 and "" when none waits.
func (mem *Memory) Parked() (n int, err string) {
	mem.mu.Lock()
	defer mem.mu.Unlock()
	if mem.parked.Len() == 0 {
		return 0, ""
	}
	return mem.parked.Len(), mem.lastErr.Error()
}

// QueuesParked reports how many overlays' queued host changes wait after a
// failure, retried every second, and one such failure; 0 and "" when none.
func (mem *Memory) QueuesParked() (n int, err string) {
	mem.mu.Lock()
	defer mem.mu.Unlock()
	for _, e := range mem.queues {
		err = e.Error()
	}
	return len(mem.queues), err
}

// parkQueue records that f's queue of host changes failed with err.
func (mem *Memory) parkQueue(f *FS, err error) {
	mem.mu.Lock()
	if mem.queues == nil {
		mem.queues = make(map[*FS]error)
	}
	mem.queues[f] = err
	logf := mem.Logf
	mem.mu.Unlock()
	if logf != nil {
		logf("overlay %s: a queued change of the upper failed, retrying: %v", f.upper, err)
	}
}

// unparkQueue records that f's queue of host changes moves again.
func (mem *Memory) unparkQueue(f *FS) {
	mem.mu.Lock()
	delete(mem.queues, f)
	mem.mu.Unlock()
}

// Usage reports the bytes charged to memory files and the budget's limit.
func (mem *Memory) Usage() (used, limit int64) {
	mem.mu.Lock()
	defer mem.mu.Unlock()
	return mem.used, mem.limit
}

// touch marks m as the most recently used file.
func (mem *Memory) touch(m *memFile) {
	mem.mu.Lock()
	if m.elem != nil {
		mem.lru.MoveToFront(m.elem)
		m.touched = time.Now()
	}
	mem.mu.Unlock()
}

// uncharge returns n of m's bytes after its data shrank.
func (mem *Memory) uncharge(m *memFile, n int64) {
	mem.mu.Lock()
	mem.used -= n
	m.charged -= n
	mem.mu.Unlock()
}

// release returns m's whole charge and forgets its LRU or parked position.
func (mem *Memory) release(m *memFile) {
	mem.mu.Lock()
	if m.elem != nil {
		mem.lru.Remove(m.elem)
		m.elem = nil
	}
	if m.pelem != nil {
		mem.parked.Remove(m.pelem)
		m.pelem = nil
		if mem.parked.Len() == 0 {
			mem.lastErr = nil
		}
	}
	mem.used -= m.charged
	m.charged = 0
	mem.mu.Unlock()
}

// victim picks the least recently used small file other than keep that can
// spill now, so admission never pays for a large spill. The caller holds
// mem.mu and f.mu; it must unlock a foreign owner after spilling.
func (mem *Memory) victim(f *FS, keep *memFile) *memFile {
	e := mem.lru.Back()
	for range victimScan {
		if e == nil {
			return nil
		}
		m := e.Value.(*memFile)
		e = e.Prev()
		if m == keep || m.charged > fileOverhead+smallFileMax {
			continue
		}
		if m.owner == f || m.owner.mu.TryLock() {
			// Spilling now would overtake host changes on its name.
			if !m.installing && m.owner.busy(m.path) == nil {
				return m
			}
			// The writeback worker is moving it to disk already.
			if m.owner != f {
				m.owner.mu.Unlock()
			}
		}
	}
	return nil
}

// memFile is an upper regular file whose data lives in memory. path is the
// name it moves to disk under first; a hard link made while it is in memory
// adds a name to others, and its other names are linked to that inode
// behind the spill ([FS.linkInstalled]). path is empty once every name is
// unlinked, while open handles keep reading and writing its data.
type memFile struct {
	owner               *FS
	path                string
	others              map[string]bool // its other names, each with its marked flag
	data                []byte
	mode                os.FileMode // memModeBits only
	atime, mtime, ctime time.Time
	created             time.Time
	id                  ident.ID // a new Synthetic one, or that of the lower file it copies
	uid, gid            uint32   // what a host create in its directory gets
	marked              bool     // path keeps a deletion marker on disk until it spills, see [FS.mayHoldMarker]
	installing          bool     // its staged copy is being renamed into place outside f.mu, see [FS.writeBack]
	handles             map[*memHandle]struct{}
	version             uint64        // counts changes of data and metadata, see [FS.writeBack]
	elem                *list.Element // LRU position while named; guarded by Memory.mu
	pelem               *list.Element // parked position after a failed spill; guarded by Memory.mu
	touched             time.Time     // last open or write; guarded by Memory.mu
	charged             int64
}

// info snapshots m's metadata under the given base name.
func (m *memFile) info(name string) os.FileInfo {
	return memInfo{name: name, size: int64(len(m.data)), mode: m.mode, atime: m.atime, mtime: m.mtime, ctime: m.ctime, created: m.created, id: m.id, uid: m.uid, gid: m.gid, nlink: uint32(1 + len(m.others))}
}

// markedAt reports whether m's name p keeps a deletion marker on disk until
// the file spills.
func (m *memFile) markedAt(p string) bool {
	if p == m.path {
		return m.marked
	}
	return m.others[p]
}

// setMarked records whether m's name p keeps a deletion marker on disk.
func (m *memFile) setMarked(p string, marked bool) {
	if p == m.path {
		m.marked = marked
	} else if _, ok := m.others[p]; ok {
		m.others[p] = marked
	}
}

// rename moves m's name a to b, where a rename put it.
func (m *memFile) rename(a, b string) {
	if a == m.path {
		m.path = b
		return
	}
	marked := m.others[a]
	delete(m.others, a)
	m.others[b] = marked
}

// promote makes p, one of m's names, the one it moves to disk under first.
func (m *memFile) promote(p string) {
	if p == m.path {
		return
	}
	marked := m.others[p]
	delete(m.others, p)
	m.others[m.path] = m.marked
	m.path, m.marked = p, marked
}

// otherName returns a name of m other than p, or "".
func (m *memFile) otherName(p string) string {
	if m.path != p {
		return m.path
	}
	for o := range m.others {
		return o
	}
	return ""
}

// access makes the owner permission check the kernel makes when the daemon
// reopens its own upper file; a file's creating open skips it.
func (m *memFile) access(name string, flag int) error { return ownerAccess(m.mode, name, flag) }

// ownerAccess is the check the kernel makes when the daemon opens a file it
// owns with mode for flag: the owner's read and write bits, unless it runs
// as root. A copy-up the daemon makes is its own, so this decides an open
// of the copy before the copy is made.
func ownerAccess(mode os.FileMode, name string, flag int) error {
	if root || (!readable(flag) || mode&0o400 != 0) && (!writable(flag) || mode&0o200 != 0) {
		return nil
	}
	return &fs.PathError{Op: "open", Path: name, Err: syscall.EACCES}
}

// resize sets m's length within its capacity, zeroing bytes that growth exposes.
func (m *memFile) resize(n int64) {
	old := int64(len(m.data))
	m.data = m.data[:n]
	if n > old {
		clear(m.data[old:])
	}
}

// memInfo is a point-in-time FileInfo of a memory file with host-style stat data.
type memInfo struct {
	name                string
	size                int64
	mode                os.FileMode
	atime, mtime, ctime time.Time
	created             time.Time
	id                  ident.ID
	uid, gid            uint32
	nlink               uint32
}

// Name returns the base name.
func (i memInfo) Name() string { return i.name }

// Size returns the data length.
func (i memInfo) Size() int64 { return i.size }

// Mode returns the permission bits of a regular file.
func (i memInfo) Mode() os.FileMode { return i.mode }

// ModTime returns the last data or chtimes change.
func (i memInfo) ModTime() time.Time { return i.mtime }

// IsDir is always false: directories live on disk.
func (memInfo) IsDir() bool { return false }

// Sys returns synthetic stat data with the owner the file has on disk.
func (i memInfo) Sys() any { return memStat(i) }

// Identity returns the memory file's identity, which its file on disk keeps
// once it spills.
func (i memInfo) Identity() ident.ID { return i.id }

// admit charges n more bytes to m, linking it into the budget's LRU, after
// spilling least recently used files while the budget is exhausted. It reports
// false when a large file would evict peers, nothing more can spill, or any
// file is parked (so no new data joins memory that may not reach the disk).
// The caller holds f.mu and falls back to disk on refusal.
func (f *FS) admit(m *memFile, n int64) bool {
	mem := f.mem
	for {
		mem.mu.Lock()
		if mem.parked.Len() > 0 {
			mem.mu.Unlock()
			return false
		}
		if n <= mem.limit-mem.used {
			mem.used += n
			m.charged += n
			if m.elem == nil {
				m.elem = mem.lru.PushFront(m)
			} else {
				mem.lru.MoveToFront(m.elem)
			}
			m.touched = time.Now()
			if mem.idle > 0 && !mem.writing {
				mem.writing = true
				go mem.writeback()
			}
			mem.mu.Unlock()
			return true
		}
		if n > fileOverhead+smallFileMax-m.charged {
			mem.mu.Unlock()
			return false
		}
		v := mem.victim(f, m)
		mem.mu.Unlock()
		if v == nil {
			return false
		}
		owner := v.owner
		err := owner.spill(v)
		if err != nil {
			mem.park(v, err)
		}
		if owner != f {
			owner.mu.Unlock()
		}
		if err != nil {
			return false
		}
	}
}

// addMem publishes p as a memory file holding data with identity id (0: a new
// Synthetic one), leaving the parent's mtime to the caller: a new name stamps
// it ([FS.touchDir]), a copy-up keeps it. A deletion marker at p stays on disk
// until the file spills. It returns nil when the budget refuses the file or no
// identity can be allocated, and the caller keeps the data on disk. The caller
// ensured p's parent exists in upper and p has no upper entry.
func (f *FS) addMem(p string, mode os.FileMode, mtime time.Time, data []byte, id ident.ID) *memFile {
	if id == 0 {
		var err error
		if id, err = f.ids.syn.allocate(); err != nil {
			return nil
		}
	}
	now := time.Now()
	m := &memFile{owner: f, path: p, data: data, mode: mode & memModeBits, atime: mtime, mtime: mtime, ctime: now, created: now, id: id, handles: make(map[*memHandle]struct{}), marked: f.mayHoldMarker(p)}
	m.uid, m.gid = newOwner(f.index.entries[parent(p)].Info)
	if !f.admit(m, fileOverhead+int64(cap(data))) {
		return nil
	}
	f.files[p] = m
	info := m.info(path.Base(p))
	f.index.set(p, Entry{Kind: KindFile, Info: info})
	f.register(p, info)
	return m
}

// copyUpMem copies a small immutable regular file into memory, reporting false
// when it must be copied to disk instead (too large, or no budget).
func (f *FS) copyUpMem(s source, p string, info os.FileInfo) (bool, error) {
	if f.mem == nil || !info.Mode().IsRegular() || info.Size() > f.mem.fileMax {
		return false, nil
	}
	var data []byte
	var err error
	if s.layer != nil {
		data, err = s.layer.ReadFile(s.path)
	} else {
		var c Content
		if c, err = f.bottom.Open(s.path); err == nil {
			data, err = readBounded(c, info.Size(), f.mem.fileMax)
			err = errors.Join(err, c.Close())
		}
	}
	if err != nil || int64(len(data)) > f.mem.fileMax {
		return false, err
	}
	return f.addMem(p, info.Mode(), info.ModTime(), data, f.inherit(s, p, info)) != nil, nil
}

// readBounded reads r, sized for about size bytes, stopping once it holds more
// than limit so callers can reject content that grew.
func readBounded(r io.Reader, size, limit int64) ([]byte, error) {
	buf := make([]byte, 0, min(size, limit)+1)
	for {
		n, err := r.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if err == io.EOF || int64(len(buf)) > limit {
			return buf, nil
		}
		if err != nil {
			return nil, err
		}
		if len(buf) == cap(buf) {
			buf = slices.Grow(buf, len(buf))
		}
	}
}

// grow makes room for size bytes of m's data, spilling m instead when it would
// outgrow the per-file limit or the budget. It reports whether m is still in
// memory. Unlinked files cannot spill and grow unbudgeted.
func (f *FS) grow(m *memFile, size int64) (bool, error) {
	c := int64(cap(m.data))
	// A copied-up slice may have spare capacity beyond the per-file limit.
	if m.path != "" && size > f.mem.fileMax {
		return false, f.spill(m)
	}
	if size <= c {
		return true, nil
	}
	next := max(size, 2*c, 4096)
	if m.path != "" {
		next = min(next, f.mem.fileMax)
		if !f.admit(m, next-c) {
			return false, f.spill(m)
		}
	}
	data := make([]byte, len(m.data), next)
	copy(data, m.data)
	m.data = data
	return true, nil
}

// truncateMem sets m's size, reporting false when it spilled instead. Emptying
// a named file returns its capacity to the budget.
func (f *FS) truncateMem(m *memFile, size int64) (bool, error) {
	in, err := f.grow(m, size)
	if !in || err != nil {
		return false, err
	}
	if size == 0 && m.path != "" {
		f.mem.uncharge(m, int64(cap(m.data)))
		m.data = nil
	} else {
		m.resize(size)
	}
	now := time.Now()
	m.mtime, m.ctime = now, now
	m.version++
	return true, nil
}

// writeBack spills m for the writeback worker if it is parked, or still
// waiting in memory and still idle. A file without open handles moves to
// disk without f.mu held across host I/O, so the checkout's other calls go
// on meanwhile: it is staged from a copy (milliseconds where Windows
// Defender scans new files), then renamed into place as a host operation on
// its name ([FS.lockPaths]), during which calls that would change the name
// or open the file wait and the index still shows it in memory. It replaces
// m only if m did not change before the rename, else a later round retries.
// [FS.Flush] and [FS.Discard] wait for a staging in progress.
func (f *FS) writeBack(m *memFile) {
	f.staging.RLock()
	defer f.staging.RUnlock()
	f.mu.Lock()
	f.mem.mu.Lock()
	due := m.pelem != nil || m.elem != nil && time.Since(m.touched) >= f.mem.idle
	f.mem.mu.Unlock()
	if !due || f.files[m.path] != m || f.busy(m.path) != nil {
		f.mu.Unlock()
		return
	}
	if len(m.handles) != 0 {
		if err := f.spill(m); err != nil {
			f.mem.park(m, err)
		}
		f.mu.Unlock()
		return
	}
	copied := &memFile{data: slices.Clone(m.data), mode: m.mode, atime: m.atime, mtime: m.mtime, created: m.created, id: m.id, uid: m.uid, gid: m.gid}
	version := m.version
	tmp, ids := f.stagingName(), f.ids
	f.mu.Unlock()
	_, err := stage(tmp, copied, ids)
	var info os.FileInfo
	if err == nil {
		// The file keeps its inode and metadata through the rename.
		info, err = statHost(tmp, ids)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	p := m.path
	if err != nil || m.version != version || f.files[p] != m || len(m.handles) != 0 || f.busy(p) != nil {
		ids.discard(tmp)
		if err != nil {
			f.mem.park(m, err)
		}
		return
	}
	op := f.begin(p)
	m.installing = true
	real, marker, marked := f.up(p), f.whiteoutPath(p), m.marked
	f.mu.Unlock()
	err = renameFile(tmp, real)
	var markerErr error
	if err == nil && marked {
		markerErr = removeMarker(marker)
	}
	f.mu.Lock()
	m.installing = false
	f.end(op)
	if err != nil {
		ids.discard(tmp)
		f.mem.park(m, err)
		return
	}
	if m.marked && markerErr == nil {
		m.marked = false
	}
	if m.version != version || len(m.handles) != 0 || m.path != p || f.files[p] != m {
		// Changed or renamed during the rename after all: m stays in memory,
		// or holds p no longer, and keeps no file of this copy on disk.
		ids.discard(real)
		return
	}
	// A marker that could not go stays beside the data until the index
	// loads again, as after a crash.
	f.installedAs(m, aliasInfo{FileInfo: info, name: path.Base(p)})
}

// installedAs retires memory file m, whose staged copy described by info
// now holds its name in the upper, without a stat: invisible to clients, the
// rename leaves the directory's indexed time alone. The caller holds f.mu.
func (f *FS) installedAs(m *memFile, info os.FileInfo) {
	p := m.path
	others := f.retire(m)
	m.data = nil
	f.cacheInfo(p, info)
	f.unsettled[parent(p)] = struct{}{}
	f.linkInstalled(p, others)
}

// stagingName returns a new private name in the work directory. The caller holds f.mu.
func (f *FS) stagingName() string {
	f.tmpSeq++
	return filepath.Join(f.work, strconv.FormatUint(f.tmpSeq, 36))
}

// detach removes m, with all its names, from the namespace and the budget;
// open handles keep its data.
func (f *FS) detach(m *memFile) {
	delete(f.files, m.path)
	for o := range m.others {
		delete(f.files, o)
	}
	m.path, m.others = "", nil
	f.mem.release(m)
}

// unname removes name p of memory file m: the file stays under its other
// names, and leaves the namespace and the budget with its last.
func (f *FS) unname(m *memFile, p string) {
	if len(m.others) == 0 {
		f.detach(m)
		return
	}
	delete(f.files, p)
	if p == m.path {
		o := m.otherName(p)
		m.path, m.marked = o, m.others[o]
		delete(m.others, o)
	} else {
		delete(m.others, p)
	}
	m.ctime = time.Now()
	m.version++
}

// linkMem gives memory file m the new name dst, in an upper directory, in
// the index alone: its inode reaches disk when it spills.
func (f *FS) linkMem(m *memFile, dst string) error {
	if m.others == nil {
		m.others = make(map[string]bool)
	}
	m.others[dst] = f.mayHoldMarker(dst)
	f.files[dst] = m
	m.ctime = time.Now()
	m.version++ // a writeback in progress copied it with fewer names
	info := m.info(path.Base(dst))
	f.index.set(dst, Entry{Kind: KindFile, Info: info})
	f.register(dst, info)
	return f.touchDir(parent(dst), false)
}

// linkedBefore reports whether link error err only says that to is from
// already: an earlier attempt of a retried queued link made it.
func linkedBefore(err error, from, to string) bool {
	if !errors.Is(err, fs.ErrExist) {
		return false
	}
	a, aerr := os.Lstat(from)
	b, berr := os.Lstat(to)
	return aerr == nil && berr == nil && os.SameFile(a, b)
}

// linkInstalled indexes others, the names a memory file had besides p, as
// hard links of the inode its spill just installed at p (with marked flags
// for the deletion markers they keep), and queues their host links behind
// the host changes on those names, as [FS.queueLink] does: a crash can lose
// such a name but never shows two copies of the file. The caller holds f.mu
// exclusively.
func (f *FS) linkInstalled(p string, others map[string]bool) {
	if len(others) == 0 {
		return
	}
	info := unalias(f.index.entries[p].Info)
	more, ok := relinked(info, len(others))
	if !ok {
		more = info // past the host's link count type: the host's count shows
	}
	f.cacheInfo(p, aliasInfo{FileInfo: more, name: path.Base(p)})
	paths := []string{p}
	for o := range others {
		f.cacheInfo(o, aliasInfo{FileInfo: more, name: path.Base(o)})
		paths = append(paths, o)
	}
	from := f.up(p)
	links := make(map[string]string, len(others)) // host path -> its marker, or ""
	for o, marked := range others {
		links[f.up(o)] = ""
		if marked {
			links[f.up(o)] = f.whiteoutPath(o)
		}
	}
	f.enqueueNow(paths, paths[1:], func() error {
		for to, marker := range links {
			if err := os.Link(from, to); err != nil && !linkedBefore(err, from, to) {
				return err
			}
			if marker != "" {
				if err := removeMarker(marker); err != nil {
					return err
				}
			}
		}
		return nil
	}, func() {
		// The links changed their directories on disk; the index keeps their times.
		for _, o := range paths[1:] {
			f.unsettled[parent(o)] = struct{}{}
		}
	})
}

// retire takes memory file m, now on disk under m.path, out of the
// namespace and the budget, and returns its other names with their marked
// flags for [FS.linkInstalled].
func (f *FS) retire(m *memFile) map[string]bool {
	others := m.others
	delete(f.files, m.path)
	for o := range others {
		delete(f.files, o)
	}
	m.others = nil
	f.mem.release(m)
	return others
}

// spillAs is [FS.spill] that moves m to disk under its name p first, for
// a caller about to change p on the host: m's other names follow as queued
// links ([FS.linkInstalled]). The caller waited for the host changes on p.
func (f *FS) spillAs(m *memFile, p string) error {
	m.promote(p)
	return f.spill(m)
}

// spill moves m into the upper as an ordinary file under m.path, its other
// names following as queued links, and hands each open handle a host
// descriptor at its offset. The caller holds f.mu; m must be named.
func (f *FS) spill(m *memFile) error {
	p := m.path
	tmp := f.stagingName()
	fds, err := stage(tmp, m, f.ids)
	if err == nil {
		err = f.preserveDirTime(parent(p), func() error { return renameFile(tmp, f.up(p)) })
	}
	if err != nil {
		for _, fd := range fds {
			fd.Close()
		}
		f.ids.discard(tmp)
		return err
	}
	return f.installed(m, fds)
}

// installed retires memory file m once its staged copy holds its name in the
// upper, then drops the deletion marker the name kept while in memory, and
// hands each open handle its staged descriptor. The caller holds f.mu.
func (f *FS) installed(m *memFile, fds map[*memHandle]*os.File) error {
	p := m.path
	marked := m.marked
	others := f.retire(m)
	err := errors.Join(f.clearWhiteout(p, marked), f.refresh(p))
	if err == nil {
		f.linkInstalled(p, others)
	}
	for h, fd := range fds {
		h.disk = f.adopt(&file{File: fd, name: h.name, writable: writable(h.flag)})
		f.bind(h.disk, p)
		h.m = nil
		f.memoryHandles--
	}
	clear(m.handles)
	m.data = nil
	return err
}

// stage writes m's data, identity (persisted in ids) and metadata to the new
// file tmp and opens a host descriptor for each handle. Descriptors open before the final chmod,
// which may forbid their access mode. The file is created with m's
// permissions plus the owner's read and write, so the usual modes need no
// chmod afterwards.
func stage(tmp string, m *memFile, ids *idStore) (map[*memHandle]*os.File, error) {
	out, err := hostfile.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_RDWR, m.mode.Perm()|0o600)
	if err != nil {
		return nil, err
	}
	// A file shows the same owner in memory and on disk (see newOwner); the
	// staging directory need not hand out the group the final one does.
	created, err := matchOwner(out, m.uid, m.gid)
	if err == nil {
		_, err = out.Write(m.data)
	}
	fds := make(map[*memHandle]*os.File, len(m.handles))
	for h := range m.handles {
		if err != nil {
			break
		}
		var fd *os.File
		if fd, err = hostfile.OpenFile(tmp, h.flag&^(os.O_CREATE|os.O_EXCL|os.O_TRUNC), 0); err != nil {
			break
		}
		fds[h] = fd
		if h.flag&os.O_APPEND == 0 {
			_, err = fd.Seek(h.off, io.SeekStart)
		}
	}
	err = errors.Join(err, out.Close())
	if err == nil {
		err = ids.persist(tmp, m.id)
	}
	if err == nil && (created == nil || created.Mode()&memModeBits != m.mode) {
		err = os.Chmod(tmp, m.mode)
	}
	if err == nil {
		err = keepMode(tmp, m.mode)
	}
	if err == nil {
		err = setTimes(tmp, m.atime, m.mtime, m.created)
	}
	return fds, err
}

// newHandle registers a handle on m so a spill can hand it a host descriptor.
func (f *FS) newHandle(m *memFile, name string, flag int) *memHandle {
	h := &memHandle{fs: f, name: name, flag: flag, m: m, id: m.id}
	f.memoryHandles++
	m.handles[h] = struct{}{}
	f.mem.touch(m)
	return h
}

// openMem opens an existing memory file like a host open: permission check,
// then truncation for O_TRUNC writers.
func (f *FS) openMem(m *memFile, name string, flag int) (billy.File, error) {
	if err := m.access(name, flag); err != nil {
		return nil, err
	}
	h := f.newHandle(m, name, flag)
	if flag&os.O_TRUNC == 0 || !writable(flag) {
		return h, nil
	}
	in, err := f.truncateMem(m, 0)
	if err == nil && !in {
		err = h.disk.File.Truncate(0)
	}
	if err != nil {
		h.Close()
		return nil, err
	}
	return h, nil
}

// Flush spills every memory file into the upper, after which the upper
// directory alone holds this overlay's changes. Snapshots, diffs and shutdown
// call it before reading the upper from disk.
func (f *FS) Flush() error {
	f.staging.Lock()
	defer f.staging.Unlock()
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.drainLocked(); err != nil {
		return upperError(err)
	}
	for _, m := range f.files {
		if err := f.spill(m); err != nil {
			return upperError(err)
		}
	}
	// The other names of multi-name files were queued as links.
	if err := f.drainLocked(); err != nil {
		return upperError(err)
	}
	return f.settleDirs(f.upper)
}

// Discard drops every memory file without writing it and returns its budget,
// for an overlay whose upper is being deleted.
func (f *FS) Discard() {
	defer f.names.Add(1)
	f.staging.Lock()
	defer f.staging.Unlock()
	f.mu.Lock()
	defer f.mu.Unlock()
	// The upper is about to go: its queued host changes are moot.
	f.dropQueue()
	for op := f.busySync(""); op != nil; op = f.busySync("") {
		f.waitOp(op)
	}
	for _, m := range f.files {
		f.detach(m)
	}
	clear(f.unsettled)
}

// Unflushed reports whether some file data lives only in memory, leaving out
// files at or below any of the paths except.
func (f *FS) Unflushed(except ...string) bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	for p := range f.files {
		if !slices.ContainsFunc(except, func(dir string) bool { return p == dir || strings.HasPrefix(p, dir+"/") }) {
			return true
		}
	}
	return false
}

// writable reports an access mode that permits writes.
func writable(flag int) bool { return flag&(os.O_WRONLY|os.O_RDWR) != 0 }

// readable reports an access mode that permits reads.
func readable(flag int) bool { return flag&(os.O_WRONLY|os.O_RDWR) != os.O_WRONLY }

// memHandle is an open descriptor of a memory file with its own offset. When
// the file spills, a host descriptor takes over for good and the handle
// forwards to it. Fields other than disk's target are guarded by fs.mu.
type memHandle struct {
	fs     *FS
	name   string
	flag   int
	id     ident.ID // the file's identity, kept once spilled
	m      *memFile // nil once spilled
	off    int64
	disk   *file // set once, by spill
	closed bool
}

// lock takes fs.mu for a memory-backed call that cannot spill the file, or
// returns the host file that took over (with fs.mu released). A parked
// queued change on the file's name does not hold it up.
func (h *memHandle) lock() *file {
	d, _ := h.lockSpill()
	return d
}

// lockSpill is lock for a call that may spill the file, a host change that
// follows the queued ones on its name: it waits for them, and returns, with
// fs.mu held, the parked queue's failure that holds them up.
func (h *memHandle) lockSpill() (*file, error) {
	h.fs.mu.Lock()
	for {
		if d := h.disk; d != nil {
			h.fs.mu.Unlock()
			return d, nil
		}
		op := (*hostOp)(nil)
		if h.m.path != "" {
			op = h.fs.busy(h.m.path)
		}
		if op == nil {
			return nil, nil
		}
		if err := h.fs.waitOp(op); err != nil {
			return nil, err
		}
	}
}

// rlock is lock for calls that only read the file.
func (h *memHandle) rlock() *file {
	h.fs.mu.RLock()
	if d := h.disk; d != nil {
		h.fs.mu.RUnlock()
		return d
	}
	return nil
}

// check rejects calls on a closed handle or one whose access mode forbids op.
func (h *memHandle) check(op string, allowed func(int) bool) error {
	if h.closed {
		return &fs.PathError{Op: op, Path: h.name, Err: os.ErrClosed}
	}
	if allowed != nil && !allowed(h.flag) {
		return &fs.PathError{Op: op, Path: h.name, Err: syscall.EBADF}
	}
	return nil
}

// Name returns the overlay-relative name the handle was opened with.
func (h *memHandle) Name() string { return h.name }

// Identity returns the file's identity, which the export keeps to find the
// file again after the handle closes; it needs no lock.
func (h *memHandle) Identity() ident.ID { return h.id }

// Read reads at the handle's offset and advances it.
func (h *memHandle) Read(p []byte) (int, error) {
	if d := h.lock(); d != nil {
		return d.Read(p)
	}
	defer h.fs.mu.Unlock()
	if err := h.check("read", readable); err != nil {
		return 0, err
	}
	if len(p) == 0 {
		return 0, nil
	}
	if h.off >= int64(len(h.m.data)) {
		return 0, io.EOF
	}
	n := copy(p, h.m.data[h.off:])
	h.off += int64(n)
	return n, nil
}

// ReadAt reads at off without moving the offset, failing with io.EOF when short.
func (h *memHandle) ReadAt(p []byte, off int64) (int, error) {
	if d := h.rlock(); d != nil {
		return d.ReadAt(p, off)
	}
	defer h.fs.mu.RUnlock()
	if err := h.check("read", readable); err != nil {
		return 0, err
	}
	if off < 0 {
		return 0, &fs.PathError{Op: "readat", Path: h.name, Err: syscall.EINVAL}
	}
	if off >= int64(len(h.m.data)) {
		if len(p) == 0 {
			return 0, nil
		}
		return 0, io.EOF
	}
	n := copy(p, h.m.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// Write writes at the offset (or the end for O_APPEND) and advances it.
func (h *memHandle) Write(p []byte) (int, error) {
	d, err := h.lockSpill()
	if err != nil {
		h.fs.mu.Unlock()
		return 0, err
	}
	if d != nil {
		return d.Write(p)
	}
	defer h.fs.mu.Unlock()
	if err := h.check("write", writable); err != nil {
		return 0, err
	}
	off := h.off
	if h.flag&os.O_APPEND != 0 {
		off = int64(len(h.m.data))
	}
	return h.write(p, off, true)
}

// WriteAt writes at off without moving the offset.
func (h *memHandle) WriteAt(p []byte, off int64) (int, error) {
	d, err := h.lockSpill()
	if err != nil {
		h.fs.mu.Unlock()
		return 0, err
	}
	if d != nil {
		return d.WriteAt(p, off)
	}
	defer h.fs.mu.Unlock()
	if err := h.check("writeat", writable); err != nil {
		return 0, err
	}
	if h.flag&os.O_APPEND != 0 {
		return 0, errAppendWriteAt
	}
	if off < 0 {
		return 0, &fs.PathError{Op: "writeat", Path: h.name, Err: syscall.EINVAL}
	}
	return h.write(p, off, false)
}

// write stores p at off in memory, or through the host descriptor when the
// write spills the file; seq moves the offset. The caller holds fs.mu.
func (h *memHandle) write(p []byte, off int64, seq bool) (int, error) {
	m := h.m
	end := off + int64(len(p))
	in, err := h.fs.grow(m, end)
	if err != nil {
		return 0, err
	}
	if !in {
		var n int
		if seq {
			n, err = h.disk.File.Write(p)
		} else {
			n, err = h.disk.File.WriteAt(p, off)
		}
		return n, err
	}
	if end > int64(len(m.data)) {
		m.resize(end)
	}
	copy(m.data[off:], p)
	now := time.Now()
	m.mtime, m.ctime = now, now
	m.version++
	if m.path != "" {
		h.fs.mem.touch(m)
	}
	if seq {
		h.off = end
	}
	return len(p), nil
}

// Seek moves the offset; positions past the end are allowed, as for host files.
func (h *memHandle) Seek(offset int64, whence int) (int64, error) {
	if d := h.lock(); d != nil {
		return d.Seek(offset, whence)
	}
	defer h.fs.mu.Unlock()
	if err := h.check("seek", nil); err != nil {
		return 0, err
	}
	var pos int64
	switch whence {
	case io.SeekStart:
		pos = offset
	case io.SeekCurrent:
		pos = h.off + offset
	case io.SeekEnd:
		pos = int64(len(h.m.data)) + offset
	default:
		return 0, &fs.PathError{Op: "seek", Path: h.name, Err: syscall.EINVAL}
	}
	if pos < 0 {
		return 0, &fs.PathError{Op: "seek", Path: h.name, Err: syscall.EINVAL}
	}
	h.off = pos
	return pos, nil
}

// Truncate resizes the file, spilling it when it outgrows memory.
func (h *memHandle) Truncate(size int64) error {
	d, err := h.lockSpill()
	if err != nil {
		h.fs.mu.Unlock()
		return err
	}
	if d != nil {
		return d.Truncate(size)
	}
	defer h.fs.mu.Unlock()
	if err := h.check("truncate", writable); err != nil {
		return err
	}
	if size < 0 {
		return &fs.PathError{Op: "truncate", Path: h.name, Err: syscall.EINVAL}
	}
	in, err := h.fs.truncateMem(h.m, size)
	if err != nil || in {
		return err
	}
	return h.disk.File.Truncate(size)
}

// Stat describes the file as it is now.
func (h *memHandle) Stat() (os.FileInfo, error) {
	if d := h.rlock(); d != nil {
		return d.Stat()
	}
	defer h.fs.mu.RUnlock()
	if err := h.check("stat", nil); err != nil {
		return nil, err
	}
	return h.m.info(path.Base(h.name)), nil
}

// Sync spills and syncs a named file when configured, otherwise it succeeds
// without I/O. An unlinked memory file has no host file to sync.
func (h *memHandle) Sync() error {
	if !h.fs.mem.SyncOnFsync {
		if d := h.rlock(); d != nil {
			return d.Sync()
		}
		defer h.fs.mu.RUnlock()
		return h.check("sync", nil)
	}
	d, err := h.lockSpill()
	if err != nil {
		h.fs.mu.Unlock()
		return err
	}
	if d != nil {
		return d.Sync()
	}
	if err := h.check("sync", nil); err != nil || h.m.path == "" {
		h.fs.mu.Unlock()
		return err
	}
	err = h.fs.spill(h.m)
	d = h.disk
	h.fs.mu.Unlock()
	if err != nil {
		return err
	}
	return d.Sync()
}

// Lock spills the file so an advisory lock reaches a host descriptor.
// An unlinked file has no host file; locking it succeeds trivially.
func (h *memHandle) Lock() error {
	d, err := h.lockSpill()
	if err != nil {
		h.fs.mu.Unlock()
		return err
	}
	if d != nil {
		return d.Lock()
	}
	if err := h.check("lock", nil); err != nil || h.m.path == "" {
		h.fs.mu.Unlock()
		return err
	}
	err = h.fs.spill(h.m)
	d = h.disk
	h.fs.mu.Unlock()
	if err != nil {
		return err
	}
	return d.Lock()
}

// Unlock releases a lock taken after spilling; a memory handle holds none.
func (h *memHandle) Unlock() error {
	if d := h.lock(); d != nil {
		return d.Unlock()
	}
	defer h.fs.mu.Unlock()
	return h.check("unlock", nil)
}

// Close releases the handle; the data stays with the file.
func (h *memHandle) Close() error {
	if d := h.lock(); d != nil {
		return d.Close()
	}
	defer h.fs.mu.Unlock()
	if err := h.check("close", nil); err != nil {
		return err
	}
	h.closed = true
	delete(h.m.handles, h)
	h.fs.memoryHandles--
	return nil
}
