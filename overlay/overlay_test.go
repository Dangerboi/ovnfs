package overlay

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/stencil-hq/ovnfs/internal/clone"
)

// past is the stable timestamp used to detect accidental copy-up changes.
var past = time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)

// dirBottom is a test-only live bottom backed by a plain directory.
type dirBottom struct{ dir string }

// Lstat reads unmodified host metadata for the test bottom.
func (b dirBottom) Lstat(p string) (os.FileInfo, error) { return os.Lstat(filepath.Join(b.dir, p)) }

// ReadDir lists host entries without interpreting overlay markers.
func (b dirBottom) ReadDir(p string) ([]os.FileInfo, error) {
	info, err := b.Lstat(p)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, &fs.PathError{Op: "readdir", Path: p, Err: syscall.ENOTDIR}
	}
	items, err := os.ReadDir(filepath.Join(b.dir, p))
	if err != nil {
		return nil, err
	}
	out := make([]os.FileInfo, 0, len(items))
	for _, item := range items {
		info, err := item.Info()
		if err != nil {
			return nil, err
		}
		out = append(out, info)
	}
	return out, nil
}

// Open exposes the test bottom read-only.
func (b dirBottom) Open(p string) (Content, error) {
	info, err := b.Lstat(p)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, &fs.PathError{Op: "open", Path: p, Err: syscall.EINVAL}
	}
	f, err := os.Open(filepath.Join(b.dir, p))
	if err != nil {
		return nil, err
	}
	return f, nil
}

// Readlink reads a test bottom symlink.
func (b dirBottom) Readlink(p string) (string, error) { return os.Readlink(filepath.Join(b.dir, p)) }

// CopyTo uses the production clone helper while keeping the bottom immutable.
func (b dirBottom) CopyTo(p, dst string) error { return clone.Tree(filepath.Join(b.dir, p), dst) }

// setup builds a lower tree from path→content (a trailing "/" makes an empty
// dir), all with mtime past, and fails the test if the overlay writes to it.
func setup(t *testing.T, files map[string]string) *FS {
	t.Helper()
	lower, upper := t.TempDir(), t.TempDir()
	for p, data := range files {
		full := filepath.Join(lower, p)
		if p[len(p)-1] == '/' {
			must(t, os.MkdirAll(full, 0o755))
			continue
		}
		must(t, os.MkdirAll(filepath.Dir(full), 0o755))
		must(t, os.WriteFile(full, []byte(data), 0o644))
	}
	must(t, filepath.Walk(lower, func(p string, _ os.FileInfo, err error) error {
		must(t, err)
		return os.Chtimes(p, past, past)
	}))
	before := snapshot(t, lower)
	t.Cleanup(func() {
		if after := snapshot(t, lower); !slices.Equal(before, after) {
			t.Errorf("lower changed:\nbefore %v\nafter  %v", before, after)
		}
	})
	f, err := New(filepath.Join(upper, "u"), nil, dirBottom{lower}, nil)
	must(t, err)
	return f
}

// snapshot records host content and metadata to detect accidental bottom writes.
func snapshot(t *testing.T, root string) []string {
	var out []string
	must(t, filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		must(t, err)
		data := ""
		if info.Mode().IsRegular() {
			b, err := os.ReadFile(p)
			must(t, err)
			data = string(b)
		}
		out = append(out, p+"|"+info.Mode().String()+"|"+info.ModTime().String()+"|"+data)
		return nil
	}))
	return out
}

// must fails a test immediately when a filesystem operation fails.
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// read returns a merged file's entire contents.
func read(t *testing.T, f *FS, p string) string {
	t.Helper()
	fh, err := f.Open(p)
	must(t, err)
	defer fh.Close()
	b, err := io.ReadAll(fh)
	must(t, err)
	return string(b)
}

// write writes test data through the overlay's public interface.
func write(t *testing.T, f *FS, p, data string, flag int) {
	t.Helper()
	fh, err := f.OpenFile(p, flag, 0o644)
	must(t, err)
	_, err = fh.Write([]byte(data))
	must(t, err)
	must(t, fh.Close())
}

// names returns sorted merged directory names.
func names(t *testing.T, f *FS, dir string) []string {
	t.Helper()
	infos, err := f.ReadDir(dir)
	must(t, err)
	var out []string
	for _, i := range infos {
		out = append(out, i.Name())
	}
	slices.Sort(out)
	return out
}

// assertNames compares a merged listing with the expected ordered names.
func assertNames(t *testing.T, f *FS, dir string, want ...string) {
	t.Helper()
	if got := names(t, f, dir); !slices.Equal(got, want) {
		t.Fatalf("ReadDir(%q) = %v, want %v", dir, got, want)
	}
}

// assertGone checks that a virtual path is absent.
func assertGone(t *testing.T, f *FS, p string) {
	t.Helper()
	if _, err := f.Lstat(p); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Lstat(%q) err = %v, want not-exist", p, err)
	}
}

