package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/stencil-hq/ovnfs/git/internal/fetch"
	"github.com/stencil-hq/ovnfs/git/internal/index"
	"github.com/stencil-hq/ovnfs/git/internal/object"
	"github.com/stencil-hq/ovnfs/git/internal/refs"
	"github.com/stencil-hq/ovnfs/git/internal/repo"
	"github.com/stencil-hq/ovnfs/git/internal/rev"
	"github.com/stencil-hq/ovnfs/internal/hostfile"
)

// ErrBranchConflict identifies a branch or index that cannot be advanced safely;
// the error is a *BranchConflict saying why.
var ErrBranchConflict = errors.New("branch conflict")

// BranchConflict is why FetchBranch left a checked-out branch where it was. It
// says what stands in the way and leaves what to do about it to the caller,
// which knows what the user asked for.
type BranchConflict struct {
	Branch string
	// Diverged reports that Branch holds commits the checkout's HEAD lacks.
	Diverged bool
	// Staged lists the paths whose staged content the checkout's commits would
	// replace. With neither set, the branch stopped being the checked-out one
	// while it was being advanced.
	Staged []string
}

// Error names the obstacle.
func (e *BranchConflict) Error() string {
	switch {
	case e.Diverged:
		return e.Branch + " is not an ancestor of the checkout's HEAD"
	case len(e.Staged) != 0:
		return "staged changes in " + strings.Join(e.Staged, ", ")
	}
	return "the checked-out branch changed meanwhile"
}

// Is makes every BranchConflict match ErrBranchConflict.
func (e *BranchConflict) Is(target error) bool { return target == ErrBranchConflict }

// PartialCloneError reports that a checkout's commits cannot be copied into
// its folder's repository, a partial clone: they build on history fetched
// inside the checkout whose objects neither holds in full, and which only the
// promisor remote can give the folder's repository.
type PartialCloneError struct {
	Top string
	// Refs are the checkout's remote-tracking refs whose commits the folder's
	// repository lacks, as "<remote>/<branch>".
	Refs []string
	Err  error
}

// Error says what to fetch in the folder's repository first.
func (e *PartialCloneError) Error() string {
	fetch := "git -C " + e.Top + " fetch"
	if len(e.Refs) != 0 {
		remote, _, _ := strings.Cut(e.Refs[0], "/")
		fetch += " " + remote
	}
	what := "history fetched inside the checkout"
	if len(e.Refs) != 0 {
		what = strings.Join(e.Refs, ", ") + " as fetched inside the checkout"
	}
	return fmt.Sprintf("%s is a partial clone and lacks %s, which the checkout's commits build on; fetch it there first (%s), then run this again: %v", e.Top, what, fetch, e.Err)
}

// Unwrap returns git's failure.
func (e *PartialCloneError) Unwrap() error { return e.Err }

// ErrBranchDryRun preserves the unsupported dry run for branches not checked out.
var ErrBranchDryRun = errors.New("--branch has no dry run")

// BranchChange describes an index entry advanced with the checked-out branch.
type BranchChange struct {
	Path, Status string
}

// branchUpdate is one path the branch's tree diff changes; a side where the
// path is absent has mode 0.
type branchUpdate struct {
	BranchChange
	old, next object.TreeEntry
}

// FetchBranch fetches a checkout's commits from its private git directory into
// r's branch, as `git fetch <gitdir> HEAD:refs/heads/<branch>` would, running
// git for the fetch when it is installed. When the branch is checked out in r,
// only its index advances with it, and only where nothing is staged; the work
// tree is never touched.
func (r *Repo) FetchBranch(ctx context.Context, gitdir, branch string, dryRun bool) (string, []BranchChange, error) {
	return r.fetchBranch(ctx, gitdir, branch, "", "", dryRun)
}

// LandBranch checks or advances the checked-out branch for exactly the
// integration plan's original folder and checkout tips. The caller must
// have copied tip before advancing the branch.
func (r *Repo) LandBranch(ctx context.Context, gitdir, branch, before, tip string, dryRun bool) (string, []BranchChange, error) {
	return r.fetchBranch(ctx, gitdir, branch, before, tip, dryRun)
}

