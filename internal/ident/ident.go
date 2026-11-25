// Package ident defines the identity of a checkout entry: the number a file
// keeps for its whole life in a checkout, whichever level of the overlay
// currently holds it.
//
// An identity comes from the backing store, never from the entry's path:
//
//   - [Capture]: an entry of the base's capture, numbered by its capture.idx
//     node, whether its bytes come from the folder or from a blob. Values
//     above every node number are [Named] identities.
//   - [State]: a file or directory on the state volume (upper, frozen layers,
//     cache snapshots), numbered by its own inode.
//   - [Synthetic]: an entry with no usable inode, such as a file held in
//     memory, numbered by a per-checkout counter.
//
// Copy-up, spill, freeze and rename carry an entry's identity to its new
// backing file (the overlay persists it beside the file), so the inode a
// client sees never changes when it first writes a lower file. The export
// packs an identity with the checkout's slot into the protocol file id; see
// export.Nodes.
package ident

import "os"

// ID is a backing-store identity: a [Space] and a value of [ValueBits] bits.
// The zero ID is no identity.
type ID uint64

// Space says where an identity's value comes from.
type Space uint8

// Identity spaces. A checkout's entries never share an ID: capture node
// numbers are unique within the base, inodes within the state volume, and
// synthetic values within the checkout.
const (
	None Space = iota
	Capture
	State
	Synthetic
)

// ValueBits bounds an identity's value, so that an ID with a 12-bit checkout
// slot fits the 53 bits JavaScript runtimes represent exactly.
const ValueBits = 39

// spaceBits holds a [Space].
const spaceBits = 2

// MaxValue is the largest value an ID can carry.
const MaxValue = 1<<ValueBits - 1

// Bits is the width of every ID.
const Bits = ValueBits + spaceBits

// New returns the identity of value v in space s; false when v does not fit
// [ValueBits] or s is [None].
func New(s Space, v uint64) (ID, bool) {
	if s == None || s > Synthetic || v > MaxValue {
		return 0, false
	}
	return ID(uint64(s)<<ValueBits | v), true
}

// NTFSRecord is the file record number of an NTFS file reference. Its high
// 16 bits identify the incarnation of that record, not a different record.
// Only the record number is a State identity's value; callers comparing
// host files for replacement must still compare the full reference.
func NTFSRecord(ref uint64) uint64 { return ref & (1<<48 - 1) }

// Space returns the identity's space; [None] for the zero ID.
func (id ID) Space() Space { return Space(id >> ValueBits) }

// Value returns the identity's value within its space.
func (id ID) Value() uint64 { return uint64(id) & MaxValue }

// Identifier is implemented by file information that knows its entry's
// identity: base entries (a [Capture] identity whatever file serves them),
// memory files, and backing files whose persisted identity differs from
// their inode. Wrappers of such information must implement it too, by
// forwarding [Of] of what they wrap.
type Identifier interface {
	Identity() ID
}

// namedFirst is the first [Capture] value of a [Named] identity: capture
// node numbers are 32-bit.
const namedFirst = 1 << 32

// Named returns the identity of a folder file with several names below a
// cache or local root, from hash, a hash of the name's view path. Each name
// is its own entry in a checkout, since a copy-up parts it from the others,
// so a client writing through one name's handle reaches that name. Two paths
// whose hashes collide share an identity; callers detect that.
func Named(hash uint64) ID {
	return ID(uint64(Capture)<<ValueBits | (namedFirst + hash%(MaxValue+1-namedFirst)))
}

// IsNamed reports whether id is a [Named] identity.
func (id ID) IsNamed() bool { return id.Space() == Capture && id.Value() >= namedFirst }

// Hosted reports whether id identifies a host file served as it is: a
// [State] inode or a [Named] folder file. Such a file can leave a cache
// root's fenced view while clients still hold its identity.
func (id ID) Hosted() bool { return id.Space() == State || id.IsNamed() }

// Of returns the identity of the entry info describes: its [Identifier]
// identity, else the [State] identity of the inode in its native stat. Only
// files on the state volume may reach Of without an [Identifier]; anything
// else must carry its identity explicitly. False when info has neither or the
// inode does not fit.
func Of(info os.FileInfo) (ID, bool) {
	if i, ok := info.(Identifier); ok {
		id := i.Identity()
		return id, id != 0
	}
	ino, ok := inode(info)
	if !ok {
		return 0, false
	}
	return New(State, ino)
}

// Inode is implemented by native stat records that carry an inode number
// where the platform's own does not (Windows file information, see
// overlay's sys_windows.go).
type Inode interface {
	Inode() uint64
}
