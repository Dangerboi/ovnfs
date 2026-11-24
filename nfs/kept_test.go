package nfs

import (
	"bytes"
	"os"
	"path"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5"
)

// seekWriteFS exposes only billy's sequential file API to the NFS writer.
type seekWriteFS struct{ *testFS }

// OpenFile hides WriteAt so cached writes must serialize Seek with Write.
func (f *seekWriteFS) OpenFile(p string, flag int, perm os.FileMode) (billy.File, error) {
	file, err := f.testFS.OpenFile(p, flag, perm)
	if err != nil {
		return nil, err
	}
	return seekWriteFile{File: file}, nil
}

type seekWriteFile struct{ billy.File }

// Seek yields between positioning and writing to expose shared-offset races.
func (f seekWriteFile) Seek(off int64, whence int) (int64, error) {
	pos, err := f.File.Seek(off, whence)
	runtime.Gosched()
	return pos, err
}

func TestV3CachedWritesPreserveOffsets(t *testing.T) {
	fs := &seekWriteFS{newTestFS(t)}
	fs.write(t, "file", "")
	s := &Server{FS: fs}
	t.Cleanup(func() { closeKept(s) })
	id := fs.FileID("file")
	const workers, block = 16, 64
	want := make([]byte, workers*block)
	if _, err := s.writeAt3(id, 0, "file", want, 0, false); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range workers {
		data := bytes.Repeat([]byte{byte(i + 1)}, block)
		copy(want[i*block:], data)
		wg.Go(func() {
			for range 32 {
				n, err := s.writeAt3(id, 0, "file", data, int64(i*block), false)
				if err != nil || n != len(data) {
					t.Errorf("write at %d: %d bytes, %v", i*block, n, err)
					return
				}
			}
		})
	}
	wg.Wait()
	got, err := os.ReadFile(fs.host("file"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("concurrent writes corrupted file offsets: got %x, want %x", got, want)
	}
}

// countingReadFS counts OpenFile and Lstat calls on top of testFS and can gate
// an open to exercise open-vs-invalidation races.
type countingReadFS struct {
	*testFS
	opens    atomic.Int64
	lstats   atomic.Int64
	gatePath string
	opening  chan struct{}
	release  chan struct{}
	gateOnce sync.Once
}

func (f *countingReadFS) OpenFile(p string, flag int, perm os.FileMode) (billy.File, error) {
	f.opens.Add(1)
	file, err := f.testFS.OpenFile(p, flag, perm)
	if err == nil && f.gatePath != "" && p == f.gatePath {
		f.gateOnce.Do(func() { close(f.opening) })
		<-f.release
	}
	return file, err
}

func (f *countingReadFS) Lstat(p string) (os.FileInfo, error) {
	f.lstats.Add(1)
	return f.testFS.Lstat(p)
}

// TestV3CachedReadsReuseAndInvalidate verifies that sequential NFSv3 READs of
// a cacheable file reuse one open descriptor, that writes/recalls invalidate
// it, and that an open racing with a recall does not cache a stale descriptor.
func TestV3CachedReadsReuseAndInvalidate(t *testing.T) {
	base := newTestFS(t)
	base.cacheable = func(string) bool { return true }
	payload := bytes.Repeat([]byte("abcdefgh"), 1024)
	base.write(t, "big", string(payload))
	fs := &countingReadFS{testFS: base}
	srv := &Server{FS: fs, Concurrency: 8}
	addr := startServer(t, srv)
	c := dial(t, addr)

	id := base.FileID("big")
	for off := uint64(0); off < uint64(len(payload)); off += 512 {
		st, data, _ := c.read3(id, off, 512)
		if st != stOK || !bytes.Equal(data, payload[off:off+512]) {
			t.Fatalf("read at %d: st=%d len=%d", off, st, len(data))
		}
	}
	if opens := fs.opens.Load(); opens != 1 {
		t.Fatalf("sequential READs opened file %d times, want 1", opens)
	}

	// Overwrite via NFSv3 WRITE: cached reader must be dropped and new data read.
	if stat, d := c.call(progNFS, 3, v3Write, writeArgs(id, 0, []byte("ZZZZZZZZ"))); stat != rpcSuccess || status(d.u32()) != stOK {
		t.Fatal("WRITE failed")
	}
	st, data, _ := c.read3(id, 0, 8)
	if st != stOK || string(data) != "ZZZZZZZZ" {
		t.Fatalf("read after WRITE: st=%d data=%q, want ZZZZZZZZ", st, data)
	}

	// Race: an open that began before a recall must not be cached afterwards.
	base.write(t, "raced", "old-bytes")
	racedID := base.FileID("raced")
	fs.gatePath = "raced"
	fs.opening = make(chan struct{})
	fs.release = make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		c2 := dial(t, addr)
		st, data, _ := c2.read3(racedID, 0, 9)
		if st != stOK || (runtime.GOOS != "windows" && string(data) != "old-bytes") {
			t.Errorf("gated read: st=%d data=%q", st, data)
		}
	}()
	select {
	case <-fs.opening:
	case <-time.After(2 * time.Second):
		t.Fatal("gated open did not start")
	}
	// Replace the file's content and recall while the gated open still holds
	// the pre-mutation descriptor. On Unix unlink first so the open FD stays
	// pinned to the old bytes.
	if runtime.GOOS != "windows" {
		if err := os.Remove(base.host("raced")); err != nil {
			t.Fatal(err)
		}
	}
	base.write(t, "raced", "new-bytes")
	srv.Recall(racedID)
	fs.gatePath = ""
	close(fs.release)
	<-done

	if srv.v3.readers.cache.Has(racedID) {
		t.Fatal("open that raced with Recall was cached")
	}
	st, data, _ = c.read3(racedID, 0, 9)
	if st != stOK || string(data) != "new-bytes" {
		t.Fatalf("read after raced Recall: st=%d data=%q, want new-bytes", st, data)
	}
}

