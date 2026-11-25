package nfs

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"strings"
	"syscall"

	"github.com/go-git/go-billy/v5"

	"github.com/stencil-hq/ovnfs/internal/kept"
)

// NFSv3 procedures (RFC 1813 section 3).
const (
	v3Null = iota
	v3GetAttr
	v3SetAttr
	v3Lookup
	v3Access
	v3Readlink
	v3Read
	v3Write
	v3Create
	v3Mkdir
	v3Symlink
	v3Mknod
	v3Remove
	v3Rmdir
	v3Rename
	v3Link
	v3ReadDir
	v3ReadDirPlus
	v3FSStat
	v3FSInfo
	v3PathConf
	v3Commit
	// v3Count is the number of procedures.
	v3Count
)

// stNotSync is NFS3ERR_NOT_SYNC: a SETATTR guard did not match.
const stNotSync status = 10002

// NFSv3 limits.
const (
	// maxFH3 is NFS3_FHSIZE, the longest handle a client may send.
	maxFH3 = 64
	// maxPath3 bounds names and symlink targets as decoded; longer names
	// than maxName decode and fail NAMETOOLONG.
	maxPath3 = 4096
)

// v3State is what the server remembers between NFSv3 calls, a stateless
// protocol: cached writers and permission fallbacks (kept), the
// verifiers of EXCLUSIVE creates, the calls under way per file
// (fileOrder) and the directories whose entries change (dirLocks).
type v3State struct {
	// kept retains writers across bursts of WRITE requests instead of
	// opening and closing a host descriptor for each small write. Files
	// opened before CREATE or SETATTR removes write permission also stay
	// here: clients may send their buffered writes afterwards. NFSv4 keeps
	// those permission fallbacks per open instead (keepWritable).
	kept     kept.Cache[keptKey]
	writes   writeQueue
	readers  readCache
	verfs    createVerfs
	maxSizes maxReportedSizes
	order    fileOrder
	dirs     dirLocks
	sidecars sidecars
}

// v3proc is an NFSv3 procedure. run appends the result after the NFS3_OK
// status that nfs3 writes; on another status nfs3 replaces it with the
// failure arm: the status and fail bytes of zeros, the discriminants of the
// absent post_op_attr and wcc_data fields the procedure's resfail carries.
// When the arguments do not decode, run returns without acting and its
// status is ignored.
type v3proc struct {
	name string // as RFC 1813 spells it, which names its Server.Stats timer
	run  func(s *Server, d *decoder, res *encoder) status
	fail int
}

// v3procs are the procedures by number; NULL is answered by nfs3 itself.
var v3procs = [v3Count]v3proc{
	v3GetAttr:     {"GETATTR", (*Server).getattr3, 0},
	v3SetAttr:     {"SETATTR", (*Server).setattr3, 8},
	v3Lookup:      {"LOOKUP", (*Server).lookup3, 4},
	v3Access:      {"ACCESS", (*Server).access3, 4},
	v3Readlink:    {"READLINK", (*Server).readlink3, 4},
	v3Read:        {"READ", (*Server).read3, 4},
	v3Write:       {"WRITE", (*Server).write3, 8},
	v3Create:      {"CREATE", (*Server).create3, 8},
	v3Mkdir:       {"MKDIR", (*Server).mkdir3, 8},
	v3Symlink:     {"SYMLINK", (*Server).symlink3, 8},
	v3Mknod:       {"MKNOD", (*Server).mknod3, 8},
	v3Remove:      {"REMOVE", (*Server).remove3, 8},
	v3Rmdir:       {"RMDIR", (*Server).rmdir3, 8},
	v3Rename:      {"RENAME", (*Server).rename3, 16},
	v3Link:        {"LINK", (*Server).link3, 12},
	v3ReadDir:     {"READDIR", (*Server).readdir3, 4},
	v3ReadDirPlus: {"READDIRPLUS", (*Server).readdirplus3, 4},
	v3FSStat:      {"FSSTAT", (*Server).fsstat3, 4},
	v3FSInfo:      {"FSINFO", (*Server).fsinfo3, 4},
	v3PathConf:    {"PATHCONF", (*Server).pathconf3, 4},
	v3Commit:      {"COMMIT", (*Server).commit3, 8},
}

// nfs3 answers NFSv3 calls.
func (s *Server) nfs3(c *call, res *encoder) acceptStat {
	if c.proc == v3Null {
		return rpcSuccess
	}
	if c.proc >= uint32(len(v3procs)) {
		return rpcProcUnavail
	}
	p := v3procs[c.proc]
	at := res.len()
	res.u32(uint32(stOK))
	timer := s.timers.v3[c.proc]
	began := timer.Start()
	var st status
	sidecar := false
	if s.DropAppleDouble {
		st, sidecar = s.sidecarCall(c.proc, c.args, res)
	}
	if !sidecar {
		if s.NoSync && c.proc != v3Write {
			// Single-object calls exclude just their file; a directory handle
			// alone cannot identify the children a namespace call will touch.
			var id uint64
			switch c.proc {
			case v3GetAttr, v3SetAttr, v3Read, v3Readlink, v3Access, v3Commit, v3PathConf, v3FSStat, v3FSInfo:
				d := c.args
				id, _, _ = d.fh(maxFH3)
			}
			_ = s.v3.writes.exclude(context.Background(), id)
			defer s.v3.writes.allow(id)
		}
		st = p.run(s, &c.args, res)
	}
	timer.Since(began)
	if c.args.err != nil {
		return rpcGarbageArgs
	}
	if st != stOK {
		res.sizeAccepted = false
		res.truncate(at)
		res.u32(uint32(st))
		for range p.fail / 4 {
			res.u32(0)
		}
	}
	return rpcSuccess
}

