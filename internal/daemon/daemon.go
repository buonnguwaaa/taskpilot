// Package daemon is the harness loop: it pulls tickets, prepares worktrees,
// runs one Claude Code session per task, parks tasks at approval gates and
// resumes them when the operator decides. It contains no model logic: the
// order of work lives in the workflow skill, the gates live in the hook.
package daemon

import (
	"context"
	"crypto/rand"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/buonnguwaaa/taskpilot/internal/config"
	"github.com/buonnguwaaa/taskpilot/internal/runner"
	"github.com/buonnguwaaa/taskpilot/internal/source"
	"github.com/buonnguwaaa/taskpilot/internal/store"
	"github.com/buonnguwaaa/taskpilot/internal/workspace"
)

// Daemon schedules task runs.
type Daemon struct {
	// Config is the config at start; the file is watched and reloaded when it
	// changes (conf returns the current one).
	Config *config.Config
	Store  *store.Store
	Source source.Source
	Runner *runner.Runner
	Log    *log.Logger
	// Scanner, when set, runs the scheduled production error scans.
	Scanner interface{ Tick(ctx context.Context) }
	// Indexer, when set, keeps the graphify graphs up to date.
	Indexer interface{ Tick(ctx context.Context) }

	cur      atomic.Pointer[config.Config]
	scanning atomic.Bool
	indexing atomic.Bool
	watch    *config.Watcher
	mu       sync.Mutex
	running  map[string]bool
	// cancels stop a task's run (the operator took back what started it).
	cancels map[string]context.CancelFunc
	wg      sync.WaitGroup
}

// CurrentConfig returns the config in use (reloaded when the file changes).
func (d *Daemon) CurrentConfig() *config.Config { return d.conf() }

// conf returns the current config.
func (d *Daemon) conf() *config.Config {
	if c := d.cur.Load(); c != nil {
		return c
	}
	return d.Config
}

// reloadConfig picks up changes of the config file (a repo or project
// added, a rule changed). Runs in flight keep the config they started with.
func (d *Daemon) reloadConfig() {
	if d.watch == nil {
		if d.Config == nil || d.Config.Path == "" {
			return
		}
		d.watch = config.NewWatcher(d.Config)
		if d.Runner != nil && d.Runner.Conf == nil {
			d.Runner.Conf = d.conf
		}
	}
	c, err := d.watch.Check()
	switch {
	case err != nil:
		d.Log.Printf("config changed but does not load, keeping the previous one: %v", err)
	case c != nil && len(c.Projects) == 0:
		d.Log.Printf("config changed but has no projects, keeping the previous one")
	case c != nil:
		d.cur.Store(c)
		d.Log.Printf("config reloaded from %s (%d projects)", c.Path, len(c.Projects))
	}
}

// Run loops until ctx is cancelled, then waits for in-flight runs to stop.
func (d *Daemon) Run(ctx context.Context) error {
	d.running = map[string]bool{}
	if err := d.recover(); err != nil {
		return err
	}
	d.Log.Printf("taskpilot daemon started (home %s, source %s, concurrency %d)", d.Store.Home, d.Source.Name(), d.conf().Daemon.Concurrency)
	for {
		d.Tick(ctx)
		select {
		case <-ctx.Done():
			d.Log.Printf("stopping; waiting for %d running task(s)", d.inFlight())
			d.wg.Wait()
			return nil
		case <-time.After(d.conf().Daemon.PollInterval):
		}
	}
}

// Once runs a single scheduling pass and waits for the runs it started.
func (d *Daemon) Once(ctx context.Context) error {
	d.running = map[string]bool{}
	if err := d.recover(); err != nil {
		return err
	}
	d.Tick(ctx)
	d.wg.Wait()
	return nil
}

// Wait blocks until all runs started by Tick have finished (used by tests).
func (d *Daemon) Wait() { d.wg.Wait() }

