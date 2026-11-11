//go:build windows

package winfsp

import (
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"sync"
	"syscall"

	"github.com/go-git/go-billy/v5"
	"github.com/stencil-hq/ovnfs/export"
	"github.com/stencil-hq/ovnfs/ntinfo"
	gowinfsp "github.com/winfsp/go-winfsp"
	"golang.org/x/sys/windows"
)

// writeAccess is the access that opens a file for writing.
const writeAccess = windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA

// fileSystem answers WinFsp requests from the export. Handles keep their
// node's stable id, which is also the reported file index, so they follow
// renames made through other handles.
type fileSystem struct {
	nodes *export.Nodes
	// sd gives the daemon's user and SYSTEM full access to every file: the
	// driver checks it before every open, and Windows has no execute bits to
	// derive a narrower one from.
	sd *windows.SECURITY_DESCRIPTOR
	// roots name the volume's root to Windows tools (O:/, //ovnfs/O/); see
	// [ntinfo.MountRelative]. Mount sets them.
	roots []string

	mu      sync.Mutex
	next    uintptr
	handles map[uintptr]*handle
}

// handle is one open file object. Regular files keep an open export file,
// read-only unless opened with write access, as the SMB server does.
type handle struct {
	release func()
	mu      sync.RWMutex // guards file and ino against replacement during I/O
	ino     uint64
	file    billy.File
	write   bool
	append  bool // FILE_APPEND_DATA without FILE_WRITE_DATA: every write goes to EOF
	list    gowinfsp.DirBuffer
}

// newFileSystem serves nodes with a security descriptor owned by the daemon's user.
func newFileSystem(nodes *export.Nodes) (*fileSystem, error) {
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, err
	}
	group, err := token.GetTokenPrimaryGroup()
	if err != nil {
		return nil, err
	}
	sid := user.User.Sid.String()
	sd, err := windows.SecurityDescriptorFromString("O:" + sid + "G:" + group.PrimaryGroup.String() + "D:P(A;;FA;;;" + sid + ")(A;;FA;;;SY)")
	if err != nil {
		return nil, err
	}
	return &fileSystem{nodes: nodes, sd: sd, handles: make(map[uintptr]*handle)}, nil
}

