package checkout

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/stencil-hq/ovnfs/base"
	"github.com/stencil-hq/ovnfs/internal/durable"
	"github.com/stencil-hq/ovnfs/overlay"
)

// stateVersion is the registry manifest layout this build writes. Builds that
// read only version 4 truncate store packs at frame kinds they do not know
// (chunks, chunk manifests, tree deltas); version 5 makes them refuse the state
// before they open the store.
const stateVersion = 5

// oldestStateVersion is the oldest layout this build reads; version 4 is a
// subset of version 5, rewritten as 5 when the registry opens.
const oldestStateVersion = 4

// diskState is the versioned registry manifest; all referenced paths live below State.
type diskState struct {
	Version      int            `json:"version"`
	Proto        string         `json:"proto"`
	Port         int            `json:"port"`
	NextSnapshot uint64         `json:"next_snapshot"`
	Bases        []base.Meta    `json:"bases"`
	Checkouts    []diskCheckout `json:"checkouts"`
	Snapshots    []diskSnapshot `json:"snapshots"`
	Layers       []diskLayer    `json:"layers"`
	Intents      []diskIntent   `json:"intents"`
}

// diskCheckout records a checkout; Head or Parent identifies its immutable stack, and
// Stack is written only when that snapshot's stack does not rebuild it. Wrote is the
// last change through the checkout's folder (checkoutRecord.wrote) as of this write:
// file calls do not write state, so a crash loses the ones since.
type diskCheckout struct {
	checkoutMeta
	Stack []string  `json:"stack,omitempty"`
	Wrote time.Time `json:"wrote,omitzero"`
}

// diskSnapshot records one layer and its snapshot ancestry. Stack is rebuilt at open
// from Layer and StackParent, so it is written only when that would rebuild it wrong.
// Git is the stored tree of the snapshot's private git directory; a git snapshot
// without one keeps a copied directory in snapshots/<id>/git instead.
type diskSnapshot struct {
	snapMeta
	Base        string   `json:"base"`
	Layer       string   `json:"layer,omitempty"`
	StackParent string   `json:"stack_parent,omitempty"`
	Dropped     bool     `json:"dropped,omitempty"`
	Stack       []string `json:"stack,omitempty"`
	Git         string   `json:"git,omitempty"`
}

// diskLayer binds a layer to its base. Metadata, when present, replaces the
// stored layer's manifest file (see embedLimit).
type diskLayer struct {
	ID, Base string
	Portable bool   `json:"portable,omitempty"`
	Metadata []byte `json:"metadata,omitempty"`
}

// embedLimit bounds the compressed metadata of a stored layer that state.json
// carries instead of a manifest file. A small manifest would occupy a whole
// filesystem block of its own, but every registry change rewrites state.json
// with all embedded metadata, so larger manifests keep their file.
const embedLimit = 1 << 10

// embeddable reports whether a stored layer's metadata belongs in state.json.
func embeddable(l *overlay.Layer) bool {
	m := l.Metadata()
	return m != nil && len(m) <= embedLimit
}

// diskIntent makes an interrupted multi-step filesystem mutation recoverable. Dir
// is the checkout's checkoutMeta.Dir.
type diskIntent struct {
	Op, Checkout, Layer, Snapshot string
	Dir                           string `json:",omitempty"`
}

// dir names the directories below uppers and git of the intent's checkout.
func (i diskIntent) dir() string {
	if i.Dir != "" {
		return i.Dir
	}
	return i.Checkout
}

// stateLocked snapshots metadata while the registry mutex is held.
func (r *Registry) stateLocked() diskState {
	lineage := r.retainedLineageLocked()
	d := diskState{Version: stateVersion, Proto: r.proto, Port: r.port, NextSnapshot: r.nextSnapshot,
		Bases: []base.Meta{}, Checkouts: []diskCheckout{}, Snapshots: []diskSnapshot{}, Layers: []diskLayer{}, Intents: []diskIntent{}}
	for _, b := range r.bases {
		d.Bases = append(d.Bases, b.view.Meta())
	}
	for _, h := range r.checkouts {
		disk := diskCheckout{checkoutMeta: cloneCheckoutMeta(h.meta)}
		if n := h.wrote.Load(); n != 0 {
			disk.Wrote = time.Unix(0, n).UTC()
		}
		if s := lineage[checkoutSnapshot(h.meta)]; s == nil || !sameStack(s.stack, h.stack) {
			disk.Stack = append([]string(nil), h.stack...)
		}
		d.Checkouts = append(d.Checkouts, disk)
	}
	for _, s := range lineage {
		disk := diskSnapshot{snapMeta: cloneSnapMeta(s.meta), Base: s.base.view.Meta().ID, Layer: s.layer, StackParent: s.stackParent, Dropped: s.dropped, Git: string(s.git)}
		if !rebuildsStack(lineage, s) {
			disk.Stack = append([]string(nil), s.stack...)
		}
		d.Snapshots = append(d.Snapshots, disk)
	}
	for _, l := range r.layers {
		disk := diskLayer{ID: l.id, Base: l.base, Portable: l.portable}
		if l.embedded {
			disk.Metadata = l.layer.Metadata()
		}
		d.Layers = append(d.Layers, disk)
	}
	for _, intent := range r.intents {
		d.Intents = append(d.Intents, intent)
	}
	sort.Slice(d.Bases, func(i, j int) bool { return d.Bases[i].ID < d.Bases[j].ID })
	sort.Slice(d.Checkouts, func(i, j int) bool { return d.Checkouts[i].Name < d.Checkouts[j].Name })
	sort.Slice(d.Snapshots, func(i, j int) bool { return snapshotLess(d.Snapshots[i].ID, d.Snapshots[j].ID) })
	sort.Slice(d.Layers, func(i, j int) bool { return d.Layers[i].ID < d.Layers[j].ID })
	sort.Slice(d.Intents, func(i, j int) bool { return d.Intents[i].Checkout < d.Intents[j].Checkout })
	return d
}

