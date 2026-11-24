package checkout

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/git"
	"github.com/stencil-hq/ovnfs/internal/hostfile"
	"github.com/stencil-hq/ovnfs/overlay"
	"github.com/stencil-hq/ovnfs/source"
	"github.com/stencil-hq/ovnfs/store"
)

// liveTree returns a checkout's current source files without freezing it: its nearest
// hashed ancestor plus every newer layer and the live upper, hashed without storing.
func (r *Registry) liveTree(h *checkoutRecord) (*tree, error) {
	r.mu.RLock()
	parent, layers, ready, err := r.ancestorTreeLocked(h.base, h.stack, h.meta.Head)
	if err == nil && !ready {
		// An ancestor is still hashing; start from the base and replay every layer.
		parent, layers, _, err = r.ancestorTreeLocked(h.base, h.stack, nil)
	}
	r.mu.RUnlock()
	if err != nil {
		return nil, err
	}
	h.freeze.RLock()
	defer h.freeze.RUnlock()
	if err := h.fs.Flush(); err != nil {
		return nil, err
	}
	upper, err := overlay.IndexUpper(h.upper)
	if err != nil {
		return nil, failure("io", "reading writable files for checkout %s: %v", h.meta.Name, err)
	}
	t, err := newTree(h.base, nil, parent, overlayView{h.fs})
	if err != nil {
		return nil, err
	}
	for _, l := range append(layers, upper) {
		if err := t.apply(l); err != nil {
			return nil, err
		}
	}
	t.finish()
	return t, nil
}

// lookupCheckout returns a live checkout.
func (r *Registry) lookupCheckout(name string) (*checkoutRecord, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	h := r.checkouts[name]
	if h == nil || h.retired.Load() {
		return nil, failure("not_found", "no checkout %s", name)
	}
	return h, nil
}

// checkoutBase returns the leaves a checkout's own changes count from: those
// of its capture, with the folder's version of each path a pull brought in
// (see folderBases) in their place.
func (r *Registry) checkoutBase(h *checkoutRecord) (map[string]store.File, error) {
	from, err := h.base.view.Store().Flatten(h.base.view.Meta().Tree)
	if err != nil {
		return nil, err
	}
	bases, err := r.folderBases(h)
	if err != nil {
		return nil, err
	}
	bases.over(from, false)
	return from, nil
}

// fromLeaves resolves the leaves a diff compares against: the checkout's base
// (see checkoutBase), or a hashed snapshot on the same capture.
func (r *Registry) fromLeaves(ctx context.Context, h *checkoutRecord, id string) (map[string]store.File, error) {
	if id == "" {
		return r.checkoutBase(h)
	}
	r.mu.RLock()
	s := r.snapshots[id]
	r.mu.RUnlock()
	if s == nil {
		return nil, failure("not_found", "no snapshot %s", id)
	}
	if s.base != h.base {
		return nil, failure("invalid", "snapshot %s is not on the same capture as checkout %s", id, h.meta.Name)
	}
	reply, err := r.waitHashed(ctx, s)
	if err != nil {
		return nil, err
	}
	return h.base.view.Store().Flatten(store.OID(reply.Snapshot.Tree))
}

