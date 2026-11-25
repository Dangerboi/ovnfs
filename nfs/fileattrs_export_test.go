package nfs

import (
	"testing"

	"github.com/go-git/go-billy/v5"
)

// CheckFileAttrs compares every descriptor-derived NFS attribute to its path counterpart.
func CheckFileAttrs(t *testing.T, fs Backend, id uint64, p string, f billy.File) {
	t.Helper()
	s := Server{FS: fs}
	want, err := s.stat(id, p)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.fileAttrs(id, f)
	if err != nil || got != want {
		t.Fatalf("%s: descriptor attrs %+v, %v; path attrs %+v", p, got, err, want)
	}
}
