package nfs

import (
	"os"
	"sync"
)

// NFSv4.0 directory operations: READDIR, and CREATE, REMOVE, RENAME and
// LINK, which report atomic change_info4 (see v4DirLocks).

// v4DirLocks serializes the NFSv4 operations that change a directory's
// entries (OPEN creating, CREATE, REMOVE, RENAME, LINK), so the change_info4
// each one reports brackets that operation alone and is marked atomic. The
// macOS client keeps its cached names of a directory across an atomic
// change that starts at the change attribute it last saw; any other
// change_info4 makes it purge them all, and every later open looks its name
// up again (one LOOKUP per open of an unchanged file, 15 per created one).
//
// Changes made outside NFSv4 operations in the same instant (NFSv3, the
// embedder) can still fall between the two readings; the client then misses
// that one change in its name cache until the directory changes again.
type v4DirLocks struct {
	mu    sync.Mutex
	locks map[uint64]*v4DirLock
}

// v4DirLock is one directory's lock, freed when no operation needs it.
type v4DirLock struct {
	sync.Mutex
	refs int
}

// lock locks directories a and b (the same for one directory), in a fixed
// order so two renames between the same directories cannot deadlock, and
// returns the function that unlocks them.
func (d *v4DirLocks) lock(a, b uint64) func() {
	ids := [2]uint64{min(a, b), max(a, b)}
	n := 2
	if a == b {
		n = 1
	}
	var held [2]*v4DirLock
	d.mu.Lock()
	if d.locks == nil {
		d.locks = make(map[uint64]*v4DirLock)
	}
	for i, id := range ids[:n] {
		l := d.locks[id]
		if l == nil {
			l = &v4DirLock{}
			d.locks[id] = l
		}
		l.refs++
		held[i] = l
	}
	d.mu.Unlock()
	for _, l := range held[:n] {
		l.Lock()
	}
	return func() {
		for _, l := range held[:n] {
			l.Unlock()
		}
		d.mu.Lock()
		for i, l := range held[:n] {
			if l.refs--; l.refs == 0 {
				delete(d.locks, ids[i])
			}
		}
		d.mu.Unlock()
	}
}

// readdir answers READDIR from the directory's listing (Server.list): entry
// i has cookie firstCookie+i, and a continuation pages through the listing
// its verifier names. A verifier that no longer matches the directory is
// NFS4ERR_NOT_SAME, as RFC 7530 section 16.24.4 requires; a cookie the
// listing never issued is NFS4ERR_BAD_COOKIE. dircount is a hint and is not
// used.
func (x *v4compound) readdir() status {
	cookie := x.d.u64()
	verf := x.d.u64()
	x.d.u32() // dircount
	maxcount := x.d.u32()
	req, _ := getBitmap4(x.d)
	if x.d.err != nil {
		return stBadXDR
	}
	cur, s := x.current()
	if s != stOK {
		return s
	}
	if s := x.s.dir4(cur.path); s != stOK {
		return s
	}
	l, s := x.s.list(cur.id, cur.path, cookie, verf)
	if s == stBadCookie {
		return stNotSame
	}
	if s != stOK {
		return s
	}
	i, ok := l.after(cookie)
	if !ok {
		return stBadCookie
	}
	// maxcount bounds READDIR4resok: the verifier, the entries and the 8
	// bytes that end the list. The reply's budget bounds it too.
	limit := int(maxcount)
	tooSmall := stTooSmall
	if r := x.room(); r < limit {
		limit, tooSmall = r, stResource
	}
	x.e.u64(l.verf)
	start := x.e.len()
	eof := true
	var fs v4fsstat
	names := reported4(req) == v4bitmap{}
	for ; i < len(l.entries); i++ {
		info := l.entries[i]
		mark := x.e.len()
		x.e.bool(true) // an entry follows
		x.e.u64(firstCookie + uint64(i))
		x.e.str(info.Name())
		if names {
			// Names alone: an empty fattr4, sparing the entry's id.
			v4bitmap{}.put(x.e)
			x.e.u32(0)
		} else {
			id := x.s.FS.FileID(v4Join(cur.path, info.Name()))
			if id == 0 {
				return stIO
			}
			a := attrsOf(id, info)
			if s := x.s.putAttrs4(x.e, &a, req, &fs); s != stOK {
				return s
			}
		}
		if 8+x.e.len()-start+8 > limit {
			x.e.truncate(mark)
			if mark == start {
				return tooSmall
			}
			eof = false
			break
		}
	}
	x.e.bool(false)
	x.e.bool(eof)
	return stOK
}

