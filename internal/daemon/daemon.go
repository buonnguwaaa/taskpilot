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
	"time"

	"github.com/buonnguwaaa/taskpilot/internal/config"
	"github.com/buonnguwaaa/taskpilot/internal/runner"
	"github.com/buonnguwaaa/taskpilot/internal/source"
	"github.com/buonnguwaaa/taskpilot/internal/store"
	"github.com/buonnguwaaa/taskpilot/internal/workspace"
)

// Daemon schedules task runs.
type Daemon struct {
	Config *config.Config
	Store  *store.Store
	Source source.Source
	Runner *runner.Runner
	Log    *log.Logger

	mu      sync.Mutex
	running map[string]bool
	wg      sync.WaitGroup
}

// Run loops until ctx is cancelled, then waits for in-flight runs to stop.
func (d *Daemon) Run(ctx context.Context) error {
	d.running = map[string]bool{}
	if err := d.recover(); err != nil {
		return err
	}
	d.Log.Printf("taskpilot daemon started (home %s, source %s, concurrency %d)", d.Store.Home, d.Source.Name(), d.Config.Daemon.Concurrency)
	for {
		d.Tick(ctx)
		select {
		case <-ctx.Done():
			d.Log.Printf("stopping; waiting for %d running task(s)", d.inFlight())
			d.wg.Wait()
			return nil
		case <-time.After(d.Config.Daemon.PollInterval):
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

// recover parks tasks a previous daemon left mid-run.
func (d *Daemon) recover() error {
	tasks, err := d.Store.ListTasks()
	if err != nil {
		return err
	}
	for _, t := range tasks {
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
	}
	return nil
}

// Tick runs one scheduling pass. Exported for tests.
func (d *Daemon) Tick(ctx context.Context) {
	if d.running == nil {
		d.running = map[string]bool{}
	}
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
		if d.isRunning(t.ID) || d.Store.TakenOver(t.ID) {
			continue
		}
		if err := d.advance(t); err != nil {
			d.Log.Printf("%s: %v", t.ID, err)
			continue
		}
		if t.State == store.StateQueued && d.inFlight() < d.Config.Daemon.Concurrency && ctx.Err() == nil {
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
		if proj, ok := d.Config.Projects[tk.Project]; !ok {
			t.LastError = fmt.Sprintf("unknown project %q (configured: %s)", tk.Project, strings.Join(d.Config.ProjectNames(), ", "))
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
		t.NextTrigger = map[bool]string{true: "approval", false: "rejection"}[dec.Approved]
		t.Transition(store.StateQueued, fmt.Sprintf("%s %s", strings.ToLower(verdict), req.ID))
		return d.Store.SaveTask(t)
	case store.StateNeedsAttention, store.StateFailed, store.StateDone:
		n, err := d.Store.TakeNudge(t.ID)
		if err != nil || n == nil {
			return err
		}
		if _, ok := d.Config.Projects[t.Project]; !ok {
			return nil // nothing to resume into
		}
		if err := d.Store.ArchiveCompletion(t.ID); err != nil {
			return err
		}
		t.NextPrompt = "[taskpilot] Message from the operator: " + n.Message
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

func (d *Daemon) start(ctx context.Context, t *store.Task) {
	d.mu.Lock()
	d.running[t.ID] = true
	d.mu.Unlock()
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		defer func() {
			d.mu.Lock()
			delete(d.running, t.ID)
			d.mu.Unlock()
		}()
		if err := d.work(ctx, t); err != nil {
			d.Log.Printf("%s: %v", t.ID, err)
		}
	}()
}

// work runs one turn of a task and parks it in the resulting state.
func (d *Daemon) work(ctx context.Context, t *store.Task) error {
	proj := d.Config.Projects[t.Project]
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
		if t.RepoHint != "" {
			if err := d.openWithBaseline(ctx, t, proj, t.RepoHint); err != nil {
				t.LastError = err.Error()
				t.Transition(store.StateFailed, "open "+t.RepoHint+": "+err.Error())
				return d.Store.SaveTask(t)
			}
		}
	}

	resume := t.Runs > 0
	prompt, trigger := t.NextPrompt, t.NextTrigger
	if prompt == "" {
		prompt, trigger = initialPrompt(t, proj), "initial"
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
	runID, err := d.Store.StartRun(t.ID, store.RunStart{N: t.Runs, SessionID: t.SessionID, Resume: resume, Trigger: trigger,
		Prompt: prompt, Model: d.Config.Claude.Model, Effort: d.Config.Claude.Effort})
	if err != nil {
		return err
	}
	d.Log.Printf("%s: run %d (%s)", t.ID, t.Runs, trigger)

	res, runErr := d.Runner.Run(ctx, t, prompt, resume)
	t.NextPrompt, t.NextTrigger = "", ""
	if res != nil {
		t.CostUSD += res.CostUSD
		if res.Text != "" {
			t.LastResult = res.Text
		}
	}

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
	case ctx.Err() != nil:
		t.Transition(store.StateNeedsAttention, "interrupted: taskpilot stopped during the run; send a message (m) to resume")
	case runErr != nil || (res != nil && res.IsError):
		reason := "run failed"
		if runErr != nil {
			reason = runErr.Error()
		} else if res.Subtype != "" {
			reason = "run ended with " + res.Subtype
		}
		t.LastError = reason
		t.Transition(store.StateNeedsAttention, reason)
	default:
		t.Transition(store.StateNeedsAttention, "agent ended its turn without requesting approval or completing")
	}
	d.Log.Printf("%s: -> %s", t.ID, t.State)
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
	var calls []store.ToolCall
	if res != nil {
		end.Subtype, end.IsError, end.NumTurns, end.CostUSD = res.Subtype, res.IsError, res.NumTurns, res.CostUSD
		end.InputTokens, end.OutputTokens = res.InputTokens, res.OutputTokens
		end.CacheReadTokens, end.CacheWriteTokens = res.CacheReadTokens, res.CacheWriteTokens
		calls = res.ToolCalls
	}
	return d.Store.FinishRun(runID, end, calls)
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
	o, err := workspace.OpenRepo(ctx, t.Workspace, t.ID, name, repo)
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

func initialPrompt(t *store.Task, proj config.Project) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[taskpilot] New task %s.\n\n", t.ID)
	fmt.Fprintf(&b, "Ticket %s: %s\n", t.TicketID, t.Title)
	if t.TicketURL != "" {
		fmt.Fprintf(&b, "Link: %s\n", t.TicketURL)
	}
	fmt.Fprintf(&b, "\n%s\n\n", strings.TrimSpace(t.Body))
	fmt.Fprintf(&b, "Project %s. The current directory is the task workspace %s. Open the repos the task needs with the\n", proj.Name, t.Workspace)
	fmt.Fprintf(&b, "open_repo tool; each becomes ./<repo> on branch %s. Writes are only allowed inside the workspace.\n", t.Branch)
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
