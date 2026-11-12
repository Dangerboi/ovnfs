//go:build darwin || linux

package fsys

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

type handle = int

const (
	dirOpenFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC
	// O_NONBLOCK: a FIFO posing as a regular file must not block before
	// fstat rejects it.
	fileOpenFlags = unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NONBLOCK

	pathSep = '/'
	// maxRead bounds one pread: larger requests fail with EINVAL on some
	// kernels and are truncated by others at this size.
	maxRead = 0x7ffff000
	// memoStableByDev: stamps carry real device numbers, so the
	// per-filesystem StableStamps verdict can be memoized per device.
	memoStableByDev = true
)

func (s *session) openDir(path string) (handle, error) {
	c, err := s.cstr(path)
	if err != nil {
		return 0, err
	}
	return openat(unix.AT_FDCWD, c, dirOpenFlags)
}

func (s *session) openChild(parent handle, name string) (handle, error) {
	c, err := s.cstr(name)
	if err != nil {
		return 0, err
	}
	return openat(parent, c, dirOpenFlags|unix.O_NOFOLLOW)
}

func closeHandle(h handle) { unix.Close(h) }

func (s *session) Identify(path string) (ID, error) {
	info, err := s.Stat(path, true)
	return ID{Dev: info.Stamp.Dev, Ino: info.Stamp.Ino}, err
}

// IdentifyDir identifies the open directory h from its descriptor, without
// resolving a path.
func (s *session) IdentifyDir(h Handle) (ID, error) {
	st, _, _, err := statHandle(h.native())
	return ID{Dev: st.Dev, Ino: st.Ino}, err
}

func typeFromDT(t uint8) Type {
	switch t {
	case unix.DT_REG:
		return File
	case unix.DT_DIR:
		return Dir
	case unix.DT_LNK:
		return Symlink
	}
	return 0
}

func typeFromMode(mode uint32) Type {
	switch mode & unix.S_IFMT {
	case unix.S_IFREG:
		return File
	case unix.S_IFDIR:
		return Dir
	case unix.S_IFLNK:
		return Symlink
	}
	return 0
}

func (s *session) OpenFile(dir Handle, name string) (Handle, FileInfo, error) {
	at := unix.AT_FDCWD
	if dir.ok {
		at = int(dir.h)
	}
	c, err := s.cstr(name)
	if err != nil {
		return Handle{}, FileInfo{}, err
	}
	fd, err := openat(at, c, fileOpenFlags)
	if err != nil {
		return Handle{}, FileInfo{}, err
	}
	st, typ, size, err := statHandle(fd)
	if err == nil && typ != File {
		err = ErrNotRegular
	}
	if err != nil {
		unix.Close(fd)
		return Handle{}, FileInfo{}, err
	}
	return Handle{h: uintptr(fd), size: size, ok: true}, FileInfo{Type: File, Size: size, Stamp: st}, nil
}

func (s *session) Read(h Handle, buf []byte, limit int64) ([]byte, error) {
	fd := int(h.h)
	return readInto(buf, h.size, limit, func(p []byte, off int64) (int, error) {
		for {
			n, err := unix.Pread(fd, p[:min(len(p), maxRead)], off)
			if err != unix.EINTR {
				return n, err
			}
		}
	})
}

// Map uses a shared mapping: on darwin a private one kept serving pages
// past a truncation instead of faulting, which Guard could not report.
// MADV_WILLNEED starts read-ahead, as callers scan mappings front to back.
func (s *session) Map(h Handle, limit int64) ([]byte, error) {
	n := min(h.size, limit)
	p, err := unix.MmapPtr(int(h.h), 0, nil, uintptr(n), unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		return nil, err
	}
	data := unsafe.Slice((*byte)(p), n)
	unix.Madvise(data, unix.MADV_WILLNEED) // advisory: failing costs only the read-ahead
	return data, nil
}

func (s *session) Unmap(data []byte) {
	// munmap fails only for a range no mmap returned.
	unix.MunmapPtr(unsafe.Pointer(unsafe.SliceData(data)), uintptr(cap(data)))
}