// path3 resolves a decoded file handle: BADHANDLE for one no server can have
// issued, STALE for one resolve refuses.
func (s *Server) path3(id, gen uint64, ok bool) (string, status) {
	if !ok {
		return "", stBadHandle
	}
	p, _, live := s.resolve(id, gen)
	if !live {
		return "", stStale
	}
	return p, stOK
}

// resolve finds the current path of the handle id with generation gen, and
// the generation its current handles carry: false when its file is gone,
// its generation is no longer current, or it is a legacy handle of anything
// but the export root. The root's legacy handle stays valid so a mount kept
// across the handle-format upgrade keeps working: everything below it is
// looked up again.
func (s *Server) resolve(id, gen uint64) (string, uint64, bool) {
	if id == 0 {
		return "", 0, false
	}
	if gen == legacyGen {
		if id != s.FS.FileID("") {
			return "", 0, false
		}
		gen = s.FS.Generation(id)
	}
	p, live := s.FS.Path(id, gen)
	return p, gen, live
}

// A handle names a file, and resolving it (path3, or PUTFH in NFSv4) gives
// where the file was then; a rename can move it, or put another file at
// that path, before the request uses the path. A client renaming a file
// over the name of one it keeps open does both: the open file moves to a
// silly name, the new one takes its place. describe and openHandle check
// that the path still holds the handle's file and follow it when not, so a
// request never reads, writes or describes another file under the handle.

// describe returns where the file handle (id, gen) names is and its
// description, starting at p, the path the handle last resolved to: ESTALE
// once the file is gone.
func (s *Server) describe(id, gen uint64, p string) (string, os.FileInfo, error) {
	info, err := s.lstatID(id, p)
	if err == nil || !moved(err) {
		return p, info, err
	}
	if p, err = s.relocate(id, gen); err != nil {
		return "", nil, err
	}
	info, err = s.lstatID(id, p)
	return p, info, err
}

// openHandle opens the file handle (id, gen) names, starting at p as
// [Server.describe] does, with flag, which neither creates nor truncates;
// it returns where the file is and the open file's description, as
// [Server.openID] does.
func (s *Server) openHandle(id, gen uint64, p string, flag int) (billy.File, string, os.FileInfo, error) {
	f, info, err := s.openID(id, p, flag)
	if err == nil || !moved(err) {
		return f, p, info, err
	}
	if p, err = s.relocate(id, gen); err != nil {
		return nil, "", nil, err
	}
	f, info, err = s.openID(id, p, flag)
	return f, p, info, err
}

// openID opens file id, at p, with flag and describes the open file:
// ESTALE when p holds another file. The description comes from the
// descriptor, like any bytes read through it; a path stat after the open
// may describe a replacement instead.
func (s *Server) openID(id uint64, p string, flag int) (billy.File, os.FileInfo, error) {
	f, err := s.FS.OpenFile(p, flag, 0)
	if err != nil {
		return nil, nil, err
	}
	var info os.FileInfo
	if fd, ok := f.(interface{ Stat() (os.FileInfo, error) }); ok {
		info, err = fd.Stat()
	} else {
		info, err = s.FS.Stat(p)
	}
	if err == nil && s.FS.IDOf(p, info) != id {
		err = syscall.ESTALE
	}
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, info, nil
}

// relocate resolves the handle (id, gen) afresh (Backend.Relocate). The
// export root, the only file a legacy handle names, never moves.
func (s *Server) relocate(id, gen uint64) (string, error) {
	p, live := s.FS.Relocate(id, gen)
	if !live {
		return "", syscall.ESTALE
	}
	return p, nil
}

// moved reports an error, from a call on the path a handle resolved to, that
// a rename since explains: the file left the path or another took it.
func moved(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.ESTALE)
}

// checkName3 vets a file name a client sends. Empty names and names with a
// slash or NUL get ACCES, as Linux's nfsd answers (lookup_one_len refuses
// them with EACCES); "." and ".." are the callers' to treat.
func checkName3(name string) status {
	switch {
	case len(name) > maxName:
		return stNameTooLong
	case name == "", strings.ContainsAny(name, "/\x00"):
		return stAccess
	}
	return stOK
}

// isDot reports the names that refer to a directory itself or its parent,
// which name no entry a procedure could create or remove.
func isDot(name string) bool { return name == "." || name == ".." }

// parentOf is the directory containing p; the root is its own parent.
func parentOf(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[:i]
	}
	return ""
}
