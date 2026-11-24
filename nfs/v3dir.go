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
// snapshot the cookie verifier names (see Server.list), or from Server.resume3
// when that snapshot is gone and the directory changed. "." and ".." come
// first, with cookies 1 and 2; entries[i] has cookie first+i. Entries are
// added while the result stays within the client's byte budgets, measured on
// the encoded reply; when not even one fits, the call fails TOOSMALL.
func (s *Server) list3(d *decoder, res *encoder, plus bool) status {
	id, gen, ok := d.fh(maxFH3)
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
	if !ok {
		return stBadHandle
	}
	epoch := s.epoch.Load()
	p, gen, live := s.resolve(id, gen)
	if !live {
		return stStale
	}
	dir := listingDir{id: id, gen: gen, path: p, epoch: epoch}
	// The type is checked rather than left to the listing's error: Windows
	// fails it with ENOTDIR, which is ERROR_PATH_NOT_FOUND there, so ENOENT.
	da, err := s.stat(id, p)
	if err == nil && da.typ != typeDir {
		return stNotDir
	}
	l, st := s.list(dir, cookie, verf)
	if st == stBadCookie {
		l, st = s.resume3(dir, l, cookie, verf)
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
		var upID uint64
		var a *attrs
		if plus {
			info, err := s.FS.Lstat(up)
			if err != nil {
				return stIO
			}
			upID = s.FS.IDOf(up, info)
			pa := attrsOf(upID, info)
			a = &pa
		} else {
			upID = s.FS.FileID(up)
		}
		if upID == 0 {
			return stIO
		}
		fits = r.add("..", 2, upID, a)
	}
	var lastCookie uint64
	var lastName string
	// A continuation reports current attributes: the client keeps those of
	// the newest reply, so a snapshot's could undo a change it saw since.
	// Namespace changes and unknown writers require a stat of every entry;
	// a write to a known file need not make the rest of the directory pay.
	continuing := plus && cookie != 0
	refreshAll := continuing && (s.changes.all.Load() > l.changes ||
		s.changes.slot(id).Load() > l.changes ||
		s.changes.dir(id).Load() > l.changes ||
		!s.FS.Cacheable(p))
	checkFiles := continuing && !refreshAll && s.changes.clock.Load() != l.changes
	for i := start; fits && i < len(l.entries); i++ {
		info := l.entries[i]
		var childID uint64
		if !refreshAll {
			childID = s.FS.ChildID(p, info)
		}
		if refreshAll || checkFiles && s.changes.file(childID).Load() > l.changes {
			// Refresh before checking space: a vanished entry must not
			// prevent a later, shorter name from fitting this page.
			child := path.Join(p, info.Name())
			fresh, err := s.FS.Lstat(child)
			if err != nil {
				continue
			}
			info, childID = fresh, s.FS.IDOf(child, fresh)
		}
		var a attrs
		if !r.room(info.Name(), &a) {
			fits = false
			break
		}
		c := l.first + uint64(i)
		if childID == 0 {
			return stIO
		}
		if plus {
			a = attrsOf(childID, info)
			fits = r.add(info.Name(), c, a.fileid, &a)
		} else {
			fits = r.add(info.Name(), c, childID, nil)
		}
		if fits {
			lastCookie, lastName = c, info.Name()
		}
	}
	if fits {
		lastName = "" // EOF needs no continuation point.
	}
	if !fits && r.n == 0 {
		return stTooSmall
	}
	res.bool(false) // no further entry
	res.bool(fits)  // eof
	return s.finishPage(dir, l.verf, lastCookie, lastName)
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
	if !r.room(name, a) {
		return false
	}
	r.res.bool(true)
	r.res.u64(id)
	r.res.str(name)
	r.res.u64(cookie)
	if r.plus {
		if a != nil {
			postAttrs3(r.res, a)
			r.res.bool(true)
			r.res.fh(id, r.s.FS.Generation(id))
		} else {
			r.res.bool(false)
			r.res.bool(false)
		}
	}
	entry := uint32(20 + len(name) + pad(len(name)))
	r.dirBytes += entry
	r.n++
	return true
}

// room checks encoded sizes before the reply buffer grows.
func (r *dirReply3) room(name string, a *attrs) bool {
	entry := uint64(20 + len(name) + pad(len(name)))
	extra := uint64(0)
	if r.plus {
		extra = 8 // absent attributes and handle
		if a != nil {
			extra += 84 + 4 + fhSize // fattr3 and opaque handle
		}
	}
	// Include this entry's list discriminator, the terminator and eof.
	return uint64(r.res.len()-r.base)+4+entry+extra+8 <= uint64(r.maxcount) &&
		uint64(r.dirBytes)+entry <= uint64(r.dircount)
}