// recover parks tasks a previous daemon left mid-run, and frees the clean
// worktrees of finished tasks (also those finished before this was automatic).
func (d *Daemon) recover() error {
	tasks, err := d.Store.ListTasks()
	if err != nil {
		return err
	}
	for _, t := range tasks {
		if t.State == store.StateDone && len(workspace.OpenedRepos(t.Workspace)) > 0 {
			d.releaseWorktrees(t)
		}
		if t.State != store.StateRunning && t.State != store.StatePreparing {
			continue
		}
		// The agent may have reached a gate (or finished) just before the stop:
		// park it there so the operator's decision still resumes it.
		pending, err := d.Store.PendingRequest(t.ID)
		if err != nil {
			return err
		}
		completion, err := d.Store.Completion(t.ID)
		if err != nil {
			return err
		}
		switch {
		case pending != nil:
			t.Transition(store.StateAwaitApproval, fmt.Sprintf("gate %s: %s (daemon restarted)", pending.Gate, pending.Summary))
		case completion != nil:
			t.Transition(store.StateDone, completion.Summary)
		default:
			t.Transition(store.StateNeedsAttention, "daemon restarted while the task was running; send a message to resume")
		}
		if err := d.Store.SaveTask(t); err != nil {
			return err
		}
		if t.State == store.StateDone {
			d.releaseWorktrees(t)
		}
	}
	return nil
}

// Tick runs one scheduling pass. Exported for tests.
func (d *Daemon) Tick(ctx context.Context) {
	if d.running == nil {
		d.running = map[string]bool{}
	}
	d.reloadConfig()
	d.scan(ctx)
	d.index(ctx)
	d.ingest(ctx)
	d.flushOutbox(ctx)
	tasks, err := d.Store.ListTasks()
	if err != nil {
		d.Log.Printf("list tasks: %v", err)
		return
	}
	// Oldest first so earlier tickets get slots first.
	for i := len(tasks) - 1; i >= 0; i-- {
		t := tasks[i]
		if d.Store.TakenOver(t.ID) {
			continue
		}
		// The operator takes back the message or decision that started the
		// latest run: stop it first, then rewind (before any new input).
		if d.Store.UndoRequested(t.ID) {
			if d.isRunning(t.ID) {
				d.stopRun(t.ID)
			} else if err := d.undo(t); err != nil {
				d.Log.Printf("%s: take back: %v", t.ID, err)
				if cur, lerr := d.Store.LoadTask(t.ID); lerr == nil {
					cur.LastError = "không thu hồi được: " + err.Error()
					_ = d.Store.SaveTask(cur)
				}
			}
			continue
		}
		if d.isRunning(t.ID) {
			continue
		}
		if err := d.advance(t); err != nil {
			d.Log.Printf("%s: %v", t.ID, err)
			continue
		}
		if t.State == store.StateQueued && d.inFlight() < d.conf().Daemon.Concurrency && ctx.Err() == nil {
			d.start(ctx, t)
		}
	}
}

// ingest creates tasks for new tickets.
func (d *Daemon) ingest(ctx context.Context) {
	tickets, err := d.Source.Fetch(ctx)
	if err != nil {
		d.Log.Printf("fetch tickets: %v", err)
		return
	}
	for _, tk := range tickets {
		src := d.Source.Name()
		if tk.Source != "" {
			src = tk.Source
		}
		id := store.TaskID(src, tk.ID)
		if !store.ValidID(id) || d.Store.Exists(id) {
			continue
		}
		now := time.Now().UTC()
		t := &store.Task{
			ID: id, Source: src, TicketID: tk.ID, TicketURL: tk.URL,
			Title: tk.Title, Body: tk.Body, Project: tk.Project, RepoHint: tk.Repo,
			SessionID: newUUID(), CreatedAt: now,
		}
		if proj, ok := d.conf().Projects[tk.Project]; !ok {
			t.LastError = fmt.Sprintf("unknown project %q (configured: %s)", tk.Project, strings.Join(d.conf().ProjectNames(), ", "))
			t.Transition(store.StateFailed, t.LastError)
		} else if _, ok := proj.Repos[tk.Repo]; tk.Repo != "" && !ok {
			t.LastError = fmt.Sprintf("repo %q is not part of project %s (repos: %s)", tk.Repo, tk.Project, strings.Join(proj.RepoNames(), ", "))
			t.Transition(store.StateFailed, t.LastError)
		} else {
			t.Transition(store.StateQueued, "picked up")
		}
		if err := d.Store.SaveTask(t); err != nil {
			d.Log.Printf("save new task %s: %v", id, err)
			continue
		}
		d.report(ctx, t, fmt.Sprintf("taskpilot: picked up as %s (%s)", id, t.State))
		d.Log.Printf("%s: new task from ticket %s (%s)", id, tk.ID, t.State)
	}
}

