//go:build darwin || linux

package source

import (
	"os"

	"github.com/stencil-hq/ovnfs/internal/search/fsys"
)

// readDir lists host directory dir sorted by name; entry keys come from their own stat data.
// The native listing already holds every entry's complete lstat data, so reading an
// entry's Info costs no system call, and an entry it could not describe reports that
// error from its own Info instead of failing the directory.
func readDir(dir string) ([]os.DirEntry, func(os.FileInfo) fileKey, error) {
	entries, err := fsys.ReadDir(dir)
	return entries, statKey, err
}
