package nfs

import (
	"math"
	"slices"
)

// NFSv4.0 byte-range locks (RFC 7530 section 9.4): LOCK, LOCKT, LOCKU and
// RELEASE_LOCKOWNER over an in-memory table. Locks are advisory and do not
// survive a restart; a client's locks go with its lease.

// nfs_lock_type4 values. The blocking variants are taken as their
// non-blocking forms: the server answers DENIED and the client polls, which
// RFC 7530 permits.
const (
	lock4Read   = 1
	lock4Write  = 2
	lock4ReadW  = 3
	lock4WriteW = 4
)

// Lock table caps, so a misbehaving or leaky client cannot grow the
// server's memory without bound; past them LOCK answers NFS4ERR_RESOURCE.
const (
	v4MaxLocksPerFile   = 512
	v4MaxLocksPerClient = 8192
)

// length4EOF as a lock length means up to the end of the file, and as a
// range end that the range is unbounded.
const length4EOF = math.MaxUint64

// v4Range is the locked byte range [start, end) of a read or, excl, write
// lock.
type v4Range struct {
	start, end uint64
	excl       bool
}

// overlaps reports whether two ranges share a byte.
func (r v4Range) overlaps(o v4Range) bool { return r.start < o.end && o.start < r.end }

// range4 validates a LOCK, LOCKT or LOCKU range and type (RFC 7530 section
// 16.10.4): the length is not zero and the range does not pass the largest
// offset.
func range4(offset, length uint64, typ uint32) (v4Range, status) {
	if length == 0 {
		return v4Range{}, stInval
	}
	end := uint64(length4EOF)
	if length != length4EOF {
		if offset > math.MaxUint64-length {
			return v4Range{}, stInval
		}
		end = offset + length
	}
	switch typ {
	case lock4Read, lock4ReadW:
		return v4Range{start: offset, end: end}, stOK
	case lock4Write, lock4WriteW:
		return v4Range{start: offset, end: end, excl: true}, stOK
	}
	return v4Range{}, stInval
}

// v4Conflict is a held lock that blocks a request.
type v4Conflict struct {
	v4Range
	owner v4Owner
}

// putDenied4 appends the LOCK4denied describing a conflict.
func putDenied4(e *encoder, c *v4Conflict) {
	length := uint64(length4EOF)
	if c.end != length4EOF {
		length = c.end - c.start
	}
	typ := uint32(lock4Read)
	if c.excl {
		typ = lock4Write
	}
	e.u64(c.start)
	e.u64(length)
	e.u32(typ)
	e.u64(c.owner.client)
	e.str(c.owner.name)
}

// v4Locks is the lock table, guarded by v4State.mu.
type v4Locks struct {
	maxPerFile, maxPerClient int
	// files holds each owner's ranges of a file, sorted by start.
	files map[uint64]map[v4Owner][]v4Range
	// owned indexes the files each owner holds ranges of.
	owned map[v4Owner]map[uint64]struct{}
	// perClient counts each client's ranges for the client cap.
	perClient map[uint64]int
}

// newLocks4 makes an empty table with the given caps.
func newLocks4(maxPerFile, maxPerClient int) v4Locks {
	return v4Locks{
		maxPerFile:   maxPerFile,
		maxPerClient: maxPerClient,
		files:        make(map[uint64]map[v4Owner][]v4Range),
		owned:        make(map[v4Owner]map[uint64]struct{}),
		perClient:    make(map[uint64]int),
	}
}

// lock grants owner r of file, replacing the owner's own overlapping ranges,
// so a holder can upgrade, downgrade or split its locks. If another owner
// holds a conflicting range it returns that lock and grants nothing; past a
// cap it answers NFS4ERR_RESOURCE and changes nothing.
func (l *v4Locks) lock(owner v4Owner, file uint64, r v4Range) (*v4Conflict, status) {
	if c := l.conflict(owner, file, r); c != nil {
		return c, stDenied
	}
	before := l.files[file][owner]
	after := coalesce4(append(subtract4(before, r), r))
	if delta := len(after) - len(before); delta > 0 &&
		(l.count(file)+delta > l.maxPerFile || l.perClient[owner.client]+delta > l.maxPerClient) {
		return nil, stResource
	}
	l.set(owner, file, after)
	return nil, stOK
}

// unlock releases r from owner's ranges of file, splitting ranges that
// straddle it. Releasing bytes the owner does not hold is no error.
func (l *v4Locks) unlock(owner v4Owner, file uint64, r v4Range) {
	l.set(owner, file, subtract4(l.files[file][owner], r))
}

