package nfs

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// ONC RPC (RFC 5531) programs this server answers.
const (
	progNFS   = 100003
	progMount = 100005
)

// RPC message fields.
const (
	msgCall           = 0
	msgReply          = 1
	replyAccepted     = 0
	replyDenied       = 1
	rejectRPCMismatch = 0
	rejectAuthError   = 1
	authNone          = 0
	authSys           = 1
	authBadCred       = 1
	maxAuthBytes      = 400
)

// acceptStat is an accepted reply's outcome (accept_stat).
type acceptStat uint32

// Accepted reply outcomes. Only rpcSuccess and rpcProgMismatch carry a body.
const (
	rpcSuccess acceptStat = iota
	rpcProgUnavail
	rpcProgMismatch
	rpcProcUnavail
	rpcGarbageArgs
	rpcSystemErr
)

// Record marking (RFC 5531 section 11).
const (
	lastFragment = 1 << 31
	// maxRecord bounds a request: a WRITE of maxIO and its headers. The
	// record is buffered whole, so a larger bound would let each call in
	// flight pin that much memory.
	maxRecord = maxIO + 64<<10
	// maxPooled bounds the buffers kept for reuse: a 1 MiB READ reply fits.
	maxPooled = 2 << 20
)

// errRecordTooLarge closes a connection whose request exceeds maxRecord.
var errRecordTooLarge = errors.New("nfs: RPC record too large")

// call is one RPC call being answered.
type call struct {
	xid              uint32
	prog, vers, proc uint32
	// args holds the procedure's arguments, after the call header.
	args decoder
	conn *conn
}

// conn is one client connection. Replies may go out in any order: each
// carries its call's xid.
type conn struct {
	srv *Server
	nc  net.Conn
	// Replies queue under qmu; the goroutine holding wmu writes them (send).
	qmu    sync.Mutex
	queued []*[]byte // reply records not yet written, pooled buffers
	wmu    sync.Mutex
	spare  []*[]byte   // the queue's other slice, owned by wmu's holder
	iov    net.Buffers // the records of one write, owned by wmu's holder
	broken bool        // a write failed; guarded by wmu
	// With Server.Concurrency above one, calls are read by one goroutine
	// at a time (read) and answered by it or by workers (serve).
	r       *bufio.Reader
	jobs    chan job
	slots   chan struct{} // one per call being answered
	workers atomic.Int32
	wg      sync.WaitGroup // readers, workers and calls a reader handed over
	inline  atomic.Uint64  // the call a reader answers itself, 0 for none (answerInline)
	since   atomic.Int64   // when that call started, in monotonic nanoseconds (sinceStart)
	seq     atomic.Uint64  // numbers inline calls
	armed   atomic.Bool    // watch is set
	watch   *time.Timer    // runs watchInline
	v3sizes clientReportedSizes
	// v4client is the NFSv4 client last identified on this connection, for
	// requests that name none (an NFSv4.0 client uses its own connections).
	v4client atomic.Uint64
}

// bufs recycles request and reply buffers.
var bufs = sync.Pool{New: func() any { return new([]byte) }}

// putBuf returns b to the pool unless it grew too large to keep.
func putBuf(b *[]byte) {
	if cap(*b) <= maxPooled {
		*b = (*b)[:0]
		bufs.Put(b)
	}
}

// job is one received call and its place in its file's order.
type job struct {
	req  *[]byte
	turn turn
}

// inlineMax is how long a call the reading goroutine answers itself may
// hold up the calls behind it before watchInline hands reading to another
// goroutine; watchInline looks this often while such calls are answered.
const inlineMax = time.Millisecond

// start anchors sinceStart.
var start = time.Now()

// sinceStart is the monotonic time since start.
func sinceStart() int64 { return int64(time.Since(start)) }

