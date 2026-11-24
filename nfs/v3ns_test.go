package nfs

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"runtime"
	"sync/atomic"
	"syscall"
	"testing"
)

// renameErrFS fails every rename with a wrapped host error.
type renameErrFS struct {
	*testFS
	err error
}

// Rename returns the injected error, wrapped as os.Rename wraps errnos.
func (f *renameErrFS) Rename(from, to string) error {
	return &os.LinkError{Op: "rename", Old: from, New: to, Err: fmt.Errorf("wrapped: %w", f.err)}
}

// linkErrFS fails every link with a wrapped host error.
type linkErrFS struct {
	*testFS
	err error
}

// Link returns the injected error.
func (f *linkErrFS) Link(from, to string) error {
	return &os.LinkError{Op: "link", Old: from, New: to, Err: fmt.Errorf("wrapped: %w", f.err)}
}

// symlinkModeFS reports symlinks with mode 0755, so a client's 0777 always
// differs, and counts Chmod calls, which fail as following a dangling link
// does.
type symlinkModeFS struct {
	*testFS
	chmods atomic.Int32
}

// symlinkInfo reports a fixed symlink mode.
type symlinkInfo struct{ os.FileInfo }

// Mode is the link's type with permission bits 0755.
func (i symlinkInfo) Mode() os.FileMode { return i.FileInfo.Mode()&^os.ModePerm | 0o755 }

// Lstat reports symlinks with mode 0755.
func (f *symlinkModeFS) Lstat(p string) (os.FileInfo, error) {
	info, err := f.testFS.Lstat(p)
	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		return symlinkInfo{info}, nil
	}
	return info, err
}

// Chmod counts the call and fails like a chmod through a dangling link.
func (f *symlinkModeFS) Chmod(p string, _ os.FileMode) error {
	f.chmods.Add(1)
	return &os.PathError{Op: "chmod", Path: p, Err: os.ErrNotExist}
}

// xdrOpaque appends an XDR variable-length opaque, encoded independently of
// the encoder under test.
func xdrOpaque(dst, b []byte) []byte {
	dst = binary.BigEndian.AppendUint32(dst, uint32(len(b)))
	dst = append(dst, b...)
	return append(dst, make([]byte, -len(b)&3)...)
}

// handle is the wire form of file id's handle at generation 0, which
// testFS ids have until RetireHandle.
func handle(id uint64) []byte {
	var e encoder
	e.fh(id, 0)
	return e.buf[4:]
}

// raw appends pre-encoded arguments.
func raw(b []byte) func(*encoder) { return func(e *encoder) { e.buf = append(e.buf, b...) } }

// TestRenameNamespaceErrors checks that wrapped errnos from Rename keep
// their statuses on the wire, with two all-absent wcc_data (16 bytes), and
// that EXDEV is numerically NFS3ERR_XDEV (18).
func TestRenameNamespaceErrors(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want status
	}{
		{syscall.EXDEV, stXDev},
		{syscall.ENOTEMPTY, stNotEmpty},
		{syscall.EEXIST, stExist},
		{syscall.EACCES, stAccess},
		{syscall.EPERM, stPerm},
		{syscall.EROFS, stROFS},
		{syscall.ENOENT, stNoEnt},
		{syscall.EIO, stIO},
	} {
		t.Run(tc.err.Error(), func(t *testing.T) {
			fs := &renameErrFS{testFS: newTestFS(t), err: tc.err}
			fs.write(t, "old", "")
			c := v3server(t, fs)
			args := xdrOpaque(nil, handle(1))
			args = xdrOpaque(args, []byte("old"))
			args = xdrOpaque(args, handle(1))
			args = xdrOpaque(args, []byte("new"))
			stat, d := c.call(progNFS, 3, v3Rename, raw(args))
			if stat != rpcSuccess || len(d.buf) != 4+16 {
				t.Fatalf("accept_stat %d, result of %d bytes, want status and 16", stat, len(d.buf))
			}
			if got := binary.BigEndian.Uint32(d.buf); got != uint32(tc.want) || !bytes.Equal(d.buf[4:], make([]byte, 16)) {
				t.Fatalf("result %x, want status %d and two empty wcc_data", d.buf, tc.want)
			}
			if tc.err == syscall.EXDEV && binary.BigEndian.Uint32(d.buf) != 18 {
				t.Fatal("EXDEV did not encode NFS3ERR_XDEV (18)")
			}
		})
	}
}

