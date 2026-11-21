package checkout

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/store"
)

// installHook writes an executable post-checkout hook into hooksDir that appends
// its arguments and working directory to a record file, and returns that file
// (pwd -W, where MSYS sh has it, records a Windows path instead of /c/...).
func installHook(t *testing.T, hooksDir string) string {
	t.Helper()
	record := filepath.Join(t.TempDir(), "record")
	script := "#!/bin/sh\n{ echo \"$@\"; pwd -W 2>/dev/null || pwd; } >> '" + filepath.ToSlash(record) + "'\n"
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooksDir, "post-checkout"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return record
}

// assertHookRuns runs post-checkout in dir through git, as git does after a
// checkout, and checks that the hook recorded its arguments and ran in dir.
func assertHookRuns(t *testing.T, dir, record string) {
	t.Helper()
	if err := os.RemoveAll(record); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "hook", "run", "--ignore-missing", "post-checkout", "--", "old", "new", "1")
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("the source's hook did not run in %s: %v", dir, err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 || lines[0] != "old new 1" {
		t.Fatalf("hook recorded %q, want its arguments and working directory", lines)
	}
	if got, want := realDir(t, filepath.FromSlash(lines[1])), realDir(t, dir); got != want {
		t.Fatalf("hook ran in %s, want %s", got, want)
	}
}

// assertHookCheckout runs the source's hook in the exported checkout and checks
// that the link left it clean.
func (f *fixture) assertHookCheckout(name, sub, record string) {
	f.t.Helper()
	exported := f.export(name)
	assertHookRuns(f.t, filepath.Join(exported, filepath.FromSlash(sub)), record)
	if got := gitRun(f.t, exported, "status", "--porcelain"); got != "" {
		f.t.Fatalf("git status in %s = %q", name, got)
	}
	if f.dirty(name) {
		f.t.Fatalf("checkout %s reports changes", name)
	}
}

// TestHooksRunInEveryCreatePath checks that the hooks of the source repository run
// in checkouts opened by every create path.
func TestHooksRunInEveryCreatePath(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		open func(t *testing.T, f *fixture, dir string) string
	}{
		{"new", func(t *testing.T, f *fixture, dir string) string {
			f.newCheckout(dir, "c")
			return "c"
		}},
		{"base", func(t *testing.T, f *fixture, dir string) string {
			first := f.newCheckout(dir, "first").Checkout
			if _, err := f.reg.New(ctx, api.NewArgs{Base: first.Base, Name: "c"}); err != nil {
				t.Fatal(err)
			}
			return "c"
		}},
		{"warm", func(t *testing.T, f *fixture, dir string) string {
			if f.reg.watcher == nil {
				t.Skip("no folder events on this platform")
			}
			f.newCheckout(dir, "first")
			eventually(t, "the pool did not fill", func() bool { return f.poolFull(dir) })
			f.newCheckout(dir, "c")
			return "c"
		}},
		{"fork", func(t *testing.T, f *fixture, dir string) string {
			f.newCheckout(dir, "parent")
			if _, err := f.reg.Fork(ctx, api.ForkArgs{Checkout: "parent", Name: "c"}); err != nil {
				t.Fatal(err)
			}
			return "c"
		}},
		{"from snapshot", func(t *testing.T, f *fixture, dir string) string {
			f.newCheckout(dir, "parent")
			snap, _ := f.snapGit("parent")
			if _, err := f.reg.New(ctx, api.NewArgs{Snapshot: snap, Name: "c"}); err != nil {
				t.Fatal(err)
			}
			return "c"
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			dir := reuseRepo(t)
			record := installHook(t, filepath.Join(dir, ".git", "hooks"))
			name := c.open(t, f, dir)
			f.assertHookCheckout(name, "", record)
		})
	}
}

