package git

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReplay(t *testing.T) {
	ctx := context.Background()
	r := testRepo(t)
	gitdir, work := branchCheckout(t, r)
	for _, name := range []string{"first", "second"} {
		testWrite(t, filepath.Join(work, name), name+"\n")
		testGit(t, work, "add", name)
		testGit(t, work, "commit", "-qm", name, "--author=Original Author <author@localhost>")
	}
	tip := testGit(t, work, "rev-parse", "HEAD")
	testWrite(t, filepath.Join(r.Top, "first"), "first\n")
	testWrite(t, filepath.Join(r.Top, "upstream"), "upstream\n")
	testGit(t, r.Top, "add", "first", "upstream")
	testGit(t, r.Top, "commit", "-qm", "upstream already has first")
	onto := testGit(t, r.Top, "rev-parse", "HEAD")

	// Intentionally invalid injected config must not reach the replay.
	t.Setenv("GIT_CONFIG_COUNT", "invalid")
	scratch := t.TempDir()
	private, err := r.Replay(ctx, gitdir, tip, onto, scratch)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_COUNT", "0")
	// The first patch is already upstream; the replay keeps the second's author.
	want := "second:Original Author <author@localhost>"
	if log := testGit(t, scratch, "log", "--reverse", "--format=%s:%an <%ae>", onto+"..HEAD"); log != want {
		t.Fatalf("replayed commits = %q, want %q", log, want)
	}
	if base := testGit(t, scratch, "rev-parse", "HEAD^"); base != onto {
		t.Fatalf("replay base = %s, want %s", base, onto)
	}
	if files := testGit(t, scratch, "ls-tree", "--name-only", "HEAD"); files != "first\nsecond\ntracked\nupstream" {
		t.Fatalf("replayed tree = %q", files)
	}
	if testGit(t, work, "rev-parse", "HEAD") != tip || testGit(t, r.Top, "rev-parse", "HEAD") != onto {
		t.Fatal("replay changed a source ref")
	}
	if err := r.FetchCommits(ctx, private); err != nil {
		t.Fatal(err)
	}
	replayed := testGit(t, scratch, "rev-parse", "HEAD")
	if testGit(t, r.Top, "rev-parse", "HEAD") != onto || testGit(t, r.Top, "cat-file", "-t", replayed) != "commit" {
		t.Fatal("fetch did not copy the replayed commit without moving HEAD")
	}
}

func TestReplayConflictLeavesSourcesUntouched(t *testing.T) {
	r := testRepo(t)
	gitdir, work := branchCheckout(t, r)
	for i, root := range []string{work, r.Top} {
		testWrite(t, filepath.Join(root, "tracked"), []string{"checkout\n", "upstream\n"}[i])
		testGit(t, root, "commit", "-qam", "edit tracked")
		testWrite(t, filepath.Join(root, "staged"), "staged\n")
		testGit(t, root, "add", "staged")
		testWrite(t, filepath.Join(root, "staged"), "dirty\n")
	}
	tip, onto := testGit(t, work, "rev-parse", "HEAD"), testGit(t, r.Top, "rev-parse", "HEAD")
	readIndex := func(dir string) []byte {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(dir, "index"))
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	checkoutIndex, sourceIndex := readIndex(gitdir), readIndex(r.GitDir)
	private, err := r.Replay(context.Background(), gitdir, tip, onto, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "could not apply") || private != "" {
		t.Fatalf("replay = %q, %v; want a conflict", private, err)
	}
	if testGit(t, work, "rev-parse", "HEAD") != tip || testGit(t, r.Top, "rev-parse", "HEAD") != onto {
		t.Fatal("conflict changed a source ref")
	}
	if !bytes.Equal(checkoutIndex, readIndex(gitdir)) || !bytes.Equal(sourceIndex, readIndex(r.GitDir)) {
		t.Fatal("conflict changed a source index")
	}
	for _, root := range []string{work, r.Top} {
		if status := testGit(t, root, "status", "--porcelain"); status != "AM staged" {
			t.Fatalf("%s: conflict changed source worktree: %q", root, status)
		}
	}
	for _, dir := range []string{gitdir, r.GitDir} {
		for _, state := range []string{"rebase-apply", "rebase-merge"} {
			if _, err := os.Stat(filepath.Join(dir, state)); !os.IsNotExist(err) {
				t.Fatalf("%s: unexpected rebase state: %v", dir, err)
			}
		}
	}
}

func TestReplayRequiresGit(t *testing.T) {
	t.Setenv("PATH", "")
	r := &Repo{}
	if _, err := r.Replay(context.Background(), "", "", "", t.TempDir()); err == nil || !strings.Contains(err.Error(), "requires the git executable") {
		t.Fatalf("replay error = %v, want missing git executable", err)
	}
}

func TestReplayRefusesMergeCommits(t *testing.T) {
	r := testRepo(t)
	gitdir, work := branchCheckout(t, r)
	onto := testGit(t, r.Top, "rev-parse", "HEAD")
	testGit(t, work, "switch", "-c", "side")
	testGit(t, work, "commit", "--allow-empty", "-qm", "side")
	testGit(t, work, "switch", "main")
	testGit(t, work, "merge", "--no-ff", "-m", "merge side", "side")
	tip := testGit(t, work, "rev-parse", "HEAD")
	if _, err := r.Replay(context.Background(), gitdir, tip, onto, t.TempDir()); err == nil || !strings.Contains(err.Error(), "merge commits") {
		t.Fatalf("replay error = %v, want refusal to flatten merges", err)
	}
}
