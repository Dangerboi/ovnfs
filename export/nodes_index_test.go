package export

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestColdRecoveryBatchesOwners(t *testing.T) {
	for _, budget := range []int64{0, 1 << 20} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			ns := newNamespace()
			nt := newNodeTable(ns.list)
			var ids []uint64
			for i := range 256 {
				p := fmt.Sprintf("co/f%d", i)
				ns.add(p)
				ids = append(ids, nt.id(p))
			}
			forceCold(t, nt, ns, "co")
			nt.cacheLimit = budget
			var reads atomic.Int32
			nt.indexIO = func() { reads.Add(1) }
			for i, id := range ids {
				if got := pathOf(t, nt, id); got != fmt.Sprintf("co/f%d", i) {
					t.Fatal(got)
				}
			}
			maxReads := int32(1)
			if budget == 0 {
				maxReads = 5 // four recovery reads and one pressure-directory spill
			}
			if reads.Load() > maxReads {
				t.Fatalf("%d index operations for one directory, want <= %d", reads.Load(), maxReads)
			}
		})
	}
}

func TestIndexIODoesNotHoldNodeMutex(t *testing.T) {
	for _, op := range []string{"read", "write"} {
		t.Run(op, func(t *testing.T) {
			ns := newNamespace("co/file", "other/file", "pressure/entry")
			nt := newNodeTable(ns.list)
			id := nt.id("co/file")
			other := nt.id("other/file")
			if op == "read" {
				forceCold(t, nt, ns, "co")
				nt.cacheLimit = 1 << 20
			} else {
				defer nt.close()
				nt.cacheLimit = 0
			}
			entered, resume := make(chan struct{}), make(chan struct{})
			var once sync.Once
			nt.indexIO = func() { once.Do(func() { close(entered); <-resume }) }
			done := make(chan struct{})
			go func() {
				defer close(done)
				if op == "read" {
					nt.path(id)
				} else {
					nt.id("pressure/entry")
				}
			}()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("index operation did not start")
			}
			unrelated := make(chan struct{})
			go func() { nt.mu.Lock(); _ = nt.byID[other]; nt.mu.Unlock(); close(unrelated) }()
			select {
			case <-unrelated:
			case <-time.After(time.Second):
				close(resume)
				t.Fatal("index I/O held node mutex")
			}
			// Move the owning directory while the snapshot is in flight. Publication
			// must revalidate location and ownership instead of evicting stale state.
			nt.mu.Lock()
			d := nt.find("co")
			nt.moved(d)
			delete(d.parent.kids, d.name)
			d.name = "moved"
			d.parent.kids[d.name] = d
			d.state().gen++
			nt.mu.Unlock()
			close(resume)
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("index I/O did not finish")
			}
			if got := pathOf(t, nt, id); got != "moved/file" {
				t.Fatal(got)
			}
		})
	}
}

func TestDegradedColdReadsDoNotAccumulateOwners(t *testing.T) {
	ns := newNamespace("pressure/entry")
	nt := newNodeTable(ns.list)
	defer nt.close()
	var ids []uint64
	for i := range 32 {
		p := fmt.Sprintf("co/d%d/file", i)
		ns.add(p)
		ids = append(ids, nt.id(p))
	}
	nt.cacheLimit = 0
	nt.id("pressure/entry")
	nt.spillErr = fmt.Errorf("unavailable spill")
	nt.spillRetry = time.Now().Add(time.Hour)
	used := nt.cacheUsed
	for i, id := range ids {
		p := fmt.Sprintf("co/d%d/file", i)
		if got := pathOf(t, nt, id); got != p {
			t.Fatal(got)
		}
		if got := nt.id(p); got != id {
			t.Fatalf("%x != %x", got, id)
		}
	}
	refs := nt.subtree("co")
	seen := make(map[uint64]bool)
	for _, ref := range refs {
		seen[ref] = true
	}
	for _, id := range ids {
		if !seen[id] {
			t.Fatal("cold subtree omitted an owner")
		}
	}
	if nt.cacheUsed != used {
		t.Fatalf("cold reads grew cache %d -> %d", used, nt.cacheUsed)
	}
	release := nt.retain(ids[0])
	if nt.byID[ids[0]] == nil || nt.byID[ids[0]].holds != 1 {
		t.Fatal("cold live state was not retained")
	}
	release()
}

