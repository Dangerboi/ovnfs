package checkout

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/base"
	"github.com/stencil-hq/ovnfs/internal/ident"
	"github.com/stencil-hq/ovnfs/watch"
)

// checkoutCaches returns the cache snapshot checkout name reads, its base's
// folder and the snapshots on the base's disk.
func (f *fixture) checkoutCaches(name string) (id, dir string, onDisk []string) {
	f.t.Helper()
	f.reg.mu.RLock()
	h := f.reg.checkouts[name]
	f.reg.mu.RUnlock()
	if h == nil {
		f.t.Fatalf("no checkout %s", name)
	}
	ids, err := h.base.view.CachesIDs()
	if err != nil {
		f.t.Fatal(err)
	}
	return h.meta.Caches, h.base.view.Meta().Dir, ids
}

// TestCacheSnapshotsIsolateCheckouts fences a checkout's cache roots at its
// snapshot however the folder changes: what the folder rewrites or removes
// leaves the checkout, never changes in it, and nothing it adds appears. A
// checkout opened after the folder changed there gets a newer snapshot while
// older checkouts keep theirs, those opened without further changes share
// one, and a snapshot no checkout uses is removed. A folder change never
// invalidates a checkout.
func TestCacheSnapshotsIsolateCheckouts(t *testing.T) {
	f := newFixture(t)
	dir := folder(t, map[string]string{"src/main": "one", "target/out": "one", "target/kept": "kept", "target/gone": "gone"})
	f.newCheckout(dir, "a", "cache dir & target")
	first, root, _ := f.checkoutCaches("a")
	invalidated := len(f.invalidations())

	write(t, filepath.Join(dir, "target/out"), "two")
	write(t, filepath.Join(dir, "target/new"), "new")
	if err := os.Remove(filepath.Join(dir, "target/gone")); err != nil {
		t.Fatal(err)
	}
	f.reg.folderChanged([]watch.Change{{Path: filepath.Join(root, "target", "out")}})
	for p, want := range map[string]string{"target/out": "<absent>", "target/new": "<absent>", "target/gone": "<absent>", "target/kept": "kept", "src/main": "one"} {
		if got := f.read("a", p); got != want {
			t.Errorf("a: %s = %q after the folder changed, want %q", p, got, want)
		}
	}
	if got := f.invalidations()[invalidated:]; len(got) != 0 {
		t.Errorf("folder changes invalidated checkouts: %+v", got)
	}

	f.newCheckout(dir, "b", "cache dir & target")
	second, _, _ := f.checkoutCaches("b")
	if second == first {
		t.Fatal("a checkout opened after the folder changed its cache root shares the older snapshot")
	}
	for p, want := range map[string]string{"target/out": "two", "target/new": "new", "target/kept": "kept", "target/gone": "<absent>"} {
		if got := f.read("b", p); got != want {
			t.Errorf("b: %s = %q, want %q", p, got, want)
		}
	}
	if got := f.read("a", "target/out"); got != "<absent>" {
		t.Errorf("a: target/out = %q after b opened, want it to stay gone", got)
	}

	// Checkouts opened while the folder does not change share one snapshot.
	if f.reg.watcher != nil && f.reg.watcher.Covers(root) && f.reg.watcher.Sync() == nil {
		f.reg.mu.RLock()
		b := f.reg.checkouts["b"].base
		f.reg.mu.RUnlock()
		x, errX := f.reg.useCaches(b)
		y, errY := f.reg.useCaches(b)
		if errX != nil || errY != nil || x != y {
			t.Errorf("two checkouts opened on an unchanged folder took snapshots %v and %v (%v, %v)", x, y, errX, errY)
		}
		for _, c := range []*base.Caches{x, y} {
			f.reg.dropCaches(&checkoutRecord{base: b, caches: c})
		}
	}

	if err := f.reg.Rm(t.Context(), api.RmArgs{Checkout: "a"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the snapshot no checkout uses was not removed", func() bool {
		_, _, onDisk := f.checkoutCaches("b")
		return !slices.Contains(onDisk, first) && slices.Contains(onDisk, second)
	})
}

