package checkout

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/git"
	"github.com/stencil-hq/ovnfs/rules"
	"github.com/stencil-hq/ovnfs/store"
)

// landFiles separates the commit range being integrated from the live files
// being merged. A replay supplies its own worktree; ordinary land still takes
// the checkout's working version of paths changed by its commits.
type landFiles struct {
	ctx                 context.Context
	changed             []git.BranchChange
	paths               map[string]bool
	old, captured, head map[string]store.File
	adjusted            map[string][]byte
	readLive            func(string, store.OID) ([]byte, error)
	replayed            bool
	worktree            string
}

func (l *landFiles) includes(p string) bool { return l.paths[p] }

func (l *landFiles) plan(h *checkoutRecord, from, to map[string]store.File, bases folderBases) ([]api.Change, error) {
	l.readLive = h.base.view.Content
	changes := make([]api.Change, 0, len(l.changed))
	for _, change := range l.changed {
		p := change.Path
		if l.replayed {
			for q := p; q != "."; q = path.Dir(q) {
				if class, known := h.base.view.Class(q); known && class != rules.Source {
					return nil, failure("unsupported", "cannot land a replay touching %s: %s is %s in the checkout", p, q, class)
				}
			}
			name := filepath.Join(l.worktree, filepath.FromSlash(p))
			f, _, err := leafAt(name)
			if err != nil {
				return nil, err
			}
			to[p] = f
		}
		if _, marked := bases.marked[p]; !marked && (l.replayed || l.captured != nil && l.captured[p] != l.old[p]) {
			if err := l.moveBase(h, p, from); err != nil {
				return nil, err
			}
		}
		if to[p].Mode == store.ModeDir {
			delete(to, p)
		}
		if from[p] != to[p] {
			changes = append(changes, api.Change{Path: p, Status: change.Status})
		} else if _, marked := bases.marked[p]; !marked && !l.replayed {
			bases.afterApply(p, to[p], skipped)
		}
	}
	return changes, nil
}

// uncommitted lists, sorted, the checkout's source paths whose version now
// differs from its Git HEAD (l.head): landed holds those the commits being
// landed change, where land would merge the difference along with them, and
// left the others. A replay lists every difference as left; otherwise two are
// not the checkout's own: off the commits' paths, a version the folder already
// holds, and on them, a pull's merge of the folder's edits, which land carries
// back to the folder uncommitted.
func (r *Registry) uncommitted(h *checkoutRecord, l *landFiles) (landed, left []string, err error) {
	live, err := r.liveTree(h)
	if err != nil {
		return nil, nil, err
	}
	base, err := h.base.view.Store().Flatten(h.base.view.Meta().Tree)
	if err != nil {
		return nil, nil, err
	}
	bases, err := r.folderBases(h)
	if err != nil {
		return nil, nil, err
	}
	bases.over(base, true)
	own := func(p string, f store.File) bool {
		switch {
		case l.replayed:
			return true
		case l.includes(p):
			_, pulled := bases.pulled[p]
			_, marked := bases.markedHere[p]
			return !pulled && !marked
		}
		return f != base[p]
	}
	note := func(p string) {
		if l.includes(p) && !l.replayed {
			landed = append(landed, p)
		} else {
			left = append(left, p)
		}
	}
	for p, f := range live.files {
		if f.Mode == store.ModeDir || f == l.head[p] || !own(p, f) {
			continue
		}
		equal, err := l.sameWorktreeBlob(h, p, f, l.head[p], func() ([]byte, error) { return r.content(h, f, live.supplier(p)) })
		if err != nil {
			return nil, nil, err
		}
		if !equal {
			note(p)
		}
	}
	for p := range l.head {
		// A path deliberately omitted by the projection isn't a deletion.
		if f := live.files[p]; (f.ID == "" || f.Mode == store.ModeDir) && base[p].ID != "" && own(p, f) {
			note(p)
		}
	}
	slices.Sort(landed)
	slices.Sort(left)
	return landed, left, nil
}

// pathList names up to five paths for a message, counting the rest.
func pathList(paths []string) string {
	const shown = 5
	if len(paths) <= shown {
		return strings.Join(paths, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(paths[:shown], ", "), len(paths)-shown)
}

func (l *landFiles) sameWorktreeBlob(h *checkoutRecord, p string, working, committed store.File, read func() ([]byte, error)) (bool, error) {
	if working == committed {
		return true, nil
	}
	if !regular(working) || working.Mode != committed.Mode {
		return false, nil
	}
	if h.base.view.LFSLeaf(p, committed.ID) {
		data, err := h.base.view.Content(p, committed.ID)
		return err == nil && blobID(data) == working.ID, err
	}
	data, err := read()
	if err != nil {
		return false, err
	}
	if !bytes.Contains(data, []byte("\r\n")) {
		return false, nil
	}
	c, err := newEOLConverter(l.ctx, h.base.view, []string{p})
	if err != nil {
		return false, nil // we cannot prove that this file is clean
	}
	defer c.Close()
	f, _, err := c.blob(p, working, data)
	return f == committed, err
}

// moveBase uses the target's committed contents for paths changed since the
// capture. If they match the destination after line ending conversion, use
// the actual working bytes so the ordinary merge sees a clean file.
func (l *landFiles) moveBase(h *checkoutRecord, p string, from map[string]store.File) error {
	old := l.old[p]
	if old.ID == "" {
		delete(from, p)
		return nil
	}
	from[p] = old
	if !regular(old) {
		return nil
	}
	name := filepath.Join(h.base.view.Meta().Dir, filepath.FromSlash(p))
	ours, _, err := leafAt(name)
	if err != nil || ours == old {
		return err
	}
	var data []byte
	same, err := l.sameWorktreeBlob(h, p, ours, old, func() ([]byte, error) {
		data, err = os.ReadFile(name)
		return data, err
	})
	if err == nil && same && data == nil {
		data, err = os.ReadFile(name)
	}
	if err == nil && same && blobID(data) == ours.ID {
		if l.adjusted == nil {
			l.adjusted = make(map[string][]byte)
		}
		l.adjusted[p], from[p] = data, ours
	}
	return err
}

func (l *landFiles) readBase(p string, id store.OID) ([]byte, error) {
	if data := l.adjusted[p]; data != nil {
		return data, nil
	}
	return l.readLive(p, id)
}

func (l *landFiles) readTheirs(p string, id store.OID) ([]byte, error) {
	if !l.replayed {
		return l.readLive(p, id)
	}
	name := filepath.Join(l.worktree, filepath.FromSlash(p))
	info, err := os.Lstat(name)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(name)
		return []byte(filepath.ToSlash(target)), err
	}
	return os.ReadFile(name)
}

// gitLeaves projects the regular and symlink entries Git tracks onto the
// merge's file representation; Git links have no file payload to merge.
func gitLeaves(ctx context.Context, repo *git.Repo, rev string) (map[string]store.File, error) {
	entries, err := repo.LsTree(ctx, rev)
	if err != nil {
		return nil, err
	}
	files := make(map[string]store.File, len(entries))
	var objects *git.Objects
	for _, e := range entries {
		if e.Type != "blob" {
			continue
		}
		id := store.OID(e.OID)
		if len(e.OID) != 40 {
			if objects == nil {
				objects, err = repo.Objects()
				if err != nil {
					return nil, err
				}
				defer objects.Close()
			}
			data, err := objects.Blob(e.OID)
			if err != nil {
				return nil, err
			}
			id = blobID(data)
		}
		files[e.Path] = store.File{ID: id, Mode: e.Mode}
	}
	return files, nil
}
