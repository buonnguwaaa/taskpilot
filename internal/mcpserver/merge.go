package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/buonnguwaaa/taskpilot/internal/bitbucket"
	"github.com/buonnguwaaa/taskpilot/internal/store"
)

// MergePullRequestIn is the input of merge_pull_request.
type MergePullRequestIn struct {
	Repo     string `json:"repo" jsonschema:"the repo whose PR to merge"`
	Target   string `json:"target,omitempty" jsonschema:"branch to merge into; default: the first allowed one in the repo's pr_flow (release/qc)"`
	Strategy string `json:"strategy,omitempty" jsonschema:"merge_commit, squash or fast_forward; default: the repository's"`
	// ResolveOnly prepares without merging: for an operator who asked to
	// resolve the conflicts, not to merge.
	ResolveOnly bool `json:"resolve_only,omitempty" jsonschema:"true when the operator asked only to resolve the conflicts (not to merge): resolve on the branch the tool names, then this pushes it and opens its PR into the target without merging"`
}

// mergeAsked matches an operator text asking for a merge.
var mergeAsked = regexp.MustCompile(`(?i)\b(merge|gộp)\b`)

// resolutionBranch is where the agent resolves conflicts between the work
// branch and target: it starts from target, so the work branch (and its PR
// into the base) never gets target's other commits.
func resolutionBranch(work, target string) string {
	return work + "--into-" + strings.NewReplacer("/", "-", " ", "-").Replace(target)
}

