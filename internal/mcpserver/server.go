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
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/buonnguwaaa/taskpilot/internal/argocd"
	"github.com/buonnguwaaa/taskpilot/internal/bitbucket"
	"github.com/buonnguwaaa/taskpilot/internal/config"
	"github.com/buonnguwaaa/taskpilot/internal/qcdb"
	"github.com/buonnguwaaa/taskpilot/internal/splunk"
	"github.com/buonnguwaaa/taskpilot/internal/store"
	"github.com/buonnguwaaa/taskpilot/internal/workspace"
)

// Version is reported to MCP clients.
const Version = "0.1.0"

// RequestApprovalIn is the input of request_approval.
type RequestApprovalIn struct {
	Gate    string `json:"gate" jsonschema:"the gate to open, one of the configured gates (e.g. plan, pr, deploy)"`
	Summary string `json:"summary" jsonschema:"one or two sentences the operator reads in the approval inbox, in the operator's language"`
	Details string `json:"details,omitempty" jsonschema:"full material for the decision: the plan, diff summary, test results, risks"`
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
	// Secret reads a value from the operator's secret store (a QC database
	// DSN not kept in a repo file); the value never reaches the agent.
	Secret func(ctx context.Context, name string) (string, error)
}

// RequestApproval records an approval request for the operator.
func (h *Handlers) RequestApproval(in RequestApprovalIn) (string, error) {
	gate := strings.TrimSpace(in.Gate)
	if !h.Config.HasGate(gate) {
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
	r, err := h.Store.AddRequest(h.TaskID, gate, in.Summary, in.Details)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Approval request %s recorded for gate %q. Do not do any more work now: end your turn with a one-line status. "+
		"The session will be resumed with the operator's decision.", r.ID, gate), nil
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
		"task_id":       t.ID,
		"ticket_id":     t.TicketID,
		"ticket_url":    t.TicketURL,
		"title":         t.Title,
		"body":          t.Body,
		"project":       proj.Name,
		"workspace":     t.Workspace,
		"work_branch":   t.Branch,
		"repos":         repos,
		"context_files": proj.ContextFiles,
		"allowed_tools": proj.AllowedToolList(),
		"tool_rules": "Only allowed_tools run; anything else is denied without asking. A denial applies to that one " +
			"command, not to Bash as a whole: keep using the allowed commands. Change files with Edit/Write, never " +
			"with shell redirection (cat > file, sed -i, tee). Writes are only allowed inside the workspace.",
		"gates":             gates,
		"approval_history":  history,
		"operator_language": h.Config.OperatorLanguage,
		"execution_profile": h.profileView(),
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
	o, err := workspace.OpenRepo(ctx, t.Workspace, t.ID, in.Name, repo)
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
	stage, err := h.pickStage(in.Repo, repo, in.Target)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(t.Workspace, in.Repo)
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		return "", fmt.Errorf("repo %s is not opened in the workspace", in.Repo)
	}
	if out, err := gitOut(ctx, dir, "status", "--porcelain"); err != nil {
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
		return "", fmt.Errorf("repo %s has no commits on %s beyond %s; nothing to open a PR for", in.Repo, t.Branch, base)
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

	pctx, cancel := context.WithTimeout(ctx, PushTimeout)
	defer cancel()
	push := exec.CommandContext(pctx, "git", "-C", dir, "push", "--porcelain", "-u", repo.Remote, "HEAD:refs/heads/"+t.Branch)
	push.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_SSH_COMMAND=ssh -o BatchMode=yes")
	if out, err := push.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git push %s failed: %v: %s", t.Branch, err, strings.TrimSpace(string(out)))
	}

	desc := strings.TrimSpace(in.Description) + fmt.Sprintf("\n\n---\nTicket: %s %s\nOpened by taskpilot (task `%s`) after operator approval.", t.TicketID, t.Title, t.ID)
	if bb == nil {
		link := bitbucket.NewPRURL(ref, t.Branch, stage.Target)
		rec := &store.PullRequest{Repo: in.Repo, URL: link, Source: t.Branch, Destination: stage.Target,
			Title: in.Title, Description: desc, Pending: true, CreatedAt: time.Now().UTC()}
		if err := h.Store.SetPullRequest(h.TaskID, rec); err != nil {
			return "", err
		}
		return fmt.Sprintf("Pushed %s to %s. Bitbucket API credentials are not configured, so the PR is not created "+
			"automatically: the operator opens it from the taskpilot TUI (key p) with your title and description pre-copied. "+
			"Do not retry and do not work around this; mention it in complete_task. Create page: %s", t.Branch, ref, link), nil
	}

	pr, err := bb.FindOpenPR(ctx, ref, t.Branch, stage.Target)
	if err != nil {
		return "", err
	}
	reused := pr != nil
	if pr == nil {
		pr, err = bb.CreatePR(ctx, ref, bitbucket.CreateInput{
			Title: in.Title, Description: desc, Source: t.Branch, Destination: stage.Target,
		})
		if err != nil {
			return "", err
		}
	}
	rec := &store.PullRequest{Repo: in.Repo, ID: pr.ID, URL: pr.URL(), Source: t.Branch,
		Destination: stage.Target, Title: pr.Title, CreatedAt: time.Now().UTC()}
	if err := h.Store.SetPullRequest(h.TaskID, rec); err != nil {
		return "", err
	}
	verb := "Opened"
	if reused {
		verb = "Pushed new commits; the existing"
	}
	msg := fmt.Sprintf("%s pull request #%d %s → %s: %s", verb, pr.ID, t.Branch, stage.Target, pr.URL())
	if next := nextStage(repo, stage); next != nil {
		msg += fmt.Sprintf("\nNext stage of this repo: PR into %s after the %q gate is approved.", next.Target, next.Gate)
	}
	return msg, nil
}

