//go:build !windows

package winfsp

import (
	"fmt"
	"runtime"

	"github.com/stencil-hq/ovnfs/export"
)

// Server is an active WinFsp mount; WinFsp is served only on Windows.
type Server struct{}

// Installed reports false: WinFsp exists only on Windows.
func Installed() bool { return false }

// Mount reports that WinFsp serving is unsupported on this OS.
func Mount(*export.Export, string) (*Server, error) {
	return nil, fmt.Errorf("winfsp: unsupported on %s", runtime.GOOS)
}

// Unmount reports that no mount can exist on this OS.
func (*Server) Unmount(bool) error { return fmt.Errorf("winfsp: unsupported on %s", runtime.GOOS) }

// Wait returns at once because no mount can exist on this OS.
func (*Server) Wait() {}
