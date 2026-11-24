package base

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/stencil-hq/ovnfs/git"
	"github.com/stencil-hq/ovnfs/internal/clone"
	"github.com/stencil-hq/ovnfs/rules"
	"github.com/stencil-hq/ovnfs/source"
	"github.com/stencil-hq/ovnfs/store"
)

// revisionView supplies rule facts from REV, not the folder's current contents.
type revisionView struct {
	files   map[string]store.File
	info    map[string]source.Info
	dirs    map[string]revisionDir
	objects *git.Objects
}

type revisionDir struct {
	class   rules.Class // cached classification, including any deciding ancestor
	visible bool        // has a revision leaf that survives the projection
}

func (v *revisionView) Lstat(p string) (os.FileInfo, error) {
	if f, ok := v.files[p]; ok {
		mode := os.FileMode(0o644)
		switch f.Mode {
		case store.ModeDir:
			mode = os.ModeDir | 0o755
		case store.ModeSymlink:
			mode = os.ModeSymlink | 0o777
		case store.ModeExec:
			mode = 0o755
		}
		return capturedInfo{name: path.Base(p), mode: mode, size: v.info[p].Size}, nil
	}
	if _, ok := v.dirs[p]; ok {
		return capturedInfo{name: path.Base(p), mode: os.ModeDir | 0o755}, nil
	}
	return nil, os.ErrNotExist
}

func (v *revisionView) tracked(p string) bool {
	_, dir := v.dirs[p]
	return dir || v.files[p].ID != ""
}

func (v *revisionView) ReadFile(p string) ([]byte, error) {
	f, ok := v.files[p]
	if !ok {
		return nil, os.ErrNotExist
	}
	return v.objects.Blob(string(f.ID))
}

// inheritedView classifies an untracked entry retained from the folder. Its
// type and marker files are the folder's, but its ignore files are REV's.
type inheritedView struct {
	source.Dir
	revision *revisionView
}

func (v inheritedView) ReadFile(p string) ([]byte, error) { return v.revision.ReadFile(p) }

type revisionProjection struct {
	hidden  map[string]int       // deciding root -> rule position
	skipped map[string]bool      // omitted index entries, including gitlinks
	ignores map[string]store.OID // hidden .gitignore blobs still used to classify takes
	private bool                 // has its own root Git objects, shared with its checkouts
}

func (pr *revisionProjection) match(set rules.Set, p string, facts rules.Facts) rules.Class {
	i := set.MatchIndex(p, facts)
	if i < 0 {
		return rules.Source
	}
	class := set.Rules[i].Class
	if class == rules.Hidden {
		if pr.hidden == nil {
			pr.hidden = map[string]int{}
		}
		pr.hidden[p] = i
	}
	return class
}

// project honors first-match and ancestor precedence without re-evaluating each
// directory for every leaf beneath it. Only Hidden removes tracked paths.
func (v *revisionView) project(set rules.Set, domains *source.Domains) revisionProjection {
	var projection revisionProjection
	match := func(p string) rules.Class {
		return projection.match(set, p, domains.Facts(v, p, nil))
	}
	var classifyDir func(string) rules.Class
	classifyDir = func(p string) rules.Class {
		if p == "." {
			return rules.Source
		}
		d := v.dirs[p]
		if d.class == "" {
			d.class = classifyDir(path.Dir(p))
			if d.class == rules.Source {
				d.class = match(p)
			}
			v.dirs[p] = d
		}
		return d.class
	}
	for p := range v.files {
		parent := path.Dir(p)
		class := classifyDir(parent)
		if class == rules.Source {
			class = match(p)
		}
		if class == rules.Hidden {
			if projection.skipped == nil {
				projection.skipped = map[string]bool{}
			}
			projection.skipped[p] = true
			continue
		}
		for dir := parent; dir != "."; dir = path.Dir(dir) {
			d := v.dirs[dir]
			if d.visible {
				break
			}
			d.visible = true
			v.dirs[dir] = d
		}
	}
	return projection
}

// revisionIndex names the private index of a revision base in its state directory.
const revisionIndex = "revision-index"

// revisionObjects is the root object directory that the checkout's Git dir
// also links to.
func revisionObjects(c Config) string { return filepath.Join(c.StateDir, "objects") }

// CheckoutRevision resolves spec against the checkout's Git directory, and
// records where this base keeps LFS objects retained from an earlier capture.
func (b *Base) CheckoutRevision(gitdir, spec string) (*git.Revision, error) {
	return git.ResolveCheckoutRevision(gitdir, spec, filepath.Join(b.config.StateDir, lfsDir))
}

