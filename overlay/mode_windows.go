//go:build windows

package overlay

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"syscall"

	"github.com/stencil-hq/ovnfs/internal/ident"
	"golang.org/x/sys/windows"
)

// Windows files hold no Unix mode bits, only a read-only attribute. An upper
// or layer file whose mode is not the default its attribute implies (0644 or
// 0444, directories 0755 or 0555) keeps it in this NTFS/ReFS alternate data
// stream as octal Unix bits. The stream belongs to the file record, so it
// follows renames, is shared by hard links, and survives freezing an upper
// into a layer and reopening it after a restart; it never appears in
// directory listings. Its absence costs readers one failed open.
const modeStream = ":ovnfs.mode"

// modeInfo is host metadata whose permission bits come from the mode stream.
type modeInfo struct {
	os.FileInfo
	mode os.FileMode
}

// Mode returns the stored Unix mode with the host's type bits.
func (i modeInfo) Mode() os.FileMode { return i.mode }

// Identity forwards the identity of the metadata beneath the stored mode.
func (i modeInfo) Identity() ident.ID { id, _ := ident.Of(i.FileInfo); return id }

// defaultPerm is the mode a file or directory without a mode stream reports:
// owner-writable unless the read-only attribute is set.
func defaultPerm(dir, readOnly bool) os.FileMode {
	switch {
	case dir && readOnly:
		return 0o555
	case dir:
		return 0o755
	case readOnly:
		return 0o444
	}
	return 0o644
}

// withMode replaces the permission bits of name's host metadata info with its
// stored mode, or its default. Symlinks and other types keep the host's. It
// fails when the stream cannot be read or holds no octal mode.
func withMode(name string, info os.FileInfo) (os.FileInfo, error) {
	m := info.Mode()
	if !m.IsDir() && !m.IsRegular() {
		return info, nil
	}
	perm, ok, err := readMode(name)
	if err != nil {
		return nil, err
	}
	if !ok {
		perm = defaultPerm(m.IsDir(), m&0o200 == 0)
	}
	return modeInfo{FileInfo: info, mode: m&os.ModeType | perm}, nil
}

// lstatIndexed is [os.Lstat] for an upper entry whose mode and identity the
// index already holds (indexed): only data and times change without the
// overlay knowing.
func lstatIndexed(name string, indexed os.FileInfo) (os.FileInfo, error) {
	info, err := os.Lstat(name)
	if err != nil || indexed == nil || indexed.Mode().Type() != info.Mode().Type() {
		return info, err
	}
	return keepIdentity(indexedMode(info, indexed), indexed), nil
}

// indexedMode restores the POSIX mode that Windows does not keep on the inode.
func indexedMode(info, indexed os.FileInfo) os.FileInfo {
	if indexed.Mode().Type() != info.Mode().Type() {
		return info
	}
	return modeInfo{FileInfo: info, mode: indexed.Mode()}
}

// readMode reads name's mode stream; ok is false when it has none.
func readMode(name string) (os.FileMode, bool, error) {
	stream, err := windows.UTF16PtrFromString(name + modeStream)
	if err != nil {
		return 0, false, err
	}
	h, err := windows.CreateFile(stream, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, &fs.PathError{Op: "open", Path: name + modeStream, Err: err}
	}
	defer windows.CloseHandle(h)
	var buf [16]byte
	var n uint32
	if err := windows.ReadFile(h, buf[:], &n, nil); err != nil {
		return 0, false, &fs.PathError{Op: "read", Path: name + modeStream, Err: err}
	}
	data := buf[:n]
	bits, err := strconv.ParseUint(string(data), 8, 32)
	if err != nil || bits&^0o7777 != 0 {
		return 0, false, fmt.Errorf("overlay: %s%s holds no Unix mode: %q", name, modeStream, data)
	}
	mode := os.FileMode(bits & 0o777)
	if bits&0o4000 != 0 {
		mode |= os.ModeSetuid
	}
	if bits&0o2000 != 0 {
		mode |= os.ModeSetgid
	}
	if bits&0o1000 != 0 {
		mode |= os.ModeSticky
	}
	return mode, true, nil
}

// lchmod refuses to change a symlink's mode, which Windows does not keep.
func lchmod(name string, _ os.FileMode) error {
	return &fs.PathError{Op: "chmod", Path: name, Err: syscall.EOPNOTSUPP}
}

// keepMode gives name, which is not a symlink, the Unix mode's permission and
// setuid/setgid/sticky bits: the read-only attribute mirrors the owner write
// bit, and a mode other than the default is stored in the mode stream, as is
// any mode of a file that already has one. Writing the stream leaves the
// file's timestamps alone. It fails on filesystems without alternate data
// streams when the mode is not the default.
func keepMode(name string, mode os.FileMode) error {
	path, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	attrs, err := windows.GetFileAttributes(path)
	if err != nil {
		return &fs.PathError{Op: "chmod", Path: name, Err: err}
	}
	mode &= memModeBits
	readOnly := mode&0o200 == 0
	_, stored, err := readMode(name)
	if err != nil {
		return err
	}
	if stored || mode != defaultPerm(attrs&windows.FILE_ATTRIBUTE_DIRECTORY != 0, readOnly) {
		// The read-only attribute also forbids writing the file's streams.
		if attrs&windows.FILE_ATTRIBUTE_READONLY != 0 {
			if err := windows.SetFileAttributes(path, attrs&^windows.FILE_ATTRIBUTE_READONLY); err != nil {
				return &fs.PathError{Op: "chmod", Path: name, Err: err}
			}
			attrs &^= windows.FILE_ATTRIBUTE_READONLY
		}
		if err := writeMode(name, mode); err != nil {
			return err
		}
	}
	want := attrs &^ windows.FILE_ATTRIBUTE_READONLY
	if readOnly {
		want |= windows.FILE_ATTRIBUTE_READONLY
	}
	if want != attrs {
		if err := windows.SetFileAttributes(path, want); err != nil {
			return &fs.PathError{Op: "chmod", Path: name, Err: err}
		}
	}
	return nil
}

// writeMode replaces name's mode stream with mode's octal Unix bits.
func writeMode(name string, mode os.FileMode) error {
	bits := uint64(mode.Perm())
	if mode&os.ModeSetuid != 0 {
		bits |= 0o4000
	}
	if mode&os.ModeSetgid != 0 {
		bits |= 0o2000
	}
	if mode&os.ModeSticky != 0 {
		bits |= 0o1000
	}
	stream, err := windows.UTF16PtrFromString(name + modeStream)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(stream, windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_ALWAYS, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return &fs.PathError{Op: "chmod", Path: name + modeStream, Err: err}
	}
	defer windows.CloseHandle(h)
	// All-ones times suspend the updates this handle's write would make.
	keep := windows.Filetime{LowDateTime: 0xFFFFFFFF, HighDateTime: 0xFFFFFFFF}
	if err := windows.SetFileTime(h, nil, &keep, &keep); err != nil {
		return &fs.PathError{Op: "chmod", Path: name + modeStream, Err: err}
	}
	if err := windows.SetEndOfFile(h); err != nil {
		return &fs.PathError{Op: "chmod", Path: name + modeStream, Err: err}
	}
	data := []byte(strconv.FormatUint(bits, 8))
	var n uint32
	if err := windows.WriteFile(h, data, &n, nil); err != nil {
		return &fs.PathError{Op: "chmod", Path: name + modeStream, Err: err}
	}
	return nil
}

func makeNode(name, _ string, _ os.FileMode) error {
	return &fs.PathError{Op: "mknod", Path: name, Err: syscall.ENOTSUP}
}