func TestGuardAcquiredDuringSpill(t *testing.T) {
	for _, prepared := range []bool{false, true} {
		t.Run(fmt.Sprint(prepared), func(t *testing.T) {
			ns := newNamespace("co/file", "pressure/entry")
			nt := newNodeTable(ns.list)
			defer nt.close()
			old := nt.id("co/file")
			nt.cacheLimit = 0
			entered, resume := make(chan struct{}), make(chan struct{})
			var once sync.Once
			nt.indexIO = func() { once.Do(func() { close(entered); <-resume }) }
			done := make(chan struct{})
			go func() { nt.id("pressure/entry"); close(done) }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("spill did not start")
			}
			var release func()
			var err error
			if prepared {
				release, err = nt.prepareRename("co/file", "co/moved")
			} else {
				release, err = nt.guard(false, "co/file")
			}
			if err != nil {
				close(resume)
				t.Fatal(err)
			}
			close(resume)
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("spill did not finish")
			}
			nt.mu.Lock()
			present := nt.find("co").dir.reading > 0 && (nt.byID[old] != nil || nt.find("co").leaf("file"))
			nt.mu.Unlock()
			if !present {
				release()
				t.Fatal("spill evicted prepared mutation state")
			}
			ns.move("co/file", "co/moved")
			nt.rename("co/file", "co/moved")
			release()
			if got := pathOf(t, nt, old); got != "co/moved" {
				t.Fatal(got)
			}
		})
	}
}

func TestRecoveryAllowsDeepColdPaths(t *testing.T) {
	ns := newNamespace("pressure/entry")
	nt := newNodeTable(ns.list)
	defer nt.close()
	p := "co"
	for range 24 {
		p += "/d"
		ns.add(p + "/file")
		nt.id(p + "/file")
	}
	old := nt.id(p + "/file")
	nt.cacheLimit = 0
	nt.id("pressure/entry")
	nt.cacheLimit = 1 << 20
	if got := nt.id(p + "/file"); got != old {
		t.Fatalf("deep recovery %x != %x", got, old)
	}
	release, err := nt.prepareRename(p+"/file", "pressure/moved")
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func TestDegradedConcurrentRetain(t *testing.T) {
	for _, byPath := range []bool{false, true} {
		t.Run(fmt.Sprint(byPath), func(t *testing.T) {
			ns := newNamespace("co/file", "pressure/entry")
			nt := newNodeTable(ns.list)
			id := nt.id("co/file")
			forceCold(t, nt, ns, "co")
			nt.spillErr = fmt.Errorf("unavailable")
			nt.spillRetry = time.Now().Add(time.Hour)
			entered, resume := make(chan struct{}), make(chan struct{})
			var once sync.Once
			nt.indexIO = func() { once.Do(func() { close(entered); <-resume }) }
			waiting := make(chan struct{})
			nt.indexWait = func() { close(waiting) }
			held := make(chan func(), 1)
			go func() { held <- nt.retain(id) }()
			<-entered
			found := make(chan bool, 1)
			go func() {
				if byPath {
					found <- nt.id("co/file") == id
				} else {
					p, ok := nt.path(id)
					found <- ok && p == "co/file"
				}
			}()
			select {
			case <-waiting:
			case <-time.After(time.Second):
				close(resume)
				t.Fatal("lookup did not wait for index")
			}
			close(resume)
			release := <-held
			defer release()
			select {
			case ok := <-found:
				if !ok {
					t.Fatal("concurrent retain made existing identity stale")
				}
			case <-time.After(time.Second):
				t.Fatal("lookup did not finish")
			}
		})
	}
}

func TestRetiredIndexReclaimed(t *testing.T) {
	ns := newNamespace("co/file", "pressure/entry")
	nt := newNodeTable(ns.list)
	nt.id("co/file")
	forceCold(t, nt, ns, "co")
	file := nt.coldPath(nt.find("co"))
	size := nt.find("co").dir.coldBytes
	before := nt.coldBytes
	nt.remove("co")
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("retired index survived", err)
	}
	if nt.coldGarbage != nil {
		t.Fatal("drained cleanup queue retained nodes")
	}
	if nt.coldBytes != before-size {
		t.Fatalf("index bytes %d != %d", nt.coldBytes, before-size)
	}
}