// serve answers calls until the client disconnects or a read fails. Up to
// Server.Concurrency calls run at once, since Linux multiplexes a mount's
// I/O over one connection and one slow call must not stall the rest; calls
// on one file keep their arrival order where replies show it (fileOrder).
//
// The reading goroutine answers a quick call (quickProc) itself when it
// waits for no earlier change and no other call is buffered: handing it to a
// worker costs a goroutine wakeup, often a thread wakeup, which took more
// CPU than answering a LOOKUP or GETATTR. Should the call take longer than
// inlineMax, another goroutine takes over reading (watchInline).
func (c *conn) serve() {
	c.srv.addConn(c)
	defer c.srv.removeConn(c)
	defer c.nc.Close()
	c.r = bufio.NewReaderSize(c.nc, 64<<10)
	if c.srv.Concurrency <= 1 {
		var cl call
		var reply encoder
		for {
			req := bufs.Get().(*[]byte)
			rec, err := readRecord(c.r, *req)
			if err != nil {
				putBuf(req)
				c.readFailed(err)
				return
			}
			c.answer(rec, &cl, &reply)
			putBuf(req)
		}
	}
	// jobs is unbuffered: a send succeeds only when a worker is idle.
	// Workers start lazily up to Concurrency and exit when jobs closes,
	// after finishing every accepted call. Each call takes a slot before
	// it is answered, so at most Concurrency calls run at once. A call
	// enters its file's order before it takes a slot, so it only ever waits
	// for calls already being answered.
	c.jobs = make(chan job)
	c.slots = make(chan struct{}, c.srv.Concurrency)
	c.watch = time.AfterFunc(time.Hour, c.watchInline)
	c.watch.Stop()
	defer c.watch.Stop()
	c.wg.Add(1)
	c.read()
	c.wg.Wait()
}

// read reads calls and dispatches them until the client disconnects or a
// read fails, then closes jobs; or until watchInline hands reading to
// another goroutine while this one answers a call itself.
func (c *conn) read() {
	defer c.wg.Done()
	var cl call
	var reply encoder
	for {
		req := bufs.Get().(*[]byte)
		rec, err := readRecord(c.r, *req)
		if err != nil {
			putBuf(req)
			c.readFailed(err)
			close(c.jobs)
			return
		}
		*req = rec
		t, quick := c.turnOf(rec)
		c.slots <- struct{}{}
		if quick && t.after == nil && c.r.Buffered() == 0 {
			reading := c.answerInline(rec, &cl, &reply)
			t.leave()
			putBuf(req)
			<-c.slots
			if !reading {
				return
			}
			continue
		}
		j := job{req: req, turn: t}
		select {
		case c.jobs <- j:
		default:
			if c.workers.Load() < int32(c.srv.Concurrency) {
				c.workers.Add(1)
				c.wg.Add(1)
				go c.work(j)
			} else {
				c.jobs <- j
			}
		}
	}
}

// work answers first, then calls from jobs until it closes.
func (c *conn) work(first job) {
	defer c.wg.Done()
	var cl call
	var reply encoder
	for j := first; j.req != nil; j = <-c.jobs {
		j.turn.wait()
		c.answer(*j.req, &cl, &reply)
		j.turn.leave()
		putBuf(j.req)
		<-c.slots
	}
}

// answerInline answers a call on the reading goroutine. It reports whether
// that goroutine still reads: false when the call outlasted inlineMax and
// watchInline started another reader. The watch is set at most once per
// inlineMax rather than for each call: setting a timer for every call woke
// the network poller for every call.
func (c *conn) answerInline(rec []byte, cl *call, reply *encoder) (reading bool) {
	seq := c.seq.Add(1)
	c.since.Store(sinceStart())
	c.inline.Store(seq)
	if c.armed.CompareAndSwap(false, true) {
		c.watch.Reset(inlineMax)
	}
	c.answer(rec, cl, reply)
	return c.inline.CompareAndSwap(seq, 0)
}

