package workspace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/buonnguwaaa/taskpilot/internal/config"
)

// A finished task keeps its worktrees (the agent can read them again when the
// task is resumed) but detaches them: HEAD stays on the same commit, and the
// branch is free for the operator to check out in their own clone. Attach
// puts them back on their branch before the next run.

// detachedFile, in the worktree's private git dir (outside the workspace the
// agent writes to), remembers the branch a worktree was detached from.
const detachedFile = "taskpilot-branch"

func worktreeGitDir(ctx context.Context, wt string) (string, error) {
	d, err := git(ctx, wt, "rev-parse", "--path-format=absolute", "--git-dir")
	return strings.TrimSpace(d), err
}

// Detach moves the clean worktrees of a workspace to a detached HEAD at the
// same commit, freeing their branches. Worktrees with uncommitted changes
// keep their branch and are returned in kept.
func Detach(ctx context.Context, workspaceDir string) (kept []string, err error) {
	var errs []string
	for _, name := range OpenedRepos(workspaceDir) {
		wt := filepath.Join(workspaceDir, name)
		branch, err := git(ctx, wt, "symbolic-ref", "--quiet", "--short", "HEAD")
		branch = strings.TrimSpace(branch)
		if err != nil || branch == "" {
			continue // already detached
		}
		status, err := git(ctx, wt, "status", "--porcelain", "--untracked-files=no")
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		if strings.TrimSpace(status) != "" {
			kept = append(kept, name)
			continue
		}
		gd, err := worktreeGitDir(ctx, wt)
		if err == nil {
			err = os.WriteFile(filepath.Join(gd, detachedFile), []byte(branch+"\n"), 0o644)
		}
		if err == nil {
			_, err = git(ctx, wt, "switch", "--quiet", "--detach")
		}
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", name, err))
		}
	}
	if len(errs) > 0 {
		return kept, fmt.Errorf("detach worktrees: %s", strings.Join(errs, "; "))
	}
	return kept, nil
}

// heldAt matches git's "is already checked out at '<path>'" (wording varies
// a little between versions).
var heldAt = regexp.MustCompile(`(?:already (?:checked out|used by worktree) at|is checked out at) '([^']+)'`)

// Attach puts the detached worktrees of a workspace back on their branch
// (fallback: branch), and re-creates the worktrees of project repos that
// have the task's branch but were removed (older versions released them when
// a task was done). It refuses rather than lose commits: a detached HEAD
// with commits the branch does not have is reported, not switched. When the
// operator has the branch checked out in their own clone, the error says so.
func Attach(ctx context.Context, workspaceDir, branch string, repos map[string]config.Repo) error {
	var errs []string
	held := func(name, b string, err error) string {
		if m := heldAt.FindStringSubmatch(err.Error()); m != nil {
			return fmt.Sprintf("%s: nhánh %s đang được checkout ở %s — chuyển repo đó sang nhánh khác (hoặc detach) rồi nhắn lại task", name, b, m[1])
		}
		return fmt.Sprintf("%s: %v", name, err)
	}
	opened := map[string]bool{}
	for _, name := range OpenedRepos(workspaceDir) {
		opened[name] = true
		wt := filepath.Join(workspaceDir, name)
		if cur, err := git(ctx, wt, "symbolic-ref", "--quiet", "--short", "HEAD"); err == nil && strings.TrimSpace(cur) != "" {
			continue // on a branch
		}
		b := branch
		gd, err := worktreeGitDir(ctx, wt)
		if err == nil {
			if saved, err := os.ReadFile(filepath.Join(gd, detachedFile)); err == nil && strings.TrimSpace(string(saved)) != "" {
				b = strings.TrimSpace(string(saved))
			}
		}
		if b == "" || !branchExists(ctx, wt, b) {
			errs = append(errs, fmt.Sprintf("%s: đang detached và không có nhánh %q để gắn lại", name, b))
			continue
		}
		if _, err := git(ctx, wt, "merge-base", "--is-ancestor", "HEAD", "refs/heads/"+b); err != nil {
			errs = append(errs, fmt.Sprintf("%s: HEAD detached có commit chưa nằm trên nhánh %s; gắn lại bằng tay (git -C %s switch %s, rồi merge/cherry-pick)", name, b, wt, b))
			continue
		}
		if _, err := git(ctx, wt, "switch", "--quiet", b); err != nil {
			errs = append(errs, held(name, b, err))
			continue
		}
		if gd != "" {
			_ = os.Remove(filepath.Join(gd, detachedFile))
		}
	}
	if branch != "" {
		for name, repo := range repos {
			if opened[name] || repo.Path == "" || !branchExists(ctx, repo.Path, branch) {
				continue
			}
			if _, err := OpenRepo(ctx, workspaceDir, branch, name, repo); err != nil {
				errs = append(errs, held(name, branch, err))
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "\n"))
	}
	return nil
}
