// Package dirbottom provides a live host-directory bottom for overlay tests.
package dirbottom

import (
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/stencil-hq/ovnfs/internal/clone"
	"github.com/stencil-hq/ovnfs/internal/hostfile"
	"github.com/stencil-hq/ovnfs/overlay"
)

// bottom exposes ordinary host entries without interpreting overlay whiteouts.
type bottom struct{ dir string }

// New returns a read-only overlay view of dir; mutations occur only through CopyTo's destination.
func New(dir string) overlay.Bottom { return bottom{dir: dir} }

// Lstat returns metadata without following the final symlink.
func (b bottom) Lstat(p string) (os.FileInfo, error) {
	return os.Lstat(filepath.Join(b.dir, filepath.FromSlash(p)))
}

// ReadDir returns children in name order, retaining symlink metadata.
func (b bottom) ReadDir(p string) ([]os.FileInfo, error) {
	info, err := b.Lstat(p)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, &fs.PathError{Op: "readdir", Path: p, Err: syscall.ENOTDIR}
	}
	entries, err := os.ReadDir(filepath.Join(b.dir, filepath.FromSlash(p)))
	if err != nil {
		return nil, err
	}
	infos := make([]os.FileInfo, 0, len(entries))
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		infos = append(infos, info)
	}
	return infos, nil
}

// Open opens a regular file read-only; symlink resolution belongs to the overlay.
func (b bottom) Open(p string) (overlay.Content, error) {
	name := filepath.Join(b.dir, filepath.FromSlash(p))
	info, err := os.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, &os.PathError{Op: "open", Path: name, Err: os.ErrInvalid}
	}
	f, err := hostfile.Open(name)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// Readlink returns the stored target without resolving it against the host
// directory, slash-separated as Windows hosts do not store it.
func (b bottom) Readlink(p string) (string, error) {
	target, err := os.Readlink(filepath.Join(b.dir, filepath.FromSlash(p)))
	return filepath.ToSlash(target), err
}

// CopyTo materializes an entry at a new host path with [clone.Tree].
func (b bottom) CopyTo(p, dst string) error {
	return clone.Tree(filepath.Join(b.dir, filepath.FromSlash(p)), dst)
}
