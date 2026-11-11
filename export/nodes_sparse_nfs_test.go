//go:build !windows

package export

import (
	"encoding/binary"
	"fmt"
	"os"
	"path"
	"strings"
	"testing"

	"github.com/stencil-hq/ovnfs/internal/nfstest"
	"github.com/stencil-hq/ovnfs/nfs"
)

// The namespace supplies attributes while the production adapter supplies
// identity and path resolution. Every timed call is a real wire LOOKUP.
type sparseLookupFS struct {
	nfs.Backend
	ns *namespace
}

type sparseLookupInfo struct {
	entryInfo
	dir bool
}

func (i sparseLookupInfo) IsDir() bool { return i.dir }
func (i sparseLookupInfo) Mode() os.FileMode {
	if i.dir {
		return os.ModeDir | 0755
	}
	return 0644
}
func (f sparseLookupFS) Lstat(p string) (os.FileInfo, error) {
	if p != "" && !f.ns.entries[p] {
		return nil, os.ErrNotExist
	}
	return sparseLookupInfo{entryInfo(path.Base(p)), !strings.Contains(p, "/")}, nil
}

func TestSparseColdNFSLookup(t *testing.T) {
	testSparseColdLookups(t, func(t *testing.T, nt *nodeTable, ns *namespace) func(string, uint64) {
		e := &Export{fs: &nodeFS{nodes: nt}}
		c := nfstest.Start(t, sparseLookupFS{e.NFS(), ns})
		dirs := make(map[string][]byte)
		for d := range 4 {
			p := fmt.Sprintf("d%d", d)
			dirs[p] = binary.BigEndian.AppendUint64(nil, e.Nodes().ID(p))
		}
		return func(p string, id uint64) {
			dir, name := split(p)
			h, a, st, err := c.Lookup(dirs[dir], name)
			if err != nil || st != 0 {
				t.Fatalf("LOOKUP %q: %d %v", p, st, err)
			}
			if len(h) != 8 || binary.BigEndian.Uint64(h) != id || a.Fileid != id {
				t.Fatalf("LOOKUP %q changed identity: %x, fileid %x, want %x", p, h, a.Fileid, id)
			}
		}
	})
}
