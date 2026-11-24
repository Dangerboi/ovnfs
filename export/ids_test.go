//go:build !windows

package export_test

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/checkout"
	exportfs "github.com/stencil-hq/ovnfs/export"
	"github.com/stencil-hq/ovnfs/internal/nfstest"
	"github.com/stencil-hq/ovnfs/overlay"
)

// writeFile writes data to the existing file p through the export.
func writeFile(t *testing.T, nodes *exportfs.Nodes, p, data string) {
	t.Helper()
	f, err := nodes.OpenFile(p, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte(data)); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// resolves fails the test unless id resolves to want.
func resolves(t *testing.T, nodes *exportfs.Nodes, id uint64, want string) {
	t.Helper()
	if p, ok := nodes.Path(id); !ok || p != want {
		t.Fatalf("id %#x resolves to %q (live %v), want %q", id, p, ok, want)
	}
}

// TestIDSurvivesCopyUpAndRename keeps a file's id, which clients cache as its
// inode number, when its first write copies it up from the base and when it
// or its directory is renamed.
func TestIDSurvivesCopyUpAndRename(t *testing.T) {
	f := newStack(t, map[string]string{"dir/file": "lower"})
	f.create(t, "app")
	nodes := f.export.Nodes()
	id := nodes.ID("app/dir/file")
	if id == 0 {
		t.Fatal("base file has no id")
	}
	writeFile(t, nodes, "app/dir/file", "upper")
	if got := nodes.ID("app/dir/file"); got != id {
		t.Fatalf("copy-up changed the id from %#x to %#x", id, got)
	}
	if err := nodes.Rename("app/dir/file", "app/dir/moved"); err != nil {
		t.Fatal(err)
	}
	if got := nodes.ID("app/dir/moved"); got != id {
		t.Fatalf("rename changed the id from %#x to %#x", id, got)
	}
	resolves(t, nodes, id, "app/dir/moved")
	dir := nodes.ID("app/dir")
	if err := nodes.Rename("app/dir", "app/renamed"); err != nil {
		t.Fatal(err)
	}
	resolves(t, nodes, id, "app/renamed/moved")
	resolves(t, nodes, dir, "app/renamed")

	created, err := nodes.Create("app/new")
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := created.(interface{ Stat() (os.FileInfo, error) })
	if !ok {
		t.Fatal("an open export file cannot describe itself")
	}
	info, err := stat.Stat()
	created.Close()
	if err != nil {
		t.Fatal(err)
	}
	fresh := nodes.IDOf("app/new", info)
	if fresh == 0 || fresh != nodes.ID("app/new") {
		t.Fatalf("open file and path give ids %#x and %#x", fresh, nodes.ID("app/new"))
	}
	if err := nodes.Rename("app/new", "app/renamed/new"); err != nil {
		t.Fatal(err)
	}
	resolves(t, nodes, fresh, "app/renamed/new")
}

// TestIDsDifferAcrossCheckouts gives the same base file in two checkouts two
// ids, so a client never confuses them.
func TestIDsDifferAcrossCheckouts(t *testing.T) {
	f := newStack(t, map[string]string{"file": "lower"})
	f.create(t, "a")
	f.create(t, "b")
	nodes := f.export.Nodes()
	for _, p := range []string{"", "/file"} {
		a, b := nodes.ID("a"+p), nodes.ID("b"+p)
		if a == 0 || a == b {
			t.Fatalf("a%s and b%s have ids %#x and %#x", p, p, a, b)
		}
		resolves(t, nodes, a, "a"+p)
		resolves(t, nodes, b, "b"+p)
	}
	entries, err := nodes.ReadDir("")
	if err != nil {
		t.Fatal(err)
	}
	for _, info := range entries {
		if got, want := nodes.ChildID("", info), nodes.ID(info.Name()); got != want {
			t.Fatalf("listed checkout %s has id %#x, lookup %#x", info.Name(), got, want)
		}
	}
	resolves(t, nodes, nodes.ID(""), "")
}

// TestRemovedIDIsStale makes the id of a removed file, upper or base, name
// nothing.
func TestRemovedIDIsStale(t *testing.T) {
	f := newStack(t, map[string]string{"base": "lower"})
	f.create(t, "app")
	nodes := f.export.Nodes()
	created, err := nodes.Create("app/upper")
	if err != nil {
		t.Fatal(err)
	}
	created.Close()
	for _, p := range []string{"app/base", "app/upper"} {
		id := nodes.ID(p)
		if err := nodes.Remove(p); err != nil {
			t.Fatal(err)
		}
		if got, ok := nodes.Path(id); ok {
			t.Fatalf("removed %s's id still resolves to %q", p, got)
		}
	}
}

// TestRetireHandleKeepsFileID makes RetireHandle's old handle stale while a
// fresh lookup gets a working handle with the same fileid, and answers a
// legacy 8-byte handle STALE, so a client looks the name up again.
func TestRetireHandleKeepsFileID(t *testing.T) {
	f := newStack(t, map[string]string{"source": "lower"})
	f.create(t, "app")
	backend := f.export.NFS()
	c := nfstest.Start(t, backend)
	old := lookup(t, c, "app/source")
	before, status, err := c.GetAttr(old)
	nfsStatus(t, status, nfstest.OK, err)
	if !backend.RetireHandle(binary.BigEndian.Uint64(old)) {
		t.Fatal("a live handle was not retired")
	}
	_, status, err = c.GetAttr(old)
	nfsStatus(t, status, nfstest.Stale, err)
	fresh := lookup(t, c, "app/source")
	after, status, err := c.GetAttr(fresh)
	nfsStatus(t, status, nfstest.OK, err)
	if after.Fileid != before.Fileid {
		t.Fatalf("retiring the handle changed the fileid from %#x to %#x", before.Fileid, after.Fileid)
	}
	nfsRead(t, c, fresh, "lower")
	_, status, err = c.GetAttr(fresh[:8])
	nfsStatus(t, status, nfstest.Stale, err)
}

// restartable is a registry and export over a state directory that a test
// closes and reopens, as a daemon restart does.
type restartable struct {
	state, mount, lower string
	reg                 *checkout.Registry
	export              *exportfs.Export
}

// open opens the registry and its export.
func (s *restartable) open(t *testing.T) {
	t.Helper()
	var e *exportfs.Export
	reg, err := checkout.Open(checkout.Config{State: s.state, Mount: s.mount, Memory: overlay.NewMemory(64<<20, 1<<20, 0), BlobCache: 64 << 20, Retire: func(name string, slot uint16) {
		if e != nil {
			e.Retire(name, slot)
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	e = exportfs.New(exportfs.NewRouter(reg), s.state)
	s.reg, s.export = reg, e
}

// close closes the registry.
func (s *restartable) close(t *testing.T) {
	t.Helper()
	if err := s.reg.Close(); err != nil {
		t.Fatal(err)
	}
}

// newRestartable opens a registry over a folder holding files.
func newRestartable(t *testing.T, files map[string]string) *restartable {
	t.Helper()
	s := &restartable{state: t.TempDir(), mount: t.TempDir(), lower: t.TempDir()}
	for name, data := range files {
		p := filepath.Join(s.lower, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s.open(t)
	t.Cleanup(func() { s.reg.Close() })
	return s
}

// create adds checkout name of the folder.
func (s *restartable) create(t *testing.T, name string) {
	t.Helper()
	if _, err := s.reg.New(context.Background(), api.NewArgs{Dir: s.lower, Name: name}); err != nil {
		t.Fatal(err)
	}
}

// TestIDsSurviveRestart keeps every kind of entry's id across a daemon
// restart: base files, copied-up files, created files and directories.
func TestIDsSurviveRestart(t *testing.T) {
	s := newRestartable(t, map[string]string{"base": "lower", "copied": "lower"})
	s.create(t, "app")
	nodes := s.export.Nodes()
	writeFile(t, nodes, "app/copied", "upper")
	if err := nodes.MkdirAll("app/dir", 0o755); err != nil {
		t.Fatal(err)
	}
	created, err := nodes.Create("app/dir/new")
	if err != nil {
		t.Fatal(err)
	}
	created.Close()
	paths := []string{"", "app", "app/base", "app/copied", "app/dir", "app/dir/new"}
	ids := make([]uint64, len(paths))
	for i, p := range paths {
		if ids[i] = nodes.ID(p); ids[i] == 0 {
			t.Fatalf("%q has no id", p)
		}
	}
	s.close(t)
	s.open(t)
	nodes = s.export.Nodes()
	for i, p := range paths {
		if got := nodes.ID(p); got != ids[i] {
			t.Errorf("%q had id %#x before the restart, %#x after", p, ids[i], got)
		}
		resolves(t, nodes, ids[i], p)
	}
}

// TestRemovedCheckoutHandleStaysStale keeps a handle of a removed checkout
// stale once a new checkout of the same folder takes the removed one's slot,
// where the same file ids name the same base files.
func TestRemovedCheckoutHandleStaysStale(t *testing.T) {
	s := newRestartable(t, map[string]string{"source": "lower"})
	s.create(t, "a")
	backend := s.export.NFS()
	id := backend.FileID("a/source")
	gen := backend.Generation(id)
	if _, ok := backend.Path(id, gen); !ok {
		t.Fatal("a live handle is stale")
	}
	slot, _, ok := s.reg.Slot("a")
	if !ok {
		t.Fatal("a has no slot")
	}
	if err := s.reg.Rm(context.Background(), api.RmArgs{Checkout: "a"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := backend.Path(id, gen); ok {
		t.Fatal("a handle of a removed checkout resolves")
	}
	// Slots are reused only after every other one was taken; point the
	// next allocation back at a's to get there without 4095 creates.
	s.close(t)
	statePath := filepath.Join(s.state, "state.json")
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]json.RawMessage
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	if state["next_slot"], err = json.Marshal(slot); err != nil {
		t.Fatal(err)
	}
	if raw, err = json.Marshal(state); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	s.open(t)
	s.create(t, "b")
	if got, _, _ := s.reg.Slot("b"); got != slot {
		t.Fatalf("b took slot %d, want %d", got, slot)
	}
	backend = s.export.NFS()
	if p, ok := backend.Path(id, gen); ok {
		t.Fatalf("a handle of removed checkout a resolves to %q in b", p)
	}
	fresh := backend.FileID("b/source")
	if _, ok := backend.Path(fresh, backend.Generation(fresh)); !ok {
		t.Fatal("b's own handle is stale")
	}
}

// TestPathFollowsChanges resolves ids, which caches their paths, then
// changes the namespace each way that moves or removes an entry, through
// the export and behind it through the checkout's overlay, and checks the
// ids resolve as the change left them.
func TestPathFollowsChanges(t *testing.T) {
	f := newStack(t, map[string]string{"dir/sub/file": "lower", "gone": "lower", "moved": "lower", "behind": "lower"})
	f.create(t, "app")
	nodes := f.export.Nodes()
	ids := map[string]uint64{}
	for _, p := range []string{"app/dir", "app/dir/sub", "app/dir/sub/file", "app/gone", "app/moved", "app/behind"} {
		ids[p] = nodes.ID(p)
		resolves(t, nodes, ids[p], p)
		resolves(t, nodes, ids[p], p) // from the cache
	}

	if err := nodes.Rename("app/moved", "app/dir/renamed"); err != nil {
		t.Fatal(err)
	}
	resolves(t, nodes, ids["app/moved"], "app/dir/renamed")

	if err := nodes.Remove("app/gone"); err != nil {
		t.Fatal(err)
	}
	if p, ok := nodes.Path(ids["app/gone"]); ok {
		t.Fatalf("removed file's id resolves to %q", p)
	}
	recreated, err := nodes.Create("app/gone")
	if err != nil {
		t.Fatal(err)
	}
	recreated.Close()
	if p, ok := nodes.Path(ids["app/gone"]); ok {
		t.Fatalf("removed file's id resolves to %q, its name's new file", p)
	}

	if err := nodes.Rename("app/dir", "app/top"); err != nil {
		t.Fatal(err)
	}
	resolves(t, nodes, ids["app/dir"], "app/top")
	resolves(t, nodes, ids["app/dir/sub"], "app/top/sub")
	resolves(t, nodes, ids["app/dir/sub/file"], "app/top/sub/file")
	resolves(t, nodes, ids["app/moved"], "app/top/renamed")

	var s checkout.Session
	if err := f.reg.Enter("app", &s); err != nil {
		t.Fatal(err)
	}
	err = s.FS.Rename("behind", "top/sub/behind")
	if err == nil {
		err = s.FS.Remove("top/sub/file")
	}
	s.Done()
	if err != nil {
		t.Fatal(err)
	}
	resolves(t, nodes, ids["app/behind"], "app/top/sub/behind")
	if p, ok := nodes.Path(ids["app/dir/sub/file"]); ok {
		t.Fatalf("file removed behind the export resolves to %q", p)
	}
}

// TestVanishedCacheHandleNFS exercises a client holding NFSv3 handles of
// fenced cache-root files (like cargo's target/) after the host rewrites or
// removes them: GETATTR and ACCESS return the pre-fence metadata, SETATTR
// size=0 materializes an empty upper file under the same handle so subsequent
// WRITEs succeed, while LOOKUP hides the host's newer file and READ/WRITE
// without truncate still fail STALE (preserving #58).
func TestVanishedCacheHandleNFS(t *testing.T) {
	f := newStack(t, map[string]string{
		"source.rs":                    "fn main() {}",
		"target/CACHEDIR.TAG":          "Signature: 8a477f597d28d172789f06886806bc55\n",
		"target/release/deps/out":      "old-out",
		"target/release/deps/lib.rlib": "old-rlib",
		"target/release/deps/ro.out":   "old-ro",
	})
	oldTime := time.Now().Add(-time.Hour).Truncate(time.Second)
	for _, rel := range []string{"target/release/deps/out", "target/release/deps/lib.rlib", "target/release/deps/ro.out"} {
		host := filepath.Join(f.lower, filepath.FromSlash(rel))
		if err := os.Chtimes(host, oldTime, oldTime); err != nil {
			t.Fatal(err)
		}
	}
	roHost := filepath.Join(f.lower, "target", "release", "deps", "ro.out")
	if err := os.Chmod(roHost, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(roHost, 0o644) })

	f.create(t, "app")
	c := nfstest.Start(t, f.export.NFS())
	depsFH := lookup(t, c, "app/target/release/deps")
	outFH, outAttr, st, err := c.Lookup(depsFH, "out")
	nfsStatus(t, st, nfstest.OK, err)
	rlibFH, rlibAttr, st, err := c.Lookup(depsFH, "lib.rlib")
	nfsStatus(t, st, nfstest.OK, err)
	roFH, _, st, err := c.Lookup(depsFH, "ro.out")
	nfsStatus(t, st, nfstest.OK, err)

	// Host rewrites out and ro.out in place and removes lib.rlib.
	outHost := filepath.Join(f.lower, "target", "release", "deps", "out")
	if err := os.WriteFile(outHost, []byte("NEW-HOST-OUTPUT-LONGER"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.lower, "target", "release", "deps", "lib.rlib")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(roHost, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(roHost, []byte("NEW-HOST-RO"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(roHost, 0o444); err != nil {
		t.Fatal(err)
	}

	// LOOKUP must not expose the host's newer file (#58).
	_, _, st, err = c.Lookup(depsFH, "out")
	nfsStatus(t, st, nfstest.NoEnt, err)
	_, _, st, err = c.Lookup(depsFH, "lib.rlib")
	nfsStatus(t, st, nfstest.NoEnt, err)

	// GETATTR and ACCESS on held handles return pre-fence metadata, never host's new size/mtime.
	gotOut, st, err := c.GetAttr(outFH)
	nfsStatus(t, st, nfstest.OK, err)
	if gotOut.Filesize != outAttr.Filesize || gotOut.Mtime != outAttr.Mtime {
		t.Fatalf("vanished GETATTR out = size %d mtime %v; want %d %v", gotOut.Filesize, gotOut.Mtime, outAttr.Filesize, outAttr.Mtime)
	}
	gotRlib, st, err := c.GetAttr(rlibFH)
	nfsStatus(t, st, nfstest.OK, err)
	if gotRlib.Filesize != rlibAttr.Filesize || gotRlib.Mtime != rlibAttr.Mtime {
		t.Fatalf("vanished GETATTR rlib = size %d mtime %v; want %d %v", gotRlib.Filesize, gotRlib.Mtime, rlibAttr.Filesize, rlibAttr.Mtime)
	}
	if _, st, err := c.Access(outFH, 0x3f); err != nil || st != nfstest.OK {
		t.Fatalf("vanished ACCESS out = st %d, err %v", st, err)
	}

	// READ and non-truncating WRITE/SETATTR still fail STALE (never serve host bytes).
	_, _, st, err = c.Read(outFH, 0, 64)
	nfsStatus(t, st, nfstest.Stale, err)
	_, _, st, err = c.Write(outFH, 0, []byte("append"), nfstest.FileSync)
	nfsStatus(t, st, nfstest.Stale, err)
	_, st, err = c.SetSize(outFH, 3)
	nfsStatus(t, st, nfstest.Stale, err)

	// Read-only vanished file refuses SETATTR size=0 with EACCES.
	_, st, err = c.SetSize(roFH, 0)
	nfsStatus(t, st, nfstest.Access, err)

	// SETATTR size=0 materializes an empty upper file under the same handle; WRITE then succeeds.
	_, st, err = c.SetSize(outFH, 0)
	nfsStatus(t, st, nfstest.OK, err)
	_, _, st, err = c.Write(outFH, 0, []byte("checkout-out"), nfstest.FileSync)
	nfsStatus(t, st, nfstest.OK, err)
	nfsRead(t, c, outFH, "checkout-out")
	if data, err := os.ReadFile(outHost); err != nil || string(data) != "NEW-HOST-OUTPUT-LONGER" {
		t.Fatalf("host out modified: %q, %v", data, err)
	}

	// A retired or removed checkout's vanished handle must remain STALE.
	if err := f.reg.Rm(context.Background(), api.RmArgs{Checkout: "app"}); err != nil {
		t.Fatal(err)
	}
	_, st, err = c.GetAttr(rlibFH)
	nfsStatus(t, st, nfstest.Stale, err)
}

// TestCachedFencedReadDetectsHostChangeNFS verifies that a fenced cache-root
// descriptor cached by NFSv3 READ still fails STALE once the host modifies the
// backing file, for both data reads and reads at or past EOF (n == 0).
func TestCachedFencedReadDetectsHostChangeNFS(t *testing.T) {
	f := newStack(t, map[string]string{
		"source.rs":                 "fn main() {}",
		"target/CACHEDIR.TAG":       "Signature: 8a477f597d28d172789f06886806bc55\n",
		"target/release/deps/a.bin": "old-aaa",
		"target/release/deps/b.bin": "old-bbb",
	})
	oldTime := time.Now().Add(-time.Hour).Truncate(time.Second)
	for _, rel := range []string{"target/release/deps/a.bin", "target/release/deps/b.bin"} {
		host := filepath.Join(f.lower, filepath.FromSlash(rel))
		if err := os.Chtimes(host, oldTime, oldTime); err != nil {
			t.Fatal(err)
		}
	}
	f.create(t, "app")
	c := nfstest.Start(t, f.export.NFS())
	depsFH := lookup(t, c, "app/target/release/deps")
	aFH, _, st, err := c.Lookup(depsFH, "a.bin")
	nfsStatus(t, st, nfstest.OK, err)
	bFH, _, st, err := c.Lookup(depsFH, "b.bin")
	nfsStatus(t, st, nfstest.OK, err)

	// Populate and hit the NFSv3 reader cache for both fenced files.
	nfsRead(t, c, aFH, "old-aaa")
	nfsRead(t, c, aFH, "old-aaa")
	nfsRead(t, c, bFH, "old-bbb")

	// Host rewrites both files after the fence.
	if err := os.WriteFile(filepath.Join(f.lower, "target/release/deps/a.bin"), []byte("new-aaa"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.lower, "target/release/deps/b.bin"), []byte("new-bbb"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Both a data read and an EOF read (off >= size) must fail STALE.
	_, _, st, err = c.Read(aFH, 0, 64)
	nfsStatus(t, st, nfstest.Stale, err)
	_, _, st, err = c.Read(bFH, 64, 64)
	nfsStatus(t, st, nfstest.Stale, err)

	// Truncating a vanished handle still materializes an upper file that reads back.
	_, st, err = c.SetSize(aFH, 0)
	nfsStatus(t, st, nfstest.OK, err)
	_, _, st, err = c.Write(aFH, 0, []byte("upper-aaa"), nfstest.FileSync)
	nfsStatus(t, st, nfstest.OK, err)
	nfsRead(t, c, aFH, "upper-aaa")
}
