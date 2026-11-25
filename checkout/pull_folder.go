package checkout

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/git"
	"github.com/stencil-hq/ovnfs/internal/hostfile"
	"github.com/stencil-hq/ovnfs/source"
	"github.com/stencil-hq/ovnfs/store"
)

// PullFolder merges into a checkout what its folder changed since the
// checkout opened or last pulled: Apply in reverse, so a conflict between the
// two is resolved in the checkout, away from the user's files. Each path
// merges three-way against the version the two last agreed on (see
// folderBases): one the checkout left alone takes the folder's version, text
// changed on both sides is merged, with conflict markers in the checkout where
// the same lines changed, and everything else is a conflict left untouched. A
// marked path stays out of applies until its markers are gone.
//
// The folder's files as pulled become the base the checkout's own changes
// count from, for Diff and Apply alike. Only files are merged: the checkout's
// git state stays as it is, and the reply says so when the folder's HEAD has
// moved past it.
func (r *Registry) PullFolder(ctx context.Context, args api.PullArgs) (api.PullReply, error) {
	resume, err := r.Quiesce(ctx)
	if err != nil {
		return api.PullReply{}, err
	}
	defer resume()
	if err := checkContext(ctx); err != nil {
		return api.PullReply{}, err
	}
	h, err := r.lookupCheckout(args.Checkout)
	if err != nil {
		return api.PullReply{}, err
	}
	meta := h.base.view.Meta()
	switch {
	case meta.Remote != nil:
		return api.PullReply{}, failure("invalid", "checkout %s is attached from %s; its folder is not on this machine", h.meta.Name, meta.Remote.Source)
	case meta.Chosen:
		// Everything the folder holds beyond that start would count as newer,
		// the uncommitted edits --rev left out and the paths --take replaced included.
		return api.PullReply{}, failure("invalid", "checkout %s started from a revision or taken paths, not from the files of %s, so the folder's files are not newer versions of its own; to follow the folder's commits, inside the checkout: git fetch %s HEAD && git rebase --autostash FETCH_HEAD", h.meta.Name, meta.Dir, shellQuote(meta.Dir))
	}
	h.merge.Lock()
	defer h.merge.Unlock()
	// The folder is read between applies, never halfway through one.
	release, err := r.applying.lock(ctx, meta.Dir)
	if err != nil {
		return api.PullReply{}, err
	}
	defer release()
	now, err := source.Capture(ctx, source.Options{Root: meta.Dir, Rules: h.base.rules, Cache: r.statCachePath(meta.Dir), CacheTrees: r.store})
	if err != nil {
		return api.PullReply{}, coded(err)
	}
	capture, err := r.store.Flatten(meta.Tree)
	if err != nil {
		return api.PullReply{}, coded(err)
	}
	bases, err := r.folderBases(h)
	if err != nil {
		return api.PullReply{}, coded(err)
	}
	reply, placed, err := r.pullFolder(ctx, h, args, capture, now.Files, bases)
	if len(placed) != 0 {
		r.invalidate(args.Checkout, placed, nil)
	}
	if err != nil {
		return api.PullReply{}, err
	}
	reply.Warnings = r.driftWarnings(ctx, h)
	if args.DryRun {
		return reply, nil
	}
	read := func(p string, f store.File) ([]byte, error) { return h.base.view.Blob(p, f.ID) }
	if err := r.setFolderBases(h, bases, read); err != nil {
		return api.PullReply{}, coded(err)
	}
	return reply, nil
}