// TestWriteCopiesUpAndKeepsLowerReadable isolates writes from the bottom.
func TestWriteCopiesUpAndKeepsLowerReadable(t *testing.T) {
	f := setup(t, map[string]string{"a/b.txt": "base"})
	if got := read(t, f, "a/b.txt"); got != "base" {
		t.Fatalf("read = %q", got)
	}
	write(t, f, "a/b.txt", "X", os.O_RDWR)
	if got := read(t, f, "a/b.txt"); got != "Xase" {
		t.Fatalf("after write = %q, want in-place edit of copied data", got)
	}
	if _, err := os.Stat(filepath.Join(f.Upper(), "a/b.txt")); err != nil {
		t.Fatal("upper lacks the copied-up file:", err)
	}
}

// Build tools decide freshness by mtime; copy-up must not touch it.
func TestMetadataCopyUpPreservesMtime(t *testing.T) {
	f := setup(t, map[string]string{"dir/f": "x"})
	must(t, f.Chmod("dir/f", 0o600))
	for _, p := range []string{"dir", "dir/f"} {
		info, err := f.Lstat(p)
		must(t, err)
		if !info.ModTime().Equal(past) {
			t.Errorf("%s mtime = %v, want %v", p, info.ModTime(), past)
		}
	}
	if info, _ := f.Lstat("dir/f"); info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", info.Mode())
	}
}

// Metadata changes on a symlink apply to the link itself: following it would
// write outside the checkout and, through a mount, deadlock the daemon (#20).
func TestMetadataChangesLeaveSymlinkTarget(t *testing.T) {
	f := setup(t, nil)
	victim := filepath.Join(t.TempDir(), "victim")
	must(t, os.WriteFile(victim, []byte("v"), 0o644))
	must(t, os.Chtimes(victim, past, past))
	before, err := os.Stat(victim)
	must(t, err)
	must(t, f.Symlink(victim, "link"))
	when := past.Add(time.Hour)
	must(t, f.Chtimes("link", when, when))
	// Linux and Windows symlinks have no mode of their own, and Windows has no
	// Unix owners: those hosts refuse the change as unsupported.
	unsupported := func(err error) bool {
		if err != nil && !errors.Is(err, errors.ErrUnsupported) {
			t.Fatal(err)
		}
		return err != nil
	}
	chmodRefused := unsupported(f.Chmod("link", 0o600))
	unsupported(f.Chown("link", -1, -1))
	target, err := os.Stat(victim)
	must(t, err)
	if !target.ModTime().Equal(past) || target.Mode() != before.Mode() {
		t.Fatalf("target changed: mtime %v, mode %v", target.ModTime(), target.Mode())
	}
	link, err := f.Lstat("link")
	must(t, err)
	if !link.ModTime().Equal(when) {
		t.Errorf("link mtime = %v, want %v", link.ModTime(), when)
	}
	if !chmodRefused && link.Mode().Perm() != 0o600 {
		t.Errorf("link mode = %v, want 0600", link.Mode())
	}
	// A zero time leaves that time unchanged.
	must(t, f.Chtimes("link", past, time.Time{}))
	link, err = f.Lstat("link")
	must(t, err)
	if !link.ModTime().Equal(when) {
		t.Errorf("link mtime after zero mtime = %v, want %v", link.ModTime(), when)
	}
}

// TestTruncatingOpenKeepsLowerMode preserves executable permissions on truncation.
func TestTruncatingOpenKeepsLowerMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a Windows host lower cannot hold 0750; checkout's TestEditKeepsExecutableMode covers Windows")
	}
	f := setup(t, map[string]string{"run.sh": "old"})
	lower := f.bottom.(dirBottom).dir
	must(t, os.Chmod(filepath.Join(lower, "run.sh"), 0o750))
	t.Cleanup(func() { os.Chmod(filepath.Join(lower, "run.sh"), 0o644) })
	write(t, f, "run.sh", "new", os.O_WRONLY|os.O_TRUNC)
	info, err := f.Lstat("run.sh")
	must(t, err)
	if got := read(t, f, "run.sh"); got != "new" || info.Mode().Perm() != 0o750 {
		t.Fatalf("content %q mode %v", got, info.Mode())
	}
}

