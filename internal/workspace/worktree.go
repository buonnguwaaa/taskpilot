// Package workspace manages a task's workspace: one directory per task, with
// one git worktree per repo the agent opens, all on the same work branch.
//
//	<home>/worktrees/<task-id>/          task workspace (the agent's cwd)
//	<home>/worktrees/<task-id>/<repo>/   worktree of <repo> on branch tp/<task-id>
package workspace

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/buonnguwaaa/taskpilot/internal/config"
)

// Dir is the workspace directory of a task.
func Dir(home, taskID string) string { return filepath.Join(home, "worktrees", taskID) }

// Branch is the provisional work branch of a task, the same in every repo it
// opens, until the agent names it by the team's convention (set_branch).
func Branch(taskID string) string { return "tp/" + taskID }

// RenameBranch renames the work branch in every repo opened in the workspace.
// It checks all repos first (the old branch is there, the new name is free)
// so a refusal changes nothing; a failure midway renames the done ones back.
func RenameBranch(ctx context.Context, workspaceDir, from, to string) ([]string, error) {
	if from == to {
		return OpenedRepos(workspaceDir), nil
	}
	if _, err := git(ctx, ".", "check-ref-format", "--branch", to); err != nil {
		return nil, fmt.Errorf("%q is not a valid branch name", to)
	}
	repos := OpenedRepos(workspaceDir)
	for _, r := range repos {
		dir := filepath.Join(workspaceDir, r)
		cur, err := git(ctx, dir, "symbolic-ref", "--short", "HEAD")
		if err != nil || strings.TrimSpace(cur) != from {
			return nil, fmt.Errorf("repo %s is not on the work branch %s (HEAD: %s)", r, from, strings.TrimSpace(cur))
		}
		if branchExists(ctx, dir, to) {
			return nil, fmt.Errorf("branch %s already exists in repo %s: pick another name", to, r)
		}
	}
	var done []string
	for _, r := range repos {
		dir := filepath.Join(workspaceDir, r)
		if _, err := git(ctx, dir, "branch", "-m", from, to); err != nil {
			for _, d := range done {
				_, _ = git(ctx, filepath.Join(workspaceDir, d), "branch", "-m", to, from)
			}
			return nil, fmt.Errorf("rename in %s: %w", r, err)
		}
		done = append(done, r)
	}
	return repos, nil
}

// Create makes the (empty) workspace directory.
func Create(home, taskID string) (string, error) {
	d := Dir(home, taskID)
	return d, os.MkdirAll(d, 0o755)
}

// Opened describes a repo checked out into a workspace.
type Opened struct {
	Name    string
	Path    string
	Branch  string
	Created bool     // false when it was already open
	Notes   []string // non-fatal problems (fetch fallback, failed prepare)
}

// OpenRepo creates (or reuses) <workspace>/<name> as a worktree of repo on the
// task's branch, started from the freshly fetched <remote>/<base>, then runs
// the repo's prepare commands. The operator's own checkout is not touched.
func OpenRepo(ctx context.Context, workspaceDir, branch, name string, repo config.Repo) (*Opened, error) {
	if !config.ValidName(name) {
		return nil, fmt.Errorf("invalid repo name %q", name)
	}
	o := &Opened{Name: name, Path: filepath.Join(workspaceDir, name), Branch: branch}
	if _, err := os.Stat(filepath.Join(o.Path, ".git")); err == nil {
		return o, nil
	}
	if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
		return nil, err
	}
	if branchExists(ctx, repo.Path, o.Branch) {
		if _, err := git(ctx, repo.Path, "worktree", "add", o.Path, o.Branch); err != nil {
			return nil, err
		}
	} else {
		start, note, err := startPoint(ctx, repo)
		if err != nil {
			return nil, err
		}
		if note != "" {
			o.Notes = append(o.Notes, note)
		}
		// --no-track: the work branch must not push to (or track) the base branch.
		if _, err := git(ctx, repo.Path, "worktree", "add", "--no-track", "-b", o.Branch, o.Path, start); err != nil {
			return nil, err
		}
	}
	o.Created = true
	for _, argv := range repo.Prepare {
		if len(argv) == 0 {
			continue
		}
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Dir = o.Path
		if out, err := cmd.CombinedOutput(); err != nil {
			o.Notes = append(o.Notes, fmt.Sprintf("prepare %q failed: %v: %s", strings.Join(argv, " "), err, tail(out, 400)))
		}
	}
	return o, nil
}

