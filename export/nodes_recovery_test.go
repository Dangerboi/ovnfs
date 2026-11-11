package export

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRecoveryAcrossDirectoryRename rejects a listing read at an old path
// after its directory (or an ancestor) has moved and the path has been reused.
func TestRecoveryAcrossDirectoryRename(t *testing.T) {
	for _, ancestor := range []bool{false, true} {
		t.Run(map[bool]string{false: "directory", true: "ancestor"}[ancestor], func(t *testing.T) {
			ns := newNamespace("co/d/file")
			name := filepath.Join(t.TempDir(), "nodes.log")
			nt := openLogged(t, name, ns)
			old := nt.id("co/d/file")
			closeLogged(t, nt)
			nt = openLogged(t, name, ns)
			original := nt.list
			moved := false
			nt.list = func(dir string) ([]os.FileInfo, error) {
				if !moved && dir == "co/d" {
					moved = true
					from, to := "co/d", "co/moved"
					if ancestor {
						from, to = "co", "moved"
					}
					ns.move(from, to)
					nt.rename(from, to)
					ns.add("co/d/replacement")
				}
				return original(dir)
			}
			want := "co/moved/file"
			if ancestor {
				want = "moved/d/file"
			}
			if got := pathOf(t, nt, old); got != want {
				t.Fatalf("old handle = %s, want %s", got, want)
			}
		})
	}
}
