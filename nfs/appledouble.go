package nfs

import (
	"bytes"
	"sync"
)

// AppleDouble sidecars, dropped with [Server.DropAppleDouble].
//
// The macOS NFS client keeps the extended attributes, resource fork and
// Finder info of a file on an NFSv3 mount, which has no xattrs, in an
// AppleDouble "._name" file beside it, and the kernel tags every file and
// directory a process creates with com.apple.provenance: each new entry costs
// a second CREATE with its WRITEs, SETATTRs and GETATTRs. Dropping them, the
// server answers those calls without the backend. LOOKUP of a sidecar finds
// nothing, CREATE hands out a sidecar handle, REMOVE and RENAME of one change
// nothing, and calls on a sidecar handle succeed. It keeps only the size the
// client wrote to each recent sidecar, so the client's cached pages stay
// valid; a file's xattrs last as long as the client caches them.
//
// The client still sends every call. Refusing the CREATE would spare the
// rest, but the kernel then fails setxattr, which ditto and xattr -w report,
// and tries the CREATE again on each later attribute write.

// sidecarBit marks the file ids of sidecar handles: 2^62 and a sequence
// number, above every id a backend issues (below 2^53), so other procedures
// find a sidecar handle stale.
const sidecarBit = 1 << 62

// sidecarSizes is how many sidecars' sizes each of the two generations
// [sidecars] keeps; an older sidecar reads as empty.
const sidecarSizes = 1 << 16

// isSidecar reports an AppleDouble name, as overlay.AppleDouble does.
func isSidecar(name []byte) bool { return bytes.HasPrefix(name, []byte("._")) }

// sidecars numbers sidecar handles and keeps the sizes clients wrote to the
// recent ones.
type sidecars struct {
	mu        sync.Mutex
	seq       uint64
	cur, prev map[uint64]uint64
}

// create numbers a new, empty sidecar.
func (c *sidecars) create() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	id := sidecarBit | c.seq
	c.store(id, 0)
	return id
}

// resize applies update, when not nil, to sidecar id's size and returns its
// size before and after.
func (c *sidecars) resize(id uint64, update func(uint64) uint64) (old, size uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	old, ok := c.cur[id]
	if !ok {
		old = c.prev[id]
	}
	size = old
	if update != nil {
		size = update(old)
		c.store(id, size)
	}
	return old, size
}

// store records sidecar id's size, starting a generation once the current
// one is full; the caller holds mu.
func (c *sidecars) store(id, size uint64) {
	if len(c.cur) >= sidecarSizes {
		c.prev, c.cur = c.cur, nil
	}
	if c.cur == nil {
		c.cur = make(map[uint64]uint64)
	}
	c.cur[id] = size
}

// sidecarAttrs describes sidecar id at size. Its timestamps are the server's
// start, so the client sees no change to it but its own writes.
func sidecarAttrs(id, size uint64) attrs {
	u := daemonIdentity()
	return attrs{typ: typeReg, mode: 0o644, nlink: 1, uid: uint32(u.uid), gid: uint32(u.gid), size: size, fileid: id, mtime: start}
}

// sidecarWcc3 appends the wcc_data of sidecar id resized from old to size.
func sidecarWcc3(e *encoder, id, old, size uint64) {
	pre, post := sidecarAttrs(id, old), sidecarAttrs(id, size)
	preAttrs3(e, &pre)
	e.bool(true)
	fattr3(e, &post)
}

