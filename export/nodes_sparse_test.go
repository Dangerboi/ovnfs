package export

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A client may retain just one handle in each of several wide directories.
// Recovering every owner on each alternating GETATTR makes work proportional
// to directory width even though only four names are requested.
func TestSparseColdHandles(t *testing.T) {
	testSparseColdLookups(t, func(t *testing.T, nt *nodeTable, ns *namespace) func(string, uint64) {
		return func(p string, id uint64) {
			if got := pathOf(t, nt, id); got != p {
				t.Fatalf("%q != %q", got, p)
			}
		}
	})
}

func TestSparseColdIDs(t *testing.T) {
	testSparseColdLookups(t, func(t *testing.T, nt *nodeTable, ns *namespace) func(string, uint64) {
		n := (&Export{fs: &nodeFS{nodes: nt}}).Nodes()
		return func(p string, id uint64) {
			if got := n.ID(p); got != id {
				t.Fatalf("ID(%q) = %x, want %x", p, got, id)
			}
		}
	})
}

func testSparseColdLookups(t *testing.T, lookup func(*testing.T, *nodeTable, *namespace) func(string, uint64)) {
	t.Helper()
	for _, budget := range []int64{0, 4096, 1 << 20, 16 << 20} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			ns := newNamespace("pressure/entry")
			nt := newNodeTable(ns.list)
			defer nt.close()
			var ids []uint64
			var paths []string
			for d := range 4 {
				for f := range 4096 {
					ns.add(fmt.Sprintf("d%d/f%04d", d, f))
				}
			}
			for d := range 4 {
				for f := range 4096 {
					p := fmt.Sprintf("d%d/f%04d", d, f)
					id := nt.id(p)
					if f == 0 {
						ids = append(ids, id)
						paths = append(paths, p)
					}
				}
			}
			check := lookup(t, nt, ns)
			nt.cacheLimit = 0
			nt.id("pressure/entry")
			// Keep a real listing pinned just as mutation preparation does. This
			// consumes the larger allowance without allowing trim to free it and
			// accidentally turning this into an under-budget batch test.
			if budget > 4096 {
				nt.mu.Lock()
				d := nt.find("pressure")
				for i := 0; nt.cacheUsed < budget-4096; i++ {
					nt.addLeaf(d, fmt.Sprintf("p%06d-%s", i, strings.Repeat("p", 232)))
				}
				d.dir.reading++
				nt.mu.Unlock()
				defer func() { d.dir.reading-- }()
			}
			nt.cacheLimit = budget
			var reads atomic.Int32
			nt.indexIO = func() { reads.Add(1) }
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			start := time.Now()
			for i := range 512 {
				j := i % len(ids)
				check(paths[j], ids[j])
			}
			runtime.ReadMemStats(&after)
			allocated := after.TotalAlloc - before.TotalAlloc
			t.Logf("512 alternating handles: %s allocated=%d index_operations=%d", time.Since(start), allocated, reads.Load())
			if allocated > 4<<20 {
				t.Fatalf("sparse recovery allocated %d bytes", allocated)
			}
			maxReads := int32(4)
			if budget == 0 {
				maxReads = 514
			} // one owner search per request, plus spill publication
			if reads.Load() > maxReads {
				t.Fatalf("%d index operations, want <= %d", reads.Load(), maxReads)
			}
			for d := range 4 {
				s := nt.find(fmt.Sprintf("d%d", d)).dir
				if s.coldLoaded || len(s.leaves) > 1 {
					t.Fatal("sparse recovery admitted unrelated owners")
				}
			}
		})
	}
}

