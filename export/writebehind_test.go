//go:build !windows

package export_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/internal/nfstest"
	"github.com/stencil-hq/ovnfs/nfs"
)

// cacheBackend is the real export, including its optional NFS cache hook.
type cacheBackend interface {
	nfs.Backend
	SetWriteBarrier(func(context.Context) (func(), error))
}

// heldExport pauses a backing write before it enters the checkout freeze lock.
type heldExport struct {
	cacheBackend
	entered chan struct{}
	allow   chan struct{}
	barrier chan struct{}
}

// SetWriteBarrier records the observer's arrival at the real registry gate.
func (e *heldExport) SetWriteBarrier(fn func(context.Context) (func(), error)) {
	e.cacheBackend.SetWriteBarrier(func(ctx context.Context) (func(), error) {
		select {
		case e.barrier <- struct{}{}:
		default:
		}
		return fn(ctx)
	})
}

// OpenFile preserves read/stat behavior and gates writable descriptors only.
func (e *heldExport) OpenFile(p string, flags int, mode os.FileMode) (billy.File, error) {
	f, err := e.cacheBackend.OpenFile(p, flags, mode)
	if err != nil || flags&(os.O_WRONLY|os.O_RDWR) == 0 {
		return f, err
	}
	return &heldExportFile{File: f, fs: e}, nil
}

// heldExportFile pins a real export descriptor while the writer is stopped.
type heldExportFile struct {
	billy.File
	fs *heldExport
}

// Stat keeps attribute seeding on the real descriptor.
func (f *heldExportFile) Stat() (os.FileInfo, error) {
	return f.File.(interface{ Stat() (os.FileInfo, error) }).Stat()
}

// WriteAt waits outside the real operation so the barrier, not the checkout
// freeze lock, must order the direct reader after the acknowledged write.
func (f *heldExportFile) WriteAt(data []byte, off int64) (int, error) {
	f.fs.entered <- struct{}{}
	<-f.fs.allow
	return f.File.(io.WriterAt).WriteAt(data, off)
}

// TestNFSWriteBehindObservers covers both registry-level quiesce and the
// retained descriptor path used by other frontends on the very same export.
func TestNFSWriteBehindObservers(t *testing.T) {
	for _, observer := range []string{"diff", "smb", "nodes"} {
		t.Run(observer, func(t *testing.T) {
			stack := newStack(t, map[string]string{"source": "base\n"})
			stack.create(t, "co")
			e := &heldExport{cacheBackend: stack.export.NFS().(cacheBackend), entered: make(chan struct{}, 1), allow: make(chan struct{}), barrier: make(chan struct{}, 1)}
			c := nfstest.Serve(t, &nfs.Server{FS: e, NoSync: true})
			resume := sync.OnceFunc(func() { close(e.allow) })
			defer resume()
			fh := lookup(t, c, "co/source")
			// Keep the external reader open before the NFS write; gating just
			// export.Open would not protect reads on an existing descriptor.
			var reader billy.File
			if observer != "diff" {
				var fs billy.Filesystem = stack.export.Nodes()
				if observer == "smb" {
					fs = stack.export.SMB()
				}
				var err error
				// A retained reader must refer to the upper being written, not the
				// lower inode left untouched by copy-up.
				reader, err = fs.OpenFile("co/source", os.O_RDWR, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer reader.Close()
			}
			nfsWrite(t, c, fh, "after\n")
			select {
			case <-e.entered:
			case <-time.After(2 * time.Second):
				t.Fatal("no backing write")
			}
			select {
			case <-e.barrier:
			default:
			}
			done := make(chan error, 1)
			go func() {
				if observer == "diff" {
					r, err := stack.reg.Diff(t.Context(), api.DiffArgs{Checkout: "co", Patch: true})
					if err == nil && !strings.Contains(r.Patch, "+after") {
						err = fmt.Errorf("diff missed WRITE: %q", r.Patch)
					}
					done <- err
				} else {
					b, err := io.ReadAll(reader)
					if err == nil && string(b) != "after\n" {
						err = fmt.Errorf("read %q", b)
					}
					done <- err
				}
			}()
			select {
			case <-e.barrier:
			case err := <-done:
				t.Fatalf("%s bypassed write barrier: %v", observer, err)
			case <-time.After(2 * time.Second):
				t.Fatal("observer never reached barrier")
			}
			resume()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}
