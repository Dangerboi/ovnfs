// Package fsys is the native directory and file access layer shared by the
// search tools, source capture and the base view: compact directory listings,
// file contents, and change stamps through [FS] and [Session], and complete
// lstat-compatible directory listings through [ReadDir]. Both run on the same
// per-platform syscall fast paths.
//
// [Native] is the host filesystem with per-platform syscall fast paths
// (native_darwin.go, native_linux.go, native_windows.go); [FromIOFS] adapts
// any io/fs.FS, such as an in-memory tree or a remote provider. The walker,
// the ignore-file loader, the listing cache, and the search tools only ever
// talk to an [FS], so a tool never bypasses the layer with its own I/O.
//
// Work happens on a [Session]: per-goroutine scratch that makes steady-state
// listing and reading allocation-free. Walkers hold one Session per worker.
// Files are opened once ([Session.OpenFile], which also stats them) and
// then read into a buffer or, when large enough to be worth it, mapped.
//
//	s := fsys.Native.NewSession()
//	h, err := s.OpenDir("/repo")
//	ls, err := s.List(h, fsys.Minimal, 0)
//	for i := range ls.Entries {
//		fmt.Println(string(ls.Name(&ls.Entries[i])), ls.Entries[i].Type)
//	}
//	s.Close(h)
package fsys

import (
	"errors"
	"runtime/debug"
	"unsafe"
)

// FS is a filesystem the walker and tools read through.
type FS interface {
	// Abs returns root as an absolute, clean path in this filesystem.
	Abs(root string) (string, error)
	// NewSession returns scratch and handle operations for one goroutine.
	NewSession() Session
	// Separator is the path separator of absolute paths ('/' or '\\').
	Separator() byte
	// Workers is how many goroutines can list in parallel before contention
	// costs more than it gains.
	Workers() int
}

// Session performs filesystem operations for one goroutine. Slices it
// returns are valid until the next call of the same method.
type Session interface {
	// OpenDir opens the directory at an absolute path, following symlinks.
	OpenDir(path string) (Handle, error)
	// OpenChild opens the subdirectory name of parent without following a
	// symlink that replaced it since parent was listed.
	OpenChild(parent Handle, name string) (Handle, error)
	// Close releases a directory or file handle.
	Close(h Handle)
	// List reads every entry of h except "." and "..", skipping special
	// files. children is the directory's entry count from its parent's Full
	// listing (0 when unknown); readers may use it to end early.
	List(h Handle, detail Detail, children uint32) (*Listing, error)
	// OpenFile opens the regular file name for Read or Map and describes
	// it. name is resolved relative to dir following symlinks and may have
	// several '/'-separated components; with a zero Handle it is an
	// absolute path. Errors: fs.ErrNotExist and other open errors, or
	// [ErrNotRegular].
	OpenFile(dir Handle, name string) (Handle, FileInfo, error)
	// Read reads up to limit leading bytes of the file h into buf (grown as
	// needed), stopping at the size OpenFile reported or at end of file.
	Read(h Handle, buf []byte, limit int64) ([]byte, error)
	// Map maps up to limit leading bytes of the file h read-only, stopping
	// at the size OpenFile reported. The mapping survives Close(h) until
	// Unmap; reading it faults once the file shrinks below it (see
	// [Guard]). It errors on an empty file and with errors.ErrUnsupported
	// where files are not mapped (Windows, io/fs filesystems).
	Map(h Handle, limit int64) ([]byte, error)
	// Unmap releases a mapping Map returned.
	Unmap(data []byte)
	// Stat describes the file or directory at an absolute path, following
	// a final symlink only when follow is set.
	Stat(path string, follow bool) (FileInfo, error)
	// StatDir stamps an open directory.
	StatDir(h Handle) (Stamp, error)
	// Identify returns the identity of the file or directory at an
	// absolute path, following symlinks; walkers compare identities to
	// stop symlink cycles. errors.ErrUnsupported when the filesystem has
	// no identities.
	Identify(path string) (ID, error)
	// IdentifyDir returns the identity of the open directory h without
	// resolving a path; errors.ErrUnsupported when the filesystem has no
	// identities or handles are not descriptors.
	IdentifyDir(h Handle) (ID, error)
	// StableStamps reports whether every change to a directory on h's
	// filesystem (whose stamp is st) yields a different stamp: timestamps
	// are fine-grained and maintained locally. Listing caches rely on it.
	StableStamps(h Handle, st Stamp) bool
}

// ID identifies a file or directory regardless of the path used to reach
// it: device and inode on Unix, volume serial number and file index on
// Windows.
type ID struct{ Dev, Ino uint64 }

// ErrNotRegular is returned by Session.OpenFile for directories, devices,
// FIFOs, and sockets.
var ErrNotRegular = errors.New("not a regular file")

