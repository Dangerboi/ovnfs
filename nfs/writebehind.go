package nfs

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/stencil-hq/ovnfs/internal/kept"
)

// writeBudget bounds the capacity of all queued and in-flight data together.
const writeBudget = 8 << 20

// writeChunk leaves room to join adjacent requests without recopying old data.
const writeChunk = 16 << 10

// writeDelay collects a short burst after the first reply, not after the last.
const writeDelay = time.Millisecond

// writeQueue owns acknowledged bytes until applied. Its lock never covers IO.
type writeQueue struct {
	mu           sync.Mutex
	files        map[uint64]*pendingFile
	faults       map[uint64]error
	holds        map[uint64]int // zero excludes all files; holds may nest
	epoch        uint64         // admission begun before a barrier must revalidate synchronously
	bytes, limit int
	ranges       int            // bounds metadata too, even for scattered one-byte writes
	writers      [64]sync.Mutex // serialize concurrent opens and backpressure writes of an id
}

// pendingFile pins its writer until every queued range has reached it.
type pendingFile struct {
	file       billy.File
	release    func()
	path       string
	gen        uint64
	post       attrs
	runs       []writeRange
	spare      []byte // tail of the last slab, shared even by discontiguous ranges
	charge     int    // total capacity of the slabs backing runs
	done       chan struct{}
	timer      *time.Timer
	running    bool
	hurry      bool // a waiter needs these bytes now, not at the end of writeDelay
	unanswered int
}

// writeRange is one contiguous run of writes in request order.
type writeRange struct {
	off  int64
	data []byte
}

// appendLocked owns data before its RPC buffer goes back to the pool. The
// capacity, not the length, is charged, including while apply is in flight.
func (q *writeQueue) appendLocked(f *pendingFile, off int64, data []byte) bool {
	if len(f.spare) >= len(data) && len(f.runs) != 0 {
		last := &f.runs[len(f.runs)-1]
		if last.off+int64(len(last.data)) == off {
			last.data = append(last.data, data...)
			f.spare = f.spare[len(data):]
			return true
		}
	}
	if q.ranges >= writeBudget/512 {
		return false
	}
	if len(f.spare) < len(data) {
		n := max(len(data), writeChunk)
		limit := q.limit
		if limit == 0 {
			limit = writeBudget
		}
		if n > limit-q.bytes {
			return false
		}
		f.spare = make([]byte, n)
		f.charge += n
		q.bytes += n
	}
	part := f.spare[:len(data)]
	copy(part, data)
	f.runs = append(f.runs, writeRange{off, part})
	q.ranges++
	f.spare = f.spare[len(data):]
	return true
}

// predict publishes only fields NFS derives from a regular write: used is
// size, and atime, ctime and NFSv4 change all derive from this same mtime.
func (f *pendingFile) predict(off int64, n int) (pre, post attrs) {
	pre = f.post
	f.post.size = max(pre.size, uint64(off)+uint64(n))
	now := time.Now().Truncate(time.Microsecond)
	if now.Equal(pre.mtime) {
		now = now.Add(time.Microsecond)
	}
	f.post.mtime = now
	f.unanswered++
	return pre, f.post
}

// answered makes the buffered bytes eligible to apply only after sending their reply.
func (s *Server) answered(f *pendingFile) {
	q := &s.v3.writes
	q.mu.Lock()
	f.unanswered--
	if f.unanswered == 0 && !f.running && f.timer == nil {
		delay := writeDelay
		if f.hurry {
			delay = 0
		}
		f.timer = time.AfterFunc(delay, func() { s.applyPending(f) })
	}
	q.mu.Unlock()
}

