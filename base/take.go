package base

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/stencil-hq/ovnfs/git"
	"github.com/stencil-hq/ovnfs/rules"
	"github.com/stencil-hq/ovnfs/source"
	"github.com/stencil-hq/ovnfs/store"
)

// ErrTake identifies an invalid take source or a path absent from both source and base.
var ErrTake = errors.New("invalid take")

// Take selects a path from the captured working tree ("."), index (""), or resolved revision.
type Take struct {
	Source   string
	Path     string
	Revision *git.Revision
}

// takeView reads classification files from the unchanged base, never from a taken ignore file.
type takeView struct {
	source.Dir
	files   map[string]store.File
	info    map[string]source.Info
	dirs    map[string]bool
	ignores map[string]store.OID
	store   *store.Store
	domains *source.Domains
	objects map[*git.Repo]*git.Objects
}

func (v *takeView) Lstat(p string) (os.FileInfo, error) {
	if f, ok := v.files[p]; ok {
		mode := os.FileMode(0o644)
		switch f.Mode {
		case store.ModeDir:
			mode = os.ModeDir | 0o755
		case store.ModeExec:
			mode = 0o755
		case store.ModeSymlink:
			mode = os.ModeSymlink | 0o777
		}
		return capturedInfo{name: path.Base(p), mode: mode, size: v.info[p].Size}, nil
	}
	if v.dirs[p] {
		return capturedInfo{name: path.Base(p), mode: os.ModeDir | 0o755}, nil
	}
	return v.Dir.Lstat(p)
}

func (v *takeView) ReadFile(p string) ([]byte, error) {
	id := v.files[p].ID
	if id == "" {
		id = v.ignores[p]
	}
	if id == "" {
		return nil, os.ErrNotExist
	}
	if v.store.Has(id) {
		return v.store.ReadBlob(id)
	}
	repo := v.domains.RepoOf(p)
	if repo == nil {
		return nil, os.ErrNotExist
	}
	objects := v.objects[repo]
	if objects == nil {
		var err error
		objects, err = repo.Objects()
		if err != nil {
			return nil, err
		}
		v.objects[repo] = objects
	}
	return objects.Blob(string(id))
}

func takeCapture(ctx context.Context, c Config, set rules.Set, res *source.Result, work source.Result, takes []Take, ignores map[string]store.OID, fork time.Time) error {
	if len(takes) == 0 {
		return nil
	}
	// Freeze classification and the working-tree source before any ordered substitutions.
	files := make(map[string]store.File, len(res.Files))
	info := make(map[string]source.Info, len(res.Info))
	view := &takeView{Dir: source.Dir(res.Root), files: res.Files, info: res.Info, dirs: map[string]bool{}, ignores: ignores, store: c.Store, domains: res.Domains, objects: map[*git.Repo]*git.Objects{}}
	for p, f := range res.Files {
		files[p] = f
		for dir := path.Dir(p); dir != "." && !view.dirs[dir]; dir = path.Dir(dir) {
			view.dirs[dir] = true
		}
	}
	maps.Copy(info, res.Info)
	defer func() {
		for _, objects := range view.objects {
			objects.Close()
		}
	}()
	rootDirs, rootParents := map[string]bool{}, map[string]bool{}
	for root := range res.Roots {
		dir := path.Dir(root)
		if dir == "." {
			continue
		}
		rootDirs[dir] = true
		for parent := path.Dir(dir); parent != "." && !rootParents[parent]; parent = path.Dir(parent) {
			rootParents[parent] = true
		}
	}
	protected := func(p string) bool {
		for prefix := p; prefix != "."; prefix = path.Dir(prefix) {
			if prefix == ".git" || prefix == ".jj" {
				return true
			}
			if _, ok := res.Roots[prefix]; ok {
				return true
			}
		}
		return false
	}
	var index *gitFiles
	revisions := map[*git.Revision]gitFiles{}
	for _, take := range takes {
		if err := ctx.Err(); err != nil {
			return err
		}
		src, sizes := work.Files, work.Info
		var pointers map[string]bool
		if take.Source != "." {
			root := work.Domains.Repo("")
			if root == nil {
				if take.Revision == nil {
					return fmt.Errorf("%w: index source needs a git repository at the folder root", ErrTake)
				}
				return fmt.Errorf("%w: revision source needs a git repository at the folder root", ErrTake)
			}
			var tree gitFiles
			if take.Revision != nil {
				var ok bool
				if tree, ok = revisions[take.Revision]; !ok {
					var err error
					if tree, err = treeFiles(take.Revision.Entries, work, root, git.AttributeOptions{Source: take.Revision.Commit}, fork, nil); err != nil {
						return err
					}
					revisions[take.Revision] = tree
				}
			} else {
				if index == nil {
					var entries []git.TreeEntry
					// Git checks files out of the index under the index's own attributes.
					attrs := git.AttributeOptions{Checkout: true}
					for _, repo := range work.Repos {
						if repo.Path == "" && repo.Index != "" {
							var err error
							entries, err = root.IndexTree(ctx, repo.Index)
							if err != nil {
								return err
							}
							captured, err := root.ReadIndex(repo.Index)
							if err != nil {
								return err
							}
							attrs.Index = &captured
						}
					}
					files, err := treeFiles(entries, work, root, attrs, fork, nil)
					if err != nil {
						return err
					}
					index = &files
				}
				tree = *index
			}
			src, sizes, pointers = tree.files, tree.info, tree.lfs
		}
		inSource := containsPath(src, take.Path) || (take.Source == "." && containsRoot(work.Root, work.Roots, take.Path))
		if !inSource && !containsPath(files, take.Path) && !containsRoot(res.Root, res.Roots, take.Path) {
			return fmt.Errorf("%w: path %q is absent from source %q and the base", ErrTake, take.Path, take.Source)
		}
		for p := range files {
			if within(p, take.Path) && !protected(p) {
				delete(files, p)
				delete(info, p)
				delete(res.BlobRepo, p)
				delete(res.LFS, p)
			}
		}
		for p, f := range src {
			if !within(p, take.Path) || protected(p) {
				continue
			}
			// Existing source paths stay source. New paths use the baseline's
			// rules and ignore files, with the selected source's type and size.
			if _, exists := view.files[p]; !exists {
				d := set.Classify(p, func(prefix string) rules.Facts {
					var stat os.FileInfo
					if sf, ok := src[prefix]; ok {
						mode := os.FileMode(0o644)
						if sf.Mode == store.ModeDir {
							mode = os.ModeDir | 0o755
						} else if sf.Mode == store.ModeSymlink {
							mode = os.ModeSymlink | 0o777
						}
						stat = capturedInfo{name: path.Base(prefix), mode: mode, size: sizes[prefix].Size}
					} else if prefix != p {
						stat = capturedInfo{name: path.Base(prefix), mode: os.ModeDir | 0o755}
					}
					return res.Domains.Facts(view, prefix, stat)
				})
				if d.Class != rules.Source {
					continue
				}
			}
			if f.Mode != store.ModeDir && (rootDirs[p] || rootParents[p]) {
				continue
			}
			for dir := path.Dir(p); dir != "."; dir = path.Dir(dir) {
				delete(files, dir)
				delete(info, dir)
				delete(res.BlobRepo, dir)
				delete(res.LFS, dir)
			}
			files[p], info[p] = f, sizes[p]
			delete(res.BlobRepo, p)
			if pointers[p] {
				if res.LFS == nil {
					res.LFS = map[string]bool{}
				}
				res.LFS[p] = true
			}
			if f.Mode != store.ModeDir {
				origin := work.Domains.Repo("")
				if take.Source == "." {
					origin = work.Domains.RepoOf(p)
				}
				current := res.Domains.RepoOf(p)
				if origin != nil && (current == nil || current.Top != origin.Top) {
					rel, err := filepath.Rel(res.Root, origin.Top)
					if err != nil {
						return err
					}
					if rel == "." {
						rel = ""
					}
					if res.BlobRepo == nil {
						res.BlobRepo = map[string]string{}
					}
					res.BlobRepo[p] = filepath.ToSlash(rel)
				}
			}
		}
	}
	// Live and hidden roots must retain their ancestor directories even when
	// the selected source removed their nonignored siblings.
	for dir := range rootDirs {
		if !rootParents[dir] && !containsPath(files, dir) {
			files[dir] = store.File{Mode: store.ModeDir, ID: store.EmptyTree}
		}
	}
	var err error
	res.Files, res.Info = files, info
	res.Tree, err = c.Store.Build(files)
	return err
}

