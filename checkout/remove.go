package checkout

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// removeTree is os.RemoveAll for a tree inside the state directory. A checkout's
// tools may leave directories below its upper unwritable, and no entry of such a
// directory can be unlinked, so on a permission error it first gives the owner
// access to the directories under path and removes again. The walk never follows
// a symlink, so only directories inside path are touched.
func removeTree(path string) error {
	err := os.RemoveAll(path)
	if err == nil || !errors.Is(err, fs.ErrPermission) {
		return err
	}
	// Failures to chmod are not reported: the removal below reports what remains.
	filepath.WalkDir(path, func(p string, d fs.DirEntry, walkErr error) error {
		if d != nil && d.IsDir() {
			if info, e := d.Info(); e == nil && info.Mode().Perm()&0o700 != 0o700 {
				os.Chmod(p, info.Mode().Perm()|0o700)
			}
		}
		return nil
	})
	return os.RemoveAll(path)
}