// watchInline starts another reader when the reader has been answering one
// call itself for inlineMax, so a slow call does not stall the calls behind
// it, and looks again later while a reader answers a call itself.
func (c *conn) watchInline() {
	c.armed.Store(false)
	seq := c.inline.Load()
	if seq == 0 {
		return // answerInline sets the watch again
	}
	if sinceStart()-c.since.Load() >= int64(inlineMax) && c.inline.CompareAndSwap(seq, 0) {
		c.wg.Add(1)
		go c.read()
		return
	}
	if c.armed.CompareAndSwap(false, true) {
		c.watch.Reset(inlineMax)
	}
}

// readFailed logs a read error other than the client disconnecting.
func (c *conn) readFailed(err error) {
	if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
		c.srv.logf("nfs: read from %v: %v", c.nc.RemoteAddr(), err)
	}
}

// quickProc marks the NFSv3 procedures the reading goroutine may answer
// itself (serve): those that usually take microseconds. One that turns out
// slow, such as the first WRITE copying a file up, holds up the calls behind
// it for at most inlineMax.
var quickProc = [...]bool{
	v3Null:     true,
	v3GetAttr:  true,
	v3Lookup:   true,
	v3Access:   true,
	v3Readlink: true,
	v3Write:    true,
	v3Create:   true,
	v3FSStat:   true,
	v3FSInfo:   true,
	v3PathConf: true,
	v3Commit:   true,
}

// turnOf enters an NFSv3 call in its file's order: SETATTR and WRITE change
// the file; GETATTR, READ and COMMIT report its attributes. Other
// calls, and calls that do not decode, get the zero turn; answer rejects
// the malformed ones. quick reports an NFSv3 call of a quickProc.
func (c *conn) turnOf(rec []byte) (t turn, quick bool) {
	d := decoder{buf: rec}
	d.u32() // xid
	if d.u32() != msgCall || d.u32() != 2 || d.u32() != progNFS || d.u32() != 3 {
		return turn{}, false
	}
	var changes bool
	proc := d.u32()
	quick = proc < uint32(len(quickProc)) && quickProc[proc]
	switch proc {
	case v3SetAttr, v3Write:
		changes = true
	case v3GetAttr, v3Read, v3Commit:
	default:
		return turn{}, quick
	}
	d.u32()
	d.opaque(maxAuthBytes) // credential
	d.u32()
	d.opaque(maxAuthBytes) // verifier
	id, _, ok := d.fh(maxFH3)
	if d.err != nil || !ok {
		return turn{}, quick
	}
	return c.srv.v3.order.enter(id, changes), quick
}

// readRecord reads one record into buf, joining its fragments.
func readRecord(r *bufio.Reader, buf []byte) ([]byte, error) {
	buf = buf[:0]
	for {
		mark, err := r.Peek(4)
		if err != nil {
			if (len(mark) != 0 || len(buf) != 0) && errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return nil, err
		}
		h := binary.BigEndian.Uint32(mark)
		_, _ = r.Discard(4)
		n := int(h &^ lastFragment)
		if len(buf)+n > maxRecord {
			return nil, errRecordTooLarge
		}
		at := len(buf)
		buf = slices.Grow(buf, n)[:at+n]
		if _, err := io.ReadFull(r, buf[at:]); err != nil {
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return nil, err
		}
		if h&lastFragment != 0 {
			return buf, nil
		}
	}
}

