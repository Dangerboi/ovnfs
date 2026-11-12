//go:build darwin

package fsys

import (
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
	"unsafe"
)

// metadataTree makes a directory of every entry type a listing meets.
func metadataTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	at := func(name string) string { return filepath.Join(dir, name) }
	write := func(name, data string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(at(name), []byte(data), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(at(name), mode); err != nil {
			t.Fatal(err)
		}
		if info, err := os.Lstat(at(name)); err != nil || info.Mode() != mode {
			t.Fatalf("fixture %s has mode %v (%v), want %v", name, info, err, mode)
		}
	}
	write("empty", "", 0o644)
	write("small", "hello", 0o600)
	write("big", string(make([]byte, 3<<20)), 0o755)
	write("sticky", "x", 0o755|os.ModeSticky)
	write("setuid", "x", 0o755|os.ModeSetuid)
	write("ünï.txt", "unicode", 0o644)
	write("forked", "data", 0o644)
	if err := os.WriteFile(at("forked")+"/..namedfork/rsrc", make([]byte, 100<<10), 0o644); err != nil {
		t.Fatalf("writing a resource fork: %v", err)
	}
	if err := os.Link(at("small"), at("hard")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("small", at("link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing", at("dangling")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(at("sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(at("fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestBulkRecordsDescribeRegularFiles runs the getattrlistbulk call and the
// record decoder that ReadDir uses, which fall back to per-entry lstat on any
// doubt, and requires them to describe every regular file completely: the
// bulk fast path really runs on the local filesystem, and its data equals
// lstat's field for field, resource fork and hard link included.
func TestBulkRecordsDescribeRegularFiles(t *testing.T) {
	if isRoot {
		t.Skip("the superuser's st_gen needs lstat")
	}
	dir := metadataTree(t)
	s := &session{fs: native}
	h, err := s.openDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer closeHandle(h)
	buf := make([]byte, listBufSize)
	seen := map[string]bool{}
	for {
		r, _, e := syscall_syscall6(libc_getattrlistbulk_trampoline_addr, uintptr(h), uintptr(unsafe.Pointer(&metadataAttrs)), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), fsoptNoFollow, 0)
		if e != 0 {
			t.Fatalf("getattrlistbulk: %v", e)
		}
		if r == 0 {
			break
		}
		off := 0
		for range int(r) {
			rec := buf[off:]
			rec = rec[:int(*(*uint32)(unsafe.Pointer(&rec[0])))]
			off += len(rec)
			raw, nameOff, ok := recordName(rec, bulkNameRef+8)
			if !ok {
				t.Fatal("malformed record name")
			}
			name := string(raw)
			if name == "." || name == ".." {
				continue
			}
			seen[name] = true
			want, err := os.Lstat(filepath.Join(dir, name))
			if err != nil {
				t.Fatal(err)
			}
			var st syscall.Stat_t
			vtype, complete := bulkStat(rec, nameOff, &st)
			if wantComplete := want.Mode().IsRegular(); complete != wantComplete {
				t.Errorf("%s: complete = %v, want %v (vnode type %d)", name, complete, wantComplete, vtype)
				continue
			}
			if complete && !reflect.DeepEqual(&st, want.Sys()) {
				t.Errorf("%s: bulk stat %+v, lstat has %+v", name, &st, want.Sys())
			}
		}
	}
	if len(seen) != 12 {
		t.Errorf("bulk listing has %d entries (%v), want 12", len(seen), seen)
	}
}

// TestReadDirNativeMatchesLstat lists the same tree with the native reader
// alone, with no fallback to os.ReadDir, and compares every entry to lstat.
func TestReadDirNativeMatchesLstat(t *testing.T) {
	dir := metadataTree(t)
	s := &session{fs: native}
	entries, err := s.readDirNative(dir)
	if err != nil {
		t.Fatalf("native listing of a local directory failed: %v", err)
	}
	plain, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(plain) {
		t.Fatalf("native listing has %d entries, os.ReadDir %d", len(entries), len(plain))
	}
	byName := map[string]os.DirEntry{}
	for _, e := range entries {
		byName[e.Name()] = e
	}
	for _, p := range plain {
		e := byName[p.Name()]
		if e == nil {
			t.Fatalf("%s missing from the native listing", p.Name())
		}
		if e.Type() != p.Type() || e.IsDir() != p.IsDir() {
			t.Errorf("%s: type %v dir %v, os.ReadDir has %v %v", p.Name(), e.Type(), e.IsDir(), p.Type(), p.IsDir())
		}
		got, err := e.Info()
		if err != nil {
			t.Fatalf("%s: %v", p.Name(), err)
		}
		want, err := os.Lstat(filepath.Join(dir, p.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if got.Name() != want.Name() || got.Mode() != want.Mode() || got.Size() != want.Size() || !got.ModTime().Equal(want.ModTime()) || got.IsDir() != want.IsDir() {
			t.Errorf("%s: got mode %v size %d mtime %v, lstat has mode %v size %d mtime %v",
				p.Name(), got.Mode(), got.Size(), got.ModTime(), want.Mode(), want.Size(), want.ModTime())
		}
		if !reflect.DeepEqual(got.Sys(), want.Sys()) {
			t.Errorf("%s: native stat %+v, lstat has %+v", p.Name(), got.Sys(), want.Sys())
		}
	}
}

// TestReadDirSearchesFirst checks that a directory that can be listed but not
// searched fails like os.ReadDir's listing, per entry, instead of listing data
// the entries' lstat would refuse.
func TestReadDirSearchesFirst(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("the superuser searches every directory")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o444); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o755)
	entries, err := ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries %v", entries)
	}
	if _, err := entries[0].Info(); err == nil {
		t.Fatal("an unsearchable directory's entry described")
	}
}
