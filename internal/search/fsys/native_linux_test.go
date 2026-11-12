//go:build linux

package fsys

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"golang.org/x/sys/unix"
)

// TestStatFallbackRawFstatat forces the statx-less path and checks the raw
// fstatat ABI: relative lookups without following symlinks, path Stat and
// descriptor-based StatDir.
func TestStatFallbackRawFstatat(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("f", filepath.Join(dir, "l")); err != nil {
		t.Fatal(err)
	}
	prev := noStatx.Swap(true)
	t.Cleanup(func() { noStatx.Store(prev) })

	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)

	s := Native.NewSession().(*session)
	for _, c := range []struct {
		name string
		typ  Type
		size int64
	}{{"f", File, 5}, {"d", Dir, -1}, {"l", Symlink, -1}} {
		var want unix.Stat_t
		if err := unix.Fstatat(fd, c.name, &want, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			t.Fatal(err)
		}
		typ, size, mtime, ok := s.stat(fd, append([]byte(c.name), 0))
		if !ok || typ != c.typ || size != c.size || mtime != nanos(int64(want.Mtim.Sec), int64(want.Mtim.Nsec)) {
			t.Errorf("stat(%q) = %v,%d,%d,%v; want type %v size %d mtime %d", c.name, typ, size, mtime, ok, c.typ, c.size, nanos(int64(want.Mtim.Sec), int64(want.Mtim.Nsec)))
		}
		st, typ, _, err := statStamp(fd, append([]byte(c.name), 0), unix.AT_SYMLINK_NOFOLLOW)
		if err != nil || typ != c.typ || st.Ino != want.Ino || st.Dev != want.Dev ||
			st.Mtime != nanos(int64(want.Mtim.Sec), int64(want.Mtim.Nsec)) || st.Ctime != nanos(int64(want.Ctim.Sec), int64(want.Ctim.Nsec)) {
			t.Errorf("statStamp(%q) = %+v,%v,%v; want ino %d", c.name, st, typ, err, want.Ino)
		}
	}
	if _, _, _, ok := s.stat(fd, []byte("missing\x00")); ok {
		t.Error("stat of missing name succeeded")
	}

	// Path Stat follows or not as requested.
	if fi, err := s.Stat(filepath.Join(dir, "l"), false); err != nil || fi.Type != Symlink {
		t.Errorf("Stat(nofollow symlink) = %+v, %v", fi, err)
	}
	if fi, err := s.Stat(filepath.Join(dir, "l"), true); err != nil || fi.Type != File || fi.Size != 5 {
		t.Errorf("Stat(follow symlink) = %+v, %v", fi, err)
	}

	// StatDir stamps the open directory descriptor.
	var want unix.Stat_t
	if err := unix.Fstat(fd, &want); err != nil {
		t.Fatal(err)
	}
	st, err := s.StatDir(Handle{h: uintptr(fd), ok: true})
	if err != nil || st.Ino != want.Ino || st.Dev != want.Dev || st.Mtime != nanos(int64(want.Mtim.Sec), int64(want.Mtim.Nsec)) {
		t.Errorf("StatDir = %+v, %v; want ino %d", st, err, want.Ino)
	}
}

// TestReadDirMatchesLstat compares native metadata listings with os.Lstat
// for regular files, directories, symlinks (including a dangling one), and a
// FIFO, then checks that the listing includes special files and that a
// non-directory path fails.
func TestReadDirMatchesLstat(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("hello"), 0o4755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "d"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("f", filepath.Join(dir, "l")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("nowhere", filepath.Join(dir, "dangling")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(dir, "p"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := Native.NewSession().(*session)
	s.dir = dir
	entries, err := s.readDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 5 {
		t.Fatalf("got %d entries, want 5", len(entries))
	}
	for _, e := range entries {
		want, err := os.Lstat(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		got, err := e.Info()
		if err != nil {
			t.Fatalf("%s: %v", e.Name(), err)
		}
		if got.Name() != want.Name() || got.Mode() != want.Mode() || got.Size() != want.Size() ||
			!got.ModTime().Equal(want.ModTime()) || e.Type() != want.Mode().Type() || e.IsDir() != want.IsDir() {
			t.Errorf("%s: got %v %d %v, want %v %d %v", e.Name(), got.Mode(), got.Size(), got.ModTime(), want.Mode(), want.Size(), want.ModTime())
		}
		if !reflect.DeepEqual(got.Sys(), want.Sys()) {
			t.Errorf("%s: Sys differs: %+v vs %+v", e.Name(), got.Sys(), want.Sys())
		}
	}
	if _, err := s.readDir(filepath.Join(dir, "f")); err == nil {
		t.Error("readDir of a regular file succeeded")
	}
}