// TestV3MaxReportedSizesReclaimedOnRetireAndRemove checks that retiring a
// checkout slot range drops its size table in O(1) so slot reuse does not
// falsely detect a size regression, and that removing a single-link file
// forgets its size entry.
func TestV3MaxReportedSizesReclaimedOnRetireAndRemove(t *testing.T) {
	const mask = uint64(1)<<sizeBucketBits - 1
	slot1First := uint64(1) << sizeBucketBits
	slot1Last := slot1First | mask
	slot2ID := (uint64(2) << sizeBucketBits) | 7
	id1 := slot1First | 42

	var m maxReportedSizes
	var client clientReportedSizes
	if got := client.check(&m, id1, 1000, m.clock.Load()); got != sizeCurrent {
		t.Fatalf("record id1: %v", got)
	}
	if got := client.check(&m, slot2ID, 2000, m.clock.Load()); got != sizeCurrent {
		t.Fatalf("record slot2ID: %v", got)
	}
	// Simulate client-requested shrink on slot 1, then retire slot 1.
	client.finish(&m, id1, 500, true)
	m.retireRange(slot1First, slot1Last)
	client.retireRange(slot1First, slot1Last)

	m.mu.Lock()
	_, slot1Still := m.buckets[1]
	_, slot2Still := m.buckets[2]
	m.mu.Unlock()
	if slot1Still {
		t.Fatal("slot 1 bucket survived retireRange")
	}
	if !slot2Still {
		t.Fatal("slot 2 bucket was dropped by slot 1 retireRange")
	}
	// Reusing id1 in a new checkout with a smaller file must be accepted.
	if got := client.check(&m, id1, 10, m.clock.Load()); got != sizeCurrent {
		t.Fatalf("reused id1 with smaller size returned %v, want sizeCurrent", got)
	}

	// Removing a single-link file via NFSv3 REMOVE must forget its size entry.
	fs := newTestFS(t)
	if err := os.MkdirAll(fs.host("d"), 0o755); err != nil {
		t.Fatal(err)
	}
	fs.write(t, "d/tmp", "hello world")
	srv := &Server{FS: fs}
	addr := startServer(t, srv)
	c := dial(t, addr)
	dirID := fs.FileID("d")
	tmpID := fs.FileID("d/tmp")
	if st, _, _ := c.read3(tmpID, 0, 16); st != stOK {
		t.Fatalf("read tmp: %d", st)
	}
	srv.v3.maxSizes.mu.Lock()
	recorded := srv.v3.maxSizes.load(tmpID).size
	srv.v3.maxSizes.mu.Unlock()
	if recorded != 11 {
		t.Fatalf("recorded size = %d, want 11", recorded)
	}
	stat, d := c.call(progNFS, 3, v3Remove, func(e *encoder) {
		e.fh(dirID, 0)
		e.str("tmp")
	})
	if stat != rpcSuccess || status(d.u32()) != stOK {
		t.Fatal("REMOVE failed")
	}
	srv.v3.maxSizes.mu.Lock()
	after := srv.v3.maxSizes.load(tmpID).size
	srv.v3.maxSizes.mu.Unlock()
	if after != 0 {
		t.Fatalf("size entry survived REMOVE: %d", after)
	}
}