// applyPending drains one pinned writer, leaving later arrivals ordered behind
// the batch in progress. Once a range fails, none of its successors is applied.
func (s *Server) applyPending(f *pendingFile) {
	q := &s.v3.writes
	q.mu.Lock()
	f.timer = nil
	f.running = true
	for {
		if f.unanswered != 0 || len(f.runs) == 0 {
			f.running = false
			if f.unanswered != 0 {
				q.mu.Unlock()
				return
			}
			delete(q.files, f.post.fileid)
			close(f.done)
			q.mu.Unlock()
			f.release()
			return
		}
		runs, charge, mtime := f.runs, f.charge, f.post.mtime
		f.runs, f.spare, f.charge = nil, nil, 0
		q.mu.Unlock()
		var err error
		for _, r := range runs {
			if err == nil {
				var n int
				n, err = kept.WriteAt(f.file, r.data, r.off)
				if n == 0 && errors.Is(err, syscall.ESTALE) {
					var replacement billy.File
					var release func()
					replacement, release, f.path, _, err = s.reopenWriter(f.post.fileid, f.gen)
					if err == nil {
						f.release()
						f.file, f.release = replacement, release
						n, err = kept.WriteAt(f.file, r.data, r.off)
					}
				}
				if err == nil && n != len(r.data) {
					err = io.ErrShortWrite
				}
			}
		}
		if err == nil {
			// Resolution of the RPC's original path could predate a rename.
			// No namespace mutation can start now until this worker drains.
			if p, _, live := s.resolve(f.post.fileid, f.gen); live {
				err = s.FS.Chtimes(p, mtime, mtime)
			} else {
				err = syscall.ESTALE
			}
		}
		q.mu.Lock()
		q.bytes -= charge
		q.ranges -= len(runs)
		if err != nil {
			if q.faults == nil {
				q.faults = make(map[uint64]error)
			}
			q.faults[f.post.fileid] = err
			q.bytes -= f.charge
			q.ranges -= len(f.runs)
			f.runs, f.spare, f.charge = nil, nil, 0
			delete(q.files, f.post.fileid)
			close(f.done)
			// An already accepted WRITE may still be sending its reply. Leave
			// running set so its completion cannot start a second worker.
			q.mu.Unlock()
			f.release()
			logf := s.Logf
			if logf == nil {
				logf = log.Printf
			}
			logf("nfs: deferred WRITE to %q failed: %v", f.path, err)
			return
		}
	}
}

// expediteLocked skips the batching window once anything needs the bytes.
func (f *pendingFile) expediteLocked() {
	f.hurry = true
	if f.timer != nil && f.timer.Stop() {
		f.timer.Reset(0)
	}
}

// exclude drains prior writes and prevents new deferrals until release. A
// namespace operation passes zero (renames, aliases and listings can touch
// more than the handle they name); single-file operations pass their id.
func (q *writeQueue) exclude(ctx context.Context, id uint64) error {
	q.mu.Lock()
	if q.holds == nil {
		q.holds = make(map[uint64]int)
	}
	q.holds[id]++
	q.epoch++
	var wait []chan struct{}
	if id == 0 {
		for _, f := range q.files {
			f.expediteLocked()
			wait = append(wait, f.done)
		}
	} else if f := q.files[id]; f != nil {
		f.expediteLocked()
		wait = append(wait, f.done)
	}
	q.mu.Unlock()
	for _, done := range wait {
		select {
		case <-done:
		case <-ctx.Done():
			q.allow(id)
			return ctx.Err()
		}
	}
	return nil
}

// allow releases an exclusion for id, or a registry-wide exclusion for zero.
func (q *writeQueue) allow(id uint64) {
	q.mu.Lock()
	if q.holds[id] == 1 {
		delete(q.holds, id)
	} else {
		q.holds[id]--
	}
	q.mu.Unlock()
}

// Quiesce excludes write-behind through a direct checkout operation. A failed
// apply fails the barrier too, without consuming the client's pending error.
func (s *Server) Quiesce(ctx context.Context) (func(), error) {
	q := &s.v3.writes
	if err := q.exclude(ctx, 0); err != nil {
		return nil, err
	}
	var err error
	q.mu.Lock()
	for _, fault := range q.faults {
		err = fault
		break
	}
	q.mu.Unlock()
	if err != nil {
		q.allow(0)
		return nil, err
	}
	return func() { q.allow(0) }, nil
}

// hasFaults makes a v4 unlink identify even a non-delegated file so its last
// name cannot disappear leaving a permanent, undeliverable checkout error.
func (q *writeQueue) hasFaults() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.faults) != 0
}

// forgetGoneFault keeps a late error through removal of a hard-link alias,
// but releases it when no client can WRITE or COMMIT the id any more.
func (s *Server) forgetGoneFault(id uint64) {
	if s.NoSync && s.v3.writes.readFault(id) != nil {
		if _, live := s.FS.Path(id, s.FS.Generation(id)); !live {
			s.v3.writes.takeFault(id)
		}
	}
}

// readFault refuses to serve incomplete data or retire a handle over the
// resulting size regression before its writer can receive the late error.
func (q *writeQueue) readFault(id uint64) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.faults[id]
}

// takeFault delivers a late error once, to the next WRITE or COMMIT of the file.
func (q *writeQueue) takeFault(id uint64) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	err := q.faults[id]
	delete(q.faults, id)
	return err
}

