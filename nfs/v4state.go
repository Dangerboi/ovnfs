package nfs

import (
	"cmp"
	"crypto/rand"
	"encoding/binary"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-git/go-billy/v5"
)

// NFSv4.0 state (RFC 7530 section 9): client ids and their leases, and the
// open, lock and delegation state their stateids name.
//
// Opens take no share reservations and byte-range locks are advisory, so
// neither is enforced against READ or WRITE. A stateid's other field names
// one state: an open owner's opens of one file, a lock owner's locks on
// one, or a delegation. Its seqid is the state's generation, which starts at
// 1 and moves on with every operation that changes the state: OPEN of the
// same file by the same owner, OPEN_CONFIRM, OPEN_DOWNGRADE and CLOSE for an
// open; LOCK and LOCKU for locks.
//
// The seqids clients present, of owners and of stateids, are not checked:
// requests are applied as they come, none is answered from a replay cache,
// and a stateid of an older generation is as good as the current one, so
// the READs and WRITEs a client has in flight keep working while an OPEN of
// the same file moves its stateid on.

// Lease timings.
const (
	// v4Lease is the default lease period (lease_time).
	v4Lease = 90 * time.Second
	// v4LeasePeriods is how many lease periods a client may go without
	// renewing before it expires and its state is released. RFC 7530
	// allows expiry after one; being generous costs little and forgives
	// slow clients, which learn of it when they next show up.
	v4LeasePeriods = 3
	// v4ExpiredMemory is how many expiry windows an expired client id is
	// answered NFS4ERR_EXPIRED; after that it is NFS4ERR_STALE_CLIENTID,
	// which also sends the client to recover.
	v4ExpiredMemory = 10
	// v4SweepEvery spaces the scans for expired leases and overdue
	// recalls, which run on the way through a state operation.
	v4SweepEvery = time.Second
)

// v4State is the server's NFSv4 state. Every method is safe for concurrent
// use once start has run; recall is a no-op before.
type v4State struct {
	// Timings and limits, set before start for tests; zero selects the
	// default. grace is how long after start reclaims are granted;
	// recallTimeout how long a recalled delegation may stay unreturned;
	// probeEvery how often a failed callback path is probed again; quiet
	// how long a file recalled twice within it stays undelegated;
	// retryDelay the first pause before a CB_RECALL answered with an error
	// is sent again, doubling after; maxDelegs the cap on outstanding
	// delegations; now the clock leases and recalls run on.
	lease, grace, recallTimeout, probeEvery, quiet, retryDelay time.Duration
	maxDelegs                                                  int
	now                                                        func() time.Time

	srv *Server
	// delegs counts outstanding delegations and grants the grants under
	// way, for recall to read without the lock.
	delegs atomic.Int64
	grants atomic.Int64
	// dirs serializes directory changes for atomic change_info4.
	dirs v4DirLocks

	mu sync.Mutex
	// boot tells this instance's client ids and stateids from those a
	// client kept across a restart: it is the high half of every client
	// id and the first bytes of every stateid's other field.
	boot     uint32
	graceEnd time.Time
	// clients are the confirmed client ids, byName indexes them by the
	// client's name, and unconfirmed are those SETCLIENTID handed out and
	// SETCLIENTID_CONFIRM has not yet confirmed.
	clients     map[uint64]*v4Client
	byName      map[string]*v4Client
	unconfirmed map[uint64]*v4Client
	// expired remembers when client ids expired, so a returning client is
	// told NFS4ERR_EXPIRED rather than NFS4ERR_STALE_CLIENTID.
	expired    map[uint64]time.Time
	lastClient uint32
	// holds indexes every state by its stateid's other field, owners an
	// owner's open or lock state by file id.
	holds     map[v4Other]*v4Hold
	owners    map[v4OwnerKey]map[uint64]*v4Hold
	counter   uint64
	lastSweep time.Time
	// files indexes opens and delegations by file id, for the conflict
	// checks delegations need.
	files map[uint64]*v4File
	// recalling are the delegations being recalled, revoked unless
	// returned within recallTimeout; recalled remembers when files'
	// delegations were last recalled (v4Recalled); granting are the grants
	// under way, by file id; order lists delegations as granted, oldest
	// first, for the cap to recall from.
	recalling map[*v4Hold]struct{}
	recalled  map[uint64]*v4Recalled
	granting  map[uint64]*v4Grant
	order     []*v4Hold
	locks     v4Locks
	// closing are the files to close once the lock is released (unlock).
	closing   []billy.File
	releasing []func()
}

