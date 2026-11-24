package overlay

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

type gatedBottom struct {
	dirBottom
	started  chan struct{}
	release  chan struct{}
	once     sync.Once
	linkGate bool
}

func (b *gatedBottom) wait(p string) {
	if p == "slow" {
		b.once.Do(func() { close(b.started) })
		<-b.release
	}
}

func (b *gatedBottom) Open(p string) (Content, error) {
	b.wait(p)
	return b.dirBottom.Open(p)
}

func (b *gatedBottom) CopyTo(p, dst string) error {
	b.wait(p)
	return b.dirBottom.CopyTo(p, dst)
}

func (b *gatedBottom) Readlink(p string) (string, error) {
	if b.linkGate && p == "link" {
		b.wait("slow")
	}
	return b.dirBottom.Readlink(p)
}

// Copy-up must not publish stale bytes when another writer or a rebase wins
// while the bottom read is in flight.
func TestSlowCopyUpRechecksSource(t *testing.T) {
	for _, change := range []string{"write", "rebase"} {
		for _, memory := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/memory=%t", change, memory), func(t *testing.T) {
				lower := t.TempDir()
				must(t, os.WriteFile(filepath.Join(lower, "slow"), []byte("old"), 0o644))
				b := &gatedBottom{dirBottom: dirBottom{lower}, started: make(chan struct{}), release: make(chan struct{})}
				var release sync.Once
				unblock := func() { release.Do(func() { close(b.release) }) }
				t.Cleanup(unblock)
				var mem *Memory
				if memory {
					mem = NewMemory(1<<20, 1<<20, 0)
				}
				f, err := New(filepath.Join(t.TempDir(), "upper"), nil, b, mem)
				must(t, err)
				type result struct {
					data []byte
					err  error
				}
				done := make(chan result, 1)
				go func() {
					h, err := f.OpenFile("slow", os.O_RDWR, 0)
					if err != nil {
						done <- result{err: err}
						return
					}
					data, err := io.ReadAll(h)
					h.Close()
					done <- result{data: data, err: err}
				}()
				select {
				case <-b.started:
				case <-time.After(2 * time.Second):
					t.Fatal("copy-up did not start")
				}
				if change == "write" {
					write(t, f, "slow", "new", os.O_WRONLY|os.O_TRUNC)
				} else {
					next := t.TempDir()
					must(t, os.WriteFile(filepath.Join(next, "slow"), []byte("new"), 0o644))
					f.Rebase(dirBottom{next})
				}
				unblock()
				got := <-done
				if got.err != nil || string(got.data) != "new" || read(t, f, "slow") != "new" {
					t.Fatalf("open after %s: %q, %v", change, got.data, got.err)
				}
			})
		}
	}
}

// A copy that finishes staging second leaves the original ID with the alias
// that was already copied up, even if both began with the same lower inode.
func TestConcurrentBottomHardlinkCopyUp(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows metadata has no link count")
	}
	memories(t, func(t *testing.T, mem func() *Memory) {
		lower := tree(t, map[string]string{"slow": "original"})
		must(t, os.Link(filepath.Join(lower, "slow"), filepath.Join(lower, "artifact")))
		b := &gatedBottom{dirBottom: dirBottom{lower}, started: make(chan struct{}), release: make(chan struct{})}
		release := sync.OnceFunc(func() { close(b.release) })
		defer release()
		f, err := New(t.TempDir(), nil, b, mem())
		must(t, err)
		shared := idOf(t, f, "slow")
		done := make(chan error, 1)
		go func() { done <- writeFile(f, "slow", []byte("+"), os.O_WRONLY|os.O_APPEND) }()
		select {
		case <-b.started:
		case <-time.After(2 * time.Second):
			t.Fatal("copy-up did not reach the bottom")
		}
		names := f.Names()
		write(t, f, "artifact", "rebuilt", os.O_WRONLY|os.O_TRUNC)
		if f.Names() == names {
			t.Error("a handle resolution cached before the split still looks current")
		}
		release()
		must(t, <-done)
		if got := idOf(t, f, "slow"); got == shared {
			t.Fatal("both independently writable copies claim the shared handle")
		} else {
			assertLocated(t, f, got, "slow", true)
		}
		assertLocated(t, f, shared, "artifact", true)
		if got := read(t, f, "slow"); got != "original+" {
			t.Fatalf("second copy = %q", got)
		}
	})
}

