package nfs

import "sync/atomic"

// dirChanges tells READDIRPLUS which attributes in a snapshot may be stale.
// Namespace mutations mark the affected directories (and their entries in
// their parents), single-file mutations mark that file (covering all hard
// links to it), and checkout-wide recalls mark the checkout's slot bucket.
// Hash collisions cost an extra stat, never a stale reply.
type dirChanges struct {
	clock atomic.Uint64
	all   atomic.Uint64
	slots [256]atomic.Uint64
	dirs  [1024]atomic.Uint64
	files [1024]atomic.Uint64
}

// file returns the change stamp shared by entry ids in the same bucket. Fold
// the high bits so consecutive checkout slots do not all alias the same bucket.
func (c *dirChanges) file(id uint64) *atomic.Uint64 {
	return &c.files[(id^(id>>32))%uint64(len(c.files))]
}

// dir returns the change stamp shared by directories in the same bucket.
func (c *dirChanges) dir(id uint64) *atomic.Uint64 {
	return &c.dirs[(id^(id>>32))%uint64(len(c.dirs))]
}

// slot returns the change stamp for the checkout slot bucket holding id.
func (c *dirChanges) slot(id uint64) *atomic.Uint64 {
	return &c.slots[(id>>sizeBucketBits)%uint64(len(c.slots))]
}

// mark publishes a mutation in the clock used to date directory snapshots.
// Two writers can reach a bucket in reverse clock order; never move it back.
func (c *dirChanges) mark(bucket *atomic.Uint64) {
	at := c.clock.Add(1)
	for last := bucket.Load(); last < at && !bucket.CompareAndSwap(last, at); last = bucket.Load() {
	}
}

// markFile marks one file id (and any hard links sharing it) as changed.
func (c *dirChanges) markFile(id uint64) { c.mark(c.file(id)) }

// markDir marks directory id as having changed entries, and also marks id's
// own entry stamp because directory mutations update its mtime and link count.
func (c *dirChanges) markDir(id uint64) {
	c.mark(c.dir(id))
	c.mark(c.file(id))
}

// markRange marks every entry and directory in [first, last] as changed,
// narrowing single-entry and single-slot ranges instead of invalidating every
// checkout.
func (c *dirChanges) markRange(first, last uint64) {
	if first == last {
		c.markDir(first)
		return
	}
	if first>>sizeBucketBits == last>>sizeBucketBits {
		c.mark(c.slot(first))
		return
	}
	c.invalidate()
}

// invalidate covers changes whose effects span multiple checkouts or cannot
// be narrowed to one directory or file.
func (c *dirChanges) invalidate() { c.mark(&c.all) }