// conflict returns the conflicting lock with the lowest start another owner
// holds of file, so denials describe the same lock every time (LOCKT).
func (l *v4Locks) conflict(owner v4Owner, file uint64, r v4Range) *v4Conflict {
	var found *v4Conflict
	for other, ranges := range l.files[file] {
		if other == owner {
			continue
		}
		for _, h := range ranges {
			if !h.overlaps(r) || !h.excl && !r.excl {
				continue
			}
			if found == nil || h.start < found.start || h.start == found.start && lessOwner4(other, found.owner) {
				found = &v4Conflict{v4Range: h, owner: other}
			}
		}
	}
	return found
}

// releaseOwner drops every range owner holds.
func (l *v4Locks) releaseOwner(owner v4Owner) {
	for file := range l.owned[owner] {
		l.set(owner, file, nil)
	}
}

// releaseClient drops every range any owner of client holds.
func (l *v4Locks) releaseClient(client uint64) {
	for owner := range l.owned {
		if owner.client == client {
			l.releaseOwner(owner)
		}
	}
}

// count is the number of ranges held of file.
func (l *v4Locks) count(file uint64) int {
	n := 0
	for _, ranges := range l.files[file] {
		n += len(ranges)
	}
	return n
}

// set stores owner's ranges of file and keeps the indexes and the client
// count in step.
func (l *v4Locks) set(owner v4Owner, file uint64, ranges []v4Range) {
	owners := l.files[file]
	delta := len(ranges) - len(owners[owner])
	if len(ranges) == 0 {
		delete(owners, owner)
		if len(owners) == 0 {
			delete(l.files, file)
		}
		if files := l.owned[owner]; files != nil {
			delete(files, file)
			if len(files) == 0 {
				delete(l.owned, owner)
			}
		}
	} else {
		if owners == nil {
			owners = make(map[v4Owner][]v4Range)
			l.files[file] = owners
		}
		owners[owner] = ranges
		if l.owned[owner] == nil {
			l.owned[owner] = make(map[uint64]struct{})
		}
		l.owned[owner][file] = struct{}{}
	}
	if n := l.perClient[owner.client] + delta; n > 0 {
		l.perClient[owner.client] = n
	} else {
		delete(l.perClient, owner.client)
	}
}

// subtract4 returns ranges without sub, splitting ranges that straddle it.
// It never modifies its input, which keeps a refused lock atomic.
func subtract4(ranges []v4Range, sub v4Range) []v4Range {
	out := make([]v4Range, 0, len(ranges)+1)
	for _, h := range ranges {
		if !h.overlaps(sub) {
			out = append(out, h)
			continue
		}
		if h.start < sub.start {
			out = append(out, v4Range{start: h.start, end: sub.start, excl: h.excl})
		}
		if sub.end < h.end {
			out = append(out, v4Range{start: sub.end, end: h.end, excl: h.excl})
		}
	}
	return out
}

// coalesce4 sorts ranges by start and merges adjacent or overlapping ranges
// of the same kind, bounding growth from repeated small locks.
func coalesce4(ranges []v4Range) []v4Range {
	if len(ranges) < 2 {
		return ranges
	}
	slices.SortFunc(ranges, func(a, b v4Range) int {
		switch {
		case a.start < b.start:
			return -1
		case a.start > b.start:
			return 1
		}
		return 0
	})
	out := ranges[:1]
	for _, h := range ranges[1:] {
		last := &out[len(out)-1]
		if h.excl == last.excl && h.start <= last.end {
			last.end = max(last.end, h.end)
			continue
		}
		out = append(out, h)
	}
	return out
}

// lessOwner4 orders owners, for a stable choice among conflicts.
func lessOwner4(a, b v4Owner) bool {
	if a.client != b.client {
		return a.client < b.client
	}
	return a.name < b.name
}

// lock acquires or changes a range for a lock owner new to file, under the
// open it presents, which must be its client's open of file.
func (st *v4State) lock(open v4StateID, owner v4Owner, file uint64, r v4Range) (v4StateID, *v4Conflict, status) {
	identity, s := st.acquireIdentity(file)
	if s != stOK {
		return v4StateID{}, nil, s
	}
	defer identity.drop()
	st.mu.Lock()
	defer st.unlock()
	h, s := st.findLocked(kind4Open, open)
	if s != stOK {
		return v4StateID{}, nil, s
	}
	if h.owner.client != owner.client || h.file != file {
		return v4StateID{}, nil, stBadStateID
	}
	return st.lockLocked(owner, file, r, identity)
}

// relock acquires or changes a range under a lock owner's stateid of file.
func (st *v4State) relock(id v4StateID, file uint64, r v4Range) (v4StateID, *v4Conflict, status) {
	st.mu.Lock()
	defer st.unlock()
	h, s := st.findLockLocked(id, file)
	if s != stOK {
		return v4StateID{}, nil, s
	}
	return st.lockLocked(h.owner, file, r, nil)
}

