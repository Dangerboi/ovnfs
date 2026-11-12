package base

import (
	"os"
	"syscall"
	"time"

	"github.com/stencil-hq/ovnfs/internal/durable"
	"golang.org/x/sys/unix"
)

// volumeID distinguishes clone-compatible device boundaries.
func volumeID(p string) (uint64, error) {
	info, err := os.Stat(p)
	if err != nil {
		return 0, err
	}
	return uint64(info.Sys().(*syscall.Stat_t).Dev), nil
}

// fileIdentity returns the device and inode that tell one file from a lookalike.
func fileIdentity(info os.FileInfo, _, _ string) (uint64, uint64, error) {
	s := info.Sys().(*syscall.Stat_t)
	return uint64(s.Dev), uint64(s.Ino), nil
}

// setTimes preserves nanosecond mtimes without following symlinks.
func setTimes(p string, t time.Time, symlink bool) error {
	flags := 0
	if symlink {
		flags = unix.AT_SYMLINK_NOFOLLOW
	}
	ts := unix.NsecToTimespec(t.UnixNano())
	return unix.UtimesNanoAt(unix.AT_FDCWD, p, []unix.Timespec{ts, ts}, flags)
}

// syncDirectory makes atomic metadata renames durable.
func syncDirectory(p string) error { return durable.Dir(p) }
