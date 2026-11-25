package checkout

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/stats"
	"github.com/stencil-hq/ovnfs/watch"
)

// trashed lists what waits in the fixture's trash.
func (f *fixture) trashed() []string {
	f.t.Helper()
	entries, err := os.ReadDir(filepath.Join(f.state, trashDir))
	if err != nil {
		f.t.Fatal(err)
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	return names
}

// awaitTrash waits for real background I/O, not a wall-time estimate of when
// the OS will schedule it. A surviving requested tree after one pass is a failure.
func (f *fixture) awaitTrash(labels ...string) {
	f.t.Helper()
	done := make(chan struct{})
	deadline := time.NewTimer(time.Minute)
	defer deadline.Stop()
	select {
	case f.reg.trashWake <- done:
	case <-deadline.C:
		f.t.Fatal("trash reaper did not accept a pass")
	}
	select {
	case <-done:
	case <-deadline.C:
		f.t.Fatal("trash reaper did not finish its pass")
	}
	for _, name := range f.trashed() {
		if len(labels) == 0 || slices.Contains(labels, trashLabel(name)) {
			f.t.Fatalf("trash tree survived a completed deletion pass: %s", name)
		}
	}
}

// TestDeadCacheSnapshotIsDeletedWhileServing checks that the cache snapshot
// of a removed checkout leaves its base and is deleted from the trash without
// a restart, while a checkout on the same base keeps its own.
func TestDeadCacheSnapshotIsDeletedWhileServing(t *testing.T) {
	f := newFixture(t)
	if err := f.reg.Close(); err != nil {
		t.Fatal(err)
	}
	f.frozenLocal = true
	f.open()
	dir := folder(t, map[string]string{"src/main": "one", ".env": "SECRET=1"})
	f.newCheckout(dir, "a")
	first, root, _ := f.checkoutCaches("a")
	write(t, filepath.Join(dir, ".env"), "SECRET=2")
	f.reg.folderChanged([]watch.Change{{Path: filepath.Join(root, ".env")}})
	f.newCheckout(dir, "b")
	second, _, _ := f.checkoutCaches("b")
	if first == second {
		t.Fatalf("both checkouts read snapshot %s; the folder changed between them", first)
	}
	if err := f.reg.Rm(t.Context(), api.RmArgs{Checkout: "a"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the removed checkout's cache snapshot was not deleted", func() bool {
		_, _, onDisk := f.checkoutCaches("b")
		return !slices.Contains(onDisk, first) && slices.Contains(onDisk, second)
	})
	f.awaitTrash("caches-" + first)
	if got := f.read("b", ".env"); got != "SECRET=2" {
		t.Errorf("b: .env = %q after a's snapshot went, want its own SECRET=2", got)
	}
}

// TestFileCallsAreTimedPerCheckout checks that a call entered by name and one
// entered by export slot both count for their checkout, each with the time
// it took and no more.
func TestFileCallsAreTimedPerCheckout(t *testing.T) {
	rec, err := stats.Open(filepath.Join(t.TempDir(), "stats"), t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rec.Close) // after the registry's, registered by newFixture
	f := newFixture(t)
	if err := f.reg.Close(); err != nil {
		t.Fatal(err)
	}
	f.stats = rec
	f.open()
	f.newCheckout(folder(t, map[string]string{"a": "base"}), "app")
	f.reg.mu.RLock()
	slot := f.reg.checkouts["app"].meta.Slot
	f.reg.mu.RUnlock()
	var s Session
	if err := f.reg.Enter("app", &s); err != nil {
		t.Fatal(err)
	}
	s.Done()
	if _, _, err := f.reg.EnterSlot(slot, &s); err != nil {
		t.Fatal(err)
	}
	s.Done()
	reply, err := rec.Report(t.Context(), api.StatsArgs{})
	if err != nil {
		t.Fatal(err)
	}
	for _, timer := range reply.Timers {
		if timer.Group == "calls" && timer.Name == "app" {
			// newCheckout's own reads count too; none of them takes a second.
			if timer.Count < 2 || timer.Seconds >= float64(timer.Count) {
				t.Errorf("app = %d calls in %v s; want at least the 2 made here, each well under a second", timer.Count, timer.Seconds)
			}
			return
		}
	}
	t.Errorf("timers = %+v; want the calls of app", reply.Timers)
}

// TestTrashBacklogIsRelieved checks that more discarded trees than
// trashBacklog do not all wait for the low-priority thread: the registry
// deletes the excess at normal priority, and every tree goes.
func TestTrashBacklogIsRelieved(t *testing.T) {
	rec, err := stats.Open(filepath.Join(t.TempDir(), "stats"), t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rec.Close) // after the registry's, registered by newFixture
	f := newFixture(t)
	if err := f.reg.Close(); err != nil {
		t.Fatal(err)
	}
	for i := range trashBacklog + 2 {
		write(t, filepath.Join(f.state, "uppers", fmt.Sprintf("orphan%d", i), "file"), "x")
	}
	f.stats = rec
	f.open()
	f.awaitTrash()
	eventually(t, "the excess was not deleted by trash.relieve", func() bool {
		reply, err := rec.Report(t.Context(), api.StatsArgs{})
		if err != nil {
			t.Fatal(err)
		}
		return slices.ContainsFunc(reply.Jobs, func(j api.StatsJob) bool { return j.Name == "trash.relieve" })
	})
}