// lockLocked locks r of file for owner and advances the owner's lock state
// of it, created on its first lock.
func (st *v4State) lockLocked(owner v4Owner, file uint64, r v4Range, identity *v4Identity) (v4StateID, *v4Conflict, status) {
	if c, s := st.locks.lock(owner, file, r); s != stOK {
		return v4StateID{}, c, s
	}
	h := st.ownerHoldLocked(kind4Lock, owner, file, identity)
	if h == nil {
		st.locks.releaseOwner(owner)
		return v4StateID{}, nil, stIO
	}
	return h.advance(), nil, stOK
}

// findLockLocked returns the lock state a stateid names if it is of file.
func (st *v4State) findLockLocked(id v4StateID, file uint64) (*v4Hold, status) {
	h, s := st.findLocked(kind4Lock, id)
	if s == stOK && h.file != file {
		return nil, stBadStateID
	}
	return h, s
}

// unlockRange releases a range under a lock stateid of file (LOCKU).
func (st *v4State) unlockRange(id v4StateID, file uint64, r v4Range) (v4StateID, status) {
	st.mu.Lock()
	defer st.unlock()
	h, s := st.findLockLocked(id, file)
	if s != stOK {
		return v4StateID{}, s
	}
	st.locks.unlock(h.owner, file, r)
	return h.advance(), stOK
}

// test returns the lock that keeps owner from locking r of file (LOCKT).
func (st *v4State) test(owner v4Owner, file uint64, r v4Range) (*v4Conflict, status) {
	st.mu.Lock()
	defer st.unlock()
	if s := st.checkClientLocked(owner.client); s != stOK {
		return nil, s
	}
	if c := st.locks.conflict(owner, file, r); c != nil {
		return c, stDenied
	}
	return nil, stOK
}

// releaseOwner drops a lock owner's state and ranges (RELEASE_LOCKOWNER).
func (st *v4State) releaseOwner(owner v4Owner) status {
	st.mu.Lock()
	defer st.unlock()
	if s := st.checkClientLocked(owner.client); s != stOK {
		return s
	}
	for _, h := range st.owners[v4OwnerKey{kind4Lock, owner}] {
		st.dropLocked(h)
	}
	st.locks.releaseOwner(owner)
	return stOK
}

// lock answers LOCK. A lock owner new to the file comes with the open it
// locks under; a known one with its lock stateid. A reclaim is granted as a
// new lock during the grace period, since no lock survives a restart.
func (x *v4compound) lock() status {
	typ := x.d.u32()
	reclaim := x.d.bool()
	offset, length := x.d.u64(), x.d.u64()
	var open, lockID v4StateID
	var owner v4Owner
	fresh := x.d.bool()
	if fresh {
		x.d.u32() // open seqid
		open = getStateID4(x.d)
		x.d.u32() // lock seqid
		owner = getOwner4(x.d)
	} else {
		lockID = getStateID4(x.d)
		x.d.u32() // lock seqid
	}
	if x.d.err != nil {
		return stBadXDR
	}
	cur, s := x.current()
	if s != stOK {
		return s
	}
	if reclaim && !x.s.v4.inGrace() {
		return stNoGrace
	}
	r, s := range4(offset, length, typ)
	if s != stOK {
		return s
	}
	var id v4StateID
	var c *v4Conflict
	if fresh {
		id, c, s = x.s.v4.lock(open, owner, cur.id, r)
	} else {
		id, c, s = x.s.v4.relock(lockID, cur.id, r)
	}
	switch s {
	case stOK:
		putStateID4(x.e, id)
	case stDenied:
		putDenied4(x.e, c)
	}
	return s
}

// lockt answers LOCKT with the lock that would deny the range, if any.
func (x *v4compound) lockt() status {
	typ := x.d.u32()
	offset, length := x.d.u64(), x.d.u64()
	owner := getOwner4(x.d)
	if x.d.err != nil {
		return stBadXDR
	}
	cur, s := x.current()
	if s != stOK {
		return s
	}
	r, s := range4(offset, length, typ)
	if s != stOK {
		return s
	}
	c, s := x.s.v4.test(owner, cur.id, r)
	if s == stDenied {
		putDenied4(x.e, c)
	}
	return s
}

// locku answers LOCKU.
func (x *v4compound) locku() status {
	typ := x.d.u32()
	x.d.u32() // seqid
	id := getStateID4(x.d)
	offset, length := x.d.u64(), x.d.u64()
	if x.d.err != nil {
		return stBadXDR
	}
	cur, s := x.current()
	if s != stOK {
		return s
	}
	r, s := range4(offset, length, typ)
	if s != stOK {
		return s
	}
	next, s := x.s.v4.unlockRange(id, cur.id, r)
	if s == stOK {
		putStateID4(x.e, next)
	}
	return s
}

// releaseLockOwner answers RELEASE_LOCKOWNER.
func (x *v4compound) releaseLockOwner() status {
	owner := getOwner4(x.d)
	if x.d.err != nil {
		return stBadXDR
	}
	return x.s.v4.releaseOwner(owner)
}