// writer3 checks a new writer against the handle and pins it across an apply.
// The cold-path lstat rejects symlinks, devices and FIFOs before opening them.
func (s *Server) writer3(id, gen uint64, p string) (billy.File, func(), string, attrs, error) {
	f, release := s.v3.kept.Borrow(keptKey{id: id, path: p})
	if f == nil {
		at, info, err := s.describe(id, gen, p)
		if err != nil {
			return nil, nil, p, attrs{}, err
		}
		if !info.Mode().IsRegular() {
			return nil, nil, p, attrs{}, syscall.EINVAL
		}
		f, p, info, err = s.openHandle(id, gen, at, os.O_WRONLY)
		if err == nil {
			release, err = s.v3.kept.Adopt(keptKey{id: id, path: p}, f)
			if err != nil {
				release()
				return nil, nil, p, attrs{}, err
			}
			return f, release, p, attrsOf(id, info), nil
		}
		if errors.Is(err, os.ErrPermission) {
			f, release = s.v3.kept.Borrow(keptKey{id: id})
		}
		if f == nil {
			return nil, nil, at, attrs{}, err
		}
		p = at
	}
	a, err := s.writerAttrs(id, p, f)
	if err != nil {
		release()
		return nil, nil, p, attrs{}, err
	}
	return f, release, p, a, nil
}

// reopenWriter follows the still-live identity after a freeze splits a hard
// link. A pinned stale writer is dropped, not closed underneath its borrower.
func (s *Server) reopenWriter(id, gen uint64) (billy.File, func(), string, attrs, error) {
	s.v3.kept.DropFunc(func(k keptKey) bool { return k.id == id })
	p, err := s.relocate(id, gen)
	if err != nil {
		return nil, nil, p, attrs{}, err
	}
	return s.writer3(id, gen, p)
}

// writerAttrs uses a path only for billy backends without descriptor stat.
func (s *Server) writerAttrs(id uint64, p string, f billy.File) (attrs, error) {
	a, err := s.fileAttrs(id, f)
	if errors.Is(err, billy.ErrNotSupported) {
		return s.stat(id, p)
	}
	return a, err
}

// writeResult keeps the WCC pair and pin associated with one WRITE reply.
type writeResult struct {
	pre, post attrs
	count     int
	pending   *pendingFile
}

// writeAt3 appends to a pinned writer's queue, or applies synchronously when
// durability, cache exclusion, a set-ID mode, or buffer pressure requires it.
func (s *Server) writeAt3(id, gen uint64, p string, data []byte, off int64, stable bool) (r writeResult, err error) {
	q := &s.v3.writes
	var epoch uint64
	deferOK := s.NoSync && len(data) != 0
	if s.NoSync {
		guard := &q.writers[(id^(id>>32))%uint64(len(q.writers))]
		guard.Lock()
		defer guard.Unlock()
		q.mu.Lock()
		epoch = q.epoch
		deferOK = deferOK && q.holds[0]+q.holds[id] == 0
		if f := q.files[id]; f != nil {
			if deferOK && q.appendLocked(f, off, data) {
				r.pre, r.post = f.predict(off, len(data))
				q.mu.Unlock()
				r.count, r.pending = len(data), f
				return r, nil
			}
			f.expediteLocked()
			q.mu.Unlock()
			<-f.done
			// This write crossed the cap or a barrier. Even if room just
			// reopened, it pays the cost rather than enqueueing again.
			deferOK = false
		} else {
			q.mu.Unlock()
		}
		if err := q.takeFault(id); err != nil {
			return r, err
		}
	}
	f, release, at, pre, err := s.writer3(id, gen, p)
	if errors.Is(err, syscall.ESTALE) {
		f, release, at, pre, err = s.reopenWriter(id, gen)
	}
	if err != nil {
		return r, err
	}
	if deferOK && pre.mode&06000 == 0 {
		q.mu.Lock()
		if epoch == q.epoch && q.holds[0]+q.holds[id] == 0 {
			file := &pendingFile{file: f, release: release, path: at, gen: gen, post: pre}
			if q.appendLocked(file, off, data) {
				file.done = make(chan struct{})
				if q.files == nil {
					q.files = make(map[uint64]*pendingFile)
				}
				q.files[id] = file
				r.pre, r.post = file.predict(off, len(data))
				r.count, r.pending = len(data), file
				q.mu.Unlock()
				return r, nil
			}
		}
		q.mu.Unlock()
	}
	r.pre = pre
	r.count, err = kept.WriteAt(f, data, off)
	if r.count == 0 && errors.Is(err, syscall.ESTALE) {
		release()
		f, release, at, r.pre, err = s.reopenWriter(id, gen)
		if err != nil {
			return r, err
		}
		r.count, err = kept.WriteAt(f, data, off)
	}
	defer release()
	if err == nil && stable {
		err = s.FS.SyncFile(at, id, gen, f)
	}
	if err == nil {
		r.post, _ = s.writerAttrs(id, at, f)
	} // optional WCC post-op attributes
	return r, err
}
