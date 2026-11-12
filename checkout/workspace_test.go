package checkout

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stencil-hq/ovnfs/api"
)

// jjRepo returns a git repository with jj colocated in it, a.txt and src/b.txt
// committed, or skips the test without git or jj.
func jjRepo(t *testing.T) string {
	t.Helper()
	for _, program := range []string{"git", "jj"} {
		if _, err := exec.LookPath(program); err != nil {
			t.Skip(program + " is not installed")
		}
	}
	isolateGit(t)
	config := filepath.Join(t.TempDir(), "jj.toml")
	write(t, config, "user.name = \"t\"\nuser.email = \"t@t\"\n")
	t.Setenv("JJ_CONFIG", config)
	dir := folder(t, map[string]string{"a.txt": "a\n", "src/b.txt": "b\n"})
	gitRun(t, dir, "init", "-q", "-b", "main")
	gitRun(t, dir, "add", "--all")
	gitRun(t, dir, "commit", "-qm", "init")
	jjRun(t, dir, "git", "init", "--colocate")
	return dir
}

// jjRun runs jj in dir and returns its standard output.
func jjRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("jj", append([]string{"--color", "never"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		var stderr []byte
		if exit, ok := err.(*exec.ExitError); ok {
			stderr = exit.Stderr
		}
		t.Fatalf("jj %v: %v\n%s", args, err, stderr)
	}
	return strings.TrimSpace(string(out))
}

// workspaces lists the names of the workspaces of the repository at dir.
func workspaces(t *testing.T, dir string) []string {
	t.Helper()
	names := strings.Fields(jjRun(t, dir, "--ignore-working-copy", "workspace", "list", "-T", `name ++ "\n"`))
	slices.Sort(names)
	return names
}

// workingCopy returns the change id of the working-copy commit of the jj
// workspace exported at dir, after jj snapshotted it.
func workingCopy(t *testing.T, dir string) string {
	t.Helper()
	return jjRun(t, dir, "log", "--no-graph", "-r", "@", "-T", "change_id")
}

// TestJJWorkspaceCheckout checks that a checkout of a colocated jj repository is
// a workspace of it with a working copy of its own, whose .jj stays out of
// dirtiness, snapshots, diff and apply, and that rm forgets it.
func TestJJWorkspaceCheckout(t *testing.T) {
	src := jjRepo(t)
	f := newFixture(t)
	reply := f.newCheckout(src, "app")
	if reply.Checkout.JJ != "app" || len(reply.Warnings) != 0 {
		t.Fatalf("checkout = %+v, warnings %v; want jj workspace app", reply.Checkout, reply.Warnings)
	}
	if got := workspaces(t, src); !slices.Equal(got, []string{"app", "default"}) {
		t.Fatalf("workspaces = %v", got)
	}
	// The checkout's .jj is its own, linked to the repository by absolute path,
	// never the folder's, whose .jj/repo is the repository itself.
	repo, err := filepath.EvalSymlinks(filepath.Join(src, ".jj", "repo"))
	if err != nil {
		t.Fatal(err)
	}
	if got := f.read("app", ".jj/repo"); got != repo {
		t.Fatalf(".jj/repo in checkout = %q, want %q", got, repo)
	}
	if f.dirty("app") {
		t.Fatal("a fresh jj workspace makes the checkout dirty")
	}
	plan, err := f.reg.Plan(context.Background(), api.PlanArgs{Dir: src})
	if err != nil {
		t.Fatal(err)
	}
	listed := append(append(append([]string{}, plan.Here.Paths...), plan.Local.Paths...), plan.Hidden.Paths...)
	for _, c := range plan.Cache {
		listed = append(listed, c.Root)
	}
	for _, p := range listed {
		if strings.HasPrefix(p, ".jj") {
			t.Fatalf("plan lists the folder's %s", p)
		}
	}

	exported := f.export("app")
	if got := jjRun(t, exported, "status"); !strings.Contains(got, "The working copy has no changes") {
		t.Fatalf("jj status in a fresh checkout:\n%s", got)
	}
	if workingCopy(t, exported) == workingCopy(t, src) {
		t.Fatal("checkout and folder share a working-copy commit")
	}
	if got := gitRun(t, exported, "status", "--porcelain", "--untracked-files=all"); got != "" {
		t.Fatalf("git status in a fresh checkout:\n%s", got)
	}
	f.put("app", ".jj/working_copy/extra", "state")
	if f.dirty("app") {
		t.Fatal("a change in the checkout's .jj makes it dirty")
	}
	f.put("app", "a.txt", "changed\n")
	exported = f.export("app")
	if got := jjRun(t, exported, "diff", "--name-only"); got != "a.txt" {
		t.Fatalf("jj diff in checkout = %q, want a.txt", got)
	}

	diff, err := f.reg.Diff(context.Background(), api.DiffArgs{Checkout: "app"})
	if err != nil {
		t.Fatal(err)
	}
	if len(diff.Changes) != 1 || diff.Changes[0].Path != "a.txt" {
		t.Fatalf("diff = %+v, want a.txt alone", diff.Changes)
	}
	if got := f.snapFiles("app"); strings.Join(got, " ") != "a.txt src/b.txt" {
		t.Fatalf("snapshot files = %v", got)
	}
	// The freeze took a.txt into a layer and left the workspace with the checkout.
	if f.read("app", ".jj/working_copy/extra") != "state" || f.read("app", ".jj/repo") != repo {
		t.Fatal("snapshot moved the checkout's .jj away")
	}
	f.reg.mu.RLock()
	h := f.reg.checkouts["app"]
	for _, l := range h.fs.Stack() {
		if _, ok := l.Get(".jj"); ok {
			t.Errorf("layer %s holds .jj", l.Dir())
		}
	}
	f.reg.mu.RUnlock()

	if _, err := f.reg.Apply(context.Background(), api.ApplyArgs{Checkout: "app"}); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(src, "a.txt")); err != nil || string(data) != "changed\n" {
		t.Fatalf("applied a.txt = %q, %v", data, err)
	}
	if info, err := os.Stat(filepath.Join(src, ".jj", "repo")); err != nil || !info.IsDir() {
		t.Fatalf("apply wrote the folder's .jj/repo: %v", err)
	}
	if _, err := os.Stat(filepath.Join(src, ".jj", "working_copy", "extra")); !os.IsNotExist(err) {
		t.Fatalf("apply wrote into the folder's .jj: %v", err)
	}

	if err := f.reg.Rm(context.Background(), api.RmArgs{Checkout: "app"}); err != nil {
		t.Fatal(err)
	}
	if got := workspaces(t, src); !slices.Equal(got, []string{"default"}) {
		t.Fatalf("workspaces after rm = %v", got)
	}
}

