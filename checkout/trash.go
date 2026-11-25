package checkout

import (
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/stencil-hq/ovnfs/internal/lowprio"
)

// trashDir holds the state trees the registry gave up, until emptyTrash has
// deleted them. Giving a tree up costs one rename however many files it
// holds, so neither a start nor garbage collection waits for a deletion.
const trashDir = "trash"

// trashBacklog is how many discarded trees may wait for emptyTrash: those of
// eight removed checkouts, an upper and a git directory each. Its thread gets
// the processor time nothing else wants, which on a saturated machine is
// little: deleting 150,000 files took 7 minutes under a load of 45 on 16
// cores, and 27 seconds under a load of 15. Past this many, discard has
// relieveTrash delete at normal priority, so the trash stays bounded.
const trashBacklog = 16

// discard gives up the state tree at path, named label in the trash: it moves
// the tree there for emptyTrash, and deletes it in place only where it cannot
// be moved. A tree that can be neither moved nor deleted is the error.
func (r *Registry) discard(path, label string) error {
	dir := filepath.Join(r.config.State, trashDir)
	if os.Rename(path, filepath.Join(dir, label+"-"+rand.Text())) != nil {
		return removeTree(path)
	}
	select {
	case r.trashWake <- nil:
	default:
	}
	if waiting, _ := os.ReadDir(dir); len(waiting) > trashBacklog && r.relieving.CompareAndSwap(false, true) {
		r.trash.Go(func() { r.relieveTrash(dir, waiting[trashBacklog:]) })
	}
	return nil
}

// emptyTrash deletes what discard moved aside, and what an earlier daemon
// stopped before deleting. A layer or an orphaned upper may hold a build's
// whole output, hundreds of thousands of files, each an unlink that no call
// can batch, so the deletions run on one thread of the lowest priority the
// OS offers (see [lowprio.Thread]) instead of keeping a core from the builds
// the daemon serves. Close does not wait for it: what remains when the
// daemon exits is deleted at the next open.
func (r *Registry) emptyTrash() {
	// Never unlocked: the demoted thread ends with this goroutine.
	runtime.LockOSThread()
	if err := lowprio.Thread(); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		r.enqueueNotice(baseNotice{fault: "lower the priority of deletions: " + err.Error()})
	}
	dir := filepath.Join(r.config.State, trashDir)
	var done chan struct{}
	for {
		entries, err := os.ReadDir(dir)
		if err != nil {
			r.enqueueNotice(baseNotice{fault: "list trash: " + err.Error()})
		}
		for _, entry := range entries {
			select {
			case <-r.stop:
				return
			default:
			}
			job := r.config.Stats.ThreadJob("trash.empty", trashLabel(entry.Name()))
			r.removeTrashed(dir, entry.Name())
			job.End()
		}
		if done != nil {
			close(done)
		}
		select {
		case <-r.stop:
			return
		case done = <-r.trashWake:
		}
	}
}

// removeTrashed reports a tree the reaper could not delete; a later pass retries it.
func (r *Registry) removeTrashed(dir, name string) {
	if err := removeTree(filepath.Join(dir, name)); err != nil {
		r.enqueueNotice(baseNotice{fault: "empty trash: " + err.Error()})
	}
}

// trashLabel returns the label discard named a trash entry with: its name
// without the random suffix, which holds no "-".
func trashLabel(name string) string {
	if i := strings.LastIndexByte(name, '-'); i > 0 {
		return name[:i]
	}
	return name
}

// relieveTrash deletes excess, the trees of trash directory dir waiting
// beyond trashBacklog, at normal priority and last first, so it meets
// emptyTrash coming the other way. Both may delete in one tree at once: each
// entry goes once, whoever unlinks it, and neither minds the other's.
func (r *Registry) relieveTrash(dir string, excess []os.DirEntry) {
	defer r.relieving.Store(false)
	for _, entry := range slices.Backward(excess) {
		select {
		case <-r.stop:
			return
		default:
		}
		job := r.config.Stats.Job("trash.relieve", trashLabel(entry.Name()))
		r.removeTrashed(dir, entry.Name())
		job.End()
	}
}