// Diff lists source paths that differ between a snapshot (default: the base) and the checkout now.
func (r *Registry) Diff(ctx context.Context, args api.DiffArgs) (api.DiffReply, error) {
	resume, err := r.Quiesce(ctx)
	if err != nil {
		return api.DiffReply{}, err
	}
	defer resume()
	h, err := r.lookupCheckout(args.Checkout)
	if err != nil {
		return api.DiffReply{}, err
	}
	from, err := r.fromLeaves(ctx, h, args.From)
	if err != nil {
		return api.DiffReply{}, coded(err)
	}
	now, err := r.liveTree(h)
	if err != nil {
		return api.DiffReply{}, coded(err)
	}
	reply := api.DiffReply{From: args.From, Changes: compare(from, now.files)}
	if reply.From == "" {
		reply.From = "base"
	}
	if reply.Changes, reply.Left, err = underPaths(reply.Changes, args.Paths); err != nil {
		return api.DiffReply{}, err
	}
	if args.Patch || args.Binary || args.Stat {
		var eol *eolConverter
		if args.Binary {
			paths := make([]string, len(reply.Changes))
			for i, c := range reply.Changes {
				paths[i] = c.Path
			}
			if eol, err = newEOLConverter(ctx, h.base.view, paths); err != nil {
				return api.DiffReply{}, coded(err)
			}
			defer eol.Close()
		}
		var patch strings.Builder
		kept := reply.Changes[:0:0]
		for _, c := range reply.Changes {
			old, err := r.content(h, from[c.Path], supply{path: c.Path})
			if err != nil {
				return api.DiffReply{}, coded(err)
			}
			cur, err := r.content(h, now.files[c.Path], now.supplier(c.Path))
			if err != nil {
				return api.DiffReply{}, coded(err)
			}
			a, b := from[c.Path], now.files[c.Path]
			if args.Binary {
				// A git patch shows what git stores: a change of line endings that
				// git normalizes away is no change.
				if a, old, err = eol.blob(c.Path, a, old); err != nil {
					return api.DiffReply{}, coded(err)
				}
				if b, cur, err = eol.blob(c.Path, b, cur); err != nil {
					return api.DiffReply{}, coded(err)
				}
				if a == b {
					continue
				}
			}
			stat := api.DiffStat{Path: c.Path}
			if args.Patch || args.Binary {
				stat.Added, stat.Deleted, stat.Binary = gitPatch(&patch, c.Path, a, b, old, cur, args.Binary)
			} else if stat.Binary = binaryContent(old, cur); !stat.Binary {
				stat.Added, stat.Deleted = countEdits(lineDiff(splitLines(old), splitLines(cur)))
			}
			if args.Stat {
				reply.Stat = append(reply.Stat, stat)
			}
			kept = append(kept, c)
		}
		reply.Changes, reply.Patch, reply.Binary = kept, patch.String(), args.Binary
	}
	return reply, nil
}

// underPaths keeps the changes at or below one of paths, each relative to the
// checkout's root, and counts the others; no path keeps them all, as does the
// root itself.
func underPaths(changes []api.Change, paths []string) (kept []api.Change, left int, err error) {
	var only pathSet
	for _, p := range paths {
		clean := path.Clean(p)
		if clean == "." {
			return changes, 0, nil
		}
		if path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
			return nil, 0, failure("invalid", "path %s is not relative to the checkout's root", p)
		}
		only.add(clean)
	}
	if len(paths) == 0 {
		return changes, 0, nil
	}
	kept = changes[:0]
	for _, c := range changes {
		if only.covers(c.Path) {
			kept = append(kept, c)
		}
	}
	return kept, len(changes) - len(kept), nil
}

// compare returns changed blob paths in order; empty-directory leaves are not changes.
func compare(from, to map[string]store.File) []api.Change {
	changes := []api.Change{}
	for _, p := range source.SortedPaths(union(from, to)) {
		a, inFrom := from[p]
		b, inTo := to[p]
		if inFrom && a.Mode == store.ModeDir {
			inFrom = false
		}
		if inTo && b.Mode == store.ModeDir {
			inTo = false
		}
		switch {
		case !inFrom && !inTo:
		case !inFrom:
			changes = append(changes, api.Change{Path: p, Status: "A"})
		case !inTo:
			changes = append(changes, api.Change{Path: p, Status: "D"})
		case (a.Mode == store.ModeSymlink) != (b.Mode == store.ModeSymlink):
			changes = append(changes, api.Change{Path: p, Status: "T"})
		case a != b:
			changes = append(changes, api.Change{Path: p, Status: "M"})
		}
	}
	return changes
}

// union merges two leaf maps' keys.
func union(a, b map[string]store.File) map[string]store.File {
	out := make(map[string]store.File, len(a)+len(b))
	maps.Copy(out, a)
	maps.Copy(out, b)
	return out
}

// content reads a leaf's bytes where from names: from the layer that supplied
// it, else the store or the source's git objects.
func (r *Registry) content(h *checkoutRecord, f store.File, from supply) ([]byte, error) {
	if f.ID == "" || f.Mode == store.ModeDir {
		return nil, nil
	}
	if from.layer != nil {
		if f.Mode == store.ModeSymlink {
			target, err := from.layer.Readlink(from.path)
			return []byte(target), err
		}
		return from.layer.ReadFile(from.path)
	}
	return h.base.view.Blob(from.path, f.ID)
}

