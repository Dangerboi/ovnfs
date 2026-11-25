//go:build windows

package main

import (
	"fmt"
	"github.com/stencil-hq/ovnfs/export"
	"github.com/stencil-hq/ovnfs/stats"
	"net"
	"os"
)

// serveNFS rejects a protocol unavailable on Windows.
func serveNFS(net.Listener, *export.Export, *stats.Recorder, mountSettings) error {
	return fmt.Errorf("NFS serving unsupported on Windows; use -proto smb")
}

// refreshRoot has no NFS client cache to refresh on Windows.
func refreshRoot(string) {}

// refreshPaths has no NFS client cache to refresh on Windows.
func refreshPaths(string, string, []string, []string, func(string) (os.FileInfo, error)) {}