// TestV3Rename checks a successful RENAME: both directories' wcc_data, the
// moved file keeping its handle, and the replaced file's going stale.
func TestV3Rename(t *testing.T) {
	fs := newTestFS(t)
	if err := os.Mkdir(fs.host("d"), 0o755); err != nil {
		t.Fatal(err)
	}
	fs.write(t, "a", "moved")
	fs.write(t, "d/b", "replaced")
	c := v3server(t, fs)
	moved, replaced, dir := fs.FileID("a"), fs.FileID("d/b"), fs.FileID("d")
	st, d := c.v3call(v3Rename, func(e *encoder) { dirop(e, 1, "a"); dirop(e, dir, "b") })
	if st != stOK {
		t.Fatalf("RENAME: %d", st)
	}
	for _, want := range []uint64{1, dir} {
		if !d.bool() {
			t.Fatal("no pre-op attributes")
		}
		d.fixed(24)
		if a, ok := postOpOf(t, d); !ok || a.fileid != want {
			t.Fatalf("post-op attributes of %d: %+v", want, a)
		}
	}
	if len(d.buf) != 0 {
		t.Fatal("trailing bytes")
	}
	if p, ok := fs.Path(moved, 0); !ok || p != "d/b" {
		t.Fatalf("moved file's handle resolves to %q %v", p, ok)
	}
	if _, ok := fs.Path(replaced, 0); ok {
		t.Fatal("replaced file's handle still resolves")
	}
	if got, _ := os.ReadFile(fs.host("d/b")); string(got) != "moved" {
		t.Fatalf("target holds %q", got)
	}
	for _, tc := range []struct {
		from, to string
		want     status
	}{
		{"missing", "x", stNoEnt},
		{".", "x", stInval},
		{"d", "..", stInval},
	} {
		if st, d := c.v3call(v3Rename, func(e *encoder) { dirop(e, 1, tc.from); dirop(e, 1, tc.to) }); st != tc.want || len(d.buf) != 16 {
			t.Errorf("RENAME %q to %q: %d, want %d", tc.from, tc.to, st, tc.want)
		}
	}
}

