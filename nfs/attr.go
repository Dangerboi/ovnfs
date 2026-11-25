package nfs

import (
	"encoding/binary"
	"os"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/stencil-hq/ovnfs/internal/unixstat"
)

// fileType is an ftype3 or nfs_ftype4; the two number these types alike.
type fileType uint32

// File types.
const (
	typeReg fileType = 1 + iota
	typeDir
	typeBlk
	typeChr
	typeLnk
	typeSock
	typeFifo
)

// attrs is the NFS view of one file, from which fattr3 and NFSv4
// attributes are encoded.
type attrs struct {
	typ          fileType
	mode         uint32 // permission, setuid, setgid and sticky bits
	nlink        uint32
	uid, gid     uint32
	size         uint64 // also reported as the space used
	major, minor uint32
	fileid       uint64
	// mtime is also reported as atime and ctime, as by every ovnfs frontend;
	// the backend keeps no separate change time for every layer.
	mtime time.Time
}

// attrsOf describes file id, whose Lstat is info.
func attrsOf(id uint64, info os.FileInfo) attrs {
	a := attrs{
		typ:    typeOf(info.Mode()),
		mode:   posixMode(info.Mode()),
		nlink:  1,
		size:   uint64(max(info.Size(), 0)),
		fileid: id,
		mtime:  info.ModTime(),
	}
	if st, ok := unixstat.Of(info); ok {
		if st.Nlink > 0 {
			a.nlink = st.Nlink
		}
		a.uid, a.gid = st.UID, st.GID
		a.major, a.minor = st.Major, st.Minor
	}
	return a
}

// stat describes file id, at p, without following a final symlink; ESTALE
// when p holds another file ([Server.lstatID]).
func (s *Server) stat(id uint64, p string) (attrs, error) {
	info, err := s.lstatID(id, p)
	if err != nil {
		return attrs{}, err
	}
	return attrsOf(id, info), nil
}

// fileAttrs describes file id from its previously identity-checked open descriptor.
func (s *Server) fileAttrs(id uint64, f billy.File) (attrs, error) {
	fd, ok := f.(interface{ Stat() (os.FileInfo, error) })
	if !ok {
		return attrs{}, billy.ErrNotSupported
	}
	info, err := fd.Stat()
	if err != nil {
		return attrs{}, err
	}
	return attrsOf(id, info), nil
}

// lstatID is [Server.stat]'s FileInfo: ESTALE when p holds a file other than
// id, as it does once a rename put another file in its place.
func (s *Server) lstatID(id uint64, p string) (os.FileInfo, error) {
	info, err := s.FS.Lstat(p)
	if err == nil && s.FS.IDOf(p, info) != id {
		return nil, syscall.ESTALE
	}
	return info, err
}

// statAt describes the file at p, without following a final symlink, for
// callers that hold its path but not its id.
func (s *Server) statAt(p string) (attrs, error) {
	info, err := s.FS.Lstat(p)
	if err != nil {
		return attrs{}, err
	}
	return attrsOf(s.FS.IDOf(p, info), info), nil
}

// readlink returns the target of the symlink at p; INVAL when p is not a
// symlink, which hosts report with differing errors (EINVAL on Unix,
// ERROR_NOT_A_REPARSE_POINT on Windows).
func (s *Server) readlink(p string) (string, status) {
	target, err := s.FS.Readlink(p)
	if err != nil {
		if info, lerr := s.FS.Lstat(p); lerr == nil && info.Mode()&os.ModeSymlink == 0 {
			return "", stInval
		}
		return "", statusOf(err)
	}
	return target, stOK
}

// typeOf classifies a Go file mode.
func typeOf(m os.FileMode) fileType {
	switch {
	case m.IsDir():
		return typeDir
	case m&os.ModeSymlink != 0:
		return typeLnk
	case m&os.ModeCharDevice != 0:
		return typeChr
	case m&os.ModeDevice != 0:
		return typeBlk
	case m&os.ModeSocket != 0:
		return typeSock
	case m&os.ModeNamedPipe != 0:
		return typeFifo
	}
	return typeReg
}

// posixMode encodes a Go file mode's permission and special bits as mode3
// and mode4 do (07777).
func posixMode(m os.FileMode) uint32 {
	v := uint32(m.Perm())
	if m&os.ModeSetuid != 0 {
		v |= 0o4000
	}
	if m&os.ModeSetgid != 0 {
		v |= 0o2000
	}
	if m&os.ModeSticky != 0 {
		v |= 0o1000
	}
	return v
}

// goMode decodes a mode3 or mode4 for Chmod and file creation.
func goMode(v uint32) os.FileMode {
	m := os.FileMode(v) & os.ModePerm
	if v&0o4000 != 0 {
		m |= os.ModeSetuid
	}
	if v&0o2000 != 0 {
		m |= os.ModeSetgid
	}
	if v&0o1000 != 0 {
		m |= os.ModeSticky
	}
	return m
}