func (r *Repo) fetchBranch(ctx context.Context, gitdir, branch, before, want string, dryRun bool) (string, []BranchChange, error) {
	if err := refs.CheckBranchName(branch); err != nil {
		return "", nil, err
	}
	ref := "refs/heads/" + branch
	store := r.repo.Refs()
	head, err := symbolicHead(store)
	if err != nil {
		return "", nil, err
	}
	src, err := repo.Open(gitdir)
	if err != nil {
		return "", nil, err
	}
	if head != ref {
		if before != "" {
			return "", nil, &BranchConflict{Branch: branch}
		}
		if dryRun {
			return "", nil, ErrBranchDryRun
		}
		program, ok := gitProgram()
		partial, err := r.partial()
		if err != nil {
			return "", nil, err
		}
		if !ok || partial {
			// git fetch cannot take a partial clone's commits; see transfer.
			return r.fetchBranchRef(ctx, src, gitdir, ref)
		}
		if err := r.runGit(ctx, program, gitRun{}, "fetch", "--no-tags", "--quiet", gitdir, "HEAD:"+ref); err != nil {
			return "", nil, err
		}
		tip, err := store.Resolve(ref)
		if err != nil {
			return "", nil, err
		}
		return tip.ID.String(), nil, nil
	}
	// Keep the lock until the ref and index agree, including a failed ref CAS.
	// The replacement index has its own temporary name so the lock stays held
	// across both its atomic installation and any rollback.
	if !dryRun {
		lock, err := os.OpenFile(r.IndexPath()+".lock", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return "", nil, err
		}
		defer os.Remove(lock.Name())
		if err := lock.Close(); err != nil {
			return "", nil, err
		}
		if head, err = symbolicHead(store); err != nil {
			return "", nil, err
		}
		if head != ref {
			return "", nil, &BranchConflict{Branch: branch}
		}
	}
	tip, err := store.Resolve(ref)
	if err != nil {
		return "", nil, err
	}
	old := tip.ID
	if before != "" && old.String() != before {
		return "", nil, &BranchConflict{Branch: branch}
	}
	srcObjects, err := src.OpenObjects()
	if err != nil {
		return "", nil, err
	}
	defer srcObjects.Close()
	s, err := src.RevSource(srcObjects)
	if err != nil {
		return "", nil, err
	}
	next, err := rev.Resolve(&s, "HEAD^{commit}", rev.Options{})
	if err != nil {
		return "", nil, err
	}
	if want != "" && want != next.String() {
		return "", nil, fmt.Errorf("checkout HEAD changed from %s to %s", want, next)
	}
	// The private git directory reads r's objects through alternates, so it
	// holds both commits.
	fastForward, err := rev.IsAncestor(&s, old, next)
	if err != nil {
		return "", nil, err
	}
	if !fastForward {
		return "", nil, &BranchConflict{Branch: branch, Diverged: true}
	}
	diff, err := srcObjects.DiffTrees(old, next)
	if err != nil {
		return "", nil, err
	}
	updates := make([]branchUpdate, len(diff))
	for i, c := range diff {
		updates[i] = branchUpdate{Path: c.Path, Status: string(c.Status), old: c.Old, next: c.New}
	}
	objects, err := r.repo.OpenObjects()
	if err != nil {
		return "", nil, err
	}
	defer objects.Close()
	idx, err := index.Read(r.IndexPath(), r.repo.Format, index.ReadOptions{ExpandSparse: treeReader(objects)})
	missing := errors.Is(err, os.ErrNotExist)
	if missing {
		idx, err = index.New(r.repo.Format), nil
	}
	if err != nil {
		return "", nil, err
	}
	// Git treats entries modified no earlier than the index file as racily
	// clean. The replacement is checked against, and a rollback restores, the
	// replaced index's mtime, or a file edited right after it was staged would
	// read as unchanged.
	stamp := idx.ModTime
	entries := make(map[string][]index.Entry)
	for _, e := range idx.Entries {
		entries[e.Path] = append(entries[e.Path], e)
	}
	var conflicts []string
	changed := make(map[string]bool, len(updates))
	added := make(map[string]bool)
	for _, update := range updates {
		changed[update.Path] = true
		if update.old.Mode == 0 && update.next.Mode != 0 {
			added[update.Path] = true
		}
	}
	changes := make([]BranchChange, 0, len(updates))
	for _, update := range updates {
		staged := entries[update.Path]
		matches := len(staged) == 0 && update.old.Mode == 0
		if update.old.Mode != 0 {
			matches = len(staged) == 1 && staged[0].Stage == 0 && staged[0].Mode == update.old.Mode && staged[0].ID == update.old.ID
		}
		if !matches {
			conflicts = append(conflicts, update.Path)
		}
		changes = append(changes, update.BranchChange)
	}
	// A staged file/directory collision is also unrelated staged work, not
	// permission to silently remove that entry.
	collisions := make(map[string]bool)
	for path := range added {
		for parent := filepath.ToSlash(filepath.Dir(path)); parent != "."; parent = filepath.ToSlash(filepath.Dir(parent)) {
			if len(entries[parent]) != 0 && !changed[parent] {
				collisions[parent] = true
			}
		}
	}
	if len(added) != 0 {
		for path := range entries {
			if changed[path] {
				continue
			}
			for parent := filepath.ToSlash(filepath.Dir(path)); parent != "."; parent = filepath.ToSlash(filepath.Dir(parent)) {
				if added[parent] {
					collisions[path] = true
					break
				}
			}
		}
	}
	for path := range collisions {
		conflicts = append(conflicts, path)
	}
	if len(conflicts) != 0 {
		sort.Strings(conflicts)
		return "", nil, &BranchConflict{Branch: branch, Staged: conflicts}
	}
	if dryRun || old == next {
		return next.String(), changes, nil
	}
	if before == "" {
		if err := r.transfer(ctx, src, gitdir, next); err != nil {
			return "", nil, err
		}
	}
	if len(updates) == 0 {
		return next.String(), changes, r.updateBranch(ref, next, old, "")
	}
	candidate, err := os.CreateTemp(r.GitDir, "ovnfs-index-*")
	if err != nil {
		return "", nil, err
	}
	candidatePath := candidate.Name()
	defer os.Remove(candidatePath)
	if err := candidate.Close(); err != nil {
		return "", nil, err
	}
	var original []byte
	if !missing {
		if original, err = os.ReadFile(r.IndexPath()); err != nil {
			return "", nil, err
		}
	}
	// Remove changed entries before adding any so file/directory replacements
	// do not depend on the lexical ordering of the tree diff.
	paths := make([]string, len(updates))
	var adds []index.Entry
	for i, update := range updates {
		paths[i] = update.Path
		if update.next.Mode != 0 {
			adds = append(adds, index.Entry{Path: update.Path, ID: update.next.ID, Mode: update.next.Mode})
		}
	}
	if err := idx.Replace(paths, adds); err != nil {
		return "", nil, err
	}
	opts, err := r.indexWriteOptions(stamp)
	if err != nil {
		return "", nil, err
	}
	if err := index.WriteFile(candidatePath, idx, opts); err != nil {
		return "", nil, err
	}
	backup, err := os.CreateTemp(r.GitDir, "ovnfs-index-backup-*")
	if err != nil {
		return "", nil, err
	}
	defer os.Remove(backup.Name())
	if _, err := backup.Write(original); err != nil {
		backup.Close()
		return "", nil, err
	}
	if err := backup.Close(); err != nil {
		return "", nil, err
	}
	if !missing {
		if err := os.Chtimes(backup.Name(), stamp, stamp); err != nil {
			return "", nil, err
		}
	}
	// Editors and git itself keep the index open; a POSIX rename replaces it anyway.
	if err := hostfile.Rename(candidatePath, r.IndexPath()); err != nil {
		return "", nil, err
	}
	// Once the index is installed, only a cancellation noticed before the ref
	// moves rolls it back.
	refErr := ctx.Err()
	if refErr == nil {
		refErr = r.updateBranch(ref, next, old, "")
	}
	if refErr != nil {
		var rollback error
		if missing {
			rollback = os.Remove(r.IndexPath())
		} else {
			rollback = hostfile.Rename(backup.Name(), r.IndexPath())
		}
		if rollback != nil {
			return "", nil, fmt.Errorf("%w; restoring index: %v", refErr, rollback)
		}
		return "", nil, refErr
	}
	return next.String(), changes, nil
}

