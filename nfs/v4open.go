package nfs

import (
	"os"

	"github.com/go-git/go-billy/v5"
)

// NFSv4.0 OPEN and the operations on its state: OPEN_CONFIRM,
// OPEN_DOWNGRADE and CLOSE.

// OPEN argument and result values.
const (
	open4NoCreate = 0
	open4Create   = 1

	create4Unchecked = 0
	create4Guarded   = 1
	create4Exclusive = 2

	share4Read      = 1
	share4Write     = 2
	share4DenyNone  = 0
	share4AccessAll = share4Read | share4Write

	claim4Null         = 0
	claim4Previous     = 1
	claim4DelegateCur  = 2
	claim4DelegatePrev = 3

	// open4LocktypePosix in OPEN's rflags advertises POSIX byte-range lock
	// semantics; without it the Linux client fails fcntl locks locally with
	// ENOLCK and never sends LOCK. OPEN4_RESULT_CONFIRM is never set, so
	// clients need not send OPEN_CONFIRM.
	open4LocktypePosix = 4
)

// v4cinfo is a change_info4: a directory's change attribute before and
// after an operation, atomic when nothing else changed it in between.
type v4cinfo struct {
	atomic        bool
	before, after uint64
}

// put appends the change_info4.
func (c v4cinfo) put(e *encoder) {
	e.bool(c.atomic)
	e.u64(c.before)
	e.u64(c.after)
}

// open answers OPEN of a name in the current directory (CLAIM_NULL), of one
// the client holds a delegation of (CLAIM_DELEGATE_CUR, which macOS sends
// before returning a delegation), or of the current file itself, reclaimed
// after a restart (CLAIM_PREVIOUS).
func (x *v4compound) open() status {
	d := x.d
	d.u32() // seqid
	access := d.u32()
	deny := d.u32()
	owner := getOwner4(d)
	create := false
	how := uint32(create4Unchecked)
	var sa v4sattr
	sast := stOK
	switch d.u32() {
	case open4NoCreate:
	case open4Create:
		create = true
		how = d.u32()
		switch how {
		case create4Unchecked, create4Guarded:
			sa, sast = getSattr4(d)
		case create4Exclusive:
			// EXCLUSIVE4 has the server keep the verifier with the file,
			// so a retransmitted OPEN finds its own file. With nowhere to
			// keep it, it is handled as GUARDED4: an existing file fails
			// with NFS4ERR_EXIST.
			d.fixed(8)
		default:
			return stInval
		}
	default:
		return stInval
	}
	claim := d.u32()
	var name string
	var delegType uint32
	var delegID v4StateID
	switch claim {
	case claim4Null, claim4DelegatePrev:
		name = d.str(v4ArgLimit)
	case claim4Previous:
		delegType = d.u32()
	case claim4DelegateCur:
		delegID = getStateID4(d)
		name = d.str(v4ArgLimit)
	default:
		return stNotSupp
	}
	if d.err != nil {
		return stBadXDR
	}
	if sast != stOK {
		return sast
	}
	if deny != share4DenyNone {
		return stNotSupp
	}
	if access&share4AccessAll == 0 {
		return stInval
	}
	write := access&share4Write != 0
	st := &x.s.v4

	// The file: a name in the current directory, or for a reclaim the
	// current filehandle itself. A create brackets its change to the
	// directory under the directory's lock (v4DirLocks); any other open
	// changes no directory and reports one reading, of the directory or the
	// reclaimed file, twice: atomic either way.
	cinfo := v4cinfo{atomic: true}
	var p string
	var parent v4fh
	switch claim {
	case claim4Null, claim4DelegateCur:
		if s := v4Component(name); s != stOK {
			return s
		}
		cur, s := x.current()
		if s != stOK {
			return s
		}
		pa, err := x.s.stat(cur.id, cur.path)
		if err != nil {
			return statusOf(err)
		}
		if pa.typ != typeDir {
			return stNotDir
		}
		cinfo.before = v4Change(&pa)
		parent = *cur
		p = v4Join(cur.path, name)
	case claim4Previous:
		// Reclaiming an open held before a restart. Nothing of it was
		// kept, so during the grace period it is granted afresh; a
		// delegation is not. This assumes one client (the local macOS
		// mount) that cannot have lost a race for the file meanwhile.
		if !st.inGrace() || delegType != deleg4None {
			return stNoGrace
		}
		if create {
			return stInval
		}
		cur, s := x.current()
		if s != stOK {
			return s
		}
		p = cur.path
	case claim4DelegatePrev:
		return stNotSupp
	}
	if s := st.checkClient(owner.client); s != stOK {
		return s
	}
	x.note(owner.client)

	info, err := x.s.FS.Lstat(p)
	exists := err == nil
	if create && exists && how != create4Unchecked {
		return stExist
	}
	if !exists && !create {
		return statusOf(err)
	}
	if exists {
		switch {
		case info.IsDir():
			return stIsDir
		case info.Mode()&os.ModeSymlink != 0:
			return stSymlink
		case !info.Mode().IsRegular():
			return stInval
		}
		id := x.s.FS.FileID(p)
		a := attrsOf(id, info)
		if claim == claim4Previous {
			cinfo.before = v4Change(&a)
		}
		if claim == claim4DelegateCur {
			if s := st.checkDeleg(delegID, owner.client, id); s != stOK {
				return s
			}
		}
		if write || create {
			if s := x.conflict(id); s != stOK {
				return s
			}
		}
	} else if claim == claim4DelegateCur {
		return stBadStateID
	}

	var kept billy.File
	// Until st.keep owns a retained creation descriptor, every error path
	// (including identity refusal after the backend commit) must close it.
	defer func() {
		if kept != nil {
			kept.Close()
		}
	}()
	var set v4bitmap
	if create {
		unlock := st.dirs.lock(parent.id, parent.id)
		cinfo.before = x.s.changeAt4(parent.path)
		var s status
		kept, set, s = x.s.createFile4(p, exists, how, write, &sa)
		cinfo.after = x.s.changeAt4(parent.path)
		unlock()
		if s != stOK {
			return s
		}
	} else {
		cinfo.after = cinfo.before
	}

	x.setCurrent(p)
	id := x.cur.id
	if id == 0 {
		return stIO
	}
	if exists && (write || create) {
		x.changed(id)
	}
	g := x.prepareDelegate(owner.client, claim, access, id, p)
	sid, did, delegated, s := st.openDelegated(owner, id, access&share4AccessAll, g)
	if s != stOK {
		return s
	}
	if kept != nil {
		st.keep(id, kept)
		kept = nil
	}
	putStateID4(x.e, sid)
	cinfo.put(x.e)
	x.e.u32(open4LocktypePosix)
	set.put(x.e)
	putDeleg4(x.e, did, delegated)
	return stOK
}

