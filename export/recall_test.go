//go:build !windows

package export_test

import (
	"context"
	"encoding/binary"
	"os"
	"slices"
	"sync"
	"testing"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/internal/nfstest"
)

// recorder collects the stable nodes the export asks the frontend to recall.
type recorder struct {
	mu  sync.Mutex
	ids []uint64
}

// add records one recalled node.
func (r *recorder) add(id uint64) {
	r.mu.Lock()
	r.ids = append(r.ids, id)
	r.mu.Unlock()
}

// has reports whether the node in fh was recalled, then clears the recorder.
func (r *recorder) has(fh []byte) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	var id uint64
	if len(fh) == 8 {
		id = binary.BigEndian.Uint64(fh)
	}
	found := slices.Contains(r.ids, id)
	r.ids = nil
	return found
}

// TestInvalidateTreeRecallsOnlyThatSubtree recalls every handle below a changed
// tree and none beside it: a rescan deep in a build directory must not recheck
// every handle the checkout ever issued.
func TestInvalidateTreeRecallsOnlyThatSubtree(t *testing.T) {
	f := newStack(t, map[string]string{"t/d/x": "1", "s": "2"})
	f.create(t, "app")
	var rec recorder
	f.export.OnRecall(rec.add)
	c := nfstest.Start(t, f.export.NFS())
	x := lookup(t, c, "app/t/d/x")
	s := lookup(t, c, "app/s")
	rec.has(nil)

	f.export.Invalidate("app", nil, []string{"t"})
	if !rec.has(x) {
		t.Fatal("a tree invalidation did not recall a handle below it")
	}
	f.export.Invalidate("app", nil, []string{"t"})
	if rec.has(s) {
		t.Fatal("a tree invalidation recalled a handle outside it")
	}
	f.export.Invalidate("app", nil, []string{""})
	if !rec.has(s) {
		t.Fatal("invalidating the whole checkout missed a handle")
	}
}

// TestRecallsChangesBehindNFSClients covers every way a file changes without
// an NFS request: another protocol, the daemon (Invalidate), and checkout
// removal, while NFS requests leave recalls to the NFS server. Vanished paths retire
// their handles so clients see stale.
func TestRecallsChangesBehindNFSClients(t *testing.T) {
	f := newStack(t, map[string]string{"src": "base"})
	f.create(t, "app")
	var rec recorder
	f.export.OnRecall(rec.add)
	c := nfstest.Start(t, f.export.NFS())
	src := lookup(t, c, "app/src")
	gone := nfsCreate(t, c, lookup(t, c, "app"), "gone", "doomed")
	rec.has(nil) // the client's own requests are not the subject here

	nfsWrite(t, c, src, "nfs")
	if rec.has(src) {
		t.Fatal("an NFS write went through the export's recall hook; the NFS server recalls conflicting delegations itself")
	}

	other, err := f.export.Nodes().OpenFile("app/src", os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		t.Fatal(err)
	}
	other.Close()
	if !rec.has(src) {
		t.Fatal("a write through another protocol did not recall the NFS handle")
	}

	s, err := f.reg.Enter("app")
	if err != nil {
		t.Fatal(err)
	}
	err = s.FS.Remove("gone")
	s.Done()
	if err != nil {
		t.Fatal(err)
	}
	f.export.Invalidate("app", []string{"gone", "never-handed-out"}, nil)
	if !rec.has(gone) {
		t.Fatal("Invalidate did not recall the changed path")
	}
	_, status, err := c.GetAttr(gone)
	nfsStatus(t, status, nfstest.Stale, err)

	if err := f.reg.Rm(context.Background(), api.RmArgs{Checkout: "app"}); err != nil {
		t.Fatal(err)
	}
	if !rec.has(src) {
		t.Fatal("removing the checkout did not recall its handles")
	}
}

// TestInvalidationRetiresColdHashes covers path and whole-checkout changes.
func TestInvalidationRetiresColdHashes(t *testing.T) {
	for _, whole := range []bool{false, true} {
		t.Run(map[bool]string{true: "checkout", false: "paths"}[whole], func(t *testing.T) {
			f := newStack(t, map[string]string{"f998": "old", "pressure/file": "pressure"})
			f.create(t, "app")
			nodes := f.export.Nodes()
			old := nodes.ID("app/f998")
			f.export.SetNodeCache(0)
			nodes.ID("app/pressure/file")
			session, err := f.reg.Enter("app")
			if err != nil {
				t.Fatal(err)
			}
			err = session.FS.Remove("f998")
			session.Done()
			if err != nil {
				t.Fatal(err)
			}
			paths := []string{"f998"}
			if whole {
				f.export.Invalidate("app", nil, []string{""})
			}
			f.export.Invalidate("app", paths, nil)
			file, err := nodes.Create("app/f4776")
			if err != nil {
				t.Fatal(err)
			}
			file.Close()
			if nodes.ID("app/f4776") == old {
				t.Fatal("replacement reused cold identity")
			}
			if _, ok := nodes.Path(old); ok {
				t.Fatal("retired handle resolves to replacement")
			}
		})
	}
}
