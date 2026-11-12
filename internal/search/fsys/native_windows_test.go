//go:build windows

package fsys

import (
	"errors"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

func TestFiletimeNanosSaturates(t *testing.T) {
	for _, c := range []struct{ ticks, want int64 }{
		{unixEpochTicks, 0},
		{unixEpochTicks + 10, 1000},
		{unixEpochTicks - 10, -1000},
		{math.MaxInt64, math.MaxInt64},
		{math.MinInt64, math.MinInt64},
		{math.MinInt64 + unixEpochTicks, math.MinInt64},
		{math.MinInt64 + unixEpochTicks + 1, math.MinInt64},
	} {
		if got := filetimeNanos(c.ticks); got != c.want {
			t.Errorf("filetimeNanos(%d) = %d, want %d", c.ticks, got, c.want)
		}
	}
}

// wantInfo describes the regular file at path as the os package sees it.
func wantInfo(t *testing.T, path string) FileInfo {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return FileInfo{Type: File, Size: fi.Size(), Stamp: Stamp{Mtime: fi.ModTime().UnixNano()}}
}

// checkStat requires every spelling to stat as the file want describes.
func checkStat(t *testing.T, s Session, want FileInfo, paths ...string) {
	t.Helper()
	for _, p := range paths {
		got, err := s.Stat(p, true)
		if err != nil {
			t.Errorf("Stat(%q): %v", p, err)
			continue
		}
		if got.Type != want.Type || got.Size != want.Size || got.Stamp.Mtime != want.Stamp.Mtime {
			t.Errorf("Stat(%q) = %+v, want type %v size %d mtime %d", p, got, want.Type, want.Size, want.Stamp.Mtime)
		}
	}
}

// TestStatPathForms checks every spelling of one file stats alike, through
// the direct NT query after Win32 normalization or through Win32.
func TestStatPathForms(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "d", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "d", "sub", "f.txt")
	if err := os.WriteFile(file, []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}
	want := wantInfo(t, file)
	s := Native.NewSession()
	vol := filepath.VolumeName(root)
	rest := strings.TrimPrefix(root, vol)
	slash := filepath.ToSlash(root)

	paths := []string{
		file,
		slash + "/d/sub/f.txt",
		root + `\d\.\sub\..\sub\f.txt`,
		root + `\d\\sub\\\f.txt`,
		root + `\d\sub\f.txt.`,
		root + `\d\sub\f.txt `,
		root + `\d\sub\..\sub\f.txt`,
		`\\?\` + file,
		`\\.\` + file,
		strings.ToUpper(file),
	}
	if strings.HasPrefix(vol, `\\`) {
		t.Skipf("temp dir %q is a UNC path", root)
	}
	if unc := `\\localhost\` + vol[:1] + `$` + rest + `\d\sub\f.txt`; func() bool { _, err := os.Stat(unc); return err == nil }() {
		paths = append(paths, unc, `//localhost/`+vol[:1]+`$`+filepath.ToSlash(rest)+`/d/./sub/f.txt`)
	}
	checkStat(t, s, want, paths...)

	// Relative, drive-relative, and rooted forms resolve through Win32.
	t.Chdir(root)
	checkStat(t, s, want,
		`d\sub\f.txt`,
		`.\d/sub\..\sub/f.txt`,
		vol+`d\sub\f.txt`,
		rest+`\d\sub\f.txt`,
	)

	// Verbatim paths are not normalized: the trailing dot names another file.
	for _, p := range []string{`\\?\` + file + "."} {
		if _, err := s.Stat(p, true); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Stat(%q) = %v, want not-exist", p, err)
		}
	}

	if info, err := s.Stat(filepath.Join(root, "d"), true); err != nil || info.Type != Dir || info.Size != -1 {
		t.Errorf("directory stat = %+v, %v", info, err)
	}
	if info, err := s.Stat(root+`\d\.\sub\..`, false); err != nil || info.Type != Dir {
		t.Errorf("nofollow directory stat = %+v, %v", info, err)
	}
	for _, follow := range []bool{true, false} {
		if _, err := s.Stat(root+`\d\missing`, follow); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("missing (follow=%v): %v", follow, err)
		}
	}
}

// TestStatLongPath covers paths beyond MAX_PATH, plain and needing
// normalization, through Stat, OpenFile, and Identify.
func TestStatLongPath(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := root
	for range 12 {
		dir = filepath.Join(dir, strings.Repeat("x", 30))
	}
	if err := os.MkdirAll(`\\?\`+dir, 0o755); err != nil {
		t.Skipf("long paths unavailable: %v", err)
	}
	file := dir + `\long.txt`
	if err := os.WriteFile(`\\?\`+file, []byte("long"), 0o644); err != nil {
		t.Fatal(err)
	}
	if len(file) < 300 {
		t.Fatalf("path too short: %d", len(file))
	}
	want := wantInfo(t, `\\?\`+file)
	s := Native.NewSession()
	spellings := []string{
		file,
		`\\?\` + file,
		filepath.ToSlash(file),
		dir + `\.\long.txt`,
		dir + `\..\` + strings.Repeat("x", 30) + `\long.txt`,
	}
	checkStat(t, s, want, spellings...)

	id, err := s.Identify(`\\?\` + file)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range spellings {
		if got, err := s.Identify(p); err != nil || got != id {
			t.Errorf("Identify(%q) = %v, %v; want %v", p, got, err, id)
		}
		h, info, err := s.OpenFile(Handle{}, p)
		if err != nil {
			t.Errorf("OpenFile(%q): %v", p, err)
			continue
		}
		data, err := s.Read(h, nil, 1<<20)
		s.Close(h)
		if err != nil || string(data) != "long" || info.Size != 4 {
			t.Errorf("OpenFile(%q) read %q size %d: %v", p, data, info.Size, err)
		}
	}
}

// TestStatSymlink checks nofollow and following stats of a symlink,
// including a dangling one.
func TestStatSymlink(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "target.txt")
	if err := os.WriteFile(target, []byte("12345"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link.txt")
	dangling := filepath.Join(root, "dangling.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join(root, "missing"), dangling); err != nil {
		t.Fatal(err)
	}
	s := Native.NewSession()
	want := wantInfo(t, target)
	checkStat(t, s, want, link, filepath.ToSlash(link), `\\?\`+link)

	for _, p := range []string{link, dangling, `\\?\` + link} {
		info, err := s.Stat(p, false)
		if err != nil || info.Type != Symlink || info.Size != -1 {
			t.Errorf("Stat(%q, false) = %+v, %v; want a symlink", p, info, err)
		}
	}
	if _, err := s.Stat(dangling, true); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("dangling follow: %v, want not-exist", err)
	}
	t.Chdir(root)
	if info, err := s.Stat("link.txt", false); err != nil || info.Type != Symlink {
		t.Errorf("relative nofollow = %+v, %v; want a symlink", info, err)
	}
	if info, err := s.Stat("link.txt", true); err != nil || info.Type != File || info.Size != 5 {
		t.Errorf("relative follow = %+v, %v", info, err)
	}
}

// TestEmbeddedNULRejected requires raw names with a NUL to fail before any
// syscall could truncate them.
func TestEmbeddedNULRejected(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	s := Native.NewSession()
	h, err := s.OpenDir(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(h)
	for _, follow := range []bool{true, false} {
		for _, p := range []string{root + "\\a\x00b", `\\?\` + root + "\\a\x00b", "a\x00b"} {
			if _, err := s.Stat(p, follow); !errors.Is(err, windows.ERROR_INVALID_NAME) {
				t.Errorf("Stat(%q, %v) = %v, want ERROR_INVALID_NAME", p, follow, err)
			}
		}
	}
	if _, err := s.OpenChild(h, "a\x00b"); !errors.Is(err, windows.ERROR_INVALID_NAME) {
		t.Errorf("OpenChild: %v", err)
	}
	if _, _, err := s.OpenFile(h, "a\x00b"); !errors.Is(err, windows.ERROR_INVALID_NAME) {
		t.Errorf("OpenFile(dir): %v", err)
	}
	if _, _, err := s.OpenFile(Handle{}, root+"\\a\x00b"); !errors.Is(err, windows.ERROR_INVALID_NAME) {
		t.Errorf("OpenFile(path): %v", err)
	}
	if _, err := s.OpenDir(root + "\x00"); !errors.Is(err, windows.ERROR_INVALID_NAME) {
		t.Errorf("OpenDir: %v", err)
	}
	if _, err := s.Identify(root + "\x00"); !errors.Is(err, windows.ERROR_INVALID_NAME) {
		t.Errorf("Identify: %v", err)
	}
}

// TestUnpairedSurrogateName creates a file whose name holds a lone
// surrogate, which only UTF-16 can spell, and requires its WTF-8 name to list,
// stat, and open exactly.
func TestUnpairedSurrogateName(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	name16 := []uint16{'s', 0xD800, 'x'}
	full, err := windows.UTF16FromString(`\\?\` + root + `\`)
	if err != nil {
		t.Fatal(err)
	}
	full = append(full[:len(full)-1], name16...)
	full = append(full, 0)
	h, err := windows.CreateFile(&full[0], windows.GENERIC_WRITE, 0, nil, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Skipf("unpaired surrogate names unavailable: %v", err)
	}
	windows.CloseHandle(h)
	name := string(appendWTF8(nil, name16))
	entries, err := ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != name {
		t.Fatalf("metadata listing = %v, want one entry named %q", entries, name)
	}
	if info, err := entries[0].Info(); err != nil || info.Name() != name {
		t.Fatalf("metadata info = %v, %v; want name %q", info, err, name)
	}

	s := Native.NewSession()
	dir, err := s.OpenDir(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(dir)
	ls, err := s.List(dir, Full, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ls.Entries) != 1 || string(ls.Name(&ls.Entries[0])) != name {
		t.Fatalf("listing = %d entries, first %q; want %q", len(ls.Entries), ls.Name(&ls.Entries[0]), name)
	}
	for _, p := range []string{
		root + `\` + name,
		root + `\.\` + name,
		root + `/` + name,
		`\\?\` + root + `\` + name,
	} {
		if info, err := s.Stat(p, true); err != nil || info.Type != File {
			t.Errorf("Stat(%q) = %+v, %v", p, info, err)
		}
	}
	fh, info, err := s.OpenFile(dir, name)
	if err != nil || info.Type != File {
		t.Fatalf("OpenFile = %+v, %v", info, err)
	}
	s.Close(fh)
	if _, _, err := s.OpenFile(Handle{}, root+`\.\`+name); err != nil {
		t.Errorf("OpenFile(path with surrogate): %v", err)
	}
}

// TestIdentifyDirSurvivesRename requires a directory handle to keep its
// identity while the directory is renamed, which the delete-sharing open
// allows, and to match what a path to the directory identifies as. NTFS
// refuses to rename a directory while anything beneath it is open, whatever
// that handle shares, so the child is closed first and reopened after.
func TestIdentifyDirSurvivesRename(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "before", "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := Native.NewSession()
	h, err := s.OpenDir(filepath.Join(root, "before"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(h)
	id, err := s.IdentifyDir(h)
	if err != nil || id == (ID{}) {
		t.Fatalf("IdentifyDir = %v, %v", id, err)
	}
	if got, err := s.Identify(filepath.Join(root, "before")); err != nil || got != id {
		t.Fatalf("Identify(path) = %v, %v; want %v", got, err, id)
	}
	child, err := s.OpenChild(h, "child")
	if err != nil {
		t.Fatal(err)
	}
	childID, err := s.IdentifyDir(child)
	s.Close(child)
	if err != nil || childID == id || childID.Dev != id.Dev {
		t.Fatalf("child identity = %v, %v; parent %v", childID, err, id)
	}
	if got, err := s.Identify(filepath.Join(root, "before", "child")); err != nil || got != childID {
		t.Fatalf("Identify(child path) = %v, %v; want %v", got, err, childID)
	}

	if err := os.Rename(filepath.Join(root, "before"), filepath.Join(root, "after")); err != nil {
		t.Fatalf("rename with the directory open: %v", err)
	}
	if got, err := s.IdentifyDir(h); err != nil || got != id {
		t.Errorf("IdentifyDir after rename = %v, %v; want %v", got, err, id)
	}
	if got, err := s.Identify(filepath.Join(root, "after")); err != nil || got != id {
		t.Errorf("Identify(new path) = %v, %v; want %v", got, err, id)
	}
	if _, err := s.Identify(filepath.Join(root, "before")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Identify(old path) = %v, want not-exist", err)
	}
	if child, err = s.OpenChild(h, "child"); err != nil {
		t.Fatalf("OpenChild after rename: %v", err)
	}
	defer s.Close(child)
	if got, err := s.IdentifyDir(child); err != nil || got != childID {
		t.Errorf("child IdentifyDir after rename = %v, %v; want %v", got, err, childID)
	}
}

// readDirSorted lists dir through the native metadata path, sorted by name.
func readDirSorted(t *testing.T, dir string) []os.DirEntry {
	t.Helper()
	s := Native.NewSession().(*session)
	s.dir = dir
	entries, err := s.readDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	entries = slices.Clone(entries)
	slices.SortFunc(entries, func(a, b os.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return entries
}

// sameInfo requires the same observable metadata, sizes included.
func sameInfo(t *testing.T, label string, got, want os.FileInfo) {
	t.Helper()
	if got.Name() != want.Name() || got.Mode() != want.Mode() || !got.ModTime().Equal(want.ModTime()) || got.IsDir() != want.IsDir() {
		t.Errorf("%s: got %q %v %v, want %q %v %v", label, got.Name(), got.Mode(), got.ModTime(), want.Name(), want.Mode(), want.ModTime())
	}
	gd, ok1 := got.Sys().(*syscall.Win32FileAttributeData)
	wd, ok2 := want.Sys().(*syscall.Win32FileAttributeData)
	if !ok1 || !ok2 {
		t.Fatalf("%s: Sys = %T and %T, want *syscall.Win32FileAttributeData", label, got.Sys(), want.Sys())
	}
	if got.Size() != want.Size() {
		t.Errorf("%s: size %d, want %d", label, got.Size(), want.Size())
	}
	if *gd != *wd {
		t.Errorf("%s: Sys = %+v, want %+v", label, *gd, *wd)
	}
}

// TestReadDirMetadata compares the native metadata listing with os.ReadDir and
// os.Lstat, and requires ordinary entries to come from the native listing
// rather than an os fallback.
func TestReadDirMetadata(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"plain.txt": "hello", "empty": "", "ro.txt": "readonly", "Zed.bin": strings.Repeat("z", 70000)} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(root, "ro.txt"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	hidden := filepath.Join(root, "hidden.txt")
	if err := os.WriteFile(hidden, []byte("h"), 0o644); err != nil {
		t.Fatal(err)
	}
	if p, err := syscall.UTF16PtrFromString(hidden); err != nil {
		t.Fatal(err)
	} else if err := syscall.SetFileAttributes(p, syscall.FILE_ATTRIBUTE_HIDDEN); err != nil {
		t.Fatal(err)
	}
	reparse := map[string]bool{}
	if err := os.Symlink(filepath.Join(root, "plain.txt"), filepath.Join(root, "filelink")); err == nil {
		reparse["filelink"] = true
		if err := os.Symlink(filepath.Join(root, "subdir"), filepath.Join(root, "dirlink")); err != nil {
			t.Fatal(err)
		}
		reparse["dirlink"] = true
		if err := os.Symlink(filepath.Join(root, "missing"), filepath.Join(root, "dangling")); err != nil {
			t.Fatal(err)
		}
		reparse["dangling"] = true
	}
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", filepath.Join(root, "junction"), filepath.Join(root, "subdir")).CombinedOutput(); err == nil {
		reparse["junction"] = true
	} else {
		t.Logf("junction unavailable: %v: %s", err, out)
	}

	got := readDirSorted(t, root)
	want, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("%d entries, want %d", len(got), len(want))
	}
	for i, e := range got {
		w := want[i]
		if e.Name() != w.Name() {
			t.Errorf("entry %d: %q, want %q", i, e.Name(), w.Name())
			continue
		}
		_, native := e.(*winInfo)
		if native == reparse[e.Name()] {
			t.Errorf("%s: native listing entry = %v, want %v", e.Name(), native, !reparse[e.Name()])
		}
		info, err := e.Info()
		if err != nil {
			t.Errorf("%s: Info: %v", e.Name(), err)
			continue
		}
		lstat, err := os.Lstat(filepath.Join(root, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if reparse[e.Name()] {
			// os.ReadDir cannot tell a reparse point's kind from the directory
			// record and reports it ModeIrregular; os.Lstat is the oracle for
			// every entry method.
			if e.Name() != lstat.Name() || e.Type() != lstat.Mode().Type() || e.IsDir() != lstat.IsDir() {
				t.Errorf("%s: entry %q %v dir=%v, Lstat %q %v dir=%v", e.Name(), e.Name(), e.Type(), e.IsDir(), lstat.Name(), lstat.Mode().Type(), lstat.IsDir())
			}
			sameInfo(t, e.Name()+" vs Lstat", info, lstat)
			continue
		}
		if e.Type() != w.Type() || e.IsDir() != w.IsDir() {
			t.Errorf("%s: %v dir=%v, os.ReadDir %v dir=%v", e.Name(), e.Type(), e.IsDir(), w.Type(), w.IsDir())
		}
		entryInfo, err := w.Info()
		if err != nil {
			t.Fatal(err)
		}
		sameInfo(t, e.Name()+" vs ReadDir", info, entryInfo)
		// Directory entries match os.ReadDir exactly (checked above, size
		// included). A fresh Lstat sees the same times, size, and mode for
		// unchanged files; a directory's size comes from a different query
		// there, so only its mode and times are compared.
		if info.Mode() != lstat.Mode() || !info.ModTime().Equal(lstat.ModTime()) || (!info.IsDir() && info.Size() != lstat.Size()) {
			t.Errorf("%s: %v %v %d, Lstat %v %v %d", e.Name(), info.Mode(), info.ModTime(), info.Size(), lstat.Mode(), lstat.ModTime(), lstat.Size())
		}
		ld, id := lstat.Sys().(*syscall.Win32FileAttributeData), info.Sys().(*syscall.Win32FileAttributeData)
		if ld.FileAttributes != id.FileAttributes || ld.CreationTime != id.CreationTime || ld.LastWriteTime != id.LastWriteTime {
			t.Errorf("%s: Sys %+v, Lstat %+v", e.Name(), *id, *ld)
		}
	}
	for _, e := range got {
		if e.Name() == "ro.txt" {
			if info, _ := e.Info(); info.Mode() != 0o444 {
				t.Errorf("read-only mode = %v, want 0444", info.Mode())
			}
		}
	}
}

// TestReadDirRejectsFile requires a non-directory path to fail rather than
// list.
func TestReadDirRejectsFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	s := Native.NewSession().(*session)
	s.dir = file
	if _, err := s.readDir(file); !errors.Is(err, windows.ERROR_DIRECTORY) {
		t.Errorf("readDir(file) = %v, want ERROR_DIRECTORY", err)
	}
	s.dir = filepath.Join(filepath.Dir(file), "missing")
	if _, err := s.readDir(s.dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("readDir(missing) = %v, want not-exist", err)
	}
}
