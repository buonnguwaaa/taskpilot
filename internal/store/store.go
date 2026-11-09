// Package store persists task state in the taskpilot SQLite database
// (<home>/taskpilot.db, see internal/db). Large append-only blobs stay on disk
// under <home>/tasks/<task-id>/ and are referenced by path:
//
//	events.jsonl         raw stream-json events of every run
//	runs/<n>.stderr.log  stderr of each run
//
// Several processes share the database (daemon, TUI, the per-task MCP server,
// the PreToolUse hook): WAL mode plus immediate write transactions serialise
// writers, and every multi-row change is one transaction.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/buonnguwaaa/taskpilot/internal/db"
)

// State is a task's position in the workflow.
type State string

const (
	StateQueued         State = "queued"
	StatePreparing      State = "preparing"
	StateRunning        State = "running"
	StateAwaitApproval  State = "awaiting_approval"
	StateNeedsAttention State = "needs_attention"
	StateDone           State = "done"
	StateFailed         State = "failed"
)

// Terminal reports whether no further automatic work happens in this state.
func (s State) Terminal() bool { return s == StateDone || s == StateFailed }

// ErrNotFound is returned for unknown tasks (it matches fs.ErrNotExist).
var ErrNotFound = fs.ErrNotExist

// Event is one entry in a task's history.
type Event struct {
	At    time.Time `json:"at"`
	State State     `json:"state"`
	Note  string    `json:"note,omitempty"`
}

// Task is the daemon-owned record of one ticket being worked on.
type Task struct {
	ID        string `json:"id"`
	Source    string `json:"source"`
	TicketID  string `json:"ticket_id"`
	TicketURL string `json:"ticket_url,omitempty"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	Project   string `json:"project"`
	// RepoHint is the repo the ticket suggested (optional); it is opened up front.
	RepoHint string `json:"repo_hint,omitempty"`

	// Branch is the work branch used in every repo the task opens.
	Branch string `json:"branch,omitempty"`
	// Workspace is the task directory; each opened repo is a worktree inside it.
	Workspace string `json:"workspace,omitempty"`
	SessionID string `json:"session_id,omitempty"`

	State State `json:"state"`
	// NextPrompt is what the next run sends; empty means the initial prompt.
	NextPrompt string `json:"next_prompt,omitempty"`
	// NextTrigger says why the next run happens: approval, rejection, operator_message.
	NextTrigger string `json:"next_trigger,omitempty"`

	Runs       int     `json:"runs"`
	CostUSD    float64 `json:"cost_usd"`
	LastResult string  `json:"last_result,omitempty"`
	LastError  string  `json:"last_error,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	History   []Event   `json:"history,omitempty"`

	persisted int // history entries already stored
}

// Transition moves the task to state s and records it in History.
func (t *Task) Transition(s State, note string) {
	now := time.Now().UTC()
	t.State = s
	t.UpdatedAt = now
	t.History = append(t.History, Event{At: now, State: s, Note: note})
}

