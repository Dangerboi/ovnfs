package export

import (
	"fmt"
	"testing"
)

func BenchmarkResidentDirectory(b *testing.B) {
	ns := newNamespace()
	const dir = "co/src/deep/directory"
	names := make([]string, 1024)
	for i := range names {
		names[i] = fmt.Sprintf("file-%04d", i)
		ns.add(dir + "/" + names[i])
	}
	nt := newNodeTable(ns.list)
	defer nt.close()
	ids := make([]uint64, len(names))
	for i, name := range names {
		ids[i] = nt.id(dir + "/" + name)
	}
	infos, err := ns.list(dir)
	if err != nil {
		b.Fatal(err)
	}
	b.Run("child-id", func(b *testing.B) {
		b.ReportAllocs()
		j := 0
		for b.Loop() {
			if id := nt.childID(dir, names[j]); id != ids[j] {
				b.Fatalf("identity changed: %d, want %d", id, ids[j])
			}
			j = (j + 1) % len(names)
		}
	})
	b.Run("unchanged-listing", func(b *testing.B) {
		nt.mu.Lock()
		defer nt.mu.Unlock()
		d := nt.find(dir)
		b.ReportAllocs()
		for b.Loop() {
			if !nt.install(d, d.dir.gen, infos) {
				b.Fatal("listing rejected")
			}
		}
	})
}
