package checkout

import (
	"fmt"
	"regexp"

	"github.com/stencil-hq/ovnfs/base"
	"github.com/stencil-hq/ovnfs/watch"
)

// A checkout of a local base reads the folder's cache and local roots as its
// cache snapshot (base.Caches) fixes them, never as the folder has them now:
// local roots forward directly to the folder by default, or are copied at
// checkout open when FrozenLocal is set; cache roots use a time fence.
// A checkout uses the base's newest snapshot, and takes a new one when there is
// none or the folder changed at those roots since the newest began, as far
// as the folder watcher reported; without the watcher covering the folder
// every checkout takes its own. Checkouts already open keep theirs. A
// snapshot costs its local roots, however large the cache roots are. One no
// checkout uses any more is discarded by collectGarbage, and one on disk no
// checkout names by the next Open.
//
// A checkout prepared on a warm capture (see prepareOnWarm) gets its snapshot
// when it is named, so it sees the folder as of its create.

// cachesPattern accepts cache snapshot names.
var cachesPattern = regexp.MustCompile(`^cv?_[0-9a-f]{12}$`)

// baseCaches is the cache snapshot bookkeeping of a local base, guarded by
// Registry.cachesMu.
type baseCaches struct {
	newest *base.Caches         // the snapshot the next checkout uses unless stale; nil for none
	stale  bool                 // the folder changed below a cache or local root since newest began
	users  map[*base.Caches]int // checkouts using each snapshot
}

// deadCaches is a cache snapshot no checkout uses, to be removed.
type deadCaches struct {
	view *base.Base
	id   string
}

// useCaches returns the snapshot a checkout opening on b now uses, counted as
// one more user: the newest unless stale, else one taken now. Remote bases
// have none. r.mu may be held.
func (r *Registry) useCaches(b *baseRecord) (*base.Caches, error) {
	m := b.view.Meta()
	if m.Remote != nil {
		return nil, nil
	}
	r.cachesMu.Lock()
	defer r.cachesMu.Unlock()
	bc := &b.caches
	if bc.newest == nil || bc.stale || r.watcher == nil || !r.watcher.Covers(m.Dir) {
		prefix := base.VirtualCachesPrefix
		if r.config.FrozenLocal {
			prefix = "c_"
		}
		id, err := randomID(prefix)
		if err != nil {
			return nil, err
		}
		bc.newest, bc.stale = b.view.TakeCaches(id), false
	}
	bc.use(bc.newest)
	return bc.newest, nil
}

// openCaches returns the snapshot named id a persisted checkout on b uses,
// counted as one more user; a checkout persisted without one (an older
// build's) gets one now, as at create. Only Open calls it.
func (r *Registry) openCaches(b *baseRecord, id string) (*base.Caches, error) {
	if b.view.Meta().Remote != nil {
		return nil, nil
	}
	if id == "" {
		return r.useCaches(b)
	}
	c := b.view.OpenCaches(id)
	r.cachesMu.Lock()
	b.caches.use(c)
	r.cachesMu.Unlock()
	return c, nil
}

// use counts one more user of c.
func (bc *baseCaches) use(c *base.Caches) {
	if bc.users == nil {
		bc.users = make(map[*base.Caches]int)
	}
	bc.users[c]++
}

// setCaches makes c, counted for it by useCaches, the snapshot checkout h reads.
func (h *checkoutRecord) setCaches(c *base.Caches) {
	h.caches, h.meta.Caches = c, ""
	if c != nil {
		h.meta.Caches = c.ID()
	}
}

// dropCaches ends h's use of its cache snapshot, when h goes away or never
// got published; the last user's leaving queues the snapshot for removal.
func (r *Registry) dropCaches(h *checkoutRecord) {
	c := h.caches
	if c == nil {
		return
	}
	h.setCaches(nil)
	r.cachesMu.Lock()
	bc := &h.base.caches
	bc.users[c]--
	dead := bc.users[c] <= 0
	if dead {
		delete(bc.users, c)
		if bc.newest == c {
			bc.newest = nil
		}
		r.deadCaches = append(r.deadCaches, deadCaches{view: h.base.view, id: c.ID()})
	}
	r.cachesMu.Unlock()
	if dead {
		r.wakeGC()
	}
}

// cachesChangedLocked marks stale the newest snapshot of every local base whose
// folder changed below, at or above a cache or local root; r.mu is held for
// reading.
func (r *Registry) cachesChangedLocked(changes []watch.Change) {
	r.cachesMu.Lock()
	defer r.cachesMu.Unlock()
	for _, b := range r.bases {
		bc := &b.caches
		if bc.newest == nil || bc.stale {
			continue
		}
		dir := b.view.Meta().Dir
		for _, c := range changes {
			if rel, ok := below(dir, c.Path); ok && bc.newest.AffectedBy(b.view, rel) {
				bc.stale = true
				break
			}
		}
	}
}

// orphanCachesLocked queues for removal the snapshots on disk that no checkout
// names: those a stop left between a checkout's removal and collectGarbage,
// or taken for a checkout whose create did not finish. Open calls it once
// the persisted checkouts hold theirs.
func (r *Registry) orphanCachesLocked() error {
	r.cachesMu.Lock()
	defer r.cachesMu.Unlock()
	for _, b := range r.bases {
		ids, err := b.view.CachesIDs()
		if err != nil {
			return fmt.Errorf("cache snapshots of base %s: %w", b.view.Meta().ID, err)
		}
		used := make(map[string]bool, len(b.caches.users))
		for c := range b.caches.users {
			used[c.ID()] = true
		}
		for _, id := range ids {
			if !used[id] {
				r.deadCaches = append(r.deadCaches, deadCaches{view: b.view, id: id})
			}
		}
	}
	return nil
}

// removeDeadCaches discards the snapshots queued for removal; collectGarbage
// calls it without registry locks, since a release waits for a snapshot still
// being taken.
func (r *Registry) removeDeadCaches() {
	r.cachesMu.Lock()
	dead := r.deadCaches
	r.deadCaches = nil
	r.cachesMu.Unlock()
	for _, d := range dead {
		if err := r.discard(d.view.ReleaseCaches(d.id), "caches-"+d.id); err != nil {
			r.enqueueNotice(baseNotice{fault: "remove cache snapshot " + d.id + ": " + err.Error()})
		}
	}
}