// Handle is an open directory or file. The zero Handle means "no
// directory".
type Handle struct {
	h    uintptr // native handle
	path string  // adapter path
	size int64   // a file's size when opened, where Read and Map stop
	ok   bool
}

// Valid reports whether h refers to an open directory or file.
func (h Handle) Valid() bool { return h.ok }

// FileInfo describes a file opened by Session.OpenFile or found by
// Session.Stat.
type FileInfo struct {
	// Type is 0 for special files (devices, FIFOs, sockets).
	Type Type
	// Size is the file's full size; Read and Map cut the content they
	// return to their limit when Size exceeds it.
	Size  int64
	Stamp Stamp
}

// Guard runs read, which reads data (a mapping from Session.Map), and
// reports false if read faulted on data: the file shrank below the mapping
// after it was made. Faults elsewhere and other panics propagate. Buffers
// never fault, so data may also be an ordinary buffer.
func Guard(data []byte, read func()) (ok bool) {
	defer debug.SetPanicOnFault(debug.SetPanicOnFault(true))
	defer func() {
		if ok {
			return
		}
		r := recover()
		if f, isFault := r.(interface{ Addr() uintptr }); isFault {
			lo := uintptr(unsafe.Pointer(unsafe.SliceData(data)))
			if a := f.Addr(); a >= lo && a-lo < uintptr(cap(data)) {
				return
			}
		}
		if r != nil {
			panic(r)
		}
	}()
	read()
	return true
}

// Stamp identifies one version of a file or directory: adding, removing,
// or renaming a directory's entries changes its mtime and ctime, and
// replacing it changes its inode. On Windows, path queries expose no file
// ID, so Ino holds the creation time; filesystems without stable stamps
// fill what they can (see Session.StableStamps).
type Stamp struct {
	Dev, Ino     uint64
	Mtime, Ctime int64 // Unix nanoseconds
}

// Latest is the newer of s's timestamps.
func (s Stamp) Latest() int64 { return max(s.Mtime, s.Ctime) }

// Type is the kind of a directory entry. Special files (devices, FIFOs,
// sockets) are never reported.
type Type uint8

const (
	// File is a regular file.
	File Type = iota + 1
	// Dir is a directory.
	Dir
	// Symlink is a symbolic link (or, on Windows, any reparse point). It is
	// never followed.
	Symlink
)

func (t Type) String() string {
	switch t {
	case File:
		return "file"
	case Dir:
		return "dir"
	case Symlink:
		return "symlink"
	}
	return "unknown"
}

// Detail selects how much metadata directory listings collect.
type Detail uint8

const (
	// Minimal collects names and entry types only: one bulk listing call per
	// directory and no per-entry metadata syscalls on any platform.
	Minimal Detail = iota
	// Full also collects modification times for every entry and sizes for
	// regular files. Darwin and Windows return these from the bulk listing;
	// Linux pays one statx per entry.
	Full
	// metadataDetail is [ReadDir]'s listing: every entry, special files and
	// symlinks included, with complete lstat metadata or the error describing
	// it, collected into session.entries rather than the compact listing.
	metadataDetail
)

// Listing is one directory's entries, owned by the Session that read it.
type Listing struct {
	Entries []Record
	names   []byte
}

// Record is one listing entry.
type Record struct {
	off, nlen uint32 // name span in Listing.names
	// Children is a subdirectory's entry count when the listing reports it
	// (darwin Full listings), else 0.
	Children uint32
	Type     Type
	// Size is the byte size of regular files when known, else -1.
	Size int64
	// Mtime is the modification time in Unix nanoseconds when known, else 0.
	Mtime int64
}

// Name returns r's name, valid as long as the listing.
func (l *Listing) Name(r *Record) []byte { return l.names[r.off : r.off+r.nlen] }

// Size approximates l's memory footprint in bytes.
func (l *Listing) Size() int {
	return len(l.names) + len(l.Entries)*int(unsafe.Sizeof(Record{}))
}

// Clone copies l so it outlives the session's next List.
func (l *Listing) Clone() *Listing {
	return &Listing{Entries: append([]Record(nil), l.Entries...), names: append([]byte(nil), l.names...)}
}

func (l *Listing) reset() {
	l.Entries = l.Entries[:0]
	l.names = l.names[:0]
}

func (l *Listing) add(name []byte, typ Type, size, mtime int64, children uint32) {
	l.Entries = append(l.Entries, Record{
		off:      uint32(len(l.names)),
		nlen:     uint32(len(name)),
		Children: children,
		Type:     typ,
		Size:     size,
		Mtime:    mtime,
	})
	l.names = append(l.names, name...)
}

func isDotOrDotDot(name []byte) bool {
	return len(name) <= 2 && len(name) > 0 && name[0] == '.' && (len(name) == 1 || name[1] == '.')
}
