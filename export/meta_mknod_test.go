package export

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/go-git/go-billy/v5"
)

// specialFS implements special creation without depending on the host's node
// support. Identity bookkeeping is independent of the backend's file type.
type specialFS struct {
	billy.Filesystem
	calls int
	after func()
}

func (f *specialFS) Mknod(name string, mode os.FileMode) error {
	f.calls++
	file, err := f.Filesystem.Create(name)
	if err != nil {
		return err
	}
	file.Close()
	if f.after != nil {
		f.after()
	}
	return nil
}

func TestColdSpecialCreation(t *testing.T) {
	for _, mode := range []os.FileMode{os.ModeSocket, os.ModeNamedPipe} {
		t.Run(mode.String(), func(t *testing.T) {
			n := newMetadataTestFS(t)
			defer n.nodes.close()
			f := &specialFS{Filesystem: n.Filesystem}
			n.Filesystem = f
			for _, p := range []string{"co/" + collideA, "pressure/entry"} {
				d, _ := split(p)
				n.MkdirAll(d, 0755)
				file, err := n.Create(p)
				if err != nil {
					t.Fatal(err)
				}
				file.Close()
			}
			old := n.nodes.id("co/" + collideA)
			n.nodes.cacheLimit = 0
			n.nodes.id("pressure/entry")
			n.nodes.cacheLimit = 1 << 20
			if err := n.Mknod("co/"+collideB, mode|0600); err != nil {
				t.Fatal(err)
			}
			id := n.nodes.id("co/" + collideB)
			if id == old {
				t.Fatal("special node inherited an issued hash")
			}
			n.nodes.cacheLimit = 0
			n.nodes.id("pressure/entry")
			if got := pathOf(t, n.nodes, id); got != "co/"+collideB {
				t.Fatal(got)
			}
			if err := n.Remove("co/" + collideB); err != nil {
				t.Fatal(err)
			}
			if _, ok := n.nodes.path(id); ok {
				t.Fatal("removed special node stayed live")
			}
		})
	}
}

func TestSpecialCreationIdentityFault(t *testing.T) {
	for _, late := range []bool{false, true} {
		t.Run(map[bool]string{false: "preflight", true: "completion"}[late], func(t *testing.T) {
			n := newMetadataTestFS(t)
			defer n.nodes.close()
			fault := func() { n.nodes.mu.Lock(); defer n.nodes.mu.Unlock(); n.nodes.cacheFailure(os.ErrPermission) }
			f := &specialFS{Filesystem: n.Filesystem}
			n.Filesystem = f
			if late {
				f.after = fault
			} else {
				fault()
			}
			if err := n.Mknod("special", os.ModeSocket|0600); !errors.Is(err, syscall.EIO) {
				t.Fatal(err)
			}
			if !late && f.calls != 0 {
				t.Fatal("fault reached special node creation")
			}
		})
	}
}

func TestSpecialCreationThenAdmissionRefusal(t *testing.T) {
	for _, mode := range []os.FileMode{os.ModeSocket, os.ModeNamedPipe} {
		t.Run(mode.String(), func(t *testing.T) {
			n := newMetadataTestFS(t)
			defer n.nodes.close()
			for _, p := range []string{"co/old", "pressure/entry"} {
				d, _ := split(p)
				if err := n.MkdirAll(d, 0755); err != nil {
					t.Fatal(err)
				}
				file, err := n.Create(p)
				if err != nil {
					t.Fatal(err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
				n.nodes.id(p)
			}
			old := n.nodes.id("co/old")
			blocked := filepath.Join(t.TempDir(), "blocked")
			if err := os.WriteFile(blocked, nil, 0600); err != nil {
				t.Fatal(err)
			}
			n.Filesystem = &specialFS{Filesystem: n.Filesystem, after: func() {
				n.nodes.mu.Lock()
				defer n.nodes.mu.Unlock()
				n.nodes.cacheLimit = 0
				n.nodes.coldRoot = blocked
			}}
			if err := n.Mknod("co/new", mode|0600); err != nil {
				t.Fatal(err)
			}
			if n.nodes.spillErr == nil {
				t.Fatal("did not force a failed spill")
			}
			if err := n.IdentityError(); err != nil {
				t.Fatal("failed spill poisoned existing identities:", err)
			}
			if id := n.nodes.id("co/new"); id != 0 {
				t.Fatalf("refused identity: got %x", id)
			}
			if got := pathOf(t, n.nodes, old); got != "co/old" {
				t.Fatal(got)
			}
			n.nodes.coldRoot = ""
		})
	}
}
