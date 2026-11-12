package git

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
)

// attrNames are the attributes the differential cases ask about: the ones the
// checkout conversions read, the binary macro's members and custom ones.
var attrNames = []string{
	"text", "eol", "crlf", "filter", "ident", "working-tree-encoding",
	"diff", "merge", "binary", "foo", "bar", "baz", "m", "nested", "sub", "qux", "x", "infomacro", "globalmacro",
}

// attrRepo creates a repository whose attributes come only from the test's files.
func attrRepo(t *testing.T) *Repo {
	t.Helper()
	r := testRepo(t)
	t.Setenv("GIT_ATTR_NOSYSTEM", "1")
	t.Setenv("GIT_ATTR_SOURCE", "")
	os.Unsetenv("GIT_ATTR_SOURCE")
	// git init turns core.ignorecase on by itself on case-insensitive file systems.
	testGit(t, r.Top, "config", "core.ignorecase", "false")
	return r
}

// gitCheckAttr asks git check-attr -z --stdin for names on every path.
func gitCheckAttr(t *testing.T, r *Repo, names, paths []string) [][]string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", r.Top, "check-attr", "-z", "--stdin"}, names...)...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	cmd.Stdin = strings.NewReader(strings.Join(paths, "\x00") + "\x00")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git check-attr: %v", err)
	}
	fields := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	if len(fields) != 3*len(names)*len(paths) {
		t.Fatalf("git check-attr gave %d fields for %d paths", len(fields), len(paths))
	}
	answers := make([][]string, len(paths))
	for n := range paths {
		for range names {
			answers[n] = append(answers[n], fields[2])
			fields = fields[3:]
		}
	}
	return answers
}

// compareAttr checks every path's attributes against git's answer.
func compareAttr(t *testing.T, r *Repo, names, paths []string) {
	t.Helper()
	want := gitCheckAttr(t, r, names, paths)
	a, err := r.Attributes(names, AttributeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for n, p := range paths {
		got, err := a.Check(p)
		if err != nil {
			t.Fatalf("Check(%q): %v", p, err)
		}
		for i, name := range names {
			if got[i] != want[n][i] {
				t.Errorf("%q %s = %q, git says %q", p, name, got[i], want[n][i])
			}
		}
	}
}

func TestAttributesMatchGit(t *testing.T) {
	r := attrRepo(t)
	global := filepath.Join(t.TempDir(), "attributes")
	testGit(t, r.Top, "config", "core.attributesFile", global)
	testWrite(t, global, strings.Join([]string{
		"[attr]globalmacro bar",
		"[attr]m baz=global",
		"*.gm globalmacro",
		"* x=global",
		"*.txt filter=global ident",
	}, "\n")+"\n")
	testWrite(t, filepath.Join(r.CommonDir, "info", "attributes"), strings.Join([]string{
		"*.jpg !diff",
		"*.txt eol=crlf",
		"[attr]infomacro foo",
		"*.im infomacro",
		"*.override binary",
	}, "\n")+"\n")
	testWrite(t, filepath.Join(r.Top, ".gitattributes"), "\ufeff"+strings.Join([]string{
		"# comment",
		"\t# indented comment",
		"[attr]m foo -bar baz=1",
		"[attr]nested m qux",
		"*.txt text eol=lf",
		"*.bat text eol=crlf -crlf",
		"*.jpg binary",
		"*.png -binary",
		"*.gif binary=yes",
		"*.sh text ident",
		"*.m m",
		"*.n nested",
		"*.mt m text",
		"*.tm text m",
		"*.ms m=set",
		"*.cfg filter=lfs diff=lfs merge=lfs -text",
		"*.utf16 working-tree-encoding=UTF-16LE-BOM",
		"*.override text diff",
		"!*.neg text",
		`\!bang.txt foo`,
		`"with space.txt" foo=quoted`,
		`"tab\there" bar`,
		`"\101\102.oct" baz`,
		`"unterminated foo`,
		`"bad\q" bar`,
		`"\041neg" foo`,
		`"glued"foo bar`,
		"**/logs/** x=logs",
		"docs/**/*.md text=auto",
		"build/ x=dir",
		"vendor x=vendor",
		"/anchored.txt x=anchored",
		"*.bad foo bad!",
		"*.reserved foo builtin_x",
		"*.val foo=a=b -bar=ignored !baz=ignored",
		"*.crlf\tfoo\r",
		"*.long foo=" + strings.Repeat("v", attrMaxLine),
		"*.notlong foo=" + strings.Repeat("v", attrMaxLine-len("*.notlong foo=")-1),
		"*.unspec foo bar",
		"*.unspec !foo",
		"*.vt foo\vbar",
	}, "\n")+"\n")
	testWrite(t, filepath.Join(r.Top, "sub", ".gitattributes"), strings.Join([]string{
		"[attr]sub qux",
		"*.s sub",
		"*.txt -text crlf",
		"/local.txt x=local",
		"deep/*.c x=deep",
		"*.n !qux",
	}, "\n")+"\n")
	testWrite(t, filepath.Join(r.Top, "sub", "deep", ".gitattributes"), "* filter=deep\n*.m -m\n")
	testWrite(t, filepath.Join(r.Top, "dir", "attr", ".gitattributes"), "* foo=wrongdir\n")
	compareAttr(t, r, attrNames, []string{
		"a.txt", "sub/a.txt", "sub/deep/a.txt", "sub/deep/a.c", "sub/local.txt", "local.txt", "sub/x/local.txt",
		"a.bat", "img.jpg", "sub/img.jpg", "a.png", "a.gif", "run.sh", "f.m", "sub/deep/f.m", "f.n", "sub/f.n",
		"f.mt", "f.tm", "f.ms", "lfs.cfg", "a.utf16", "f.override", "x.neg", "!bang.txt", "bang.txt",
		"with space.txt", "with space", "tab\there", "AB.oct", `"unterminated`, "unterminated", `"bad\q"`, `"badq"`,
		"!neg", "glued", "logs", "logs/a", "a/logs/b/c", "docs/a.md", "docs/x/y/a.md", "docs/a.txt",
		"build/", "build", "build/a", "vendor", "vendor/", "vendor/a", "anchored.txt", "sub/anchored.txt",
		"f.bad", "f.reserved", "f.val", "f.crlf", "f.long", "f.notlong", "f.unspec", "f.vt", "f.im", "f.gm", "sub/s.s",
		"dir/attr", "dir/attr/", "dir/attr/f", "plain", "deep/a/b/c/d.txt",
	})
}

func TestAttributesIgnoreCase(t *testing.T) {
	r := attrRepo(t)
	testWrite(t, filepath.Join(r.Top, ".gitattributes"), strings.Join([]string{
		"*.TXT text",
		"Docs/**/*.Md eol=crlf",
		"[A-C]*.z foo",
		"/Anchored x=anchored",
		"\u00c9.e foo=accent",
		"[attr]Mac bar",
		"*.mac mac",
	}, "\n")+"\n")
	testWrite(t, filepath.Join(r.Top, "sub", ".gitattributes"), "/Q x=q\nA* baz\n")
	paths := []string{
		"a.txt", "A.TXT", "a.Txt", "docs/x/readme.md", "DOCS/README.MD", "b.z", "B.Z", "d.z",
		"anchored", "ANCHORED", "\u00e9.e", "\u00c9.e", "f.mac", "sub/q", "sub/Q", "sub/abc", "sub/Abc",
	}
	names := []string{"text", "eol", "foo", "x", "bar", "baz", "mac", "Mac"}
	for _, fold := range []string{"false", "true"} {
		t.Run("ignorecase="+fold, func(t *testing.T) {
			testGit(t, r.Top, "config", "core.ignorecase", fold)
			compareAttr(t, r, names, paths)
		})
	}
}

func TestAttributesDefaultGlobalFile(t *testing.T) {
	r := attrRepo(t)
	testWrite(t, filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "git", "attributes"), "*.g foo\n*.h bar\n")
	testWrite(t, filepath.Join(r.Top, ".gitattributes"), "*.h -bar\n")
	compareAttr(t, r, []string{"foo", "bar"}, []string{"a.g", "a.h", "sub/a.g"})
}

