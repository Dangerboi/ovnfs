package overlay

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCleanCanonicalForms(t *testing.T) {
	for in, want := range map[string]string{
		"": "", ".": "", "/": "", "//": "", "..": "", "../..": "", "/../a": "a",
		"a": "a", "a/b": "a/b", "a/": "a", "a//b": "a/b", "a/./b": "a/b",
		"a/../b": "b", "a/..": "", "/a/b": "a/b", ".a": ".a", "a/..b": "a/..b", "...": "...",
	} {
		if got := Clean(in); got != want {
			t.Errorf("Clean(%q) = %q, want %q", in, got, want)
		}
	}
	if n := testing.AllocsPerRun(10, func() { _ = Clean("a/b/c.txt") }); n != 0 {
		t.Errorf("Clean allocated %v times for a canonical path", n)
	}
}

func TestReadDirBottomOnlyFiltersWhiteoutNamesAndSorts(t *testing.T) {
	bottom := t.TempDir()
	for _, n := range []string{"b", "a", ".wh.x"} {
		if err := os.WriteFile(filepath.Join(bottom, n), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fsys, err := New(t.TempDir(), nil, dirBottom{bottom}, nil)
	if err != nil {
		t.Fatal(err)
	}
	items, err := fsys.ReadDir("")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].Name() != "a" || items[1].Name() != "b" {
		t.Fatalf("got %v", items)
	}
	empty := t.TempDir()
	fsys, err = New(t.TempDir(), nil, dirBottom{empty}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if items, err := fsys.ReadDir(""); err != nil || items == nil || len(items) != 0 {
		t.Fatalf("empty = %v, %v", items, err)
	}
}