func TestCommittedColdMutationsAfterFault(t *testing.T) {
	for _, op := range []string{"remove", "collision"} {
		t.Run(op, func(t *testing.T) {
			ns := newNamespace("co/"+collideA, "pressure/entry")
			log := filepath.Join(t.TempDir(), "nodes.log")
			nt := openLogged(t, log, ns)
			old := nt.id("co/" + collideA)
			nt.cacheLimit = 0
			nt.id("pressure/entry")
			nt.cacheLimit = 1 << 20
			target := "co/" + collideA
			if op == "collision" {
				target = "co/" + collideB
			}
			release, err := nt.guard(false, target)
			if err != nil {
				t.Fatal(err)
			}
			if op == "remove" {
				ns.del(target)
			} else {
				ns.add(target)
			}
			nt.mu.Lock()
			nt.cacheFailure(os.ErrPermission)
			nt.mu.Unlock()
			if op == "remove" {
				nt.remove(target)
				ns.add(target)
			}
			nt.created(target)
			release()
			if nt.close() == nil {
				t.Fatal("fault disappeared")
			}
			nt = openLogged(t, log, ns)
			defer nt.close()
			fresh := nt.id(target)
			if fresh == 0 || fresh == old || fresh&derivedBit != 0 {
				t.Fatal("committed replacement was not logged")
			}
			if op == "remove" {
				if _, ok := nt.path(old); ok {
					t.Fatal("removed handle names replacement after restart")
				}
			} else if got := pathOf(t, nt, old); got != "co/"+collideA {
				t.Fatal(got)
			}
		})
	}
}

func TestDegradedCheckoutRetirement(t *testing.T) {
	ns := newNamespace("co/file", "pressure/entry")
	log := filepath.Join(t.TempDir(), "nodes.log")
	nt := openLogged(t, log, ns)
	old := nt.id("co/file")
	key := nt.find("co").key
	nt.cacheLimit = 0
	nt.id("pressure/entry")
	nt.spillErr = os.ErrPermission
	nt.spillRetry = time.Now().Add(time.Hour)
	e := &Export{fs: &nodeFS{nodes: nt}}
	e.OnRecall(func(uint64) {})
	e.Retire("co")
	if nt.byKey[key] != nil || pathOf(t, nt, old) != "<stale>" {
		t.Fatal("degraded retirement left old key live")
	}
	ns.del("co")
	ns.add("co/file")
	nt.cacheLimit = 1 << 20
	nt.spillErr = nil
	fresh := nt.id("co/file")
	if fresh == old {
		t.Fatal("checkout name reuse inherited old handle")
	}
	closeLogged(t, nt)
	nt = openLogged(t, log, ns)
	defer nt.close()
	if _, ok := nt.path(old); ok {
		t.Fatal("retired checkout handle recovered after restart")
	}
}

func TestDegradedInvalidationRetiresColdOwners(t *testing.T) {
	for _, whole := range []bool{false, true} {
		t.Run(fmt.Sprint(whole), func(t *testing.T) {
			n := newMetadataTestFS(t)
			for _, p := range []string{"co/" + collideA, "pressure/entry"} {
				dir, _ := split(p)
				if err := n.Filesystem.MkdirAll(dir, 0755); err != nil {
					t.Fatal(err)
				}
				f, err := n.Filesystem.Create(p)
				if err != nil {
					t.Fatal(err)
				}
				f.Close()
			}
			log := filepath.Join(t.TempDir(), "nodes.log")
			nt, err := openNodeTable(log, n.Filesystem.ReadDir)
			if err != nil {
				t.Fatal(err)
			}
			n.nodes = nt
			old := nt.id("co/" + collideA)
			nt.cacheLimit = 0
			nt.id("pressure/entry")
			nt.spillErr = os.ErrPermission
			nt.spillRetry = time.Now().Add(time.Hour)
			if err := n.Filesystem.Remove("co/" + collideA); err != nil {
				t.Fatal(err)
			}
			paths := []string{collideA}
			// Explicit paths retire missing handles; tree rescans only recall them.
			if whole {
				(&Export{fs: n}).Invalidate("co", nil, []string{""})
			}
			(&Export{fs: n}).Invalidate("co", paths, nil)
			if nt.cacheErr != nil || nt.spillErr == nil || nt.cacheUsed <= nt.cacheLimit {
				t.Fatal("probe lost degraded pressure")
			}
			if _, ok := nt.path(old); ok {
				t.Fatal("invalidation left cold handle live")
			}
			f, err := n.Filesystem.Create("co/" + collideB)
			if err != nil {
				t.Fatal(err)
			}
			f.Close()
			nt.cacheLimit = 1 << 20
			nt.spillErr = nil
			nt.created("co/" + collideB)
			if nt.id("co/"+collideB) == old {
				t.Fatal("collision inherited invalidated handle")
			}
			closeLogged(t, nt)
			nt, err = openNodeTable(log, n.Filesystem.ReadDir)
			if err != nil {
				t.Fatal(err)
			}
			defer nt.close()
			if _, ok := nt.path(old); ok {
				t.Fatal("invalidated handle recovered after restart")
			}
		})
	}
}