// TestAttributesIndexFallback checks that a .gitattributes git cannot open in the
// work tree is read from the index the way git reads it, while a directory in
// its place hides the staged file.
func TestAttributesIndexFallback(t *testing.T) {
	r := attrRepo(t)
	stage := func(dir, content string) {
		name := filepath.Join(r.Top, dir, ".gitattributes")
		testWrite(t, name, content)
		testGit(t, r.Top, "add", "--", filepath.ToSlash(filepath.Join(dir, ".gitattributes")))
		if err := os.Remove(name); err != nil {
			t.Fatal(err)
		}
	}
	stage("", "[attr]staged foo\n*.r staged\n")
	stage("missing", "*.a bar\n")
	stage("asdir", "*.a bar\n")
	if err := os.Mkdir(filepath.Join(r.Top, "asdir", ".gitattributes"), 0o755); err != nil {
		t.Fatal(err)
	}
	paths := []string{"a.r", "missing/a.a", "missing/x/a.r", "asdir/a.a"}
	if runtime.GOOS != "windows" {
		stage("link", "*.a bar\n")
		testWrite(t, filepath.Join(r.Top, "target"), "*.a baz\n")
		if err := os.Symlink("../target", filepath.Join(r.Top, "link", ".gitattributes")); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, "link/a.a")
	}
	compareAttr(t, r, []string{"staged", "foo", "bar", "baz"}, paths)
}

