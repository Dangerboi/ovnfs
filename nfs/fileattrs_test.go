//go:build !windows

package nfs_test

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/checkout"
	"github.com/stencil-hq/ovnfs/export"
	"github.com/stencil-hq/ovnfs/nfs"
	"github.com/stencil-hq/ovnfs/overlay"
)

// TestFileAttrs checks WRITE attributes through real retained export descriptors.
func TestFileAttrs(t *testing.T) {
	for _, tier := range []string{"disk", "memory"} {
		t.Run(tier, func(t *testing.T) {
			var memory *overlay.Memory
			if tier == "memory" {
				memory = overlay.NewMemory(1<<20, 64<<10, 0)
			}
			lower, state := t.TempDir(), t.TempDir()
			if err := os.WriteFile(filepath.Join(lower, "base"), []byte("base"), 0640); err != nil {
				t.Fatal(err)
			}
			reg, err := checkout.Open(checkout.Config{State: state, Mount: t.TempDir(), Memory: memory})
			if err != nil {
				t.Fatal(err)
			}
			defer reg.Close()
			if _, err := reg.New(t.Context(), api.NewArgs{Dir: lower, Name: "parent"}); err != nil {
				t.Fatal(err)
			}
			fs := export.New(export.NewRouter(reg), state).NFS()
			for _, name := range []string{"base", "created"} {
				t.Run(name, func(t *testing.T) {
					p := "parent/" + name
					flags := os.O_WRONLY
					id := fs.FileID(p)
					if name == "created" {
						flags |= os.O_CREATE | os.O_EXCL
					}
					f, err := fs.OpenFile(p, flags, 0640)
					if err != nil {
						t.Fatal(err)
					}
					defer f.Close()
					if name == "created" {
						id = fs.FileID(p)
					}
					nfs.CheckFileAttrs(t, fs, id, p, f)
					grow := func(off int64) {
						t.Helper()
						if _, err := f.(io.WriterAt).WriteAt([]byte("grown"), off); err != nil {
							t.Fatal(err)
						}
						nfs.CheckFileAttrs(t, fs, id, p, f)
					}
					grow(4096)
					if _, err := reg.Fork(t.Context(), api.ForkArgs{Checkout: "parent", Name: "child-" + name}); err != nil {
						t.Fatal(err)
					}
					grow(8192)
					if err := fs.Link(p, p+"-alias"); err != nil {
						t.Fatal(err)
					}
					grow(16384)
					if _, err := reg.Fork(t.Context(), api.ForkArgs{Checkout: "parent", Name: "linked-" + name}); err != nil {
						t.Fatal(err)
					}
					if _, err := f.(io.WriterAt).WriteAt([]byte("wrong inode"), 0); !errors.Is(err, syscall.ESTALE) {
						t.Fatalf("write after hard-link identity split: %v, want ESTALE", err)
					}
				})
			}
		})
	}
}