// reviseCapture substitutes tree metadata, not a materialized checkout. The
// capture's stat identities remain usable only for content identical to REV.
// LFS pointers, under REV's attributes, present their objects (see lfsFile).
// Its index has no stat data yet: seedRevisionIndex adds it once takes are done.
func reviseCapture(ctx context.Context, c Config, res *source.Result, revision *git.Revision, set rules.Set, fork time.Time, taking bool) (revisionProjection, error) {
	repo := res.Domains.Repo("")
	private, err := revision.PreparePrivateObjects(ctx, repo, revisionObjects(c))
	if err != nil {
		return revisionProjection{}, err
	}
	index := filepath.Join(c.StateDir, revisionIndex)
	if err := revision.WriteIndex(ctx, index, nil, nil); err != nil {
		return revisionProjection{}, err
	}
	repos := []source.Repo{{Path: "", Head: revision.Commit, Index: index}}
	domains, err := source.NewDomains(res.Root, repos, nil)
	if err != nil {
		return revisionProjection{}, err
	}
	repo = domains.Repo("")
	if private {
		repo = repo.WithObjectDirectory(revisionObjects(c))
	}
	objects, err := repo.Objects()
	if err != nil {
		return revisionProjection{}, err
	}
	defer objects.Close()
	var privateLFS map[string]git.LFSPointer
	if private {
		privateLFS = make(map[string]git.LFSPointer)
	}
	tree, err := treeFiles(revision.Entries, *res, repo, git.AttributeOptions{Source: revision.Commit}, fork, privateLFS)
	if err != nil {
		return revisionProjection{}, err
	}
	files, info := tree.files, tree.info
	view := &revisionView{files: files, info: info, dirs: map[string]revisionDir{}, objects: objects}
	for p := range files {
		for dir := path.Dir(p); dir != "."; dir = path.Dir(dir) {
			if _, ok := view.dirs[dir]; ok {
				break
			}
			view.dirs[dir] = revisionDir{}
		}
	}
	projection := view.project(set, domains)
	projection.private = private
	// Gitlinks start empty in revision checkouts. Their contents belong to the
	// submodule and must not be judged by the superproject's ignore rules.
	gitlinks := map[string]bool{}
	for _, entry := range revision.Entries {
		if entry.Type == "commit" {
			gitlinks[entry.Path] = true
		}
	}
	blocked := func(p string) bool {
		if len(projection.hidden) == 0 && len(gitlinks) == 0 {
			return false
		}
		for ; p != "." && p != ""; p = path.Dir(p) {
			if _, hidden := projection.hidden[p]; hidden || gitlinks[p] {
				return true
			}
		}
		return false
	}
	ignored := func(p string, isDir bool) bool {
		var mode os.FileMode
		if isDir {
			mode = os.ModeDir
		}
		return domains.Facts(view, p, capturedInfo{mode: mode}).Ignored()
	}
	roots := map[string]rules.Class{}
	folder := inheritedView{Dir: source.Dir(res.Root), revision: view}
	matchInherited := func(p string, info os.FileInfo) rules.Class {
		return projection.match(set, p, domains.Facts(folder, p, info))
	}
	// Tracked ancestors already have REV's decision. Classify each remaining
	// folder directory once, stopping at the first non-source ancestor.
	dirClasses := map[string]rules.Class{}
	var dirClass func(string) rules.Class
	dirClass = func(p string) rules.Class {
		if p == "." {
			return rules.Source
		}
		if dir, ok := view.dirs[p]; ok {
			return dir.class
		}
		if _, leaf := files[p]; leaf {
			return rules.Hidden // a revision leaf cannot hold inherited entries
		}
		if class, ok := dirClasses[p]; ok {
			return class
		}
		class := dirClass(path.Dir(p))
		if class == rules.Source {
			class = matchInherited(p, capturedInfo{mode: os.ModeDir})
		}
		dirClasses[p] = class
		return class
	}
	dirs, parents := map[string]bool{}, map[string]bool{}
	keepDir := func(dir string) {
		if dir != "." {
			dirs[dir] = true
			for p := path.Dir(dir); p != "." && !parents[p]; p = path.Dir(p) {
				parents[p] = true
			}
		}
	}
	// A retained ignored directory that REV explicitly classifies as source
	// cannot pass through wholesale: a child may have its own hidden rule.
	retain := func(p string, dir bool, info os.FileInfo, fallback rules.Class) bool {
		var class rules.Class
		if dir {
			class = dirClass(p)
		} else if class = dirClass(path.Dir(p)); class == rules.Source {
			class = matchInherited(p, info)
		}
		if class == rules.Hidden {
			return true
		}
		if dir && class == rules.Source && fallback != rules.Hidden {
			keepDir(p)
			return false
		}
		roots[p] = fallback
		return true
	}
	// Ignored roots remain live and copy-on-write, and hidden ones absent, just
	// as in an ordinary checkout. Nonignored roots can contain ignored
	// descendants; descend only those roots, never a retained ignored build
	// directory.
	keep := func(p string, class rules.Class) error {
		return filepath.WalkDir(filepath.Join(res.Root, filepath.FromSlash(p)), func(host string, entry fs.DirEntry, walkErr error) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if walkErr != nil {
				if os.IsNotExist(walkErr) {
					return nil
				}
				return walkErr
			}
			rel, err := filepath.Rel(res.Root, host)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			if blocked(rel) {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if !view.tracked(rel) && ignored(rel, entry.IsDir()) {
				if retain(rel, entry.IsDir(), nil, class) && entry.IsDir() {
					return filepath.SkipDir
				}
			}
			// A tracked file replaces anything formerly beneath it.
			if f, ok := files[rel]; ok && f.Mode != store.ModeDir && entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		})
	}
	for p, class := range res.Roots {
		if path.Base(p) == ".git" || blocked(p) {
			continue // discarded nested repositories need no object-directory walk; gitlinks start empty
		}
		if class == rules.Hidden && !view.tracked(p) {
			// REV tracks nothing here; the captured hidden root stays unwalked.
			roots[p] = rules.Hidden
			continue
		}
		if err := keep(p, class); err != nil {
			return revisionProjection{}, err
		}
	}
	for p, f := range res.Files {
		if !view.tracked(p) && !blocked(p) && ignored(p, f.Mode == store.ModeDir) {
			var mode os.FileMode
			if f.Mode == store.ModeSymlink {
				mode = os.ModeSymlink
			}
			retain(p, f.Mode == store.ModeDir, capturedInfo{mode: mode, size: res.Info[p].Size}, rules.Local)
		}
	}
	for p := range projection.hidden {
		roots[p] = rules.Hidden
	}
	// Preserve the source directories of inherited roots and projected children.
	// Only the deepest directories without visible leaves need empty tree entries.
	for p, class := range roots {
		if class == rules.Hidden && !view.tracked(p) {
			continue // a hidden root from the folder cannot create a directory in REV
		}
		keepDir(path.Dir(p))
	}
	for dir := range dirs {
		if _, leaf := files[dir]; !leaf && !view.dirs[dir].visible && !parents[dir] {
			files[dir] = store.File{Mode: store.ModeDir, ID: store.EmptyTree}
		}
	}
	// Discard hidden roots from the folder whose parents REV left out.
	for p, class := range roots {
		dir := path.Dir(p)
		if class != rules.Hidden || dir == "." || dirs[dir] || parents[dir] {
			continue
		}
		if f, ok := files[dir]; !view.tracked(dir) || ok && f.Mode != store.ModeDir {
			delete(roots, p)
		}
	}
	for p := range projection.skipped {
		if _, root := projection.hidden[p]; taking && root && path.Base(p) == ".gitignore" {
			if projection.ignores == nil {
				projection.ignores = map[string]store.OID{}
			}
			projection.ignores[p] = files[p].ID
		}
		delete(files, p)
		delete(info, p)
		delete(tree.lfs, p)
	}
	for p, pointer := range privateLFS {
		if tree.lfs[p] {
			if err := retainPrivateLFS(c, revision, pointer); err != nil {
				return revisionProjection{}, err
			}
		}
	}
	res.Files, res.Info, res.Roots, res.Repos, res.Domains, res.LFS = files, info, roots, repos, domains, tree.lfs
	res.Tree, err = c.Store.Build(files)
	return projection, err
}

