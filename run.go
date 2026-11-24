package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/alecthomas/kong"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/source"
)

// runOptions separates ovnfs flags from the command's untouched arguments.
type runOptions struct {
	new       api.NewArgs
	keep      bool
	on        string
	remoteBin string
	command   []string
}

// runFlags are shared by the visible command grammar and the prefix parser.
type runFlags struct {
	Name      string   `name:"name" help:"checkout name"`
	Rev       string   `name:"rev" help:"start at this git commit or local branch"`
	Take      []string `name:"take" sep:"none" help:"SRC:PATH override of the starting tree (repeatable)"`
	Rules     []string `name:"rule" sep:"none" help:"extra rule line (repeatable)"`
	Keep      bool     `name:"keep" help:"keep the checkout after the command exits"`
	On        string   `name:"on" help:"run on this ssh destination, keeping both checkouts"`
	RemoteBin string   `name:"remote-bin" help:"path of ovnfs on the remote machine"`
}

type runCmd struct {
	runFlags
	Args    []string `arg:"" optional:"" passthrough:"" name:"DIR -- CMD ..." help:"optional folder, then -- and the command with its arguments"`
	options runOptions
}

// AfterApply parses only the prefix before --. The passthrough positional keeps
// kong from interpreting child arguments, including flags and further separators.
func (r *runCmd) AfterApply(ctx *kong.Context) error {
	args := ctx.Args[1:]
	separator := slices.Index(args, "--")
	if separator < 0 || separator == len(args)-1 {
		return errors.New("run requires -- followed by a command")
	}
	var prefix struct {
		runFlags
		Dir string `arg:"" optional:"" name:"DIR" help:"folder to open the checkout on (default .)"`
	}
	parser, err := kong.New(&prefix, kong.Name("ovnfs run"))
	if err != nil {
		return err
	}
	if _, err = parser.Parse(args[:separator]); err != nil {
		return err
	}
	dir := prefix.Dir
	if dir == "" {
		dir = "."
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return err
	}
	r.options = runOptions{
		new:  api.NewArgs{Dir: dir, Name: prefix.Name, Rev: prefix.Rev, Take: prefix.Take, Rules: prefix.Rules},
		keep: prefix.Keep, on: prefix.On, remoteBin: prefix.RemoteBin,
		command: args[separator+1:],
	}
	return nil
}

// Run opens an isolated checkout, runs a command there, and closes or retains the lane.
func (run *runCmd) Run(c *cli) error {
	o := run.options
	// A run's stdout belongs exclusively to its command, even under --json.
	c.json = false
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	if o.on != "" {
		return runRemote(c, o, cwd, signals)
	}
	r, err := call[api.NewReply](c, "new", o.new)
	if err != nil {
		return err
	}
	defer func() {
		if o.keep {
			fmt.Fprintf(stderr, "%s %s %s\n", paint(stderr, styleGreen, "kept"), paint(stderr, styleAccent, r.Checkout.Name), paint(stderr, styleDim, r.Checkout.Path))
			return
		}
		// SIGINT/SIGTERM cancel the CLI context, but must not cancel removal.
		c.ctx = context.WithoutCancel(c.ctx)
		if _, err := call[struct{}](c, "rm", api.RmArgs{Checkout: r.Checkout.Name}); err != nil {
			fmt.Fprintf(stderr, "%s %v\n", paint(stderr, styleYellow, "run: removing "+r.Checkout.Name+":"), err)
		}
	}()
	cmd := exec.Command(o.command[0], o.command[1:]...)
	cmd.Dir = runWorkingDir(o.new, r.Checkout, cwd)
	cmd.Env = append(os.Environ(), "OVNFS_CHECKOUT="+r.Checkout.Name)
	return runChild(cmd, signals)
}

