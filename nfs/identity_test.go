package nfs

import (
	"github.com/go-git/go-billy/v5"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
)

// failingIdentityFS models an index fault during FileID, after Lstat worked.
type failingIdentityFS struct {
	*testFS
	failed atomic.Bool
}

func (f *failingIdentityFS) FileID(p string) uint64 {
	if p == "f" {
		f.failed.Store(true)
		return 0
	}
	return f.testFS.FileID(p)
}

func (f *failingIdentityFS) IdentityError() error {
	if f.failed.Load() {
		return syscall.EIO
	}
	return nil
}

// TestIdentityFailureIsIO never publishes zero or a replacement identity when
// a backend's separate identity store fails during a request.
func TestIdentityFailureIsIO(t *testing.T) {
	t.Run("v3", func(t *testing.T) {
		fs := &failingIdentityFS{testFS: newTestFS(t)}
		fs.write(t, "f", "content")
		c := v3server(t, fs)
		st, d := c.v3call(v3Lookup, func(e *encoder) { dirop(e, 1, "f") })
		if st != stIO {
			t.Fatalf("LOOKUP status %d, want IO", st)
		}
		if d.bool() || d.err != nil || len(d.buf) != 0 {
			t.Fatal("malformed LOOKUP failure arm")
		}
	})
	t.Run("v4", func(t *testing.T) {
		fs := &failingIdentityFS{testFS: newTestFS(t)}
		fs.write(t, "f", "content")
		_, addr := v4serve(t, fs, nil)
		c := v4dial(t, addr)
		r := c.run(v4Root(), v4Look("f"))
		r.skip(op4PutRootFH)
		r.next(op4Lookup, stIO)
		r.end()
	})
}

// failingHoldFS fails inside acquisition, after its caller chose a file id.
type failingHoldFS struct {
	*testFS
	failed atomic.Bool
	held   atomic.Int64
}

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
func TestLateHoldFailureReleasesState(t *testing.T) {
	fs := &failingHoldFS{testFS: newTestFS(t)}
	srv, addr := v4serve(t, fs, nil)
	fs.write(t, "f", "content")
	c := v4dial(t, addr)
	c.setup(srv, "late", nil)
	if st, _ := c.open("reader", "f", share4Read, false); st != stIO {
		t.Fatalf("OPEN %d", st)
	}
	srv.v4.mu.Lock()
	holds := len(srv.v4.holds)
	owners := len(srv.v4.owners)
	srv.v4.mu.Unlock()
	if fs.held.Load() != 0 || holds != 0 || owners != 0 {
		t.Fatalf("unpublished state leaked: holds %d owners %d retained %d", holds, owners, fs.held.Load())
	}
}

// allocationFS refuses a fresh id without faulting the rest of the backend.
type allocationFS struct{ *testFS }

func (f allocationFS) FileID(p string) uint64 {
	if p == "f" {
		return 0
	}
	return f.testFS.FileID(p)
}
func TestRefusedAllocationIsIO(t *testing.T) {
	fs := allocationFS{newTestFS(t)}
	fs.write(t, "f", "content")
	c := v3server(t, fs)
	if st, _ := c.v3call(v3Lookup, func(e *encoder) { dirop(e, 1, "f") }); st != stIO {
		t.Fatalf("LOOKUP %d", st)
	}
	_, addr := v4serve(t, fs, nil)
	v4 := v4dial(t, addr)
	if st := v4.run(v4Root(), v4Look("f"), v4GetFH()).status; st != stIO {
		t.Fatalf("LOOKUP %d", st)
	}
}

// refusedSpecialFS commits the creation but refuses its result identity,
// without poisoning existing identities through IdentityError.
type refusedSpecialFS struct{ allocationFS }

func (f refusedSpecialFS) Mknod(name string, mode os.FileMode) error {
	file, err := f.testFS.Create(name)
	if err != nil {
		return err
	}
	return file.Close()
}

func TestSpecialCreationRefusedIdentity(t *testing.T) {
	for _, tc := range []struct {
		name string
		typ  fileType
	}{{"socket", typeSock}, {"fifo", typeFifo}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("v3", func(t *testing.T) {
				fs := refusedSpecialFS{allocationFS{newTestFS(t)}}
				c := v3server(t, fs)
				st, d := c.v3call(v3Mknod, func(e *encoder) {
					dirop(e, 1, "f")
					e.u32(uint32(tc.typ))
					noSattr(e)
				})
				if st != stIO {
					t.Fatalf("MKNOD status %d, want IO", st)
				}
				// MKNOD3resfail contains wcc_data: absent pre-op and
				// post-op attributes, with no success-arm bytes left over.
				if d.bool() || d.bool() || d.err != nil || len(d.buf) != 0 {
					t.Fatal("malformed MKNOD failure arm")
				}
				if _, err := fs.Lstat("f"); err != nil {
					t.Fatal("backend creation did not commit:", err)
				}
			})
			t.Run("v4", func(t *testing.T) {
				fs := refusedSpecialFS{allocationFS{newTestFS(t)}}
				_, addr := v4serve(t, fs, nil)
				c := v4dial(t, addr)
				r := c.run(v4Root(), v4Create(tc.typ, "f", "", v4fattr{}), v4GetFH())
				r.skip(op4PutRootFH)
				r.next(op4Create, stIO)
				r.end()
				if _, err := fs.Lstat("f"); err != nil {
					t.Fatal("backend creation did not commit:", err)
				}
			})
		})
	}
}