// TestJJForkGetsOwnWorkspace checks that a fork, a checkout from a snapshot and a
// --rev checkout each get a workspace of their own, never the parent's .jj.
func TestJJForkGetsOwnWorkspace(t *testing.T) {
	src := jjRepo(t)
	f := newFixture(t)
	f.newCheckout(src, "app")
	f.put("app", "a.txt", "parent\n")
	fork, err := f.reg.Fork(context.Background(), api.ForkArgs{Checkout: "app", Name: "child"})
	if err != nil {
		t.Fatal(err)
	}
	if fork.Checkout.JJ != "child" || len(fork.Warnings) != 0 {
		t.Fatalf("fork = %+v, warnings %v", fork.Checkout, fork.Warnings)
	}
	restored, err := f.reg.New(context.Background(), api.NewArgs{Snapshot: fork.Snapshot.ID, Name: "restored"})
	if err != nil {
		t.Fatal(err)
	}
	revision, err := f.reg.New(context.Background(), api.NewArgs{Dir: src, Name: "revision", Rev: "HEAD"})
	if err != nil {
		t.Fatal(err)
	}
	if restored.Checkout.JJ != "restored" || revision.Checkout.JJ != "revision" {
		t.Fatalf("restored %+v, revision %+v", restored.Checkout, revision.Checkout)
	}
	if got := workspaces(t, src); !slices.Equal(got, []string{"app", "child", "default", "restored", "revision"}) {
		t.Fatalf("workspaces = %v", got)
	}
	changes := map[string]bool{}
	for _, name := range []string{"app", "child", "restored", "revision"} {
		exported := f.export(name)
		if name == "child" || name == "restored" {
			if got := jjRun(t, exported, "diff", "--name-only"); got != "a.txt" {
				t.Fatalf("jj diff in %s = %q, want the parent's a.txt", name, got)
			}
		}
		changes[workingCopy(t, exported)] = true
	}
	if len(changes) != 4 {
		t.Fatalf("checkouts share working-copy commits: %v", changes)
	}
}

