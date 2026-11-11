package nfs

import (
	"testing"
	"time"
)

// v4newState starts the NFSv4 state of a server, on clock if not nil.
func v4newState(t *testing.T, clock *v4clock) *v4State {
	t.Helper()
	s := &Server{FS: newTestFS(t)}
	if clock != nil {
		s.v4.now = clock.now
	}
	s.start()
	return &s.v4
}

// v4testClient returns a confirmed client id for name.
func v4testClient(t *testing.T, st *v4State, name string) uint64 {
	t.Helper()
	id, confirm := st.setClientID(name, [8]byte{1}, nil)
	if s := st.confirmClientID(id, confirm); s != stOK {
		t.Fatalf("confirming client %s: status %d", name, s)
	}
	return id
}

// v4testOpen opens file for reading and writing as owner.
func v4testOpen(t *testing.T, st *v4State, owner v4Owner, file uint64) v4StateID {
	t.Helper()
	id, s := st.open(owner, file, share4AccessAll)
	if s != stOK {
		t.Fatalf("open %d: status %d", file, s)
	}
	return id
}

// v4testLock locks as a lock owner new to file, under an open of file by an
// open owner of the same client.
func v4testLock(t *testing.T, st *v4State, owner v4Owner, file uint64, r v4Range) (v4StateID, *v4Conflict, status) {
	t.Helper()
	open := v4testOpen(t, st, v4Owner{client: owner.client, name: "open"}, file)
	return st.lock(open, owner, file, r)
}

// v4mustRange is range4 that must succeed.
func v4mustRange(t *testing.T, offset, length uint64, typ uint32) v4Range {
	t.Helper()
	r, s := range4(offset, length, typ)
	if s != stOK {
		t.Fatalf("range4(%d, %d, %d): status %d", offset, length, typ, s)
	}
	return r
}

// v4lockHolds counts lock states.
func v4lockHolds(st *v4State) int {
	st.mu.Lock()
	defer st.mu.Unlock()
	n := 0
	for _, h := range st.holds {
		if h.kind == kind4Lock {
			n++
		}
	}
	return n
}

// v4has reports whether a stateid's state is still kept.
func v4has(st *v4State, id v4StateID) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.holds[id.other] != nil
}

func TestV4LockRangeValidation(t *testing.T) {
	for _, tc := range []struct {
		offset, length uint64
		typ            uint32
	}{
		{0, 0, lock4Write},
		{10, ^uint64(0) - 5, lock4Write},
		{0, 100, 9},
		{0, 100, 0},
	} {
		if _, s := range4(tc.offset, tc.length, tc.typ); s != stInval {
			t.Errorf("range4(%d, %d, %d): status %d, want INVAL", tc.offset, tc.length, tc.typ, s)
		}
	}
	if r := v4mustRange(t, 5, length4EOF, lock4ReadW); r != (v4Range{start: 5, end: length4EOF}) {
		t.Errorf("blocking read to EOF = %+v, want shared [5, EOF)", r)
	}
	if r := v4mustRange(t, 5, 10, lock4WriteW); r != (v4Range{start: 5, end: 15, excl: true}) {
		t.Errorf("blocking write = %+v, want exclusive [5, 15)", r)
	}
	if r := v4mustRange(t, 10, ^uint64(0)-10, lock4Read); r.end != ^uint64(0) {
		t.Errorf("range up to the largest offset = %+v", r)
	}
}