// fetchBranchRef stores the private git directory's HEAD commit as ref, a
// branch not checked out here, refusing what `git fetch` refuses: a branch
// checked out in another work tree and a non-fast-forward update.
func (r *Repo) fetchBranchRef(ctx context.Context, src *repo.Repository, gitdir, ref string) (string, []BranchChange, error) {
	if where, err := r.checkedOutAt(ref); err != nil {
		return "", nil, err
	} else if where != "" {
		return "", nil, fmt.Errorf("refusing to fetch into branch '%s' checked out at '%s'", ref, where)
	}
	head, err := src.Refs().Resolve("HEAD")
	if err != nil {
		return "", nil, err
	}
	if err := r.transfer(ctx, src, gitdir, head.ID); err != nil {
		return "", nil, err
	}
	store := r.repo.Refs()
	old := r.repo.Format.Zero()
	message := "fetch " + gitdir + " HEAD:" + ref + ": storing ref"
	if tip, err := store.Resolve(ref); err == nil {
		old = tip.ID
		if old == head.ID {
			return old.String(), nil, nil
		}
		var fastForward bool
		err = r.session(func(s *rev.Source) error {
			fastForward, err = rev.IsAncestor(s, old, head.ID)
			return err
		})
		if err != nil {
			return "", nil, err
		}
		if !fastForward {
			return "", nil, fmt.Errorf("fetch %s HEAD:%s: rejected non-fast-forward update", gitdir, ref)
		}
		message = "fetch " + gitdir + " HEAD:" + ref + ": fast-forward"
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", nil, err
	}
	if err := r.updateBranch(ref, head.ID, old, message); err != nil {
		return "", nil, err
	}
	return head.ID.String(), nil, nil
}