// advance applies operator input that arrived while the task was parked.
func (d *Daemon) advance(t *store.Task) error {
	switch t.State {
	case store.StateAwaitApproval:
		pending, err := d.Store.PendingRequest(t.ID)
		if err != nil || pending != nil {
			return err
		}
		req, dec, err := d.lastDecision(t.ID)
		if err != nil || req == nil {
			return err
		}
		// The operator may have closed the task (marked it done) since it was
		// listed; closing decides the pending request, which must not resume it.
		if cur, err := d.Store.LoadTask(t.ID); err != nil || cur.State != store.StateAwaitApproval {
			return err
		}
		verdict := "APPROVED"
		next := "Continue the workflow from the step after this gate."
		if !dec.Approved {
			verdict = "REJECTED"
			next = fmt.Sprintf("Address the comment, then request the %q gate again. Do not proceed past this gate.", req.Gate)
		}
		comment := dec.Comment
		if comment == "" {
			comment = "(none)"
		}
		t.NextPrompt = fmt.Sprintf("[taskpilot] The operator %s request %s (gate %q). Operator comment: %s\n%s",
			verdict, req.ID, req.Gate, comment, next)
		if req.Gate == store.QuestionGate { // ask_operator: an answer, not a gate decision
			if dec.Approved {
				t.NextPrompt = fmt.Sprintf("[taskpilot] The operator answered your question(s) (request %s):\n%s\n"+
					"Continue from where you asked, following these answers. Do not ask the same question again.", req.ID, comment)
			} else {
				t.NextPrompt = fmt.Sprintf("[taskpilot] The operator did not pick an answer to request %s and replied: %s\n"+
					"Take this into account and continue; ask again (with better options) only if you really cannot proceed.", req.ID, comment)
			}
		}
		t.NextTrigger = map[bool]string{true: "approval", false: "rejection"}[dec.Approved]
		t.Transition(store.StateQueued, fmt.Sprintf("%s %s", strings.ToLower(verdict), req.ID))
		return d.Store.SaveTask(t)
	case store.StateNeedsAttention, store.StateFailed, store.StateDone:
		n, err := d.Store.TakeNudge(t.ID)
		if err != nil || n == nil {
			return err
		}
		if _, ok := d.conf().Projects[t.Project]; !ok {
			return nil // nothing to resume into
		}
		if err := d.Store.ArchiveCompletion(t.ID); err != nil {
			return err
		}
		msg := "[taskpilot] Message from the operator: " + n.Message
		if t.NextPrompt != "" { // not sent yet (e.g. the worktrees could not be re-attached): keep it
			msg = t.NextPrompt + "\n\n" + msg
		}
		t.NextPrompt = msg
		t.NextTrigger = "operator_message"
		t.Transition(store.StateQueued, "resumed by operator message")
		return d.Store.SaveTask(t)
	}
	return nil
}

func (d *Daemon) lastDecision(id string) (*store.ApprovalRequest, *store.Decision, error) {
	reqs, err := d.Store.Requests(id)
	if err != nil {
		return nil, nil, err
	}
	decs, err := d.Store.Decisions(id)
	if err != nil {
		return nil, nil, err
	}
	for i := len(reqs) - 1; i >= 0; i-- {
		if dec, ok := decs[reqs[i].ID]; ok {
			return reqs[i], dec, nil
		}
	}
	return nil, nil, nil
}

// stopRun cancels the task's run in progress.
func (d *Daemon) stopRun(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if c := d.cancels[id]; c != nil {
		c()
	}
}

func (d *Daemon) start(ctx context.Context, t *store.Task) {
	ctx, cancel := context.WithCancel(ctx)
	d.mu.Lock()
	d.running[t.ID] = true
	if d.cancels == nil {
		d.cancels = map[string]context.CancelFunc{}
	}
	d.cancels[t.ID] = cancel
	d.mu.Unlock()
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		defer func() {
			d.mu.Lock()
			delete(d.running, t.ID)
			delete(d.cancels, t.ID)
			d.mu.Unlock()
			cancel()
		}()
		if err := d.work(ctx, t); err != nil {
			d.Log.Printf("%s: %v", t.ID, err)
		}
	}()
}