func TestV4LockDeniedDescribesConflictingHolder(t *testing.T) {
	st := v4newState(t, nil)
	alice := v4Owner{client: v4testClient(t, st, "a"), name: "alice"}
	bob := v4Owner{client: v4testClient(t, st, "b"), name: "bob"}
	if _, _, s := v4testLock(t, st, alice, 7, v4mustRange(t, 0, 100, lock4Write)); s != stOK {
		t.Fatalf("lock: status %d", s)
	}
	_, c, s := v4testLock(t, st, bob, 7, v4mustRange(t, 50, 10, lock4Read))
	if s != stDenied {
		t.Fatalf("lock over a conflict: status %d, want DENIED", s)
	}
	if want := (v4Conflict{v4Range{0, 100, true}, alice}); *c != want {
		t.Fatalf("conflict %+v, want %+v", *c, want)
	}
	if n := v4lockHolds(st); n != 1 {
		t.Fatalf("%d lock states: a denied lock must not create one", n)
	}
	// The same holder's own lock does not conflict; LOCKT reports another's.
	if c, s := st.test(alice, 7, v4mustRange(t, 0, length4EOF, lock4Write)); s != stOK || c != nil {
		t.Fatalf("LOCKT by the holder: %+v, %d", c, s)
	}
	c, s = st.test(bob, 7, v4mustRange(t, 99, 1, lock4Read))
	if s != stDenied || c.owner != alice {
		t.Fatalf("LOCKT by another: %+v, %d", c, s)
	}
	var e encoder
	putDenied4(&e, &v4Conflict{v4Range{5, length4EOF, false}, alice})
	d := decoder{buf: e.buf}
	if off, length, typ, client, owner := d.u64(), d.u64(), d.u32(), d.u64(), d.str(10); off != 5 || length != length4EOF || typ != lock4Read || client != alice.client || owner != "alice" {
		t.Fatalf("LOCK4denied %d %d %d %d %q", off, length, typ, client, owner)
	}
}

func TestV4LockStateReuseAndSeqid(t *testing.T) {
	st := v4newState(t, nil)
	owner := v4Owner{client: v4testClient(t, st, "c"), name: "o"}

	id, _, s := v4testLock(t, st, owner, 7, v4mustRange(t, 0, 10, lock4Write))
	if s != stOK || id.seqid != 1 {
		t.Fatalf("lock: status %d seqid %d, want a new state at 1", s, id.seqid)
	}
	id2, _, s := st.relock(id, 7, v4mustRange(t, 20, 10, lock4Write))
	if s != stOK || id2.other != id.other || id2.seqid != 2 {
		t.Fatalf("relock: status %d stateid %+v, want the same state at 2", s, id2)
	}
	id3, s := st.unlockRange(id2, 7, v4mustRange(t, 0, 10, lock4Write))
	if s != stOK || id3.seqid != 3 {
		t.Fatalf("unlock: status %d seqid %d, want 3", s, id3.seqid)
	}
	if held := v4heldOn(st, 7); len(held) != 1 || held[0].v4Range != (v4Range{20, 30, true}) {
		t.Fatalf("held %+v", held)
	}
	if _, _, s := v4testLock(t, st, owner, 8, v4mustRange(t, 0, 10, lock4Write)); s != stOK {
		t.Fatalf("lock of another file: status %d", s)
	}
	if n := v4lockHolds(st); n != 2 {
		t.Fatalf("%d lock states, want one per owner and file", n)
	}
}

func TestV4UnlockBadStateID(t *testing.T) {
	st := v4newState(t, nil)
	r := v4mustRange(t, 0, 10, lock4Write)
	if _, s := st.unlockRange(v4StateID{}, 7, r); s != stStaleStateID {
		t.Fatalf("unlock with another instance's stateid: status %d, want STALE_STATEID", s)
	}
	owner := v4Owner{client: v4testClient(t, st, "c"), name: "o"}
	id, _, _ := v4testLock(t, st, owner, 7, r)
	unknown := id
	unknown.other[11]++
	if _, s := st.unlockRange(unknown, 7, r); s != stBadStateID {
		t.Fatalf("unknown stateid: status %d, want BAD_STATEID", s)
	}
	if _, s := st.unlockRange(id, 8, r); s != stBadStateID {
		t.Fatalf("unlock of another file: status %d, want BAD_STATEID", s)
	}
	if _, _, s := st.relock(id, 8, r); s != stBadStateID {
		t.Fatalf("relock of another file: status %d, want BAD_STATEID", s)
	}
	open := v4testOpen(t, st, owner, 7)
	if _, s := st.unlockRange(open, 7, r); s != stBadStateID {
		t.Fatalf("unlock with an open stateid: status %d, want BAD_STATEID", s)
	}
	if _, s := st.close(id); s != stBadStateID {
		t.Fatalf("close with a lock stateid: status %d, want BAD_STATEID", s)
	}
	if _, _, s := st.lock(id, owner, 7, r); s != stBadStateID {
		t.Fatalf("new lock owner under a lock stateid: status %d, want BAD_STATEID", s)
	}
	if _, _, s := st.lock(open, owner, 8, r); s != stBadStateID {
		t.Fatalf("new lock owner under an open of another file: status %d, want BAD_STATEID", s)
	}
	other := v4Owner{client: v4testClient(t, st, "other"), name: "o"}
	if _, _, s := st.lock(open, other, 7, r); s != stBadStateID {
		t.Fatalf("new lock owner under another client's open: status %d, want BAD_STATEID", s)
	}
	if held := v4heldOn(st, 7); len(held) != 1 {
		t.Fatalf("bad stateids changed the locks: %+v", held)
	}
}

