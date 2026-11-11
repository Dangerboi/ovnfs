package nfs

import (
	"bytes"
	"path"
	"strings"
)

// NFSv4.0 (RFC 7530): NULL and COMPOUND. A COMPOUND's operations run in
// order against a current and a saved filehandle until one fails; each
// operation's result is encoded straight into the reply.

// NFSv4 procedures.
const (
	proc4Null     = 0
	proc4Compound = 1
)

// Operation numbers (nfs_opnum4). 3 through 39 are every NFSv4.0 operation.
const (
	op4Access             = 3
	op4Close              = 4
	op4Commit             = 5
	op4Create             = 6
	op4DelegPurge         = 7
	op4DelegReturn        = 8
	op4GetAttr            = 9
	op4GetFH              = 10
	op4Link               = 11
	op4Lock               = 12
	op4LockT              = 13
	op4LockU              = 14
	op4Lookup             = 15
	op4LookupP            = 16
	op4NVerify            = 17
	op4Open               = 18
	op4OpenAttr           = 19
	op4OpenConfirm        = 20
	op4OpenDowngrade      = 21
	op4PutFH              = 22
	op4PutPubFH           = 23
	op4PutRootFH          = 24
	op4Read               = 25
	op4ReadDir            = 26
	op4ReadLink           = 27
	op4Remove             = 28
	op4Rename             = 29
	op4Renew              = 30
	op4RestoreFH          = 31
	op4SaveFH             = 32
	op4SecInfo            = 33
	op4SetAttr            = 34
	op4SetClientID        = 35
	op4SetClientIDConfirm = 36
	op4Verify             = 37
	op4Write              = 38
	op4ReleaseLockOwner   = 39
	op4Illegal            = 10044
)

// Statuses only NFSv4 has (RFC 7530 section 13).
const (
	stSame              status = 10009
	stDenied            status = 10010
	stExpired           status = 10011
	stResource          status = 10018
	stNoFileHandle      status = 10020
	stMinorVersMismatch status = 10021
	stStaleClientID     status = 10022
	stStaleStateID      status = 10023
	stBadStateID        status = 10025
	stNotSame           status = 10027
	stSymlink           status = 10029
	stAttrNotSupp       status = 10032
	stNoGrace           status = 10033
	stBadXDR            status = 10036
	stOpenMode          status = 10038
	stBadOwner          status = 10039
	stBadName           status = 10041
	stOpIllegal         status = 10044
	stCBPathDown        status = 10048
)

// Argument limits.
const (
	// v4OpaqueLimit is NFS4_OPAQUE_LIMIT, bounding tags, client names,
	// owners and component names.
	v4OpaqueLimit = 1024
	// v4ArgLimit bounds the other variable-length arguments but WRITE data,
	// so a malformed length cannot claim much memory.
	v4ArgLimit = 64 << 10
	// v4MaxOps bounds the operations of a COMPOUND; clients send a dozen
	// at most.
	v4MaxOps = 128
	// v4ReplyBudget bounds a COMPOUND's results: one full READ and room
	// for the operations around it, and, with the last operation's
	// overrun, within maxRecord. Without it one small request of many
	// READs could ask for any amount of memory, and a reply past 2 GiB
	// would not fit its record mark.
	v4ReplyBudget = maxIO + 32<<10
)

// ACCESS bits (RFC 7530 section 16.1).
const (
	access4Read    = 0x01
	access4Lookup  = 0x02
	access4Modify  = 0x04
	access4Extend  = 0x08
	access4Delete  = 0x10
	access4Execute = 0x20
	access4All     = access4Read | access4Lookup | access4Modify | access4Extend | access4Delete | access4Execute
)

// Security flavors SECINFO offers.
const (
	auth4Sys  = 1
	auth4None = 0
)

// nfs4 answers NFSv4 calls.
func (s *Server) nfs4(c *call, res *encoder) acceptStat {
	switch c.proc {
	case proc4Null:
		return rpcSuccess
	case proc4Compound:
		return s.compound4(c, res)
	}
	return rpcProcUnavail
}

