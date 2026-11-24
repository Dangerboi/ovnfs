package nfs

import (
	"sync/atomic"

	"github.com/go-git/go-billy/v5"
	"github.com/stencil-hq/ovnfs/internal/kept"
)

// keptKey names an NFSv3 kept file (v3State.kept). It separates cached
// writers, keyed by id and current path, from permission fallbacks, whose
// empty path lets them follow renames like an already-open file. Cached
// writers reopen after a rename so a backend that reopens descriptors across
// snapshots remembers the current path.
type keptKey struct {
	id   uint64
	path string
}

// defaultKeptReaders bounds the read-only descriptors retained across NFSv3
// READ bursts. Keeping readers apart from v3State.kept ensures scanning many
// files cannot evict a permission-fallback writer.
const defaultKeptReaders = 512

// readCache retains open read-only files across sequential NFSv3 READ bursts
// so streaming large files does not reopen, lstat, fstat and close the host
// descriptor on every RPC. Mutations advance the file's stamp and drop its
// descriptor; keep checks the stamp under the cache lock so an open that
// began before a mutation cannot cache a stale handle.
type readCache struct {
	cache  kept.Cache[uint64]
	clock  atomic.Uint64
	all    atomic.Uint64
	stamps [1024]atomic.Uint64
}

func (r *readCache) active() bool { return r.cache.Len() > 0 }

func (r *readCache) stamp() uint64 { return r.clock.Load() }

func (r *readCache) bucket(id uint64) *atomic.Uint64 {
	return &r.stamps[(id^(id>>32))%uint64(len(r.stamps))]
}

func (r *readCache) bump(b *atomic.Uint64) {
	at := r.clock.Add(1)
	for last := b.Load(); last < at && !b.CompareAndSwap(last, at); last = b.Load() {
	}
}

func (r *readCache) changed(id, before uint64) bool {
	return r.all.Load() > before || r.bucket(id).Load() > before
}

func (r *readCache) with(id uint64, fn func(billy.File) error) (bool, error) {
	return r.cache.WithShared(id, fn)
}

func (r *readCache) keep(id uint64, f billy.File, before uint64) error {
	return r.cache.KeepIf(id, f, func() bool { return !r.changed(id, before) })
}

func (r *readCache) drop(id uint64) { _ = r.cache.Drop(id) }

func (r *readCache) invalidate(id uint64) {
	r.bump(r.bucket(id))
	_ = r.cache.Drop(id)
}

func (r *readCache) invalidateRange(first, last uint64) {
	if first == last {
		r.invalidate(first)
		return
	}
	r.bump(&r.all)
	r.cache.DropFunc(func(id uint64) bool { return first <= id && id <= last })
}

func (r *readCache) close() { r.cache.Close() }
