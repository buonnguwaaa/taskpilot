// Package mcpserver exposes the taskpilot control tools to the agent over MCP.
//
// The agent uses these tools to talk to the harness: ask the operator for an
// approval, declare the task done, or re-read the ticket after a resume.
package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/buonnguwaaa/taskpilot/internal/argocd"
	"github.com/buonnguwaaa/taskpilot/internal/bitbucket"
	"github.com/buonnguwaaa/taskpilot/internal/claudeai"
	"github.com/buonnguwaaa/taskpilot/internal/config"
	"github.com/buonnguwaaa/taskpilot/internal/graphify"
	"github.com/buonnguwaaa/taskpilot/internal/jenkins"
	"github.com/buonnguwaaa/taskpilot/internal/kafbat"
	"github.com/buonnguwaaa/taskpilot/internal/qcdb"
	"github.com/buonnguwaaa/taskpilot/internal/refs"
	"github.com/buonnguwaaa/taskpilot/internal/splunk"
	"github.com/buonnguwaaa/taskpilot/internal/store"
	"github.com/buonnguwaaa/taskpilot/internal/vault"
	"github.com/buonnguwaaa/taskpilot/internal/workspace"
)

// Version is reported to MCP clients.
const Version = "0.1.0"

// RequestApprovalIn is the input of request_approval.
type RequestApprovalIn struct {
	Gate    string `json:"gate" jsonschema:"the gate to open, one of the configured gates (e.g. plan, pr, deploy)"`
	Summary string `json:"summary" jsonschema:"one or two sentences the operator reads in the approval inbox, in the operator's language"`
	Details string `json:"details,omitempty" jsonschema:"full material for the decision: the plan, diff summary, test results, risks"`
	// Questions the operator answers by picking options while deciding.
	Questions []store.Question `json:"questions,omitempty" jsonschema:"optional multiple-choice questions the plan depends on (1-4, each with 2-6 options, recommended option first); the operator picks an answer when deciding"`
}

// AskOperatorIn is the input of ask_operator.
type AskOperatorIn struct {
	Context   string           `json:"context,omitempty" jsonschema:"what you found and why you need a decision, in the operator's language (shown above the questions)"`
	Questions []store.Question `json:"questions" jsonschema:"1-4 questions, each with 2-6 short options; put the recommended option first and say so in its description; the operator can always write another answer"`
}

// CompleteTaskIn is the input of complete_task.
type CompleteTaskIn struct {
	Summary string `json:"summary" jsonschema:"what was delivered, in the operator's language"`
	PRURL   string `json:"pr_url,omitempty" jsonschema:"link to the pull request, if one was created"`
}

// TaskInfoIn is the (empty) input of task_info.
type TaskInfoIn struct{}

// Handlers holds the tool implementations; split out so tests call them directly.
type Handlers struct {
	Store  *store.Store
	Config *config.Config
	TaskID string
	// Bitbucket returns a client with the operator's credentials; the token
	// stays inside this process and never reaches the agent.
	Bitbucket func(ctx context.Context) (*bitbucket.Client, error)
	// Splunk returns a client with the operator's Splunk credentials.
	Splunk func(ctx context.Context) (*splunk.Client, error)
	// ArgoCD returns a client for the QC Argo CD instance.
	ArgoCD func(ctx context.Context) (*argocd.Client, error)
	// Vault returns a client for the QC Vault with the operator's token.
	Vault func(ctx context.Context) (*vault.Client, error)
	// Kafka returns a client for the QC Kafka UI with the operator's session,
	// and the cluster to read.
	Kafka func(ctx context.Context) (*kafbat.Client, string, error)
	// ClaudeArtifact downloads a claude.ai artifact (prototype) into dir with
	// the operator's claude.ai session.
	ClaudeArtifact func(ctx context.Context, link, dir string) (*claudeai.Artifact, error)
	// MergeIntent decides whether the operator's texts ask to merge repo into
	// target (nil: ask Claude). Tests replace it.
	MergeIntent func(ctx context.Context, texts []string, repo, target string) (ok bool, reason string, err error)
	// Jenkins returns a read-only Jenkins client with the operator's API token.
	Jenkins func(ctx context.Context) (*jenkins.Client, error)
	// Secret reads a value from the operator's secret store (a QC database
	// DSN not kept in a repo file); the value never reaches the agent.
	Secret func(ctx context.Context, name string) (string, error)

	// graphUsed: graphify was called in this run; graphNudged: a plan
	// request was sent back once to use it first.
	graphUsed, graphNudged atomic.Bool
}

// RequestApproval records an approval request for the operator.
func (h *Handlers) RequestApproval(in RequestApprovalIn) (string, error) {
	gate := strings.TrimSpace(in.Gate)
	if !slices.Contains(h.Config.Gates, gate) {
		return "", fmt.Errorf("unknown gate %q; valid gates: %s", gate, strings.Join(h.Config.Gates, ", "))
	}
	if strings.TrimSpace(in.Summary) == "" {
		return "", fmt.Errorf("summary is required")
	}
	if pending, err := h.Store.PendingRequest(h.TaskID); err != nil {
		return "", err
	} else if pending != nil {
		return "", fmt.Errorf("request %s for gate %q is still waiting for the operator; end your turn now", pending.ID, pending.Gate)
	}
	if gate == "plan" && h.needGraphify() {
		return "", errors.New("not sent: plan without graphify. The project has graphify graphs: before asking for the plan, " +
			"call the graphify tool — affected on each function you plan to change (line task, and line qc to see whether QC differs), " +
			"links for any service or gRPC method involved — and list the callers it found in the plan. " +
			"If graphify cannot help for this task, say why in the plan. Then call request_approval again (it goes through)")
	}
	r, err := h.Store.AddRequestWithQuestions(h.TaskID, gate, in.Summary, in.Details, in.Questions)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Approval request %s recorded for gate %q. Do not do any more work now: end your turn with a one-line status. "+
		"The session will be resumed with the operator's decision.", r.ID, gate), nil
}

// needGraphify reports whether a plan request goes back once to use
// graphify first: the project has graphs and the task never called it.
func (h *Handlers) needGraphify() bool {
	if h.graphUsed.Load() || h.Config.Graphify == nil || h.graphNudged.Load() {
		return false
	}
	t, err := h.Store.LoadTask(h.TaskID)
	if err != nil || len(h.Config.ProjectGraphLines(t.Project)) == 0 {
		return false
	}
	if used, err := h.Store.UsedGraphify(h.TaskID); err != nil || used {
		return false
	}
	return !h.graphNudged.Swap(true)
}

// AskOperator sends multiple-choice questions to the operator and parks the
// task until they answer, like Claude Code's AskUserQuestion.
func (h *Handlers) AskOperator(in AskOperatorIn) (string, error) {
	if len(in.Questions) == 0 {
		return "", fmt.Errorf("questions are required")
	}
	if pending, err := h.Store.PendingRequest(h.TaskID); err != nil {
		return "", err
	} else if pending != nil {
		return "", fmt.Errorf("request %s is still waiting for the operator; end your turn now", pending.ID)
	}
	summary := strings.TrimSpace(in.Context)
	if summary == "" {
		summary = in.Questions[0].Question
	}
	r, err := h.Store.AddRequestWithQuestions(h.TaskID, store.QuestionGate, summary, "", in.Questions)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Question(s) sent to the operator as request %s. End your turn now with a one-line status; "+
		"the session resumes with their answers.", r.ID), nil
}

// CompleteTask marks the task complete.
func (h *Handlers) CompleteTask(in CompleteTaskIn) (string, error) {
	if strings.TrimSpace(in.Summary) == "" {
		return "", fmt.Errorf("summary is required")
	}
	if pending, err := h.Store.PendingRequest(h.TaskID); err != nil {
		return "", err
	} else if pending != nil {
		return "", fmt.Errorf("request %s is still pending; the task cannot complete before the operator decides", pending.ID)
	}
	if err := h.Store.SetCompletion(h.TaskID, &store.Completion{Summary: in.Summary, PRURL: in.PRURL, CreatedAt: time.Now().UTC()}); err != nil {
		return "", err
	}
	return "Task marked complete. End your turn now.", nil
}

