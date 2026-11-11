package export

import (
	"strings"

	"github.com/stencil-hq/ovnfs/overlay"
	"github.com/stencil-hq/ovnfs/smb"
)

// Nodes is the export for frontends that address files by stable node id
// (FUSE inodes, remote-fs). Its filesystem and metadata calls go through the
// same node bookkeeping as NFS and SMB, so ids follow renames and removals.
// FileInfo values retain native metadata; ID supplies the stable node id.
type Nodes struct {
	*nodeFS
	volume string
}

// Nodes exposes the routed filesystem with its stable node ids.
func (e *Export) Nodes() *Nodes { return &Nodes{nodeFS: e.fs, volume: e.volume} }

// ID returns p's stable node id, allocating one on first use; the export root is 1.
// Zero means an index fault or a degraded cache refused a fresh allocation.
func (n *Nodes) ID(p string) uint64 { return n.nodes.id(overlay.Clean(p)) }

// Path resolves a live node id to its current export-relative path, or false
// once the id was never issued, removed, or replaced by a rename.
func (n *Nodes) Path(id uint64) (string, bool) {
	parts, ok := n.nodes.path(id)
	return strings.Join(parts, "/"), ok
}

// HoldFileID retains an identity while a frontend has live file state.
// The returned function must be called once when that state is released.
func (n *Nodes) HoldFileID(id uint64) func() { return n.nodes.retain(id) }

// Statfs reports space on the upper volume, where all overlay writes land.
func (n *Nodes) Statfs() (smb.Statfs, error) { return volumeStatfs(n.volume) }