// binaryContent reports whether either side of a change is binary, as git
// tells: a NUL byte near its start.
func binaryContent(old, cur []byte) bool {
	return bytes.IndexByte(head(old), 0) >= 0 || bytes.IndexByte(head(cur), 0) >= 0
}

// countEdits returns how many lines a line diff script inserts and deletes.
func countEdits(script []byte) (added, deleted int) {
	for _, op := range script {
		switch op {
		case opInsert:
			added++
		case opDelete:
			deleted++
		}
	}
	return added, deleted
}

// diffContext is the number of unchanged lines around each change, as diff -u.
const diffContext = 3

// Edit operations of a line diff script.
const (
	opEqual  byte = ' '
	opDelete byte = '-'
	opInsert byte = '+'
)

// writeHunk prints script[start:end] as one hunk: line positions are recovered
// by counting the deletes/equals and inserts/equals that precede start.
func writeHunk(w *strings.Builder, script []byte, start, end int, a, b []string) {
	ai, bi := 0, 0
	for _, op := range script[:start] {
		if op != opInsert {
			ai++
		}
		if op != opDelete {
			bi++
		}
	}
	an, bn := 0, 0
	for _, op := range script[start:end] {
		if op != opInsert {
			an++
		}
		if op != opDelete {
			bn++
		}
	}
	fmt.Fprintf(w, "@@ -%s +%s @@\n", hunkRange(ai, an), hunkRange(bi, bn))
	for _, op := range script[start:end] {
		var line string
		switch op {
		case opInsert:
			line = b[bi]
			bi++
		case opDelete:
			line = a[ai]
			ai++
		default:
			line = a[ai]
			ai++
			bi++
		}
		w.WriteByte(op)
		w.WriteString(line)
		if !strings.HasSuffix(line, "\n") {
			w.WriteString("\n\\ No newline at end of file\n")
		}
	}
}

// hunkRange formats a hunk side as diff -u does: an empty side names the line
// before it, and a count of one is left out.
func hunkRange(before, n int) string {
	switch n {
	case 0:
		return fmt.Sprintf("%d,0", before)
	case 1:
		return fmt.Sprint(before + 1)
	}
	return fmt.Sprintf("%d,%d", before+1, n)
}

// splitLines cuts b after every newline; a final line without one keeps no
// terminator, so it differs from the same text with one, as in diff.
func splitLines(b []byte) []string {
	var lines []string
	for len(b) > 0 {
		n := bytes.IndexByte(b, '\n') + 1
		if n == 0 {
			n = len(b)
		}
		lines = append(lines, string(b[:n]))
		b = b[n:]
	}
	return lines
}

// maxEdits and maxSteps bound the Myers search: past this many inserted plus
// deleted lines, or this many diagonal and match steps, the middle left after
// trimming the common prefix and suffix is reported as wholly replaced, still
// a correct patch, instead of spending quadratic time and memory on a minimal one.
const (
	maxEdits = 2000
	maxSteps = 1 << 26
)

// lineDiff returns an edit script turning a into b, deletions before insertions
// within each change.
func lineDiff(a, b []string) []byte {
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	ids := map[string]int32{}
	intern := func(lines []string) []int32 {
		out := make([]int32, len(lines))
		for i, l := range lines {
			id, ok := ids[l]
			if !ok {
				id = int32(len(ids))
				ids[l] = id
			}
			out[i] = id
		}
		return out
	}
	x, y := intern(a[pre:len(a)-suf]), intern(b[pre:len(b)-suf])
	script := make([]byte, 0, len(a)+len(b))
	for range pre {
		script = append(script, opEqual)
	}
	middle, ok := myers(x, y)
	if !ok {
		middle = middle[:0]
		for range x {
			middle = append(middle, opDelete)
		}
		for range y {
			middle = append(middle, opInsert)
		}
	}
	script = append(script, middle...)
	for range suf {
		script = append(script, opEqual)
	}
	return script
}

