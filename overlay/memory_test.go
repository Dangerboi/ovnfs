package overlay

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"math/rand/v2"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5"
)

// TestMemoryMatchesDisk runs one random operation sequence on a memory upper
// whose tiny budget forces spills by size and by eviction, and on a disk upper
// over the same layers. The merged views, errors and open-handle contents must
// agree throughout, and after Flush the memory upper must equal the disk one.
func TestMemoryMatchesDisk(t *testing.T) {
	disk := stackFixture(t)
	budget := NewMemory(6<<10, 2<<10, 0)
	mem, err := New(t.TempDir(), disk.Stack(), disk.bottom, budget)
	must(t, err)
	both := [2]*FS{disk, mem}
	files := []string{"d/low", "d/top", "d/base", "d/replace", "d/sub/low", "d/new", "d/sub/new", "x/a", "x/b"}
	dirs := []string{"d/sub", "x", "x/y", "d/moved"}
	modes := []os.FileMode{0o644, 0o600, 0o444, 0o755}
	var held [][2]billy.File
	rng := rand.New(rand.NewPCG(5, 23))
	for step := range 1500 {
		p, q := files[rng.IntN(len(files))], files[rng.IntN(len(files))]
		dir := dirs[rng.IntN(len(dirs))]
		size, off := rng.IntN(1200), int64(rng.IntN(1200))
		if rng.IntN(6) == 0 {
			size, off = 1<<10+rng.IntN(2<<10), int64(rng.IntN(3<<10)) // outgrows the per-file limit
		}
		data := bytes.Repeat([]byte{byte('a' + step%26)}, size)
		mode := modes[rng.IntN(len(modes))]
		if info, err := disk.Lstat(p); err == nil && info.IsDir() {
			mode = 0o755 // directories stay searchable so the views remain walkable
		}
		stamp := past.Add(time.Duration(step) * time.Second)
		op := rng.IntN(15)
		pick := -1
		if len(held) > 0 {
			pick = rng.IntN(len(held))
		}
		var errs [2]error
		var opened [2]billy.File
		var got [2][]byte
		for k, f := range both {
			errs[k] = func() error {
				switch op {
				case 0:
					return writeFile(f, p, data, os.O_CREATE|os.O_WRONLY|os.O_TRUNC)
				case 1:
					return writeFile(f, p, data, os.O_CREATE|os.O_WRONLY|os.O_APPEND)
				case 2:
					h, err := f.OpenFile(p, os.O_RDWR, 0)
					if err != nil {
						return err
					}
					_, err = h.(io.WriterAt).WriteAt(data, off)
					return errors.Join(err, h.Close())
				case 3:
					h, err := f.OpenFile(p, os.O_RDWR, 0)
					if err != nil {
						return err
					}
					return errors.Join(h.Truncate(off), h.Close())
				case 4:
					return f.Remove(p)
				case 5:
					return f.Rename(p, q)
				case 6:
					return f.Rename(dir, p)
				case 7:
					return f.Chmod(p, mode)
				case 8:
					return f.Chtimes(p, stamp, stamp)
				case 9:
					return f.Link(p, q)
				case 10:
					return f.MkdirAll(dir, 0o755)
				case 11:
					return f.Rename(p, path.Join(dir, path.Base(p)))
				case 12:
					h, err := f.OpenFile(p, os.O_RDWR, 0)
					opened[k] = h
					return err
				case 13:
					if pick < 0 {
						return nil
					}
					h := held[pick][k]
					return errors.Join(h.Lock(), h.Unlock())
				default:
					if pick < 0 {
						return nil
					}
					h := held[pick][k]
					if _, err := h.(io.WriterAt).WriteAt(data, off); err != nil {
						return err
					}
					info, err := h.(interface{ Stat() (os.FileInfo, error) }).Stat()
					if err != nil {
						return err
					}
					if _, err := h.Seek(0, io.SeekStart); err != nil {
						return err
					}
					b, err := io.ReadAll(h)
					if err != nil {
						return err
					}
					tail := make([]byte, 512)
					n, err := h.ReadAt(tail, off/2)
					if err != nil && err != io.EOF {
						return err
					}
					got[k] = fmt.Appendf(b, " size=%d tail=%q", info.Size(), tail[:n])
					return nil
				}
			}()
		}
		if errClass(errs[0]) != errClass(errs[1]) {
			t.Fatalf("step %d op %d (%s, %s, %s): disk %v, memory %v", step, op, p, q, dir, errs[0], errs[1])
		}
		if opened[0] != nil {
			held = append(held, opened)
		}
		if !bytes.Equal(got[0], got[1]) {
			t.Fatalf("step %d: held handle reads %q on disk, %q in memory", step, got[0], got[1])
		}
		if a, b := mergedTree(t, disk), mergedTree(t, mem); !maps.Equal(a, b) {
			t.Fatalf("step %d op %d (%s, %s, %s): views differ\ndisk   %v\nmemory %v", step, op, p, q, dir, a, b)
		}
		if used, limit := budget.Usage(); used > limit {
			t.Fatalf("step %d: %d bytes in memory exceed the %d budget", step, used, limit)
		}
	}
	for _, h := range held {
		must(t, h[0].Close())
		must(t, h[1].Close())
	}
	must(t, mem.Flush())
	assertIndexMatchesDisk(t, mem)
	if a, b := upperTree(t, disk.upper), upperTree(t, mem.upper); !maps.Equal(a, b) {
		t.Fatalf("flushed upper differs from the disk upper\ndisk   %v\nmemory %v", a, b)
	}
	if used, _ := budget.Usage(); used != 0 {
		t.Fatalf("flush left %d bytes charged", used)
	}
}