// work runs one turn of a task and parks it in the resulting state.
func (d *Daemon) work(ctx context.Context, t *store.Task) error {
	proj := d.conf().Projects[t.Project]
	if t.Workspace == "" || !exists(t.Workspace) {
		t.Transition(store.StatePreparing, "creating workspace")
		if err := d.Store.SaveTask(t); err != nil {
			return err
		}
		ws, err := workspace.Create(d.Store.Home, t.ID)
		if err != nil {
			t.LastError = err.Error()
			t.Transition(store.StateFailed, "workspace: "+err.Error())
			return d.Store.SaveTask(t)
		}
		t.Workspace, t.Branch = ws, workspace.Branch(t.ID)
		// A new task starts from the latest branches: bring the graphify
		// graphs (release/prod, release/qc) up to date before its first run.
		if r, ok := d.Indexer.(interface {
			Refresh(ctx context.Context, project string) []string
		}); ok && d.Indexer != nil {
			for _, n := range r.Refresh(ctx, t.Project) {
				t.History = append(t.History, store.Event{At: time.Now().UTC(), State: store.StatePreparing, Note: n})
			}
		}
		if t.RepoHint != "" {
			if err := d.openWithBaseline(ctx, t, proj, t.RepoHint); err != nil {
				t.LastError = err.Error()
				t.Transition(store.StateFailed, "open "+t.RepoHint+": "+err.Error())
				return d.Store.SaveTask(t)
			}
		}
	} else if err := d.attachWorktrees(ctx, t, proj); err != nil {
		// The prompt stays in NextPrompt: the next message is appended to it.
		t.LastError = err.Error()
		t.Transition(store.StateNeedsAttention, "không gắn lại được worktree:\n"+err.Error())
		return d.Store.SaveTask(t)
	}

	resume := t.Runs > 0
	prompt, trigger := t.NextPrompt, t.NextTrigger
	if prompt == "" {
		prompt, trigger = initialPrompt(t, proj, graphOn(d.conf(), t.Project)), "initial"
	}
	if trigger == "" {
		trigger = "operator_message"
	}
	t.Runs++
	t.LastError = ""
	t.Transition(store.StateRunning, fmt.Sprintf("run %d", t.Runs))
	if err := d.Store.SaveTask(t); err != nil {
		return err
	}
	kind := d.Runner.KindFor(t)
	model, effort := d.Runner.Profile(t)
	// The session entry this run continues from, so it can be taken back:
	// after a take-back the entry before it, else the session's last one.
	resumeAt, from := "", ""
	if resume && kind != config.AgentCursor {
		resumeAt = d.Store.ResumeAt(t.ID)
		if from = resumeAt; from == "" {
			from = lastEntry(d.Store.EventsPath(t.ID))
		}
	}
	runID, err := d.Store.StartRun(t.ID, store.RunStart{N: t.Runs, SessionID: t.SessionID, Resume: resume, Trigger: trigger,
		Prompt: prompt, Model: model, Effort: effort, ResumeAt: from})
	if err != nil {
		return err
	}
	d.Log.Printf("%s: run %d (%s, agent %s, model %s, effort %s)", t.ID, t.Runs, trigger, kind, orDefault(model), orDefault(effort))

	res, runErr := d.Runner.Run(ctx, t, prompt, resume)
	if resumeAt != "" { // the session now continues from there
		_ = d.Store.SetResumeAt(t.ID, "")
	}
	applyRunResult(t, res)

	pending, err := d.Store.PendingRequest(t.ID)
	if err != nil {
		return err
	}
	completion, err := d.Store.Completion(t.ID)
	if err != nil {
		return err
	}

	if pending == nil && completion == nil && ctx.Err() == nil && d.shouldFallbackToCursor(kind, res, runErr) {
		reason := quotaReason(res, runErr)
		d.Log.Printf("%s: Claude quota (%s); falling back to Cursor", t.ID, reason)
		// "interrupted": this Claude attempt is abandoned so we can retry on Cursor.
		// (runs.outcome CHECK does not allow a dedicated quota_fallback value.)
		end := store.RunEnd{Outcome: "interrupted", Error: "quota_fallback: " + reason, IsError: true}
		if res != nil {
			end.Subtype, end.NumTurns, end.CostUSD = res.Subtype, res.NumTurns, res.CostUSD
			end.InputTokens, end.OutputTokens = res.InputTokens, res.OutputTokens
			end.CacheReadTokens, end.CacheWriteTokens = res.CacheReadTokens, res.CacheWriteTokens
		}
		if err := d.Store.FinishRun(runID, end, toolCallsOf(res)); err != nil {
			return err
		}
		resets := time.Time{}
		if res != nil {
			resets = res.Quota.Resets
		}
		if resets.IsZero() {
			resets = time.Now().UTC().Add(runner.DefaultQuotaLatch)
		}
		_ = d.Store.SetClaudeQuotaLatch(resets, reason)
		_ = d.Store.SetTaskWorker(t.ID, config.AgentCursor, reason)
		d.report(ctx, t, "taskpilot: Claude hết quota — chuyển sang Cursor Agent: "+reason)

		// Cursor cannot resume a Claude session: start a fresh chat with the
		// same operator prompt plus a short note about the switch.
		t.SessionID = ""
		prompt = cursorFallbackPrompt(reason, prompt)
		// runs.trigger CHECK: initial|approval|rejection|operator_message
		trigger = "operator_message"
		resume = false
		t.Runs++
		t.LastError = ""
		t.Transition(store.StateRunning, fmt.Sprintf("run %d (cursor fallback)", t.Runs))
		if err := d.Store.SaveTask(t); err != nil {
			return err
		}
		model, effort = d.Runner.Profile(t)
		runID, err = d.Store.StartRun(t.ID, store.RunStart{N: t.Runs, SessionID: t.SessionID, Resume: false, Trigger: trigger,
			Prompt: prompt, Model: model, Effort: effort})
		if err != nil {
			return err
		}
		d.Log.Printf("%s: run %d (cursor fallback after Claude quota, model %s)", t.ID, t.Runs, orDefault(model))
		res, runErr = d.Runner.RunKind(ctx, t, prompt, false, config.AgentCursor)
		applyRunResult(t, res)
		if pending, err = d.Store.PendingRequest(t.ID); err != nil {
			return err
		}
		if completion, err = d.Store.Completion(t.ID); err != nil {
			return err
		}
	}

	t.NextPrompt, t.NextTrigger = "", ""
	switch {
	case pending != nil:
		t.Transition(store.StateAwaitApproval, fmt.Sprintf("gate %s: %s", pending.Gate, pending.Summary))
		d.report(ctx, t, fmt.Sprintf("taskpilot: waiting for approval of %s: %s", pending.ID, pending.Summary))
	case completion != nil:
		t.Transition(store.StateDone, completion.Summary)
		msg := "taskpilot: done: " + completion.Summary
		if prs, err := d.Store.PullRequests(t.ID); err == nil && len(prs) > 0 {
			for _, pr := range prs {
				msg += fmt.Sprintf(" | PR %s → %s: %s", pr.Repo, pr.Destination, pr.URL)
			}
		} else if completion.PRURL != "" {
			msg += " PR: " + completion.PRURL
		}
		d.report(ctx, t, msg)
		d.releaseWorktrees(t)
	case ctx.Err() != nil:
		t.Transition(store.StateNeedsAttention, "interrupted: taskpilot stopped during the run; send a message (m) to resume")
	case runErr != nil || (res != nil && res.IsError):
		reason := "run failed"
		if runErr != nil {
			reason = runErr.Error()
		} else if res.Subtype != "" {
			reason = "run ended with " + res.Subtype
		}
		if res != nil && res.Text != "" && reason == "run ended with "+res.Subtype {
			reason = res.Text
		}
		t.LastError = reason
		t.Transition(store.StateNeedsAttention, reason)
	default:
		t.Transition(store.StateNeedsAttention, "agent ended its turn without requesting approval or completing")
	}
	d.Log.Printf("%s: -> %s", t.ID, t.State)
	// The agent may have renamed the work branch (set_branch, saved by the MCP
	// process) during the run: keep it rather than this run's stale copy.
	if cur, err := d.Store.LoadTask(t.ID); err == nil {
		t.Branch = cur.Branch
	}
	if err := d.Store.SaveTask(t); err != nil {
		return err
	}
	end := store.RunEnd{Outcome: string(t.State)}
	if ctx.Err() != nil && pending == nil && completion == nil {
		end.Outcome = "interrupted"
	}
	if runErr != nil {
		end.Error = runErr.Error()
	}
	if res != nil {
		end.Subtype, end.IsError, end.NumTurns, end.CostUSD = res.Subtype, res.IsError, res.NumTurns, res.CostUSD
		end.InputTokens, end.OutputTokens = res.InputTokens, res.OutputTokens
		end.CacheReadTokens, end.CacheWriteTokens = res.CacheReadTokens, res.CacheWriteTokens
	}
	return d.Store.FinishRun(runID, end, toolCallsOf(res))
}

