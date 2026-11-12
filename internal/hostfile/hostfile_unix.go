//go:build !windows

package hostfile

import "os"

// OpenFile is [os.OpenFile]: Unix descriptors never block renames or unlinks.
func OpenFile(name string, flag int, perm os.FileMode) (*os.File, error) {
	return os.OpenFile(name, flag, perm)
}

// Rename is [os.Rename], which already replaces open files.
func Rename(oldpath, newpath string) error { return os.Rename(oldpath, newpath) }

// Remove is [os.Remove], which never changes other hard links' modes.
func Remove(name string) error { return os.Remove(name) }

// CreateTemp is [os.CreateTemp].
func CreateTemp(dir, pattern string) (*os.File, error) { return os.CreateTemp(dir, pattern) }

// Symlink is [os.Symlink]; every Unix account may create symlinks.
func Symlink(target, link string) error { return os.Symlink(target, link) }

// Linked returns info: Unix has no junctions.
func Linked(_ string, info os.FileInfo) os.FileInfo { return info }
