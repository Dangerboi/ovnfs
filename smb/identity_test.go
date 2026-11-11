package smb

import (
	"sync/atomic"
	"syscall"
	"testing"
)

type failingHoldFS struct {
	Backend
	held   atomic.Int64
	failed atomic.Bool
}

func (f *failingHoldFS) FileID(string) uint64 { return 123 }
func (f *failingHoldFS) HoldFileID(uint64) func() {
	f.held.Add(1)
	f.failed.Store(true)
	return func() { f.held.Add(-1) }
}
func (f *failingHoldFS) IdentityError() error {
	if f.failed.Load() {
		return syscall.EIO
	}
	return nil
}
func TestLateHoldFailureDoesNotPublishOpen(t *testing.T) {
	fixture := newLeaseStateFixture(t)
	fs := &failingHoldFS{Backend: fixture.c.srv.FS}
	fixture.c.srv.FS = fs
	o := &open{path: "file"}
	r := &request{c: fixture.c, tree: fixture.tree}
	if publishOpen(r, o) {
		t.Fatal("published failed hold")
	}
	if fs.held.Load() != 0 || len(fixture.tree.files) != 0 {
		t.Fatal("failed hold leaked open")
	}
	if o.closeFn != nil {
		o.closeFn()
	}
	if fs.held.Load() != 0 {
		t.Fatal("teardown released hold twice")
	}
}