// create answers CREATE of directories and symbolic links; regular files
// are created by OPEN, and device and special files are not supported.
func (x *v4compound) create() status {
	typ := fileType(x.d.u32())
	var target string
	switch typ {
	case typeLnk:
		target = x.d.str(v4ArgLimit)
	case typeBlk, typeChr:
		x.d.u32() // specdata4
		x.d.u32()
	}
	name := x.d.str(v4ArgLimit)
	sa, sast := getSattr4(x.d)
	if x.d.err != nil {
		return stBadXDR
	}
	cur, s := x.current()
	if s != stOK {
		return s
	}
	switch typ {
	case typeDir, typeLnk, typeSock, typeFifo:
	case typeBlk, typeChr:
		return stNotSupp
	default:
		return stBadType
	}
	if s := v4Component(name); s != stOK {
		return s
	}
	if typ == typeLnk && (target == "" || len(target) > v4OpaqueLimit) {
		return stInval
	}
	if sast != stOK {
		return sast
	}
	if s := x.s.dir4(cur.path); s != stOK {
		return s
	}
	dir := *cur
	p := v4Join(dir.path, name)
	cinfo := v4cinfo{atomic: true}
	s = func() status {
		defer x.s.v4.dirs.lock(dir.id, dir.id)()
		if _, err := x.s.FS.Lstat(p); err == nil {
			return stExist
		}
		cinfo.before = x.s.changeAt4(dir.path)
		var err error
		if typ == typeLnk {
			err = x.s.FS.Symlink(target, p)
		} else if typ == typeSock || typ == typeFifo {
			maker, ok := x.s.FS.(interface {
				Mknod(string, os.FileMode) error
			})
			if !ok {
				return stNotSupp
			}
			mode := os.FileMode(0o777)
			if sa.mask.has(attr4Mode) {
				mode = goMode(sa.mode)
			}
			if typ == typeSock {
				mode |= os.ModeSocket
			} else {
				mode |= os.ModeNamedPipe
			}
			err = maker.Mknod(p, mode)
		} else {
			mode := goMode(0o777)
			if sa.mask.has(attr4Mode) {
				mode = goMode(sa.mode)
			}
			err = x.s.FS.MkdirAll(p, mode)
		}
		if err != nil {
			return statusOf(err)
		}
		if s := x.s.apply4(p, &sa, nil); s != stOK {
			return s
		}
		cinfo.after = x.s.changeAt4(dir.path)
		return stOK
	}()
	if s != stOK {
		return s
	}
	x.setCurrent(p)
	cinfo.put(x.e)
	sa.mask.put(x.e)
	return stOK
}

// remove answers REMOVE.
func (x *v4compound) remove() status {
	name, s := x.name()
	if s != stOK {
		return s
	}
	cur, s := x.current()
	if s != stOK {
		return s
	}
	if s := x.s.dir4(cur.path); s != stOK {
		return s
	}
	id, s := x.conflictName(cur.path, name)
	if s != stOK {
		return s
	}
	unlock := x.s.v4.dirs.lock(cur.id, cur.id)
	cinfo := v4cinfo{atomic: true, before: x.s.changeAt4(cur.path)}
	err := x.s.FS.Remove(v4Join(cur.path, name))
	cinfo.after = x.s.changeAt4(cur.path)
	unlock()
	if err != nil {
		return statusOf(err)
	}
	if id != 0 {
		// A delegation granted while the file was being removed.
		x.changed(id)
	}
	cinfo.put(x.e)
	return stOK
}

// rename answers RENAME of a name in the saved directory to a name in the
// current one. The file keeps its id, so handles the client holds follow
// it.
func (x *v4compound) rename() status {
	from := x.d.str(v4ArgLimit)
	to := x.d.str(v4ArgLimit)
	if x.d.err != nil {
		return stBadXDR
	}
	if !x.saved.ok {
		return stNoFileHandle
	}
	src := x.saved
	cur, s := x.current()
	if s != stOK {
		return s
	}
	dst := *cur
	if s := v4Component(from); s != stOK {
		return s
	}
	if s := v4Component(to); s != stOK {
		return s
	}
	if s := x.s.dir4(src.path); s != stOK {
		return s
	}
	if s := x.s.dir4(dst.path); s != stOK {
		return s
	}
	if _, s := x.conflictName(src.path, from); s != stOK {
		return s
	}
	replaced, s := x.conflictName(dst.path, to)
	if s != stOK {
		return s
	}
	newPath := v4Join(dst.path, to)
	unlock := x.s.v4.dirs.lock(src.id, dst.id)
	srcInfo := v4cinfo{atomic: true, before: x.s.changeAt4(src.path)}
	dstInfo := v4cinfo{atomic: true, before: x.s.changeAt4(dst.path)}
	err := x.s.FS.Rename(v4Join(src.path, from), newPath)
	srcInfo.after = x.s.changeAt4(src.path)
	dstInfo.after = x.s.changeAt4(dst.path)
	unlock()
	if err != nil {
		return statusOf(err)
	}
	// Recall delegations granted while the change was under way: of the
	// replaced file, and of the moved one, whose ctime (reported as its
	// mtime) a rename moves.
	if replaced != 0 {
		x.changed(replaced)
	}
	if x.s.v4.active() {
		x.changed(x.s.FS.FileID(newPath))
	}
	srcInfo.put(x.e)
	dstInfo.put(x.e)
	return stOK
}

// link answers LINK: the saved filehandle is the file, the current one the
// directory the new name goes in.
func (x *v4compound) link() status {
	name := x.d.str(v4ArgLimit)
	if x.d.err != nil {
		return stBadXDR
	}
	if !x.saved.ok {
		return stNoFileHandle
	}
	src := x.saved
	cur, s := x.current()
	if s != stOK {
		return s
	}
	dir := *cur
	if s := v4Component(name); s != stOK {
		return s
	}
	if s := x.s.dir4(dir.path); s != stOK {
		return s
	}
	info, err := x.s.FS.Lstat(src.path)
	if err != nil {
		return statusOf(err)
	}
	if info.IsDir() {
		return stIsDir
	}
	if s := x.conflict(src.id); s != stOK {
		return s
	}
	unlock := x.s.v4.dirs.lock(dir.id, dir.id)
	cinfo := v4cinfo{atomic: true, before: x.s.changeAt4(dir.path)}
	err = x.s.FS.Link(src.path, v4Join(dir.path, name))
	cinfo.after = x.s.changeAt4(dir.path)
	unlock()
	if err != nil {
		return statusOf(err)
	}
	// The link count, and so the change attribute, of the file moved.
	x.changed(src.id)
	cinfo.put(x.e)
	return stOK
}