// myers finds a shortest edit script from x to y (Myers 1986), keeping each
// round's furthest-reaching diagonals to backtrack. It gives up (false) past
// maxEdits or maxSteps.
func myers(x, y []int32) ([]byte, bool) {
	n, m := len(x), len(y)
	off := maxEdits + 1
	v := make([]int32, 2*maxEdits+3)
	var trace [][]int32
	steps := 0
	for d := 0; d <= maxEdits; d++ {
		for k := -d; k <= d; k += 2 {
			var i int
			if k == -d || (k != d && v[off+k-1] < v[off+k+1]) {
				i = int(v[off+k+1])
			} else {
				i = int(v[off+k-1]) + 1
			}
			j := i - k
			steps++
			for i < n && j < m && x[i] == y[j] {
				i++
				j++
				steps++
			}
			if steps > maxSteps {
				return nil, false
			}
			v[off+k] = int32(i)
			if i >= n && j >= m {
				trace = append(trace, slices.Clone(v[off-d:off+d+1]))
				return backtrack(trace, n, m), true
			}
		}
		trace = append(trace, slices.Clone(v[off-d:off+d+1]))
	}
	return nil, false
}

// backtrack walks myers' rounds from (n, m) back to the origin, then orders
// each change's deletions before its insertions as diff -u prints them.
func backtrack(trace [][]int32, n, m int) []byte {
	var rev []byte
	i, j := n, m
	for d := len(trace) - 1; d > 0; d-- {
		prev := trace[d-1] // diagonal k is at prev[k+d-1]
		k := i - j
		pk := k - 1
		if k == -d || (k != d && prev[k-1+d-1] < prev[k+1+d-1]) {
			pk = k + 1
		}
		pi := int(prev[pk+d-1])
		pj := pi - pk
		mi := pi + 1 // the snake starts after this round's edit
		if pk == k+1 {
			mi = pi
		}
		for i > mi {
			rev = append(rev, opEqual)
			i--
			j--
		}
		if pk == k+1 {
			rev = append(rev, opInsert)
		} else {
			rev = append(rev, opDelete)
		}
		i, j = pi, pj
	}
	for range i {
		rev = append(rev, opEqual)
	}
	slices.Reverse(rev)
	for s := 0; s < len(rev); {
		if rev[s] == opEqual {
			s++
			continue
		}
		e := s
		dels := 0
		for e < len(rev) && rev[e] != opEqual {
			if rev[e] == opDelete {
				dels++
			}
			e++
		}
		for t := s; t < e; t++ {
			rev[t] = opInsert
			if t-s < dels {
				rev[t] = opDelete
			}
		}
		s = e
	}
	return rev
}

// head is the prefix binary detection inspects, as git does.
func head(b []byte) []byte { return b[:min(len(b), 8000)] }

// Apply merges a checkout's source changes back into the folder it came from, three-way
// against the base capture: paths unchanged in the folder take the checkout's version,
// text files changed on both sides are merged, with conflict markers where both changed
// the same lines, and everything else is a conflict left untouched. A path the folder
// took, or whose markers the user resolved, merges later changes against the version
// it took then (see folderBases), so applying again brings only what changed since.
// Applies into one folder take turns, each merging against what the last one wrote.
// Atomic writes nothing when a path would conflict. With Branch it instead copies the
// checkout's commits.
func (r *Registry) Apply(ctx context.Context, args api.ApplyArgs) (api.ApplyReply, error) {
	resume, err := r.Quiesce(ctx)
	if err != nil {
		return api.ApplyReply{}, err
	}
	defer resume()
	h, err := r.lookupCheckout(args.Checkout)
	if err != nil {
		return api.ApplyReply{}, err
	}
	if remote := h.base.view.Meta().Remote; remote != nil {
		return api.ApplyReply{}, failure("invalid", "checkout %s is attached from %s; its folder is not on this machine", h.meta.Name, remote.Source)
	}
	release, err := r.lockFolderMerge(ctx, h)
	if err != nil {
		return api.ApplyReply{}, err
	}
	defer release()
	if args.Branch == "" {
		reply, _, err := r.mergeFiles(ctx, h, mergeOptions{dryRun: args.DryRun, atomic: args.Atomic})
		return reply, err
	}
	if args.Atomic {
		return api.ApplyReply{}, failure("invalid", "--atomic applies to files; --branch moves commits and writes none")
	}
	repo := h.base.view.Domains().Repo("")
	if !h.meta.Git || repo == nil {
		return api.ApplyReply{}, failure("invalid", "checkout %s has no git directory; --branch needs a repository source", h.meta.Name)
	}
	commit, updated, err := repo.FetchBranch(ctx, r.gitdirPath(h.meta.dir()), args.Branch, args.DryRun)
	if conflict, ok := errors.AsType[*git.BranchConflict](err); ok {
		advice := "choose a different branch name"
		if !conflict.Diverged && len(conflict.Staged) == 0 {
			advice = "run it again, or " + advice
		}
		return api.ApplyReply{}, failure("conflict", "%s; %s", conflict, advice)
	}
	if errors.Is(err, git.ErrBranchDryRun) {
		return api.ApplyReply{}, failure("invalid", "%s", err)
	}
	if err != nil {
		return api.ApplyReply{}, codedTransfer(err)
	}
	reply := api.ApplyReply{Source: h.base.view.Meta().Dir, Applied: []api.Change{}, Merged: []string{}, Conflicts: []api.Conflict{}, Branch: args.Branch, Commit: commit}
	for _, change := range updated {
		reply.Applied = append(reply.Applied, api.Change{Path: change.Path, Status: change.Status})
	}
	return reply, nil
}

