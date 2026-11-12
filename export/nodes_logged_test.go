package export

import (
	"fmt"
	"path/filepath"
	"testing"
)

// evictAll pages out every logged node it can by touching an unrelated path
// with a zero budget, and returns the resident logged node count.
func evictAll(t *testing.T, nt *nodeTable, ns *namespace) int {
	t.Helper()
	ns.add("pressure/entry")
	nt.mu.Lock()
	nt.cacheLimit = 0
	nt.mu.Unlock()
	nt.id("pressure/entry")
	nt.mu.Lock()
	defer nt.mu.Unlock()
	return len(nt.byKey)
}

// checkIDs resolves every id to its path and every path to its id.
func checkIDs(t *testing.T, nt *nodeTable, want map[string]uint64) {
	t.Helper()
	for p, id := range want {
		if got := pathOf(t, nt, id); got != p {
			t.Fatalf("%x → %q, want %q", id, got, p)
		}
		if got, ok := nt.lookup(p); !ok || got != id {
			t.Fatalf("lookup %s → %x %v, want %x", p, got, ok, id)
		}
		if got := nt.id(p); got != id {
			t.Fatalf("id %s → %x, want %x", p, got, id)
		}
	}
}

// TestLoggedNodesPageOut evicts logged directories and logged files, then
// pages them back in by id and by path, also after clean and crash restarts.
func TestLoggedNodesPageOut(t *testing.T) {
	for _, crash := range []bool{false, true} {
		t.Run(fmt.Sprintf("crash=%v", crash), func(t *testing.T) {
			name := filepath.Join(t.TempDir(), "nodes.log")
			ns := newNamespace("co/"+collideA, "co/"+collideB)
			for i := range 20 {
				ns.add(fmt.Sprintf("co/d%d/sub/file", i))
			}
			nt := openLogged(t, name, ns)
			want := map[string]uint64{}
			for p := range ns.entries {
				want[p] = nt.id(p)
			}
			if resident := evictAll(t, nt, ns); resident > 4 {
				t.Fatalf("%d logged nodes stayed resident", resident)
			}
			checkIDs(t, nt, want)
			evictAll(t, nt, ns)
			want["pressure"], want["pressure/entry"] = nt.id("pressure"), nt.id("pressure/entry")
			reopen := name
			if crash {
				reopen = crashCopy(t, name)
			} else {
				closeLogged(t, nt)
			}
			nt = openLogged(t, reopen, ns)
			if len(nt.byKey) != 1 {
				t.Fatalf("open made %d logged nodes resident", len(nt.byKey))
			}
			checkIDs(t, nt, want)
		})
	}
}

// TestColdLoggedDirectoryRename moves a paged-out directory: every id below
// it follows, resolved by id and by path, before and after restarts.
func TestColdLoggedDirectoryRename(t *testing.T) {
	for _, crash := range []bool{false, true} {
		t.Run(fmt.Sprintf("crash=%v", crash), func(t *testing.T) {
			name := filepath.Join(t.TempDir(), "nodes.log")
			ns := newNamespace("co/a/b/c/file", "co/a/x/file", "co/other")
			nt := openLogged(t, name, ns)
			ids := map[string]uint64{}
			for _, p := range []string{"co/a", "co/a/b", "co/a/b/c", "co/a/b/c/file", "co/a/x", "co/a/x/file"} {
				ids[p] = nt.id(p)
			}
			evictAll(t, nt, ns)
			if nt.find("co/a") != nil {
				t.Fatal("co/a stayed resident")
			}
			ns.move("co/a", "co/z")
			nt.rename("co/a", "co/z")
			want := map[string]uint64{}
			for p, id := range ids {
				want["co/z"+p[len("co/a"):]] = id
			}
			for _, old := range []string{"co/a", "co/a/b/c/file"} {
				if _, ok := nt.lookup(old); ok {
					t.Fatalf("%s still resolves", old)
				}
			}
			checkIDs(t, nt, want)
			evictAll(t, nt, ns)
			recalled := map[uint64]bool{}
			for _, id := range nt.subtree("co/z") {
				recalled[id] = true
			}
			for p, id := range want {
				if !recalled[id] {
					t.Fatalf("subtree missed cold %s", p)
				}
			}
			checkIDs(t, nt, want)
			evictAll(t, nt, ns)
			reopen := name
			if crash {
				reopen = crashCopy(t, name)
			} else {
				closeLogged(t, nt)
			}
			for range 2 {
				nt = openLogged(t, reopen, ns)
				checkIDs(t, nt, want)
				closeLogged(t, nt)
			}
		})
	}
}

