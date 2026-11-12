// Package clone copies regular files and directory hierarchies by sharing their
// data blocks where the filesystem supports it, and by copying bytes everywhere
// else, so callers get one portable copy primitive:
//
//	Platform  Primitive                        Filesystems
//	macOS     clonefile(2), fclonefileat(2)    APFS
//	Linux     FICLONE                          btrfs, XFS, bcachefs, OCFS2, …
//	Windows   FSCTL_DUPLICATE_EXTENTS_TO_FILE  ReFS, Dev Drive
//
// Reflink alone reports errors.ErrUnsupported, leaving dst absent, where blocks
// cannot be shared (another volume, a filesystem without cloning, another
// platform), for callers with a cheaper fallback than a plain copy.
package clone

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"

	"github.com/stencil-hq/ovnfs/internal/hostfile"
)

// File clones the regular file at src to the new path dst, preserving its permission bits.
func File(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("clone %s: %w", src, err)
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return fmt.Errorf("clone %s: %w", src, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("clone %s: not a regular file", src)
	}
	return FD(in, dst, info.Mode().Perm())
}

// FD clones the open regular file in to the new path dst (O_EXCL) with mode,
// copying bytes from the open handle when the filesystem cannot clone.
func FD(in *os.File, dst string, mode os.FileMode) error {
	err := Reflink(in, dst, mode)
	if errors.Is(err, errors.ErrUnsupported) {
		return copyBytes(in, dst, mode)
	}
	return err
}

// Tree clones the file, symlink or directory hierarchy at src to the new path
// dst without following symlinks, preserving permission bits and modification
// times. APFS clones a whole hierarchy in one clonefile(2) call, which also
// keeps extended attributes and any sockets, fifos and devices inside it.
// Elsewhere, or when that call is unsupported, Tree walks the hierarchy,
// cloning each regular file as [FD] does and skipping special files. A special
// file at src copies nothing. A Windows junction is a symlink here
// ([hostfile.Linked]) and copies as one ([hostfile.Symlink]).
func Tree(src, dst string) error {
	info, err := hostfile.Lstat(src)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() && !info.IsDir() && info.Mode()&fs.ModeSymlink == 0 {
		return nil
	}
	if cloned, err := cloneHierarchy(src, dst, info); cloned || err != nil {
		return err
	}
	var w walk
	return w.tree(src, dst, info)
}

// walk is one entry-by-entry copy of a hierarchy for [Tree].
type walk struct {
	// copying is set once a clone was unsupported, so later files skip the
	// attempt: the rest of the hierarchy is almost always on the same volume.
	copying bool
}

// tree copies the entry at src, described by info, to the new path dst.
func (w *walk) tree(src, dst string, info fs.FileInfo) error {
	switch mode := info.Mode(); {
	case mode&fs.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		if err := hostfile.Symlink(target, dst); err != nil {
			return err
		}
		return hostfile.Lchtimes(dst, info.ModTime(), info.ModTime())
	case mode.IsDir():
		// Owner access until the children are in, so a read-only directory fills.
		if err := os.Mkdir(dst, mode.Perm()|0o700); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, e := range entries {
			child, err := e.Info()
			if err != nil {
				return err
			}
			from := filepath.Join(src, e.Name())
			if err := w.tree(from, filepath.Join(dst, e.Name()), hostfile.Linked(child, from)); err != nil {
				return err
			}
		}
		if err := os.Chmod(dst, mode.Perm()); err != nil {
			return err
		}
	case mode.IsRegular():
		if err := w.file(src, dst, mode.Perm()); err != nil {
			return err
		}
	default:
		return nil
	}
	return os.Chtimes(dst, info.ModTime(), info.ModTime())
}

// file clones or copies the regular file src to the new path dst with mode.
func (w *walk) file(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if !w.copying {
		err := Reflink(in, dst, mode)
		if !errors.Is(err, errors.ErrUnsupported) {
			return err
		}
		w.copying = true
	}
	return copyBytes(in, dst, mode)
}

// copyBytes creates dst exclusively and copies all of in from offset zero, so
// the result matches a clone regardless of the descriptor's current offset.
func copyBytes(in *os.File, dst string, mode os.FileMode) error {
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("clone to %s: %w", dst, err)
	}
	_, err = io.Copy(out, io.NewSectionReader(in, 0, math.MaxInt64))
	if err == nil {
		err = out.Chmod(mode)
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(dst)
		return fmt.Errorf("copy to %s: %w", dst, err)
	}
	return nil
}