// Exercise the partial-read validation boundary, including ownership changes
// that do not change the namespace generation.
func TestSparseColdReadRevalidated(t *testing.T) {
	for _, change := range []string{"directory", "retired", "owner", "move"} {
		t.Run(change, func(t *testing.T) {
			ns := newNamespace("co/file", "pressure/entry")
			nt := newNodeTable(ns.list)
			old := nt.id("co/file")
			forceCold(t, nt, ns, "co")
			entered, resume := make(chan struct{}), make(chan struct{})
			var once sync.Once
			nt.indexIO = func() { once.Do(func() { close(entered); <-resume }) }
			done := make(chan string, 1)
			go func() {
				parts, ok := nt.path(old)
				if !ok {
					done <- ""
				} else {
					done <- strings.Join(parts, "/")
				}
			}()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("read did not start")
			}
			nt.mu.Lock()
			d := nt.find("co")
			switch change {
			case "directory":
				nt.drop(d)
			case "retired":
				d.dir.retired = map[uint32]struct{}{nameHash("file"): {}}
			case "owner":
				nt.addLeaf(d, "file")
				nt.child(d, "file")
			case "move":
				d.moves++
				delete(d.parent.kids, d.name)
				d.name = "moved"
				d.parent.kids[d.name] = d
			}
			nt.mu.Unlock()
			close(resume)
			want := ""
			if change == "owner" {
				want = "co/file"
			}
			if change == "move" {
				want = "moved/file"
			}
			select {
			case got := <-done:
				if got != want {
					t.Fatalf("got %q want %q", got, want)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("read stuck")
			}
			if nt.cacheErr != nil {
				t.Fatal(nt.cacheErr)
			}
		})
	}
}

func TestSparseColdPreRestartHandle(t *testing.T) {
	for _, budget := range []int64{0, 1 << 20} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			ns := newNamespace("co/first", "co/second", "pressure/entry")
			log := filepath.Join(t.TempDir(), "nodes.log")
			nt := openLogged(t, log, ns)
			first, second := nt.id("co/first"), nt.id("co/second")
			closeLogged(t, nt)
			nt = openLogged(t, log, ns)
			if got := pathOf(t, nt, first); got != "co/first" {
				t.Fatal(got)
			}
			forceCold(t, nt, ns, "co")
			if budget != 0 {
				d := nt.find("pressure")
				for i := 0; nt.cacheUsed < budget-4096; i++ {
					nt.addLeaf(d, fmt.Sprintf("p%06d-%s", i, strings.Repeat("p", 232)))
				}
				d.dir.reading++
				defer func() { d.dir.reading-- }()
			}
			nt.cacheLimit = budget
			if got := pathOf(t, nt, second); got != "co/second" {
				t.Fatalf("pre-restart handle: %q", got)
			}
			if got := pathOf(t, nt, first); got != "co/first" {
				t.Fatal(got)
			}
			if nt.cacheErr != nil {
				t.Fatal(nt.cacheErr)
			}
		})
	}
}

// A partially recovered directory can contain leaves and materialized nodes.
// Looking up either must not recover the directory's other issued owners.
func TestSparseResidentIDs(t *testing.T) {
	for _, budget := range []int64{0, 4096, 1 << 20, 16 << 20} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			for _, materialize := range []bool{false, true} {
				ns := newNamespace("co/file", "co/other", "pressure/entry")
				nt := newNodeTable(ns.list)
				defer nt.close()
				id := nt.id("co/file")
				nt.id("co/other")
				forceCold(t, nt, ns, "co")
				nt.cacheLimit = budget
				d := nt.find("co")
				nt.addLeaf(d, "file")
				if materialize {
					nt.child(d, "file")
				}
				nt.mu.Lock()
				nt.trimCache()
				nt.mu.Unlock()
				var reads int
				nt.indexIO = func() { reads++ }
				n := (&Export{fs: &nodeFS{nodes: nt}}).Nodes()
				if got := n.ID("co/file"); got != id {
					t.Fatalf("resident ID = %x, want %x", got, id)
				}
				if reads != 0 || d.dir.coldLoaded || len(d.dir.leaves) > 1 {
					t.Fatal("resident ID loaded unrelated owners")
				}
			}
		})
	}
}

func TestColdIDRecoveryBatchesOwners(t *testing.T) {
	for _, budget := range []int64{0, 1 << 20} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			ns := newNamespace()
			nt := newNodeTable(ns.list)
			defer nt.close()
			n := (&Export{fs: &nodeFS{nodes: nt}}).Nodes()
			var ids []uint64
			for i := range 256 {
				p := fmt.Sprintf("co/f%d", i)
				ns.add(p)
				ids = append(ids, n.ID(p))
			}
			forceCold(t, nt, ns, "co")
			nt.cacheLimit = budget
			var reads int
			nt.indexIO = func() { reads++ }
			for i, id := range ids {
				if got := n.ID(fmt.Sprintf("co/f%d", i)); got != id {
					t.Fatalf("ID changed: %x != %x", got, id)
				}
			}
			maxReads := 1
			if budget == 0 {
				maxReads = 5
			}
			if reads > maxReads {
				t.Fatalf("%d index operations, want <= %d", reads, maxReads)
			}
		})
	}
}