// TestCheckoutOwnsWhatItChangedBelowACacheRoot checks that a cache file a
// checkout wrote to is its own from then on, a copy in its upper that
// starts from the folder's content, and that one it removed stays removed:
// what the folder does to its files afterwards reaches neither.
func TestCheckoutOwnsWhatItChangedBelowACacheRoot(t *testing.T) {
	f := newFixture(t)
	dir := folder(t, map[string]string{"src/main": "one", "target/log": "built\n", "target/dropped": "x"})
	f.newCheckout(dir, "a", "cache dir & target")
	var s Session
	if err := f.reg.Enter("a", &s); err != nil {
		t.Fatal(err)
	}
	log, err := s.FS.OpenFile("target/log", os.O_WRONLY|os.O_APPEND, 0)
	if err == nil {
		_, err = log.Write([]byte("checked\n"))
		err = errors.Join(err, log.Close())
	}
	s.Done()
	if err != nil {
		t.Fatal(err)
	}
	f.remove("a", "target/dropped")

	write(t, filepath.Join(dir, "target/log"), "rebuilt\n")
	write(t, filepath.Join(dir, "target/dropped"), "back")
	for p, want := range map[string]string{"target/log": "built\nchecked\n", "target/dropped": "<absent>"} {
		if got := f.read("a", p); got != want {
			t.Errorf("%s = %q after the folder changed its own, want %q", p, got, want)
		}
	}
}