// CurrentBranch is `git symbolic-ref --quiet --short HEAD`: the branch checked
// out in r's work tree, "" when HEAD is detached.
func (r *Repo) CurrentBranch() (string, error) {
	head, err := symbolicHead(r.repo.Refs())
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(head, "refs/heads/"), nil
}

// FetchCommits copies the commits at the HEAD of gitdir, a checkout's private
// git directory, into r's object database and moves no ref: the part of
// FetchBranch that can fail for reasons outside r, done ahead of changes that
// are not easily taken back.
func (r *Repo) FetchCommits(ctx context.Context, gitdir string) error {
	return r.copyCommit(ctx, gitdir, "")
}

// CopyCommit transfers a pinned commit, without moving any ref.
func (r *Repo) CopyCommit(ctx context.Context, gitdir, commit string) error {
	return r.copyCommit(ctx, gitdir, commit)
}

func (r *Repo) copyCommit(ctx context.Context, gitdir, commit string) error {
	src, err := repo.Open(gitdir)
	if err != nil {
		return err
	}
	if commit != "" {
		id, err := object.ParseIDFormat(src.Format, commit)
		if err != nil {
			return err
		}
		return r.transfer(ctx, src, gitdir, id)
	}
	head, err := src.Refs().Resolve("HEAD")
	if err != nil {
		return err
	}
	return r.transfer(ctx, src, gitdir, head.ID)
}