// MergePullRequest merges the task's PR of a repo into an allowed target
// (agent_merge_targets), only in a run the operator started with a message
// or decision that asks for a merge. Conflicts are resolved by the agent on
// resolutionBranch, which this tool then pushes, opens a PR for and merges.
func (h *Handlers) MergePullRequest(ctx context.Context, in MergePullRequestIn) (string, error) {
	t, err := h.Store.LoadTask(h.TaskID)
	if err != nil {
		return "", err
	}
	proj, ok := h.Config.Projects[t.Project]
	if !ok {
		return "", fmt.Errorf("task project %q is not configured", t.Project)
	}
	repo, ok := proj.Repos[in.Repo]
	if !ok {
		return "", fmt.Errorf("repo %q is not part of project %s", in.Repo, proj.Name)
	}
	allowed := h.Config.AgentMergeTargets
	target := in.Target
	if target == "" {
		for _, st := range repo.PRFlow {
			if slices.Contains(allowed, st.Target) {
				target = st.Target
				break
			}
		}
	}
	if target == "" || !slices.Contains(allowed, target) {
		return "", fmt.Errorf("merging into %q is not allowed; the agent may merge only into %s", target, strings.Join(allowed, ", "))
	}
	if _, ok := repo.Stage(target); !ok {
		return "", fmt.Errorf("%s is not a PR target of %s; its flow is %s", target, in.Repo, flowString(repo))
	}
	if !in.ResolveOnly {
		if err := h.mergeGranted(ctx, in.Repo, target); err != nil {
			return "", err
		}
	}
	pr, err := h.Store.PullRequestInto(h.TaskID, in.Repo, target)
	if err != nil {
		return "", err
	}
	if pr == nil || pr.Pending || pr.ID <= 0 {
		return "", fmt.Errorf("repo %s has no PR into %s yet; open it with create_pull_request first", in.Repo, target)
	}
	if h.Bitbucket == nil {
		return "", errors.New("Bitbucket is not configured for merges")
	}
	bb, err := h.Bitbucket(ctx)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(t.Workspace, in.Repo)
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		return "", fmt.Errorf("repo %s is not opened in the workspace", in.Repo)
	}
	if repo.Remote == "-" {
		return "", fmt.Errorf("repo %s has no remote", in.Repo)
	}
	// The work branch of the PR, as it was pushed. A previous resolution PR
	// is recorded with its own source: derive the work branch from it.
	work := pr.Source
	if w, ok := strings.CutSuffix(work, resolutionBranch("", target)); ok && w != "" {
		work = w
	}
	remoteURL, err := gitOut(ctx, dir, "config", "--get", "remote."+repo.Remote+".url")
	if err != nil {
		return "", err
	}
	ref, err := bitbucket.ParseRemote(remoteURL)
	if err != nil {
		return "", err
	}
	if err := gitNet(ctx, dir, "fetch", "--quiet", repo.Remote, target, work); err != nil {
		return "", err
	}
	rTarget, rWork := repo.Remote+"/"+target, repo.Remote+"/"+work
	if out, err := gitOut(ctx, dir, "rev-list", "--count", rWork+".."+work); err == nil && strings.TrimSpace(out) != "0" {
		return "", fmt.Errorf("%s has commits not pushed yet; call create_pull_request for %s first (it pushes them to the PR), then merge", work, in.Repo)
	}
	merge := bitbucket.MergeInput{Strategy: in.Strategy}

	conflicts, err := mergeConflicts(ctx, dir, rTarget, rWork)
	if err != nil {
		return "", err
	}
	if len(conflicts) == 0 && in.ResolveOnly {
		return fmt.Sprintf("PR #%d (%s → %s) has no conflict with %s: nothing to resolve. It is not merged; "+
			"merge only when the operator asks.", pr.ID, work, target, target), nil
	}
	if len(conflicts) == 0 {
		done, err := bb.MergeByURL(ctx, pr.URL, merge)
		if err != nil {
			return "", fmt.Errorf("merge PR #%d: %w", pr.ID, err)
		}
		return mergedMsg(done, pr.ID, work, target, pr.URL), nil
	}

	// Conflicts: merge through the resolution branch.
	res := resolutionBranch(work, target)
	steps := fmt.Sprintf("git -C %[1]s fetch %[2]s && git -C %[1]s switch -C %[3]s %[4]s && git -C %[1]s merge --no-ff %[5]s, "+
		"resolve the conflicts (keep both sides' intent; read %[6]s's changes before choosing), build/test, git -C %[1]s add the files and "+
		"git -C %[1]s commit --no-edit, then git -C %[1]s switch %[7]s and call merge_pull_request again (same resolve_only)",
		dir, repo.Remote, res, rTarget, rWork, target, work)
	if _, err := gitOut(ctx, dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+res); err != nil {
		return "", fmt.Errorf("PR #%d (%s → %s) conflicts with %s in: %s.\nResolve them on a separate branch (never merge %s into %s: "+
			"its PR into the base must keep only this task's commits): %s",
			pr.ID, work, target, target, strings.Join(conflicts, ", "), target, work, steps)
	}
	if err := gitIsAncestor(ctx, dir, rTarget, res); err != nil {
		return "", fmt.Errorf("%s moved on since %s was made; merge it in again: git -C %s switch %s && git -C %s merge %s, resolve, commit, switch back to %s, call again",
			target, res, dir, res, dir, rTarget, work)
	}
	if err := gitIsAncestor(ctx, dir, rWork, res); err != nil {
		return "", fmt.Errorf("%s does not contain the latest %s; merge it in: git -C %s switch %s && git -C %s merge %s, commit, switch back to %s, call again",
			res, work, dir, res, dir, rWork, work)
	}
	if cur, _ := gitOut(ctx, dir, "symbolic-ref", "--short", "HEAD"); strings.TrimSpace(cur) == res {
		if out, _ := gitOut(ctx, dir, "status", "--porcelain", "--untracked-files=no"); strings.TrimSpace(out) != "" {
			return "", fmt.Errorf("%s has uncommitted changes (unfinished conflict resolution?); commit them, switch back to %s and call again", res, work)
		}
	}
	if out, err := gitOut(ctx, dir, "grep", "-l", "-E", "^(<<<<<<<|>>>>>>>) ", res, "--"); err == nil && strings.TrimSpace(out) != "" {
		return "", fmt.Errorf("%s still has conflict markers in: %s", res, strings.Join(strings.Fields(out), ", "))
	}
	if out, err := gitOut(ctx, dir, "diff", "--name-only", "--diff-filter=A", rTarget+"..."+res); err != nil {
		return "", err
	} else if added := testFiles(out); len(added) > 0 {
		return "", fmt.Errorf("%s adds test files, which stay local: %s; remove them from its commits and call again", res, strings.Join(added, ", "))
	}
	if err := gitNet(ctx, dir, "push", "--porcelain", "-u", repo.Remote, "refs/heads/"+res+":refs/heads/"+res); err != nil {
		return "", err
	}
	rpr, err := bb.FindOpenPR(ctx, ref, res, target)
	if err != nil {
		return "", err
	}
	if rpr == nil {
		rpr, err = bb.CreatePR(ctx, ref, bitbucket.CreateInput{
			Title: fmt.Sprintf("Merge %s into %s (conflicts resolved) [%s]", work, target, t.TicketID),
			Description: fmt.Sprintf("Same changes as #%d (%s), merged with %s and its conflicts resolved (%s).\n\n---\n"+
				"Ticket: %s %s\nOpened by taskpilot (task `%s`) on the operator's instruction to merge.",
				pr.ID, work, target, strings.Join(conflicts, ", "), t.TicketID, t.Title, t.ID),
			Source: res, Destination: target, CloseSourceBranch: true,
		})
		if err != nil {
			return "", err
		}
	}
	if in.ResolveOnly {
		return fmt.Sprintf("Conflicts resolved on %s and pushed; PR #%d (%s → %s) is open and NOT merged: %s\n"+
			"Tell the operator it is ready. When they ask to merge, call merge_pull_request again without resolve_only "+
			"(it merges #%d and declines #%d).", res, rpr.ID, res, target, rpr.URL(), rpr.ID, pr.ID), nil
	}
	done, err := bb.MergeByURL(ctx, rpr.URL(), merge)
	if err != nil {
		return "", fmt.Errorf("merge PR #%d: %w", rpr.ID, err)
	}
	msg := mergedMsg(done, rpr.ID, res, target, rpr.URL())
	if err := bb.DeclinePR(ctx, ref, pr.ID); err != nil {
		msg += fmt.Sprintf("\nCould not decline the original PR #%d (%v): tell the operator it is superseded.", pr.ID, err)
	} else {
		msg += fmt.Sprintf("\nDeclined the original PR #%d (%s → %s): superseded by #%d.", pr.ID, work, target, rpr.ID)
	}
	if err := h.Store.SetPullRequest(h.TaskID, &store.PullRequest{Repo: in.Repo, ID: rpr.ID, URL: rpr.URL(), Source: res,
		Destination: target, Title: rpr.Title, CreatedAt: time.Now().UTC()}); err != nil {
		return "", err
	}
	return msg, nil
}

