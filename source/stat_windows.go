package source

import (
	"os"
	"path/filepath"
	"unsafe"

	"github.com/stencil-hq/ovnfs/internal/search/fsys"
	"golang.org/x/sys/windows"
)

// readers is how many directories are read ahead at once.
const readers = 4

// readDir lists host directory dir sorted by name, enumerating it once more first
// for its entries' NTFS file ids, which os.FileInfo hides. Ids are read before the
// entries, so a file replaced in between carries a stale id, which only stops it
// being served from the folder. Ctime stays 0: Git for Windows stores creation
// time in the index ctime. Entries get ino 0 when the volume has no stable file
// ids (FAT, exFAT), the enumeration fails, or the name appeared in between.
// Subdirectories are statted themselves; see [freshDir].
func readDir(dir string) ([]os.DirEntry, func(os.FileInfo) fileKey, error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	ids := fileIDs(windows.Handle(f.Fd()))
	entries, err := fsys.ReadDir(dir)
	for i, e := range entries {
		if e.IsDir() {
			entries[i] = freshDir{DirEntry: e, path: filepath.Join(dir, e.Name())}
		}
	}
	return entries, func(info os.FileInfo) fileKey { return fileKey{ino: ids[info.Name()]} }, err
}

// freshDir is a subdirectory entry whose Info is the directory's own
// [os.Lstat]. NTFS refreshes the copy of a directory's timestamps kept in its
// parent's index only once a handle to the changed directory closes, so listing
// data would show a directory's mtime from before its last added or removed
// entry until the walk itself opened it, keying an unchanged tree differently
// on the next walk.
type freshDir struct {
	os.DirEntry
	path string
}

// Info stats the directory itself rather than its parent's index entry.
func (d freshDir) Info() (os.FileInfo, error) { return os.Lstat(d.path) }

// fileIDs maps the names in directory h to their file ids, which equal the
// FileIndexHigh<<32|FileIndexLow that GetFileInformationByHandle reports.
func fileIDs(h windows.Handle) map[string]uint64 {
	var flags uint32
	if err := windows.GetVolumeInformationByHandle(h, nil, 0, nil, nil, &flags, nil, 0); err != nil || flags&windows.FILE_SUPPORTS_OPEN_BY_FILE_ID == 0 {
		return nil
	}
	// uint64 words keep every entry 8-byte aligned, as the API requires.
	buf := make([]uint64, 8<<10)
	ids := map[string]uint64{}
	class := uint32(windows.FileIdBothDirectoryRestartInfo)
	for {
		if err := windows.GetFileInformationByHandleEx(h, class, (*byte)(unsafe.Pointer(&buf[0])), uint32(len(buf)*8)); err != nil {
			// ERROR_NO_MORE_FILES ends the listing; any other failure leaves the rest at ino 0.
			return ids
		}
		class = windows.FileIdBothDirectoryInfo
		for off := uintptr(0); ; {
			e := (*fileIDBothDirInfo)(unsafe.Add(unsafe.Pointer(&buf[0]), off))
			name := windows.UTF16ToString(unsafe.Slice(&e.FileName[0], e.FileNameLength/2))
			if name != "." && name != ".." {
				ids[name] = e.FileID
			}
			if e.NextEntryOffset == 0 {
				break
			}
			off += uintptr(e.NextEntryOffset)
		}
	}
}

// fileIDBothDirInfo is FILE_ID_BOTH_DIR_INFO, which x/sys/windows does not define.
type fileIDBothDirInfo struct {
	NextEntryOffset uint32
	FileIndex       uint32
	CreationTime    windows.Filetime
	LastAccessTime  windows.Filetime
	LastWriteTime   windows.Filetime
	ChangeTime      windows.Filetime
	EndOfFile       uint64
	AllocationSize  uint64
	FileAttributes  uint32
	FileNameLength  uint32
	EaSize          uint32
	ShortNameLength uint32
	ShortName       [12]uint16
	FileID          uint64
	FileName        [1]uint16
}
