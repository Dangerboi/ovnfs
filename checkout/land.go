package checkout

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/git"
)

// Land puts a checkout's commits on the branch checked out in its folder: what
// `git commit` there would have left. The branch fast-forwards to the
// checkout's HEAD, its index follows for the paths those commits changed, and
// the checkout's files for those paths merge into the folder as an atomic
// Apply merges them, so the folder's own uncommitted edits stay on top.
//
// Everything that can refuse does so before the folder is written: a detached
// HEAD there, a branch holding commits the checkout's HEAD lacks, staged
// changes on the commits' paths, uncommitted source changes in the checkout
// (unless SkipDirty leaves those off the commits' paths behind), commits that
// cannot be copied, and files that would conflict (the reply then has Refused
// set), unless Markers asks for their conflict markers in the folder instead.
// The branch moves last, once every file is merged; until then, and should
// moving it still fail, the folder holds the merged files as uncommitted edits
// and landing again moves it.
func (r *Registry) Land(ctx context.Context, args api.LandArgs) (api.LandReply, error) {
	resume, err := r.Quiesce(ctx)
	if err != nil {
		return api.LandReply{}, err
	}
	defer resume()
	h, err := r.lookupCheckout(args.Checkout)
	if err != nil {
		return api.LandReply{}, err
	}
	name, meta := h.meta.Name, h.base.view.Meta()
	if meta.Remote != nil {
		return api.LandReply{}, failure("invalid", "checkout %s is attached from %s; its folder is not on this machine", name, meta.Remote.Source)
	}
	repo := h.base.view.Domains().Repo("")
	if !h.meta.Git || repo == nil {
		return api.LandReply{}, failure("invalid", "checkout %s has no git directory, so no commits to land; `ovnfs apply %s` merges its files", name, name)
	}
	if args.Rebase && args.Markers {
		return api.LandReply{}, failure("invalid", "--rebase cannot be combined with --markers")
	}
	release, err := r.lockFolderMerge(ctx, h)
	if err != nil {
		return api.LandReply{}, err
	}
	defer release()
	branch, err := repo.CurrentBranch()
	if err != nil {
		return api.LandReply{}, coded(err)
	}
	if branch == "" {
		return api.LandReply{}, failure("conflict", "HEAD is detached in %s, so there is no branch to land on; `ovnfs apply %s --branch NAME` brings the commits as a branch", meta.Dir, name)
	}
	old, err := repo.Head(ctx)
	if err != nil {
		return api.LandReply{}, coded(err)
	}
	gitdir := r.gitdirPath(h.meta.dir())
	private, err := git.OpenPrivate(gitdir)
	if err != nil {
		return api.LandReply{}, coded(err)
	}
	tip, err := private.Head(ctx)
	if err != nil {
		return api.LandReply{}, coded(err)
	}
	if tip == "" || old == "" {
		return api.LandReply{}, failure("conflict", "both the folder and checkout need a commit before landing")
	}
	refused := func(err error) error {
		conflict, ok := errors.AsType[*git.BranchConflict](err)
		switch {
		case !ok:
			return codedTransfer(err)
		case conflict.Diverged:
			return failure("conflict", "%s in %s holds commits the checkout's are not based on; use `ovnfs land %s --rebase` for a clean checkout, or inside the checkout run `git fetch %s %s && git rebase --autostash FETCH_HEAD`, then land again",
				branch, meta.Dir, name, shellQuote(meta.Dir), branch)
		case len(conflict.Staged) != 0:
			return failure("conflict", "%s in %s, which the checkout's commits change too; commit or unstage them there, then land again", conflict, meta.Dir)
		}
		return failure("conflict", "%s in %s; land again", conflict, meta.Dir)
	}
	commit, changed, err := repo.LandBranch(ctx, gitdir, branch, old, tip, true)
	plan := landFiles{ctx: ctx}
	if conflict, ok := errors.AsType[*git.BranchConflict](err); args.Rebase && ok && conflict.Diverged {
		clean, checkErr := private.IndexMatches(ctx, tip)
		if checkErr != nil {
			return api.LandReply{}, coded(checkErr)
		}
		if !clean {
			return api.LandReply{}, failure("conflict", "checkout %s has a changed index or a Git operation in progress; finish it or rebase manually", name)
		}
		scratch, scratchErr := os.MkdirTemp(r.config.State, "land-replay-")
		if scratchErr != nil {
			return api.LandReply{}, coded(scratchErr)
		}
		defer os.RemoveAll(scratch)
		gitdir, err = repo.Replay(ctx, gitdir, tip, old, scratch)
		if err != nil {
			return api.LandReply{}, failure("conflict", "could not replay checkout %s without changing it: %v; %v", name, err, refused(conflict))
		}
		replay, openErr := git.OpenPrivate(gitdir)
		if openErr != nil {
			return api.LandReply{}, coded(openErr)
		}
		next, headErr := replay.Head(ctx)
		if headErr != nil {
			return api.LandReply{}, coded(headErr)
		}
		commit, changed, err = repo.LandBranch(ctx, gitdir, branch, old, next, true)
		plan.replayed, plan.worktree = true, filepath.Dir(gitdir)
	}
	if err != nil {
		return api.LandReply{}, refused(err)
	}
	reply := api.LandReply{Old: old, Source: meta.Dir, Applied: []api.Change{}, Merged: []string{}, Conflicts: []api.Conflict{}, Branch: branch, Commit: commit}
	if commit == old && plan.replayed {
		reply.ReplayedFrom = tip
		return reply, nil
	}
	plan.changed, plan.paths = changed, make(map[string]bool, len(changed))
	for _, c := range changed {
		plan.paths[c.Path] = true
	}
	if plan.head, err = gitLeaves(ctx, private, tip); err != nil {
		return api.LandReply{}, coded(err)
	}
	landed, left, err := r.uncommitted(h, &plan)
	if err != nil {
		return api.LandReply{}, coded(err)
	}
	switch {
	case plan.replayed && len(left) != 0:
		return api.LandReply{}, failure("conflict", "checkout %s has uncommitted source changes in %s; commit them or rebase manually", name, pathList(left))
	case len(landed) != 0:
		return api.LandReply{}, failure("conflict", "checkout %s has uncommitted changes in %s on top of the commits that change it; commit them there, then land again", name, pathList(landed))
	case len(left) != 0 && !args.SkipDirty && commit == old:
		return api.LandReply{}, failure("conflict", "checkout %s holds no commit beyond %s, only uncommitted source changes in %s; commit them there, then land, or `ovnfs apply %s` merges them as uncommitted edits", name, branch, pathList(left), name)
	case len(left) != 0 && !args.SkipDirty:
		return api.LandReply{}, failure("conflict", "checkout %s has uncommitted source changes in %s; commit them there, then land again, or `ovnfs land %s --skip-dirty` lands only its commits and leaves them in the checkout", name, pathList(left), name)
	}
	reply.Left = len(left)
	if commit == old {
		return reply, nil
	}
	for _, captured := range h.base.view.Repos() {
		if captured.Path == "" && captured.Head != "" && captured.Head != old && !plan.replayed {
			if plan.captured, err = gitLeaves(ctx, private, captured.Head); err != nil {
				return api.LandReply{}, coded(err)
			}
			break
		}
	}
	if plan.replayed || plan.captured != nil {
		if plan.old, err = gitLeaves(ctx, repo, old); err != nil {
			return api.LandReply{}, coded(err)
		}
	}
	if !args.DryRun {
		// Copy the pinned commit before changing any destination files.
		if err := repo.CopyCommit(ctx, gitdir, commit); err != nil {
			return api.LandReply{}, codedTransfer(err)
		}
	}
	files, err := r.mergeFiles(ctx, h, mergeOptions{dryRun: args.DryRun, atomic: !args.Markers, land: &plan})
	if err != nil {
		return api.LandReply{}, err
	}
	files.Branch, files.Commit = branch, commit
	reply.ApplyReply = files
	if len(files.Conflicts) != 0 {
		reply.Commit = old
		return reply, nil
	}
	if !args.DryRun {
		if _, _, err := repo.LandBranch(ctx, gitdir, branch, old, commit, false); err != nil {
			return api.LandReply{}, failure("conflict", "the checkout's files are merged into %s as uncommitted edits, but %s did not move: %v", meta.Dir, branch, refused(err))
		}
	}
	if plan.replayed {
		reply.ReplayedFrom = tip
	}
	return reply, nil
}