// retainPrivateLFS keeps locally stored objects that could otherwise disappear
// with the checkout that committed their pointer. Objects absent there still
// use the ordinary host LFS storage or smudge path.
func retainPrivateLFS(c Config, revision *git.Revision, pointer git.LFSPointer) error {
	dir := filepath.Join(c.StateDir, lfsDir)
	dst := filepath.Join(dir, pointer.OID)
	if _, err := os.Stat(dst); err == nil {
		return nil // several retained paths may name the same object
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	sources, err := revision.PrivateLFSPaths(pointer.OID)
	if err != nil {
		return err
	}
	for _, src := range sources {
		f, ok := openSized(src, pointer.Size)
		if !ok {
			continue
		}
		defer f.Close()
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		return clone.FD(f, dst, 0o600)
	}
	return nil
}

// seedRevisionIndex rewrites the revision index with the stat data the view
// presents for every file it serves with exactly REV's blob and mode (an LFS
// pointer's object, which git's clean filter stores as that blob), so git's
// first refresh trusts those files instead of reading each one. It runs on the
// final capture: a path taken with other content, a gitlink, an LFS pointer
// git's clean filter would rewrite, and other paths not serving REV's content
// keep zero stat data. Only hidden paths are skipped by git's refresh.
func seedRevisionIndex(ctx context.Context, c Config, res source.Result, revision *git.Revision, skipped map[string]bool, fork time.Time) error {
	stat := make(map[string]git.IndexStat, len(revision.Entries)-len(skipped))
	for _, e := range revision.Entries {
		if res.Files[e.Path] != (store.File{Mode: e.Mode, ID: store.OID(e.OID)}) {
			continue
		}
		info := res.Info[e.Path]
		if res.LFS[e.Path] && info.Mtime.IsZero() {
			continue // a noncanonical pointer, presented without lfsMtime (treeFiles)
		}
		stat[e.Path] = git.IndexStat{Size: info.Size, Mtime: presentedMtime(info, fork)}
	}
	return revision.WriteIndex(ctx, filepath.Join(c.StateDir, revisionIndex), stat, skipped)
}
