package source

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/stencil-hq/ovnfs/git"
	"github.com/stencil-hq/ovnfs/store"
)

// statEntry remembers a file's stat data and the blob id its content hashed to.
type statEntry struct {
	Size  int64
	Mtime int64
	Ctime int64
	Ino   uint64
	Mode  uint32
	OID   store.OID
}

// fileKey is the identity that detects a replaced or rewritten file: its inode
// (NTFS file id on Windows) and inode-change time; zero where the host has none.
type fileKey struct {
	ino   uint64
	ctime int64
}

// newStatEntry records the stat data that must be unchanged for id to be reused.
func newStatEntry(info os.FileInfo, key fileKey, mode uint32, id store.OID) statEntry {
	return statEntry{Size: info.Size(), Mtime: info.ModTime().UnixNano(), Ctime: key.ctime, Ino: key.ino, Mode: mode, OID: id}
}

// matches reports a file whose stat data equals the recorded one.
func (e statEntry) matches(info os.FileInfo, key fileKey, mode uint32) bool {
	return e.Size == info.Size() && e.Mtime == info.ModTime().UnixNano() && e.Ctime == key.ctime && e.Ino == key.ino && e.Mode == mode
}

// statCache is a source folder's stat cache: entries whose blobs were all in the
// store during the store epoch Epoch.
type statCache struct {
	Epoch   string
	Entries map[string]statEntry
}

// loadStatCache reads a source folder's stat cache, whose paths and blob ids
// are the capture tree it names in trees; a missing, damaged or older-format
// file, or one whose tree is no longer stored, is an empty cache.
func loadStatCache(name string, trees *store.Store) statCache {
	empty := statCache{Entries: map[string]statEntry{}}
	if name == "" || trees == nil {
		return empty
	}
	data, err := os.ReadFile(name)
	if err != nil {
		return empty
	}
	c, err := decodeStatCache(data, trees)
	if err != nil {
		return empty
	}
	return c
}

// saveStatCache atomically replaces the cache with entries, leaves of the
// capture files stored as tree, leaving out entries modified within two
// seconds of cutoff: on coarse-timestamp filesystems their mtime cannot prove
// that a later write did not happen (git's racy-clean rule).
func saveStatCache(name string, tree store.OID, files map[string]store.File, entries map[string]statEntry, cutoff time.Time, epoch string) error {
	limit := cutoff.Add(-2 * time.Second).UnixNano()
	kept := statCache{Epoch: epoch, Entries: make(map[string]statEntry, len(entries))}
	for p, e := range entries {
		if e.Mtime < limit {
			kept.Entries[p] = e
		}
	}
	data, err := encodeStatCache(kept, tree, files)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		return err
	}
	// A unique temp so two concurrent saves cannot share name+".tmp": the
	// first rename would then leave the second with ENOENT, or the second
	// could write into the inode after it was published.
	f, err := os.CreateTemp(filepath.Dir(name), filepath.Base(name)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, name); err != nil {
		return err
	}
	// Builds before the binary format kept this cache as JSON beside it.
	legacy := strings.TrimSuffix(name, filepath.Ext(name)) + ".json"
	if legacy == name {
		return nil
	}
	if err := os.Remove(legacy); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// indexClean reports a regular file whose stat data still equals its git index
// entry, so its content is the entry's blob before any conversion git applies
// between blob and work tree; racily clean entries never count.
func indexClean(e git.IndexEntry, info os.FileInfo, key fileKey, mode uint32, indexModified time.Time) bool {
	if e.Mode != mode || int64(e.Size) != info.Size()&0xffffffff || !info.ModTime().Before(indexModified) {
		return false
	}
	if e.Inode != 0 && e.Inode != uint32(key.ino) {
		return false
	}
	return sameTime(e.Mtime, info.ModTime()) && (key.ctime == 0 || sameTime(e.Ctime, time.Unix(0, key.ctime)))
}

// sameTime compares index times, which may lack nanoseconds.
func sameTime(index, actual time.Time) bool {
	if index.Unix() != actual.Unix() {
		return false
	}
	return index.Nanosecond() == 0 || index.Nanosecond() == actual.Nanosecond()
}