// TestHooksSnapshotRelink gives a checkout made from a snapshot the source's hooks
// even when the snapshot's checkout had none, as one taken by an older build,
// keeps a real hooks directory, and stores no hooks link.
func TestHooksSnapshotRelink(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	dir := reuseRepo(t)
	record := installHook(t, filepath.Join(dir, ".git", "hooks"))
	f.newCheckout(dir, "old")
	if err := os.Remove(filepath.Join(f.gitdir("old"), "hooks")); err != nil {
		t.Fatal(err)
	}
	snap, _ := f.snapGit("old")
	if _, err := f.reg.New(ctx, api.NewArgs{Snapshot: snap, Name: "fromold"}); err != nil {
		t.Fatal(err)
	}
	f.assertHookCheckout("fromold", "", record)

	f.newCheckout(dir, "linked")
	snap, tree := f.snapGit("linked")
	files, err := f.reg.store.Flatten(store.OID(tree))
	if err != nil {
		t.Fatal(err)
	}
	for p := range files {
		if p == "hooks" || strings.HasPrefix(p, "hooks/") {
			t.Fatalf("snapshot stores %s of the hooks link", p)
		}
	}
	if _, err := f.reg.New(ctx, api.NewArgs{Snapshot: snap, Name: "fromlinked"}); err != nil {
		t.Fatal(err)
	}
	f.assertHookCheckout("fromlinked", "", record)

	f.newCheckout(dir, "own")
	own := filepath.Join(f.gitdir("own"), "hooks")
	if err := os.Remove(own); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(own, "custom"), "mine")
	snap, _ = f.snapGit("own")
	if _, err := f.reg.New(ctx, api.NewArgs{Snapshot: snap, Name: "fromown"}); err != nil {
		t.Fatal(err)
	}
	copied := filepath.Join(f.gitdir("fromown"), "hooks")
	if info, err := os.Lstat(copied); err != nil || !info.IsDir() {
		t.Fatalf("real hooks directory in the snapshot's copy: %v, %v", info, err)
	}
	if data, err := os.ReadFile(filepath.Join(copied, "custom")); err != nil || string(data) != "mine" {
		t.Fatalf("hooks directory content = %q, %v", data, err)
	}
	if _, err := os.Lstat(filepath.Join(copied, "post-checkout")); !os.IsNotExist(err) {
		t.Fatalf("the source's hook leaked into a real hooks directory: %v", err)
	}
}

// TestHooksReopenRelinks adds the link to checkouts that lack it when the
// registry opens, and leaves existing hooks entries alone.
func TestHooksReopenRelinks(t *testing.T) {
	f := newFixture(t)
	dir := reuseRepo(t)
	record := installHook(t, filepath.Join(dir, ".git", "hooks"))
	f.newCheckout(dir, "old")
	f.newCheckout(dir, "own")
	if err := os.Remove(filepath.Join(f.gitdir("old"), "hooks")); err != nil {
		t.Fatal(err)
	}
	own := filepath.Join(f.gitdir("own"), "hooks")
	if err := os.Remove(own); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(own, "custom"), "mine")
	f.reg.Close()
	f.open()
	f.assertHookCheckout("old", "", record)
	if info, err := os.Lstat(own); err != nil || !info.IsDir() {
		t.Fatalf("a real hooks directory was replaced: %v, %v", info, err)
	}
}

// TestHooksSurviveNoSourceHooks makes checkouts of a repository without a hooks
// directory: no link, and no error, also on reopen.
func TestHooksSurviveNoSourceHooks(t *testing.T) {
	f := newFixture(t)
	dir := reuseRepo(t)
	if err := os.RemoveAll(filepath.Join(dir, ".git", "hooks")); err != nil {
		t.Fatal(err)
	}
	f.newCheckout(dir, "app")
	f.reg.Close()
	f.open()
	if _, err := os.Lstat(filepath.Join(f.gitdir("app"), "hooks")); !os.IsNotExist(err) {
		t.Fatalf("hooks entry without source hooks: %v", err)
	}
}

// TestHooksRmKeepsSource removes a checkout with its link and leaves the source's
// hooks as they were.
func TestHooksRmKeepsSource(t *testing.T) {
	f := newFixture(t)
	dir := reuseRepo(t)
	hooks := filepath.Join(dir, ".git", "hooks")
	script := "#!/bin/sh\nexit 0\n"
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(hooks, "post-checkout"), script)
	f.newCheckout(dir, "app")
	if _, err := os.Lstat(filepath.Join(f.gitdir("app"), "hooks")); err != nil {
		t.Fatalf("checkout has no hooks link: %v", err)
	}
	if err := f.reg.Rm(context.Background(), api.RmArgs{Checkout: "app"}); err != nil {
		t.Fatal(err)
	}
	if entries := checkoutDirs(t, f.state, "git"); len(entries) != 0 {
		t.Fatalf("git directories after rm = %v", entries)
	}
	if data, err := os.ReadFile(filepath.Join(hooks, "post-checkout")); err != nil || string(data) != script {
		t.Fatalf("source hook after rm = %q, %v", data, err)
	}
}

// TestHooksSubmodule runs a submodule repository's own hooks in the submodule of
// a checkout, also in a fork and after the link is lost.
func TestHooksSubmodule(t *testing.T) {
	f := newFixture(t)
	app := submoduleRepo(t)
	record := installHook(t, filepath.Join(app, ".git", "modules", "vendor", "sub", "hooks"))
	f.newCheckout(app, "app")
	check := func(name string) { f.assertHookCheckout(name, "vendor/sub", record) }
	check("app")
	if _, err := f.reg.Fork(context.Background(), api.ForkArgs{Checkout: "app", Name: "child"}); err != nil {
		t.Fatal(err)
	}
	check("child")
	if err := os.Remove(filepath.Join(f.gitdir("app"), "modules", "vendor", "sub", "hooks")); err != nil {
		t.Fatal(err)
	}
	f.reg.Close()
	f.open()
	check("app")
}