// TestMemoryEvictsAcrossOverlays spills another overlay's least recently used
// files when the shared budget is full, keeping their contents.
func TestMemoryEvictsAcrossOverlays(t *testing.T) {
	budget := NewMemory(16<<10, 4<<10, 0)
	a, err := New(t.TempDir(), nil, nil, budget)
	must(t, err)
	b, err := New(t.TempDir(), nil, nil, budget)
	must(t, err)
	content := func(fsName string, i int) string { return strings.Repeat(fsName+fmt.Sprint(i), 300) }
	for i := range 6 {
		write(t, a, fmt.Sprintf("f%d", i), content("a", i), os.O_CREATE|os.O_WRONLY)
	}
	for i := range 6 {
		write(t, b, fmt.Sprintf("f%d", i), content("b", i), os.O_CREATE|os.O_WRONLY)
	}
	if used, limit := budget.Usage(); used > limit {
		t.Fatalf("%d bytes in memory exceed the %d budget", used, limit)
	}
	if _, err := os.Stat(filepath.Join(a.upper, "f0")); err != nil {
		t.Fatalf("the coldest file of the other overlay did not spill: %v", err)
	}
	if !b.Unflushed() {
		t.Fatal("the newest files left memory")
	}
	for i := range 6 {
		for name, f := range map[string]*FS{"a": a, "b": b} {
			if got := read(t, f, fmt.Sprintf("f%d", i)); got != content(name, i) {
				t.Fatalf("%s/f%d = %.12q…", name, i, got)
			}
		}
	}
}

// TestMemoryWritesBackIdleFiles moves files left untouched for the idle age
// into the upper in the background, while an open handle keeps working and
// files deleted before then never reach the disk.
func TestMemoryWritesBackIdleFiles(t *testing.T) {
	budget := NewMemory(1<<20, 64<<10, 20*time.Millisecond)
	f, err := New(t.TempDir(), nil, nil, budget)
	must(t, err)
	h, err := f.OpenFile("open", os.O_CREATE|os.O_RDWR, 0o644)
	must(t, err)
	_, err = h.Write([]byte("before "))
	must(t, err)
	write(t, f, "temp", "short-lived", os.O_CREATE|os.O_WRONLY)
	must(t, f.Remove("temp"))
	deadline := time.Now().Add(5 * time.Second)
	for f.Unflushed() {
		if time.Now().After(deadline) {
			t.Fatal("idle file still in memory after 5s")
		}
		time.Sleep(5 * time.Millisecond)
	}
	_, err = h.Write([]byte("after"))
	must(t, err)
	must(t, h.Close())
	if data, err := os.ReadFile(filepath.Join(f.upper, "open")); err != nil || string(data) != "before after" {
		t.Fatalf("upper holds %q, %v", data, err)
	}
	if _, err := os.Lstat(filepath.Join(f.upper, "temp")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a file removed while in memory reached the upper: %v", err)
	}
	if used, _ := budget.Usage(); used != 0 {
		t.Fatalf("writeback left %d bytes charged", used)
	}
}

