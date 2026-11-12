package hostfile

import (
	"os"
	"path/filepath"
	"testing"
)

// TestJunctionStandsInForSymlink checks the junction [Symlink] falls back to
// without the symlink privilege: it reads back as a symlink to its exact
// target (the registry compares the objects link's target), leads into the
// directory, and removing the tree holding it leaves the target alone.
func TestJunctionStandsInForSymlink(t *testing.T) {
	target := filepath.Join(t.TempDir(), "objects")
	if err := os.MkdirAll(filepath.Join(target, "pack"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitdir := t.TempDir()
	link := filepath.Join(gitdir, "objects")
	if err := os.Mkdir(link, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := setJunction(link, target); err != nil {
		t.Fatal(err)
	}
	if got, err := os.Readlink(link); err != nil || got != target {
		t.Fatalf("readlink = %q, %v; want %q", got, err, target)
	}
	if info, err := Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("lstat = %v, %v; want a symlink", info, err)
	}
	if info, err := os.Stat(filepath.Join(link, "pack")); err != nil || !info.IsDir() {
		t.Fatalf("stat through junction = %v, %v", info, err)
	}
	if err := os.RemoveAll(gitdir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(target, "pack")); err != nil {
		t.Fatalf("target after removing the junction's tree: %v", err)
	}
}