// answer decodes one call, dispatches it and sends the reply. A record that
// is not a well-formed call closes the connection: its stream cannot be
// trusted to stay in step. Each worker owns its call and encoder storage;
// dispatch never retains them after returning.
func (c *conn) answer(rec []byte, cl *call, e *encoder) {
	d := decoder{buf: rec}
	xid, mtype, rpcvers := d.u32(), d.u32(), d.u32()
	*cl = call{xid: xid, prog: d.u32(), vers: d.u32(), proc: d.u32(), conn: c}
	flavor := d.u32()
	d.opaque(maxAuthBytes)
	d.u32()
	d.opaque(maxAuthBytes)
	if d.err != nil || mtype != msgCall {
		c.srv.logf("nfs: malformed call from %v", c.nc.RemoteAddr())
		c.nc.Close()
		return
	}

	rb := bufs.Get().(*[]byte)
	*e = encoder{
		buf:      *rb,
		sizes:    &c.v3sizes,
		maxSizes: &c.srv.v3.maxSizes,
	}
	e.sampleSizes()
	e.u32(0) // record mark, set below
	e.u32(xid)
	e.u32(msgReply)
	switch {
	case rpcvers != 2:
		e.u32(replyDenied)
		e.u32(rejectRPCMismatch)
		e.u32(2)
		e.u32(2)
	case flavor != authNone && flavor != authSys:
		e.u32(replyDenied)
		e.u32(rejectAuthError)
		e.u32(authBadCred)
	default:
		e.u32(replyAccepted)
		e.u32(authNone) // verifier
		e.u32(0)
		at := e.len()
		e.u32(0) // accept_stat, set below
		cl.args = d
		stat := c.srv.dispatch(cl, e)
		if stat != rpcSuccess && stat != rpcProgMismatch {
			e.truncate(at + 4)
		}
		e.put32(at, uint32(stat))
	}
	e.put32(0, lastFragment|uint32(e.len()-4))
	*rb = e.buf
	sent := c.send(rb, e.sizePending || e.pending != nil)
	if e.pending != nil {
		c.srv.answered(e.pending)
		e.pending = nil
	}
	e.finishSize(sent)
	cl.args = decoder{}
	e.buf = nil
}

// send queues one reply record, taking over its pooled buffer, and writes
// the queue unless another goroutine is writing, which then writes this
// record too: replies finished together go out in one writev. A failed write
// closes the connection. With wait, send blocks until the record is written
// and reports whether it was; otherwise it reports true once queued.
func (c *conn) send(rec *[]byte, wait bool) bool {
	c.qmu.Lock()
	c.queued = append(c.queued, rec)
	c.qmu.Unlock()
	sent := true
	for {
		if wait {
			c.wmu.Lock()
		} else if !c.wmu.TryLock() {
			// The holder looks at the queue again after unlocking.
			return sent
		}
		c.flush()
		if wait {
			sent, wait = !c.broken, false
		}
		c.wmu.Unlock()
		c.qmu.Lock()
		empty := len(c.queued) == 0
		c.qmu.Unlock()
		if empty {
			return sent
		}
	}
}

// flush writes the queued records until the queue is empty, returning their
// buffers to the pool. The caller holds wmu.
func (c *conn) flush() {
	for {
		c.qmu.Lock()
		q := c.queued
		c.queued = c.spare
		c.qmu.Unlock()
		if len(q) == 0 {
			c.spare = q
			return
		}
		if !c.broken {
			var err error
			if len(q) == 1 {
				_, err = c.nc.Write(*q[0])
			} else {
				for _, b := range q {
					c.iov = append(c.iov, *b)
				}
				iov := c.iov
				_, err = iov.WriteTo(c.nc)
				clear(c.iov)
				c.iov = c.iov[:0]
			}
			if err != nil {
				c.broken = true
				c.nc.Close()
			}
		}
		for _, b := range q {
			putBuf(b)
		}
		clear(q)
		c.spare = q[:0]
	}
}

// dispatch answers a call, appending the result body to res, and returns the
// accept_stat to reply with. The program handlers (mount, nfs3, nfs4) decode
// their arguments from c.args and return rpcGarbageArgs when they do not
// decode, rpcProcUnavail for unknown procedures, and rpcSuccess otherwise.
func (s *Server) dispatch(c *call, res *encoder) acceptStat {
	switch c.prog {
	case progMount:
		if c.vers == 3 {
			return s.mount(c, res)
		}
		res.u32(3)
		res.u32(3)
		return rpcProgMismatch
	case progNFS:
		switch c.vers {
		case 3:
			return s.nfs3(c, res)
		case 4:
			return s.nfs4(c, res)
		}
		res.u32(3)
		res.u32(4)
		return rpcProgMismatch
	}
	return rpcProgUnavail
}
