package nfs

import (
	"math"
	"path"
)

// readdir3 answers READDIR (RFC 1813 section 3.3.16).
func (s *Server) readdir3(d *decoder, res *encoder) status { return s.list3(d, res, false) }

// readdirplus3 answers READDIRPLUS (section 3.3.17).
func (s *Server) readdirplus3(d *decoder, res *encoder) status { return s.list3(d, res, true) }

// list3 answers READDIR or, with plus, READDIRPLUS from the listing
// snapshot the cookie verifier names (see Server.list), or from Server.relist
// when that snapshot is gone and the directory changed. "." and ".." come
// first, with cookies 1 and 2; entries[i] has cookie first+i. Entries are
// added while the result stays within the client's byte budgets, measured on
// the encoded reply; when not even one fits, the call fails TOOSMALL.
func (s *Server) list3(d *decoder, res *encoder, plus bool) status {
	id, ok := d.fh(maxFH3)
	cookie := d.u64()
	verf := d.u64()
	r := dirReply3{s: s, res: res, plus: plus, base: res.len() - 4, dircount: math.MaxUint32}
	if plus {
		r.dircount = d.u32()
	}
	r.maxcount = min(d.u32(), maxIO)
	if d.err != nil {
		return stInval
	}
	p, st := s.path3(id, ok)
	if st != stOK {
		return st
	}
	// The type is checked rather than left to the listing's error: Windows
	// fails it with ENOTDIR, which is ERROR_PATH_NOT_FOUND there, so ENOENT.
	da, err := s.stat(id, p)
	if err == nil && da.typ != typeDir {
		return stNotDir
	}
	l, st := s.list(id, p, cookie, verf)
	if st == stBadCookie {
		l, st = s.relist(id, p, cookie)
	}
	if st != stOK {
		return st
	}
	start, ok := l.after(cookie)
	if !ok {
		return stBadCookie
	}
	if err != nil {
		res.bool(false)
	} else {
		postAttrs3(res, &da)
	}
	res.u64(l.verf)

	fits := true
	if cookie == 0 {
		dot := &da
		if err != nil {
			dot = nil
		}
		fits = r.add(".", 1, id, dot)
	}
	if fits && cookie <= 1 {
		up := parentOf(p)
		upID := s.FS.FileID(up)
		if upID == 0 {
			return stIO
		}
		var a *attrs
		if plus {
			if pa, err := s.stat(upID, up); err == nil {
				a = &pa
			}
		}
		fits = r.add("..", 2, upID, a)
	}
	for i := start; fits && i < len(l.entries); i++ {
		info := l.entries[i]
		child := path.Join(p, info.Name())
		c := l.first + uint64(i)
		childID := s.FS.FileID(child)
		if childID == 0 {
			return stIO
		}
		if plus {
			// A continuation's listing may be older than the client's
			// cached attributes, which it would replace: describe the
			// entry as it is now, and leave it out once it is gone.
			if cookie != 0 {
				fresh, err := s.FS.Lstat(child)
				if err != nil {
					continue
				}
				info = fresh
			}
			a := attrsOf(childID, info)
			fits = r.add(info.Name(), c, a.fileid, &a)
		} else {
			fits = r.add(info.Name(), c, childID, nil)
		}
	}
	if !fits && r.n == 0 {
		return stTooSmall
	}
	res.bool(false) // no further entry
	res.bool(fits)  // eof
	return stOK
}

// dirReply3 appends the entries of a READDIR or READDIRPLUS result within
// the client's budgets.
type dirReply3 struct {
	s    *Server
	res  *encoder
	plus bool
	// base marks the result's status: maxcount bounds the whole result.
	base     int
	maxcount uint32
	// dircount bounds the bytes of READDIRPLUS entries' ids, names and
	// cookies, leaving out attributes and handles (section 3.3.17).
	dircount uint32
	dirBytes uint32
	n        int
}

// add appends an entry (for READDIRPLUS, with attributes a, if known, and
// handle id); false, with nothing appended, when it does not fit.
func (r *dirReply3) add(name string, cookie, id uint64, a *attrs) bool {
	at := r.res.len()
	r.res.bool(true)
	r.res.u64(id)
	r.res.str(name)
	r.res.u64(cookie)
	if r.plus {
		if a != nil {
			postAttrs3(r.res, a)
			r.res.bool(true)
			r.res.fh(id)
		} else {
			r.res.bool(false)
			r.res.bool(false)
		}
	}
	entry := uint32(20 + len(name) + pad(len(name)))
	// 8 bytes close the result: the list's end and eof.
	if uint64(r.res.len()-r.base+8) > uint64(r.maxcount) || uint64(r.dirBytes)+uint64(entry) > uint64(r.dircount) {
		r.res.truncate(at)
		return false
	}
	r.dirBytes += entry
	r.n++
	return true
}
