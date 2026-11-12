//go:build !windows

package checkout

import (
	"context"
	"os"
	"testing"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/store"
)

// TestSpecialNodesStayOutOfTrees checks a socket or FIFO made in a checkout
// is left out of its snapshot and diff, and hides the file it replaced.
func TestSpecialNodesStayOutOfTrees(t *testing.T) {
	f := newFixture(t)
	f.newCheckout(folder(t, map[string]string{"x": "1", "gone": "2"}), "c")
	var s Session
	if err := f.reg.Enter("c", &s); err != nil {
		t.Fatal(err)
	}
	if err := s.FS.Remove("gone"); err != nil {
		t.Fatal(err)
	}
	for p, mode := range map[string]os.FileMode{"sock": os.ModeSocket | 0o755, "fifo": os.ModeNamedPipe | 0o644, "gone": os.ModeNamedPipe | 0o644} {
		if err := s.FS.Mknod(p, mode); err != nil {
			t.Fatal(err)
		}
	}
	s.Done()
	diff, err := f.reg.Diff(context.Background(), api.DiffArgs{Checkout: "c"})
	if err != nil {
		t.Fatal(err)
	}
	if len(diff.Changes) != 1 || diff.Changes[0] != (api.Change{Path: "gone", Status: "D"}) {
		t.Fatalf("diff %v, want only gone deleted", diff.Changes)
	}
	reply, err := f.reg.Snap(context.Background(), api.SnapArgs{Checkout: "c", Wait: true})
	if err != nil {
		t.Fatal(err)
	}
	files, err := f.reg.store.Flatten(store.OID(reply.Snapshot.Tree))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := files["x"]; !ok || len(files) != 1 {
		t.Fatalf("snapshot leaves %v, want only x", files)
	}
}