// sidecarCall answers an NFSv3 call on a sidecar, by handle or by name, from
// a copy of its arguments; false leaves the call to its procedure.
func (s *Server) sidecarCall(proc uint32, d decoder, res *encoder) (status, bool) {
	c := &s.v3.sidecars
	switch proc {
	case v3GetAttr, v3SetAttr, v3Access, v3Read, v3Write, v3Commit:
		id, _, ok := d.fh(maxFH3)
		if !ok || id&sidecarBit == 0 {
			return 0, false
		}
		switch proc {
		case v3GetAttr:
			_, size := c.resize(id, nil)
			a := sidecarAttrs(id, size)
			fattr3(res, &a)
		case v3SetAttr:
			sa := decodeSattr3(&d)
			var update func(uint64) uint64
			if sa.setSize {
				update = func(uint64) uint64 { return sa.size }
			}
			old, size := c.resize(id, update)
			sidecarWcc3(res, id, old, size)
		case v3Access:
			mask := d.u32()
			res.bool(false) // obj_attributes
			res.u32(mask)
		case v3Read:
			off, count := d.u64(), d.u32()
			_, size := c.resize(id, nil)
			var n uint64
			if off < size {
				n = min(uint64(count), size-off, maxIO)
			}
			a := sidecarAttrs(id, size)
			res.bool(true)
			fattr3(res, &a)
			res.u32(uint32(n))
			res.bool(off+n >= size)
			res.opaqueFrom(int(n), func(b []byte) (int, error) { clear(b); return len(b), nil })
		case v3Write:
			off := d.u64()
			d.u32() // count
			d.u32() // stable
			end := off + uint64(len(d.opaque(maxRecord)))
			old, size := c.resize(id, func(size uint64) uint64 { return max(size, end) })
			sidecarWcc3(res, id, old, size)
			res.u32(uint32(end - off))
			res.u32(fileSync3)
			res.fixed(s.verf[:])
		case v3Commit:
			_, size := c.resize(id, nil)
			sidecarWcc3(res, id, size, size)
			res.fixed(s.verf[:])
		}
		if d.err != nil {
			return stInval, true
		}
		return stOK, true
	case v3Lookup:
		d.fh(maxFH3)
		if name := d.opaque(maxPath3); d.err != nil || !isSidecar(name) {
			return 0, false
		}
		return stNoEnt, true
	case v3Create:
		dirID, dirGen, ok := d.fh(maxFH3)
		if name := d.opaque(maxPath3); d.err != nil || !isSidecar(name) {
			return 0, false
		}
		dir, st := s.sidecarDir(dirID, dirGen, ok)
		if st != stOK {
			return st, true
		}
		id := c.create()
		a := sidecarAttrs(id, 0)
		res.bool(true)
		res.fh(id, 0)
		res.bool(true)
		fattr3(res, &a)
		unchanged3(res, &dir)
		return stOK, true
	case v3Remove:
		dirID, dirGen, ok := d.fh(maxFH3)
		if name := d.opaque(maxPath3); d.err != nil || !isSidecar(name) {
			return 0, false
		}
		dir, st := s.sidecarDir(dirID, dirGen, ok)
		if st != stOK {
			return st, true
		}
		unchanged3(res, &dir)
		return stOK, true
	case v3Rename:
		fromID, fromGen, fromOK := d.fh(maxFH3)
		if name := d.opaque(maxPath3); d.err != nil || !isSidecar(name) {
			return 0, false
		}
		toID, toGen, toOK := d.fh(maxFH3)
		d.opaque(maxPath3)
		if d.err != nil {
			return 0, false
		}
		from, st := s.sidecarDir(fromID, fromGen, fromOK)
		if st != stOK {
			return st, true
		}
		to, st := s.sidecarDir(toID, toGen, toOK)
		if st != stOK {
			return st, true
		}
		unchanged3(res, &from)
		unchanged3(res, &to)
		return stOK, true
	}
	return 0, false
}

// sidecarDir describes the directory a sidecar CREATE, REMOVE or RENAME names.
func (s *Server) sidecarDir(id, gen uint64, ok bool) (attrs, status) {
	p, st := s.path3(id, gen, ok)
	if st != stOK {
		return attrs{}, st
	}
	info, err := s.FS.Lstat(p)
	if err != nil {
		return attrs{}, statusOf(err)
	}
	if !info.IsDir() {
		return attrs{}, stNotDir
	}
	return attrsOf(id, info), stOK
}

// unchanged3 appends the wcc_data of a directory a sidecar call left as it
// was: the same attributes before and after, so the client keeps its cached
// names.
func unchanged3(e *encoder, a *attrs) {
	preAttrs3(e, a)
	postAttrs3(e, a)
}
