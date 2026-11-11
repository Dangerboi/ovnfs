package export

import (
	"fmt"
	"os"
	"sort"
	"sync"
	"testing"
	"time"
)

// TestColdWideLatency is an opt-in latency probe, shared by the pre-batching
// and batched implementations. It reports directory and unrelated lookup tails.
func TestColdWideLatency(t *testing.T) {
	if os.Getenv("OVNFS_LATENCY") == "" {
		t.Skip("set OVNFS_LATENCY=1")
	}
	ns := newNamespace("other/file", "pressure/entry")
	for i := range 20000 {
		ns.add(fmt.Sprintf("wide/f%05d", i))
	}
	nt := newNodeTable(ns.list)
	defer nt.close()
	for i := range 20000 {
		nt.id(fmt.Sprintf("wide/f%05d", i))
	}
	nt.id("other/file")
	nt.cacheLimit = 0
	nt.id("pressure/entry")
	infos, err := ns.list("wide")
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var lat []time.Duration
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			start := time.Now()
			nt.id("other/file")
			lat = append(lat, time.Since(start))
			time.Sleep(100 * time.Microsecond)
		}
	})
	start := time.Now()
	nt.observe(nt.startListing("wide"), infos)
	for _, info := range infos {
		if nt.id("wide/"+info.Name()) == 0 {
			t.Fatal("identity failed")
		}
	}
	elapsed := time.Since(start)
	close(stop)
	wg.Wait()
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	if len(lat) == 0 {
		t.Fatal("no concurrent lookup samples")
	}
	t.Logf("wide_names=%d rewalk=%s unrelated_samples=%d p50=%s p95=%s max=%s", len(infos), elapsed, len(lat), lat[len(lat)/2], lat[len(lat)*95/100], lat[len(lat)-1])
}