// v4Client is a client id the server has handed out: its SETCLIENTID
// arguments, the verifier that confirms it, and when its lease was last
// renewed.
type v4Client struct {
	id      uint64
	name    string
	verf    [8]byte
	confirm [8]byte
	renewed time.Time
	// cb is the callback path from SETCLIENTID, nil without a usable one.
	// cbUp says a CB_NULL probe got through and no callback failed since;
	// cbDown that a callback failed, which RENEW reports until a probe
	// gets through again.
	cb        *v4Callback
	cbUp      bool
	cbDown    bool
	cbProbing bool
	cbProbed  time.Time
}

// v4Other is a stateid's other field.
type v4Other [12]byte

// v4StateID is a stateid4.
type v4StateID struct {
	seqid uint32
	other v4Other
}

// The special stateids READ, WRITE and SETATTR may present without holding
// state: all zeros for anonymous access, all ones to bypass locks.
var (
	stateid4Anon   = v4StateID{}
	stateid4Bypass = v4StateID{seqid: ^uint32(0), other: v4Other{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}}
)

// getStateID4 decodes a stateid4.
func getStateID4(d *decoder) v4StateID {
	id := v4StateID{seqid: d.u32()}
	copy(id.other[:], d.fixed(len(id.other)))
	return id
}

// putStateID4 appends a stateid4.
func putStateID4(e *encoder, id v4StateID) {
	e.u32(id.seqid)
	e.fixed(id.other[:])
}

// v4Kind is the kind of state a stateid names.
type v4Kind uint8

// State kinds.
const (
	kind4Open v4Kind = iota + 1
	kind4Lock
	kind4Deleg
)

// v4Owner is an open_owner4 or lock_owner4: the client id and the
// client-chosen owner bytes. A delegation's owner is its client alone.
type v4Owner struct {
	client uint64
	name   string
}

// getOwner4 decodes an open_owner4 or lock_owner4.
func getOwner4(d *decoder) v4Owner {
	return v4Owner{client: d.u64(), name: string(d.opaque(v4OpaqueLimit))}
}

// v4OwnerKey keeps open owners and lock owners apart: a client may use the
// same bytes for both.
type v4OwnerKey struct {
	kind  v4Kind
	owner v4Owner
}

// v4Hold is the state behind one stateid.
type v4Hold struct {
	release func()
	kind    v4Kind
	owner   v4Owner
	file    uint64
	id      v4StateID
	// access is an open's share access bits.
	access uint32
	// granted and recalled are when a delegation was handed out and when
	// its recall started (zero while it is not being recalled).
	granted  time.Time
	recalled time.Time
}

// advance moves a state to its next generation and returns its stateid.
func (h *v4Hold) advance() v4StateID {
	h.id.seqid++
	return h.id
}

// v4File is the open and delegation state of one file.
type v4File struct {
	opens  map[*v4Hold]struct{}
	delegs map[uint64]*v4Hold // by client id
	// kept is the file kept open for WRITE (see keepWritable).
	kept *v4Kept
}

// writers counts the opens of the file for writing.
func (f *v4File) writers() int {
	n := 0
	for h := range f.opens {
		if h.access&share4Write != 0 {
			n++
		}
	}
	return n
}

// unlock releases the state lock, then closes the files let go of under
// it: closing a backend file may take the backend's locks and stat, which
// must not stall every other state operation and Server.Recall.
func (st *v4State) unlock() {
	closing := st.closing
	st.closing = nil
	releasing := st.releasing
	st.releasing = nil
	st.mu.Unlock()
	for _, release := range releasing {
		release()
	}
	for _, f := range closing {
		_ = f.Close()
	}
}

