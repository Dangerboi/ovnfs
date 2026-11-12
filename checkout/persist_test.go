package checkout

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stencil-hq/ovnfs/api"
)

// readDisk loads the state manifest of a closed registry.
func readDisk(t *testing.T, state string) diskState {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(state, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var d diskState
	if err := json.Unmarshal(data, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

// TestOpenUpgradesVersion4State opens state an older build wrote and rewrites it
// at once, so builds that read only version 4 refuse it from then on instead of
// opening a store whose packs they would truncate.
func TestOpenUpgradesVersion4State(t *testing.T) {
	f := newFixture(t)
	f.newCheckout(folder(t, map[string]string{"a": "base"}), "c")
	f.put("c", "a", "edited")
	if _, err := f.reg.Snap(context.Background(), api.SnapArgs{Checkout: "c", Wait: true}); err != nil {
		t.Fatal(err)
	}
	if err := f.reg.Close(); err != nil {
		t.Fatal(err)
	}
	d := readDisk(t, f.state)
	d.Version = 4
	if err := writeState(f.state, d); err != nil {
		t.Fatal(err)
	}
	f.open()
	if got := readDisk(t, f.state).Version; got <= 4 {
		t.Fatalf("opened state still has version %d", got)
	}
	if got := f.read("c", "a"); got != "edited" {
		t.Fatalf("a reads %q after the upgrade", got)
	}
}

// TestCompactStateRebuildsStacks checks that the manifest is compact, that snapshot
// and checkout records do not repeat layer stacks, and that every stack reopens.
func TestCompactStateRebuildsStacks(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.newCheckout(folder(t, map[string]string{"file": "base"}), "app")
	var ids []string
	for _, content := range []string{"one", "two", "three"} {
		f.put("app", "file", content)
		reply, err := f.reg.Snap(ctx, api.SnapArgs{Checkout: "app", Wait: true})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, reply.Snapshot.ID)
	}
	if err := f.reg.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(f.state, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(bytes.TrimSpace(data), []byte("\n")) {
		t.Fatalf("state.json is indented:\n%s", data)
	}
	d := readDisk(t, f.state)
	for _, s := range d.Snapshots {
		if s.Stack != nil {
			t.Fatalf("snapshot %s repeats its stack %v", s.ID, s.Stack)
		}
	}
	for _, h := range d.Checkouts {
		if h.Stack != nil {
			t.Fatalf("checkout %s repeats its stack %v", h.Name, h.Stack)
		}
	}
	f.open()
	for i, want := range []string{"one", "two", "three"} {
		name := "view-" + want
		if _, err := f.reg.New(ctx, api.NewArgs{Snapshot: ids[i], Name: name}); err != nil {
			t.Fatal(err)
		}
		if got := f.read(name, "file"); got != want {
			t.Fatalf("snapshot %s reads %q, want %q", ids[i], got, want)
		}
	}
	if got := f.read("app", "file"); got != "three" {
		t.Fatalf("checkout reads %q", got)
	}
}

// frozenRepo snapshots a dirty repository checkout, closes the registry and returns the
// committed manifest with the snapshot and layer the freeze created.
func frozenRepo(t *testing.T, fork bool) (f *fixture, dir string, d diskState, snapshot, layer, tree string) {
	t.Helper()
	f = newFixture(t)
	dir = revisionRepo(t)
	f.newCheckout(dir, "app")
	f.put("app", "tracked.txt", "changed\n")
	var reply api.SnapReply
	var err error
	if fork {
		var fr api.ForkReply
		fr, err = f.reg.Fork(context.Background(), api.ForkArgs{Checkout: "app", Name: "child"})
		if err == nil {
			reply, err = f.reg.waitHashed(context.Background(), f.reg.snapshots[fr.Snapshot.ID])
		}
	} else {
		reply, err = f.reg.Snap(context.Background(), api.SnapArgs{Checkout: "app", Wait: true})
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := f.reg.Close(); err != nil {
		t.Fatal(err)
	}
	d = readDisk(t, f.state)
	for _, s := range d.Snapshots {
		if s.ID == reply.Snapshot.ID {
			layer = s.Layer
		}
	}
	if layer == "" {
		t.Fatalf("snapshot %s froze no layer", reply.Snapshot.ID)
	}
	return f, dir, d, reply.Snapshot.ID, layer, reply.Snapshot.Tree
}

// intentState rewinds a committed freeze of app to the manifest its recovery intent
// published: the snapshot recorded unhashed, the checkout still on its old head, the
// layer not yet indexed, and without the forked child.
func intentState(d diskState, snapshot, layer string) diskState {
	for i := range d.Snapshots {
		if d.Snapshots[i].ID == snapshot {
			d.Snapshots[i].Tree = nil
		}
	}
	var checkouts []diskCheckout
	for _, h := range d.Checkouts {
		if h.Name == "child" {
			continue
		}
		if h.Name == "app" {
			h.Head, h.Stack = nil, nil
		}
		checkouts = append(checkouts, h)
	}
	d.Checkouts = checkouts
	var layers []diskLayer
	for _, l := range d.Layers {
		if l.ID != layer {
			layers = append(layers, l)
		}
	}
	d.Layers = layers
	d.Intents = []diskIntent{{Op: "freezing", Checkout: "app", Layer: layer, Snapshot: snapshot}}
	return d
}

// frozenChanges reconstructs the raw upper before asynchronous storage removed it.
func frozenChanges(t *testing.T, dir, gitdir string) {
	t.Helper()
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeGitfile(dir, gitdir); err != nil {
		t.Fatal(err)
	}
}

// TestFreezeCrashRecovery reopens state left by a crash at each step of snap and fork:
// before the upper moved (rolled back), after it moved (rolled forward, the forked
// child discarded), and after the commit with the tree id unsynced (hashed again).
func TestFreezeCrashRecovery(t *testing.T) {
	ctx := context.Background()
	t.Run("before rename", func(t *testing.T) {
		f, _, d, snapshot, layer, _ := frozenRepo(t, false)
		// The upper never moved: it still holds the change, and the layer does not exist.
		// (Close stored the hashed layer, so it cannot simply be renamed back.)
		upper := filepath.Join(f.state, "uppers", "app")
		if err := os.RemoveAll(filepath.Join(f.state, "layers", layer)); err != nil {
			t.Fatal(err)
		}
		frozenChanges(t, upper, f.gitdir("app"))
		if err := writeState(f.state, intentState(d, snapshot, layer)); err != nil {
			t.Fatal(err)
		}
		f.open()
		if ls := f.ls(); len(ls.Snapshots) != 0 || !ls.Checkouts[0].Dirty {
			t.Fatalf("rolled back freeze left %+v", ls)
		}
		if got := f.read("app", "tracked.txt"); got != "changed\n" {
			t.Fatalf("rolled back upper reads %q", got)
		}
		if _, err := os.Lstat(filepath.Join(f.state, "layers", layer)); !os.IsNotExist(err) {
			t.Fatalf("layer survived rollback: %v", err)
		}
	})
	for _, fork := range []bool{false, true} {
		t.Run(map[bool]string{false: "snap after rename", true: "fork after rename"}[fork], func(t *testing.T) {
			f, dir, d, snapshot, layer, tree := frozenRepo(t, fork)
			// The upper was renamed away and not yet recreated or hashed.
			if err := os.RemoveAll(filepath.Join(f.state, "uppers", "app")); err != nil {
				t.Fatal(err)
			}
			frozenChanges(t, filepath.Join(f.state, "layers", layer), f.reg.gitdirPath("app"))
			if err := writeState(f.state, intentState(d, snapshot, layer)); err != nil {
				t.Fatal(err)
			}
			f.open()
			ls := f.ls()
			if len(ls.Checkouts) != 1 || ls.Checkouts[0].Dirty || len(ls.Snapshots) != 1 || ls.Snapshots[0].ID != snapshot {
				t.Fatalf("rolled forward freeze = %+v", ls)
			}
			for _, kind := range []string{"uppers", "git"} {
				if _, err := os.Lstat(filepath.Join(f.state, kind, "child")); !os.IsNotExist(err) {
					t.Fatalf("unpublished child's %s survived: %v", kind, err)
				}
			}
			if got := f.read("app", "tracked.txt"); got != "changed\n" {
				t.Fatalf("checkout reads %q", got)
			}
			if got := f.read("app", ".git"); got != "gitdir: "+f.gitdir("app")+"\n" {
				t.Fatalf("recreated upper's .git = %q", got)
			}
			reply, err := f.reg.waitHashed(ctx, f.reg.snapshots[snapshot])
			if err != nil || reply.Snapshot.Tree != tree {
				t.Fatalf("rehashed %+v, %v; want tree %s", reply, err, tree)
			}
			if _, err := f.reg.New(ctx, api.NewArgs{Snapshot: snapshot, Name: "view"}); err != nil {
				t.Fatal(err)
			}
			if got := gitRun(t, dir, "--git-dir", f.gitdir("view"), "ls-files"); got == "" {
				t.Fatal("view of recovered snapshot has an empty index")
			}
		})
	}
	t.Run("tree id lost", func(t *testing.T) {
		f, _, d, snapshot, _, tree := frozenRepo(t, false)
		for i := range d.Snapshots {
			d.Snapshots[i].Tree = nil
		}
		if err := writeState(f.state, d); err != nil {
			t.Fatal(err)
		}
		f.open()
		reply, err := f.reg.waitHashed(ctx, f.reg.snapshots[snapshot])
		if err != nil || reply.Snapshot.Tree != tree {
			t.Fatalf("rehashed %+v, %v; want tree %s", reply, err, tree)
		}
	})
}

// TestTouchedSurvivesStop checks that a change through a checkout's folder moves
// its Touched past its creation, and that a planned stop keeps it although file
// calls never write state.
func TestTouchedSurvivesStop(t *testing.T) {
	f := newFixture(t)
	f.newCheckout(folder(t, map[string]string{"a": "base"}), "c")
	fresh := f.ls().Checkouts[0]
	if !fresh.Touched.Equal(fresh.Created) {
		t.Fatalf("fresh checkout touched %v, created %v", fresh.Touched, fresh.Created)
	}
	var s Session
	if err := f.reg.Enter("c", &s); err != nil {
		t.Fatal(err)
	}
	s.Touch()
	s.Done()
	touched := f.ls().Checkouts[0].Touched
	if !touched.After(fresh.Created) {
		t.Fatalf("touched %v, not after creation %v", touched, fresh.Created)
	}
	if err := f.reg.Close(); err != nil {
		t.Fatal(err)
	}
	f.open()
	if got := f.ls().Checkouts[0].Touched; !got.Equal(touched) {
		t.Fatalf("reopened touched %v, want %v", got, touched)
	}
}