// codedTransfer is coded for a failure to copy a checkout's commits into its
// folder's repository, which knows the one cause the user can act on.
func codedTransfer(err error) error {
	if partial, ok := errors.AsType[*git.PartialCloneError](err); ok {
		return failure("unavailable", "%s", partial)
	}
	return coded(err)
}

// lockFolderMerge protects both the checkout's merge state and the folder being
// updated. PullFolder takes the same locks in this order.
func (r *Registry) lockFolderMerge(ctx context.Context, h *checkoutRecord) (func(), error) {
	h.merge.Lock()
	release, err := r.applying.lock(ctx, h.base.view.Meta().Dir)
	if err != nil {
		h.merge.Unlock()
		return nil, err
	}
	return func() {
		release()
		h.merge.Unlock()
	}, nil
}

// mergeOptions shape one merge of a checkout's files into its folder.
type mergeOptions struct {
	dryRun bool
	// atomic writes nothing unless a first pass finds no conflict.
	atomic bool
	land   *landFiles
}

// mergeFiles merges h into its folder with both merge locks held. For land,
// Git defines the paths being integrated, not the checkout's open-time diff.
func (r *Registry) mergeFiles(ctx context.Context, h *checkoutRecord, o mergeOptions) (reply api.ApplyReply, left int, err error) {
	root := h.base.view.Meta().Dir
	snap, err := r.Snap(ctx, api.SnapArgs{Checkout: h.meta.Name, Label: "apply", Wait: !o.dryRun})
	if err != nil {
		return api.ApplyReply{}, 0, err
	}
	// The snapshot outlives only a merge that left something in the folder to
	// come back to: an error, or conflicts that were written.
	id := snap.Snapshot.ID
	defer func() {
		if o.dryRun || reply.Refused || err == nil && len(reply.Conflicts) == 0 {
			if dropErr := r.Drop(context.WithoutCancel(ctx), api.DropArgs{Snapshot: id}); dropErr != nil && err == nil {
				reply, err = api.ApplyReply{}, dropErr
			}
		}
	}()
	if o.dryRun {
		r.mu.RLock()
		s := r.snapshots[id]
		r.mu.RUnlock()
		if s == nil {
			return api.ApplyReply{}, 0, failure("not_found", "no snapshot %s", id)
		}
		if snap, err = r.waitHashed(ctx, s); err != nil {
			return api.ApplyReply{}, 0, err
		}
	}
	// Unix would fail each path with ENOTDIR, but Windows reports them absent,
	// which would turn every path into a deletion conflict.
	if info, err := os.Stat(root); err == nil && !info.IsDir() {
		return api.ApplyReply{}, 0, failure("invalid", "source %s is not a directory", root)
	}
	st := h.base.view.Store()
	from, err := st.Flatten(h.base.view.Meta().Tree)
	if err != nil {
		return api.ApplyReply{}, 0, coded(err)
	}
	to, err := st.Flatten(store.OID(snap.Snapshot.Tree))
	if err != nil {
		return api.ApplyReply{}, 0, coded(err)
	}
	bases, err := r.folderBases(h)
	if err != nil {
		return api.ApplyReply{}, 0, coded(err)
	}
	bases.over(from, true)
	baseRead, theirRead := h.base.view.Content, h.base.view.Content
	var changes []api.Change
	if o.land == nil {
		changes = compare(from, to)
	} else {
		changes, left, err = o.land.plan(h, from, to, bases)
		if err != nil {
			return api.ApplyReply{}, 0, coded(err)
		}
		baseRead, theirRead = o.land.readBase, o.land.readTheirs
	}
	s := sides{ours: "in the source folder", theirs: "in the checkout",
		labels: git.Labels{Ours: "folder", Base: "base", Theirs: "checkout " + h.meta.Name}}
	allowMarkers := o.land == nil || !o.land.replayed
	// pass merges every change, writing unless dry, and records in bases what
	// the folder took.
	pass := func(dry bool, bases folderBases) (api.ApplyReply, error) {
		reply := api.ApplyReply{Source: root, Applied: []api.Change{}, Merged: []string{}, Conflicts: []api.Conflict{}}
		seen := map[string]bool{}
		for _, c := range changes {
			seen[c.Path] = true
			var held bool
			var err error
			if o.land == nil || !o.land.replayed {
				held, err = r.holdsPullMarkers(h, bases, c.Path, to[c.Path])
			}
			o, reason := unresolved, "conflict markers from a pull are not resolved in the checkout"
			if err == nil && !held {
				o, reason, err = r.applyOne(h, root, c.Path, from[c.Path], to[c.Path], baseRead, theirRead, s, dry, allowMarkers)
			}
			if err != nil {
				return api.ApplyReply{}, coded(fmt.Errorf("%s: %w", c.Path, err))
			}
			report(&reply, c, o, reason)
			bases.afterApply(c.Path, to[c.Path], o)
		}
		err := reportPending(&reply, bases.marked, seen, func(p string) (bool, error) {
			if o.land != nil && !o.land.includes(p) {
				return false, nil
			}
			return hasMarkersAt(filepath.Join(root, filepath.FromSlash(p)))
		})
		return reply, coded(err)
	}
	// A conflict can be settled in the checkout instead when a pull can bring
	// the folder's files there.
	pullable := !h.base.view.Meta().Chosen
	if o.atomic && !o.dryRun {
		// The folder may change between the passes: the second then writes
		// that conflict's markers, as an apply that is not atomic would.
		probe, err := pass(true, bases.clone())
		if err != nil || len(probe.Conflicts) != 0 {
			probe.Refused, probe.Pullable = err == nil, err == nil && pullable
			return probe, left, err
		}
	}
	if reply, err = pass(o.dryRun, bases); err != nil {
		return api.ApplyReply{}, 0, err
	}
	reply.Pullable = pullable && len(reply.Conflicts) != 0
	if o.dryRun {
		reply.Refused = o.atomic && len(reply.Conflicts) != 0
		return reply, left, nil
	}
	if o.land != nil && o.land.replayed {
		return reply, left, nil // the original checkout was not replayed
	}
	read := func(p string, f store.File) ([]byte, error) { return h.base.view.Blob(p, f.ID) }
	if err := r.setFolderBases(h, bases, read); err != nil {
		return api.ApplyReply{}, 0, coded(err)
	}
	return reply, left, nil
}