// Name lookups must not publish the owner read before a concurrent change.
func TestSparseColdIDReadRevalidated(t *testing.T) {
	for _, change := range []string{"directory", "retired", "owner", "replacement", "move", "ancestor-away-back", "generation"} {
		t.Run(change, func(t *testing.T) {
			ns := newNamespace("co/d/file", "pressure/entry")
			nt := newNodeTable(ns.list)
			defer nt.close()
			old := nt.id("co/d/file")
			forceCold(t, nt, ns, "co/d")
			nt.cacheLimit = 0
			entered, resume := make(chan struct{}), make(chan struct{})
			var once sync.Once
			var reads atomic.Int32
			coldDir := nt.find("co/d")
			nt.indexIO = func() {
				nt.mu.Lock()
				if op := coldDir.dir.indexing; op != nil && op.read {
					reads.Add(1)
				}
				nt.mu.Unlock()
				once.Do(func() { close(entered); <-resume })
			}
			done := make(chan uint64, 1)
			n := (&Export{fs: &nodeFS{nodes: nt}}).Nodes()
			go func() { done <- n.ID("co/d/file") }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("read did not start")
			}
			nt.mu.Lock()
			d := nt.find("co/d")
			switch change {
			case "directory":
				nt.drop(d)
			case "retired":
				d.dir.retired = map[uint32]struct{}{nameHash("file"): {}}
			case "owner":
				nt.addLeaf(d, "file")
				nt.child(d, "file")
			case "replacement":
				nt.addLeaf(d, "file")
				nt.drop(nt.child(d, "file"))
				nt.alloc(d, "file")
			case "ancestor-away-back":
				d.parent.moves += 2
			case "move":
				ns.move("co/d", "co/moved")
				ns.add("co/d/file")
				d.moves++
				delete(d.parent.kids, d.name)
				d.name = "moved"
				d.parent.kids[d.name] = d
			case "generation":
				d.dir.gen++
			}
			nt.mu.Unlock()
			close(resume)
			select {
			case got := <-done:
				same := change == "owner" || change == "generation" || change == "ancestor-away-back"
				if got == 0 || (got == old) != same {
					t.Fatalf("ID = %x, old %x, want same: %v", got, old, same)
				}
				if (change == "generation" || change == "ancestor-away-back") && reads.Load() < 2 {
					t.Fatal("lookup accepted the obsolete snapshot without retrying")
				}
				if gotPath := pathOf(t, nt, got); gotPath != "co/d/file" {
					t.Fatalf("ID resolves to %q", gotPath)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("read stuck")
			}
		})
	}
}

// A waiter can resume after the completed read's owner has been replaced.
func TestSparseColdIDWaiterRevalidated(t *testing.T) {
	ns := newNamespace("co/file", "pressure/entry")
	nt := newNodeTable(ns.list)
	defer nt.close()
	old := nt.id("co/file")
	forceCold(t, nt, ns, "co")
	nt.cacheLimit = 0
	entered, resume := make(chan struct{}), make(chan struct{})
	waiting, consume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	nt.indexIO = func() { once.Do(func() { close(entered); <-resume }) }
	nt.indexWait = func() { close(waiting); <-consume }
	n := (&Export{fs: &nodeFS{nodes: nt}}).Nodes()
	first, waiter := make(chan uint64, 1), make(chan uint64, 1)
	go func() { first <- n.ID("co/file") }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("read did not start")
	}
	go func() { waiter <- n.ID("co/file") }()
	select {
	case <-waiting:
	case <-time.After(time.Second):
		t.Fatal("waiter did not start")
	}
	close(resume)
	select {
	case got := <-first:
		if got != old {
			t.Fatalf("first ID = %x, want %x", got, old)
		}
	case <-time.After(time.Second):
		t.Fatal("read stuck")
	}
	nt.mu.Lock()
	d := nt.find("co")
	nt.addLeaf(d, "file")
	nt.drop(nt.child(d, "file"))
	replacement := nt.alloc(d, "file").id
	nt.mu.Unlock()
	close(consume)
	select {
	case got := <-waiter:
		if got != replacement || got == old {
			t.Fatalf("waiter ID = %x, want %x", got, replacement)
		}
	case <-time.After(time.Second):
		t.Fatal("waiter stuck")
	}
}