// ACCESS is advisory, not RPC authorization. Backend operations run as the
// daemon, so select its permission class rather than combining classes.
type accessIdentity struct {
	uid, gid int
	groups   []int
}

var daemonIdentity = sync.OnceValue(func() accessIdentity {
	groups, _ := os.Getgroups()
	return accessIdentity{os.Geteuid(), os.Getegid(), groups}
})

// permissions returns the daemon's rwx bits. Virtual files without POSIX
// ownership use the owner bits, including read-only export-root entries.
func (u accessIdentity) permissions(info os.FileInfo) os.FileMode {
	mode := info.Mode()
	st, ok := unixstat.Of(info)
	switch {
	case !ok || u.uid < 0:
		return mode.Perm() >> 6
	case u.uid == 0:
		if mode.IsDir() || mode.Perm()&0o111 != 0 {
			return 7
		}
		return 6
	case st.UID == uint32(u.uid):
		return mode.Perm() >> 6
	case st.GID == uint32(u.gid) || slices.Contains(u.groups, int(st.GID)):
		return mode >> 3 & 7
	default:
		return mode & 7
	}
}

// accessOf returns the requested, applicable rights (RFC 7530 §16.1)
// and the subset permitted by the mode. Deleting a non-directory depends
// on its parent, not this file handle.
func accessOf(info os.FileInfo, want uint32) (supported, granted uint32) {
	perm := daemonIdentity().permissions(info)
	supported = access4Read | access4Modify | access4Extend
	if perm&4 != 0 {
		granted |= access4Read
	}
	if info.IsDir() {
		supported |= access4Lookup | access4Delete
		if perm&1 != 0 {
			granted |= access4Lookup
			if perm&2 != 0 {
				granted |= access4Modify | access4Extend | access4Delete
			}
		}
	} else {
		if perm&2 != 0 {
			granted |= access4Modify | access4Extend
		}
		if info.Mode().IsRegular() {
			supported |= access4Execute
			if perm&1 != 0 {
				granted |= access4Execute
			}
		}
	}
	return want & supported, want & granted
}

// fhSize is the length of every file handle this server issues: the file id,
// then the generation Backend.Generation tagged it with, both big-endian,
// then padding to NFS3_FHSIZE that spreads the handle's byte sum (fhSpread).
const fhSize = 64

// fhKeySize is the length of the part of a handle that identifies the file.
const fhKeySize = 16

// fhSpread returns the padding of the handle of file id at generation gen.
//
// The macOS NFS client hashes file handles by summing their bytes (nfs_hash
// in Apple's nfs_node.c) and walks the bucket under one global mutex on every
// lookup, create and READDIRPLUS entry. Sequential ids and checkout
// generations sum to a few hundred distinct values, putting hundreds of nodes
// in every bucket. Padding whose bytes sum to a hash of (id, gen) across
// 0..len*255 gives the client about twenty times as many buckets.
func fhSpread(id, gen uint64) (pad [fhSize - fhKeySize]byte) {
	x := id ^ gen*0x9e3779b97f4a7c15
	x ^= x >> 33
	x *= 0xff51afd7ed558ccd
	x ^= x >> 33
	sum := int(x % uint64(len(pad)*255+1))
	for i := range pad {
		b := min(sum, 255)
		pad[i] = byte(b)
		sum -= b
	}
	return pad
}

// Legacy handle lengths that earlier servers issued: id-only (8 bytes) and
// unpadded id+generation (16 bytes). Clients still holding one after an
// upgrade get STALE, which makes them look the name up again, rather than
// BADHANDLE, which they treat as fatal; the export root's still resolves
// (Server.resolve), since a mount kept across the upgrade reaches everything
// else through it.
const (
	legacyIDOnlyFHSize   = 8
	legacyUnpaddedFHSize = 16
)

// legacyGen is the generation a legacy handle decodes to; no current handle
// carries it.
const legacyGen = ^uint64(0)

// handleOf decodes a file handle; ok is false for one no server can have
// issued (NFS3ERR_BADHANDLE, NFS4ERR_BADHANDLE). A legacy handle decodes to
// its id with legacyGen.
func handleOf(fh []byte) (id, gen uint64, ok bool) {
	switch len(fh) {
	case fhSize:
		return binary.BigEndian.Uint64(fh), binary.BigEndian.Uint64(fh[8:]), true
	case legacyIDOnlyFHSize, legacyUnpaddedFHSize:
		return binary.BigEndian.Uint64(fh), legacyGen, true
	}
	return 0, 0, false
}