// start readies the state before the first connection: defaults, the boot
// value, and the grace period during which clients reclaim what they held
// before a restart. Leases and recalls are swept on the way through state
// operations, so nothing runs in the background.
func (st *v4State) start(s *Server) {
	st.srv = s
	if st.now == nil {
		st.now = time.Now
	}
	st.lease = cmp.Or(st.lease, v4Lease)
	st.grace = cmp.Or(st.grace, st.lease)
	st.recallTimeout = cmp.Or(st.recallTimeout, st.lease)
	st.probeEvery = cmp.Or(st.probeEvery, v4ProbeEvery)
	st.quiet = cmp.Or(st.quiet, v4Quiet)
	st.retryDelay = cmp.Or(st.retryDelay, v4RetryDelay)
	st.maxDelegs = cmp.Or(st.maxDelegs, v4MaxDelegs)

	var boot [4]byte
	_, _ = rand.Read(boot[:])
	st.mu.Lock()
	defer st.unlock()
	st.boot = binary.BigEndian.Uint32(boot[:])
	st.graceEnd = st.now().Add(st.grace)
	st.clients = make(map[uint64]*v4Client)
	st.byName = make(map[string]*v4Client)
	st.unconfirmed = make(map[uint64]*v4Client)
	st.expired = make(map[uint64]time.Time)
	st.holds = make(map[v4Other]*v4Hold)
	st.owners = make(map[v4OwnerKey]map[uint64]*v4Hold)
	st.files = make(map[uint64]*v4File)
	st.recalling = make(map[*v4Hold]struct{})
	st.recalled = make(map[uint64]*v4Recalled)
	st.granting = make(map[uint64]*v4Grant)
	st.locks = newLocks4(v4MaxLocksPerFile, v4MaxLocksPerClient)
}

// expiry is how long a client may go without renewing its lease.
func (st *v4State) expiry() time.Duration { return v4LeasePeriods * st.lease }

// inGrace reports whether the grace period after start is still on, during
// which clients may reclaim their opens and locks.
func (st *v4State) inGrace() bool {
	st.mu.Lock()
	defer st.unlock()
	return st.now().Before(st.graceEnd)
}

// setClientID hands out a client id for a client's name and boot verifier
// (SETCLIENTID). A client presenting the verifier it was confirmed with gets
// the same id back; a new verifier means the client rebooted, and it gets a
// new id, which replaces the old one and its state once confirmed.
func (st *v4State) setClientID(name string, verf [8]byte, cb *v4Callback) (uint64, [8]byte) {
	var confirm [8]byte
	_, _ = rand.Read(confirm[:])
	st.mu.Lock()
	defer st.unlock()
	st.sweepLocked()

	var id uint64
	if c := st.byName[name]; c != nil && c.verf == verf {
		id = c.id
	} else {
		st.lastClient++
		id = uint64(st.boot)<<32 | uint64(st.lastClient)
	}
	for uid, u := range st.unconfirmed {
		if u.name == name {
			u.cb.close()
			delete(st.unconfirmed, uid)
		}
	}
	st.unconfirmed[id] = &v4Client{id: id, name: name, verf: verf, confirm: confirm, renewed: st.now(), cb: cb}
	return id, confirm
}

// confirmClientID confirms a client id (SETCLIENTID_CONFIRM), starting its
// lease and probing its callback path. It releases the state of the id it
// replaces; a retransmitted confirmation just renews the lease.
func (st *v4State) confirmClientID(id uint64, confirm [8]byte) status {
	st.mu.Lock()
	defer st.unlock()
	st.sweepLocked()

	if u := st.unconfirmed[id]; u != nil && u.confirm == confirm {
		delete(st.unconfirmed, id)
		if old := st.byName[u.name]; old != nil && old.id != id {
			st.dropClientLocked(old.id)
		} else if old != nil {
			// The same client id with a new callback path: its
			// delegations stay, recalled through the new path.
			old.cb.close()
		}
		u.renewed = st.now()
		st.clients[id] = u
		st.byName[u.name] = u
		st.probeLocked(u)
		return stOK
	}
	if c := st.clients[id]; c != nil && c.confirm == confirm {
		c.renewed = st.now()
		return stOK
	}
	return stStaleClientID
}

// renew renews a client's lease (RENEW). A client whose callback path is
// not up is probed again, at most every probeEvery. One whose callback
// failed is also told NFS4ERR_CB_PATH_DOWN until a probe gets through; one
// whose first probe failed is not, as macOS goes into state recovery on
// any RENEW error, and that client lost nothing.
func (st *v4State) renew(client uint64) status {
	st.mu.Lock()
	defer st.unlock()
	if s := st.checkClientLocked(client); s != stOK {
		return s
	}
	c := st.clients[client]
	if !c.cbUp && st.now().Sub(c.cbProbed) >= st.probeEvery {
		st.probeLocked(c)
	}
	if c.cbDown {
		return stCBPathDown
	}
	return stOK
}

