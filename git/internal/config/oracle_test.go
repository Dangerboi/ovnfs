package config

import (
	"bytes"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// oracle runs the real git binary in an isolated environment so its view of
// configuration can be compared with ours.
type oracle struct {
	t    *testing.T
	root string // temp dir with symlinks resolved, so git and we spell paths alike
	env  map[string]string
}

func newOracle(t *testing.T) *oracle {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	o := &oracle{t: t, root: root, env: map[string]string{
		"HOME":                filepath.Join(root, "home"),
		"XDG_CONFIG_HOME":     filepath.Join(root, "xdg"),
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_CONFIG_GLOBAL":   os.DevNull,
		"LC_ALL":              "C",
	}}
	for _, k := range []string{"PATH", "SYSTEMROOT", "TMPDIR", "TEMP", "TMP"} {
		if v, ok := os.LookupEnv(k); ok {
			o.env[k] = v
		}
	}
	o.mkdir("home")
	o.mkdir("xdg")
	return o
}

func (o *oracle) path(rel string) string { return filepath.Join(o.root, filepath.FromSlash(rel)) }

// cfgPath is o.path spelled for a config file value or condition, where
// Windows backslashes would be read as escapes.
func (o *oracle) cfgPath(rel string) string { return filepath.ToSlash(o.path(rel)) }

func (o *oracle) mkdir(rel string) string {
	o.t.Helper()
	p := o.path(rel)
	if err := os.MkdirAll(p, 0o755); err != nil {
		o.t.Fatal(err)
	}
	return p
}

func (o *oracle) write(rel, content string) string {
	o.t.Helper()
	p := o.path(rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		o.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		o.t.Fatal(err)
	}
	return p
}

// with returns a copy of the oracle's environment with extra set; an empty
// value with a "-" key prefix unsets.
func (o *oracle) with(extra map[string]string) map[string]string {
	env := make(map[string]string, len(o.env)+len(extra))
	maps.Copy(env, o.env)
	for k, v := range extra {
		if name, ok := strings.CutPrefix(k, "-"); ok {
			delete(env, name)
			continue
		}
		env[k] = v
	}
	return env
}

func lookup(env map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := env[k]
		return v, ok
	}
}

type result struct {
	out    string
	code   int
	stderr string
}

// git runs git in dir with env (nil: the oracle's base environment).
func (o *oracle) git(dir string, env map[string]string, args ...string) result {
	o.t.Helper()
	return o.gitStdin(dir, env, "", args...)
}

// gitStdin is git with standard input.
func (o *oracle) gitStdin(dir string, env map[string]string, stdin string, args ...string) result {
	o.t.Helper()
	if env == nil {
		env = o.env
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(stdin)
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		o.t.Fatalf("git %v: %v", args, err)
	}
	return result{out: stdout.String(), code: cmd.ProcessState.ExitCode(), stderr: stderr.String()}
}

// mustGit runs git and fails the test unless it succeeds.
func (o *oracle) mustGit(dir string, args ...string) string {
	o.t.Helper()
	r := o.git(dir, nil, args...)
	if r.code != 0 {
		o.t.Fatalf("git %v: exit %d: %s", args, r.code, r.stderr)
	}
	return r.out
}

// listed is one entry as both sides report it.
type listed struct {
	Scope, File, Key, Value string
	HasValue                bool
}

// gitList parses `git config --list -z --show-origin --show-scope`.
func (o *oracle) gitList(dir string, env map[string]string, args ...string) ([]listed, result) {
	o.t.Helper()
	r := o.git(dir, env, append([]string{"config", "--list", "-z", "--show-origin", "--show-scope"}, args...)...)
	if r.code != 0 {
		return nil, r
	}
	var out []listed
	fields := strings.Split(r.out, "\x00")
	for i := 0; i+2 < len(fields); i += 3 {
		l := listed{Scope: fields[i]}
		if file, ok := strings.CutPrefix(fields[i+1], "file:"); ok {
			if !filepath.IsAbs(file) {
				file = filepath.Join(dir, file)
			}
			l.File = filepath.Clean(file)
		}
		l.Key, l.Value, l.HasValue = strings.Cut(fields[i+2], "\n")
		out = append(out, l)
	}
	return out, r
}

func ourList(c *Config) []listed {
	var out []listed
	for e := range c.All() {
		l := listed{Scope: e.Scope.String(), Key: e.Key, Value: e.Value, HasValue: e.HasValue}
		if e.File != "" {
			abs, err := filepath.Abs(e.File)
			if err != nil {
				abs = e.File
			}
			l.File = abs
		}
		out = append(out, l)
	}
	return out
}

func diffLists(t *testing.T, want, got []listed) {
	t.Helper()
	if slices.Equal(want, got) {
		return
	}
	var b strings.Builder
	for i := range max(len(want), len(got)) {
		var w, g listed
		if i < len(want) {
			w = want[i]
		}
		if i < len(got) {
			g = got[i]
		}
		mark := "  "
		if w != g {
			mark = "! "
		}
		b.WriteString(mark)
		b.WriteString(strings.ReplaceAll(strings.Join([]string{"git:", w.Scope, w.File, w.Key, q(w.Value, w.HasValue), " ours:", g.Scope, g.File, g.Key, q(g.Value, g.HasValue)}, " "), "\n", `\n`))
		b.WriteByte('\n')
	}
	t.Errorf("entries differ:\n%s", b.String())
}

func q(v string, has bool) string {
	if !has {
		return "<bare>"
	}
	return "=" + strconv.Quote(v)
}

// repo is a repository created by git init, with symlink-free paths.
type repo struct {
	top, gitDir string
}

func (o *oracle) initRepo(rel string) repo {
	o.t.Helper()
	top := o.mkdir(rel)
	o.mustGit(top, "init", "-q", "-b", "main")
	return repo{top: top, gitDir: filepath.Join(top, ".git")}
}

// branchOf reads HEAD the way discovery will hand it to Options.Branch.
func branchOf(gitDir string) func() (string, error) {
	return func() (string, error) {
		data, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
		if err != nil {
			return "", err
		}
		ref, _ := strings.CutPrefix(strings.TrimSpace(string(data)), "ref: ")
		if !strings.HasPrefix(ref, "refs/") {
			return "", nil
		}
		return ref, nil
	}
}

// compareRepo loads r's configuration both ways with env and compares.
func (o *oracle) compareRepo(t *testing.T, r repo, env map[string]string) {
	t.Helper()
	o.compareIn(t, r.top, r, env)
}

// compareIn is compareRepo with git running in dir, which must be the test's
// working directory when env names files relatively.
func (o *oracle) compareIn(t *testing.T, dir string, r repo, env map[string]string) {
	t.Helper()
	if env == nil {
		env = o.env
	}
	gitEnv := map[string]string{"GIT_DIR": r.gitDir}
	maps.Copy(gitEnv, env)
	want, res := o.gitList(dir, gitEnv)
	cfg, err := Load(Options{GitDir: r.gitDir, CommonDir: r.gitDir, WorkTree: r.top, LookupEnv: lookup(env), Branch: branchOf(r.gitDir)})
	if res.code != 0 {
		if err == nil {
			t.Fatalf("git failed (%s) but Load succeeded", strings.TrimSpace(res.stderr))
		}
		return
	}
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	diffLists(t, want, ourList(cfg))
}