// compound4 runs a COMPOUND (RFC 7530 section 15.2). A header that does not
// decode is GARBAGE_ARGS; an operation whose arguments do not decode ends
// the COMPOUND with NFS4ERR_BADXDR, as RFC 7530 section 13.1.1.1 has it.
func (s *Server) compound4(c *call, res *encoder) acceptStat {
	d := &c.args
	tag := d.opaque(v4OpaqueLimit)
	minor := d.u32()
	n := d.u32()
	if d.err != nil {
		return rpcGarbageArgs
	}
	at := res.len()
	res.u32(uint32(stOK))
	res.opaque(tag)
	count := res.len()
	res.u32(0)
	if minor != 0 {
		res.put32(at, uint32(stMinorVersMismatch))
		return rpcSuccess
	}

	x := v4compound{s: s, conn: c.conn, d: d, e: res, limit: at + v4ReplyBudget}
	st := stOK
	var done uint32
	for st == stOK && done < n {
		done++
		op := d.u32()
		if d.err != nil {
			res.u32(op4Illegal)
			res.u32(uint32(stBadXDR))
			st = stBadXDR
			break
		}
		if op < op4Access || op > op4ReleaseLockOwner {
			op = op4Illegal
		}
		res.u32(op)
		mark := res.len()
		res.u32(0)
		switch {
		case n > v4MaxOps:
			// NFSv4.0 has no NFS4ERR_TOO_MANY_OPS: the first operation
			// fails with NFS4ERR_RESOURCE, as on Linux, before any runs.
			st = stResource
		case x.room() <= 0:
			st = stResource
		default:
			st = x.op(op)
		}
		if st != stOpIllegal && st != stBadXDR && (identityError(s.FS) != nil || x.cur.ok && x.cur.id == 0) {
			st = stIO
		}
		res.put32(mark, uint32(st))
		if st != stOK && !opKeepsBody4(op, st) {
			res.truncate(mark + 4)
		}
	}
	res.put32(at, uint32(st))
	res.put32(count, done)
	return rpcSuccess
}

// opKeepsBody4 reports the failures whose result carries more than the
// status: SETATTR's attrsset, and the conflicting lock of a LOCK or LOCKT
// denied.
func opKeepsBody4(op uint32, st status) bool {
	return op == op4SetAttr || (op == op4Lock || op == op4LockT) && st == stDenied
}

// v4compound is what a COMPOUND carries from operation to operation.
type v4compound struct {
	s    *Server
	conn *conn
	d    *decoder
	e    *encoder
	// limit is the reply length an operation may not start past
	// (v4ReplyBudget); READ and READDIR fit within it.
	limit int
	cur   v4fh
	// saved is SAVEFH's filehandle: RENAME's source directory, LINK's
	// source file.
	saved v4fh
	// client is the client an operation of this COMPOUND named, if any.
	client uint64
}

// v4fh is a filehandle in use: the file id and the path it resolved to.
type v4fh struct {
	id   uint64
	path string
	ok   bool
}

// op runs one operation: it decodes its arguments, encodes its result after
// the status, and returns the status.
func (x *v4compound) op(op uint32) status {
	switch op {
	case op4Access:
		return x.access()
	case op4Close:
		return x.close()
	case op4Commit:
		return x.commit()
	case op4Create:
		return x.create()
	case op4DelegPurge, op4OpenAttr:
		// Neither applies: no delegation survives a restart to purge, and
		// there are no named attributes.
		return stNotSupp
	case op4DelegReturn:
		return x.delegReturn()
	case op4GetAttr:
		return x.getattr()
	case op4GetFH:
		return x.getfh()
	case op4Link:
		return x.link()
	case op4Lock:
		return x.lock()
	case op4LockT:
		return x.lockt()
	case op4LockU:
		return x.locku()
	case op4Lookup:
		return x.lookup()
	case op4LookupP:
		return x.lookupp()
	case op4NVerify:
		return x.verify(false)
	case op4Open:
		return x.open()
	case op4OpenConfirm:
		return x.openConfirm()
	case op4OpenDowngrade:
		return x.openDowngrade()
	case op4PutFH:
		return x.putfh()
	case op4PutPubFH, op4PutRootFH:
		// The public filehandle is the root: there is one export.
		return x.putrootfh()
	case op4Read:
		return x.read()
	case op4ReadDir:
		return x.readdir()
	case op4ReadLink:
		return x.readlink()
	case op4Remove:
		return x.remove()
	case op4Rename:
		return x.rename()
	case op4Renew:
		return x.renew()
	case op4RestoreFH:
		return x.restorefh()
	case op4SaveFH:
		return x.savefh()
	case op4SecInfo:
		return x.secinfo()
	case op4SetAttr:
		return x.setattr()
	case op4SetClientID:
		return x.setClientID()
	case op4SetClientIDConfirm:
		return x.setClientIDConfirm()
	case op4Verify:
		return x.verify(true)
	case op4Write:
		return x.write()
	case op4ReleaseLockOwner:
		return x.releaseLockOwner()
	}
	return stOpIllegal
}

