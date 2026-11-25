package config

import (
	maps0 "maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func appendFile(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
}

func TestScopesMatchGit(t *testing.T) {
	o := newOracle(t)
	r := o.initRepo("repo")
	system := o.write("etc/gitconfig", "[s]\n\tv = system\n[shared]\n\tv = system\n")
	o.write("xdg/git/config", "[g]\n\tv = xdg\n[shared]\n\tv = xdg\n")
	o.write("home/.gitconfig", "[g]\n\tv = home\n[shared]\n\tv = home\n")
	o.write("home/.config/git/config", "[g]\n\tv = dotconfig\n")
	explicit := o.write("global.cfg", "[g]\n\tv = explicit\n")
	appendFile(t, filepath.Join(r.gitDir, "config"), "[extensions]\n\tworktreeConfig = true\n[shared]\n\tv = local\n")
	o.write("repo/.git/config.worktree", "[w]\n\tv = worktree\n[shared]\n\tv = worktree\n")

	command := map[string]string{
		"GIT_CONFIG_COUNT":      "2",
		"GIT_CONFIG_KEY_0":      "Shared.V",
		"GIT_CONFIG_VALUE_0":    "count",
		"GIT_CONFIG_KEY_1":      "c.Sub Section.Key",
		"GIT_CONFIG_VALUE_1":    "",
		"GIT_CONFIG_PARAMETERS": `'a.B'='x y' 'c.d.E'='it'\''s'\!'' 'f.g'= 'h.I J.k'='' 'legacy.key=v'`,
	}
	for name, env := range map[string]map[string]string{
		"defaults":           nil,
		"system":             {"-GIT_CONFIG_NOSYSTEM": "", "GIT_CONFIG_SYSTEM": system},
		"nosystem wins":      {"GIT_CONFIG_NOSYSTEM": "true", "GIT_CONFIG_SYSTEM": system},
		"nosystem false":     {"GIT_CONFIG_NOSYSTEM": "no", "GIT_CONFIG_SYSTEM": system},
		"xdg and home":       {"-GIT_CONFIG_GLOBAL": ""},
		"no xdg":             {"-GIT_CONFIG_GLOBAL": "", "-XDG_CONFIG_HOME": ""},
		"empty xdg":          {"-GIT_CONFIG_GLOBAL": "", "XDG_CONFIG_HOME": ""},
		"no home":            {"-GIT_CONFIG_GLOBAL": "", "-HOME": ""},
		"explicit global":    {"GIT_CONFIG_GLOBAL": explicit},
		"empty global":       {"GIT_CONFIG_GLOBAL": ""},
		"command":            command,
		"bad count":          {"GIT_CONFIG_COUNT": "1x"},
		"negative count":     {"GIT_CONFIG_COUNT": "-1"},
		"missing value":      {"GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "a.b"},
		"invalid key":        {"GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "a_b.c", "GIT_CONFIG_VALUE_0": "v"},
		"no section":         {"GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "nodot", "GIT_CONFIG_VALUE_0": "v"},
		"bad nosystem":       {"GIT_CONFIG_NOSYSTEM": "maybe"},
		"padded count":       {"GIT_CONFIG_COUNT": " 1", "GIT_CONFIG_KEY_0": "x.y", "GIT_CONFIG_VALUE_0": "v"},
		"empty count":        {"GIT_CONFIG_COUNT": ""},
		"bad parameters":     {"GIT_CONFIG_PARAMETERS": "'a.b"},
		"everything at once": merge(command, map[string]string{"-GIT_CONFIG_NOSYSTEM": "", "GIT_CONFIG_SYSTEM": system, "-GIT_CONFIG_GLOBAL": ""}),
	} {
		t.Run(name, func(t *testing.T) { o.compareRepo(t, r, o.with(env)) })
	}

	t.Run("worktree config off", func(t *testing.T) {
		o.mustGit(r.top, "config", "extensions.worktreeConfig", "false")
		defer o.mustGit(r.top, "config", "extensions.worktreeConfig", "true")
		o.compareRepo(t, r, nil)
	})
}

func merge(maps ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range maps {
		maps0.Copy(out, m)
	}
	return out
}

func TestIncludesMatchGit(t *testing.T) {
	o := newOracle(t)
	home := o.env["HOME"]
	r := o.initRepo("home/work/Repo")
	o.mustGit(r.top, "symbolic-ref", "HEAD", "refs/heads/feature/x")
	local := filepath.Join(r.gitDir, "config")
	base, err := os.ReadFile(local)
	if err != nil {
		t.Fatal(err)
	}
	mark := func(name string) string { return "[t]\n\t" + name + " = 1\n" }
	for _, n := range []string{"abs", "home", "nested", "gd", "gdexact", "gdnomatch", "gdi", "gdcase", "dot", "tilde", "glob", "bracket", "ob", "obglob", "obdir", "obfull", "wt", "wti", "wtslash", "unknown", "hc", "hcno", "hclater", "hccount", "empty"} {
		o.write("inc/"+n+".inc", mark(n))
	}
	o.write("inc/sub/rel.inc", mark("rel")+"[include]\n\tpath = ../nested.inc\n")
	o.write("home/home.inc", mark("home"))
	o.write("home/work/Repo/dot.cfg", "[includeIf \"gitdir:./\"]\n\tpath = "+o.cfgPath("inc/dot.inc")+"\n")
	inc := func(n string) string { return o.cfgPath("inc/" + n + ".inc") }
	gitDir, top := filepath.ToSlash(r.gitDir), filepath.ToSlash(r.top)

	scenarios := map[string]string{
		"plain includes":     "[include]\n\tpath = " + o.cfgPath("inc/sub/rel.inc") + "\n\tpath = " + inc("abs") + "\n\tpath = ~/home.inc\n\tpath = missing.inc\n",
		"empty include path": "[include]\n\tpath =\n",
		"relative include":   "[include]\n\tpath = ../../../../inc/sub/rel.inc\n",
		"gitdir": "[includeIf \"gitdir:Repo/\"]\n\tpath = " + inc("gd") + "\n" +
			"[includeIf \"gitdir:" + gitDir + "\"]\n\tpath = " + inc("gdexact") + "\n" +
			"[includeIf \"gitdir:" + top + "\"]\n\tpath = " + inc("gdnomatch") + "\n" +
			"[includeIf \"gitdir/i:REPO/\"]\n\tpath = " + inc("gdi") + "\n" +
			"[includeIf \"gitdir:REPO/\"]\n\tpath = " + inc("gdcase") + "\n" +
			"[includeIf \"gitdir:~/work/\"]\n\tpath = " + inc("tilde") + "\n" +
			"[includeIf \"gitdir:**/w*k/*/.git\"]\n\tpath = " + inc("glob") + "\n" +
			"[includeIf \"gitdir:" + filepath.ToSlash(home) + "/[v-x]ork/R[!x]po/.git\"]\n\tpath = " + inc("bracket") + "\n" +
			"[includeIf \"GitDir:Repo/\"]\n\tpath = " + inc("unknown") + "\n" +
			"[include]\n\tpath = ../dot.cfg\n",
		"onbranch": "[includeIf \"onbranch:feature/x\"]\n\tpath = " + inc("ob") + "\n" +
			"[includeIf \"onbranch:f*/*\"]\n\tpath = " + inc("obglob") + "\n" +
			"[includeIf \"onbranch:feature/\"]\n\tpath = " + inc("obdir") + "\n" +
			"[includeIf \"onbranch:refs/heads/feature/x\"]\n\tpath = " + inc("obfull") + "\n",
		"worktree": "[includeIf \"worktree:" + top + "\"]\n\tpath = " + inc("wt") + "\n" +
			"[includeIf \"worktree/i:" + strings.ToUpper(top) + "\"]\n\tpath = " + inc("wti") + "\n" +
			"[includeIf \"worktree:" + top + "/\"]\n\tpath = " + inc("wtslash") + "\n",
		"hasconfig": "[includeIf \"hasconfig:remote.*.url:https://example.com/**\"]\n\tpath = " + inc("hc") + "\n" +
			"[includeIf \"hasconfig:remote.*.url:https://example.com/*\"]\n\tpath = " + inc("hcno") + "\n" +
			"[includeIf \"hasconfig:remote.*.url:git@later:*\"]\n\tpath = " + inc("hclater") + "\n" +
			"[includeIf \"hasconfig:remote.*.url:count://*\"]\n\tpath = " + inc("hccount") + "\n" +
			"[includeIf \"hasconfig:remote.*.pushurl:*\"]\n\tpath = " + inc("unknown") + "\n" +
			"[remote \"origin\"]\n\turl = https://example.com/a/b\n" +
			"[include]\n\tpath = " + filepath.ToSlash(o.write("inc/remote.inc", "[remote \"later\"]\n\turl = git@later:x\n")) + "\n",
		"hasconfig url in included file": "[includeIf \"hasconfig:remote.*.url:https://nomatch/**\"]\n\tpath = " + filepath.ToSlash(o.write("inc/hcremote.inc", "[remote \"x\"]\n\turl = https://nomatch/x\n")) + "\n",
		"hasconfig url behind other includeIf": "[includeIf \"hasconfig:remote.*.url:https://q/**\"]\n\tpath = " + inc("hc") + "\n" +
			"[includeIf \"onbranch:feature/x\"]\n\tpath = " + o.cfgPath("inc/hcremote.inc") + "\n",
		"hasconfig url behind false includeIf": "[includeIf \"hasconfig:remote.*.url:https://q/**\"]\n\tpath = " + inc("hc") + "\n" +
			"[includeIf \"onbranch:main\"]\n\tpath = " + o.cfgPath("inc/hcremote.inc") + "\n",
		"cycle":                          "[include]\n\tpath = " + filepath.ToSlash(o.write("inc/self.inc", "[include]\n\tpath = self.inc\n")) + "\n",
		"missing value":                  "[include]\n\tpath\n",
		"missing value, false condition": "[includeIf \"onbranch:main\"]\n\tpath\n",
		"missing value, true condition":  "[includeIf \"onbranch:feature/x\"]\n\tpath\n",
		"bad include":                    "[include]\n\tpath = " + filepath.ToSlash(o.write("inc/bad.inc", "[t]\n\tok = 1\n[broken\n")) + "\n",
		"unknown user":                   "[include]\n\tpath = ~nosuchuser-ovnfs/x\n",
	}
	countEnv := o.with(map[string]string{"GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "remote.c.url", "GIT_CONFIG_VALUE_0": "count://x"})
	// included lists the markers each successful scenario must pull in, so
	// agreement with git is not vacuous.
	included := map[string][]string{
		"plain includes":   {"rel", "nested", "abs", "home"},
		"relative include": {"rel", "nested"},
		"gitdir":           {"gd", "gdexact", "gdi", "tilde", "glob", "bracket", "dot"},
		"onbranch":         {"ob", "obglob", "obdir"},
		"worktree":         {"wt", "wti"},
		"hasconfig":        {"hc", "hclater", "hccount"},
	}
	for name, extra := range scenarios {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(local, append(slices.Clip(base), extra...), 0o644); err != nil {
				t.Fatal(err)
			}
			o.compareRepo(t, r, countEnv)
			want, ok := included[name]
			if !ok {
				return
			}
			cfg, err := Load(Options{GitDir: r.gitDir, CommonDir: r.gitDir, WorkTree: r.top, LookupEnv: lookup(countEnv), Branch: branchOf(r.gitDir)})
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for e := range cfg.All() {
				if e.Section() == "t" {
					got = append(got, e.Name())
				}
			}
			slices.Sort(got)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("included %q, want %q", got, want)
			}
		})
	}

	t.Run("include depth", func(t *testing.T) {
		for i := range 12 {
			o.write("chain/f"+strconv.Itoa(i), "[c]\n\tv"+strconv.Itoa(i)+" = 1\n[include]\n\tpath = f"+strconv.Itoa(i+1)+"\n")
		}
		for _, last := range []string{"f11", "f12"} {
			os.Remove(o.path("chain/" + last))
			env := o.with(map[string]string{"GIT_CONFIG_GLOBAL": o.path("chain/f0")})
			if err := os.WriteFile(local, base, 0o644); err != nil {
				t.Fatal(err)
			}
			o.compareRepo(t, r, env)
		}
	})

	t.Run("command line includes", func(t *testing.T) {
		if err := os.WriteFile(local, base, 0o644); err != nil {
			t.Fatal(err)
		}
		for _, kv := range [][2]string{
			{"include.path", inc("abs")},
			{"include.path", "relative.inc"},
			{"includeIf.onbranch:feature/**.path", inc("ob")},
			{"includeIf.gitdir:Repo/.path", "~/home.inc"},
		} {
			o.compareRepo(t, r, o.with(map[string]string{"GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": kv[0], "GIT_CONFIG_VALUE_0": kv[1]}))
		}
	})
}

func TestNoRepository(t *testing.T) {
	o := newOracle(t)
	o.write("gd.inc", "[t]\n\tgd = 1\n")
	o.write("xdg/git/config", "[includeIf \"gitdir:**\"]\n\tpath = "+o.cfgPath("gd.inc")+"\n[includeIf \"onbranch:*\"]\n\tpath = "+o.cfgPath("gd.inc")+"\n[u]\n\tv = 1\n")
	env := o.with(map[string]string{"-GIT_CONFIG_GLOBAL": ""})
	want, res := o.gitList(o.mkdir("norepo"), env)
	if res.code != 0 {
		t.Fatal(res.stderr)
	}
	cfg, err := Load(Options{LookupEnv: lookup(env)})
	if err != nil {
		t.Fatal(err)
	}
	diffLists(t, want, ourList(cfg))
}

func TestLoadProtectedSkipsRepository(t *testing.T) {
	o := newOracle(t)
	r := o.initRepo("repo")
	appendFile(t, filepath.Join(r.gitDir, "config"), "[safe]\n\tdirectory = *\n")
	o.write("xdg/git/config", "[safe]\n\tdirectory = /trusted\n")
	env := o.with(map[string]string{"-GIT_CONFIG_GLOBAL": "", "GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "safe.directory", "GIT_CONFIG_VALUE_0": "/cmd"})
	cfg, err := LoadProtected(Options{GitDir: r.gitDir, CommonDir: r.gitDir, LookupEnv: lookup(env)})
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.GetAll("safe.directory")
	if want := []string{"/trusted", "/cmd"}; !slices.Equal(got, want) {
		t.Fatalf("protected safe.directory = %q, want %q", got, want)
	}
	for e := range cfg.All() {
		if !e.Scope.Protected() {
			t.Fatalf("unprotected entry %+v", e)
		}
	}
}

func TestFilesListsConsultedPaths(t *testing.T) {
	o := newOracle(t)
	r := o.initRepo("repo")
	appendFile(t, filepath.Join(r.gitDir, "config"), "[include]\n\tpath = missing.inc\n")
	env := o.with(map[string]string{"-GIT_CONFIG_GLOBAL": ""})
	cfg, err := Load(Options{GitDir: r.gitDir, CommonDir: r.gitDir, LookupEnv: lookup(env)})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		filepath.Join(env["XDG_CONFIG_HOME"], "git", "config"),
		filepath.Join(env["HOME"], ".gitconfig"),
		filepath.Join(r.gitDir, "config"),
		filepath.Join(r.gitDir, "missing.inc"),
	}
	if got := cfg.Files(); !slices.Equal(got, want) {
		t.Fatalf("Files() = %q, want %q", got, want)
	}
}

func TestHostnameCondition(t *testing.T) {
	host, err := os.Hostname()
	if err != nil {
		t.Skip(err)
	}
	dir := t.TempDir()
	inc := filepath.Join(dir, "inc")
	if err := os.WriteFile(inc, []byte("[t]\n\tv = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for pattern, want := range map[string]bool{host: true, host[:1] + "*": true, "no-such-host-ovnfs": false} {
		env := map[string]string{"GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": "", "GIT_CONFIG_COUNT": "1",
			"GIT_CONFIG_KEY_0": "includeIf.hostname:" + pattern + ".path", "GIT_CONFIG_VALUE_0": inc}
		cfg, err := Load(Options{LookupEnv: lookup(env)})
		if err != nil {
			t.Fatal(err)
		}
		if _, got := cfg.Get("t.v"); got != want {
			t.Errorf("hostname:%s included = %v, want %v", pattern, got, want)
		}
	}
}

func TestConcurrentQueries(t *testing.T) {
	cfg, err := Parse([]byte("[http \"https://h\"]\n\tk = 1\n[core]\n\tbare = false\n"), "c")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 100 {
				cfg.Get("core.bare")
				cfg.Bool("core.bare")
				cfg.URLMatch("http", "k", "https://h/x")
				for range cfg.All() {
				}
			}
		})
	}
	wg.Wait()
}

// TestIncludePathsResolveLikeGit covers relative config file names and
// symlinks: include paths are joined to the includer's directory textually
// (so "link/.." is resolved by the OS: physically on Unix, textually on
// Windows), while a "./" condition pattern stands for the real directory of
// the config file.
func TestIncludePathsResolveLikeGit(t *testing.T) {
	o := newOracle(t)
	work := o.initRepo("work/repo")
	other := o.initRepo("a/repo")
	o.write("inc/marker.inc", "[t]\n\tdot = 1\n")
	dotCfg := "[includeIf \"gitdir:./repo/\"]\n\tpath = " + o.cfgPath("inc/marker.inc") + "\n"
	o.write("work/global.cfg", dotCfg)
	o.write("a/dot.cfg", dotCfg)
	o.write("a/shared.cfg", "[s]\n\tv = physical\n")
	o.write("shared.cfg", "[s]\n\tv = cleaned\n")
	o.write("viadotdot.cfg", "[include]\n\tpath = link/../shared.cfg\n")
	o.mkdir("a/sub")
	if err := os.Symlink(o.path("a/sub"), o.path("link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(o.path("a/dot.cfg"), o.path("work/filelink.cfg")); err != nil {
		t.Fatal(err)
	}
	t.Chdir(o.path("work"))
	viaDotDot, linkDot := "physical", "1"
	if runtime.GOOS == "windows" {
		viaDotDot, linkDot = "cleaned", "" // link/../dot.cfg is the missing <root>/dot.cfg
	}

	for _, c := range []struct {
		name, global string
		r            repo
		key, want    string
	}{
		{"relative config file", "global.cfg", work, "t.dot", "1"},
		{"relative config file via ..", "../work/global.cfg", work, "t.dot", "1"},
		{"include through symlink and ..", o.path("viadotdot.cfg"), work, "s.v", viaDotDot},
		{"config reached via symlink and ..", o.path("link") + "/../dot.cfg", other, "t.dot", linkDot},
		{"config file symlink", "filelink.cfg", other, "t.dot", "1"},
		{"config file symlink, link's directory", "filelink.cfg", work, "t.dot", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := o.with(map[string]string{"GIT_CONFIG_GLOBAL": c.global})
			o.compareIn(t, o.path("work"), c.r, env)
			cfg, err := Load(Options{GitDir: c.r.gitDir, CommonDir: c.r.gitDir, LookupEnv: lookup(env)})
			if err != nil {
				t.Fatal(err)
			}
			if got, _ := cfg.Get(c.key); got != c.want {
				t.Errorf("%s = %q, want %q", c.key, got, c.want)
			}
		})
	}
}
