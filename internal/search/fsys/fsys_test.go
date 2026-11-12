package fsys

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
)

// A listing owns its names and metadata, including when the same native
// session is reused or other goroutines borrow ReadDir's pooled sessions.
func TestReadDirOwnsMetadata(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	const count = 48
	for i := range count {
		for _, dir := range []string{first, second} {
			prefix := "first"
			if dir == second {
				prefix = "other"
			}
			name := fmt.Sprintf("%s-%03d-\u00e9-\U00010000", prefix, i)
			if err := os.WriteFile(filepath.Join(dir, name), []byte(strings.Repeat("x", i)), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	s := &session{fs: native}
	retained, err := s.readDir(first)
	if err != nil {
		t.Fatal(err)
	}
	if len(retained) != count {
		t.Fatalf("got %d entries, want %d", len(retained), count)
	}
	slices.SortFunc(retained, func(a, b os.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	type snapshot struct {
		name string
		info os.FileInfo
	}
	saved := make([]snapshot, len(retained))
	for i, entry := range retained {
		info, err := os.Lstat(filepath.Join(first, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		saved[i] = snapshot{strings.Clone(entry.Name()), info}
	}
	if _, err := s.readDir(second); err != nil {
		t.Fatal(err)
	}
	clear(s.buf)
	clear(s.cname)
	clear(s.ls.names)
	check := func() {
		t.Helper()
		for i, entry := range retained {
			want := saved[i]
			info, err := entry.Info()
			if err != nil {
				t.Error(err)
				continue
			}
			if entry.Name() != want.name || info.Name() != want.name ||
				info.Size() != want.info.Size() || info.Mode() != want.info.Mode() ||
				!info.ModTime().Equal(want.info.ModTime()) {
				t.Errorf("retained entry %d changed: name %q, info %v; want %q, %v", i, entry.Name(), info, want.name, want.info)
			}
		}
	}
	check()
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 8 {
				if _, err := ReadDir(second); err != nil {
					t.Error(err)
					return
				}
				check()
			}
		}()
	}
	wg.Wait()
}

func BenchmarkReadDirMetadata(b *testing.B) {
	for _, count := range []int{1, 32, 1024} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			dir := b.TempDir()
			for i := range count {
				name := fmt.Sprintf("metadata-name-%05d", i)
				if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportAllocs()
			for b.Loop() {
				entries, err := ReadDir(dir)
				if err != nil || len(entries) != count {
					b.Fatalf("ReadDir: %d entries, %v", len(entries), err)
				}
			}
		})
	}
}

func listNames(t *testing.T, s Session, h Handle) []string {
	t.Helper()
	ls, err := s.List(h, Full, 0)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for i := range ls.Entries {
		names = append(names, string(ls.Name(&ls.Entries[i]))+":"+ls.Entries[i].Type.String())
	}
	slices.Sort(names)
	return names
}

// readFile opens name below dir and reads up to limit bytes of it into buf.
func readFile(s Session, dir Handle, name string, buf []byte, limit int64) ([]byte, FileInfo, error) {
	h, info, err := s.OpenFile(dir, name)
	if err != nil {
		return nil, info, err
	}
	defer s.Close(h)
	data, err := s.Read(h, buf, limit)
	return data, info, err
}

// Both filesystems must answer the same operations the same way.
func TestNativeAndIOFSAgree(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"a.txt": "hello", "dir/b.txt": "0123456789"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mem := fstest.MapFS{
		"a.txt":     {Data: []byte("hello")},
		"dir/b.txt": {Data: []byte("0123456789")},
	}
	for _, c := range []struct {
		name string
		fs   FS
		root string
	}{{"native", Native, root}, {"iofs", FromIOFS(mem), "/"}} {
		t.Run(c.name, func(t *testing.T) {
			abs, err := c.fs.Abs(c.root)
			if err != nil {
				t.Fatal(err)
			}
			s := c.fs.NewSession()
			h, err := s.OpenDir(abs)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close(h)
			if got, want := listNames(t, s, h), []string{"a.txt:file", "dir:dir"}; !slices.Equal(got, want) {
				t.Errorf("list: got %q, want %q", got, want)
			}

			data, info, err := readFile(s, h, "dir/b.txt", nil, 4)
			if err != nil || string(data) != "0123" || info.Size != 10 {
				t.Errorf("limited read: %q size=%d err=%v", data, info.Size, err)
			}
			data, info, err = readFile(s, h, "a.txt", data, 1<<20)
			if err != nil || string(data) != "hello" || info.Size != 5 {
				t.Errorf("full read: %q size=%d err=%v", data, info.Size, err)
			}
			if _, _, err := s.OpenFile(h, "dir"); !errors.Is(err, ErrNotRegular) {
				t.Errorf("opening a directory: err=%v, want ErrNotRegular", err)
			}
			if _, _, err := s.OpenFile(h, "missing"); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("opening a missing file: err=%v, want not-exist", err)
			}

			child, err := s.OpenChild(h, "dir")
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close(child)
			if got, want := listNames(t, s, child), []string{"b.txt:file"}; !slices.Equal(got, want) {
				t.Errorf("child list: got %q, want %q", got, want)
			}
		})
	}
}