// transfer copies the objects of want, a commit of the checkout whose private
// git directory is src, at gitdir, into r, touching no ref of either and no
// FETCH_HEAD. git does it when installed: by fetch, or for a partial clone by
// transferPack, since fetch's connectivity check there refuses a pack whose
// trees name blobs r was only promised.
func (r *Repo) transfer(ctx context.Context, src *repo.Repository, gitdir string, want object.ID) error {
	program, ok := gitProgram()
	if !ok {
		return r.copyObjects(ctx, src, want)
	}
	partial, err := r.partial()
	if err != nil {
		return err
	}
	if !partial {
		return r.runGit(ctx, program, gitRun{}, "fetch", "--no-tags", "--no-write-fetch-head", "--quiet", gitdir, want.String())
	}
	return r.transferPack(ctx, program, src, gitdir, want)
}

// partial reports whether r is a partial clone: it has a promisor remote,
// which promised objects r does not hold.
func (r *Repo) partial() (bool, error) {
	cfg, err := r.repo.Config()
	if err != nil {
		return false, err
	}
	if name, ok := cfg.Get("extensions.partialclone"); ok && name != "" {
		return true, nil
	}
	for _, name := range cfg.Subsections("remote") {
		if promisor, _, err := cfg.Bool("remote." + name + ".promisor"); err == nil && promisor {
			return true, nil
		}
	}
	return false, nil
}

// transferPack copies the objects of want that r's refs do not reach from the
// checkout's private git directory into r, a partial clone, as one pack:
// `git pack-objects --revs` there, read by `git index-pack --stdin` in r.
// Everything r's refs reach is left out, held or promised, so the pack carries
// only what the checkout added, and neither side may fetch from the promisor
// remote to complete it: a checkout whose commits build on history fetched
// inside it, which r lacks, fails with a *PartialCloneError.
func (r *Repo) transferPack(ctx context.Context, program string, src *repo.Repository, gitdir string, want object.ID) error {
	from, err := src.OpenDB()
	if err != nil {
		return err
	}
	defer from.Close()
	to, err := r.repo.OpenDB()
	if err != nil {
		return err
	}
	defer to.Close()
	if to.Has(want) {
		return nil // an empty pack is no pack to index
	}
	var revs strings.Builder
	revs.WriteString(want.String() + "\n")
	tips, err := r.repo.Refs().List("refs/")
	if err != nil {
		return err
	}
	if head, err := r.repo.Refs().Resolve("HEAD"); err == nil {
		tips = append(tips, head)
	}
	for _, tip := range tips {
		// The checkout reads r's objects too, but not one r was only promised.
		if !tip.Symbolic() && from.Contains(tip.ID) {
			revs.WriteString("^" + tip.ID.String() + "\n")
		}
	}
	env := append(gitEnv(), "GIT_NO_LAZY_FETCH=1")
	pack := exec.CommandContext(ctx, program, "--git-dir", gitdir, "pack-objects", "--revs", "--stdout", "--delta-base-offset", "-q")
	index := exec.CommandContext(ctx, program, "-C", r.dir(), "index-pack", "--stdin")
	read, write, err := os.Pipe()
	if err != nil {
		return err
	}
	var packErr, indexErr bytes.Buffer
	pack.Env, pack.Stdin, pack.Stdout, pack.Stderr = env, strings.NewReader(revs.String()), write, &packErr
	index.Env, index.Stdin, index.Stderr = env, read, &indexErr
	packRun, indexRun := pack.Start(), index.Start()
	// The children hold the pipe now; index-pack sees its end once pack-objects exits.
	write.Close()
	read.Close()
	if packRun == nil {
		packRun = pack.Wait()
	}
	if indexRun == nil {
		indexRun = index.Wait()
	}
	if packRun == nil && indexRun == nil {
		return nil
	}
	failure := fmt.Errorf("git pack-objects: %s: %w", strings.TrimSpace(packErr.String()), packRun)
	if packRun == nil {
		failure = fmt.Errorf("git index-pack: %s: %w", strings.TrimSpace(indexErr.String()), indexRun)
	}
	// Remote-tracking refs the checkout moved past r's are the history it fetched.
	missing := &PartialCloneError{Top: r.dir(), Err: failure}
	if tracking, err := src.Refs().List("refs/remotes/"); err == nil {
		for _, ref := range tracking {
			if !ref.Symbolic() && !to.Contains(ref.ID) {
				missing.Refs = append(missing.Refs, strings.TrimPrefix(ref.Name, "refs/remotes/"))
			}
		}
	}
	return missing
}