// TestJJWorkspaceNameTaken checks that a checkout whose name the repository
// already gives a workspace gets the first free numbered variant.
func TestJJWorkspaceNameTaken(t *testing.T) {
	src := jjRepo(t)
	jjRun(t, src, "workspace", "add", "--name", "app", filepath.Join(t.TempDir(), "app"))
	jjRun(t, src, "workspace", "add", "--name", "app-2", filepath.Join(t.TempDir(), "app-2"))
	f := newFixture(t)
	if got := f.newCheckout(src, "app").Checkout.JJ; got != "app-3" {
		t.Fatalf("workspace = %q, want app-3", got)
	}
	if err := f.reg.Rm(context.Background(), api.RmArgs{Checkout: "app"}); err != nil {
		t.Fatal(err)
	}
	if got := workspaces(t, src); !slices.Equal(got, []string{"app", "app-2", "default"}) {
		t.Fatalf("workspaces after rm = %v", got)
	}
}

// TestJJRmWithoutRepository checks that rm goes on when the repository of a
// checkout's workspace is gone.
func TestJJRmWithoutRepository(t *testing.T) {
	src := jjRepo(t)
	f := newFixture(t)
	f.newCheckout(src, "app")
	if err := os.RemoveAll(src); err != nil {
		t.Fatal(err)
	}
	if err := f.reg.Rm(context.Background(), api.RmArgs{Checkout: "app"}); err != nil {
		t.Fatal(err)
	}
}

// TestJJCrashRecovery reopens state left by crashes: a snapshot whose upper was
// renamed into its layer before the .jj moved back keeps the workspace with the
// checkout, and an unpublished create's workspace is forgotten.
func TestJJCrashRecovery(t *testing.T) {
	src := jjRepo(t)
	f := newFixture(t)
	f.newCheckout(src, "app")
	f.newCheckout(src, "lost")
	f.put("app", "a.txt", "changed\n")
	reply, err := f.reg.Snap(context.Background(), api.SnapArgs{Checkout: "app", Wait: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.reg.Close(); err != nil {
		t.Fatal(err)
	}
	d := readDisk(t, f.state)
	layer := ""
	for _, s := range d.Snapshots {
		if s.ID == reply.Snapshot.ID {
			layer = s.Layer
		}
	}
	// The upper moved to the layer with its .jj, and nothing was recreated.
	upper, layerDir := filepath.Join(f.state, "uppers", "app"), filepath.Join(f.state, "layers", layer)
	workspace := filepath.Join(t.TempDir(), ".jj")
	if err := os.Rename(filepath.Join(upper, ".jj"), workspace); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{upper, layerDir} {
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(layerDir, "a.txt"), "changed\n")
	if err := writeGitfile(layerDir, f.reg.gitdirPath("app")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(workspace, filepath.Join(layerDir, ".jj")); err != nil {
		t.Fatal(err)
	}
	d = intentState(d, reply.Snapshot.ID, layer)
	// lost was being created: the state does not name it yet.
	var kept []diskCheckout
	for _, h := range d.Checkouts {
		if h.Name != "lost" {
			kept = append(kept, h)
		}
	}
	d.Checkouts = kept
	d.Intents = append(d.Intents, diskIntent{Op: "creating", Checkout: "lost"})
	if err := writeState(f.state, d); err != nil {
		t.Fatal(err)
	}
	f.open()
	if got := workspaces(t, src); !slices.Equal(got, []string{"app", "default"}) {
		t.Fatalf("workspaces after recovery = %v", got)
	}
	if _, err := os.Lstat(filepath.Join(layerDir, ".jj")); !os.IsNotExist(err) {
		t.Fatalf("recovered layer keeps .jj: %v", err)
	}
	if f.dirty("app") {
		t.Fatal("recovered checkout is dirty")
	}
	if got := jjRun(t, f.export("app"), "diff", "--name-only"); got != "a.txt" {
		t.Fatalf("jj diff in recovered checkout = %q, want a.txt", got)
	}
}

// TestJJNotInstalled checks that without jj a checkout of a jj repository is
// created all the same, without a workspace and without the folder's .jj, and
// the reply says why.
func TestJJNotInstalled(t *testing.T) {
	src := jjRepo(t)
	t.Setenv("PATH", t.TempDir())
	f := newFixture(t)
	reply := f.newCheckout(src, "app")
	if reply.Checkout.JJ != "" || len(reply.Warnings) != 1 || !strings.Contains(reply.Warnings[0], "jj is not installed") {
		t.Fatalf("checkout = %+v, warnings %v", reply.Checkout, reply.Warnings)
	}
	if got := f.read("app", ".jj/repo"); got != "<absent>" {
		t.Fatalf("the folder's .jj shows through: .jj/repo = %q", got)
	}
	if got := f.read("app", "a.txt"); got != "a\n" {
		t.Fatalf("a.txt = %q", got)
	}
}
