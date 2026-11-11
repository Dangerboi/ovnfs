package export

import (
	"crypto/rand"
	"io"
	"os"
	"path/filepath"
)

// openColdStore detaches the entire previous store without visiting its files.
// The state lock excludes other users of coldParent. The discard directory is
// separate: even a cleaner still returning from I/O after close cannot reach
// the next instance's authoritative owners. Windows never renames an open
// index: readers belong to the previous, already closed export.
func (t *nodeTable) openColdStore() error {
	discard := t.coldParent + ".discard"
	if err := os.MkdirAll(discard, 0700); err != nil {
		return err
	}
	// Publish the detached tree with one rename. No empty container is
	// exposed to an older cleaner still returning from a filesystem call.
	old := filepath.Join(discard, "store-"+rand.Text())
	if err := os.Rename(t.coldParent, old); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(t.coldParent, 0700); err != nil {
		return err
	}
	t.coldStop = make(chan struct{})
	go func() {
		// Errors leave garbage for the next open; they cannot invalidate
		// any live owner or turn a cache cleanup failure into a serving fault.
		reclaimCold(discard, t.coldStop, t.reclaimIO)
	}()
	return nil
}

// reclaimCold removes detached stores in bounded batches. It leaves its root
// in place so an older worker cannot remove a newer opener's discard directory.
// A stop or crash leaves the remaining entries for the next open. Directory
// streams close before their directory is removed, including on Windows.
func reclaimCold(root string, stop <-chan struct{}, beforeRemove func()) error {
	for {
		select {
		case <-stop:
			return nil
		default:
		}
		f, err := os.Open(root)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		for {
			names, err := f.Readdirnames(128)
			if err != nil && err != io.EOF {
				f.Close()
				return err
			}
			removed := 0
			for _, name := range names {
				if beforeRemove != nil {
					beforeRemove()
				}
				select {
				case <-stop:
					f.Close()
					return nil
				default:
				}
				path := filepath.Join(root, name)
				info, err := os.Lstat(path)
				if os.IsNotExist(err) {
					removed++
					continue
				}
				if err != nil {
					continue // retry inaccessible garbage on a later open
				}
				if info.IsDir() {
					if err := reclaimCold(path, stop, beforeRemove); err != nil {
						continue
					}
				}
				// Never follow a symlink; removal failures remain garbage.
				if err := os.Remove(path); err == nil || os.IsNotExist(err) {
					removed++
				}
			}
			if len(names) < 128 {
				f.Close()
				return nil
			}
			if removed > 0 {
				// Deletion can reshuffle directory offsets. Reopen after
				// progress so the next batch cannot skip surviving entries.
				f.Close()
				break
			}
			// A full batch of failures must advance, not reopen and spin.
		}
	}
}
