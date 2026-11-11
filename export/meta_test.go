//go:build !windows

package export

import (
	"io"
	"os"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5"
)

// TestFailedRenameDoesNotLogAncestors keeps failed probes from accumulating
// logged identities for destination directories which do not exist.
func TestFailedRenameDoesNotLogAncestors(t *testing.T) {
	n := newMetadataTestFS(t)
	f, err := n.Create("source")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	n.nodes.id("source")
	before, next := len(n.nodes.byID), n.nodes.next
	if err := n.Rename("source", "missing/deep/destination"); !os.IsNotExist(err) {
		t.Fatalf("rename: %v", err)
	}
	if len(n.nodes.byID) != before || n.nodes.next != next {
		t.Fatal("failed rename logged nonexistent ancestors")
	}
}

// TestRenameMetadataByProtocol covers the metadata clients see when a linker
// replaces its output while keeping the temporary file open. Only NFS needs
// a synthetic mtime change; SMB and inode-based frontends must preserve it.
func TestRenameMetadataByProtocol(t *testing.T) {
	for _, protocol := range []string{"SMB", "Nodes", "NFS"} {
		for name, mode := range map[string]os.FileMode{"executable": 0o755, "archive": 0o644} {
			t.Run(protocol+"/"+name, func(t *testing.T) {
				e := &Export{fs: newMetadataTestFS(t)}
				var fs billy.Filesystem
				var change billy.Change
				switch protocol {
				case "SMB":
					backend := e.SMB()
					fs, change = backend, backend
				case "Nodes":
					nodes := e.Nodes()
					fs, change = nodes, nodes
				case "NFS":
					backend := e.NFS()
					fs, change = backend, backend
				}

				const from, to = "output.tmp", "output"
				f, err := fs.OpenFile(from, os.O_CREATE|os.O_RDWR, mode)
				if err != nil {
					t.Fatal(err)
				}
				defer f.Close()
				const prefix, suffix = "before rename\n", "after rename\n"
				if _, err := f.Write([]byte(prefix)); err != nil {
					t.Fatal(err)
				}
				victim, err := fs.Create(to)
				if err != nil {
					t.Fatal(err)
				}
				if err := victim.Close(); err != nil {
					t.Fatal(err)
				}
				nodes := e.Nodes()
				id, replaced := nodes.ID(from), nodes.ID(to)
				if err := change.Chmod(from, mode); err != nil {
					t.Fatal(err)
				}
				past := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
				if err := change.Chtimes(from, past, past); err != nil {
					t.Fatal(err)
				}
				if err := fs.Rename(from, to); err != nil {
					t.Fatal(err)
				}
				info, err := fs.Lstat(to)
				if err != nil {
					t.Fatal(err)
				}
				if protocol == "NFS" {
					if !info.ModTime().After(past) {
						t.Errorf("NFS rename did not advance freshness: mtime %v", info.ModTime())
					}
				} else if !info.ModTime().Equal(past) {
					t.Errorf("rename changed mtime from %v to %v", past, info.ModTime())
				}
				if _, err := fs.Lstat(from); !os.IsNotExist(err) {
					t.Fatalf("old name remains: %v", err)
				}
				if nodes.ID(to) != id {
					t.Fatal("rename changed the file id")
				}
				if p, ok := nodes.Path(id); !ok || p != to {
					t.Fatalf("renamed id resolves to %q, live %v", p, ok)
				}
				if _, ok := nodes.Path(replaced); ok {
					t.Fatal("replaced file id remains live")
				}
				if err := fs.Rename(to, to); err != nil {
					t.Fatal(err)
				}
				same, err := fs.Lstat(to)
				if err != nil {
					t.Fatal(err)
				}
				if !same.ModTime().Equal(info.ModTime()) {
					t.Fatal("same-path rename changed mtime")
				}

				if _, err := f.Write([]byte(suffix)); err != nil {
					t.Fatal(err)
				}
				if err := f.Close(); err != nil {
					t.Fatal(err)
				}
				reopened, err := fs.Open(to)
				if err != nil {
					t.Fatal(err)
				}
				data, err := io.ReadAll(reopened)
				closeErr := reopened.Close()
				if err != nil || closeErr != nil {
					t.Fatalf("read renamed output: %v; close: %v", err, closeErr)
				}
				if string(data) != prefix+suffix {
					t.Fatalf("renamed output %q, want %q", data, prefix+suffix)
				}
				if err := fs.Remove(to); err != nil {
					t.Fatal(err)
				}
				if _, ok := nodes.Path(id); ok {
					t.Fatal("removed file id remains live")
				}
			})
		}
	}
}
