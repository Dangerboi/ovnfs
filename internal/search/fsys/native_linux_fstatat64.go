//go:build linux && (386 || arm || mips || mipsle || ppc)

package fsys

import "golang.org/x/sys/unix"

// sysFstatat is the fstatat-with-flags syscall of this architecture; it
// fills the 64-bit unix.Stat_t layout.
const sysFstatat = unix.SYS_FSTATAT64
