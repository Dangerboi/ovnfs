package nfs

import "time"

// NFSv4.0 read delegations (RFC 7530 section 10.4). A client holding a read
// delegation of a file may serve its opens, attributes and data from its
// cache until the server recalls the delegation, which it does before
// anything changes the file: a conflicting request from another client is
// answered NFS4ERR_DELAY while the recall runs, and the embedder calls
// Server.Recall for changes that do not come through this server.

// Defaults of the injectable delegation timings and limits of v4State.
const (
	// v4MaxDelegs caps outstanding delegations; granting past it recalls
	// the least recently granted one. A build reads thousands of files, and
	// macOS doubles the count with the AppleDouble file it keeps beside
	// each file it creates; a cap within reach of that recalls delegations
	// as fast as it grants them.
	v4MaxDelegs = 1 << 16
	// v4Quiet keeps a file recalled twice within it from being delegated
	// again until it has gone that long without a recall, so a file that
	// keeps changing is not delegated and recalled over and over.
	v4Quiet = 10 * time.Second
	// v4ProbeEvery spaces the CB_NULL probes of a callback path that is
	// down.
	v4ProbeEvery = 10 * time.Second
	// v4RecallRetries and v4RetryDelay (doubling) repeat a CB_RECALL the
	// client answered with an error, for about 1.5 s.
	v4RecallRetries = 5
	v4RetryDelay    = 50 * time.Millisecond
)

// open_delegation_type4 values and the nfsace4 type a read delegation
// carries.
const (
	deleg4None      = 0
	deleg4Read      = 1
	ace4AccessAllow = 0
)

// v4Recalled is when a file's delegations were last recalled, and whether
// that was the second recall within the quiet period. A single recall does
// not stop delegating the file: a change reported late, as by a folder
// watcher reporting a client's own write, recalls a delegation granted
// after the change, and the file is better delegated again at once.
type v4Recalled struct {
	last   time.Time
	thrash bool
}

// v4Grant is a delegation grant under way on a file: the OPENs granting
// one, and whether the file changed meanwhile.
type v4Grant struct {
	opens   int
	changed bool
}

// recall starts the recall of every delegation of file id, without waiting
// (Server.Recall). It takes the lock only while delegations are outstanding
// or being granted, and does nothing before start.
func (st *v4State) recall(id uint64) { st.changed(0, id) }

// probeLocked starts a CB_NULL probe of a client's callback path. Until one
// succeeds the client is granted no delegations.
func (st *v4State) probeLocked(c *v4Client) {
	if c.cb == nil || c.cbProbing {
		return
	}
	c.cbProbing = true
	c.cbProbed = st.now()
	cb := c.cb
	go func() {
		err := cb.null()
		st.mu.Lock()
		defer st.unlock()
		if c.cb != cb {
			return
		}
		c.cbProbing = false
		c.cbUp = err == nil
		if err == nil {
			c.cbDown = false
		} else {
			st.srv.logf("nfs: NFSv4 callback path of client %x is down: %v", c.id, err)
		}
	}()
}

// mayDelegateLocked reports whether client may be granted a delegation of
// file now: its callback path is up, the file is not thrashing
// (v4Recalled), the client holds none of it, and no other client has it
// open for writing.
func (st *v4State) mayDelegateLocked(client, file uint64) bool {
	c := st.clients[client]
	if c == nil || !c.cbUp || c.cbDown {
		return false
	}
	if r := st.recalled[file]; r != nil && r.thrash {
		return false
	}
	if f := st.files[file]; f != nil {
		if f.delegs[client] != nil {
			return false
		}
		for h := range f.opens {
			if h.owner.client != client && h.access&share4Write != 0 {
				return false
			}
		}
	}
	return true
}

// beginGrant notes a grant to client under way on file, before
// Backend.Cacheable is asked, which must not run under the lock; grant or
// endGrant ends it. It returns nil, sparing the question, when the client
// may not have a delegation of the file anyway.
func (st *v4State) beginGrant(client, file uint64) *v4Grant {
	st.mu.Lock()
	defer st.unlock()
	if !st.mayDelegateLocked(client, file) {
		return nil
	}
	g := st.granting[file]
	if g == nil {
		g = &v4Grant{}
		st.granting[file] = g
	}
	g.opens++
	st.grants.Add(1)
	return g
}

// endGrant ends a grant begun with beginGrant without granting.
func (st *v4State) endGrant(file uint64, g *v4Grant) {
	st.mu.Lock()
	defer st.unlock()
	st.endGrantLocked(file, g)
}

// endGrantLocked ends a grant.
func (st *v4State) endGrantLocked(file uint64, g *v4Grant) {
	g.opens--
	if g.opens == 0 {
		delete(st.granting, file)
	}
	st.grants.Add(-1)
}

