package nfs

import (
	"bytes"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/stencil-hq/ovnfs/internal/kept"
)

// deferredTestFS gates each host write independently of the RPC answering it.
type deferredTestFS struct {
	*testFS
	entered chan string
	allow   chan struct{}
	failure atomic.Int32
}

// OpenFile preserves file Stat and isolates the data-application phase for observation.
func (fs *deferredTestFS) OpenFile(p string, flag int, perm os.FileMode) (billy.File, error) {
	f, err := fs.testFS.OpenFile(p, flag, perm)
	if err != nil || flag&(os.O_WRONLY|os.O_RDWR) == 0 {
		return f, err
	}
	return &deferredTestFile{File: f, fs: fs, path: p}, nil
}

// deferredTestFile exposes the host IO that the queue defers.
type deferredTestFile struct {
	billy.File
	fs   *deferredTestFS
	path string
}

// Stat describes the original host inode.
func (f *deferredTestFile) Stat() (os.FileInfo, error) { return os.Stat(f.fs.host(f.path)) }

// WriteAt records the exact payload after the RPC buffer is free for reuse.
func (f *deferredTestFile) WriteAt(data []byte, off int64) (int, error) {
	f.fs.entered <- string(data)
	<-f.fs.allow
	if failure := f.fs.failure.Swap(0); failure != 0 {
		if syscall.Errno(failure) == syscall.ESTALE {
			if err := f.fs.Rename(f.path, "relocated"); err != nil {
				return 0, err
			}
		}
		return 0, syscall.Errno(failure)
	}
	return kept.WriteAt(f.File, data, off)
}

// deferredFixture releases all blocked workers even if an assertion fails.
func deferredFixture(t *testing.T) (*deferredTestFS, func()) {
	fs := &deferredTestFS{testFS: newTestFS(t), entered: make(chan string, 16), allow: make(chan struct{})}
	release := sync.OnceFunc(func() { close(fs.allow) })
	t.Cleanup(release)
	return fs, release
}

// applied waits for one write to reach the backend, with a bounded failure path.
func (fs *deferredTestFS) applied(t *testing.T) string {
	t.Helper()
	select {
	case p := <-fs.entered:
		return p
	case <-time.After(2 * time.Second):
		t.Fatal("write never reached backend")
		return ""
	}
}

// rpcWriteAttrs checks the complete WCC chain instead of just the byte count.
func rpcWriteAttrs(t *testing.T, c *rpcClient, id, off uint64, data string, prev wireAttr) wireAttr {
	t.Helper()
	st, d := c.v3call(v3Write, writeArgs(id, off, []byte(data)))
	if st != stOK || !d.bool() {
		t.Fatalf("WRITE: %v, missing WCC", st)
	}
	size := d.u64()
	mtime := [2]uint32{d.u32(), d.u32()}
	ctime := [2]uint32{d.u32(), d.u32()}
	post, ok := postOpOf(t, d)
	if prev.size != size || prev.mtime != mtime || prev.ctime != ctime || !ok || d.u32() != uint32(len(data)) {
		t.Fatalf("WRITE pre %d %v %v, expected %+v; post %+v", size, mtime, ctime, prev, post)
	}
	return post
}

// TestWriteBehindVisibility verifies a second connection cannot read a stale or
// intermediate version and an evicted writer stays open until both batches finish.
func TestWriteBehindVisibility(t *testing.T) {
	fs, release := deferredFixture(t)
	fs.write(t, "f", "")
	s := &Server{FS: fs, NoSync: true, Concurrency: 8}
	addr := startServer(t, s)
	writer, observer, reader := dial(t, addr), dial(t, addr), dial(t, addr)
	_ = writer.nc.SetReadDeadline(time.Now().Add(4 * time.Second))
	id := fs.FileID("f")
	fh := func(e *encoder) { e.fh(id, 0) }
	st, d := writer.v3call(v3GetAttr, fh)
	if st != stOK {
		t.Fatal(st)
	}
	post := rpcWriteAttrs(t, writer, id, 1, "a", fattr3Of(d))
	if got := fs.applied(t); got != "a" {
		t.Fatal(got)
	}
	post = rpcWriteAttrs(t, writer, id, 2, "b", post)
	post = rpcWriteAttrs(t, writer, id, 3, "cd", post)
	s.v3.kept.Close()
	observer.send(1, v3GetAttr, fh)
	reader.send(1, v3Read, func(e *encoder) { fh(e); e.u64(0); e.u32(8) })
	if xid, ok := reader.replyXID(10 * time.Millisecond); ok {
		t.Fatalf("READ %d saw an unapplied write", xid)
	}
	if xid, ok := observer.replyXID(10 * time.Millisecond); ok {
		t.Fatalf("GETATTR %d saw an unapplied write", xid)
	}
	release()
	got := fattr3Of(observer.replies(1)[1])
	if got != post {
		t.Fatalf("GETATTR %+v, WRITE predicted %+v", got, post)
	}
	if data := fs.applied(t); data != "bcd" {
		t.Fatalf("adjacent writes arrived as %q", data)
	}
	read := reader.replies(1)[1]
	if a, ok := postOpOf(t, read); !ok || a != post {
		t.Fatalf("READ attrs %+v, want %+v", a, post)
	}
	if n, eof, data := read.u32(), read.bool(), read.opaque(8); n != 5 || !eof || string(data) != "\x00abcd" {
		t.Fatalf("READ %d %q eof %t", n, data, eof)
	}
}

