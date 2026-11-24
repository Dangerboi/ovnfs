package git

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/stencil-hq/ovnfs/git/internal/proc"
)

// gitProgram returns the git executable on PATH, if any. Fetches prefer it to
// package fetch so that a pack from an untrusted server is unpacked in git's
// process rather than in ovnfs.
func gitProgram() (string, bool) {
	program, err := exec.LookPath("git")
	return program, err == nil
}

// dir is where git runs for r: its work tree, or the git directory of a bare
// repository.
func (r *Repo) dir() string {
	if r.Top == "" {
		return r.GitDir
	}
	return r.Top
}

// gitEnv is the environment of a git process ovnfs starts: this process's,
// without the variables that would point git at another repository, index or
// work tree, and with untranslated messages.
func gitEnv() []string {
	var env []string
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		switch key {
		case "LC_ALL", "GIT_INDEX_FILE", "GIT_WORK_TREE", "GIT_DIR", "GIT_COMMON_DIR", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES":
			continue
		}
		env = append(env, value)
	}
	return append(env, "LC_ALL=C")
}

// gitRun tunes runGit: variables ("KEY=VALUE") added to the environment, and
// the process's standard input and output, unconnected when nil. ignoreGitEnv
// excludes all inherited GIT_* variables before applying env.
type gitRun struct {
	env          []string
	in           io.Reader
	out          io.Writer
	ignoreGitEnv bool
}

// runGit runs the git executable program in r with args, leading `-c` pairs
// included. It never inherits another repository, index or work tree from the
// environment and runs no hooks. A failure carries git's stderr.
func (r *Repo) runGit(ctx context.Context, program string, o gitRun, args ...string) error {
	cmd := exec.CommandContext(ctx, program, append([]string{"-C", r.dir(), "-c", "core.hooksPath=/dev/null"}, args...)...)
	env := gitEnv()
	if o.ignoreGitEnv {
		keep := env[:0]
		for _, value := range env {
			if !strings.HasPrefix(value, "GIT_") {
				keep = append(keep, value)
			}
		}
		env = keep
	}
	cmd.Env = append(env, o.env...)
	cmd.Stdin, cmd.Stdout = o.in, o.out
	proc.Prepare(cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		for len(args) >= 2 && args[0] == "-c" {
			args = args[2:]
		}
		return fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), msg, err)
	}
	return nil
}
