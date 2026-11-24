package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/stencil-hq/ovnfs/git/internal/object"
	"github.com/stencil-hq/ovnfs/git/internal/repo"
)

// Replay rebases tip from gitdir onto a commit in r. It returns the Git
// directory in scratch (an empty directory owned by the caller). Both source
// object stores remain read-only alternates and must outlive use of the result.
// Merge commits in the replay range are refused rather than flattened. A failed
// replay may leave an in-progress rebase in scratch only.
func (r *Repo) Replay(ctx context.Context, gitdir, tip, onto, scratch string) (string, error) {
	program, ok := gitProgram()
	if !ok {
		return "", errors.New("rebase requires the git executable")
	}
	source, err := repo.Open(gitdir)
	if err != nil {
		return "", err
	}
	format := r.repo.Format
	if source.Format != format {
		return "", errors.New("cannot rebase repositories with different object formats")
	}
	// Only accept full object IDs, not revision expressions or Git options.
	tipID, err := object.ParseIDFormat(format, tip)
	if err != nil {
		return "", fmt.Errorf("rebase tip: %w", err)
	}
	ontoID, err := object.ParseIDFormat(format, onto)
	if err != nil {
		return "", fmt.Errorf("rebase target: %w", err)
	}
	tip, onto = tipID.String(), ontoID.String()

	var alternates strings.Builder
	for _, dir := range []string{source.CommonDir, r.CommonDir} {
		if strings.ContainsAny(dir, "\r\n") {
			return "", errors.New("cannot rebase with a newline in the git directory path")
		}
		alternates.WriteString(filepath.Join(dir, "objects"))
		alternates.WriteByte('\n')
	}
	scratch, err = filepath.Abs(scratch)
	if err != nil {
		return "", err
	}
	private := filepath.Join(scratch, ".git")
	isolated := &Repo{Top: scratch}
	// runGit discards repository redirects and disables hooks. Override the
	// remaining Git environment as well (including injected config, identity,
	// filters, and editor settings), with no config from outside scratch.
	env := []string{
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_ATTR_NOSYSTEM=1",
		"GIT_ALLOW_PROTOCOL=",
		"GIT_COMMITTER_NAME=ovnfs",
		"GIT_COMMITTER_EMAIL=ovnfs@localhost",
	}
	run := func(output *strings.Builder, args ...string) error {
		o := gitRun{env: env, ignoreGitEnv: true}
		if output != nil {
			o.out = output
		}
		return isolated.runGit(ctx, program, o, append([]string{
			"-c", "core.attributesFile=" + os.DevNull,
			"-c", "maintenance.auto=false",
			"-c", "gc.auto=0",
		}, args...)...)
	}
	if err := run(nil, "--git-dir="+private, "--work-tree="+scratch, "init", "--template=", "--object-format="+format.String()); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(private, "objects", "info", "alternates"), []byte(alternates.String()), 0o600); err != nil {
		return "", err
	}
	var merges strings.Builder
	if err := run(&merges, "rev-list", "--max-count=1", "--merges", onto+".."+tip); err != nil {
		return "", err
	}
	if merges.Len() != 0 {
		return "", errors.New("cannot rebase a checkout containing merge commits")
	}
	if err := run(nil, "checkout", "--detach", tip); err != nil {
		return "", err
	}
	if err := run(nil, "rebase", "--merge", "--no-fork-point", "--no-gpg-sign", onto); err != nil {
		return "", err
	}
	return private, nil
}