// TestMemoryNameChangesStampParent keeps NFS directory caches valid: creating,
// renaming or removing a memory file advances its directory's mtime as a host
// operation would, while a copy-up into memory leaves it alone.
func TestMemoryNameChangesStampParent(t *testing.T) {
	lower := tree(t, map[string]string{"d/lower": "data"})
	f, err := New(filepath.Join(t.TempDir(), "u"), nil, dirBottom{lower}, NewMemory(1<<20, 64<<10, 0))
	must(t, err)
	stamped := func(change func() error) bool {
		t.Helper()
		must(t, f.Chtimes("d", past, past))
		must(t, change())
		info, err := f.Lstat("d")
		must(t, err)
		return info.ModTime().After(past)
	}
	if stamped(func() error { return writeFile(f, "d/lower", []byte("X"), os.O_RDWR) }) {
		t.Fatal("copy-up changed the directory mtime")
	}
	if !f.Unflushed() {
		t.Fatal("copy-up did not stay in memory")
	}
	if !stamped(func() error { return writeFile(f, "d/new", []byte("n"), os.O_CREATE|os.O_WRONLY) }) {
		t.Fatal("create left the directory mtime unchanged")
	}
	if !stamped(func() error { return f.Rename("d/new", "d/renamed") }) {
		t.Fatal("rename left the directory mtime unchanged")
	}
	if !stamped(func() error { return f.Remove("d/renamed") }) {
		t.Fatal("remove left the directory mtime unchanged")
	}
}

func TestFlushReconcilesVanishedDirectory(t *testing.T) {
	if !retimable() {
		t.Skip("host does not defer visible directory times")
	}
	f, err := New(t.TempDir(), nil, nil, NewMemory(1<<20, 64<<10, 0))
	must(t, err)
	must(t, f.MkdirAll("build/old/leaf", 0o755))
	must(t, f.MkdirAll("build/old/sibling", 0o755))
	must(t, f.Flush())
	must(t, f.Chtimes("build", past, past))
	before, err := f.Lstat("build/old")
	must(t, err)
	deferTestDirTime(t, f, "build/old/leaf")
	if _, ok := f.unsettled["build/old"]; ok {
		t.Fatal("only a descendant should need its time settled")
	}
	write(t, f, "source.txt", "source edit", os.O_CREATE|os.O_WRONLY)
	epoch := f.Names()
	must(t, os.RemoveAll(f.up("build/old")))
	must(t, f.Flush())

	assertNames(t, f, "build")
	assertGone(t, f, "build/old/sibling")
	if p, ok := f.Locate(handleID(before)); ok {
		t.Fatalf("vanished directory identity still resolves to %s", p)
	}
	after, err := f.Lstat("build")
	must(t, err)
	host, err := os.Stat(f.up("build"))
	must(t, err)
	if f.Names() == epoch || !host.ModTime().After(past) || !host.ModTime().Equal(after.ModTime()) || len(f.unsettled) != 0 {
		t.Fatalf("reconciliation did not settle the visible parent change: host %s, index %s, pending %v", host.ModTime(), after.ModTime(), f.unsettled)
	}
	data, err := os.ReadFile(f.up("source.txt"))
	must(t, err)
	if string(data) != "source edit" {
		t.Fatalf("unrelated pending write = %q", data)
	}
	must(t, f.Flush())
	assertIndexMatchesDisk(t, f)
}

func TestFlushRefusesLostUpperData(t *testing.T) {
	if !retimable() {
		t.Skip("host does not defer visible directory times")
	}
	for _, tc := range []struct {
		name  string
		lower map[string]string
		make  func(*testing.T, *FS)
		keep  string
	}{
		{"file", nil, func(t *testing.T, f *FS) {
			write(t, f, "d/code", "source edit", os.O_CREATE|os.O_WRONLY)
		}, "d/code"},
		{"whiteout", map[string]string{"d/code": "lower"}, func(t *testing.T, f *FS) {
			must(t, f.Remove("d/code"))
		}, "d/code"},
		{"lower directory metadata", map[string]string{"d/code": "lower"}, func(t *testing.T, f *FS) {
			must(t, f.Chmod("d", 0o700))
		}, "d"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, err := New(t.TempDir(), nil, dirBottom{tree(t, tc.lower)}, NewMemory(1<<20, 64<<10, 0))
			must(t, err)
			must(t, f.MkdirAll("d/pending", 0o755))
			tc.make(t, f)
			must(t, f.Flush())
			deferTestDirTime(t, f, "d/pending")
			must(t, os.RemoveAll(f.up("d")))

			for range 2 {
				err := f.Flush()
				if !errors.Is(err, syscall.EIO) || missing(err) || !strings.Contains(err.Error(), f.up("d")) {
					t.Fatalf("Flush with vanished upper = %v, want EIO naming the directory", err)
				}
			}
			if _, ok := f.index.entries[tc.keep]; !ok {
				t.Fatalf("failed reconciliation forgot %s", tc.keep)
			}
		})
	}
}

