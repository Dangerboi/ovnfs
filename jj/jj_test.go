package jj

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRepo checks how a workspace root names its repository: the main
// workspace holds it, another links to it relative to .jj or by absolute path,
// and a folder without .jj/repo has none.
func TestRepo(t *testing.T) {
	work := t.TempDir()
	main, relative, absolute, plain := filepath.Join(work, "main"), filepath.Join(work, "relative"), filepath.Join(work, "absolute"), filepath.Join(work, "plain")
	repo := filepath.Join(main, Dir, "repo")
	for _, dir := range []string{repo, filepath.Join(relative, Dir), filepath.Join(absolute, Dir), filepath.Join(plain, Dir)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(relative, Dir, "repo"), []byte("../../main/.jj/repo"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(absolute, Dir, "repo"), []byte(repo), 0o644); err != nil {
		t.Fatal(err)
	}
	for root, want := range map[string]string{main: repo, relative: repo, absolute: repo, plain: "", work: ""} {
		if got, err := Repo(root); err != nil || got != want {
			t.Errorf("Repo(%s) = %q, %v; want %q", root, got, err, want)
		}
	}
}