// exportPath converts a WinFsp name such as `\alpha\src` to the export path `alpha/src`.
func exportPath(name string) string {
	return strings.ReplaceAll(strings.TrimPrefix(name, `\`), `\`, "/")
}

// parent returns the export path of p's directory.
func parent(p string) string {
	if dir := path.Dir(p); dir != "." {
		return dir
	}
	return ""
}

// status converts an export error to the NTSTATUS the SMB server would report.
func status(err error) error {
	if err == nil {
		return nil
	}
	return windows.NTStatus(ntinfo.Status(err))
}

// add registers an open handle and returns its file context.
func (x *fileSystem) add(h *handle) uintptr {
	h.release = x.nodes.HoldFileID(h.ino)
	if x.nodes.IdentityError() != nil {
		h.release()
		if h.file != nil {
			h.file.Close()
		}
		return 0
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	x.next++
	x.handles[x.next] = h
	return x.next
}

// get returns the handle behind file context fc.
func (x *fileSystem) get(fc uintptr) (*handle, error) {
	if err := x.nodes.IdentityError(); err != nil {
		return nil, status(err)
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if h := x.handles[fc]; h != nil {
		return h, nil
	}
	return nil, windows.STATUS_INVALID_HANDLE
}

// fill describes node ino in the driver's terms.
func fill(out *gowinfsp.FSP_FSCTL_FILE_INFO, ino uint64, info os.FileInfo) {
	*out = gowinfsp.FSP_FSCTL_FILE_INFO{
		FileAttributes: ntinfo.Attributes(info),
		AllocationSize: ntinfo.AllocationSize(info),
		CreationTime:   ntinfo.Filetime(ntinfo.BirthTime(info)),
		LastAccessTime: ntinfo.Filetime(ntinfo.AccessTime(info)),
		LastWriteTime:  ntinfo.Filetime(info.ModTime()),
		ChangeTime:     ntinfo.Filetime(ntinfo.ChangeTime(info)),
		IndexNumber:    ino,
	}
	if !info.IsDir() {
		out.FileSize = uint64(info.Size())
	}
	if info.Mode()&os.ModeSymlink != 0 {
		out.ReparseTag = ntinfo.ReparseTagSymlink
	}
}

// stat describes h's node by its current name, falling back to its open file
// once the name is gone (deleted or replaced while open). The caller holds h.mu.
func (x *fileSystem) stat(h *handle) (os.FileInfo, error) {
	p, ok := x.nodes.Path(h.ino)
	if err := x.nodes.IdentityError(); err != nil {
		return nil, status(err)
	}
	var err error = windows.STATUS_FILE_DELETED
	if ok {
		var info os.FileInfo
		if info, err = x.nodes.Lstat(p); err == nil {
			return info, nil
		}
		err = status(err)
	}
	if fault := x.nodes.IdentityError(); fault != nil {
		return nil, status(fault)
	}
	if s, ok := h.file.(interface{ Stat() (os.FileInfo, error) }); ok {
		info, serr := s.Stat()
		return info, status(serr)
	}
	return nil, err
}

// describe fills out with h's current attributes; the caller holds h.mu.
func (x *fileSystem) describe(h *handle, out *gowinfsp.FSP_FSCTL_FILE_INFO) error {
	info, err := x.stat(h)
	if err != nil {
		return err
	}
	fill(out, h.ino, info)
	return nil
}

// GetVolumeInfo reports the upper volume, where all overlay writes land.
func (x *fileSystem) GetVolumeInfo(_ *gowinfsp.FileSystemRef, out *gowinfsp.FSP_FSCTL_VOLUME_INFO) error {
	s, err := x.nodes.Statfs()
	if err != nil {
		return status(err)
	}
	block := uint64(s.BlockSize)
	if block == 0 {
		block = 4096
	}
	out.TotalSize, out.FreeSize = s.Blocks*block, s.Avail*block
	label := windows.StringToUTF16("ovnfs")
	out.VolumeLabelLength = uint16(2 * copy(out.VolumeLabel[:], label[:len(label)-1]))
	return nil
}

// GetSecurityByName reports a name's attributes and security for the
// driver's access check. A name whose directory is missing reports
// STATUS_OBJECT_PATH_NOT_FOUND, as NTFS does, unless a symlink stands in for
// a directory: then STATUS_REPARSE makes the driver resolve the link
// (go-winfsp drops the reparse index, so resolution rescans from the start).
func (x *fileSystem) GetSecurityByName(ref *gowinfsp.FileSystemRef, name string, _ gowinfsp.GetSecurityByNameFlags) (uint32, *windows.SECURITY_DESCRIPTOR, error) {
	p := exportPath(name)
	info, err := x.nodes.Lstat(p)
	if err == nil {
		return ntinfo.Attributes(info), x.sd, nil
	}
	if dir, derr := x.nodes.Lstat(parent(p)); derr == nil && dir.IsDir() {
		return 0, nil, status(err)
	}
	if found, _, _ := ref.FindReparsePoint(name); found {
		return 0, nil, windows.STATUS_REPARSE
	}
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return 0, nil, windows.STATUS_OBJECT_PATH_NOT_FOUND
	}
	return 0, nil, status(err)
}

// GetSecurity reports the descriptor every file shares.
func (x *fileSystem) GetSecurity(*gowinfsp.FileSystemRef, uintptr) (*windows.SECURITY_DESCRIPTOR, error) {
	return x.sd, nil
}

// Create makes a new file or directory: the driver calls it only for names
// that do not exist. FILE_ATTRIBUTE_READONLY clears the write bits.
func (x *fileSystem) Create(_ *gowinfsp.FileSystemRef, name string, options, access, attributes uint32, _ *windows.SECURITY_DESCRIPTOR, _ uint64, out *gowinfsp.FSP_FSCTL_FILE_INFO) (uintptr, error) {
	p := exportPath(name)
	h := &handle{append: access&writeAccess == windows.FILE_APPEND_DATA}
	mode := os.FileMode(0o666)
	if attributes&windows.FILE_ATTRIBUTE_READONLY != 0 {
		mode &^= 0o222
	}
	if options&windows.FILE_DIRECTORY_FILE != 0 {
		// MkdirAll succeeds on an existing directory, which Create must refuse.
		if _, err := x.nodes.Lstat(p); err == nil {
			return 0, windows.STATUS_OBJECT_NAME_COLLISION
		} else if !errors.Is(err, fs.ErrNotExist) {
			return 0, status(err)
		}
		if err := x.nodes.MkdirAll(p, mode|0o111); err != nil {
			return 0, status(err)
		}
	} else {
		f, err := x.nodes.OpenFile(p, os.O_RDWR|os.O_CREATE|os.O_EXCL, mode)
		if err != nil {
			return 0, status(err)
		}
		h.file, h.write = f, true
	}
	h.ino = x.nodes.ID(p)
	if h.ino == 0 {
		if h.file != nil {
			h.file.Close()
		}
		return 0, status(syscall.EIO)
	}
	if err := x.describe(h, out); err != nil {
		if h.file != nil {
			h.file.Close()
		}
		return 0, err
	}
	fc := x.add(h)
	if fc == 0 {
		return 0, status(syscall.EIO)
	}
	return fc, nil
}

// Open opens an existing name. Regular files open read-only unless access
// asks to write, which copies them up; other handles of the node that only
// read then reopen it, as copy-up replaces the object behind them.
func (x *fileSystem) Open(_ *gowinfsp.FileSystemRef, name string, options, access uint32, out *gowinfsp.FSP_FSCTL_FILE_INFO) (uintptr, error) {
	p := exportPath(name)
	info, err := x.nodes.Lstat(p)
	if err != nil {
		return 0, status(err)
	}
	// A directory symlink opened as itself counts as a directory, as on NTFS.
	switch {
	case options&windows.FILE_DIRECTORY_FILE != 0 && !ntinfo.IsDir(info):
		return 0, windows.STATUS_NOT_A_DIRECTORY
	case options&windows.FILE_NON_DIRECTORY_FILE != 0 && ntinfo.IsDir(info):
		return 0, windows.STATUS_FILE_IS_A_DIRECTORY
	}
	h := &handle{ino: x.nodes.ID(p), append: access&writeAccess == windows.FILE_APPEND_DATA}
	if h.ino == 0 {
		return 0, status(syscall.EIO)
	}
	if info.Mode().IsRegular() {
		h.write = access&writeAccess != 0
		flag := os.O_RDONLY
		if h.write {
			flag = os.O_RDWR
		}
		if h.file, err = x.nodes.OpenFile(p, flag, 0); err != nil {
			return 0, status(err)
		}
		if h.write {
			if err := x.reopenReaders(h, p); err != nil {
				h.file.Close()
				return 0, err
			}
		}
	}
	fill(out, h.ino, info)
	fc := x.add(h)
	if fc == 0 {
		return 0, status(syscall.EIO)
	}
	return fc, nil
}

// reopenReaders points the read-only handles of w's node at the object w
// opened; w is not registered yet.
func (x *fileSystem) reopenReaders(w *handle, p string) error {
	x.mu.Lock()
	open := make([]*handle, 0, len(x.handles))
	for _, h := range x.handles {
		open = append(open, h)
	}
	x.mu.Unlock()
	for _, h := range open {
		if err := h.reopen(w.ino, func() (billy.File, error) { return x.nodes.OpenFile(p, os.O_RDONLY, 0) }); err != nil {
			return status(err)
		}
	}
	return nil
}

// reopen replaces h's file with open's when h reads node ino and is not closed.
func (h *handle) reopen(ino uint64, open func() (billy.File, error)) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ino != ino || h.file == nil || h.write {
		return nil
	}
	f, err := open()
	if err != nil {
		return err
	}
	h.file, f = f, h.file
	return f.Close()
}

// Overwrite truncates a file opened with FILE_OVERWRITE(_IF) or FILE_SUPERSEDE;
// FILE_ATTRIBUTE_READONLY clears its write bits.
func (x *fileSystem) Overwrite(_ *gowinfsp.FileSystemRef, fc uintptr, attributes uint32, _ bool, _ uint64, out *gowinfsp.FSP_FSCTL_FILE_INFO) error {
	h, err := x.get(fc)
	if err != nil {
		return err
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	if !h.write {
		return windows.STATUS_ACCESS_DENIED
	}
	if err := h.file.Truncate(0); err != nil {
		return status(err)
	}
	if attributes&windows.FILE_ATTRIBUTE_READONLY != 0 {
		if err := x.setReadOnly(h, true); err != nil {
			return err
		}
	}
	return x.describe(h, out)
}

// setReadOnly maps FILE_ATTRIBUTE_READONLY onto the owner write bit, as the SMB
// server does for Windows clients. The caller holds h.mu.
func (x *fileSystem) setReadOnly(h *handle, readOnly bool) error {
	p, ok := x.nodes.Path(h.ino)
	if !ok {
		return windows.STATUS_FILE_DELETED
	}
	info, err := x.nodes.Lstat(p)
	if err != nil {
		return status(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil
	}
	mode := info.Mode()
	if readOnly {
		mode &^= 0o200
	} else {
		mode |= 0o200
	}
	if mode == info.Mode() {
		return nil
	}
	return status(x.nodes.Chmod(p, mode))
}

// Cleanup removes a name whose last handle closed with delete pending, or
// that a POSIX delete unlinked; open handles keep reading their file.
func (x *fileSystem) Cleanup(_ *gowinfsp.FileSystemRef, _ uintptr, name string, flags uint32) {
	if flags&gowinfsp.FspCleanupDelete != 0 {
		_ = x.nodes.Remove(exportPath(name))
	}
}

// Close releases a handle's file and listing.
func (x *fileSystem) Close(_ *gowinfsp.FileSystemRef, fc uintptr) {
	x.mu.Lock()
	h := x.handles[fc]
	delete(x.handles, fc)
	x.mu.Unlock()
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.file != nil {
		h.file.Close()
		h.file = nil
	}
	h.list.Delete()
	if h.release != nil {
		h.release()
		h.release = nil
	}
}

// Read reads at the driver's offset.
func (x *fileSystem) Read(_ *gowinfsp.FileSystemRef, fc uintptr, buf []byte, off uint64) (int, error) {
	h, err := x.get(fc)
	if err != nil {
		return 0, err
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.file == nil {
		return 0, windows.STATUS_INVALID_DEVICE_REQUEST
	}
	n, err := h.file.ReadAt(buf, int64(off))
	if errors.Is(err, io.EOF) {
		return n, io.EOF
	}
	return n, status(err)
}

// Write writes at the driver's offset, or at end of file for append-only
// handles and FILE_WRITE_TO_END_OF_FILE. Constrained (paging) writes never
// extend the file.
func (x *fileSystem) Write(_ *gowinfsp.FileSystemRef, fc uintptr, buf []byte, off uint64, toEnd, constrained bool, out *gowinfsp.FSP_FSCTL_FILE_INFO) (int, error) {
	h, err := x.get(fc)
	if err != nil {
		return 0, err
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	at, ok := h.file.(io.WriterAt)
	if !h.write || !ok {
		return 0, windows.STATUS_ACCESS_DENIED
	}
	size, err := h.file.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, status(err)
	}
	pos := int64(off)
	if toEnd || h.append {
		pos = size
	}
	if constrained {
		if pos >= size {
			return 0, x.describe(h, out)
		}
		buf = buf[:min(int64(len(buf)), size-pos)]
	}
	n, err := at.WriteAt(buf, pos)
	if err != nil {
		return n, status(err)
	}
	return n, x.describe(h, out)
}

// Flush makes a file's data durable; a volume flush (no handle) has nothing to do.
func (x *fileSystem) Flush(_ *gowinfsp.FileSystemRef, fc uintptr, out *gowinfsp.FSP_FSCTL_FILE_INFO) error {
	if fc == 0 {
		return nil
	}
	h, err := x.get(fc)
	if err != nil {
		return err
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	if s, ok := h.file.(interface{ Sync() error }); ok {
		if err := s.Sync(); err != nil {
			return status(err)
		}
	}
	return x.describe(h, out)
}

// GetFileInfo reports a handle's current attributes.
func (x *fileSystem) GetFileInfo(_ *gowinfsp.FileSystemRef, fc uintptr, out *gowinfsp.FSP_FSCTL_FILE_INFO) error {
	h, err := x.get(fc)
	if err != nil {
		return err
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	return x.describe(h, out)
}

// SetBasicInfo applies FILE_ATTRIBUTE_READONLY and access and write times;
// creation and change times cannot be set. A symlink's own times are left
// alone, since the export only changes times through symlinks.
func (x *fileSystem) SetBasicInfo(_ *gowinfsp.FileSystemRef, fc uintptr, flags gowinfsp.SetBasicInfoFlags, attributes uint32, _, atime, mtime, _ uint64, out *gowinfsp.FSP_FSCTL_FILE_INFO) error {
	h, err := x.get(fc)
	if err != nil {
		return err
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	if flags&gowinfsp.SetBasicInfoAttributes != 0 && attributes != 0 {
		if err := x.setReadOnly(h, attributes&windows.FILE_ATTRIBUTE_READONLY != 0); err != nil {
			return err
		}
	}
	info, err := x.stat(h)
	if err != nil {
		return err
	}
	// -1 and -2 suspend and resume automatic time updates; there are none to suspend.
	settable := func(set gowinfsp.SetBasicInfoFlags, v uint64) bool { return flags&set != 0 && v < ^uint64(1) }
	at, mt := ntinfo.AccessTime(info), info.ModTime()
	changed := false
	if settable(gowinfsp.SetBasicInfoLastAccessTime, atime) {
		at, changed = ntinfo.FromFiletime(atime), true
	}
	if settable(gowinfsp.SetBasicInfoLastWriteTime, mtime) {
		mt, changed = ntinfo.FromFiletime(mtime), true
	}
	if changed && info.Mode()&os.ModeSymlink == 0 {
		p, ok := x.nodes.Path(h.ino)
		if !ok {
			return windows.STATUS_FILE_DELETED
		}
		if err := x.nodes.Chtimes(p, at, mt); err != nil {
			return status(err)
		}
	}
	return x.describe(h, out)
}

// SetFileSize truncates or extends a file. Setting the allocation size only
// truncates when it falls below the end of file.
func (x *fileSystem) SetFileSize(_ *gowinfsp.FileSystemRef, fc uintptr, size uint64, allocation bool, out *gowinfsp.FSP_FSCTL_FILE_INFO) error {
	h, err := x.get(fc)
	if err != nil {
		return err
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	if allocation {
		info, err := x.stat(h)
		if err != nil {
			return err
		}
		if int64(size) >= info.Size() {
			fill(out, h.ino, info)
			return nil
		}
	}
	if !h.write {
		return windows.STATUS_ACCESS_DENIED
	}
	if err := h.file.Truncate(int64(size)); err != nil {
		return status(err)
	}
	return x.describe(h, out)
}

// CanDelete refuses the export root and checkouts, which only `ovnfs rm`
// removes, and directories that are not empty.
func (x *fileSystem) CanDelete(_ *gowinfsp.FileSystemRef, _ uintptr, name string) error {
	p := exportPath(name)
	if !strings.Contains(p, "/") {
		return windows.STATUS_ACCESS_DENIED
	}
	info, err := x.nodes.Lstat(p)
	if err != nil {
		return status(err)
	}
	if info.IsDir() {
		entries, err := x.nodes.ReadDir(p)
		if err != nil {
			return status(err)
		}
		if len(entries) != 0 {
			return windows.STATUS_DIRECTORY_NOT_EMPTY
		}
	}
	return nil
}

// Rename moves a name within one checkout. Replacing an existing name needs
// replace and never replaces a directory; across checkouts it reports
// STATUS_NOT_SAME_DEVICE, so moves fall back to copying.
func (x *fileSystem) Rename(_ *gowinfsp.FileSystemRef, _ uintptr, source, target string, replace bool) error {
	from, to := exportPath(source), exportPath(target)
	if from == to {
		return nil
	}
	if info, err := x.nodes.Lstat(to); err == nil {
		if !replace {
			return windows.STATUS_OBJECT_NAME_COLLISION
		}
		if info.IsDir() {
			return windows.STATUS_ACCESS_DENIED
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return status(err)
	}
	return status(x.nodes.Rename(from, to))
}

// GetOrNewDirBuffer returns the handle's listing buffer, which the driver
// refills when a query restarts and pages through by name otherwise.
func (x *fileSystem) GetOrNewDirBuffer(_ *gowinfsp.FileSystemRef, fc uintptr) (*gowinfsp.DirBuffer, error) {
	h, err := x.get(fc)
	if err != nil {
		return nil, err
	}
	return &h.list, nil
}

// ReadDirectory lists a directory, with "." and ".." below the root. The
// driver matches the pattern itself.
func (x *fileSystem) ReadDirectory(_ *gowinfsp.FileSystemRef, fc uintptr, _ string, add func(string, *gowinfsp.FSP_FSCTL_FILE_INFO) (bool, error)) error {
	h, err := x.get(fc)
	if err != nil {
		return err
	}
	h.mu.RLock()
	p, ok := x.nodes.Path(h.ino)
	h.mu.RUnlock()
	if !ok {
		return windows.STATUS_FILE_DELETED
	}
	infos, err := x.nodes.ReadDir(p)
	if err != nil {
		return status(err)
	}
	var out gowinfsp.FSP_FSCTL_FILE_INFO
	if p != "" {
		for _, d := range []struct{ name, path string }{{".", p}, {"..", parent(p)}} {
			info, err := x.nodes.Lstat(d.path)
			if err != nil {
				return status(err)
			}
			ino := x.nodes.ID(d.path)
			if ino == 0 {
				return status(syscall.EIO)
			}
			fill(&out, ino, info)
			if more, err := add(d.name, &out); err != nil || !more {
				return err
			}
		}
	}
	for _, info := range infos {
		ino := x.nodes.ID(path.Join(p, info.Name()))
		if ino == 0 {
			return status(syscall.EIO)
		}
		fill(&out, ino, info)
		if more, err := add(info.Name(), &out); err != nil || !more {
			return err
		}
	}
	return nil
}

// reparse encodes the symlink at export path p.
func (x *fileSystem) reparse(p string, buf []byte) (int, error) {
	info, err := x.nodes.Lstat(p)
	if err != nil {
		return 0, status(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return 0, windows.STATUS_NOT_A_REPARSE_POINT
	}
	target, err := x.nodes.Readlink(p)
	if err != nil {
		return 0, status(err)
	}
	b, err := ntinfo.SymlinkReparse(target)
	if err != nil {
		return 0, windows.STATUS_INVALID_PARAMETER
	}
	switch {
	case len(buf) == 0: // FindReparsePoint only asks whether p is one
		return 0, nil
	case len(buf) < len(b):
		return 0, windows.STATUS_BUFFER_TOO_SMALL
	}
	return copy(buf, b), nil
}

// GetReparsePointByName lets the driver resolve symlinks within paths.
func (x *fileSystem) GetReparsePointByName(_ *gowinfsp.FileSystemRef, name string, _ bool, buf []byte) (int, error) {
	return x.reparse(exportPath(name), buf)
}

// GetReparsePoint answers FSCTL_GET_REPARSE_POINT on an open symlink.
func (x *fileSystem) GetReparsePoint(_ *gowinfsp.FileSystemRef, _ uintptr, name string, buf []byte) (int, error) {
	return x.reparse(exportPath(name), buf)
}

// SetReparsePoint turns the empty file or directory that CreateSymbolicLink
// or a junction tool just created into a symlink, as the SMB server does.
func (x *fileSystem) SetReparsePoint(_ *gowinfsp.FileSystemRef, fc uintptr, name string, buf []byte) error {
	target, err := ntinfo.ParseSymlinkReparse(buf)
	switch {
	case errors.Is(err, ntinfo.ErrNotSymlink):
		return windows.STATUS_IO_REPARSE_TAG_NOT_HANDLED
	case err != nil:
		return windows.STATUS_INVALID_PARAMETER
	}
	h, err := x.get(fc)
	if err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	p := exportPath(name)
	target = ntinfo.MountRelative(target, p, x.roots)
	info, err := x.nodes.Lstat(p)
	if err != nil {
		return status(err)
	}
	switch {
	case info.IsDir():
		entries, err := x.nodes.ReadDir(p)
		if err != nil {
			return status(err)
		}
		if len(entries) != 0 {
			return windows.STATUS_DIRECTORY_NOT_EMPTY
		}
	case binary.LittleEndian.Uint32(buf) == ntinfo.ReparseTagMountPoint:
		return windows.STATUS_NOT_A_DIRECTORY
	case h.file == nil:
		return windows.STATUS_INVALID_DEVICE_REQUEST
	case !info.Mode().IsRegular() || info.Size() != 0:
		return windows.STATUS_INVALID_PARAMETER
	}
	// acquire replaces a published handle's identity hold without leaving a
	// released callback or a closed descriptor for Close to consume again.
	acquire := func() error {
		if h.release != nil {
			h.release()
			h.release = nil
		}
		h.ino = x.nodes.ID(p)
		if h.ino == 0 {
			return status(syscall.EIO)
		}
		release := x.nodes.HoldFileID(h.ino)
		if err := x.nodes.IdentityError(); err != nil {
			release()
			return status(err)
		}
		h.release = release
		return nil
	}
	restore := func() error {
		if info.IsDir() {
			if err := x.nodes.MkdirAll(p, info.Mode().Perm()); err != nil {
				return status(err)
			}
		} else {
			f, err := x.nodes.OpenFile(p, os.O_CREATE|os.O_RDWR, info.Mode().Perm())
			if err != nil {
				return status(err)
			}
			h.file, h.write = f, true
		}
		if err := acquire(); err != nil {
			if h.file != nil {
				h.file.Close()
				h.file = nil
				h.write = false
			}
			return err
		}
		return nil
	}
	if h.file != nil {
		if err := h.file.Close(); err != nil {
			return status(err)
		}
		h.file, h.write = nil, false
	}
	if err := x.nodes.Remove(p); err != nil {
		if rerr := restore(); rerr != nil {
			return rerr
		}
		return status(err)
	}
	if err := x.nodes.Symlink(target, p); err != nil {
		if rerr := restore(); rerr != nil {
			return rerr
		}
		return status(err)
	}
	return acquire()
}