func TestFlushPreservesMemoryFileInVanishedDirectory(t *testing.T) {
	f, err := New(t.TempDir(), nil, nil, NewMemory(1<<20, 64<<10, 0))
	must(t, err)
	must(t, f.MkdirAll("src", 0o755))
	must(t, f.Flush())
	write(t, f, "src/code", "pending source edit", os.O_CREATE|os.O_WRONLY)
	away := filepath.Join(t.TempDir(), "src")
	must(t, os.Rename(f.up("src"), away))

	if err := f.Flush(); !errors.Is(err, syscall.EIO) || missing(err) || !strings.Contains(err.Error(), f.up("src/code")) {
		t.Fatalf("Flush with unavailable spill destination = %v, want EIO naming the file", err)
	}
	if !f.Unflushed() || read(t, f, "src/code") != "pending source edit" {
		t.Fatal("failed flush did not retain the pending write")
	}
	must(t, os.Rename(away, f.up("src")))
	must(t, f.Flush())
	data, err := os.ReadFile(f.up("src/code"))
	must(t, err)
	if string(data) != "pending source edit" {
		t.Fatalf("flushed pending write = %q", data)
	}
}

func deferTestDirTime(t *testing.T, f *FS, d string) {
	t.Helper()
	p := path.Join(d, "temporary")
	write(t, f, p, "transient", os.O_CREATE|os.O_WRONLY)
	must(t, f.Remove(p))
	if _, ok := f.unsettled[d]; !ok || f.Unflushed() {
		t.Fatalf("%s should have a pending mtime but no pending writes", d)
	}
}

// TestMemoryFilesShowTheirDiskOwner reports a memory file's owner as a host
// create in its directory would set it, so moving the file to disk changes
// no attribute a client caches.
func TestMemoryFilesShowTheirDiskOwner(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("numeric owners")
	}
	groups, err := os.Getgroups()
	must(t, err)
	alt := -1
	for _, g := range groups {
		if g != os.Getegid() {
			alt = g
			break
		}
	}
	if alt < 0 {
		t.Skip("needs a supplementary group")
	}
	upper := t.TempDir()
	// The directory hands its group to new files: always on darwin, setgid on Linux.
	must(t, os.Chown(upper, -1, alt))
	must(t, os.Chmod(upper, 0o755|os.ModeSetgid))
	f, err := New(upper, nil, nil, NewMemory(1<<20, 64<<10, 0))
	must(t, err)
	write(t, f, "new", "data", os.O_CREATE|os.O_WRONLY)
	info, err := f.Lstat("new")
	must(t, err)
	want := fmt.Sprintf("%d:%d", os.Geteuid(), alt)
	if got := owner(info); got != want {
		t.Fatalf("memory file owner %s, want %s", got, want)
	}
	must(t, f.Flush())
	info, err = f.Lstat("new")
	must(t, err)
	if got := owner(info); got != want {
		t.Fatalf("owner after moving to disk %s, want %s", got, want)
	}
}

// writeFile writes data through a fresh descriptor opened with flag.
func writeFile(f *FS, p string, data []byte, flag int) error {
	h, err := f.OpenFile(p, flag, 0o666)
	if err != nil {
		return err
	}
	_, err = h.Write(data)
	return errors.Join(err, h.Close())
}

// errClass reduces an error to what callers can observe: its errno or sentinel.
func errClass(err error) string {
	var errno syscall.Errno
	switch {
	case err == nil:
		return ""
	case errors.As(err, &errno):
		return errno.Error()
	case errors.Is(err, fs.ErrNotExist):
		return "not exist"
	case errors.Is(err, fs.ErrExist):
		return "exist"
	}
	return err.Error()
}

