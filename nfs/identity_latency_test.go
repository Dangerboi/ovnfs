package nfs

import (
	"sync"
	"testing"
	"time"
)

type slowIdentityFS struct {
	*testFS
	once            sync.Once
	entered, resume chan struct{}
	release         bool
}

func (f *slowIdentityFS) pause() { f.once.Do(func() { close(f.entered); <-f.resume }) }
func (f *slowIdentityFS) HoldFileID(uint64) func() {
	if !f.release {
		f.pause()
	}
	return func() {
		if f.release {
			f.pause()
		}
	}
}

// An index read during OPEN, or a spill on CLOSE, must not delay another
// client's RENEW. The barrier models an index operation without disk timing.
func TestSlowIdentityDoesNotBlockRenew(t *testing.T) {
	for _, release := range []bool{false, true} {
		t.Run(map[bool]string{false: "acquire", true: "release"}[release], func(t *testing.T) {
			fs := &slowIdentityFS{testFS: newTestFS(t), entered: make(chan struct{}), resume: make(chan struct{}), release: release}
			fs.cacheable = func(string) bool { return false }
			srv, addr := v4serve(t, fs, nil)
			c, _ := v4holder(t, srv, addr, "opener")
			other := v4dial(t, addr)
			other.setup(srv, "unrelated", nil)
			fs.write(t, "f", "content")
			done := make(chan status, 1)
			go func() {
				s, o := c.open("reader", "f", share4Read, false)
				if s == stOK && release {
					s = c.run(v4Root(), v4Look("f"), v4Close(o.id)).status
				}
				done <- s
			}()
			select {
			case <-fs.entered:
			case <-time.After(time.Second):
				t.Fatal("identity I/O did not start")
			}
			renewed := make(chan status, 1)
			go func() { renewed <- other.run(v4Renew(other.client)).status }()
			select {
			case s := <-renewed:
				if s != stOK {
					t.Error(s)
				}
			case <-time.After(time.Second):
				close(fs.resume)
				t.Fatal("unrelated RENEW blocked behind index I/O")
			}
			close(fs.resume)
			select {
			case s := <-done:
				if s != stOK {
					t.Fatal(s)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("state operation did not finish")
			}
		})
	}
}