// TestV3ReaddirPlusScopedInvalidation checks that namespace changes in one
// directory do not force READDIRPLUS continuations of another directory to
// Lstat every remaining entry, while changes in the same directory still do.
func TestV3ReaddirPlusScopedInvalidation(t *testing.T) {
	base := newTestFS(t)
	base.cacheable = func(string) bool { return true }
	files := dirFixture(t, base, 30)
	if err := os.MkdirAll(base.host("other"), 0o755); err != nil {
		t.Fatal(err)
	}
	base.write(t, "other/keep", "x")
	fs := &countingReadFS{testFS: base}
	s := &Server{FS: fs}
	call := direct(s)
	dirID := base.FileID("d")
	otherID := base.FileID("other")

	first := readdir3(t, call, true, dirID, 0, 0, 4096, 600)
	if first.st != stOK || first.eof {
		t.Fatalf("first page: %+v", first)
	}
	// Mutate an unrelated directory: d's continuation must not re-Lstat its entries.
	if stat, d := call(v3Create, func(e *encoder) {
		e.fh(otherID, 0)
		e.str("newfile")
		e.u32(createUnchecked)
		for range 4 {
			e.bool(false)
		}
		e.u32(0)
		e.u32(0)
	}); stat != rpcSuccess || status(d.u32()) != stOK {
		t.Fatal("CREATE in other dir failed")
	}
	before := fs.lstats.Load()
	cookie := first.entries[len(first.entries)-1].cookie
	pg := readdir3(t, call, true, dirID, cookie, first.verf, 4096, 600)
	if pg.st != stOK || len(pg.entries) == 0 {
		t.Fatalf("continuation page: %+v", pg)
	}
	// Only the directory itself is Lstat'ed once at the start of list3.
	if delta := fs.lstats.Load() - before; delta > 1 {
		t.Fatalf("unrelated CREATE caused %d Lstats during READDIRPLUS continuation of d (want <= 1)", delta)
	}

	// Mutating d itself must still refresh d's continuation entries.
	last := files[len(files)-1]
	base.write(t, path.Join("d", last), "grown after same-dir create")
	if stat, d := call(v3Create, func(e *encoder) {
		e.fh(dirID, 0)
		e.str("zz-added")
		e.u32(createUnchecked)
		for range 4 {
			e.bool(false)
		}
		e.u32(0)
		e.u32(0)
	}); stat != rpcSuccess || status(d.u32()) != stOK {
		t.Fatal("CREATE in d failed")
	}
	for !pg.eof {
		cookie = pg.entries[len(pg.entries)-1].cookie
		pg = readdir3(t, call, true, dirID, cookie, first.verf, 4096, 600)
		if pg.st != stOK {
			t.Fatalf("continuation after same-dir create: %d", pg.st)
		}
		for _, e := range pg.entries {
			if e.name == last {
				if e.attrs == nil || e.attrs.size != uint64(len("grown after same-dir create")) {
					t.Fatalf("stale entry after same-dir CREATE: %+v", e.attrs)
				}
				return
			}
		}
	}
	t.Fatalf("%s not found in continuation", last)
}