// holdsPullMarkers reports whether theirs, the checkout's version of p, still
// holds the conflict markers a pull from the folder wrote into it: such a path
// stays out of the folder until they are resolved.
func (r *Registry) holdsPullMarkers(h *checkoutRecord, bases folderBases, p string, theirs store.File) (bool, error) {
	if _, pending := bases.markedHere[p]; !pending || !regular(theirs) {
		return false, nil
	}
	data, err := h.base.view.Content(p, theirs.ID)
	return hasMarkers(data), err
}

// applyOne brings one path of the folder to the checkout's version when that is safe,
// writing conflict markers where both sides changed the same lines of a text file.
func (r *Registry) applyOne(h *checkoutRecord, root, p string, base, theirs store.File, baseRead, theirRead func(string, store.OID) ([]byte, error), s sides, dry, allowMarkers bool) (outcome, string, error) {
	dst := filepath.Join(root, filepath.FromSlash(p))
	ours, isDir, err := leafAt(dst)
	if err != nil {
		return 0, "", err
	}
	if isDir {
		return conflict, "is a directory in the source folder", nil
	}
	if ours == theirs {
		return skipped, "", nil
	}
	if clean := lfsCleanHost(h.base.view, p, ours, dst); clean == base {
		ours = clean // the folder still holds the object of the base's LFS pointer
	}
	if ours == base {
		if dry {
			return applied, "", nil
		}
		return applied, "", r.place(h, dst, p, theirs, theirRead)
	}
	why, text := s.conflict(base, ours, theirs)
	if !text {
		return conflict, why, nil
	}
	current, err := os.ReadFile(dst)
	if err != nil {
		return 0, "", err
	}
	var baseData []byte
	if base.ID != "" {
		if baseData, err = baseRead(p, base.ID); err != nil {
			return 0, "", err
		}
	}
	theirData, err := theirRead(p, theirs.ID)
	if err != nil {
		return 0, "", err
	}
	result, o, reason := mergeText(s.labels, current, baseData, theirData, why)
	if !allowMarkers && o == marked {
		return conflict, reason, nil
	}
	if dry || (o != merged && o != marked) {
		return o, reason, nil
	}
	return o, reason, writeAtomic(dst, func(tmp string) error { return os.WriteFile(tmp, result, modeOf(theirs)) })
}

