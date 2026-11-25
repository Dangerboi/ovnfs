// Package kept retains open files between the requests of stateless file
// protocols (NFSv3, the kernel client's), which name files but never open or
// close them: opening and closing a host descriptor per request costs more
// than the request.
package kept

import (
	"container/list"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-git/go-billy/v5"
)

// Defaults of a zero [Cache]'s limits.
const (
	// DefaultIdle is how long a kept file outlives its last use. Clients
	// send a file's cached writes when it is closed, so the writes of one
	// open come in a burst.
	DefaultIdle = 10 * time.Second
	// DefaultMax bounds the files kept at once; past it the least recently
	// used idle one is closed.
	DefaultMax = 4096
)

// Cache keeps open files by key, closing each once it has been idle for
// Idle, and the least recently used idle one when Max files are kept. The
// zero value is ready to use with the default limits; set the limits before
// first use.
type Cache[K comparable] struct {
	// Idle is how long a kept file outlives its last use; zero: DefaultIdle.
	Idle time.Duration
	// Max bounds the files kept at once; zero: DefaultMax.
	Max int

	mu    sync.Mutex
	files map[K]*list.Element // of *entry[K]
	lru   list.List           // most recently used first
	count atomic.Int32
}

// Len reports the number of currently kept files.
func (c *Cache[K]) Len() int { return int(c.count.Load()) }

// entry is one kept file. It closes once idle with no call under way, or
// once dropped and its last call returned.
type entry[K comparable] struct {
	mu    sync.RWMutex // exclusive for With (e.g. seek/write); shared for WithShared (ReadAt/Stat)
	key   K
	f     billy.File
	users int
	gone  bool // dropped while in use: the last user closes it
	timer *time.Timer
}

// Has reports whether key has a kept file.
func (c *Cache[K]) Has(key K) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.files[key]
	return ok
}

// Keep takes ownership of f, closing it if key is already cached or every
// slot is busy. Any descriptor closed here reports its error to the caller.
func (c *Cache[K]) Keep(key K, f billy.File) error {
	return c.KeepIf(key, f, nil)
}

// KeepIf takes ownership of f and caches it under key when allow is nil or
// returns true under the cache lock; otherwise it closes f without caching.
// It also closes f if key is already cached or every slot is busy. Any
// descriptor closed here reports its error to the caller.
func (c *Cache[K]) KeepIf(key K, f billy.File, allow func() bool) error {
	_, err := c.insert(key, f, allow, false)
	return err
}

// Adopt pins a newly opened file until release. Like Keep it takes ownership
// even if the cache is full: then release closes f instead of retaining it.
func (c *Cache[K]) Adopt(key K, f billy.File) (release func(), err error) {
	e, err := c.insert(key, f, nil, true)
	if e == nil {
		return func() { _ = f.Close() }, err
	}
	return func() { e.mu.Unlock(); c.release(e) }, err
}

// insert optionally pins f before making it visible to other cache users.
func (c *Cache[K]) insert(key K, f billy.File, allow func() bool, pin bool) (*entry[K], error) {
	c.mu.Lock()
	var evicted billy.File
	admit := (allow == nil || allow()) && c.files[key] == nil
	if admit && len(c.files) >= c.max() {
		evicted = c.evictLocked()
		admit = evicted != nil
	}
	if !admit {
		c.mu.Unlock()
		if pin {
			return nil, nil
		}
		return nil, f.Close()
	}
	if c.files == nil {
		c.files = make(map[K]*list.Element)
	}
	e := &entry[K]{key: key, f: f}
	if pin {
		e.users++
		e.mu.Lock()
	}
	c.files[key] = c.lru.PushFront(e)
	c.count.Add(1)
	e.timer = time.AfterFunc(c.idle(), func() { c.expire(e) })
	c.mu.Unlock()
	if evicted != nil {
		return e, evicted.Close()
	}
	return e, nil
}

// Borrow holds a kept file exclusively until release, including across
// goroutines; eviction, Drop and Close cannot close it during this use.
func (c *Cache[K]) Borrow(key K) (f billy.File, release func()) {
	e := c.acquire(key)
	if e == nil {
		return nil, nil
	}
	e.mu.Lock()
	return e.f, func() { e.mu.Unlock(); c.release(e) }
}

// With serializes fn on key's kept file; ok is false when there is none.
func (c *Cache[K]) With(key K, fn func(billy.File) error) (ok bool, err error) {
	return c.with(key, false, fn)
}

// WithShared runs fn on key's kept file concurrently with other WithShared
// callers (for positioned operations such as ReadAt and Stat); ok is false
// when there is none.
func (c *Cache[K]) WithShared(key K, fn func(billy.File) error) (ok bool, err error) {
	return c.with(key, true, fn)
}