func TestPreparedDestinationRemainsResident(t *testing.T) {
	for _, fatal := range []bool{false, true} {
		t.Run(fmt.Sprint(fatal), func(t *testing.T) {
			ns := newNamespace("co/src", "co/dst/existing", "pressure/entry")
			log := filepath.Join(t.TempDir(), "nodes.log")
			nt := openLogged(t, log, ns)
			old := nt.id("co/src")
			release, err := nt.prepareRename("co/src", "co/dst/moved")
			if err != nil {
				t.Fatal(err)
			}
			nt.id("co/dst/existing")
			nt.cacheLimit = 0
			nt.id("pressure/entry")
			d := nt.find("co/dst")
			if d.dir == nil || d.dir.reading == 0 || d.dir.entries == nil {
				release()
				t.Fatal("prepared destination escaped guard")
			}
			reads := 0
			nt.indexIO = func() { reads++ }
			ns.move("co/src", "co/dst/moved")
			nt.mu.Lock()
			if fatal {
				nt.cacheFailure(os.ErrPermission)
			} else {
				nt.spillErr = os.ErrPermission
				nt.spillRetry = time.Now().Add(time.Hour)
			}
			nt.mu.Unlock()
			nt.rename("co/src", "co/dst/moved")
			release()
			if reads != 0 || nt.byID[old] == nil || nt.byID[old].fullPath() != "co/dst/moved" {
				t.Fatal("committed move needed index recovery or lost its identity")
			}
			if err := nt.close(); (err != nil) != fatal {
				t.Fatal(err)
			}
			nt = openLogged(t, log, ns)
			defer nt.close()
			if got := pathOf(t, nt, old); got != "co/dst/moved" {
				t.Fatal(got)
			}
		})
	}
}

func TestRetainResumesAfterRename(t *testing.T) {
	ns := newNamespace("co/src", "co/dst", "pressure/entry")
	log := filepath.Join(t.TempDir(), "nodes.log")
	nt := openLogged(t, log, ns)
	dst := nt.id("co/dst")
	closeLogged(t, nt)
	nt = openLogged(t, log, ns)
	src := nt.id("co/src")
	forceCold(t, nt, ns, "co")
	nt.cacheLimit = 1 << 20
	entered, resume := make(chan struct{}), make(chan struct{})
	waiting, resumeWait := make(chan struct{}), make(chan struct{})
	var once sync.Once
	nt.indexIO = func() { once.Do(func() { close(entered); <-resume }) }
	nt.indexWait = func() { close(waiting); <-resumeWait }
	heldB, heldA := make(chan func(), 1), make(chan func(), 1)
	go func() { heldB <- nt.retain(src) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first retain did not read")
	}
	go func() { heldA <- nt.retain(src) }()
	select {
	case <-waiting:
	case <-time.After(time.Second):
		t.Fatal("second retain did not wait")
	}
	close(resume)
	releaseB := <-heldB
	defer releaseB()
	releaseMove, err := nt.prepareRename("co/src", "co/dst")
	if err != nil {
		t.Fatal(err)
	}
	ns.move("co/src", "co/dst")
	nt.rename("co/src", "co/dst")
	releaseMove()
	close(resumeWait)
	var releaseA func()
	select {
	case releaseA = <-heldA:
	case <-time.After(time.Second):
		t.Fatal("second retain did not resume")
	}
	defer releaseA()
	if nt.find("co").leaf("dst") {
		t.Fatal("retainer created a second destination identity")
	}
	if got := pathOf(t, nt, src); got != "co/dst" {
		t.Fatal(got)
	}
	if _, ok := nt.path(dst); ok {
		t.Fatal("overwritten destination handle names source")
	}
}

