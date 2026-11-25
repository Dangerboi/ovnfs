package base

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

// locator finds the view path of a State identity, an inode of the state
// volume, with fsgetpath(2): a few microseconds, without walking anything.
// A file with several names, whose path that call could not choose, never
// hands out its inode's identity (see [Caches.lookupInfo]).
type locator struct {
	once sync.Once
	fsid unix.Fsid
	// The snapshot's tree and the folder as the kernel names them, symlinks
	// resolved; "" when unknown.
	tree, folder string
}

// seen remembers nothing: fsgetpath(2) needs no earlier lookup.
func (*locator) seen(os.FileInfo, string) {}

// listed remembers nothing either.
func (*locator) listed(os.FileInfo, string) {}

// locate returns a view path of the entry with inode ino that the view
// shows, as shown reports: a file of a local root, in the snapshot's tree, or
// the folder's own file below a cache root.
func (c *Caches) locate(b *Base, ino uint64, shown func(p string) bool) (string, bool) {
	l := &c.locator
	l.once.Do(func() {
		// The base's state exists before any snapshot's tree does.
		var st unix.Statfs_t
		state, err := filepath.EvalSymlinks(b.config.StateDir)
		if err != nil || unix.Statfs(state, &st) != nil {
			return
		}
		l.fsid, l.tree = st.Fsid, filepath.Join(state, cachesDir, c.id, treeDir)
		if c.shared() {
			l.folder, _ = filepath.EvalSymlinks(b.meta.Dir)
		}
	})
	if l.tree == "" {
		return "", false
	}
	var buf [unix.PathMax]byte
	n, _, errno := unix.Syscall6(unix.SYS_FSGETPATH, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), uintptr(unsafe.Pointer(&l.fsid)), uintptr(ino), 0, 0)
	if errno != 0 || int(n) > len(buf) {
		return "", false
	}
	found := buf[:n]
	if i := bytes.IndexByte(found, 0); i >= 0 {
		found = found[:i]
	}
	rel, ok := strings.CutPrefix(string(found), l.tree+"/")
	if !ok && l.folder != "" {
		rel, ok = strings.CutPrefix(string(found), l.folder+"/")
	}
	return rel, ok && shown(rel)
}