// TaskInfo describes the ticket, the project's repos and the gate status.
func (h *Handlers) TaskInfo() (string, error) {
	t, err := h.Store.LoadTask(h.TaskID)
	if err != nil {
		return "", err
	}
	proj, ok := h.Config.Projects[t.Project]
	if !ok {
		return "", fmt.Errorf("task project %q is not configured", t.Project)
	}
	gates := map[string]string{}
	for _, g := range h.Config.Gates {
		ok, err := h.Store.GateApproved(h.TaskID, g)
		if err != nil {
			return "", err
		}
		gates[g] = map[bool]string{true: "approved", false: "not approved"}[ok]
	}
	reqs, err := h.Store.Requests(h.TaskID)
	if err != nil {
		return "", err
	}
	decs, err := h.Store.Decisions(h.TaskID)
	if err != nil {
		return "", err
	}
	type reqView struct {
		ID, Gate, Summary, Decision, Comment string
	}
	var history []reqView
	for _, r := range reqs {
		v := reqView{ID: r.ID, Gate: r.Gate, Summary: r.Summary, Decision: "pending"}
		if d, ok := decs[r.ID]; ok {
			v.Decision = map[bool]string{true: "approved", false: "rejected"}[d.Approved]
			v.Comment = d.Comment
		}
		history = append(history, v)
	}
	baselines, err := h.Store.Baselines(h.TaskID)
	if err != nil {
		return "", err
	}
	prs, err := h.Store.PullRequests(h.TaskID)
	if err != nil {
		return "", err
	}
	opened := map[string]bool{}
	for _, r := range workspace.OpenedRepos(t.Workspace) {
		opened[r] = true
	}
	type repoView struct {
		Name           string            `json:"name"`
		Description    string            `json:"description,omitempty"`
		BaseBranch     string            `json:"base_branch"`
		Verify         string            `json:"verify_command,omitempty"`
		PRFlow         string            `json:"pr_flow"`
		Opened         bool              `json:"opened"`
		PullRequests   map[string]string `json:"pull_requests,omitempty"` // target → URL
		Path           string            `json:"path,omitempty"`
		BaselinePassed *bool             `json:"baseline_verify_passed,omitempty"`
	}
	var repos []repoView
	for _, name := range proj.RepoNames() {
		r := proj.Repos[name]
		v := repoView{Name: name, Description: r.Description, BaseBranch: r.Base, PRFlow: flowString(r), Verify: r.Verify, Opened: opened[name]}
		for _, pr := range prs {
			if pr.Repo == name {
				if v.PullRequests == nil {
					v.PullRequests = map[string]string{}
				}
				v.PullRequests[pr.Destination] = pr.URL
			}
		}
		if v.Opened {
			v.Path = filepath.Join(t.Workspace, name)
		}
		if b, ok := baselines[name]; ok {
			passed := b.Passed
			v.BaselinePassed = &passed
		}
		repos = append(repos, v)
	}
	info := map[string]any{
		"task_id":     t.ID,
		"ticket_id":   t.TicketID,
		"ticket_url":  t.TicketURL,
		"title":       t.Title,
		"body":        t.Body,
		"project":     proj.Name,
		"workspace":   t.Workspace,
		"work_branch": t.Branch,
		"branch_convention": "<type>/<english-kebab-description>--<FUNCTION_CODE>, e.g. feat/add-hawaiian-lifestyle-category--PLM_DSN_STY_0013 " +
			"(type: feat, fix, hotfix, refactor, perf, chore…; several codes comma-separated; a ticket code such as HSK-SLJ68DMP " +
			"only when no function code applies). Set it with set_branch (or git branch -m in each repo) before the first commit. Must match: " + proj.BranchRule().String(),
		"repos":         repos,
		"context_files": proj.ContextFiles,
		"allowed_tools": proj.AllowedToolList(),
		"granted_tools": h.grantedView(t.ID, t.Project),
		"tool_rules": "Only allowed_tools run; anything else is denied without asking. A denial applies to that one " +
			"command, not to Bash as a whole: keep using the allowed commands. When a command you really need is denied, call " +
			"request_permission (narrowest rule, the command, why) and end your turn; granted_tools lists what the operator allowed. Change files with Edit/Write, never " +
			"with shell redirection (cat > file, sed -i, tee). Writes are only allowed inside the workspace.",
		"gates":             gates,
		"approval_history":  history,
		"operator_language": h.Config.OperatorLanguage,
		"execution_profile": h.profileView(),
		"graphify":          h.graphifyView(proj.Name, t.Project),
		"qc_config_paths":   proj.VaultPaths,
		"qc_kafka_topics":   proj.KafkaTopics,
	}
	b, err := json.MarshalIndent(info, "", "  ")
	return string(b), err
}

// profileView tells the agent which model/effort it runs with and what it
// may pick for the next runs.
func (h *Handlers) profileView() map[string]any {
	c := h.Config.Claude
	v := map[string]any{
		"current_model":   orDefault(c.Model),
		"current_effort":  orDefault(c.Effort),
		"set_by":          "config default",
		"allowed_models":  c.Models,
		"allowed_efforts": config.Efforts,
		"note":            "Pick the profile for the rest of the task with set_execution_profile; it applies from the next run.",
	}
	p, err := h.Store.TaskProfile(h.TaskID)
	if err != nil || p == nil {
		if a, _ := h.Store.TaskAssessment(h.TaskID); a != nil {
			if lp, ok := c.ForLevel(a.Level); ok {
				if lp.Model != "" {
					v["current_model"] = lp.Model
				}
				if lp.Effort != "" {
					v["current_effort"] = lp.Effort
				}
				v["set_by"] = "level " + a.Level + " (claude.level_profiles)"
			}
		}
		return v
	}
	if p.Model != "" {
		v["current_model"] = p.Model
	}
	if p.Effort != "" {
		v["current_effort"] = p.Effort
	}
	v["set_by"], v["reason"] = p.SetBy, p.Reason
	if p.SetBy == "operator" {
		v["note"] = "The operator pinned this profile; do not try to change it."
	}
	return v
}

func orDefault(s string) string {
	if s == "" {
		return "Claude Code default"
	}
	return s
}

// SetProfileIn is the input of set_execution_profile.
type SetProfileIn struct {
	Model  string `json:"model" jsonschema:"one of task_info.execution_profile.allowed_models"`
	Effort string `json:"effort" jsonschema:"low, medium, high, xhigh or max"`
	Reason string `json:"reason" jsonschema:"one or two sentences: the assessment behind the choice (size, risk, ambiguity, kind of work)"`
	Level  string `json:"level,omitempty" jsonschema:"your difficulty assessment: light, standard, hard or hardest (shown to the operator)"`
	Kind   string `json:"kind,omitempty" jsonschema:"kind of work: feature, bugfix, investigation, improvement, refactor, config, test, docs, ops or other (shown to the operator)"`
}

// SetExecutionProfile records the model and effort for the task's next runs.
func (h *Handlers) SetExecutionProfile(in SetProfileIn) (string, error) {
	model, effort := strings.TrimSpace(in.Model), strings.ToLower(strings.TrimSpace(in.Effort))
	if !h.Config.Claude.AllowsModel(model) {
		return "", fmt.Errorf("model %q is not allowed; pick one of %s", model, strings.Join(h.Config.Claude.Models, ", "))
	}
	if !config.ValidEffort(effort) {
		return "", fmt.Errorf("effort %q is not valid; pick one of %s", effort, strings.Join(config.Efforts, ", "))
	}
	if strings.TrimSpace(in.Reason) == "" {
		return "", fmt.Errorf("reason is required: say what in the task justifies this profile")
	}
	level, kind := strings.ToLower(strings.TrimSpace(in.Level)), strings.ToLower(strings.TrimSpace(in.Kind))
	if level != "" && !slices.Contains(store.Levels, level) {
		return "", fmt.Errorf("level %q is not valid; pick one of %s", level, strings.Join(store.Levels, ", "))
	}
	if kind != "" && !slices.Contains(store.Kinds, kind) {
		return "", fmt.Errorf("kind %q is not valid; pick one of %s", kind, strings.Join(store.Kinds, ", "))
	}
	// The assessment is recorded even when the operator pinned the profile.
	if level != "" || kind != "" {
		if err := h.Store.SetAssessment(h.TaskID, &store.Assessment{Level: level, Kind: kind, Reason: strings.TrimSpace(in.Reason)}); err != nil {
			return "", err
		}
	}
	err := h.Store.SetTaskProfile(h.TaskID, &store.Profile{Model: model, Effort: effort, Reason: strings.TrimSpace(in.Reason), SetBy: "agent"})
	if errors.Is(err, store.ErrProfilePinned) {
		p, _ := h.Store.TaskProfile(h.TaskID)
		return fmt.Sprintf("Not changed: the operator pinned model %s, effort %s for this task. Continue with it.", orDefault(p.Model), orDefault(p.Effort)), nil
	}
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Execution profile set: model %s, effort %s. It applies from the next run (after the next gate); "+
		"this run continues as it started.", model, effort), nil
}