type retainedFaultFile struct {
	billy.File
	closes atomic.Int64
}

func (f *retainedFaultFile) Close() error { f.closes.Add(1); return f.File.Close() }

type refusedCreateFS struct {
	*testFS
	file *retainedFaultFile
}

func (f *refusedCreateFS) FileID(p string) uint64 {
	if p == "readonly" {
		return 0
	}
	return f.testFS.FileID(p)
}
func (f *refusedCreateFS) OpenFile(p string, flag int, perm os.FileMode) (billy.File, error) {
	file, err := f.testFS.OpenFile(p, flag, perm)
	if err != nil {
		return nil, err
	}
	f.file = &retainedFaultFile{File: file}
	return f.file, nil
}
func TestRefusedCreateOpenClosesRetainedFile(t *testing.T) {
	fs := &refusedCreateFS{testFS: newTestFS(t)}
	srv, addr := v4serve(t, fs, nil)
	c := v4dial(t, addr)
	c.setup(srv, "refused-create", nil)
	r := c.run(v4Root(), v4Open(v4openArgs{owner: c.owner("writer"), access: share4AccessAll, create: true, name: "readonly", attrs: v4Mode(0400)}))
	if r.status != stIO {
		t.Fatalf("OPEN: %d", r.status)
	}
	if fs.file == nil || fs.file.closes.Load() != 1 {
		t.Fatal("refused OPEN did not close retained file once")
	}
	srv.v4.mu.Lock()
	defer srv.v4.mu.Unlock()
	if len(srv.v4.holds) != 0 || len(srv.v4.files) != 0 {
		t.Fatal("refused OPEN published state")
	}
}

// acquisitionFaultFS faults a selected hold after retaining it, and counts
// creation descriptors so an unpublished OPEN cannot leak either resource.
type acquisitionFaultFS struct {
	*testFS
	calls, held atomic.Int64
	failAt      int64
	failed      atomic.Bool
	file        *retainedFaultFile
}

func (f *acquisitionFaultFS) HoldFileID(uint64) func() {
	f.held.Add(1)
	if f.calls.Add(1) == f.failAt {
		f.failed.Store(true)
	}
	return func() { f.held.Add(-1) }
}
func (f *acquisitionFaultFS) IdentityError() error {
	if f.failed.Load() {
		return syscall.EIO
	}
	return nil
}
func (f *acquisitionFaultFS) OpenFile(p string, flag int, perm os.FileMode) (billy.File, error) {
	file, err := f.testFS.OpenFile(p, flag, perm)
	if err != nil {
		return nil, err
	}
	f.file = &retainedFaultFile{File: file}
	return f.file, nil
}
func TestOpenAcquisitionRollback(t *testing.T) {
	for _, tc := range []struct {
		name             string
		fail             int64
		create, previous bool
	}{
		{"first", 1, false, false}, {"delegation", 2, false, false},
		{"creation", 1, true, false}, {"additional", 2, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := &acquisitionFaultFS{testFS: newTestFS(t), failAt: tc.fail}
			fs.cacheable = func(string) bool { return true }
			srv, addr := v4serve(t, fs, nil)
			c, _ := v4holder(t, srv, addr, tc.name)
			var prior v4opened
			if !tc.create && !tc.previous {
				fs.write(t, "f", "content")
			}
			if tc.previous {
				r := c.run(v4Root(), v4Open(v4openArgs{owner: c.owner("owner"), access: share4Write, create: true, name: "f", attrs: v4Mode(0400)}))
				r.skip(op4PutRootFH)
				prior = v4getOpened(t, r.next(op4Open, stOK))
				r.end()
			}
			access := uint32(share4Read)
			if tc.create {
				access = share4AccessAll
			}
			r := c.run(v4Root(), v4Open(v4openArgs{owner: c.owner("owner"), access: access, create: tc.create, name: "f", attrs: v4Mode(0400)}))
			if r.status != stIO {
				t.Fatalf("OPEN: %d", r.status)
			}
			srv.v4.mu.Lock()
			holds, owners, files := len(srv.v4.holds), len(srv.v4.owners), len(srv.v4.files)
			if tc.previous {
				h := srv.v4.holds[prior.id.other]
				if h == nil || h.id != prior.id || h.access != share4Write {
					t.Error("failed additional OPEN changed previous state")
				}
			}
			grants := len(srv.v4.granting)
			srv.v4.mu.Unlock()
			want := 0
			if tc.previous {
				want = 1
			}
			if holds != want || owners != want || files != want || fs.held.Load() != int64(want) || grants != 0 {
				t.Fatalf("unpublished state: holds=%d owners=%d files=%d retained=%d grants=%d", holds, owners, files, fs.held.Load(), grants)
			}
			if tc.create && (fs.file == nil || fs.file.closes.Load() != 1) {
				t.Fatal("creation descriptor was not closed once")
			}
			if tc.previous {
				if fs.file == nil || fs.file.closes.Load() != 0 {
					t.Fatal("failed additional OPEN closed previous retained file")
				}
				fs.failed.Store(false)
				r := c.run(v4Root(), v4Look("f"), v4Close(prior.id))
				r.skip(op4PutRootFH)
				r.skip(op4Lookup)
				getStateID4(r.next(op4Close, stOK))
				r.end()
				if fs.held.Load() != 0 {
					t.Fatal("previous state could not be closed")
				}
				if fs.file.closes.Load() != 1 {
					t.Fatal("previous retained file did not close once")
				}
			}
		})
	}
}