// TestExclusiveCreateSeesLowerFile rejects creating an existing merged name.
func TestExclusiveCreateSeesLowerFile(t *testing.T) {
	f := setup(t, map[string]string{"f": "x"})
	_, err := f.OpenFile("f", os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if !errors.Is(err, fs.ErrExist) {
		t.Fatalf("err = %v, want exist", err)
	}
}

// TestRemoveWhitesOutAndRecreateShadows replaces whiteouts with recreated files.
func TestRemoveWhitesOutAndRecreateShadows(t *testing.T) {
	f := setup(t, map[string]string{"d/keep": "k", "d/gone": "g"})
	must(t, f.Remove("d/gone"))
	assertGone(t, f, "d/gone")
	assertNames(t, f, "d", "keep")

	write(t, f, "d/gone", "again", os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if got := read(t, f, "d/gone"); got != "again" {
		t.Fatalf("recreated = %q", got)
	}
	assertNames(t, f, "d", "gone", "keep")
}

// TestRecreatedDirectoryHidesOldLowerContents keeps deleted subtrees hidden.
func TestRecreatedDirectoryHidesOldLowerContents(t *testing.T) {
	f := setup(t, map[string]string{"d/x": "1", "d/sub/y": "2"})
	must(t, f.Remove("d/sub/y"))
	must(t, f.Remove("d/sub"))
	must(t, f.Remove("d/x"))
	must(t, f.Remove("d"))
	assertGone(t, f, "d")

	must(t, f.MkdirAll("d/sub", 0o755))
	assertNames(t, f, "d", "sub")
	assertNames(t, f, "d/sub")
	assertGone(t, f, "d/x")
}

// TestRemoveRefusesNonEmptyMergedDir checks emptiness in the merged view.
func TestRemoveRefusesNonEmptyMergedDir(t *testing.T) {
	f := setup(t, map[string]string{"d/x": "1"})
	if err := f.Remove("d"); !errors.Is(err, syscall.ENOTEMPTY) {
		t.Fatalf("err = %v, want ENOTEMPTY", err)
	}
}

// TestReadDirMergesLayers suppresses markers and shadowed lower names.
func TestReadDirMergesLayers(t *testing.T) {
	f := setup(t, map[string]string{"d/low": "l", "d/both": "lower", "d/del": "x", "d/.wh.sneaky": ""})
	write(t, f, "d/both", "upper", os.O_WRONLY|os.O_TRUNC)
	write(t, f, "d/new", "n", os.O_WRONLY|os.O_CREATE)
	must(t, f.Remove("d/del"))
	assertNames(t, f, "d", "both", "low", "new")
	if got := read(t, f, "d/both"); got != "upper" {
		t.Fatalf("both = %q", got)
	}
	assertNames(t, f, "", "d")
}

// TestReservedNamesCannotBeCreated prevents users from forging overlay metadata.
func TestReservedNamesCannotBeCreated(t *testing.T) {
	f := setup(t, map[string]string{"d/": ""})
	if _, err := f.Create("d/.wh.x"); err == nil {
		t.Fatal("created a whiteout name")
	}
	if err := f.MkdirAll("d/.wh..wh..opq", 0o755); err == nil {
		t.Fatal("created an opaque marker name")
	}
}

// TestRenameLowerFile masks both the old source and replaced destination.
func TestRenameLowerFile(t *testing.T) {
	f := setup(t, map[string]string{"a": "A", "b": "B"})
	must(t, f.Rename("a", "b"))
	assertGone(t, f, "a")
	if got := read(t, f, "b"); got != "A" {
		t.Fatalf("b = %q", got)
	}
	must(t, f.Rename("b", "c/../a"))
	assertNames(t, f, "", "a")
}

// TestRenameMergedDirectoryMovesWholeTree copies a complete merged subtree.
func TestRenameMergedDirectoryMovesWholeTree(t *testing.T) {
	f := setup(t, map[string]string{"d/low": "l", "d/sub/deep": "x", "d/del": "", "e/": ""})
	write(t, f, "d/sub/up", "u", os.O_WRONLY|os.O_CREATE)
	must(t, f.Remove("d/del"))
	must(t, f.Rename("d", "e"))

	assertGone(t, f, "d")
	assertNames(t, f, "e", "low", "sub")
	assertNames(t, f, "e/sub", "deep", "up")
	if got := read(t, f, "e/sub/deep"); got != "x" {
		t.Fatalf("deep = %q", got)
	}
	info, err := f.Lstat("e/sub/deep")
	must(t, err)
	if !info.ModTime().Equal(past) {
		t.Errorf("cloned file mtime = %v, want %v", info.ModTime(), past)
	}
	// The name is free again and must not resurrect lower's d.
	must(t, f.MkdirAll("d", 0o755))
	assertNames(t, f, "d")
}

// TestRenameOntoNonEmptyDirFails enforces directory replacement constraints.
func TestRenameOntoNonEmptyDirFails(t *testing.T) {
	f := setup(t, map[string]string{"a/x": "", "b/y": ""})
	if err := f.Rename("a", "b"); !errors.Is(err, syscall.ENOTEMPTY) {
		t.Fatalf("err = %v, want ENOTEMPTY", err)
	}
	if err := f.Rename("a", "a/x/inside"); !errors.Is(err, syscall.EINVAL) {
		t.Fatalf("err = %v, want EINVAL", err)
	}
}