func TestSlowBottomOpenDoesNotBlockOtherOperations(t *testing.T) {
	for _, tc := range []struct {
		name     string
		path     string
		flag     int
		memory   bool
		linkGate bool
	}{
		{"read", "slow", os.O_RDONLY, false, false},
		{"symlink", "link", os.O_RDONLY, false, false},
		{"symlink target", "link", os.O_RDONLY, false, true},
		{"write", "slow", os.O_RDWR, false, false},
		{"memory write", "slow", os.O_RDWR, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lower := t.TempDir()
			if err := os.WriteFile(filepath.Join(lower, "slow"), []byte("original"), 0o644); err != nil {
				t.Fatal(err)
			}
			if tc.path == "link" {
				if err := os.Symlink("slow", filepath.Join(lower, "link")); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			}
			b := &gatedBottom{dirBottom: dirBottom{lower}, started: make(chan struct{}), release: make(chan struct{}), linkGate: tc.linkGate}
			var release sync.Once
			unblock := func() { release.Do(func() { close(b.release) }) }
			t.Cleanup(unblock)
			var memory *Memory
			if tc.memory {
				memory = NewMemory(1<<20, 1<<20, 0)
			}
			f, err := New(filepath.Join(t.TempDir(), "upper"), nil, b, memory)
			if err != nil {
				t.Fatal(err)
			}
			// The mkdir below may still be queued (queue.go) when the test ends.
			t.Cleanup(func() { f.Settle() })
			opened := make(chan error, 1)
			go func() {
				h, err := f.OpenFile(tc.path, tc.flag, 0)
				if err == nil {
					_, err = io.ReadAll(h)
					h.Close()
				}
				opened <- err
			}()
			select {
			case <-b.started:
			case <-time.After(2 * time.Second):
				t.Fatal("bottom open did not start")
			}
			other := make(chan error, 1)
			go func() { other <- f.MkdirAll("independent", 0o755) }()
			select {
			case err := <-other:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(250 * time.Millisecond):
				t.Error("unrelated mkdir blocked on a bottom open")
			}
			unblock()
			if err := <-opened; err != nil {
				t.Error(err)
			}
		})
	}
}

type countingInfoBottom struct {
	dirBottom
	lstats    int
	openInfos int
}

func (b *countingInfoBottom) Lstat(p string) (os.FileInfo, error) {
	b.lstats++
	return b.dirBottom.Lstat(p)
}

func (b *countingInfoBottom) OpenInfo(p string) (Content, os.FileInfo, error) {
	b.openInfos++
	info, err := b.dirBottom.Lstat(p)
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, info, nil
	}
	c, err := b.dirBottom.Open(p)
	return c, info, err
}

// TestOpenLowerUsesSingleOpenInfo checks that opening a regular bottom file
// via InfoOpener does not also call Bottom.Lstat.
func TestOpenLowerUsesSingleOpenInfo(t *testing.T) {
	lower := t.TempDir()
	must(t, os.WriteFile(filepath.Join(lower, "data.bin"), []byte("payload"), 0o644))
	b := &countingInfoBottom{dirBottom: dirBottom{lower}}
	f, err := New(filepath.Join(t.TempDir(), "upper"), nil, b, nil)
	must(t, err)
	b.lstats = 0
	h, err := f.Open("data.bin")
	must(t, err)
	got, err := io.ReadAll(h)
	h.Close()
	must(t, err)
	if string(got) != "payload" {
		t.Fatalf("read = %q, want payload", got)
	}
	if b.openInfos != 1 || b.lstats != 0 {
		t.Fatalf("openInfos=%d lstats=%d, want 1 and 0", b.openInfos, b.lstats)
	}
}