// TestHooksRetrofitKeepsTouched keeps a checkout's idle time when reopening adds
// its hooks link, which changes the git directory.
func TestHooksRetrofitKeepsTouched(t *testing.T) {
	f := newFixture(t)
	dir := reuseRepo(t)
	installHook(t, filepath.Join(dir, ".git", "hooks"))
	f.newCheckout(dir, "app")
	gitdir := f.gitdir("app")
	if err := os.Remove(filepath.Join(gitdir, "hooks")); err != nil {
		t.Fatal(err)
	}
	old := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, p := range []string{filepath.Join(gitdir, "logs", "HEAD"), gitdir} {
		if err := os.Chtimes(p, old, old); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	f.reg.Close()
	f.open()
	if _, err := os.Lstat(filepath.Join(gitdir, "hooks")); err != nil {
		t.Fatalf("reopen did not link hooks: %v", err)
	}
	c := f.ls().Checkouts[0]
	if !c.Touched.Equal(c.Created) {
		t.Fatalf("touched %v after the retrofit, want the creation time %v", c.Touched, c.Created)
	}
}

// TestHooksOnlyOfCapturedRepo does not link a checkout to the hooks of a
// repository enclosing its folder, which a reopen finds once the folder's own
// repository is gone.
func TestHooksOnlyOfCapturedRepo(t *testing.T) {
	f := newFixture(t)
	outer := folder(t, nil)
	gitRun(t, outer, "init", "-q", "-b", "main")
	installHook(t, filepath.Join(outer, ".git", "hooks"))
	dir := filepath.Join(outer, "inner")
	write(t, filepath.Join(dir, "a.txt"), "one")
	gitRun(t, dir, "init", "-q", "-b", "main")
	gitRun(t, dir, "add", "--all")
	gitRun(t, dir, "commit", "-qm", "init")
	f.newCheckout(dir, "app")
	f.reg.Close()
	if err := os.Rename(filepath.Join(dir, ".git"), filepath.Join(t.TempDir(), "moved")); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(f.gitdir("app"), "hooks")
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	f.open()
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("checkout linked to the enclosing repository's hooks: %v", err)
	}
}

// TestHooksAddedAfterPoolFilled opens a checkout from a pool filled while the
// source had no hooks directory, after one was added.
func TestHooksAddedAfterPoolFilled(t *testing.T) {
	f := newFixture(t)
	if f.reg.watcher == nil {
		t.Skip("no folder events on this platform")
	}
	dir := reuseRepo(t)
	hooks := filepath.Join(dir, ".git", "hooks")
	if err := os.RemoveAll(hooks); err != nil {
		t.Fatal(err)
	}
	f.newCheckout(dir, "first")
	eventually(t, "the pool did not fill", func() bool { return f.poolFull(dir) })
	record := installHook(t, hooks)
	f.newCheckout(dir, "c")
	f.assertHookCheckout("c", "", record)
}

// TestHooksSourceHooksNotADirectory makes checkouts of a repository whose hooks
// entry is a file: no link, and no failure.
func TestHooksSourceHooksNotADirectory(t *testing.T) {
	f := newFixture(t)
	dir := reuseRepo(t)
	hooks := filepath.Join(dir, ".git", "hooks")
	if err := os.RemoveAll(hooks); err != nil {
		t.Fatal(err)
	}
	write(t, hooks, "not a directory")
	f.newCheckout(dir, "app")
	if _, err := os.Lstat(filepath.Join(f.gitdir("app"), "hooks")); !os.IsNotExist(err) {
		t.Fatalf("hooks entry from a source hooks file: %v", err)
	}
}

// TestHooksOwnLinkSurvivesSnapshot keeps a hooks link the user pointed at their
// own directory through a snapshot and a checkout from it.
func TestHooksOwnLinkSurvivesSnapshot(t *testing.T) {
	f := newFixture(t)
	dir := reuseRepo(t)
	installHook(t, filepath.Join(dir, ".git", "hooks"))
	f.newCheckout(dir, "app")
	mine := filepath.Join(t.TempDir(), "myhooks")
	record := installHook(t, mine)
	link := filepath.Join(f.gitdir("app"), "hooks")
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(mine, link); err != nil {
		t.Skipf("no symlinks: %v", err)
	}
	snap, _ := f.snapGit("app")
	if _, err := f.reg.New(context.Background(), api.NewArgs{Snapshot: snap, Name: "child"}); err != nil {
		t.Fatal(err)
	}
	if got, err := os.Readlink(filepath.Join(f.gitdir("child"), "hooks")); err != nil || got != mine {
		t.Fatalf("child hooks link = %q, %v, want %s", got, err, mine)
	}
	f.assertHookCheckout("child", "", record)
}