// createFile4 makes the regular file at p for an OPEN(create) with the
// attributes sa and returns the attributes set. UNCHECKED4 of an existing
// file applies none but a size of zero, which truncates it (RFC 7530
// section 16.16.5): open(O_CREAT) leaves an existing file's mode alone.
//
// A file created without write permission for its owner cannot be opened
// for writing again, but its creator may write it through the open, so for
// an open for writing the file stays open for WRITE (see keepWritable).
func (s *Server) createFile4(p string, exists bool, how uint32, write bool, sa *v4sattr) (billy.File, v4bitmap, status) {
	if exists {
		if !sa.mask.has(attr4Size) || sa.size != 0 {
			return nil, v4bitmap{}, stOK
		}
		if err := s.truncate4(p, 0); err != nil {
			return nil, v4bitmap{}, statusOf(err)
		}
		return nil, bitmap4(attr4Size), stOK
	}
	flags := os.O_RDWR | os.O_CREATE
	if how != create4Unchecked {
		flags |= os.O_EXCL
	}
	mode := os.FileMode(0o666)
	if sa.mask.has(attr4Mode) {
		mode = goMode(sa.mode)
	}
	f, err := s.FS.OpenFile(p, flags, mode)
	if err != nil {
		return nil, v4bitmap{}, statusOf(err)
	}
	if st := s.apply4(p, sa, f); st != stOK {
		_ = f.Close()
		return nil, v4bitmap{}, st
	}
	if write {
		if info, err := s.FS.Lstat(p); err == nil && info.Mode().Perm()&0o200 == 0 {
			return f, sa.mask, stOK
		}
	}
	if err := f.Close(); err != nil {
		return nil, v4bitmap{}, statusOf(err)
	}
	return nil, sa.mask, stOK
}

// openConfirm answers OPEN_CONFIRM, which no client needs to send (see
// open4LocktypePosix) but may.
func (x *v4compound) openConfirm() status {
	id := getStateID4(x.d)
	x.d.u32() // seqid
	if x.d.err != nil {
		return stBadXDR
	}
	if _, s := x.current(); s != stOK {
		return s
	}
	next, s := x.s.v4.updateOpen(id, 0)
	if s == stOK {
		putStateID4(x.e, next)
	}
	return s
}

