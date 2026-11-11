package checkout

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/overlay"
)

// enforcedPerms skips tests whose failure needs the OS to refuse unlinking in an
// unwritable directory, which neither root nor Windows does.
func enforcedPerms(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("directory permissions do not stop removal here")
	}
}

// lockedDir creates dir/locked with a file and a nested directory below it, then
// takes write access from both, as a build tree's read-only output does.
func lockedDir(t *testing.T, dir string) {
	t.Helper()
	write(t, filepath.Join(dir, "locked", "f.txt"), "x")
	write(t, filepath.Join(dir, "locked", "sub", "g.txt"), "y")
	for _, d := range []string{filepath.Join(dir, "locked", "sub"), filepath.Join(dir, "locked")} {
		if err := os.Chmod(d, 0o555); err != nil {
			t.Fatal(err)
		}
	}
}

// TestRemoveTreeUnlocksDirsWithoutLeavingTheTree checks that removeTree deletes a
// tree with unwritable and unreadable directories, and that a symlink in it to an
// unwritable directory outside is removed as a link, the target left untouched.
func TestRemoveTreeUnlocksDirsWithoutLeavingTheTree(t *testing.T) {
	enforcedPerms(t)
	outside := filepath.Join(t.TempDir(), "outside")
	write(t, filepath.Join(outside, "keep.txt"), "keep")
	if err := os.Chmod(outside, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(outside, 0o700) })
	tree := filepath.Join(t.TempDir(), "tree")
	if err := os.MkdirAll(filepath.Join(tree, "locked"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, link := range []string{filepath.Join(tree, "out"), filepath.Join(tree, "locked", "out")} {
		if err := os.Symlink(outside, link); err != nil {
			t.Fatal(err)
		}
	}
	lockedDir(t, tree)
	if err := os.Chmod(filepath.Join(tree, "locked", "sub"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		os.Chmod(filepath.Join(tree, "locked", "sub"), 0o700)
		os.Chmod(filepath.Join(tree, "locked"), 0o700)
	})

	if err := removeTree(tree); err != nil {
		t.Fatalf("removeTree = %v", err)
	}
	if _, err := os.Lstat(tree); !os.IsNotExist(err) {
		t.Fatalf("tree survived: %v", err)
	}
	if info, err := os.Stat(outside); err != nil || info.Mode().Perm() != 0o500 {
		t.Fatalf("symlink target after removal: %v, %v; want mode 0500", info, err)
	}
	if data, err := os.ReadFile(filepath.Join(outside, "keep.txt")); err != nil || string(data) != "keep" {
		t.Fatalf("symlink target's file after removal: %q, %v", data, err)
	}
}

// TestRmRemovesUnwritableDirs checks that rm of a checkout whose upper holds an
// unwritable directory with files removes the upper and succeeds, rather than
// failing after the checkout left the manifest and leaving the upper an orphan.
func TestRmRemovesUnwritableDirs(t *testing.T) {
	enforcedPerms(t)
	f := newFixture(t)
	f.newCheckout(folder(t, map[string]string{"a": "base"}), "c")
	upper := f.reg.upperPath("c")
	lockedDir(t, upper)
	t.Cleanup(func() {
		os.Chmod(filepath.Join(upper, "locked", "sub"), 0o700)
		os.Chmod(filepath.Join(upper, "locked"), 0o700)
	})
	if err := f.reg.Rm(context.Background(), api.RmArgs{Checkout: "c"}); err != nil {
		t.Fatalf("rm = %v", err)
	}
	if _, err := os.Lstat(upper); !os.IsNotExist(err) {
		t.Fatalf("upper survived rm: %v", err)
	}
	if ls := f.ls(); len(ls.Checkouts) != 0 {
		t.Fatalf("checkouts after rm = %+v", ls.Checkouts)
	}
}

// TestOpenReclaimsOrphanWithUnwritableDir checks that an orphaned upper holding
// an unwritable directory is reclaimed at open, and the checkouts kept.
func TestOpenReclaimsOrphanWithUnwritableDir(t *testing.T) {
	enforcedPerms(t)
	f := newFixture(t)
	f.newCheckout(folder(t, map[string]string{"a": "base"}), "live")
	if err := f.reg.Close(); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(f.state, "uppers", "gone")
	lockedDir(t, orphan)
	t.Cleanup(func() {
		os.Chmod(filepath.Join(orphan, "locked", "sub"), 0o700)
		os.Chmod(filepath.Join(orphan, "locked"), 0o700)
	})
	f.open()
	if _, err := os.Lstat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan upper survived open: %v", err)
	}
	if ls := f.ls(); len(ls.Checkouts) != 1 || ls.Checkouts[0].Name != "live" {
		t.Fatalf("checkouts after open = %+v", ls.Checkouts)
	}
}

// TestOpenSurvivesUndeletableOrphan checks that one orphan that cannot be removed
// at all (here its parent directory is read-only) is reported as a fault, and the
// registry still opens with its checkouts instead of exiting before it serves.
func TestOpenSurvivesUndeletableOrphan(t *testing.T) {
	enforcedPerms(t)
	f := newFixture(t)
	f.newCheckout(folder(t, map[string]string{"a": "base"}), "live")
	if err := f.reg.Close(); err != nil {
		t.Fatal(err)
	}
	uppers := filepath.Join(f.state, "uppers")
	if err := os.Mkdir(filepath.Join(uppers, "stuck"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(uppers, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(uppers, 0o700) })

	var mu sync.Mutex
	var faults []string
	reg, err := Open(Config{State: f.state, Mount: f.mount, Node: "n_stuck", Memory: overlay.NewMemory(64<<20, 1<<20, 0), BlobCache: 64 << 20,
		Emit: func(e Event) {
			if e.Name == "fault" {
				mu.Lock()
				faults = append(faults, e.Payload.(map[string]any)["message"].(string))
				mu.Unlock()
			}
		}})
	if err != nil {
		t.Fatalf("open with an undeletable orphan = %v", err)
	}
	f.reg = reg
	if ls := f.ls(); len(ls.Checkouts) != 1 || ls.Checkouts[0].Name != "live" {
		t.Fatalf("checkouts after open = %+v", ls.Checkouts)
	}
	eventually(t, "no fault names the orphan", func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, m := range faults {
			if strings.Contains(m, "uppers/stuck") {
				return true
			}
		}
		return false
	})
	if _, err := os.Lstat(filepath.Join(uppers, "stuck")); err != nil {
		t.Fatalf("undeletable orphan: %v", err)
	}
}
