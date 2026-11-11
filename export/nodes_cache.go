package export

import (
	"syscall"
	"time"

	"github.com/stencil-hq/ovnfs/api"
)

// prepareRename makes the identities needed to finish a committed rename
// resident before backend I/O. A fault elsewhere after that commit must not
// prevent its move record from reaching the identity log.
func (t *nodeTable) prepareRename(from, to string) (func(), error) {
	release, err := t.guard(false, from, to)
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	t.node(from)
	dir, _ := split(to)
	t.dirNode(dir)
	t.node(to)
	extra, err := t.keepPaths(from, to)
	if err == nil {
		err = t.cacheErr
	}
	t.flush()
	t.mu.Unlock()
	finish := func() { t.mu.Lock(); extra(); t.mu.Unlock(); release() }
	if err != nil {
		finish()
		return nil, err
	}
	return finish, nil
}

// guard keeps the affected cold hash owners resident while a backend mutation
// runs. A failed index read therefore rejects the mutation before it changes
// the namespace. Ancestor prefixes cover directories MkdirAll may create.
// all names a TempFile directory, whose generated basename is not known yet.
func (t *nodeTable) guard(all bool, paths ...string) (func(), error) {
	t.mu.Lock()
	if t.cacheErr != nil {
		err := t.cacheErr
		t.mu.Unlock()
		return nil, err
	}
	if t.spillErr != nil {
		t.trimCache()
		if t.spillErr != nil {
			// Generated names and cold-owner promotion need fresh resident state.
			// Refuse them before backend I/O; a known resident path needs neither.
			resident := !all
			for _, p := range paths {
				for p != "" {
					if t.find(p) == nil {
						dir, name := split(p)
						d := t.find(dir)
						if d == nil || !d.leaf(name) {
							resident = false
							break
						}
					}
					p, _ = split(p)
				}
			}
			if !resident {
				t.mu.Unlock()
				return nil, syscall.ENOSPC
			}
		}
	}
	releasePaths, err := t.keepPaths(paths...)
	if err != nil {
		releasePaths()
		t.mu.Unlock()
		return nil, err
	}
	err = t.cacheErr
	t.mu.Unlock()
	release := func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		releasePaths()
		t.trimCache()
	}
	if err != nil {
		release()
		return nil, err
	}
	return release, nil
}

// admitListing prevents a degraded cache from growing through new snapshots.
// Existing handles and cached listings remain usable while spill writes retry.
func (t *nodeTable) admitListing(p string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.spillErr != nil {
		t.trimCache()
		if t.spillErr != nil {
			if d := t.find(p); d == nil || d.dir == nil || d.dir.entries == nil {
				return syscall.ENOSPC
			}
		}
	}
	return t.cacheErr
}

// NodeCacheError reports a failure of the identity index.
func (e *Export) NodeCacheError() string {
	t := e.fs.nodes
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cacheErr != nil {
		return t.cacheErr.Error()
	}
	if t.spillErr != nil {
		return t.spillErr.Error()
	}
	return ""
}

// DefaultNodeCache is the estimated byte budget for reconstructible identities.
const DefaultNodeCache = 512 << 20

// SetNodeCache sets the reconstructible identity budget. Zero keeps only the
// current directory, so a READDIR can still identify its entries in linear time.
// Logged nodes and retired hashes never count towards this budget.
func (e *Export) SetNodeCache(limit int64) {
	t := e.fs.nodes
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cacheLimit = max(limit, 0)
	t.trimCache()
}

// NodeCache reports the estimated resident bytes and configured budget.
func (e *Export) NodeCache() api.Usage {
	t := e.fs.nodes
	t.mu.Lock()
	defer t.mu.Unlock()
	return api.Usage{Used: t.cacheUsed, Limit: t.cacheLimit}
}

// NodeIndexBytes reports the ownership index files, separately from RSS.
func (e *Export) NodeIndexBytes() int64 {
	t := e.fs.nodes
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.coldBytes
}

// touch marks a cached directory most recently used, under the node mutex.
func (t *nodeTable) touch(d *node) {
	s := d.state()
	if s.recent != nil {
		t.cacheLRU.MoveToFront(s.recent)
	}
}

// charge adjusts a directory's estimated cache bytes. Estimates include map
// overhead and count shared names twice rather than undercounting.
func (t *nodeTable) charge(d *node, delta int64) {
	if delta == 0 {
		t.touch(d)
		return
	}
	s := d.state()
	s.version++
	s.cached += delta
	t.cacheUsed += delta
	if s.cached == 0 {
		if s.recent != nil {
			t.cacheLRU.Remove(s.recent)
			s.recent = nil
		}
	} else if s.recent == nil {
		s.recent = t.cacheLRU.PushFront(d)
	} else {
		t.cacheLRU.MoveToFront(s.recent)
	}
}