// openDowngrade narrows the share access an open is recorded with, which
// decides whether others may be granted delegations of the file and how
// long a kept file stays open. Opens take no share reservations, so the
// deny bits are not kept.
func (x *v4compound) openDowngrade() status {
	id := getStateID4(x.d)
	x.d.u32() // seqid
	access := x.d.u32()
	x.d.u32() // share deny
	if x.d.err != nil {
		return stBadXDR
	}
	if _, s := x.current(); s != stOK {
		return s
	}
	if access&share4AccessAll == 0 || access&^share4AccessAll != 0 {
		return stInval
	}
	next, s := x.s.v4.updateOpen(id, access)
	if s == stOK {
		putStateID4(x.e, next)
	}
	return s
}

// close answers CLOSE.
func (x *v4compound) close() status {
	x.d.u32() // seqid
	id := getStateID4(x.d)
	if x.d.err != nil {
		return stBadXDR
	}
	if _, s := x.current(); s != stOK {
		return s
	}
	next, s := x.s.v4.close(id)
	if s == stOK {
		putStateID4(x.e, next)
	}
	return s
}

// A client that opened a file for writing may write it however its mode
// changes afterwards, as through a local file descriptor: git, for one,
// creates an object, makes it read-only and then writes it. The server
// opens the file afresh for every WRITE, which fails once the owner may not
// write. So before a file open for writing loses its owner's write
// permission, the server opens it and keeps that file for WRITE (and
// SETATTR of the size) under write opens, and under the lock stateids of
// clients holding one (clients write with their lock stateid while they
// hold byte-range locks), until the last write open is closed or
// downgraded.

// v4Kept is a file kept open for WRITE. The file's state holds one
// reference and every WRITE or SETATTR using it another, so a CLOSE racing
// a WRITE does not close the file under it.
type v4Kept struct {
	f    billy.File
	refs int
}

// keptLocked returns, with a reference taken, the file kept for writes under
// state h: an open for writing, or a lock owner's locks whose client has the
// file open for writing.
func (st *v4State) keptLocked(h *v4Hold) *v4Kept {
	f := st.files[h.file]
	if f == nil || f.kept == nil {
		return nil
	}
	switch h.kind {
	case kind4Open:
		if h.access&share4Write == 0 {
			return nil
		}
	case kind4Lock:
		writes := false
		for o := range f.opens {
			if o.owner.client == h.owner.client && o.access&share4Write != 0 {
				writes = true
				break
			}
		}
		if !writes {
			return nil
		}
	default:
		return nil
	}
	f.kept.refs++
	return f.kept
}

// kept returns, with a reference taken, the file kept for writes to file
// under stateid id, if any: a WRITE that lost a race with the SETATTR taking
// its owner's write permission looks again.
func (st *v4State) kept(id v4StateID, file uint64) *v4Kept {
	st.mu.Lock()
	defer st.unlock()
	if h := st.holds[id.other]; h != nil && h.file == file {
		return st.keptLocked(h)
	}
	return nil
}

// release lets go of a reference keptLocked took; nil is none.
func (st *v4State) release(k *v4Kept) {
	if k == nil {
		return
	}
	st.mu.Lock()
	defer st.unlock()
	st.unrefLocked(k)
}

// unrefLocked drops a reference to k, closing it, once the lock is
// released, with the last.
func (st *v4State) unrefLocked(k *v4Kept) {
	if k.refs--; k.refs == 0 {
		st.closing = append(st.closing, k.f)
	}
}

// releaseKeptLocked lets go of f's kept file once f is not open for
// writing.
func (st *v4State) releaseKeptLocked(f *v4File) {
	if f.kept != nil && f.writers() == 0 {
		st.unrefLocked(f.kept)
		f.kept = nil
	}
}

// keepWritable keeps the file fh names open for writing if it is open for
// writing and a SETATTR of mode is about to take its owner's write
// permission.
func (x *v4compound) keepWritable(fh *v4fh, mode uint32) {
	if mode&0o200 != 0 || !x.s.v4.wantsKept(fh.id) {
		return
	}
	f, err := x.s.FS.OpenFile(fh.path, os.O_RDWR, 0)
	if err != nil {
		return
	}
	x.s.v4.keep(fh.id, f)
}

// wantsKept reports whether file is open for writing and has no kept file.
func (st *v4State) wantsKept(file uint64) bool {
	st.mu.Lock()
	defer st.unlock()
	f := st.files[file]
	return f != nil && f.kept == nil && f.writers() > 0
}

// keep keeps f for WRITE to file while file is open for writing, and closes
// it otherwise.
func (st *v4State) keep(file uint64, f billy.File) {
	st.mu.Lock()
	defer st.unlock()
	if v := st.files[file]; v != nil && v.kept == nil && v.writers() > 0 {
		v.kept = &v4Kept{f: f, refs: 1}
		return
	}
	st.closing = append(st.closing, f)
}
