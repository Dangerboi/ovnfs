// Package nfs serves a Backend over NFSv3 (RFC 1813, with MOUNT v3) and
// NFSv4.0 (RFC 7530) on one TCP port.
//
// rpc.go frames ONC RPC records and dispatches calls to mount.go, v3*.go and
// v4*.go. File handles are the backend's stable file id and the generation
// Backend.Generation tags it with (16 bytes, big-endian), so they survive
// renames and server restarts; Backend.RetireHandle makes a file's handles
// stale without changing its fileid. NFSv4 clients may
// get read delegations on files the backend calls cacheable; Server.Recall
// takes them back when a file changes outside this server.
//
// Example (not runnable):
//
//	srv := &nfs.Server{FS: backend, Concurrency: 64}
//	go srv.Serve(listener)
//	// a change made through another protocol:
//	srv.Recall(backend.FileID("co/src/main.go"))
package nfs

import (
	"crypto/rand"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/stencil-hq/ovnfs/stats"
)

// Limits both NFS versions advertise.
const (
	// maxIO is the largest READ or WRITE (rtmax and wtmax, maxread and
	// maxwrite): the most Linux and macOS transfer per call.
	maxIO = 1 << 20
	// maxName is the longest file name (NAME_MAX).
	maxName = 255
	// maxLink is the most names a file may have (LINK_MAX on macOS).
	maxLink = 32767
)

// Server exports one Backend to every client that mounts any path.
// Credentials are not checked: the backend's own permissions apply.
type Server struct {
	FS Backend
	// Concurrency bounds the calls of one connection answered at once; zero
	// or one answers them one at a time, in order.
	Concurrency int
	Logf        func(format string, args ...any)
	// Stats times the calls answered, by procedure (group "nfs"); nil times none.
	Stats *stats.Recorder

	once sync.Once
	// verf is this instance's write verifier: it changes when the server
	// restarts, telling clients to resend unstable writes.
	verf    [8]byte
	changes dirChanges // freshness of attributes in paged listings
	// listMu protects both listing caches and serializes publication with
	// RetireRange. Epoch changes only when a range is retired.
	listMu  sync.Mutex
	epoch   atomic.Uint64
	dirs    dirCache
	resumes resumeCache
	connMu  sync.Mutex
	conns   map[*conn]struct{}
	v3      v3State
	v4      v4State
	// timers are the Stats timers of the NFSv3 procedures by number, and of
	// the NFSv4 COMPOUND, set by start; nil without Stats.
	timers struct {
		v3       [v3Count]*stats.Timer
		compound *stats.Timer
	}
}

// Serve accepts connections until the listener fails, and closes it.
func (s *Server) Serve(l net.Listener) error {
	defer l.Close()
	if s.FS == nil {
		return errors.New("nfs: filesystem is required")
	}
	s.start()
	var delay time.Duration
	for {
		nc, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			// Running out of descriptors (EMFILE) passes: back off rather
			// than stop serving, which would leave the mount dead.
			delay = min(max(2*delay, 5*time.Millisecond), time.Second)
			s.logf("nfs: accept: %v", err)
			time.Sleep(delay)
			continue
		}
		delay = 0
		c := &conn{srv: s, nc: nc}
		go c.serve()
	}
}

// start initializes per-instance state once, before the first connection.
func (s *Server) start() {
	s.once.Do(func() {
		_, _ = rand.Read(s.verf[:])
		if s.v3.readers.cache.Max == 0 {
			s.v3.readers.cache.Max = defaultKeptReaders
		}
		s.v4.start(s)
		for proc, p := range v3procs {
			if p.run != nil {
				s.timers.v3[proc] = s.Stats.Timer("nfs", p.name)
			}
		}
		s.timers.compound = s.Stats.Timer("nfs", "COMPOUND")
	})
}

// Recall makes NFSv4 clients return their delegations of file id before it
// changes. Changes through this server recall conflicting delegations
// themselves; the embedder calls Recall for every other change to a file
// Backend.Cacheable admits. It does not block.
func (s *Server) Recall(id uint64) {
	s.changes.markDir(id)
	s.v3.readers.invalidate(id)
	s.v4.recall(id)
}

// recallAttrs narrows a known regular-file mutation to its file id. Directory
// and symlink changes can affect other entries, so keep the broad recall.
func (s *Server) recallAttrs(id uint64, typ fileType) {
	if typ == typeReg {
		s.changes.markFile(id)
		s.v3.readers.invalidate(id)
		s.v4.recall(id)
	} else {
		s.Recall(id)
	}
}

// RecallRange is Recall of every file id in [first, last], for the
// embedder's changes that reach many files at once, such as every file of a
// checkout (a checkout's ids share one range). It does not block.
func (s *Server) RecallRange(first, last uint64) {
	s.changes.markRange(first, last)
	s.v3.readers.invalidateRange(first, last)
	s.v4.recallRange(first, last)
}

// RetireRange recalls [first, last] and releases its directory snapshots and
// continuation points before those ids can be reused. The caller retires the
// range under its registry lock; no listing-cache lock calls the backend.
func (s *Server) RetireRange(first, last uint64) {
	s.RecallRange(first, last)
	s.v3.kept.DropFunc(func(k keptKey) bool { return first <= k.id && k.id <= last })
	s.v3.maxSizes.retireRange(first, last)
	s.forEachConn(func(c *conn) { c.v3sizes.retireRange(first, last) })
	s.listMu.Lock()
	defer s.listMu.Unlock()
	s.epoch.Add(1)
	for _, e := range s.dirs.byVerf {
		id := e.Value.(*cachedListing).dir
		if first <= id && id <= last {
			s.dirs.remove(e)
		}
	}
	for k, e := range s.resumes.byKey {
		if first <= k.dir && k.dir <= last {
			delete(s.resumes.byKey, k)
			s.resumes.lru.Remove(e)
		}
	}
}

func (s *Server) addConn(c *conn) {
	s.connMu.Lock()
	if s.conns == nil {
		s.conns = make(map[*conn]struct{})
	}
	s.conns[c] = struct{}{}
	s.connMu.Unlock()
}

func (s *Server) removeConn(c *conn) {
	s.connMu.Lock()
	delete(s.conns, c)
	s.connMu.Unlock()
}

func (s *Server) forEachConn(fn func(*conn)) {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	for c := range s.conns {
		fn(c)
	}
}

func (s *Server) forgetFileSize(id uint64) {
	s.v3.maxSizes.forget(id)
	s.forEachConn(func(c *conn) { c.v3sizes.forget(id) })
}

// logf logs through Logf, if set.
func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}
