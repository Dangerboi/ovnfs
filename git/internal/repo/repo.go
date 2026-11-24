// Package repo locates and opens git repositories the way git itself does:
// discovery from a directory inside a work tree (gitfiles, linked worktrees,
// core.worktree, GIT_CEILING_DIRECTORIES, filesystem boundaries,
// safe.directory and repository format checks), opening an explicit git
// directory, and creating a bare repository. A Repository then hands out the
// pieces other packages work on: its refs, configuration, shallow set, object
// database (with replace refs applied), committer identity and reflog policy.
//
// Everything here is written from git's documentation (gitrepository-layout,
// git, git-config, git-worktree, git-replace, git-commit-tree,
// technical/repository-version) and from observing the git binary. The
// environment variables that redirect a repository (GIT_DIR, GIT_WORK_TREE,
// GIT_COMMON_DIR, GIT_INDEX_FILE, GIT_OBJECT_DIRECTORY,
// GIT_ALTERNATE_OBJECT_DIRECTORIES) are deliberately ignored.
package repo

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/stencil-hq/ovnfs/git/internal/config"
	"github.com/stencil-hq/ovnfs/git/internal/index"
	"github.com/stencil-hq/ovnfs/git/internal/object"
	"github.com/stencil-hq/ovnfs/git/internal/refs"
	"github.com/stencil-hq/ovnfs/git/internal/rev"
)

var (
	// ErrNotRepository reports a directory that is not inside any git
	// repository git would find from it.
	ErrNotRepository = errors.New("not a git repository")
	// ErrNoWorkTree reports a directory that belongs to a repository but not
	// to its work tree: inside a git directory, in a bare repository, or
	// outside the directory core.worktree names.
	ErrNoWorkTree = errors.New("not inside a work tree")
	// ErrUnsafe reports a repository owned by someone other than the current
	// user and not listed in safe.directory.
	ErrUnsafe = errors.New("dubious ownership")
	// ErrUnsupportedFormat reports a repository whose format version or
	// extensions git refuses to operate on.
	ErrUnsupportedFormat = errors.New("unsupported repository format")
	// ErrInvalidGitfile reports a .git file that is malformed or does not
	// point to a valid git directory; git stops there instead of searching on.
	ErrInvalidGitfile = errors.New("invalid gitfile")
)

// Repository is a located repository. It is a plain value: it holds no open
// files and every method reads the current state from disk.
type Repository struct {
	// Top is the top of the work tree. It is "" for a bare repository, and
	// may be "" for a non-bare one opened by its git directory alone (no
	// core.worktree, not named .git): its work tree is wherever git runs.
	Top string
	// GitDir is the git directory: <Top>/.git, a gitfile's target, or
	// <CommonDir>/worktrees/<id> for a linked worktree.
	GitDir string
	// CommonDir holds what all worktrees share (objects, refs, config).
	CommonDir string
	// Bare reports a repository without a work tree.
	Bare bool
	// Format is the object hash format (extensions.objectFormat).
	Format object.Format
	// RefBackend is the ref storage format (extensions.refStorage).
	RefBackend refs.Backend
	// IndexPath is the work tree's index file, <GitDir>/index.
	IndexPath string

	refs      *refs.Store
	objectDir string
}

// newRepository fills in the derived parts of a Repository.
func newRepository(top, gitDir, commonDir string, bare bool, s settings) (*Repository, error) {
	store, err := refs.Open(refs.Options{GitDir: gitDir, CommonDir: commonDir, Format: s.format, Backend: s.backend})
	if err != nil {
		return nil, err
	}
	return &Repository{
		Top:        top,
		GitDir:     gitDir,
		CommonDir:  commonDir,
		Bare:       bare,
		Format:     s.format,
		RefBackend: s.backend,
		IndexPath:  filepath.Join(gitDir, "index"),
		refs:       store,
	}, nil
}

