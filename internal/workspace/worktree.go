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
	"sort"
	"strings"
	"time"

	"github.com/buonnguwaaa/taskpilot/internal/config"
)

// Dir is the workspace directory of a task.
func Dir(home, taskID string) string { return filepath.Join(home, "worktrees", taskID) }

// Branch is the work branch of a task, the same in every repo it opens.
func Branch(taskID string) string { return "tp/" + taskID }

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
func OpenRepo(ctx context.Context, workspaceDir, taskID, name string, repo config.Repo) (*Opened, error) {
	if !config.ValidName(name) {
		return nil, fmt.Errorf("invalid repo name %q", name)
	}
	o := &Opened{Name: name, Path: filepath.Join(workspaceDir, name), Branch: Branch(taskID)}
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
		start, note := startPoint(ctx, repo)
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
func startPoint(ctx context.Context, repo config.Repo) (string, string) {
	if repo.Remote == "" || repo.Remote == "-" {
		return repo.Base, ""
	}
	remoteRef := repo.Remote + "/" + repo.Base
	_, fetchErr := git(ctx, repo.Path, "fetch", "--quiet", repo.Remote, repo.Base)
	if _, err := git(ctx, repo.Path, "rev-parse", "--verify", "--quiet", "refs/remotes/"+remoteRef); err == nil {
		if fetchErr != nil {
			return remoteRef, fmt.Sprintf("fetch %s failed (%v); using the last fetched %s", remoteRef, fetchErr, remoteRef)
		}
		return remoteRef, ""
	}
	return repo.Base, fmt.Sprintf("no %s (fetch: %v); branching from local %s", remoteRef, fetchErr, repo.Base)
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
