package config

import (
	"maps"
	"testing"
)

// TestGlobMatchesGit checks the wildmatch implementation through includeIf
// onbranch:, the condition whose pattern git applies with the fewest
// rewrites (only a trailing '/' gains "**").
func TestGlobMatchesGit(t *testing.T) {
	o := newOracle(t)
	r := o.initRepo("repo")
	marker := o.write("marker.inc", "[t]\n\tm = 1\n")
	branches := []string{"main", "Main", "feature/x", "feature/x/y", "a.b", "fix-1", "x/y/z", "x/z", "ab", "a", "release/v1.2", "]"}
	patterns := []string{
		"*", "**", "*/*", "**/x", "feature/**", "feature/**/y", "**/y", "f*/x", "feature/*", "feature/",
		"fea?ure/x", "[a-m]ain", "[!m]ain", "[^m]ain", "[[:alpha:]]ain", "[[:upper:]]ain", `\main`, `ma\*`,
		"a/**", "a**", "**b", "x/**/z", "x/**/**/z", "*.b", "[a-]*", "[]]", "[", "main/", "**/", "/main",
		"fix-[0-9]", "release/v1.[0-9]", "[[:digit:][:alpha:]]*", "[z-a]*", "[[:bogus:]]ain", `main\`,
		"***", "x/***/z", "x**/z", "**/**", "[!]]*", `[\]]`, "[a-c-e]*", "?", "??",
	}
	for _, branch := range branches {
		o.mustGit(r.top, "symbolic-ref", "HEAD", "refs/heads/"+branch)
		for _, pattern := range patterns {
			env := o.with(map[string]string{"GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "includeIf.onbranch:" + pattern + ".path", "GIT_CONFIG_VALUE_0": marker})
			gitEnv := o.with(map[string]string{"GIT_DIR": r.gitDir})
			maps.Copy(gitEnv, env)
			res := o.git(r.top, gitEnv, "config", "--get", "t.m")
			cfg, err := Load(Options{GitDir: r.gitDir, CommonDir: r.gitDir, LookupEnv: lookup(env), Branch: branchOf(r.gitDir)})
			if err != nil {
				t.Fatal(err)
			}
			_, got := cfg.Get("t.m")
			if want := res.code == 0; got != want {
				t.Errorf("onbranch:%s on %s: git %v, ours %v", pattern, branch, want, got)
			}
		}
	}
}