// room is how much more the reply may hold.
func (x *v4compound) room() int { return x.limit - x.e.len() }

// note records the client an operation (OPEN, RENEW, a stateid) shows the
// COMPOUND comes from. An NFSv4.0 COMPOUND names no client otherwise, so the
// connection remembers it for requests that name none, such as REMOVE: a
// client uses its own connections.
func (x *v4compound) note(client uint64) {
	if client == 0 {
		return
	}
	x.client = client
	x.conn.v4client.Store(client)
}

// who is the client the COMPOUND comes from, as far as known, or 0.
func (x *v4compound) who() uint64 {
	if x.client != 0 {
		return x.client
	}
	return x.conn.v4client.Load()
}

// current returns the current filehandle, NFS4ERR_NOFILEHANDLE without one.
func (x *v4compound) current() (*v4fh, status) {
	if !x.cur.ok {
		return nil, stNoFileHandle
	}
	return &x.cur, stOK
}

// setCurrent makes the file at p the current filehandle.
func (x *v4compound) setCurrent(p string) {
	x.cur = v4fh{id: x.s.FS.FileID(p), path: p, ok: true}
}

// name decodes a component4 and checks it.
func (x *v4compound) name() (string, status) {
	name := x.d.str(v4ArgLimit)
	if x.d.err != nil {
		return "", stBadXDR
	}
	return name, v4Component(name)
}

// v4Component checks a component4: one non-empty path element naming an
// entry of a directory. "." and ".." name none in NFSv4 (LOOKUPP finds a
// parent), and joined onto a path they would reach the directory or its
// parent, so REMOVE ".." could remove the parent and RENAME ".." move it.
func v4Component(name string) status {
	if name == "" || name == "." || name == ".." || len(name) > v4OpaqueLimit || strings.ContainsAny(name, "/\x00") {
		return stBadName
	}
	return stOK
}