// pickStage chooses the pr_flow stage: the requested target, or else the first
// stage whose gate is approved and that has no PR yet (falling back to the
// last approved stage, which just pushes new commits to its PR).
func (h *Handlers) pickStage(repoName string, repo config.Repo, target string) (config.PRStage, error) {
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
		if existing == nil {
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
	Earliest  string `json:"earliest,omitempty" jsonschema:"relative start like -15m, -4h, -2d (default -1h) or an ISO time 2026-11-09T10:00:00"`
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
	if d, ok := splunk.Lookback(earliest); ok {
		if d > sc.MaxLookback {
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
		if err != nil || time.Since(t) > sc.MaxLookback {
			return "", fmt.Errorf("earliest %s is invalid or older than the allowed %s", earliest, sc.MaxLookback)
		}
	}
	max := in.MaxEvents
	if max <= 0 || max > sc.MaxEvents {
		max = sc.MaxEvents
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

func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

// NewServer builds the MCP server bound to one task.
func NewServer(h *Handlers) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "taskpilot", Version: Version}, nil)
	mcp.AddTool(s, &mcp.Tool{
		Name: "request_approval",
		Description: "Ask the human operator to approve a gate (plan, pr, deploy, ...). " +
			"Tool calls guarded by a gate are blocked until it is approved. After calling this, end your turn immediately.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in RequestApprovalIn) (*mcp.CallToolResult, any, error) {
		msg, err := h.RequestApproval(in)
		if err != nil {
			return nil, nil, err
		}
		return text(msg), nil, nil
	})
	mcp.AddTool(s, &mcp.Tool{
		Name:        "complete_task",
		Description: "Declare the task finished (after the pr gate was approved and the branch was pushed). End your turn afterwards.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in CompleteTaskIn) (*mcp.CallToolResult, any, error) {
		msg, err := h.CompleteTask(in)
		if err != nil {
			return nil, nil, err
		}
		return text(msg), nil, nil
	})
	mcp.AddTool(s, &mcp.Tool{
		Name: "open_repo",
		Description: "Check out one of the project's repos into the task workspace (a git worktree on the task's work branch, " +
			"from the latest remote base) and report its baseline verify result. Only repos listed in task_info can be opened. " +
			"Read-only and allowed before any gate.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in OpenRepoIn) (*mcp.CallToolResult, any, error) {
		msg, err := h.OpenRepo(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		return text(msg), nil, nil
	})
	mcp.AddTool(s, &mcp.Tool{
		Name: "create_pull_request",
		Description: "After the pr gate is approved: push the task's work branch of one repo and open a Bitbucket pull request " +
			"into the repo's target branch (reuses an open PR from the same branch). Call once per changed repo. " +
			"Use this instead of git push.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in CreatePullRequestIn) (*mcp.CallToolResult, any, error) {
		msg, err := h.CreatePullRequest(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		return text(msg), nil, nil
	})
	mcp.AddTool(s, &mcp.Tool{
		Name: "splunk_search",
		Description: "Search application logs in Splunk (read-only). Use it to investigate a bug or production issue, and to check " +
			"QC logs after a deploy. Searches are scoped to the task's project, limited in time range and size, and logged for the operator.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in SplunkSearchIn) (*mcp.CallToolResult, any, error) {
		msg, err := h.SplunkSearch(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		return text(msg), nil, nil
	})
	mcp.AddTool(s, &mcp.Tool{
		Name: "set_execution_profile",
		Description: "Choose the model and effort for the rest of this task, from your assessment of it (see the workflow " +
			"skill, 'Execution profile'). Call it once while planning, before requesting the plan gate, and again only if " +
			"the task turns out much harder or easier. Applies from the next run. The operator sees it and may pin another.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in SetProfileIn) (*mcp.CallToolResult, any, error) {
		msg, err := h.SetExecutionProfile(in)
		if err != nil {
			return nil, nil, err
		}
		return text(msg), nil, nil
	})
	mcp.AddTool(s, &mcp.Tool{
		Name: "db_query",
		Description: "Read-only SQL on the task project's QC database (MySQL): SELECT, SHOW TABLES, DESCRIBE, EXPLAIN. " +
			"Use it to check data while investigating a bug or verifying a change on QC. Never production. Runs in a read-only " +
			"transaction with row and time limits; every query is logged for the operator. Prefer SHOW TABLES / DESCRIBE first, " +
			"then narrow SELECTs with WHERE and LIMIT.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in DBQueryIn) (*mcp.CallToolResult, any, error) {
		msg, err := h.DBQuery(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		return text(msg), nil, nil
	})
	mcp.AddTool(s, &mcp.Tool{
		Name: "qc_apps",
		Description: "Read-only: the QC deployment state (Argo CD) of the project's applications — health, sync status, deployed " +
			"revision and images. Use it after a QC PR is merged to check the change is actually deployed.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ TaskInfoIn) (*mcp.CallToolResult, any, error) {
		msg, err := h.QCApps(ctx)
		if err != nil {
			return nil, nil, err
		}
		return text(msg), nil, nil
	})
	mcp.AddTool(s, &mcp.Tool{
		Name: "qc_logs",
		Description: "Read-only: recent pod logs of one of the project's QC applications (Argo CD), optionally filtered. " +
			"Use it to verify a change on QC or to investigate a QC bug report.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in QCLogsIn) (*mcp.CallToolResult, any, error) {
		msg, err := h.QCLogs(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		return text(msg), nil, nil
	})
	mcp.AddTool(s, &mcp.Tool{
		Name:        "task_info",
		Description: "Return the ticket, the project's repos (opened or not, verify commands, baselines), the workspace and the approval status of every gate.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ TaskInfoIn) (*mcp.CallToolResult, any, error) {
		msg, err := h.TaskInfo()
		if err != nil {
			return nil, nil, err
		}
		return text(msg), nil, nil
	})
	return s
}