// OpenRepoIn is the input of open_repo.
type OpenRepoIn struct {
	Name string `json:"name" jsonschema:"repo name from task_info.repos"`
}

// SetBranchIn is the input of set_branch.
type SetBranchIn struct {
	Name string `json:"name" jsonschema:"the work branch name by the team convention (task_info.branch_convention), e.g. feat/add-hawaiian-lifestyle-category--PLM_DSN_STY_0013"`
}

// SetBranch names the task's work branch by the team convention, renaming it
// in every opened repo. Repos opened later start on the new name. A PR
// already opened keeps its source; the next create_pull_request opens new ones.
func (h *Handlers) SetBranch(ctx context.Context, in SetBranchIn) (string, error) {
	name := strings.TrimSpace(in.Name)
	t, err := h.Store.LoadTask(h.TaskID)
	if err != nil {
		return "", err
	}
	proj, ok := h.Config.Projects[t.Project]
	if !ok {
		return "", fmt.Errorf("task project %q is not configured", t.Project)
	}
	if rule := proj.BranchRule(); !rule.MatchString(name) {
		return "", fmt.Errorf("%q does not follow the convention <type>/<english-kebab-description>--<FUNCTION_CODE> (must match %s)", name, rule)
	}
	old := t.WorkBranch()
	if name == old {
		return "work branch is already " + name, nil
	}
	prs, err := h.Store.PullRequests(h.TaskID)
	if err != nil {
		return "", err
	}
	repos, err := workspace.RenameBranch(ctx, t.Workspace, old, name)
	if err != nil {
		return "", err
	}
	t.Branch = name
	if err := h.Store.SaveTask(t); err != nil {
		// Keep the repos and the task in step.
		_, _ = workspace.RenameBranch(ctx, t.Workspace, name, old)
		return "", err
	}
	msg := fmt.Sprintf("work branch renamed %s → %s", old, name)
	if len(repos) > 0 {
		msg += " in " + strings.Join(repos, ", ")
	}
	msg += ". Repos opened from now on start on it; create_pull_request pushes it."
	if len(prs) > 0 {
		msg += " PRs already opened from " + old + " keep that source: create_pull_request opens new ones from " + name +
			"; tell the operator to decline the old ones."
	}
	return msg, nil
}

// VerifyTimeout bounds the baseline verify run.
var VerifyTimeout = 15 * time.Minute

// OpenRepo checks a project repo out into the task workspace and records the
// verify result on the untouched base the first time.
func (h *Handlers) OpenRepo(ctx context.Context, in OpenRepoIn) (string, error) {
	t, err := h.Store.LoadTask(h.TaskID)
	if err != nil {
		return "", err
	}
	proj, ok := h.Config.Projects[t.Project]
	if !ok {
		return "", fmt.Errorf("task project %q is not configured", t.Project)
	}
	repo, ok := proj.Repos[in.Name]
	if !ok {
		return "", fmt.Errorf("repo %q is not part of project %s; available: %s", in.Name, proj.Name, strings.Join(proj.RepoNames(), ", "))
	}
	o, err := workspace.OpenRepo(ctx, t.Workspace, t.WorkBranch(), in.Name, repo)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Repo %s is at %s on branch %s (base %s).\n", in.Name, o.Path, o.Branch, repo.Base)
	for _, n := range o.Notes {
		fmt.Fprintf(&b, "Note: %s\n", n)
	}
	baselines, err := h.Store.Baselines(h.TaskID)
	if err != nil {
		return "", err
	}
	base, have := baselines[in.Name]
	if !have && repo.Verify != "" && o.Created {
		r := workspace.RunVerify(ctx, o.Path, repo.Verify, VerifyTimeout)
		base = &store.Baseline{Repo: in.Name, Command: repo.Verify, Passed: r.Passed, Output: r.Output,
			DurationMS: r.Duration.Milliseconds(), At: time.Now().UTC()}
		if err := h.Store.SetBaseline(h.TaskID, base); err != nil {
			return "", err
		}
	}
	switch {
	case repo.Verify == "":
		b.WriteString("This repo has no verify command.\n")
	case base == nil:
		fmt.Fprintf(&b, "Verify command: %s (no baseline recorded).\n", repo.Verify)
	case base.Passed:
		fmt.Fprintf(&b, "Baseline: `%s` PASSES on the untouched base. It must still pass after your change.\n", repo.Verify)
	default:
		fmt.Fprintf(&b, "Baseline: `%s` ALREADY FAILS on the untouched base (not caused by you). Do not fix unrelated failures; "+
			"make sure you add no new ones, and list the pre-existing failures in your pr request. Output tail:\n%s\n",
			repo.Verify, base.Output)
	}
	return b.String(), nil
}

// CreatePullRequestIn is the input of create_pull_request.
type CreatePullRequestIn struct {
	Repo        string `json:"repo" jsonschema:"an opened repo of the task with committed changes"`
	Title       string `json:"title" jsonschema:"PR title, e.g. 'feat(order): cap discount at 50% [HSK-9772A8LR]'"`
	Description string `json:"description" jsonschema:"PR description in markdown: what and why, how it was verified, risks"`
	Target      string `json:"target,omitempty" jsonschema:"optional target branch from the repo's pr_flow; default: the next stage whose gate is approved"`
}

// PushTimeout bounds git push.
var PushTimeout = 3 * time.Minute