// gitFiles are the files git's checkout of a tree or index writes: the
// leaves, their stat data, and the Git LFS pointers among them
// (source.Result.LFS).
type gitFiles struct {
	files map[string]store.File
	info  map[string]source.Info
	lfs   map[string]bool
}

// treeFiles describes checking out entries of repo, whose lfs filter reads
// the attributes attrs selects. An LFS pointer presents its object's size and,
// when git's clean filter gives it back unchanged, lfsMtime: git trusts that
// object unread. One whose pointer git-lfs would rewrite shows the fork time
// and is never seeded (seedRevisionIndex), so git reads it and reports the
// rewrite. A file the work tree holds unchanged keeps its stat data; other
// content presents its blob's size. When pointers is not nil, it also receives
// the parsed LFS pointers so their local objects can be retained.
func treeFiles(entries []git.TreeEntry, work source.Result, repo *git.Repo, attrs git.AttributeOptions, fork time.Time, pointers map[string]git.LFSPointer) (gitFiles, error) {
	lfs, err := newLFSFinder(repo, attrs)
	if err != nil {
		return gitFiles{}, err
	}
	defer lfs.Close()
	t := gitFiles{files: make(map[string]store.File, len(entries)), info: make(map[string]source.Info, len(entries)), lfs: map[string]bool{}}
	for _, e := range entries {
		f := store.File{Mode: e.Mode, ID: store.OID(e.OID)}
		if e.Type == "commit" {
			f = store.File{Mode: store.ModeDir, ID: store.EmptyTree}
		}
		t.files[e.Path] = f
		pointer, canonical, isPointer, err := lfs.object(e)
		switch {
		case err != nil:
			return gitFiles{}, err
		case isPointer:
			t.info[e.Path] = source.Info{Size: pointer.Size}
			if canonical {
				t.info[e.Path] = source.Info{Size: pointer.Size, Mtime: lfsMtime(fork)}
			}
			t.lfs[e.Path] = true
			if pointers != nil {
				pointers[e.Path] = pointer
			}
		case work.Files[e.Path] == f:
			t.info[e.Path] = work.Info[e.Path]
		default:
			t.info[e.Path] = source.Info{Size: e.Size}
		}
	}
	return t, nil
}

func within(p, root string) bool {
	return root == "" || p == root || strings.HasPrefix(p, root+"/")
}

func containsPath(files map[string]store.File, p string) bool {
	if p == "" {
		return true
	}
	for file := range files {
		if within(file, p) {
			return true
		}
	}
	return false
}

func containsRoot(dir string, roots map[string]rules.Class, p string) bool {
	for root := range roots {
		if within(root, p) {
			return true
		}
		if within(p, root) {
			_, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(p)))
			return err == nil
		}
	}
	return false
}