func applyRunResult(t *store.Task, res *runner.Result) {
	if res == nil {
		return
	}
	// Cursor assigns the chat id on the first run; Claude uses the
	// predetermined --session-id. Keep whichever the agent reported.
	if res.SessionID != "" {
		t.SessionID = res.SessionID
	}
	t.CostUSD += res.CostUSD
	if res.Text != "" {
		t.LastResult = res.Text
	}
}

func toolCallsOf(res *runner.Result) []store.ToolCall {
	if res == nil {
		return nil
	}
	return res.ToolCalls
}

func quotaReason(res *runner.Result, runErr error) string {
	if res != nil && res.Quota.Reason != "" {
		return res.Quota.Reason
	}
	if res != nil {
		if reason, ok := runner.ParseQuotaReason(res.Text); ok {
			return reason
		}
	}
	if runErr != nil {
		if reason, ok := runner.ParseQuotaReason(runErr.Error()); ok {
			return reason
		}
		return runErr.Error()
	}
	return "Claude usage limit"
}

func (d *Daemon) shouldFallbackToCursor(kind string, res *runner.Result, runErr error) bool {
	if kind != config.AgentClaude || !d.conf().Cursor.FallbackEnabled() {
		return false
	}
	hit := res != nil && res.Quota.Hit
	if !hit && res != nil {
		_, hit = runner.ParseQuotaReason(res.Text)
	}
	if !hit && runErr != nil {
		_, hit = runner.ParseQuotaReason(runErr.Error())
	}
	if !hit {
		return false
	}
	if !d.Runner.CursorAvailable() {
		d.Log.Printf("Claude quota hit but Cursor binary %q not found; not falling back", d.Runner.BinFor(config.AgentCursor))
		return false
	}
	return true
}