// Open opens the git directory at path, as `git --git-dir=<path>` would: a
// bare repository, a work tree's .git directory, a linked worktree's
// <common>/worktrees/<id>, or a .git file pointing at one of these. Paths are
// resolved to their physical form. The safe.directory ownership check is not
// applied: callers name the directory explicitly.
//
// Top is core.worktree when set; else the gitfile's directory when path is a
// gitfile, the worktree a linked git directory's "gitdir" file records, or
// the parent of a directory named .git. The repository is bare when
// core.bare says so or, with core.bare unset, when it is none of these.
func Open(path string) (*Repository, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	p, err := realPath(abs)
	if err != nil {
		return nil, fmt.Errorf("repo: %s: %w", abs, ErrNotRepository)
	}
	fi, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	gitDir, gitfile := p, ""
	if fi.Mode().IsRegular() {
		gitfile = p
		if gitDir, err = readGitfile(p); err != nil {
			return nil, err
		}
	}
	commonDir, err := gitDirLayout(gitDir)
	if errors.Is(err, errNotGitDir) {
		if gitfile != "" {
			return nil, fmt.Errorf("repo: %s: %w: does not point to a valid repository", gitfile, ErrInvalidGitfile)
		}
		return nil, fmt.Errorf("repo: %s: %w", gitDir, ErrNotRepository)
	}
	if err != nil {
		return nil, err
	}
	return openGitDir(gitDir, commonDir, gitfile)
}

// openGitDir builds the Repository for a validated git directory.
func openGitDir(gitDir, commonDir, gitfile string) (*Repository, error) {
	s, err := readSettings(gitDir, commonDir)
	if err != nil {
		return nil, err
	}
	linked := gitDir != commonDir
	bare := gitfile == "" && !linked && filepath.Base(gitDir) != ".git"
	if s.bareSet {
		bare = s.bare
	}
	if bare {
		return newRepository("", gitDir, commonDir, true, s)
	}
	var top string
	switch {
	case s.worktreeSet:
		if top, err = worktreePath(gitDir, s.worktree); err != nil {
			return nil, err
		}
	case gitfile != "":
		top = filepath.Dir(gitfile)
	case linked:
		top = linkedTop(gitDir)
	case filepath.Base(gitDir) == ".git":
		top = filepath.Dir(gitDir)
	}
	return newRepository(top, gitDir, commonDir, false, s)
}

// linkedTop returns the work tree of a linked worktree's git directory from
// its "gitdir" file (the path of the worktree's .git file, relative paths
// being relative to the git directory), or "" when it is unreadable.
func linkedTop(gitDir string) string {
	data, err := os.ReadFile(filepath.Join(gitDir, "gitdir"))
	if err != nil {
		return ""
	}
	p := trimEOL(string(data))
	if p == "" {
		return ""
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(gitDir, p)
	}
	return physical(filepath.Dir(p))
}

