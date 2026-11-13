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
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
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

// WorkBranch is the task's work branch (the provisional tp/<id> until set).
func (t *Task) WorkBranch() string {
	if t.Branch != "" {
		return t.Branch
	}
	return "tp/" + t.ID
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
	// Questions the operator answers by picking options (may be empty).
	Questions []Question `json:"questions,omitempty"`
}

// QuestionGate is the pseudo-gate of a request that only asks questions
// (MCP tool ask_operator): answering it resumes the agent.
const QuestionGate = "question"

// Question is a multiple-choice question for the operator, as Claude Code's
// AskUserQuestion: the operator picks one option (or several when Multi), or
// writes another answer.
type Question struct {
	Question string   `json:"question"`
	Header   string   `json:"header,omitempty"`
	Options  []Option `json:"options"`
	Multi    bool     `json:"multiSelect,omitempty"`
}

// Option is one choice of a Question.
type Option struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// ValidateQuestions checks what an agent sends: 1–4 questions, 2–6 labelled
// options each.
func ValidateQuestions(qs []Question) error {
	if len(qs) > 4 {
		return fmt.Errorf("at most 4 questions per request (got %d)", len(qs))
	}
	for i, q := range qs {
		if strings.TrimSpace(q.Question) == "" {
			return fmt.Errorf("question %d is empty", i+1)
		}
		if len(q.Options) < 2 || len(q.Options) > 6 {
			return fmt.Errorf("question %d needs 2 to 6 options (got %d); the operator can always write another answer", i+1, len(q.Options))
		}
		for j, o := range q.Options {
			if strings.TrimSpace(o.Label) == "" {
				return fmt.Errorf("question %d, option %d has no label", i+1, j+1)
			}
		}
	}
	return nil
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
	return s.AddRequestWithQuestions(id, gate, summary, details, nil)
}

// AddRequestWithQuestions records a request that also asks the operator
// multiple-choice questions.
func (s *Store) AddRequestWithQuestions(id, gate, summary, details string, qs []Question) (*ApprovalRequest, error) {
	if err := ValidateQuestions(qs); err != nil {
		return nil, err
	}
	qjson := ""
	if len(qs) > 0 {
		b, err := json.Marshal(qs)
		if err != nil {
			return nil, err
		}
		qjson = string(b)
	}
	tx, err := s.DB.BeginTx(bg(), nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRow(`SELECT count(*) FROM approvals WHERE task_id = ? AND gate = ?`, id, gate).Scan(&n); err != nil {
		return nil, err
	}
	r := &ApprovalRequest{ID: fmt.Sprintf("%s-%d", gate, n+1), Gate: gate, Summary: summary, Details: details, CreatedAt: time.Now().UTC(), Questions: qs}
	if _, err := tx.Exec(`INSERT INTO approvals (task_id, id, gate, attempt, summary, details, created_at, questions) VALUES (?,?,?,?,?,?,?,?)`,
		id, r.ID, gate, n+1, summary, details, ts(r.CreatedAt), qjson); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, fmt.Errorf("task %s already has a request waiting for the operator", id)
		}
		return nil, err
	}
	return r, tx.Commit()
}