// runRemote retains the local and remote checkouts so changes can later be pulled back.
func runRemote(c *cli, o runOptions, cwd string, signals <-chan os.Signal) error {
	if o.new.Rev != "" {
		st, err := call[api.Status](c, "status", struct{}{})
		if err != nil {
			return err
		}
		if runCheckoutRoot(st.Mount, o.new.Dir) != "" {
			return errors.New("run --rev from a checkout cannot be combined with --on; omit --on, or run from the original source folder using a revision available there")
		}
	}
	root, err := source.Resolve(o.new.Dir)
	if err != nil {
		return err
	}
	a := api.AttachArgs{Dir: o.new.Dir, To: o.on, Name: o.new.Name, RemoteBin: o.remoteBin}
	if a.Name == "" {
		a.Name = runLaneName(root)
	}
	if o.new.Rev != "" || len(o.new.Take) > 0 || len(o.new.Rules) > 0 {
		r, err := call[api.NewReply](c, "new", o.new)
		if err != nil {
			return err
		}
		a.Dir, a.Checkout, root = "", r.Checkout.Name, r.Checkout.Source
	}
	r, err := call[api.AttachReply](c, "attach", a)
	if err != nil {
		return err
	}
	fmt.Fprintf(stderr, "%s %s %s %s\n", paint(stderr, styleDim, "lane"), paint(stderr, styleAccent, r.Checkout), paint(stderr, styleDim, "→"), paint(stderr, styleAccent, o.on+":"+r.RemotePath))
	remoteDir := path.Join(r.RemotePath, filepath.ToSlash(runRelativeDir(root, cwd)))
	cmd := sshCommand(o.on, runRemoteCommand(remoteDir, o.command))
	return runChild(cmd, signals)
}

// runWorkingDir uses the input checkout's root for a nested revision, even
// though the reply names the original host source folder.
func runWorkingDir(a api.NewArgs, opened api.Checkout, cwd string) string {
	root := opened.Source
	if a.Rev != "" {
		if parent := runCheckoutRoot(filepath.Dir(opened.Path), a.Dir); parent != "" {
			root = parent
		}
	}
	return filepath.Join(opened.Path, runRelativeDir(root, cwd))
}

func runCheckoutRoot(mount, dir string) string {
	if rel, ok := runPathFromAncestor(mount, dir); ok && rel != "." {
		name, _, _ := strings.Cut(rel, string(filepath.Separator))
		return filepath.Join(mount, name)
	}
	return ""
}

// runRelativeDir preserves the caller's subdirectory only when it is inside the source.
func runRelativeDir(root, cwd string) string {
	if rel, ok := runPathFromAncestor(root, cwd); ok {
		return rel
	}
	return "."
}

// runPathFromAncestor also recognizes alternate mounted path spellings on a
// case-insensitive volume or through a bind mount.
func runPathFromAncestor(root, dir string) (string, bool) {
	if !filepath.IsAbs(root) || !filepath.IsAbs(dir) {
		return "", false
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	if rel, err := filepath.Rel(root, dir); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return rel, true
	}
	want, err := os.Stat(root)
	if err != nil {
		return "", false
	}
	for here := dir; ; here = filepath.Dir(here) {
		if info, err := os.Stat(here); err == nil && os.SameFile(info, want) {
			rel, err := filepath.Rel(here, dir)
			return rel, err == nil
		}
		if here == filepath.Dir(here) {
			return "", false
		}
	}
}

// runLaneName keeps a folder's remote name stable across fresh captures.
func runLaneName(root string) string {
	var b strings.Builder
	for _, ch := range strings.ToLower(filepath.Base(root)) {
		switch {
		case ch >= 'a' && ch <= 'z', ch >= '0' && ch <= '9', ch == '.', ch == '_', ch == '-':
			b.WriteRune(ch)
		default:
			b.WriteByte('-')
		}
	}
	name := strings.TrimLeft(b.String(), "._-")
	if len(name) > 56 {
		name = name[:56]
	}
	if name == "" {
		return "checkout"
	}
	return name
}

// runRemoteCommand protects every argument from the remote login shell.
func runRemoteCommand(dir string, args []string) string {
	var b strings.Builder
	b.WriteString("cd ")
	b.WriteString(shellQuote(dir))
	b.WriteString(" && exec")
	for _, arg := range args {
		b.WriteByte(' ')
		b.WriteString(shellQuote(arg))
	}
	return b.String()
}

// runChild forwards termination signals while preserving the command's exit status.
func runChild(cmd *exec.Cmd, signals <-chan os.Signal) error {
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(stderr, colorError(fmt.Errorf("run: %w", err)))
		return exitCode(127)
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case sig := <-signals:
				_ = cmd.Process.Signal(sig)
			case <-done:
				return
			}
		}
	}()
	if err := cmd.Wait(); err != nil {
		if exited, ok := errors.AsType[*exec.ExitError](err); ok {
			if status, ok := exited.Sys().(syscall.WaitStatus); ok && status.Signaled() {
				return exitCode(128 + int(status.Signal()))
			}
			return exitCode(exited.ExitCode())
		}
		return err
	}
	return nil
}
