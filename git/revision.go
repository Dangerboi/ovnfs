package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/stencil-hq/ovnfs/git/internal/fetch"
	"github.com/stencil-hq/ovnfs/git/internal/index"
	"github.com/stencil-hq/ovnfs/git/internal/object"
	"github.com/stencil-hq/ovnfs/git/internal/rev"
)

// Revision is a commit resolved before a checkout is created. Branch is nonempty
// only when the requested revision names a local branch, not HEAD or a tag.
type Revision struct {
	Commit, Branch  string
	Entries         []TreeEntry
	repo            *Repo
	private         *Repo
	privateLFSCache string
	shallow         []object.ID
}

// ResolveRevision validates spec, any git revision naming a commit, without
// changing the repository.
func (r *Repo) ResolveRevision(ctx context.Context, spec string) (*Revision, error) {
	v, err := r.resolveRevision(spec)
	if err != nil {
		return nil, err
	}
	if v.Entries, err = r.lsTree(ctx, v.Commit, true); err != nil {
		return nil, err
	}
	return v, nil
}

func (r *Repo) resolveRevision(spec string) (*Revision, error) {
	v := &Revision{repo: r}
	err := r.session(func(s *rev.Source) error {
		id, err := rev.Resolve(s, spec+"^{commit}", rev.Options{EndOfOptions: true})
		if err != nil {
			return err
		}
		v.Commit = id.String()
		branch := spec
		if !strings.HasPrefix(branch, "refs/heads/") {
			branch = "refs/heads/" + branch
		}
		if tip, err := s.Refs.Resolve(branch); err == nil && tip.ID == id {
			v.Branch = branch
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return v, nil
}

// ResolveCheckoutRevision reads a checkout's private Git state directly,
// without discovering a repository through the mounted checkout. lfsCache is
// the capture's cache of LFS objects it has already retained.
func ResolveCheckoutRevision(gitdir, spec, lfsCache string) (*Revision, error) {
	r, err := OpenPrivate(gitdir)
	if err != nil {
		return nil, err
	}
	v, err := r.resolveRevision(spec)
	if err == nil {
		v.private = r
		v.privateLFSCache = lfsCache
	}
	return v, err
}

// PreparePrivateObjects imports the selected private revision's history into
// a new base's objects, returning false for a revision from the source folder.
// The new base borrows the host repository's objects, never the parent's.
func (v *Revision) PreparePrivateObjects(ctx context.Context, host *Repo, dir string) (bool, error) {
	if v.private == nil {
		return false, nil
	}
	if host == nil {
		return false, errors.New("the checkout's source folder no longer has its Git repository")
	}
	if host.repo.Format != v.repo.repo.Format {
		return false, errors.New("the checkout and its source folder use different Git object formats")
	}
	if err := host.PrepareObjects(dir); err != nil {
		return false, err
	}
	src, err := v.private.repo.OpenDB()
	if err != nil {
		return false, err
	}
	defer src.Close()
	dst, err := object.Open(dir, host.repo.Format)
	if err != nil {
		return false, err
	}
	defer dst.Close()
	id, err := object.ParseIDFormat(src.Format(), v.Commit)
	if err != nil {
		return false, err
	}
	shallow, err := fetch.ReadShallow(v.private.CommonDir)
	if err != nil {
		return false, err
	}
	result, err := fetch.Local(ctx, src, dst, []object.ID{id}, fetch.LocalOptions{
		SourceShallow: shallow,
		PackDir:       filepath.Join(dir, "pack"),
	})
	if err != nil {
		return false, fmt.Errorf("copying private revision %s: %w", v.Commit, err)
	}
	v.shallow = result.Shallow
	// The child inherits the host's config and refs, not the private refs
	// (for example replacements) that were only used to resolve spec.
	v.repo = host.WithObjectDirectory(dir)
	v.Entries, err = v.repo.lsTree(ctx, v.Commit, true)
	return true, err
}

// PrivateLFSPaths are the checkout-local and capture-owned paths of an LFS
// object, respectively. It is only used for revisions from private Git dirs.
func (v *Revision) PrivateLFSPaths(oid string) ([2]string, error) {
	local, err := v.private.LFSObject(oid)
	if err != nil {
		return [2]string{}, err
	}
	return [2]string{local, filepath.Join(v.privateLFSCache, oid)}, nil
}

// WithObjectDirectory returns a read view with the same config and refs but
// using dir for Git object lookups.
func (r *Repo) WithObjectDirectory(dir string) *Repo {
	return newRepo(r.repo.WithObjectDirectory(dir))
}

// IndexStat is the stat data a work tree presents for one file: what git
// compares under core.checkStat=minimal before trusting an entry unread.
type IndexStat struct {
	Size  int64
	Mtime time.Time
}

// WriteIndex writes the index `git read-tree` makes of the revision to the file
// name, a private index; it never checks out or copies files. An entry whose
// path is in stat records that stat data, so git's first refresh trusts the file
// instead of reading it; the caller must present exactly the revision's blob and
// mode there. Paths in skipped keep their entries but are marked skip-worktree.
// Other entries keep zero stat data, which git always reads. Entries whose
// mtime is in or after the second of the write are smudged as racily clean.
func (v *Revision) WriteIndex(ctx context.Context, name string, stat map[string]IndexStat, skipped map[string]bool) error {
	objects, err := v.repo.repo.OpenObjects()
	if err != nil {
		return err
	}
	defer objects.Close()
	commit, err := object.ParseIDFormat(v.repo.repo.Format, v.Commit)
	if err != nil {
		return err
	}
	tree, err := objects.Peel(commit, object.Tree)
	if err != nil {
		return err
	}
	idx, err := index.FromTree(treeReader(objects), tree, v.repo.repo.Format)
	if err != nil {
		return err
	}
	for i := range idx.Entries {
		e := &idx.Entries[i]
		if skipped[e.Path] {
			e.SkipWorktree = true
			continue
		}
		if s, ok := stat[e.Path]; ok {
			mtime := index.Time{Sec: uint32(s.Mtime.Unix()), Nsec: uint32(s.Mtime.Nanosecond())}
			e.Stat = index.Stat{MTime: mtime, Size: index.StatSize(s.Size)}
		}
	}
	// A fresh index is its own racy-git reference: a file presented with an
	// mtime in this second could still change without its stat data changing.
	return index.Write(name, idx, index.WriteOptions{RacyTime: time.Now()})
}

// SetHEAD attaches a private git directory to a local branch, or detaches it at
// the resolved commit. It never updates the source repository's refs.
func (v *Revision) SetHEAD(gitdir string) error {
	if len(v.shallow) != 0 {
		if err := fetch.UpdateShallow(gitdir, v.shallow, nil); err != nil {
			return err
		}
	}
	head := v.Commit
	if v.Branch != "" {
		head = "ref: " + v.Branch
		// Refs were copied separately from revision resolution. Pin the private
		// branch to the resolved commit even if the source moved meanwhile.
		name := filepath.Join(gitdir, filepath.FromSlash(v.Branch))
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(name, []byte(v.Commit+"\n"), 0o644); err != nil {
			return err
		}
	}
	return os.WriteFile(filepath.Join(gitdir, "HEAD"), []byte(head+"\n"), 0o644)
}