// TestHardLinksPartOnWrite checks two names of one folder file below a cache
// or local root, as cargo links an artifact into deps/ and beside it. Each
// name has its own identity, so a write through one name's handle copies up
// that name, keeps its identity, and leaves the other the folder's.
func TestHardLinksPartOnWrite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows file information has no link count")
	}
	for _, class := range []string{"cache", "local"} {
		t.Run(class, func(t *testing.T) {
			f := newFixture(t)
			dir := folder(t, map[string]string{"src/main": "one", "target/deps/app-1a2b": "built\n"})
			if err := os.Link(filepath.Join(dir, "target/deps/app-1a2b"), filepath.Join(dir, "target/app")); err != nil {
				t.Fatal(err)
			}
			f.newCheckout(dir, "a", class+" dir & target")
			var s Session
			if err := f.reg.Enter("a", &s); err != nil {
				t.Fatal(err)
			}
			defer s.Done()
			identity := func(p string) ident.ID {
				t.Helper()
				info, err := s.FS.Lstat(p)
				if err != nil {
					t.Fatal(err)
				}
				id, ok := ident.Of(info)
				if !ok {
					t.Fatalf("%s has no identity", p)
				}
				return id
			}
			// A client writes through the identity it looked up, as an NFS
			// handle does, not through a path.
			extend := func(id ident.ID, data string) {
				t.Helper()
				p, ok := s.FS.Locate(id)
				if !ok {
					t.Fatalf("identity %v resolves to nothing", id)
				}
				file, err := s.FS.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
				if err == nil {
					_, err = file.Write([]byte(data))
					err = errors.Join(err, file.Close())
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			app, deps := identity("target/app"), identity("target/deps/app-1a2b")
			if app == deps {
				t.Fatalf("two names of one folder file share identity %v", app)
			}

			extend(deps, "signed\n")
			if got := identity("target/deps/app-1a2b"); got != deps {
				t.Errorf("the name written has identity %v, want the %v its handle holds", got, deps)
			}
			if p, ok := s.FS.Locate(app); !ok || p != "target/app" {
				t.Errorf("the unwritten name's identity resolves to %q, %v", p, ok)
			}
			extend(app, "stripped\n")
			for p, want := range map[string]string{"target/app": "built\nstripped\n", "target/deps/app-1a2b": "built\nsigned\n"} {
				if got := f.read("a", p); got != want {
					t.Errorf("%s = %q, want %q", p, got, want)
				}
			}
			if data, err := os.ReadFile(filepath.Join(dir, "target/app")); err != nil || string(data) != "built\n" {
				t.Errorf("the folder's file = %q, %v; want it untouched", data, err)
			}
		})
	}
}

// TestCacheSnapshotsSurviveRestart reopens a checkout on its own snapshot, not
// the folder as it is then: its local roots as copied and its cache roots
// behind the same fence. It removes snapshots on disk no checkout names.
func TestCacheSnapshotsSurviveRestart(t *testing.T) {
	f := newFixture(t)
	if err := f.reg.Close(); err != nil {
		t.Fatal(err)
	}
	f.frozenLocal = true
	f.open()
	dir := folder(t, map[string]string{"src/main": "one", "target/out": "one", "target/kept": "kept", ".env": "SECRET=1"})
	f.newCheckout(dir, "a", "cache dir & target")
	id, _, _ := f.checkoutCaches("a")
	if id == "" {
		t.Fatal("a checkout on a local base has no cache snapshot")
	}
	write(t, filepath.Join(dir, "target/out"), "two")
	write(t, filepath.Join(dir, ".env"), "SECRET=2")
	f.reg.mu.RLock()
	baseID := f.reg.checkouts["a"].base.view.Meta().ID
	f.reg.mu.RUnlock()
	orphan := filepath.Join(f.state, "bases", baseID, "caches", "c_000000000000")
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := f.reg.Close(); err != nil {
		t.Fatal(err)
	}
	f.open()
	if got, _, _ := f.checkoutCaches("a"); got != id {
		t.Fatalf("reopened checkout reads snapshot %q, want %q", got, id)
	}
	for p, want := range map[string]string{"target/out": "<absent>", "target/kept": "kept", ".env": "SECRET=1"} {
		if got := f.read("a", p); got != want {
			t.Errorf("%s = %q after a restart, want the snapshot's %q", p, got, want)
		}
	}
	eventually(t, "a snapshot no checkout names survived the restart", func() bool {
		_, err := os.Stat(orphan)
		return os.IsNotExist(err)
	})
}

func TestLocalVirtualDefaultAndFrozenOptIn(t *testing.T) {
	f := newFixture(t)
	dir := folder(t, map[string]string{"src/main": "one", ".env": "private", "local/keep": "original", "local/owned": "from-host"})
	// Default mode is virtual: local paths forward directly to the folder until written.
	f.newCheckout(dir, "virtual", "local dir & local")
	if got := f.read("virtual", ".env"); got != "private" {
		t.Fatalf("default virtual local file = %q", got)
	}
	if got := f.read("virtual", "local/keep"); got != "original" {
		t.Fatalf("default virtual local dir = %q", got)
	}
	f.put("virtual", "local/owned", "written-in-checkout")

	reopen := func(frozen bool) {
		t.Helper()
		if err := f.reg.Close(); err != nil {
			t.Fatal(err)
		}
		f.frozenLocal = frozen
		f.open()
	}
	reopen(true)
	f.newCheckout(dir, "frozen", "local dir & local")
	if got := f.read("frozen", ".env"); got != "private" {
		t.Fatalf("frozen local file = %q", got)
	}

	write(t, filepath.Join(dir, ".env"), "changed")
	write(t, filepath.Join(dir, "local/keep"), "edited")
	write(t, filepath.Join(dir, "local/owned"), "host-overwrite")
	write(t, filepath.Join(dir, "local/new"), "added")

	checkViews := func() {
		t.Helper()
		for _, check := range []struct{ checkout, path, want string }{
			{"virtual", ".env", "changed"},
			{"virtual", "local/keep", "edited"},
			{"virtual", "local/new", "added"},
			{"virtual", "local/owned", "written-in-checkout"},
			{"frozen", ".env", "private"},
			{"frozen", "local/keep", "original"},
			{"frozen", "local/new", "<absent>"},
		} {
			if got := f.read(check.checkout, check.path); got != check.want {
				t.Errorf("%s/%s = %q, want %q", check.checkout, check.path, got, check.want)
			}
		}
	}
	checkViews()
	reopen(false)
	checkViews()
}

// TestCacheRootsOfPreparedCheckouts gives a checkout taken from a folder's
// prepared pool the folder's cache roots as of its create, not as of its
// preparation.
func TestCacheRootsOfPreparedCheckouts(t *testing.T) {
	f := newFixture(t)
	if err := f.reg.Close(); err != nil {
		t.Fatal(err)
	}
	f.frozenLocal = true
	f.open()
	dir := folder(t, map[string]string{"src/main": "one", ".env": "SECRET=1"})
	f.newCheckout(dir, "a")
	_, root, _ := f.checkoutCaches("a")
	write(t, filepath.Join(dir, ".env"), "SECRET=2")
	f.reg.folderChanged([]watch.Change{{Path: filepath.Join(root, ".env")}})
	f.newCheckout(dir, "b")
	if got := f.read("b", ".env"); got != "SECRET=2" {
		t.Errorf("b: .env = %q, want the folder's at its create", got)
	}
	if got := f.read("a", ".env"); got != "SECRET=1" {
		t.Errorf("a: .env = %q, want its snapshot's", got)
	}
}
