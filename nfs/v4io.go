package nfs

import (
	"errors"
	"io"
	"math"
	"os"

	"github.com/go-git/go-billy/v5"
	"github.com/stencil-hq/ovnfs/internal/kept"
)

// NFSv4.0 READ, WRITE, COMMIT and SETATTR.

// stable_how4 values. DATA_SYNC is implemented as FILE_SYNC.
const (
	stable4Unstable = 0
	stable4FileSync = 2
)

// regular4 checks that info describes a regular file, for READ or WRITE.
func regular4(info os.FileInfo) status {
	switch {
	case info.IsDir():
		return stIsDir
	case !info.Mode().IsRegular():
		return stInval
	}
	return stOK
}

// read answers READ, reading into the reply in place.
func (x *v4compound) read() status {
	id := getStateID4(x.d)
	offset := x.d.u64()
	count := x.d.u32()
	if x.d.err != nil {
		return stBadXDR
	}
	cur, s := x.current()
	if s != stOK {
		return s
	}
	client, _, s := x.s.v4.checkIO(id, cur.id, false)
	if s != stOK {
		return s
	}
	x.note(client)
	info, s := x.follow(cur)
	if s != stOK {
		return s
	}
	if s := regular4(info); s != stOK {
		return s
	}
	size := info.Size()
	// The data must fit the reply's budget, after eof, its length and its
	// padding: a short read, which the client continues.
	n := min(int(min(count, maxIO)), x.room()-12)
	if count > 0 && n <= 0 {
		return stResource
	}
	if n <= 0 || offset >= uint64(size) {
		x.e.bool(offset >= uint64(size))
		x.e.u32(0)
		return stOK
	}
	eofAt := x.e.len()
	x.e.u32(0)
	f, info, err := x.openFollowed(cur, os.O_RDONLY)
	if err != nil {
		return statusOf(err)
	}
	defer f.Close()
	size = info.Size() // the descriptor's, like the bytes
	got, err := x.e.opaqueFrom(n, func(b []byte) (int, error) { return f.ReadAt(b, int64(offset)) })
	eof := errors.Is(err, io.EOF)
	if err != nil && !eof {
		return statusOf(err)
	}
	if eof || offset+uint64(got) >= uint64(size) {
		x.e.put32(eofAt, 1)
	}
	return stOK
}

// write answers WRITE: through the kept file of a write open whose file
// lost its owner's write permission (keepWritable), else through a file
// opened for the write. A SETATTR taking the permission may come between
// the two, making the open fail; the kept file it left serves then.
func (x *v4compound) write() status {
	id := getStateID4(x.d)
	offset := x.d.u64()
	stable := x.d.u32()
	data := x.d.opaque(maxRecord)
	if x.d.err != nil {
		return stBadXDR
	}
	if stable > stable4FileSync {
		return stInval
	}
	cur, s := x.current()
	if s != stOK {
		return s
	}
	st := &x.s.v4
	client, held, s := st.checkIO(id, cur.id, true)
	if s != stOK {
		return s
	}
	defer func() { st.release(held) }()
	x.note(client)
	if s := x.conflict(cur.id); s != stOK {
		return s
	}
	info, s := x.follow(cur)
	if s != stOK {
		return s
	}
	if s := regular4(info); s != stOK {
		return s
	}
	if offset > math.MaxInt64-uint64(len(data)) {
		return stFBig
	}
	var gen uint64
	if stable != stable4Unstable {
		gen = x.handleGen(cur)
	}
	var f billy.File
	if held == nil {
		var err error
		f, _, err = x.openFollowed(cur, os.O_WRONLY)
		if errors.Is(err, os.ErrPermission) && client != 0 {
			held = st.kept(id, cur.id)
		}
		if err != nil && held == nil {
			return statusOf(err)
		}
	}
	if held != nil {
		f = held.f
	}
	n, err := kept.WriteAt(f, data, int64(offset))
	if err == nil {
		x.changed(cur.id)
		if x.s.NoSync {
			stable = stable4FileSync
		} else if stable != stable4Unstable {
			err = x.s.FS.SyncFile(cur.path, cur.id, gen, f)
			stable = stable4FileSync
		}
	}
	if held == nil {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		return statusOf(err)
	}
	x.e.u32(uint32(n))
	x.e.u32(stable)
	x.e.fixed(x.s.verf[:])
	return stOK
}

// commit answers COMMIT, syncing the whole file even when only a range
// was requested. With [Server.NoSync] it syncs nothing.
func (x *v4compound) commit() status {
	x.d.u64() // offset
	x.d.u32() // count
	if x.d.err != nil {
		return stBadXDR
	}
	cur, s := x.current()
	if s != stOK {
		return s
	}
	if !x.s.NoSync {
		var writer billy.File
		if kept := x.s.v4.commitWriter(cur.id); kept != nil {
			defer x.s.v4.release(kept)
			writer = kept.f
		}
		if err := x.s.FS.SyncFile(cur.path, cur.id, x.handleGen(cur), writer); err != nil {
			return statusOf(err)
		}
	}
	x.e.fixed(x.s.verf[:])
	return stOK
}

// setattr answers SETATTR with the attributes it set; SETATTR4res carries
// attrsset whatever the status, empty on failure.
func (x *v4compound) setattr() status {
	set, s := x.setattrs()
	if s != stOK {
		set = v4bitmap{}
	}
	set.put(x.e)
	return s
}

// setattrs sets the current file's attributes. A delegation stateid may
// not change the size; any change recalls other clients' delegations.
func (x *v4compound) setattrs() (v4bitmap, status) {
	id := getStateID4(x.d)
	sa, s := getSattr4(x.d)
	if s != stOK {
		return v4bitmap{}, s
	}
	cur, s := x.current()
	if s != stOK {
		return v4bitmap{}, s
	}
	// kept, the file kept for a write open of a file that lost its owner's
	// write permission before, truncates it. A mode this SETATTR sets
	// applies after the size (apply4), so the file it keeps now is not
	// needed for that.
	client, kept, s := x.s.v4.checkIO(id, cur.id, sa.mask.has(attr4Size))
	if s != stOK {
		return v4bitmap{}, s
	}
	defer x.s.v4.release(kept)
	x.note(client)
	if s := x.conflict(cur.id); s != stOK {
		return v4bitmap{}, s
	}
	info, s := x.follow(cur)
	if s != stOK {
		return v4bitmap{}, s
	}
	if sa.mask.has(attr4Mode) {
		x.keepWritable(cur, sa.mode)
	}
	var kf billy.File
	if kept != nil {
		kf = kept.f
	}
	a := attrsOf(cur.id, info)
	if s := x.s.applyTo4(&a, cur.path, &sa, kf); s != stOK {
		return v4bitmap{}, s
	}
	x.changed(cur.id)
	return sa.mask, stOK
}
