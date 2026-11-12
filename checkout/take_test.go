package checkout

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stencil-hq/ovnfs/api"
)

func TestTakeIndexIsBaseNotPrivateIndex(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	dir := revisionRepo(t)
	write(t, filepath.Join(dir, "tracked.txt"), "staged modification\n")
	write(t, filepath.Join(dir, "added.txt"), "staged addition\n")
	gitRun(t, dir, "rm", "other.txt")
	gitRun(t, dir, "add", "tracked.txt", "added.txt")
	write(t, filepath.Join(dir, "tracked.txt"), "unstaged edit\n")
	write(t, filepath.Join(dir, "unstaged.txt"), "untracked edit\n")
	if _, err := f.reg.New(ctx, api.NewArgs{Dir: dir, Name: "index", Rev: "HEAD", Take: []string{":"}}); err != nil {
		t.Fatal(err)
	}
	assertContent := func(name string) {
		t.Helper()
		for p, want := range map[string]string{
			"tracked.txt": "staged modification\n", "added.txt": "staged addition\n",
			"other.txt": "<absent>", "unstaged.txt": "<absent>",
		} {
			if got := f.read(name, p); got != want {
				t.Errorf("%s/%s = %q, want %q", name, p, got, want)
			}
		}
	}
	assertContent("index")
	if diff, err := f.reg.Diff(ctx, api.DiffArgs{Checkout: "index"}); err != nil || len(diff.Changes) != 0 {
		t.Fatalf("initial diff = %+v, %v", diff, err)
	}
	work := t.TempDir()
	for _, p := range []string{".gitignore", "tracked.txt", "added.txt"} {
		write(t, filepath.Join(work, p), f.read("index", p))
	}
	status := gitRun(t, work, "--git-dir", f.gitdir("index"), "--work-tree", work, "status", "--porcelain")
	// --take changes only the work tree. The HEAD index deliberately reports
	// the staged source content as unstaged changes in this private checkout.
	if want := "D other.txt\n M tracked.txt\n?? added.txt"; status != want {
		t.Fatalf("private status = %q, want %q", status, want)
	}
	fork, err := f.reg.Fork(ctx, api.ForkArgs{Checkout: "index", Name: "child"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.reg.New(ctx, api.NewArgs{Name: "snapshot", Snapshot: fork.Snapshot.ID}); err != nil {
		t.Fatal(err)
	}
	if err := f.reg.Close(); err != nil {
		t.Fatal(err)
	}
	f.open()
	for _, name := range []string{"index", "child", "snapshot"} {
		assertContent(name)
		if diff, err := f.reg.Diff(ctx, api.DiffArgs{Checkout: name}); err != nil || len(diff.Changes) != 0 {
			t.Fatalf("%s diff after reopen = %+v, %v", name, diff, err)
		}
	}
}

func TestTakeWorkingSubtree(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	dir := revisionRepo(t)
	write(t, filepath.Join(dir, "dir/changed.txt"), "committed\n")
	write(t, filepath.Join(dir, "dir/deleted.txt"), "delete me\n")
	gitRun(t, dir, "add", "dir")
	gitRun(t, dir, "commit", "-qm", "subtree")
	write(t, filepath.Join(dir, "dir/changed.txt"), "working edit\n")
	write(t, filepath.Join(dir, "dir/new.txt"), "untracked\n")
	write(t, filepath.Join(dir, "tracked.txt"), "outside dirty\n")
	write(t, filepath.Join(dir, ".gitignore"), "node_modules/\n")
	write(t, filepath.Join(dir, "node_modules/kept/artifact"), "ignored cache\n")
	if err := os.Remove(filepath.Join(dir, "dir/deleted.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reg.New(ctx, api.NewArgs{Dir: dir, Name: "working", Rev: "HEAD", Take: []string{".:dir"}}); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]string{
		"dir/changed.txt": "working edit\n", "dir/new.txt": "untracked\n", "dir/deleted.txt": "<absent>",
		"tracked.txt": "original\n", "node_modules/kept/artifact": "ignored cache\n",
	} {
		if got := f.read("working", p); got != want {
			t.Errorf("%s = %q, want %q", p, got, want)
		}
	}
	if diff, err := f.reg.Diff(ctx, api.DiffArgs{Checkout: "working"}); err != nil || len(diff.Changes) != 0 {
		t.Fatalf("initial diff = %+v, %v", diff, err)
	}
}

func TestTakeRevisionDoesNotApplyTakenContent(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	dir := revisionRepo(t)
	write(t, filepath.Join(dir, "tracked.txt"), "historical selection\n")
	gitRun(t, dir, "add", "tracked.txt")
	gitRun(t, dir, "commit", "-qm", "selection")
	rev := gitRun(t, dir, "rev-parse", "HEAD")
	gitRun(t, dir, "reset", "--hard", "HEAD~1")
	write(t, filepath.Join(dir, "tracked.txt"), "folder dirty\n")
	write(t, filepath.Join(dir, "other.txt"), "outside dirty\n")
	write(t, filepath.Join(dir, "build/artifact"), "ignored artifact\n")
	if _, err := f.reg.New(ctx, api.NewArgs{Dir: dir, Name: "selected", Take: []string{rev + ":tracked.txt"}}); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]string{"tracked.txt": "historical selection\n", "other.txt": "outside dirty\n", "build/artifact": "ignored artifact\n"} {
		if got := f.read("selected", p); got != want {
			t.Fatalf("%s = %q, want %q", p, got, want)
		}
	}
	if diff, err := f.reg.Diff(ctx, api.DiffArgs{Checkout: "selected"}); err != nil || len(diff.Changes) != 0 {
		t.Fatalf("initial diff = %+v, %v", diff, err)
	}
	if got := gitRun(t, dir, "--git-dir", f.gitdir("selected"), "show", ":tracked.txt"); got != "original" {
		t.Fatalf("take changed copied private index: %q", got)
	}
	f.put("selected", "other.txt", "agent edit\n")
	for _, dry := range []bool{true, false} {
		reply, err := f.reg.Apply(ctx, api.ApplyArgs{Checkout: "selected", DryRun: dry})
		if err != nil || len(reply.Applied) != 1 || reply.Applied[0].Path != "other.txt" || len(reply.Conflicts) != 0 {
			t.Fatalf("apply dry=%t = %+v, %v", dry, reply, err)
		}
		if got, err := os.ReadFile(filepath.Join(dir, "tracked.txt")); err != nil || string(got) != "folder dirty\n" {
			t.Fatalf("taken path applied = %q, %v", got, err)
		}
	}
	if got, err := os.ReadFile(filepath.Join(dir, "other.txt")); err != nil || string(got) != "agent edit\n" {
		t.Fatalf("other path not applied = %q, %v", got, err)
	}
}

func TestTakeOrderingModesAndProtectedPaths(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	dir := revisionRepo(t)
	write(t, filepath.Join(dir, "dir/a"), "committed\n")
	write(t, filepath.Join(dir, ".gitignore"), "node_modules/\nbuild/\n")
	gitRun(t, dir, "add", "dir", ".gitignore")
	gitRun(t, dir, "commit", "-qm", "directory")
	write(t, filepath.Join(dir, "dir/a"), "working\n")
	write(t, filepath.Join(dir, "dir/b"), "staged\n")
	if err := os.Symlink("a", filepath.Join(dir, "dir/link")); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "dir/b", "dir/link")
	// Through the index: chmod does not reach git on Windows (core.fileMode false).
	gitRun(t, dir, "update-index", "--chmod=+x", "dir/b")
	write(t, filepath.Join(dir, "dir/b"), "unstaged\n")
	write(t, filepath.Join(dir, "node_modules/pkg/data"), "cache\n")
	write(t, filepath.Join(dir, "secret/file"), "local\n")
	write(t, filepath.Join(dir, "hidden/file"), "hidden\n")
	args := api.NewArgs{Dir: dir, Name: "ordered", Take: []string{".:dir", ":dir/b", "HEAD:dir/a", "HEAD:", ".:dir", ":dir/b"}, Rules: []string{"local /secret/", "hidden /hidden/"}}
	if _, err := f.reg.New(ctx, args); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]string{"dir/a": "working\n", "dir/b": "staged\n", "node_modules/pkg/data": "cache\n", "secret/file": "local\n", "hidden/file": "<absent>"} {
		if got := f.read("ordered", p); got != want {
			t.Errorf("%s = %q, want %q", p, got, want)
		}
	}
	var s Session
	if err := f.reg.Enter("ordered", &s); err != nil {
		t.Fatal(err)
	}
	defer s.Done()
	if info, err := s.FS.Lstat("dir/b"); err != nil || info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("executable mode = %v, %v", info, err)
	}
	if target, err := s.FS.Readlink("dir/link"); err != nil || target != "a" {
		t.Fatalf("symlink = %q, %v", target, err)
	}
}

