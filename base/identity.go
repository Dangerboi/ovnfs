package base

import (
	"os"

	"github.com/stencil-hq/ovnfs/internal/ident"
	"github.com/stencil-hq/ovnfs/internal/unixstat"
)

// Every entry a base serves carries its identity (see package ident): a
// captured entry, source or root, its Capture identity, the number of its
// capture index node, whether the folder's file or the captured blob serves
// it; an entry below a root, the State identity of the file serving it, which
// is the cache snapshot's copy below a local root and the folder's own file
// below a cache root.

// captureID is the identity of capture index node n.
func captureID(n uint32) ident.ID {
	id, _ := ident.New(ident.Capture, uint64(n))
	return id
}

// hostInfo is the folder's own metadata of a captured entry it still holds
// unchanged, source or root, carrying the entry's Capture identity instead
// of the folder inode's.
type hostInfo struct {
	os.FileInfo
	id ident.ID
}

// Identity returns the entry's Capture identity.
func (i hostInfo) Identity() ident.ID { return i.id }

// A folder file with several names below a cache or local root (cargo links
// an artifact into deps/ and beside it) gives each name its own
// [ident.Named] identity instead of its inode's. A client names a file by
// identity alone (an NFS handle), and the first write through one name parts
// it from the others (overlay.FS.inherit), so that write must reach the name
// it came through. A snapshot remembers the names it handed out, until it is
// released, for [bottom.Locate].

// lookupInfo describes the folder's entry at view path p below a cache or
// local root, from its lstat data info: with p's Named identity when it is a
// file of several names, else as the locator does (host: see there).
func (c *Caches) lookupInfo(info os.FileInfo, host, p string) os.FileInfo {
	if hardLinked(info) {
		if id, ok := c.name(p); ok {
			return hostInfo{FileInfo: info, id: id}
		}
	}
	return c.locator.lookupInfo(info, host, p)
}

// listedInfo is [Caches.lookupInfo] for an entry of a listing of view
// directory dir.
func (c *Caches) listedInfo(info os.FileInfo, host, dir string) os.FileInfo {
	if hardLinked(info) {
		if id, ok := c.name(dir + "/" + info.Name()); ok {
			return hostInfo{FileInfo: info, id: id}
		}
	}
	return c.locator.listedInfo(info, host, dir)
}

// hardLinked reports whether info describes a file with several names.
// Windows reports no link count, so its names keep sharing the inode's.
func hardLinked(info os.FileInfo) bool {
	st, ok := unixstat.Of(info)
	return ok && st.Nlink > 1 && !info.IsDir()
}

// name returns the Named identity of view path p and remembers p as its
// holder; false when another path holds it (a hash collision), which leaves
// p its inode's identity.
func (c *Caches) name(p string) (ident.ID, bool) {
	id := ident.Named(pathHash(p))
	c.namesMu.RLock()
	q, known := c.names[id]
	c.namesMu.RUnlock()
	if known {
		return id, q == p
	}
	c.namesMu.Lock()
	defer c.namesMu.Unlock()
	if q, known = c.names[id]; known {
		return id, q == p
	}
	if c.names == nil {
		c.names = make(map[ident.ID]string)
	}
	c.names[id] = p
	return id, true
}

// named returns the view path Named identity id was handed out for.
func (c *Caches) named(id ident.ID) (string, bool) {
	c.namesMu.RLock()
	defer c.namesMu.RUnlock()
	p, ok := c.names[id]
	return p, ok
}

// nodePath returns the path of capture index node v, when it is one.
func (b *Base) nodePath(v uint64) (string, bool) {
	if v >= uint64(b.index.nodes) {
		return "", false
	}
	return b.index.pathOf(uint32(v)), true
}