func TestV4OpenStateGenerations(t *testing.T) {
	st := v4newState(t, nil)
	owner := v4Owner{client: v4testClient(t, st, "c"), name: "o"}

	first := v4testOpen(t, st, owner, 7)
	if first.seqid != 1 {
		t.Fatalf("first open seqid %d, want 1", first.seqid)
	}
	if again := v4testOpen(t, st, owner, 7); again.other != first.other || again.seqid != 2 {
		t.Fatalf("second open %+v, want the same state at seqid 2", again)
	}
	// The same bytes as a lock owner name a different owner.
	lock, _, _ := st.lock(first, owner, 7, v4mustRange(t, 0, 1, lock4Write))
	if lock.other == first.other {
		t.Fatal("open and lock state must have different stateids")
	}
	down, s := st.updateOpen(first, 0)
	if s != stOK || down.other != first.other || down.seqid != 3 {
		t.Fatalf("confirm %+v, %d; want seqid 3", down, s)
	}
	closed, s := st.close(down)
	if s != stOK || closed.seqid != 4 {
		t.Fatalf("close %+v, %d; want seqid 4", closed, s)
	}
	if _, s := st.updateOpen(closed, 0); s != stBadStateID {
		t.Fatalf("confirm after close: status %d, want BAD_STATEID", s)
	}
	if reopened := v4testOpen(t, st, owner, 7); reopened.other == first.other || reopened.seqid != 1 {
		t.Fatalf("reopen after close %+v, want a new state at seqid 1", reopened)
	}
}

func TestV4LockLimitMapsToResource(t *testing.T) {
	st := v4newState(t, nil)
	st.mu.Lock()
	st.locks.maxPerFile = 1
	st.mu.Unlock()
	owner := v4Owner{client: v4testClient(t, st, "c"), name: "o"}
	if _, _, s := v4testLock(t, st, owner, 7, v4mustRange(t, 0, 10, lock4Write)); s != stOK {
		t.Fatalf("lock: status %d", s)
	}
	other := v4Owner{client: owner.client, name: "p"}
	if _, _, s := v4testLock(t, st, other, 7, v4mustRange(t, 20, 10, lock4Write)); s != stResource {
		t.Fatalf("lock over the limit: status %d, want RESOURCE", s)
	}
	if n := v4lockHolds(st); n != 1 {
		t.Fatalf("%d lock states: a refused lock must not create one", n)
	}
}