// Worktrees lists every work tree of the repository's common directory: the
// main one first (unless the repository is bare), then each linked worktree
// under <CommonDir>/worktrees in name order. Each is a Repository whose
// Refs() resolves per-worktree refs such as HEAD in that worktree. Linked
// worktree directories that are no longer valid git directories are skipped.
func (r *Repository) Worktrees() ([]*Repository, error) {
	var out []*Repository
	main, err := openGitDir(r.CommonDir, r.CommonDir, "")
	if err != nil {
		return nil, err
	}
	if !main.Bare {
		out = append(out, main)
	}
	entries, err := os.ReadDir(filepath.Join(r.CommonDir, "worktrees"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		gitDir := filepath.Join(r.CommonDir, "worktrees", e.Name())
		commonDir, err := gitDirLayout(gitDir)
		if errors.Is(err, errNotGitDir) {
			continue
		}
		if err != nil {
			return nil, err
		}
		wt, err := openGitDir(gitDir, commonDir, "")
		if err != nil {
			return nil, err
		}
		out = append(out, wt)
	}
	return out, nil
}

// Refs returns the repository's reference store, routing per-worktree refs
// to GitDir and shared ones to CommonDir.
func (r *Repository) Refs() *refs.Store { return r.refs }

// Config loads the configuration git sees in this repository (system,
// global, local, worktree and command scopes with includes), fresh on each
// call; includeIf onbranch: conditions see the branch HEAD points to.
func (r *Repository) Config() (*config.Config, error) {
	return config.Load(config.Options{
		GitDir:    r.GitDir,
		CommonDir: r.CommonDir,
		WorkTree:  r.Top,
		Branch:    r.headBranch,
	})
}

// headBranch returns the ref HEAD points to, or "" when HEAD is detached.
func (r *Repository) headBranch() (string, error) {
	name, err := r.refs.Symbolic("HEAD")
	if errors.Is(err, refs.ErrNotSymbolic) {
		return "", nil
	}
	return name, err
}

// Shallow returns the commits recorded in <CommonDir>/shallow, whose parents
// are absent from a shallow clone. A missing file yields an empty set.
func (r *Repository) Shallow() (map[object.ID]bool, error) {
	name := filepath.Join(r.CommonDir, "shallow")
	data, err := os.ReadFile(name)
	if errors.Is(err, os.ErrNotExist) {
		return map[object.ID]bool{}, nil
	}
	if err != nil {
		return nil, err
	}
	set := map[object.ID]bool{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for line := 1; sc.Scan(); line++ {
		text := strings.TrimSuffix(sc.Text(), "\r")
		if text == "" {
			continue
		}
		id, err := object.ParseIDFormat(r.Format, text)
		if err != nil {
			return nil, fmt.Errorf("repo: %s:%d: %w", name, line, err)
		}
		set[id] = true
	}
	return set, sc.Err()
}

// WithObjectDirectory returns a read view of r with a different object database.
func (r *Repository) WithObjectDirectory(dir string) *Repository {
	view := *r
	view.objectDir = dir
	return &view
}

// OpenDB opens the repository's object database (its objects directory and
// alternates) without replace refs, as transfers and reachability walks need
// it. The caller closes it.
func (r *Repository) OpenDB() (*object.DB, error) {
	dir := r.objectDir
	if dir == "" {
		dir = filepath.Join(r.CommonDir, "objects")
	}
	return object.Open(dir, r.Format)
}

// LogPolicy maps core.logAllRefUpdates onto the reflog creation policy:
// "always" is LogAlways, true LogDefault, false LogExisting; unset means
// LogDefault in a repository with a work tree and LogExisting in a bare one.
func (r *Repository) LogPolicy() (refs.LogPolicy, error) {
	cfg, err := r.Config()
	if err != nil {
		return 0, err
	}
	return logPolicy(cfg, r.Bare)
}

func logPolicy(cfg *config.Config, bare bool) (refs.LogPolicy, error) {
	e, ok := cfg.Lookup("core.logAllRefUpdates")
	if !ok {
		if bare {
			return refs.LogExisting, nil
		}
		return refs.LogDefault, nil
	}
	if e.HasValue && strings.EqualFold(e.Value, "always") {
		return refs.LogAlways, nil
	}
	v, err := e.Bool()
	if err != nil {
		return 0, err
	}
	if v {
		return refs.LogDefault, nil
	}
	return refs.LogExisting, nil
}

// RevSource assembles what git/internal/rev resolves revisions against,
// reading objects through objects (which the caller keeps open and closes).
// The index is offered only for a repository with a work tree.
func (r *Repository) RevSource(objects *Objects) (rev.Source, error) {
	cfg, err := r.Config()
	if err != nil {
		return rev.Source{}, err
	}
	shallow, err := r.Shallow()
	if err != nil {
		return rev.Source{}, err
	}
	src := rev.Source{
		Refs:     r.refs,
		Objects:  objects,
		Config:   cfg,
		Shallow:  shallow,
		Replaced: objects.Replaced(),
	}
	if !r.Bare {
		path, format := r.IndexPath, r.Format
		src.Index = func() (*index.Index, error) { return index.Read(path, format, index.ReadOptions{}) }
	}
	return src, nil
}
