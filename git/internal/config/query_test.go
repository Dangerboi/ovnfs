package config

import (
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// typedValues are spelled as written in a config file after "k<n>".
var typedValues = []string{
	"", " =", " = true", " = TRUE", " = Yes", " = on", " = off", " = No", " = 1", " = 0",
	" = 2", " = -1", ` = " true"`, " = 1k", " = 1K", " = 1m", " = 1G", " = 1t", " = 0x10",
	" = 010", " = 08", " = +5", ` = " 5"`, ` = "5 "`, " = 2147483647", " = 2147483648",
	" = 9223372036854775807", " = 9223372036854775808", " = 8388608k", " = -8589934592g",
	" = 8589934592g", " = k", " = 1kk", " = 1.5", " = -0", " = 0x", " = ~", " = ~/", " = ~/a/b",
	" = a/~/b", " = ~root/x", " = ~nosuchuser-ovnfs/x", " = :(optional)/nonexistent-ovnfs",
	" = :(optional)~/", " = %(prefix)", " = ./%(prefix)/bin",
}

func TestQueriesMatchGit(t *testing.T) {
	o := newOracle(t)
	r := o.initRepo("repo")
	var b strings.Builder
	b.WriteString("[v]\n")
	for i, v := range typedValues {
		fmt.Fprintf(&b, "\tk%d%s\n", i, v)
	}
	b.WriteString("[Multi \"Sub.Sect\"]\n\tKey = one\n\tkey\n\tKEY = three\n[multi \"sub.sect\"]\n\tkey = other-subsection\n[legacy.Sub]\n\tkey = lower\n")
	b.WriteString("[p]\n\topt = /tmp\n\topt = :(optional)/nonexistent-ovnfs\n")
	appendFile(t, r.gitDir+"/config", b.String())
	env := o.with(map[string]string{"GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "Multi.Sub.Sect.key", "GIT_CONFIG_VALUE_0": "from-env"})
	gitEnv := o.with(map[string]string{"GIT_DIR": r.gitDir})
	maps.Copy(gitEnv, env)
	cfg, err := Load(Options{GitDir: r.gitDir, CommonDir: r.gitDir, LookupEnv: lookup(env)})
	if err != nil {
		t.Fatal(err)
	}

	keys := []string{"Multi.Sub.Sect.KEY", "multi.sub.sect.key", "MULTI.Sub.Sect.Key", "legacy.sub.key", "legacy.Sub.key", "nosuch.key", "p.opt"}
	for i := range typedValues {
		keys = append(keys, "V.K"+strconv.Itoa(i))
	}
	for _, key := range keys {
		t.Run(key, func(t *testing.T) {
			ask := func(args ...string) result {
				return o.git(r.top, gitEnv, append(append([]string{"config"}, args...), key)...)
			}
			check := func(what string, res result, got string, found bool, err error) {
				t.Helper()
				switch {
				case res.code == 1:
					if found || err != nil {
						t.Errorf("%s: git finds nothing, we found %q (err %v)", what, got, err)
					}
				case res.code != 0:
					if err == nil {
						t.Errorf("%s: git fails (%s), we gave %q", what, strings.TrimSpace(res.stderr), got)
					}
				case err != nil || !found:
					t.Errorf("%s: git gives %q, we found=%v err=%v", what, res.out, found, err)
				case strings.TrimSuffix(res.out, "\n") != got:
					t.Errorf("%s: git gives %q, we give %q", what, strings.TrimSuffix(res.out, "\n"), got)
				}
			}

			v, found := cfg.Get(key)
			check("--get", ask("--get"), v, found, nil)

			all := cfg.GetAll(key)
			res := ask("--get-all", "-z")
			var want []string
			if res.code == 0 {
				want = strings.Split(strings.TrimSuffix(res.out, "\x00"), "\x00")
			}
			if !slices.Equal(all, want) && !(len(all) == 0 && len(want) == 0) {
				t.Errorf("--get-all: git %q, we %q", want, all)
			}

			bv, found, err := cfg.Bool(key)
			check("--bool", ask("--bool"), strconv.FormatBool(bv), found, err)

			iv, found, err := cfg.Int(key)
			check("--int", ask("--int"), strconv.FormatInt(iv, 10), found, err)

			pv, found, err := cfg.Path(key)
			pres := ask("--path")
			if strings.HasPrefix(v, "%(prefix)/") {
				if err == nil {
					t.Errorf("--path %q: want an unsupported error", v)
				}
				return
			}
			// Git for Windows turns the expanded home directory's
			// backslashes into slashes; we keep the native spelling.
			pres.out = filepath.FromSlash(pres.out)
			check("--path", pres, filepath.FromSlash(pv), found, err)
		})
	}
}

func TestSubsectionsAndKeys(t *testing.T) {
	cfg, err := Parse([]byte(`[remote "origin"]
	url = a
[Remote "Fork"]
	URL = b
[remote "origin"]
	fetch = c
[branch "main"]
	remote = origin
[remote]
	plain = d
`), "test")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.Subsections("REMOTE"), []string{"origin", "Fork"}; !slices.Equal(got, want) {
		t.Fatalf("Subsections = %q, want %q", got, want)
	}
	e, ok := cfg.Lookup("Remote.Fork.Url")
	if !ok || e.Key != "remote.Fork.url" || e.Section() != "remote" || e.Name() != "url" || e.Line != 4 {
		t.Fatalf("Lookup = %+v, %v", e, ok)
	}
	if sub, ok := e.Subsection(); !ok || sub != "Fork" {
		t.Fatalf("Subsection = %q, %v", sub, ok)
	}
	if _, ok := cfg.Lookup("remote.fork.url"); ok {
		t.Fatal("subsection lookup must be case-sensitive")
	}
}

func TestCanonicalKey(t *testing.T) {
	for in, want := range map[string]string{
		"Core.IgnoreCase":      "core.ignorecase",
		"A.B C.D":              "a.B C.d",
		"remote.My.Origin.URL": "remote.My.Origin.url",
		"A-1.c":                "a-1.c",
		"a..c":                 "a..c",
		"nodot":                "",
		".b":                   "",
		"a.":                   "",
		"a_b.c":                "",
		"a.1b":                 "",
		"a.b\nc.d":             "",
		"":                     "",
	} {
		got, err := CanonicalKey(in)
		if (err == nil) != (want != "") || got != want {
			t.Errorf("CanonicalKey(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}