func TestV4ReleaseOwnerDropsStateAndRanges(t *testing.T) {
	st := v4newState(t, nil)
	client := v4testClient(t, st, "c")
	owner := v4Owner{client: client, name: "o"}
	other := v4Owner{client: client, name: "p"}
	id, _, _ := v4testLock(t, st, owner, 7, v4mustRange(t, 0, 100, lock4Write))
	v4testLock(t, st, owner, 8, v4mustRange(t, 0, 100, lock4Write))
	v4testLock(t, st, other, 7, v4mustRange(t, 200, 100, lock4Write))
	open := v4testOpen(t, st, owner, 7)

	if s := st.releaseOwner(owner); s != stOK {
		t.Fatalf("release owner: status %d", s)
	}
	if n := v4lockHolds(st); n != 1 {
		t.Fatalf("%d lock states, want the other owner's", n)
	}
	if _, s := st.unlockRange(id, 7, v4mustRange(t, 0, 100, lock4Write)); s != stBadStateID {
		t.Fatalf("released stateid: status %d, want BAD_STATEID", s)
	}
	if _, s := st.close(open); s != stOK {
		t.Fatalf("an open owner with the same bytes must keep its open: status %d", s)
	}
	if held := v4heldOn(st, 7); len(held) != 1 || held[0].owner != other {
		t.Fatalf("held of 7: %+v", held)
	}
	if held := v4heldOn(st, 8); len(held) != 0 {
		t.Fatalf("held of 8: %+v", held)
	}
	if s := st.releaseOwner(v4Owner{client: 99, name: "x"}); s != stStaleClientID {
		t.Fatalf("release of an unknown client's owner: status %d", s)
	}
}

func TestV4LeaseExpiryReleasesClientStateAndTellsTheClient(t *testing.T) {
	var clock v4clock
	st := v4newState(t, &clock)
	dead := v4Owner{client: v4testClient(t, st, "dead"), name: "dead"}
	live := v4Owner{client: v4testClient(t, st, "live"), name: "live"}
	deadLock, _, s := v4testLock(t, st, dead, 7, v4mustRange(t, 0, 100, lock4Write))
	if s != stOK {
		t.Fatal("setup lock failed")
	}
	deadOpen := v4testOpen(t, st, dead, 7)
	expiry := st.expiry()

	// A live client's activity after the expiry window expires the dead one.
	clock.advance(expiry / 2)
	if s := st.renew(live.client); s != stOK {
		t.Fatalf("live RENEW: status %d", s)
	}
	clock.advance(expiry/2 + time.Second)
	if c, s := st.test(live, 7, v4mustRange(t, 0, 100, lock4Write)); s != stOK {
		t.Fatalf("live LOCKT after the dead lease expired: %+v, status %d", c, s)
	}
	if v4has(st, deadLock) || v4has(st, deadOpen) {
		t.Fatal("dead client's state kept")
	}
	if held := v4heldOn(st, 7); len(held) != 0 {
		t.Fatalf("dead client's ranges kept: %+v", held)
	}

	// When the dead client comes back, it learns its lease expired.
	if s := st.renew(dead.client); s != stExpired {
		t.Fatalf("RENEW of an expired client: status %d, want EXPIRED", s)
	}
	if _, s := st.open(dead, 7, share4Read); s != stExpired {
		t.Fatalf("OPEN by an expired client: status %d, want EXPIRED", s)
	}
	if _, s := st.test(dead, 7, v4mustRange(t, 0, 100, lock4Write)); s != stExpired {
		t.Fatalf("LOCKT by an expired client: status %d, want EXPIRED", s)
	}
	// Having recovered, it has a new lease.
	again := v4testClient(t, st, "dead")
	if again == dead.client {
		t.Fatal("recovered client got its expired client id back")
	}
	if s := st.renew(again); s != stOK {
		t.Fatalf("RENEW after recovery: status %d", s)
	}
	// Much later the expired id is forgotten and stale.
	clock.advance(v4ExpiredMemory*expiry + time.Second)
	if s := st.renew(again); s != stExpired {
		t.Fatalf("RENEW after a long silence: status %d, want EXPIRED", s)
	}
	clock.advance(v4ExpiredMemory*expiry + time.Second)
	if s := st.renew(dead.client); s != stStaleClientID {
		t.Fatalf("RENEW of a long expired client: status %d, want STALE_CLIENTID", s)
	}
}

func TestV4IdleClientIsExpiredWhenItReturns(t *testing.T) {
	var clock v4clock
	st := v4newState(t, &clock)
	owner := v4Owner{client: v4testClient(t, st, "c"), name: "o"}
	open := v4testOpen(t, st, owner, 7)
	clock.advance(st.expiry() + time.Second)
	if _, _, s := st.checkIO(open, 7, false); s != stExpired {
		t.Fatalf("READ after the lease ran out: status %d, want EXPIRED", s)
	}
}