// TestColdLoggedRemovalStaysRetired removes a directory whose logged
// descendants are all paged out: none of their ids resolves or is issued
// again, across clean and crash restarts.
func TestColdLoggedRemovalStaysRetired(t *testing.T) {
	for _, crash := range []bool{false, true} {
		t.Run(fmt.Sprintf("crash=%v", crash), func(t *testing.T) {
			name := filepath.Join(t.TempDir(), "nodes.log")
			files := []string{"co/d/e/f/file", "co/d/g/file", "co/d/" + collideA, "co/d/" + collideB}
			ns := newNamespace(files...)
			nt := openLogged(t, name, ns)
			issued := map[uint64]bool{}
			var old []uint64
			for p := range ns.entries {
				if p != "co" {
					id := nt.id(p)
					old = append(old, id)
					issued[id] = true
				}
			}
			evictAll(t, nt, ns)
			if nt.find("co/d") != nil {
				t.Fatal("co/d stayed resident")
			}
			ns.del("co/d")
			nt.remove("co/d")
			if nt.find("co/d") != nil {
				t.Fatal("removal paged co/d in")
			}
			stale := func(nt *nodeTable) {
				t.Helper()
				for _, id := range old {
					if got := pathOf(t, nt, id); got != "<stale>" {
						t.Fatalf("retired %x → %q", id, got)
					}
				}
			}
			stale(nt)
			fresh := func(nt *nodeTable) map[string]uint64 {
				t.Helper()
				ns.add(files...)
				ids := map[string]uint64{}
				for p := range ns.entries {
					if p != "co" && p != "pressure" && p != "pressure/entry" {
						ids[p] = nt.id(p)
						if issued[ids[p]] && ids[p] != nt.find("co").id {
							t.Fatalf("%s reissued %x", p, ids[p])
						}
					}
				}
				return ids
			}
			again := fresh(nt)
			for _, id := range again {
				issued[id] = true
			}
			evictAll(t, nt, ns)
			stale(nt)
			reopen := name
			if crash {
				reopen = crashCopy(t, name)
			} else {
				closeLogged(t, nt)
			}
			for range 2 {
				nt = openLogged(t, reopen, ns)
				stale(nt)
				checkIDs(t, nt, again)
				closeLogged(t, nt)
			}
		})
	}
}

// TestWideWalkBoundsLoggedNodes walks thousands of logged directories under
// a small budget: resident logged nodes stay bounded, and every walked id
// resolves afterwards.
func TestWideWalkBoundsLoggedNodes(t *testing.T) {
	const dirs = 3000
	name := filepath.Join(t.TempDir(), "nodes.log")
	ns := newNamespace()
	for i := range dirs {
		ns.add(fmt.Sprintf("co/d%04d/sub/file", i))
	}
	nt := openLogged(t, name, ns)
	nt.cacheLimit = 64 << 10
	ids := map[string]uint64{}
	maxResident, maxUsed := 0, int64(0)
	for i := range dirs {
		for _, p := range []string{fmt.Sprintf("co/d%04d/sub", i), fmt.Sprintf("co/d%04d/sub/file", i)} {
			ids[p] = nt.id(p)
		}
		nt.mu.Lock()
		maxResident, maxUsed = max(maxResident, len(nt.byKey)), max(maxUsed, nt.cacheUsed)
		var total int64
		for _, n := range nt.byID {
			if s := n.dir; s != nil {
				total += s.cached
			}
		}
		if total != nt.cacheUsed {
			nt.mu.Unlock()
			t.Fatalf("accounted %d, charged %d", nt.cacheUsed, total)
		}
		nt.mu.Unlock()
	}
	logged := 2*dirs + 1
	t.Logf("walk of %d logged directories: at most %d resident (budget %d bytes, peak estimate %d bytes)", logged, maxResident, nt.cacheLimit, maxUsed)
	// co and its stubs alone exceed the budget: they stay as the ancestors
	// of every walked directory.
	if maxResident > logged/20 {
		t.Fatalf("%d of %d logged nodes resident", maxResident, logged)
	}
	if bound := nt.cacheLimit + 2*dirs*stubBytes + 64<<10; maxUsed > bound {
		t.Fatalf("estimate reached %d, bound %d", maxUsed, bound)
	}
	for p, id := range ids {
		if got := pathOf(t, nt, id); got != p {
			t.Fatalf("%x → %q, want %q", id, got, p)
		}
	}
	nt.mu.Lock()
	resident := len(nt.byKey)
	nt.mu.Unlock()
	if resident > logged/20 {
		t.Fatalf("resolving every id left %d logged nodes resident", resident)
	}
}