// rebuildsStack reports whether restoreStacks rebuilds s's stack from its layer and
// its stack parent's stack, which lineage retains whenever s is retained.
func rebuildsStack(lineage map[string]*snapRecord, s *snapRecord) bool {
	var below []string
	if s.stackParent != "" {
		parent := lineage[s.stackParent]
		if parent == nil {
			return false
		}
		below = parent.stack
	}
	if s.layer == "" {
		return sameStack(s.stack, below)
	}
	return len(s.stack) == len(below)+1 && s.stack[0] == s.layer && sameStack(s.stack[1:], below)
}

// persistLocked commits all metadata with a file and directory durability barrier.
func (r *Registry) persistLocked() error {
	if err := r.writeOrdered(r.stateLocked(), r.writes.stamp.Add(1), true); err != nil {
		return err
	}
	r.pruneLineageLocked()
	return nil
}

// persistIntentLocked writes metadata atomically without a durability barrier: recovery
// intents published before filesystem changes, and snapshot tree ids, which reopening
// computes again when a crash lost them.
func (r *Registry) persistIntentLocked() error {
	return r.writeOrdered(r.stateLocked(), r.writes.stamp.Add(1), false)
}

// stateWrites orders state.json writes made with and without the registry lock:
// each snapshot gets a stamp while the lock is held, and a write never replaces
// the file with an older snapshot than it holds.
type stateWrites struct {
	stamp   atomic.Uint64 // the latest snapshot's
	mu      sync.Mutex    // held while writing
	written uint64        // mu: stamp of the snapshot in state.json
	durable bool          // mu: state.json passed a durability barrier since
}

// writeOrdered writes snapshot d, stamped stamp while r.mu was held, unless the
// file holds a newer one already; then a durable write only completes the newer
// one's barrier, since that snapshot holds every change d does.
func (r *Registry) writeOrdered(d diskState, stamp uint64, durable bool) error {
	w := &r.writes
	w.mu.Lock()
	defer w.mu.Unlock()
	if stamp < w.written {
		if !durable || w.durable {
			return nil
		}
		if err := syncState(r.config.State); err != nil {
			return err
		}
		w.durable = true
		return nil
	}
	if err := writeStateMode(r.config.State, d, durable); err != nil {
		return err
	}
	w.written, w.durable = stamp, durable
	return nil
}

// stateCommits coalesces durable state commits that callers request after
// releasing the registry lock: one write covers every request made before its
// snapshot, so concurrent creates share a single file and directory sync, and
// the registry lock is held only to take the snapshot.
type stateCommits struct {
	mu      sync.Mutex
	cond    sync.Cond // on mu; broadcast when a commit ends
	want    uint64    // the latest request
	done    uint64    // every request up to done is durable
	failed  uint64    // requests up to failed ended in err, unless done covers them
	err     error
	running bool
}

// requestCommitLocked registers a mutation made under the registry's write lock
// for commitState and returns its request.
func (r *Registry) requestCommitLocked() uint64 {
	c := &r.commits
	c.mu.Lock()
	defer c.mu.Unlock()
	c.want++
	return c.want
}

// commitState makes request seq durable, by its own write or one in flight that
// will cover it. The caller holds no registry lock.
func (r *Registry) commitState(seq uint64) error {
	c := &r.commits
	c.mu.Lock()
	defer c.mu.Unlock()
	for {
		switch {
		case c.done >= seq:
			return nil
		case c.failed >= seq:
			return c.err
		case c.running:
			c.cond.Wait()
			continue
		}
		c.running = true
		c.mu.Unlock()
		r.mu.RLock()
		c.mu.Lock()
		target := c.want
		c.mu.Unlock()
		d, stamp := r.stateLocked(), r.writes.stamp.Add(1)
		r.mu.RUnlock()
		err := r.writeOrdered(d, stamp, true)
		c.mu.Lock()
		c.running = false
		if err == nil {
			c.done = target
		} else {
			c.failed, c.err = target, err
		}
		c.cond.Broadcast()
	}
}