// ApprovalRequest is raised by the agent through the MCP request_approval tool.
type ApprovalRequest struct {
	ID        string    `json:"id"`
	Gate      string    `json:"gate"`
	Summary   string    `json:"summary"`
	Details   string    `json:"details,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Decision is the operator's answer to an ApprovalRequest.
type Decision struct {
	RequestID string    `json:"request_id"`
	Gate      string    `json:"gate"`
	Approved  bool      `json:"approved"`
	Comment   string    `json:"comment,omitempty"`
	DecidedAt time.Time `json:"decided_at"`
}

// Completion is written when the agent calls complete_task.
type Completion struct {
	Summary   string    `json:"summary"`
	PRURL     string    `json:"pr_url,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Nudge is an operator message used to resume a stopped task.
type Nudge struct {
	Message   string    `json:"message"`
	CreatedAt time.Time `json:"created_at"`
}

// Baseline is the verify result of a repo before the agent changed anything.
type Baseline struct {
	Repo       string    `json:"repo"`
	Command    string    `json:"command"`
	Passed     bool      `json:"passed"`
	Output     string    `json:"output"` // tail
	DurationMS int64     `json:"duration_ms"`
	At         time.Time `json:"at"`
}

// PullRequest records a PR the task opened.
type PullRequest struct {
	Repo        string `json:"repo"`
	ID          int    `json:"id"`
	URL         string `json:"url"`
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	// Pending: the branch is pushed but the PR is not created yet; URL is the
	// pre-filled "create pull request" page for the operator to open.
	Pending   bool      `json:"pending,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// SplunkQuery is an audit record of one agent search.
type SplunkQuery struct {
	At       time.Time `json:"at"`
	Query    string    `json:"query"` // as sent, with the project filter
	Earliest string    `json:"earliest"`
	Latest   string    `json:"latest,omitempty"`
	Events   int       `json:"events"`
	Error    string    `json:"error,omitempty"`
}

// Store is rooted at the taskpilot home directory.
type Store struct {
	Home string
	DB   *db.DB
}

// New opens (and migrates) the database under home.
func New(home string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(home, "tasks"), 0o755); err != nil {
		return nil, err
	}
	d, err := db.Open(context.Background(), home)
	if err != nil {
		return nil, err
	}
	return &Store{Home: home, DB: d}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.DB.Close() }

var unsafeID = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// TaskID builds a filesystem-safe task id from a source name and ticket id.
func TaskID(source, ticketID string) string {
	id := unsafeID.ReplaceAllString(source+"-"+ticketID, "-")
	return strings.Trim(id, "-.")
}

// ValidID reports whether id is safe to use as a path component.
func ValidID(id string) bool {
	return id != "" && id != "." && id != ".." && !unsafeID.MatchString(id)
}

// TaskDir returns the directory holding a task's on-disk blobs.
func (s *Store) TaskDir(id string) string { return filepath.Join(s.Home, "tasks", id) }

// EventsPath is the stream-json event log of a task.
func (s *Store) EventsPath(id string) string { return filepath.Join(s.TaskDir(id), "events.jsonl") }

// StderrPath is the stderr log of run n.
func (s *Store) StderrPath(id string, n int) string {
	return filepath.Join(s.TaskDir(id), "runs", fmt.Sprintf("%d.stderr.log", n))
}

func ts(t time.Time) string {
	if t.IsZero() {
		t = time.Now()
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func parseTS(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

func bg() context.Context { return context.Background() }

// ---- tasks ---------------------------------------------------------------------

// Exists reports whether a task record exists.
func (s *Store) Exists(id string) bool {
	var n int
	_ = s.DB.QueryRowContext(bg(), `SELECT count(*) FROM tasks WHERE id = ?`, id).Scan(&n)
	return n > 0
}

// SaveTask writes the task record and its new history entries. Only the daemon
// calls this; data written by other processes lives in other tables.
func (s *Store) SaveTask(t *Task) error {
	if !ValidID(t.ID) {
		return fmt.Errorf("invalid task id %q", t.ID)
	}
	if err := os.MkdirAll(s.TaskDir(t.ID), 0o755); err != nil {
		return err
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}
	if t.UpdatedAt.IsZero() {
		t.UpdatedAt = t.CreatedAt
	}
	var closed any
	if t.State.Terminal() {
		closed = ts(t.UpdatedAt)
	}
	tx, err := s.DB.BeginTx(bg(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`
		INSERT INTO tasks (id, source, ticket_id, ticket_url, title, body, project, repo_hint, branch, workspace,
		                   session_id, state, next_prompt, next_trigger, runs_count, cost_usd, last_result, last_error,
		                   created_at, updated_at, closed_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT (id) DO UPDATE SET
			source=excluded.source, ticket_id=excluded.ticket_id, ticket_url=excluded.ticket_url,
			title=excluded.title, body=excluded.body, project=excluded.project, repo_hint=excluded.repo_hint,
			branch=excluded.branch, workspace=excluded.workspace, session_id=excluded.session_id,
			state=excluded.state, next_prompt=excluded.next_prompt, next_trigger=excluded.next_trigger,
			runs_count=excluded.runs_count, cost_usd=excluded.cost_usd, last_result=excluded.last_result,
			last_error=excluded.last_error, updated_at=excluded.updated_at, closed_at=excluded.closed_at`,
		t.ID, t.Source, t.TicketID, t.TicketURL, t.Title, t.Body, t.Project, t.RepoHint, t.Branch, t.Workspace,
		t.SessionID, string(t.State), t.NextPrompt, t.NextTrigger, t.Runs, t.CostUSD, t.LastResult, t.LastError,
		ts(t.CreatedAt), ts(t.UpdatedAt), closed)
	if err != nil {
		return err
	}
	for _, e := range t.History[min(t.persisted, len(t.History)):] {
		if _, err := tx.Exec(`INSERT INTO task_transitions (task_id, at, state, note) VALUES (?,?,?,?)`,
			t.ID, ts(e.At), string(e.State), e.Note); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	t.persisted = len(t.History)
	return nil
}

const taskCols = `id, source, ticket_id, ticket_url, title, body, project, repo_hint, branch, workspace, session_id,
	state, next_prompt, next_trigger, runs_count, cost_usd, last_result, last_error, created_at, updated_at`

type scanner interface{ Scan(dest ...any) error }

func scanTask(r scanner) (*Task, error) {
	var t Task
	var state, created, updated string
	err := r.Scan(&t.ID, &t.Source, &t.TicketID, &t.TicketURL, &t.Title, &t.Body, &t.Project, &t.RepoHint,
		&t.Branch, &t.Workspace, &t.SessionID, &state, &t.NextPrompt, &t.NextTrigger, &t.Runs, &t.CostUSD,
		&t.LastResult, &t.LastError, &created, &updated)
	if err != nil {
		return nil, err
	}
	t.State, t.CreatedAt, t.UpdatedAt = State(state), parseTS(created), parseTS(updated)
	return &t, nil
}

func (s *Store) history(id string) (map[string][]Event, error) {
	out := map[string][]Event{}
	q := `SELECT task_id, at, state, note FROM task_transitions`
	var args []any
	if id != "" {
		q += ` WHERE task_id = ?`
		args = append(args, id)
	}
	rows, err := s.DB.QueryContext(bg(), q+` ORDER BY task_id, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var tid, at, state, note string
		if err := rows.Scan(&tid, &at, &state, &note); err != nil {
			return nil, err
		}
		out[tid] = append(out[tid], Event{At: parseTS(at), State: State(state), Note: note})
	}
	return out, rows.Err()
}

// LoadTask reads one task with its history.
func (s *Store) LoadTask(id string) (*Task, error) {
	t, err := scanTask(s.DB.QueryRowContext(bg(), `SELECT `+taskCols+` FROM tasks WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("task %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	h, err := s.history(id)
	if err != nil {
		return nil, err
	}
	t.History, t.persisted = h[id], len(h[id])
	return t, nil
}

// ListTasks returns all tasks, newest first.
func (s *Store) ListTasks() ([]*Task, error) {
	rows, err := s.DB.QueryContext(bg(), `SELECT `+taskCols+` FROM tasks ORDER BY created_at DESC, id`)
	if err != nil {
		return nil, err
	}
	var out []*Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	h, err := s.history("")
	if err != nil {
		return nil, err
	}
	for _, t := range out {
		t.History, t.persisted = h[t.ID], len(h[t.ID])
	}
	return out, nil
}

// ---- approvals -----------------------------------------------------------------

// AddRequest records a new approval request for gate and returns it.
func (s *Store) AddRequest(id, gate, summary, details string) (*ApprovalRequest, error) {
	tx, err := s.DB.BeginTx(bg(), nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRow(`SELECT count(*) FROM approvals WHERE task_id = ? AND gate = ?`, id, gate).Scan(&n); err != nil {
		return nil, err
	}
	r := &ApprovalRequest{ID: fmt.Sprintf("%s-%d", gate, n+1), Gate: gate, Summary: summary, Details: details, CreatedAt: time.Now().UTC()}
	if _, err := tx.Exec(`INSERT INTO approvals (task_id, id, gate, attempt, summary, details, created_at) VALUES (?,?,?,?,?,?,?)`,
		id, r.ID, gate, n+1, summary, details, ts(r.CreatedAt)); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, fmt.Errorf("task %s already has a request waiting for the operator", id)
		}
		return nil, err
	}
	return r, tx.Commit()
}

// Requests returns all approval requests, oldest first.
func (s *Store) Requests(id string) ([]*ApprovalRequest, error) {
	rows, err := s.DB.QueryContext(bg(), `SELECT id, gate, summary, details, created_at FROM approvals WHERE task_id = ? ORDER BY rowid`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ApprovalRequest
	for rows.Next() {
		var r ApprovalRequest
		var created string
		if err := rows.Scan(&r.ID, &r.Gate, &r.Summary, &r.Details, &created); err != nil {
			return nil, err
		}
		r.CreatedAt = parseTS(created)
		out = append(out, &r)
	}
	return out, rows.Err()
}

// Decisions returns all decisions keyed by request id.
func (s *Store) Decisions(id string) (map[string]*Decision, error) {
	rows, err := s.DB.QueryContext(bg(),
		`SELECT id, gate, decision, comment, decided_at FROM approvals WHERE task_id = ? AND decision IS NOT NULL`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*Decision{}
	for rows.Next() {
		var d Decision
		var decision, at string
		if err := rows.Scan(&d.RequestID, &d.Gate, &decision, &d.Comment, &at); err != nil {
			return nil, err
		}
		d.Approved, d.DecidedAt = decision == "approved", parseTS(at)
		out[d.RequestID] = &d
	}
	return out, rows.Err()
}

// Decide records the operator's decision on a pending request.
func (s *Store) Decide(id string, req *ApprovalRequest, approved bool, comment string) error {
	decision := map[bool]string{true: "approved", false: "rejected"}[approved]
	res, err := s.DB.ExecContext(bg(), `UPDATE approvals SET decision = ?, comment = ?, decided_by = 'operator', decided_at = ?
		WHERE task_id = ? AND id = ? AND decision IS NULL`, decision, comment, ts(time.Now()), id, req.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("request %s of %s is not pending (already decided?)", req.ID, id)
	}
	return nil
}

// PendingRequest returns the request waiting for the operator, or nil.
func (s *Store) PendingRequest(id string) (*ApprovalRequest, error) {
	var r ApprovalRequest
	var created string
	err := s.DB.QueryRowContext(bg(), `SELECT id, gate, summary, details, created_at FROM approvals
		WHERE task_id = ? AND decision IS NULL ORDER BY rowid LIMIT 1`, id).Scan(&r.ID, &r.Gate, &r.Summary, &r.Details, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.CreatedAt = parseTS(created)
	return &r, nil
}

// GateApproved reports whether any request for gate has been approved.
func (s *Store) GateApproved(id, gate string) (bool, error) {
	var n int
	err := s.DB.QueryRowContext(bg(), `SELECT count(*) FROM approvals WHERE task_id = ? AND gate = ? AND decision = 'approved'`, id, gate).Scan(&n)
	return n > 0, err
}

// ---- completion ----------------------------------------------------------------

// SetCompletion records that the agent finished the task.
func (s *Store) SetCompletion(id string, c *Completion) error {
	tx, err := s.DB.BeginTx(bg(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE completions SET archived_at = ? WHERE task_id = ? AND archived_at IS NULL`, ts(time.Now()), id); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO completions (task_id, summary, pr_url, created_at) VALUES (?,?,?,?)`,
		id, c.Summary, c.PRURL, ts(c.CreatedAt)); err != nil {
		return err
	}
	return tx.Commit()
}

// Completion returns the current completion record, or nil.
func (s *Store) Completion(id string) (*Completion, error) {
	var c Completion
	var created string
	err := s.DB.QueryRowContext(bg(), `SELECT summary, pr_url, created_at FROM completions WHERE task_id = ? AND archived_at IS NULL`, id).
		Scan(&c.Summary, &c.PRURL, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c.CreatedAt = parseTS(created)
	return &c, nil
}

// ArchiveCompletion sets the completion aside so a follow-up run on a reopened
// task is judged on its own outcome.
func (s *Store) ArchiveCompletion(id string) error {
	_, err := s.DB.ExecContext(bg(), `UPDATE completions SET archived_at = ? WHERE task_id = ? AND archived_at IS NULL`, ts(time.Now()), id)
	return err
}

// ---- operator messages -----------------------------------------------------------

// SetNudge records an operator message that resumes a stopped task.
func (s *Store) SetNudge(id, msg string) error {
	_, err := s.DB.ExecContext(bg(), `INSERT INTO operator_messages (task_id, message, created_at) VALUES (?,?,?)`, id, msg, ts(time.Now()))
	return err
}

// TakeNudge consumes the pending messages (joined, oldest first), or returns nil.
func (s *Store) TakeNudge(id string) (*Nudge, error) {
	tx, err := s.DB.BeginTx(bg(), nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT message, created_at FROM operator_messages WHERE task_id = ? AND consumed_at IS NULL ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	var msgs []string
	var first string
	for rows.Next() {
		var m, at string
		if err := rows.Scan(&m, &at); err != nil {
			rows.Close()
			return nil, err
		}
		if first == "" {
			first = at
		}
		msgs = append(msgs, m)
	}
	rows.Close()
	if len(msgs) == 0 {
		return nil, nil
	}
	if _, err := tx.Exec(`UPDATE operator_messages SET consumed_at = ? WHERE task_id = ? AND consumed_at IS NULL`, ts(time.Now()), id); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &Nudge{Message: strings.Join(msgs, "\n\n"), CreatedAt: parseTS(first)}, nil
}

// ---- takeover --------------------------------------------------------------------

// SetTakeover marks (or clears) that the operator drives the session from this
// process; the pid lets a row left by a killed process be recognised as stale.
func (s *Store) SetTakeover(id string, on bool) error {
	if !on {
		_, err := s.DB.ExecContext(bg(), `DELETE FROM takeovers WHERE task_id = ?`, id)
		return err
	}
	_, err := s.DB.ExecContext(bg(), `INSERT INTO takeovers (task_id, pid, started_at) VALUES (?,?,?)
		ON CONFLICT (task_id) DO UPDATE SET pid = excluded.pid, started_at = excluded.started_at`, id, os.Getpid(), ts(time.Now()))
	return err
}

// TakenOver reports whether the operator drives the session. A row whose
// process no longer exists does not count.
func (s *Store) TakenOver(id string) bool {
	var pid int
	if err := s.DB.QueryRowContext(bg(), `SELECT pid FROM takeovers WHERE task_id = ?`, id).Scan(&pid); err != nil {
		return false
	}
	if pid > 0 {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return false
		}
	}
	return true
}

// ---- baselines, PRs, Splunk audit --------------------------------------------------

// SetBaseline records a repo's baseline verify result.
func (s *Store) SetBaseline(id string, b *Baseline) error {
	_, err := s.DB.ExecContext(bg(), `INSERT INTO baselines (task_id, repo, command, passed, output, duration_ms, at) VALUES (?,?,?,?,?,?,?)
		ON CONFLICT (task_id, repo) DO UPDATE SET command=excluded.command, passed=excluded.passed, output=excluded.output,
		duration_ms=excluded.duration_ms, at=excluded.at`,
		id, b.Repo, b.Command, b.Passed, b.Output, b.DurationMS, ts(b.At))
	return err
}

// Baselines returns recorded baselines keyed by repo.
func (s *Store) Baselines(id string) (map[string]*Baseline, error) {
	rows, err := s.DB.QueryContext(bg(), `SELECT repo, command, passed, output, duration_ms, at FROM baselines WHERE task_id = ?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*Baseline{}
	for rows.Next() {
		var b Baseline
		var at string
		if err := rows.Scan(&b.Repo, &b.Command, &b.Passed, &b.Output, &b.DurationMS, &at); err != nil {
			return nil, err
		}
		b.At = parseTS(at)
		out[b.Repo] = &b
	}
	return out, rows.Err()
}

// SetPullRequest records a repo's PR into one target branch.
func (s *Store) SetPullRequest(id string, pr *PullRequest) error {
	_, err := s.DB.ExecContext(bg(), `INSERT INTO pull_requests (task_id, repo, destination, pr_id, url, source, title, description, pending, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT (task_id, repo, destination) DO UPDATE SET pr_id=excluded.pr_id, url=excluded.url, source=excluded.source,
		title=excluded.title, description=excluded.description, pending=excluded.pending`,
		id, pr.Repo, pr.Destination, pr.ID, pr.URL, pr.Source, pr.Title, pr.Description, pr.Pending, ts(pr.CreatedAt))
	return err
}

// PullRequests returns the task's PRs ordered by repo, then creation time.
func (s *Store) PullRequests(id string) ([]*PullRequest, error) {
	rows, err := s.DB.QueryContext(bg(), `SELECT repo, destination, pr_id, url, source, title, description, pending, created_at
		FROM pull_requests WHERE task_id = ? ORDER BY repo, created_at, rowid`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*PullRequest
	for rows.Next() {
		var p PullRequest
		var created string
		if err := rows.Scan(&p.Repo, &p.Destination, &p.ID, &p.URL, &p.Source, &p.Title, &p.Description, &p.Pending, &created); err != nil {
			return nil, err
		}
		p.CreatedAt = parseTS(created)
		out = append(out, &p)
	}
	return out, rows.Err()
}

// PullRequestInto returns the repo's PR into target, or nil.
func (s *Store) PullRequestInto(id, repo, target string) (*PullRequest, error) {
	prs, err := s.PullRequests(id)
	if err != nil {
		return nil, err
	}
	for _, pr := range prs {
		if pr.Repo == repo && pr.Destination == target {
			return pr, nil
		}
	}
	return nil, nil
}

// AppendSplunkQuery records one agent search.
func (s *Store) AppendSplunkQuery(id string, q *SplunkQuery) error {
	_, err := s.DB.ExecContext(bg(), `INSERT INTO splunk_queries (task_id, at, query, earliest, latest, events, error) VALUES (?,?,?,?,?,?,?)`,
		id, ts(q.At), q.Query, q.Earliest, q.Latest, q.Events, q.Error)
	return err
}

// SplunkQueries returns a task's searches, oldest first.
func (s *Store) SplunkQueries(id string) ([]*SplunkQuery, error) {
	rows, err := s.DB.QueryContext(bg(), `SELECT at, query, earliest, latest, events, error FROM splunk_queries WHERE task_id = ? ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*SplunkQuery
	for rows.Next() {
		var q SplunkQuery
		var at string
		if err := rows.Scan(&at, &q.Query, &q.Earliest, &q.Latest, &q.Events, &q.Error); err != nil {
			return nil, err
		}
		q.At = parseTS(at)
		out = append(out, &q)
	}
	return out, rows.Err()
}
