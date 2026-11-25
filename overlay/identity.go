package overlay

import (
	"errors"
	"os"
	"sync"

	"github.com/stencil-hq/ovnfs/internal/ident"
)

// An entry's identity (see package ident) comes from its backing file: the
// file's own inode ([ident.State]), unless an identity is persisted beside it.
// The overlay persists one whenever a new backing file takes over an entry
// whose identity it must keep (copy-up, spill, a truncating open of a lower
// file, an ancestor directory made for a write), and for every new entry of
// an upper on a volume that reuses inode numbers (see [reusesInodes]), where
// an inode's identity could alias a removed file's that a client still
// holds. The attribute names below are the medium: an extended attribute on
// Unix, an alternate data stream on Windows (see the platform files), holding
// 8 bytes big-endian. Both belong to the inode, so they follow renames and
// hard links and survive freezing an upper into a layer; a clone of the file
// (clonefile on macOS) copies them. A file that cannot hold one keeps its
// identity in its directory's [idTable].
const (
	idAttr   = "id"   // an entry's persisted identity
	nextAttr = "next" // an upper's or layer's Synthetic high-water mark, on its root directory
)

// syntheticBlock is how many Synthetic values one persisted high-water mark
// reserves, so that allocating rarely writes.
const syntheticBlock = 1 << 16

// syntheticLimit bounds the values [synthetic] hands out; the values above
// are the hashed identities of entries of version 1 manifests that recorded
// no inode (see [legacyIdentity]).
const syntheticLimit = 1 << (ident.ValueBits - 1)

// errNoAttr reports a backing file that cannot hold an attribute: a Linux
// symlink or special file (user extended attributes are regular files' and
// directories' only), or a filesystem without them. Its identity goes to the
// [idTable] instead.
var errNoAttr = errors.New("overlay: file cannot hold identity attributes")

// errShortAttr reports an interrupted write of a Windows attribute stream.
var errShortAttr = errors.New("overlay: incomplete attribute stream")

// Seams through which tests imitate other hosts on this one.
var (
	inodeOf            = fileKey      // the inode of a host file
	canHoldAttr        = holdsAttr    // whether a file of a type can hold an attribute
	volumeReusesInodes = reusesInodes // whether a volume hands out removed files' inode numbers again
)

// idInfo is host metadata of a backing file with the identity of the entry it
// backs. Unix wraps only files whose persisted identity differs from their
// inode, whose native stat [ident.Of] reads otherwise; Windows wraps every
// file, its native record carrying no file index.
type idInfo struct {
	os.FileInfo
	key inodeKey
	id  ident.ID
}

// Identity returns the entry's identity.
func (i idInfo) Identity() ident.ID { return i.id }

// backing returns the identity record beneath an alias name, if any.
func backing(info os.FileInfo) (idInfo, bool) {
	if a, ok := info.(aliasInfo); ok {
		info = a.FileInfo
	}
	i, ok := untimed(info).(idInfo)
	return i, ok
}

// hostKey returns the backing inode of upper or layer metadata; false for a
// memory file or metadata without a host stat.
func hostKey(info os.FileInfo) (inodeKey, bool) {
	if i, ok := backing(info); ok {
		return i.key, true
	}
	if _, ok := unalias(info).(memInfo); ok {
		return inodeKey{}, false
	}
	return nativeKey(info)
}

// keepIdentity gives fresh host metadata info of a file the identity indexed,
// earlier metadata of the same file, recorded.
func keepIdentity(info, indexed os.FileInfo) os.FileInfo {
	if i, ok := backing(indexed); ok {
		return idInfo{FileInfo: info, key: i.key, id: i.id}
	}
	return info
}

// sameEntry reports whether a and b describe one entry: two hard-link names
// of one file, at any level, share its identity.
func sameEntry(a, b os.FileInfo) bool {
	x, ok := ident.Of(a)
	y, ok2 := ident.Of(b)
	return ok && ok2 && x == y
}