// TestAttributesIndexSnapshot checks that a captured index backs the fallback,
// not the live index, and still lists .gitattributes entries ordinary Entries
// omit (skip-worktree).
func TestAttributesIndexSnapshot(t *testing.T) {
	r := attrRepo(t)
	stage := func(dir, content string) {
		name := filepath.Join(r.Top, dir, ".gitattributes")
		testWrite(t, name, content)
		testGit(t, r.Top, "add", "--", filepath.ToSlash(filepath.Join(dir, ".gitattributes")))
		if err := os.Remove(name); err != nil {
			t.Fatal(err)
		}
	}
	stage("sparse", "*.a snap\n")
	stage("plain", "*.a snap\n")
	testGit(t, r.Top, "update-index", "--skip-worktree", "sparse/.gitattributes")
	snap, err := r.ReadIndex(r.IndexPath())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range snap.Entries {
		if e.Path == "sparse/.gitattributes" {
			t.Fatal("skip-worktree entry listed in Entries")
		}
	}
	// The live index moves on after the snapshot.
	stage("plain", "*.a live\n")
	stage("late", "*.a live\n")

	check := func(opts AttributeOptions, p string) string {
		t.Helper()
		a, err := r.Attributes([]string{"snap", "live"}, opts)
		if err != nil {
			t.Fatal(err)
		}
		got, err := a.Check(p)
		if err != nil {
			t.Fatal(err)
		}
		return got[0] + "/" + got[1]
	}
	for p, want := range map[string]string{
		"sparse/x.a": "set/unspecified",
		"plain/x.a":  "set/unspecified",
		"late/x.a":   "unspecified/unspecified",
	} {
		if got := check(AttributeOptions{Index: &snap}, p); got != want {
			t.Errorf("snapshot %s = %s, want %s", p, got, want)
		}
	}
	if got := check(AttributeOptions{}, "late/x.a"); got != "unspecified/set" {
		t.Errorf("live late/x.a = %s, want unspecified/set", got)
	}
}

func TestAttributesConcurrentChecks(t *testing.T) {
	r := attrRepo(t)
	var paths []string
	for d := range 8 {
		dir := fmt.Sprintf("d%d", d)
		testWrite(t, filepath.Join(r.Top, dir, ".gitattributes"), fmt.Sprintf("*.c foo=%d\n*.h -text\n", d))
		paths = append(paths, dir+"/a.c", dir+"/x/a.h", dir+"/b.txt")
	}
	testWrite(t, filepath.Join(r.Top, ".gitattributes"), "*.txt text\n*.h text foo\n")
	names := []string{"text", "foo"}
	want := gitCheckAttr(t, r, names, paths)
	a, err := r.Attributes(names, AttributeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for n, p := range paths {
				got, err := a.Check(p)
				if err != nil || !slices.Equal(got, want[n]) {
					t.Errorf("Check(%q) = %q, %v; git says %q", p, got, err, want[n])
				}
			}
		})
	}
	wg.Wait()
}

func TestAttributesRejectedQueries(t *testing.T) {
	r := attrRepo(t)
	for _, names := range [][]string{nil, {"a+b"}, {"-text"}, {"[attr]x"}} {
		if _, err := r.Attributes(names, AttributeOptions{}); err == nil {
			t.Errorf("Attributes(%q) succeeded", names)
		}
	}
	if _, err := r.Attributes([]string{"builtin_objectmode"}, AttributeOptions{}); !errors.Is(err, ErrAttrUnsupported) {
		t.Errorf("builtin_objectmode: %v", err)
	}
	a, err := r.Attributes([]string{"text"}, AttributeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"", "/abs", "a//b", "./a", "a/../b"} {
		if _, err := a.Check(p); err == nil {
			t.Errorf("Check(%q) succeeded", p)
		}
	}
}

// TestAttributesTreeSource reads .gitattributes from the tree attr.tree or
// GIT_ATTR_SOURCE names instead of the work tree, as git does; an attr.tree
// naming no tree is ignored, a bad GIT_ATTR_SOURCE is an error.
func TestAttributesTreeSource(t *testing.T) {
	r := attrRepo(t)
	testWrite(t, filepath.Join(r.Top, ".gitattributes"), "*.c foo\n")
	testWrite(t, filepath.Join(r.Top, "sub", ".gitattributes"), "*.c bar=committed\n")
	testGit(t, r.Top, "add", "--all")
	testGit(t, r.Top, "commit", "-qm", "attributes")
	testWrite(t, filepath.Join(r.Top, "sub", ".gitattributes"), "*.c bar=worktree\n")
	names, paths := []string{"foo", "bar"}, []string{"a.c", "sub/a.c"}
	for _, tree := range []string{"HEAD", "HEAD~0^{tree}", "no-such-ref"} {
		t.Run("attr.tree="+tree, func(t *testing.T) {
			testGit(t, r.Top, "config", "attr.tree", tree)
			defer testGit(t, r.Top, "config", "--unset", "attr.tree")
			compareAttr(t, r, names, paths)
		})
	}
	t.Run("GIT_ATTR_SOURCE", func(t *testing.T) {
		t.Setenv("GIT_ATTR_SOURCE", "HEAD")
		compareAttr(t, r, names, paths)
		t.Setenv("GIT_ATTR_SOURCE", "no-such-ref")
		if _, err := r.Attributes(names, AttributeOptions{}); err == nil {
			t.Error("a GIT_ATTR_SOURCE naming no tree was accepted")
		}
	})
}