// copyObjects copies the objects reachable from want that r lacks from src,
// keeping r's shallow boundary consistent with src's.
func (r *Repo) copyObjects(ctx context.Context, src *repo.Repository, want object.ID) error {
	from, err := src.OpenDB()
	if err != nil {
		return err
	}
	defer from.Close()
	to, err := r.repo.OpenDB()
	if err != nil {
		return err
	}
	defer to.Close()
	srcShallow, err := fetch.ReadShallow(src.CommonDir)
	if err != nil {
		return err
	}
	dstShallow, err := fetch.ReadShallow(r.CommonDir)
	if err != nil {
		return err
	}
	res, err := fetch.Local(ctx, from, to, []object.ID{want}, fetch.LocalOptions{
		SourceShallow: srcShallow,
		TargetShallow: dstShallow,
		PackDir:       filepath.Join(r.CommonDir, "objects", "pack"),
	})
	if err != nil {
		return err
	}
	return fetch.UpdateShallow(r.CommonDir, res.Shallow, res.Unshallow)
}

// updateBranch moves ref from old (the zero ID: absent) to next, logged with
// message as git would log it.
func (r *Repo) updateBranch(ref string, next, old object.ID, message string) error {
	reflog, err := r.reflog(message)
	if err != nil {
		return err
	}
	return r.repo.Refs().Update(ref, next, refs.UpdateOptions{Old: old, Reflog: reflog})
}

// reflog describes how a ref update by r is logged: by the committer git would
// name, under r's core.logAllRefUpdates.
func (r *Repo) reflog(message string) (*refs.ReflogUpdate, error) {
	committer, err := r.repo.Committer()
	if err != nil {
		return nil, err
	}
	policy, err := r.repo.LogPolicy()
	if err != nil {
		return nil, err
	}
	return &refs.ReflogUpdate{Committer: committer, Message: message, Policy: policy}, nil
}

// symbolicHead is `git symbolic-ref --quiet HEAD`: the branch HEAD names, ""
// when it is detached.
func symbolicHead(store *refs.Store) (string, error) {
	head, err := store.Symbolic("HEAD")
	if errors.Is(err, refs.ErrNotSymbolic) {
		return "", nil
	}
	return head, err
}

// checkedOutAt returns the work tree of r's repository, main or linked, whose
// HEAD names ref, "" when none does.
func (r *Repo) checkedOutAt(ref string) (string, error) {
	worktrees, err := r.repo.Worktrees()
	if err != nil {
		return "", err
	}
	for _, w := range worktrees {
		head, err := symbolicHead(w.Refs())
		if err != nil {
			return "", err
		}
		if head == ref {
			return w.Top, nil
		}
	}
	return "", nil
}

// indexWriteOptions are the racy-git settings git applies when it rewrites r's
// index whose previous mtime was racyTime.
func (r *Repo) indexWriteOptions(racyTime time.Time) (index.WriteOptions, error) {
	cfg, err := r.repo.Config()
	if err != nil {
		return index.WriteOptions{}, err
	}
	trustCTime, found, err := cfg.Bool("core.trustctime")
	if err != nil {
		return index.WriteOptions{}, err
	}
	checkStat, _ := cfg.Get("core.checkStat")
	return index.WriteOptions{
		RacyTime:    racyTime,
		Worktree:    r.Top,
		IgnoreCTime: found && !trustCTime,
		MinimalStat: checkStat == "minimal",
	}, nil
}