// idStore keeps the identities of the files of one upper or raw layer
// directory: a cache of those persisted, the [idTable] of files that cannot
// hold an attribute, and, for an upper, the allocator of new ones.
type idStore struct {
	known  identities
	table  *idTable
	syn    *synthetic // nil for a frozen layer, which is never written
	fresh  bool       // the volume reuses inode numbers: every entry gets a Synthetic identity
	assign sync.Mutex // serializes giving an entry first seen without identity one
}

// newUpperStore opens the identities of upper directory root over layers.
func newUpperStore(root string, layers []*Layer) (*idStore, error) {
	syn, err := newSynthetic(root, layers)
	if err != nil {
		return nil, err
	}
	table, err := loadTable(root, false)
	if err != nil {
		return nil, err
	}
	fresh, err := volumeReusesInodes(root)
	if err != nil {
		return nil, err
	}
	return &idStore{table: table, syn: syn, fresh: fresh}, nil
}

// layerStore opens the identities of raw layer directory root, read-only.
func layerStore(root string) (*idStore, error) {
	table, err := loadTable(root, true)
	if err != nil {
		return nil, err
	}
	return &idStore{table: table}, nil
}

// persisted returns the identity persisted for host file name, described by
// its Lstat info, with inode key; 0 when none is.
func (s *idStore) persisted(name string, info os.FileInfo, key inodeKey) (ident.ID, error) {
	if canHoldAttr(info.Mode()) {
		id, err := readID(name)
		// Only a writable upper can replace an incomplete ID. Treat it like
		// a missing one so inheritance and the locked allocator work as usual.
		if err != nil && s != nil && s.syn != nil && errors.Is(err, errShortAttr) {
			err = nil
		}
		if err != nil || id != 0 {
			return id, err
		}
	}
	if s == nil {
		return 0, nil
	}
	return s.table.get(key.inode), nil
}

// write persists id for host file name, described by its Lstat info, with
// inode key: as an attribute, or in the table where the file cannot hold one.
func (s *idStore) write(name string, info os.FileInfo, key inodeKey, id ident.ID) error {
	if canHoldAttr(info.Mode()) {
		if err := writeAttr(name, idAttr, uint64(id)); !errors.Is(err, errNoAttr) {
			return err
		}
	}
	return s.table.set(key.inode, id)
}

// persist makes host file name, which the overlay just made, back the entry
// with identity id. 0 makes it a new entry: a new Synthetic identity where
// the volume reuses inode numbers, else its own inode's, dropping whatever a
// clone copied or a removed file left.
func (s *idStore) persist(name string, id ident.ID) error {
	info, err := os.Lstat(name)
	if err != nil {
		return err
	}
	key, err := inodeOf(name, info)
	if err != nil {
		return err
	}
	if id == 0 && s.fresh {
		if id, err = s.syn.allocate(); err != nil {
			return err
		}
	}
	if id == 0 {
		s.known.forget(key)
		if canHoldAttr(info.Mode()) {
			if err := removeAttr(name, idAttr); err != nil {
				return err
			}
		}
		return s.table.drop(key.inode)
	}
	if err := s.write(name, info, key, id); err != nil {
		return err
	}
	s.known.remember(key, id)
	return nil
}

// created gives host file name, an entry the overlay just made from nothing
// (no copy), its identity: a new Synthetic one where the volume reuses inode
// numbers, so that it cannot take a removed file's; elsewhere its own inode's,
// at no cost.
func (s *idStore) created(name string) error {
	if !s.fresh {
		return nil
	}
	return s.persist(name, 0)
}

// release forgets the identity of the upper entry info describes before the
// overlay removes its last name, so that the next file the host gives its
// inode number starts afresh.
func (s *idStore) release(info os.FileInfo) error {
	key, ok := hostKey(info)
	if !ok {
		return nil
	}
	s.known.forget(key)
	if info.IsDir() || hardLinks(info) <= 1 {
		return s.table.drop(key.inode)
	}
	return nil
}

// discard removes staged host file name, which never became an entry, with
// any identity the table recorded for it.
func (s *idStore) discard(name string) {
	if info, err := os.Lstat(name); err == nil {
		if key, err := inodeOf(name, info); err == nil {
			s.known.forget(key)
			_ = s.table.drop(key.inode)
		}
	}
	_ = os.RemoveAll(name)
}