// mergedTree describes every visible entry: kind, permissions, size and content.
func mergedTree(t *testing.T, f *FS) map[string]string {
	t.Helper()
	out := make(map[string]string)
	var walk func(string)
	walk = func(dir string) {
		infos, err := f.ReadDir(dir)
		must(t, err)
		for _, info := range infos {
			p := path.Join(dir, info.Name())
			if info.IsDir() {
				out[p] = fmt.Sprintf("dir %v", info.Mode().Perm())
				walk(p)
				continue
			}
			lst, err := f.Lstat(p)
			must(t, err)
			if lst.Size() != info.Size() || lst.Mode() != info.Mode() {
				t.Fatalf("%s: listing %v/%d disagrees with lstat %v/%d", p, info.Mode(), info.Size(), lst.Mode(), lst.Size())
			}
			content := "<unreadable>"
			if info.Mode().Perm()&0o400 != 0 {
				content = read(t, f, p)
			}
			out[p] = fmt.Sprintf("file %v %d %s %q", info.Mode(), info.Size(), owner(info), content)
		}
	}
	walk("")
	return out
}

func TestMemorySync(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("enabled=%v", enabled), func(t *testing.T) {
			upper := filepath.Join(t.TempDir(), "upper")
			mem := NewMemory(1<<20, 64<<10, 0)
			mem.SyncOnFsync = enabled
			f, err := New(upper, nil, dirBottom{tree(t, map[string]string{"d/lower": "data"})}, mem)
			must(t, err)
			for _, name := range []string{"d/new", "d/lower"} {
				h, err := f.OpenFile(name, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o644)
				must(t, err)
				other, err := f.OpenFile(name, os.O_RDWR, 0)
				must(t, err)
				_, err = h.Write([]byte("synced"))
				must(t, err)
				if _, err := os.Stat(filepath.Join(upper, name)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("file present before sync: %v", err)
				}
				must(t, h.(interface{ Sync() error }).Sync())
				if enabled {
					b, err := os.ReadFile(filepath.Join(upper, name))
					if err != nil || string(b) != "synced" {
						t.Fatalf("after sync: %q, %v", b, err)
					}
				} else if _, err := os.Stat(filepath.Join(upper, name)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("disabled sync spilled file: %v", err)
				}
				_, err = other.(io.WriterAt).WriteAt([]byte("S"), 0)
				must(t, err)
				_, err = h.Write([]byte(" more"))
				must(t, err)
				if got := read(t, f, name); got != "Synced more" {
					t.Fatalf("handles after sync: %q", got)
				}
				must(t, other.Close())
				must(t, h.Close())
			}
			if f.Unflushed() == enabled {
				t.Fatalf("unflushed = %v, enabled = %v", f.Unflushed(), enabled)
			}
		})
	}
}

func TestMemorySyncFailureAndHandleStates(t *testing.T) {
	mem := NewMemory(1<<20, 64<<10, 0)
	mem.SyncOnFsync = true
	f, err := New(t.TempDir(), nil, nil, mem)
	must(t, err)
	h, err := f.OpenFile("file", os.O_CREATE|os.O_RDWR, 0o644)
	must(t, err)
	_, err = h.Write([]byte("data"))
	must(t, err)
	must(t, os.RemoveAll(f.work))
	if err := h.(interface{ Sync() error }).Sync(); err == nil {
		t.Fatal("sync succeeded despite failed spill")
	}
	if got := read(t, f, "file"); got != "data" || !f.Unflushed() {
		t.Fatalf("failed spill lost memory data: %q", got)
	}
	must(t, os.Mkdir(f.work, 0o700))
	must(t, h.(interface{ Sync() error }).Sync())
	must(t, h.Close())
	if err := h.(interface{ Sync() error }).Sync(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("sync after close: %v", err)
	}

	unlinked, err := f.OpenFile("unlinked", os.O_CREATE|os.O_RDWR, 0o644)
	must(t, err)
	must(t, f.Remove("unlinked"))
	must(t, unlinked.(interface{ Sync() error }).Sync())
	must(t, unlinked.Close())
	if err := unlinked.(interface{ Sync() error }).Sync(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("sync closed unlinked file: %v", err)
	}
}

// upperTree describes a host upper (markers included, staging excluded) by
// kind, permissions and content.
func upperTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := make(map[string]string)
	must(t, filepath.Walk(dir, func(real string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, real)
		if err != nil {
			return err
		}
		switch {
		case rel == workName:
			return filepath.SkipDir
		case info.IsDir():
			out[rel] = fmt.Sprintf("dir %v", info.Mode().Perm())
		default:
			data, err := os.ReadFile(real)
			if err != nil && !errors.Is(err, fs.ErrPermission) {
				return err
			}
			out[rel] = fmt.Sprintf("file %v %q", info.Mode(), data)
		}
		return nil
	}))
	return out
}