// TestWriteBehindFailure checks that acknowledged errors are logged, kept
// through an external barrier, and consumed exactly once by WRITE or COMMIT.
func TestWriteBehindFailure(t *testing.T) {
	for _, op := range []string{"write", "commit", "remove4"} {
		t.Run(op, func(t *testing.T) {
			fs, release := deferredFixture(t)
			release()
			fs.write(t, "f", "")
			logged := make(chan struct{}, 1)
			s := &Server{FS: fs, NoSync: true, Logf: func(string, ...any) { logged <- struct{}{} }}
			addr := startServer(t, s)
			c := dial(t, addr)
			id := fs.FileID("f")
			fs.failure.Store(int32(syscall.ENOSPC))
			if st, _, _, _ := c.write3(id, 0, fileSync3, []byte("lost")); st != stOK {
				t.Fatal(st)
			}
			select {
			case <-logged:
			case <-time.After(2 * time.Second):
				t.Fatal("late error not logged")
			}
			if resume, err := s.Quiesce(t.Context()); err == nil {
				resume()
				t.Fatal("barrier published incomplete data")
			}
			if st, _ := c.v3call(v3GetAttr, func(e *encoder) { e.fh(id, 0) }); st != stNoSpc {
				t.Fatalf("GETATTR on incomplete data: %v", st)
			}
			if op == "remove4" {
				if st := v4dial(t, addr).run(v4Root(), v4Remove("f")).status; st != stOK {
					t.Fatal(st)
				}
				resume, err := s.Quiesce(t.Context())
				if err != nil {
					t.Fatalf("undeliverable fault after NFSv4 REMOVE: %v", err)
				}
				resume()
				return
			}
			var got status
			if op == "write" {
				got, _, _, _ = c.write3(id, 0, fileSync3, []byte("not applied"))
			} else {
				got, _ = c.commit3(id)
			}
			if got != stNoSpc {
				t.Fatalf("late status = %v", got)
			}
			if st, _, _, _ := c.write3(id, 0, fileSync3, []byte("restored")); st != stOK {
				t.Fatalf("error reported twice: %v", st)
			}
			if st, _ := c.commit3(id); st != stOK {
				t.Fatalf("commit after recovery: %v", st)
			}
		})
	}
}

// TestWriteBehindCap charges in-flight writes of every file to one budget.
func TestWriteBehindCap(t *testing.T) {
	fs, release := deferredFixture(t)
	fs.write(t, "a", "")
	fs.write(t, "b", "")
	s := &Server{FS: fs, NoSync: true}
	s.v3.writes.limit = writeChunk
	addr := startServer(t, s)
	first, second := dial(t, addr), dial(t, addr)
	if st, _, _, _ := first.write3(fs.FileID("a"), 0, fileSync3, []byte("queued")); st != stOK {
		t.Fatal(st)
	}
	if got := fs.applied(t); got != "queued" {
		t.Fatal(got)
	}
	second.send(1, v3Write, writeArgs(fs.FileID("b"), 0, []byte("synchronous")))
	if got := fs.applied(t); got != "synchronous" {
		t.Fatal(got)
	}
	if xid, ok := second.replyXID(10 * time.Millisecond); ok {
		t.Fatalf("over-budget WRITE %d replied before applying", xid)
	}
	release()
	second.replies(1)
	if st, _ := first.commit3(fs.FileID("a")); st != stOK {
		t.Fatal(st)
	}
}

// TestWriteBehindReopensStaleWriter retries a generation-aware descriptor
// that loses its identity at apply, both in the caller and after the reply.
func TestWriteBehindReopensStaleWriter(t *testing.T) {
	for _, deferWrite := range []bool{false, true} {
		fs, release := deferredFixture(t)
		release()
		fs.write(t, "a", "")
		s := &Server{FS: fs, NoSync: deferWrite}
		c := dial(t, startServer(t, s))
		id := fs.FileID("a")
		fs.failure.Store(int32(syscall.ESTALE))
		if st, _, _, _ := c.write3(id, 0, fileSync3, []byte("retried")); st != stOK {
			t.Fatal(st)
		}
		if st, _ := c.commit3(id); st != stOK {
			t.Fatal(st)
		}
		if data, err := os.ReadFile(fs.host("relocated")); err != nil || string(data) != "retried" {
			t.Fatalf("contents %q, %v", data, err)
		}
	}
}

// BenchmarkWrite3 measures the RPC procedure alone, at random aligned offsets
// in a pre-sized file, including the release of reply-owned data after send.
func BenchmarkWrite3(b *testing.B) {
	for _, mode := range []string{"apply", "defer"} {
		b.Run(mode, func(b *testing.B) {
			fs := newTestFS(b)
			f, err := os.Create(fs.host("f"))
			if err != nil {
				b.Fatal(err)
			}
			if err = f.Truncate(32 << 20); err != nil {
				b.Fatal(err)
			}
			f.Close()
			s := &Server{FS: fs, NoSync: mode == "defer"}
			s.start()
			defer closeKept(s)
			id := fs.FileID("f")
			var arg, reply encoder
			payload := bytes.Repeat([]byte{'x'}, 4096)
			writeArgs(id, 0, payload)(&arg)
			arg.put32(4+fhSize+12, unstable3) // neither path fsyncs
			x := uint32(17)
			b.ReportAllocs()
			for b.Loop() {
				x = 1664525*x + 1013904223
				arg.put32(4+fhSize+4, (x>>19)<<12)
				d := decoder{buf: arg.buf}
				reply.buf = reply.buf[:0]
				if st := s.write3(&d, &reply); st != stOK {
					b.Fatal(st)
				}
				if reply.pending != nil {
					s.answered(reply.pending)
					reply.pending = nil
				}
			}
			if s.NoSync {
				resume, err := s.Quiesce(b.Context())
				if err != nil {
					b.Fatal(err)
				}
				resume()
			}
		})
	}
}
