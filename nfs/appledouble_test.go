package nfs

import (
	"bytes"
	"os"
	"testing"
)

// TestV3DropAppleDouble checks that a server dropping AppleDouble sidecars
// answers a macOS client's calls on one without storing it, reports back the
// size the client wrote so its cached pages stay valid, finds no sidecar by
// name, and leaves other names alone; a default server stores the sidecar.
func TestV3DropAppleDouble(t *testing.T) {
	createSidecar := func(t *testing.T, c *rpcClient, dir uint64) (status, uint64) {
		t.Helper()
		st, d := c.v3call(v3Create, func(e *encoder) {
			dirop(e, dir, "._f")
			e.u32(createUnchecked)
			noSattr(e)
		})
		if st != stOK {
			return st, 0
		}
		if !d.bool() {
			t.Fatal("CREATE returned no handle")
		}
		id, _, _ := d.fh(maxFH3)
		return st, id
	}
	sizeOf := func(t *testing.T, c *rpcClient, id uint64) uint64 {
		t.Helper()
		st, d := c.v3call(v3GetAttr, func(e *encoder) { e.fh(id, 0) })
		if st != stOK {
			t.Fatalf("GETATTR: %d", st)
		}
		return fattr3Of(d).size
	}

	t.Run("dropped", func(t *testing.T) {
		fs := newTestFS(t)
		fs.write(t, "f", "x")
		c := dial(t, startServer(t, &Server{FS: fs, DropAppleDouble: true}))
		root := fs.FileID("")

		st, id := createSidecar(t, c, root)
		if st != stOK || id&sidecarBit == 0 {
			t.Fatalf("CREATE ._f: %d, id %#x", st, id)
		}
		st, d := c.v3call(v3Write, func(e *encoder) {
			e.fh(id, 0)
			e.u64(0)
			e.u32(100)
			e.u32(unstable3)
			e.opaque(make([]byte, 100))
		})
		if st != stOK {
			t.Fatalf("WRITE: %d", st)
		}
		if !d.bool() || d.u64() != 0 {
			t.Fatal("WRITE pre-op size is not 0")
		}
		d.u64() // mtime
		d.u64() // ctime
		if post, ok := postOpOf(t, d); !ok || post.size != 100 {
			t.Fatalf("WRITE post-op %+v, want size 100", post)
		}
		if got := sizeOf(t, c, id); got != 100 {
			t.Fatalf("size after WRITE %d, want 100", got)
		}
		st, d = c.v3call(v3Read, func(e *encoder) { e.fh(id, 0); e.u64(0); e.u32(4096) })
		if st != stOK {
			t.Fatalf("READ: %d", st)
		}
		postOpOf(t, d)
		if n, eof, data := d.u32(), d.bool(), d.opaque(maxIO); n != 100 || !eof || !bytes.Equal(data, make([]byte, 100)) {
			t.Fatalf("READ %d bytes, eof %v", n, eof)
		}
		if st, _ := c.v3call(v3SetAttr, func(e *encoder) {
			e.fh(id, 0)
			e.u32(0) // mode
			e.u32(0) // uid
			e.u32(0) // gid
			e.u32(1) // size
			e.u64(0)
			e.u32(0) // atime
			e.u32(0) // mtime
			e.u32(0) // guard
		}); st != stOK {
			t.Fatalf("SETATTR size 0: %d", st)
		}
		if got := sizeOf(t, c, id); got != 0 {
			t.Fatalf("size after truncate %d, want 0", got)
		}
		if st, _ := c.v3call(v3Readlink, func(e *encoder) { e.fh(id, 0) }); st != stStale {
			t.Fatalf("READLINK of a sidecar: %d, want STALE", st)
		}
		if st, _ := c.v3call(v3Lookup, func(e *encoder) { dirop(e, root, "._f") }); st != stNoEnt {
			t.Fatalf("LOOKUP ._f: %d, want NOENT", st)
		}
		if st, _ := c.v3call(v3Rename, func(e *encoder) { dirop(e, root, "._f"); dirop(e, root, "._g") }); st != stOK {
			t.Fatalf("RENAME ._f: %d", st)
		}
		if st, _ := c.v3call(v3Remove, func(e *encoder) { dirop(e, root, "._g") }); st != stOK {
			t.Fatalf("REMOVE ._g: %d", st)
		}
		for _, name := range []string{"._f", "._g"} {
			if _, err := os.Lstat(fs.host(name)); !os.IsNotExist(err) {
				t.Fatalf("%s on the host: %v", name, err)
			}
		}
		if st, _ := c.v3call(v3Lookup, func(e *encoder) { dirop(e, root, "f") }); st != stOK {
			t.Fatalf("LOOKUP f: %d", st)
		}
	})

	t.Run("kept", func(t *testing.T) {
		fs := newTestFS(t)
		c := v3server(t, fs)
		if st, id := createSidecar(t, c, fs.FileID("")); st != stOK || id&sidecarBit != 0 {
			t.Fatalf("CREATE ._f: %d, id %#x", st, id)
		}
		if _, err := os.Lstat(fs.host("._f")); err != nil {
			t.Fatalf("._f not stored: %v", err)
		}
	})
}
