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
	f, ok := v.files[p]
	if !ok {
		return nil, os.ErrNotExist
	}
	if v.store.Has(f.ID) {
		return v.store.ReadBlob(f.ID)
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
	return objects.Blob(string(f.ID))
}

func takeCapture(ctx context.Context, c Config, set rules.Set, res *source.Result, work source.Result, takes []Take) error {
	if len(takes) == 0 {
		return nil
	}
	// Freeze classification and the working-tree source before any ordered substitutions.
	files := make(map[string]store.File, len(res.Files))
	info := make(map[string]source.Info, len(res.Info))
	view := &takeView{Dir: source.Dir(res.Root), files: res.Files, info: res.Info, dirs: map[string]bool{}, store: c.Store, domains: res.Domains, objects: map[*git.Repo]*git.Objects{}}
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
	var indexFiles map[string]store.File
	var indexInfo map[string]source.Info
	type selectedTree struct {
		files map[string]store.File
		info  map[string]source.Info
	}
	revisions := map[*git.Revision]selectedTree{}
	for _, take := range takes {
		if err := ctx.Err(); err != nil {
			return err
		}
		src, sizes := work.Files, work.Info
		if take.Source != "." {
			if work.Domains.Repo("") == nil {
				if take.Revision == nil {
					return fmt.Errorf("%w: index source needs a git repository at the folder root", ErrTake)
				}
				return fmt.Errorf("%w: revision source needs a git repository at the folder root", ErrTake)
			}
			if take.Revision != nil {
				tree, ok := revisions[take.Revision]
				if !ok {
					tree.files, tree.info = treeFiles(take.Revision.Entries, work)
					revisions[take.Revision] = tree
				}
				src, sizes = tree.files, tree.info
			} else {
				if indexFiles == nil {
					var entries []git.TreeEntry
					for _, repo := range work.Repos {
						if repo.Path == "" && repo.Index != "" {
							var err error
							entries, err = work.Domains.Repo("").IndexTree(ctx, repo.Index)
							if err != nil {
								return err
							}
						}
					}
					indexFiles, indexInfo = treeFiles(entries, work)
				}
				src, sizes = indexFiles, indexInfo
			}
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
			}
			files[p], info[p] = f, sizes[p]
			delete(res.BlobRepo, p)
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

func treeFiles(entries []git.TreeEntry, work source.Result) (map[string]store.File, map[string]source.Info) {
	files := make(map[string]store.File, len(entries))
	info := make(map[string]source.Info, len(entries))
	for _, e := range entries {
		f := store.File{Mode: e.Mode, ID: store.OID(e.OID)}
		if e.Type == "commit" {
			f = store.File{Mode: store.ModeDir, ID: store.EmptyTree}
		}
		files[e.Path] = f
		if work.Files[e.Path] == f {
			info[e.Path] = work.Info[e.Path]
		} else {
			info[e.Path] = source.Info{Size: e.Size}
		}
	}
	return files, info
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