// mergeGranted checks the operator's text that started this turn asks to
// merge repo into target, by meaning (claudeMergeIntent), not by a word.
func (h *Handlers) mergeGranted(ctx context.Context, repo, target string) error {
	const no = "Do not merge and do not try another way. If they asked to resolve the conflicts, call merge_pull_request " +
		"with resolve_only: true (it resolves on a branch and opens its PR without merging); otherwise say the PR is ready " +
		"in your request or completion and let the operator decide"
	texts, err := h.Store.OperatorTextOfRun(h.TaskID)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(texts, mergeAsked.MatchString) { // no mention at all: no need to ask
		return errors.New("not allowed: the operator did not ask for a merge in the message (or approval comment) that started this turn. " + no)
	}
	intent := h.MergeIntent
	if intent == nil {
		intent = h.claudeMergeIntent
	}
	ok, reason, err := intent(ctx, texts, repo, target)
	if err != nil {
		return fmt.Errorf("not allowed: could not check that the operator asked for this merge (%v). %s", err, no)
	}
	if !ok {
		return fmt.Errorf("not allowed: the operator's message does not ask to merge %s into %s (%s). %s", repo, target, reason, no)
	}
	return nil
}

func mergedMsg(done bool, id int, source, target, link string) string {
	if !done {
		return fmt.Sprintf("Bitbucket accepted the merge of PR #%d (%s → %s) and finishes it in the background: %s", id, source, target, link)
	}
	return fmt.Sprintf("Merged PR #%d %s → %s: %s\nNext: check the deploy (qc_apps, then qc_logs) as the workflow says.", id, source, target, link)
}

// mergeConflicts lists the files that conflict when merging b into a, or
// none when the merge is clean (git merge-tree, nothing is checked out).
func mergeConflicts(ctx context.Context, dir, a, b string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "merge-tree", "--write-tree", "--name-only", "--no-messages", a, b)
	out, err := cmd.Output()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return nil, nil
	case errors.As(err, &exit) && exit.ExitCode() == 1:
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		if len(lines) > 1 {
			return lines[1:], nil // first line: the tree
		}
		return []string{"(unknown files)"}, nil
	default:
		return nil, fmt.Errorf("git merge-tree %s %s: %v", a, b, err)
	}
}

func gitIsAncestor(ctx context.Context, dir, a, b string) error {
	_, err := gitOut(ctx, dir, "merge-base", "--is-ancestor", a, b)
	return err
}

// gitNet runs a git command that talks to the remote, non-interactively.
func gitNet(ctx context.Context, dir string, args ...string) error {
	pctx, cancel := context.WithTimeout(ctx, PushTimeout)
	defer cancel()
	cmd := exec.CommandContext(pctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_SSH_COMMAND=ssh -o BatchMode=yes")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git %s failed: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