func cursorFallbackPrompt(reason, original string) string {
	return fmt.Sprintf("[taskpilot] Claude Code hit a usage/quota limit (%s). "+
		"Continuing this task on Cursor Agent in a fresh session. "+
		"Workspace, gates and approvals are unchanged — call task_info, then continue.\n\n%s",
		reason, original)
}

// outboxMaxAttempts bounds delivery retries of one report.
const outboxMaxAttempts = 8

// report queues a progress note for the ticket and tries to deliver it now;
// failures are retried by flushOutbox with backoff.
func (d *Daemon) report(ctx context.Context, t *store.Task, msg string) {
	id, err := d.Store.Enqueue(t.ID, t.Source, t.TicketID, msg)
	if err != nil {
		d.Log.Printf("%s: queue report: %v", t.ID, err)
		return
	}
	d.deliver(ctx, store.OutboxItem{ID: id, TaskID: t.ID, TicketID: t.TicketID, Message: msg})
}

func (d *Daemon) deliver(ctx context.Context, o store.OutboxItem) {
	if err := d.Source.Report(ctx, o.TicketID, o.Message); err != nil {
		d.Log.Printf("%s: report to %s failed (will retry): %v", o.TaskID, d.Source.Name(), err)
		_ = d.Store.MarkFailed(o.ID, err, outboxMaxAttempts)
		return
	}
	_ = d.Store.MarkSent(o.ID)
}

// flushOutbox retries reports whose next attempt is due.
func (d *Daemon) flushOutbox(ctx context.Context) {
	due, err := d.Store.DueOutbox(20)
	if err != nil {
		d.Log.Printf("outbox: %v", err)
		return
	}
	for _, o := range due {
		d.deliver(ctx, o)
	}
}

func (d *Daemon) isRunning(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.running[id]
}

// InFlight is the number of task runs in progress.
func (d *Daemon) InFlight() int { return d.inFlight() }

func (d *Daemon) inFlight() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.running)
}

