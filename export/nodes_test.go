package export

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// pathOf describes a handle, including its retirement state.
func pathOf(t *testing.T, nt *nodeTable, id uint64) string {
	t.Helper()
	p, ok := nt.path(id)
	if !ok {
		return "<stale>"
	}
	return p
}

// namespace is an in-memory tree of entry paths, listed for the node table
// as the export's directories would be. Ancestors of an entry exist too.
type namespace struct {
	entries map[string]bool
	lists   int
}

// newNamespace holds paths and their ancestors.
func newNamespace(paths ...string) *namespace {
	ns := &namespace{entries: map[string]bool{}}
	ns.add(paths...)
	return ns
}

// add creates paths and their ancestors.
func (ns *namespace) add(paths ...string) {
	for _, p := range paths {
		for p != "" {
			ns.entries[p] = true
			p, _ = split(p)
		}
	}
}

// del removes p and everything below it.
func (ns *namespace) del(p string) {
	for e := range ns.entries {
		if e == p || strings.HasPrefix(e, p+"/") {
			delete(ns.entries, e)
		}
	}
}

// move renames from's subtree to to.
func (ns *namespace) move(from, to string) {
	ns.del(to)
	for e := range ns.entries {
		if e == from || strings.HasPrefix(e, from+"/") {
			delete(ns.entries, e)
			ns.add(to + strings.TrimPrefix(e, from))
		}
	}
}

// list returns the direct entries of dir.
func (ns *namespace) list(dir string) ([]os.FileInfo, error) {
	ns.lists++
	if dir != "" && !ns.entries[dir] {
		return nil, os.ErrNotExist
	}
	var out []os.FileInfo
	for e := range ns.entries {
		if d, name := split(e); d == dir {
			out = append(out, entryInfo(name))
		}
	}
	return out, nil
}

// entryInfo is a listed name.
type entryInfo string

func (e entryInfo) Name() string       { return string(e) }
func (e entryInfo) Size() int64        { return 0 }
func (e entryInfo) Mode() os.FileMode  { return 0 }
func (e entryInfo) ModTime() time.Time { return time.Time{} }
func (e entryInfo) IsDir() bool        { return false }
func (e entryInfo) Sys() any           { return nil }

// derived reports whether id is a derived id.
func derived(id uint64) bool { return id&derivedBit != 0 }

// collidingNames share a name hash.
const collideA, collideB = "f998", "f4776"

// TestCollidingNamesShareAHash keeps the fixture meaningful.
func TestCollidingNamesShareAHash(t *testing.T) {
	if nameHash(collideA) != nameHash(collideB) {
		t.Fatal("fixture names no longer collide")
	}
}

// TestHandlesFollowRenames checks subtree moves and replacement retirement.
func TestHandlesFollowRenames(t *testing.T) {
	ns := newNamespace("a/d/f", "b/x")
	nt := newNodeTable(ns.list)
	dir, child := nt.id("a/d"), nt.id("a/d/f")
	victim := nt.id("b/x")

	ns.move("a/d", "b/x")
	nt.rename("a/d", "b/x")

	if got := pathOf(t, nt, dir); got != "b/x" {
		t.Fatalf("renamed dir handle → %q", got)
	}
	if got := pathOf(t, nt, child); got != "b/x/f" {
		t.Fatalf("child handle → %q", got)
	}
	if got := pathOf(t, nt, victim); got != "<stale>" {
		t.Fatalf("replaced target handle → %q, want stale", got)
	}
	if nt.id("b/x/f") != child {
		t.Fatal("lookup of new path minted a new id instead of reusing the moved one")
	}
	ns.add("a/d")
	if id := nt.id("a/d"); id == dir || id == victim {
		t.Fatal("old path reached a moved or retired id")
	}
}

// TestRemovedHandlesGoStaleAndNamesGetFreshIDs checks non-reuse of retired ids.
func TestRemovedHandlesGoStaleAndNamesGetFreshIDs(t *testing.T) {
	for _, list := range []func(string) ([]os.FileInfo, error){nil, newNamespace("x/y").list} {
		nt := newNodeTable(list)
		old, oldDir := nt.id("x/y"), nt.id("x")
		nt.remove("x")
		if got := pathOf(t, nt, old); got != "<stale>" {
			t.Fatalf("removed handle → %q", got)
		}
		if id := nt.id("x/y"); id == old || nt.id("x") == oldDir {
			t.Fatal("recreated path reused a retired id")
		}
		if p, ok := nt.path(1); !ok || p != "" {
			t.Fatalf("root path = %q, %v", p, ok)
		}
	}
}

// TestRetiredDerivedIDsAreNotReissued covers removal and replacement of a
// file whose id derives from its name: the same name in the same directory
// gets another id, and the old one never resolves again.
func TestRetiredDerivedIDsAreNotReissued(t *testing.T) {
	ns := newNamespace("co/f", "co/g", "co/h")
	nt := newNodeTable(ns.list)
	f, g := nt.id("co/f"), nt.id("co/g")
	if !derived(f) || !derived(g) {
		t.Fatalf("file ids %x %x not derived", f, g)
	}
	ns.del("co/f")
	nt.remove("co/f")
	ns.add("co/f")
	if id := nt.id("co/f"); id == f {
		t.Fatal("recreated file reused its retired id")
	}
	// h was never handed out; it replaces g, whose id must not follow it.
	ns.move("co/h", "co/g")
	nt.rename("co/h", "co/g")
	if id := nt.id("co/g"); id == g {
		t.Fatal("replacement took the replaced file's id")
	}
	for _, id := range []uint64{f, g} {
		if got := pathOf(t, nt, id); got != "<stale>" {
			t.Fatalf("retired %x → %q", id, got)
		}
	}
}

