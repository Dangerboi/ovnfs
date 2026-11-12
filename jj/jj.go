// Package jj makes checkouts of a jj (Jujutsu) repository workspaces of that
// repository, through the jj executable on PATH. A workspace shares the
// repository and its operation log and has a working copy of its own, kept in
// the .jj directory at the workspace's root.
//
// jj only ever runs on host paths: the source folder, the repository and
// directories of the daemon's state, never a path inside the checkout mount,
// whose requests the calling daemon serves.
package jj

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// Dir is the name of a workspace's directory at its root.
const Dir = ".jj"

// ErrNotInstalled reports that no jj executable is on PATH.
var ErrNotInstalled = errors.New("jj is not installed")

// Repo returns the repository directory of the workspace rooted at root, ""
// when root holds no workspace: .jj/repo itself in the workspace that created
// the repository, else the directory the file .jj/repo names, relative to .jj
// unless absolute.
func Repo(root string) (string, error) {
	dir := filepath.Join(root, Dir)
	link := filepath.Join(dir, "repo")
	info, err := os.Stat(link)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return link, nil
	}
	data, err := os.ReadFile(link)
	if err != nil {
		return "", err
	}
	target := filepath.FromSlash(string(data))
	if !filepath.IsAbs(target) {
		target = filepath.Join(dir, target)
	}
	return filepath.Clean(target), nil
}

// Add creates a workspace of the repository of the workspace at root, named
// name or, when the repository already has a workspace of that name, the first
// free of name-2, name-3 and so on, and returns the name used. jj writes it to
// the empty directory dir, its working-copy commit on the parents of the
// working-copy commit of root's workspace, as `jj workspace add` does, and
// without a Git worktree: a checkout has a git directory of its own. The .jj
// left in dir names the repository by absolute path, so it works wherever it
// moves, and git ignores it, as in a workspace colocated with git. A failed
// Add forgets the workspace it may have created.
func Add(ctx context.Context, root, name, dir string) (string, error) {
	program, err := exec.LookPath("jj")
	if err != nil {
		return "", ErrNotInstalled
	}
	listed, err := run(ctx, program, root, "--repository", root, "--ignore-working-copy", "workspace", "list", "--template", `name ++ "\n"`)
	if err != nil {
		return "", err
	}
	names := strings.Split(listed, "\n")
	chosen := name
	for n := 2; slices.Contains(names, chosen); n++ {
		chosen = name + "-" + strconv.Itoa(n)
	}
	_, err = run(ctx, program, root, "--repository", root, "workspace", "add", "--no-colocate", "--name", chosen, dir)
	link := filepath.Join(dir, Dir, "repo")
	var target []byte
	if err == nil {
		target, err = os.ReadFile(link)
	}
	if relative := filepath.FromSlash(string(target)); err == nil && !filepath.IsAbs(relative) {
		// jj spells the link from the physical path of .jj.
		var physical string
		if physical, err = filepath.EvalSymlinks(filepath.Dir(link)); err == nil {
			err = os.WriteFile(link, []byte(filepath.Join(physical, relative)), 0o644)
		}
	}
	if err == nil {
		// Without it git lists the workspace as untracked, and `git add -A` takes it.
		err = os.WriteFile(filepath.Join(dir, Dir, ".gitignore"), []byte("/*\n"), 0o644)
	}
	if err != nil {
		return "", errors.Join(err, Forget(context.WithoutCancel(ctx), dir))
	}
	return chosen, nil
}

// Forget forgets the workspace rooted at root in its repository, as `jj
// workspace forget` does, without touching any file of it. A root without a
// workspace and a workspace whose repository is gone have nothing to forget.
func Forget(ctx context.Context, root string) error {
	repo, err := Repo(root)
	if err != nil || repo == "" {
		return err
	}
	if _, err := os.Stat(repo); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	program, err := exec.LookPath("jj")
	if err != nil {
		return ErrNotInstalled
	}
	_, err = run(ctx, program, root, "--repository", root, "--ignore-working-copy", "workspace", "forget")
	return err
}

// run runs jj in host directory dir with args and returns its standard output.
// A failure carries jj's standard error.
func run(ctx context.Context, program, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, program, append([]string{"--color", "never", "--no-pager"}, args...)...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("jj %s: %s: %w", strings.Join(args, " "), msg, err)
	}
	return stdout.String(), nil
}