// checkClient validates the client id an OPEN, LOCKT or RELEASE_LOCKOWNER
// presents, renewing its lease.
func (st *v4State) checkClient(client uint64) status {
	st.mu.Lock()
	defer st.unlock()
	return st.checkClientLocked(client)
}

// checkClientLocked validates a client id and renews its lease. A client
// whose lease ran out is told NFS4ERR_EXPIRED, and one this instance never
// confirmed NFS4ERR_STALE_CLIENTID, so the client knows its state is gone
// and recovers.
func (st *v4State) checkClientLocked(client uint64) status {
	st.sweepLocked()
	now := st.now()
	if c := st.clients[client]; c != nil {
		if now.Sub(c.renewed) > st.expiry() {
			st.expireClientLocked(client)
			return stExpired
		}
		c.renewed = now
		return stOK
	}
	if _, ok := st.expired[client]; ok {
		return stExpired
	}
	return stStaleClientID
}

// sweepLocked expires clients that stopped renewing, so their locks stop
// blocking others, revokes delegations not returned in time, and forgets
// what is old enough not to matter. It runs at most every v4SweepEvery.
func (st *v4State) sweepLocked() {
	now := st.now()
	if now.Sub(st.lastSweep) < v4SweepEvery {
		return
	}
	st.lastSweep = now

	for id, c := range st.clients {
		if now.Sub(c.renewed) > st.expiry() {
			st.expireClientLocked(id)
		}
	}
	for id, c := range st.unconfirmed {
		if now.Sub(c.renewed) > st.expiry() {
			c.cb.close()
			delete(st.unconfirmed, id)
		}
	}
	for id, when := range st.expired {
		if now.Sub(when) > v4ExpiredMemory*st.expiry() {
			delete(st.expired, id)
		}
	}
	for h := range st.recalling {
		if now.Sub(h.recalled) > st.recallTimeout {
			st.srv.logf("nfs: NFSv4 delegation of file %d not returned in time, revoked", h.file)
			st.dropLocked(h)
		}
	}
	for file, r := range st.recalled {
		if now.Sub(r.last) > st.quiet {
			delete(st.recalled, file)
		}
	}
}

// expireClientLocked ends a client's lease, releasing its state, and
// remembers that it expired.
func (st *v4State) expireClientLocked(client uint64) {
	st.dropClientLocked(client)
	st.expired[client] = st.now()
}

// dropClientLocked forgets a client id and releases its state: opens,
// locks and delegations.
func (st *v4State) dropClientLocked(client uint64) {
	for _, h := range st.holds {
		if h.owner.client == client {
			st.dropLocked(h)
		}
	}
	if c := st.clients[client]; c != nil {
		c.cb.close()
		delete(st.clients, client)
		if st.byName[c.name] == c {
			delete(st.byName, c.name)
		}
	}
	st.locks.releaseClient(client)
}

// dropLocked forgets a state.
func (st *v4State) dropLocked(h *v4Hold) {
	if h.release != nil {
		st.releasing = append(st.releasing, h.release)
		h.release = nil
	}
	delete(st.holds, h.id.other)
	st.unindexLocked(h)
	key := v4OwnerKey{h.kind, h.owner}
	if files, ok := st.owners[key]; ok && files[h.file] == h {
		delete(files, h.file)
		if len(files) == 0 {
			delete(st.owners, key)
		}
	}
}

// unindexLocked removes an open or delegation from the file index.
func (st *v4State) unindexLocked(h *v4Hold) {
	f := st.files[h.file]
	if f == nil {
		return
	}
	switch h.kind {
	case kind4Open:
		delete(f.opens, h)
		st.releaseKeptLocked(f)
	case kind4Deleg:
		if f.delegs[h.owner.client] == h {
			delete(f.delegs, h.owner.client)
			delete(st.recalling, h)
			st.delegs.Add(-1)
		}
	}
	if len(f.opens) == 0 && len(f.delegs) == 0 {
		delete(st.files, h.file)
	}
}

// fileLocked returns the state of file id, creating it.
func (st *v4State) fileLocked(id uint64) *v4File {
	f := st.files[id]
	if f == nil {
		f = &v4File{opens: make(map[*v4Hold]struct{}), delegs: make(map[uint64]*v4Hold)}
		st.files[id] = f
	}
	return f
}