// OpenedRepos lists the repos already checked out in a workspace.
func OpenedRepos(workspaceDir string) []string {
	entries, err := os.ReadDir(workspaceDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			if _, err := os.Stat(filepath.Join(workspaceDir, e.Name(), ".git")); err == nil {
				out = append(out, e.Name())
			}
		}
	}
	sort.Strings(out)
	return out
}

// VerifyResult is the outcome of running a repo's verify command.
type VerifyResult struct {
	Passed   bool
	Output   string // tail
	Duration time.Duration
}

// RunVerify runs command with sh in dir, bounded by timeout.
func RunVerify(ctx context.Context, dir, command string, timeout time.Duration) VerifyResult {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	r := VerifyResult{Passed: err == nil, Output: tail(out, 3000), Duration: time.Since(start)}
	if ctx.Err() == context.DeadlineExceeded {
		r.Output += fmt.Sprintf("\n[taskpilot: verify timed out after %s]", timeout)
	}
	return r
}

// startPoint fetches <remote>/<base> and returns it, falling back to the local
// base branch when the fetch fails and no remote-tracking ref exists.
// FetchAttempts is how many times the base is fetched before a new work
// branch is refused.
var FetchAttempts = 3

// startPoint is where a new work branch starts: the latest <remote>/<base>,
// fetched now. A task never starts from a stale base: when the fetch keeps
// failing, the repo is not opened (the operator retries once the network is
// back). Remote "-" opts out (local base).
func startPoint(ctx context.Context, repo config.Repo) (string, string, error) {
	if repo.Remote == "" || repo.Remote == "-" {
		return repo.Base, "", nil
	}
	remoteRef := repo.Remote + "/" + repo.Base
	if _, err := git(ctx, repo.Path, "remote", "get-url", repo.Remote); err != nil {
		// A repo without that remote has nothing to fetch.
		return repo.Base, fmt.Sprintf("no remote %s: branching from the local %s", repo.Remote, repo.Base), nil
	}
	var fetchErr error
	for i := 0; i < FetchAttempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return "", "", ctx.Err()
			case <-time.After(time.Duration(i) * 2 * time.Second):
			}
		}
		// The explicit refspec updates refs/remotes/<remote>/<base> itself.
		if _, fetchErr = git(ctx, repo.Path, "fetch", "--quiet", repo.Remote,
			"+refs/heads/"+repo.Base+":refs/remotes/"+remoteRef); fetchErr == nil {
			break
		}
	}
	if fetchErr != nil {
		return "", "", fmt.Errorf("cannot fetch the latest %s (%d attempts): %v — the task must start from the latest base; retry when the remote is reachable", remoteRef, FetchAttempts, fetchErr)
	}
	sha, _ := git(ctx, repo.Path, "rev-parse", "--short", "refs/remotes/"+remoteRef)
	return remoteRef, fmt.Sprintf("started from the latest %s (%s)", remoteRef, strings.TrimSpace(sha)), nil
}

func branchExists(ctx context.Context, repo, branch string) bool {
	_, err := git(ctx, repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, tail(out.Bytes(), 400))
	}
	return out.String(), nil
}

func tail(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}

// Remove deletes a task workspace: each repo worktree is unregistered from its
// repository (uncommitted changes are lost), then the directory is removed.
// The work branches stay in the repositories.
func Remove(ctx context.Context, workspaceDir string) error {
	if workspaceDir == "" {
		return nil
	}
	var errs []string
	for _, name := range OpenedRepos(workspaceDir) {
		wt := filepath.Join(workspaceDir, name)
		common, err := git(ctx, wt, "rev-parse", "--path-format=absolute", "--git-common-dir")
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		if _, err := git(ctx, "", "--git-dir="+strings.TrimSpace(common), "worktree", "remove", "--force", wt); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", name, err))
		}
	}
	if err := os.RemoveAll(workspaceDir); err != nil {
		errs = append(errs, err.Error())
	}
	if len(errs) > 0 {
		return fmt.Errorf("remove workspace: %s", strings.Join(errs, "; "))
	}
	return nil
}