// active reports whether delegations are outstanding or being granted, read
// without the lock. Grants go first: grant counts a delegation before it
// ends its grant, so a grant finishing meanwhile is seen as one or the
// other.
func (st *v4State) active() bool { return st.grants.Load() != 0 || st.delegs.Load() != 0 }

// grantLocked hands client a read delegation of file if it may still have one and
// nothing changed the file since beginGrant returned g.
//
// The delegation is counted before the grant stops being: active, which
// reads the counts the other way round, then never sees neither.
func (st *v4State) grantLocked(client, file uint64, g *v4Grant, identity *v4Identity) (v4StateID, bool, status) {
	if g.changed {
		return v4StateID{}, false, stOK
	}
	st.sweepLocked()
	if !st.mayDelegateLocked(client, file) {
		return v4StateID{}, false, stOK
	}
	if int(st.delegs.Load()) >= st.maxDelegs {
		st.recallOldestLocked()
	}
	h := st.newHoldLocked(kind4Deleg, v4Owner{client: client}, file, identity)
	if h == nil {
		return v4StateID{}, false, stIO
	}
	h.granted = st.now()
	st.fileLocked(file).delegs[client] = h
	st.delegs.Add(1)
	st.order = append(st.order, h)
	if len(st.order) > 2*int(st.delegs.Load())+64 {
		// Drop the returned and revoked delegations order still lists.
		live := st.order[:0]
		for _, d := range st.order {
			if st.holds[d.id.other] == d {
				live = append(live, d)
			}
		}
		clear(st.order[len(live):])
		st.order = live
	}
	return h.advance(), true, stOK
}

// recallOldestLocked recalls the least recently granted delegation that is
// not being recalled already.
func (st *v4State) recallOldestLocked() {
	for len(st.order) > 0 {
		d := st.order[0]
		st.order[0] = nil
		st.order = st.order[1:]
		if st.holds[d.id.other] == d && d.recalled.IsZero() {
			st.recallLocked(d)
			return
		}
	}
}

// changed starts the recall of the delegations that clients other than
// client (0 for none) hold of file, which has just changed, and refuses the
// grants under way on it. It takes the lock only while delegations are
// outstanding or being granted.
func (st *v4State) changed(client, file uint64) {
	if !st.active() {
		return
	}
	st.mu.Lock()
	defer st.unlock()
	if g := st.granting[file]; g != nil {
		g.changed = true
	}
	st.recallOthersLocked(client, file)
}

// recallOthersLocked starts the recall of the delegations of file that
// clients other than client hold, and counts it as one recall of the file
// for the thrash rule (v4Recalled), however many clients hold one.
func (st *v4State) recallOthersLocked(client, file uint64) {
	f := st.files[file]
	if f == nil {
		return
	}
	started := false
	for holder, d := range f.delegs {
		if holder != client && st.recallLocked(d) {
			started = true
		}
	}
	if !started {
		return
	}
	now := st.now()
	if r := st.recalled[file]; r != nil {
		r.thrash = now.Sub(r.last) <= st.quiet
		r.last = now
	} else {
		st.recalled[file] = &v4Recalled{last: now}
	}
}

// conflict checks a request from client (0 when unknown) that changes file
// against other clients' delegations of it: it starts their recall and
// answers NFS4ERR_DELAY until they are returned or revoked. Changes from the
// holder itself are no conflict.
func (st *v4State) conflict(client, file uint64) status {
	if st.delegs.Load() == 0 {
		return stOK
	}
	st.mu.Lock()
	defer st.unlock()
	st.sweepLocked()
	st.recallOthersLocked(client, file)
	// recallLocked revokes a delegation without a callback path at once.
	if f := st.files[file]; f != nil {
		for holder := range f.delegs {
			if holder != client {
				return stDelay
			}
		}
	}
	return stOK
}

// recallLocked starts the recall of a delegation, reporting whether it was
// not under way already: CB_RECALL goes out on its own goroutine, and the
// delegation is revoked if the callback fails or it is not returned within
// recallTimeout.
func (st *v4State) recallLocked(d *v4Hold) bool {
	if !d.recalled.IsZero() {
		return false
	}
	d.recalled = st.now()
	c := st.clients[d.owner.client]
	if c == nil || c.cb == nil || c.cbDown {
		st.dropLocked(d)
		return true
	}
	st.recalling[d] = struct{}{}
	go st.recallLoop(d, c, c.cb)
	return true
}