// v4Identity is acquired without the state lock. Publication consumes the
// hold; unused or failed preparations are released after the transaction.
type v4Identity struct{ release func() }

func (i *v4Identity) drop() {
	if i != nil && i.release != nil {
		i.release()
		i.release = nil
	}
}

func (st *v4State) acquireIdentity(file uint64) (*v4Identity, status) {
	i := &v4Identity{}
	if fs, ok := st.srv.FS.(interface{ HoldFileID(uint64) func() }); ok {
		i.release = fs.HoldFileID(file)
	}
	if identityError(st.srv.FS) != nil {
		i.drop()
		return nil, stIO
	}
	return i, stOK
}

// newHoldLocked publishes a prepared identity with a fresh stateid.
func (st *v4State) newHoldLocked(kind v4Kind, owner v4Owner, file uint64, identity *v4Identity) *v4Hold {
	if identity == nil {
		return nil
	}
	h := &v4Hold{kind: kind, owner: owner, file: file, release: identity.release}
	identity.release = nil
	st.counter++
	binary.BigEndian.PutUint32(h.id.other[0:4], st.boot)
	binary.BigEndian.PutUint64(h.id.other[4:12], st.counter)
	st.holds[h.id.other] = h
	return h
}

// ownerHoldLocked returns owner's open or lock state of file, creating it.
func (st *v4State) ownerHoldLocked(kind v4Kind, owner v4Owner, file uint64, identity *v4Identity) *v4Hold {
	key := v4OwnerKey{kind, owner}
	if h := st.owners[key][file]; h != nil {
		return h
	}
	h := st.newHoldLocked(kind, owner, file, identity)
	if h == nil {
		return nil
	}
	if st.owners[key] == nil {
		st.owners[key] = make(map[uint64]*v4Hold)
	}
	st.owners[key][file] = h
	return h
}

// findLocked returns the state of the given kind a presented stateid names,
// renewing its client's lease. A stateid from before a restart is
// NFS4ERR_STALE_STATEID, and one of a client whose lease expired
// NFS4ERR_EXPIRED.
func (st *v4State) findLocked(kind v4Kind, id v4StateID) (*v4Hold, status) {
	if binary.BigEndian.Uint32(id.other[0:4]) != st.boot {
		return nil, stStaleStateID
	}
	h := st.holds[id.other]
	if h == nil || h.kind != kind {
		return nil, stBadStateID
	}
	if s := st.checkClientLocked(h.owner.client); s != stOK {
		return nil, s
	}
	return h, stOK
}

// checkIO validates the stateid READ, WRITE or SETATTR of file presents,
// renewing its client's lease (RFC 7530 section 9.5), and returns the client
// it belongs to and, if write, the file kept for it (keptLocked), which the
// caller lets go of with release. A stateid of another file is
// NFS4ERR_BAD_STATEID. The special stateids need no state and name no
// client. A read delegation's stateid serves READ only: write is a WRITE or
// a SETATTR of the size.
func (st *v4State) checkIO(id v4StateID, file uint64, write bool) (uint64, *v4Kept, status) {
	if id == stateid4Anon || id == stateid4Bypass {
		return 0, nil, stOK
	}
	st.mu.Lock()
	defer st.unlock()
	if binary.BigEndian.Uint32(id.other[0:4]) != st.boot {
		return 0, nil, stStaleStateID
	}
	h := st.holds[id.other]
	if h == nil || h.file != file {
		return 0, nil, stBadStateID
	}
	if s := st.checkClientLocked(h.owner.client); s != stOK {
		return 0, nil, s
	}
	if h.kind == kind4Deleg && write {
		return 0, nil, stOpenMode
	}
	var kept *v4Kept
	if write {
		kept = st.keptLocked(h)
	}
	return h.owner.client, kept, stOK
}

// open records an OPEN of file by owner for share access, which adds to
// what the owner opened it for.
func (st *v4State) open(owner v4Owner, file uint64, access uint32) (v4StateID, status) {
	id, _, _, s := st.openDelegated(owner, file, access, nil)
	return id, s
}

