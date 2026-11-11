package nfs

// MOUNT v3 procedures (RFC 1813 appendix I).
const (
	mntNull = iota
	mntMnt
	mntDump
	mntUmnt
	mntUmntAll
	mntExport
)

// mntPathLen is MNTPATHLEN, the longest dirpath.
const mntPathLen = 1024

// mount answers MOUNT v3 calls. There is one export: MNT of any path
// returns the root's handle with AUTH_NULL as the only flavor, since
// credentials are not checked. No mounts are recorded,
// so DUMP lists none and UMNT and UMNTALL do nothing; EXPORT lists "/" open
// to every client.
func (s *Server) mount(c *call, res *encoder) acceptStat {
	d := &c.args
	switch c.proc {
	case mntNull, mntUmntAll:
	case mntUmnt:
		d.opaque(mntPathLen)
	case mntMnt:
		d.opaque(mntPathLen)
		if d.err != nil {
			break
		}
		if identityError(s.FS) != nil {
			res.u32(uint32(stIO))
			break
		}
		id := s.FS.FileID("")
		if id == 0 || identityError(s.FS) != nil {
			res.u32(uint32(stIO))
			break
		}
		res.u32(0) // MNT3_OK
		res.fh(id)
		res.u32(1) // one auth flavor
		res.u32(authNone)
	case mntDump:
		res.bool(false) // empty mountlist
	case mntExport:
		res.bool(true)
		res.str("/")
		res.bool(false) // no groups: every client
		res.bool(false) // end of exports
	default:
		return rpcProcUnavail
	}
	if d.err != nil {
		return rpcGarbageArgs
	}
	return rpcSuccess
}