func TestV4RenewKeepsLeaseAlive(t *testing.T) {
	var clock v4clock
	st := v4newState(t, &clock)
	holder := v4Owner{client: v4testClient(t, st, "holder"), name: "holder"}
	if _, _, s := v4testLock(t, st, holder, 7, v4mustRange(t, 0, 100, lock4Write)); s != stOK {
		t.Fatal("setup lock failed")
	}
	open := v4testOpen(t, st, holder, 7)
	// Renewing by RENEW and by I/O presenting a stateid keeps the state
	// although another client stays active well past the first expiry.
	other := v4Owner{client: v4testClient(t, st, "other"), name: "other"}
	for i := range 10 {
		clock.advance(st.lease)
		var s status
		if i%2 == 0 {
			s = st.renew(holder.client)
		} else {
			_, _, s = st.checkIO(open, 7, false)
		}
		if s != stOK {
			t.Fatalf("renewal %d: status %d", i, s)
		}
		if _, s := st.test(other, 7, v4mustRange(t, 0, 100, lock4Write)); s != stDenied {
			t.Fatalf("renewed client's lock gone: status %d", s)
		}
	}
}

func TestV4StateFromBeforeRestartIsStale(t *testing.T) {
	before := v4newState(t, nil)
	owner := v4Owner{client: v4testClient(t, before, "c"), name: "o"}
	open := v4testOpen(t, before, owner, 7)
	lock, _, _ := before.lock(open, owner, 7, v4mustRange(t, 0, 10, lock4Write))

	after := v4newState(t, nil)
	after.mu.Lock()
	if after.boot == before.boot {
		after.boot++
	}
	after.mu.Unlock()
	v4testClient(t, after, "someone else")
	v4testOpen(t, after, v4Owner{client: v4testClient(t, after, "x"), name: "o"}, 8)

	if s := after.renew(owner.client); s != stStaleClientID {
		t.Fatalf("RENEW with a client id from before a restart: status %d, want STALE_CLIENTID", s)
	}
	if _, _, s := after.checkIO(open, 7, false); s != stStaleStateID {
		t.Fatalf("READ with a stateid from before a restart: status %d, want STALE_STATEID", s)
	}
	if _, s := after.close(open); s != stStaleStateID {
		t.Fatalf("CLOSE with a stateid from before a restart: status %d, want STALE_STATEID", s)
	}
	if _, s := after.unlockRange(lock, 7, v4mustRange(t, 0, 10, lock4Write)); s != stStaleStateID {
		t.Fatalf("LOCKU with a stateid from before a restart: status %d, want STALE_STATEID", s)
	}
}

func TestV4SetClientID(t *testing.T) {
	st := v4newState(t, nil)
	id, confirm := st.setClientID("c", [8]byte{1}, nil)
	if s := st.renew(id); s != stStaleClientID {
		t.Fatalf("RENEW before confirmation: status %d, want STALE_CLIENTID", s)
	}
	if s := st.confirmClientID(id, [8]byte{9}); s != stStaleClientID {
		t.Fatalf("confirm with the wrong verifier: status %d, want STALE_CLIENTID", s)
	}
	if s := st.confirmClientID(id, confirm); s != stOK {
		t.Fatalf("confirm: status %d", s)
	}
	if s := st.confirmClientID(id, confirm); s != stOK {
		t.Fatalf("retransmitted confirm: status %d", s)
	}
	owner := v4Owner{client: id, name: "o"}
	lock, _, _ := v4testLock(t, st, owner, 7, v4mustRange(t, 0, 10, lock4Write))

	// The same boot verifier keeps the client id and its state.
	same, confirm := st.setClientID("c", [8]byte{1}, nil)
	if same != id {
		t.Fatalf("SETCLIENTID with the same verifier = %x, want %x", same, id)
	}
	if s := st.confirmClientID(same, confirm); s != stOK {
		t.Fatalf("confirm: status %d", s)
	}
	if _, s := st.unlockRange(lock, 7, v4mustRange(t, 0, 10, lock4Write)); s != stOK {
		t.Fatalf("lock state after SETCLIENTID with the same verifier: status %d", s)
	}

	// A new boot verifier means the client rebooted: once the new id is
	// confirmed, the old one and its state are gone.
	rebooted, confirm := st.setClientID("c", [8]byte{2}, nil)
	if rebooted == id {
		t.Fatal("rebooted client got its old client id")
	}
	if s := st.renew(id); s != stOK {
		t.Fatalf("old client id before the new one is confirmed: status %d", s)
	}
	if s := st.confirmClientID(rebooted, confirm); s != stOK {
		t.Fatalf("confirm: status %d", s)
	}
	if s := st.renew(id); s != stStaleClientID {
		t.Fatalf("replaced client id: status %d, want STALE_CLIENTID", s)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.holds) != 0 || len(st.locks.files) != 0 || len(st.locks.perClient) != 0 {
		t.Fatalf("replaced client's state kept: %d states, %d locked files", len(st.holds), len(st.locks.files))
	}
}