// pullFolder is PullFolder under the checkout's freeze lock: it merges now,
// the folder's leaves, into h over capture, those of its base, advancing bases
// as it goes. It returns the paths it wrote, which the caller invalidates once
// the locks are released.
func (r *Registry) pullFolder(ctx context.Context, h *checkoutRecord, args api.PullArgs, capture, now map[string]store.File, bases folderBases) (reply api.PullReply, placed []string, err error) {
	h.freeze.Lock()
	defer h.freeze.Unlock()
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.checkouts[args.Checkout] != h || h.retired.Load() {
		return api.PullReply{}, nil, failure("not_found", "no checkout %s", args.Checkout)
	}
	if r.closed {
		return api.PullReply{}, nil, failure("io", "registry is closed")
	}
	root := h.base.view.Meta().Dir
	reply = api.PullReply{Folder: root, Source: filepath.Join(r.config.Mount, args.Checkout), Applied: []api.Change{}, Merged: []string{}, Conflicts: []api.Conflict{}}
	s := sides{ours: "in the checkout", theirs: "in the source folder",
		labels: git.Labels{Ours: "checkout " + h.meta.Name, Base: "base", Theirs: "folder"}}
	// held is the folder as the checkout last took it, which tells the paths
	// the folder changed since; against is what each of those merges from.
	held, against := maps.Clone(capture), capture
	bases.over(held, false)
	bases.over(against, true)
	seen := map[string]bool{}
	for _, c := range compare(held, now) {
		if err := checkContext(ctx); err != nil {
			return api.PullReply{}, placed, err
		}
		seen[c.Path] = true
		fail := func(err error) (api.PullReply, []string, error) {
			return api.PullReply{}, placed, coded(fmt.Errorf("%s: %w", c.Path, err))
		}
		// The capture named the path; the file read now is what merges. A
		// directory there means the file itself is gone.
		theirs, incoming, err := folderLeaf(filepath.Join(root, filepath.FromSlash(c.Path)))
		if err != nil {
			return fail(err)
		}
		base := against[c.Path]
		o, reason, out := skipped, "", incoming
		switch _, wasMarked := bases.marked[c.Path]; {
		case wasMarked && regular(theirs) && hasMarkers(incoming):
			o, reason = unresolved, "conflict markers are not resolved yet in the source folder"
		case theirs == base:
			// The folder holds what an apply left there: nothing new to merge.
		default:
			blob := func(f store.File) ([]byte, error) { return h.base.view.Content(c.Path, f.ID) }
			if o, reason, out, err = mergeOverlay(h, c.Path, base, theirs, incoming, blob, s); err != nil {
				return fail(err)
			}
		}
		if settled := o != conflict && o != unresolved; settled && !args.DryRun {
			if o != skipped {
				placed = append(placed, c.Path)
				if err := placeOverlay(h.fs, c.Path, theirs, out); err != nil {
					return fail(err)
				}
			}
			// Kept as the path's base for when the folder has moved on from it.
			if theirs.ID != "" && !r.store.Has(theirs.ID) {
				if _, err := r.store.PutBytes(incoming); err != nil {
					return fail(err)
				}
			}
		}
		report(&reply.ApplyReply, c, o, reason)
		bases.afterPull(c.Path, theirs, o)
	}
	err = reportPending(&reply.ApplyReply, bases.markedHere, seen, func(p string) (bool, error) {
		ours, current, _, err := overlayLeaf(h.fs, p)
		return regular(ours) && hasMarkers(current), err
	})
	if err != nil {
		return api.PullReply{}, placed, coded(err)
	}
	return reply, placed, nil
}

// folderLeaf reads a folder path as a tree leaf with its content; a path that
// is absent, or a directory, is the zero leaf.
func folderLeaf(name string) (store.File, []byte, error) {
	info, err := hostfile.Lstat(name)
	if errors.Is(err, os.ErrNotExist) || err == nil && info.IsDir() {
		return store.File{}, nil, nil
	}
	if err != nil {
		return store.File{}, nil, err
	}
	mode, ok := store.ModeOf(info.Mode())
	if !ok {
		return store.File{}, nil, fmt.Errorf("unsupported file type %s", info.Mode().Type())
	}
	var data []byte
	if mode == store.ModeSymlink {
		target, err := os.Readlink(name)
		if err != nil {
			return store.File{}, nil, err
		}
		data = []byte(filepath.ToSlash(target))
	} else if data, err = os.ReadFile(name); err != nil {
		return store.File{}, nil, err
	}
	return store.File{Mode: mode, ID: store.HashBytes("blob", data)}, data, nil
}

// shellQuote spells a path for a POSIX shell command line shown to the user.
func shellQuote(p string) string {
	if !strings.ContainsAny(p, " \t\n'\"$\\`&|;<>()*?[]{}~#!") {
		return p
	}
	return "'" + strings.ReplaceAll(p, "'", `'\''`) + "'"
}

// driftWarnings says what a pull from the folder leaves to git: files merge,
// but the checkout's HEAD stays where it was when the folder's has moved on.
func (r *Registry) driftWarnings(ctx context.Context, h *checkoutRecord) []string {
	repo := localRepo(h.base)
	if !h.meta.Git || repo == nil {
		return nil
	}
	d, err := repo.Drift(ctx, r.gitdirPath(h.meta.dir()))
	if err != nil || !d.Behind {
		return nil
	}
	folder := h.base.view.Meta().Dir
	quoted := shellQuote(folder)
	short := func(id string) string { return id[:min(len(id), 12)] }
	if d.FastForward {
		return []string{fmt.Sprintf("only files were merged: the checkout's HEAD is still %s and %s is at %s; inside the checkout, `git fetch %s HEAD && git reset FETCH_HEAD` moves it there and keeps the files",
			short(d.Checkout), folder, short(d.Folder), quoted)}
	}
	return []string{fmt.Sprintf("only files were merged: the checkout's commits are not based on %s, where %s is now; inside the checkout, `git fetch %s HEAD && git rebase --autostash FETCH_HEAD` moves them there",
		short(d.Folder), folder, quoted)}
}