// stored returns the identity persisted for host file name; 0 when none is.
func (s *idStore) stored(name string) (ident.ID, error) {
	info, err := os.Lstat(name)
	if err != nil {
		return 0, err
	}
	key, err := inodeOf(name, info)
	if err != nil {
		return 0, err
	}
	return s.persisted(name, info, key)
}

// assignNew gives upper file name, described by its Lstat info, with inode key
// and no persisted identity a new Synthetic one: an entry made before its
// volume's entries all got one, a crash's leftover, or an inode too large
// for an identity. Concurrent readers may describe the file at once, so it
// checks again under s.assign.
func (s *idStore) assignNew(name string, info os.FileInfo, key inodeKey) (ident.ID, error) {
	s.assign.Lock()
	defer s.assign.Unlock()
	if id, known := s.known.lookup(key); known && id != 0 {
		return id, nil
	}
	if id, err := s.persisted(name, info, key); err != nil || id != 0 {
		return id, err
	}
	id, err := s.syn.allocate()
	if err != nil {
		return 0, err
	}
	return id, s.write(name, info, key, id)
}

// identities caches the identities persisted on an upper's files by backing
// inode, so a stat costs no attribute read once its inode was seen. A zero
// value records that none is persisted. The overlay is its upper's only
// writer: it forgets an inode whenever it removes a name of it, so a number
// the host hands out again is read afresh.
type identities struct {
	mu    sync.Mutex
	known map[inodeKey]ident.ID
}

// lookup returns the cached identity of key; known is false on a miss.
func (c *identities) lookup(key inodeKey) (id ident.ID, known bool) {
	c.mu.Lock()
	id, known = c.known[key]
	c.mu.Unlock()
	return id, known
}

// remember caches id, or its absence, for key.
func (c *identities) remember(key inodeKey, id ident.ID) {
	c.mu.Lock()
	if c.known == nil {
		c.known = make(map[inodeKey]ident.ID)
	}
	c.known[key] = id
	c.mu.Unlock()
}

// forget drops key after the overlay removed a name of its inode.
func (c *identities) forget(key inodeKey) {
	c.mu.Lock()
	delete(c.known, key)
	c.mu.Unlock()
}

// reset drops everything, as [FS.Cool] drops reconstructible metadata.
func (c *identities) reset() {
	c.mu.Lock()
	c.known = nil
	c.mu.Unlock()
}

// synthetic hands out an upper's [ident.Synthetic] identities (memory files,
// new entries where inode numbers are reused, inodes too large for an
// identity). The values never repeat for a
// checkout, across restarts and freezes: the upper's root directory persists
// a high-water mark in blocks, a frozen layer keeps the mark of the upper it
// was, and a new upper starts above every layer's.
type synthetic struct {
	mu             sync.Mutex
	root           string // the upper directory holding the mark
	next, reserved uint64
}

// newSynthetic continues numbering after the upper root's persisted mark and
// every mark of layers.
func newSynthetic(root string, layers []*Layer) (*synthetic, error) {
	mark, err := readAttr(root, nextAttr)
	if err != nil {
		return nil, err
	}
	for _, l := range layers {
		mark = max(mark, l.synthetic)
	}
	mark = max(mark, 1)
	return &synthetic{root: root, next: mark, reserved: mark}, nil
}

// allocate returns a new Synthetic identity, first persisting a new mark when
// the reserved block is used up.
func (s *synthetic) allocate() (ident.ID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.next >= syntheticLimit {
		return 0, errors.New("overlay: synthetic identities exhausted")
	}
	if s.next >= s.reserved {
		mark := min(s.next+syntheticBlock, syntheticLimit)
		if err := writeAttr(s.root, nextAttr, mark); err != nil && !errors.Is(err, errNoAttr) {
			return 0, err
		}
		s.reserved = mark
	}
	id, _ := ident.New(ident.Synthetic, s.next)
	s.next++
	return id, nil
}

