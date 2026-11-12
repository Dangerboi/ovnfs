package source

import (
	"os"
	"syscall"
)

// statKey reads the inode and inode-change time from stat data.
func statKey(info os.FileInfo) fileKey {
	s, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fileKey{}
	}
	return fileKey{ino: s.Ino, ctime: s.Ctim.Nano()}
}

// readers is how many directories are read ahead at once: eight workers walked faster and steadier than sixteen on a 16-vCPU VM (see search/fsys).
const readers = 8