// TestLinkHandEncodedRFC1813 sends a hand-encoded LINK3args (file handle,
// then diropargs3) and checks the complete LINK3resok layout: status, the
// file's post_op_attr and the directory's wcc_data, with no handle. The
// names then share an inode and its writes.
func TestLinkHandEncodedRFC1813(t *testing.T) {
	fs := newTestFS(t)
	fs.write(t, "original", "original bytes")
	if err := os.Mkdir(fs.host("links"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := v3server(t, fs)
	file, dir := fs.FileID("original"), fs.FileID("links")
	args := xdrOpaque(nil, handle(file))
	args = xdrOpaque(args, handle(dir))
	args = xdrOpaque(args, []byte("linked"))
	stat, d := c.call(progNFS, 3, v3Link, raw(args))
	if stat != rpcSuccess {
		t.Fatalf("accept_stat %d", stat)
	}
	b := d.buf
	word := func(at int) uint32 { return binary.BigEndian.Uint32(b[at:]) }
	// status 4 + (follows 4 + fattr3 84) + (follows 4 + wcc_attr 24) + (follows 4 + fattr3 84)
	if len(b) != 4+88+28+88 {
		t.Fatalf("LINK result of %d bytes, want %d", len(b), 4+88+28+88)
	}
	if word(0) != 0 || word(4) != 1 || word(92) != 1 || word(120) != 1 {
		t.Fatalf("status %d, follows %d %d %d", word(0), word(4), word(92), word(120))
	}
	fa := fattr3Of(&decoder{buf: b[8:92]})
	da := fattr3Of(&decoder{buf: b[124:]})
	if fa.fileid != file || fa.typ != uint32(typeReg) || da.fileid != dir || da.typ != uint32(typeDir) {
		t.Fatalf("file attributes %+v, directory %+v", fa, da)
	}
	if fa.nlink != 2 && runtime.GOOS != "windows" {
		t.Fatalf("file link count %d", fa.nlink)
	}
	orig, err := os.Stat(fs.host("original"))
	if err != nil {
		t.Fatal(err)
	}
	linked, err := os.Stat(fs.host("links/linked"))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(orig, linked) {
		t.Fatal("LINK did not make a hard link")
	}
	if err := os.WriteFile(fs.host("links/linked"), []byte("new bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(fs.host("original")); err != nil || string(got) != "new bytes" {
		t.Fatalf("write through the link: %q %v", got, err)
	}
}

// linkArgs encodes LINK of file to name in dir.
func linkArgs(file, dir uint64, name string) []byte {
	args := xdrOpaque(nil, handle(file))
	args = xdrOpaque(args, handle(dir))
	return xdrOpaque(args, []byte(name))
}

// TestLinkUnsupportedReturnsRFCFailure checks a backend refusing links with
// ENOTSUP gives NOTSUPP and LINK3resfail: the file's post_op_attr and the
// directory's wcc_data, all absent (12 bytes).
func TestLinkUnsupportedReturnsRFCFailure(t *testing.T) {
	fs := &linkErrFS{testFS: newTestFS(t), err: syscall.ENOTSUP}
	fs.write(t, "original", "")
	c := v3server(t, fs)
	stat, d := c.call(progNFS, 3, v3Link, raw(linkArgs(fs.FileID("original"), 1, "linked")))
	want := binary.BigEndian.AppendUint32(nil, uint32(stNotSupp))
	want = append(want, make([]byte, 12)...)
	if stat != rpcSuccess || !bytes.Equal(d.buf, want) {
		t.Fatalf("accept_stat %d, result %x, want %x", stat, d.buf, want)
	}
}

// TestLinkErrors checks wrapped link errnos keep their statuses with a
// 12-byte failure body, and the statuses LINK itself decides.
func TestLinkErrors(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want status
	}{
		{syscall.EXDEV, stXDev},
		{syscall.EEXIST, stExist},
		{syscall.EPERM, stPerm},
		{syscall.EACCES, stAccess},
		{syscall.EROFS, stROFS},
		{syscall.ENOENT, stNoEnt},
		{syscall.EMLINK, stMLink},
	} {
		t.Run(tc.err.Error(), func(t *testing.T) {
			fs := &linkErrFS{testFS: newTestFS(t), err: tc.err}
			fs.write(t, "original", "")
			c := v3server(t, fs)
			st, d := c.v3call(v3Link, raw(linkArgs(fs.FileID("original"), 1, "linked")))
			if st != tc.want || !bytes.Equal(d.buf, make([]byte, 12)) {
				t.Fatalf("status %d body %x, want %d and 12 zero bytes", st, d.buf, tc.want)
			}
		})
	}

	fs := newTestFS(t)
	fs.write(t, "f", "")
	if err := os.Mkdir(fs.host("d"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := v3server(t, fs)
	f, d := fs.FileID("f"), fs.FileID("d")
	for _, tc := range []struct {
		name      string
		file, dir uint64
		link      string
		want      status
	}{
		{"stale file", 999, 1, "x", stStale},
		{"stale directory", f, 999, "x", stStale},
		{"directory source", d, 1, "x", stPerm},
		{"file as directory", f, f, "x", stNotDir},
		{"existing name", f, 1, "d", stExist},
		{"dot", f, 1, "..", stExist},
	} {
		st, body := c.v3call(v3Link, raw(linkArgs(tc.file, tc.dir, tc.link)))
		if st != tc.want || len(body.buf) != 12 {
			t.Errorf("%s: %d with %d body bytes, want %d with 12", tc.name, st, len(body.buf), tc.want)
		}
	}
}

// TestLinkTruncatedArgs checks LINK arguments missing the new name are
// GARBAGE_ARGS and make no link.
func TestLinkTruncatedArgs(t *testing.T) {
	fs := newTestFS(t)
	fs.write(t, "original", "")
	c := v3server(t, fs)
	args := xdrOpaque(nil, handle(fs.FileID("original")))
	args = xdrOpaque(args, handle(1))
	if stat, _ := c.call(progNFS, 3, v3Link, raw(args)); stat != rpcGarbageArgs {
		t.Fatalf("accept_stat %d, want GARBAGE_ARGS", stat)
	}
	if stat, _ := c.call(progNFS, 3, v3Link, raw(args[:len(args)-2])); stat != rpcGarbageArgs {
		t.Fatalf("cut handle: accept_stat %d, want GARBAGE_ARGS", stat)
	}
}

// TestSymlinkModeDoesNotFollowDanglingTarget sends SYMLINK with mode 0777
// and a missing target. The fixture reports the link as 0755, so a mode
// change is always due; it must not reach Chmod, which follows the link.
// SETATTR of a mode on the link is skipped alike.
func TestSymlinkModeDoesNotFollowDanglingTarget(t *testing.T) {
	fs := &symlinkModeFS{testFS: newTestFS(t)}
	c := v3server(t, fs)
	args := xdrOpaque(nil, handle(1))
	args = xdrOpaque(args, []byte("link"))
	for _, v := range []uint32{1, 0o777, 0, 0, 0, 0, 0} { // sattr3: mode 0777 only
		args = binary.BigEndian.AppendUint32(args, v)
	}
	args = xdrOpaque(args, []byte("nonexistent-target"))
	if st, _ := c.v3call(v3Symlink, raw(args)); st != stOK {
		t.Fatalf("SYMLINK: %d", st)
	}
	if n := fs.chmods.Load(); n != 0 {
		t.Fatalf("Chmod called %d times for a symlink", n)
	}
	if target, err := os.Readlink(fs.host("link")); err != nil || target != "nonexistent-target" {
		t.Fatalf("Readlink = %q, %v", target, err)
	}
	if _, err := os.Stat(fs.host("link")); !os.IsNotExist(err) {
		t.Fatalf("symlink target exists: %v", err)
	}

	link := fs.FileID("link")
	if st, _ := c.v3call(v3SetAttr, func(e *encoder) { e.fh(link, 0); modeSattr(e, 0o600); e.u32(0) }); st != stOK {
		t.Fatalf("SETATTR mode of a symlink: %d", st)
	}
	if n := fs.chmods.Load(); n != 0 {
		t.Fatalf("SETATTR called Chmod %d times for a symlink", n)
	}
	st, _ := c.v3call(v3SetAttr, func(e *encoder) {
		e.fh(link, 0)
		for _, v := range []uint32{0, 0, 0, 1, 0, 0, 0, 0} { // size 0
			e.u32(v)
		}
		e.u32(0)
	})
	if st != stNotSupp {
		t.Fatalf("SETATTR size of a symlink: %d, want NOTSUPP", st)
	}
}