func TestV4CheckIOSpecialStateIDs(t *testing.T) {
	st := v4newState(t, nil)
	for _, id := range []v4StateID{stateid4Anon, stateid4Bypass} {
		if client, _, s := st.checkIO(id, 7, true); s != stOK || client != 0 {
			t.Fatalf("special stateid %+v: client %d status %d", id, client, s)
		}
	}
	owner := v4Owner{client: v4testClient(t, st, "c"), name: "o"}
	lock, _, _ := v4testLock(t, st, owner, 7, v4mustRange(t, 0, 10, lock4Write))
	if client, _, s := st.checkIO(lock, 7, true); s != stOK || client != owner.client {
		t.Fatalf("lock stateid: client %d status %d", client, s)
	}
	lock.other[11]++
	if _, _, s := st.checkIO(lock, 7, false); s != stBadStateID {
		t.Fatalf("unknown stateid: status %d, want BAD_STATEID", s)
	}
}

// TestV4RecallIsCheap: Server.Recall is a no-op on the zero value and
// before start, and takes no lock while nothing is delegated or granted.
func TestV4RecallIsCheap(t *testing.T) {
	var zero Server
	zero.Recall(1)
	st := v4newState(t, nil)
	st.mu.Lock()
	done := make(chan struct{})
	go func() {
		st.srv.Recall(1)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Recall waited for the state lock with nothing delegated")
	}
	st.mu.Unlock()
}

// holdingFS records the lifecycle of backend identity holds.
type holdingFS struct {
	*testFS
	held map[uint64]int
}

func (f *holdingFS) HoldFileID(id uint64) func() {
	f.held[id]++
	return func() { f.held[id]-- }
}

// TestV4IdentityHolds pairs every open, lock and delegation with a release,
// including lease expiry. A backend may evict an identity once none remains.
func TestV4IdentityHolds(t *testing.T) {
	clock := &v4clock{}
	fs := &holdingFS{testFS: newTestFS(t), held: map[uint64]int{}}
	srv := &Server{FS: fs}
	srv.v4.now = clock.now
	srv.start()
	st := &srv.v4
	client := v4testClient(t, st, "held")
	owner := v4Owner{client: client, name: "owner"}
	id := v4testOpen(t, st, owner, 123)
	if fs.held[123] != 1 {
		t.Fatal(fs.held)
	}
	if _, s := st.close(id); s != stOK {
		t.Fatal(s)
	}
	if fs.held[123] != 0 {
		t.Fatal("close leaked hold")
	}
	for _, kind := range []v4Kind{kind4Open, kind4Lock, kind4Deleg} {
		identity, _ := st.acquireIdentity(456)
		st.mu.Lock()
		h := st.newHoldLocked(kind, owner, 456, identity)
		st.dropLocked(h)
		st.unlock()
	}
	if fs.held[456] != 0 {
		t.Fatal("state drop leaked hold")
	}
	v4testOpen(t, st, owner, 789)
	clock.advance(st.expiry() + time.Second)
	st.renew(client)
	if fs.held[789] != 0 {
		t.Fatal("expired lease leaked hold")
	}
}
