//go:build linux && (amd64 || arm64 || loong64 || mips64 || mips64le || ppc64 || ppc64le || riscv64 || s390x)

package fsys

import "golang.org/x/sys/unix"

// sysFstatat is the fstatat-with-flags syscall of this architecture.
const sysFstatat = unix.SYS_NEWFSTATAT
