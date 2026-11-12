package checkout

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stencil-hq/ovnfs/api"
)

// TestLargeSnapshotMetadataStaysOutOfState keeps a large snapshot's metadata out
// of state.json, which every registry change rewrites, and reads the snapshot
// back after a restart.
func TestLargeSnapshotMetadataStaysOutOfState(t *testing.T) {
	f := newFixture(t)
	files := map[string]string{}
	for i := range 200 {
		files[fmt.Sprintf("src/file%03d.go", i)] = "package p\n"
	}
	f.newCheckout(folder(t, files), "app")
	if err := f.reg.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(f.state, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	before := info.Size()
	f.open()
	for p := range files {
		f.put("app", p, "package p // edited\n")
	}
	reply, err := f.reg.Snap(context.Background(), api.SnapArgs{Checkout: "app", Wait: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.reg.Close(); err != nil {
		t.Fatal(err)
	}
	if info, err = os.Stat(filepath.Join(f.state, "state.json")); err != nil {
		t.Fatal(err)
	}
	grew := info.Size() - before
	f.open()
	f.reg.mu.RLock()
	metadata := len(f.reg.layers[f.reg.snapshots[reply.Snapshot.ID].layer].layer.Metadata())
	f.reg.mu.RUnlock()
	if metadata <= embedLimit {
		t.Fatalf("the snapshot's %d bytes of metadata do not exceed the limit", metadata)
	}
	if grew >= int64(metadata) {
		t.Fatalf("a snapshot with %d bytes of metadata grew state.json by %d bytes", metadata, grew)
	}
	if _, err := f.reg.New(context.Background(), api.NewArgs{Snapshot: reply.Snapshot.ID, Name: "view"}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"src/file000.go", "src/file199.go"} {
		if got := f.read("view", p); got != "package p // edited\n" {
			t.Fatalf("%s reads %q", p, got)
		}
	}
}

// TestSnapshotMetadataSharedState keeps metadata-only history off per-layer
// filesystem blocks and restores it after a crash left a redundant manifest.
func TestSnapshotMetadataSharedState(t *testing.T) {
	f := newFixture(t)
	f.newCheckout(folder(t, map[string]string{"file": "base", "gone": "old"}), "app")
	var snapshots []string
	for _, change := range []func(){
		func() { f.put("app", "file", "edited") },
		func() { f.remove("app", "gone") },
		func() {
			var s Session
			if err := f.reg.Enter("app", &s); err != nil {
				t.Fatal(err)
			}
			defer s.Done()
			if err := s.FS.MkdirAll("empty/nested", 0o755); err != nil {
				t.Fatal(err)
			}
		},
	} {
		change()
		reply, err := f.reg.Snap(context.Background(), api.SnapArgs{Checkout: "app", Wait: true})
		if err != nil {
			t.Fatal(err)
		}
		snapshots = append(snapshots, reply.Snapshot.ID)
	}
	if err := f.reg.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(f.state, "layers"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("stored-only history still allocates %d layer paths", len(entries))
	}
	d := readDisk(t, f.state)
	if len(d.Layers) != len(snapshots) {
		t.Fatalf("history has %d layers for %d distinct snapshots", len(d.Layers), len(snapshots))
	}
	for _, layer := range d.Layers {
		if len(layer.Metadata) == 0 {
			t.Fatalf("layer %s has no recoverable metadata", layer.ID)
		}
	}
	leftover := filepath.Join(f.state, "layers", d.Layers[0].ID)
	if err := os.WriteFile(leftover, d.Layers[0].Metadata, 0o600); err != nil {
		t.Fatal(err)
	}
	f.open()
	for i, id := range snapshots {
		name := "view-" + id
		if _, err := f.reg.New(context.Background(), api.NewArgs{Snapshot: id, Name: name}); err != nil {
			t.Fatal(err)
		}
		if got := f.read(name, "file"); got != "edited" {
			t.Fatalf("snapshot %s reads %q", id, got)
		}
		want := "old"
		if i > 0 {
			want = "<absent>"
		}
		if got := f.read(name, "gone"); got != want {
			t.Fatalf("snapshot %s: gone = %q, want %q", id, got, want)
		}
		if i == 2 {
			var s Session
			if err := f.reg.Enter(name, &s); err != nil {
				t.Fatal(err)
			}
			info, err := s.FS.Lstat("empty/nested")
			s.Done()
			if err != nil || !info.IsDir() {
				t.Fatalf("empty directory lost: %v %v", info, err)
			}
		}
	}
	if _, err := os.Lstat(leftover); !os.IsNotExist(err) {
		t.Fatalf("redundant manifest remains after recovery: %v", err)
	}
}
