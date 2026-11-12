//go:build darwin || linux

package fsys

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestNanosSaturates(t *testing.T) {
	const billion = 1_000_000_000
	for _, c := range []struct{ sec, nsec, want int64 }{
		{1, 5, 1_000_000_005},
		{-1, 0, -billion},
		{math.MaxInt64, 999_999_999, math.MaxInt64},
		{math.MaxInt64 / billion, math.MaxInt64 % billion, math.MaxInt64},
		{math.MaxInt64 / billion, math.MaxInt64%billion + 1, math.MaxInt64},
		{math.MaxInt64/billion + 1, 0, math.MaxInt64},
		{math.MinInt64, 0, math.MinInt64},
		{math.MinInt64, 5, math.MinInt64 + 5},
		{math.MinInt64/billion - 1, 7, math.MinInt64 + 7},
		{math.MinInt64 / billion, math.MinInt64 % billion, math.MinInt64},
		{math.MinInt64 / billion, math.MinInt64%billion - 1, math.MinInt64},
	} {
		if got := nanos(c.sec, c.nsec); got != c.want {
			t.Errorf("nanos(%d,%d) = %d, want %d", c.sec, c.nsec, got, c.want)
		}
	}
}

func TestNULNameRejected(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	s := Native.NewSession()
	if _, err := s.OpenDir(dir + "\x00junk"); err == nil {
		t.Error("OpenDir accepted embedded NUL")
	}
	if _, err := s.Stat(filepath.Join(dir, "a\x00b"), true); err == nil {
		t.Error("Stat accepted embedded NUL")
	}
	if _, _, err := s.OpenFile(Handle{}, filepath.Join(dir, "a\x00b")); err == nil {
		t.Error("OpenFile accepted embedded NUL")
	}
}

func TestIdentifyDirSurvivesRename(t *testing.T) {
	root := t.TempDir()
	d := filepath.Join(root, "d")
	if err := os.Mkdir(d, 0o755); err != nil {
		t.Fatal(err)
	}
	s := Native.NewSession()
	h, err := s.OpenDir(d)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(h)
	want, err := s.Identify(d)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(d, filepath.Join(root, "e")); err != nil {
		t.Fatal(err)
	}
	got, err := s.IdentifyDir(h)
	if err != nil || got != want {
		t.Fatalf("IdentifyDir = %v, %v; want %v", got, err, want)
	}
}