// recallLoop sends CB_RECALL of delegation d to client c over cb.
func (st *v4State) recallLoop(d *v4Hold, c *v4Client, cb *v4Callback) {
	id, delay := d.id, st.retryDelay
	// want runs when the call's turn comes, after the calls queued before
	// it over the same path: a delegation returned meanwhile needs no
	// recall, and one whose path failed or was replaced meanwhile is
	// revoked without waiting on a path known to be down.
	want := func() bool {
		st.mu.Lock()
		defer st.unlock()
		if st.holds[id.other] != d {
			return false
		}
		if c.cb != cb || c.cbDown {
			st.dropLocked(d)
			return false
		}
		return true
	}
	// failed runs before the next call over the path, whose want then
	// finds the path down.
	failed := func(err error) {
		st.mu.Lock()
		defer st.unlock()
		if st.holds[id.other] != d {
			return // returned meanwhile
		}
		st.srv.logf("nfs: NFSv4 CB_RECALL of file %d failed, revoking: %v", d.file, err)
		if c.cb == cb {
			c.cbUp = false
			c.cbDown = true
		}
		st.dropLocked(d)
	}
	for attempt := 0; ; attempt++ {
		s, err := cb.recall(id, d.file, want, failed)
		if err != nil {
			return // skipped, or revoked by failed
		}
		st.mu.Lock()
		held := st.holds[id.other] == d
		st.unlock()
		if !held {
			return // returned meanwhile
		}
		// A client recalled before it has processed the OPEN reply
		// granting the delegation does not know it yet and answers
		// with an error (macOS: NFS4ERR_BAD_STATEID). Revoking then
		// would fail its later CLOSE, WRITE and DELEGRETURN and send it
		// into recovery, whose reclaims this server refuses; ask again,
		// as knfsd does, and leave a delegation never returned to
		// recallTimeout.
		if s == stOK || attempt == v4RecallRetries {
			return
		}
		time.Sleep(delay)
		delay *= 2
	}
}

// delegReturn releases a delegation, recalled or not (DELEGRETURN).
func (st *v4State) delegReturn(id v4StateID) status {
	st.mu.Lock()
	defer st.unlock()
	d, s := st.findLocked(kind4Deleg, id)
	if s != stOK {
		return s
	}
	st.dropLocked(d)
	return stOK
}

// checkDeleg validates the delegation stateid an OPEN CLAIM_DELEGATE_CUR
// presents: client's delegation of file.
func (st *v4State) checkDeleg(id v4StateID, client, file uint64) status {
	st.mu.Lock()
	defer st.unlock()
	d, s := st.findLocked(kind4Deleg, id)
	if s != stOK {
		return s
	}
	if d.owner.client != client || d.file != file {
		return stBadStateID
	}
	return stOK
}

// conflict checks a change to file against other clients' delegations
// (v4State.conflict).
func (x *v4compound) conflict(file uint64) status { return x.s.v4.conflict(x.who(), file) }

// conflictName is conflict for the regular file name in directory dir, if
// there is one, and returns its id for changed once the change is made
// (0 while nothing is delegated or being granted, which spares the lookup).
func (x *v4compound) conflictName(dir, name string) (uint64, status) {
	if !x.s.v4.active() {
		return 0, stOK
	}
	p := v4Join(dir, name)
	info, err := x.s.FS.Lstat(p)
	if err != nil || !info.Mode().IsRegular() {
		return 0, stOK
	}
	id := x.s.FS.FileID(p)
	return id, x.conflict(id)
}

// changed recalls other clients' delegations of a file just changed: one
// may have been granted while the change was under way.
func (x *v4compound) changed(file uint64) { x.s.v4.changed(x.who(), file) }

// prepareDelegate begins a possible read delegation before the state lock is
// taken to install OPEN. Backend.Cacheable must run outside that lock; the
// open finishes the grant or releases it on every error path.
func (x *v4compound) prepareDelegate(client uint64, claim, access uint32, file uint64, p string) *v4Grant {
	if access != share4Read || (claim != claim4Null && claim != claim4DelegateCur) {
		return nil
	}
	st := &x.s.v4
	g := st.beginGrant(client, file)
	if g == nil {
		return nil
	}
	if !x.s.FS.Cacheable(p) {
		st.endGrant(file, g)
		return nil
	}
	return g
}

// putDeleg4 appends an open_delegation4: none, or a read delegation with
// recall false and an empty ALLOW nfsace4. The ACE says who may open the
// file without asking the server; the empty one allows nobody, so clients
// keep checking access with the server (macOS discards an ACE whose who it
// cannot map, and Linux servers send the same).
func putDeleg4(e *encoder, id v4StateID, ok bool) {
	if !ok {
		e.u32(deleg4None)
		return
	}
	e.u32(deleg4Read)
	putStateID4(e, id)
	e.bool(false)
	e.u32(ace4AccessAllow)
	e.u32(0) // flag
	e.u32(0) // access mask
	e.str("")
}

// delegReturn returns a delegation (DELEGRETURN).
func (x *v4compound) delegReturn() status {
	id := getStateID4(x.d)
	if x.d.err != nil {
		return stBadXDR
	}
	if _, s := x.current(); s != stOK {
		return s
	}
	return x.s.v4.delegReturn(id)
}
