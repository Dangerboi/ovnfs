//go:build windows

package winfsp

import (
	"fmt"
	"strings"
	"sync"

	"github.com/stencil-hq/ovnfs/export"
	gowinfsp "github.com/winfsp/go-winfsp"
)

// Server is an active WinFsp mount of the export root.
type Server struct {
	vol        *gowinfsp.FileSystem
	mountpoint string
	once       sync.Once
	done       chan struct{}
}

// Installed reports whether the WinFsp driver and its signed DLL are present.
func Installed() bool { return gowinfsp.LoadWinFSP() == nil }

// Mount serves e on the free drive root mountpoint (such as `O:\`) until
// Unmount, as the network volume \\ovnfs\O. It fails when WinFsp is not
// installed or the drive letter is taken. The drive belongs to the caller's
// logon session, as a net use mapping does.
func Mount(e *export.Export, mountpoint string) (*Server, error) {
	fs, err := newFileSystem(e.Nodes())
	if err != nil {
		return nil, fmt.Errorf("winfsp: mount %s: %w", mountpoint, err)
	}
	drive := strings.TrimSuffix(mountpoint, `\`)
	fs.roots = []string{drive + "/", "//ovnfs/" + strings.TrimSuffix(drive, ":") + "/"}
	vol, err := gowinfsp.Mount(fs, drive,
		// Microsoft Defender re-reads a file on every open of a local WinFsp
		// disk; as a network volume, like the SMB share, it scans far less
		// (a no-op go build of ovnfs: 3.6 s as a disk, 1.2 s as a network volume).
		gowinfsp.VolumePrefix(`\ovnfs\`+strings.TrimSuffix(drive, ":")),
		// Export names are case-sensitive, as over SMB.
		gowinfsp.CaseSensitive(true),
		gowinfsp.CasePreserveNames(true),
		// Some tools only use symlinks and file ids on NTFS; SMB reports it too.
		gowinfsp.FileSystemName("NTFS"),
		gowinfsp.SectorSize(512, 8),
		gowinfsp.FileInfoTimeout(TTL),
		gowinfsp.Attributes(
			// Delete and rename files other handles hold open, as on Unix.
			gowinfsp.FspFSAttributeSupportsPosixUnlinkRename|
				// Only deletions need Cleanup.
				gowinfsp.FspFSAttributePostCleanupWhenModifiedOnly),
	)
	if err != nil {
		return nil, fmt.Errorf("winfsp: mount %s: %w", mountpoint, err)
	}
	return &Server{vol: vol, mountpoint: mountpoint, done: make(chan struct{})}, nil
}

// Unmount removes the drive, ending every open handle on it, so it never needs
// force.
func (s *Server) Unmount(bool) error {
	s.once.Do(func() {
		s.vol.Unmount()
		close(s.done)
	})
	return nil
}

// Wait blocks until Unmount; nothing else can remove the drive.
func (s *Server) Wait() { <-s.done }