// syncState passes state.json, as it is, through the durability barrier.
func syncState(dir string) error {
	f, err := os.OpenFile(filepath.Join(dir, "state.json"), os.O_RDWR, 0)
	if err != nil {
		return err
	}
	err = durable.Sync(f)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return durable.Dir(dir)
}

// writeState atomically replaces state.json and makes it durable against process
// crashes, not power loss; Darwin uses fsync(2), not F_FULLFSYNC.
func writeState(dir string, d diskState) error { return writeStateMode(dir, d, true) }

// writeStateMode keeps intermediate recovery snapshots atomic and syncs only the
// operation's final commit. The barrier syncs the file after the rename, then the
// directory: on journaling filesystems the file's sync commits the rename with it,
// leaving the directory's sync next to free, where syncing before the rename costs
// two journal commits. Like every unsynced intent write, a kernel crash during the
// barrier may leave the new name without its data on filesystems that do not
// order data before renames (ext4's defaults do).
func writeStateMode(dir string, d diskState, syncCommit bool) error {
	data, err := json.Marshal(d)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".state-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0o600); err == nil {
		_, err = f.Write(append(data, '\n'))
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	final := filepath.Join(dir, "state.json")
	if err = os.Rename(name, final); err != nil || !syncCommit {
		return err
	}
	// Reopened rather than kept open: Windows cannot rename an open file.
	return syncState(dir)
}

// exists reports whether a path survived an interrupted operation.
func exists(path string) bool { _, err := os.Lstat(path); return err == nil }

// recoverIntents finishes freezes and removals and rolls back unpublished creates.
func recoverIntents(state string, d *diskState) error {
	for _, intent := range d.Intents {
		hi, si := -1, -1
		for i := range d.Checkouts {
			if d.Checkouts[i].Name == intent.Checkout {
				hi = i
				break
			}
		}
		for i := range d.Snapshots {
			if d.Snapshots[i].ID == intent.Snapshot {
				si = i
				break
			}
		}
		upper := filepath.Join(state, "uppers", intent.dir())
		gitdir := filepath.Join(state, "git", intent.dir())
		switch intent.Op {
		case "creating":
			if hi >= 0 {
				continue
			}
			if err := removeTree(upper); err != nil {
				return err
			}
			if err := removeTree(gitdir); err != nil {
				return err
			}
			if intent.Layer != "" && !baseUsed(d, intent.Layer) {
				if err := os.RemoveAll(filepath.Join(state, "bases", intent.Layer)); err != nil {
					return err
				}
				kept := d.Bases[:0]
				for _, b := range d.Bases {
					if b.ID != intent.Layer {
						kept = append(kept, b)
					}
				}
				d.Bases = kept
			}
		case "freezing":
			layerDir := filepath.Join(state, "layers", intent.Layer)
			frozen := intent.Layer != "" && exists(layerDir)
			// Git state is in the snapshot record itself, or in a directory copy by older builds.
			saved := hi >= 0 && (!d.Checkouts[hi].Git || (si >= 0 && d.Snapshots[si].Git != "") || exists(filepath.Join(state, "snapshots", intent.Snapshot, "git")))
			if hi >= 0 && si >= 0 && saved && (frozen || intent.Layer == "") {
				h := &d.Checkouts[hi]
				h.Head = copyString(&d.Snapshots[si].ID)
				h.Stack = append([]string(nil), d.Snapshots[si].Stack...)
				if frozen {
					if err := os.RemoveAll(filepath.Join(layerDir, ".wh..wh.work")); err != nil {
						return err
					}
					found := false
					for _, l := range d.Layers {
						found = found || l.ID == intent.Layer
					}
					if !found {
						d.Layers = append(d.Layers, diskLayer{ID: intent.Layer, Base: h.Base})
					}
				}
				if err := os.MkdirAll(upper, 0o755); err != nil {
					return err
				}
				if h.Git {
					if err := writeGitfile(upper, gitdir); err != nil {
						return err
					}
				}
			} else {
				if frozen && hi >= 0 {
					if err := removeTree(upper); err != nil {
						return err
					}
					if err := os.Rename(layerDir, upper); err != nil {
						return err
					}
				}
				if si >= 0 {
					d.Snapshots = append(d.Snapshots[:si], d.Snapshots[si+1:]...)
				}
				if err := os.RemoveAll(filepath.Join(state, "snapshots", intent.Snapshot)); err != nil {
					return err
				}
			}
		case "removing":
			if err := removeTree(gitdir); err != nil {
				return err
			}
			if err := removeTree(upper); err != nil {
				return err
			}
			if hi >= 0 {
				d.Checkouts = append(d.Checkouts[:hi], d.Checkouts[hi+1:]...)
			}
		default:
			return fmt.Errorf("unknown registry intent %q", intent.Op)
		}
	}
	d.Intents = nil
	return writeState(state, *d)
}

// baseUsed reports whether any checkout or snapshot still sits on a base.
func baseUsed(d *diskState, id string) bool {
	for _, h := range d.Checkouts {
		if h.Base == id {
			return true
		}
	}
	for _, s := range d.Snapshots {
		if s.Base == id {
			return true
		}
	}
	return false
}