// openDelegated publishes the open and delegation together. A failed second
// acquisition restores an existing owner's access and stateid under the same
// lock, or drops a new open, before any concurrent operation can use it.
func (st *v4State) openDelegated(owner v4Owner, file uint64, access uint32, g *v4Grant) (v4StateID, v4StateID, bool, status) {
	if g != nil {
		defer st.endGrant(file, g)
	}
	identity, s := st.acquireIdentity(file)
	if s != stOK {
		return v4StateID{}, v4StateID{}, false, s
	}
	defer identity.drop()
	var delegation *v4Identity
	if g != nil {
		delegation, s = st.acquireIdentity(file)
		if s != stOK {
			return v4StateID{}, v4StateID{}, false, s
		}
		defer delegation.drop()
	}
	st.mu.Lock()
	defer st.unlock()
	// Lease, owner and grant eligibility are checked again after index I/O.
	if s := st.checkClientLocked(owner.client); s != stOK {
		return v4StateID{}, v4StateID{}, false, s
	}
	h := st.ownerHoldLocked(kind4Open, owner, file, identity)
	if h == nil {
		return v4StateID{}, v4StateID{}, false, stIO
	}
	previous, previousAccess := h.id, h.access
	if h.access == 0 {
		st.fileLocked(file).opens[h] = struct{}{}
	}
	h.access |= access
	var did v4StateID
	var delegated bool
	if g != nil {
		var s status
		did, delegated, s = st.grantLocked(owner.client, file, g, delegation)
		if s != stOK {
			if previousAccess == 0 {
				st.dropLocked(h)
			} else {
				h.id, h.access = previous, previousAccess
			}
			return v4StateID{}, v4StateID{}, false, s
		}
	}
	return h.advance(), did, delegated, stOK
}

// updateOpen advances an open for OPEN_CONFIRM or OPEN_DOWNGRADE, which
// narrows its share access to access unless that is zero. Access the open
// does not hold is NFS4ERR_INVAL (RFC 7530 section 16.19.4): a downgrade
// cannot widen an open.
func (st *v4State) updateOpen(id v4StateID, access uint32) (v4StateID, status) {
	st.mu.Lock()
	defer st.unlock()
	h, s := st.findLocked(kind4Open, id)
	if s != stOK {
		return v4StateID{}, s
	}
	if access != 0 {
		if access&^h.access != 0 {
			return v4StateID{}, stInval
		}
		h.access = access
		if f := st.files[h.file]; f != nil {
			st.releaseKeptLocked(f)
		}
	}
	return h.advance(), stOK
}

// close ends an open (CLOSE).
func (st *v4State) close(id v4StateID) (v4StateID, status) {
	st.mu.Lock()
	defer st.unlock()
	h, s := st.findLocked(kind4Open, id)
	if s != stOK {
		return v4StateID{}, s
	}
	next := h.advance()
	st.dropLocked(h)
	return next, stOK
}

// setClientID hands out a client id (SETCLIENTID) and takes the client's
// callback path, probed once the id is confirmed.
func (x *v4compound) setClientID() status {
	var verf [8]byte
	copy(verf[:], x.d.fixed(len(verf)))
	name := x.d.opaque(v4OpaqueLimit)
	prog := x.d.u32()
	netid := x.d.str(v4OpaqueLimit)
	uaddr := x.d.str(v4OpaqueLimit)
	ident := x.d.u32()
	if x.d.err != nil {
		return stBadXDR
	}
	cb, err := newCallback4(prog, netid, uaddr, ident)
	if err != nil && (netid != "" || uaddr != "") {
		x.s.logf("nfs: NFSv4 callback address %q/%q unusable: %v", netid, uaddr, err)
	}
	id, confirm := x.s.v4.setClientID(string(name), verf, cb)
	x.e.u64(id)
	x.e.fixed(confirm[:])
	return stOK
}

// setClientIDConfirm confirms a client id (SETCLIENTID_CONFIRM).
func (x *v4compound) setClientIDConfirm() status {
	id := x.d.u64()
	var confirm [8]byte
	copy(confirm[:], x.d.fixed(len(confirm)))
	if x.d.err != nil {
		return stBadXDR
	}
	s := x.s.v4.confirmClientID(id, confirm)
	if s == stOK {
		x.note(id)
	}
	return s
}

// renew renews a client's lease (RENEW).
func (x *v4compound) renew() status {
	id := x.d.u64()
	if x.d.err != nil {
		return stBadXDR
	}
	s := x.s.v4.renew(id)
	if s == stOK || s == stCBPathDown {
		x.note(id)
	}
	return s
}