// TestCollidingNamesNeverShareAnID gives one of two names with the same hash
// a logged counter id, whichever is handed out first.
func TestCollidingNamesNeverShareAnID(t *testing.T) {
	for _, first := range []string{collideA, collideB} {
		ns := newNamespace("co/"+collideA, "co/"+collideB)
		nt := newNodeTable(ns.list)
		a := nt.id("co/" + first)
		other := collideB
		if first == collideB {
			other = collideA
		}
		b := nt.id("co/" + other)
		if a == b {
			t.Fatal("colliding names share an id")
		}
		if derived(a) || !derived(b) {
			t.Fatalf("first %x should be counted, second %x derived", a, b)
		}
	}
}

// TestFreshListingNeedsNoExtraReads lets a listed directory's children take
// derived ids without the table reading the directory again.
func TestFreshListingNeedsNoExtraReads(t *testing.T) {
	ns := newNamespace("co/a", "co/b", "co/c")
	nt := newNodeTable(ns.list)
	nt.id("co")
	l := nt.startListing("co")
	infos, _ := ns.list("co")
	nt.observe(l, infos)
	before := ns.lists
	for _, name := range []string{"a", "b", "c"} {
		if id := nt.id("co/" + name); !derived(id) {
			t.Fatalf("%s: id %x not derived", name, id)
		}
	}
	if ns.lists != before {
		t.Fatalf("listed %d more times", ns.lists-before)
	}
}

// TestIssuedIDsFitDoubles keeps every id exactly representable as a
// float64, the type JavaScript runtimes report inode numbers in, across
// derived, counter, collision, retirement and rename paths.
func TestIssuedIDsFitDoubles(t *testing.T) {
	ns := newNamespace("co/dir/f", "co/"+collideA, "co/"+collideB, "co/g", "deep/a/b/c")
	nt := newNodeTable(ns.list)
	var ids []uint64
	issue := func(p string) { ids = append(ids, nt.id(p)) }
	for _, p := range []string{"co", "co/dir", "co/dir/f", "co/" + collideA, "co/" + collideB, "co/g", "deep/a/b/c"} {
		issue(p)
	}
	ns.del("co/g")
	nt.remove("co/g")
	ns.add("co/g")
	issue("co/g") // retired derived id: counter fallback
	ns.move("co/dir/f", "deep/f")
	nt.rename("co/dir/f", "deep/f")
	issue("deep/f") // pinned away from its name
	nt.next = maxDerivingKey
	ns.add("big/x")
	issue("big/x") // key too wide to derive from
	issue("big")
	kinds := map[bool]int{}
	for _, id := range ids {
		if id >= 1<<53 || uint64(float64(id)) != id {
			t.Fatalf("id %#x does not fit a double", id)
		}
		kinds[derived(id)]++
	}
	if kinds[true] == 0 || kinds[false] == 0 {
		t.Fatalf("paths issued only one kind of id: %v", kinds)
	}
	if id, _ := nt.lookup("big/x"); derived(id) {
		t.Fatalf("child of a %d-bit key got derived id %#x", hashBits, id)
	}
	// A handle with bits above the id range must not alias a real file.
	ns.add("co/fresh")
	wide := 1<<60 | derivedID(nt.find("co").key, nameHash("fresh"))
	if got := pathOf(t, nt, wide); got != "<stale>" {
		t.Fatalf("out-of-range handle resolved to %q", got)
	}
}

// TestCheckoutRetirementLeavesOtherIdentitiesLive checks the checkout boundary.
func TestCheckoutRetirementLeavesOtherIdentitiesLive(t *testing.T) {
	nt := newNodeTable(newNamespace("a/file", "ab/file").list)
	a, b := nt.id("a/file"), nt.id("ab/file")
	e := &Export{fs: &nodeFS{nodes: nt}}
	e.Retire("a")
	if _, ok := nt.path(a); ok {
		t.Fatal("removed checkout handle lives")
	}
	if got := pathOf(t, nt, b); got != "ab/file" {
		t.Fatalf("other checkout: %s", got)
	}
}

// TestDeepPathsAndLocations protects paths and location snapshots deeper than
// both the inline snapshot and the stack used to build paths.
func TestDeepPathsAndLocations(t *testing.T) {
	parts := make([]string, pathStack+inlineDepth)
	for i := range parts {
		parts[i] = fmt.Sprintf("d%d", i)
	}
	dir := strings.Join(parts, "/")
	file := dir + "/file"
	ns := newNamespace(file)
	nt := newNodeTable(ns.list)
	defer nt.close()
	id := nt.id(file)
	if got := pathOf(t, nt, id); got != file {
		t.Fatalf("deep path = %q, want %q", got, file)
	}

	nt.mu.Lock()
	d := nt.find(dir)
	loc := locate(d)
	same := loc.holds(d)
	nt.mu.Unlock()
	if d == nil || len(loc.deep) == 0 || !same {
		t.Fatal("deep location did not snapshot or overflow")
	}

	// The top ancestor is the last overflow entry: moving it away and back
	// must still be rejected, though every path matches again.
	ns.move("d0", "away")
	nt.rename("d0", "away")
	ns.move("away", "d0")
	nt.rename("away", "d0")
	nt.mu.Lock()
	moved := !loc.holds(nt.find(dir))
	nt.mu.Unlock()
	if !moved {
		t.Fatal("location accepted a deep ancestor that moved away and back")
	}
	if got := pathOf(t, nt, id); got != file {
		t.Fatalf("deep path after moves = %q, want %q", got, file)
	}
}