// v4Join is path.Join(dir, name) for a checked component name.
func v4Join(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

// dir4 checks that p is a directory.
func (s *Server) dir4(p string) status {
	info, err := s.FS.Lstat(p)
	if err != nil {
		return statusOf(err)
	}
	if !info.IsDir() {
		return stNotDir
	}
	return stOK
}

// putfh sets the current filehandle (PUTFH). Handles are file ids, so a
// handle of the wrong length is not one of this server's and one whose id
// is gone is stale.
func (x *v4compound) putfh() status {
	id, ok := x.d.fh(v4ArgLimit)
	if x.d.err != nil {
		return stBadXDR
	}
	if !ok {
		return stBadHandle
	}
	p, live := x.s.FS.Path(id)
	if !live {
		return stStale
	}
	x.cur = v4fh{id: id, path: p, ok: true}
	return stOK
}

// putrootfh makes the export's root the current filehandle (PUTROOTFH,
// PUTPUBFH).
func (x *v4compound) putrootfh() status {
	x.setCurrent("")
	return stOK
}

// getfh returns the current filehandle (GETFH).
func (x *v4compound) getfh() status {
	cur, st := x.current()
	if st != stOK {
		return st
	}
	x.e.fh(cur.id)
	return stOK
}

// savefh saves the current filehandle (SAVEFH).
func (x *v4compound) savefh() status {
	cur, st := x.current()
	if st != stOK {
		return st
	}
	x.saved = *cur
	return stOK
}

// restorefh makes the saved filehandle current (RESTOREFH).
func (x *v4compound) restorefh() status {
	if !x.saved.ok {
		return stNoFileHandle
	}
	x.cur = x.saved
	return stOK
}

// lookup makes an entry of the current directory current (LOOKUP).
func (x *v4compound) lookup() status {
	name, st := x.name()
	if st != stOK {
		return st
	}
	cur, st := x.current()
	if st != stOK {
		return st
	}
	// An entry found shows the current file is a directory; only a miss
	// needs to tell NFS4ERR_NOTDIR from the entry's absence.
	p := v4Join(cur.path, name)
	if _, err := x.s.FS.Lstat(p); err != nil {
		if st := x.s.dir4(cur.path); st != stOK {
			return st
		}
		return statusOf(err)
	}
	x.setCurrent(p)
	return stOK
}

// lookupp makes the current directory's parent current (LOOKUPP); the root
// has none.
func (x *v4compound) lookupp() status {
	cur, st := x.current()
	if st != stOK {
		return st
	}
	if cur.path == "" {
		return stNoEnt
	}
	parent := path.Dir(cur.path)
	if parent == "." {
		parent = ""
	}
	x.setCurrent(parent)
	return stOK
}

// access answers ACCESS with every right asked for: the backend checks
// permissions when the operation comes.
func (x *v4compound) access() status {
	want := x.d.u32()
	if x.d.err != nil {
		return stBadXDR
	}
	if _, st := x.current(); st != stOK {
		return st
	}
	x.e.u32(access4All)
	x.e.u32(want & access4All)
	return stOK
}

// getattr returns the current file's attributes (GETATTR).
func (x *v4compound) getattr() status {
	req, _ := getBitmap4(x.d)
	if x.d.err != nil {
		return stBadXDR
	}
	cur, st := x.current()
	if st != stOK {
		return st
	}
	a, err := x.s.stat(cur.id, cur.path)
	if err != nil {
		return statusOf(err)
	}
	var fs v4fsstat
	return x.s.putAttrs4(x.e, &a, req, &fs)
}

// verify compares the current file's attributes with those given, encoded
// alike: VERIFY (same) continues only if they match, NVERIFY only if not.
func (x *v4compound) verify(same bool) status {
	mask, extra := getBitmap4(x.d)
	vals := x.d.opaque(v4ArgLimit)
	if x.d.err != nil {
		return stBadXDR
	}
	cur, st := x.current()
	if st != stOK {
		return st
	}
	if mask.has(attr4RDAttrError) || mask.intersects(attr4WriteOnly) {
		return stInval
	}
	if extra || !mask.within(attr4Supported) {
		return stAttrNotSupp
	}
	a, err := x.s.stat(cur.id, cur.path)
	if err != nil {
		return statusOf(err)
	}
	// Encode the current values where the result goes, compare and drop
	// them: VERIFY and NVERIFY results carry only the status.
	at := x.e.len()
	var fs v4fsstat
	if st := x.s.putAttrVals4(x.e, &a, mask, &fs); st != stOK {
		return st
	}
	equal := bytes.Equal(x.e.buf[at:], vals)
	x.e.truncate(at)
	switch {
	case same && !equal:
		return stNotSame
	case !same && equal:
		return stSame
	}
	return stOK
}

// readlink returns a symlink's target (READLINK).
func (x *v4compound) readlink() status {
	cur, st := x.current()
	if st != stOK {
		return st
	}
	target, st := x.s.readlink(cur.path)
	if st != stOK {
		return st
	}
	x.e.str(target)
	return stOK
}

// secinfo offers AUTH_SYS and AUTH_NONE for any name (SECINFO): a secinfo4
// for either is its flavor alone.
func (x *v4compound) secinfo() status {
	if _, st := x.name(); st != stOK {
		return st
	}
	if _, st := x.current(); st != stOK {
		return st
	}
	x.e.u32(2)
	x.e.u32(auth4Sys)
	x.e.u32(auth4None)
	return stOK
}