func (c *Cache[K]) with(key K, shared bool, fn func(billy.File) error) (ok bool, err error) {
	e := c.acquire(key)
	if e == nil {
		return false, nil
	}
	if shared {
		e.mu.RLock()
		err = fn(e.f)
		e.mu.RUnlock()
	} else {
		e.mu.Lock()
		err = fn(e.f)
		e.mu.Unlock()
	}
	c.release(e)
	return true, err
}

// acquire reserves one user before waiting on the entry's IO lock.
func (c *Cache[K]) acquire(key K) *entry[K] {
	c.mu.Lock()
	defer c.mu.Unlock()
	el := c.files[key]
	if el == nil {
		return nil
	}
	e := el.Value.(*entry[K])
	e.users++
	c.lru.MoveToFront(el)
	return e
}

// release closes a dropped entry once its last user is done with it.
func (c *Cache[K]) release(e *entry[K]) {
	c.mu.Lock()
	e.users--
	closing := e.gone && e.users == 0
	if !e.gone && e.users == 0 {
		e.timer.Reset(c.idle())
	}
	c.mu.Unlock()
	if closing {
		e.f.Close()
	}
}

// Drop forgets key's kept file and closes it, at once when it is idle, else
// after the call using it returns. Callers drop a file whose name or id went
// away, or whose descriptor no longer shows the file's current content.
func (c *Cache[K]) Drop(key K) error {
	c.mu.Lock()
	el := c.files[key]
	if el == nil {
		c.mu.Unlock()
		return nil
	}
	e := c.removeLocked(el)
	busy := e.users > 0
	c.mu.Unlock()
	if busy {
		return nil
	}
	return e.f.Close()
}

// DropFunc forgets every kept file whose key match accepts, closing idle
// ones at once and busy ones once their in-flight calls return.
func (c *Cache[K]) DropFunc(match func(K) bool) {
	c.mu.Lock()
	var idle []billy.File
	for el := c.lru.Front(); el != nil; {
		next := el.Next()
		if match(el.Value.(*entry[K]).key) {
			if e := c.removeLocked(el); e.users == 0 {
				idle = append(idle, e.f)
			}
		}
		el = next
	}
	c.mu.Unlock()
	for _, f := range idle {
		f.Close()
	}
}

// Close drops every kept file, as a server's process exiting would: Windows
// cannot remove a folder while a file in it is open.
func (c *Cache[K]) Close() {
	c.mu.Lock()
	var idle []billy.File
	for el := c.lru.Front(); el != nil; el = c.lru.Front() {
		if e := c.removeLocked(el); e.users == 0 {
			idle = append(idle, e.f)
		}
	}
	c.mu.Unlock()
	for _, f := range idle {
		f.Close()
	}
}

// removeLocked unlinks el's entry and marks it gone; the caller closes it
// when it has no users.
func (c *Cache[K]) removeLocked(el *list.Element) *entry[K] {
	e := el.Value.(*entry[K])
	e.timer.Stop()
	e.gone = true
	c.lru.Remove(el)
	delete(c.files, e.key)
	c.count.Add(-1)
	return e
}

// expire closes e once it is idle.
func (c *Cache[K]) expire(e *entry[K]) {
	c.mu.Lock()
	el := c.files[e.key]
	if el == nil || el.Value != e {
		c.mu.Unlock()
		return
	}
	if e.users > 0 {
		e.timer.Reset(c.idle())
		c.mu.Unlock()
		return
	}
	c.removeLocked(el)
	c.mu.Unlock()
	e.f.Close()
}

// evictLocked drops the least recently used idle file and returns it for
// the caller to close outside the lock; nil when every file is in use.
func (c *Cache[K]) evictLocked() billy.File {
	for el := c.lru.Back(); el != nil; el = el.Prev() {
		if e := el.Value.(*entry[K]); e.users == 0 {
			c.removeLocked(el)
			return e.f
		}
	}
	return nil
}

// idle is the configured idle lifetime.
func (c *Cache[K]) idle() time.Duration {
	if c.Idle > 0 {
		return c.Idle
	}
	return DefaultIdle
}

// max is the configured bound.
func (c *Cache[K]) max() int {
	if c.Max > 0 {
		return c.Max
	}
	return DefaultMax
}

// WriteAt writes data at offset of f, without moving an offset others
// share where the file can.
func WriteAt(f billy.File, data []byte, offset int64) (int, error) {
	if w, ok := f.(io.WriterAt); ok {
		return w.WriteAt(data, offset)
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return 0, err
	}
	return f.Write(data)
}