// allocateRun returns the first of up to n consecutive new Synthetic
// identities and how many there are, persisting a mark past all of them
// first, so they are never handed out again, after a restart too.
func (s *synthetic) allocateRun(n uint64) (ident.ID, uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n = min(n, syntheticLimit-s.next)
	if n == 0 {
		return 0, 0, errors.New("overlay: synthetic identities exhausted")
	}
	if s.next+n > s.reserved {
		mark := min(s.next+n+syntheticBlock, syntheticLimit)
		if err := writeAttr(s.root, nextAttr, mark); err != nil && !errors.Is(err, errNoAttr) {
			return 0, 0, err
		}
		s.reserved = mark
	}
	id, _ := ident.New(ident.Synthetic, s.next)
	s.next += n
	return id, n, nil
}

// issued reports whether Synthetic identity id was handed out already, by
// this allocator or one before it on the same upper or its layers.
func (s *synthetic) issued(id ident.ID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return id.Space() == ident.Synthetic && id.Value() < s.next
}

// mark returns the high-water mark a layer frozen from this upper keeps.
func (s *synthetic) mark() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reserved
}

// readID returns the identity persisted on host file name; 0 when none is.
func readID(name string) (ident.ID, error) {
	v, err := readAttr(name, idAttr)
	if err != nil || v == 0 {
		return 0, err
	}
	id := ident.ID(v)
	if _, ok := ident.New(id.Space(), id.Value()); !ok || uint64(id)>>ident.Bits != 0 {
		return 0, &os.PathError{Op: "identity", Path: name, Err: errors.New("overlay: malformed persisted identity")}
	}
	return id, nil
}

// describe returns the metadata the overlay reports for host file name, of
// which info is a fresh Lstat: with its stored mode (Windows) and its
// entry's identity. s is the store of the directory holding the file (nil:
// attributes only). An upper's store gives a file without identity a new one
// where its volume reuses inode numbers, or its inode does not fit one.
func describe(name string, info os.FileInfo, s *idStore) (os.FileInfo, error) {
	info, err := withMode(name, info)
	if err != nil {
		return nil, err
	}
	key, err := inodeOf(name, info)
	if err != nil {
		return nil, err
	}
	var id ident.ID
	known := false
	if s != nil {
		id, known = s.known.lookup(key)
	}
	if !known {
		if id, err = s.persisted(name, info, key); err != nil {
			return nil, err
		}
		if _, fits := ident.New(ident.State, stateValue(key)); id == 0 && s != nil && s.syn != nil && (s.fresh || !fits) {
			if id, err = s.assignNew(name, info, key); err != nil {
				return nil, err
			}
			// Persisting the identity wrote to the file where it is an
			// attribute, which moved its change time past info's.
			if info, err = os.Lstat(name); err != nil {
				return nil, err
			}
			if info, err = withMode(name, info); err != nil {
				return nil, err
			}
		}
		if s != nil {
			s.known.remember(key, id)
		}
	}
	if id == 0 {
		if nativeIdentity {
			return info, nil
		}
		id, _ = ident.New(ident.State, stateValue(key))
	}
	return idInfo{FileInfo: info, key: key, id: id}, nil
}

// statHost is [os.Lstat] of host file name reported by [describe].
func statHost(name string, s *idStore) (os.FileInfo, error) {
	info, err := os.Lstat(name)
	if err != nil {
		return nil, err
	}
	return describe(name, info, s)
}

// inherit returns the identity an upper copy made at p takes from source s;
// 0 leaves the copy its own identity. A frozen file with other hard links
// keeps its identity under them. A bottom whose names of one file share an
// identity (base gives each name of a folder file its own, except on
// Windows) parts them here: the first copy takes the shared ID, and a later
// copy gets its own, even if the first was frozen in between.
// The caller holds f.mu with the index loaded.
func (f *FS) inherit(s source, p string, info os.FileInfo) ident.ID {
	id, _ := ident.Of(info)
	if id == 0 || info.IsDir() || hardLinks(info) < 2 {
		return id
	}
	// Copy-up parts this name from the other hard links. A cached path for
	// their shared ID may no longer be the one that serves its bytes.
	f.names.Add(1)
	if s.layer != nil {
		return 0
	}
	if holder, held := f.paths[id]; held && holder != p {
		return 0
	}
	if _, held := f.locateLayers(id); held {
		return 0
	}
	return id
}