func TestTakeErrorsLeaveNoCheckout(t *testing.T) {
	f := newFixture(t)
	repo := revisionRepo(t)
	plain := folder(t, map[string]string{"file": "plain\n"})
	write(t, filepath.Join(repo, "build/exists"), "ignored\n")
	cases := []api.NewArgs{
		{Snapshot: "missing", Take: []string{".:"}},
		{Dir: plain, Take: []string{":"}},
		{Dir: plain, Take: []string{"HEAD:"}},
		{Dir: repo, Take: []string{"missing-revision:"}},
		{Dir: repo, Take: []string{"HEAD:missing/path"}},
		{Dir: repo, Take: []string{".:missing/path"}},
		{Dir: repo, Take: []string{".:build/missing"}},
	}
	for _, spec := range []string{"", "HEAD", ".:/absolute", ".:..", ".:../file", ".:dir/../file", ".:./file", ".:dir//file", ".:dir/", ".:.", ".:dir\\file", ".:bad\x00path"} {
		cases = append(cases, api.NewArgs{Dir: repo, Take: []string{spec}})
	}
	for _, args := range cases {
		args.Name = "bad"
		_, err := f.reg.New(context.Background(), args)
		var public *Error
		if !errors.As(err, &public) || public.Code != "invalid" {
			t.Fatalf("new %+v = %v, want invalid", args, err)
		}
		if len(f.reg.checkouts) != 0 || len(f.reg.bases) != 0 || len(f.reg.reserved) != 0 || len(f.reg.intents) != 0 {
			t.Fatal("failed take left registry state")
		}
		for _, sub := range []string{"git", "uppers", "bases"} {
			entries, err := os.ReadDir(filepath.Join(f.state, sub))
			if err != nil || len(entries) != 0 {
				t.Fatalf("failed creation left %s: %v, %v", sub, entries, err)
			}
		}
	}
	if runtime.GOOS == "windows" {
		return // Windows file names cannot hold ':' and treat '\\' as a separator.
	}
	// Working-tree takes do not require git, and the first colon is the separator.
	write(t, filepath.Join(plain, "with:colon"), "colon name\n")
	write(t, filepath.Join(plain, "with\\backslash"), "literal backslash\n")
	if _, err := f.reg.New(context.Background(), api.NewArgs{Dir: plain, Name: "plain", Take: []string{".:with:colon", ".:with\\backslash"}}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.snapFiles("plain"), " "); got != "file with:colon with\\backslash" {
		t.Fatalf("plain snapshot = %q", got)
	}
}