// openWithBaseline opens a repo the ticket named up front and records its
// baseline verify result, as the open_repo tool would.
func (d *Daemon) openWithBaseline(ctx context.Context, t *store.Task, proj config.Project, name string) error {
	repo := proj.Repos[name]
	o, err := workspace.OpenRepo(ctx, t.Workspace, t.WorkBranch(), name, repo)
	if err != nil {
		return err
	}
	for _, n := range o.Notes {
		t.History = append(t.History, store.Event{At: time.Now().UTC(), State: store.StatePreparing, Note: name + ": " + n})
	}
	if repo.Verify == "" || !o.Created {
		return nil
	}
	r := workspace.RunVerify(ctx, o.Path, repo.Verify, 15*time.Minute)
	return d.Store.SetBaseline(t.ID, &store.Baseline{Repo: name, Command: repo.Verify, Passed: r.Passed,
		Output: r.Output, DurationMS: r.Duration.Milliseconds(), At: time.Now().UTC()})
}

// graphOn reports whether the project has graphify graphs.
func graphOn(cfg *config.Config, project string) bool {
	return cfg != nil && cfg.Graphify != nil && len(cfg.ProjectGraphLines(project)) > 0
}

func initialPrompt(t *store.Task, proj config.Project, graph bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[taskpilot] New task %s.\n\n", t.ID)
	fmt.Fprintf(&b, "Ticket %s: %s\n", t.TicketID, t.Title)
	if t.TicketURL != "" {
		fmt.Fprintf(&b, "Link: %s\n", t.TicketURL)
	}
	fmt.Fprintf(&b, "\n%s\n\n", strings.TrimSpace(t.Body))
	fmt.Fprintf(&b, "Project %s. The current directory is the task workspace %s. Open the repos the task needs with the\n", proj.Name, t.Workspace)
	fmt.Fprintf(&b, "open_repo tool; each becomes ./<repo> on the provisional branch %s. Writes are only allowed inside the workspace.\n", t.Branch)
	fmt.Fprintf(&b, "Before the first commit, name the branch by the team convention (task_info.branch_convention): set_branch, or git branch -m in each repo.\n")
	b.WriteString("Repos of this project:\n")
	for _, name := range proj.RepoNames() {
		r := proj.Repos[name]
		line := "  - " + name
		if r.Description != "" {
			line += ": " + r.Description
		}
		if name == t.RepoHint {
			line += "  [already opened: the ticket points here]"
		}
		b.WriteString(line + "\n")
	}
	if len(proj.ContextFiles) > 0 {
		b.WriteString("Project knowledge to read before planning (outside the workspace, read-only):\n")
		for _, f := range proj.ContextFiles {
			fmt.Fprintf(&b, "  - %s\n", f)
		}
	}
	if graph {
		b.WriteString("\nThe graphify tool has graphs of these repos, joined across repos by their gRPC calls. Use it before searching " +
			"the code: links for a service or gRPC method, affected on each function you plan to change (line task, and qc), " +
			"then read what it points to. The plan lists the callers it found.\n")
	}
	b.WriteString("\nLoad the taskpilot:workflow skill and follow it. Start by calling the task_info tool.\n")
	return b.String()
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func orDefault(s string) string {
	if s == "" {
		return "default"
	}
	return s
}

// releaseWorktrees frees the branches of a finished task so the operator can
// check them out in their own clone: the worktrees stay (detached at the same
// commit) for when the task is resumed.
func (d *Daemon) releaseWorktrees(t *store.Task) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	kept, err := workspace.Detach(ctx, t.Workspace)
	if err != nil {
		d.Log.Printf("%s: %v", t.ID, err)
	}
	if len(kept) > 0 {
		d.Log.Printf("%s: done, but worktree(s) with uncommitted changes keep their branch: %s", t.ID, strings.Join(kept, ", "))
	}
}

// attachWorktrees puts a resumed task's worktrees back on its branch (and
// re-creates those removed by older versions) before the agent runs.
func (d *Daemon) attachWorktrees(ctx context.Context, t *store.Task, proj config.Project) error {
	actx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	return workspace.Attach(actx, t.Workspace, t.Branch, proj.Repos)
}

// index refreshes the graphify graphs in the background, one pass at a time.
func (d *Daemon) index(ctx context.Context) {
	if d.Indexer == nil || !d.indexing.CompareAndSwap(false, true) {
		return
	}
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		defer d.indexing.Store(false)
		d.Indexer.Tick(ctx)
	}()
}

// scan starts the due production error scans in the background (a slow
// Splunk search must not hold the scheduling loop); one at a time.
func (d *Daemon) scan(ctx context.Context) {
	if d.Scanner == nil || !d.scanning.CompareAndSwap(false, true) {
		return
	}
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		defer d.scanning.Store(false)
		d.Scanner.Tick(ctx)
	}()
}
