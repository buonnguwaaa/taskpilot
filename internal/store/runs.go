package store

import (
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"time"
)

// RunStart describes a run about to begin.
type RunStart struct {
	N         int
	SessionID string
	Resume    bool
	Trigger   string // initial, approval, rejection, operator_message
	Prompt    string
	Model     string
	Effort    string
	// ResumeAt is the session entry the run continues from ("" = unknown).
	ResumeAt string
}

// RunEnd is what a finished run produced.
type RunEnd struct {
	Subtype          string
	IsError          bool
	Error            string
	NumTurns         int
	CostUSD          float64
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	Outcome          string // awaiting_approval, done, needs_attention, failed, interrupted
}

// ToolCall is one tool use of a run (input already summarised and redacted).
type ToolCall struct {
	Seq          int
	ToolUseID    string
	Name         string
	InputSummary string
	IsError      bool
	DeniedBy     string // "", hook, permission
	DenyReason   string
}

// StartRun records a run and returns its id.
func (s *Store) StartRun(taskID string, r RunStart) (int64, error) {
	trigger := r.Trigger
	if trigger == "" {
		trigger = "initial"
	}
	res, err := s.DB.ExecContext(bg(), `INSERT INTO runs (task_id, n, session_id, resume, trigger, prompt, model, effort,
		started_at, events_path, stderr_path, resume_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		taskID, r.N, r.SessionID, r.Resume, trigger, r.Prompt, r.Model, r.Effort, ts(time.Now()),
		s.EventsPath(taskID), s.StderrPath(taskID, r.N), r.ResumeAt)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// FinishRun stores a run's result and its tool calls.
func (s *Store) FinishRun(runID int64, e RunEnd, calls []ToolCall) error {
	tx, err := s.DB.BeginTx(bg(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var started string
	if err := tx.QueryRow(`SELECT started_at FROM runs WHERE id = ?`, runID).Scan(&started); err != nil {
		return err
	}
	var outcome any
	if e.Outcome != "" {
		outcome = e.Outcome
	}
	now := time.Now()
	if _, err := tx.Exec(`UPDATE runs SET ended_at=?, duration_ms=?, result_subtype=?, is_error=?, error=?, num_turns=?,
		cost_usd=?, input_tokens=?, output_tokens=?, cache_read_tokens=?, cache_write_tokens=?, outcome=? WHERE id=?`,
		ts(now), now.Sub(parseTS(started)).Milliseconds(), e.Subtype, e.IsError, e.Error, e.NumTurns, e.CostUSD,
		e.InputTokens, e.OutputTokens, e.CacheReadTokens, e.CacheWriteTokens, outcome, runID); err != nil {
		return err
	}
	for _, c := range calls {
		var denied any
		if c.DeniedBy != "" {
			denied = c.DeniedBy
		}
		if _, err := tx.Exec(`INSERT INTO tool_calls (run_id, seq, tool_use_id, tool_name, input_summary, is_error, denied_by, deny_reason)
			VALUES (?,?,?,?,?,?,?,?)`, runID, c.Seq, c.ToolUseID, c.Name, c.InputSummary, c.IsError, denied, c.DenyReason); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Run is a stored run (for reports).
type Run struct {
	ID        int64
	N         int
	Trigger   string
	StartedAt time.Time
	Duration  time.Duration
	CostUSD   float64
	NumTurns  int
	Outcome   string
	Subtype   string
	ToolCalls int
	Denied    int
}

// Runs lists a task's runs, oldest first.
func (s *Store) Runs(taskID string) ([]Run, error) {
	rows, err := s.DB.QueryContext(bg(), `SELECT r.id, r.n, r.trigger, r.started_at, coalesce(r.duration_ms,0), r.cost_usd, r.num_turns,
		coalesce(r.outcome,''), r.result_subtype,
		(SELECT count(*) FROM tool_calls c WHERE c.run_id = r.id),
		(SELECT count(*) FROM tool_calls c WHERE c.run_id = r.id AND c.denied_by IS NOT NULL)
		FROM runs r WHERE r.task_id = ? ORDER BY r.n`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var r Run
		var started string
		var ms int64
		if err := rows.Scan(&r.ID, &r.N, &r.Trigger, &started, &ms, &r.CostUSD, &r.NumTurns, &r.Outcome, &r.Subtype, &r.ToolCalls, &r.Denied); err != nil {
			return nil, err
		}
		r.StartedAt, r.Duration = parseTS(started), time.Duration(ms)*time.Millisecond
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- outbox ------------------------------------------------------------------------

// OutboxItem is a report waiting to reach the ticket system.
type OutboxItem struct {
	ID       int64
	TaskID   string
	Source   string
	TicketID string
	Message  string
	Attempts int
}

// Enqueue adds a report to the outbox and returns its id.
func (s *Store) Enqueue(taskID, source, ticketID, msg string) (int64, error) {
	now := ts(time.Now())
	res, err := s.DB.ExecContext(bg(), `INSERT INTO outbox (task_id, source, ticket_id, message, next_attempt_at, created_at) VALUES (?,?,?,?,?,?)`,
		taskID, source, ticketID, msg, now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// DueOutbox returns pending reports whose next attempt time has come, oldest first.
func (s *Store) DueOutbox(limit int) ([]OutboxItem, error) {
	rows, err := s.DB.QueryContext(bg(), `SELECT id, task_id, source, ticket_id, message, attempts FROM outbox
		WHERE status = 'pending' AND next_attempt_at <= ? ORDER BY id LIMIT ?`, ts(time.Now()), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutboxItem
	for rows.Next() {
		var o OutboxItem
		if err := rows.Scan(&o.ID, &o.TaskID, &o.Source, &o.TicketID, &o.Message, &o.Attempts); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// MarkSent records a delivered report.
func (s *Store) MarkSent(id int64) error {
	_, err := s.DB.ExecContext(bg(), `UPDATE outbox SET status='sent', attempts=attempts+1, sent_at=?, last_error='' WHERE id=?`, ts(time.Now()), id)
	return err
}

// MarkFailed records a failed delivery: retried with exponential backoff, given
// up (status failed) after maxAttempts.
func (s *Store) MarkFailed(id int64, cause error, maxAttempts int) error {
	var attempts int
	if err := s.DB.QueryRowContext(bg(), `SELECT attempts FROM outbox WHERE id=?`, id).Scan(&attempts); err != nil {
		return err
	}
	attempts++
	status := "pending"
	if attempts >= maxAttempts {
		status = "failed"
	}
	backoff := time.Duration(1<<min(attempts, 10)) * 30 * time.Second
	_, err := s.DB.ExecContext(bg(), `UPDATE outbox SET status=?, attempts=?, last_error=?, next_attempt_at=? WHERE id=?`,
		status, attempts, cause.Error(), ts(time.Now().Add(backoff)), id)
	return err
}

// ---- stats -----------------------------------------------------------------------

// GateStat is one row of v_gate_stats.
type GateStat struct {
	Gate                                  string
	Requests, Approved, Rejected, Pending int
	ApprovalRate                          float64 // -1 when nothing was decided yet
	AvgWaitMinutes                        float64
}

// Stats summarises activity for the status page.
type Stats struct {
	CostToday, Cost7d float64
	Runs7d            int
	TasksDone7d       int
	Gates             []GateStat
	TopDenials        []string
	OutboxPending     int
	OutboxFailed      int
}

// Stats computes the status-page summary.
func (s *Store) Stats() (*Stats, error) {
	st := &Stats{}
	y, mo, d := time.Now().Date()
	today := ts(time.Date(y, mo, d, 0, 0, 0, 0, time.Local)) // local midnight; started_at is UTC
	week := ts(time.Now().AddDate(0, 0, -7))
	q := s.DB.QueryRowContext
	if err := q(bg(), `SELECT coalesce(sum(cost_usd),0) FROM runs WHERE started_at >= ?`, today).Scan(&st.CostToday); err != nil {
		return nil, err
	}
	if err := q(bg(), `SELECT coalesce(sum(cost_usd),0), count(*) FROM runs WHERE started_at >= ?`, week).Scan(&st.Cost7d, &st.Runs7d); err != nil {
		return nil, err
	}
	if err := q(bg(), `SELECT count(*) FROM tasks WHERE state = 'done' AND closed_at >= ?`, week).Scan(&st.TasksDone7d); err != nil {
		return nil, err
	}
	if err := q(bg(), `SELECT count(*) FILTER (WHERE status='pending'), count(*) FILTER (WHERE status='failed') FROM outbox`).
		Scan(&st.OutboxPending, &st.OutboxFailed); err != nil {
		return nil, err
	}
	rows, err := s.DB.QueryContext(bg(), `SELECT gate, requests, approved, rejected, pending, approval_rate, avg_wait_minutes FROM v_gate_stats ORDER BY gate`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var g GateStat
		var rate, wait sql.NullFloat64
		if err := rows.Scan(&g.Gate, &g.Requests, &g.Approved, &g.Rejected, &g.Pending, &rate, &wait); err != nil {
			rows.Close()
			return nil, err
		}
		g.ApprovalRate, g.AvgWaitMinutes = -1, wait.Float64
		if rate.Valid {
			g.ApprovalRate = rate.Float64
		}
		st.Gates = append(st.Gates, g)
	}
	rows.Close()
	rows, err = s.DB.QueryContext(bg(), `SELECT tool_name || ' (' || denied_by || ') ×' || n FROM v_denials LIMIT 5`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		st.TopDenials = append(st.TopDenials, d)
	}
	if err := rows.Err(); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	return st, nil
}

// ---- graphify graph usage -----------------------------------------------------------------

// ExploreStats is the average planning run (run 1) of a group of tasks.
type ExploreStats struct {
	Tasks       int
	Lookups     float64 // Grep, Glob, Read and search commands (rg, grep, find, ls, cat…) per planning run
	Explorers   float64 // subagents spawned per planning run
	PlanCost    float64 // USD
	PlanMinutes float64
}

// GraphUse is how agents used the graphify graph since a time, and how their
// planning runs compare with those of tasks that did not use it.
type GraphUse struct {
	Since      time.Time
	Calls      int
	Errors     int
	ByAction   map[string]int
	Tasks      int // tasks whose first run started since
	GraphTasks int // of which used graphify
	// SearchRuns are the runs since that searched code (Grep, rg…);
	// GraphRuns of them also queried the graph.
	SearchRuns, GraphRuns int
	With                  ExploreStats
	Without               ExploreStats
	// Recent are the latest graphify calls ("task: action text").
	Recent []string
}

// searchCmd matches a shell command that only looks for or at code.
var searchCmd = regexp.MustCompile(`^\s*(cd\s+\S+\s*&&\s*)?(rg|grep|egrep|find|fd|ls|tree|cat|head|tail|wc|sed\s+-n|git\s+(-C\s+\S+\s+)?(ls-files|grep|show|log|blame))\b`)

func isGraphTool(name string) bool { return strings.HasSuffix(name, "graphify") }

// GraphUsage computes the graphify graph usage since a time.
func (s *Store) GraphUsage(since time.Time) (*GraphUse, error) {
	u := &GraphUse{Since: since, ByAction: map[string]int{}}
	rows, err := s.DB.QueryContext(bg(), `SELECT r.task_id, r.n, c.tool_name, c.input_summary, c.is_error, r.started_at
		FROM tool_calls c JOIN runs r ON r.id = c.run_id
		WHERE r.task_id IN (SELECT task_id FROM runs WHERE n = 1 AND started_at >= ?)
		ORDER BY r.started_at, c.seq`, ts(since))
	if err != nil {
		return nil, err
	}
	type acc struct {
		graph            bool
		lookups, explore int
	}
	tasks := map[string]*acc{}
	get := func(id string) *acc {
		if tasks[id] == nil {
			tasks[id] = &acc{}
		}
		return tasks[id]
	}
	for rows.Next() {
		var task, tool, input, started string
		var n int
		var isErr bool
		if err := rows.Scan(&task, &n, &tool, &input, &isErr, &started); err != nil {
			rows.Close()
			return nil, err
		}
		a := get(task)
		switch {
		case isGraphTool(tool):
			a.graph = true
			u.Calls++
			if isErr {
				u.Errors++
			}
			action, _, _ := strings.Cut(strings.TrimSpace(input), " ")
			if action == "" {
				action = "?"
			}
			u.ByAction[action]++
			u.Recent = append(u.Recent, task+": "+input)
		case n != 1:
		case tool == "Grep" || tool == "Glob" || tool == "Read" || tool == "Search":
			a.lookups++
		case (tool == "Bash" || tool == "Shell") && searchCmd.MatchString(input):
			a.lookups++
		case tool == "Agent" || tool == "Task":
			a.explore++
		}
	}
	rows.Close()
	if len(u.Recent) > 5 {
		u.Recent = u.Recent[len(u.Recent)-5:]
	}
	// Runs since that searched code, and how many of them asked the graph.
	if err := s.DB.QueryRowContext(bg(), `SELECT count(*), coalesce(sum(g), 0) FROM (
		SELECT r.id, max(c.tool_name LIKE '%graphify') AS g,
		       max(c.tool_name IN ('Grep','Search') OR ((c.tool_name IN ('Bash','Shell')) AND (c.input_summary LIKE '%rg %' OR c.input_summary LIKE '%grep %'))) AS searched
		FROM runs r JOIN tool_calls c ON c.run_id = r.id WHERE r.started_at >= ? GROUP BY r.id) WHERE searched`, ts(since)).
		Scan(&u.SearchRuns, &u.GraphRuns); err != nil {
		return nil, err
	}
	// The planning run of every task started since (tasks without tool calls too).
	rows, err = s.DB.QueryContext(bg(), `SELECT task_id, cost_usd, coalesce(duration_ms, 0) FROM runs WHERE n = 1 AND started_at >= ?`, ts(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	add := func(e *ExploreStats, a *acc, cost float64, ms int64) {
		e.Tasks++
		e.Lookups += float64(a.lookups)
		e.Explorers += float64(a.explore)
		e.PlanCost += cost
		e.PlanMinutes += float64(ms) / 60000
	}
	for rows.Next() {
		var task string
		var cost float64
		var ms int64
		if err := rows.Scan(&task, &cost, &ms); err != nil {
			return nil, err
		}
		a := get(task)
		u.Tasks++
		if a.graph {
			u.GraphTasks++
			add(&u.With, a, cost, ms)
		} else {
			add(&u.Without, a, cost, ms)
		}
	}
	for _, e := range []*ExploreStats{&u.With, &u.Without} {
		if e.Tasks > 0 {
			n := float64(e.Tasks)
			e.Lookups, e.Explorers, e.PlanCost, e.PlanMinutes = e.Lookups/n, e.Explorers/n, e.PlanCost/n, e.PlanMinutes/n
		}
	}
	return u, rows.Err()
}

// OperatorTextOfRun returns what the operator wrote that started the task's
// latest run: messages consumed and request decisions (approve/reject
// comments, answers) made after the previous run started, up to this run's
// start. Only the TUI and the CLI write these rows, so an agent cannot
// forge them; merge_pull_request uses them as the operator's instruction.
func (s *Store) OperatorTextOfRun(taskID string) ([]string, error) {
	rows, err := s.DB.QueryContext(bg(), `SELECT started_at FROM runs WHERE task_id = ? ORDER BY n DESC, id DESC LIMIT 2`, taskID)
	if err != nil {
		return nil, err
	}
	var starts []time.Time
	for rows.Next() {
		var at string
		if err := rows.Scan(&at); err != nil {
			rows.Close()
			return nil, err
		}
		starts = append(starts, parseTS(at))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(starts) == 0 {
		return nil, nil
	}
	cur, prev := starts[0], time.Time{}
	if len(starts) > 1 {
		prev = starts[1]
	}
	in := func(at string) bool {
		t := parseTS(at)
		return !t.IsZero() && t.After(prev) && !t.After(cur)
	}
	var out []string
	collect := func(q string) error {
		rows, err := s.DB.QueryContext(bg(), q, taskID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var text, at string
			if err := rows.Scan(&text, &at); err != nil {
				return err
			}
			if in(at) && strings.TrimSpace(text) != "" {
				out = append(out, text)
			}
		}
		return rows.Err()
	}
	if err := collect(`SELECT message, consumed_at FROM operator_messages WHERE task_id = ? AND consumed_at IS NOT NULL`); err != nil {
		return nil, err
	}
	if err := collect(`SELECT coalesce(comment, ''), decided_at FROM approvals WHERE task_id = ? AND decided_at IS NOT NULL AND decided_by = 'operator'`); err != nil {
		return nil, err
	}
	return out, nil
}

// UsedGraphify reports whether an earlier run of the task called graphify
// (the tool calls of a run are recorded when it ends).
func (s *Store) UsedGraphify(taskID string) (bool, error) {
	var n int
	err := s.DB.QueryRowContext(bg(), `SELECT count(*) FROM tool_calls c JOIN runs r ON r.id = c.run_id
		WHERE r.task_id = ? AND c.tool_name LIKE '%graphify'`, taskID).Scan(&n)
	return n > 0, err
}