func TestCompletedIndexReadIsShared(t *testing.T) {
	for _, action := range []string{"path", "id", "lookup", "subtree"} {
		t.Run(action, func(t *testing.T) {
			ns := newNamespace("co/file", "pressure/entry")
			nt := newNodeTable(ns.list)
			old := nt.id("co/file")
			forceCold(t, nt, ns, "co")
			nt.spillErr = os.ErrPermission
			nt.spillRetry = time.Now().Add(time.Hour)
			first, last := make(chan struct{}), make(chan struct{})
			resumeFirst, resumeLast := make(chan struct{}), make(chan struct{})
			var reads atomic.Int32
			nt.indexIO = func() {
				switch reads.Add(1) {
				case 1:
					close(first)
					<-resumeFirst
				case 5:
					close(last)
					<-resumeLast
				}
			}
			waiting, resumeWait := make(chan struct{}), make(chan struct{})
			var once sync.Once
			nt.indexWait = func() { once.Do(func() { close(waiting); <-resumeWait }) }
			reader := func() chan struct{} {
				done := make(chan struct{})
				go func() { nt.path(old); close(done) }()
				return done
			}
			// The first operation must request the full set: sparse Path reads
			// intentionally do not provide unrelated owners to subtree waiters.
			fullDone := make(chan struct{})
			go func() { nt.mu.Lock(); nt.readOwners(nt.find("co")); nt.mu.Unlock(); close(fullDone) }()
			b := fullDone
			select {
			case <-first:
			case <-time.After(time.Second):
				t.Fatal("first read did not start")
			}
			looked := make(chan bool, 1)
			done := make(chan struct{})
			go func() {
				defer close(done)
				ok := false
				switch action {
				case "path":
					_, ok = nt.path(old)
				case "id":
					ok = nt.id("co/file") == old
				case "lookup":
					id, found := nt.lookup("co/file")
					ok = found && id == old
				case "subtree":
					for _, ref := range nt.subtree("co") {
						ok = ok || ref == old
					}
				}
				looked <- ok
				if ok && (action == "lookup" || action == "subtree") {
					nt.remove("co/file")
				}
			}()
			select {
			case <-waiting:
			case <-time.After(time.Second):
				t.Fatal("lookup did not wait")
			}
			close(resumeFirst)
			<-b
			for range 3 {
				nt.path(old)
			}
			b = reader()
			select {
			case <-last:
			case <-time.After(time.Second):
				t.Fatal("fifth read did not start")
			}
			close(resumeWait)
			select {
			case ok := <-looked:
				if !ok {
					t.Error("serialized read lost an issued owner")
				}
			case <-time.After(time.Second):
				close(resumeLast)
				t.Fatal("waiter discarded completed read behind later I/O")
			}
			close(resumeLast)
			<-b
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("retirement did not finish")
			}
			if action == "lookup" || action == "subtree" {
				if _, ok := nt.path(old); ok {
					t.Fatal("invalidation stage skipped retirement")
				}
			}
			if nt.cacheErr != nil {
				t.Fatal("contention latched a terminal fault")
			}
		})
	}
}

func TestFirstSpillPreservesGuardedCompletion(t *testing.T) {
	ns := newNamespace("co/"+collideA, "pressure/entry")
	log := filepath.Join(t.TempDir(), "nodes.log")
	nt := openLogged(t, log, ns)
	defer func() { nt.close() }()
	old := nt.id("co/" + collideA)
	nt.cacheLimit = 0
	entered, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	nt.indexIO = func() { once.Do(func() { close(entered); <-resume }) }
	done := make(chan struct{})
	go func() { nt.id("pressure/entry"); close(done) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first spill did not start")
	}
	release, err := nt.guard(false, "co/"+collideB)
	if err != nil {
		close(resume)
		t.Fatal(err)
	}
	close(resume)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("spill did not finish")
	}
	ns.add("co/" + collideB)
	nt.mu.Lock()
	nt.cacheFailure(os.ErrPermission)
	nt.mu.Unlock()
	nt.created("co/" + collideB)
	release()
	if err := nt.close(); err == nil {
		t.Fatal("fault disappeared")
	}
	ns.del("co/" + collideA)
	nt = openLogged(t, log, ns)
	if got := pathOf(t, nt, old); got != "<stale>" {
		t.Fatalf("guarded completion lost its collision record: old handle = %s", got)
	}
}