// Release unregisters the clean worktrees of a finished task, so their
// branches can be checked out in the operator's own clone again (git refuses a
// branch another worktree holds). Worktrees with uncommitted changes are kept
// and returned. Branches and commits stay in the repositories; reopening the
// task opens the repos again.
func Release(ctx context.Context, workspaceDir string) (kept []string, err error) {
	var errs []string
	for _, name := range OpenedRepos(workspaceDir) {
		wt := filepath.Join(workspaceDir, name)
		status, err := git(ctx, wt, "status", "--porcelain")
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		if strings.TrimSpace(status) != "" {
			kept = append(kept, name)
			continue
		}
		common, err := git(ctx, wt, "rev-parse", "--path-format=absolute", "--git-common-dir")
		if err == nil {
			_, err = git(ctx, "", "--git-dir="+strings.TrimSpace(common), "worktree", "remove", wt)
		}
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", name, err))
		}
	}
	if len(errs) > 0 {
		return kept, fmt.Errorf("release worktrees: %s", strings.Join(errs, "; "))
	}
	return kept, nil
}

// ReviewAction is what ReviewCheckout did in the operator's clone.
type ReviewAction string

const (
	ReviewOpened   ReviewAction = "opened"   // switched to the task's latest commit (detached)
	ReviewUpdated  ReviewAction = "updated"  // was on an older commit of the task: moved to the latest
	ReviewReturned ReviewAction = "returned" // was already on the latest: back to the previous branch
)

// ReviewCheckout lets the operator look at a task's branch in their own
// clone while the task's worktree holds it: git refuses to check a branch out
// twice, but a detached HEAD at its tip is fine and leaves the agent alone.
// Called again it follows the agent's new commits, or, when already up to
// date, returns to the branch the operator was on. The clone must be clean.
func ReviewCheckout(ctx context.Context, repoPath, branch string) (ReviewAction, error) {
	if out, err := git(ctx, repoPath, "status", "--porcelain", "--untracked-files=no"); err != nil {
		return "", err
	} else if strings.TrimSpace(out) != "" {
		return "", fmt.Errorf("%s có thay đổi chưa commit: commit hoặc stash trước", filepath.Base(repoPath))
	}
	tip, err := git(ctx, repoPath, "rev-parse", "--verify", "--quiet", branch+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("%s chưa có nhánh %s", filepath.Base(repoPath), branch)
	}
	tip = strings.TrimSpace(tip)
	head, _ := git(ctx, repoPath, "rev-parse", "HEAD")
	head = strings.TrimSpace(head)
	_, symErr := git(ctx, repoPath, "symbolic-ref", "-q", "HEAD")
	detached := symErr != nil
	switch {
	case detached && head == tip:
		prev := previousBranch(ctx, repoPath, branch)
		if prev == "" {
			return "", fmt.Errorf("%s: không tìm thấy nhánh cũ trong reflog; tự `git switch <nhánh>`", filepath.Base(repoPath))
		}
		if _, err := git(ctx, repoPath, "switch", prev); err != nil {
			return "", err
		}
		return ReviewReturned, nil
	case detached && isAncestor(ctx, repoPath, head, tip):
		if _, err := git(ctx, repoPath, "switch", "--detach", tip); err != nil {
			return "", err
		}
		return ReviewUpdated, nil
	}
	if _, err := git(ctx, repoPath, "switch", "--detach", tip); err != nil {
		return "", err
	}
	return ReviewOpened, nil
}

var movingRe = regexp.MustCompile(`^checkout: moving from (\S+) to `)

// previousBranch is the latest branch HEAD was on before detaching, from the
// reflog ("checkout: moving from <branch> to <commit>"), skipping commits.
func previousBranch(ctx context.Context, repo, skip string) string {
	out, err := git(ctx, repo, "reflog", "-n", "200", "--format=%gs")
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(out, "\n") {
		m := movingRe.FindStringSubmatch(strings.TrimSpace(l))
		if m == nil || m[1] == skip {
			continue
		}
		if _, err := git(ctx, repo, "show-ref", "--verify", "--quiet", "refs/heads/"+m[1]); err == nil {
			return m[1]
		}
	}
	return ""
}

func isAncestor(ctx context.Context, repo, a, b string) bool {
	_, err := git(ctx, repo, "merge-base", "--is-ancestor", a, b)
	return err == nil
}
