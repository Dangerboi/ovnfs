//go:build darwin || linux

package fsys

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
)

// TestReadDirManyEntries lists more entries than one listing buffer holds and
// checks each against lstat, sorted by name.
func TestReadDirManyEntries(t *testing.T) {
	dir := t.TempDir()
	for i := range 4000 {
		name := fmt.Sprintf("a-rather-long-file-name-to-fill-the-listing-buffer-%05d", i)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name[:i%40]), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(plain) {
		t.Fatalf("ReadDir lists %d entries, os.ReadDir %d", len(got), len(plain))
	}
	for i, e := range got {
		if e.Name() != plain[i].Name() {
			t.Fatalf("entry %d is %q, want %q (sorted by name)", i, e.Name(), plain[i].Name())
		}
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		want, err := os.Lstat(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode() != want.Mode() || info.Size() != want.Size() || !info.ModTime().Equal(want.ModTime()) || !reflect.DeepEqual(info.Sys(), want.Sys()) {
			t.Fatalf("%s: got %v %d %v %+v, lstat has %v %d %v %+v", e.Name(),
				info.Mode(), info.Size(), info.ModTime(), info.Sys(), want.Mode(), want.Size(), want.ModTime(), want.Sys())
		}
	}
}

// TestReadDirErrors checks that failures to list a directory are classified as
// os.ReadDir's are, and that an empty directory lists nothing.
func TestReadDirErrors(t *testing.T) {
	dir := t.TempDir()
	if got, err := ReadDir(dir); err != nil || len(got) != 0 {
		t.Fatalf("empty directory lists %v, %v", got, err)
	}
	if _, err := ReadDir(filepath.Join(dir, "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing directory: %v, want not exist", err)
	}
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadDir(file); !errors.Is(err, syscall.ENOTDIR) {
		t.Fatalf("file as directory: %v, want not a directory", err)
	}
	if os.Geteuid() == 0 {
		return
	}
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(locked, 0o755)
	if _, err := ReadDir(locked); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("unreadable directory: %v, want permission", err)
	}
}