// Requests returns all approval requests, oldest first.
func (s *Store) Requests(id string) ([]*ApprovalRequest, error) {
	rows, err := s.DB.QueryContext(bg(), `SELECT id, gate, summary, details, created_at, questions FROM approvals WHERE task_id = ? ORDER BY rowid`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ApprovalRequest
	for rows.Next() {
		var r ApprovalRequest
		var created, qs string
		if err := rows.Scan(&r.ID, &r.Gate, &r.Summary, &r.Details, &created, &qs); err != nil {
			return nil, err
		}
		r.CreatedAt = parseTS(created)
		r.Questions = parseQuestions(qs)
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
	var created, qs string
	err := s.DB.QueryRowContext(bg(), `SELECT id, gate, summary, details, created_at, questions FROM approvals
		WHERE task_id = ? AND decision IS NULL ORDER BY rowid LIMIT 1`, id).Scan(&r.ID, &r.Gate, &r.Summary, &r.Details, &created, &qs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.CreatedAt = parseTS(created)
	r.Questions = parseQuestions(qs)
	return &r, nil
}

func parseQuestions(s string) []Question {
	if s == "" {
		return nil
	}
	var qs []Question
	_ = json.Unmarshal([]byte(s), &qs)
	return qs
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

// CreatedPRs keeps the PRs that exist on Bitbucket (not pending), optionally
// only those of repo and/or into dest.
func CreatedPRs(prs []*PullRequest, repo, dest string) []*PullRequest {
	var out []*PullRequest
	for _, pr := range prs {
		if pr.Pending || pr.ID <= 0 || repo != "" && pr.Repo != repo || dest != "" && pr.Destination != dest {
			continue
		}
		out = append(out, pr)
	}
	return out
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

// HasakiLink is the Hasaki Work task a local task was pushed to.
type HasakiLink struct {
	HWID      int64
	Code      string
	CreatedAt time.Time
}

// SetHasakiLink records that the task was pushed to Hasaki Work.
func (s *Store) SetHasakiLink(id string, l *HasakiLink) error {
	_, err := s.DB.ExecContext(bg(), `INSERT INTO hasaki_links (task_id, hw_id, code, created_at) VALUES (?,?,?,?)
		ON CONFLICT (task_id) DO UPDATE SET hw_id=excluded.hw_id, code=excluded.code, created_at=excluded.created_at`,
		id, l.HWID, l.Code, ts(l.CreatedAt))
	return err
}

// HasakiLink returns the Hasaki Work task the task was pushed to, or nil.
func (s *Store) HasakiLink(id string) (*HasakiLink, error) {
	var l HasakiLink
	var created string
	err := s.DB.QueryRowContext(bg(), `SELECT hw_id, code, created_at FROM hasaki_links WHERE task_id = ?`, id).Scan(&l.HWID, &l.Code, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	l.CreatedAt = parseTS(created)
	return &l, nil
}

// DeleteTask removes a task: its record and everything attached to it (the
// related tables cascade) and its directory (event log, stderr logs). The
// workspace and the source ticket are the caller's business.
func (s *Store) DeleteTask(id string) error {
	if !ValidID(id) {
		return fmt.Errorf("invalid task id %q", id)
	}
	res, err := s.DB.ExecContext(bg(), `DELETE FROM tasks WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return os.RemoveAll(s.TaskDir(id))
}

// DBQuery is an audit record of one agent query on a QC database.
type DBQuery struct {
	At     time.Time
	Target string // host:port/schema
	Query  string
	Rows   int
	Error  string
}

// AppendDBQuery records one agent query.
func (s *Store) AppendDBQuery(id string, q *DBQuery) error {
	_, err := s.DB.ExecContext(bg(), `INSERT INTO db_queries (task_id, at, target, query, rows, error) VALUES (?,?,?,?,?,?)`,
		id, ts(q.At), q.Target, q.Query, q.Rows, q.Error)
	return err
}

// Profile is the model and effort a task runs with. The agent sets it after
// assessing the task; the operator can pin one, which the agent cannot change.
type Profile struct {
	Model     string
	Effort    string
	Reason    string
	SetBy     string // "agent" or "operator"
	UpdatedAt time.Time
}

// ErrProfilePinned is returned when the agent tries to change a profile the
// operator pinned.
var ErrProfilePinned = errors.New("the operator pinned this task's execution profile")

// TaskProfile returns the task's profile, or nil.
func (s *Store) TaskProfile(id string) (*Profile, error) {
	var p Profile
	var at string
	err := s.DB.QueryRowContext(bg(), `SELECT model, effort, reason, set_by, updated_at FROM task_profiles WHERE task_id = ?`, id).
		Scan(&p.Model, &p.Effort, &p.Reason, &p.SetBy, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p.UpdatedAt = parseTS(at)
	return &p, nil
}

// SetTaskProfile records the task's profile. The agent cannot replace one the
// operator pinned (ErrProfilePinned).
func (s *Store) SetTaskProfile(id string, p *Profile) error {
	q := `INSERT INTO task_profiles (task_id, model, effort, reason, set_by, updated_at) VALUES (?,?,?,?,?,?)
		ON CONFLICT (task_id) DO UPDATE SET model=excluded.model, effort=excluded.effort, reason=excluded.reason,
		set_by=excluded.set_by, updated_at=excluded.updated_at`
	if p.SetBy == "agent" {
		q += ` WHERE task_profiles.set_by = 'agent'`
	}
	res, err := s.DB.ExecContext(bg(), q, id, p.Model, p.Effort, p.Reason, p.SetBy, ts(time.Now()))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrProfilePinned
	}
	return nil
}

// ClearTaskProfile removes the task's profile (back to the config default).
func (s *Store) ClearTaskProfile(id string) error {
	_, err := s.DB.ExecContext(bg(), `DELETE FROM task_profiles WHERE task_id = ?`, id)
	return err
}

// VaultRead is an audit record of one agent read of the QC Vault (no values).
type VaultRead struct {
	At     time.Time
	Action string // list, get
	Path   string
	Keys   int
	Error  string
}

// AppendVaultRead records one agent read of the QC Vault.
func (s *Store) AppendVaultRead(id string, r *VaultRead) error {
	_, err := s.DB.ExecContext(bg(), `INSERT INTO vault_reads (task_id, at, action, path, keys, error) VALUES (?,?,?,?,?,?)`,
		id, ts(r.At), r.Action, r.Path, r.Keys, r.Error)
	return err
}

// VaultReads lists a task's QC Vault reads, oldest first.
func (s *Store) VaultReads(id string) ([]*VaultRead, error) {
	rows, err := s.DB.QueryContext(bg(), `SELECT at, action, path, keys, error FROM vault_reads WHERE task_id = ? ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*VaultRead
	for rows.Next() {
		var r VaultRead
		var at string
		if err := rows.Scan(&at, &r.Action, &r.Path, &r.Keys, &r.Error); err != nil {
			return nil, err
		}
		r.At = parseTS(at)
		out = append(out, &r)
	}
	return out, rows.Err()
}

// RunTime is the total time the agent ran on a task (all runs).
func (s *Store) RunTime(id string) time.Duration {
	var ms sql.NullInt64
	_ = s.DB.QueryRowContext(bg(), `SELECT sum(duration_ms) FROM runs WHERE task_id = ?`, id).Scan(&ms)
	return time.Duration(ms.Int64) * time.Millisecond
}

// ---- production error scans ------------------------------------------------------

// ErrorScan is one scheduled scan of a project's production errors.
type ErrorScan struct {
	ID         int64
	Project    string
	StartedAt  time.Time
	FinishedAt time.Time
	Earliest   time.Time
	Latest     time.Time
	Groups     int
	New        int
	Error      string
}

// Suggestion is a production error group kept for the operator to act on.
type Suggestion struct {
	ID          int64
	Project     string
	Service     string
	Signature   string
	Status      string // new, dismissed, snoozed, task
	SnoozeUntil time.Time
	TaskID      string
	Count       int // over all scans
	LastCount   int // in the latest scan that saw it
	FirstSeen   time.Time
	LastSeen    time.Time
	Sample      string
	Caller      string
	Prefix      string
	RequestID   string
	Stacktrace  string
	FlowID      int64 // 0 = no flow
}

// LastScan returns the project's latest finished scan, or nil.
func (s *Store) LastScan(project string) (*ErrorScan, error) { return s.lastScan(project, false) }

// LastGoodScan returns the project's latest scan that succeeded, or nil.
func (s *Store) LastGoodScan(project string) (*ErrorScan, error) { return s.lastScan(project, true) }

func (s *Store) lastScan(project string, okOnly bool) (*ErrorScan, error) {
	var sc ErrorScan
	var started, finished, earliest, latest string
	cond := ""
	if okOnly {
		cond = " AND error = ''"
	}
	err := s.DB.QueryRowContext(bg(), `SELECT id, project, started_at, coalesce(finished_at, ''), earliest, latest, groups, new, error
		FROM error_scans WHERE project = ? AND finished_at IS NOT NULL`+cond+` ORDER BY started_at DESC, id DESC LIMIT 1`, project).
		Scan(&sc.ID, &sc.Project, &started, &finished, &earliest, &latest, &sc.Groups, &sc.New, &sc.Error)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	sc.StartedAt, sc.FinishedAt, sc.Earliest, sc.Latest = parseTS(started), parseTS(finished), parseTS(earliest), parseTS(latest)
	return &sc, nil
}

// StartScan records the start of a scan over [earliest, latest).
func (s *Store) StartScan(project string, earliest, latest time.Time) (int64, error) {
	res, err := s.DB.ExecContext(bg(), `INSERT INTO error_scans (project, started_at, earliest, latest) VALUES (?,?,?,?)`,
		project, ts(time.Now()), ts(earliest), ts(latest))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// FinishScan records the outcome of a scan.
func (s *Store) FinishScan(id int64, groups, fresh int, scanErr error) error {
	msg := ""
	if scanErr != nil {
		msg = scanErr.Error()
	}
	_, err := s.DB.ExecContext(bg(), `UPDATE error_scans SET finished_at = ?, groups = ?, new = ?, error = ? WHERE id = ?`,
		ts(time.Now()), groups, fresh, msg, id)
	return err
}

// UpsertSuggestion records a group seen by scan scanID. A new group, or one
// whose snooze is over, becomes (again) a suggestion to show: fresh=true.
// Dismissed groups and groups with a task only get their counts updated.
func (s *Store) UpsertSuggestion(g *Suggestion, scanID int64, now time.Time) (fresh bool, err error) {
	tx, err := s.DB.BeginTx(bg(), nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var id int64
	defer func() {
		if err == nil {
			g.ID = id
		}
	}()
	var status, snooze string
	err = tx.QueryRow(`SELECT id, status, coalesce(snooze_until, '') FROM error_suggestions WHERE project = ? AND service = ? AND signature = ?`,
		g.Project, g.Service, g.Signature).Scan(&id, &status, &snooze)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		var res sql.Result
		res, err = tx.Exec(`INSERT INTO error_suggestions (project, service, signature, count, last_count, first_seen, last_seen,
			sample, caller, prefix, request_id, stacktrace, scan_id) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			g.Project, g.Service, g.Signature, g.LastCount, g.LastCount, ts(g.FirstSeen), ts(g.LastSeen),
			g.Sample, g.Caller, g.Prefix, g.RequestID, g.Stacktrace, scanID)
		if err == nil {
			id, err = res.LastInsertId()
		}
		fresh = true
	case err != nil:
		return false, err
	default:
		next := status
		if status == "snoozed" && !parseTS(snooze).After(now) {
			next, fresh = "new", true
		}
		_, err = tx.Exec(`UPDATE error_suggestions SET status = ?, count = count + ?, last_count = ?, last_seen = ?, sample = ?,
			caller = ?, prefix = ?, request_id = ?, stacktrace = ?, scan_id = ? WHERE id = ?`,
			next, g.LastCount, g.LastCount, ts(g.LastSeen), g.Sample, g.Caller, g.Prefix, g.RequestID, g.Stacktrace, scanID, id)
	}
	if err != nil {
		return false, err
	}
	return fresh, tx.Commit()
}

// Suggestions lists a project's suggestions to act on ("" = every project):
// status new, most frequent in their latest scan first.
func (s *Store) Suggestions(project string) ([]*Suggestion, error) {
	q := `SELECT id, project, service, signature, status, coalesce(snooze_until, ''), coalesce(task_id, ''), count, last_count,
		first_seen, last_seen, sample, caller, prefix, request_id, stacktrace, coalesce(flow_id, 0) FROM error_suggestions WHERE status = 'new'`
	var args []any
	if project != "" {
		q += ` AND project = ?`
		args = append(args, project)
	}
	rows, err := s.DB.QueryContext(bg(), q+` ORDER BY last_count DESC, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Suggestion
	for rows.Next() {
		var g Suggestion
		var snooze, first, last string
		if err := rows.Scan(&g.ID, &g.Project, &g.Service, &g.Signature, &g.Status, &snooze, &g.TaskID, &g.Count, &g.LastCount,
			&first, &last, &g.Sample, &g.Caller, &g.Prefix, &g.RequestID, &g.Stacktrace, &g.FlowID); err != nil {
			return nil, err
		}
		g.SnoozeUntil, g.FirstSeen, g.LastSeen = parseTS(snooze), parseTS(first), parseTS(last)
		out = append(out, &g)
	}
	return out, rows.Err()
}

// SetSuggestion changes a suggestion's status: dismissed (never again),
// snoozed until a time, or task (a task was created from it: taskID).
func (s *Store) SetSuggestion(id int64, status string, until time.Time, taskID string) error {
	var snooze any
	if !until.IsZero() {
		snooze = ts(until)
	}
	var task any
	if taskID != "" {
		task = taskID
	}
	_, err := s.DB.ExecContext(bg(), `UPDATE error_suggestions SET status = ?, snooze_until = ?, task_id = coalesce(?, task_id) WHERE id = ?`,
		status, snooze, task, id)
	return err
}

// ErrorFlow is the suggestions of one failing path (see internal/scan).
type ErrorFlow struct {
	ID      int64 // 0: a suggestion on its own
	Title   string
	Cause   string
	How     string // request_id, claude
	Members []*Suggestion
}

// Total is the members' count in their latest scan.
func (f *ErrorFlow) Total() int {
	n := 0
	for _, m := range f.Members {
		n += m.LastCount
	}
	return n
}

// SaveFlow records a flow of scan scanID over suggestions ids.
func (s *Store) SaveFlow(project string, scanID int64, title, cause, how string, ids []int64) error {
	tx, err := s.DB.BeginTx(bg(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT INTO error_flows (project, scan_id, title, cause, how, created_at) VALUES (?,?,?,?,?,?)`,
		project, scanID, title, cause, how, ts(time.Now()))
	if err != nil {
		return err
	}
	fid, err := res.LastInsertId()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := tx.Exec(`UPDATE error_suggestions SET flow_id = ? WHERE id = ?`, fid, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SuggestionFlows lists the waiting suggestions grouped by flow, the biggest
// first (a suggestion without a flow is a flow of its own).
func (s *Store) SuggestionFlows(project string) ([]*ErrorFlow, error) {
	gs, err := s.Suggestions(project)
	if err != nil {
		return nil, err
	}
	var out []*ErrorFlow
	byID := map[int64]*ErrorFlow{}
	for _, g := range gs {
		if g.FlowID == 0 {
			out = append(out, &ErrorFlow{Members: []*Suggestion{g}})
			continue
		}
		f := byID[g.FlowID]
		if f == nil {
			f = &ErrorFlow{ID: g.FlowID}
			if err := s.DB.QueryRowContext(bg(), `SELECT title, cause, how FROM error_flows WHERE id = ?`, g.FlowID).
				Scan(&f.Title, &f.Cause, &f.How); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
			byID[g.FlowID] = f
			out = append(out, f)
		}
		f.Members = append(f.Members, g)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Total() > out[j].Total() })
	return out, nil
}

// ---- task assessment -----------------------------------------------------------------

// Task levels, easiest first (see the workflow skill, "Execution profile").
var Levels = []string{"light", "standard", "hard", "hardest"}

// Task kinds.
var Kinds = []string{"feature", "bugfix", "investigation", "improvement", "refactor", "config", "test", "docs", "ops", "other"}

// Assessment is the agent's view of a task: how hard and what kind of work.
type Assessment struct {
	Level     string
	Kind      string
	Reason    string
	SetBy     string // "agent" (default) or "estimate" (Claude, from the ticket)
	UpdatedAt time.Time
}

// SetAssessment records the task's assessment (empty fields keep the old
// value). An estimate never replaces an existing assessment; the agent's
// replaces an estimate.
func (s *Store) SetAssessment(id string, a *Assessment) error {
	by := a.SetBy
	if by == "" {
		by = "agent"
	}
	q := `INSERT INTO task_assessments (task_id, level, kind, reason, set_by, updated_at) VALUES (?,?,?,?,?,?)
		ON CONFLICT (task_id) DO UPDATE SET level = coalesce(nullif(excluded.level, ''), level),
		kind = coalesce(nullif(excluded.kind, ''), kind), reason = coalesce(nullif(excluded.reason, ''), reason),
		set_by = excluded.set_by, updated_at = excluded.updated_at`
	if by == "estimate" {
		q = `INSERT INTO task_assessments (task_id, level, kind, reason, set_by, updated_at) VALUES (?,?,?,?,?,?)
		ON CONFLICT (task_id) DO NOTHING`
	}
	_, err := s.DB.ExecContext(bg(), q, id, a.Level, a.Kind, a.Reason, by, ts(time.Now()))
	return err
}

// TaskAssessment returns the task's assessment, or nil.
func (s *Store) TaskAssessment(id string) (*Assessment, error) {
	var a Assessment
	var at string
	err := s.DB.QueryRowContext(bg(), `SELECT level, kind, reason, set_by, updated_at FROM task_assessments WHERE task_id = ?`, id).
		Scan(&a.Level, &a.Kind, &a.Reason, &a.SetBy, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	a.UpdatedAt = parseTS(at)
	return &a, nil
}

// ---- conversation --------------------------------------------------------------------

// RunPrompt is what taskpilot sent the agent at the start of a run.
type RunPrompt struct {
	ID        int64
	N         int
	Trigger   string // initial, approval, rejection, operator_message
	Prompt    string
	Model     string
	Effort    string
	StartedAt time.Time
	EndedAt   time.Time
	Outcome   string
	CostUSD   float64
	ResumeAt  string // the session entry it continued from ("" = unknown)
	Revoked   bool   // the operator took back what started it
}

// RunPrompts lists the task's runs, oldest first.
func (s *Store) RunPrompts(id string) ([]*RunPrompt, error) {
	rows, err := s.DB.QueryContext(bg(), `SELECT id, n, trigger, prompt, model, effort, started_at, coalesce(ended_at, ''),
		coalesce(outcome, ''), cost_usd, resume_at, revoked_at IS NOT NULL FROM runs WHERE task_id = ? ORDER BY n, id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*RunPrompt
	for rows.Next() {
		var r RunPrompt
		var started, ended string
		if err := rows.Scan(&r.ID, &r.N, &r.Trigger, &r.Prompt, &r.Model, &r.Effort, &started, &ended, &r.Outcome, &r.CostUSD,
			&r.ResumeAt, &r.Revoked); err != nil {
			return nil, err
		}
		r.StartedAt, r.EndedAt = parseTS(started), parseTS(ended)
		out = append(out, &r)
	}
	return out, rows.Err()
}

// OperatorMessage is a message the operator sent to the agent.
type OperatorMessage struct {
	Text     string
	At       time.Time
	Consumed bool
}

// OperatorMessages lists the task's operator messages, oldest first.
func (s *Store) OperatorMessages(id string) ([]*OperatorMessage, error) {
	rows, err := s.DB.QueryContext(bg(), `SELECT message, created_at, consumed_at IS NOT NULL FROM operator_messages WHERE task_id = ? ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*OperatorMessage
	for rows.Next() {
		var m OperatorMessage
		var at string
		if err := rows.Scan(&m.Text, &at, &m.Consumed); err != nil {
			return nil, err
		}
		m.At = parseTS(at)
		out = append(out, &m)
	}
	return out, rows.Err()
}

// ---- graphify nudges ---------------------------------------------------------------

// graphify markers live in the task dir: the run in which the agent last
// queried the code graph, and the run it was last reminded in.
func (s *Store) graphifyMarker(id, name string) string {
	return filepath.Join(s.TaskDir(id), "graphify."+name)
}

func (s *Store) markerRun(id, name string) int {
	b, err := os.ReadFile(s.graphifyMarker(id, name))
	if err != nil {
		return -1
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return -1
	}
	return n
}

func (s *Store) setMarker(id, name string, run int) error {
	if err := os.MkdirAll(s.TaskDir(id), 0o755); err != nil {
		return err
	}
	return os.WriteFile(s.graphifyMarker(id, name), []byte(strconv.Itoa(run)), 0o644)
}

// MarkGraphifyUsed records that the agent queried the code graph in the task's current run.
func (s *Store) MarkGraphifyUsed(id string) error {
	t, err := s.LoadTask(id)
	if err != nil {
		return err
	}
	return s.setMarker(id, "used", t.Runs)
}

// GraphifyNudge reports whether the agent should be reminded of the code
// graph now (no query and no reminder yet in the current run), and records
// the reminder when it should.
func (s *Store) GraphifyNudge(id string) bool {
	t, err := s.LoadTask(id)
	if err != nil {
		return false
	}
	if s.markerRun(id, "used") == t.Runs || s.markerRun(id, "nudged") == t.Runs {
		return false
	}
	return s.setMarker(id, "nudged", t.Runs) == nil
}
