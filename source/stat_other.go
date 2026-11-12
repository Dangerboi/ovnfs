//go:build !darwin && !linux && !windows

package source

import "os"

// readDir lists host directory dir sorted by name; there is no inode data here,
// so stat matches rely on size and mtime alone.
func readDir(dir string) ([]os.DirEntry, func(os.FileInfo) fileKey, error) {
	entries, err := os.ReadDir(dir)
	return entries, func(os.FileInfo) fileKey { return fileKey{} }, err
}

// readers is how many directories are read ahead at once.
const readers = 4