// hasMarkersAt reports whether the folder file name holds conflict markers; anything but a regular file holds none.
func hasMarkersAt(name string) (bool, error) {
	info, err := os.Lstat(name)
	if errors.Is(err, os.ErrNotExist) || err == nil && !info.Mode().IsRegular() {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	data, err := os.ReadFile(name)
	return hasMarkers(data), err
}

// leafAt reads the folder's current state of a path as a tree leaf (zero when absent).
func leafAt(name string) (store.File, bool, error) {
	info, err := hostfile.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return store.File{}, false, nil
	}
	if err != nil {
		return store.File{}, false, err
	}
	if info.IsDir() {
		return store.File{}, true, nil
	}
	mode, ok := store.ModeOf(info.Mode())
	if !ok {
		return store.File{}, false, fmt.Errorf("unsupported file type %s", info.Mode().Type())
	}
	if mode == store.ModeSymlink {
		target, err := os.Readlink(name)
		return store.File{Mode: mode, ID: store.HashBytes("blob", []byte(filepath.ToSlash(target)))}, false, err
	}
	id, _, err := store.HashFile(name)
	return store.File{Mode: mode, ID: id}, false, err
}

// place writes the checkout's version of a path into the folder, or deletes it.
func (r *Registry) place(h *checkoutRecord, dst, p string, f store.File, read func(string, store.OID) ([]byte, error)) error {
	if f.ID == "" {
		err := os.Remove(dst)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	st := h.base.view.Store()
	return writeAtomic(dst, func(tmp string) error {
		switch {
		case f.Mode == store.ModeSymlink:
			target, err := read(p, f.ID)
			if err != nil {
				return err
			}
			return os.Symlink(string(target), tmp)
		case st.Has(f.ID) && !h.base.view.LFSLeaf(p, f.ID):
			return st.CloneBlob(f.ID, tmp, modeOf(f))
		default:
			// An LFS leaf's work-tree bytes are its object, even where the
			// store holds the pointer blob too.
			data, err := read(p, f.ID)
			if err != nil {
				return err
			}
			return os.WriteFile(tmp, data, modeOf(f))
		}
	})
}

// modeOf maps a blob leaf to its permission bits.
func modeOf(f store.File) os.FileMode {
	if f.Mode == store.ModeExec {
		return 0o755
	}
	return 0o644
}

// writeAtomic creates content at a temporary name beside dst and renames it over dst
// with [hostfile.Rename], which replaces dst even when it is read-only or, on
// Windows, held open by a process that shares delete access.
func writeAtomic(dst string, write func(tmp string) error) error {
	tmp := filepath.Join(filepath.Dir(dst), ".ovnfs-apply-"+filepath.Base(dst))
	_ = os.Remove(tmp)
	if err := write(tmp); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := hostfile.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