// listBytes estimates the listing's maps, duplicate slices and cloned names.
func (s *dirState) listBytes() int64 {
	var n int64
	for _, name := range s.entries {
		n += int64(64 + len(name))
	}
	for h := range s.dups {
		n += s.dupBytes(h)
	}
	return n
}

func (s *dirState) dupBytes(h uint32) int64 {
	var n int64
	for _, name := range s.dups[h] {
		n += int64(32 + len(name))
	}
	return n
}

// uncache removes a deleted directory from the LRU without changing its identity.
func (t *nodeTable) uncache(d *node) {
	if s := d.dir; s != nil {
		t.cacheUsed -= s.cached
		s.cached = 0
		if s.recent != nil {
			t.cacheLRU.Remove(s.recent)
			s.recent = nil
		}
	}
}

// trimCache drops whole cold directories. Keeping the current directory avoids
// rereading a large directory once per returned entry: retained cache is bounded
// by the budget plus one directory, even when that directory exceeds the budget.
func (t *nodeTable) trimCache() {
	if len(t.coldGarbage) > 0 {
		t.cleanCold()
	}
	if t.cacheUsed <= t.cacheLimit && t.spillErr == nil {
		return
	}
	// Invalidation can free the failed candidate without another eviction.
	// Resume normal admission and start a new spill attempt if it fills again.
	if t.cacheUsed <= t.cacheLimit {
		t.spillErr = nil
		t.spillRetry = time.Time{}
		t.spillBackoff = 0
		return
	}
	if t.cacheErr != nil || time.Now().Before(t.spillRetry) {
		return
	}
	for t.cacheUsed > t.cacheLimit && t.cacheLRU.Len() > 1 {
		candidate := t.cacheLRU.Back()
		for candidate != t.cacheLRU.Front() && candidate.Value.(*node).dir.reading > 0 {
			candidate = candidate.Prev()
		}
		if candidate == t.cacheLRU.Front() {
			return
		}
		d := candidate.Value.(*node)
		written, err := t.spillNames(d)
		if err != nil {
			// A failed replacement leaves the old index and resident owners intact.
			t.spillErr = err
			if t.spillBackoff == 0 {
				t.spillBackoff = 100 * time.Millisecond
			} else {
				t.spillBackoff = min(2*t.spillBackoff, 30*time.Second)
			}
			t.spillRetry = time.Now().Add(t.spillBackoff)
			return
		}
		if !written || t.byID[d.id] != d || d.dir.reading != 0 || d.dir.recent == t.cacheLRU.Front() || t.cacheUsed <= t.cacheLimit {
			return
		}
		t.spillErr = nil
		t.spillRetry = time.Time{}
		t.spillBackoff = 0
		t.uncache(d)
		s := d.dir
		s.gen++ // an in-flight listing must not resurrect the evicted snapshot
		s.entries, s.dups, s.leaves = nil, nil, nil
		s.coldLoaded = false
		for name, k := range d.kids {
			if k.key == 0 && k.holds == 0 {
				// A node with children is logged. An unlogged directory may
				// have a listing of its own, which must leave the LRU too.
				t.uncache(k)
				delete(t.byID, k.id)
				delete(d.kids, name)
			}
		}
		// Maps do not shrink on delete; rebuild the pinned children only.
		if len(d.kids) == 0 {
			d.kids = nil
		} else {
			kids := make(map[string]*node, len(d.kids))
			for name, k := range d.kids {
				kids[name] = k
			}
			d.kids = kids
		}
	}
}

// retain holds a handle used for protocol state. Such identities must remain in
// subtree invalidation and recall scans even while ordinary walk results expire.
func (t *nodeTable) retain(id uint64) func() {
	t.mu.Lock()
	defer t.mu.Unlock()
	defer t.trimCache()
	defer t.flush()
	n := t.byID[id]
	if n == nil {
		d, name := t.resolve(id)
		// Resolution may wait while another retainer materializes or moves it.
		// Retain that exact identity; never derive a second id at its new path.
		n = t.byID[id]
		if n == nil && d != nil && derivedID(d.key, nameHash(name)) == id && d.kids[name] == nil {
			if !t.takenExceptCold(d, nameHash(name)) {
				t.addLeaf(d, name)
			}
			n = t.child(d, name)
		}
	}
	if n == nil {
		return func() {}
	}
	if n.key == 0 && n.holds == 0 {
		t.charge(n.parent, -int64(160+len(n.name)))
	}
	n.holds++
	return func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		n.holds--
		if n.key == 0 && n.holds == 0 && t.byID[id] == n {
			t.charge(n.parent, int64(160+len(n.name)))
		}
		t.trimCache()
	}
}