// CreatePullRequest pushes the work branch of a repo and opens (or reuses) a
// PR into one stage of the repo's pr_flow, once that stage's gate is approved.
func (h *Handlers) CreatePullRequest(ctx context.Context, in CreatePullRequestIn) (string, error) {
	if strings.TrimSpace(in.Title) == "" {
		return "", fmt.Errorf("title is required")
	}
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
	dir := filepath.Join(t.Workspace, in.Repo)
	cur, _ := gitOut(ctx, dir, "symbolic-ref", "--short", "HEAD") // "" until the repo is opened; checked below
	stage, err := h.pickStage(in.Repo, repo, in.Target, strings.TrimSpace(cur))
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		return "", fmt.Errorf("repo %s is not opened in the workspace", in.Repo)
	}
	// The source is the branch the repo is on: the agent may have renamed it
	// (set_branch or git branch -m); the task follows it.
	head, err := gitOut(ctx, dir, "symbolic-ref", "--short", "HEAD")
	head = strings.TrimSpace(head)
	if err != nil || head == "" {
		return "", fmt.Errorf("repo %s is not on a branch (detached HEAD); check out the work branch", in.Repo)
	}
	if !proj.BranchRule().MatchString(head) {
		return "", fmt.Errorf("repo %s is on %s, which does not follow the team convention (task_info.branch_convention); "+
			"rename it (git branch -m or set_branch), e.g. feat/<english-kebab-description>--<FUNCTION_CODE>", in.Repo, head)
	}
	if head != t.Branch {
		t.Branch = head // repos opened later start on it
		if err := h.Store.SaveTask(t); err != nil {
			return "", err
		}
	}
	// Untracked files are never pushed: local-only tests may stay there.
	if out, err := gitOut(ctx, dir, "status", "--porcelain", "--untracked-files=no"); err != nil {
		return "", err
	} else if strings.TrimSpace(out) != "" {
		return "", fmt.Errorf("repo %s has uncommitted changes; commit them first:\n%s", in.Repo, out)
	}
	base := repo.Base
	if repo.Remote != "-" {
		base = repo.Remote + "/" + repo.Base
	}
	if out, err := gitOut(ctx, dir, "rev-list", "--count", base+"..HEAD"); err != nil {
		return "", err
	} else if strings.TrimSpace(out) == "0" {
		return "", fmt.Errorf("repo %s has no commits on %s beyond %s; nothing to open a PR for", in.Repo, head, base)
	}
	// The operator's rule: tests are for verifying locally, not for the PR.
	if out, err := gitOut(ctx, dir, "diff", "--name-only", "--diff-filter=A", base+"...HEAD"); err != nil {
		return "", err
	} else if added := testFiles(out); len(added) > 0 {
		return "", fmt.Errorf("repo %s: the commits add test files, which stay local (verify with them, do not commit them): %s. "+
			"Remove them from the commits (e.g. git rm --cached <file> and amend or add a commit), keep them untracked, then call create_pull_request again",
			in.Repo, strings.Join(added, ", "))
	}
	if repo.Remote == "-" {
		return "", fmt.Errorf("repo %s has no remote configured (remote: \"-\"); cannot open a PR", in.Repo)
	}
	// The configured URL, not `remote get-url`: that applies insteadOf rewrites.
	remoteURL, err := gitOut(ctx, dir, "config", "--get", "remote."+repo.Remote+".url")
	if err != nil {
		return "", err
	}
	ref, err := bitbucket.ParseRemote(remoteURL)
	if err != nil {
		return "", err
	}
	// Without API credentials the branch is still pushed with the machine's own
	// git credentials (SSH key); the operator then opens the pre-filled PR page.
	var bb *bitbucket.Client
	if h.Bitbucket != nil {
		bb, err = h.Bitbucket(ctx)
		if err != nil && !errors.Is(err, bitbucket.ErrNoCredentials) {
			return "", err
		}
	}

	renamed := ""
	if old, err := h.Store.PullRequestInto(h.TaskID, in.Repo, stage.Target); err == nil && old != nil && old.Source != "" && old.Source != head &&
		old.Source != resolutionBranch(head, stage.Target) {
		renamed = fmt.Sprintf("\nThe branch was renamed after PR %s (from %s) was opened: that PR stays open on Bitbucket; tell the operator to decline it.", old.URL, old.Source)
	}
	pctx, cancel := context.WithTimeout(ctx, PushTimeout)
	defer cancel()
	push := exec.CommandContext(pctx, "git", "-C", dir, "push", "--porcelain", "-u", repo.Remote, "HEAD:refs/heads/"+head)
	push.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_SSH_COMMAND=ssh -o BatchMode=yes")
	if out, err := push.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git push %s failed: %v: %s", head, err, strings.TrimSpace(string(out)))
	}

	desc := strings.TrimSpace(in.Description) + fmt.Sprintf("\n\n---\nTicket: %s %s\nOpened by taskpilot (task `%s`) after operator approval.", t.TicketID, t.Title, t.ID)
	if bb == nil {
		link := bitbucket.NewPRURL(ref, head, stage.Target)
		rec := &store.PullRequest{Repo: in.Repo, URL: link, Source: head, Destination: stage.Target,
			Title: in.Title, Description: desc, Pending: true, CreatedAt: time.Now().UTC()}
		if err := h.Store.SetPullRequest(h.TaskID, rec); err != nil {
			return "", err
		}
		return fmt.Sprintf("Pushed %s to %s. Bitbucket API credentials are not configured, so the PR is not created "+
			"automatically: the operator opens it from the taskpilot TUI (key p) with your title and description pre-copied. "+
			"Do not retry and do not work around this; mention it in complete_task. Create page: %s%s", head, ref, link, renamed), nil
	}

	pr, err := bb.FindOpenPR(ctx, ref, head, stage.Target)
	if err != nil {
		return "", err
	}
	reused := pr != nil
	if pr == nil {
		pr, err = bb.CreatePR(ctx, ref, bitbucket.CreateInput{
			Title: in.Title, Description: desc, Source: head, Destination: stage.Target,
		})
		if err != nil {
			return "", err
		}
	}
	rec := &store.PullRequest{Repo: in.Repo, ID: pr.ID, URL: pr.URL(), Source: head,
		Destination: stage.Target, Title: pr.Title, CreatedAt: time.Now().UTC()}
	if err := h.Store.SetPullRequest(h.TaskID, rec); err != nil {
		return "", err
	}
	verb := "Opened"
	if reused {
		verb = "Pushed new commits; the existing"
	}
	msg := fmt.Sprintf("%s pull request #%d %s → %s: %s%s", verb, pr.ID, head, stage.Target, pr.URL(), renamed)
	if next := nextStage(repo, stage); next != nil {
		if ok, _ := h.Store.GateApproved(h.TaskID, next.Gate); ok {
			msg += fmt.Sprintf("\nNext stage of this repo: PR into %s, already allowed (gate %q approved): call create_pull_request again for %s now.",
				next.Target, next.Gate, in.Repo)
		} else {
			msg += fmt.Sprintf("\nNext stage of this repo: PR into %s after the %q gate is approved.", next.Target, next.Gate)
		}
	}
	return msg, nil
}

// testFile matches test sources and fixtures in the languages of the team's
// repos (Go, JS/TS, Python).
var testFile = regexp.MustCompile(`(_test\.go|\.(test|spec)\.[cm]?[jt]sx?|_test\.py)$|(^|/)(test_[^/]*\.py|__tests__/|testdata/)`)

// testFiles returns the test files among git's list of paths.
func testFiles(paths string) []string {
	var out []string
	for _, p := range strings.Fields(paths) {
		if testFile.MatchString(p) {
			out = append(out, p)
		}
	}
	return out
}

// pickStage chooses the pr_flow stage: the requested target, or else the first
// stage whose gate is approved and that has no PR yet from branch (falling back to the
// last approved stage, which just pushes new commits to its PR).
func (h *Handlers) pickStage(repoName string, repo config.Repo, target, branch string) (config.PRStage, error) {
	if len(repo.PRFlow) == 0 {
		return config.PRStage{}, fmt.Errorf("repo %s has no pr_flow configured", repoName)
	}
	approved := func(st config.PRStage) (bool, error) { return h.Store.GateApproved(h.TaskID, st.Gate) }
	if target != "" {
		st, ok := repo.Stage(target)
		if !ok {
			return st, fmt.Errorf("%s is not a PR target of %s; its flow is %s", target, repoName, flowString(repo))
		}
		if ok, err := approved(st); err != nil || !ok {
			return st, fmt.Errorf("a PR into %s needs the %q gate; call request_approval with gate=%q and end your turn", st.Target, st.Gate, st.Gate)
		}
		return st, nil
	}
	var last *config.PRStage
	for i, st := range repo.PRFlow {
		ok, err := approved(st)
		if err != nil {
			return st, err
		}
		if !ok {
			break // stages are sequential
		}
		last = &repo.PRFlow[i]
		existing, err := h.Store.PullRequestInto(h.TaskID, repoName, st.Target)
		if err != nil {
			return st, err
		}
		if existing == nil || branch != "" && existing.Source != branch { // none yet from this branch (renamed since)
			return st, nil
		}
	}
	if last == nil {
		first := repo.PRFlow[0]
		return first, fmt.Errorf("the %q gate is not approved; call request_approval with gate=%q and end your turn", first.Gate, first.Gate)
	}
	return *last, nil
}

func nextStage(repo config.Repo, cur config.PRStage) *config.PRStage {
	for i, st := range repo.PRFlow {
		if st.Target == cur.Target && i+1 < len(repo.PRFlow) {
			return &repo.PRFlow[i+1]
		}
	}
	return nil
}

func flowString(repo config.Repo) string {
	var parts []string
	for _, st := range repo.PRFlow {
		parts = append(parts, fmt.Sprintf("%s (gate %s)", st.Target, st.Gate))
	}
	return strings.Join(parts, " → ")
}

