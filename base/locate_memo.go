//go:build linux || windows

package base

import "sync/atomic"

// memoBits sets how many identities a memo remembers, 1<<memoBits: a quarter
// of a megabyte of slots once used, and the paths behind them.
const memoBits = 15

// memo remembers the view paths of State identities a snapshot handed out,
// by inode. It is direct-mapped: each inode has one slot, a newer one there
// replaces an older one, and readers take no lock.
type memo struct {
	slots atomic.Pointer[[1 << memoBits]atomic.Pointer[located]]
}

// located is one observed name of ino. Different names of one inode are
// hard links; keep all of them until another inode takes the slot.
type located struct {
	ino  uint64
	path string
	next *located
}

// slot returns where ino's entry goes; nil while nothing was ever put,
// unless make is set. Inodes of one directory are often consecutive, so a
// multiplicative hash spreads them.
func (m *memo) slot(ino uint64, make bool) *atomic.Pointer[located] {
	slots := m.slots.Load()
	if slots == nil {
		if !make {
			return nil
		}
		m.slots.CompareAndSwap(nil, new([1 << memoBits]atomic.Pointer[located]))
		slots = m.slots.Load()
	}
	return &slots[(ino*0x9e3779b97f4a7c15)>>(64-memoBits)]
}

// put remembers that the entry with inode ino is at view path p.
func (m *memo) put(ino uint64, p string) {
	slot := m.slot(ino, true)
	for {
		old := slot.Load()
		var next *located
		if old != nil && old.ino == ino {
			for e := old; e != nil; e = e.next {
				if e.path == p {
					return
				}
			}
			next = old
		}
		if slot.CompareAndSwap(old, &located{ino: ino, path: p, next: next}) {
			return
		}
	}
}

// find tries each remembered name of ino until shown accepts one.
func (m *memo) find(ino uint64, shown func(string) bool) (string, bool) {
	if slot := m.slot(ino, false); slot != nil {
		for e := slot.Load(); e != nil && e.ino == ino; e = e.next {
			if shown(e.path) {
				return e.path, true
			}
		}
	}
	return "", false
}