// SplunkSearchIn is the input of splunk_search.
type SplunkSearchIn struct {
	Query     string `json:"query" jsonschema:"SPL base search plus optional pipes, e.g. 'kubernetes.container_name=payment-worker level=error | head 50'. Read-only commands only; no subsearches."`
	Earliest  string `json:"earliest,omitempty" jsonschema:"relative start like -15m, -4h, -2d (default -1h), an ISO time 2026-11-09T10:00:00, or 0 for all time when the operator allows it"`
	Latest    string `json:"latest,omitempty" jsonschema:"end time, default now"`
	MaxEvents int    `json:"max_events,omitempty" jsonschema:"events to return (capped by the operator's config)"`
}

var isoTime = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}(:\d{2})?$`)

// SplunkSearch runs a read-only, project-scoped Splunk search.
func (h *Handlers) SplunkSearch(ctx context.Context, in SplunkSearchIn) (string, error) {
	sc := h.Config.Splunk
	if sc.URL == "" {
		return "", fmt.Errorf("splunk is not configured (splunk.url in the taskpilot config)")
	}
	if strings.TrimSpace(in.Query) == "" {
		return "", fmt.Errorf("query is required")
	}
	if err := splunk.Validate(in.Query); err != nil {
		return "", err
	}
	earliest := strings.TrimSpace(in.Earliest)
	if earliest == "" {
		earliest = sc.DefaultEarliest
	}
	unlimited := sc.MaxLookback <= 0
	if earliest == "0" {
		if !unlimited {
			return "", fmt.Errorf("all-time search (earliest 0) is not allowed; the limit is %s back", sc.MaxLookback)
		}
	} else if d, ok := splunk.Lookback(earliest); ok {
		if !unlimited && d > sc.MaxLookback {
			return "", fmt.Errorf("earliest %s reaches back further than the allowed %s; narrow the time range", earliest, sc.MaxLookback)
		}
	} else if !isoTime.MatchString(earliest) {
		return "", fmt.Errorf("earliest must be relative (-15m, -4h, -2d) or ISO (2026-11-09T10:00:00), got %q", earliest)
	} else {
		iso := earliest
		if len(iso) == len("2006-01-02T15:04") {
			iso += ":00"
		}
		t, err := time.ParseInLocation("2006-01-02T15:04:05", iso, time.Local)
		if err != nil || !unlimited && time.Since(t) > sc.MaxLookback {
			return "", fmt.Errorf("earliest %s is invalid or older than the allowed %s", earliest, sc.MaxLookback)
		}
	}
	max := in.MaxEvents
	if sc.MaxEvents > 0 && (max <= 0 || max > sc.MaxEvents) {
		max = sc.MaxEvents
	}
	if max < 0 {
		max = 0 // no limit
	}
	t, err := h.Store.LoadTask(h.TaskID)
	if err != nil {
		return "", err
	}
	if p := h.Config.Projects[t.Project]; p.NoLogs {
		return "", fmt.Errorf("project %s has no logs in Splunk (no_logs); investigate with the code, the database or the ticket instead", p.Name)
	}
	query := splunk.Scope(h.Config.Projects[t.Project].SplunkFilter, in.Query)
	if h.Splunk == nil {
		return "", splunk.ErrNoCredentials
	}
	client, err := h.Splunk(ctx)
	if err != nil {
		return "", err
	}
	sctx, cancel := context.WithTimeout(ctx, sc.Timeout)
	defer cancel()
	events, err := client.Search(sctx, splunk.Request{SPL: query, Earliest: earliest, Latest: in.Latest, MaxEvents: max})
	rec := &store.SplunkQuery{At: time.Now().UTC(), Query: query, Earliest: earliest, Latest: in.Latest, Events: len(events)}
	if err != nil {
		rec.Error = err.Error()
	}
	_ = h.Store.AppendSplunkQuery(h.TaskID, rec)
	if err != nil {
		return "", err
	}
	return splunk.Format(query, earliest, events, max), nil
}

// DBQueryIn is the input of db_query.
type DBQueryIn struct {
	SQL     string `json:"sql" jsonschema:"one read-only MySQL statement: SELECT, SHOW, DESCRIBE or EXPLAIN. Always filter (WHERE) and select only the columns you need."`
	MaxRows int    `json:"max_rows,omitempty" jsonschema:"rows to return (capped by the operator's config, default 200)"`
}

// DBQuery runs one read-only statement on the task project's QC database.
func (h *Handlers) DBQuery(ctx context.Context, in DBQueryIn) (string, error) {
	t, err := h.Store.LoadTask(h.TaskID)
	if err != nil {
		return "", err
	}
	p := h.Config.Projects[t.Project]
	if p.QCDB == nil {
		return "", fmt.Errorf("project %s has no QC database configured; ask the operator if you need data", p.Name)
	}
	if err := qcdb.Validate(in.SQL); err != nil {
		return "", err
	}
	dsn, err := qcdb.DSN(ctx, t.Project, p.QCDB, h.Secret)
	if err != nil {
		return "", err
	}
	max := in.MaxRows
	if max <= 0 || max > p.QCDB.MaxRows {
		max = p.QCDB.MaxRows
	}
	res, err := qcdb.Query(ctx, dsn, in.SQL, max, p.QCDB.Timeout)
	rec := &store.DBQuery{At: time.Now().UTC(), Target: qcdb.Target(dsn), Query: strings.TrimSpace(in.SQL)}
	if err != nil {
		rec.Error = err.Error()
	} else {
		rec.Rows = len(res.Rows)
	}
	_ = h.Store.AppendDBQuery(h.TaskID, rec)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("QC database %s (read-only)\n%s", rec.Target, qcdb.Format(res)), nil
}

// QCConfigIn is the input of qc_config_list / qc_config_get.
type QCConfigIn struct {
	Path string `json:"path,omitempty" jsonschema:"logical Vault path under the project's allowed paths (task_info.qc_config_paths), e.g. kv/payment/payment-vnpay; empty lists the allowed roots"`
}

// qcConfig checks the path against the project's vault_paths and returns a
// client; every call is audited.
func (h *Handlers) qcConfig(ctx context.Context, action, p string) (*vault.Client, []string, error) {
	t, err := h.Store.LoadTask(h.TaskID)
	if err != nil {
		return nil, nil, err
	}
	proj := h.Config.Projects[t.Project]
	if h.Config.Vault.URL == "" || len(proj.VaultPaths) == 0 {
		return nil, nil, fmt.Errorf("project %s has no QC Vault paths configured (vault_paths); ask the operator", proj.Name)
	}
	if p != "" && !vault.Allowed(p, proj.VaultPaths) {
		return nil, proj.VaultPaths, fmt.Errorf("%q is outside this project's QC Vault paths: %s", p, strings.Join(proj.VaultPaths, ", "))
	}
	if h.Vault == nil {
		return nil, nil, vault.ErrNoCredentials
	}
	c, err := h.Vault(ctx)
	return c, proj.VaultPaths, err
}

func (h *Handlers) auditVault(action, p string, keys int, err error) {
	r := &store.VaultRead{At: time.Now().UTC(), Action: action, Path: p, Keys: keys}
	if err != nil {
		r.Error = err.Error()
	}
	_ = h.Store.AppendVaultRead(h.TaskID, r)
}

// QCConfigList lists keys of the QC Vault under an allowed path.
func (h *Handlers) QCConfigList(ctx context.Context, in QCConfigIn) (string, error) {
	c, roots, err := h.qcConfig(ctx, "list", in.Path)
	if err != nil {
		h.auditVault("list", in.Path, 0, err)
		return "", err
	}
	if strings.TrimSpace(in.Path) == "" {
		return "QC Vault paths this project may read:\n" + strings.Join(roots, "\n"), nil
	}
	keys, err := c.List(ctx, in.Path)
	h.auditVault("list", in.Path, len(keys), err)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s (QC Vault, folders end in /):\n%s", in.Path, strings.Join(keys, "\n")), nil
}

// QCConfigGet reads one secret of the QC Vault under an allowed path.
func (h *Handlers) QCConfigGet(ctx context.Context, in QCConfigIn) (string, error) {
	if strings.TrimSpace(in.Path) == "" {
		return "", fmt.Errorf("path is required")
	}
	c, _, err := h.qcConfig(ctx, "get", in.Path)
	if err != nil {
		h.auditVault("get", in.Path, 0, err)
		return "", err
	}
	kv, err := c.Read(ctx, in.Path)
	h.auditVault("get", in.Path, len(kv), err)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s (QC Vault, %d keys; these are QC secrets: never copy values into code, commits, PRs, gate requests or logs)\n%s",
		in.Path, len(kv), vault.Format(kv)), nil
}

// ReadClaudeArtifactIn is the input of read_claude_artifact.
type ReadClaudeArtifactIn struct {
	URL string `json:"url" jsonschema:"a claude.ai artifact link (https://claude.ai/artifact/...?sk=... or https://claude.ai/code/artifact/...), as found in the ticket or a comment"`
}

// ReadClaudeArtifact downloads a claude.ai artifact next to the task.
func (h *Handlers) ReadClaudeArtifact(ctx context.Context, in ReadClaudeArtifactIn) (string, error) {
	link := strings.TrimSpace(in.URL)
	if _, err := claudeai.Slug(link); err != nil {
		return "", err
	}
	if h.ClaudeArtifact == nil {
		return "", errors.New("claude.ai artifacts are not available in this session")
	}
	a, err := h.ClaudeArtifact(ctx, link, refs.Dir(h.Store.Home, h.TaskID))
	if err != nil {
		return "", err
	}
	return a.Format() + "\nRead the screenshot (what the link shows) and the source files; a design canvas keeps its boards in project/*.dc.html and their layout and notes in project/canvas.json.", nil
}

// QCKafkaTopicsIn is the input of qc_kafka_topics.
type QCKafkaTopicsIn struct {
	Search string `json:"search,omitempty" jsonschema:"only topics whose name contains this text"`
	Topic  string `json:"topic,omitempty" jsonschema:"one topic: show its partitions and offsets instead of a list"`
}

// QCKafkaMessagesIn is the input of qc_kafka_messages.
type QCKafkaMessagesIn struct {
	Topic      string `json:"topic" jsonschema:"topic name (one of the project's qc_kafka_topics)"`
	Filter     string `json:"filter,omitempty" jsonschema:"only messages whose key or value contains this text, e.g. an order code"`
	Limit      int    `json:"limit,omitempty" jsonschema:"how many messages (capped)"`
	Partition  *int   `json:"partition,omitempty" jsonschema:"only this partition"`
	FromOffset *int64 `json:"from_offset,omitempty" jsonschema:"read forward from this offset (use with partition)"`
	Since      string `json:"since,omitempty" jsonschema:"read forward from this time: a duration back from now (30m, 2h, 1d) or 2026-11-12T08:00 local time"`
}

// QCKafkaGroupsIn is the input of qc_kafka_consumer_groups.
type QCKafkaGroupsIn struct {
	Topic string `json:"topic,omitempty" jsonschema:"list the consumer groups of this topic with their lag"`
	Group string `json:"group,omitempty" jsonschema:"one consumer group: offsets and lag per partition (only the project's topics are shown)"`
}

// qcKafka returns a client and cluster when the project may read Kafka.
func (h *Handlers) qcKafka(ctx context.Context) (*kafbat.Client, string, []string, error) {
	t, err := h.Store.LoadTask(h.TaskID)
	if err != nil {
		return nil, "", nil, err
	}
	proj := h.Config.Projects[t.Project]
	if h.Config.Kafka.URL == "" || len(proj.KafkaTopics) == 0 {
		return nil, "", nil, fmt.Errorf("project %s has no QC Kafka topics configured (kafka_topics); ask the operator", proj.Name)
	}
	if h.Kafka == nil {
		return nil, "", nil, kafbat.ErrNoCredentials
	}
	c, cluster, err := h.Kafka(ctx)
	return c, cluster, proj.KafkaTopics, err
}

func outsideTopics(topic string, allowed []string) error {
	return fmt.Errorf("topic %q is outside this project's QC Kafka topics: %s", topic, strings.Join(allowed, ", "))
}

// QCKafkaTopics lists the project's QC topics, or one topic's offsets.
func (h *Handlers) QCKafkaTopics(ctx context.Context, in QCKafkaTopicsIn) (string, error) {
	c, cluster, allowed, err := h.qcKafka(ctx)
	if err != nil {
		return "", err
	}
	if in.Topic != "" {
		if !kafbat.Allowed(in.Topic, allowed) {
			return "", outsideTopics(in.Topic, allowed)
		}
		td, err := c.Topic(ctx, cluster, in.Topic)
		if err != nil {
			return "", err
		}
		return "QC Kafka (" + cluster + ")\n" + kafbat.FormatTopic(td), nil
	}
	ts, err := c.Topics(ctx, cluster, in.Search)
	if err != nil {
		return "", err
	}
	ts = kafbat.FilterTopics(ts, allowed)
	if len(ts) == 0 {
		return fmt.Sprintf("no topic matches (project patterns: %s)", strings.Join(allowed, ", ")), nil
	}
	return fmt.Sprintf("QC Kafka (%s), %d topics this project may read:\n%s", cluster, len(ts), kafbat.FormatTopics(ts)), nil
}

// QCKafkaMessages reads messages of one of the project's QC topics.
func (h *Handlers) QCKafkaMessages(ctx context.Context, in QCKafkaMessagesIn) (string, error) {
	topic := strings.TrimSpace(in.Topic)
	if topic == "" {
		return "", fmt.Errorf("topic is required")
	}
	c, cluster, allowed, err := h.qcKafka(ctx)
	if err != nil {
		return "", err
	}
	if !kafbat.Allowed(topic, allowed) {
		return "", outsideTopics(topic, allowed)
	}
	q := kafbat.MessagesQuery{Filter: in.Filter, Limit: in.Limit}
	if q.Limit <= 0 {
		q.Limit = 20
	}
	if m := h.Config.Kafka.MaxMessages; m > 0 && q.Limit > m {
		q.Limit = m
	}
	if in.Partition != nil {
		q.Partitions = []int{*in.Partition}
	}
	if err := q.Start(in.FromOffset, in.Since, time.Now()); err != nil {
		return "", err
	}
	ms, err := c.Messages(ctx, cluster, topic, q)
	if err != nil {
		return "", err
	}
	head := fmt.Sprintf("QC Kafka %s, topic %s, mode %s: %d messages (#partition@offset; QC data: do not copy personal data into code, commits or PRs)\n",
		cluster, topic, q.Mode, len(ms))
	if len(ms) == 0 {
		return head + "(no message)", nil
	}
	return head + kafbat.FormatMessages(ms, 4000), nil
}

// QCKafkaGroups shows consumer groups of a topic, or one group.
func (h *Handlers) QCKafkaGroups(ctx context.Context, in QCKafkaGroupsIn) (string, error) {
	if (in.Topic == "") == (in.Group == "") {
		return "", fmt.Errorf("give exactly one of topic or group")
	}
	c, cluster, allowed, err := h.qcKafka(ctx)
	if err != nil {
		return "", err
	}
	keep := func(t string) bool { return kafbat.Allowed(t, allowed) }
	if in.Topic != "" {
		if !keep(in.Topic) {
			return "", outsideTopics(in.Topic, allowed)
		}
		gs, err := c.TopicGroups(ctx, cluster, in.Topic)
		if err != nil {
			return "", err
		}
		if len(gs) == 0 {
			return "no consumer group reads " + in.Topic, nil
		}
		return fmt.Sprintf("consumer groups of %s (QC Kafka %s):\n%s", in.Topic, cluster, kafbat.FormatGroups(gs)), nil
	}
	g, err := c.Group(ctx, cluster, in.Group)
	if err != nil {
		return "", err
	}
	mine := false
	for _, p := range g.Partitions {
		if keep(p.Topic) {
			mine = true
			break
		}
	}
	if !mine {
		return "", fmt.Errorf("consumer group %q reads none of this project's QC Kafka topics: %s", in.Group, strings.Join(allowed, ", "))
	}
	return "QC Kafka " + cluster + "\n" + kafbat.FormatGroup(g, keep), nil
}

// QCLogsIn is the input of qc_logs.
type QCLogsIn struct {
	App          string `json:"app" jsonschema:"application name from qc_apps"`
	Filter       string `json:"filter,omitempty" jsonschema:"only lines containing this text, e.g. a transaction code or 'error'"`
	Tail         int    `json:"tail,omitempty" jsonschema:"number of latest lines (capped)"`
	SinceMinutes int    `json:"since_minutes,omitempty" jsonschema:"only lines from the last N minutes (max 1440)"`
}

// projectApps lists the QC applications of the task's project.
func (h *Handlers) projectApps(ctx context.Context) (*argocd.Client, []argocd.App, error) {
	if h.Config.ArgoCD.URL == "" {
		return nil, nil, fmt.Errorf("argo cd is not configured (argocd.url)")
	}
	if h.ArgoCD == nil {
		return nil, nil, argocd.ErrNoCredentials
	}
	t, err := h.Store.LoadTask(h.TaskID)
	if err != nil {
		return nil, nil, err
	}
	p := h.Config.Projects[t.Project]
	cl, err := h.ArgoCD(ctx)
	if err != nil {
		return nil, nil, err
	}
	all, err := cl.Apps(ctx)
	if err != nil {
		return nil, nil, err
	}
	return cl, argocd.Match(all, p.ArgoCD.Apps, p.ArgoCD.Projects, p.RepoNames(), p.ArgoCD.MatchesRepos(), h.Config.AllRepoNames()), nil
}

// QCApps reports the QC deployment state of the project's applications.
func (h *Handlers) QCApps(ctx context.Context) (string, error) {
	_, apps, err := h.projectApps(ctx)
	if err != nil {
		return "", err
	}
	if len(apps) == 0 {
		return "No Argo CD applications are mapped to this project.", nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d QC application(s) (%s):\n", len(apps), h.Config.ArgoCD.URL)
	for _, a := range apps {
		rev := a.Revision
		if len(rev) > 10 {
			rev = rev[:10]
		}
		fmt.Fprintf(&b, "\n- %s: health=%s sync=%s revision=%s target=%s", a.Name, a.Health, a.Sync, rev, a.TargetRevision)
		if a.OperationPhase != "" {
			fmt.Fprintf(&b, " last-op=%s %s", a.OperationPhase, a.FinishedAt)
		}
		if len(a.Images) > 0 {
			fmt.Fprintf(&b, "\n  images: %s", strings.Join(a.Images, ", "))
		}
	}
	return b.String(), nil
}

// QCLogs returns recent pod logs of one of the project's QC applications.
func (h *Handlers) QCLogs(ctx context.Context, in QCLogsIn) (string, error) {
	cl, apps, err := h.projectApps(ctx)
	if err != nil {
		return "", err
	}
	var app *argocd.App
	var names []string
	for i := range apps {
		names = append(names, apps[i].Name)
		if apps[i].Name == in.App {
			app = &apps[i]
		}
	}
	if app == nil {
		return "", fmt.Errorf("application %q is not one of this project's QC apps: %s", in.App, strings.Join(names, ", "))
	}
	tail := in.Tail
	if max := h.Config.ArgoCD.MaxLogLines; tail <= 0 || tail > max {
		tail = max
	}
	since := in.SinceMinutes
	if since > 1440 {
		since = 1440
	}
	lines, err := cl.Logs(ctx, *app, argocd.LogOptions{TailLines: tail, SinceSeconds: since * 60, Filter: in.Filter})
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d line(s) from %s (QC)", len(lines), app.Name)
	if in.Filter != "" {
		fmt.Fprintf(&b, " containing %q", in.Filter)
	}
	b.WriteString(":\n")
	const total = 48 << 10
	for _, l := range lines {
		c := l.Content
		if r := []rune(c); len(r) > 600 {
			c = string(r[:600]) + "…"
		}
		line := fmt.Sprintf("\n%s %s", l.Time, c)
		if b.Len()+len(line) > total {
			b.WriteString("\n… truncated; use filter or a smaller tail")
			break
		}
		b.WriteString(line)
	}
	return b.String(), nil
}

func gitOut(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// reply turns a tool handler's result into the MCP answer.
func reply(msg string, err error) (*mcp.CallToolResult, any, error) {
	if err != nil {
		return nil, nil, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: msg}}}, nil, nil
}

// NewServer builds the MCP server bound to one task.
func NewServer(h *Handlers) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "taskpilot", Version: Version}, nil)
	mcp.AddTool(s, &mcp.Tool{
		Name: "request_approval",
		Description: "Ask the human operator to approve a gate (plan, pr, deploy, ...). " +
			"Tool calls guarded by a gate are blocked until it is approved. After calling this, end your turn immediately.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in RequestApprovalIn) (*mcp.CallToolResult, any, error) {
		return reply(h.RequestApproval(in))
	})
	mcp.AddTool(s, &mcp.Tool{
		Name: "ask_operator",
		Description: "Ask the human operator multiple-choice questions when you need a decision you cannot take yourself " +
			"(ambiguous requirement, trade-off, missing information). Like AskUserQuestion: give 1-4 questions with 2-6 short " +
			"options each, recommended first; the operator picks quickly or writes another answer. After calling this, end your turn.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in AskOperatorIn) (*mcp.CallToolResult, any, error) {
		return reply(h.AskOperator(in))
	})
	mcp.AddTool(s, &mcp.Tool{
		Name: "request_permission",
		Description: "A command you need was denied because it is not in allowed_tools: ask the operator to allow it. " +
			"Give the narrowest rule (Bash(curl *), not Bash(*)), the denied command and why. After calling this, end your turn; " +
			"once allowed, run the command again.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in RequestPermissionIn) (*mcp.CallToolResult, any, error) {
		return reply(h.RequestPermission(in))
	})
	mcp.AddTool(s, &mcp.Tool{
		Name:        "complete_task",
		Description: "Declare the task finished (after the pr gate was approved and the branch was pushed). End your turn afterwards.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in CompleteTaskIn) (*mcp.CallToolResult, any, error) {
		return reply(h.CompleteTask(in))
	})
	mcp.AddTool(s, &mcp.Tool{
		Name: "open_repo",
		Description: "Check out one of the project's repos into the task workspace (a git worktree on the task's work branch, " +
			"from the latest remote base) and report its baseline verify result. Only repos listed in task_info can be opened. " +
			"Read-only and allowed before any gate.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in OpenRepoIn) (*mcp.CallToolResult, any, error) {
		return reply(h.OpenRepo(ctx, in))
	})
	mcp.AddTool(s, &mcp.Tool{
		Name: "set_branch",
		Description: "Name the task's work branch by the team convention (task_info.branch_convention: " +
			"<type>/<english-kebab-description>--<FUNCTION_CODE>), renaming it in every opened repo. Call it once the plan is " +
			"approved and the function code is known, before the first commit (the repos' commit hook reads the code from " +
			"the branch). Renaming with git branch -m works too (create_pull_request follows the branch each repo is on). " +
			"After a PR exists, renaming opens new PRs from the new name; the old ones stay open.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in SetBranchIn) (*mcp.CallToolResult, any, error) {
		return reply(h.SetBranch(ctx, in))
	})
	mcp.AddTool(s, &mcp.Tool{
		Name: "create_pull_request",
		Description: "After the pr gate is approved: push the task's work branch of one repo and open a Bitbucket pull request " +
			"into the repo's target branch (reuses an open PR from the same branch). Call once per changed repo. " +
			"Use this instead of git push.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in CreatePullRequestIn) (*mcp.CallToolResult, any, error) {
		return reply(h.CreatePullRequest(ctx, in))
	})
	mcp.AddTool(s, &mcp.Tool{
		Name: "merge_pull_request",
		Description: "Merge the task's PR of one repo into release/qc (agent_merge_targets) — ONLY when the operator's message or approval " +
			"comment that started this turn tells you to merge it (taskpilot checks what they meant, not the word: \"resolve the " +
			"conflicts of the PR into QC\" is not a merge); otherwise it refuses, and you must not merge any other way. " +
			"When the operator asked only to resolve the conflicts, pass resolve_only: true: nothing is merged. " +
			"When the PR conflicts with the target it returns the files and the exact steps: resolve them on the branch " +
			"<work>--into-<target> made from the target (never merge the target into the work branch), then call it again " +
			"(same resolve_only); it pushes that branch and opens its PR, then — unless resolve_only — merges it and declines the original one.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in MergePullRequestIn) (*mcp.CallToolResult, any, error) {
		return reply(h.MergePullRequest(ctx, in))
	})
	mcp.AddTool(s, &mcp.Tool{
		Name: "splunk_search",
		Description: "Search application logs in Splunk (read-only). Use it to investigate a bug or production issue, and to check " +
			"QC logs after a deploy. Searches are scoped to the task's project, limited in time range and size, and logged for the operator.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in SplunkSearchIn) (*mcp.CallToolResult, any, error) {
		return reply(h.SplunkSearch(ctx, in))
	})
	mcp.AddTool(s, &mcp.Tool{
		Name: "set_execution_profile",
		Description: "Choose the model and effort for the rest of this task, from your assessment of it (see the workflow " +
			"skill, 'Execution profile'), and record that assessment: level (light/standard/hard/hardest) and kind of work, " +
			"which the operator sees on the task. Call it once while planning, before requesting the plan gate, and again only if " +
			"the task turns out much harder or easier. Applies from the next run. The operator sees it and may pin another.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in SetProfileIn) (*mcp.CallToolResult, any, error) {
		return reply(h.SetExecutionProfile(in))
	})
	mcp.AddTool(s, &mcp.Tool{
		Name: "db_query",
		Description: "Read-only SQL on the task project's QC database (MySQL): SELECT, SHOW TABLES, DESCRIBE, EXPLAIN. " +
			"Use it to check data while investigating a bug or verifying a change on QC. Never production. Runs in a read-only " +
			"transaction with row and time limits; every query is logged for the operator. Prefer SHOW TABLES / DESCRIBE first, " +
			"then narrow SELECTs with WHERE and LIMIT.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in DBQueryIn) (*mcp.CallToolResult, any, error) {
		return reply(h.DBQuery(ctx, in))
	})
	mcp.AddTool(s, &mcp.Tool{
		Name: "qc_config_list",
		Description: "Read-only: list the keys of the QC Vault (service configuration and secrets of the QC environment) " +
			"under the project's allowed paths. Empty path lists the allowed roots. Every call is logged for the operator.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in QCConfigIn) (*mcp.CallToolResult, any, error) {
		return reply(h.QCConfigList(ctx, in))
	})
	mcp.AddTool(s, &mcp.Tool{
		Name: "qc_config_get",
		Description: "Read-only: the keys and values of one QC Vault secret (a service's QC configuration) under the " +
			"project's allowed paths. Use it to check how a service is configured on QC while debugging. Values are QC " +
			"secrets: quote only what the investigation needs and never copy them into code, commits, PRs or gate " +
			"requests. Every call is logged for the operator.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in QCConfigIn) (*mcp.CallToolResult, any, error) {
		return reply(h.QCConfigGet(ctx, in))
	})
	mcp.AddTool(s, &mcp.Tool{
		Name: "read_claude_artifact",
		Description: "Read-only: download a claude.ai artifact (a prototype or design linked from the ticket, a comment or a doc) " +
			"with the operator's claude.ai session: its source files and a screenshot are saved next to the task and listed. " +
			"WebFetch cannot open these links (they need a login).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in ReadClaudeArtifactIn) (*mcp.CallToolResult, any, error) {
		return reply(h.ReadClaudeArtifact(ctx, in))
	})
	mcp.AddTool(s, &mcp.Tool{
		Name: "qc_kafka_topics",
		Description: "Read-only: the QC Kafka topics this project may read (task_info.qc_kafka_topics), or with topic, " +
			"one topic's partitions and offsets.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in QCKafkaTopicsIn) (*mcp.CallToolResult, any, error) {
		return reply(h.QCKafkaTopics(ctx, in))
	})
	mcp.AddTool(s, &mcp.Tool{
		Name: "qc_kafka_messages",
		Description: "Read-only: messages of one of the project's QC Kafka topics, newest first by default; or forward " +
			"from an offset / a time. Use filter (an order code, an id) to find the message an event produced. Never " +
			"produces or changes offsets. QC data: quote only what the investigation needs.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in QCKafkaMessagesIn) (*mcp.CallToolResult, any, error) {
		return reply(h.QCKafkaMessages(ctx, in))
	})
	mcp.AddTool(s, &mcp.Tool{
		Name: "qc_kafka_consumer_groups",
		Description: "Read-only: which consumer groups read a project topic and how far behind they are (lag), or one " +
			"group's offsets per partition. Use it to tell whether a consumer on QC is stuck or has not processed a message yet.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in QCKafkaGroupsIn) (*mcp.CallToolResult, any, error) {
		return reply(h.QCKafkaGroups(ctx, in))
	})
	if h.Config.Jenkins.URL != "" {
		mcp.AddTool(s, &mcp.Tool{
			Name: "jenkins_jobs",
			Description: "Read-only: the Jenkins jobs that build the project's repos (by default those matching its repo names), " +
				"with the status of their last build. Start here to find the job of a repo/branch.",
		}, func(ctx context.Context, _ *mcp.CallToolRequest, in JenkinsJobsIn) (*mcp.CallToolResult, any, error) {
			return reply(h.JenkinsJobs(ctx, in))
		})
		mcp.AddTool(s, &mcp.Tool{
			Name: "jenkins_builds",
			Description: "Read-only: the latest builds of a Jenkins job — result, time, duration, commit and branch built, cause, " +
				"parameters and the commits each build picked up. Use it after a push or a merge into release/qc to see whether the build passed.",
		}, func(ctx context.Context, _ *mcp.CallToolRequest, in JenkinsBuildsIn) (*mcp.CallToolResult, any, error) {
			return reply(h.JenkinsBuilds(ctx, in))
		})
		mcp.AddTool(s, &mcp.Tool{
			Name: "jenkins_log",
			Description: "Read-only: the console log of a Jenkins build (default the last one): its tail, or the lines matching a filter " +
				"with context. Use it to find why a build failed. Quote only what proves the point.",
		}, func(ctx context.Context, _ *mcp.CallToolRequest, in JenkinsLogIn) (*mcp.CallToolResult, any, error) {
			return reply(h.JenkinsLog(ctx, in))
		})
	}
	mcp.AddTool(s, &mcp.Tool{
		Name: "qc_apps",
		Description: "Read-only: the QC deployment state (Argo CD) of the project's applications — health, sync status, deployed " +
			"revision and images. Use it after a QC PR is merged to check the change is actually deployed.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ TaskInfoIn) (*mcp.CallToolResult, any, error) {
		return reply(h.QCApps(ctx))
	})
	mcp.AddTool(s, &mcp.Tool{
		Name: "qc_logs",
		Description: "Read-only: recent pod logs of one of the project's QC applications (Argo CD), optionally filtered. " +
			"Use it to verify a change on QC or to investigate a QC bug report.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in QCLogsIn) (*mcp.CallToolResult, any, error) {
		return reply(h.QCLogs(ctx, in))
	})
	if h.Config.Graphify != nil {
		mcp.AddTool(s, &mcp.Tool{
			Name: "graphify",
			Description: "Read-only graphify graphs of the project's repos, joined across repos by the gRPC calls between " +
				"them. line: task (your branch: the repos you opened with your changes, plus the others from prod; the " +
				"default once a repo is open), prod (release/prod), qc (release/qc). Use it before grepping: which repo " +
				"serves a gRPC method and who calls it (links), what calls or depends on a function across repos " +
				"(affected), how two functions connect (path), a node's neighbours (explain), where a concept lives " +
				"(query), code of a line's branch (read). Answers print paths as repo/path:line.",
		}, func(ctx context.Context, _ *mcp.CallToolRequest, in GraphifyIn) (*mcp.CallToolResult, any, error) {
			return reply(h.Graphify(ctx, in))
		})
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "task_info",
		Description: "Return the ticket, the project's repos (opened or not, verify commands, baselines), the workspace and the approval status of every gate.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ TaskInfoIn) (*mcp.CallToolResult, any, error) {
		return reply(h.TaskInfo())
	})
	return s
}

// graphifyView tells the agent which code lines it can query.
func (h *Handlers) graphifyView(_, project string) any {
	if h.Config.Graphify == nil {
		return nil
	}
	return map[string]any{
		"lines": h.Config.ProjectGraphLines(project),
		"state": graphify.Summary(h.Config, project),
		"note":  "Use the graphify tool: links (gRPC between repos), affected / path / explain / query, read (code of a line's branch).",
	}
}
